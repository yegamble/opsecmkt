package market

import (
	"strings"
	"testing"
)

// Integration tests for the user-facing gaps: moderator dispute view, counterparty PGP keys and message
// prefill, the unread-notification count in the navigation, and vendor demotion.

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Fatalf("page does not contain %q", s)
		}
	}
}

func mustNotContain(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(body, s) {
			t.Fatalf("page unexpectedly contains %q", s)
		}
	}
}

func TestModeratorViewsDisputedOrderReadOnly(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("admin_"+randomToken()[:6], "admin")
	digital := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, digital, "BTC", stateDelivered)
	if _, err := w.e.DB.Exec("INSERT INTO deliveries(order_id,content) VALUES($1,$2)", order, "licence-key-ABC-123"); err != nil {
		t.Fatal(err)
	}
	notDisputed := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", statePaid)

	// Before any dispute a moderator who is not party to the order cannot open it.
	w.page("/order?id="+order, w.mod, 404)
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", "The licence key is rejected by the activation server."), 303, "")

	for name, session := range map[string]string{"moderator": w.mod, "admin": admin} {
		body := w.page("/order?id="+order, session, 200)
		mustContain(t, body,
			"Moderator review",
			"The licence key is rejected by the activation server.", // dispute reason
			"licence-key-ABC-123", // delivery record
			"Dispute opened",      // order events
			`href="/moderator">moderation desk</a>`,
		)
		// No buyer/vendor action forms render for a reviewer.
		mustNotContain(t, body, `action="/orders/`, `action="/reviews"`, `action="/disputes"`, "Confirm receipt", "Open a dispute")
		if name == "moderator" {
			// The actions themselves refuse a non-party (404, as on GET), so nothing can be changed from here.
			w.expect("/orders/complete", session, form("order_id", order), 404, "Order not found")
			w.expect("/orders/cancel", session, form("order_id", order, "from", statePaid), 404, "Order not found")
			w.expect("/reviews", session, form("order_id", order, "rating", "5"), 404, "Order not found")
		}
	}
	// Other users and non-disputed orders stay hidden.
	w.page("/order?id="+order, w.other, 404)
	w.page("/order?id="+notDisputed, w.mod, 404)
	w.page("/order?id="+notDisputed, admin, 404)

	// The moderator desk links each dispute to its order page.
	mustContain(t, w.page("/moderator", w.mod, 200), `href="/order?id=`+order+`"`)

	// Resolved orders stay readable for history, with the recorded decision.
	var disputeID string
	if err := w.e.DB.QueryRow("SELECT id FROM disputes WHERE order_id=$1", order).Scan(&disputeID); err != nil {
		t.Fatal(err)
	}
	w.expect("/resolve", w.mod, form("id", disputeID, "outcome", "refund", "resolution", "Key verified as invalid; refund the buyer."), 303, "")
	body := w.page("/order?id="+order, w.mod, 200)
	mustContain(t, body, "Key verified as invalid; refund the buyer.", "Resolved")
	// Parties see the dispute record on their own order page as well.
	mustContain(t, w.page("/order?id="+order, w.buyer, 200), "The licence key is rejected by the activation server.")
}

func TestCounterpartyKeysAndMessagePrefill(t *testing.T) {
	w := newOrderWorld(t)
	_, vendorPub := testPGPKey(t, "vendor")
	_, fp, err := parsePublicKey(vendorPub)
	if err != nil {
		t.Fatal(err)
	}
	var vendorHandle, buyerHandle string
	w.e.DB.QueryRow("SELECT handle FROM users WHERE id=$1", w.vendorID).Scan(&vendorHandle)
	w.e.DB.QueryRow("SELECT handle FROM users WHERE id=$1", w.buyerID).Scan(&buyerHandle)
	physical := w.e.product(w.vendorID, "physical")

	// No key yet: say so plainly on the vendor page.
	body := w.page("/vendor?id="+w.vendorID, w.buyer, 200)
	mustContain(t, body, vendorHandle+" has not added a PGP public key", "cannot send them an encrypted message until they add one", `href="/messages?to=`+vendorHandle+`"`)

	// Unverified key: armored key and grouped fingerprint shown, marked not verified.
	if _, err = w.e.DB.Exec("UPDATE users SET pgp=$1,pgp_fingerprint=$2 WHERE id=$3", vendorPub, fp, w.vendorID); err != nil {
		t.Fatal(err)
	}
	body = w.page("/vendor?id="+w.vendorID, w.other, 200)
	mustContain(t, body, formatFingerprint(fp), "BEGIN PGP PUBLIC KEY BLOCK", "Ownership not verified")
	mustNotContain(t, body, "has not added a PGP public key", "Ownership verified")
	// Anonymous visitors see the key too (public vendor profile).
	mustContain(t, w.page("/vendor?id="+w.vendorID, "", 200), formatFingerprint(fp))

	// Verified only when the ownership proof matches the saved key.
	w.e.DB.Exec("UPDATE users SET pgp_verified_at=now() WHERE id=$1", w.vendorID)
	mustContain(t, w.page("/vendor?id="+w.vendorID, w.other, 200), "Ownership verified")
	w.e.DB.Exec("UPDATE users SET pgp_fingerprint='0000' WHERE id=$1", w.vendorID)
	mustContain(t, w.page("/vendor?id="+w.vendorID, w.other, 200), "Ownership not verified")
	w.e.DB.Exec("UPDATE users SET pgp_fingerprint=$1 WHERE id=$2", fp, w.vendorID)

	// Product page contact link prefills the vendor.
	mustContain(t, w.page("/product?id="+physical, w.buyer, 200), `href="/messages?to=`+vendorHandle+`"`)

	// Order page: each party sees the other's key status; the buyer of a draft gets no shipping prompt.
	draft := w.e.order(w.buyerID, physical, "BTC", stateDraft)
	body = w.page("/order?id="+draft, w.buyer, 200)
	mustContain(t, body, formatFingerprint(fp), "Ownership verified", `href="/messages?to=`+vendorHandle+`"`)
	mustNotContain(t, body, "shipping address")
	body = w.page("/order?id="+draft, w.vendor, 200)
	mustContain(t, body, buyerHandle+" has not added a PGP public key", `href="/messages?to=`+buyerHandle+`"`)

	// Paid physical order: the buyer is told to send shipping details encrypted to the vendor's key.
	paid := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", statePaid)
	body = w.page("/order?id="+paid, w.buyer, 200)
	mustContain(t, body, "Send your shipping address", "encrypted to "+vendorHandle, "never stores shipping addresses", `href="/messages?to=`+vendorHandle+`"`)
	body = w.page("/order?id="+paid, w.vendor, 200)
	mustContain(t, body, "shipping address", "encrypted message")
	// Paid digital orders need no address.
	paidDigital := w.e.order(w.buyerID, w.e.product(w.vendorID, "digital"), "BTC", statePaid)
	mustNotContain(t, w.page("/order?id="+paidDigital, w.buyer, 200), "Send your shipping address")

	// Messages: ?to prefills a valid handle and shows that recipient's key; invalid values are ignored.
	body = w.page("/messages?to="+vendorHandle, w.buyer, 200)
	mustContain(t, body, `name="recipient" value="`+vendorHandle+`"`, formatFingerprint(fp))
	body = w.page("/messages?to=%3Cscript%3E", w.buyer, 200)
	mustContain(t, body, `name="recipient" value=""`)
	mustNotContain(t, body, "&lt;script&gt;")
	body = w.page("/messages?to="+buyerHandle, w.vendor, 200)
	mustContain(t, body, `value="`+buyerHandle+`"`, buyerHandle+" has not added a PGP public key")
	// A well-formed handle with no account is prefilled but reveals no key.
	body = w.page("/messages?to=nobody_here", w.buyer, 200)
	mustContain(t, body, `value="nobody_here"`)
	mustNotContain(t, body, "BEGIN PGP PUBLIC KEY BLOCK")
}

func TestUnreadNotificationCountInNavigation(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("notify_user", "buyer")
	nav := func(path, session string) string {
		t.Helper()
		b := e.body("GET", path, session, nil, 200)
		start := strings.Index(b, `aria-label="Main navigation"`)
		end := strings.Index(b[start:], "</nav>")
		if start < 0 || end < 0 {
			t.Fatal("no main navigation")
		}
		return b[start : start+end]
	}
	n := nav("/", s)
	mustContain(t, n, `href="/notifications"`, ">Notifications<")
	mustNotContain(t, n, "unread")
	for i, read := range []bool{false, false, false, true} {
		if _, err := e.DB.Exec("INSERT INTO notifications(id,user_id,body,is_read) VALUES($1,$2,$3,$4)", randomToken(), uid, "Update "+string(rune('A'+i)), read); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/", "/orders", "/account", "/notifications"} {
		mustContain(t, nav(path, s), `>Notifications <span class="nav-count">(3 unread)</span>`)
	}
	// The header no longer carries a desktop-only link.
	mustNotContain(t, e.body("GET", "/", s, nil, 200), `class="hide-mobile" href="/notifications"`)
	var id string
	e.DB.QueryRow("SELECT id FROM notifications WHERE user_id=$1 AND NOT is_read LIMIT 1", uid).Scan(&id)
	e.check(e.do("POST", "/notifications", s, form("id", id)), 303)
	mustContain(t, nav("/", s), `(2 unread)`)
	// Signed-out visitors get no notifications link.
	mustNotContain(t, nav("/", randomToken()), "/notifications")
}

func TestVendorDemotionArchivesListingsAndRefusesNewOrders(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("admin_"+randomToken()[:6], "admin")
	a := w.e.product(w.vendorID, "physical")
	b := w.e.product(w.vendorID, "physical")
	old := w.e.product(w.vendorID, "digital")
	w.e.DB.Exec("UPDATE products SET archived=true,archived_at=now() WHERE id=$1", old)
	paid := w.e.order(w.buyerID, a, "BTC", statePaid)
	draft := w.e.order(w.buyerID, b, "BTC", stateDraft)

	// Promoting or keeping a vendor archives nothing.
	w.expect("/admin", admin, form("action", "role", "role", "vendor", "user_id", w.vendorID), 303, "")
	if n := w.count("SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", w.vendorID); n != 2 {
		t.Fatalf("active listings after vendor->vendor: %d", n)
	}

	w.expect("/admin", admin, form("action", "role", "role", "buyer", "user_id", w.vendorID), 303, "")
	if n := w.count("SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", w.vendorID); n != 0 {
		t.Fatalf("listings still active after demotion: %d", n)
	}
	if n := w.count("SELECT count(*) FROM products WHERE vendor_id=$1 AND archived_at IS NOT NULL", w.vendorID); n != 3 {
		t.Fatalf("archived_at not set: %d", n)
	}
	if !w.e.auditExact(w.vendorID, "Archived 2 active listing(s): role changed to buyer by an administrator") {
		t.Fatal("demoted user's audit event missing")
	}
	if w.count("SELECT count(*) FROM audit_events WHERE action='Changed user role to buyer; archived 2 active listing(s)'") != 1 {
		t.Fatal("administrator audit event missing")
	}

	// Archived: new drafts and payment requests are refused; the product page is gone from the catalog.
	w.expect("/orders", w.other, form("product_id", a, "currency", "BTC"), 400, "archived")
	w.expect("/orders/pay", w.buyer, form("order_id", draft), 409, "no longer a vendor")
	// Even if a listing is active again (restored directly in the database), the non-vendor owner blocks orders.
	w.e.DB.Exec("UPDATE products SET archived=false WHERE id=$1", a)
	w.expect("/orders", w.other, form("product_id", a, "currency", "BTC"), 400, "no longer a vendor")
	// An administrator cannot restore a listing whose owner is not a vendor.
	w.expect("/listings/restore", admin, form("id", old), 409, "no longer a vendor")
	if w.state(draft) != stateDraft {
		t.Fatal("refused payment changed the draft")
	}

	// Existing open orders are unaffected: the former vendor can still fulfil the paid order.
	if w.state(paid) != statePaid {
		t.Fatal("demotion changed an open order")
	}
	w.expect("/orders/ship", w.vendor, form("order_id", paid, "note", "Posted today"), 303, "")
	if w.state(paid) != stateShipped {
		t.Fatalf("paid order after ship: %s", w.state(paid))
	}

	// Demoting to moderator archives too (the new listing and the one reactivated above).
	w.expect("/admin", admin, form("action", "role", "role", "vendor", "user_id", w.vendorID), 303, "")
	c := w.e.product(w.vendorID, "digital")
	w.expect("/admin", admin, form("action", "role", "role", "moderator", "user_id", w.vendorID), 303, "")
	var archived bool
	w.e.DB.QueryRow("SELECT archived FROM products WHERE id=$1", c).Scan(&archived)
	if !archived || !w.e.auditExact(w.vendorID, "Archived 2 active listing(s): role changed to moderator by an administrator") {
		t.Fatal("demotion to moderator left the listing active or unaudited")
	}
}
