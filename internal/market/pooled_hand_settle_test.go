package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-445 (ARCH-12): a blocked payout (its recipient saved no payout address, so the watcher can never send it) can be
// recorded as sent by hand: Mark sent on /admin with the wallet's transaction ID (64 hex characters, stored in lower
// case), the administrator's password and an audit row naming the payout and the transaction. Its state is read under
// a row lock in the action's transaction, and only a blocked, held or failed payout or a stuck send is changed: a
// pending payout, which the watcher may claim at any moment, or a live send never is.

// blockedRelease completes a paid order whose vendor saved no payout address and returns its blocked release's id.
func (p *payEnv) blockedRelease(txid string) (order, id string) {
	p.t.Helper()
	order = p.completedWithPayout(txid)
	if st, _, addr, _ := p.payout(order); st != "blocked" || addr != "" {
		p.t.Fatalf("release %s %q, want blocked", st, addr)
	}
	return order, p.payoutID(order)
}

func TestPooledHandSettleBlockedPayout(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hand-paid")
	txid := strings.Repeat("Ab", 32)
	// The admin page offers it, saying that the market does not check the transaction against the chain.
	admin := p.page("/admin", p.adminSess)
	mustContain(t, admin, `href="#resolve-payout-`+id+`"`, `id="resolve-payout-`+id+`"`, "Record sent by hand", "does not check the transaction ID against the chain")
	before := p.count("SELECT count(*) FROM audit_events")
	events := p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)
	unchanged := func(what string) {
		t.Helper()
		if st, got, _, _ := p.payout(order); st != "blocked" || got != "" || p.count("SELECT count(*) FROM audit_events") != before ||
			p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != events {
			t.Fatalf("%s changed something: %s %q", what, st, got)
		}
	}
	// Denied: a non-administrator (403), a wrong password (401), a malformed transaction ID (400).
	for _, s := range []string{p.vendorSess, p.buyerSess} {
		if code, _ := p.payoutAction(s, id, "sent", txid, testPassword); code != 403 {
			t.Fatalf("non-administrator: %d", code)
		}
	}
	unchanged("a non-administrator")
	if code, _ := p.payoutAction(p.adminSess, id, "sent", txid, "wrong-password-123"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	unchanged("a wrong password")
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if code, _ := p.payoutAction(p.adminSess, id, "sent", bad, testPassword); code != 400 {
			t.Fatalf("txid %q: %d", bad, code)
		}
	}
	unchanged("a malformed transaction ID")

	if code, body := p.payoutAction(p.adminSess, id, "sent", " "+txid+" ", testPassword); code != 303 {
		t.Fatalf("hand settlement: %d %s", code, body)
	}
	lower := strings.ToLower(txid)
	if st, got, _, _ := p.payout(order); st != "sent" || got != lower {
		t.Fatalf("payout %s %q", st, got)
	}
	audit := p.str("SELECT action FROM audit_events ORDER BY id DESC LIMIT 1")
	for _, want := range []string{"payout " + id + " ", "was blocked", "sent with transaction " + lower, "by hand", "(confirmed with password)"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("audit %q lacks %q", audit, want)
		}
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%recorded as sent by hand by an administrator%' AND note NOT LIKE '%'||$2||'%'", order, lower); n != 1 {
		t.Fatalf("order history: %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%recorded as sent by hand%Transaction ID: '||$2", p.vendor.ID, lower); n != 1 {
		t.Fatalf("recipient notification: %d", n)
	}
	wantRows(t, "drain after hand settlement", drainRows(t, p.testEnv, "unsent_payout"))

	// A repeat answers 409 and changes nothing.
	audits, events := p.count("SELECT count(*) FROM audit_events"), p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)
	if code, body := p.payoutAction(p.adminSess, id, "sent", strings.Repeat("cd", 32), testPassword); code != 409 || !strings.Contains(body, "Nothing was changed") {
		t.Fatalf("repeat: %d %s", code, body)
	}
	if st, got, _, _ := p.payout(order); st != "sent" || got != lower || p.count("SELECT count(*) FROM audit_events") != audits ||
		p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != events {
		t.Fatalf("repeat changed the payout: %s %q", st, got)
	}
	// The recipient saving an address later never sends it again.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
	p.poll()
	if st, got, _, _ := p.payout(order); st != "sent" || got != lower || len(p.fake.Sends()) != 0 {
		t.Fatalf("after the address was saved: %s %q sends=%d", st, got, len(p.fake.Sends()))
	}
}

// A payout the watcher may be sending, or may claim at any moment, is never recorded as sent: pending (claimed by
// compare-and-set before each wallet call) and a send under way answer 409 and change nothing.
func TestPooledHandSettleRefusesWhatTheSenderMaySend(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hand-pending")
	txid := strings.Repeat("ef", 32)
	for _, st := range []string{"pending", "sending"} {
		agExec(p.testEnv, "UPDATE payouts SET state=$2,address='fake-testnet-vendor',updated=now() WHERE id=$1::bigint", id, st)
		if code, body := p.payoutAction(p.adminSess, id, "sent", txid, testPassword); code != 409 || !strings.Contains(body, "Nothing was changed") {
			t.Fatalf("%s: %d %s", st, code, body)
		}
		if got, _, _, _ := p.payout(order); got != st {
			t.Fatalf("%s payout became %s", st, got)
		}
	}
}

// The state is read under the payout's row lock in the action's transaction: a hand settlement submitted while another
// transaction holds the row (the recipient saving an address, which moves blocked to pending) waits for it and then
// answers 409, changing nothing, rather than marking a payout the watcher is about to send.
func TestPooledHandSettleWaitsForTheRowLock(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hand-lock")
	ctx := context.Background()
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE payouts SET address='fake-testnet-vendor',state='pending' WHERE id=$1::bigint AND state='blocked'", id); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := p.payoutAction(p.adminSess, id, "sent", strings.Repeat("12", 32), testPassword)
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("hand settlement did not wait for the row lock: %d", code)
	case <-time.After(500 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 409 {
		t.Fatalf("after the address was saved: %d", code)
	}
	if st, got, _, _ := p.payout(order); st != "pending" || got != "" {
		t.Fatalf("payout %s %q", st, got)
	}
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("the watcher's send: %s sends=%d", st, len(p.fake.Sends()))
	}
}

// Concurrent submissions settle it once: one 303, the rest 409, one audit row. Four at once, the password-hashing
// slots (passwordWork): a fifth would be refused as busy (503) before reaching the payout.
func TestPooledHandSettleConcurrentOnce(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hand-race")
	rs := agRace(cap(passwordWork), func(i int) (int, string) {
		return p.payoutAction(p.adminSess, id, "sent", strings.Repeat(string("0123456789"[i]), 64), testPassword)
	})
	tally := agTally(t, rs, 303, 409)
	if tally[303] != 1 {
		t.Fatalf("results %v", tally)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Marked payout " + id + " %'"); n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%by hand%'", order); n != 1 {
		t.Fatalf("order events: %d", n)
	}
}
