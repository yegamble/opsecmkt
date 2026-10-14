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
// canary names stagenet a public test network and fakechain local, and (A-512) never claims per-instance proof: the
// project's recorded public-network runs are listed in docs/verification.md and this market itself has not been
// checked. The same pages name Bitcoin escrow's chain (regtest, signet, testnet4). DB integration with the simulated
// Core and wallet-rpc: copy checks, not chain evidence.

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
			listed: "<li>Monero escrow (Monero stagenet, a public test network): an order in Monero can be paid into an escrow account of its own",
			runs:   "It runs on Monero stagenet, a public test network with no real funds. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network.",
			badge:  "Monero escrow (Monero stagenet)", on: stagenetOn,
			tile: "<strong>STAGENET</strong>", span: "One account per order; " + xmrCustody,
			gone: []string{"fakechain", "not verified yet", "not verified on any public network", "tested only on local test chains"},
		},
		{
			name: "fakechain", edit: nil, known: true,
			listed: "<li>Monero escrow (local fakechain only): an order in Monero can be paid into an escrow account of its own",
			runs:   "It runs on a local Monero fakechain, a test chain with no real funds. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network.",
			badge:  "Monero escrow (local fakechain)", on: fakechainOn,
			tile: "<strong>FAKECHAIN</strong>", span: "One account per order; " + xmrCustody,
			gone: []string{"stagenet", "not verified yet", "tested only on local test chains", "not verified on any public network"},
		},
		{
			// A stagenet daemon still syncing (hard fork 1) is never verified, and nothing is pinned: no network is known.
			name: "unknown", known: false,
			edit: func(f *fakeXMREscrow) {
				f.nettype, f.peers, f.targetAhead, f.hardFork = "stagenet", 8, 1_000_000, 1
			},
			listed: "<li>Monero escrow (network not verified yet): an order in Monero can be paid into an escrow account of its own",
			runs:   "It runs on a Monero test network with no real funds, and the market has not yet checked which. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network.",
			badge:  "Monero escrow (network not verified yet)", on: unknownOn,
			tile: "<strong>UNAVAILABLE</strong>", span: "One account per order; " + xmrCustody,
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
			// QA5W1-8: the line says "not verified" once at most (the heading when the network is unknown), never twice.
			if n := strings.Count(li, "not verified"); n > 1 {
				t.Errorf("canary's Monero line says \"not verified\" %d times:\n%s", n, li)
			}
			// A-512: the unavailable list makes no public-network claim of its own; the escrow lines carry the record note.
			unavailable := pageLine(canary, "commission or fee accounting")
			for _, w := range []string{"on any public network", "verified on the public stagenet"} {
				if strings.Contains(unavailable, w) {
					t.Errorf("canary's unavailable list with %s says %q:\n%s", c.name, w, unavailable)
				}
			}
			notice := "A Monero escrow order pays into its own account on " + c.on + ". The coins are " + xmrCustody + "."
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
			if !strings.Contains(dash, "<div class=\"eyebrow\">Monero escrow</div>"+c.tile) || !strings.Contains(dash, c.span) {
				t.Errorf("vendor desk tile lacks %q and %q:\n%s", c.tile, c.span, dash)
			}
		})
	}
}

// Bitcoin escrow's chain on the same pages: each of regtest, signet and testnet4 is named (signet and testnet4 as public
// test networks, regtest as local), and each page points to the project's records instead of claiming per-instance proof.
func TestEscrowCanaryAndNoticesNameChain(t *testing.T) {
	for _, c := range []struct{ chain, challenge, runs, panel string }{
		{"regtest", "", "It runs on regtest, a local test chain with no real funds. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network.", "Bitcoin escrow on this market runs on regtest, a local test chain; its coins have no value. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."},
		{"signet", "51", "It runs on signet (the public signet, unless this operator runs a private one) with no real funds. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network.", "Bitcoin escrow on this market runs on signet (the public signet, unless this operator runs a private one); its coins have no value. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."},
		{"testnet4", "", "It runs on testnet4, a public test network with no real funds. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network. The project has no recorded run on public testnet4.", "Bitcoin escrow on this market runs on testnet4, a public test network; its coins have no value. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network. The project has no recorded run on public testnet4."},
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
			if unavailable := pageLine(canary, "commission or fee accounting"); strings.Contains(unavailable, "on any public network") || strings.Contains(unavailable, "verified on public testnet4") {
				t.Errorf("canary's unavailable list makes a public-network claim of its own:\n%s", unavailable)
			}
			if page := html.UnescapeString(x.page("/", x.buyerSess)); !strings.Contains(page, "Bitcoin escrow ("+c.chain+")") {
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
		stage = "Monero escrow on this market runs on Monero stagenet, a public test network; its coins have no value. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."
		fake  = "Monero escrow on this market runs on a Monero fakechain, a local test chain; its coins have no value. The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."
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

// QA5W1-3: the buyer's Monero escrow checkout offer names the network by the rule of payment-status:xmr-net: "network
// not verified yet" until a daemon was verified (never the fakechain the unverified market defaults to), Monero
// stagenet, or a local Monero fakechain.
func TestXMREscrowCheckoutOfferNamesNetwork(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(f *fakeXMREscrow)
		heading string
		text    string
		gone    []string
	}{
		{
			name: "stagenet", edit: onStagenet,
			heading: "Or pay into a Monero escrow (test network only)",
			text:    "Monero escrow on this market runs on Monero stagenet, a public test network, only.",
			gone:    []string{"fakechain", "local test chain", "not verified yet"},
		},
		{
			name: "fakechain", edit: nil,
			heading: "Or pay into a Monero escrow (local test chain only)",
			text:    "Monero escrow on this market runs on a local test chain (a Monero fakechain) only.",
			gone:    []string{"stagenet", "not verified yet"},
		},
		{
			// A stagenet daemon still syncing (hard fork 1) is never verified, and nothing is pinned: no network is known.
			name: "unknown", edit: func(f *fakeXMREscrow) {
				f.nettype, f.peers, f.targetAhead, f.hardFork = "stagenet", 8, 1_000_000, 1
			},
			heading: "Or pay into a Monero escrow (network not verified yet)",
			text:    "Monero escrow on this market runs on a Monero test network that is not verified yet, with no real funds.",
			gone:    []string{"fakechain", "stagenet", "local test chain", "test network only"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newXMREscrowEnvWith(t, c.edit)
			order := x.testEnv.order(x.buyer, x.product, "XMR", stateDraft)
			page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
			offer := pageLine(page, "Or pay into a Monero escrow")
			if !strings.Contains(page, "<h3 id=\"xmr-escrow-checkout-heading\">"+c.heading+"</h3>") || !strings.Contains(page, c.text) {
				t.Errorf("the checkout offer lacks %q and %q:\n%s", c.heading, c.text, offer)
			}
			_, section, _ := strings.Cut(page, `aria-labelledby="xmr-escrow-checkout-heading"`)
			section, _, _ = strings.Cut(section, "</section>")
			for _, w := range c.gone {
				if strings.Contains(section, w) {
					t.Errorf("the checkout offer says %q:\n%s", w, section)
				}
			}
		})
	}
}

// A-540: the escrow key page and the canary say where Bitcoin escrow runs and whether its coins are real exactly once.
// On a mainnet market "Bitcoin mainnet, with real funds" is the whole statement: "no real funds" follows it nowhere. The
// test-network sentences (regtest, signet, testnet4, and the chain not yet verified) are pinned byte for byte. The
// mainnet market is the simulated Core on chain main (escrow_mainnet_test.go): copy checks, never a mainnet node.
func TestEscrowKeyAndCanaryRealFundsWording(t *testing.T) {
	const record = " The project's recorded public-network runs are listed in docs/verification.md; this market itself has not been checked on a public network."
	for _, c := range []struct{ chain, challenge, key, canary string }{
		{"regtest", "", "runs on regtest, a local test chain, with no real funds.", "It runs on regtest, a local test chain with no real funds." + record},
		{"signet", "51", "runs on signet (the public signet, unless this operator runs a private one), with no real funds.", "It runs on signet (the public signet, unless this operator runs a private one) with no real funds." + record},
		{"testnet4", "", "runs on testnet4, a public test network, with no real funds.", "It runs on testnet4, a public test network with no real funds." + record + " The project has no recorded run on public testnet4."},
		{"main", "", "runs on Bitcoin mainnet, with real funds.", "It runs on Bitcoin mainnet, with real funds. The project has no recorded mainnet run: escrow on mainnet is the operator's choice, with real funds."},
	} {
		t.Run(c.chain, func(t *testing.T) {
			x := newEscrowEnv(t)
			switch c.chain {
			case "regtest":
			case "main":
				mainnetCore(t, x.core)
				t.Setenv("BITCOIN_CHAIN", "main")
				x.restart()
			default:
				x.core.mu.Lock()
				x.core.chain, x.core.challenge, x.core.peers, x.core.tipTime = c.chain, c.challenge, 8, time.Now().Unix()
				x.core.mu.Unlock()
				x.restart()
			}
			if e := x.A.escrow.Load(); e == nil || e.network != c.chain {
				t.Fatalf("escrow did not start on %s", c.chain)
			}
			x.checkWording(c.key, c.canary, c.chain == "main")
		})
	}
	// A-540: a mainnet market whose escrow node has not answered and whose chain was never pinned still says mainnet, with
	// real funds, on /canary: never "a test network (chain not verified yet)" beside the catalog's "Mainnet · real funds".
	t.Run("mainnet, chain not verified yet", func(t *testing.T) {
		x := newEscrowEnv(t)
		x.DB.Exec("DELETE FROM settings WHERE key=$1", escrowChainPin)
		if _, err := x.DB.Exec(`UPDATE bitcoin_escrow_health SET enabled=false,disabled_class='not_checked',last_pass=NULL,
			chain='unknown',height=NULL,pin_match=NULL,keypool_high_water=NULL,highest_market_index=0,updated_at=now() WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		mainnetCore(t, x.core)
		t.Setenv("BITCOIN_CHAIN", "main")
		srvDown(x, true)
		x.restart()
		if !x.A.live || x.A.escrowNetKnown() {
			t.Fatalf("live %v, chain known %v: want a live market whose chain is not known yet", x.A.live, x.A.escrowNetKnown())
		}
		x.checkWording("", "It runs on Bitcoin mainnet, with real funds. The project has no recorded mainnet run: escrow on mainnet is the operator's choice, with real funds.", true)
		if canary := pageLine(x.page("/canary", ""), "<li>Bitcoin escrow ("); strings.Contains(canary, "test network") || strings.Contains(canary, "not verified yet") {
			t.Errorf("/canary on a mainnet market names a test network or an unverified chain:\n%s", canary)
		}
	})
	t.Run("chain not verified yet", func(t *testing.T) {
		x := newEscrowEnv(t)
		x.DB.Exec("DELETE FROM settings WHERE key=$1", escrowChainPin)
		if _, err := x.DB.Exec(`UPDATE bitcoin_escrow_health SET enabled=false,disabled_class='not_checked',last_pass=NULL,
			chain='unknown',height=NULL,pin_match=NULL,keypool_high_water=NULL,highest_market_index=0,updated_at=now() WHERE singleton`); err != nil {
			t.Fatal(err)
		}
		srvDown(x, true)
		x.restart()
		// The escrow key page has no such sentence while escrow is unavailable (its only way to an unverified chain).
		x.checkWording("",
			"It runs on a test network (chain not verified yet) with no real funds."+record, false)
	})
}

// checkWording asserts the escrow key page's and the canary's statements of where Bitcoin escrow runs: key and canary
// are the sentences expected, and mainnet says whether the market is on mainnet (then "no real funds" appears in
// neither Bitcoin statement, and the real-funds wording does).
func (x *escrowEnv) checkWording(key, canary string, mainnet bool) {
	x.t.Helper()
	keyLine := pageLine(x.page("/escrow-key", x.buyerSess), "Bitcoin escrow on this market runs on")
	canaryLine := pageLine(x.page("/canary", ""), "<li>Bitcoin escrow (")
	if key == "" && keyLine != "" || key != "" && !strings.Contains(keyLine, key) {
		x.t.Errorf("/escrow-key lacks %q:\n%s", key, keyLine)
	}
	if !strings.Contains(canaryLine, canary) {
		x.t.Errorf("/canary lacks %q:\n%s", canary, canaryLine)
	}
	for name, line := range map[string]string{"/escrow-key": keyLine, "/canary": canaryLine} {
		if line == "" {
			continue // an expected absence (key == "") was asserted above
		}
		if mainnet && strings.Contains(line, "no real funds") {
			x.t.Errorf("%s says \"no real funds\" on a mainnet market:\n%s", name, line)
		}
		if mainnet && !strings.Contains(line, "Bitcoin mainnet, with real funds") {
			x.t.Errorf("%s lacks the real-funds wording on a mainnet market:\n%s", name, line)
		}
	}
}
