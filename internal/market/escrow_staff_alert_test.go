package market

import (
	"html"
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

// orderCodes is the status of GET /order?id=order for each session. Each response's status and first 300 bytes are
// logged with it (shown only when the test fails), so an unexpected status explains itself on a recurrence.
func (x *escrowEnv) orderCodes(order string, sessions ...string) []int {
	x.t.Helper()
	var out []int
	for i, s := range sessions {
		w := x.do("GET", "/order?id="+order, s, nil)
		body := w.Body.String()
		x.t.Logf("GET /order?id=%s as session %d of %d: status %d, body starts %q", order, i+1, len(sessions), w.Code, body[:min(len(body), 300)])
		out = append(out, w.Code)
	}
	return out
}

// staffHelp is the help line the order page shows staff who read it (order.html), or "" when it carries none.
func staffHelp(page string) string {
	_, help, ok := strings.Cut(html.UnescapeString(page), "You are viewing this order as ")
	if !ok {
		return ""
	}
	help, _, _ = strings.Cut(help, "</p>")
	return help
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
	// QA5W1-1: no deposit is flagged and no dispute is open, so only the alert admits staff: the page says so and links
	// the desk's Escrow alerts, not the (empty) Payment review list.
	for _, sess := range []string{mod, admin} {
		help := staffHelp(x.page("/order?id="+order, sess))
		for _, want := range []string{"because its escrow has an open staff alert", "a settlement queued or stuck, not seen by the network", `href="/moderator#escrow-alerts"`} {
			if !strings.Contains(help, want) {
				t.Fatalf("the staff help for the stall alert lacks %q: %q", want, help)
			}
		}
		for _, no := range []string{"flagged", "payment-review", "too few"} {
			if strings.Contains(help, no) {
				t.Fatalf("the staff help for the stall alert says %q: %q", no, help)
			}
		}
	}
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

// QA5W1-7: the stall alert also clears when the settlement leaves broadcast with no seen note written, as a settlement
// confirmed or spent outside the market does (the escrow watcher need not write the seen note first): the alert is the
// stall note while the settlement is in state broadcast, not the stall note alone. Staff lose the order and the desk
// entry, for both ways out of broadcast.
func TestEscrowStallAlertClearsWhenSettlementLeavesBroadcast(t *testing.T) {
	for _, next := range []string{"confirmed", "external"} {
		t.Run(next, func(t *testing.T) {
			x := newEscrowEnv(t)
			_, mod := x.user("stallleavemod_"+randomToken()[:6], "moderator")
			_, admin := x.user("stallleaveadmin_"+randomToken()[:6], "admin")
			order, id := x.built(regtestAddr("stall-leave-refund-"+next), regtestAddr("stall-leave-claim-"+next), 100000)
			x.core.mu.Lock()
			x.core.private = true
			x.core.mu.Unlock()
			x.setBlocks(200)
			x.signAs(order, roleVendor, id)
			x.poll()
			x.setBlocks(200 + escrowStallBlocks)
			x.poll()
			if _, notes, seen := x.stallNotices(order); notes != 1 || seen != 0 {
				t.Fatalf("the stall alert: %d stall notes, %d seen notes", notes, seen)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order); n != 0 {
				t.Fatalf("%d flagged deposits: the test needs an order no flag opens", n)
			}
			if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{200, 200}) {
				t.Fatalf("staff while the stall alert is open: %v", got)
			}
			if e := x.deskAlert(order, mod); !strings.Contains(e, "a settlement queued or stuck") {
				t.Fatalf("the desk lacks the open stall: %q", e)
			}
			// The settlement leaves broadcast; no seen note is written.
			x.exec("UPDATE settlements SET state=$2::text, confirmed_at=CASE WHEN $2::text='confirmed' THEN now() END WHERE id=$1", id, next)
			if _, _, seen := x.stallNotices(order); seen != 0 {
				t.Fatalf("a seen note was written: %d", seen)
			}
			if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{404, 404}) {
				t.Fatalf("staff after the settlement became %s: %v", next, got)
			}
			if e := x.deskAlert(order, mod); e != "" {
				t.Fatalf("the desk still lists the stall after the settlement became %s: %s", next, e)
			}
		})
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
	// The deposits are flagged as well, so a flag admits staff: the help keeps the payment-review text.
	if help := staffHelp(x.page("/order?id="+order, mod)); !strings.Contains(help, "because a payment on this order was flagged for review") || !strings.Contains(help, `href="/moderator#payment-review"`) || strings.Contains(help, "staff alert") {
		t.Fatalf("the staff help with a flag open: %q", help)
	}
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
	// QA5W1-1: with only the alert admitting them, the help names the too-small release and links the Escrow alerts.
	for _, sess := range []string{mod, admin} {
		help := staffHelp(x.page("/order?id="+order, sess))
		for _, want := range []string{"because its escrow has an open staff alert", "the release to the vendor could not be built, the coins are too few", `href="/moderator#escrow-alerts"`} {
			if !strings.Contains(help, want) {
				t.Fatalf("the staff help for the too-small release lacks %q: %q", want, help)
			}
		}
		for _, no := range []string{"flagged", "payment-review", "queued or stuck"} {
			if strings.Contains(help, no) {
				t.Fatalf("the staff help for the too-small release says %q: %q", no, help)
			}
		}
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

// QA5W1-2: the desk's Escrow alerts list is newest first and says how many more it does not list, so that the alert a
// notification just announced is never the one cut, and the cut is never silent. 101 open alerts (releases too few,
// created a second apart): the newest 100 are listed, the oldest is not, and the count line reads "1 more"; at exactly
// 100 there is no such line.
func TestEscrowDeskAlertsNewestFirstAndCounted(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("deskover_"+randomToken()[:6], "moderator")
	var orders []string
	for i := 0; i < escrowAlertLimit+1; i++ {
		o := x.testEnv.order(x.buyer, x.product, "BTC", stateCompleted)
		x.exec("UPDATE orders SET created=now()+make_interval(secs => $2) WHERE id=$1", o, i)
		x.exec("INSERT INTO settlements(order_id,kind,state,build_error) VALUES($1,'release','failed',$2)", o, settleTooSmall)
		orders = append(orders, o)
	}
	section := func() string {
		desk := x.page("/moderator", mod)
		_, sec, _ := strings.Cut(desk, `<section id="escrow-alerts"`)
		sec, _, _ = strings.Cut(sec, "</section>")
		return sec
	}
	sec := section()
	oldest, newest := orders[0], orders[len(orders)-1]
	if n := strings.Count(sec, "<li>"); n != escrowAlertLimit {
		t.Fatalf("%d alerts listed, want %d:\n%s", n, escrowAlertLimit, sec)
	}
	if !strings.Contains(sec, `href="/order?id=`+newest+`"`) || strings.Contains(sec, `href="/order?id=`+oldest+`"`) {
		t.Fatalf("the newest alert listed = %v, the oldest = %v:\n%s", strings.Contains(sec, newest), strings.Contains(sec, oldest), sec)
	}
	if first := strings.Index(sec, `href="/order?id=`+newest+`"`); first > strings.Index(sec, `href="/order?id=`+orders[len(orders)-2]+`"`) {
		t.Fatalf("the list is not newest first:\n%s", sec)
	}
	if !strings.Contains(sec, "1 more order has an open alert") {
		t.Fatalf("the desk does not say how many alerts it leaves out:\n%s", sec)
	}
	// One alert clears (the oldest listed one is superseded, the end a changed dispute decision gives it): exactly 100 are
	// open, all are listed, and no count line is left.
	x.exec("UPDATE settlements SET state='superseded' WHERE order_id=$1", orders[1])
	sec = section()
	if n := strings.Count(sec, "<li>"); n != escrowAlertLimit || !strings.Contains(sec, `href="/order?id=`+oldest+`"`) || strings.Contains(sec, " more order") {
		t.Fatalf("with exactly %d alerts open: %d listed, oldest listed %v:\n%s", escrowAlertLimit, n, strings.Contains(sec, oldest), sec)
	}
}
