package market

import (
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
// is on (regtest only), never that payments and escrow are not active, that multisig escrow is not configured or
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
// escrow tile follows it; the catalog notice adds the escrow sentence after the pooled one.
func TestEscrowNoticesBesidePooledProvider(t *testing.T) {
	x := newEscrowEnv(t)
	x.A.payMu.Lock()
	x.A.payments["BTC"] = newFakeProvider("BTC", 3)
	x.A.payMu.Unlock()
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
