package market

import (
	"net/url"
	"strings"
	"testing"
)

// QA31-2 (ES-10): the site-wide payment notices (partials/payment-status.html) never deny escrow or custody while
// escrow is on or escrow orders hold coins. DB integration with the simulated Core (not chain evidence).

// The pooled-only wording of each surface, exactly as it is with neither a pooled provider nor escrow.
const (
	pooledOffFooter    = `<span class="accent">Payments disabled</span>`
	pooledOffCatalog   = "<strong>Order drafts only.</strong> Payments and escrow are not active.</p>"
	pooledOffProduct   = "<p>Payments and escrow are not active. No protection or verification claim is implied by a listing.</p>"
	pooledOffCheckout  = "<strong>UNFUNDED DRAFT ONLY.</strong> Bitcoin, Monero, multisig escrow, shipping addresses, and settlement are not configured. Do not send funds.</p>"
	pooledOffDashboard = "<div class=\"eyebrow\">Escrow</div>\n<strong>OFF</strong>\n<span>No funds are held</span>"
)

// With ESCROW_BTC=on and no pooled provider, the footer, catalog, product, checkout and vendor dashboard say escrow
// is on (test networks only), never that payments and escrow are not active, that multisig escrow is not configured or
// that no funds are held. A vendor with a funded escrow order is never told no funds are held, with ESCROW_BTC
// switched off afterwards too.
func TestEscrowSiteNoticesWithoutPooledProvider(t *testing.T) {
	x := newEscrowEnv(t)
	if len(x.A.providers()) != 0 {
		t.Fatal("a pooled provider is configured")
	}
	denials := []string{pooledOffFooter, pooledOffCatalog, pooledOffProduct, pooledOffCheckout, pooledOffDashboard,
		"Payments and escrow are not active", "multisig escrow, shipping addresses, and settlement are not configured", "No funds are held", "no multisig escrow"}
	check := func(path, sess string, want ...string) {
		t.Helper()
		body := x.page(path, sess)
		for _, bad := range denials {
			if strings.Contains(body, bad) {
				t.Fatalf("%s says %q with escrow on:\n%s", path, bad, body)
			}
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Fatalf("%s lacks %q:\n%s", path, w, body)
			}
		}
	}
	const footer = `<span class="badge testnet">Bitcoin escrow only (regtest) — no real funds</span>`
	check("/", x.buyerSess, footer, "<strong>Bitcoin escrow only (regtest) — no real funds.</strong> The market's pooled wallet is not configured.")
	check("/product?id="+x.product, x.buyerSess, footer, "The market's pooled wallet is not active; a digital listing with automatic delivery can be paid into a 2-of-3 Bitcoin escrow on regtest.")
	check("/checkout?id="+x.product, x.buyerSess, footer, "<strong>UNFUNDED DRAFT.</strong> Creating a draft collects nothing.", "send nothing until the order page shows an escrow address")
	check("/vendor-dashboard", x.vendorSess, "<div class=\"eyebrow\">Bitcoin escrow</div>\n<strong>REGTEST</strong>", "none of your orders uses it yet")

	// A funded escrow order: counted on the vendor dashboard, with escrow on, unavailable and off. One cancelled with
	// nothing recorded is counted only while escrow is not enabled (its deposits are then not read).
	empty := x.draft()
	x.mustCheckout(empty, "bcrt1qbuyerrefund00000000000000000000009")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", empty)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.core.deposit(addr, "held", 0, 100000, 3)
	x.poll()
	if x.state(x.order) != stateCompleted || x.state(empty) != stateCancelled {
		t.Fatalf("funded order %s, empty order %s", x.state(x.order), x.state(empty))
	}
	const held = "1 escrow order(s) may hold coins at their own 2-of-3 escrow addresses"
	const unread = "2 escrow order(s) may hold coins at their own 2-of-3 escrow addresses"
	check("/vendor-dashboard", x.vendorSess, "<strong>REGTEST</strong>", held)
	srvDown(x, true)
	x.restart()
	check("/vendor-dashboard", x.vendorSess, "<strong>UNAVAILABLE</strong>", unread)
	srvDown(x, false)
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	body := x.page("/vendor-dashboard", x.vendorSess)
	for _, w := range []string{"<div class=\"eyebrow\">Bitcoin escrow</div>\n<strong>OFF</strong>", unread} {
		if !strings.Contains(body, w) {
			t.Fatalf("vendor dashboard with escrow off and a funded escrow order lacks %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "No funds are held") {
		t.Fatalf("vendor dashboard with escrow off says no funds are held beside a funded escrow order:\n%s", body)
	}
	// With escrow off, the rest of the site is back to the pooled-only wording (the vendor of that order excepted).
	for path, want := range map[string]string{"/": pooledOffCatalog, "/product?id=" + x.product: pooledOffProduct, "/checkout?id=" + x.product: pooledOffCheckout} {
		if body := x.page(path, x.buyerSess); !strings.Contains(body, want) || !strings.Contains(body, pooledOffFooter) {
			t.Fatalf("%s with escrow off lacks the pooled-only wording:\n%s", path, body)
		}
	}
}

// A pooled provider and ESCROW_BTC=on: the vendor desk's pooled tile no longer says "no multisig escrow" and an
// escrow tile follows it; the catalog notice adds the escrow sentence after the pooled one. The tile's count takes
// the viewer's escrow orders as buyer too (a vendor who bought from another vendor), and with ESCROW_BTC off the pooled
// tile still reads as the pooled provider's while the escrow tile says OFF with that count (A-335: A326-6, A326-11).
func TestEscrowNoticesBesidePooledProvider(t *testing.T) {
	x := newEscrowEnv(t)
	pooled := func() {
		x.A.payMu.Lock()
		x.A.payments["BTC"] = newFakeProvider("BTC", 3)
		x.A.payMu.Unlock()
	}
	pooled()
	body := x.page("/vendor-dashboard", x.vendorSess)
	if strings.Contains(body, "no multisig escrow") || !strings.Contains(body, "<strong>POOLED</strong>\n<span>Custodial test wallet</span>") ||
		!strings.Contains(body, "<div class=\"eyebrow\">Bitcoin escrow</div>\n<strong>REGTEST</strong>") {
		t.Fatalf("vendor dashboard beside a pooled provider:\n%s", body)
	}
	body = x.page("/", x.buyerSess)
	if !strings.Contains(body, "This is custodial holding, not multisig escrow. A digital listing with automatic delivery can instead be paid into its own 2-of-3 Bitcoin escrow on regtest") ||
		!strings.Contains(body, "TESTNET payments (BTC") {
		t.Fatalf("catalog beside a pooled provider:\n%s", body)
	}
	// The vendor's only escrow order is one they bought, as buyer, from another vendor (none is on their own listings).
	other, _ := x.user("eother_"+randomToken()[:6], "vendor")
	x.activeKey(other, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 2))
	bought := x.testEnv.order(x.vendor, x.autoListing(other), "BTC", stateDraft)
	if w := x.do("POST", "/orders/escrow", x.vendorSess, url.Values{"order_id": {bought}, "refund_address": {"bcrt1qbuyerrefund00000000000000000000007"}}); w.Code != 303 || x.cookie(w, "notice") != "escrow-created" {
		t.Fatalf("the vendor's escrow checkout as buyer: %d %s", w.Code, w.Body.String())
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrows es JOIN orders o ON o.id=es.order_id JOIN products p ON p.id=o.product_id WHERE p.vendor_id=$1", x.vendor); n != 0 {
		t.Fatalf("the vendor has %d escrow orders on their own listings", n)
	}
	const pooledTile = "<strong>POOLED</strong>\n<span>Custodial test wallet</span>"
	const count = "1 escrow order(s) may hold coins at their own 2-of-3 escrow addresses"
	body = x.page("/vendor-dashboard", x.vendorSess)
	if !strings.Contains(body, "<strong>REGTEST</strong>") || !strings.Contains(body, count) || !strings.Contains(body, pooledTile) {
		t.Fatalf("vendor dashboard: a buyer-side escrow order is not counted:\n%s", body)
	}
	t.Setenv("ESCROW_BTC", "off")
	x.restart()
	pooled()
	body = x.page("/vendor-dashboard", x.vendorSess)
	if !strings.Contains(body, pooledTile) || strings.Contains(body, "no multisig escrow") || strings.Contains(body, "Pooled payments</div>") ||
		!strings.Contains(body, "<div class=\"eyebrow\">Bitcoin escrow</div>\n<strong>OFF</strong>") || !strings.Contains(body, count) {
		t.Fatalf("vendor dashboard with a pooled provider, escrow off and an escrow order:\n%s", body)
	}
}

// Neither a pooled provider nor escrow: every surface keeps the pooled-only wording, byte for byte.
func TestPooledOffEscrowOffNoticesUnchanged(t *testing.T) {
	t.Setenv("ESCROW_BTC", "")
	e := newTestApp(t)
	if len(e.A.providers()) != 0 || e.A.escrowConfigured {
		t.Fatal("a pooled provider or escrow is configured")
	}
	vendor, vendorSess := e.user("nvendor", "vendor")
	_, buyerSess := e.user("nbuyer", "buyer")
	product := e.product(vendor, "digital")
	for path, want := range map[string][]string{
		"/":                       {pooledOffFooter, pooledOffCatalog},
		"/product?id=" + product:  {pooledOffFooter, pooledOffProduct},
		"/checkout?id=" + product: {pooledOffFooter, pooledOffCheckout},
		"/vendor-dashboard":       {pooledOffFooter, "<div class=\"eyebrow\">Payments</div>\n<strong>OFF</strong>\n<span>Provider not configured</span>", pooledOffDashboard},
	} {
		sess := buyerSess
		if path == "/vendor-dashboard" {
			sess = vendorSess
		}
		w := e.do("GET", path, sess, nil)
		e.check(w, 200)
		body := w.Body.String()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Fatalf("%s lacks %q:\n%s", path, s, body)
			}
		}
		if strings.Contains(body, "Bitcoin escrow") {
			t.Fatalf("%s mentions Bitcoin escrow with escrow off", path)
		}
	}
}

// A-345 (ES-10): since P1-c2 the market co-signs and broadcasts a settlement once a party has signed it, so the
// escrow-only payment banner and the cancel form of a funded escrow order (buyer and vendor, escrow available) say so
// and never that this build does not co-sign or broadcast.
func TestEscrowStatementsSayTheMarketCosignsAndBroadcasts(t *testing.T) {
	x := newEscrowEnv(t)
	check := func(path, sess string, want ...string) {
		t.Helper()
		body := x.page(path, sess)
		for _, bad := range []string{"co-sign or broadcast", "does not co-sign", "not yet co-signed"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s says %q:\n%s", path, bad, body)
			}
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Fatalf("%s lacks %q:\n%s", path, w, body)
			}
		}
	}
	// A-355: both paths, one party's signature the market co-signs and both parties' the market broadcasts unsigned.
	check("/", x.buyerSess, "the market co-signs it and broadcasts it to the regtest network; when both have signed it, the market does not sign it and broadcasts what they signed. Other orders stay unfunded drafts.")
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.fund(x.order, "partial", 3, 40000)
	if x.state(x.order) != stateAwaitingPayment || x.A.escrow.Load() == nil {
		t.Fatalf("order %s", x.state(x.order))
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if sess == x.vendorSess {
			// Beside a pooled provider the catalog carries the same sentence in its own notice.
			x.A.payMu.Lock()
			x.A.payments["BTC"] = newFakeProvider("BTC", 3)
			x.A.payMu.Unlock()
			check("/", x.buyerSess, "the market co-signs it and broadcasts it to the regtest network; when both have signed it, the market does not sign it and broadcasts what they signed.")
		}
		check("/order?id="+x.order, sess, "Returns one unit of stock. The coins in this escrow go back to the buyer's refund address by a settlement: the market builds the refund transaction, the buyer or the vendor signs it on this page, and the market then co-signs it and broadcasts it to the regtest network (when both sign it, the market does not sign it and broadcasts what they signed).",
			"Once the buyer or the vendor has signed one in their own wallet, the market co-signs it and broadcasts it to the regtest network; when both have signed it, the market does not sign it and broadcasts what they signed.")
	}
}
