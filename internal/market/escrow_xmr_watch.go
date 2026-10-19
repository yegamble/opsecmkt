package market

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The Monero escrow watcher (WR-escrow Phase 3, slice X-c; P3-R3, P3-R5; ES-7: one escrow wallet per month, one
// account per order). One per deployment (its own advisory lock, xmrEscrowWatcherLock), apart from the pooled and the
// Bitcoin escrow watchers, while ESCROW_XMR=on. Until Monero escrow is ready (escrow_xmr_config.go) a pass runs only
// the release deadlines (xmrDeadlines, no RPC), so a shipped order is never stranded while the node is down. It alone
// talks to the escrow wallet-rpc; checkout makes no RPC (escrow_xmr_create.go).
//
// Wallets (xmr_escrow_wallets). Each wallet is created as a row in state creating first, then create_wallet with the
// password derived from MONERO_WALLET_SECRET and the name escrow-YYYYMM-NN; its 25-word seed (query_key) is sealed to
// the pinned ESCROW_XMR_SEED_KEY and stored with the primary address and the restore height, and the row becomes
// sealed. Next month's wallet is created from xmrLeadTime before the month starts, so that a daily backup holds its
// seal before it takes an order; it becomes active at the month's start (UTC), when the active one becomes closing and
// its free slots are retired. On a first run, or after the market was down past a month's start, that month's wallet is
// created and activated at once. Wallet creation stops while the secret verifier or the seed key pin refuses.
//
// Crash safety. A row left creating is finished on the next pass: create_wallet again, and when the file exists
// (-21, the first call took effect) open_wallet and seal it; nothing is ever created twice. A file that already has a
// row's name is never taken as a new wallet unless the row was left creating and the file is provably as
// create_wallet leaves it (xmrWalletFresh): one that opens with the market's password otherwise belongs to orders a
// restored database lost, and it is sealed and retained as an orphan (xmrSealOrphan, slice X-f), never offering a
// slot; one that does not open is an orphan left alone (A-418). Either way the administrators are told and the month's
// next name is used.
//
// Each pass then works through every wallet that may hold coins (sealed, active, closing, retained, and missing ones
// in case they open again), one open at a time as monero-wallet-rpc requires: open_wallet (a definite refusal records
// the wallet missing, or destroyed again, escrow_xmr_retain.go); recheckEscrowXMRNetwork (it refreshes; the wallet is
// used only when it passes); the primary address must be the row's (another wallet is never read as this one);
// during the restore sweep, the lookahead of a wallet older than the database (escrow_xmr_restore.go); get_transfers in,
// pool, all_accounts; get_accounts; the slot work; the settlements; the destruction of a retained wallet that is due
// (escrow_xmr_retain.go); store.
//
// Slots (xmr_escrow_slots). The active wallet keeps ESCROW_XMR_SLOTS free slots, each made by create_account and
// inserted only once get_address of its address 0 matches what create_account returned. An account with no row (a
// create_account whose answer was lost) is checked the same way and added free, or retired when it has seen coins or
// its wallet is not active. A free slot that receives coins is retired (no order may start funded). Account 0 is never
// a slot and the watcher never spends: coins there alert the administrators once per episode.
//
// Funding (P3-R5). Deposits are aggregated per transaction and account into one payments row each, idx
// xmrPaymentIdx(wallet, account), for the order the slot names. A row the database refuses as another order's account
// (payments_xmr_escrow_idx) is flagged to staff for that row alone and the pass goes on. A deposit counts from
// escrowXMR.confirmations (at least 10) with no unlock time and no double_spend_seen; a locked one is flagged once and
// never counted. A digital order completes on funding through the escrow-only system edge (escrowTransitionHook); a
// physical one becomes paid with its ship-by time. An escrow that received nothing in its payment window is
// cancelled; one underpaid then is cancelled too, which requests its refund (slice X-d). A deposit after the order
// closed is recorded after_close and flagged (flagLateDeposits) for the stray refund; one that did not count when the
// order was cancelled and has confirmed since is left out for it (xmrLeaveOutUncounted, A-424).
//
// Settlements (slice X-d, escrow_xmr_settle.go) are built, relayed and followed in each wallet's pass, while it is open
// and checked.
//
// Under the restore gate, and after a restore until Monero escrow's sweep (xmrEscrowSweepKey, escrow_xmr_restore.go),
// no wallet or slot is created, no wallet activated and none destroyed; funding is still read. Each pass also keeps the
// wallets' retention dates (xmrRetain, escrow_xmr_retain.go).

// xmrMinConfirmations is the least number of confirmations at which a Monero escrow deposit counts (P3-R5): the
// 10-block lock of a received output, so a release can spend what funded it.
const xmrMinConfirmations = 10

// xmrReorgSafeDepth is the depth at which a Monero escrow deposit is no longer expected to move (the 100 confirmations a
// settlement is followed to, P3-R6). A deposit's stored confirmations stop there (xmrConfirmationsCeiling), so a row
// whose transaction is that deep is not rewritten, nor its order settled again, on every block.
const xmrReorgSafeDepth = 100

// xmrConfirmationsCeiling is the most confirmations stored for a Monero escrow deposit: xmrReorgSafeDepth, or the
// deposit threshold when PAYMENT_CONFIRMATIONS_XMR sets it higher. Nothing reads a deposit's confirmations against a
// number above it: funding and the payment window compare them with the threshold, and the order page says "or more"
// at the ceiling. A shallower reading (a reorganisation) is still stored.
func xmrConfirmationsCeiling(threshold int64) int64 { return max(xmrReorgSafeDepth, threshold) }

// xmrLeadTime: next month's wallet is created this long before the month starts (P3-R3, A-403).
const xmrLeadTime = 7 * 24 * time.Hour

// xmrEscrowSweepKey is the setting a restore writes (scripts/restore.sh, slice X-f; P3-R10): checkout and wallet and
// slot creation wait until Monero escrow's sweep has run in full, which clears it (escrow_xmr_restore.go).
const xmrEscrowSweepKey = "xmr_escrow_sweep_required"

// xmrEscrowPassTimeout bounds one Monero escrow pass. Every wallet that may hold coins is opened, refreshed and stored
// in turn: open_wallet and store may take up to xmrWalletSlowTimeout on a large wallet and a catch-up refresh up to
// xmrWalletRefreshTimeout, so the bound is twice the other watchers'; a pass cut short leaves the remaining wallets to
// the next one. A variable so tests can shorten it.
var xmrEscrowPassTimeout = 2 * watcherPassTimeout

// xmrEscrowCompletedNote is the order history note of a Monero escrow order's completion on funding.
const xmrEscrowCompletedNote = "Completed on funding (Monero escrow): the content was delivered. The vendor claims the escrowed coins from the order page."

// Fixed lines; nothing in them comes from a node.
var (
	errXMRWrongWallet      = errors.New("monero escrow: the open wallet's primary address is not the one recorded for the wallet asked for; it was not read")
	errXMRAccountMismatch  = errors.New("monero escrow: get_address does not return the account's address; no slot was written")
	errXMRWalletNotWritten = errors.New("monero escrow: the wallet row was not in state creating; its seal was not written")
)

const xmrLogUnrecorded = "escrow: Monero escrow disabled: its secret verifier or seed key could not be checked in the database"

func init() {
	registerBackground(xmrEscrowWatcher)
	registerLoader("admin", xmrEscrowNodeLoader)
}

// xmrEscrowWatcher runs while ESCROW_XMR=on. Until Monero escrow is ready (a node unreachable at startup, say) each
// pass first retries its startup checks (retryEscrowXMR), as the Bitcoin escrow watcher does (A-316).
func xmrEscrowWatcher(ctx context.Context, a *App) {
	if a.escrowXMR == nil {
		return
	}
	interval, err := pollInterval()
	if err != nil {
		interval = 30 * time.Second
	}
	retry := &escrowRetry{last: a.escrowXMR.startState}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.retryEscrowXMR(ctx, retry)
		pass, cancel := context.WithTimeout(ctx, xmrEscrowPassTimeout)
		err := a.xmrEscrowPollOnce(pass)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("monero escrow watcher: %s", watcherCause(err))
		}
		t.Reset(interval)
	}
}

// retryEscrowXMR runs startEscrowXMR again while Monero escrow is configured but not ready, logging its state line
// when it changes. A node that answers but fails the network check disables it until a restart (refuseEscrowXMR).
func (a *App) retryEscrowXMR(ctx context.Context, r *escrowRetry) {
	x := a.escrowXMR
	if x == nil || r.refused || x.ready.Load() {
		return
	}
	pass, cancel := context.WithTimeout(ctx, watcherPassTimeout)
	state, class, err := a.startEscrowXMR(pass)
	cancel()
	var refusal *xmrRefusal
	switch {
	case ctx.Err() != nil:
		return
	case errors.As(err, &refusal):
		x.refuseEscrowXMR(refusal)
		r.refused = true
		return
	case errors.Is(err, errXMRRefusedUntilRestart):
		r.refused = true
		return
	case err != nil:
		state, class = xmrLogUnrecorded, errorCause(err)
	}
	x.state.Store(&xmrState{state, class})
	if state != r.last {
		logEscrowState(state, class)
		if state == xmrLogOn && !x.canCreate.Load() {
			log.Print(xmrLogSeedKey)
		}
		r.last = state
	}
}

// clock is the watcher's time, in UTC.
func (x *escrowXMR) clock() time.Time {
	if x.now != nil {
		return x.now().UTC()
	}
	return time.Now().UTC()
}

// xmrPaymentIdx is the payments idx of a deposit to account of wallet (migration 084): 2^62 + wallet·2^32 + account.
func xmrPaymentIdx(wallet, account int64) int64 { return 1<<62 + wallet<<32 + account }

// xmrWalletRow is an escrow wallet as a pass reads it.
type xmrWalletRow struct {
	id                   int64
	name, state, primary string
}

// xmrSlotRow is one slot of the wallet being read; order is the order the slot names ("" unless claimed).
type xmrSlotRow struct {
	id                    int64
	state, address, order string
}

// xmrEscrowPollOnce runs one Monero escrow pass unless another process holds its lock (see the file comment).
func (a *App) xmrEscrowPollOnce(ctx context.Context) (outcome error) {
	x := a.escrowXMR
	if a.db == nil || x == nil {
		return nil
	}
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", xmrEscrowWatcherLock).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", xmrEscrowWatcherLock); err != nil {
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	providerSynced, providerUnread := false, false
	defer func() {
		a.noteProviderEpisode(ctx, "escrow_XMR", !x.ready.Load() || !providerSynced || providerUnread || outcome != nil)
	}()
	// Monero escrow configured but not ready (its node unreachable, or refused): no wallet is touched, but the deadlines,
	// which make no RPC, still apply, as escrowPollOnce applies Bitcoin escrow's when its node check fails (P3-R7).
	if !x.ready.Load() {
		return a.xmrDeadlines(ctx)
	}
	// The sync guard (A-449, monero_sync.go): while the daemon is not synced, or cannot be read, the pass applies only the
	// deadlines: no wallet is opened, created or destroyed, no deposit read, no order funded or expired, no settlement
	// built, relayed or followed. A daemon on another network than the one escrow started on disables it until a
	// restart. A daemon that is not synced is not an error: its state is logged once per change (noteXMREscrowSync).
	switch node, err := x.rpc.node(ctx); {
	case err != nil:
		return errors.Join(&watchError{label: "monero escrow", class: "daemon check failed: " + xmrErrorClass(err), err: err}, a.xmrDeadlines(ctx))
	case node.NetType != x.rpc.nettype():
		x.refuseEscrowXMR(errXMRNetworkMoved)
		return a.xmrDeadlines(ctx)
	default:
		why := node.notSynced(time.Now(), moneroMaxTipAge())
		a.noteXMREscrowSync(why)
		if why != "" {
			return a.xmrDeadlines(ctx)
		}
		providerSynced = true
	}
	var gate, sweep bool
	if err = a.db.QueryRowContext(ctx, "SELECT "+restoreGateSQL+",EXISTS(SELECT 1 FROM settings WHERE key=$1 AND value='true')", xmrEscrowSweepKey).Scan(&gate, &sweep); err != nil {
		return err
	}
	paused := gate || sweep
	now := x.clock()
	var errs []error
	if !paused {
		if err = a.xmrEnsureWallets(ctx, now); err != nil {
			errs = append(errs, &watchError{label: "monero escrow", class: "wallet creation failed: " + xmrErrorClass(err), err: err})
		}
		if err = a.xmrActivate(ctx, now); err != nil {
			errs = append(errs, err)
		}
	}
	if err = a.xmrRetain(ctx, now); err != nil {
		errs = append(errs, err)
	}
	// After a restore, the sweep (escrow_xmr_restore.go) is complete only when the probe and every wallet's check passed.
	swept := true
	if sweep {
		if err = a.xmrSweepProbe(ctx, now); err != nil {
			swept = false
			errs = append(errs, &watchError{label: "monero escrow", class: "restore sweep incomplete: " + xmrErrorClass(err), err: err})
		}
	}
	wallets, err := a.xmrWallets(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	read := map[int64]bool{}
	touched := map[string]bool{}
	for _, w := range wallets {
		if ctx.Err() != nil || x.refused.Load() {
			swept = false
			break
		}
		err := x.rpc.openWallet(ctx, w.name, xmrWalletPassword(x.secret, w.name))
		switch {
		case err == nil:
			x.locked.Delete(w.name)
			var checked bool
			checked, err = a.xmrWalletPass(ctx, w, now, paused, sweep, touched)
			swept, read[w.id] = swept && checked, err == nil
			providerUnread = providerUnread || !checked
		case xmrOpenRefusal(err) == xmrOpenLocked:
			providerUnread = true
			// Another wallet program has its file open (an operator inspecting it, say): it waits, keeps its state and its
			// slots, and is tried again on the next pass (A-421). Logged once while it lasts.
			swept, err = false, nil
			if _, logged := x.locked.LoadOrStore(w.name, true); !logged {
				log.Printf("escrow: Monero escrow wallet %s is opened by another wallet program; not read until it is closed there", w.name)
			}
		case xmrWalletDenied(err):
			providerUnread = true
			// It did not open: recorded missing, or destroyed again on its own wrong-password refusal outside a restore when
			// the market itself began destroying it (escrow_xmr_retain.go); checked, but never read.
			if err = a.xmrWalletUnopenable(ctx, w, now, xmrOpenRefusal(err) == xmrOpenPassword && !paused); err != nil {
				swept = false
			}
		default:
			swept = false
			providerUnread = true
		}
		if err != nil {
			errs = append(errs, &watchError{label: "monero escrow wallet " + w.name, class: xmrErrorClass(err), err: err})
		}
	}
	if sweep && swept {
		if _, err = a.db.ExecContext(ctx, "DELETE FROM settings WHERE key=$1", xmrEscrowSweepKey); err != nil {
			errs = append(errs, err)
		} else {
			log.Print("escrow: Monero escrow's restore sweep is complete; Monero escrow checkout is open again once the restore gate is cleared")
		}
	}
	if err = a.xmrSettleOrders(ctx, read, touched); err != nil {
		errs = append(errs, err)
	}
	if err = a.xmrDeadlines(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// noteXMREscrowSync records why (a moneroNode reason class, "" when synced) as the Monero escrow daemon's sync state for
// the admin page and logs one fixed line when it changes. In memory only: nothing is stored for the guard (A-449).
func (a *App) noteXMREscrowSync(why string) {
	prev, _ := a.escrowXMR.sync.Swap(why).(string)
	switch {
	case prev == why:
	case why == "":
		log.Print("escrow: Monero escrow daemon synced; Monero escrow watcher passes resumed")
	default:
		log.Printf("escrow: Monero escrow daemon not synced (%s); Monero escrow watcher passes skipped until it is", why)
	}
}

// xmrEscrowNodeLoader shows on /admin, under Payment providers, why the Monero escrow daemon is not used: the pinned
// network and the daemon's when startEscrowXMR refused it (A-449), any other state that leaves Monero escrow configured
// but not ready, with what to do (notReady, QA4W4B-1), or, once ready, the reason class it is not synced for; and in
// APP_MODE=tor why Monero relays are held (xmrRelayHold, A-515).
func xmrEscrowNodeLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	x := a.escrowXMR
	if x == nil || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &EscrowNodeView{Mismatch: x.mismatch.Load()}
	if x.ready.Load() {
		v.NotSynced, _ = x.sync.Load().(string)
	} else {
		v.NotReady, v.Remedy = x.notReady()
	}
	v.TorHold = a.xmrRelayHold()
	if v.NotSynced != "" || v.Mismatch != nil || v.NotReady != "" || v.TorHold != "" {
		d.XMREscrowNode = v
	}
	return nil
}

// monthStart is the first instant of t's month, UTC.
func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// xmrEnsureWallets finishes any wallet left creating, then creates this month's wallet when none is creating, sealed
// or active (a first run, or a market down past the month's start), and next month's from xmrLeadTime before it.
// Nothing is created unless the secret verifier and the seed key pin still match (P3-R4).
func (a *App) xmrEnsureWallets(ctx context.Context, now time.Time) error {
	x := a.escrowXMR
	secretOK, err := pinXMRSetting(ctx, a.db, xmrVerifierSetting, xmrSecretVerifier(x.secret))
	if err != nil {
		return err
	}
	keyOK, err := pinXMRSetting(ctx, a.db, xmrSeedKeySetting, x.seedKey.fpr)
	if err != nil {
		return err
	}
	x.canCreate.Store(keyOK)
	if !secretOK || !keyOK {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, "SELECT id,name FROM xmr_escrow_wallets WHERE state='creating' ORDER BY id")
	if err != nil {
		return err
	}
	var pending []xmrWalletRow
	for rows.Next() {
		var w xmrWalletRow
		if err = rows.Scan(&w.id, &w.name); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, w)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, w := range pending {
		if err = a.xmrCreateWallet(ctx, w.id, w.name, true); err != nil {
			return err
		}
	}
	cur := monthStart(now)
	months := []time.Time{cur}
	if next := cur.AddDate(0, 1, 0); !now.Before(next.Add(-xmrLeadTime)) {
		months = append(months, next)
	}
	for _, m := range months {
		month := m.Format("2006-01-02")
		for {
			var have bool
			if err = a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xmr_escrow_wallets WHERE month=$1::date AND state IN ('creating','sealed','active'))", month).Scan(&have); err != nil {
				return err
			}
			if have {
				break
			}
			// The month's next number: an orphan row keeps its name, and the next wallet takes the one after it.
			var id int64
			var name string
			if err = a.db.QueryRowContext(ctx, `WITH allocation AS (
				INSERT INTO retention_xmr_wallet_names(month,next_n) VALUES($1::date,2)
				ON CONFLICT(month) DO UPDATE SET next_n=retention_xmr_wallet_names.next_n+1 WHERE retention_xmr_wallet_names.next_n<=99 RETURNING next_n-1 AS n)
				INSERT INTO xmr_escrow_wallets(name,month,state)
				SELECT 'escrow-'||to_char($1::date,'YYYYMM')||'-'||lpad(n::text,2,'0'),$1::date,'creating' FROM allocation RETURNING id,name`, month).Scan(&id, &name); err != nil {
				return err
			}
			if err = a.xmrCreateWallet(ctx, id, name, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// xmrCreateWallet creates wallet name for row id and seals it. When a file of that name already exists (-21) it is
// opened with the derived password. resume is a row found creating at the start of the pass: the file may be the one
// its earlier create_wallet made (the answer was lost), but a database restored from an older backup can also leave a
// creating row whose name a wallet of lost orders has, opened by the same derived password. So the file is taken as the
// row's new wallet only when the row is resumed and the file is provably as create_wallet leaves a wallet
// (xmrWalletFresh): its unfunded accounts would otherwise become free slots for new buyers. A file that opens but is
// not, on a new row or a resumed one, belongs to orders this database lost: it is sealed and retained as an orphan,
// never offering a slot (xmrSealOrphan, P3-R10). A file that open_wallet refuses (a definite refusal: a file left
// under another password, say) is an orphan left alone, and the administrators are told once (A-418); a transport
// error is retried. Either way the month's next name is used.
func (a *App) xmrCreateWallet(ctx context.Context, id int64, name string, resume bool) error {
	x := a.escrowXMR
	password := xmrWalletPassword(x.secret, name)
	err := x.rpc.createWallet(ctx, name, password)
	var re *xmrRPCError
	if errors.As(err, &re) && re.Code == -21 { // WALLET_ALREADY_EXISTS
		if err = x.rpc.openWallet(ctx, name, password); xmrWalletDenied(err) && xmrOpenRefusal(err) != xmrOpenLocked {
			return a.xmrOrphanWallet(ctx, id, name)
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
			return a.xmrSealOrphan(ctx, id, name)
		}
	} else if err != nil {
		return err
	}
	// An escrow address is always an account's address 0, so the wallet looks one address ahead in each account rather
	// than wallet2's 200; monero-wallet-rpc writes it into the keys file, and every account created later gets it. Set
	// while the wallet has account 0 alone and before the row is sealed, so a pass cut short sets it again. PAYE-E2's
	// variant (2,000 accounts, 200 deposits over 110 blocks, 0.18.5.1 fakechain) measured the catch-up refresh at 0.70 s
	// with it, against 21 s without, and create_account 7 ms against 54 ms (median).
	if err = x.rpc.setSubaddressLookahead(ctx, password, xmrLookaheadMajor, 1); err != nil {
		return err
	}
	return a.xmrSealWallet(ctx, id)
}

// xmrWalletFresh reports whether the open wallet is as create_wallet leaves it: account 0 alone, a zero balance, and
// no transfer in, out, pending, failed or in the pool on any account.
func xmrWalletFresh(ctx context.Context, r *xmrEscrowRPC) (bool, error) {
	accounts, err := r.accounts(ctx)
	if err != nil {
		return false, err
	}
	if len(accounts) != 1 || accounts[0].Index != 0 || accounts[0].Balance != 0 || accounts[0].Unlocked != 0 {
		return false, nil
	}
	var res map[string][]json.RawMessage
	if err = r.walletCall(ctx, "get_transfers", map[string]any{"in": true, "out": true, "pending": true, "failed": true, "pool": true, "all_accounts": true}, &res); err != nil {
		return false, err
	}
	for _, list := range res {
		if len(list) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// xmrOrphanWallet marks row id orphan: its name belongs to a wallet file that does not open with the market's password,
// which the market never opens again.
func (a *App) xmrOrphanWallet(ctx context.Context, id int64, name string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='orphan',updated=now() WHERE id=$1 AND state='creating'", id); err != nil {
		return err
	}
	if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+name+" already exists on the escrow wallet-rpc and does not open with the market's password (a file left by another install?). The market did not use it and created the month's next wallet instead; check that wallet yourself."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow wallet %s exists and does not open with the market's password; marked orphan, not used", name)
	return nil
}

// xmrSealWallet seals the open wallet (just created or reopened) to the seed key and records it on row id: the network
// re-check, then the primary address, seed and height, then the row, then store.
func (a *App) xmrSealWallet(ctx context.Context, id int64) error {
	x := a.escrowXMR
	if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
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
	res, err := a.db.ExecContext(ctx, `UPDATE xmr_escrow_wallets SET primary_address=$2,restore_height=$3,seed_sealed=$4,seed_key_fpr=$5,sealed_at=now(),
		state='sealed',updated=now() WHERE id=$1 AND state='creating'`, id, primary, height, sealed, fpr)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errXMRWalletNotWritten
	}
	return x.rpc.store(ctx)
}

// xmrActivate makes the newest sealed wallet whose month has started active, and every other active wallet (and any
// older sealed one) closing with its free slots retired, in one transaction. An active wallet stays active until a newer
// one is sealed.
func (a *App) xmrActivate(ctx context.Context, now time.Time) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM xmr_escrow_wallets WHERE state='sealed' AND month <= $1::date
		AND month >= COALESCE((SELECT max(month) FROM xmr_escrow_wallets WHERE state='active'), month)
		ORDER BY month DESC, name DESC LIMIT 1 FOR UPDATE`, monthStart(now).Format("2006-01-02")).Scan(&id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	// An older sealed wallet that was never activated (the market was down through its month) closes too, so it is not
	// left sealed and read on every pass for good.
	if _, err = tx.ExecContext(ctx, `WITH closed AS (UPDATE xmr_escrow_wallets SET state='closing',updated=now()
			WHERE id<>$1 AND (state='active' OR state='sealed' AND month <= (SELECT month FROM xmr_escrow_wallets WHERE id=$1)) RETURNING id)
		UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE state='free' AND wallet_id IN (SELECT id FROM closed)`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='active',updated=now() WHERE id=$1", id); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrWallets lists the wallets that may hold coins, missing ones included (they are tried again), oldest first.
func (a *App) xmrWallets(ctx context.Context) ([]xmrWalletRow, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,name,state,primary_address FROM xmr_escrow_wallets WHERE state IN ('sealed','active','closing','retained','missing') ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []xmrWalletRow
	for rows.Next() {
		var w xmrWalletRow
		if err = rows.Scan(&w.id, &w.name, &w.state, &w.primary); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// xmrWalletPass does the work of wallet w, which is open (see the file comment); touched collects the orders whose
// ledger changed. paused: the restore gate or the sweep is set, so no slot is made free and nothing is destroyed. sweep:
// the restore sweep is running, so a wallet older than the database gets its lookahead. checked reports that the wallet
// passed its checks and was read and reconciled, as the sweep requires.
func (a *App) xmrWalletPass(ctx context.Context, w xmrWalletRow, now time.Time, paused, sweep bool, touched map[string]bool) (checked bool, err error) {
	x := a.escrowXMR
	if err := a.recheckEscrowXMRNetwork(ctx); err != nil {
		return false, err
	}
	if primary, err := x.rpc.primaryAddress(ctx); err != nil {
		return false, err
	} else if primary != w.primary {
		return false, errXMRWrongWallet
	}
	if w.state == "missing" {
		// It opens again: closing, so it takes no order, and read as any closing wallet; retained when the market had begun
		// destroying it (migration 086), so its destruction re-runs every precondition (escrow_xmr_retain.go).
		if err := a.db.QueryRowContext(ctx, "UPDATE xmr_escrow_wallets SET state=CASE WHEN destroy_started_at IS NULL THEN 'closing' ELSE 'retained' END,updated=now() WHERE id=$1 AND state='missing' RETURNING state", w.id).Scan(&w.state); err != nil {
			return false, err
		}
		log.Printf("escrow: Monero escrow wallet %s opens again; %s", w.name, w.state)
	}
	if sweep {
		if err := a.xmrLookahead(ctx, w); err != nil {
			return false, err
		}
	}
	// The slots are read before the wallet: a deposit is recorded for the order its slot named when the pass began.
	slots := map[int64]*xmrSlotRow{}
	rows, err := a.db.QueryContext(ctx, "SELECT id,account_index,state,address,COALESCE(order_id,'') FROM xmr_escrow_slots WHERE wallet_id=$1", w.id)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var s xmrSlotRow
		var account int64
		if err = rows.Scan(&s.id, &account, &s.state, &s.address, &s.order); err != nil {
			rows.Close()
			return false, err
		}
		slots[account] = &s
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	transfers, err := x.rpc.transfers(ctx)
	if err != nil {
		return false, err
	}
	funded, err := a.xmrRecordTransfers(ctx, w, slots, transfers, touched)
	if err != nil {
		return false, err
	}
	accounts, err := x.rpc.accounts(ctx)
	if err != nil {
		return false, err
	}
	for _, acc := range accounts {
		if acc.Balance > 0 {
			funded[acc.Index] = true
		}
	}
	if err = a.xmrAccountZero(ctx, w, funded[0]); err != nil {
		return false, err
	}
	if err = a.xmrReconcile(ctx, w, slots, accounts, funded, paused); err != nil {
		return false, err
	}
	// Settlements are built, relayed and followed only here, while this wallet is open and checked
	// (escrow_xmr_settle.go). The wallet is stored after them whatever they found.
	settled := a.xmrSettleWallet(ctx, w)
	switch {
	case w.state == "active" && !paused:
		if err = a.xmrTopUpSlots(ctx, w); err != nil {
			return true, errors.Join(settled, err)
		}
	case w.state == "retained" && !paused && settled == nil:
		// Destroyed, it is closed: nothing to store.
		if destroyed, err := a.xmrDestroyIfDue(ctx, w, now); err != nil || destroyed {
			return true, err
		}
	}
	return true, errors.Join(settled, x.rpc.store(ctx))
}

// xmrRecordTransfers writes the wallet's incoming transfers to the payments ledger, one row per transaction and claimed
// account (the outputs of one transaction to several addresses of an account are summed; a mined entry replaces its pool
// entry), and marks the account rows the wallet no longer reports -1. It returns the accounts that have seen coins.
func (a *App) xmrRecordTransfers(ctx context.Context, w xmrWalletRow, slots map[int64]*xmrSlotRow, transfers []xmrTransfer, touched map[string]bool) (map[int64]bool, error) {
	type key struct {
		txid    string
		account int64
	}
	sums := map[key]*Incoming{}
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
				continue // listed as mined too: the mined entry counts
			}
			mined[k] = !pool
			confs := t.Confirmations
			if pool {
				confs = 0
			}
			if t.DoubleSpendSeen {
				confs = -1 // as the pooled adapter reads it: conflicted, never counted
			}
			confs = min(confs, ceiling)
			in := sums[k]
			if in == nil {
				in = &Incoming{TxID: t.TxID, Index: xmrPaymentIdx(w.id, t.Account), Confirmations: confs}
				sums[k] = in
				keys = append(keys, k)
			} else {
				in.Confirmations = min(in.Confirmations, confs)
			}
			in.Amount += t.Amount
			in.Locked = in.Locked || t.Locked
		}
	}
	slices.SortFunc(keys, func(p, q key) int { return cmp.Or(cmp.Compare(p.account, q.account), strings.Compare(p.txid, q.txid)) })
	seen := map[ledgerKey]bool{}
	var addresses []string
	for _, s := range slots {
		if s.order != "" {
			addresses = append(addresses, s.address)
		}
	}
	for _, k := range keys {
		s := slots[k.account]
		if s == nil || s.order == "" {
			continue // account 0, a free or retired slot, or an account with no row: see xmrAccountZero and xmrReconcile
		}
		in := *sums[k]
		in.Address = s.address
		seen[ledgerKey{in.TxID, in.Index}] = true
		changed, err := a.upsertIncoming(ctx, "XMR", s.order, in)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.ConstraintName == "payments_xmr_escrow_idx" {
			if err = a.xmrFlagCollision(ctx, w, k.account, s.order, in.TxID); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if changed {
			touched[s.order] = true
		}
	}
	if err := a.markMissing(ctx, "XMR", addresses, seen); err != nil {
		return nil, err
	}
	return funded, a.xmrMissingHeights(ctx, addresses)
}

// xmrMissingHeights records the open wallet's height on each deposit at addresses credited toward its order's funding
// that is at -1 (no longer listed, or read as double-spent) with no height yet: from it the order's release or refund
// waits a bounded number of blocks (xmrCreditedHold, A-554). A deposit listed again loses its height, so a later
// disappearance starts a new wait. The wallet is asked only when such a deposit exists.
func (a *App) xmrMissingHeights(ctx context.Context, addresses []string) error {
	if _, err := a.db.ExecContext(ctx, "UPDATE payments SET missing_height=NULL WHERE currency='XMR' AND address=ANY($1) AND confirmations>=0 AND missing_height IS NOT NULL", addresses); err != nil {
		return err
	}
	var due bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE currency='XMR' AND address=ANY($1) AND credited AND confirmations<0 AND missing_height IS NULL)", addresses).Scan(&due); err != nil || !due {
		return err
	}
	height, err := a.escrowXMR.rpc.height(ctx)
	if err != nil {
		return err
	}
	_, err = a.db.ExecContext(ctx, "UPDATE payments SET missing_height=$2 WHERE currency='XMR' AND address=ANY($1) AND credited AND confirmations<0 AND missing_height IS NULL", addresses, height)
	return err
}

// xmrFlagCollision flags a deposit the database refused as another order's escrow account (payments_xmr_escrow_idx):
// the order the watcher read it for gets a record (xmr_escrow_collisions, A-480), a fixed note and the staff a
// notification, and the operator a fixed log line, once per order and transaction: the record decides it, not the note
// text (a deposit met again on a later pass finds its record and writes nothing, whatever its disposition). A record
// migration 090 backfilled from an earlier note holds the transaction ID's first 20 characters only; the same deposit
// met again completes it and writes nothing else. While the record is open, the order has an escrow staff alert
// (escrowStaffAlert). Nothing is written to the ledger for it.
func (a *App) xmrFlagCollision(ctx context.Context, w xmrWalletRow, account int64, orderID, txid string) error {
	tx, err := a.beginEscrowOrderTx(ctx, orderID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err != nil {
		return err
	}
	if len(txid) > 20 {
		res, err := tx.ExecContext(ctx, "UPDATE xmr_escrow_collisions SET txid=$3,txid_truncated=false WHERE order_id=$1 AND txid_truncated AND txid=$2", orderID, txid[:20], txid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return tx.Commit()
		}
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid) VALUES($1,$2,$3,$4) ON CONFLICT (order_id,txid) DO NOTHING", orderID, w.name, account, txid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	note := fmt.Sprintf("%s %s Monero escrow deposit %s to wallet %s account %d was not recorded: the database refused it as another order's escrow account. Moderator review required.",
		a.netTag(), a.xmrEscrowNetwork(), truncate(txid, 20), w.name, account)
	if err = a.flagOrder(ctx, tx, &o, note, ""); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: a Monero escrow deposit to wallet %s account %d was refused by payments_xmr_escrow_idx (another order's account); flagged for staff on order %s", w.name, account, shortID(orderID))
	return nil
}

// xmrAccountZeroKey is the setting that marks an open episode of coins at account 0 of wallet id.
func xmrAccountZeroKey(id int64) string { return "escrow_xmr_account0_" + strconv.FormatInt(id, 10) }

// xmrAccountZero alerts the administrators once per episode while account 0 of wallet w holds or receives coins
// (funded): it is never an escrow and the market never spends from it. The episode ends when it is empty again.
func (a *App) xmrAccountZero(ctx context.Context, w xmrWalletRow, funded bool) error {
	key := xmrAccountZeroKey(w.id)
	if !funded {
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
	if err = notifyAdmins(ctx, tx, "Monero escrow wallet "+w.name+": coins arrived at its primary account (account 0), which is never an escrow account and which the market never spends. Check where they came from; they stay there."); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: coins at account 0 of Monero escrow wallet %s; administrators notified", w.name)
	return nil
}

// xmrReconcile matches the wallet's accounts with its slot rows. An account with no row is checked against get_address
// and added retired, never free: create_account's answer may have been lost, or the database restored to an earlier
// point without scripts/restore.sh, so that the wallet made it, and perhaps gave it and some slots the records show
// free to buyers, after that point (A-556). On the active wallet while nothing is paused, every free slot is therefore
// retired with it in one transaction and the administrators are told once (xmrRetireRowless); the top-up makes fresh
// accounts. A free slot that has seen coins is retired, so no order starts funded. Account 0 is never a slot.
func (a *App) xmrReconcile(ctx context.Context, w xmrWalletRow, slots map[int64]*xmrSlotRow, accounts []xmrAccount, funded map[int64]bool, paused bool) error {
	x := a.escrowXMR
	var rowless []xmrAccount
	for _, acc := range accounts {
		if acc.Index == 0 {
			continue
		}
		if s := slots[acc.Index]; s != nil {
			if s.state != "free" || !funded[acc.Index] {
				continue
			}
			res, err := a.db.ExecContext(ctx, "UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE id=$1 AND state='free'", s.id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				s.state = "retired"
				if err = a.xmrFundedAccountAlert(ctx, w, acc.Index); err != nil {
					return err
				}
			}
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
	if len(rowless) == 0 {
		return nil
	}
	if err := a.xmrRetireRowless(ctx, w, slots, rowless, w.state == "active" && !paused); err != nil {
		return err
	}
	for _, acc := range rowless {
		if funded[acc.Index] {
			if err := a.xmrFundedAccountAlert(ctx, w, acc.Index); err != nil {
				return err
			}
		}
	}
	return nil
}

// xmrRetireRowless adds the accounts of wallet w that have no row as retired slots, in one transaction. With burn (the
// active wallet, nothing paused) it also retires every free slot of w, by compare-and-set against checkout's claim, and
// tells the administrators with a notification and an audit row (the rows added make it once per discovery). Under the
// restore gate or the sweep, restore.sh has already retired the free slots, and a wallet that is not active offers none.
// The watcher's advisory lock serialises it with the top-up; slots is the pass's copy, discarded if this fails.
func (a *App) xmrRetireRowless(ctx context.Context, w xmrWalletRow, slots map[int64]*xmrSlotRow, rowless []xmrAccount, burn bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var retired int64
	if burn {
		res, err := tx.ExecContext(ctx, "UPDATE xmr_escrow_slots SET state='retired',updated=now() WHERE wallet_id=$1 AND state='free'", w.id)
		if err != nil {
			return err
		}
		if retired, err = res.RowsAffected(); err != nil {
			return err
		}
	}
	for _, s := range slots {
		if burn && s.state == "free" {
			s.state = "retired"
		}
	}
	for _, acc := range rowless {
		var id int64
		if err = tx.QueryRowContext(ctx, "INSERT INTO xmr_escrow_slots(wallet_id,account_index,address,state) VALUES($1,$2,$3,'retired') RETURNING id", w.id, acc.Index, acc.BaseAddress).Scan(&id); err != nil {
			return err
		}
		slots[acc.Index] = &xmrSlotRow{id: id, state: "retired", address: acc.BaseAddress}
	}
	if burn {
		if err = notifyAdmins(ctx, tx, fmt.Sprintf("Monero escrow wallet %s holds %d account(s) the market has no record of: a wallet answer lost in a crash, or a database restored to an earlier point without scripts/restore.sh. "+
			"So that no account the wallet may already have given to a buyer is offered again, the market retired them and the wallet's %d free account(s); new accounts are created on the next pass. "+
			"If the database was restored to an earlier point, follow UPGRADING.md section 6 now: orders, deposits and settlements made after that point are unknown to it.", w.name, len(rowless), retired)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)", fmt.Sprintf("Monero escrow wallet %s: retired %d free account(s) and %d account(s) the records lacked", w.name, retired, len(rowless))); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if burn {
		log.Printf("escrow: Monero escrow wallet %s holds %d account(s) with no slot row; retired them and its %d free slot(s), administrators notified", w.name, len(rowless), retired)
	}
	return nil
}

// xmrFundedAccountAlert tells the administrators about coins at an account no order claimed (now retired).
func (a *App) xmrFundedAccountAlert(ctx context.Context, w xmrWalletRow, account int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = notifyAdmins(ctx, tx, fmt.Sprintf("Monero escrow wallet %s: account %d, which no order had claimed, has received coins. It was retired, so no order uses it; the coins stay there.", w.name, account)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: coins at unclaimed account %d of Monero escrow wallet %s; retired, administrators notified", account, w.name)
	return nil
}

// xmrTopUpSlots keeps ESCROW_XMR_SLOTS free slots on the active wallet w: create_account, then get_address of the new
// account's address 0 must be the address create_account returned before the row is written.
func (a *App) xmrTopUpSlots(ctx context.Context, w xmrWalletRow) error {
	x := a.escrowXMR
	var free int
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM xmr_escrow_slots WHERE wallet_id=$1 AND state='free'", w.id).Scan(&free); err != nil {
		return err
	}
	for ; free < x.slots; free++ {
		account, addr, err := x.rpc.createAccount(ctx)
		if err != nil {
			return err
		}
		got, err := x.rpc.address(ctx, account, 0)
		if err != nil {
			return err
		}
		if got != addr {
			return errXMRAccountMismatch
		}
		if _, err = a.db.ExecContext(ctx, "INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) VALUES($1,$2,$3)", w.id, account, addr); err != nil {
			return err
		}
	}
	return nil
}

// notifyAdmins notifies every administrator who is not suspended.
func notifyAdmins(ctx context.Context, tx *sql.Tx, body string) error {
	admins, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL")
	if err != nil {
		return err
	}
	for _, uid := range admins {
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body); err != nil {
			return err
		}
	}
	return nil
}

// xmrSettleOrders applies the ledger to every open Monero escrow order and to each order whose ledger changed in this
// pass. read holds the wallets read in this pass; an order of a wallet not read never expires in it.
func (a *App) xmrSettleOrders(ctx context.Context, read map[int64]bool, touched map[string]bool) error {
	changed := []string{}
	for id := range touched {
		changed = append(changed, id)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT es.order_id,s.wallet_id FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN orders o ON o.id=es.order_id
		WHERE es.coin='XMR' AND (o.state IN ('awaiting_payment',`+fundedOpenStates+`) OR es.order_id=ANY($1)
			OR EXISTS (SELECT 1 FROM payments p WHERE p.order_id=es.order_id AND p.after_close AND NOT p.flagged AND p.xmr_settlement_id IS NOT NULL)) ORDER BY o.updated`, changed)
	if err != nil {
		return err
	}
	type target struct {
		order  string
		wallet int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.order, &t.wallet); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		if err = a.settleXMREscrow(ctx, t.order, read[t.wallet]); err != nil {
			errs = append(errs, &watchError{label: "monero escrow order " + shortID(t.order), err: err})
		}
	}
	return errors.Join(errs...)
}

// settleXMREscrow applies the ledger to one Monero escrow order in its own transaction (settleEscrow's steps): a
// locked deposit is flagged once; a closed order's late deposits are flagged, and a cancelled one's deposits that
// confirmed only after it was cancelled left out; an order awaiting payment is funded,
// or, at the end of its payment window, cancelled when nothing came or noted once when it is underpaid.
func (a *App) settleXMREscrow(ctx context.Context, orderID string, walletRead bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 FOR UPDATE OF o", orderID))
	if err != nil {
		return err
	}
	if !o.Escrow || o.Currency != "XMR" {
		return tx.Commit()
	}
	threshold := a.escrowXMR.confirmations
	if err = a.flagLockedXMR(ctx, tx, &o); err != nil {
		return err
	}
	if o.State != stateAwaitingPayment {
		// A-374, P3-R8: a deposit no settlement spends, once the order's release or refund is built, is left out for the
		// stray refund (escrow_xmr_settle.go).
		if err = a.xmrLeaveOutTopUps(ctx, tx, &o); err != nil {
			return err
		}
	}
	if isTerminal(o.State) {
		// A-424: a deposit that did not count when the order was cancelled and has confirmed since is left out for the
		// stray refund too.
		if err = a.xmrLeaveOutUncounted(ctx, tx, &o, threshold); err != nil {
			return err
		}
		// A deposit first recorded after the order closed is never counted; it is flagged for the stray refund (P3-R8).
		if err = a.flagLateDeposits(ctx, tx, &o, threshold); err != nil {
			return err
		}
		return tx.Commit()
	}
	if o.State != stateAwaitingPayment {
		return tx.Commit()
	}
	var total, sum int64
	var n int
	if err = tx.QueryRowContext(ctx, "SELECT amount FROM orders WHERE id=$1", orderID).Scan(&total); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(sum(amount),0),count(*) FROM payments WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold).Scan(&sum, &n); err != nil {
		return err
	}
	if n > 0 && sum >= total {
		if _, err = tx.ExecContext(ctx, "UPDATE payments SET credited=true WHERE order_id=$1 AND confirmations>=$2 AND NOT locked", orderID, threshold); err != nil {
			return err
		}
		note := fmt.Sprintf("%s %s escrow funded: %s XMR in %d deposit(s) with at least %d confirmations", a.netTag(), a.xmrEscrowNetwork(), amount(sum, 12), n, threshold)
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, statePaid, nil, note); err != nil {
			return err
		}
		if sum > total {
			// What happens to the excess is decided by the settlement (P3-R6); it is noted, never paid out here.
			excess := priceDifference(sum, total, "XMR") + " was paid into this escrow; the release to the vendor returns it to the buyer's refund address (or adds it to the vendor's output when it is below " + amount(payoutFloor("XMR", "release"), 12) + " XMR)."
			if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", orderID, excess); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if !walletRead {
		return tx.Commit()
	}
	// The ordinary payment window and fixed first-recorded-deposit confirmation wait use the same inputs
	// as a Bitcoin escrow's (settleEscrow).
	var unpaid, underpaid, stalled bool
	var received int64
	if err = tx.QueryRowContext(ctx, `WITH d AS (SELECT amount,confirmations,created FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close AND NOT escrow_left_out AND NOT spent_outside),
		h AS (SELECT min(created) AS first_seen FROM payments WHERE order_id=$1 AND NOT locked AND NOT after_close)
		SELECT es.created < now()-make_interval(secs => $2) AND NOT EXISTS (SELECT 1 FROM d),
			es.created < now()-make_interval(secs => $2) AND EXISTS (SELECT 1 FROM d) AND NOT EXISTS (SELECT 1 FROM d WHERE confirmations<$3)
				AND (SELECT max(created) FROM d) < now()-make_interval(secs => $2),
			EXISTS (SELECT 1 FROM d WHERE confirmations<$3) AND GREATEST(es.created+make_interval(secs => $2),
				(SELECT first_seen FROM h)+make_interval(secs => $4)) <= now(),
			(SELECT COALESCE(sum(amount),0) FROM d)
		FROM escrows es WHERE es.order_id=$1 AND es.coin='XMR'`, orderID, expiryWindow().Seconds(), threshold, confirmationWait().Seconds()).Scan(&unpaid, &underpaid, &stalled, &received); err != nil {
		return err
	}
	switch {
	case unpaid:
		// Nothing received: cancelled as a pooled order is; the stock hook returns the unit, and nothing is refunded.
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired"); err != nil {
			return err
		}
	case stalled:
		// The Monero settlement may be requested, but its builder freezes any
		// unconfirmed output and never sends it before the threshold.
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment confirmation wait ended; unconfirmed deposits were not counted"); err != nil {
			return err
		}
		body := "Order " + shortID(orderID) + ": the fixed confirmation wait ended, so the order was cancelled and its stock returned. Unconfirmed escrow deposits were not sent; deposits that confirm later follow the escrow refund or stray-recovery path shown on the order page."
		if _, err = insertNotification(ctx, tx, o.BuyerID, body, o.ID); err != nil {
			return err
		}
	case underpaid:
		// Every deposit confirmed, short of the price, and no top-up for a whole window: cancelled as a Bitcoin escrow
		// order is, and the cancellation requests the refund of the escrow (escrowSettleTerminal), which this watcher
		// builds and sends (P3-R8).
		what := fmt.Sprintf("%s of %s XMR received", amount(received, 12), amount(total, 12))
		if _, err = a.transition(ctx, tx, orderID, stateAwaitingPayment, stateCancelled, nil, "payment window expired underpaid: "+what); err != nil {
			return err
		}
		body := "Order " + shortID(orderID) + ": the payment window ended with " + what + " into its Monero escrow (" + a.netTag() + " " + a.xmrEscrowNetwork() + "), so the order was cancelled. " +
			"The market returns the escrowed coins, less the network fee, to the refund address you gave at checkout: it builds and sends the refund transaction from this order's escrow account."
		if _, err = insertNotification(ctx, tx, o.BuyerID, body, o.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// flagLockedXMR flags, once each, the deposits of o with an unlock time: never counted, reported to staff and the buyer.
func (a *App) flagLockedXMR(ctx context.Context, tx *sql.Tx, o *Order) error {
	locked, err := collectStrings(ctx, tx, "UPDATE payments SET flagged=true WHERE order_id=$1 AND locked AND NOT flagged RETURNING txid", o.ID)
	if err != nil {
		return err
	}
	for _, txid := range locked {
		note := fmt.Sprintf("%s %s Monero escrow deposit %s has an unlock time; locked transfer ignored: it is not counted toward payment and is never paid out automatically. Moderator review required.", a.netTag(), a.xmrEscrowNetwork(), truncate(txid, 20))
		if err = a.flagOrder(ctx, tx, o, note, ""); err != nil {
			return err
		}
		body := "Order " + shortID(o.ID) + ": locked transfer ignored. Deposit " + truncate(txid, 20) + " to this order's escrow has an unlock time and does not count toward payment."
		if o.State == stateAwaitingPayment {
			body += " Send an ordinary transfer to the escrow address."
		}
		if _, err = insertNotification(ctx, tx, o.BuyerID, body, o.ID); err != nil {
			return err
		}
	}
	return nil
}
