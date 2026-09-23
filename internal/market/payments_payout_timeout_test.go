package market

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Payout sends are bounded by payoutSendTimeout, not by the shorter per-call rpcTimeout, and a failed send is
// recorded as a definite wallet rejection (nothing broadcast) or as ambiguous (may have been broadcast). An
// ambiguous failure or a stuck send is requeued only with the administrator's explicit confirmation.

// shortRPCTimeouts scales the production bounds (10 s reads, 30 s sends) down for tests and restores them.
func shortRPCTimeouts(t *testing.T, read, send time.Duration) {
	t.Helper()
	oldRead, oldSend := rpcTimeout, payoutSendTimeout
	rpcTimeout, payoutSendTimeout = read, send
	t.Cleanup(func() { rpcTimeout, payoutSendTimeout = oldRead, oldSend })
}

// slowly answers after delay (a wallet that takes longer than rpcTimeout to build and broadcast).
func slowly(delay time.Duration, result any) rpcHandler {
	return func(string, string, json.RawMessage) (any, int, string) {
		time.Sleep(delay)
		return result, 0, ""
	}
}

func TestSlowPayoutSendOutlivesTheReadTimeout(t *testing.T) {
	// Production: reads 10 s, sends 30 s. Scaled: a wallet answering after 3x the read bound but well inside
	// the send bound must still be recorded as sent; a read that slow must still time out.
	read, send, delay := 200*time.Millisecond, 5*time.Second, 600*time.Millisecond
	shortRPCTimeouts(t, read, send)
	txid := strings.Repeat("ab", 32)

	_, bov, bs := newGapCore(t, "testnet4")
	bp, err := newBitcoinProvider(context.Background(), bs.url(""), "opsecmkt", "testnet4", 1)
	if err != nil {
		t.Fatal(err)
	}
	bov.answer("sendtoaddress", slowly(delay, txid))
	ctx, cancel := context.WithTimeout(context.Background(), payoutSendTimeout) // as sendPayouts does
	defer cancel()
	start := time.Now()
	got, err := bp.Send(ctx, gapVendorBTC, 100000)
	if err != nil || got != txid || time.Since(start) < delay {
		t.Fatalf("slow bitcoin send: txid=%q err=%v after %s", got, err, time.Since(start))
	}
	bov.answer("listreceivedbyaddress", slowly(delay, []any{}))
	start = time.Now()
	if _, err = bp.Incoming(context.Background(), []string{"tb1qdepositaddress0000000000000000000"}); err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) >= delay {
		t.Fatalf("slow read not bounded by rpcTimeout: %v after %s", err, time.Since(start))
	}

	// Monero (Digest auth: the 401 challenge and the authenticated retry share the send's bound).
	_, xov, xs := newGapMonero(t)
	xp, err := newMoneroProvider(context.Background(), xs.url(""), "", "stagenet", 1)
	if err != nil {
		t.Fatal(err)
	}
	xov.answer("transfer", slowly(delay, map[string]any{"tx_hash": txid}))
	ctx2, cancel2 := context.WithTimeout(context.Background(), payoutSendTimeout)
	defer cancel2()
	start = time.Now()
	if got, err = xp.Send(ctx2, payStagenetSub, 42); err != nil || got != txid || time.Since(start) < delay {
		t.Fatalf("slow monero transfer: txid=%q err=%v after %s", got, err, time.Since(start))
	}
}

func TestSendFailureClassification(t *testing.T) {
	shortRPCTimeouts(t, 100*time.Millisecond, 300*time.Millisecond)
	send := func(c *rpcClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), payoutSendTimeout)
		defer cancel()
		var txid string
		return c.callWithin(ctx, payoutSendTimeout, "", "sendtoaddress", []any{gapVendorBTC, "0.001"}, &txid)
	}
	client := func(url string) *rpcClient {
		c, err := newRPCClient("bitcoin", url, "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// Definite: the wallet answered with a JSON-RPC error.
	_, ov, s := newGapCore(t, "testnet4")
	ov.fail("sendtoaddress", -6, "Insufficient funds")
	if err := send(client(s.url(""))); err == nil || !walletRejected(err) {
		t.Fatalf("JSON-RPC error not a definite rejection: %v", err)
	}
	// Ambiguous: no answer within the send bound.
	ov.stall("sendtoaddress", true)
	if err := send(client(s.url(""))); err == nil || walletRejected(err) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout classified as definite: %v", err)
	}
	// Ambiguous: the connection is dropped after the request was read.
	reset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			conn.Close()
		}
	}))
	defer reset.Close()
	if err := send(client(reset.URL)); err == nil || walletRejected(err) {
		t.Fatalf("dropped connection classified as definite: %v", err)
	}
	// Ambiguous: an unreadable reply.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }))
	defer garbage.Close()
	if err := send(client(garbage.URL)); err == nil || walletRejected(err) || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("unreadable reply classified as definite: %v", err)
	}
}

func TestSlowWalletSendIsRecordedSentByTheWatcher(t *testing.T) {
	shortRPCTimeouts(t, 250*time.Millisecond, 5*time.Second)
	p := newPayEnv(t)
	core, bov, bnode, _ := gapBTC(t, p, 1)
	p.setPayoutAddress(p.vendor, gapVendorBTC)
	addr := "tb1qgapslowsend0000000000000000000000"
	order := p.gapOrder("BTC", addr)
	core.gapReceive(addr, "tx-slow-send", 0, 100000, 1)
	p.poll()
	p.complete(order)
	txid := strings.Repeat("cd", 32)
	bov.answer("sendtoaddress", slowly(750*time.Millisecond, txid)) // 3x rpcTimeout, inside payoutSendTimeout
	p.poll()
	if st, got, _, _ := p.payout(order); st != "sent" || got != txid || bnode.called("sendtoaddress") != 1 {
		t.Fatalf("slow send recorded as %s %q (calls %d)", st, got, bnode.called("sendtoaddress"))
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%failed%'", order); n != 0 {
		t.Fatal("slow send recorded a failure")
	}
}

// failPayout drives a fresh completed order's payout into failed with the fake wallet returning err.
func (p *payEnv) failPayout(txid string, err error) (order, id string) {
	p.t.Helper()
	order = p.completedWithPayout(txid)
	p.fake.sendErr = err
	if perr := p.A.pollOnce(context.Background()); perr == nil {
		p.t.Fatal("send failure not reported")
	}
	p.fake.sendErr = nil
	return order, p.payoutID(order)
}

func TestAmbiguousFailedPayoutRequeueNeedsBroadcastConfirmation(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	ambiguous, aID := p.failPayout("tx-amb", errors.New("bitcoin RPC unreachable: timed out"))
	definite, dID := p.failPayout("tx-def", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	if p.str("SELECT send_ambiguous::text FROM payouts WHERE id=$1", aID) != "true" || p.str("SELECT send_ambiguous::text FROM payouts WHERE id=$1", dID) != "false" {
		t.Fatal("failures not classified")
	}
	if e := p.str("SELECT error FROM payouts WHERE id=$1", aID); !strings.Contains(e, "may or may not have been broadcast") || !strings.Contains(e, "timed out") {
		t.Fatalf("ambiguous error text %q", e)
	}
	if e := p.str("SELECT error FROM payouts WHERE id=$1", dID); !strings.Contains(e, "nothing was broadcast") || !strings.Contains(e, "Insufficient funds") {
		t.Fatalf("definite error text %q", e)
	}
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	if strings.Count(admin, `name="not_broadcast"`) != 1 || !strings.Contains(admin, "outcome unknown, may have been broadcast") || !strings.Contains(admin, "rejected by the wallet, nothing broadcast") {
		t.Fatal("admin page does not distinguish the ambiguous failure or ask for the confirmation once")
	}

	// Server-enforced: without (or with a wrong) confirmation the ambiguous payout stays failed.
	audits := func() int { return p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout%'") }
	for _, form := range []url.Values{
		{"payout_id": {aID}, "op": {"requeue"}, "password": {testPassword}},
		{"payout_id": {aID}, "op": {"requeue"}, "password": {testPassword}, "not_broadcast": {"yes"}},
	} {
		w := p.do("POST", "/admin/payout", p.adminSess, form)
		if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "may have been broadcast") {
			t.Fatalf("unconfirmed requeue: %d", w.Code)
		}
	}
	if code, _ := p.payoutActionConfirmed(p.vendorSess, aID, "requeue", testPassword); code != 403 {
		t.Fatalf("vendor requeue: %d", code)
	}
	if code, _ := p.payoutActionConfirmed(p.adminSess, aID, "requeue", "wrong-password-123"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if st, _, _, _ := p.payout(ambiguous); st != "failed" || audits() != 0 {
		t.Fatalf("refused requeue changed the payout: %s audits=%d", st, audits())
	}
	p.poll()
	if len(p.fake.Sends()) != 0 {
		t.Fatal("ambiguous failure sent again without confirmation")
	}

	// Confirmed: requeued, audited with the confirmation, recorded in the order history, sent once.
	if code, body := p.payoutActionConfirmed(p.adminSess, aID, "requeue", testPassword); code != 303 {
		t.Fatalf("confirmed requeue: %d %s", code, body)
	}
	if code, _ := p.payoutActionConfirmed(p.adminSess, aID, "requeue", testPassword); code != 409 {
		t.Fatalf("second requeue: %d", code)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout " + aID + " %administrator confirmed the wallet shows no broadcast transaction (confirmed with password)'"); n != 1 {
		t.Fatalf("confirmation not audited: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator confirmed the wallet shows no broadcast transaction%'", ambiguous); n != 1 {
		t.Fatalf("order history: %d", n)
	}
	if p.str("SELECT send_ambiguous::text FROM payouts WHERE id=$1", aID) != "false" {
		t.Fatal("requeue kept the ambiguity flag")
	}

	// A definite rejection requeues as before: password only.
	if code, body := p.payoutAction(p.adminSess, dID, "requeue", "", testPassword); code != 303 {
		t.Fatalf("definite requeue: %d %s", code, body)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Requeued payout " + dID + " %' AND action NOT LIKE '%outcome unknown%'"); n != 1 {
		t.Fatalf("definite requeue audit: %d", n)
	}
	p.poll()
	if sa, _, _, _ := p.payout(ambiguous); sa != "sent" {
		t.Fatalf("confirmed requeue: %s", sa)
	}
	if sd, _, _, _ := p.payout(definite); sd != "sent" || len(p.fake.Sends()) != 2 {
		t.Fatalf("definite requeue: %s sends=%d", sd, len(p.fake.Sends()))
	}
}

func TestPayoutSentButNotRecordedStaysSendingAndNeedsConfirmation(t *testing.T) {
	old := payoutRecordTimeout
	payoutRecordTimeout = 300 * time.Millisecond
	t.Cleanup(func() { payoutRecordTimeout = old })
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-unrecorded")
	id := p.payoutID(order)
	// The database stalls after the wallet broadcast: another session holds the payout row, so recording the
	// outcome times out.
	lock, err := p.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	p.fake.sendHook = func() {
		if _, err := lock.Exec("SELECT 1 FROM payouts WHERE id=$1 FOR UPDATE", id); err != nil {
			t.Error(err)
		}
	}
	err = p.A.pollOnce(context.Background())
	p.fake.sendHook = nil
	lock.Rollback()
	if err == nil || !strings.Contains(err.Error(), "payout "+id+" recorded as sending only") {
		t.Fatalf("unrecorded send not reported: %v", err)
	}
	if st, txid, _, _ := p.payout(order); st != "sending" || txid != "" || len(p.fake.Sends()) != 1 {
		t.Fatalf("after unrecorded send: %s %q sends=%d", st, txid, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%payout of%'", p.vendor.ID); n != 0 {
		t.Fatal("recipient notified of an unrecorded outcome")
	}
	p.poll()
	if len(p.fake.Sends()) != 1 {
		t.Fatal("payout left in sending was sent again")
	}
	// Once clearly stuck it is surfaced; it may have been broadcast, so requeue needs the confirmation, and
	// mark sent (the wallet shows the transaction) needs none.
	if _, err = p.DB.Exec("UPDATE payouts SET updated=now()-interval '1 hour' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	if !strings.Contains(admin, "Stuck in sending") || strings.Count(admin, `name="not_broadcast"`) != 1 {
		t.Fatal("stuck payout not shown with the broadcast confirmation")
	}
	if code, _ := p.payoutAction(p.adminSess, id, "requeue", "", testPassword); code != 400 {
		t.Fatalf("unconfirmed requeue of a stuck send: %d", code)
	}
	txid := strings.Repeat("ef", 32)
	if code, body := p.payoutAction(p.adminSess, id, "sent", txid, testPassword); code != 303 {
		t.Fatalf("mark sent: %d %s", code, body)
	}
	p.poll()
	if st, got, _, _ := p.payout(order); st != "sent" || got != txid || len(p.fake.Sends()) != 1 {
		t.Fatalf("reconciled payout: %s %q sends=%d", st, got, len(p.fake.Sends()))
	}
}

func TestPayoutAmbiguityMigrationBackfillsFailedRows(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	failed, _ := p.failPayout("tx-old-failed", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	queued := p.completedWithPayout("tx-old-queued")
	// A database from before migration 053: no column, not recorded as applied.
	if _, err := p.DB.Exec("ALTER TABLE payouts DROP COLUMN send_ambiguous; DELETE FROM schema_migrations WHERE version=53"); err != nil {
		t.Fatal(err)
	}
	p.restart()
	p.fake = newFakeProvider("BTC", 3)
	p.A.payments["BTC"] = p.fake
	// Failures recorded before classification may have been broadcast.
	if p.str("SELECT send_ambiguous::text FROM payouts WHERE order_id=$1", failed) != "true" || p.str("SELECT send_ambiguous::text FROM payouts WHERE order_id=$1", queued) != "false" {
		t.Fatal("migration did not backfill")
	}
	// Re-running the migration changes nothing.
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.DB.Exec("UPDATE payouts SET send_ambiguous=false WHERE order_id=$1", failed); err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Version == 53 {
			if _, err = p.DB.Exec(m.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	if p.str("SELECT send_ambiguous::text FROM payouts WHERE order_id=$1", failed) != "false" {
		t.Fatal("migration is not idempotent")
	}
}
