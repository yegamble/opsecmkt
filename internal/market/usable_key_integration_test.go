package market

import (
	"bytes"
	"html"
	"net/url"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// A-68 and A-83: a saved key nothing can be encrypted to (expired, revoked or signing-only) is not offered as a
// contact key, and a buyer cannot pay for a physical order whose vendor has no key that can receive encrypted
// messages, since the shipping address could then not be sent.

const (
	noUsableKeyNote  = "cannot receive encrypted messages"
	noUsableKeyPay   = "Payment unavailable. This vendor has no usable encryption key, so you could not send your shipping address. Ask them to add one."
	noUsableKeyPaid  = "message the vendor or open a dispute"
	sendAddressLabel = "Send encrypted shipping details"
)

// armoredPublicKey returns key's public packets armored.
func armoredPublicKey(t *testing.T, key *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// testUnusableKeys returns keys that parse (and so could be saved) but that nothing can be encrypted to now: the
// whole key expired, no encryption subkey (signing only), and a valid primary key whose only encryption subkey
// expired or was revoked.
func testUnusableKeys(t *testing.T, name string) map[string]string {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	then := func() time.Time { return past }
	newKey := func(lifetime uint32) *openpgp.Entity {
		e, err := openpgp.NewEntity(name, "", name+"@example.test", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Time: then, KeyLifetimeSecs: lifetime})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	keys := map[string]string{}
	keys["expired key"] = armoredPublicKey(t, newKey(3600))

	signing := newKey(0)
	signing.Subkeys = nil
	keys["signing-only key"] = armoredPublicKey(t, signing)

	subExpired := newKey(0)
	sub := &subExpired.Subkeys[0]
	lifetime := uint32(3600)
	sub.Sig.KeyLifetimeSecs = &lifetime
	if err := sub.Sig.SignKey(sub.PublicKey, subExpired.PrivateKey, &packet.Config{Time: then}); err != nil {
		t.Fatal(err)
	}
	keys["expired encryption subkey"] = armoredPublicKey(t, subExpired)

	subRevoked := newKey(0)
	if err := subRevoked.RevokeSubkey(&subRevoked.Subkeys[0], packet.KeyCompromised, "test", nil); err != nil {
		t.Fatal(err)
	}
	keys["revoked encryption subkey"] = armoredPublicKey(t, subRevoked)
	return keys
}

// TestCanReceiveEncrypted: the one check (canReceiveEncrypted) accepts a current key with an encryption subkey and
// refuses every unusable variant, each of which still parses as a public key.
func TestCanReceiveEncrypted(t *testing.T) {
	now := time.Now()
	_, usable := testPGPKey(t, "usable_key")
	key, _, err := parsePublicKey(usable)
	if err != nil || !canReceiveEncrypted(key, now) {
		t.Fatalf("a current key with an encryption subkey is not usable: %v", err)
	}
	if canReceiveEncrypted(nil, now) {
		t.Fatal("no key is usable")
	}
	for variant, armored := range testUnusableKeys(t, "unusable_key") {
		key, _, err := parsePublicKey(armored)
		if err != nil {
			t.Fatalf("%s does not parse: %v", variant, err)
		}
		if canReceiveEncrypted(key, now) {
			t.Fatalf("%s reported usable", variant)
		}
		if _, err = encryptTo(key, "test"); err == nil {
			t.Fatalf("%s: encryption succeeded, so the variant does not test an unusable key", variant)
		}
	}
}

// setKey stores armored as userID's saved key (as /account would, including a key that later stops being usable).
func (e *testEnv) setKey(userID, armored string) {
	e.t.Helper()
	fp := ""
	if armored != "" {
		_, parsed, err := parsePublicKey(armored)
		if err != nil {
			e.t.Fatal(err)
		}
		fp = parsed
	}
	if _, err := e.DB.Exec("UPDATE users SET pgp=$1,pgp_fingerprint=$2 WHERE id=$3", armored, fp, userID); err != nil {
		e.t.Fatal(err)
	}
}

// usableKey saves a freshly generated key that can receive encrypted messages for userID: a vendor needs one before
// buyers can pay for its physical listings (A-83).
func (e *testEnv) usableKey(userID string) {
	e.t.Helper()
	_, key := testPGPKey(e.t, "vendor_"+userID[:8])
	e.setKey(userID, key)
}

// TestHuntUnusableEncryptionKeyShownAsUsableContact (A-68, HUNT2-C-2): an expired, signing-only, or subkey-expired
// or -revoked key is shown wherever a contact key is offered (vendor page, message recipient, order page, dispute
// staff) as "No usable key", never with its fingerprint or armored text, since nothing can be encrypted to it.
func TestHuntUnusableEncryptionKeyShownAsUsableContact(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("hunt_buyer", "buyer")
	i := 0
	for variant, armored := range testUnusableKeys(t, "hunt_contact") {
		i++
		suffix := string(rune('a' + i))
		vendorID, _ := e.user("hunt_vendor_"+suffix, "vendor")
		modID, _ := e.user("hunt_mod_"+suffix, "moderator")
		e.setKey(vendorID, armored)
		e.setKey(modID, armored)
		_, fp, _ := parsePublicKey(armored)
		product := e.product(vendorID, "physical")
		paid := e.order(buyerID, product, "BTC", statePaid)
		disputed := e.order(buyerID, product, "BTC", stateDisputed)
		for name, page := range map[string]string{
			"vendor page":     e.body("GET", "/vendor?id="+vendorID, "", nil, 200),
			"messages":        e.body("GET", "/messages?to=hunt_vendor_"+suffix, buyer, nil, 200),
			"order":           e.body("GET", "/order?id="+paid, buyer, nil, 200),
			"dispute staff":   e.body("GET", "/order?id="+disputed, buyer, nil, 200),
			"staff recipient": e.body("GET", "/messages?to=hunt_mod_"+suffix, buyer, nil, 200),
		} {
			page = html.UnescapeString(page)
			if keys := shownKeys(t, page); len(keys) != 0 {
				t.Fatalf("%s, %s: a key nothing can be encrypted to is offered as a contact key", variant, name)
			}
			mustContain(t, page, "No usable key.", noUsableKeyNote)
			mustNotContain(t, page, formatFingerprint(fp))
		}
		// The dispute staff entry itself carries the note, not only the vendor's key on the same page.
		staffPage := html.UnescapeString(e.body("GET", "/order?id="+disputed, buyer, nil, 200))
		mustContain(t, staffPage, "Contact dispute staff", "hunt_mod_"+suffix+"’s saved PGP key "+noUsableKeyNote)
	}
	// Control: a usable key is shown with its fingerprint.
	vendorID, _ := e.user("hunt_vendor_ok", "vendor")
	_, usable := testPGPKey(t, "hunt_vendor_ok")
	e.setKey(vendorID, usable)
	_, fp, _ := parsePublicKey(usable)
	page := e.body("GET", "/vendor?id="+vendorID, "", nil, 200)
	mustContain(t, page, formatFingerprint(fp))
	mustNotContain(t, page, "No usable key.")
}

// A physical escrow checkout requires a vendor key able to receive the buyer's shipping details.
func TestPhysicalEscrowRefusedWithoutUsableVendorKey(t *testing.T) {
	x := newEscrowEnv(t)
	unusable := testUnusableKeys(t, "vendor")
	for variant, armored := range map[string]string{"no key": "", "expired key": unusable["expired key"], "signing-only key": unusable["signing-only key"]} {
		x.setKey(x.vendor, armored)
		product := x.physicalListing(x.vendor, 3)
		draft := x.testEnv.order(x.buyer, product, "BTC", stateDraft)
		page := html.UnescapeString(x.page("/checkout?id="+product, x.buyerSess))
		mustContain(t, page, "no usable encryption key")
		before := agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product)
		w := x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {draft}, "refund_address": {regtestAddr("unusable-key-" + variant)}})
		if w.Code != 409 {
			t.Fatalf("%s: checkout status %d: %s", variant, w.Code, w.Body.String())
		}
		if x.state(draft) != stateDraft || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != before || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", draft) != 0 {
			t.Fatalf("%s changed draft or stock", variant)
		}
	}
	_, usable := testPGPKey(t, "vendor")
	x.setKey(x.vendor, usable)
	product := x.physicalListing(x.vendor, 3)
	draft := x.testEnv.order(x.buyer, product, "BTC", stateDraft)
	w := x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {draft}, "refund_address": {regtestAddr("usable-key")}})
	if w.Code != 303 {
		t.Fatalf("usable key checkout: %d %s", w.Code, w.Body.String())
	}
}

// TestPaidOrderVendorKeyStopsBeingUsable (A-83): once a physical order is paid, a vendor key that expires (or is
// removed) replaces the buyer's send-address prompt and warns the vendor to save a usable key.
func TestPaidOrderVendorKeyStopsBeingUsable(t *testing.T) {
	p := newPayEnv(t)
	order, _ := p.newOrder(statePaid)
	b := html.UnescapeString(p.body("GET", "/order?id="+order, p.buyerSess, nil, 200))
	mustContain(t, b, "Send your shipping address", sendAddressLabel)
	mustNotContain(t, b, noUsableKeyPaid)
	for variant, armored := range map[string]string{"expired key": testUnusableKeys(t, p.vendor.Handle)["expired key"], "no key": ""} {
		p.setKey(p.vendor.ID, armored)
		b = html.UnescapeString(p.body("GET", "/order?id="+order, p.buyerSess, nil, 200))
		mustContain(t, b, "Send your shipping address", "No usable key.", noUsableKeyPaid)
		mustNotContain(t, b, sendAddressLabel)
		vendorPage := html.UnescapeString(p.body("GET", "/order?id="+order, p.vendorSess, nil, 200))
		mustContain(t, vendorPage, "No usable key.", "Buyers cannot encrypt their shipping address to your saved PGP key.", "/account")
		mustNotContain(t, vendorPage, "The buyer sends their shipping address to you as an encrypted message.")
		// The order stays paid; nothing else changes.
		if p.count("SELECT count(*) FROM orders WHERE id=$1 AND state=$2", order, statePaid) != 1 {
			t.Fatalf("%s: the order left Paid", variant)
		}
	}
	_, usableArmor := testPGPKey(t, p.vendor.Handle)
	p.setKey(p.vendor.ID, usableArmor)
	vendorPage := html.UnescapeString(p.body("GET", "/order?id="+order, p.vendorSess, nil, 200))
	mustContain(t, vendorPage, "The buyer sends their shipping address to you as an encrypted message.")
	mustNotContain(t, vendorPage, "Buyers cannot encrypt their shipping address to your saved PGP key.")
	// A digital paid order never shows the shipping prompt.
	digital := p.testEnv.order(p.buyer.ID, p.product(p.vendor.ID, "digital"), "BTC", statePaid)
	b = html.UnescapeString(p.body("GET", "/order?id="+digital, p.buyerSess, nil, 200))
	mustNotContain(t, b, noUsableKeyPaid, sendAddressLabel)
}
