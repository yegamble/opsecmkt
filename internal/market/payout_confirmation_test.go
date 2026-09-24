package market

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-161 / A-122: every action that queues a payout (resolve, complete, a vendor's cancel of a paid order) states
// the amount payoutBasis would pay, the recipient and the difference from the price, needs a ticked confirmation
// and carries amount_seen, which enqueuePayout compares with the amount it counts under its row locks.

// payoutConfirmed adds the confirmation fields those forms carry.
func payoutConfirmed(v url.Values, seen int64) url.Values {
	v.Set("amount_seen", strconv.FormatInt(seen, 10))
	v.Set("payout_confirmed", "confirmed")
	return v
}

var amountSeenField = regexp.MustCompile(`name="amount_seen" value="(\d+)"`)

// amountSeen returns the amount_seen values rendered on a page, in order.
func amountSeen(t *testing.T, page string) []int64 {
	t.Helper()
	var out []int64
	for _, m := range amountSeenField.FindAllStringSubmatch(page, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// disputedOrder pays the fixture order with deposit (satoshi; the price is 100000) and opens a dispute on it
// as the buyer. It returns the dispute id and a fresh moderator session.
func (p *payEnv) disputedOrder(deposit int64) (dispute, modSess string) {
	p.t.Helper()
	p.fake.Deposit(p.addr, "tx-"+randomToken()[:8], 0, deposit, 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		p.t.Fatalf("state %s", s)
	}
	p.check(p.do("POST", "/disputes", p.buyerSess, url.Values{"order_id": {p.order}, "reason": {"The parcel never arrived; please review the order."}}), 303)
	_, modSess = p.user("a161mod_"+randomToken()[:6], "moderator")
	if err := p.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", p.order).Scan(&dispute); err != nil {
		p.t.Fatal(err)
	}
	return dispute, modSess
}

func TestResolveStatesPayoutAmountAndNeedsConfirmation(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	// The buyer sends ten times the price (0.001 BTC) and has no payout address.
	dispute, modSess := p.disputedOrder(1000000)
	desk := p.page("/moderator", modSess)
	mustContain(t, desk,
		"Release 0.01 BTC (test network) to "+p.vendor.Handle+" — 0.009 BTC more than the price",
		"Refund 0.01 BTC (test network) to "+p.buyer.Handle+" — 0.009 BTC more than the price; no payout address: the payout waits for one",
		`name="payout_confirmed" value="confirmed" required`,
		"Resolving queues one payout of the whole counted amount to one party; the payment watcher sends it automatically. There is no split.",
		"Never include an address, real name or tracking number",
		`aria-describedby="resolution-help-`+dispute+`"`)
	mustNotContain(t, desk, "moves no funds")
	if seen := amountSeen(t, desk); len(seen) != 1 || seen[0] != 1000000 {
		t.Fatalf("amount_seen %v", seen)
	}
	if _, err := p.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	mustContain(t, p.page("/moderator", modSess), "to "+p.vendor.Handle+" — 0.009 BTC more than the price; account suspended: an administrator must check the address")
	if _, err := p.DB.Exec("UPDATE users SET suspended_at=NULL WHERE id=$1", p.vendor.ID); err != nil {
		t.Fatal(err)
	}

	decision := url.Values{"id": {dispute}, "outcome": {"release"}, "resolution": {"Shipped with proof; release to the vendor."}}
	// No confirmation: refused, nothing changes.
	w := p.do("POST", "/resolve", modSess, url.Values{"id": decision["id"], "outcome": decision["outcome"], "resolution": decision["resolution"], "amount_seen": {"1000000"}})
	p.check(w, 400)
	if st := p.state(p.order); st != stateDisputed || p.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome=''", dispute) != 1 || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
		t.Fatalf("unconfirmed resolve changed state %s", st)
	}
	// A missing amount_seen is refused too.
	missing := url.Values{"id": decision["id"], "outcome": decision["outcome"], "resolution": decision["resolution"], "payout_confirmed": {"confirmed"}}
	p.check(p.do("POST", "/resolve", modSess, missing), 400)
	p.check(p.do("POST", "/resolve", modSess, payoutConfirmed(decision, 1000000)), 303)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-vendor" || amt != 1000000 {
		t.Fatalf("payout %s %s %d", st, addr, amt)
	}
	// The release names the excess to both parties.
	for _, u := range []*User{p.buyer, p.vendor} {
		if p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%0.009 BTC more than the price%'", u.ID) != 1 {
			t.Fatalf("release notification to %s does not name the excess", u.Handle)
		}
	}

	// Nothing counted (address issued, no deposit): the outcome says so and queues nothing.
	empty, _ := p.newOrder(stateDisputed)
	emptyDispute := randomToken()
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Nothing arrived and nothing was paid either.')", emptyDispute, empty); err != nil {
		t.Fatal(err)
	}
	desk = p.page("/moderator", modSess)
	mustContain(t, desk, "Release (test network) to "+p.vendor.Handle+" — nothing counted yet; deposits confirming later go to the chosen party",
		"Refund (test network) to "+p.buyer.Handle+" — nothing counted yet; deposits confirming later go to the chosen party; no payout address: the payout waits for one")
	p.check(p.do("POST", "/resolve", modSess, payoutConfirmed(url.Values{"id": {emptyDispute}, "outcome": {"refund"}, "resolution": {"Nothing was paid; close the order."}}, 0)), 303)
	if p.state(empty) != stateResolved || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", empty) != 0 {
		t.Fatal("empty resolve")
	}
}

func TestResolveRefusesChangedAmount(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	dispute, modSess := p.disputedOrder(100000)
	seen := amountSeen(t, p.page("/moderator", modSess))
	if len(seen) != 1 || seen[0] != 100000 {
		t.Fatalf("amount_seen %v", seen)
	}
	// Another confirmed deposit arrives between the render and the POST.
	p.fake.Deposit(p.addr, "tx-extra", 0, 50000, 3)
	p.poll()
	decision := url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"No proof of shipment; refund the buyer."}}
	w := p.do("POST", "/resolve", modSess, payoutConfirmed(decision, seen[0]))
	p.check(w, 409)
	if !strings.Contains(w.Body.String(), "The amount changed to 0.0015 BTC — review again") {
		t.Fatalf("body %q", w.Body.String())
	}
	if st := p.state(p.order); st != stateDisputed || p.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome=''", dispute) != 1 || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
		t.Fatalf("changed-amount resolve changed state %s", st)
	}
	seen = amountSeen(t, p.page("/moderator", modSess))
	if len(seen) != 1 || seen[0] != 150000 {
		t.Fatalf("amount_seen after the deposit %v", seen)
	}
	p.check(p.do("POST", "/resolve", modSess, payoutConfirmed(decision, seen[0])), 303)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-buyer" || amt != 150000 {
		t.Fatalf("payout %s %s %d", st, addr, amt)
	}
}

// Invariant 4: repeated and concurrent resolutions of one dispute queue exactly one payout.
func TestConcurrentResolveQueuesOnePayout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	dispute, modSess := p.disputedOrder(100000)
	_, otherMod := p.user("a161mod2_"+randomToken()[:6], "moderator")
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := range 8 {
		sess := modSess
		if i%2 == 1 {
			sess = otherMod
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- p.do("POST", "/resolve", sess, payoutConfirmed(url.Values{"id": {dispute}, "outcome": {"release"}, "resolution": {"Shipped with proof; release to the vendor."}}, 100000)).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[303] != 1 || got[409] != 7 || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 1 {
		t.Fatalf("status counts %v", got)
	}
	// A repeated POST after the resolution changes nothing.
	p.check(p.do("POST", "/resolve", modSess, payoutConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Changed my mind; refund the buyer instead."}}, 100000)), 409)
	p.poll()
	p.poll()
	if sends := p.fake.Sends(); len(sends) != 1 || sends[0].Amount != 100000 || sends[0].To != "fake-testnet-vendor" {
		t.Fatalf("sends %+v", sends)
	}
}

func TestCompleteNeedsConfirmationAndUnchangedAmount(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "tx-paid")
	p.check(p.do("POST", "/orders/ship", p.vendorSess, url.Values{"order_id": {p.order}}), 303)
	page := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, "Release 0.001 BTC (test network) to "+p.vendor.Handle+" — exactly the price. This is final.", `name="payout_confirmed" value="confirmed" required`)
	if seen := amountSeen(t, page); len(seen) != 1 || seen[0] != 100000 {
		t.Fatalf("amount_seen %v", seen)
	}
	unchanged := func(label string) {
		t.Helper()
		if st := p.state(p.order); st != stateShipped || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
			t.Fatalf("%s: state %s", label, st)
		}
	}
	p.check(p.do("POST", "/orders/complete", p.buyerSess, url.Values{"order_id": {p.order}, "amount_seen": {"100000"}}), 400)
	unchanged("missing confirmation")
	p.fake.Deposit(p.addr, "tx-extra", 0, 900000, 3)
	p.poll()
	w := p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {p.order}}, 100000))
	p.check(w, 409)
	if !strings.Contains(w.Body.String(), "The amount changed to 0.01 BTC — review again") {
		t.Fatalf("body %q", w.Body.String())
	}
	unchanged("changed amount")
	page = p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, "Release 0.01 BTC (test network) to "+p.vendor.Handle+" — 0.009 BTC more than the price. This is final.")
	p.check(p.do("POST", "/orders/complete", p.buyerSess, payoutConfirmed(url.Values{"order_id": {p.order}}, 1000000)), 303)
	if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 1000000 {
		t.Fatalf("payout %s %d", st, amt)
	}
	if p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%0.009 BTC more than the price%'", p.vendor.ID) != 1 {
		t.Fatal("release notification does not name the excess")
	}
}

func TestVendorCancelOfPaidOrderNeedsConfirmation(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcher(p.order, p.addr, "tx-paid")
	page := p.page("/order?id="+p.order, p.vendorSess)
	mustContain(t, page, "Refund 0.001 BTC (test network) to "+p.buyer.Handle+" — exactly the price. This is final.")
	unchanged := func(label string) {
		t.Helper()
		if st := p.state(p.order); st != statePaid || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 0 {
			t.Fatalf("%s: state %s", label, st)
		}
	}
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, url.Values{"order_id": {p.order}, "from": {statePaid}, "amount_seen": {"100000"}}), 400)
	unchanged("missing confirmation")
	p.fake.Deposit(p.addr, "tx-extra", 0, 20000, 3)
	p.poll()
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(url.Values{"order_id": {p.order}, "from": {statePaid}}, 100000)), 409)
	unchanged("changed amount")
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(url.Values{"order_id": {p.order}, "from": {statePaid}}, 120000)), 303)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-buyer" || amt != 120000 {
		t.Fatalf("refund %s %s %d", st, addr, amt)
	}
}

func TestOverpaidOrderPagesWarnBothParties(t *testing.T) {
	p := newPayEnv(t)
	buyerCopy := "Ask the vendor to cancel and refund everything, then order again. After shipping, a dispute sends the whole received amount to one party; there is no split."
	p.paidByWatcher(p.order, p.addr, "tx-paid")
	mustNotContain(t, p.page("/order?id="+p.order, p.buyerSess), buyerCopy, "more than the price (test network)")
	p.fake.Deposit(p.addr, "tx-extra", 0, 900000, 3)
	p.poll()
	buyerPage := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, buyerPage, "You sent 0.009 BTC more than the price (test network).", buyerCopy)
	vendorPage := p.page("/order?id="+p.order, p.vendorSess)
	mustContain(t, vendorPage, "The buyer sent 0.009 BTC more than the price (test network).", "Cancelling this paid order refunds every deposit to the buyer", "there is no split")
	mustNotContain(t, vendorPage, "Ask the vendor to cancel")
}

func TestNoTemplateSaysResolvingMovesNoFunds(t *testing.T) {
	files, err := filepath.Glob("web/templates/*/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates %v %v", files, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "moves no funds") {
			t.Errorf("%s still says a money-moving form moves no funds", f)
		}
	}
}
