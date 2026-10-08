package market

import (
	"encoding/hex"
	"html"
	"strconv"
	"strings"
	"testing"
)

// A-456b (rulings P4-R2, P4-R17): the party-only setup page of a per-order (shape (b)) escrow, /escrow-setup?id=<order>,
// behind the gate of /escrow-descriptor: only the order's buyer and vendor, each seeing its own setup (its own key's
// origin and path, the other party's and the market's per-order keys served rooted as Vpubs, the escrow address as the
// first receiving address and a Sparrow descriptor of the order wallet); never staff, a stranger or the other party's
// setup; a Bitcoin Core escrow has none. The cosigner keys are Go's derivation checked against the keys the escrow
// stores (each checked with Core at checkout). DB integration with the simulated node and the fake signer.

// perOrderSetupWant is what the setup page shows reader of order: its own origin with the per-order index, the
// derivation path, the cosigner Vpubs (the other party's, the market's) and the Sparrow descriptor.
func (x *escrowEnv) perOrderSetupWant(order, reader string, signer bip32Key) (ownPath, deriv string, cosigners []string, sparrow string) {
	x.t.Helper()
	rows := x.escrowRows(order)
	exprs := map[string]string{"buyer": x.buyerKey, "vendor": x.vendorKey}
	desc := map[string]string{}
	for _, party := range []string{"buyer", "vendor", "market"} {
		steps, _ := parseOriginPath(rows[party].suffix)
		i := strconv.FormatUint(uint64(steps[0]), 10)
		account := signer
		if party != "market" {
			account = accountKey(x.t, exprs[party])
		}
		child, _ := perOrderChild(x.t, account, steps[0])
		fp := child.Fingerprint()
		switch {
		case party == reader:
			k := mustParseKey(x.t, exprs[party])
			ownPath, deriv = "["+k.Fingerprint+k.PathText+"/"+i+"]", "m"+k.PathText+"/"+i
			desc[party] = ownPath + child.Serialize(versionTpub) + "/<0;1>/*"
		default:
			cosigners = append(cosigners, child.Rooted(versionVpub))
			desc[party] = "[" + hex.EncodeToString(fp[:]) + "]" + child.Rooted(versionTpub) + "/<0;1>/*"
		}
	}
	return ownPath, deriv, cosigners, withChecksum("wsh(sortedmulti(2," + desc["buyer"] + "," + desc["vendor"] + "," + desc["market"] + "))")
}

func TestEscrowShapeBSetupPage(t *testing.T) {
	x, f := newPerOrderEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("setup-1"))
	path := "/escrow-setup?id=" + x.order
	pages := map[string]string{}
	for _, c := range []struct{ reader, sess string }{{roleBuyer, x.buyerSess}, {roleVendor, x.vendorSess}} {
		page := html.UnescapeString(x.page(path, c.sess))
		pages[c.reader] = page
		own, deriv, cosigners, sparrow := x.perOrderSetupWant(x.order, c.reader, f.key)
		for _, want := range append([]string{own, deriv, addr, sparrow,
			"Pay only to the escrow address", "--offline", "Delete the order wallet", "cannot check the other party's key"}, cosigners...) {
			if !strings.Contains(page, want) {
				t.Fatalf("%s's setup page lacks %q:\n%s", c.reader, want, page)
			}
		}
		// Never the other party's own setup: its origin with the per-order index.
		otherOwn, _, _, _ := x.perOrderSetupWant(x.order, otherParty(c.reader), f.key)
		if strings.Contains(page, otherOwn) {
			t.Fatalf("%s's setup page shows the %s's own path %s", c.reader, otherParty(c.reader), otherOwn)
		}
	}
	// The order page links each party, and no one else, to it.
	link := `href="/escrow-setup?id=` + x.order + `"`
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if !strings.Contains(x.page("/order?id="+x.order, sess), link) {
			t.Fatal("the order page does not link a party to its setup page")
		}
	}
	_, mod := x.user("setup_mod_"+randomToken()[:6], "moderator")
	_, admin := x.user("setup_adm_"+randomToken()[:6], "admin")
	_, stranger := x.user("setup_str_"+randomToken()[:6], "buyer")
	for name, sess := range map[string]string{"moderator": mod, "administrator": admin, "stranger": stranger} {
		if w := x.do("GET", path, sess, nil); w.Code != 404 {
			t.Fatalf("%s: %d, want 404 (setup material is party-only)", name, w.Code)
		}
	}
	if w := x.do("GET", "/order?id="+x.order, admin, nil); w.Code == 200 && strings.Contains(w.Body.String(), link) {
		t.Fatal("the order page links staff to the setup page")
	}
	if w := x.do("GET", path, "", nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("signed out: %d %q", w.Code, w.Header().Get("Location"))
	}
	// The signer not answering (QA5W5A-1): the market's order wallet key is the one checkout stored, so the page is the
	// same, the market's cosigner key and the Sparrow descriptor included.
	f.stop()
	page := html.UnescapeString(x.page(path, x.buyerSess))
	_, _, cosigners, sparrow := x.perOrderSetupWant(x.order, roleBuyer, f.key)
	if !strings.Contains(page, cosigners[0]) || !strings.Contains(page, cosigners[1]) || !strings.Contains(page, sparrow) {
		t.Fatalf("setup page without the signer:\n%s", page)
	}
	// A market row with no stored order wallet key (none outside tests): the page says the market's key cannot be read
	// from the market's records, and shows no cosigner key.
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=NULL WHERE party='market' AND escrow_id=(SELECT id FROM escrows WHERE order_id=$1)", x.order)
	page = html.UnescapeString(x.page(path, x.buyerSess))
	if !strings.Contains(page, "The market's cosigner key of this escrow cannot be read from the market's records") || strings.Contains(page, cosigners[0]) || strings.Contains(page, sparrow) {
		t.Fatalf("setup page with no stored market key:\n%s", page)
	}
	f.start()
	// A Bitcoin Core escrow has no setup page.
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	core := x.draft()
	x.mustCheckout(core, regtestAddr("setup-core"))
	if w := x.do("GET", "/escrow-setup?id="+core, x.buyerSess, nil); w.Code != 404 {
		t.Fatalf("a Bitcoin Core escrow's setup page: %d", w.Code)
	}
	if strings.Contains(x.page("/order?id="+core, x.buyerSess), "/escrow-setup") {
		t.Fatal("a Bitcoin Core escrow's order page links to a setup page")
	}
}

// A party that registered another escrow key since checkout (QA5W4A-5): the order wallet keys stored at checkout still
// give both setup pages in full, the party's own path and Sparrow descriptor and the other party's cosigner key, never
// anything of the new key.
func TestEscrowShapeBSetupKeyChanged(t *testing.T) {
	x, f := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("setup-changed"))
	vendorOwn, _, _, vendorSparrow := x.perOrderSetupWant(x.order, roleVendor, f.key)
	_, _, buyerCosigners, buyerSparrow := x.perOrderSetupWant(x.order, roleBuyer, f.key)
	x.activeKey(x.vendor, altEscrowKey(t, testBIP48Key, "c4a76e0d", 9))
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id=$1", x.vendor)
	page := html.UnescapeString(x.page("/escrow-setup?id="+x.order, x.vendorSess))
	if !strings.Contains(page, vendorOwn) || !strings.Contains(page, vendorSparrow) || strings.Contains(page, "c4a76e0d") || strings.Contains(page, "no longer the one this escrow was built with") {
		t.Fatalf("vendor's setup page after a key change:\n%s", page)
	}
	page = html.UnescapeString(x.page("/escrow-setup?id="+x.order, x.buyerSess))
	if !strings.Contains(page, buyerCosigners[0]) || !strings.Contains(page, buyerSparrow) || strings.Contains(page, "no longer the one this escrow was built with") {
		t.Fatalf("buyer's setup page after the vendor's key change:\n%s", page)
	}
}
