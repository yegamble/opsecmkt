package market

import (
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Escrow Phase 3, slice X-c (WR-escrow A-248, P3-R3, P3-R11): Monero escrow checkout, which claims a slot the watcher
// made and makes no RPC, and the Monero escrow panel of the order page. DB integration tests against the simulated
// wallet-rpc of escrow_xmr_fake_test.go.

// 100 concurrent checkouts on 5 free slots: exactly 5 escrows, on 5 distinct slots, and 95 fixed 503s whose orders stay
// drafts (FOR UPDATE SKIP LOCKED, compare-and-set on the slot).
func TestXMREscrowSlotClaimRace(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	const n = 100
	type buyer struct{ sess, order string }
	buyers := make([]buyer, n)
	for i := range buyers {
		_, sess := x.user("race_"+randomToken()[:10], "buyer")
		uid := agStrings(t, x.testEnv, "SELECT user_id FROM sessions WHERE token_hash=$1", digest(sess))[0]
		buyers[i] = buyer{sess, x.order(uid, x.digital(x.vendor), "XMR", stateDraft)}
	}
	// Room for every request at once in both admission classes a post passes, or they refuse some as busy before the
	// claim: userPostWork's 16 slots at once, and the session lookup's 16 once a lookup has waited postLookupWait (2 s),
	// which a slow runner reached while the 100 checkouts shared the pool's 12 connections.
	savedPost, savedLookup := userPostWork, sessionLookupWork
	userPostWork, sessionLookupWork = make(chan struct{}, n), make(chan struct{}, n)
	defer func() { userPostWork, sessionLookupWork = savedPost, savedLookup }()
	before := x.fake.count()
	codes := make([]int, n)
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i, b := range buyers {
		wg.Go(func() {
			w := x.checkout(b.sess, b.order, testXMRAddr("race refund "+b.order, true))
			codes[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	wg.Wait()
	ok := 0
	for i, c := range codes {
		switch {
		case c == 303:
			ok++
		case c != 503 || !strings.Contains(bodies[i], errXMREscrowNoSlot.Error()):
			t.Fatalf("checkout %d: %d %s", i, c, bodies[i])
		}
	}
	if ok != 5 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE coin='XMR'") != 5 ||
		agInt(x.testEnv, "SELECT count(DISTINCT xmr_slot_id) FROM escrows WHERE coin='XMR'") != 5 ||
		agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != 5 ||
		agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='free'") != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM orders WHERE currency='XMR' AND state='draft'") != n-5 {
		t.Fatalf("%d checkouts succeeded", ok)
	}
	if x.fake.count() != before {
		t.Fatalf("checkout made %d RPCs", x.fake.count()-before)
	}
}

// Checkout makes no RPC (P3-R3): neither a successful checkout nor any refusal contacts the wallet-rpc or the daemon.
// Without a free slot, under the restore gate, after a restore until the sweep (xmr_escrow_sweep_required) and while
// Monero escrow is unavailable it answers a fixed 503 and the order stays a draft. A free slot of a wallet that is not
// active is never claimed.
func TestXMREscrowNoRPCAtCheckout(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	before := x.fake.count()
	order := x.draft(x.digital(x.vendor))
	x.mustCheckout(order)
	refused := func(what, order string, want error) {
		t.Helper()
		w := x.checkout(x.buyerSess, order, testXMRAddr("refund "+what, true))
		if w.Code != 503 || !strings.Contains(w.Body.String(), want.Error()) || x.state(order) != stateDraft {
			t.Fatalf("%s: %d %s (%s)", what, w.Code, w.Body.String(), x.state(order))
		}
	}
	for _, s := range []struct {
		key  string
		want error
	}{{"payments_recovery_required", errXMREscrowRestoring}, {xmrEscrowSweepKey, errXMREscrowSweep}} {
		agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", s.key)
		refused(s.key, x.draft(x.digital(x.vendor)), s.want)
		agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", s.key)
	}
	x.A.escrowXMR.ready.Store(false)
	refused("unavailable", x.draft(x.digital(x.vendor)), errXMREscrowDown)
	x.A.escrowXMR.ready.Store(true)
	// Only free slots of wallets that are not active are left.
	agExec(x.testEnv, "UPDATE xmr_escrow_slots SET state='retired' WHERE state='free'")
	for _, state := range []string{"sealed", "closing"} {
		w := xmrWallet(t, x.testEnv, 40+len(state), state)
		xmrSlot(t, x.testEnv, w, 1)
	}
	refused("no slot", x.draft(x.digital(x.vendor)), errXMREscrowNoSlot)
	if x.fake.count() != before {
		t.Fatalf("checkout made %d RPCs", x.fake.count()-before)
	}
}

// The refund address (P3-R2) is parsed offline and refused, each with its own message and the order left a draft, when
// it is missing, malformed, integrated, of another network, an escrow or slot address, a wallet's primary address, the
// fee address, or already used on this market; a subaddress or standard address of the buyer's is pinned.
func TestXMREscrowRefundAddressRefusals(t *testing.T) {
	fee := testXMRAddr("market fee", false)
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250") // over newXMREnv's defaults; the fee applies from the restart
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
	x.restart()
	x.mustPoll()
	first := x.draft(x.digital(x.vendor))
	escrowAddr := x.mustCheckout(first)
	usedRefund := testXMRAddr("refund "+first, true)
	slotAddr := agStrings(t, x.testEnv, "SELECT address FROM xmr_escrow_slots WHERE state='free' LIMIT 1")[0]
	primary := agStrings(t, x.testEnv, "SELECT primary_address FROM xmr_escrow_wallets LIMIT 1")[0]
	cases := []struct {
		what, addr string
		code       int
		want       string
	}{
		{"missing", " ", 400, "Enter a refund address"},
		{"bad checksum", testXMRAddressWith(xmrTagSubaddress, testXMRKey("s"), testXMRKey("v"), nil, true), 400, errXMRAddressChecksum.Error()},
		{"integrated", testXMRAddressWith(xmrTagIntegrated, testXMRKey("s"), testXMRKey("v"), testXMRKey("p")[:8], false), 400, errXMRAddressIntegrated.Error()},
		{"stagenet", testXMRAddressWith(24, testXMRKey("s"), testXMRKey("v"), nil, false), 400, xmrNets["fakechain"].refusal.Error()},
		{"an off-curve spend key (A-417)", testXMRAddressWith(xmrTagSubaddress, testXMROffCurveKey(), testXMRKey("v"), nil, false), 400, errXMRAddressKeys.Error()},
		{"a small-order view key (A-417)", testXMRAddressWith(xmrTagStandard, testXMRKey("s"), testXMRSmallOrderKey(), nil, false), 400, errXMRAddressKeys.Error()},
		{"a Bitcoin address", "bcrt1qbuyerrefund00000000000000000000001", 400, errXMRAddressLength.Error()},
		{"the escrow address of another order", escrowAddr, 409, "an escrow address of this market"},
		{"a free slot's address", slotAddr, 409, "an escrow address of this market"},
		{"an escrow wallet's primary address", primary, 409, "one of this market's escrow wallets"},
		{"the fee address", fee, 409, "the market fee address"},
		{"a refund address used before", usedRefund, 409, errEscrowRefundUsed.Error()},
	}
	order := x.draft(x.digital(x.vendor))
	for _, c := range cases {
		w := x.checkout(x.buyerSess, order, c.addr)
		if w.Code != c.code || !strings.Contains(html.UnescapeString(w.Body.String()), c.want) || x.state(order) != stateDraft {
			t.Fatalf("%s: %d %s", c.what, w.Code, w.Body.String())
		}
	}
	mine := testXMRAddr("buyer standard", false)
	if w := x.checkout(x.buyerSess, order, " "+mine+" "); w.Code != 303 {
		t.Fatalf("a standard address of the buyer's: %d %s", w.Code, w.Body.String())
	}
	if got := agStrings(t, x.testEnv, "SELECT refund_address||' '||fee_bps||' '||fee_destination FROM escrows WHERE order_id=$1", order); !slices.Equal(got, []string{mine + " 250 " + fee}) {
		t.Fatalf("escrow row %v", got)
	}
}

// pageForm is one form of a rendered page: its action and the values a person filling it in would send.
type pageForm struct {
	action string
	values url.Values
}

var (
	formRe     = regexp.MustCompile(`(?s)<form[^>]*method="post" action="([^"]+)"[^>]*>(.*?)</form>`)
	inputRe    = regexp.MustCompile(`<input ([^>]*)>`)
	attrRe     = regexp.MustCompile(`([a-z_-]+)(?:="([^"]*)")?`)
	textareaRe = regexp.MustCompile(`<textarea [^>]*name="([^"]+)"`)
	selectRe   = regexp.MustCompile(`(?s)<select [^>]*name="([^"]+)"[^>]*>.*?<option value="([^"]+)"`)
)

// orderForms lists the post forms of order's page as sess sees it, filled in: hidden values, each required box ticked,
// the account password, a dispute reason, delivery content and the first option of each list.
func (x *xmrEscrowEnv) orderForms(order, sess string) []pageForm {
	x.t.Helper()
	var out []pageForm
	for _, m := range formRe.FindAllStringSubmatch(x.page("/order?id="+order, sess), -1) {
		f := pageForm{action: html.UnescapeString(m[1]), values: url.Values{}}
		for _, in := range inputRe.FindAllStringSubmatch(m[2], -1) {
			attrs := map[string]string{}
			for _, a := range attrRe.FindAllStringSubmatch(in[1], -1) {
				attrs[a[1]] = html.UnescapeString(a[2])
			}
			switch name := attrs["name"]; {
			case name == "" || name == "csrf":
			case attrs["type"] == "hidden" || attrs["type"] == "checkbox":
				f.values.Set(name, attrs["value"])
			case name == "password":
				f.values.Set(name, testPassword)
			case name == "address": // a Monero escrow claim address (slice X-e): fresh for each form
				f.values.Set(name, testXMRAddr("form address "+randomToken(), true))
			case attrs["type"] == "radio":
				if !f.values.Has(name) {
					f.values.Set(name, attrs["value"])
				}
			}
		}
		for _, ta := range textareaRe.FindAllStringSubmatch(m[2], -1) {
			f.values.Set(ta[1], "The licence key in this order does not activate the product at all.")
		}
		for _, s := range selectRe.FindAllStringSubmatch(m[2], -1) {
			f.values.Set(s[1], s[2])
		}
		out = append(out, f)
	}
	return out
}

// QA on X-a (chair addendum): a Monero escrow order's page offers only actions that can succeed. In awaiting payment
// (with a deposit), paid (digital and physical), completed and, since slice X-e, shipped and resolved for the vendor,
// every form the buyer or the vendor is shown is submitted once, each on a fresh copy of the order, and none answers
// 409 (or a server error): the cancel carries amount_seen, and the pooled "Mark shipped" and the buyer's cancel of a
// paid digital order are not offered. The Monero escrow forms (Mark shipped, the claim, Confirm receipt) are offered
// exactly where they apply and each succeeds (303).
func TestXMREscrowOrderFormsNeverConflict(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	agExec(x.testEnv, "UPDATE products SET stock=100 WHERE id=$1", physical) // one order per form and viewer
	_, adminSess := x.user("xforms_admin_"+randomToken()[:6], "admin")
	scenarios := map[string]func() string{
		"awaiting payment with a deposit": func() string {
			order, addr := x.freshOrder(x.digital(x.vendor))
			x.fake.payAddress(addr, hex64("part "+order), xmrListingPrice/3, 4)
			x.mustPoll()
			return order
		},
		"paid digital": func() string {
			p := x.digital(x.vendor)
			order, addr := x.freshOrder(p)
			agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='Taken down for review.' WHERE id=$1", p) // so it is not delivered automatically
			x.fake.payAddress(addr, hex64("paid "+order), xmrListingPrice, 10)
			x.mustPoll()
			return order
		},
		"paid physical": func() string {
			order, addr := x.freshOrder(physical)
			x.fake.payAddress(addr, hex64("phys "+order), xmrListingPrice, 10)
			x.mustPoll()
			return order
		},
		"completed": func() string {
			order, addr := x.freshOrder(x.digital(x.vendor))
			x.fake.payAddress(addr, hex64("done "+order), xmrListingPrice, 10)
			x.mustPoll()
			return order
		},
		// Slice X-e: the buyer's Confirm receipt and the dispute; the vendor's claim after a decision for the vendor.
		"shipped": func() string {
			order, _, _ := x.shippedXMR(physical)
			return order
		},
		"resolved for the vendor before shipping": func() string {
			order, sess := x.paidXMRPhysical(physical)
			if code, body := x.xmrDispute(order, sess); code != 303 {
				t.Fatalf("dispute: %d %s", code, body)
			}
			dispute := agStrings(t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
			if code, body := x.xmrPost("/resolve", adminSess, url.Values{"id": {dispute}, "outcome": {"release"}, "resolution": {"Decided from the evidence both parties sent."},
				"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}}); code != 303 {
				t.Fatalf("resolve: %d %s", code, body)
			}
			return order
		},
	}
	want := map[string]string{"awaiting payment with a deposit": stateAwaitingPayment, "paid digital": statePaid, "paid physical": statePaid, "completed": stateCompleted,
		"shipped": stateShipped, "resolved for the vendor before shipping": stateResolved}
	// The Monero escrow forms of slice X-e, by scenario and viewer: each is offered there, and succeeds.
	offered := map[string]string{"paid physical/vendor": "/escrow-xmr/ship", "completed/vendor": "/escrow-xmr/claim", "shipped/buyer": "/escrow-xmr/confirm",
		"resolved for the vendor before shipping/vendor": "/escrow-xmr/claim"}
	for name, build := range scenarios {
		for _, viewer := range []string{roleBuyer, roleVendor} {
			order := build()
			if x.state(order) != want[name] {
				t.Fatalf("%s: the order is %s", name, x.state(order))
			}
			forms := x.orderForms(order, x.sessOf(order, viewer))
			for i := range forms {
				fresh := order
				if i > 0 {
					fresh = build()
				}
				f := x.orderForms(fresh, x.sessOf(fresh, viewer))[i]
				if f.action == "/logout" {
					continue
				}
				w := x.do("POST", f.action, x.sessOf(fresh, viewer), f.values)
				t.Logf("%s, %s: %s -> %d", name, viewer, f.action, w.Code)
				if w.Code == 409 || w.Code >= 500 || strings.HasPrefix(f.action, "/escrow-xmr/") && w.Code != 303 {
					t.Errorf("%s, %s: %s answered %d %s", name, viewer, f.action, w.Code, w.Body.String())
				}
			}
			if want, ok := offered[name+"/"+viewer]; ok && !slices.ContainsFunc(forms, func(f pageForm) bool { return f.action == want }) {
				t.Errorf("%s, %s: %s is not offered", name, viewer, want)
			}
			for _, f := range forms {
				if a := f.action; strings.HasPrefix(a, "/escrow-xmr/") && offered[name+"/"+viewer] != a {
					t.Errorf("%s, %s: offered %s", name, viewer, a)
				}
				if f.action == "/orders/ship" || (f.action == "/orders/cancel" && f.values.Get("from") == statePaid && viewer == roleBuyer && name == "paid digital") {
					t.Errorf("%s, %s: offered %s", name, viewer, f.action)
				}
				if f.action == "/orders/cancel" && f.values.Get("from") == stateAwaitingPayment && f.values.Get("amount_seen") == "" {
					t.Errorf("%s, %s: the cancel form carries no amount_seen", name, viewer)
				}
			}
		}
	}

	// QA-Xe F1 (A-416): with ESCROW_XMR off after a restart, the panel says Monero escrow is switched off, no Monero
	// escrow form is offered, and no form rendered answers 404, 409 or a server error. The orders are made while it is
	// on, enough for one per form.
	offStates := []string{"paid physical", "shipped", "completed"}
	pool := map[string][]string{}
	for _, name := range offStates {
		for range 6 {
			pool[name] = append(pool[name], scenarios[name]())
		}
	}
	t.Setenv("ESCROW_XMR", "off")
	x.restart()
	if x.A.escrowXMR != nil {
		t.Fatal("ESCROW_XMR=off did not switch Monero escrow off")
	}
	for _, name := range offStates {
		for _, viewer := range []string{roleBuyer, roleVendor} {
			order := pool[name][0]
			if panel := x.panel(order, x.sessOf(order, viewer)); !strings.Contains(panel, "Monero escrow is switched off on this market") {
				t.Errorf("ESCROW_XMR off, %s, %s: the panel does not say so:\n%s", name, viewer, panel)
			}
			for i := range x.orderForms(order, x.sessOf(order, viewer)) {
				fresh := pool[name][0]
				pool[name] = pool[name][1:]
				f := x.orderForms(fresh, x.sessOf(fresh, viewer))[i]
				if f.action == "/logout" {
					continue
				}
				if strings.HasPrefix(f.action, "/escrow-xmr/") {
					t.Errorf("ESCROW_XMR off, %s, %s: offered %s", name, viewer, f.action)
				}
				w := x.do("POST", f.action, x.sessOf(fresh, viewer), f.values)
				t.Logf("ESCROW_XMR off, %s, %s: %s -> %d", name, viewer, f.action, w.Code)
				if w.Code == 404 || w.Code == 409 || w.Code >= 500 {
					t.Errorf("ESCROW_XMR off, %s, %s: %s answered %d %s", name, viewer, f.action, w.Code, w.Body.String())
				}
			}
		}
	}
}

// freshOrder takes a Monero escrow for a new buyer's draft on product and returns the order and the escrow address.
func (x *xmrEscrowEnv) freshOrder(product string) (string, string) { return x.freshCheckout(product) }

// sessOf is a live session of order's buyer or vendor.
func (x *xmrEscrowEnv) sessOf(order, viewer string) string {
	x.t.Helper()
	q := "SELECT o.buyer_id FROM orders o WHERE o.id=$1"
	if viewer == roleVendor {
		q = "SELECT p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1"
	}
	uid := agStrings(x.t, x.testEnv, q, order)[0]
	sess := randomToken()
	agExec(x.testEnv, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(sess), uid)
	return sess
}

// panel is the Monero escrow panel of the order page as uid's session sees it, from its heading to the next steps
// section that follows it (the panel holds sections of its own).
func (x *xmrEscrowEnv) panel(order, sess string) string {
	x.t.Helper()
	body := html.UnescapeString(x.page("/order?id="+order, sess))
	i := strings.Index(body, "<h2>Monero escrow")
	if i < 0 {
		x.t.Fatalf("no Monero escrow panel:\n%s", body)
	}
	j := strings.Index(body[i:], `aria-labelledby="order-next"`)
	return body[i : i+j]
}

func (x *xmrEscrowEnv) page(path, sess string) string {
	x.t.Helper()
	w := x.do("GET", path, sess, nil)
	if w.Code != 200 {
		x.t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// The Monero escrow panel (P3-R11, A-402): the custody sentence verbatim, the escrow address, the price in XMR, the
// confirmations needed, the deposits, the market fee and, for the buyer only, the pinned refund address; no Bitcoin
// escrow words; the Bitcoin panel's 320 px-safe markup (wrapping address block, scrolling deposits table, no inline
// widths); its unavailable and restore-paused states. The draft offers Monero escrow checkout with the same sentence.
func TestXMREscrowOrderPanel(t *testing.T) {
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddr("market fee", false))
	x.restart()
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	draft := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	for _, want := range []string{a402Custody, `action="/orders/escrow-xmr"`, `name="refund_address"`, "Or pay into a Monero escrow"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("the draft's checkout offer lacks %q:\n%s", want, draft)
		}
	}
	var productID string
	var listedXMR int64
	if err := x.DB.QueryRow("SELECT product_id FROM orders WHERE id=$1", order).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	if err := x.DB.QueryRow("SELECT xmr FROM products WHERE id=$1", productID).Scan(&listedXMR); err != nil {
		t.Fatal(err)
	}
	x.DB.Exec("UPDATE products SET xmr=$2 WHERE id=$1", productID, payoutFloor("XMR", "release")-1)
	draft = html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if strings.Contains(draft, `action="/orders/escrow-xmr"`) || !strings.Contains(draft, "below the escrow settlement minimum") {
		t.Fatalf("legacy sub-floor Monero draft still offers escrow or lacks the floor reason:\n%s", draft)
	}
	x.DB.Exec("UPDATE products SET xmr=$2 WHERE id=$1", productID, listedXMR)
	draft = html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !strings.Contains(draft, `action="/orders/escrow-xmr"`) {
		t.Fatalf("Monero escrow offer did not return at the original valid price:\n%s", draft)
	}
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("panel-dep"), 123456789012, 4)
	x.mustPoll()
	buyer := x.panel(order, x.buyerSess)
	for _, want := range []string{a402Custody, addr, "0.5 XMR", "0.123456789012 XMR", "at least 10 confirmations", hex64("panel-dep"),
		testXMRAddr("refund "+order, true), "2.5% of the price", `<div class="payment-address">`, `<p class="mono">` + addr,
		`class="table-wrap payment-deposits" tabindex="0" role="region"`, "Waiting for confirmations"} {
		if !strings.Contains(buyer, want) {
			t.Fatalf("buyer's panel lacks %q:\n%s", want, buyer)
		}
	}
	for _, banned := range []string{"Bitcoin", "BTC", "regtest", "REGTEST", "descriptor", "multisig", "PSBT", "sign", "style=", "width="} {
		if strings.Contains(buyer, banned) {
			t.Fatalf("the Monero escrow panel says %q:\n%s", banned, buyer)
		}
	}
	vendor := x.panel(order, x.vendorSess)
	if strings.Contains(vendor, testXMRAddr("refund "+order, true)) || !strings.Contains(vendor, a402Custody) {
		t.Fatalf("vendor's panel:\n%s", vendor)
	}
	if page := x.page("/order?id="+order, x.buyerSess); strings.Contains(page, "Request payment address") || strings.Contains(page, "Bitcoin escrow") {
		t.Fatal("a Monero escrow order shows the pooled payment panel or the Bitcoin escrow")
	}
	x.A.escrowXMR.ready.Store(false)
	if p := x.panel(order, x.buyerSess); !strings.Contains(p, "Monero escrow is unavailable on this market right now") {
		t.Fatalf("unavailable:\n%s", p)
	}
	x.A.escrowXMR.ready.Store(true)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	if p := x.panel(order, x.buyerSess); !strings.Contains(p, "Paused after a restore from backup") {
		t.Fatalf("restore paused:\n%s", p)
	}
	// Funded: the completion note in the history names Monero escrow.
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	x.fake.setConfs(hex64("panel-dep"), 10)
	x.fake.payAddress(addr, hex64("panel-dep-2"), xmrListingPrice-123456789012, 10)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		t.Fatalf("funded: %s", x.state(order))
	}
	if p := x.panel(order, x.buyerSess); !strings.Contains(p, "Funded and completed") {
		t.Fatalf("completed:\n%s", p)
	}
}
