package market

import (
	"html"
	"maps"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-710: the vendor desk's "Waiting for you" section, its payout column and filter, the Monero escrow tile, and the
// claim cue in the notices of a decision or concession for the vendor. DB integration tests against the simulated
// wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not chain evidence).

// deskSection is the HTML of the vendor desk element with id, through the end of its table (or the section's end).
func deskSection(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("the vendor desk has no %q:\n%s", id, body)
	}
	s := body[start:]
	if end := strings.Index(s, "</table>"); end >= 0 {
		s = s[:end]
	}
	return s
}

// deskRow is the text of the table row of section that links order ("" when there is none).
func deskRow(section, order string) string {
	for _, row := range strings.Split(section, "<tr>")[1:] {
		if strings.Contains(row, "/order?id="+order) {
			return html.UnescapeString(row[:strings.Index(row+"</tr>", "</tr>")])
		}
	}
	return ""
}

// waitingOf lists the order ids of the vendor's "Waiting for you" section as the loader fills it, with each action.
func (x *xmrEscrowEnv) waitingOf() (map[string]string, []string) {
	x.t.Helper()
	d := &PageData{Page: "vendor-dashboard", User: x.loadUser(x.vendor), Settings: map[string]string{}}
	if err := loadIncomingOrders(x.t.Context(), x.A, httptest.NewRequest("GET", "/vendor-dashboard", nil), d); err != nil {
		x.t.Fatal(err)
	}
	waiting := map[string]string{}
	for _, w := range d.VendorWaiting {
		waiting[w.Order.ID] = w.Action
	}
	var incoming []string
	for _, o := range d.IncomingOrders {
		incoming = append(incoming, o.ID)
	}
	return waiting, incoming
}

// claimOffered reports whether the order page offers the vendor the Monero claim form.
func (x *xmrEscrowEnv) claimOffered(order string) bool {
	return strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow-xmr/claim"`)
}

// TestXMREscrowDeskListsUnclaimedRelease: the "Waiting for you" section lists, above the desk table and outside its
// history cap, paid orders to fulfil with their ship-by time, Monero escrow orders the vendor may claim (the order
// view's claim predicate, xmrClaimDueSQL) and open disputes the buyer opened that the vendor has not replied to. An
// unclaimed order older than 101 closed ones is still listed; each entry leaves once the vendor has acted.
func TestXMREscrowDeskListsUnclaimedRelease(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)

	// Digital, delivered automatically: in its dispute window (A-709) there is no claim yet, on the desk or the order
	// page; once the window ends with no dispute the order completes and waits for the vendor's claim.
	completed, _ := x.fundedXMR(x.product)
	if x.state(completed) != stateDelivered {
		t.Fatalf("funded digital order is %s, want delivered", x.state(completed))
	}
	if waiting, _ := x.waitingOf(); waiting[completed] != "" || x.claimOffered(completed) {
		t.Fatalf("a digital order in its dispute window is offered a claim: desk %q, order page %v", waiting[completed], x.claimOffered(completed))
	}
	if row := deskRow(deskSection(t, x.page("/vendor-dashboard", x.vendorSess), "vendor-orders"), completed); !strings.Contains(row, "<td>Waiting for buyer or deadline</td>") {
		t.Fatalf("a digital order in its dispute window has the wrong payout:\n%s", row)
	}
	x.endWindows()
	if x.state(completed) != stateCompleted {
		t.Fatalf("after its dispute window the digital order is %s", x.state(completed))
	}
	paid, _ := x.paidXMRPhysical(physical)
	decided, _ := x.paidXMRPhysical(physical) // decided for the vendor before shipping: no release yet
	x.disputeAndResolve(decided, "release")
	shippedDecided, _, _ := x.shippedXMR(physical) // decided for the vendor after shipping: the shipment's release pays
	x.disputeAndResolve(shippedDecided, "release")
	refunded, _ := x.paidXMRPhysical(physical)
	x.disputeAndResolve(refunded, "refund")
	confirmed, confirmSess, _ := x.shippedXMR(physical)
	if code, body := x.xmrConfirm(confirmed, confirmSess, testPassword); code != 303 {
		t.Fatalf("confirm receipt: %d %s", code, body)
	}
	disputed, disputeSess := x.undeliveredXMR()
	if code, body := x.xmrDispute(disputed, disputeSess); code != 303 {
		t.Fatalf("buyer's dispute: %d %s", code, body)
	}
	vendorOpened, _ := x.undeliveredXMR()
	if code, body := x.xmrDispute(vendorOpened, x.vendorSess); code != 303 {
		t.Fatalf("vendor's dispute: %d %s", code, body)
	}
	manual, _ := x.undeliveredXMR() // paid digital, delivered by hand by its deadline (A-709)

	want := map[string]string{completed: "Claim your payment", decided: "Claim your payment", paid: "Mark shipped", disputed: "Reply to dispute", manual: "Fulfil this order"}
	waiting, _ := x.waitingOf()
	if !maps.Equal(waiting, want) {
		t.Fatalf("waiting for the vendor: %v, want %v", waiting, want)
	}
	// The same predicate as the order view's claim form: offered exactly where the desk says to claim.
	for _, o := range []string{completed, decided, shippedDecided, refunded, confirmed, paid, disputed, vendorOpened, manual} {
		if offered, listed := x.claimOffered(o), waiting[o] == "Claim your payment"; offered != listed {
			t.Errorf("order %s (%s): claim form offered=%v, desk lists a claim=%v", shortID(o), x.state(o), offered, listed)
		}
	}

	dash := x.page("/vendor-dashboard", x.vendorSess)
	if w, table := strings.Index(dash, `id="vendor-waiting"`), strings.Index(dash, `id="vendor-orders"`); w < 0 || table < 0 || w > table {
		t.Fatalf("the Waiting for you section is not above the desk table:\n%s", dash)
	}
	section := deskSection(t, dash, "vendor-waiting")
	mustContain(t, section, "Waiting for you")
	// The disputes tile says who decides: not the vendor.
	mustContain(t, dash, "Open disputes</div>\n<strong>2</strong>\n<span>Disputes on your orders awaiting an administrator's decision</span>")
	mustNotContain(t, dash, "needing a dispute decision")
	shipBy := func(o string) string {
		return agStrings(t, x.testEnv, `SELECT to_char(ship_by,'YYYY-MM-DD HH24:MI "UTC"') FROM escrows WHERE order_id=$1`, o)[0]
	}
	for order, wantRow := range map[string][]string{
		completed: {"Claim your payment", `href="/order?id=` + completed + `#xmr-escrow-claim"`},
		decided:   {"Claim your payment"},
		paid:      {"Mark shipped", "Ship by " + shipBy(paid)},
		manual:    {"Fulfil this order", "Deliver by " + shipBy(manual)},
		disputed:  {"Reply to dispute", `href="/order?id=` + disputed + `#order-dispute"`},
	} {
		row := deskRow(section, order)
		if row == "" {
			t.Fatalf("the section does not list %s:\n%s", shortID(order), section)
		}
		for _, w := range wantRow {
			if !strings.Contains(row, html.UnescapeString(w)) {
				t.Errorf("row of %s lacks %q:\n%s", shortID(order), w, row)
			}
		}
	}
	for _, o := range []string{shippedDecided, refunded, confirmed, vendorOpened} {
		if row := deskRow(section, o); row != "" {
			t.Errorf("the section lists %s (%s):\n%s", shortID(o), x.state(o), row)
		}
	}

	// 101 newer closed orders push the unclaimed one out of the capped history, not out of the section.
	agExec(x.testEnv, `INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,created,updated)
		SELECT 'newer-'||lpad(g::text,3,'0'),$1,$2,'XMR',100000,'completed',now(),now()+g*interval '1 minute' FROM generate_series(1,101) g`,
		x.buyer, x.product)
	agExec(x.testEnv, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations)
		SELECT id,'XMR',encode(sha256(convert_to(id,'UTF8')),'hex'),0,'newer'||right(id,3),100000,3 FROM orders WHERE id LIKE 'newer-%'`)
	waiting, incoming := x.waitingOf()
	if slices.Contains(incoming, completed) || waiting[completed] != "Claim your payment" || waiting[decided] != "Claim your payment" {
		t.Fatalf("past the history cap: listed in history=%v, waiting=%v", slices.Contains(incoming, completed), waiting)
	}
	if row := deskRow(deskSection(t, x.page("/vendor-dashboard", x.vendorSess), "vendor-waiting"), completed); !strings.Contains(row, "Claim your payment") {
		t.Fatalf("past the history cap the page lost the claim:\n%s", row)
	}

	// Each entry leaves once the vendor acts: the claim, the shipment and the reply.
	if code, body := x.xmrClaim(completed, x.vendorSess, testXMRAddr("desk claim "+completed, true), testPassword); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	if code, body := x.xmrShip(paid, x.vendorSess, testXMRAddr("desk ship "+paid, true), testPassword); code != 303 {
		t.Fatalf("ship: %d %s", code, body)
	}
	if w := x.do("POST", "/disputes/reply", x.vendorSess, url.Values{"order_id": {disputed}, "reply": {"The licence key was sent through the order page."}}); w.Code != 303 {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	if waiting, _ = x.waitingOf(); !maps.Equal(waiting, map[string]string{decided: "Claim your payment", manual: "Fulfil this order"}) {
		t.Fatalf("after the claim, the shipment and the reply: %v", waiting)
	}
	if x.claimOffered(completed) {
		t.Fatal("the order page still offers a claimed order's claim")
	}
}

// TestXMRDeskPayoutColumn: each Monero escrow row of the desk carries the state of its latest release that is not
// superseded, in short words; "Payouts not yet confirmed" lists the vendor's orders whose release (or refund) is not
// confirmed and followed to the reorganisation depth, and the tile counts exactly those, leaving out the vendor's own
// purchases.
func TestXMRDeskPayoutColumn(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	digital := x.digital(x.vendor) // a listing has 5 in stock
	claimable, _ := x.fundedXMR(x.product)
	sent, _ := x.fundedXMR(x.product)
	confirmed, _ := x.fundedXMR(x.product)
	safe, _ := x.fundedXMR(digital)
	requested, _ := x.fundedXMR(digital)
	failed, _ := x.fundedXMR(digital)
	shipped, _, _ := x.shippedXMR(physical)
	held, heldSess, _ := x.shippedXMR(physical)
	paid, _ := x.paidXMRPhysical(physical)
	refunded, _ := x.paidXMRPhysical(physical)
	x.disputeAndResolve(refunded, "refund")
	// The vendor's own purchase from another vendor: not the vendor's payout, so neither listed nor counted.
	otherVendor, _ := x.user("xother_vendor_"+randomToken()[:6], "vendor")
	x.enableOrderPGP(otherVendor)
	purchase, _ := x.fundedXMR(x.digital(otherVendor))
	agExec(x.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", x.vendor, purchase)
	x.endWindows()                        // the digital orders complete with no dispute (A-709)
	inWindow, _ := x.fundedXMR(x.product) // delivered, its dispute window still open: no claim yet

	claim := func(orders ...string) {
		t.Helper()
		for _, o := range orders {
			if code, body := x.xmrClaim(o, x.vendorSess, testXMRAddr("payout "+o, true), testPassword); code != 303 {
				t.Fatalf("claim %s: %d %s", shortID(o), code, body)
			}
		}
	}
	claim(confirmed, safe)
	x.mustPoll() // builds and sends the claimed releases
	x.fake.mine(10)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE xmr_settlements SET reorg_safe_at=now() WHERE order_id=$1 AND kind='release' AND state='confirmed'", safe)
	claim(sent)
	x.mustPoll()
	// After the market's last pass: claims it has not built yet (one then failed), and a release held while the
	// vendor's account was closed.
	claim(requested, failed)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error='rpc' WHERE order_id=$1 AND kind='release'", failed)
	if code, body := x.xmrConfirm(held, heldSess, testPassword); code != 303 {
		t.Fatalf("confirm receipt: %d %s", code, body)
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE order_id=$1 AND kind='release' AND state='requested'", held, xmrVendorClosed)
	for o, want := range map[string]string{confirmed: "confirmed", safe: "confirmed", sent: "relayed", failed: "failed", requested: "requested", held: "requested"} {
		if got := x.xmrSettlements(o); len(got) != 1 || !strings.HasPrefix(got[0], "release "+want+" ") {
			t.Fatalf("settlements of %s: %v, want one release %s", shortID(o), got, want)
		}
	}

	labels := map[string]string{
		claimable: "Waiting for your claim", shipped: "Waiting for buyer or deadline", inWindow: "Waiting for buyer or deadline", requested: "Requested", sent: "Sent",
		confirmed: "Confirmed", safe: "Confirmed", failed: "Not sent – see order", held: "Held", paid: "—", refunded: "None: refund to the buyer",
	}
	dash := x.page("/vendor-dashboard", x.vendorSess)
	table := deskSection(t, dash, "vendor-orders")
	mustContain(t, table, `<th scope="col">Payout</th>`)
	for o, label := range labels {
		if row := deskRow(table, o); !strings.Contains(row, "<td>"+label+"</td>") {
			t.Errorf("row of %s (%s) lacks the payout %q:\n%s", shortID(o), x.state(o), label, row)
		}
	}
	mustContain(t, dash, `href="/vendor-dashboard?payouts=unconfirmed#vendor-orders"`, "Payouts not yet confirmed")

	// The filter and the tile: every vendor-side Monero escrow order but the one confirmed past the reorganisation depth.
	open := []string{claimable, inWindow, sent, confirmed, requested, failed, shipped, held, paid, refunded}
	filtered := x.page("/vendor-dashboard?payouts=unconfirmed", x.vendorSess)
	ftable := deskSection(t, filtered, "vendor-orders")
	for _, o := range open {
		if deskRow(ftable, o) == "" {
			t.Errorf("the filter lost %s (%s)", shortID(o), x.state(o))
		}
	}
	for _, o := range []string{safe, purchase} {
		if deskRow(ftable, o) != "" {
			t.Errorf("the filter lists %s", shortID(o))
		}
	}
	mustContain(t, filtered, `aria-current="page">Payouts not yet confirmed`)
	tile := "<span>" + strconv.Itoa(len(open)) + " order(s) may hold coins; " + xmrCustody + "</span>"
	if !strings.Contains(dash, tile) || !strings.Contains(filtered, tile) {
		t.Fatalf("the Monero escrow tile does not count %d orders:\n%s", len(open), dash[strings.Index(dash, "Monero escrow</div>"):][:400])
	}
}

// TestXMRClaimCueOnResolveAndConcession: a decision or a concession for the vendor that leaves the release unclaimed
// tells the vendor to claim it on the order page with a fresh address; one that the shipment's release pays, and a
// refund, do not.
func TestXMRClaimCueOnResolveAndConcession(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	unshipped, _ := x.paidXMRPhysical(physical)
	x.disputeAndResolve(unshipped, "release")
	shipped, _, _ := x.shippedXMR(physical)
	x.disputeAndResolve(shipped, "release")
	refunded, _ := x.paidXMRPhysical(physical)
	x.disputeAndResolve(refunded, "refund")
	conceded, buyerSess := x.undeliveredXMR()
	if code, body := x.xmrDispute(conceded, buyerSess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", conceded)[0]
	page := x.page("/order?id="+conceded, buyerSess)
	x.check(x.do("POST", "/disputes/concede", buyerSess, concessionForm(dispute, "release", renderedFormNonce(t, page, "/disputes/concede"), xmrListingPrice)), 303)
	if x.state(conceded) != stateCompleted {
		t.Fatalf("conceded order is %s", x.state(conceded))
	}

	cued := func(order, to string) bool {
		for _, n := range x.noticesOf(x.vendor, order) {
			if strings.Contains(n, "is now "+stateLabel(to)) {
				return strings.Contains(n, xmrClaimCue)
			}
		}
		t.Fatalf("the vendor has no notice of %s's move to %s: %q", shortID(order), to, x.noticesOf(x.vendor, order))
		return false
	}
	for _, c := range []struct {
		order, to string
		want      bool
	}{{unshipped, stateResolved, true}, {conceded, stateCompleted, true}, {shipped, stateResolved, false}, {refunded, stateResolved, false}} {
		if got := cued(c.order, c.to); got != c.want {
			t.Errorf("order %s moved to %s: claim cue %v, want %v: %q", shortID(c.order), c.to, got, c.want, x.noticesOf(x.vendor, c.order))
		}
	}
	mustContain(t, xmrClaimCue, "claim", "order page", "fresh address")
	if !x.claimOffered(unshipped) || !x.claimOffered(conceded) || x.claimOffered(shipped) {
		t.Fatal("the cue and the order page's claim form disagree")
	}
}
