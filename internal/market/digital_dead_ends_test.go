package market

import (
	"html/template"
	"net/url"
	"strings"
	"testing"
)

// A-711 (VEN-3): a reusable download with no content would be published as delivered automatically while checkout
// refuses it. The create form, the edit form and the digital delivery method form each refuse it with the problem
// next to its field (A-59), saving nothing; with content it is accepted.
func TestReusableModeRequiresContent(t *testing.T) {
	p := newPayEnv(t)
	before := p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID)
	sent := form("title", "Reusable download listing", "description", "Kept description.", "category", "Digital", "subcategory", "",
		"region", "Digital", "kind", "digital", "stock", "5", "price_xmr", "0.5", "digital_delivery_mode", "reusable", "delivery_content", " \r\n\t ")
	w := refusalPost(p.testEnv, "POST", "/listings", p.vendorSess, sent, "http://example.com/vendor-dashboard")
	b := w.Body.String()
	if w.Code != 400 {
		t.Fatalf("blank reusable create: %d\n%s", w.Code, b)
	}
	f := listingFormSection(t, b, "/listings")
	mustContain(t, b, `role="alert">The listing was not published; nothing was saved. `+template.HTMLEscapeString(reusableContentRequired))
	mustContain(t, f, `value="Reusable download listing"`, `<option value="reusable" selected>`)
	checkFieldError(t, f, "delivery_content", reusableContentRequired)
	if n := p.count("SELECT count(*) FROM products WHERE vendor_id=$1", p.vendor.ID); n != before {
		t.Fatalf("a refused create saved a listing: %d, want %d", n, before)
	}
	// The other digital methods need no reusable content, and a physical listing ignores the method.
	for _, mode := range []string{"manual", "inventory"} {
		sent.Set("digital_delivery_mode", mode)
		p.check(p.do("POST", "/listings", p.vendorSess, sent), 303)
	}
	sent.Set("digital_delivery_mode", "reusable")
	sent.Set("delivery_content", "https://example.invalid/reusable-download")
	w = p.do("POST", "/listings", p.vendorSess, sent)
	p.check(w, 303)
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	listing := location.Query().Get("id")
	if got := p.str("SELECT digital_delivery_mode||'|'||delivery_content FROM products WHERE id=$1", listing); got != "reusable|https://example.invalid/reusable-download" {
		t.Fatalf("reusable listing with content: %q", got)
	}

	// Edit: the vendor blanking a reusable download's content is refused and the stored content kept.
	edit := inventoryForm(listing, map[string]string{"kind": "digital", "category": "Digital", "region": "Digital", "title": "Edited title", "delivery_content": "   "})
	w = refusalPost(p.testEnv, "POST", "/listings/update", p.vendorSess, edit, "")
	b = w.Body.String()
	if w.Code != 400 {
		t.Fatalf("blank reusable edit: %d\n%s", w.Code, b)
	}
	mustContain(t, b, `role="alert">The changes were not saved. `+template.HTMLEscapeString(reusableContentRequiredEdit))
	checkFieldError(t, listingFormSection(t, b, "/listings/update"), "delivery_content", reusableContentRequiredEdit)
	if got := p.str("SELECT title||'|'||delivery_content FROM products WHERE id=$1", listing); got != "Reusable download listing|https://example.invalid/reusable-download" {
		t.Fatalf("a refused edit changed the listing: %q", got)
	}
	// An administrator's edit never carries the content (the stored content is kept), so it is not refused.
	edit.Del("delivery_content")
	p.check(p.do("POST", "/listings/update", p.adminSess, edit), 303)
	if got := p.str("SELECT title||'|'||delivery_content FROM products WHERE id=$1", listing); got != "Edited title|https://example.invalid/reusable-download" {
		t.Fatalf("administrator edit: %q", got)
	}

	// The method form: a manual listing without saved content cannot switch to a reusable download.
	manual := p.str("SELECT id FROM products WHERE vendor_id=$1 AND digital_delivery_mode='manual' AND kind='digital'", p.vendor.ID)
	w = refusalPost(p.testEnv, "POST", "/listings/digital-mode", p.vendorSess, url.Values{"id": {manual}, "mode": {"reusable"}}, "")
	b = w.Body.String()
	if w.Code != 400 {
		t.Fatalf("blank reusable mode: %d\n%s", w.Code, b)
	}
	mustContain(t, b, `role="alert">`+template.HTMLEscapeString(reusableModeNeedsContent),
		`<select name="mode" aria-invalid="true" aria-describedby="di-mode-error">`,
		`<p class="field-error" id="di-mode-error">`+template.HTMLEscapeString(reusableModeNeedsContent)+`</p>`,
		`action="/listings/update"`)
	if got := p.str("SELECT digital_delivery_mode FROM products WHERE id=$1", manual); got != "manual" {
		t.Fatalf("a refused method change saved %q", got)
	}
	// Saved content first, then the method change is accepted.
	saved := inventoryForm(manual, map[string]string{"kind": "digital", "category": "Digital", "region": "Digital", "title": "Reusable download listing",
		"description": "Kept description.", "price_xmr": "0.5", "delivery_content": "https://example.invalid/now-reusable"})
	p.check(p.do("POST", "/listings/update", p.vendorSess, saved), 303)
	p.check(p.do("POST", "/listings/digital-mode", p.vendorSess, url.Values{"id": {manual}, "mode": {"reusable"}}), 303)
	if got := p.str("SELECT digital_delivery_mode FROM products WHERE id=$1", manual); got != "reusable" {
		t.Fatalf("method change with saved content: %q", got)
	}
	// A physical listing kept migration 141's default method; made digital without content, it starts with manual
	// delivery, as a new digital listing does, rather than as an empty reusable download.
	if got := p.str("SELECT digital_delivery_mode FROM products WHERE id=$1", p.productID); got != "reusable" {
		t.Fatalf("fixture listing method %q", got)
	}
	converted := inventoryForm(p.productID, map[string]string{"kind": "digital", "category": "Digital", "region": "Digital"})
	p.check(p.do("POST", "/listings/update", p.vendorSess, converted), 303)
	if got := p.str("SELECT kind||'|'||digital_delivery_mode FROM products WHERE id=$1", p.productID); got != "digital|manual" {
		t.Fatalf("physical listing made digital: %q", got)
	}
}

// A-711 (VEN-3): a buyer's draft on a listing that cannot take escrow says why under the escrow checkout, instead of
// showing nothing: a reusable download without content, a listing taken down, distinct inventory that has run out.
func TestXMRCheckoutOfferStatesIneligibleReason(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	blank := x.digital(x.vendor)
	agExec(x.testEnv, "UPDATE products SET delivery_content='' WHERE id=$1", blank)
	takenDown := x.digital(x.vendor)
	agExec(x.testEnv, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Taken down for the test' WHERE id=$1", takenDown)
	empty := digitalInventoryFixture(t, x)
	for product, reason := range map[string]string{blank: reusableUnavailableReason, takenDown: takenDownCheckoutReason,
		empty: "There are not enough distinct digital units left. Choose a smaller quantity; nothing was charged."} {
		order := x.draft(product)
		page := x.page("/order?id="+order, x.buyerSess)
		mustContain(t, page, `id="xmr-escrow-checkout-heading"`, `<p class="order-unavailable" role="note"><strong>Unavailable.</strong> `+template.HTMLEscapeString(reason)+`</p>`)
		mustNotContain(t, page, `action="/orders/escrow-xmr"`)
	}
	// An eligible listing still offers the checkout form.
	mustContain(t, x.page("/order?id="+x.draft(x.product), x.buyerSess), `action="/orders/escrow-xmr"`)
}

// A-711 (VEN-3): a reusable download blanked after checkout leaves the funded order paid, with a history note and a
// notice that tells the vendor why and to deliver or cancel; nothing is delivered.
func TestXMRReusableBlankedAfterCheckoutNotifiesVendor(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.product)
	agExec(x.testEnv, "UPDATE products SET delivery_content='' WHERE id=$1", x.product)
	x.fake.payAddress(addr, hex64("a711 blanked "+order), xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 {
		t.Fatalf("blanked reusable download: state %s", x.state(order))
	}
	checkAutoDeliveryFailure(t, x, order, "reusable download content is empty")
}

// A-711 (PM-2): a listing taken down, or a reserved distinct batch no longer available, leaves the funded order paid
// with a history note and a vendor notice naming the cause and saying to deliver or cancel.
func TestAutoDeliveryFailureNotifiesVendor(t *testing.T) {
	t.Run("takedown", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, addr := x.freshCheckout(x.product)
		agExec(x.testEnv, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Taken down for the test' WHERE id=$1", x.product)
		x.fake.payAddress(addr, hex64("a711 takedown "+order), xmrListingPrice, 10)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("taken-down listing: state %s", x.state(order))
		}
		checkAutoDeliveryFailure(t, x, order, "an administrator took this listing down")
	})
	t.Run("batch unavailable", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		p := digitalInventoryFixture(t, x, "a711 unit one", "a711 unit two")
		x.mustPoll()
		order, addr := x.freshCheckout(p)
		agExec(x.testEnv, "UPDATE digital_units SET content='',fingerprint=NULL WHERE order_id=$1", order)
		x.fake.payAddress(addr, hex64("a711 batch "+order), xmrListingPrice, 10)
		x.mustPoll()
		if x.state(order) != statePaid {
			t.Fatalf("unavailable batch: state %s", x.state(order))
		}
		checkAutoDeliveryFailure(t, x, order, "reserved distinct digital units are no longer available")
	})
}

// checkAutoDeliveryFailure: order has one system history note and one vendor notice, each naming cause and saying the
// vendor delivers by hand or cancels; nothing was delivered.
func checkAutoDeliveryFailure(t *testing.T, x *xmrEscrowEnv, order, cause string) {
	t.Helper()
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND from_state='paid' AND to_state='paid' AND strpos(note,$2)>0 AND strpos(note,'deliver')>0 AND strpos(note,'cancel')>0", order, cause); n != 1 {
		t.Fatalf("history notes naming %q: %d: %q", cause, n, x.events(order))
	}
	var notices []string
	for _, n := range x.noticesOf(x.vendor, order) {
		if strings.Contains(n, "not delivered automatically") {
			notices = append(notices, n)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], cause) || !strings.Contains(notices[0], "deliver") || !strings.Contains(notices[0], "cancel") {
		t.Fatalf("vendor notices: %q", notices)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order); n != 0 {
		t.Fatalf("%d deliveries", n)
	}
}

// A-711 (BE97-2): distinct units reserved by a paid order and never delivered return to stock when its dispute ends in a
// refund, by the vendor's concession (disputed -> cancelled) or an administrator's decision (disputed -> resolved,
// refund); units already delivered stay consumed, and a decision for the vendor keeps them with the order.
func TestDigitalUnitsReturnAfterDisputeRefund(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, "a711 unit A", "a711 unit B", "a711 unit C", "a711 unit D", "a711 unit E", "a711 unit F")
	x.mustPoll()
	available := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL", p)
	}
	// paid is a funded order of quantity units, left undelivered by a takedown during payment.
	paid := func(quantity int) string {
		t.Helper()
		order, addr := x.freshCheckoutOf(p, quantity)
		agExec(x.testEnv, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason='Taken down for the test' WHERE id=$1", p)
		x.fake.payAddress(addr, hex64("a711 dispute "+order), int64(quantity)*xmrListingPrice, 10)
		x.mustPoll()
		agExec(x.testEnv, "UPDATE products SET archived=false,archived_at=NULL,admin_archive_reason=NULL WHERE id=$1", p)
		if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND delivered_at IS NULL", order) != quantity {
			t.Fatalf("fixture order %s: %s", order, x.state(order))
		}
		return order
	}
	stock := func() int { return agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) }

	// An administrator's refund decision: both undelivered units return.
	refunded := paid(2)
	if available() != 4 || stock() != 4 {
		t.Fatalf("after checkout: %d available, stock %d", available(), stock())
	}
	x.disputeAndResolve(refunded, "refund")
	if x.state(refunded) != stateResolved || available() != 6 || stock() != 6 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", refunded) != 0 {
		t.Fatalf("refund decision: %s, %d available, stock %d", x.state(refunded), available(), stock())
	}

	// The vendor's concession: the undelivered unit returns, the one marked delivered stays with the order.
	conceded := paid(2)
	agExec(x.testEnv, "UPDATE digital_units SET delivered_at=now() WHERE id=(SELECT id FROM digital_units WHERE order_id=$1 ORDER BY created,id LIMIT 1)", conceded)
	bsess := x.sessOf(conceded, roleBuyer)
	x.check(x.do("POST", "/disputes", bsess, url.Values{"order_id": {conceded}, "reason": {"The download links in the delivery do not work."}}), 303)
	dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", conceded)[0]
	vsess := x.sessOf(conceded, roleVendor)
	mustContain(t, x.page("/order?id="+conceded, vsess), "Distinct digital units reserved for this order and not yet delivered return to the listing's stock; delivered content is not taken back.")
	mustContain(t, x.page("/order?id="+conceded, bsess), "reserved units are not returned to stock.")
	x.check(x.do("POST", "/disputes/concede", vsess, payoutConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "password": {testPassword}}, 2*xmrListingPrice)), 303)
	if x.state(conceded) != stateCancelled || available() != 5 || stock() != 5 ||
		agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND delivered_at IS NOT NULL", conceded) != 1 {
		t.Fatalf("vendor concession: %s, %d available, stock %d", x.state(conceded), available(), stock())
	}

	// A decision for the vendor keeps the units with the order.
	released := paid(1)
	x.disputeAndResolve(released, "release")
	if x.state(released) != stateResolved || available() != 4 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", released) != 1 {
		t.Fatalf("release decision: %s, %d available", x.state(released), available())
	}
	// A recorded delivery keeps the order's units consumed, whatever their delivered_at says.
	withDelivery := paid(1)
	agExec(x.testEnv, "INSERT INTO deliveries(order_id,content) VALUES($1,'a711 recorded delivery')", withDelivery)
	x.disputeAndResolve(withDelivery, "refund")
	if x.state(withDelivery) != stateResolved || available() != 3 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", withDelivery) != 1 {
		t.Fatalf("refund decision after a delivery: %s, %d available", x.state(withDelivery), available())
	}
}
