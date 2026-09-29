package market

import (
	"html"
	"net/url"
	"testing"
)

// A-158: a buyer about to encrypt their shipping address on a paid physical order is told when the vendor's key was
// changed or re-verified after the order was placed, since a swapped key would send the address to someone else.

const keyChangedAfterOrder = ", after this order was placed — confirm with the vendor before sending your address"

func TestVendorKeyChangedAfterOrderWarnsBuyer(t *testing.T) {
	p := newPayEnv(t)
	vendorKey, vendorPub := testPGPKey(t, "a158_vendor")
	// The vendor's key was saved and proved a day before the order.
	p.provePGPAt(p.vendor.ID, vendorPub, "1 day")
	p.usableKey(p.buyer.ID) // so the vendor's view shows a key block too
	order, _ := p.newOrder(statePaid)
	today := agStr(p.testEnv, "SELECT to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD')")
	warning := "This key was changed or re-verified on " + today + " UTC" + keyChangedAfterOrder
	page := func(sess string) string {
		return html.UnescapeString(p.body("GET", "/order?id="+order, sess, nil, 200))
	}

	b := page(p.buyerSess)
	mustContain(t, b, sendAddressLabel, "Ownership verified")
	mustNotContain(t, b, keyChangedAfterOrder)

	// Re-verified after the order: warned.
	p.check(p.do("POST", "/pgp/challenge", p.vendorSess, url.Values{"kind": {"sign"}}), 303)
	challenge := agStr(p.testEnv, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", p.vendor.ID)
	p.check(p.do("POST", "/pgp/verify", p.vendorSess, url.Values{"signature": {testClearsign(t, vendorKey, challenge)}}), 303)
	mustContain(t, page(p.buyerSess), warning, sendAddressLabel)
	// Only the buyer is warned; the vendor's own view and a digital order are unchanged.
	mustNotContain(t, page(p.vendorSess), keyChangedAfterOrder)
	digital := p.testEnv.order(p.buyer.ID, p.product(p.vendor.ID, "digital"), "BTC", statePaid)
	mustNotContain(t, html.UnescapeString(p.body("GET", "/order?id="+digital, p.buyerSess, nil, 200)), keyChangedAfterOrder)

	// Changed after the order (the /account key-change path records when): warned, the new key unverified.
	p.provePGPAt(p.vendor.ID, vendorPub, "1 day")
	mustNotContain(t, page(p.buyerSess), keyChangedAfterOrder)
	_, otherPub := testPGPKey(t, "a158_other")
	p.check(p.do("POST", "/account", p.vendorSess, url.Values{"pgp": {otherPub}, "password": {testPassword}}), 303)
	b = page(p.buyerSess)
	mustContain(t, b, warning, "Ownership not verified", sendAddressLabel)

	// A key changed before an order is not a warning on that order.
	later, _ := p.newOrder(statePaid)
	mustNotContain(t, html.UnescapeString(p.body("GET", "/order?id="+later, p.buyerSess, nil, 200)), keyChangedAfterOrder)
}

// provePGPAt stores armored as userID's key, saved and proved the given interval ago.
func (p *payEnv) provePGPAt(userID, armored, ago string) {
	p.t.Helper()
	_, fp, err := parsePublicKey(armored)
	if err != nil {
		p.t.Fatal(err)
	}
	agExec(p.testEnv, "UPDATE users SET pgp=$2,pgp_fingerprint=$3,pgp_verified_at=now()-$4::interval,pgp_changed_at=now()-$4::interval WHERE id=$1", userID, armored, fp, ago)
}
