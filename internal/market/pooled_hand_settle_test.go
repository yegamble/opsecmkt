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
	// The refusal says plainly that the market queued, is sending or sent it itself (A-486, R2).
	for st, says := range map[string]string{
		"pending": "The market has queued this payout and will send it itself, so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.",
		"sending": "The market is sending this payout itself right now, so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.",
		"sent":    "This payout is already recorded as sent (by the market's wallet, or by an administrator), so a payment you made by hand would be a second payment. Reconcile with the recipient. Nothing was changed.",
	} {
		agExec(p.testEnv, "UPDATE payouts SET state=$2,address='fake-testnet-vendor',updated=now() WHERE id=$1::bigint", id, st)
		if code, body := p.payoutAction(p.adminSess, id, "sent", txid, testPassword); code != 409 || !strings.Contains(body, says) {
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

// holdPayout submits op=hold as session with note (password testPassword unless password is given).
func (p *payEnv) holdPayout(session, id, note string, password ...string) (int, string) {
	p.t.Helper()
	pw := testPassword
	if len(password) > 0 {
		pw = password[0]
	}
	w := p.do("POST", "/admin/payout", session, url.Values{"payout_id": {id}, "op": {"hold"}, "note": {note}, "password": {pw}})
	return w.Code, w.Body.String()
}

const handNote = "Recipient asked for payment to a fresh address by PGP mail."

// R2 (A-486; QA445's TestQA445HandSettleDoublePayWindow reworked): the safe hand payment is hold, then pay, then record.
// A held payout keeps its hold when the recipient saves an address meanwhile, so the watcher never sends it; the
// record then marks it sent by hand.
func TestPooledHandSettleDoublePayWindow(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("window-paid")
	if code, body := p.holdPayout(p.adminSess, id, handNote); code != 303 {
		t.Fatalf("hold: %d %s", code, body)
	}
	if st, _, addr, _ := p.payout(order); st != "held" || addr != "" {
		t.Fatalf("after hold: %s %q", st, addr)
	}
	if e := p.str("SELECT error FROM payouts WHERE id=$1::bigint", id); e != handHoldPrefix+" "+handNote {
		t.Fatalf("hold reason %q", e)
	}
	// (the administrator pays it by hand here, outside the market) The recipient saves an address meanwhile.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
	p.poll()
	p.poll()
	if st, _, addr, _ := p.payout(order); st != "held" || addr != "fake-testnet-vendor" || len(p.fake.Sends()) != 0 {
		t.Fatalf("after the address was saved: %s %q sends=%d", st, addr, len(p.fake.Sends()))
	}
	admin := p.page("/admin", p.adminSess)
	mustContain(t, admin, "Held to be paid by hand — record it sent once paid, or release it", `name="not_paid"`, "Record sent by hand")
	txid := strings.Repeat("9f", 32)
	if code, body := p.payoutAction(p.adminSess, id, "sent", txid, testPassword); code != 303 {
		t.Fatalf("record: %d %s", code, body)
	}
	p.poll()
	if st, got, _, _ := p.payout(order); st != "sent" || got != txid || len(p.fake.Sends()) != 0 {
		t.Fatalf("after the record: %s %q sends=%d", st, got, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%held by an administrator to be paid by hand%' AND note NOT LIKE '%PGP%'", order); n != 1 {
		t.Fatalf("hold order note: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%recorded as sent by hand%'", order); n != 1 {
		t.Fatalf("record order note: %d", n)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Held blocked payout "+id+" %to pay it by hand: '||$1||'%'", handNote); n != 1 {
		t.Fatalf("hold audit: %d", n)
	}
	wantRows(t, "drain after hold and record", drainRows(t, p.testEnv, "unsent_payout"))
}

// Hold is for a blocked payout only, by an administrator with the password and a note of 10-500 characters; every
// refusal changes nothing, and a repeat answers 409.
func TestPooledHandHoldRefusals(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hold-refused")
	audits := p.count("SELECT count(*) FROM audit_events")
	unchanged := func(what string) {
		t.Helper()
		if st, _, _, _ := p.payout(order); st != "blocked" || p.count("SELECT count(*) FROM audit_events") != audits {
			t.Fatalf("%s changed something: %s", what, st)
		}
	}
	for _, s := range []string{p.vendorSess, p.buyerSess} {
		if code, _ := p.holdPayout(s, id, handNote); code != 403 {
			t.Fatalf("non-administrator: %d", code)
		}
	}
	unchanged("a non-administrator")
	if code, _ := p.holdPayout(p.adminSess, id, handNote, "wrong-password-123"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	unchanged("a wrong password")
	for _, note := range []string{"", "   ", "too short", strings.Repeat("n", 501)} {
		if code, body := p.holdPayout(p.adminSess, id, note); code != 400 || !strings.Contains(body, "10–500 characters") {
			t.Fatalf("note %q: %d %s", note, code, body)
		}
	}
	unchanged("a missing or bad note")
	if code, _ := p.holdPayout(p.adminSess, id, strings.Repeat("é", 500)); code != 303 { // characters, not bytes
		t.Fatalf("500-character note: %d", code)
	}
	audits++
	if code, body := p.holdPayout(p.adminSess, id, handNote); code != 409 || !strings.Contains(body, "Only a blocked payout") || !strings.Contains(body, "Nothing was changed") {
		t.Fatalf("repeat: %d %s", code, body)
	}
	for _, st := range []string{"pending", "failed", "sent"} {
		agExec(p.testEnv, "UPDATE payouts SET state=$2,address='fake-testnet-vendor',error='' WHERE id=$1::bigint", id, st)
		if code, _ := p.holdPayout(p.adminSess, id, handNote); code != 409 {
			t.Fatalf("hold of a %s payout: %d", st, code)
		}
		if got, _, _, _ := p.payout(order); got != st || p.count("SELECT count(*) FROM audit_events") != audits {
			t.Fatalf("hold of a %s payout changed it", st)
		}
	}
}

// Hold against the recipient's address save. When the save holds the row first (blocked to pending), the hold waits
// for its lock and then answers 409, changing nothing, and the watcher sends the payout; when the hold commits first,
// the save keeps the hold and the watcher sends nothing. Concurrent holds hold it once.
func TestPooledHandHoldRacesAddressSave(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hold-race-save")
	ctx := context.Background()
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE payouts SET address='fake-testnet-vendor',state='pending',error='' WHERE id=$1::bigint AND state='blocked'", id); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := p.holdPayout(p.adminSess, id, handNote)
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("hold did not wait for the row lock: %d", code)
	case <-time.After(500 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 409 {
		t.Fatalf("hold after the address was saved: %d", code)
	}
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("the watcher's send: %s sends=%d", st, len(p.fake.Sends()))
	}

	// Hold first, then the real address save: the hold stays.
	agExec(p.testEnv, "UPDATE users SET payout_btc='' WHERE id=$1", p.vendor.ID)
	order2, id2 := p.blockedRelease("hold-race-first")
	rs := agRace(cap(passwordWork), func(int) (int, string) { return p.holdPayout(p.adminSess, id2, handNote) })
	if tally := agTally(t, rs, 303, 409); tally[303] != 1 {
		t.Fatalf("concurrent holds %v", tally)
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
	p.poll()
	if st, _, addr, _ := p.payout(order2); st != "held" || addr != "fake-testnet-vendor" || len(p.fake.Sends()) != 1 {
		t.Fatalf("held payout after the save: %s %q sends=%d", st, addr, len(p.fake.Sends()))
	}
}

// Releasing a payout held to be paid by hand needs the confirmation that nobody paid it by hand (409 without it,
// nothing changed); with it the payout goes back to blocked (no address) and is sent once an address is saved.
func TestPooledHandHoldReleaseNeedsConfirmation(t *testing.T) {
	p := newPayEnv(t)
	order, id := p.blockedRelease("hold-release")
	if code, body := p.holdPayout(p.adminSess, id, handNote); code != 303 {
		t.Fatalf("hold: %d %s", code, body)
	}
	if code, body := p.payoutAction(p.adminSess, id, "release", "", testPassword); code != 409 || !strings.Contains(body, "release it only if nobody paid it by hand") {
		t.Fatalf("release without the confirmation: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(order); st != "held" {
		t.Fatalf("refused release changed the payout: %s", st)
	}
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "not_paid": {"confirmed"}})
	p.check(w, 303)
	if st, _, _, _ := p.payout(order); st != "blocked" {
		t.Fatalf("released hand hold: %s", st)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Released held payout " + id + " %administrator confirmed it was not paid by hand%'"); n != 1 {
		t.Fatalf("release audit: %d", n)
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-vendor"}, "password": {testPassword}}), 303)
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("after the release and the address: %s sends=%d", st, len(p.fake.Sends()))
	}
}
