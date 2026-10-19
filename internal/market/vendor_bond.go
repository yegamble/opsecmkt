package market

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
)

// A-547 (owner decision 2026-10-18): the vendor bond (vendor_applications.go, A-544) paid automatically in XMR
// (migration 124). The bond is a NON-REFUNDABLE fee. A-544's "record bond received" and waiver stay.
//
// Issuance. While bond addresses may be issued (xmrBondIssuable: Monero escrow ready, new XMR payments on, neither the
// restore gate nor the restore sweep set), the Monero escrow watcher gives each application waiting for a bond a
// one-time deposit address: an account of the bond wallet, made by create_account and inserted only once get_address of
// its address 0 matches (vendor_bond_accounts), for that application alone and never reused. Checkout and the pages make
// no RPC: /account shows the address once the watcher made it, and says the bond is recorded by an administrator while
// issuance is not allowed (the address is then not shown).
//
// The bond wallet (xmr_bond_wallets) is one long-lived wallet on the escrow wallet-rpc, bond-NN, created when the first
// address is needed as an escrow wallet is (the password derived from MONERO_WALLET_SECRET, the seed sealed to the
// pinned ESCROW_XMR_SEED_KEY, the primary address and restore height kept), and read in each watcher pass after the
// escrow wallets, inside the watcher's advisory lock, one wallet open at a time: open_wallet, recheckEscrowXMRNetwork,
// the primary address must be the row's; during the restore sweep a lookahead past its highest recorded account;
// get_transfers; get_accounts; accounts with no row; issuance; store. Its accounts are not escrow slots and its
// deposits are not payments rows: no order ever sees them. A bond wallet file found with no row (create_wallet answers
// that the name exists, or the restore sweep finds the next name) is recorded as an orphan, its seed sealed when it
// opens, and the administrators are told; the market takes the next name. The market NEVER sends from the bond wallet:
// there is no transfer, relay, refund or forward. The operator sweeps it offline from its sealed seed (operator guide).
//
// Deposits (vendor_bond_deposits), one row per transaction and account, count from the escrow threshold
// (escrowXMR.confirmations) with no unlock time and no double_spend_seen. Once the counted deposits of an application
// waiting for its bond cover the bond captured on it, it is recorded received (bond_received_how 'xmr') and moves to
// waiting for approval, or with automatic approval is approved (an active buyer account only; otherwise it waits for
// an administrator), audited and notified. An under-payment waits with no time limit; an administrator may record or
// waive the bond as before. An over-payment is kept and the administrators told. A deposit that is locked, or that is
// not counted by an application no longer waiting for its bond (withdrawn, rejected, waived, recorded or approved) is
// kept and flagged to the administrators once; it changes nothing. A counted deposit that later falls below the
// threshold (a reorganisation) is flagged once; nothing is revoked and no role changes.
//
// Under the restore gate or the sweep, no bond wallet or account is created and no address shown; deposits are still
// read, but no bond is recorded and nothing is approved until both clear. An account of the bond wallet with no row (a
// create_account answer lost, or a database restored to an earlier point) is added with no application and the
// administrators are told (xmrRetireRowless's rule); coins at it, or at an account whose application was erased, are
// reported once and fund nothing. The bond wallet's addresses are of the verified network: createAccount and
// primaryAddress parse them for it, and pinXMRNetwork counts a bond wallet as Monero escrow state.

// xmrBondIssueBatch bounds the accounts issued in one pass.
const xmrBondIssueBatch = 20

var errXMRBondNotWritten = errors.New("monero escrow: the bond wallet row was not in state creating; its seal was not written")

// xmrBondWalletRow is the bond wallet in use (creating or sealed).
type xmrBondWalletRow struct {
	id                   int64
	name, state, primary string
}

// xmrBondAccountRow is one recorded account of the bond wallet; app is "" when it funds no application.
type xmrBondAccountRow struct {
	id      int64
	address string
	app     string
	alerted bool
}

// xmrBondIssuable reports whether bond addresses may be issued and shown now: Monero escrow ready, new XMR payments on
// (payment_intake_XMR 'escrow'), and neither the restore gate nor the sweep set.
func (a *App) xmrBondIssuable(ctx context.Context, q rowQuerier) (bool, error) {
	if !a.xmrEscrowReady() {
		return false, nil
	}
	var ok bool
	err := q.QueryRowContext(ctx, "SELECT NOT "+restoreGateSQL+" AND NOT EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true') AND EXISTS(SELECT 1 FROM settings WHERE key=$2 AND value=$3)",
		xmrEscrowSweepKey, paymentIntakeKey("XMR"), intakeEscrow).Scan(&ok)
	return ok, err
}

// xmrBondPass is the bond wallet's turn in a Monero escrow pass (see the file comment). paused: the restore gate or
// the sweep is set; sweep: the sweep is running. checked reports that the bond wallet, when there is one, was opened,
// checked and read (as the sweep requires), or definitely refused (it is then reported, as a missing escrow wallet is).
func (a *App) xmrBondPass(ctx context.Context, paused, sweep bool) (checked bool, err error) {
	x := a.escrowXMR
	if sweep {
		if err = a.xmrBondSweepProbe(ctx); err != nil {
			return false, err
		}
	}
	issue := false
	if !paused {
		if issue, err = a.xmrBondIssuable(ctx, a.db); err != nil {
			return false, err
		}
	}
	w, err := a.xmrBondWallet(ctx)
	if err != nil {
		return false, err
	}
	if !paused && x.canCreate.Load() {
		if w.state == "creating" {
			if err = a.xmrBondCreate(ctx, w.id, w.name, true); err != nil {
				return false, err
			}
		} else if w.id == 0 && issue {
			if err = a.xmrBondEnsure(ctx); err != nil {
				return false, err
			}
		}
		if w, err = a.xmrBondWallet(ctx); err != nil {
			return false, err
		}
	}
	checked = true
	var walletErr error
	if w.state == "sealed" {
		walletErr = x.rpc.openWallet(ctx, w.name, xmrWalletPassword(x.secret, w.name))
		switch {
		case walletErr == nil:
			if walletErr = a.xmrBondUnopenable(ctx, w, false); walletErr == nil {
				walletErr = a.xmrBondWalletPass(ctx, w, sweep, issue)
			}
			checked = walletErr == nil
		case xmrWalletDenied(walletErr) && xmrOpenRefusal(walletErr) != xmrOpenLocked:
			// It does not open (its file missing, or another password): reported once while it lasts, never replaced,
			// since it may hold bonds. The sweep goes on, as for a missing escrow wallet.
			if alertErr := a.xmrBondUnopenable(ctx, w, true); alertErr != nil {
				return false, errors.Join(walletErr, alertErr)
			}
		default:
			checked = false
		}
	}
	// The deposits as last read apply in the database even when the wallet was not read in this pass.
	return checked, errors.Join(walletErr, a.xmrBondSettle(ctx, paused))
}

// xmrBondWallet reads the bond wallet in use; id 0 when there is none.
func (a *App) xmrBondWallet(ctx context.Context) (xmrBondWalletRow, error) {
	var w xmrBondWalletRow
	err := a.db.QueryRowContext(ctx, "SELECT id,name,state,COALESCE(primary_address,'') FROM xmr_bond_wallets WHERE state IN ('creating','sealed')").Scan(&w.id, &w.name, &w.state, &w.primary)
	if err == sql.ErrNoRows {
		return xmrBondWalletRow{}, nil
	}
	return w, err
}

// xmrBondEnsure creates the bond wallet when an application waiting for a bond has no address yet, under the next
// free name: a name whose file exists is recorded as an orphan by xmrBondCreate and the next one is tried.
func (a *App) xmrBondEnsure(ctx context.Context) error {
	var due bool
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM vendor_applications a WHERE a.state='awaiting_bond' AND a.bond_piconero>0
		AND NOT EXISTS(SELECT 1 FROM vendor_bond_accounts c WHERE c.application_id=a.id))`).Scan(&due); err != nil || !due {
		return err
	}
	for {
		w, err := a.xmrBondWallet(ctx)
		if err != nil || w.id != 0 {
			return err
		}
		var id int64
		var name string
		if err = a.db.QueryRowContext(ctx, `INSERT INTO xmr_bond_wallets(name)
			SELECT 'bond-'||lpad((COALESCE(max(substr(name,6)::int),0)+1)::text,2,'0') FROM xmr_bond_wallets RETURNING id,name`).Scan(&id, &name); err != nil {
			return err
		}
		if err = a.xmrBondCreate(ctx, id, name, false); err != nil {
			return err
		}
	}
}

// xmrBondCreate creates bond wallet name for row id and seals it, as xmrCreateWallet does an escrow wallet: a file of
// that name that opens with the derived password is taken only when the row was resumed and the file is as
// create_wallet leaves it (xmrWalletFresh); otherwise it is an orphan (xmrBondOrphan).
func (a *App) xmrBondCreate(ctx context.Context, id int64, name string, resume bool) error {
	x := a.escrowXMR
	password := xmrWalletPassword(x.secret, name)
	err := x.rpc.createWallet(ctx, name, password)
	var re *xmrRPCError
	if errors.As(err, &re) && re.Code == -21 { // WALLET_ALREADY_EXISTS
		if err = x.rpc.openWallet(ctx, name, password); xmrWalletDenied(err) && xmrOpenRefusal(err) != xmrOpenLocked {
			return a.xmrBondOrphan(ctx, id, name, false)
		} else if err != nil {
			return err
		}
		if err = a.recheckEscrowXMRNetwork(ctx); err != nil {
			return err
		}
		fresh := false
		if resume {
			if fresh, err = xmrWalletFresh(ctx, x.rpc); err != nil {
				return err
			}
		}
		if !fresh {
			return a.xmrBondOrphan(ctx, id, name, true)
		}
	} else if err != nil {
		return err
	}
	// Every bond address is an account's address 0, as an escrow address is (xmrCreateWallet).
	if err = x.rpc.setSubaddressLookahead(ctx, password, xmrLookaheadMajor, 1); err != nil {
		return err
	}
	if err = a.recheckEscrowXMRNetwork(ctx); err != nil {
		return err
	}
	primary, err := x.rpc.primaryAddress(ctx)
	if err != nil {
		return err
	}
	mnemonic, err := x.rpc.mnemonic(ctx)
	if err != nil {
		return err
	}
	height, err := x.rpc.height(ctx)
	if err != nil {
		return err
	}
	sealed, fpr, err := x.seedKey.sealSeed(mnemonic)
	if err != nil {
		return err
	}
	res, err := a.db.ExecContext(ctx, `UPDATE xmr_bond_wallets SET primary_address=$2,restore_height=$3,seed_sealed=$4,seed_key_fpr=$5,sealed_at=now(),
		state='sealed',updated=now() WHERE id=$1 AND state='creating'`, id, primary, height, sealed, fpr)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errXMRBondNotWritten
	}
	log.Printf("escrow: vendor bond wallet %s created and its seed sealed", name)
	return x.rpc.store(ctx)
}

// xmrBondOrphan records bond wallet file name, which this database did not create, as an orphan: row id (left
// creating), or a new row when id is 0 (the restore sweep). opened: the file opened with the market's password and is
// the open wallet; its seed is then sealed to the pinned key (restore height 0) so the operator can sweep it, and its
// funded accounts are listed to the administrators. The market never reads or issues from it again.
func (a *App) xmrBondOrphan(ctx context.Context, id int64, name string, opened bool) error {
	x := a.escrowXMR
	var primary, sealed, fpr string
	var funded []string
	if opened {
		if !x.canCreate.Load() {
			return errXMRSweepSeedKey
		}
		if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
			return err
		}
		var err error
		if primary, err = x.rpc.primaryAddress(ctx); err != nil {
			return err
		}
		mnemonic, err := x.rpc.mnemonic(ctx)
		if err != nil {
			return err
		}
		accounts, err := x.rpc.accounts(ctx)
		if err != nil {
			return err
		}
		if sealed, fpr, err = x.seedKey.sealSeed(mnemonic); err != nil {
			return err
		}
		for _, acc := range accounts {
			if acc.Balance > 0 {
				funded = append(funded, "account "+strconv.FormatInt(acc.Index, 10)+" ("+amount(acc.Balance, 12)+" XMR)")
			}
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seal := []any{primary, sealed, fpr}
	if id == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO xmr_bond_wallets(name,state,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at)
			VALUES($1,'orphan',NULLIF($2,''),CASE WHEN $3<>'' THEN 0 END,NULLIF($3,''),NULLIF($4,''),CASE WHEN $3<>'' THEN now() END)`, append([]any{name}, seal...)...)
	} else {
		var res sql.Result
		if res, err = tx.ExecContext(ctx, `UPDATE xmr_bond_wallets SET state='orphan',primary_address=NULLIF($2,''),restore_height=CASE WHEN $3<>'' THEN 0 END,
			seed_sealed=NULLIF($3,''),seed_key_fpr=NULLIF($4,''),sealed_at=CASE WHEN $3<>'' THEN now() END,updated=now() WHERE id=$1 AND state='creating'`, append([]any{id}, seal...)...); err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				err = errXMRBondNotWritten
			}
		}
	}
	if err != nil {
		return err
	}
	body := "Vendor bond wallet " + name + " exists on the escrow wallet-rpc and does not open with the market's password (a file left by another install?). The market recorded it as an orphan and never uses it; check that wallet yourself."
	if opened {
		coins := "None of its accounts holds coins."
		if len(funded) > 0 {
			coins = "Funded accounts: " + strings.Join(funded, ", ") + ". Check them for vendor bonds this database lost and record those bonds by hand."
		}
		body = "Vendor bond wallet " + name + " was found on the escrow wallet-rpc with no record in this database (created after the backup it was restored from?). " +
			"The market sealed its seed to the seed key so that you can sweep it offline, and never issues a bond address from it again; new bond addresses come from the next bond wallet. " + coins
	}
	if err = notifyAdmins(ctx, tx, body); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: vendor bond wallet %s, which this database did not create, recorded as an orphan (sealed: %t); administrators notified", name, opened)
	if opened {
		return x.rpc.store(ctx)
	}
	return nil
}

// xmrBondSweepProbe, during the restore sweep, opens the bond wallet names after the newest row, up to the first with
// no file: a bond wallet created after the backup is recorded as an orphan (xmrBondOrphan), so its bonds are reported
// and its seed kept; one another wallet program has open leaves the sweep incomplete.
func (a *App) xmrBondSweepProbe(ctx context.Context) error {
	x := a.escrowXMR
	var n int
	if err := a.db.QueryRowContext(ctx, "SELECT COALESCE(max(substr(name,6)::int),0) FROM xmr_bond_wallets").Scan(&n); err != nil {
		return err
	}
	for n++; n <= 99; n++ {
		name := fmt.Sprintf("bond-%02d", n)
		found, err := x.rpc.probeWallet(ctx, name, xmrWalletPassword(x.secret, name))
		switch {
		case err == nil && !found:
			return nil
		case xmrOpenRefusal(err) == xmrOpenLocked:
			return err
		case xmrWalletDenied(err):
			err = a.xmrBondOrphan(ctx, 0, name, false)
		case err == nil:
			err = a.xmrBondOrphan(ctx, 0, name, true)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// xmrBondUnopenableKey is the setting that marks an open episode of bond wallet id refusing to open.
func xmrBondUnopenableKey(id int64) string {
	return "vendor_bond_wallet_unopenable_" + strconv.FormatInt(id, 10)
}

// xmrBondUnopenable tells the administrators once while bond wallet w does not open (refused), and ends the episode
// when it opens again.
func (a *App) xmrBondUnopenable(ctx context.Context, w xmrBondWalletRow, refused bool) error {
	return a.xmrEpisodeAlert(ctx, xmrBondUnopenableKey(w.id), refused,
		"Vendor bond wallet "+w.name+" does not open on the escrow wallet-rpc (its file is missing or no longer opens with the market's password). No bond address is issued and no bond deposit is read until it opens again; it may hold vendor bonds, so restore it from its sealed seed rather than letting the market create another one.",
		"escrow: vendor bond wallet "+w.name+" does not open; administrators notified")
}

// xmrBondWalletPass does the work of the open bond wallet w (see the file comment).
func (a *App) xmrBondWalletPass(ctx context.Context, w xmrBondWalletRow, sweep, issue bool) error {
	x := a.escrowXMR
	if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
		return err
	}
	if primary, err := x.rpc.primaryAddress(ctx); err != nil {
		return err
	} else if primary != w.primary {
		return errXMRWrongWallet
	}
	rows := map[int64]*xmrBondAccountRow{}
	var highest int64
	list, err := a.db.QueryContext(ctx, "SELECT id,account_index,address,COALESCE(application_id,''),unclaimed_alerted FROM vendor_bond_accounts WHERE wallet_id=$1", w.id)
	if err != nil {
		return err
	}
	for list.Next() {
		r := &xmrBondAccountRow{}
		var idx int64
		if err = list.Scan(&r.id, &idx, &r.address, &r.app, &r.alerted); err != nil {
			list.Close()
			return err
		}
		rows[idx], highest = r, max(highest, idx)
	}
	list.Close()
	if err = list.Err(); err != nil {
		return err
	}
	if sweep {
		// A wallet older than the database: deposits to the accounts the records have but it does not know are seen.
		if err = a.xmrRaiseLookahead(ctx, w.name, highest); err != nil {
			return err
		}
	}
	transfers, err := x.rpc.transfers(ctx)
	if err != nil {
		return err
	}
	funded, err := a.xmrBondRecord(ctx, rows, transfers)
	if err != nil {
		return err
	}
	accounts, err := x.rpc.accounts(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		if acc.Balance > 0 {
			funded[acc.Index] = true
		}
	}
	if err = a.xmrEpisodeAlert(ctx, "vendor_bond_account0_"+strconv.FormatInt(w.id, 10), funded[0],
		"Vendor bond wallet "+w.name+": coins arrived at its primary account (account 0), which is never a bond address. Check where they came from; they stay there.",
		"escrow: coins at account 0 of vendor bond wallet "+w.name+"; administrators notified"); err != nil {
		return err
	}
	if err = a.xmrBondReconcile(ctx, w, rows, accounts, funded); err != nil {
		return err
	}
	if issue {
		if err = a.xmrBondIssue(ctx, w, rows, int64(len(accounts))-1, highest); err != nil {
			return err
		}
	}
	return x.rpc.store(ctx)
}

// xmrBondRecord writes the bond wallet's incoming transfers to vendor_bond_deposits, one row per transaction and
// account of an application (the outputs of one transaction to an account are summed; a mined entry replaces its pool
// entry), and marks -1 those the wallet no longer lists. It returns the accounts that have seen coins.
func (a *App) xmrBondRecord(ctx context.Context, rows map[int64]*xmrBondAccountRow, transfers []xmrTransfer) (map[int64]bool, error) {
	type key struct {
		txid    string
		account int64
	}
	type sum struct {
		amount, confs int64
		locked        bool
	}
	sums := map[key]*sum{}
	var keys []key
	funded := map[int64]bool{}
	mined := map[key]bool{}
	ceiling := xmrConfirmationsCeiling(a.escrowXMR.confirmations)
	for _, pool := range []bool{false, true} {
		for _, t := range transfers {
			if (t.Type == "pool") != pool || t.Amount <= 0 {
				continue
			}
			funded[t.Account] = true
			k := key{t.TxID, t.Account}
			if pool && mined[k] {
				continue
			}
			mined[k] = !pool
			confs := t.Confirmations
			if pool {
				confs = 0
			}
			if t.DoubleSpendSeen {
				confs = -1
			}
			confs = min(confs, ceiling)
			s := sums[k]
			if s == nil {
				s = &sum{confs: confs}
				sums[k] = s
				keys = append(keys, k)
			}
			s.amount += t.Amount
			s.confs = min(s.confs, confs)
			s.locked = s.locked || t.Locked
		}
	}
	slices.SortFunc(keys, func(p, q key) int { return cmp.Or(cmp.Compare(p.account, q.account), strings.Compare(p.txid, q.txid)) })
	var ids []int64
	seen := []string{""}
	for _, r := range rows {
		if r.app != "" {
			ids = append(ids, r.id)
		}
	}
	for _, k := range keys {
		r := rows[k.account]
		if r == nil || r.app == "" {
			continue // account 0, or an account of no application: see xmrBondReconcile
		}
		s := sums[k]
		seen = append(seen, strconv.FormatInt(r.id, 10)+":"+k.txid)
		// late is decided once, when the row is first written: the application no longer waited for its bond.
		if _, err := a.db.ExecContext(ctx, `INSERT INTO vendor_bond_deposits AS d(account_id,txid,amount,confirmations,locked,late)
			VALUES($1,$2,$3,$4,$5,NOT EXISTS(SELECT 1 FROM vendor_applications WHERE id=$6 AND state='awaiting_bond'))
			ON CONFLICT (txid,account_id) DO UPDATE SET amount=excluded.amount,confirmations=excluded.confirmations,locked=excluded.locked,updated=now()
			WHERE (d.amount,d.confirmations,d.locked) IS DISTINCT FROM (excluded.amount,excluded.confirmations,excluded.locked)`,
			r.id, k.txid, s.amount, s.confs, s.locked, r.app); err != nil {
			return nil, err
		}
	}
	_, err := a.db.ExecContext(ctx, "UPDATE vendor_bond_deposits SET confirmations=-1,updated=now() WHERE account_id=ANY($1) AND confirmations>=0 AND NOT (account_id::text||':'||txid)=ANY($2)", ids, seen)
	return funded, err
}

// xmrBondReconcile matches the bond wallet's accounts with their rows. An account with no row (checked against
// get_address) is added with no application in one transaction, and the administrators are told once (the rows added
// make it once per discovery): it may be one the wallet gave an application after the point the database was restored
// to. Coins at an account of no application are reported once per account; they fund nothing. Account 0 is never one.
func (a *App) xmrBondReconcile(ctx context.Context, w xmrBondWalletRow, rows map[int64]*xmrBondAccountRow, accounts []xmrAccount, funded map[int64]bool) error {
	x := a.escrowXMR
	var rowless []xmrAccount
	for _, acc := range accounts {
		if acc.Index == 0 || rows[acc.Index] != nil {
			continue
		}
		addr, err := x.rpc.address(ctx, acc.Index, 0)
		if err != nil {
			return err
		}
		if addr != acc.BaseAddress {
			return errXMRAccountMismatch
		}
		rowless = append(rowless, acc)
	}
	if len(rowless) > 0 {
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, acc := range rowless {
			r := &xmrBondAccountRow{address: acc.BaseAddress}
			if err = tx.QueryRowContext(ctx, "INSERT INTO vendor_bond_accounts(wallet_id,account_index,address) VALUES($1,$2,$3) RETURNING id", w.id, acc.Index, acc.BaseAddress).Scan(&r.id); err != nil {
				return err
			}
			rows[acc.Index] = r
		}
		if err = notifyAdmins(ctx, tx, fmt.Sprintf("Vendor bond wallet %s holds %d account(s) the market has no record of: a wallet answer lost in a crash, or a database restored to an earlier point without scripts/restore.sh. "+
			"They fund no vendor application; coins there are reported to you. If the database was restored to an earlier point, follow UPGRADING.md section 6 now, and check those accounts for bonds paid after that point.", w.name, len(rowless))); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)", fmt.Sprintf("Vendor bond wallet %s: recorded %d account(s) the records lacked, funding no application", w.name, len(rowless))); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		log.Printf("escrow: vendor bond wallet %s holds %d account(s) with no row; recorded with no application, administrators notified", w.name, len(rowless))
	}
	for idx, r := range rows {
		if r.app != "" || r.alerted || !funded[idx] {
			continue
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE vendor_bond_accounts SET unclaimed_alerted=true WHERE id=$1 AND NOT unclaimed_alerted AND application_id IS NULL", r.id)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				err = notifyAdmins(ctx, tx, fmt.Sprintf("Vendor bond wallet %s: account %d, which belongs to no vendor application (unrecorded, or its application was erased), has received coins. They fund nothing and stay there; check whose bond they are and record it by hand.", w.name, idx))
			}
		}
		if err == nil {
			err = tx.Commit()
		}
		tx.Rollback()
		if err != nil {
			return err
		}
		r.alerted = true
	}
	return nil
}

// xmrBondIssue gives each application waiting for a bond with no address yet (the oldest first, at most
// xmrBondIssueBatch) an account of the open bond wallet w. known is the wallet's highest account and highest the
// highest the records name: a wallet older than the database is first brought up to the recorded accounts, each
// create_account answering the recorded address (accounts derive from the seed), so no recorded account is given again.
func (a *App) xmrBondIssue(ctx context.Context, w xmrBondWalletRow, rows map[int64]*xmrBondAccountRow, known, highest int64) error {
	x := a.escrowXMR
	apps, err := a.db.QueryContext(ctx, `SELECT a.id FROM vendor_applications a WHERE a.state='awaiting_bond' AND a.bond_piconero>0
		AND NOT EXISTS(SELECT 1 FROM vendor_bond_accounts c WHERE c.application_id=a.id) ORDER BY a.created,a.id LIMIT $1`, xmrBondIssueBatch)
	if err != nil {
		return err
	}
	var due []string
	for apps.Next() {
		var id string
		if err = apps.Scan(&id); err != nil {
			apps.Close()
			return err
		}
		due = append(due, id)
	}
	apps.Close()
	if err = apps.Err(); err != nil || len(due) == 0 {
		return err
	}
	for known < highest {
		idx, addr, err := x.rpc.createAccount(ctx)
		if err != nil {
			return err
		}
		if r := rows[idx]; r == nil || r.address != addr {
			return errXMRAccountMismatch
		}
		known = idx
	}
	for _, app := range due {
		idx, addr, err := x.rpc.createAccount(ctx)
		if err != nil {
			return err
		}
		got, err := x.rpc.address(ctx, idx, 0)
		if err != nil {
			return err
		}
		if got != addr {
			return errXMRAccountMismatch
		}
		// The application may have been erased since it was read: the account is then recorded for none.
		r := &xmrBondAccountRow{address: addr}
		if err = a.db.QueryRowContext(ctx, `INSERT INTO vendor_bond_accounts(wallet_id,account_index,address,application_id)
			VALUES($1,$2,$3,(SELECT id FROM vendor_applications WHERE id=$4)) RETURNING id,COALESCE(application_id,'')`, w.id, idx, addr, app).Scan(&r.id, &r.app); err != nil {
			return err
		}
		rows[idx] = r
	}
	return nil
}

// xmrBondSettle applies the deposits to the applications, in the database only: it flags locked deposits, deposits a
// decided application did not count and counted deposits that fell below the threshold, once each, and, unless paused,
// records the bond of each application waiting for it whose counted deposits cover it (xmrBondCredit).
func (a *App) xmrBondSettle(ctx context.Context, paused bool) error {
	threshold := a.escrowXMR.confirmations
	const who = `SELECT d.id,d.txid,d.amount,u.handle,va.state FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id
		JOIN vendor_applications va ON va.id=c.application_id JOIN users u ON u.id=va.user_id `
	type flag struct {
		id                  int64
		txid, handle, state string
		amount              int64
	}
	read := func(where string, args ...any) ([]flag, error) {
		rows, err := a.db.QueryContext(ctx, who+where+" ORDER BY d.id", args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []flag
		for rows.Next() {
			var f flag
			if err = rows.Scan(&f.id, &f.txid, &f.amount, &f.handle, &f.state); err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, rows.Err()
	}
	// mark sets column on deposit id once and tells the administrators body in the same transaction.
	mark := func(id int64, column, body string) error {
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, "UPDATE vendor_bond_deposits SET "+column+"=true,updated=now() WHERE id=$1 AND NOT "+column, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if err = notifyAdmins(ctx, tx, body); err != nil {
			return err
		}
		return tx.Commit()
	}
	locked, err := read("WHERE d.locked AND NOT d.flagged")
	if err != nil {
		return err
	}
	for _, f := range locked {
		if err = mark(f.id, "flagged", fmt.Sprintf("Vendor bond: deposit %s of %s XMR to the bond address of %s's vendor application has an unlock time. It is not counted toward the bond and is kept (the bond is non-refundable); record or waive the bond by hand if it should count.",
			truncate(f.txid, 20), amount(f.amount, 12), f.handle)); err != nil {
			return err
		}
	}
	// A deposit the application did not count and no longer can: it was decided (or its bond recorded) without it.
	uncounted, err := read("WHERE NOT d.credited AND NOT d.locked AND NOT d.flagged AND va.state<>'awaiting_bond'")
	if err != nil {
		return err
	}
	for _, f := range uncounted {
		if err = mark(f.id, "flagged", fmt.Sprintf("Vendor bond: deposit %s of %s XMR to the bond address of %s's vendor application was not counted toward the bond, because the application stopped waiting for its bond (now: %s) before the market counted it. It is kept, since the bond is non-refundable and the market sends nothing back, and it changes nothing about the application.",
			truncate(f.txid, 20), amount(f.amount, 12), f.handle, strings.ToLower(vendorStateLabel(f.state)))); err != nil {
			return err
		}
	}
	reorged, err := read("WHERE d.credited AND d.confirmations<$1 AND NOT d.reorg_flagged", threshold)
	if err != nil {
		return err
	}
	for _, f := range reorged {
		if err = mark(f.id, "reorg_flagged", fmt.Sprintf("Vendor bond: deposit %s of %s XMR, counted toward %s's vendor bond, now has fewer than %d confirmations or is no longer listed by the bond wallet (a chain reorganisation or a double spend). Nothing was changed: the application (now: %s) and the account's role stay as they are. Check the bond wallet and decide yourself.",
			truncate(f.txid, 20), amount(f.amount, 12), f.handle, threshold, strings.ToLower(vendorStateLabel(f.state)))); err != nil {
			return err
		}
	}
	if paused {
		return nil // under the restore gate or the sweep, no bond is recorded and nothing approved
	}
	apps, err := a.db.QueryContext(ctx, `SELECT va.id FROM vendor_applications va JOIN vendor_bond_accounts c ON c.application_id=va.id
		JOIN vendor_bond_deposits d ON d.account_id=c.id WHERE va.state='awaiting_bond' AND d.confirmations>=$1 AND NOT d.locked AND NOT d.late
		GROUP BY va.id,va.bond_piconero HAVING sum(d.amount)>=va.bond_piconero ORDER BY va.id`, threshold)
	if err != nil {
		return err
	}
	var due []string
	for apps.Next() {
		var id string
		if err = apps.Scan(&id); err != nil {
			apps.Close()
			return err
		}
		due = append(due, id)
	}
	apps.Close()
	if err = apps.Err(); err != nil {
		return err
	}
	var errs []error
	for _, id := range due {
		if err = a.xmrBondCredit(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// xmrBondCredit records the bond of application id as received on the Monero network, in one transaction after the
// applicant's row and then the application's are locked (the order every vendor form locks them in), when it still waits
// for its bond and its counted deposits cover it. With automatic approval an active buyer account is approved;
// otherwise the application waits for an administrator.
func (a *App) xmrBondCredit(ctx context.Context, id string) error {
	threshold := a.escrowXMR.confirmations
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var uid, role string
	var held bool
	err = tx.QueryRowContext(ctx, `SELECT u.id,u.role,u.suspended_at IS NOT NULL OR u.closed_at IS NOT NULL OR u.deletion_requested_at IS NOT NULL OR u.retention_restricted_at IS NOT NULL
		FROM users u JOIN vendor_applications a ON a.user_id=u.id WHERE a.id=$1 FOR NO KEY UPDATE OF u`, id).Scan(&uid, &role, &held)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var state, approval string
	var bond int64
	if err = tx.QueryRowContext(ctx, "SELECT state,approval,bond_piconero FROM vendor_applications WHERE id=$1 FOR UPDATE", id).Scan(&state, &approval, &bond); err != nil {
		return err
	}
	if state != "awaiting_bond" {
		return nil
	}
	const counted = ` FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id WHERE c.application_id=$1 AND d.confirmations>=$2 AND NOT d.locked AND NOT d.late`
	var sum int64
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(sum(d.amount),0),count(*)"+counted, id, threshold).Scan(&sum, &n); err != nil {
		return err
	}
	if n == 0 || sum < bond {
		return nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE vendor_bond_deposits SET credited=true,updated=now() WHERE id IN (SELECT d.id"+counted+")", id, threshold); err != nil {
		return err
	}
	next := "awaiting_approval"
	if approval == "automatic" && role == "buyer" && !held {
		next = "approved"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE vendor_applications SET bond_received_at=now(),bond_received_by=NULL,bond_received_how='xmr',state=$2,
		decided_at=CASE WHEN $2='approved' THEN now() END WHERE id=$1 AND state='awaiting_bond'`, id, next); err != nil {
		return err
	}
	received := fmt.Sprintf("%s XMR in %d deposit(s) with at least %d confirmations", amount(sum, 12), n, threshold)
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid, "Vendor bond of "+vendorBondText(bond)+" received on the Monero network: "+received); err != nil {
		return err
	}
	body := "Your vendor bond of " + vendorBondText(bond) + " was received on the Monero network (" + received + ")."
	if sum > bond {
		excess := amount(sum-bond, 12) + " XMR"
		body += " You paid " + excess + " more than the bond; it is kept, since the bond is a non-refundable fee and the market sends nothing back."
		var handle string
		if err = tx.QueryRowContext(ctx, "SELECT handle FROM users WHERE id=$1", uid).Scan(&handle); err != nil {
			return err
		}
		if err = notifyAdmins(ctx, tx, "Vendor bond: "+handle+" paid "+amount(sum, 12)+" XMR toward a bond of "+vendorBondText(bond)+"; the "+excess+" over the bond is kept in the bond wallet (non-refundable)."); err != nil {
			return err
		}
	}
	if next == "approved" {
		err = approveVendor(ctx, tx, uid, "automatic vendor approval", body+" ", "")
	} else {
		_, err = insertNotification(ctx, tx, uid, body+" Your application now waits for administrator approval.", "")
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: vendor bond of application %s received on the Monero network; application %s", shortID(id), strings.ReplaceAll(next, "_", " "))
	return nil
}

// xmrEpisodeAlert tells the administrators body once per episode while on holds, recorded by the setting key; the
// episode ends (the setting is removed) once on is false.
func (a *App) xmrEpisodeAlert(ctx context.Context, key string, on bool, body, logLine string) error {
	if !on {
		_, err := a.db.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", key)
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO NOTHING", key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err = notifyAdmins(ctx, tx, body); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Print(logLine)
	return nil
}

// fillVendorBond adds the automatic bond's figures to application v: its address (admin always; the applicant only
// while v waits for its bond and issuable), the XMR counted and still confirming, what remains, and for an
// administrator the flags. Nothing is shown while ESCROW_XMR is off.
func (a *App) fillVendorBond(ctx context.Context, v *VendorApplicationView, issuable, admin bool) error {
	x := a.escrowXMR
	if x == nil || !v.HasBond {
		return nil
	}
	v.BondAuto, v.BondThreshold = issuable && v.AwaitingBond, x.confirmations
	var addr string
	var bond, confirmed, pending, least, late, locked, reorg int64
	var deposits int
	err := a.db.QueryRowContext(ctx, `SELECT c.address,va.bond_piconero,
		COALESCE(sum(d.amount) FILTER (WHERE d.confirmations>=$2 AND NOT d.locked AND NOT d.late),0),
		COALESCE(sum(d.amount) FILTER (WHERE d.confirmations BETWEEN 0 AND $2-1 AND NOT d.locked AND NOT d.late),0),
		COALESCE(min(d.confirmations) FILTER (WHERE d.confirmations BETWEEN 0 AND $2-1 AND NOT d.locked AND NOT d.late),0),
		COALESCE(sum(d.amount) FILTER (WHERE d.late),0),count(d.id) FILTER (WHERE d.locked),count(d.id) FILTER (WHERE d.reorg_flagged),count(d.id)
		FROM vendor_bond_accounts c JOIN vendor_applications va ON va.id=c.application_id LEFT JOIN vendor_bond_deposits d ON d.account_id=c.id
		WHERE c.application_id=$1 GROUP BY c.address,va.bond_piconero`, v.ID, x.confirmations).Scan(&addr, &bond, &confirmed, &pending, &least, &late, &locked, &reorg, &deposits)
	if err == sql.ErrNoRows {
		v.BondPreparing = v.BondAuto
		return nil
	}
	if err != nil {
		return err
	}
	// An address is shown only when it parses for the network Monero escrow is verified on or pinned to.
	if _, perr := parseXMRAddress(addr, x.formNet()); perr == nil && (admin || v.BondAuto) {
		v.BondAddress = addr
	}
	v.HasDeposits = deposits > 0
	v.BondConfirmed, v.BondPending, v.BondPendingAny, v.BondLeastConfs = amount(confirmed, 12), amount(pending, 12), pending > 0, least
	v.BondRemaining = amount(max(bond-confirmed, 0), 12)
	if admin {
		if late > 0 {
			v.BondFlags = append(v.BondFlags, amount(late, 12)+" XMR arrived after the application stopped waiting for its bond: kept, not counted.")
		}
		if locked > 0 {
			v.BondFlags = append(v.BondFlags, strconv.FormatInt(locked, 10)+" deposit(s) with an unlock time: kept, not counted.")
		}
		if reorg > 0 {
			v.BondFlags = append(v.BondFlags, strconv.FormatInt(reorg, 10)+" counted deposit(s) later fell below "+strconv.FormatInt(x.confirmations, 10)+" confirmations: nothing was changed.")
		}
	}
	return nil
}
