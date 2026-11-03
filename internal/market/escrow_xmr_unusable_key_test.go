package market

import (
	"net/url"
	"strings"
	"testing"
)

// A-792 (c): an unusable PGP key on either party refuses a new Monero escrow order at the draft step (POST /orders) and
// at checkout (POST /orders/escrow-xmr), for a digital and a physical listing, with the refusal's exact status and text,
// and writes nothing: no order or change to the draft, no stock reserved, no escrow, no claimed escrow slot or reserved
// refund address, no notification, receipt, order event or audit row, and no wallet RPC call. A buyer whose saved key cannot receive encrypted messages has no PGP
// access in this browser even with a session proof for that very key (requirePGPAccess: 403); a vendor with no usable
// key takes no new order (lockNewIntake's seller rule: 409). "Missing" is a legacy account with no saved key; the
// expired and signing-only keys parse and could be saved (testUnusableKeys). Each case passes once the key is usable,
// so the refusal is about the key alone. DB integration test against the simulated Monero wallet RPC.
func TestXMREscrowCheckoutRefusesUnusableKeys(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	products := map[string]string{"digital": x.digital(x.vendor), "physical": x.physical(x.vendor)}
	// Each passing checkout below reserves a unit; physical() saved the vendor a usable key.
	agExec(x.testEnv, "UPDATE products SET stock=100 WHERE vendor_id=$1", x.vendor)
	vendorKey := agStr(x.testEnv, "SELECT pgp FROM users WHERE id=$1", x.vendor)
	unusable := testUnusableKeys(t, "a792_unusable")
	variants := map[string]string{"missing key": "", "expired key": unusable["expired key"], "signing-only key": unusable["signing-only key"]}
	refusals := map[string]struct {
		code int
		text string
	}{
		roleBuyer:  {403, pgpAccessRequiredMessage},
		roleVendor: {409, "Unavailable. " + sellerPGPRequired + " Nothing was changed."},
	}
	// Everything a draft or checkout could write, market-wide.
	written := func() string {
		return agStr(x.testEnv, `SELECT concat_ws(' | ',
			(SELECT count(*)::text||':'||COALESCE(string_agg(id||'='||state||'@'||amount,',' ORDER BY id),'') FROM orders),
			(SELECT string_agg(stock::text,',' ORDER BY id) FROM products),
			(SELECT count(*) FROM escrows), (SELECT count(*) FROM xmr_escrow_slots WHERE state<>'free'),
			(SELECT count(*) FROM escrow_external_addresses), (SELECT count(*) FROM notifications),
			(SELECT count(*) FROM form_submissions), (SELECT count(*) FROM order_events), (SELECT count(*) FROM audit_events))`)
	}
	for _, party := range []string{roleBuyer, roleVendor} {
		for variant, armored := range variants {
			for kind, product := range products {
				for _, step := range []string{"draft", "checkout"} {
					name := party + " " + variant + ", " + kind + " " + step
					buyer, sess := x.user("a792_"+randomToken()[:8], "buyer")
					order := ""
					if step == "checkout" {
						order = x.order(buyer, product, "XMR", stateDraft)
					}
					post := func() (int, string) {
						if step == "draft" {
							return x.xmrPost("/orders", sess, url.Values{"product_id": {product}, "currency": {"XMR"}})
						}
						return x.xmrPost("/orders/escrow-xmr", sess, url.Values{"order_id": {order}, "refund_address": {testXMRAddr("a792 refund "+buyer, true)}})
					}
					owner := buyer
					if party == roleVendor {
						owner = x.vendor
					}
					x.setKey(owner, armored)
					if party == roleBuyer && armored != "" {
						x.pgpSessionKey(sess) // this browser proved the saved key: only its usability refuses
					}
					before, calls := written(), x.fake.count()
					want := refusals[party]
					if code, body := post(); code != want.code || !strings.Contains(body, want.text) {
						t.Fatalf("%s: %d %q, want %d %q", name, code, body, want.code, want.text)
					}
					if after := written(); after != before || x.fake.count() != calls {
						t.Fatalf("%s wrote something or called the wallet RPC (%d calls):\n before %s\n after  %s", name, x.fake.count()-calls, before, after)
					}
					// The same request passes once the key is usable again.
					if party == roleBuyer {
						x.pgpSession(sess, accountPGPFixture.public)
					} else {
						x.setKey(x.vendor, vendorKey)
					}
					x.mustPoll() // the watcher replaces the escrow slots earlier checkouts claimed
					if code, body := post(); code != 303 {
						t.Fatalf("%s, with a usable key: %d %s", name, code, body)
					}
				}
			}
		}
	}
}
