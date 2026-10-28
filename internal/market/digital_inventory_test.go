package market

import (
	"database/sql"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDigitalInventoryModeImportAndOwnerSecrecy(t *testing.T) {
	e := newTestApp(t)
	owner, session := e.user("unit_owner", "vendor")
	_, admin := e.user("unit_admin", "admin")
	listing := e.product(owner, "digital")
	e.check(e.do("POST", "/listings/digital-mode", session, url.Values{"id": {listing}, "mode": {"inventory"}}), 303)
	e.check(e.do("POST", "/listings/digital-units", session, url.Values{"id": {listing}, "format": {"lines"}, "contents": {"key-one\nhttps://example.invalid/download-two"}}), 303)
	if agInt(e, "SELECT stock FROM products WHERE id=$1", listing) != 2 {
		t.Fatal("unit import did not derive stock")
	}
	own := e.body("GET", "/listing-edit?id="+listing, session, nil, 200)
	mustContain(t, own, "key-one", "https://example.invalid/download-two", "Digital unit inventory")
	other := e.body("GET", "/listing-edit?id="+listing, admin, nil, 200)
	mustNotContain(t, other, "key-one", "https://example.invalid/download-two")
	for _, path := range []string{"/listings/digital-mode", "/listings/digital-units"} {
		e.check(e.do("POST", path, admin, url.Values{"id": {listing}, "mode": {"manual"}, "contents": {"injected"}}), 404)
	}
	e.check(e.do("POST", "/listings/digital-units", session, url.Values{"id": {listing}, "contents": {"key-one"}}), 409)
	agExec(e, "UPDATE products SET archived=true,archived_at=now()-interval '60 days' WHERE id=$1", listing)
	e.check(e.do("POST", "/listings/digital-units", session, url.Values{"id": {listing}, "contents": {"do-not-accept-expiring-stock"}}), 409)
	if strings.Contains(agStr(e, "SELECT coalesce(string_agg(action,' '),'') FROM audit_events"), "key-one") {
		t.Fatal("secret leaked to audit")
	}
}

func digitalInventoryFixture(t *testing.T, x *xmrEscrowEnv, contents ...string) string {
	t.Helper()
	p := x.digital(x.vendor)
	x.check(x.do("POST", "/listings/digital-mode", x.vendorSess, url.Values{"id": {p}, "mode": {"inventory"}}), 303)
	for _, content := range contents {
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {content}}), 303)
	}
	return p
}
func digitalQuantityDraft(t *testing.T, x *xmrEscrowEnv, p string, quantity string) string {
	t.Helper()
	w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {p}, "currency": {"XMR"}, "quantity": {quantity}})
	x.check(w, 303)
	return strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
}

func TestDigitalManualWholeOrderRealEscrowCheckoutAndReplay(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := x.digital(x.vendor)
	x.check(x.do("POST", "/listings/digital-mode", x.vendorSess, url.Values{"id": {p}, "mode": {"manual"}}), 303)
	agExec(x.testEnv, "UPDATE products SET delivery_content='' WHERE id=$1", p)
	x.mustPoll()
	order := digitalQuantityDraft(t, x, p, "2")
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("manual-batch-paid"), 2*xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 0 {
		t.Fatal("manual mode auto delivered or failed funding")
	}
	content := "First purchased item\nhttps://example.invalid/first\n\nSecond purchased item\nsecond key"
	page := x.page("/order?id="+order, x.vendorSess)
	f := url.Values{"order_id": {order}, "content": {content}, formNonceField: {renderedFormNonce(t, page, "/orders/deliver")}}
	x.check(x.do("POST", "/orders/deliver", x.vendorSess, f), 303)
	x.check(x.do("POST", "/orders/deliver", x.vendorSess, f), 303)
	if x.state(order) != stateDelivered || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 1 || testDecrypt(t, accountPGPFixture.key, agStr(x.testEnv, "SELECT content FROM deliveries WHERE order_id=$1", order)) != content {
		t.Fatal("manual delivery was not one whole-order record")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 1 || agInt(x.testEnv, "SELECT count(*) FROM messages") != 0 {
		t.Fatal("batch duplicated escrow or leaked into direct inbox")
	}
	mustContain(t, x.page("/order?id="+order, x.buyerSess), "Encrypted to the buyer", "BEGIN PGP MESSAGE")
	mustNotContain(t, x.page("/order?id="+order, x.buyerSess), "First purchased item", "Second purchased item")
}

func TestDigitalInventoryAutomaticBatchReservationCancellationAndPrivacy(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, "unit A\nmultiline", "https://example.invalid/unit-b", "unit C")
	x.mustPoll()
	order := digitalQuantityDraft(t, x, p, "2")
	x.mustCheckout(order)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 1 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", order) != 2 {
		t.Fatal("checkout did not reserve exact distinct units")
	}
	old := time.Now().UTC().Add(-time.Hour)
	// An old listing tombstone never erases units now claimed by an order.
	runInTx(t, x.DB, func(tx *sql.Tx) error {
		return applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "listing_private", ID: p, At: old})
	})
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content<>''", order) != 2 {
		t.Fatal("listing replay erased a reserved order payload")
	}
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}}), 303)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 3 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id IS NULL AND product_id=$1", p) != 3 {
		t.Fatal("cancel did not return undisclosed units")
	}
	next := digitalQuantityDraft(t, x, p, "2")
	addr := x.mustCheckout(next)
	// Replaying an expired old order cannot erase its returned units now assigned to a later order.
	runInTx(t, x.DB, func(tx *sql.Tx) error {
		return applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: order, At: time.Now().UTC().Add(time.Second)})
	})
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content<>''", next) != 2 {
		t.Fatal("old order replay erased reused undisclosed stock")
	}
	x.fake.payAddress(addr, hex64("distinct-batch-paid"), 2*xmrListingPrice, 10)
	x.mustPoll()
	x.mustPoll()
	want, err := digitalBatch([]string{"unit A\nmultiline", "https://example.invalid/unit-b"})
	if err != nil {
		t.Fatal(err)
	}
	if x.state(next) != stateDelivered || agInt(x.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", next) != 1 || testDecrypt(t, accountPGPFixture.key, agStr(x.testEnv, "SELECT content FROM deliveries WHERE order_id=$1", next)) != want {
		t.Fatal("auto delivery lost distinct units or duplicated batch")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND delivered_at IS NOT NULL", next) != 2 {
		t.Fatal("disclosed units not consumed")
	}
	runInTx(t, x.DB, func(tx *sql.Tx) error {
		return applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: next, At: time.Now().UTC().Add(time.Second)})
	})
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content='' AND fingerprint IS NULL", next) != 2 {
		t.Fatal("order retention retained payload/digest")
	}
	o, err := scanOrder(x.DB.QueryRow(orderQuery+" WHERE o.id=$1", next))
	if err != nil {
		t.Fatal(err)
	}
	runInTx(t, x.DB, func(tx *sql.Tx) error { return releaseDigitalUnits(t.Context(), tx, &o) })
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 1 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL", p) != 1 {
		t.Fatal("disclosed or expired units recycled")
	}
}

func TestDigitalInventorySizeGateAndStockEditor(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, strings.Repeat("a", 16000), strings.Repeat("b", 16000))
	x.mustPoll()
	before := x.fake.count()
	x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {p}, "currency": {"XMR"}, "quantity": {"2"}}), 409)
	if x.fake.count() != before || agInt(x.testEnv, "SELECT count(*) FROM escrows") != 0 || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 2 {
		t.Fatal("oversized batch changed payment or stock")
	}
	// Recheck at checkout even if a draft predates the import or a stock/content edit.
	order := x.draft(p)
	agExec(x.testEnv, "UPDATE orders SET quantity=2 WHERE id=$1", order)
	x.check(x.checkout(x.buyerSess, order, testXMRAddr("oversized checkout", true)), 409)
	if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1", order) != 0 {
		t.Fatal("oversize checkout allocated units")
	}
	f := inventoryForm(p, map[string]string{"stock": "50", "stock_seen": "2", "kind": "digital"})
	x.check(x.do("POST", "/listings/update", x.vendorSess, f), 409)
	f = inventoryForm(p, map[string]string{"stock": "2", "stock_seen": "2", "kind": "physical"})
	x.check(x.do("POST", "/listings/update", x.vendorSess, f), 409)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 2 {
		t.Fatal("editor inflated distinct stock")
	}
}

func TestDigitalInventoryConcurrentCheckoutsNeverReuseUnits(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, "race one", "race two", "race three")
	x.mustPoll()
	orders := []string{x.draft(p), ""}
	buyer, sess := x.user("digital_race_buyer", "buyer")
	orders[1] = x.order(buyer, p, "XMR", stateDraft)
	sessions := []string{x.buyerSess, sess}
	for _, id := range orders {
		agExec(x.testEnv, "UPDATE orders SET quantity=2 WHERE id=$1", id)
	}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range orders {
		wg.Go(func() {
			codes[i] = x.checkout(sessions[i], orders[i], testXMRAddr("digital race "+orders[i], true)).Code
		})
	}
	wg.Wait()
	if !((codes[0] == 303 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 303)) {
		t.Fatalf("checkout race codes %v", codes)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NOT NULL", p) != 2 || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 1 || agInt(x.testEnv, "SELECT count(*) FROM escrows") != 1 {
		t.Fatal("concurrent checkout reused or oversold units")
	}
}

func TestDigitalInventoryFormerVendorCleanupKeepsClaimedUnits(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("unit_former_vendor", "vendor")
	e.user("unit_role_admin", "admin")
	p := e.product(owner, "digital")
	e.check(e.do("POST", "/listings/digital-mode", s, url.Values{"id": {p}, "mode": {"inventory"}}), 303)
	e.check(e.do("POST", "/listings/digital-units", s, url.Values{"id": {p}, "contents": {"unused secret"}}), 303)
	buyer, _ := e.user("unit_cleanup_buyer", "buyer")
	order := e.order(buyer, p, "XMR", statePaid)
	agExec(e, "INSERT INTO digital_units(id,product_id,content,fingerprint,order_id,reserved_at) VALUES($1,$2,'reserved secret',$3,$4,now())", randomToken(), p, digest("reserved secret"), order)
	if _, err := SetRole(t.Context(), e.DB, "unit_former_vendor", "buyer"); err != nil {
		t.Fatal(err)
	}
	page := e.body("GET", "/delivery-cleanup", s, nil, 200)
	mustContain(t, page, p)
	mustNotContain(t, page, "unused secret", "reserved secret")
	e.check(e.do("POST", "/delivery-cleanup", s, url.Values{"id": {p}, "password": {testPassword}}), 303)
	if agInt(e, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL", p) != 0 || agInt(e, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content='reserved secret'", order) != 1 || agInt(e, "SELECT stock FROM products WHERE id=$1", p) != 0 {
		t.Fatal("cleanup lost reserved content or retained available secret")
	}
}

func TestDigitalInventoryRetentionHonorsOrderPreservation(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	now := time.Now().UTC()
	vendor, _ := e.user("unit_hold_vendor", "vendor")
	buyer, _ := e.user("unit_hold_buyer", "buyer")
	p := e.product(vendor, "digital")
	o := e.order(buyer, p, "XMR", stateCompleted)
	agExec(e, "UPDATE orders SET financial_closed_at=$2 WHERE id=$1", o, now.Add(-40*24*time.Hour))
	agExec(e, "INSERT INTO digital_units(id,product_id,content,fingerprint,order_id,reserved_at,delivered_at) VALUES($1,$2,'held unit secret',$3,$4,$5,$5)", randomToken(), p, digest("held unit secret"), o, now.Add(-50*24*time.Hour))
	h := retentionHold{Scope: "order", RecordID: o, Authority: "Case obligation", Reason: "Specific record", Owner: "Case owner", Readers: "Case handler", ReleaseCondition: "Written release", BeganAt: now.Add(-time.Hour), ReviewAt: now.Add(24 * time.Hour)}
	commitRetentionHold(t, e, h)
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if agInt(e, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content='held unit secret'", o) != 1 {
		t.Fatal("preservation erased inventory secret")
	}
}

func TestDigitalInventoryNewListingExplicitModeAndPhysicalSafety(t *testing.T) {
	e := newTestApp(t)
	owner, s := e.user("unit_new_owner", "vendor")
	for _, tc := range []struct {
		kind, mode string
		stock      int
	}{{"digital", "", 5}, {"digital", "inventory", 0}, {"physical", "inventory", 5}} {
		f := inventoryForm("", map[string]string{"kind": tc.kind, "digital_delivery_mode": tc.mode})
		w := e.do("POST", "/listings", s, f)
		e.check(w, 303)
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		p := location.Query().Get("id")
		mode := agStr(e, "SELECT digital_delivery_mode FROM products WHERE id=$1", p)
		want := "manual"
		if tc.kind == "digital" && tc.mode == "inventory" {
			want = "inventory"
		}
		if mode != want || agInt(e, "SELECT stock FROM products WHERE id=$1", p) != tc.stock {
			t.Fatalf("%s/%s mode %s stock", tc.kind, tc.mode, mode)
		}
		if want == "manual" && tc.kind == "digital" {
			mustContain(t, e.body("GET", "/product?id="+p, s, nil, 200), "delivered by the vendor after payment")
		}
	}
	legacy := e.product(owner, "digital")
	if agStr(e, "SELECT digital_delivery_mode FROM products WHERE id=$1", legacy) != "reusable" {
		t.Fatal("migration default broke old listing compatibility")
	}
}

func TestDigitalInventoryPaidCancellationCannotReviveExpiredListingStock(t *testing.T) {
	x := newXMREscrowEnv(t)
	now := time.Now().UTC()
	if err := x.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: retentionPrivateTestDir(t)}); err != nil {
		t.Fatal(err)
	}
	ageRetentionEnrollment(t, x.A, now.Add(-31*24*time.Hour))
	p := digitalInventoryFixture(t, x, "expired reserved key one", "expired reserved key two")
	x.mustPoll()
	order := digitalQuantityDraft(t, x, p, "2")
	address := x.mustCheckout(order)
	agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='Listing taken down for review',archived_at=$2,updated=$2 WHERE id=$1", p, now.Add(-31*24*time.Hour))
	x.fake.payAddress(address, hex64("expired inventory paid cancellation"), 2*xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		t.Fatal("taken-down digital inventory was automatically delivered")
	}
	runInTx(t, x.DB, func(tx *sql.Tx) error {
		return applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "listing_private", ID: p, At: now.Add(time.Second)})
	})
	if agInt(x.testEnv, "SELECT count(*) FROM products WHERE id=$1 AND private_expired_at IS NOT NULL", p) != 1 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content<>''", order) != 2 {
		t.Fatal("listing expiry did not preserve claimed paid content")
	}
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, payoutConfirmed(form("order_id", order, "from", statePaid), 2*xmrListingPrice)), 303)
	runRetentionAt(t, x.A, now.Add(2*time.Second))
	if x.state(order) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL AND (content<>'' OR fingerprint IS NOT NULL)", p) != 0 || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 0 {
		t.Fatal("cancellation revived expired available stock or its plaintext/digest")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM products WHERE id=$1 AND private_expired_at IS NOT NULL", p) != 1 {
		t.Fatal("cancellation reset the listing's established private expiry")
	}
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content<>'' AND fingerprint IS NOT NULL", order) != 2 {
		t.Fatal("cancellation prematurely erased the order's retained private claims")
	}
	// Once that order's own private lifecycle expires, the retained claims lose
	// both their text and digest while remaining unavailable for reuse.
	runInTx(t, x.DB, func(tx *sql.Tx) error {
		return applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: order, At: now.Add(31 * 24 * time.Hour)})
	})
	if agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id=$1 AND content='' AND fingerprint IS NULL", order) != 2 || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 0 {
		t.Fatal("expired canceled-order claims retained secrets or became available")
	}
}

func TestDigitalInventoryRejectsRecipientCiphertextBeforePayment(t *testing.T) {
	x := newXMREscrowEnv(t)
	p := digitalInventoryFixture(t, x, "plain unit")
	key, _ := testPGPKey(t, "inventory-cannot-know-future-buyer")
	ciphertext := encryptToRecipients(t, "encrypted unit", key)
	for _, format := range []string{"", "lines"} {
		x.check(x.do("POST", "/listings/digital-units", x.vendorSess, url.Values{"id": {p}, "contents": {ciphertext}, "format": {format}}), 400)
	}
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", p) != 1 {
		t.Fatal("refused encrypted inventory changed stock")
	}
	// Older imported rows cannot bypass the same rule when composing a batch.
	agExec(x.testEnv, "INSERT INTO digital_units(id,product_id,content,fingerprint,created) VALUES($1,$2,$3,$4,now()-interval '1 day')", randomToken(), p, ciphertext, digest(ciphertext))
	agExec(x.testEnv, "UPDATE products SET stock=2 WHERE id=$1", p)
	x.mustPoll()
	before := x.fake.count()
	x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {p}, "currency": {"XMR"}, "quantity": {"2"}}), 409)
	order := x.draft(p)
	agExec(x.testEnv, "UPDATE orders SET quantity=2 WHERE id=$1", order)
	x.check(x.checkout(x.buyerSess, order, testXMRAddr("encryptedinventory-rejected", true)), 409)
	if x.fake.count() != before || agInt(x.testEnv, "SELECT count(*) FROM escrows") != 0 || agInt(x.testEnv, "SELECT count(*) FROM digital_units WHERE order_id IS NOT NULL") != 0 {
		t.Fatal("encrypted inventory rejection requested payment or claimed units")
	}
}
