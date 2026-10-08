package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-34: hold a queued payout before paying by hand. The row lock and the
// watcher's pending-to-sending claim decide which action wins.
func TestPooledPendingHold(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE users SET payout_btc='fake-testnet-vendor' WHERE id=$1", p.vendor.ID)
	order := p.completedWithPayout("pending-hand-hold")
	id := p.payoutID(order)
	if st, _, _, _ := p.payout(order); st != "pending" {
		t.Fatalf("before hold: %s", st)
	}
	mustContain(t, p.page("/admin", p.adminSess), `id="resolve-payout-`+id+`"`, "Hold to pay by hand", "The market may start sending this queued payout")
	if code, body := p.holdPayout(p.adminSess, id, handNote); code != 303 {
		t.Fatalf("hold: %d %s", code, body)
	}
	p.poll()
	p.poll()
	if st, _, addr, _ := p.payout(order); st != "held" || addr != "fake-testnet-vendor" || len(p.fake.Sends()) != 0 {
		t.Fatalf("held: %s %q sends=%d", st, addr, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Held pending payout " + id + " %'"); n != 1 {
		t.Fatalf("hold audit: %d", n)
	}
	if code, _ := p.payoutAction(p.adminSess, id, "release", "", testPassword); code != 409 {
		t.Fatalf("release without wallet confirmation: %d", code)
	}
	p.check(p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "not_paid": {"confirmed"}}), 303)
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 1 {
		t.Fatalf("released: %s sends=%d", st, len(p.fake.Sends()))
	}
}

func TestPooledPendingHoldRecordsByHand(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE users SET payout_btc='fake-testnet-vendor' WHERE id=$1", p.vendor.ID)
	order := p.completedWithPayout("pending-hand-record")
	id := p.payoutID(order)
	if code, body := p.holdPayout(p.adminSess, id, handNote); code != 303 {
		t.Fatalf("hold: %d %s", code, body)
	}
	if code, body := p.payoutAction(p.adminSess, id, "sent", strings.Repeat("ab", 32), testPassword); code != 303 {
		t.Fatalf("record: %d %s", code, body)
	}
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" || len(p.fake.Sends()) != 0 {
		t.Fatalf("recorded payout: %s sends=%d", st, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%recorded as sent by hand%' AND note NOT LIKE '%no payout address%'", order); n != 1 {
		t.Fatalf("history incorrectly says this queued payout lacked an address: %d", n)
	}
}

func TestPooledPendingHoldLosesToSendClaim(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE users SET payout_btc='fake-testnet-vendor' WHERE id=$1", p.vendor.ID)
	order := p.completedWithPayout("pending-claim-first")
	id := p.payoutID(order)
	tx, err := p.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Hold the same row lock the watcher's atomic send claim takes, before it
	// commits. The form must wait, then read sending and refuse.
	if _, err = tx.Exec("UPDATE payouts SET state='sending',updated=now() WHERE id=$1::bigint AND state='pending'", id); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { code, _ := p.holdPayout(p.adminSess, id, handNote); done <- code }()
	select {
	case code := <-done:
		t.Fatalf("hold did not wait for the send claim: %d", code)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 409 {
			t.Fatalf("hold after sending claim: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hold did not finish after send claim committed")
	}
	if st, _, _, _ := p.payout(order); st != "sending" {
		t.Fatalf("send claim was changed: %s", st)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Held %payout " + id + " %'"); n != 0 {
		t.Fatalf("refusal audited success: %d", n)
	}
}

func TestPooledPendingHoldsNeverHidden(t *testing.T) {
	p := newPayEnv(t)
	var ids []string
	for i := range 60 {
		o := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateCompleted)
		var id string
		if err := p.DB.QueryRow(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
			VALUES($1,'release',$2,'BTC',100000,'fake-testnet-vendor','pending') RETURNING id::text`, o, p.vendor.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	body := p.page("/admin", p.adminSess)
	for _, id := range ids {
		mustContain(t, body, `id="resolve-payout-`+id+`"`)
	}
	// Repeated simultaneous forms hold the first queued payout once.
	rs := agRace(cap(passwordWork), func(int) (int, string) { return p.holdPayout(p.adminSess, ids[0], handNote) })
	if tally := agTally(t, rs, 303, 409); tally[303] != 1 {
		t.Fatalf("concurrent holds %v", tally)
	}
}
