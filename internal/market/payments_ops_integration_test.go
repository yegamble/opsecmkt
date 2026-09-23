package market

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Operational safety: wallet outages never expire orders, unavailable nodes do not stop the site, shutdown
// does not abort a payout, restored payouts stay held, and administrators can recover held or failed payouts.

// restoredHold is the error scripts/restore.sh writes on payouts that were pending or sending in a dump.
const restoredHold = "Restored from backup: verify in the wallet before releasing; this payout may already have been sent."

// expireAddress backdates an order's deposit address past PAYMENT_EXPIRY.
func (p *payEnv) expireAddress(order string) {
	p.t.Helper()
	if _, err := p.DB.Exec("UPDATE payment_addresses SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
		p.t.Fatal(err)
	}
}

// completedWithPayout drives a fresh order to completed with a queued release to the vendor.
func (p *payEnv) completedWithPayout(txid string) string {
	p.t.Helper()
	order, addr := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(order, addr, txid)
	p.move(order, statePaid, stateShipped, p.vendor)
	p.move(order, stateShipped, stateCompleted, p.buyer)
	return order
}

func (p *payEnv) payoutID(order string) string {
	p.t.Helper()
	var id string
	if err := p.DB.QueryRow("SELECT id::text FROM payouts WHERE order_id=$1", order).Scan(&id); err != nil {
		p.t.Fatal(err)
	}
	return id
}

func TestWatcherNeverExpiresWithoutWalletRead(t *testing.T) {
	p := newPayEnv(t)
	p.expireAddress(p.order)
	p.fake.SetWallet(errors.New("wallet RPC timed out"), nil, false)
	if err := p.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "wallet RPC timed out") {
		t.Fatalf("wallet read failure not reported: %v", err)
	}
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("order expired although the wallet was not read this pass: %s", s)
	}
	// The same order expires once a pass reads the wallet and finds nothing.
	p.fake.SetWallet(nil, nil, false)
	p.poll()
	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("expired order after a successful read: %s", s)
	}
}

func TestWatcherSkipsPassWhileWalletCheckFailsOrNodeSyncs(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	queued := p.completedWithPayout("tx-q") // release queued, not yet sent
	if st, _, _, _ := p.payout(queued); st != "pending" {
		t.Fatalf("staged payout is %s", st)
	}
	p.expireAddress(p.order)
	p.fake.Deposit(p.addr, "tx-sync", 0, 100000, 5)

	p.fake.SetWallet(nil, nil, true)
	p.poll() // syncing is not an error, but nothing happens
	var lastErr string
	p.DB.QueryRow("SELECT last_error FROM payment_status WHERE currency='BTC'").Scan(&lastErr)
	if s := p.state(p.order); s != stateAwaitingPayment || lastErr != syncingReason || len(p.fake.Sends()) != 0 {
		t.Fatalf("syncing pass acted: state=%s last_error=%q sends=%d", s, lastErr, len(p.fake.Sends()))
	}
	if !strings.Contains(p.page("/admin", p.adminSess), "Node is still syncing") {
		t.Fatal("admin page hides the syncing state")
	}
	p.fake.SetWallet(nil, errors.New("wallet not loaded"), false)
	if err := p.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "wallet not loaded") {
		t.Fatalf("failed check not reported: %v", err)
	}
	if s := p.state(p.order); s != stateAwaitingPayment || len(p.fake.Sends()) != 0 || p.A.provider("BTC") == nil {
		t.Fatalf("failed-check pass acted or dropped the provider: state=%s sends=%d", s, len(p.fake.Sends()))
	}
	p.fake.SetWallet(nil, nil, false)
	p.poll()
	if s := p.state(p.order); s != statePaid || len(p.fake.Sends()) != 1 {
		t.Fatalf("recovered pass: state=%s sends=%d", s, len(p.fake.Sends()))
	}
}

func TestUnreachableNodeAtStartupKeepsSiteUp(t *testing.T) {
	core, node := newPayCore(t, "testnet4")
	node.down.Store(true)
	setPaymentEnv(t, node.url(""), "", "", "", "")
	e := newTestApp(t) // New() must not fail
	b, bs := e.user("ubuyer", "buyer")
	v, _ := e.user("uvendor", "vendor")
	_, as := e.user("uadmin", "admin")
	draft := e.order(b, e.product(v, "physical"), "BTC", stateDraft)
	get := func(path, sess string) string {
		t.Helper()
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	admin := get("/admin", as)
	if !strings.Contains(admin, "Unavailable — retried every poll") || !strings.Contains(admin, "bitcoin RPC unreachable") {
		t.Fatal("admin page does not show the provider error")
	}
	if body := get("/order?id="+draft, bs); !strings.Contains(body, "temporarily unavailable") {
		t.Fatal("order page does not say payment is temporarily unavailable")
	}
	w := e.do("POST", "/orders/pay", bs, url.Values{"order_id": {draft}})
	e.check(w, 409)
	if !strings.Contains(w.Body.String(), "Payment unavailable for BTC") {
		t.Fatalf("pay while unavailable: %s", w.Body.String())
	}
	if err := e.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("pass while unavailable: %v", err)
	}
	node.down.Store(false)
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("pass after recovery: %v", err)
	}
	if admin = get("/admin", as); !strings.Contains(admin, "TESTNET testnet4") || strings.Contains(admin, "Unavailable — retried") {
		t.Fatal("recovered provider not shown as enabled")
	}
	// The node is later replaced by a mainnet node: the running site disables BTC and keeps serving.
	core.set(func(c *payCore) { c.chain = "main" })
	if err := e.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "not a test network") {
		t.Fatalf("mainnet after startup: %v", err)
	}
	if admin = get("/admin", as); !strings.Contains(admin, "Refused — not a test network") || !strings.Contains(admin, `bitcoin chain &#34;main&#34; is not a test network`) {
		t.Fatal("admin page does not show the refusal")
	}
	e.check(e.do("POST", "/orders/pay", bs, url.Values{"order_id": {draft}}), 409)
}

func TestPayoutSendIsNotCancelledByShutdown(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-shut")
	ctx, cancel := context.WithCancel(context.Background())
	p.fake.sendHook = cancel // SIGTERM arrives while the wallet call is in flight
	p.A.pollOnce(ctx)
	p.fake.sendHook = nil
	if st, txid, _, _ := p.payout(order); st != "sent" || txid == "" {
		t.Fatalf("payout after shutdown during send: %s %q", st, txid)
	}
	if errs := p.fake.sendCtxErrs; len(errs) != 1 || errs[0] != nil {
		t.Fatalf("wallet send saw a cancelled context: %v", errs)
	}
}

func TestCloseWaitsForInFlightPayout(t *testing.T) {
	t.Setenv("PAYMENT_POLL_INTERVAL", "1s")
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-close")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.fake.sendHook = func() { once.Do(func() { close(started) }); <-release }
	p.A.Start(context.Background())
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not start the payout")
	}
	closed := make(chan struct{})
	go func() { p.A.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a payout was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the payout finished")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var st string
	if err = db.QueryRow("SELECT state FROM payouts WHERE order_id=$1", order).Scan(&st); err != nil || st != "sent" {
		t.Fatalf("payout recorded as %q (%v)", st, err)
	}
}

func TestRestoredHoldIsNeverReleasedByTheWatcher(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-restored")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", order, restoredHold); err != nil {
		t.Fatal(err)
	}
	p.poll()
	p.poll()
	if st, _, _, _ := p.payout(order); st != "held" || len(p.fake.Sends()) != 0 {
		t.Fatalf("restored hold released by the watcher: %s sends=%d", st, len(p.fake.Sends()))
	}
}

func (p *payEnv) payoutAction(session, id, op, txid, password string) (int, string) {
	p.t.Helper()
	w := p.do("POST", "/admin/payout", session, url.Values{"payout_id": {id}, "op": {op}, "txid": {txid}, "password": {password}})
	return w.Code, w.Body.String()
}

// payoutActionConfirmed submits op with the explicit "no transaction was broadcast" confirmation.
func (p *payEnv) payoutActionConfirmed(session, id, op, password string) (int, string) {
	p.t.Helper()
	w := p.do("POST", "/admin/payout", session, url.Values{"payout_id": {id}, "op": {op}, "password": {password}, "not_broadcast": {"confirmed"}})
	return w.Code, w.Body.String()
}

func TestAdminPayoutActions(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	txid := strings.Repeat("ab", 32)

	// Held after a restore: shown with its actions, released only by an administrator with a password.
	held := p.completedWithPayout("tx-held")
	p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", held, restoredHold)
	heldID := p.payoutID(held)
	admin := p.page("/admin", p.adminSess)
	for _, want := range []string{"Resolve payout " + heldID, "Release held payout", "Mark sent", `name="password"`} {
		if !strings.Contains(admin, want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
	if code, _ := p.payoutAction(p.vendorSess, heldID, "release", "", testPassword); code != 403 {
		t.Fatalf("vendor released a payout: %d", code)
	}
	if code, _ := p.payoutAction(p.adminSess, heldID, "release", "", "wrong-password-123"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, body := p.payoutAction(p.adminSess, heldID, "requeue", "", testPassword); code != 409 || !strings.Contains(body, "does not apply") {
		t.Fatalf("requeue of a held payout: %d", code)
	}
	if code, body := p.payoutAction(p.adminSess, heldID, "release", "", testPassword); code != 303 {
		t.Fatalf("release: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(held); st != "pending" {
		t.Fatalf("released payout is %s", st)
	}
	if code, _ := p.payoutAction(p.adminSess, heldID, "release", "", testPassword); code != 409 {
		t.Fatalf("second release: %d", code)
	}
	p.poll()
	if st, _, _, _ := p.payout(held); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("released payout: %s sends=%d", st, len(p.fake.Sends()))
	}

	// Failed with a definite wallet rejection (nothing broadcast): requeued after checking the wallet, with
	// no extra confirmation, then sent once more.
	failed := p.completedWithPayout("tx-failed")
	p.fake.sendErr = &rpcError{Method: "fake RPC send", Code: -6, Message: "insufficient funds"}
	p.A.pollOnce(context.Background())
	p.fake.sendErr = nil
	failedID := p.payoutID(failed)
	if code, body := p.payoutAction(p.adminSess, failedID, "requeue", "", testPassword); code != 303 {
		t.Fatalf("requeue: %d %s", code, body)
	}
	p.poll()
	if st, _, _, _ := p.payout(failed); st != "sent" || len(p.fake.Sends()) != 2 {
		t.Fatalf("requeued payout: %s sends=%d", st, len(p.fake.Sends()))
	}

	// Failed ambiguously but the wallet shows it was broadcast: recorded as sent with the wallet's txid,
	// never re-sent. Mark sent needs no broadcast confirmation (it pays nothing).
	broadcast := p.completedWithPayout("tx-broadcast")
	p.fake.sendErr = errors.New("timed out")
	p.A.pollOnce(context.Background())
	p.fake.sendErr = nil
	bID := p.payoutID(broadcast)
	if code, _ := p.payoutAction(p.adminSess, bID, "sent", "not-a-txid", testPassword); code != 400 {
		t.Fatalf("malformed txid: %d", code)
	}
	if code, body := p.payoutAction(p.adminSess, bID, "sent", strings.ToUpper(txid), testPassword); code != 303 {
		t.Fatalf("mark sent: %d %s", code, body)
	}
	p.poll()
	if st, got, _, _ := p.payout(broadcast); st != "sent" || got != txid || len(p.fake.Sends()) != 2 {
		t.Fatalf("marked payout: %s %q sends=%d", st, got, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%recorded as sent by an administrator%'", p.vendor.ID); n != 1 {
		t.Fatalf("recipient notifications: %d", n)
	}

	// A stuck send can be requeued only once it is clearly stuck, and (since it may have been broadcast)
	// only with the explicit not-broadcast confirmation; a live one cannot.
	stuck := p.completedWithPayout("tx-stuck")
	p.DB.Exec("UPDATE payouts SET state='sending',updated=now() WHERE order_id=$1", stuck)
	stuckID := p.payoutID(stuck)
	if code, _ := p.payoutActionConfirmed(p.adminSess, stuckID, "requeue", testPassword); code != 409 {
		t.Fatalf("requeue of a live send: %d", code)
	}
	p.DB.Exec("UPDATE payouts SET updated=now()-interval '1 hour' WHERE order_id=$1", stuck)
	// (Refusal without the confirmation: TestPayoutSentButNotRecordedStaysSendingAndNeedsConfirmation; this
	// test already uses all ten password confirmations the rate limit allows.)
	if code, _ := p.payoutActionConfirmed(p.adminSess, stuckID, "requeue", testPassword); code != 303 {
		t.Fatalf("requeue of a stuck send: %d", code)
	}
	if code, _ := p.payoutAction(p.adminSess, stuckID, "sent", txid, testPassword); code != 409 {
		t.Fatalf("mark sent on a pending payout: %d", code)
	}

	// Conflict holds stay with the watcher while the deposit is still conflicted.
	conflict := p.completedWithPayout("tx-conflict")
	p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", conflict, heldReason)
	p.DB.Exec("UPDATE payments SET confirmations=-1 WHERE order_id=$1", conflict)
	if code, body := p.payoutAction(p.adminSess, p.payoutID(conflict), "release", "", testPassword); code != 409 || !strings.Contains(body, "still conflicted") {
		t.Fatalf("release while conflicted: %d", code)
	}

	var audits []string
	rows, err := p.DB.Query("SELECT action FROM audit_events WHERE action LIKE '%payout%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		rows.Scan(&a)
		audits = append(audits, a)
	}
	rows.Close()
	if len(audits) != 4 || !strings.HasPrefix(audits[0], "Released held payout "+heldID) || !strings.Contains(audits[2], "sent with transaction "+txid) || !strings.Contains(audits[0], "(confirmed with password)") ||
		strings.Contains(audits[1], "outcome unknown") || !strings.Contains(audits[3], "was sending") || !strings.Contains(audits[3], "administrator confirmed the wallet shows no broadcast transaction") {
		t.Fatalf("audit trail %q", audits)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator released%'", held); n != 1 {
		t.Fatalf("order history for the release: %d", n)
	}
}

func TestAdminPayoutActionsAreSingleUseUnderConcurrency(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	failed := p.completedWithPayout("tx-cf")
	p.fake.sendErr = &rpcError{Method: "fake RPC send", Code: -13, Message: "wallet locked"}
	p.A.pollOnce(context.Background())
	p.fake.sendErr = nil
	held := p.completedWithPayout("tx-ch")
	p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", held, restoredHold)
	for _, tc := range []struct{ order, op string }{{failed, "requeue"}, {held, "release"}} {
		id := p.payoutID(tc.order)
		codes := make(chan int, 4)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _ := p.payoutAction(p.adminSess, id, tc.op, "", testPassword)
				codes <- code
			}()
		}
		wg.Wait()
		close(codes)
		got := map[int]int{}
		for c := range codes {
			got[c]++
		}
		if got[303] != 1 || got[409] != 3 {
			t.Fatalf("%s: concurrent results %v", tc.op, got)
		}
	}
	p.poll()
	p.poll()
	if sends := p.fake.Sends(); len(sends) != 2 {
		t.Fatalf("sends after concurrent requeue and release: %+v", sends)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout%' OR action LIKE 'Released held payout%'"); n != 2 {
		t.Fatalf("audit rows: %d", n)
	}
}
