package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestA60OwnListingAndInvalidDisputeCopy(t *testing.T) {
	e := newTestApp(t)
	vendorID, vendor := e.user("a60_vendor", "vendor")
	buyerID, buyer := e.user("a60_buyer", "buyer")
	product := e.product(vendorID, "physical")

	ownerPage := e.body("GET", "/product?id="+product, vendor, nil, 200)
	if strings.Contains(ownerPage, `href="/checkout?id=`+product+`"`) || strings.Contains(ownerPage, "Review order draft") {
		t.Fatal("listing owner was offered a checkout that the order handler refuses")
	}
	publicPage := e.body("GET", "/product?id="+product, buyer, nil, 200)
	if !strings.Contains(publicPage, `href="/checkout?id=`+product+`"`) {
		t.Fatal("another account lost the valid checkout link")
	}
	account := e.body("GET", "/account", vendor, nil, 200)
	if !strings.Contains(account, "Getting ready to sell?") || !strings.Contains(account, `href="/vendor-dashboard"`) {
		t.Fatal("vendor account page has no onboarding link to the vendor desk")
	}

	order := e.order(buyerID, product, "BTC", stateDisputed)
	form := url.Values{"order_id": {order}, "reason": {strings.Repeat("x", 30)}, formNonceField: {randomToken()[:32]}}
	refused := e.do("POST", "/disputes", buyer, form)
	if refused.Code != 409 || !strings.Contains(refused.Body.String(), "cannot be disputed from its current state") || strings.Contains(refused.Body.String(), "Draft orders contain no funds") {
		t.Fatalf("disputed-order resubmit got %d %q", refused.Code, refused.Body.String())
	}
	if got := agStr(e, "SELECT state FROM orders WHERE id=$1", order); got != stateDisputed {
		t.Fatalf("refused dispute resubmit changed state to %q", got)
	}
}

func TestA60AccountPgpLimitMatchesServerByteCap(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("a60_account", "buyer")
	page := e.body("GET", "/account", session, nil, 200)
	if !strings.Contains(page, `<textarea name="pgp" maxlength="16384"`) || !strings.Contains(page, "At most 16 KiB (16,384 UTF-8 bytes)") {
		t.Fatal("PGP input does not communicate its server-side byte limit")
	}
}
