package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-472: the public canary and the site-wide payment notices (partials/payment-status.html) name the network Monero
// escrow was verified on, by the rule /admin uses (admin-payments.html): "network not verified yet" until a daemon was
// verified or a network pin was read, "Monero stagenet" on stagenet, "a local Monero fakechain" on a fakechain. The
// canary says "not verified on any public network" only where it is true: stagenet is a public test network, and
// nothing is recorded on it beyond local test chains and an offline fork (docs/escrow.md, A-449; the owner's public run,
// A-447, has not happened). The same pages name Bitcoin escrow's chain (regtest, signet, testnet4). DB integration with
// the simulated Core and wallet-rpc: copy checks, not chain evidence.

// pageLine is the first line of body (unescaped) that holds marker: the canary's list items and the payment notices are
// one line each. "" when none does.
func pageLine(body, marker string) string {
	for _, l := range strings.Split(html.UnescapeString(body), "\n") {
		if strings.Contains(l, marker) {
			return l
		}
	}
	return ""
}

func TestXMREscrowPagesNameNetwork(t *testing.T) {
	const (
		stagenetOn  = "Monero stagenet, a public test network"
		fakechainOn = "a local Monero fakechain"
		unknownOn   = "a Monero test network that is not verified yet"
	)
	for _, c := range []struct {
		name   string
		edit   func(f *fakeXMREscrow)
		known  bool
		listed string // the canary's list item heading
		runs   string // the canary's statement of where it runs
		badge  string // the footer badge
		on     string // the checkout notice's "on …"
		tile   string // the vendor desk tile's headline
		span   string // and its text
		gone   []string
	}{
		{
			name: "stagenet", edit: onStagenet, known: true,
			listed: "<li>Monero escrow (Monero stagenet, a public test network): an order in Monero",
			runs:   "It runs on Monero stagenet, a public test network with no real funds, and has been tested only on local test chains, not yet on the public stagenet.",
			badge:  "Monero escrow only (Monero stagenet) — no real funds", on: stagenetOn,
			tile: "<strong>STAGENET</strong>", span: "One account per order, Monero stagenet only; none of your orders uses it yet",
			gone: []string{"fakechain", "not verified yet", "not verified on any public network"},
		},
		{
			name: "fakechain", edit: nil, known: true,
			listed: "<li>Monero escrow (local fakechain only): an order in Monero",
			runs:   "It runs on a local Monero fakechain, a test chain with no real funds, and is not verified on any public network.",
			badge:  "Monero escrow only (local fakechain) — no real funds", on: fakechainOn,
			tile: "<strong>FAKECHAIN</strong>", span: "One account per order, local fakechain only; none of your orders uses it yet",
			gone: []string{"stagenet", "not verified yet", "tested only on local test chains"},
		},
		{
			// A stagenet daemon still syncing (hard fork 1) is never verified, and nothing is pinned: no network is known.
			name: "unknown", known: false,
			edit: func(f *fakeXMREscrow) {
				f.nettype, f.peers, f.targetAhead, f.hardFork = "stagenet", 8, 1_000_000, 1
			},
			listed: "<li>Monero escrow (network not verified yet): an order in Monero",
			runs:   "It runs on " + unknownOn + " with no real funds, and is not verified on any public network.",
			badge:  "Monero escrow only (network not verified yet) — no real funds", on: unknownOn,
			tile: "<strong>UNAVAILABLE</strong>", span: "One account per order on a network that is not verified yet; none of your orders uses it yet",
			gone: []string{"fakechain", "stagenet", "tested only on local test chains"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnvWith(t, c.edit)
			d := &PageData{}
			if err := paymentsGlobalLoader(nil, x.A, nil, d); err != nil || d.XMREscrowNetKnown != c.known {
				t.Fatalf("the loader sets XMREscrowNetKnown = %v (%v), want %v", d.XMREscrowNetKnown, err, c.known)
			}
			product, checkout := "/product?id="+x.product, "/checkout?id="+x.product
			canary := x.page("/canary", "")
			li := pageLine(canary, "<li>Monero escrow (")
			if !strings.HasPrefix(li, c.listed) {
				t.Errorf("canary list item lacks %q:\n%s", c.listed, li)
			}
			if !strings.Contains(li, c.runs) {
				t.Errorf("canary lacks %q:\n%s", c.runs, li)
			}
			for _, w := range c.gone {
				if strings.Contains(li, w) {
					t.Errorf("canary's Monero line says %q:\n%s", w, li)
				}
			}
			// "Monero escrow on any public network" stays unavailable except on stagenet, which is one.
			unavailable := pageLine(canary, "Mainnet payments (off unless")
			if has := strings.Contains(unavailable, "Monero escrow on any public network"); has == (c.name == "stagenet") {
				t.Errorf("canary's unavailable list with %s says \"Monero escrow on any public network\" = %v:\n%s", c.name, has, unavailable)
			}
			if c.name == "stagenet" && !strings.Contains(unavailable, "Monero escrow verified on the public stagenet") {
				t.Errorf("canary's unavailable list on stagenet:\n%s", unavailable)
			}
			notice := "A listing paid by Monero escrow pays into an account of its own, on " + c.on + " with no real funds. The coins are " + xmrCustody + "."
			for path, body := range map[string]string{"/": x.page("/", x.buyerSess), product: x.page(product, x.buyerSess), checkout: x.page(checkout, x.buyerSess)} {
				page := html.UnescapeString(body)
				if !strings.Contains(page, c.badge) {
					t.Errorf("%s lacks the badge %q", path, c.badge)
				}
				if !strings.Contains(page, notice) {
					t.Errorf("%s lacks %q", path, notice)
				}
				if line := pageLine(page, "UNFUNDED DRAFT."); line != "" && strings.Contains(line, "fakechain") != (c.name == "fakechain") {
					t.Errorf("%s's unfunded-draft notice with %s:\n%s", path, c.name, line)
				}
			}
			dash := html.UnescapeString(x.page("/vendor-dashboard", x.vendorSess))
			if !strings.Contains(dash, "<div class=\"eyebrow\">Monero escrow</div>\n"+c.tile) || !strings.Contains(dash, c.span) {
				t.Errorf("vendor desk tile lacks %q and %q:\n%s", c.tile, c.span, dash)
			}
		})
	}
}

// Bitcoin escrow's chain on the same pages: each of regtest, signet and testnet4 is named, and testnet4, a public test
// network, is not also called unverified on every public network.
func TestEscrowCanaryAndNoticesNameChain(t *testing.T) {
	for _, c := range []struct{ chain, challenge, runs, unavailable, panel string }{
		{"regtest", "", "It runs on regtest, a local test chain with no real funds, and is not verified on any public network.", "Bitcoin escrow on any public network", "Bitcoin escrow on this market runs on regtest, a local test chain, and has not been verified on any public network"},
		{"signet", "51", "It runs on signet, a test network with no real funds, and is not verified on any public network.", "Bitcoin escrow on any public network", "Bitcoin escrow on this market runs on signet, a test network, and has not been verified on any public network"},
		{"testnet4", "", "It runs on testnet4, a public test network with no real funds, and has been tested only on local test chains, not yet on public testnet4.", "Bitcoin escrow verified on public testnet4", "Bitcoin escrow on this market runs on testnet4, a public test network, and has been tested only on local test chains, not yet on public testnet4"},
	} {
		t.Run(c.chain, func(t *testing.T) {
			x := newEscrowEnv(t)
			if c.chain != "regtest" {
				x.core.mu.Lock()
				x.core.chain, x.core.challenge, x.core.peers, x.core.tipTime = c.chain, c.challenge, 8, time.Now().Unix()
				x.core.mu.Unlock()
				x.restart()
			}
			if e := x.A.escrow.Load(); e == nil || e.network != c.chain {
				t.Fatalf("escrow did not start on %s", c.chain)
			}
			canary := x.page("/canary", "")
			li := pageLine(canary, "<li>Bitcoin escrow (")
			for _, w := range []string{"<li>Bitcoin escrow (" + c.chain + " only): ", c.runs} {
				if !strings.Contains(li, w) {
					t.Errorf("canary lacks %q:\n%s", w, li)
				}
			}
			if unavailable := pageLine(canary, "Mainnet payments (off unless"); !strings.Contains(unavailable, c.unavailable) {
				t.Errorf("canary's unavailable list lacks %q:\n%s", c.unavailable, unavailable)
			}
			if page := html.UnescapeString(x.page("/", x.buyerSess)); !strings.Contains(page, "Bitcoin escrow only ("+c.chain+") — no real funds") {
				t.Errorf("footer badge on %s:\n%s", c.chain, page)
			}
			if dash := html.UnescapeString(x.page("/vendor-dashboard", x.vendorSess)); !strings.Contains(dash, "2-of-3 per order, "+c.chain) {
				t.Errorf("vendor desk tile on %s:\n%s", c.chain, dash)
			}
			// The order panel's headline (order-escrow.html) says the same as the canary.
			order, _ := x.built(x.netAddr("net-refund"), x.netAddr("net-claim"), 100000)
			if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, c.panel) {
				t.Errorf("order panel on %s lacks %q:\n%s", c.chain, c.panel, pageLine(page, "Bitcoin escrow on this market runs on"))
			}
		})
	}
}

// forgetNetwork makes x's Monero escrow network unknown while its escrows stay: the pin is removed and the daemon is
// syncing at hard fork 1 (never verified), then the market restarts: a database from before the pin (A-449).
func (x *xmrEscrowEnv) forgetNetwork() {
	x.t.Helper()
	agExec(x.testEnv, "DELETE FROM settings WHERE key='escrow_xmr_network'")
	x.fake.edit(func(f *fakeXMREscrow) { f.targetAhead, f.hardFork = 1_000_000, 1 })
	x.restart()
	if x.A.escrowXMR == nil || x.A.escrowXMR.formNet() != "" {
		x.t.Fatalf("the network is still known after the pin was removed and the daemon left unverified")
	}
}

// A-472: the dispute pages, the dispute preview and /admin's Monero escrow recovery heading name the network by the
// same rule (a funded, disputed Monero escrow order); the amounts name it only once it is known.
func TestXMREscrowDisputeAndAdminNameNetwork(t *testing.T) {
	for _, c := range []struct {
		name, net, amount string
		edit              func(f *fakeXMREscrow)
		forget            bool
	}{
		{"stagenet", "Monero stagenet", "0.5 stagenet XMR", onStagenet, false},
		{"fakechain", "local fakechain", "0.5 fakechain XMR", nil, false},
		{"unknown", "network not verified yet", "0.5 XMR", onStagenet, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnvWith(t, c.edit)
			x.mustPoll()
			order, addr := x.freshCheckout(x.physical(x.vendor))
			x.fake.payAddress(addr, hex64("paid "+order), xmrListingPrice, 10)
			x.mustPoll()
			if w := x.do("POST", "/disputes", x.sessOf(order, roleBuyer), url.Values{"order_id": {order}, "reason": {"The parcel never arrived at the address I sent."}}); w.Code != 303 {
				t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
			}
			if c.forget {
				x.forgetNetwork()
			}
			_, mod := x.user("xmod_"+randomToken()[:6], "moderator")
			_, admin := x.user("xadm_"+randomToken()[:6], "admin")
			for _, p := range []struct{ name, path, sess, want string }{
				{"moderator's refusal", "/disputes", mod, "Only an administrator decides a dispute on a Monero escrow order (" + c.net + "): the decision moves coins from the escrow."},
				{"administrator's preview", "/disputes", admin, "This is a Monero escrow order (" + c.net + "): its coins are " + xmrCustody + "."},
				{"administrator's amounts", "/disputes", admin, "the escrowed " + c.amount},
				{"recovery heading", "/admin", admin, "Monero escrow recovery and retention (" + c.net + ")"},
			} {
				page := html.UnescapeString(x.page(p.path, p.sess))
				if !strings.Contains(page, p.want) {
					t.Errorf("%s lacks %q:\n%s", p.name, p.want, pageLine(page, "Monero escrow"))
				}
			}
		})
	}
}

// A-472: a Monero escrow order's panel names the network its escrow address was issued on (the address's tag, read
// from the stored address), never the market's current or default one: the same after the network was forgotten
// (a database from before the pin, its daemon unverified) and after Monero escrow was switched off.
func TestXMREscrowOrderPanelNamesItsOwnNetwork(t *testing.T) {
	const (
		stage = "Monero escrow on this market runs on Monero stagenet, a public test network, and has been tested only on local test chains, not yet on the public stagenet"
		fake  = "Monero escrow on this market runs on a Monero fakechain, a local test chain, and has not been verified on any public network"
	)
	for _, c := range []struct {
		name, badge, want, gone string
		edit                    func(f *fakeXMREscrow)
	}{
		{"stagenet", "STAGENET", stage, "fakechain", onStagenet},
		{"fakechain", "FAKECHAIN", fake, "stagenet", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnvWith(t, c.edit)
			x.mustPoll()
			order, _ := x.freshCheckout(x.digital(x.vendor))
			sess := x.sessOf(order, roleBuyer)
			check := func(state string) {
				t.Helper()
				page := html.UnescapeString(x.page("/order?id="+order, sess))
				if !strings.Contains(page, `<span class="badge testnet">`+c.badge+`</span>`) || !strings.Contains(page, c.want) {
					t.Errorf("%s: the order panel does not name %s:\n%s", state, c.name, pageLine(page, "Monero escrow"))
				}
				if strings.Contains(strings.ToLower(page), c.gone) {
					t.Errorf("%s: the order page names %s:\n%s", state, c.gone, pageLine(strings.ToLower(page), c.gone))
				}
			}
			check("ready")
			x.forgetNetwork()
			check("network forgotten")
			t.Setenv("ESCROW_XMR", "off")
			x.restart()
			check("switched off")
		})
	}
}
