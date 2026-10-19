package market

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// Escrow Phase 3, slice X-c (WR-escrow A-248, P3-R3, P3-R5): the Monero escrow watcher's wallet lifecycle, slots and
// funding, against the simulated wallet-rpc and monerod of escrow_xmr_fake_test.go. DB integration tests (simulated
// RPC, not chain evidence; scripts/test-escrow-xmr-regtest.sh is the real-chain leg).

// xmrEscrowEnv is a market with ESCROW_XMR on against the fake, a buyer, a vendor, an administrator and a digital listing
// with automatic delivery, and the watcher's clock (now, UTC) the test moves.
type xmrEscrowEnv struct {
	*testEnv
	fake               *fakeXMREscrow
	seedKey            *openpgp.Entity // the private half of ESCROW_XMR_SEED_KEY
	buyer, buyerSess   string
	vendor, vendorSess string
	admin              string
	product            string
	now                time.Time
	keyPath            string
}

func newXMREscrowEnv(t *testing.T) *xmrEscrowEnv {
	t.Helper()
	return newXMREscrowEnvWith(t, nil)
}

// newXMREscrowEnvWith is newXMREscrowEnv with edit applied to the fake before the market starts (its daemon's network,
// say: A-449).
func newXMREscrowEnvWith(t *testing.T, edit func(f *fakeXMREscrow)) *xmrEscrowEnv {
	t.Helper()
	fake := newFakeXMREscrow(t)
	if edit != nil {
		edit(fake)
	}
	key, pub := testSeedKey(t, nil)
	x := &xmrEscrowEnv{fake: fake, seedKey: key, keyPath: writeSeedKey(t, pub), now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	fake.setEnv(t, x.keyPath)
	x.testEnv = newTestApp(t)
	x.clock()
	x.buyer, x.buyerSess = x.user("xbuyer_"+randomToken()[:6], "buyer")
	x.vendor, x.vendorSess = x.user("xvendor_"+randomToken()[:6], "vendor")
	x.admin, _ = x.user("xadmin_"+randomToken()[:6], "admin")
	x.product = x.digital(x.vendor)
	return x
}

// clock gives the App's Monero escrow the test's clock (again after a restart).
func (x *xmrEscrowEnv) clock() {
	if m := x.A.escrowXMR; m != nil {
		m.now = func() time.Time { return x.now }
	}
}

func (x *xmrEscrowEnv) restart() {
	x.t.Helper()
	x.testEnv.restart()
	x.clock()
}

// digital is a digital listing of vendor's with automatic delivery (0.5 XMR).
func (x *xmrEscrowEnv) digital(vendor string) string {
	p := x.testEnv.product(vendor, "digital")
	agExec(x.testEnv, "UPDATE products SET delivery_content='licence key XC-1234' WHERE id=$1", p)
	return p
}

// physical is a physical listing of vendor's with a 3-day dispatch window; the vendor gets a key that can receive an
// encrypted shipping address (A-83).
func (x *xmrEscrowEnv) physical(vendor string) string {
	p := x.testEnv.product(vendor, "physical")
	agExec(x.testEnv, "UPDATE products SET dispatch_days=3 WHERE id=$1", p)
	_, pub := testPGPKey(x.t, "xvendorkey"+randomToken()[:6])
	x.setKey(vendor, pub)
	return p
}

func (x *xmrEscrowEnv) poll() error { return x.A.xmrEscrowPollOnce(context.Background()) }

func (x *xmrEscrowEnv) mustPoll() {
	x.t.Helper()
	if err := x.poll(); err != nil {
		x.t.Fatalf("Monero escrow pass: %v", err)
	}
}

// draft is a new XMR draft of the buyer's on product.
func (x *xmrEscrowEnv) draft(product string) string {
	return x.order(x.buyer, product, "XMR", stateDraft)
}

func (x *xmrEscrowEnv) checkout(sess, order, refund string) *httptest.ResponseRecorder {
	return x.do("POST", "/orders/escrow-xmr", sess, url.Values{"order_id": {order}, "refund_address": {refund}})
}

// freshCheckout takes a Monero escrow for a new buyer's draft on product (a buyer has at most 3 orders awaiting payment)
// and returns the order and the escrow address.
func (x *xmrEscrowEnv) freshCheckout(product string) (string, string) {
	x.t.Helper()
	buyer, sess := x.user("xb_"+randomToken()[:8], "buyer")
	order := x.order(buyer, product, "XMR", stateDraft)
	if w := x.checkout(sess, order, testXMRAddrOn(x.refundNet(), "refund "+order, true)); w.Code != 303 {
		x.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return order, agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
}

// refundNet is the network type the helpers' refund addresses are of: the fake daemon's, or the fakechain for the
// real-chain legs (escrow_xmr_regtest_test.go), which have no fake.
func (x *xmrEscrowEnv) refundNet() string {
	if x.fake == nil {
		return "fakechain"
	}
	return x.fake.network()
}

// mustCheckout takes a Monero escrow for the buyer's draft order and returns its address.
func (x *xmrEscrowEnv) mustCheckout(order string) string {
	x.t.Helper()
	if w := x.checkout(x.buyerSess, order, testXMRAddrOn(x.refundNet(), "refund "+order, true)); w.Code != 303 {
		x.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1 AND coin='XMR'", order)[0]
}

func (x *xmrEscrowEnv) state(order string) string {
	return agStrings(x.t, x.testEnv, "SELECT state FROM orders WHERE id=$1", order)[0]
}

// wallets lists the escrow wallets as name:state, oldest first.
func (x *xmrEscrowEnv) wallets() []string {
	return agStrings(x.t, x.testEnv, "SELECT name||':'||state FROM xmr_escrow_wallets ORDER BY id")
}

// slots counts the slots of wallet name in state.
func (x *xmrEscrowEnv) slots(name, state string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.name=$1 AND s.state=$2", name, state)
}

// notified counts the notifications of uid whose body contains part.
func (x *xmrEscrowEnv) notified(uid, part string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", uid, part)
}

// unseal decrypts a wallet row's sealed seed with the test's private seed key.
func (x *xmrEscrowEnv) unseal(name string) string {
	x.t.Helper()
	sealed := agStrings(x.t, x.testEnv, "SELECT seed_sealed FROM xmr_escrow_wallets WHERE name=$1", name)[0]
	block, err := armor.Decode(strings.NewReader(sealed))
	if err != nil {
		x.t.Fatal(err)
	}
	md, err := openpgp.ReadMessage(block.Body, openpgp.EntityList{x.seedKey}, nil, nil)
	if err != nil {
		x.t.Fatal(err)
	}
	b, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		x.t.Fatal(err)
	}
	return string(b)
}

const xmrListingPrice = 500000000000 // the fixture listing's price, 0.5 XMR in piconero

// First run (P3-R3): with no wallet at all, one pass creates this month's wallet (escrow-YYYYMM-01) with the derived
// password, seals its seed to the pinned key with the restore height and primary address, activates it and keeps
// ESCROW_XMR_SLOTS free slots on it, each checked against get_address before its row is written. Account 0 is never a
// slot. A second pass changes nothing.
func TestXMREscrowFirstRun(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) {
		t.Fatalf("wallets after the first pass: %v", got)
	}
	file, ok := x.fake.file("escrow-202610-01")
	if !ok || file.password != xmrWalletPassword(testXMRSecret, "escrow-202610-01") {
		t.Fatal("the wallet was not created with the password derived from MONERO_WALLET_SECRET")
	}
	if got := x.unseal("escrow-202610-01"); got != file.mnemonic {
		t.Fatalf("the sealed seed is not the wallet's: %q", got)
	}
	row := agStrings(t, x.testEnv, "SELECT primary_address||' '||restore_height||' '||seed_key_fpr FROM xmr_escrow_wallets WHERE name='escrow-202610-01'")[0]
	if row != file.accounts[0]+" 120 "+x.A.escrowXMR.seedKey.fpr {
		t.Fatalf("wallet row %q", row)
	}
	slots := agStrings(t, x.testEnv, "SELECT account_index||' '||address FROM xmr_escrow_slots WHERE state='free' ORDER BY account_index")
	if len(slots) != 5 || len(file.accounts) != 6 {
		t.Fatalf("free slots %v, accounts %d", slots, len(file.accounts))
	}
	for i, s := range slots {
		if s != strconv.Itoa(i+1)+" "+file.accounts[i+1] {
			t.Fatalf("slot %d is %q, not account %d at its address", i, s, i+1)
		}
	}
	if x.fake.countOf("get_address") < 5 {
		t.Fatalf("slots inserted without checking get_address (%d calls)", x.fake.countOf("get_address"))
	}
	creates := x.fake.countOf("create_wallet") + x.fake.countOf("create_account")
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) || x.fake.countOf("create_wallet")+x.fake.countOf("create_account") != creates {
		t.Fatalf("a second pass changed the wallets: %v", got)
	}
}

// P3-R3 rotation, on the test's clock: next month's wallet is created (sealed, not active) from 7 days before the month
// starts, not earlier; it becomes active at the month's start (UTC), when this month's becomes closing and its free
// slots are retired (its claimed slot stays its order's); free slots are then kept on the new wallet only.
func TestXMREscrowWalletRotation(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	x.mustCheckout(order)

	x.now = time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC) // 7 days and a minute before November
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) {
		t.Fatalf("more than 7 days before the month: %v", got)
	}
	x.now = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active", "escrow-202611-01:sealed"}) {
		t.Fatalf("7 days before the month: %v", got)
	}
	if x.slots("escrow-202611-01", "free") != 0 || x.slots("escrow-202610-01", "free") != 5 {
		t.Fatal("free slots belong on the active wallet only")
	}
	// Checkout still claims from October's wallet.
	early := x.draft(x.digital(x.vendor))
	x.mustCheckout(early)
	if w := agStrings(t, x.testEnv, "SELECT w.name FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE es.order_id=$1", early)[0]; w != "escrow-202610-01" {
		t.Fatalf("checkout before November claimed from %s", w)
	}

	x.now = time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active", "escrow-202611-01:sealed"}) {
		t.Fatalf("a second before November: %v", got)
	}
	x.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:closing", "escrow-202611-01:active"}) {
		t.Fatalf("at November's start: %v", got)
	}
	if x.slots("escrow-202610-01", "free") != 0 || x.slots("escrow-202610-01", "claimed") != 2 || x.slots("escrow-202610-01", "retired") != 5 {
		t.Fatalf("October's slots: free %d claimed %d retired %d", x.slots("escrow-202610-01", "free"), x.slots("escrow-202610-01", "claimed"), x.slots("escrow-202610-01", "retired"))
	}
	if x.slots("escrow-202611-01", "free") != 5 {
		t.Fatalf("November's free slots: %d", x.slots("escrow-202611-01", "free"))
	}
	late := x.draft(x.digital(x.vendor))
	x.mustCheckout(late)
	if w := agStrings(t, x.testEnv, "SELECT w.name FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE es.order_id=$1", late)[0]; w != "escrow-202611-01" {
		t.Fatalf("checkout in November claimed from %s", w)
	}
	// The closing wallet is still read: a deposit to October's escrow is recorded.
	addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	x.fake.payAddress(addr, hex64("oct-dep"), xmrListingPrice, 12)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("a closing wallet's escrow was not funded: %s", x.state(order))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE s.state='free' AND w.state<>'active'"); n != 0 {
		t.Fatalf("%d free slots on a wallet that is not active", n)
	}

	// A market down through the lead window and past the month's start creates and activates that month's wallet at once.
	x.now = time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:closing", "escrow-202611-01:closing", "escrow-202701-01:active"}) {
		t.Fatalf("after a gap: %v", got)
	}
}

// P3-R3 crash safety: a create_wallet that took effect without an answer leaves the row creating, and the next pass
// opens that file (never creates it again) and seals it; a create_account that took effect without an answer leaves an
// account with no row, which the next pass checks against get_address and adds as a retired slot, never a free one
// (A-556; escrow_xmr_rowless_test.go). A wallet file that
// exists without a row (left by a restored database) is not adopted: its row is marked orphan and the month's next name
// is used.
func TestXMREscrowCreatingResumed(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.fake.fault("create_wallet", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a create_wallet with no answer passed")
	}
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:creating"}) {
		t.Fatalf("after the lost answer: %v", got)
	}
	file, ok := x.fake.file("escrow-202610-01")
	if !ok {
		t.Fatal("the fake did not create the file")
	}
	x.fake.fault("create_account", fakeXMRDrop)
	x.poll() // the lost create_account answer is this pass's error
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) {
		t.Fatalf("after resuming: %v", got)
	}
	if again, _ := x.fake.file("escrow-202610-01"); again.mnemonic != file.mnemonic || x.unseal("escrow-202610-01") != file.mnemonic {
		t.Fatal("the resumed wallet was not the file created first")
	}
	x.mustPoll()
	accounts, _ := x.fake.file("escrow-202610-01")
	rows := agStrings(t, x.testEnv, "SELECT account_index||' '||address||' '||state FROM xmr_escrow_slots ORDER BY account_index")
	if len(rows) != len(accounts.accounts)-1 || x.slots("escrow-202610-01", "free") < 5 {
		t.Fatalf("accounts %d, slot rows %v", len(accounts.accounts), rows)
	}
	for i, r := range rows {
		state := " free"
		if i == 0 {
			state = " retired"
		}
		if r != strconv.Itoa(i+1)+" "+accounts.accounts[i+1]+state {
			t.Fatalf("slot row %q, account %d at %s", r, i+1, accounts.accounts[i+1])
		}
	}

	// A file left with no row: next month's first name is taken by an orphan file.
	x.fake.edit(func(f *fakeXMREscrow) { f.newFile("escrow-202611-01", "someone else's password") })
	x.now = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active", "escrow-202611-01:orphan", "escrow-202611-02:sealed"}) {
		t.Fatalf("with an orphan file: %v", got)
	}
	if o, _ := x.fake.file("escrow-202611-01"); o.password != "someone else's password" {
		t.Fatal("the orphan file was changed")
	}
	if x.notified(x.admin, "escrow-202611-01") != 1 {
		t.Fatal("the orphan file was not reported to the administrators")
	}
}

// A-429 (PAYE-E2): every escrow wallet gets a per-account subaddress lookahead of 1 (an escrow address is always an
// account's address 0; the account lookahead stays 50), set with its own password once create_wallet has made it, also
// when a lost create_wallet answer is resumed, and before it is sealed or gets any account. A refused lookahead leaves
// the row creating, so the next pass sets it again.
func TestXMREscrowWalletLookaheadMinor(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.fake.fault("create_wallet", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a create_wallet with no answer passed")
	}
	x.fake.fault("set_subaddress_lookahead", fakeXMRNotOpen)
	if err := x.poll(); err == nil || !slices.Equal(x.wallets(), []string{"escrow-202610-01:creating"}) {
		t.Fatalf("a refused lookahead: %v, wallets %v", err, x.wallets())
	}
	x.mustPoll()
	x.now = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // next month's wallet is created new
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active", "escrow-202611-01:sealed"}) {
		t.Fatalf("wallets: %v", got)
	}
	var methods []string
	var lookaheads []string
	for _, c := range x.fake.callLog() {
		methods = append(methods, c.Method)
		if c.Method == "set_subaddress_lookahead" {
			lookaheads = append(lookaheads, c.Params)
		}
	}
	for _, name := range []string{"escrow-202610-01", "escrow-202611-01"} {
		want := `{"major_idx":50,"minor_idx":1,"password":"` + xmrWalletPassword(testXMRSecret, name) + `"}`
		if !slices.Contains(lookaheads, want) {
			t.Fatalf("%s: lookaheads set %v", name, lookaheads)
		}
	}
	// Each wallet's order of calls: create_wallet (answered), the lookahead, query_key (the seal), create_account.
	set := slices.Index(methods, "set_subaddress_lookahead")
	if created := slices.Index(methods[1:], "create_wallet") + 1; created == 0 || set < created || slices.Index(methods[created:set], "query_key") >= 0 ||
		slices.Index(methods[created:set], "create_account") >= 0 || slices.Index(methods[set:], "query_key") < 0 {
		t.Fatalf("calls: %v", methods)
	}
}

// P3-R4: with MONERO_WALLET_SECRET changed from the one this database pinned, the watcher creates no wallet and reads
// none, and the startup retry keeps Monero escrow disabled.
func TestXMREscrowSecretMismatch(t *testing.T) {
	x := newXMREscrowEnv(t) // pins the first secret; no pass has run, so no wallet exists
	t.Setenv("MONERO_WALLET_SECRET", "another-wallet-secret-9876543210fedcbaZYXW")
	x.restart()
	before := x.fake.count()
	if x.A.escrowXMR.ready.Load() {
		t.Fatal("ready with a changed secret")
	}
	x.mustPoll()
	x.A.retryEscrowXMR(context.Background(), &escrowRetry{last: x.A.escrowXMR.startState})
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets"); n != 0 || x.fake.countOf("create_wallet") != 0 || x.fake.count() != before {
		t.Fatalf("%d wallets, %d create_wallet, %d requests with a changed secret", n, x.fake.countOf("create_wallet"), x.fake.count()-before)
	}
}

// P3-R4: with ESCROW_XMR_SEED_KEY changed from the pinned key, no wallet is created (not even next month's) and the pin
// is not moved to the new key; the existing wallet is still read, so its escrows are funded.
func TestXMREscrowSeedKeyRepinRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	pinned := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrSeedKeySetting)[0]
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	_, other := testSeedKey(t, nil)
	t.Setenv("ESCROW_XMR_SEED_KEY", writeSeedKey(t, other))
	x.restart()
	if x.A.escrowXMR.canCreate.Load() || !x.A.escrowXMR.ready.Load() {
		t.Fatal("a changed seed key must stop wallet creation only")
	}
	x.now = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	x.fake.payAddress(addr, hex64("repin-dep"), xmrListingPrice, 10)
	creates := x.fake.countOf("create_wallet")
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) || x.fake.countOf("create_wallet") != creates {
		t.Fatalf("a wallet was created under a changed seed key: %v", got)
	}
	if got := agStrings(t, x.testEnv, "SELECT value FROM settings WHERE key=$1", xmrSeedKeySetting)[0]; got != pinned {
		t.Fatal("the seed key pin moved to the new key")
	}
	if x.state(order) != stateCompleted {
		t.Fatalf("the existing wallet's escrow was not funded: %s", x.state(order))
	}
}

// Account 0 (P3-R3) is never a slot: no slot, claim or payments row ever names it, a deposit there alerts the
// administrators once per episode (a balance there, until it is back to zero), and the watcher never spends.
func TestXMREscrowAccountZeroNeverSpent(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	for range 5 {
		x.freshCheckout(x.product)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE account_index=0"); n != 0 {
		t.Fatalf("%d slots at account 0", n)
	}
	x.fake.pay("escrow-202610-01", 0, hex64("acct0"), 7000000000, 12)
	x.mustPoll()
	x.mustPoll()
	const alert = "primary account (account 0)"
	if x.notified(x.admin, alert) != 1 || x.notified(x.buyer, alert) != 0 {
		t.Fatalf("account 0 alerts: admin %d", x.notified(x.admin, alert))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1", hex64("acct0")); n != 0 {
		t.Fatalf("%d payments rows for account 0", n)
	}
	// The balance goes back to zero (the transfer is gone after a reorg, say): the episode ends; a new deposit alerts again.
	x.fake.edit(func(f *fakeXMREscrow) { f.files["escrow-202610-01"].transfers = nil })
	x.mustPoll()
	x.fake.pay("escrow-202610-01", 0, hex64("acct0-again"), 5000000000, 12)
	x.mustPoll()
	if x.notified(x.admin, alert) != 2 {
		t.Fatalf("a second episode: %d alerts", x.notified(x.admin, alert))
	}
	for _, m := range []string{"transfer", "sweep_all", "sweep_single", "relay_tx"} {
		if x.fake.countOf(m) != 0 {
			t.Fatalf("the watcher called %s", m)
		}
	}
}

// P3-R5: a deposit counts only at max(PAYMENT_CONFIRMATIONS_XMR, 10) confirmations (here PAYMENT_CONFIRMATIONS_XMR=3), with
// no unlock time and no double_spend_seen; a pool entry is recorded at 0 confirmations. A digital order completes on
// funding through the escrow-only system edge (content delivered, a Monero note, no payout); a locked deposit is flagged
// to staff and the buyer once and never counted; a double spend is recorded conflicted.
func TestXMREscrowFundingThreshold(t *testing.T) {
	t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "3")
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("pool-dep"), xmrListingPrice, 0)
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||credited FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"0 false"}) || x.state(order) != stateAwaitingPayment {
		t.Fatalf("a pool entry: %v %s", got, x.state(order))
	}
	x.fake.setConfs(hex64("pool-dep"), 9)
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||credited FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"9 false"}) || x.state(order) != stateAwaitingPayment {
		t.Fatalf("9 confirmations: %v %s", got, x.state(order))
	}
	x.fake.setConfs(hex64("pool-dep"), 10)
	x.mustPoll()
	if x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND idx >= 4611686018427387904", order) != 1 {
		t.Fatalf("10 confirmations: %s", x.state(order))
	}
	notes := strings.Join(agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 ORDER BY id", order), "\n")
	if !strings.Contains(notes, xmrEscrowCompletedNote) || strings.Contains(notes, "Bitcoin") || strings.Contains(notes, " BTC") {
		t.Fatalf("history of a funded Monero escrow order:\n%s", notes)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order); n != 0 {
		t.Fatalf("%d payouts for a Monero escrow order", n)
	}

	// Locked (unlock_time) at 50 confirmations: recorded, flagged once, never counted.
	locked := x.draft(x.digital(x.vendor))
	laddr := x.mustCheckout(locked)
	x.fake.payAddress(laddr, hex64("locked-dep"), xmrListingPrice, 50)
	x.fake.setTransfer(hex64("locked-dep"), func(t *fakeXMRTransfer) { t.UnlockTime = 3000 })
	x.mustPoll()
	x.mustPoll()
	if x.state(locked) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND locked AND flagged AND NOT credited", locked) != 1 ||
		x.notified(x.admin, "unlock time") != 1 || x.notified(x.buyer, "unlock time") != 1 {
		t.Fatalf("a locked deposit: %s, admin %d buyer %d", x.state(locked), x.notified(x.admin, "unlock time"), x.notified(x.buyer, "unlock time"))
	}

	// double_spend_seen: recorded conflicted (-1), never counted.
	spent := x.draft(x.digital(x.vendor))
	saddr := x.mustCheckout(spent)
	x.fake.payAddress(saddr, hex64("dss-dep"), xmrListingPrice, 20)
	x.fake.setTransfer(hex64("dss-dep"), func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT confirmations::text FROM payments WHERE order_id=$1", spent); !slices.Equal(got, []string{"-1"}) || x.state(spent) != stateAwaitingPayment {
		t.Fatalf("a double spend: %v %s", got, x.state(spent))
	}
}

// One transaction paying two escrow accounts is two payments rows (P3-R1: idx = 2^62 + wallet_id·2^32 + account), each
// its own order's; outputs of one transaction to two addresses of one account are one row.
func TestXMREscrowTwoAccountsOneTx(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	a, b := x.draft(x.digital(x.vendor)), x.draft(x.digital(x.vendor))
	addrA, addrB := x.mustCheckout(a), x.mustCheckout(b)
	tx := hex64("two-accounts")
	x.fake.payAddress(addrA, tx, xmrListingPrice/2, 10)
	x.fake.payAddress(addrB, tx, xmrListingPrice, 10)
	_, accA := x.fake.accountOf(addrA)
	x.fake.edit(func(f *fakeXMREscrow) { // the rest of a's price to another address of its account, in the same transaction
		w := f.files["escrow-202610-01"]
		w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: tx, Account: accA, Minor: 3, Amount: xmrListingPrice / 2, Confs: 10})
	})
	x.mustPoll()
	wallet := int64(agInt(x.testEnv, "SELECT id FROM xmr_escrow_wallets WHERE name='escrow-202610-01'"))
	_, accB := x.fake.accountOf(addrB)
	want := []string{a + " " + strconv.FormatInt(xmrEscrowIdx(wallet, accA), 10) + " " + strconv.FormatInt(xmrListingPrice, 10),
		b + " " + strconv.FormatInt(xmrEscrowIdx(wallet, accB), 10) + " " + strconv.FormatInt(xmrListingPrice, 10)}
	if got := agStrings(t, x.testEnv, "SELECT order_id||' '||idx||' '||amount FROM payments WHERE txid=$1 ORDER BY idx", tx); !slices.Equal(got, want) {
		t.Fatalf("payments rows of one transaction:\n got %v\nwant %v", got, want)
	}
	if x.state(a) != stateCompleted || x.state(b) != stateCompleted {
		t.Fatalf("orders %s %s", x.state(a), x.state(b))
	}
}

// A ledger row the database refuses as another order's account (payments_xmr_escrow_idx) is caught for that row only:
// it is flagged to staff once with a fixed line, and the pass records the other rows and settles their orders.
func TestXMREscrowCollisionFlaggedPerRow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	a, b := x.draft(x.digital(x.vendor)), x.draft(x.digital(x.vendor))
	addrA, addrB := x.mustCheckout(a), x.mustCheckout(b)
	other := x.draft(x.digital(x.vendor))
	tx := hex64("collide")
	x.fake.payAddress(addrA, tx, xmrListingPrice, 10) // account 1: written first
	x.fake.payAddress(addrB, tx, xmrListingPrice, 10)
	// After the watcher read its slots and before it writes the ledger, a's slot comes to name another order, as no
	// code path can do (the slot trigger and the escrow's foreign key forbid it): the database then refuses a's row.
	agExec(x.testEnv, "ALTER TABLE escrows DROP CONSTRAINT escrows_xmr_slot")
	flip := func(from, to string) {
		agExec(x.testEnv, "ALTER TABLE xmr_escrow_slots DISABLE TRIGGER xmr_escrow_slots_kept")
		agExec(x.testEnv, "UPDATE xmr_escrow_slots SET order_id=$2 WHERE order_id=$1", from, to)
		agExec(x.testEnv, "ALTER TABLE xmr_escrow_slots ENABLE TRIGGER xmr_escrow_slots_kept")
	}
	arm := func() {
		x.fake.hook("get_transfers", func() { x.fake.hook("get_transfers", nil); flip(a, other) })
	}
	arm()
	logs := captureLog(t)
	x.poll()
	if x.state(b) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", b) != 1 {
		t.Fatalf("the other row was not recorded: %s", x.state(b))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE txid=$1", tx) != 1 || x.state(a) != stateAwaitingPayment {
		t.Fatal("the refused row was recorded")
	}
	const line = "was not recorded: the database refused it as another order's escrow account"
	flip(other, a) // the next pass reads a's slot again, and meets the same refusal
	arm()
	x.poll()
	if x.notified(x.admin, line) != 1 || agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", a, line) != 1 {
		t.Fatalf("collision flags: admin %d", x.notified(x.admin, line))
	}
	if !strings.Contains(logs.String(), "payments_xmr_escrow_idx") || strings.Contains(logs.String(), addrA) {
		t.Fatalf("log:\n%s", logs.String())
	}
}

// The network re-check runs after every open_wallet, before the wallet is used (P3-R2): a wallet-rpc whose height
// drifted 11 blocks from the daemon is refused for good, and nothing it reports is recorded. A different wallet than
// the one asked for (its primary address not the row's) is never read either.
func TestXMREscrowRecheckAfterOpen(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("drift"), xmrListingPrice, 12)
	// Another wallet file whose account of the same index received the price: read as this wallet, it would fund the order.
	_, account := x.fake.accountOf(addr)
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.newFile("escrow-209901-01", "x")
		for i := int64(1); i <= account; i++ {
			w.accounts = append(w.accounts, testXMRAddr("other wallet account "+strconv.FormatInt(i, 10), true))
		}
		w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: hex64("other"), Account: account, Amount: xmrListingPrice, Confs: 12})
	})
	x.fake.fault("open_wallet", fakeXMRWrongWallet)
	if err := x.poll(); err == nil || x.state(order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments") != 0 {
		t.Fatalf("another wallet was read: %v %s", err, x.state(order))
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.walletLag = 11 })
	if err := x.poll(); !errors.Is(err, errXMRWalletLag) {
		t.Fatalf("a drifted wallet-rpc: %v", err)
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.walletLag = 0 })
	x.poll()
	if x.state(order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments") != 0 || !x.A.escrowXMR.refused.Load() {
		t.Fatalf("a refused wallet-rpc was read: %s", x.state(order))
	}
}

// A-475: the daemon gains 100 blocks while the escrow wallet refreshes (a daemon catching up after downtime, a slow
// pass, the browser journey's mine of 100): the wallet, synced to that daemon, is 100 blocks past the height read
// before the refresh. Monero escrow stays on, the pass reads the wallet and funds the order; a wallet-rpc on another
// daemon (100 blocks ahead of it on both readings) is still refused for good.
func TestXMREscrowDaemonGainsBlocksDuringRefresh(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("race"), xmrListingPrice, 12)
	x.fake.hook("refresh", func() { x.fake.mine(100) })
	x.mustPoll()
	x.fake.hook("refresh", nil)
	if x.A.escrowXMR.refused.Load() || !x.A.escrowXMR.ready.Load() || x.state(order) != stateCompleted {
		t.Fatalf("daemon gained 100 blocks during the refresh: refused %v, ready %v, order %s", x.A.escrowXMR.refused.Load(), x.A.escrowXMR.ready.Load(), x.state(order))
	}
	x.mustPoll()

	x.fake.edit(func(f *fakeXMREscrow) { f.walletLag = 100 })
	if err := x.poll(); !errors.Is(err, errXMRWalletLag) || !x.A.escrowXMR.refused.Load() || x.A.escrowXMR.ready.Load() {
		t.Fatalf("a wallet-rpc on another daemon: %v", err)
	}

	// A wallet-rpc on another daemon 11 blocks behind the height ours had before the refresh, while ours gains 100
	// during it: below both readings, refused on that same pass.
	y := newXMREscrowEnv(t)
	y.mustPoll()
	y.fake.hook("refresh", func() {
		y.fake.mine(100)
		y.fake.edit(func(f *fakeXMREscrow) { f.walletLag = -111 })
	})
	if err := y.poll(); !errors.Is(err, errXMRWalletLag) || !y.A.escrowXMR.refused.Load() || y.A.escrowXMR.ready.Load() {
		t.Fatalf("a wallet-rpc on a daemon lagging ours by 11: %v", err)
	}
}

// A wallet-rpc that loses its open wallet mid-pass (-13) fails that wallet's read: nothing is marked missing and no
// order expires; the next pass reads it again.
func TestXMREscrowNotOpenMidPass(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("half"), xmrListingPrice/2, 12)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.fake.fault("get_transfers", fakeXMRNotOpen)
	if err := x.poll(); err == nil {
		t.Fatal("a failed read passed")
	}
	if got := agStrings(t, x.testEnv, "SELECT confirmations::text FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"12"}) ||
		agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND underpaid_noted IS NOT NULL", order) != 0 {
		t.Fatalf("after a failed read: %v", got)
	}
	// Read again: the payment window ended underpaid, so the order is cancelled once, which requests its Monero refund
	// (slice X-d), never a Bitcoin settlement.
	x.mustPoll()
	x.mustPoll()
	if x.state(order) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order) != 1 ||
		x.notified(x.buyer, "payment window ended") != 1 {
		t.Fatalf("underpaid: %s, buyer notices %d", x.state(order), x.notified(x.buyer, "payment window ended"))
	}
}

// A-429 (PAYE-E2: a catch-up refresh after downtime outlasted its bound): a refresh that times out fails that wallet's
// pass. Nothing more is asked of the wallet-rpc (nothing read, no store), the wallet keeps its state and no order
// expires on what was not read. The wallet-rpc finishes the refresh on its own, and the next pass reads the wallet and
// funds the order.
func TestXMREscrowRefreshTimeoutCatchUp(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.fake.payAddress(addr, hex64("catch up"), xmrListingPrice, 12)
	old := xmrWalletRefreshTimeout
	xmrWalletRefreshTimeout = 200 * time.Millisecond
	t.Cleanup(func() { xmrWalletRefreshTimeout = old })
	x.fake.hook("refresh", func() { time.Sleep(500 * time.Millisecond) })
	from := len(x.fake.callLog())
	err := x.poll()
	var re *xmrRPCError
	if !errors.As(err, &re) || re.Method != "refresh" || re.Class != "timeout" {
		t.Fatalf("a refresh past its bound: %v", err)
	}
	var methods []string
	for _, c := range x.fake.callLog()[from:] {
		methods = append(methods, c.Method)
	}
	if len(methods) == 0 || methods[len(methods)-1] != "refresh" {
		t.Fatalf("calls after the refresh timed out: %v", methods)
	}
	if x.state(order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments") != 0 || !slices.Equal(x.wallets(), []string{"escrow-202610-01:active"}) {
		t.Fatalf("after a refresh timeout: %s, wallets %v", x.state(order), x.wallets())
	}
	x.fake.hook("refresh", nil)
	x.mustPoll()
	if x.state(order) != stateCompleted || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 1 {
		t.Fatalf("the next pass: %s", x.state(order))
	}
}

// The payment window (P3-R5 with Phase 1's rules): an escrow that received nothing is cancelled; a physical order
// becomes paid with its ship-by time; a deposit to a closed order is recorded after_close and flagged for the stray
// refund, never counted.
func TestXMREscrowWindowAndLateDeposit(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	unpaid := x.draft(x.digital(x.vendor))
	uaddr := x.mustCheckout(unpaid)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", unpaid)
	x.mustPoll()
	if x.state(unpaid) != stateCancelled {
		t.Fatalf("unpaid: %s", x.state(unpaid))
	}
	x.fake.payAddress(uaddr, hex64("late"), xmrListingPrice, 12)
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND flagged AND NOT credited", unpaid) != 1 ||
		x.notified(x.buyer, "after the order closed") != 1 {
		t.Fatal("a deposit after close was not recorded and flagged")
	}
	if body := strings.Join(agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1", x.buyer), "\n"); strings.Contains(body, "sign") {
		t.Fatalf("a Monero escrow notice asks for a signature:\n%s", body)
	}

	phys := x.physical(x.vendor)
	order := x.draft(phys)
	addr := x.mustCheckout(order)
	if got := agStrings(t, x.testEnv, "SELECT dispatch_days||' '||release_days FROM escrows WHERE order_id=$1", order); len(got) != 1 || !strings.HasPrefix(got[0], "3 ") {
		t.Fatalf("physical snapshot: %v", got)
	}
	x.fake.payAddress(addr, hex64("phys"), xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND ship_by > now()+interval '4 days'", order) != 1 {
		t.Fatalf("physical funded: %s", x.state(order))
	}
}

// A-477: retry a failed after-close flag even when a confirmed deposit disappears from get_transfers after its stray
// refund is sent. The transaction failure below models a settleXMREscrow database error on the first pass.
func TestXMREscrowLateDepositFlaggedWhenStrayBuildFailsAtHighThreshold(t *testing.T) {
	t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "1000")
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.mustPoll()
	if x.state(order) != stateCancelled {
		t.Fatalf("unpaid: %s", x.state(order))
	}
	txid := hex64("late high threshold")
	x.fake.payAddress(addr, txid, xmrListingPrice, 1000)
	agExec(x.testEnv, `CREATE FUNCTION a477_fail_late_note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF position('arrived at this order''s escrow address after the order closed' in NEW.note)>0 THEN
			RAISE EXCEPTION 'A-477 forced settleXMREscrow flag failure';
		END IF;
		RETURN NEW;
	END $$;
	CREATE TRIGGER a477_fail_late_note BEFORE INSERT ON order_events FOR EACH ROW EXECUTE FUNCTION a477_fail_late_note()`)
	if err := x.poll(); err == nil || !strings.Contains(err.Error(), "A-477 forced settleXMREscrow flag failure") {
		t.Fatalf("the first late-deposit flag failure: %v", err)
	}
	stray := x.settlementOf(order, "stray_refund")
	if stray.State != "relayed" || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=(SELECT id FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund')", order) != 1 {
		t.Fatalf("after the first pass: stray refund %+v", stray)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 0 {
		t.Fatal("the failed flag transaction persisted")
	}
	agExec(x.testEnv, "DROP TRIGGER a477_fail_late_note ON order_events; DROP FUNCTION a477_fail_late_note()")
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files["escrow-202610-01"]
		kept := w.transfers[:0]
		for _, transfer := range w.transfers {
			if transfer.TxID != txid {
				kept = append(kept, transfer)
			}
		}
		w.transfers = kept
	})
	if err := x.poll(); err != nil {
		t.Fatalf("next pass after the flag failure: %v", err)
	}
	if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||after_close||' '||flagged||' '||credited FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"-1 true true false"}) {
		t.Fatalf("late deposit after its incoming transfer disappeared: %v", got)
	}
	if x.notified(x.admin, "after the order closed") != 1 {
		t.Fatalf("staff notification after retry: %d", x.notified(x.admin, "after the order closed"))
	}
}

// Under the restore gate (and after a restore until Monero escrow's sweep) no wallet or slot is created and no wallet
// activated; deposits are still read and orders funded.
func TestXMREscrowGatePausesCreation(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	for _, key := range []string{"payments_recovery_required", xmrEscrowSweepKey} {
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", key)
		x.now = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		creates := x.fake.countOf("create_wallet") + x.fake.countOf("create_account")
		x.mustPoll()
		if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:active"}) || x.fake.countOf("create_wallet")+x.fake.countOf("create_account") != creates {
			t.Fatalf("%s set: %v", key, got)
		}
		agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", key)
	}
	x.fake.payAddress(addr, hex64("gated"), xmrListingPrice, 10)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funding under the gate: %s", x.state(order))
	}
}
