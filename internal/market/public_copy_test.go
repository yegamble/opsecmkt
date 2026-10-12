package market

import (
	"html"
	"strings"
	"testing"
)

// A-512 (iteration 60): public copy follows the evidence and the mechanism. The transparency, escrow key and order
// pages name the configured network (stagenet, signet and testnet4 as public test networks; regtest and fakechain as
// local) and never claim that nothing has run on a public network: the project's recorded public-network runs are in
// docs/verification.md and this market itself has not been checked. With neither escrow on, the pages say once that
// payments are off and give no how-to-pay instructions. DB integration with simulated nodes: copy checks, not chain
// evidence.

const (
	publicRecordNote = "The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."
	paymentsOffText  = "This market is not accepting payments right now"
)

// stalePublicNetworkClaims are sentences that were unconditional (or per-instance) claims about public networks.
var stalePublicNetworkClaims = []string{
	"not verified on any public network",
	"has not been verified on any public network",
	"nothing has run on a public network",
	"not yet on the public stagenet",
	"not yet on public testnet4 ",
	"has been tested only on local test chains",
	"does not run on any public network",
	"Monero escrow on any public network",
	"Bitcoin escrow on any public network",
}

func rejectStaleClaims(t *testing.T, name, text string) {
	t.Helper()
	for _, bad := range stalePublicNetworkClaims {
		if strings.Contains(text, bad) {
			t.Errorf("%s still says %q", name, bad)
		}
	}
}

func TestCanaryMoneroCopyFollowsConfiguredNetwork(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(f *fakeXMREscrow)
		want    []string
		notWant []string
	}{
		{
			"stagenet", onStagenet,
			[]string{
				"(Monero stagenet, a public test network; custodial:",
				"The coins and transactions exist only on that test network.",
				"It runs on Monero stagenet, a public test network with no real funds. " + publicRecordNote,
			},
			[]string{"fakechain", "local test chain"},
		},
		{
			"fakechain", nil,
			[]string{
				"(a local Monero fakechain; custodial:",
				"The coins and transactions exist only on that test network.",
				"It runs on a local Monero fakechain, a test chain with no real funds. " + publicRecordNote,
			},
			[]string{"stagenet", "a public test network"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnvWith(t, c.edit)
			canary := html.UnescapeString(x.page("/canary", ""))
			record := pageLine(canary, "<li><strong>Monero escrow.</strong>")
			list := pageLine(canary, "<li>Monero escrow (")
			if record == "" || list == "" {
				t.Fatalf("canary lacks the Monero records item or its escrow line:\n%s", canary)
			}
			for _, w := range c.want {
				if !strings.Contains(record+"\n"+list, w) {
					t.Errorf("canary lacks %q:\n%s\n%s", w, record, list)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(record+"\n"+list, w) {
					t.Errorf("canary names %q on %s:\n%s\n%s", w, c.name, record, list)
				}
			}
			rejectStaleClaims(t, "canary on "+c.name, canary)
		})
	}
}

func TestCanaryBitcoinCopyFollowsConfiguredNetwork(t *testing.T) {
	x := newEscrowEnv(t)
	canary := html.UnescapeString(x.page("/canary", ""))
	li := pageLine(canary, "<li>Bitcoin escrow (")
	if want := "It runs on regtest, a local test chain with no real funds. " + publicRecordNote; !strings.Contains(li, want) {
		t.Errorf("canary on regtest lacks %q:\n%s", want, li)
	}
	rejectStaleClaims(t, "canary on regtest", canary)
	s := signetEscrowEnv(t)
	li = pageLine(html.UnescapeString(s.page("/canary", "")), "<li>Bitcoin escrow (")
	if want := "It runs on signet, a public test network with no real funds. " + publicRecordNote; !strings.Contains(li, want) {
		t.Errorf("canary on signet lacks %q:\n%s", want, li)
	}
}

func TestEscrowKeyPageNamesConfiguredNetwork(t *testing.T) {
	for _, c := range []struct {
		name string
		env  func(t *testing.T, env ...string) *escrowEnv
		want string
	}{
		{"signet", signetEscrowEnv, "Bitcoin escrow on this market runs on signet, a public test network, with no real funds."},
		{"regtest", newEscrowEnv, "Bitcoin escrow on this market runs on regtest, a local test chain, with no real funds."},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := c.env(t)
			page := html.UnescapeString(x.page("/escrow-key", x.buyerSess))
			if !strings.Contains(page, "Bitcoin escrow · "+c.name+" only") {
				t.Fatalf("escrow key page lacks its eyebrow:\n%s", page)
			}
			if !strings.Contains(page, c.want) {
				t.Errorf("escrow key page on %s lacks %q:\n%s", c.name, c.want, pageLine(page, "Compare the origin"))
			}
			rejectStaleClaims(t, "escrow key page on "+c.name, page)
		})
	}
}

func TestOrderPanelNamesConfiguredBitcoinNetwork(t *testing.T) {
	for _, c := range []struct {
		name string
		env  func(t *testing.T, env ...string) *escrowEnv
		want string
	}{
		{"signet", signetEscrowEnv, "Bitcoin escrow on this market runs on signet, a public test network; its coins have no value. " + publicRecordNote},
		{"regtest", newEscrowEnv, "Bitcoin escrow on this market runs on regtest, a local test chain; its coins have no value. " + publicRecordNote},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := c.env(t)
			order, _ := x.built(x.netAddr("copy-refund"), x.netAddr("copy-claim"), 100000)
			page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
			if !strings.Contains(page, c.want) {
				t.Errorf("order panel on %s lacks %q:\n%s", c.name, c.want, pageLine(page, "Bitcoin escrow on this market runs on"))
			}
			rejectStaleClaims(t, "order page on "+c.name, page)
			// The checkout offer on a draft: "Pay into", not "Or pay into" (A-512; the heading stands alone).
			draft := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
			if !strings.Contains(draft, "Pay into a Bitcoin escrow ("+c.name+" only)") || strings.Contains(draft, "Or pay into a Bitcoin escrow") {
				t.Errorf("checkout offer on %s:\n%s", c.name, pageLine(draft, "pay into a Bitcoin escrow"))
			}
		})
	}
}

// With neither Bitcoin nor Monero escrow on, the catalog, product, checkout and the order's draft panel say once that
// the market is not accepting payments and give no instruction about paying or completing an escrow checkout.
func TestEscrowOffPagesSayPaymentsAreOffOnce(t *testing.T) {
	v := newCopyEnv(t, false, false)
	buyerID, buyerSess := v.user("copybuyer_"+randomToken()[:6], "buyer")
	vendorID, _ := v.user("copyvendor_"+randomToken()[:6], "vendor")
	product := v.testEnv.product(vendorID, "digital")
	order := v.order(buyerID, product, "BTC", stateDraft)
	howTo := []string{
		"can be paid through per-order escrow", "complete it under Next steps", "Open the order and complete",
		"escrow checkout is available", "complete it; send nothing until", "Checkout availability is shown",
		"waits until the administrator enables it",
	}
	for _, p := range []struct{ name, path string }{
		{"catalog", "/"}, {"product", "/product?id=" + product}, {"checkout", "/checkout?id=" + product}, {"order draft", "/order?id=" + order},
	} {
		page := html.UnescapeString(v.page(p.path, buyerSess))
		if n := strings.Count(page, paymentsOffText); n != 1 {
			t.Errorf("%s says %q %d times, want once", p.name, paymentsOffText, n)
		}
		for _, bad := range howTo {
			if strings.Contains(page, bad) {
				t.Errorf("%s with escrow off still says %q", p.name, bad)
			}
		}
	}
	// With Bitcoin escrow on the sentence is gone and the instructions are back.
	on := newCopyEnv(t, true, false)
	onBuyerID, onSess := on.user("copybuyer2_"+randomToken()[:6], "buyer")
	onOrder := on.order(onBuyerID, on.product, "BTC", stateDraft)
	for _, path := range []string{"/", "/product?id=" + on.product, "/checkout?id=" + on.product, "/order?id=" + onOrder} {
		if page := html.UnescapeString(on.page(path, onSess)); strings.Contains(page, paymentsOffText) {
			t.Errorf("%s with escrow on says payments are off", path)
		}
	}
	if page := html.UnescapeString(on.page("/order?id="+onOrder, onSess)); !strings.Contains(page, "complete it under Next steps") {
		t.Errorf("draft panel with escrow on lacks the next step")
	}
}

func TestCatalogEyebrowIsNotAProductClaim(t *testing.T) {
	v := newCopyEnv(t, false, false)
	page := html.UnescapeString(v.page("/", v.buyerSess))
	for _, bad := range []string{"Purpose-built for privacy", "more private digital life"} {
		if strings.Contains(page, bad) {
			t.Errorf("catalog still says %q", bad)
		}
	}
	if !strings.Contains(page, "Test-network alpha · no real funds") {
		t.Errorf("catalog lacks the test-network alpha eyebrow")
	}
}

func TestAccountSaysSellingNeedsTheVendorRole(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("sellbuyer_"+randomToken()[:6], "buyer")
	_, vendor := e.user("sellvendor_"+randomToken()[:6], "vendor")
	const sentence = "Selling needs an administrator to give your account the vendor role. Ask the operator of this market."
	w := e.do("GET", "/account", buyer, nil)
	e.check(w, 200)
	if body := html.UnescapeString(w.Body.String()); strings.Count(body, "Selling needs an administrator") != 1 || !strings.Contains(body, sentence) {
		t.Errorf("buyer's account lacks %q:\n%s", sentence, body)
	}
	w = e.do("GET", "/account", vendor, nil)
	e.check(w, 200)
	if strings.Contains(w.Body.String(), "Selling needs an administrator") {
		t.Errorf("a vendor's account explains how to get the vendor role")
	}
}
