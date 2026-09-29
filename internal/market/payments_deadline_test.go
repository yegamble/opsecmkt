package market

import (
	"net/url"
	"regexp"
	"testing"
	"time"
)

// A-123: the buyer's awaiting-payment page states when the watcher can cancel the order, from the same inputs as
// its expiry query (settleOrder): the later of the address's issue time and the latest deposit that counts (not
// conflicted, not locked), plus PAYMENT_EXPIRY. System and moderator moves tell the parties why.

const cancellationSentence = ", it can be cancelled; a new deposit moves this time; anything sent after cancellation is not refunded automatically."

func cancelAfter(t time.Time) string {
	return "If the order is still unpaid or short at " + t.UTC().Truncate(time.Minute).Format("2006-01-02 15:04") + " UTC" + cancellationSentence
}

func TestAwaitingPageStatesCancellationTime(t *testing.T) {
	t.Setenv("PAYMENT_EXPIRY", "2h")
	p := newPayEnv(t)
	issued := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if _, err := p.DB.Exec("UPDATE payment_addresses SET created=$2 WHERE order_id=$1", p.order, issued); err != nil {
		t.Fatal(err)
	}
	// Unpaid: the address's issue time plus the payment window.
	page := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, cancelAfter(issued.Add(2*time.Hour)),
		"its confirmed deposits are refunded to your saved BTC payout address (the refund waits until one is saved); if together they are below the minimum automatic refund of 0.0001 BTC, they are not refunded automatically and market staff are notified.")
	// A-233(c): no unconditional refund promise (a total below the floor is flagged for staff, A-99/A-211).
	mustNotContain(t, page, "it is cancelled and what you sent is refunded")
	mustContain(t, p.page("/order?id="+p.order, p.vendorSess), cancelAfter(issued.Add(2*time.Hour)), "refunded to the buyer's saved BTC payout address")

	// Underpaid: the latest counted deposit moves the time; a conflicted or locked deposit does not.
	p.fake.Deposit(p.addr, "tx-deadline-a", 0, 20000, 3)
	p.fake.Deposit(p.addr, "tx-deadline-b", 0, 20000, 3)
	p.poll()
	if s := p.state(p.order); s != stateAwaitingPayment {
		t.Fatalf("underpaid order moved to %s", s)
	}
	first, latest := issued.Add(5*time.Minute), issued.Add(17*time.Minute+40*time.Second)
	for txid, at := range map[string]time.Time{"tx-deadline-a": first, "tx-deadline-b": latest} {
		if _, err := p.DB.Exec("UPDATE payments SET created=$3 WHERE order_id=$1 AND txid=$2", p.order, txid, at); err != nil {
			t.Fatal(err)
		}
	}
	for i, q := range []string{
		"INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,created) VALUES($1,'BTC','tx-deadline-conflicted',0,$2,10000,-1,now())",
		"INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked,flagged,created) VALUES($1,'BTC','tx-deadline-locked',0,$2,10000,3,true,true,now())",
	} {
		if _, err := p.DB.Exec(q, p.order, p.addr); err != nil {
			t.Fatalf("deposit %d: %v", i, err)
		}
	}
	page = p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, cancelAfter(latest.Add(2*time.Hour)), "Send <strong>0.0006")
	mustNotContain(t, page, cancelAfter(issued.Add(2*time.Hour)), cancelAfter(time.Now().Add(2*time.Hour)))
	// Read-only moderators see no instructions for the buyer.
	mustNotContain(t, p.page("/order?id="+p.order, p.adminSess), cancellationSentence)

	// Once the order is closed the time is gone.
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	mustNotContain(t, p.page("/order?id="+p.order, p.buyerSess), cancellationSentence)
}

var absoluteTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}|\d{1,2}:\d{2}`)

func TestSystemAndModeratorTransitionsNotifyReason(t *testing.T) {
	p := newPayEnv(t)
	notices := func(u *User, like string) int {
		t.Helper()
		return p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", u.ID, like)
	}
	short := func(order string) string { return order[:8] }

	// A backdated unpaid order: the watcher's cancellation says why.
	unpaid, _ := p.newOrder(stateAwaitingPayment)
	p.expireAddress(unpaid)
	// An underpaid order: the reason names the amounts.
	underpaid, underAddr := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(underAddr, "tx-reason-under", 0, 40000, 3)
	p.poll()
	p.ageDeposits(underpaid)
	p.poll()
	if p.state(unpaid) != stateCancelled || p.state(underpaid) != stateCancelled {
		t.Fatalf("states %s %s", p.state(unpaid), p.state(underpaid))
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := notices(u, "Order "+short(unpaid)+" is now Cancelled — payment window expired"); n != 1 {
			t.Fatalf("%s: expiry notices with the reason %d", u.Handle, n)
		}
		if n := notices(u, "Order "+short(underpaid)+" is now Cancelled — payment window expired underpaid: 0.0004 of 0.001 BTC received"); n != 1 {
			t.Fatalf("%s: underpaid expiry notices with the reason %d", u.Handle, n)
		}
	}

	// A moderator's resolution names the outcome to both parties; the buyer's own dispute stays a plain notice.
	dispute, modSess := p.disputedOrder(100000)
	if n := notices(p.vendor, "Order "+short(p.order)+" is now Disputed. You may put one reply to the dispute on record from the order page before a moderator decides."); n != 1 {
		t.Fatalf("party move notice changed: %d", n)
	}
	p.check(p.do("POST", "/resolve", modSess, resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Nothing was shipped; refund the buyer in full."}}, 100000)), 303)
	for _, u := range []*User{p.buyer, p.vendor} {
		if n := notices(u, "Order "+short(p.order)+" is now Resolved — refund to buyer"); n != 1 {
			t.Fatalf("%s: resolve notices with the outcome %d", u.Handle, n)
		}
	}
	released, relAddr := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(released, relAddr, "tx-reason-release")
	p.move(released, statePaid, stateDisputed, p.buyer)
	var relDispute string
	if err := p.DB.QueryRow("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parcel never arrived; please review it.') RETURNING id", randomToken(), released).Scan(&relDispute); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/resolve", modSess, resolveConfirmed(url.Values{"id": {relDispute}, "outcome": {"release"}, "resolution": {"Tracking shows delivery; release to the vendor."}}, 100000)), 303)
	if n := notices(p.buyer, "Order "+short(released)+" is now Resolved — release to vendor"); n != 1 {
		t.Fatalf("release resolve notices %d", n)
	}

	// No notification states an absolute time (the order page does).
	rows, err := p.DB.Query("SELECT body FROM notifications WHERE user_id IN ($1,$2) AND body LIKE '% is now %'", p.buyer.ID, p.vendor.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		if absoluteTime.MatchString(body) {
			t.Fatalf("notification states a time: %q", body)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
