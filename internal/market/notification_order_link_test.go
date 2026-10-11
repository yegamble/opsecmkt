package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestOrderNotificationsLinkOnlyTheirExplicitOrder(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	vendorID, vendor := w.vendorID, w.vendor
	buyerID, buyer := w.buyerID, w.buyer
	strangerID, _ := e.user("notice_link_stranger", "buyer")
	order := w.fundedPhysicalFor(buyerID, buyer, vendorID, vendor)
	product := agStr(e, "SELECT product_id FROM orders WHERE id=$1", order)

	page := e.do("GET", orderPage(order), vendor, nil)
	e.check(page, 200)
	nonce := renderedFormNonce(t, page.Body.String(), "/escrow/ship")
	e.check(e.do("POST", "/escrow/ship", vendor, url.Values{"order_id": {order}, "address": {regtestAddr("notice-link-claim")}, "password": {testPassword}, formNonceField: {nonce}}), 303)
	if got := agStr(e, "SELECT COALESCE(order_id,'') FROM notifications WHERE user_id=$1 ORDER BY created DESC,id DESC LIMIT 1", buyerID); got != order {
		t.Fatalf("state-change notice order_id=%q, want %q", got, order)
	}

	strangerOrder := e.order(strangerID, product, "BTC", statePaid)
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body) VALUES('a37-legacy-no-order',$1,'Legacy notice stays unlinked')`, buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,order_id) VALUES('a37-private-order',$1,'Private order notice',$2)`, strangerID, strangerOrder); err != nil {
		t.Fatal(err)
	}
	body := e.body("GET", "/notifications", buyer, nil, 200)
	legacyAt := strings.Index(body, "Legacy notice stays unlinked")
	if !strings.Contains(body, `href="/order?id=`+order+`"`) || legacyAt < 0 || strings.Contains(body, strangerOrder) {
		t.Fatalf("notification page links/leaks incorrect order data:\n%s", body)
	}
	articleStart := strings.LastIndex(body[:legacyAt], "<article")
	articleEnd := strings.Index(body[legacyAt:], "</article>")
	if articleStart < 0 || articleEnd < 0 || strings.Contains(body[articleStart:legacyAt+articleEnd], `href="/order?id=`) {
		t.Fatalf("legacy notification should remain visible without an order link:\n%s", body)
	}
	if got := e.do("GET", orderPage(strangerOrder), buyer, nil).Code; got != 404 {
		t.Fatalf("notification association widened order access: got %d", got)
	}
	if got := agStr(e, "SELECT COALESCE(order_id,'') FROM notifications WHERE id='a37-legacy-no-order'"); got != "" {
		t.Fatalf("legacy notice acquired an order association: %q", got)
	}
}
