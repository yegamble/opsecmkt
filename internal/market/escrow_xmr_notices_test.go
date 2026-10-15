package market

import (
	"regexp"
	"strings"
	"testing"
)

// Site-wide notices, vendor tiles and transparency copy describe the only supported currency: Monero.
// These database tests use simulated wallet RPC and verify copy, not chain behavior. Custody and the
// configured fakechain must be explicit; an enabled escrow must never be described as paused or noncustodial.

type copyEnv struct {
	*testEnv
	buyerSess, vendorSess, adminSess, product string
}

// newCopyEnv starts a market with Monero escrow enabled or disabled.
func newCopyEnv(t *testing.T, xmr bool) *copyEnv {
	t.Helper()
	for _, k := range []string{"ESCROW_BTC", "ESCROW_XMR", "BITCOIN_RPC_URL", "BITCOIN_CHAIN", "MONERO_WALLET_RPC_URL", "MONERO_NETWORK"} {
		t.Setenv(k, "")
	}
	if xmr {
		_, pub := testSeedKey(t, nil)
		newFakeXMREscrow(t).setEnv(t, writeSeedKey(t, pub))
	}
	e := newTestApp(t)
	if e.A.escrowConfigured || e.A.escrow.Load() != nil || (e.A.escrowXMR != nil) != xmr || (xmr && !e.A.xmrEscrowReady()) {
		t.Fatalf("escrow state: btc %v/%v, xmr %v/%v", e.A.escrowConfigured, e.A.escrow.Load() != nil, e.A.escrowXMR != nil, e.A.xmrEscrowReady())
	}

	v := &copyEnv{testEnv: e}
	vendor, vendorSess := e.user("cvendor_"+randomToken()[:6], "vendor")
	_, v.buyerSess = e.user("cbuyer_"+randomToken()[:6], "buyer")
	_, v.adminSess = e.user("cadmin_"+randomToken()[:6], "admin")
	v.vendorSess, v.product = vendorSess, e.product(vendor, "digital")
	return v
}

func (v *copyEnv) page(path, sess string) string {
	v.t.Helper()
	w := v.do("GET", path, sess, nil)
	v.check(w, 200)
	return w.Body.String()
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// multisigAboutMonero returns the sentence of body (tags stripped) that names Monero and multisig together, if any.
func multisigAboutMonero(body string) string {
	text := htmlTag.ReplaceAllString(body, " ")
	for _, s := range regexp.MustCompile(`[.!?;]\s|\n`).Split(text, -1) {
		l := strings.ToLower(s)
		if strings.Contains(l, "monero") && strings.Contains(l, "multisig") {
			return s
		}
	}
	return ""
}

func TestMoneroEscrowSiteCopyEnabledAndPaused(t *testing.T) {
	const (
		disabled = `<span class="accent">New payments paused</span>`
		xmrBdg   = `<span class="badge testnet">Monero escrow (local fakechain)</span>`
	)
	for _, c := range []struct {
		name  string
		xmr   bool
		badge string
	}{
		{"paused", false, disabled},
		{"enabled", true, xmrBdg},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newCopyEnv(t, c.xmr)
			product, checkout := "/product?id="+v.product, "/checkout?id="+v.product
			pages := map[string]string{
				"/":                 v.page("/", v.buyerSess),
				product:             v.page(product, v.buyerSess),
				checkout:            v.page(checkout, v.buyerSess),
				"/vendor-dashboard": v.page("/vendor-dashboard", v.vendorSess),
				"/canary":           v.page("/canary", v.buyerSess),
				"/admin":            v.page("/admin", v.adminSess),
			}
			for path, body := range pages {
				if regexp.MustCompile(`(?i)\bbitcoin\b|\bBTC\b`).MatchString(htmlTag.ReplaceAllString(body, " ")) {
					t.Errorf("%s advertises a retired payment currency", path)
				}
				// The footer badge is on every page.
				if !strings.Contains(body, c.badge) {
					t.Errorf("%s lacks the badge %q", path, c.badge)
				}
				if c.xmr {
					if strings.Contains(body, "Payments disabled") {
						t.Errorf("%s says Payments disabled with Monero escrow on", path)
					}
					if s := multisigAboutMonero(body); s != "" {
						t.Errorf("%s calls Monero escrow multisig: %q", path, s)
					}
				}
			}
			if !c.xmr {
				for path, body := range pages {
					if path != "/admin" && path != "/canary" && strings.Contains(body, "Monero escrow") || strings.Contains(body, xmrCustody) {
						t.Errorf("%s advertises Monero escrow with ESCROW_XMR off", path)
					}
				}
				return
			}
			// The catalog, vendor tile, and canary carry the custody disclosure and verified network.
			for _, path := range []string{"/", "/vendor-dashboard", "/canary"} {
				body := pages[path]
				if !strings.Contains(body, xmrCustody) {
					t.Errorf("%s lacks the custody sentence", path)
				}
				if !strings.Contains(strings.ToLower(body), "fakechain") {
					t.Errorf("%s does not say fakechain", path)
				}
			}
			const notice = "A Monero escrow order pays into its own account on a local Monero fakechain. The coins are " + xmrCustody + "."
			if !strings.Contains(pages["/"], notice) {
				t.Errorf("catalog lacks %q", notice)
			}
			// The vendor desk has only the Monero tile and keeps its custody warning.
			dash := pages["/vendor-dashboard"]
			if !strings.Contains(dash, "<div class=\"eyebrow\">Monero escrow</div><strong>FAKECHAIN</strong>") || !strings.Contains(dash, "One account per order; "+xmrCustody) {
				t.Errorf("vendor desk lacks the Monero escrow tile")
			}
			if strings.Contains(dash, "No funds are held") || strings.Contains(dash, "no multisig escrow") {
				t.Errorf("vendor desk made a false custody claim:\n%s", dash)
			}
			// The canary's lines: the Monero line, and what stays unavailable.
			canary := pages["/canary"]
			for _, w := range []string{"<li>Monero escrow (local fakechain only): an order in Monero can be paid into an escrow account of its own, and its coins are " + xmrCustody + ".",
				publicRecordNote, "Noncustodial Monero escrow,"} {
				if !strings.Contains(canary, w) {
					t.Errorf("canary lacks %q", w)
				}
			}
			if strings.Contains(canary, "this is custodial holding, not escrow") {
				t.Error("canary denies escrow")
			}

		})
	}
}

// The tile counts the viewer's Monero escrow orders: a vendor with a funded one is never told no funds are held, with
// Monero escrow unavailable and switched off afterwards too.
func TestMoneroEscrowTileCountsOrders(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.fundedXMR(x.product)
	const held = "1 order(s) may hold coins; " + xmrCustody
	dash := x.page("/vendor-dashboard", x.vendorSess)
	if !strings.Contains(dash, "<div class=\"eyebrow\">Monero escrow</div><strong>FAKECHAIN</strong>") || !strings.Contains(dash, held) {
		t.Fatalf("vendor desk with a funded Monero escrow order:\n%s", dash)
	}
	x.A.escrowXMR.ready.Store(false)
	if dash = x.page("/vendor-dashboard", x.vendorSess); !strings.Contains(dash, "<strong>UNAVAILABLE</strong>") || !strings.Contains(dash, held) {
		t.Fatalf("vendor desk with Monero escrow unavailable:\n%s", dash)
	}
	t.Setenv("ESCROW_XMR", "off")
	x.restart()
	// A-472: switched off, no daemon is verified, so the tile names no network (it said "fakechain" whatever the order's).
	const heldOff = "1 order(s) may hold coins; " + xmrCustody
	dash = x.page("/vendor-dashboard", x.vendorSess)
	if !strings.Contains(dash, "<div class=\"eyebrow\">Monero escrow</div><strong>OFF</strong>") || !strings.Contains(dash, heldOff) || strings.Contains(dash, "No funds are held") {
		t.Fatalf("vendor desk with Monero escrow off beside a funded order:\n%s", dash)
	}
	if body := x.page("/", x.buyerSess); !strings.Contains(body, `<span class="accent">New payments paused</span>`) || strings.Contains(body, "Monero escrow") {
		t.Fatalf("catalog with Monero escrow off:\n%s", body)
	}
}
