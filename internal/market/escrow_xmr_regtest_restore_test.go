package market

import (
	"context"
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-f: the real-chain legs of Monero escrow retention, destruction and restore
// (scripts/test-escrow-xmr-regtest.sh; not run in CI), on monerod and monero-wallet-rpc 0.18.5.1 fakechain, each with a
// real App and its watcher against the escrow wallet-rpc behind the counting proxy of the X-d legs. Claim and refund
// addresses are subaddresses of the funder's wallet. A restore is simulated in SQL: scripts/restore.sh's own Monero
// block and the settings it writes (restoreXMRSQL).

// TestXMREscrowRegtestRestore: a database restored from a backup taken before a wallet and a relay. The restore holds
// the built release whose relay the daemon lost, retires the free slots and closes the wallet; under the gate nothing is
// built or relayed. The sweep finds the wallet created after the backup (it opens with the derived password), records
// it as an orphan with its seed sealed to the seed key (the seal restores to its primary address) and lists its funded
// account. An administrator confirms the held release, which is relayed once (the same transaction, no transfer) and
// confirms. Once the gate is cleared the next checkout gets a new wallet's account, no address reused.
func TestXMREscrowRegtestRestore(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	ctx := context.Background()
	x := r.A.escrowXMR
	order, _ := r.completedReal()
	other, _ := r.completedReal()
	// After the backup: the market created the month's next wallet and an account in it, which a buyer funded.
	lost := "escrow-" + monthStart(r.now).Format("200601") + "-02"
	if err := x.rpc.createWallet(ctx, lost, xmrWalletPassword(x.secret, lost)); err != nil {
		t.Fatalf("create_wallet %s: %v", lost, err)
	}
	seed, err := x.rpc.mnemonic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, lostAddr, err := x.rpc.createAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.rpc.store(ctx); err != nil {
		t.Fatal(err)
	}
	r.pay(map[string]int64{lostAddr: xmrListingPrice})
	r.mine(10 + xmrMineMargin)
	before := agStrings(t, r.testEnv, "SELECT address FROM xmr_escrow_slots")
	before = append(before, lostAddr)
	// And a release whose relay took effect though its answer was lost, then left the daemon's pool.
	id := r.releaseReal(order)
	p.dropNext("relay_tx")
	r.poll()
	held := r.settlement(id)
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": []string{held.TxHash}}, &flushed)
	if held.State != "built" {
		t.Fatalf("before the backup: %+v", held)
	}

	heldN, retired, closed := r.restoreBackup()
	if heldN != 1 || retired == 0 || closed != 1 {
		t.Fatalf("restore counts: held %d retired %d closed %d", heldN, retired, closed)
	}
	idOther := r.releaseReal(other) // requested under the gate
	transfers, relays := p.count("transfer"), p.count("relay_tx")
	r.mustPoll()
	r.mustPoll()
	if p.count("transfer") != transfers || p.count("relay_tx") != relays || r.settlement(id).State != "built" || r.settlement(idOther).State != "requested" {
		t.Fatalf("under the gate: transfer %d, relay_tx %d, %+v, %+v", p.count("transfer")-transfers, p.count("relay_tx")-relays, r.settlement(id), r.settlementOf(other, "release"))
	}
	if r.sweepRequired() || r.walletState(lost) != "retained" {
		t.Fatalf("after the sweep: %v, sweep %v", r.wallets(), r.sweepRequired())
	}
	if r.unseal(lost) != seed {
		t.Fatal("the orphan's seal is not its seed")
	}
	var primary string
	if err = r.DB.QueryRow("SELECT primary_address FROM xmr_escrow_wallets WHERE name=$1 AND restore_height=0", lost).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Address string `json:"address"`
	}
	r.call(r.restore, "restore_deterministic_wallet", map[string]any{"filename": "orphan-" + randomToken()[:10], "password": "", "seed": r.unseal(lost),
		"restore_height": 0, "language": "English", "autosave_current": false}, &restored)
	if restored.Address != primary {
		t.Fatal("the orphan's seal does not restore to its primary address")
	}
	if r.notified(r.admin, "account 1 (0.5 XMR)") != 1 {
		t.Fatal("the orphan's funded account was not listed")
	}
	t.Logf("sweep: %s (created after the backup) recorded as an orphan, sealed (restores to %s…), its funded account 1 listed; retained", lost, primary[:12])

	_, adminSess := r.user("xr_admin2_"+randomToken()[:6], "admin")
	if w := r.do("POST", "/admin/escrow-xmr-rebroadcast", adminSess, url.Values{"settlement_id": {strconv.FormatInt(id, 10)}, "not_sent": {"confirmed"}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("confirmation: %d %s", w.Code, html.UnescapeString(w.Body.String()))
	}
	r.mustPoll()
	r.mustPoll()
	s := r.settlement(id)
	if s.State != "relayed" || s.TxHash != held.TxHash || p.count("relay_tx") != relays+1 || p.count("transfer") != transfers || r.settlement(idOther).State != "requested" {
		t.Fatalf("after the confirmation: %+v, relay_tx %d, transfer %d", s, p.count("relay_tx")-relays, p.count("transfer")-transfers)
	}
	r.xmrShape(s)
	r.confirmReal(id)
	t.Logf("held release %s relayed once after the administrator's confirmation (same transaction, no transfer) and confirmed, under the gate", held.TxHash)

	r.clearGate()
	r.mustPoll()
	next := "escrow-" + monthStart(r.now).Format("200601") + "-03"
	if got := r.walletState(next); got != "active" {
		t.Fatalf("after the gate: %v", r.wallets())
	}
	if r.settlement(idOther).State != "relayed" {
		t.Fatalf("the release requested under the gate: %+v", r.settlement(idOther))
	}
	fresh, addr := r.checkoutReal()
	if w := r.walletOf(fresh); w != next || slices.Contains(before, addr) {
		t.Fatalf("checkout after the restore: wallet %s, address reused %v", w, slices.Contains(before, addr))
	}
	t.Logf("checkout after the restore: an account of %s, no address of a restored wallet reused", next)
}

// TestXMREscrowRegtestLookahead: a wallet that does not know an account the database has (an older copy of the wallet,
// or accounts beyond its lookahead): a deposit to account 300 is missed by the wallet's own refresh with the default
// lookahead of 50 accounts; the restore sweep raises the lookahead past the highest slot (with the wallet's password)
// and rescans, and the deposit is recorded and funds the order.
func TestXMREscrowRegtestLookahead(t *testing.T) {
	r, _ := newXMRSettleRegtest(t)
	ctx := context.Background()
	x := r.A.escrowXMR
	name := r.walletName(false)
	// Account 300's address, from the wallet's seed restored on the restore wallet-rpc.
	var height int64
	if err := r.DB.QueryRow("SELECT restore_height FROM xmr_escrow_wallets WHERE name=$1", name).Scan(&height); err != nil {
		t.Fatal(err)
	}
	r.call(r.restore, "restore_deterministic_wallet", map[string]any{"filename": "deep-" + randomToken()[:10], "password": "", "seed": r.unseal(name),
		"restore_height": height, "language": "English", "autosave_current": false}, nil)
	var account struct {
		Index   int64  `json:"account_index"`
		Address string `json:"address"`
	}
	for account.Index < 300 {
		r.call(r.restore, "create_account", nil, &account)
	}
	agExec(r.testEnv, "UPDATE xmr_escrow_slots SET state='retired' WHERE state='free'")
	agExec(r.testEnv, "INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,300,$1 FROM xmr_escrow_wallets WHERE name=$2", account.Address, name)
	order, addr := r.checkoutReal()
	if addr != account.Address {
		t.Fatalf("checkout took %s", addr)
	}
	tx := r.pay(map[string]int64{addr: xmrListingPrice})
	r.mineTo(tx, 10+xmrMineMargin)
	r.synced()
	if tr, err := x.rpc.transfers(ctx); err != nil || slices.ContainsFunc(tr, func(t xmrTransfer) bool { return t.TxID == tx }) {
		t.Fatalf("the default lookahead saw the deposit to account 300 (%v)", err)
	}
	r.mustPoll()
	if r.state(order) != stateAwaitingPayment {
		t.Fatalf("recorded without the lookahead: %s", r.state(order))
	}
	t.Logf("deposit %s to account 300 missed with the default lookahead", tx)
	agExec(r.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrEscrowSweepKey)
	r.mustPoll()
	if r.state(order) != stateCompleted || r.sweepRequired() {
		t.Fatalf("after the sweep's lookahead: %s (deposits %v), sweep %v", r.state(order), r.depositConfs(order), r.sweepRequired())
	}
	t.Logf("the sweep raised the lookahead past account 300 and rescanned: the deposit is recorded and order %s completed", shortID(order))
}

// TestXMREscrowRegtestWrongSecret: with MONERO_WALLET_SECRET changed from the one the database pinned, Monero escrow is
// disabled and no wallet is created or opened on the wallet-rpc; checkout says it is unavailable.
func TestXMREscrowRegtestWrongSecret(t *testing.T) {
	r := newXMRRegtest(t)
	p := newXMRRPCProxy(t, os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL"))
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", p.URL)
	t.Setenv("MONERO_WALLET_SECRET", "another-wallet-secret-9876543210fedcbaZYXW")
	r.restart()
	if r.A.escrowXMR.ready.Load() {
		t.Fatal("ready with a changed secret")
	}
	r.mustPoll()
	r.A.retryEscrowXMR(context.Background(), &escrowRetry{last: r.A.escrowXMR.startState})
	r.mustPoll()
	if n := agInt(r.testEnv, "SELECT count(*) FROM xmr_escrow_wallets"); n != 0 || p.count("create_wallet") != 0 || p.count("open_wallet") != 0 {
		t.Fatalf("%d wallets, create_wallet %d, open_wallet %d with a changed secret", n, p.count("create_wallet"), p.count("open_wallet"))
	}
	order := r.draft(r.product)
	if w := r.checkout(r.buyerSess, order, r.funderAddress()); w.Code != 503 {
		t.Fatalf("checkout with a changed secret: %d", w.Code)
	}
	t.Log("changed MONERO_WALLET_SECRET: Monero escrow disabled, no wallet created or opened, checkout 503")
}

// TestXMREscrowRegtestDestroy: a wallet whose orders all closed is retained; a deposit to a closed order's escrow on day
// 89 is returned by a stray refund to the refund address; from day 90, with that refund 100 blocks deep and nothing
// left in the wallet, it is destroyed: its password replaced, its seal wiped, and open_wallet with the derived password
// then fails.
func TestXMREscrowRegtestDestroy(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	name := r.walletName(false)
	order, addr := r.completedReal()
	id := r.releaseReal(order)
	r.mustPoll()
	r.confirmReal(id)
	r.mine(100)
	r.mustPoll()
	if agInt(r.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", id) != 1 {
		t.Fatalf("release not 100 deep: %+v", r.settlement(id))
	}
	// The order closed on the database's clock, centuries before the leg's: its close moves onto the leg's clock (the
	// dates only move later), so the wallet is kept 90 days of the leg's time.
	agExec(r.testEnv, "UPDATE xmr_escrow_wallets SET last_closed_at=$2,destroy_after=$2::timestamptz+interval '2160 hours' WHERE name=$1", name, r.now)
	r.now = monthStart(r.now).AddDate(0, 1, 0)
	r.mustPoll()
	if got := r.walletState(name); got != "retained" {
		t.Fatalf("with its orders closed: %s", got)
	}
	r.days(name, 89)
	refund := agStrings(t, r.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	late := r.pay(map[string]int64{addr: 70000000000})
	r.mineTo(late, 10+xmrMineMargin)
	r.mustPoll()
	r.mustPoll()
	stray := r.settlementOf(order, "stray_refund")
	if stray.State != "relayed" || agStrings(t, r.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", stray.ID)[0] != refund {
		t.Fatalf("the day-89 deposit: %+v (deposits %v)", stray, r.depositConfs(order))
	}
	r.xmrShape(stray)
	r.confirmReal(stray.ID)
	if r.walletState(name) != "retained" {
		t.Fatalf("on day 89: %s", r.walletState(name))
	}
	r.mine(100)
	r.days(name, 91)
	r.synced()
	r.mustPoll()
	if n := agInt(r.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND state='destroyed' AND seed_sealed IS NULL AND destroyed_at IS NOT NULL AND destroy_started_at=destroyed_at", name); n != 1 {
		t.Fatalf("on day 91: %s", r.walletState(name))
	}
	if p.count("change_wallet_password") != 1 {
		t.Fatalf("change_wallet_password %d", p.count("change_wallet_password"))
	}
	// monero-wallet-rpc 0.18.5.1's refusals, as the watcher classifies them (A-419): the destroyed wallet's is its own
	// wrong password, the only refusal destroyed again automatically (and only once the market recorded the start of its
	// destruction); a name with no file is no-file.
	err := r.A.escrowXMR.rpc.openWallet(context.Background(), name, xmrWalletPassword(testXMRSecret, name))
	if !xmrWalletDenied(err) || xmrOpenRefusal(err) != xmrOpenPassword {
		t.Fatalf("open_wallet with the derived password after destruction: %v", err)
	}
	none := "escrow-" + monthStart(r.now).AddDate(0, 6, 0).Format("200601") + "-99"
	if err = r.A.escrowXMR.rpc.openWallet(context.Background(), none, xmrWalletPassword(testXMRSecret, none)); xmrOpenRefusal(err) != xmrOpenNoFile {
		t.Fatalf("open_wallet of a name with no file: %v", err)
	}
	r.now = r.now.Add(24 * time.Hour)
	r.mustPoll()
	if r.walletState(name) != "destroyed" {
		t.Fatalf("a pass after destruction: %s", r.walletState(name))
	}
	if p := r.panel(order, r.sessOf(order, roleBuyer)); !strings.Contains(p, "cannot be recovered") {
		t.Fatalf("the order page after destruction:\n%s", p)
	}
	t.Logf("wallet %s: day-89 deposit returned by stray refund %s; destroyed on day 91 (password replaced, seal wiped), open_wallet with the derived password refused", name, stray.TxHash)
}
