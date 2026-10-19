package market

import (
	"context"
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const (
	publicRecordNote = "The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."
	paymentsOffText  = "This market is not accepting payments right now"
)

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

// With Monero escrow off, the catalog, product, checkout and the order's draft panel say once that
// the market is not accepting payments and give no instruction about paying or completing an escrow checkout.
func TestEscrowOffPagesSayPaymentsAreOffOnce(t *testing.T) {
	v := newCopyEnv(t, false)
	buyerID, buyerSess := v.user("copybuyer_"+randomToken()[:6], "buyer")
	vendorID, _ := v.user("copyvendor_"+randomToken()[:6], "vendor")
	product := v.testEnv.product(vendorID, "digital")
	order := v.order(buyerID, product, "XMR", stateDraft)
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
	// With Monero escrow on the sentence is gone and the instructions are back.
	on := newCopyEnv(t, true)
	onBuyerID, onSess := on.user("copybuyer2_"+randomToken()[:6], "buyer")
	onOrder := on.order(onBuyerID, on.product, "XMR", stateDraft)
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
	v := newCopyEnv(t, false)
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

// A-525: a mainnet market (MONERO_NETWORK=mainnet) must not tell visitors the alpha has no real funds. Env only: escrow
// stays off, so no node is contacted.
func TestCatalogEyebrowMainnet(t *testing.T) {
	for _, k := range []string{"ESCROW_BTC", "ESCROW_XMR", "BITCOIN_RPC_URL", "MONERO_WALLET_RPC_URL", "MONERO_NETWORK"} {
		t.Setenv(k, "")
	}
	t.Setenv("MONERO_NETWORK", "mainnet")
	e := newTestApp(t)
	e.user("mainnet_buyer", "buyer") // completes first-run setup so "/" renders the catalog
	if !e.A.live {
		t.Fatal("MONERO_NETWORK=mainnet did not make the market live")
	}
	page := html.UnescapeString(e.body("GET", "/", "", nil, 200))
	mustNotContain(t, page, "no real funds", "Test-network")
	mustContain(t, page, `<span class="eyebrow">Mainnet · real funds</span>`)
	mustContain(t, page, `<meta name="description" content="Marketplace with PGP-based accounts and messages and no browser JavaScript. Mainnet payments are real funds.">`)
	// The layout covers every page, including a refusal that never runs the page loaders.
	for _, path := range []string{"/login", "/canary", "/no-such-page"} {
		w := e.do("GET", path, "", nil)
		body := html.UnescapeString(w.Body.String())
		mustNotContain(t, body, "Test-network marketplace")
		mustContain(t, body, "Mainnet payments are real funds.")
	}
}

func TestAccountSaysSellingNeedsTheVendorRole(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("sellbuyer_"+randomToken()[:6], "buyer")
	_, vendor := e.user("sellvendor_"+randomToken()[:6], "vendor")
	const sentence = `Selling needs an approved vendor application: see <a class="text-link" href="#vendor-application">Sell on this market</a>. Ask the operator of this market.`
	w := e.do("GET", "/account", buyer, nil)
	e.check(w, 200)
	if body := html.UnescapeString(w.Body.String()); strings.Count(body, "Selling needs an approved vendor application") != 1 || !strings.Contains(body, sentence) {
		t.Errorf("buyer's account lacks %q:\n%s", sentence, body)
	}
	w = e.do("GET", "/account", vendor, nil)
	e.check(w, 200)
	if strings.Contains(w.Body.String(), "Selling needs an approved vendor application") {
		t.Errorf("a vendor's account explains how to get the vendor role")
	}
}

// Preview is read-only layout evidence. The same templates must expose only XMR without browser scripting.
func TestPreviewMoneroOnlyPublicPages(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	retired := regexp.MustCompile(`(?i)\bbitcoin\b|\bBTC\b|name="price_btc"|/escrow-key|/escrow/setup`)
	for _, path := range []string{"/", "/?currency=BTC", "/product?id=encrypted-drive", "/checkout?id=encrypted-drive", "/listing-edit?id=encrypted-drive", "/vendor-dashboard", "/account", "/admin", "/canary", "/order?id=" + previewDraftID} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if retired.MatchString(body) {
				t.Errorf("retired Bitcoin interface rendered on %s", path)
			}
			if strings.Contains(body, "<script") {
				t.Error("page contains a browser script")
			}
		})
	}
}
