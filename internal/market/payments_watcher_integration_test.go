package market

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// payEnv is a market with a deterministic fake BTC provider and one physical order awaiting payment.
type payEnv struct {
	*testEnv
	t                      *testing.T
	fake                   *fakeProvider
	buyer, vendor, mod     *User
	buyerSess, vendorSess  string
	adminSess              string
	order, addr, productID string
}

func newPayEnv(t *testing.T) *payEnv {
	e := newTestApp(t)
	fake := newFakeProvider("BTC", 3)
	e.A.payments["BTC"] = fake
	suffix := randomToken()[:6]
	b, bs := e.user("pbuyer_"+suffix, "buyer")
	v, vs := e.user("pvendor_"+suffix, "vendor")
	m, _ := e.user("pmod_"+suffix, "moderator")
	_, as := e.user("padmin_"+suffix, "admin")
	p := &payEnv{testEnv: e, t: t, fake: fake, buyer: e.loadUser(b), vendor: e.loadUser(v), mod: e.loadUser(m), buyerSess: bs, vendorSess: vs, adminSess: as}
	p.productID = e.product(v, "physical")
	p.order, p.addr = p.newOrder(stateAwaitingPayment)
	return p
}

// newOrder inserts an order in state with a provider-issued deposit address (as /orders/pay does).
func (p *payEnv) newOrder(state string) (string, string) {
	p.t.Helper()
	id := p.testEnv.order(p.buyer.ID, p.productID, "BTC", state)
	addr, err := p.fake.NewAddress(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'fake')", id, addr); err != nil {
		p.t.Fatal(err)
	}
	return id, addr
}

func (p *payEnv) poll() {
	p.t.Helper()
	if err := p.A.pollOnce(context.Background()); err != nil {
		p.t.Fatalf("pollOnce: %v", err)
	}
}

func (p *payEnv) move(order, from, to string, actor *User) {
	p.t.Helper()
	ctx := context.Background()
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = p.A.transition(ctx, tx, order, from, to, actor, "test"); err != nil {
		p.t.Fatalf("%s -> %s: %v", from, to, err)
	}
	if err = tx.Commit(); err != nil {
		p.t.Fatal(err)
	}
}

func (p *payEnv) state(order string) string {
	p.t.Helper()
	var s string
	if err := p.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s); err != nil {
		p.t.Fatal(err)
	}
	return s
}

func (p *payEnv) count(query string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.DB.QueryRow(query, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *payEnv) payout(order string) (state, txid, address string, amount int64) {
	p.t.Helper()
	if err := p.DB.QueryRow("SELECT state,txid,address,amount FROM payouts WHERE order_id=$1", order).Scan(&state, &txid, &address, &amount); err != nil {
		p.t.Fatalf("payout for %s: %v", order[:8], err)
	}
	return
}

func (p *payEnv) setPayoutAddress(u *User, addr string) {
	p.t.Helper()
	if _, err := p.DB.Exec("UPDATE users SET payout_btc=$1 WHERE id=$2", addr, u.ID); err != nil {
		p.t.Fatal(err)
	}
}

func (p *payEnv) page(path, session string) string {
	p.t.Helper()
	w := p.do("GET", path, session, nil)
	p.check(w, 200)
	return w.Body.String()
}

func (p *payEnv) paidByWatcher(order, addr, txid string) {
	p.t.Helper()
	p.fake.Deposit(addr, txid, 0, 100000, 3)
	p.poll()
	if s := p.state(order); s != statePaid {
		p.t.Fatalf("order not paid: %s", s)
	}
}

func TestWatcherMarksPaidOnceAtThreshold(t *testing.T) {
	p := newPayEnv(t)
	body := p.page("/order?id="+p.order, p.buyerSess)
	for _, want := range []string{"TESTNET fake deposit address", p.addr, "Waiting for a deposit"} {
		if !strings.Contains(body, want) {
			t.Fatalf("order page missing %q", want)
		}
	}
	p.fake.Deposit(p.addr, "tx-1", 0, 100000, 1)
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("paid below the confirmation threshold: %s", s)
	}
	body = p.page("/order?id="+p.order, p.buyerSess)
	if !strings.Contains(body, "Deposit seen; waiting for 3 confirmations") || !strings.Contains(body, "tx-1:0") {
		t.Fatal("unconfirmed deposit not shown truthfully")
	}
	p.fake.SetConfirmations("tx-1", 3)
	p.DB.SetMaxOpenConns(20)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- p.A.pollOnce(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("state %s", s)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='paid'", p.order); n != 1 {
		t.Fatalf("paid transitions: %d", n)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND credited AND confirmations=3", p.order); n != 1 {
		t.Fatalf("ledger rows: %d", n)
	}
	var note string
	var actor *string
	p.DB.QueryRow("SELECT note,actor_id FROM order_events WHERE order_id=$1 AND to_state='paid'", p.order).Scan(&note, &actor)
	if actor != nil || !strings.Contains(note, "TESTNET fake payment confirmed: 0.001 BTC") {
		t.Fatalf("paid event: %q actor=%v", note, actor)
	}
	body = p.page("/order?id="+p.order, p.buyerSess)
	if strings.Contains(body, p.addr) || !strings.Contains(body, "Deposit address closed") {
		t.Fatal("deposit address still offered after payment")
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); n != 1 {
		t.Fatalf("vendor notifications: %d", n)
	}
}

func TestWatcherSumsOutputsAndKeepsLedgerUnique(t *testing.T) {
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-a", 0, 60000, 5)
	p.poll()
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("partial payment marked %s", s)
	}
	if !strings.Contains(p.page("/order?id="+p.order, p.buyerSess), "Confirmed deposits are below the required amount") {
		t.Fatal("partial payment status missing")
	}
	p.fake.Deposit(p.addr, "tx-b", 2, 40000, 4)
	p.poll()
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("two outputs summing to the amount: %s", s)
	}
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1", p.order); n != 2 {
		t.Fatalf("ledger rows after repeated polls: %d", n)
	}
	if _, err := p.DB.Exec("INSERT INTO payments(order_id,currency,txid,idx,address,amount) VALUES($1,'BTC','tx-a',0,$2,1)", p.order, p.addr); err == nil {
		t.Fatal("ledger accepted a duplicate (currency, txid, idx)")
	}
}

func TestWatcherFlagsConflictAfterPaidWithoutReverting(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-c")
	p.fake.SetConfirmations("tx-c", -1)
	p.poll()
	p.poll()
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("conflict reverted state to %s", s)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%conflicted or missing%'", p.order); n != 1 {
		t.Fatalf("conflict notes: %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payment review needed%'", p.mod.ID); n != 1 {
		t.Fatalf("moderator notifications: %d", n)
	}
	// Completing a flagged order holds the payout instead of sending it.
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "held" || len(p.fake.Sends()) != 0 {
		t.Fatalf("payout %s sends=%d", st, len(p.fake.Sends()))
	}
	if !strings.Contains(p.page("/order?id="+p.order, p.buyerSess), "A credited deposit is conflicted") {
		t.Fatal("order page hides the conflict")
	}
	// A transaction that disappears from the wallet is treated the same way.
	order2, addr2 := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(order2, addr2, "tx-d")
	p.fake.Drop("tx-d")
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND confirmations=-1", order2); n != 1 || p.state(order2) != statePaid {
		t.Fatalf("dropped credited deposit not flagged: %d", n)
	}
}

func TestPayoutReleaseSentExactlyOnce(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-r")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	st, _, addr, amt := p.payout(p.order)
	if st != "pending" || addr != "fake-testnet-vendor" || amt != 100000 {
		t.Fatalf("queued payout %s %s %d", st, addr, amt)
	}
	p.poll()
	p.poll()
	sends := p.fake.Sends()
	if len(sends) != 1 || sends[0].To != "fake-testnet-vendor" || sends[0].Amount != 100000 {
		t.Fatalf("sends %+v", sends)
	}
	st, txid, _, _ := p.payout(p.order)
	if st != "sent" || txid != sends[0].TxID {
		t.Fatalf("payout %s %s", st, txid)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%payout of 0.001 BTC sent%'", p.vendor.ID); n != 1 {
		t.Fatalf("vendor payout notifications: %d", n)
	}
	body := p.page("/order?id="+p.order, p.vendorSess)
	if !strings.Contains(body, "Release to vendor") || !strings.Contains(body, txid) {
		t.Fatal("order page missing payout")
	}
	// A deposit that confirms after settlement is flagged, never paid out automatically.
	p.fake.Deposit(p.addr, "tx-late", 0, 5000, 6)
	p.poll()
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%arrived after this order was settled%'", p.order); n != 1 || len(p.fake.Sends()) != 1 {
		t.Fatalf("late deposit: notes=%d sends=%d", n, len(p.fake.Sends()))
	}
}

func TestPayoutBlockedUntilAddressSaved(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-b1")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "blocked" || len(p.fake.Sends()) != 0 {
		t.Fatalf("payout without address: %s", st)
	}
	if !strings.Contains(p.page("/account", p.vendorSess), "1 payout(s) are waiting for a payout address") {
		t.Fatal("account page does not show the blocked payout")
	}
	for addr, code := range map[string]int{"bc1qmainnetaddress0000000000000000000": 400, "fake-testnet-" + strings.Repeat("x", 120): 400} {
		p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {addr}}), code)
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"XMR"}, "address": {"fake-testnet-x"}}), 409)
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"DOGE"}, "address": {"x"}}), 400)
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {" fake-testnet-vend "}}), 303)
	st, _, addr, _ := p.payout(p.order)
	if st != "pending" || addr != "fake-testnet-vend" {
		t.Fatalf("unblocked payout %s %q", st, addr)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Saved BTC payout address (TESTNET fake); 1 waiting payout(s) now use it'", p.vendor.ID); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "sent" {
		t.Fatalf("payout after address: %s", st)
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {""}}), 303)
	if n := p.count("SELECT count(*) FROM users WHERE id=$1 AND payout_btc=''", p.vendor.ID); n != 1 {
		t.Fatal("payout address not removed")
	}
}

func TestPayoutFailuresAndStuckSendsAreNotRetried(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-f")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.fake.sendErr = errors.New("wallet locked")
	if err := p.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "wallet locked") {
		t.Fatalf("send failure not reported: %v", err)
	}
	p.fake.sendErr = nil
	p.poll()
	st, _, _, _ := p.payout(p.order)
	var msg string
	p.DB.QueryRow("SELECT error FROM payouts WHERE order_id=$1", p.order).Scan(&msg)
	if st != "failed" || !strings.Contains(msg, "may or may not have been broadcast") || len(p.fake.Sends()) != 0 {
		t.Fatalf("failed payout retried or unlabelled: %s %q sends=%d", st, msg, len(p.fake.Sends()))
	}
	// A payout left in 'sending' (crash between claim and record) is surfaced, never re-sent.
	order2, addr2 := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(order2, addr2, "tx-s")
	p.move(order2, statePaid, stateShipped, p.vendor)
	p.move(order2, stateShipped, stateCompleted, p.buyer)
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending',updated=now()-interval '1 hour' WHERE order_id=$1", order2); err != nil {
		t.Fatal(err)
	}
	p.poll()
	if st, _, _, _ := p.payout(order2); st != "sending" || len(p.fake.Sends()) != 0 {
		t.Fatalf("stuck payout: %s sends=%d", st, len(p.fake.Sends()))
	}
	body := p.page("/admin", p.adminSess)
	for _, want := range []string{"Stuck in sending — never retried", "Failed — not retried", "Needs attention", "TESTNET fake", "Enabled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
	if n := p.count("SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role='admin' AND n.body LIKE 'Payout failed%'"); n != 1 {
		t.Fatalf("admin failure notifications: %d", n)
	}
}

func TestRefundsOnCancelAndDisputeOutcome(t *testing.T) {
	p := newPayEnv(t)
	// P3 adds disputes.outcome in migration 030; create it here so this package's tests stand alone.
	if _, err := p.DB.Exec("ALTER TABLE disputes ADD COLUMN IF NOT EXISTS outcome text NOT NULL DEFAULT ''"); err != nil {
		t.Fatal(err)
	}
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	// Cancelled while awaiting payment with a confirmed partial deposit: refund what arrived.
	p.fake.Deposit(p.addr, "tx-partial", 0, 50000, 3)
	p.poll()
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-buyer" || amt != 50000 {
		t.Fatalf("cancel refund %s %s %d", st, addr, amt)
	}
	// Disputed, resolved with refund.
	o2, a2 := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(o2, a2, "tx-d2")
	p.move(o2, statePaid, stateDisputed, p.buyer)
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason,outcome) VALUES($1,$2,'Item never arrived at all','refund')", randomToken(), o2); err != nil {
		t.Fatal(err)
	}
	p.move(o2, stateDisputed, stateResolved, p.mod)
	if st, _, addr, _ := p.payout(o2); st != "pending" || addr != "fake-testnet-buyer" {
		t.Fatalf("dispute refund %s %s", st, addr)
	}
	// Resolved before the outcome was recorded: the watcher queues the release once the outcome exists.
	o3, a3 := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(o3, a3, "tx-d3")
	p.move(o3, statePaid, stateDisputed, p.vendor)
	dispute := randomToken()
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Buyer unresponsive after delivery')", dispute, o3); err != nil {
		t.Fatal(err)
	}
	p.move(o3, stateDisputed, stateResolved, p.mod)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", o3); n != 0 {
		t.Fatal("payout queued without an outcome")
	}
	p.DB.Exec("UPDATE disputes SET outcome='release' WHERE id=$1", dispute)
	p.poll()
	sends := map[string]int64{}
	for _, s := range p.fake.Sends() {
		sends[s.To] += s.Amount
	}
	if sends["fake-testnet-buyer"] != 150000 || sends["fake-testnet-vendor"] != 100000 || len(p.fake.Sends()) != 3 {
		t.Fatalf("sends %+v", p.fake.Sends())
	}
	// A cancelled draft with no deposit address queues nothing.
	o4 := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateDraft)
	p.move(o4, stateDraft, stateCancelled, p.buyer)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", o4); n != 0 {
		t.Fatal("payout for an unfunded draft")
	}
}

func TestPaymentLabelsFollowProviders(t *testing.T) {
	e := newTestApp(t)
	b, bs := e.user("lbuyer", "buyer")
	v, _ := e.user("lvendor", "vendor")
	_, as := e.user("ladmin", "admin")
	product := e.product(v, "physical")
	draft := e.order(b, product, "BTC", stateDraft)
	get := func(path, sess string) string {
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	for path, wants := range map[string][]string{
		"/":                       {"Payments disabled", "Order drafts only."},
		"/order?id=" + draft:      {"Payment is unavailable", "BTC payments are not configured"},
		"/account":                {"Unavailable — payments disabled"},
		"/checkout?id=" + product: {"UNFUNDED DRAFT ONLY."},
	} {
		body := get(path, bs)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s without provider: missing %q", path, want)
			}
		}
		if strings.Contains(body, "TESTNET payments") {
			t.Errorf("%s claims payments without a provider", path)
		}
	}
	admin := get("/admin", as)
	if !strings.Contains(admin, "Not configured (BITCOIN_RPC_URL is blank)") || !strings.Contains(admin, "Not configured (MONERO_WALLET_RPC_URL is blank)") {
		t.Fatal("admin page does not list disabled providers")
	}
	e.check(e.do("POST", "/account/payout", bs, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-x"}}), 409)

	fake := newFakeProvider("BTC", 2)
	e.A.payments["BTC"] = fake
	for path, want := range map[string]string{
		"/":                       "TESTNET payments (BTC fake) — no real funds",
		"/order?id=" + draft:      "Payment not requested yet",
		"/account":                "Bitcoin payout address",
		"/checkout?id=" + product: "Request payment on the order page",
	} {
		if body := get(path, bs); !strings.Contains(body, want) {
			t.Errorf("%s with provider: missing %q", path, want)
		}
	}
	if body := get("/account", bs); !strings.Contains(body, "Monero payouts are unavailable") {
		t.Error("XMR payout form offered without an XMR provider")
	}
	// Address issued, then the provider is removed: the address is no longer shown.
	order := e.order(b, product, "BTC", stateAwaitingPayment)
	addr, _ := fake.NewAddress(context.Background(), order)
	e.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'fake')", order, addr)
	if body := get("/order?id="+order, bs); !strings.Contains(body, addr) {
		t.Fatal("issued address not shown while monitored")
	}
	delete(e.A.payments, "BTC")
	if body := get("/order?id="+order, bs); strings.Contains(body, addr) || !strings.Contains(body, "Payment is not monitored") {
		t.Fatal("unmonitored address still shown")
	}
	e.A.payments["BTC"] = fake
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if admin = get("/admin", as); strings.Contains(admin, "Not polled yet") || !strings.Contains(admin, "TESTNET fake") {
		t.Fatal("admin page does not show the poll result")
	}
}

func TestWatcherRunsInBackgroundAndStops(t *testing.T) {
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	p := newPayEnv(t)
	p.fake.Deposit(p.addr, "tx-bg", 0, 100000, 3)
	p.A.Start(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for p.state(p.order) != statePaid {
		if time.Now().After(deadline) {
			t.Fatal("background watcher did not mark the order paid")
		}
		time.Sleep(100 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { p.A.stop(); p.A.background.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop on Close")
	}
}
