package market

import (
	"slices"
	"strings"
	"testing"
)

// A-470, A-468: an escrow order with an open staff alert (a notice to staff through flagOrder: the broadcast stall of
// A-441, the release too few of A-461) is readable by moderators and administrators who are not parties until the
// alert clears, with no action offered; nobody else is admitted. DB integration against the fake escrow Core
// (simulated RPC, not chain evidence).

// outsiders is an unrelated account and the buyer and the vendor of another order: none may read an escrow order.
func (x *escrowEnv) outsiders() []string {
	x.t.Helper()
	s := randomToken()[:6]
	_, stranger := x.user("estranger_"+s, "buyer")
	otherBuyer, otherBuyerSess := x.user("eotherbuyer_"+s, "buyer")
	otherVendor, otherVendorSess := x.user("eothervendor_"+s, "vendor")
	x.testEnv.order(otherBuyer, x.testEnv.product(otherVendor, "digital"), "BTC", statePaid)
	return []string{stranger, otherBuyerSess, otherVendorSess}
}

// exec runs a fixture write, failing the test on error.
func (x *escrowEnv) exec(query string, args ...any) {
	x.t.Helper()
	if _, err := x.DB.Exec(query, args...); err != nil {
		x.t.Fatal(err)
	}
}

// deskAlert is the moderation desk's escrow-alerts entry for order as sess sees it, or "" when it is not listed.
func (x *escrowEnv) deskAlert(order, sess string) string {
	x.t.Helper()
	desk := x.page("/moderator", sess)
	_, section, ok := strings.Cut(desk, `<section id="escrow-alerts"`)
	if !ok {
		return ""
	}
	section, _, _ = strings.Cut(section, "</section>")
	_, entry, ok := strings.Cut(section, `<li><a class="text-link mono" href="/order?id=`+order+`">`)
	if !ok {
		return ""
	}
	entry, _, _ = strings.Cut(entry, "</li>")
	return entry
}

// orderCodes is the status of GET /order?id=order for each session.
func (x *escrowEnv) orderCodes(order string, sessions ...string) []int {
	x.t.Helper()
	var out []int
	for _, s := range sessions {
		out = append(out, x.do("GET", "/order?id="+order, s, nil).Code)
	}
	return out
}

// staffReadOnly fails when the order page staff are shown offers any form that changes something.
func (x *escrowEnv) staffReadOnly(order, sess string) {
	x.t.Helper()
	if page := x.page("/order?id="+order, sess); strings.Contains(page, `method="post"`) || strings.Contains(page, escrowDelivery) {
		x.t.Fatalf("the staff order page offers an action or shows the delivery:\n%s", page)
	}
}

// The stall alert has no deposit flag: while it is open, staff read the order (no form, no delivery) and its archived
// listing (A-164, the same rule); an unrelated user and the parties of another order do not; once the network sees the
// transaction the alert clears and staff lose both pages again.
func TestEscrowStallAlertStaffRead(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("stallreadmod_"+randomToken()[:6], "moderator")
	_, admin := x.user("stallreadadmin_"+randomToken()[:6], "admin")
	outsiders := x.outsiders()
	order, id := x.built(regtestAddr("stall-read-refund"), regtestAddr("stall-read-claim"), 100000)
	x.core.mu.Lock()
	x.core.private = true
	x.core.mu.Unlock()
	x.setBlocks(200)
	x.signAs(order, roleVendor, id)
	x.poll()
	if s := x.settleByID(id); s.State != "broadcast" {
		t.Fatalf("after the broadcast pass: %+v", s)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order); n != 0 {
		t.Fatalf("%d flagged deposits: the test needs an order no flag opens", n)
	}
	product := agStrings(t, x.testEnv, "SELECT product_id FROM orders WHERE id=$1", order)[0]
	x.exec("UPDATE products SET archived=true WHERE id=$1", product)
	listing := func(sess string) int { return x.do("GET", "/product?id="+product, sess, nil).Code }
	if got, l := x.orderCodes(order, mod, admin), listing(mod); !slices.Equal(got, []int{404, 404}) || l != 404 {
		t.Fatalf("staff before any alert: %v, archived listing %d", got, l)
	}
	if e := x.deskAlert(order, mod); e != "" {
		t.Fatalf("the desk lists an escrow alert before any: %s", e)
	}
	x.setBlocks(200 + escrowStallBlocks)
	x.poll()
	if _, notes, _ := x.stallNotices(order); notes != 1 {
		t.Fatalf("no stall alert: %d notes", notes)
	}
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("staff while the stall alert is open: %v", got)
	}
	x.staffReadOnly(order, mod)
	x.staffReadOnly(order, admin)
	if got := x.orderCodes(order, outsiders...); !slices.Equal(got, []int{404, 404, 404}) {
		t.Fatalf("an unrelated user or a party of another order while the stall alert is open: %v", got)
	}
	if l, u := listing(mod), listing(outsiders[0]); l != 200 || u != 404 {
		t.Fatalf("the archived listing while the stall alert is open: staff %d, unrelated %d", l, u)
	}
	if got := x.orderCodes(order, x.buyerSess, x.vendorSess); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("the parties: %v", got)
	}
	// The moderation desk lists it, linked, for a moderator and an administrator.
	for _, sess := range []string{mod, admin} {
		if e := x.deskAlert(order, sess); !strings.Contains(e, "a settlement queued or stuck, not seen by the network") || strings.Contains(e, "too few") {
			t.Fatalf("the desk's escrow alert for the stall: %q", e)
		}
	}
	// The /admin stall line links the order page.
	if page := x.adminPage(); !strings.Contains(page, `href="/order?id=`+order+`"`) {
		t.Fatalf("the admin stall line does not link the order:\n%s", page)
	}

	// The network sees it: the alert clears, and with it the staff's access.
	x.core.mu.Lock()
	x.core.private = false
	x.core.mu.Unlock()
	x.poll()
	x.poll()
	if _, _, seen := x.stallNotices(order); seen != 1 {
		t.Fatalf("the stall alert did not clear: %d seen notes", seen)
	}
	if got, l := x.orderCodes(order, mod, admin), listing(mod); !slices.Equal(got, []int{404, 404}) || l != 404 {
		t.Fatalf("staff after the stall alert cleared: %v, archived listing %d", got, l)
	}
	if e := x.deskAlert(order, mod); e != "" {
		t.Fatalf("the desk still lists the cleared stall: %s", e)
	}
	if got := x.orderCodes(order, x.buyerSess, x.vendorSess); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("the parties after the alert cleared: %v", got)
	}
}

// A-468: a completed escrow order, never disputed, whose release fails too-small. Staff open it from the notice (the
// deposit flag of QA462-3, listed on the moderation desk with its real reason and flag time) without the release's
// counted deposits changing; outsiders do not. The alert itself admits staff too (one rule, A-470), flag or not, until
// the failed release no longer stands (superseded, as a changed dispute decision does).
func TestEscrowReleaseTooFewStaffAlertRead(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("toofewmod_"+randomToken()[:6], "moderator")
	_, admin := x.user("toofewadmin_"+randomToken()[:6], "admin")
	outsiders := x.outsiders()
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("too few alert refund"))
	funded := txidOf("too few alert funded " + order)
	x.core.deposit(addr, funded, 0, 100000, 3)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatalf("order %s after funding", x.state(order))
	}
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{404, 404}) {
		t.Fatalf("staff before any alert: %v", got)
	}
	x.exec("UPDATE payments SET amount=3000 WHERE order_id=$1", order)
	const counted = "SELECT amount||' '||credited||' '||escrow_left_out||' '||after_close||' '||COALESCE(settlement_id::text,'-')||' '||COALESCE(escrow_too_few_by::text,'-') FROM payments WHERE order_id=$1 ORDER BY id"
	before := agStrings(t, x.testEnv, counted, order)
	if code, body := x.claim(order, x.vendorSess, regtestAddr("too few alert claim")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	x.poll()
	sid, kind, state, class := x.settlement(order)
	if kind != "release" || state != "failed" || class != settleTooSmall || x.state(order) != stateCompleted {
		t.Fatalf("the release: %s %s %s; order %s", kind, state, class, x.state(order))
	}
	if told := agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0",
		"Payment review needed for order "+shortID(order)); told == 0 {
		t.Fatal("staff were not told")
	}
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("staff after the release too few: %v", got)
	}
	x.staffReadOnly(order, mod)
	if got := x.orderCodes(order, outsiders...); !slices.Equal(got, []int{404, 404, 404}) {
		t.Fatalf("an unrelated user or a party of another order: %v", got)
	}
	// Opening the order changed none of the deposits' counting; the flag only lists it.
	if after := agStrings(t, x.testEnv, counted, order); !slices.Equal(after, before) {
		t.Fatalf("the deposits' counting changed: %q, then %q", before, after)
	}
	// From the notice: the moderation desk links the order, with the deposit's real reason and its flag time.
	desk := x.page("/moderator", mod)
	row := deskRow(desk, funded)
	if !strings.Contains(desk, `href="/order?id=`+order+`"`) || !flaggedAt.MatchString(row) || !strings.Contains(row, "Escrow deposit counted for the order (credited toward its funding)") ||
		strings.Contains(row, "confirmed again") || strings.Contains(row, "Not recorded") || strings.Contains(desk, `name="payment_id"`) {
		t.Fatalf("the desk's row for the flagged deposit: %s\n%s", row, desk)
	}

	if e := x.deskAlert(order, mod); !strings.Contains(e, "the release to the vendor could not be built, the coins are too few") || strings.Contains(e, "queued or stuck") {
		t.Fatalf("the desk's escrow alert for the release too few: %q", e)
	}
	// The alert alone admits staff: with no deposit flagged, still 200 while the failed release stands.
	x.exec("UPDATE payments SET flagged=false WHERE order_id=$1", order)
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("staff with the alert open and no flag: %v", got)
	}
	if got := x.orderCodes(order, outsiders...); !slices.Equal(got, []int{404, 404, 404}) {
		t.Fatalf("outsiders with the alert open and no flag: %v", got)
	}
	// The failed release superseded (the end a changed dispute decision gives it): the alert clears, and so does access.
	x.exec("UPDATE settlements SET state='superseded' WHERE id=$1", sid)
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{404, 404}) {
		t.Fatalf("staff after the failed release was superseded: %v", got)
	}
	if e := x.deskAlert(order, mod); e != "" {
		t.Fatalf("the desk still lists the superseded release: %s", e)
	}
}

// The order-note help on a Bitcoin escrow order names the open staff alert among the times staff read the order (A-470);
// a pooled order's help does not mention escrow (TestEscrowLeavesPooledOrdersUnchanged).
func TestEscrowNoteHelpNamesStaffAlert(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.paidPhysical(regtestAddr("note help refund"), 100000)
	page := x.page("/order?id="+order, x.vendorSess)
	_, help, ok := strings.Cut(page, `id="note-help-cancelled">`)
	if help, _, _ = strings.Cut(help, "</span>"); !ok ||
		!strings.Contains(help, "moderators and administrators if the order is disputed, a payment on it is flagged or its escrow has an open staff alert, and the operator") {
		t.Fatalf("the escrow order's note help: %q", help)
	}
}
