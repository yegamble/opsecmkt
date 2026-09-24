package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Operational safety: wallet outages never expire orders, unavailable nodes do not stop the site, shutdown
// does not abort a payout, restored payouts stay held, and administrators can recover held or failed payouts.

// restoredHold is the error scripts/restore.sh writes on payouts that were pending, sending, blocked or held in a
// dump; a payout held for an account suspension keeps that text after it (A-112).
const restoredHold = "Restored from backup: verify in the wallet before releasing; this payout may already have been sent."

// restoredFailed starts the error scripts/restore.sh writes on payouts that were failed in a dump, followed by
// the failure recorded before the backup.
const restoredFailed = "Restored from backup: verify in the wallet before requeueing; this payout may have been requeued and sent after the backup. Last error: "

// restorePayoutSQL is the payout protection scripts/restore.sh and the manual SQL in UPGRADING.md apply to a
// restored dump (TestRestoredHoldMarkerMatchesRestoreScripts keeps them identical, whitespace aside).
var restorePayoutSQL = []string{
	"UPDATE payouts SET state='held', updated=now(), error='" + restoredHold + "' || coalesce(' ' || substring(error from 'Suspended account:.*'), '') WHERE state IN ('pending','sending','blocked','held')",
	"UPDATE payouts SET send_ambiguous=true WHERE state='failed'",
	"UPDATE payouts SET updated=now(), error='" + restoredFailed + "' || error WHERE state='failed'",
}

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
	var heldAmount int64
	if err := p.DB.QueryRow("SELECT amount FROM payouts WHERE order_id=$1", held).Scan(&heldAmount); err != nil {
		t.Fatal(err)
	}
	// The summary names what is resolved even when the row's other columns are scrolled out of view (A-116).
	summary := "Resolve payout " + heldID + ": release " + amount(heldAmount, 8) + " BTC to " + p.vendor.Handle + ", order " + held[:8] + "</summary>"
	for _, want := range []string{summary, "Release held payout", "Mark sent", `name="password"`} {
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
	// (Refusal without the "no broadcast" confirmation: TestReleaseRestoredHeldPayoutRequiresConfirmation.)
	if code, body := p.payoutActionConfirmed(p.adminSess, heldID, "release", testPassword); code != 303 {
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
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator confirmed the wallet shows no broadcast%released it%'", held); n != 1 {
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
				// The restored hold needs the "no broadcast" confirmation; the definite failure does not.
				var code int
				if tc.op == "release" {
					code, _ = p.payoutActionConfirmed(p.adminSess, id, tc.op, testPassword)
				} else {
					code, _ = p.payoutAction(p.adminSess, id, tc.op, "", testPassword)
				}
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

// resolveSection returns the admin page's resolve controls for one payout.
func resolveSection(t *testing.T, admin, id string) string {
	t.Helper()
	_, s, ok := strings.Cut(admin, "Resolve payout "+id+":")
	if !ok {
		t.Fatalf("admin page has no controls for payout %s", id)
	}
	s, _, _ = strings.Cut(s, "</details>")
	return s
}

// A payout held by a restore may already have been broadcast (a restored "sending" payout is indistinguishable
// from one that never left), so releasing it needs the same explicit wallet confirmation as requeueing an
// ambiguous failure. A payout the watcher holds for a conflicted deposit was never sent and needs none.
func TestReleaseRestoredHeldPayoutRequiresConfirmation(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	restored := p.completedWithPayout("tx-restored-release")
	p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", restored, restoredHold)
	conflict := p.completedWithPayout("tx-conflict-release")
	p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", conflict, heldReason)
	p.DB.Exec("UPDATE payments SET confirmations=-1 WHERE order_id=$1", conflict)
	rID, cID := p.payoutID(restored), p.payoutID(conflict)

	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	if s := resolveSection(t, admin, rID); !strings.Contains(s, `name="not_broadcast" value="confirmed" required`) || !strings.Contains(s, "Release held payout") {
		t.Fatalf("restored hold offered without the broadcast confirmation: %s", s)
	}
	if !strings.Contains(admin, "Held after a restore from backup — may already have been sent") {
		t.Fatal("restored hold not labelled as possibly sent")
	}
	if s := resolveSection(t, admin, cID); strings.Contains(s, "not_broadcast") {
		t.Fatalf("watcher hold asks for a broadcast confirmation: %s", s)
	}

	// Without the confirmation: refused, still held, nothing recorded.
	code, body := p.payoutAction(p.adminSess, rID, "release", "", testPassword)
	if code != 400 || !strings.Contains(body, "Check the wallet") || !strings.Contains(body, "mark it sent with the wallet") {
		t.Fatalf("unconfirmed release of a restored hold: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(restored); st != "held" {
		t.Fatalf("restored hold is %s after a refused release", st)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator%'", restored); n != 0 {
		t.Fatalf("refused release recorded %d order events", n)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Released held payout%'"); n != 0 {
		t.Fatalf("refused release audited %d times", n)
	}

	// With it: released once, and the confirmation is on the record.
	if code, body := p.payoutActionConfirmed(p.adminSess, rID, "release", testPassword); code != 303 {
		t.Fatalf("confirmed release: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(restored); st != "pending" {
		t.Fatalf("released restored hold is %s", st)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator confirmed the wallet shows no broadcast transaction%restored from backup%'", restored); n != 1 {
		t.Fatalf("order history for the confirmed release: %d", n)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Released held payout "+rID+" %restored from backup, administrator confirmed the wallet shows no broadcast transaction%"); n != 1 {
		t.Fatalf("audit for the confirmed release: %d", n)
	}

	// A watcher hold: still refused while the deposit is unsettled, then released with the password alone.
	// (Checked before polling: a poll re-reads the fake wallet's confirmations and would settle it itself.)
	if code, body := p.payoutAction(p.adminSess, cID, "release", "", testPassword); code != 409 || !strings.Contains(body, "still conflicted") {
		t.Fatalf("release while conflicted: %d %s", code, body)
	}
	p.DB.Exec("UPDATE payments SET confirmations=100 WHERE order_id=$1", conflict)
	if code, body := p.payoutAction(p.adminSess, cID, "release", "", testPassword); code != 303 {
		t.Fatalf("release of a settled watcher hold: %d %s", code, body)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1 AND action NOT LIKE '%restored from backup%'", "Released held payout "+cID+" %"); n != 1 {
		t.Fatalf("audit for the watcher-hold release: %d", n)
	}
	p.poll()
	for _, o := range []string{restored, conflict} {
		if st, _, _, _ := p.payout(o); st != "sent" {
			t.Fatalf("released payout %s is %s", o[:8], st)
		}
	}
	if n := len(p.fake.Sends()); n != 2 {
		t.Fatalf("sends after both releases: %d", n)
	}
}

// The restore script and the manual SQL in UPGRADING.md apply the payout protection the tests use, with the
// marker the release and requeue checks match.
func TestRestoredHoldMarkerMatchesRestoreScripts(t *testing.T) {
	for _, marker := range []string{restoredHold, restoredFailed} {
		if !strings.HasPrefix(marker, restoredHoldPrefix) {
			t.Fatalf("restore marker %q lacks prefix %q", marker, restoredHoldPrefix)
		}
	}
	for _, f := range []string{"scripts/restore.sh", "UPGRADING.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, q := range restorePayoutSQL {
			if !strings.Contains(text, q) {
				t.Fatalf("%s does not apply the restore payout protection %q", f, q)
			}
		}
	}
}

// A payout whose send the wallet definitely rejected before the backup may have been requeued and sent after
// it. The restore marks it as possibly sent, so it is not labelled "nothing broadcast" and requeueing it needs
// the explicit "wallet shows no broadcast" confirmation; without the restore marking the same requeue would
// pay the recipient twice once the recovery gate is cleared.
func TestRestoredDefiniteFailureRequeuesOnlyAfterWalletCheck(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-restore-failed")
	p.fake.sendErr = &rpcError{Method: "fake wallet RPC sendtoaddress", Code: -6, Message: "Insufficient funds"}
	p.A.pollOnce(context.Background())
	if st, _, _, _ := p.payout(order); st != "failed" {
		t.Fatalf("payout is %s, want failed", st)
	}
	id := p.payoutID(order)
	var backupErr string
	var ambiguous bool
	p.DB.QueryRow("SELECT error,send_ambiguous FROM payouts WHERE id=$1", id).Scan(&backupErr, &ambiguous)
	if ambiguous || !strings.Contains(backupErr, "nothing was broadcast") {
		t.Fatalf("backed-up failure is not a definite rejection: ambiguous=%v %q", ambiguous, backupErr)
	}

	// After the backup: the wallet is funded, an administrator requeues the definite failure, it is sent.
	p.fake.sendErr = nil
	if code, body := p.payoutAction(p.adminSess, id, "requeue", "", testPassword); code != 303 {
		t.Fatalf("requeue before the restore: %d %s", code, body)
	}
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("after requeue: %s sends=%d", st, len(p.fake.Sends()))
	}

	// Restore the backup (the row is back to its backed-up failed state), then the restore payout protection.
	if _, err := p.DB.Exec("UPDATE payouts SET state='failed',txid='',send_ambiguous=false,error=$2 WHERE id=$1::bigint", id, backupErr); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value"); err != nil {
		t.Fatal(err)
	}
	for _, q := range restorePayoutSQL {
		if _, err := p.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var restoredErr string
	p.DB.QueryRow("SELECT error,send_ambiguous FROM payouts WHERE id=$1", id).Scan(&restoredErr, &ambiguous)
	if !ambiguous || restoredErr != restoredFailed+backupErr {
		t.Fatalf("restored failure: ambiguous=%v %q", ambiguous, restoredErr)
	}

	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	var row string
	for _, r := range strings.Split(admin, "<tr") {
		if strings.Contains(r, "Resolve payout "+id+":") {
			row = r
		}
	}
	if row == "" || strings.Contains(row, "nothing broadcast") || !strings.Contains(row, "Failed after a restore from backup — may have been requeued and sent after the backup, check the wallet") {
		t.Fatalf("restored failure labelled: %s", row)
	}
	if s := resolveSection(t, admin, id); !strings.Contains(s, `name="not_broadcast" value="confirmed" required`) || !strings.Contains(s, "Requeue payout") {
		t.Fatalf("restored failure offered for requeue without the broadcast confirmation: %s", s)
	}

	// Without the confirmation: refused, still failed, nothing sent or recorded.
	code, body := p.payoutAction(p.adminSess, id, "requeue", "", testPassword)
	if code == 303 {
		p.DB.Exec("UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
		p.poll()
		t.Fatalf("restored failed payout requeued without a wallet check; wallet sends now %d", len(p.fake.Sends()))
	}
	if code != 400 || !strings.Contains(body, "restored from backup") || !strings.Contains(body, "mark it sent with the wallet") {
		t.Fatalf("unconfirmed requeue of a restored failure: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(order); st != "failed" {
		t.Fatalf("restored failure is %s after a refused requeue", st)
	}

	// The wallet shows the send made after the backup: the administrator records it instead, and nothing is sent.
	if code, body := p.payoutAction(p.adminSess, id, "sent", strings.Repeat("cd", 32), testPassword); code != 303 {
		t.Fatalf("mark sent: %d %s", code, body)
	}
	p.DB.Exec("UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	p.poll()
	if st, txid, _, _ := p.payout(order); st != "sent" || txid != strings.Repeat("cd", 32) || len(p.fake.Sends()) != 1 {
		t.Fatalf("after reconciliation: %s %s sends=%d", st, txid, len(p.fake.Sends()))
	}
}

// A restored failure whose confirmation is given is requeued once, and the record says it was restored.
func TestRestoredFailureConfirmedRequeueIsAudited(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-restore-failed-requeue")
	id := p.payoutID(order)
	if _, err := p.DB.Exec("UPDATE payouts SET state='failed',error='Insufficient funds' WHERE id=$1::bigint", id); err != nil {
		t.Fatal(err)
	}
	for _, q := range restorePayoutSQL {
		if _, err := p.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := p.payoutActionConfirmed(p.adminSess, id, "requeue", testPassword); code != 303 {
		t.Fatalf("confirmed requeue: %d %s", code, body)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator confirmed the wallet shows no broadcast transaction%restored from backup%'", order); n != 1 {
		t.Fatalf("order history for the confirmed requeue: %d", n)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Requeued payout "+id+" %restored from backup, administrator confirmed the wallet shows no broadcast transaction%"); n != 1 {
		t.Fatalf("audit for the confirmed requeue: %d", n)
	}
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("requeued restored failure: %s sends=%d", st, len(p.fake.Sends()))
	}
}

// A payout needing attention and its only resolve form are never pushed off /admin by newer payouts: every
// blocked, held, failed or stuck-sending payout is listed first (oldest first) under a count, then the most
// recent others up to a cap with a truncation notice. An order ID links to the order only where the
// administrator can open it (a disputed or resolved order, read as a non-party reviewer).
func TestAdminPayoutsAttentionNeverHidden(t *testing.T) {
	p := newPayEnv(t)
	insert := func(state, orderState, kind, txid, age string) (order, id string) {
		t.Helper()
		order = p.testEnv.order(p.buyer.ID, p.productID, "BTC", orderState)
		if err := p.DB.QueryRow(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid,updated)
			VALUES($1,$2,$3,'BTC',100000,'fake-testnet-addr',$4,$5,now()-$6::interval) RETURNING id::text`,
			order, kind, p.vendor.ID, state, txid, age).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return order, id
	}
	failedOrder, failedID := insert("failed", stateResolved, "refund", "", "1 hour")
	stuckOrder, stuckID := insert("sending", stateCompleted, "release", "", "10 minutes")
	var sent []string // txids, oldest first
	for i := range 60 {
		txid := fmt.Sprintf("sent-txid-%02d", i)
		insert("sent", stateCompleted, "release", txid, "0 seconds")
		sent = append(sent, txid)
	}
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	for _, want := range []string{
		"2 payouts need attention",
		"Showing every payout that needs attention and the 50 most recent other payouts",
		"Resolve payout " + failedID + ":", "Resolve payout " + stuckID + ":",
		`href="/order?id=` + failedOrder + `"`,
	} {
		if !strings.Contains(admin, want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
	if strings.Index(admin, "Resolve payout "+failedID+":") > strings.Index(admin, "Resolve payout "+stuckID+":") {
		t.Fatal("payouts needing attention are not listed oldest first")
	}
	if strings.Contains(admin, `href="/order?id=`+stuckOrder+`"`) || !strings.Contains(admin, stuckOrder) {
		t.Fatal("an order the administrator cannot open is linked, or its ID is missing")
	}
	if n := strings.Count(admin, `<tr class="payout-attention">`); n != 2 {
		t.Fatalf("attention rows: %d", n)
	}
	// The fifty newest sent payouts are listed; the ten oldest fall past the cap.
	for i, txid := range sent {
		if listed := strings.Contains(admin, txid+"<"); listed != (i >= 10) {
			t.Fatalf("sent payout %d listed=%v", i, listed)
		}
	}
	// The link is correct: the administrator can open the linked order and not the unlinked one.
	p.check(p.do("GET", "/order?id="+failedOrder, p.adminSess, nil), 200)
	p.check(p.do("GET", "/order?id="+stuckOrder, p.adminSess, nil), 404)
	// A payment flagged for review opens the order to staff (A-24), so its payout links to it too.
	if _, err := p.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged)
		VALUES($1,'BTC','flagged-tx',0,'fake-testnet-deposit',1000,-1,true)`, stuckOrder); err != nil {
		t.Fatal(err)
	}
	if admin = html.UnescapeString(p.page("/admin", p.adminSess)); !strings.Contains(admin, `href="/order?id=`+stuckOrder+`"`) {
		t.Fatal("a payout whose order has a flagged payment is not linked")
	}
	p.check(p.do("GET", "/order?id="+stuckOrder, p.adminSess, nil), 200)

	// Nothing needing attention and few payouts: no alarm and no truncation notice.
	if _, err := p.DB.Exec("DELETE FROM payouts WHERE state='sent' OR id IN ($1::bigint,$2::bigint)", failedID, stuckID); err != nil {
		t.Fatal(err)
	}
	insert("sent", stateCompleted, "release", "sent-txid-last", "0 seconds")
	admin = html.UnescapeString(p.page("/admin", p.adminSess))
	if !strings.Contains(admin, "No payouts need attention.") || strings.Contains(admin, "most recent other payouts") || !strings.Contains(admin, "sent-txid-last<") {
		t.Fatal("empty attention state, spurious truncation notice or missing payout")
	}
}
