package market

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func TestOrderDraftValidation(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("val_buyer", "buyer")
	vendorID, vendor := e.user("val_vendor", "vendor")
	e.enableOrderPGP(buyerID, vendorID)
	product := e.product(vendorID, "physical")
	draft := func(p, cur string) url.Values { return url.Values{"product_id": {p}, "currency": {cur}} }

	agExpect(e, "/orders", vendor, draft(product, "XMR"), 400, "cannot order your own listing")
	for _, cur := range []string{"", "ETH", "btc", "BTC"} {
		agExpect(e, "/orders", buyer, draft(product, cur), 400, "This market accepts Monero (XMR) only")
	}
	agExpect(e, "/orders", buyer, draft("missing", "XMR"), 400, "Product unavailable")
	agExec(e, "UPDATE products SET stock=0 WHERE id=$1", product)
	agExpect(e, "/orders", buyer, draft(product, "XMR"), 400, "Product unavailable")
	if n := agInt(e, "SELECT count(*) FROM orders"); n != 0 {
		t.Fatalf("refused drafts created %d orders", n)
	}
	agExec(e, "UPDATE products SET stock=1 WHERE id=$1", product)
	e.check(e.do("POST", "/orders", buyer, draft(product, "XMR")), 303)
	if agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND currency='XMR' AND amount=500000000000 AND state='draft'", buyerID) != 1 {
		t.Fatal("valid draft not saved at the listing's XMR price")
	}
}

func TestProfileKeyRejectsRevokedAndPrivateMaterial(t *testing.T) {
	e := newTestApp(t)
	id, s := e.keylessUser("val_keys", "buyer")

	revoked, err := openpgp.NewEntity("revoked", "", "revoked@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	if err = revoked.RevokeKey(packet.KeyCompromised, "test", nil); err != nil {
		t.Fatal(err)
	}
	var rev bytes.Buffer
	w, _ := armor.Encode(&rev, openpgp.PublicKeyType, nil)
	if err = revoked.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, _, err = parsePublicKey(rev.String()); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked key parsed: %v", err)
	}

	// Private key packets inside a PUBLIC KEY BLOCK header (the armor type alone must not be trusted).
	secret, _ := testPGPKey(t, "secret")
	var priv bytes.Buffer
	w, _ = armor.Encode(&priv, openpgp.PublicKeyType, nil)
	if err = secret.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()

	for name, tc := range map[string]struct{ key, msg string }{
		"revoked": {rev.String(), "this key is revoked"},
		"private": {priv.String(), "private key material rejected"},
	} {
		if b := e.body("POST", "/account", s, url.Values{"pgp": {tc.key}, "password": {testPassword}}, 400); !strings.Contains(b, tc.msg) {
			t.Errorf("%s: %s", name, b)
		}
	}
	if agStr(e, "SELECT pgp||'|'||pgp_fingerprint||'|'||xmpp FROM users WHERE id=$1", id) != "||" || e.auditHas(id, "PGP key") {
		t.Fatal("rejected key (or the rest of the form) was stored")
	}
}

func TestRegisterBeforeInstallRefused(t *testing.T) {
	e := newTestApp(t)
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"early_bird"}, "password": {testPassword}}, 403, "Complete installation first")
	if agInt(e, "SELECT count(*) FROM users") != 0 || agInt(e, "SELECT count(*) FROM sessions") != 0 {
		t.Fatal("registration before install created data")
	}
	w := e.do("GET", "/register", randomToken(), nil)
	e.check(w, 303)
	if w.Header().Get("Location") != "/setup" {
		t.Fatalf("register page before install redirected to %q", w.Header().Get("Location"))
	}
}

func TestSetupSiteNameLimit(t *testing.T) {
	e := newTestApp(t)
	setup := func(name string) url.Values {
		return url.Values{"token": {testSetupToken}, "handle": {"first_admin"}, "password": {testPassword}, "site_name": {name}}
	}
	agExpect(e, "/setup", randomToken(), setup(strings.Repeat("n", 81)), 400, "Site name too long")
	agExpect(e, "/setup", randomToken(), setup(strings.Repeat("é", 81)), 400, "Site name too long")
	// The admin row inserted before the check is rolled back with the rest of the transaction.
	if agInt(e, "SELECT count(*) FROM users") != 0 || agInt(e, "SELECT count(*) FROM settings WHERE key='installed'") != 0 || agInt(e, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatal("refused setup left a partial installation")
	}
	agExpect(e, "/setup", randomToken(), url.Values{"token": {"wrong-token"}, "handle": {"first_admin"}, "password": {testPassword}}, 403, "token incorrect")
	name := strings.Repeat("é", 80)
	e.check(e.do("POST", "/setup", randomToken(), setup(name)), 303)
	if agStr(e, "SELECT value FROM settings WHERE key='site_name'") != name || agStr(e, "SELECT role FROM users WHERE handle='first_admin'") != "admin" {
		t.Fatal("80-character multibyte site name not accepted")
	}
}

func TestResolveResolutionBounds(t *testing.T) {
	w := newDisputeOrderWorld(t)
	order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The parcel never arrived at all.")
	_, admin := w.e.user("resolve_bounds_admin", "admin")
	amount := int64(agInt(w.e, "SELECT amount FROM orders WHERE id=$1", order))
	for _, text := range []string{"", "too short", strings.Repeat("r", 19), strings.Repeat("r", 5001)} {
		w.expect("/resolve", admin, form("id", dispute, "outcome", "refund", "resolution", text), 400, "20–5000")
	}
	if agStr(w.e, "SELECT status||'|'||outcome||'|'||resolution FROM disputes WHERE id=$1", dispute) != "Open||" || w.state(order) != stateDisputed {
		t.Fatal("refused resolution changed the dispute")
	}
	w.expect("/resolve", admin, resolveConfirmed(form("id", dispute, "outcome", "refund", "resolution", strings.Repeat("r", 5000)), amount), 303, "")
	if agStr(w.e, "SELECT outcome FROM disputes WHERE id=$1", dispute) != "refund" || w.state(order) != stateResolved {
		t.Fatal("5000-character resolution not accepted")
	}
}

func TestOrderNoteLength(t *testing.T) {
	w := newDisputeOrderWorld(t)
	order := w.fundedPhysicalFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	beforeEvents := w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order)
	beforeSettlements := w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order)
	w.expect("/escrow-xmr/ship", w.vendor, form("order_id", order, "address", testXMRAddr("note-length-claim", true), "password", testPassword, "note", strings.Repeat("n", 501)), 400, "under 500 characters")
	w.expect("/orders/cancel", w.buyer, form("order_id", order, "from", statePaid, "note", strings.Repeat("n", 501)), 400, "under 500 characters")
	if w.state(order) != statePaid || w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != beforeEvents ||
		w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) != beforeSettlements {
		t.Fatal("refused note changed the order")
	}
	// The limit counts characters, not bytes: 500 two-byte characters are accepted and stored intact.
	note := strings.Repeat("é", 500)
	w.expect("/escrow-xmr/ship", w.vendor, form("order_id", order, "address", testXMRAddr("note-length-claim", true), "password", testPassword, "note", note), 303, "")
	if w.state(order) != stateShipped || agStr(w.e, "SELECT note FROM order_events WHERE order_id=$1 AND from_state='paid' AND to_state='shipped'", order) != note {
		t.Fatal("500-character note not recorded")
	}
}

func TestMessageRecipientNotFound(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("val_sender", "buyer")
	body := "-----BEGIN PGP MESSAGE-----\n\n" + strings.Repeat("A", 80) + "\n-----END PGP MESSAGE-----"
	for _, r := range []string{"ghost_user", "", "VAL_SENDER "} {
		agExpect(e, "/messages", s, url.Values{"recipient": {r}, "body": {body}}, 403, messageNotPermitted)
	}
	if agInt(e, "SELECT count(*) FROM messages") != 0 || agInt(e, "SELECT count(*) FROM notifications") != 0 || e.auditHas(id, "Sent OpenPGP") {
		t.Fatal("message to an unknown recipient stored data")
	}
}

func TestDeliverTwiceKeepsFirstContent(t *testing.T) {
	w := newDisputeOrderWorld(t)
	order := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", "first licence key"), 303, "")
	w.expect("/orders/deliver", w.vendor, form("order_id", order, "content", "second licence key"), 409, "")
	content := commerceDelivery(w.e, order)
	deliveries := w.count("SELECT count(*) FROM deliveries WHERE order_id=$1", order)
	events := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state='delivered'", order)
	state := w.state(order)
	if content != "first licence key" || deliveries != 1 || events != 1 || state != stateCompleted {
		t.Fatalf("second delivery changed data: content=%q deliveries=%d delivered events=%d state=%s", content, deliveries, events, state)
	}
}

func TestUnknownAndMissingResources404(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("val_viewer", "buyer")
	vendorID, _ := e.user("val_seller", "vendor")
	otherID, _ := e.user("val_other", "buyer")
	product := e.product(vendorID, "physical")
	foreign := e.order(otherID, product, "XMR", stateDraft)
	for _, path := range []string{"/no-such-page", "/admin/../secret", "/product?id=missing", "/product", "/vendor?id=missing", "/vendor?id=" + buyerID, "/checkout?id=missing", "/order?id=missing", "/order?id=" + foreign} {
		// A-514: the shared refusal page, the same for every kind of missing page.
		if w := e.do("GET", path, buyer, nil); w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
			!strings.Contains(w.Body.String(), `role="alert">Page not found.</div>`) {
			t.Errorf("GET %s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	agExpect(e, "/no-such-action", buyer, nil, 404, "Unknown action")
	agExpect(e, "/no-such-action", randomToken(), nil, 404, "Unknown action")
	// Existing resources still render (the 404s above are not a broken fixture).
	for _, path := range []string{"/product?id=" + product, "/vendor?id=" + vendorID} {
		e.check(e.do("GET", path, buyer, nil), 200)
	}
}

func TestHealthzReportsDatabase(t *testing.T) {
	a := testHTTPApp(false)
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a.db = db
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 503 || !regexp.MustCompile(`^unavailable\nReference: [0-9a-f]{8}\n$`).MatchString(w.Body.String()) {
		t.Fatalf("healthz with the database down: %d %q", w.Code, w.Body.String())
	}
}

func TestHealthzWithDatabase(t *testing.T) {
	e := newTestApp(t)
	if b := e.body("GET", "/healthz", randomToken(), nil, 200); b != "ok" {
		t.Fatalf("healthz body %q", b)
	}
	e.A.health.Close() // a closed pool fails the ping exactly like an unreachable server (A-178: its own connection)
	w := e.do("GET", "/healthz", randomToken(), nil)
	if w.Code != 503 {
		t.Fatalf("healthz after the database closed: %d", w.Code)
	}
}

func TestStaticRejectsPost(t *testing.T) {
	a := testHTTPApp(false)
	for _, path := range []string{"/static/auth.css", "/static/", "/static/missing.css"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x=1"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/auth.css", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("GET static: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}
