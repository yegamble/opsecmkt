package market

import (
	"context"
	"html"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Escrow Phase 3, slice X-e (WR-escrow A-248, P3-R7): the release of a Monero escrow order. The vendor claims a
// completed digital order, or marks a paid physical one shipped, with a fresh Monero address and a password step-up;
// the buyer confirms receipt of a shipped one with a password step-up; nothing is built before the order is completed
// or resolved for the vendor. DB integration tests against the simulated wallet-rpc of escrow_xmr_fake_test.go
// (simulated RPC, not chain evidence: TestXMREscrowRegtestPhysical is the real-chain leg).

// xmrPost posts form to path as sess and returns the status and the unescaped body.
func (x *xmrEscrowEnv) xmrPost(path, sess string, form url.Values) (int, string) {
	w := x.do("POST", path, sess, form)
	return w.Code, refusalText(w.Body.String())
}

func (x *xmrEscrowEnv) xmrShip(order, sess, addr, password string) (int, string) {
	return x.xmrPost("/escrow-xmr/ship", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {password}})
}

func (x *xmrEscrowEnv) xmrClaim(order, sess, addr, password string) (int, string) {
	return x.xmrPost("/escrow-xmr/claim", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {password}})
}

func (x *xmrEscrowEnv) xmrConfirm(order, sess, password string) (int, string) {
	return x.xmrPost("/escrow-xmr/confirm", sess, url.Values{"order_id": {order}, "password": {password}})
}

// xmrBuyerCancel posts the buyer's cancellation of a paid Monero escrow order with the refund confirmation it carries.
func (x *xmrEscrowEnv) xmrBuyerCancel(order, sess string) (int, string) {
	return x.xmrPost("/orders/cancel", sess, url.Values{"order_id": {order}, "from": {statePaid}, "payout_confirmed": {"confirmed"},
		"amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}})
}

func (x *xmrEscrowEnv) xmrDispute(order, sess string) (int, string) {
	return x.xmrPost("/disputes", sess, url.Values{"order_id": {order}, "reason": {"The parcel never arrived at my address at all."}})
}

// fundedXMR takes a Monero escrow for a fresh buyer's draft on product and funds it with its price at 10
// confirmations: the order and the buyer's session.
func (x *xmrEscrowEnv) fundedXMR(product string) (string, string) {
	x.t.Helper()
	order, addr := x.freshCheckout(product)
	x.fake.payAddress(addr, hex64("fund "+order), xmrListingPrice, 10)
	x.mustPoll()
	return order, x.sessOf(order, roleBuyer)
}

// undeliveredXMR is a funded digital Monero escrow order left paid (its listing was taken down after checkout, so it
// is not delivered automatically), and its buyer's session.
func (x *xmrEscrowEnv) undeliveredXMR() (string, string) {
	x.t.Helper()
	p := x.digital(x.vendor)
	order, addr := x.freshCheckout(p)
	agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='Taken down for review.' WHERE id=$1", p)
	x.fake.payAddress(addr, hex64("fund "+order), xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		x.t.Fatalf("undelivered digital order is %s", x.state(order))
	}
	return order, x.sessOf(order, roleBuyer)
}

// paidXMRPhysical is a funded physical Monero escrow order waiting to be shipped, and its buyer's session.
func (x *xmrEscrowEnv) paidXMRPhysical(product string) (string, string) {
	x.t.Helper()
	order, sess := x.fundedXMR(product)
	if x.state(order) != statePaid {
		x.t.Fatalf("funded physical Monero escrow order is %s", x.state(order))
	}
	return order, sess
}

// shippedXMR is a funded physical Monero escrow order the vendor marked shipped: the order, its buyer's session and
// the claim address its release pays.
func (x *xmrEscrowEnv) shippedXMR(product string) (string, string, string) {
	x.t.Helper()
	order, sess := x.paidXMRPhysical(product)
	claim := testXMRAddr("claim "+order, true)
	if code, body := x.xmrShip(order, x.vendorSess, claim, testPassword); code != 303 {
		x.t.Fatalf("ship: %d %s", code, body)
	}
	return order, sess, claim
}

// xmrSettlements lists order's Monero settlements as "kind state destination", oldest first.
func (x *xmrEscrowEnv) xmrSettlements(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT kind||' '||state||' '||destination FROM xmr_settlements WHERE order_id=$1 ORDER BY id", order)
}

// forceDeadline writes set on order's escrow row past migration 079's freeze trigger (escrowEnv.forceDeadline).
func (x *xmrEscrowEnv) forceDeadline(order, set string) {
	(&escrowEnv{testEnv: x.testEnv, t: x.t}).forceDeadline(order, set)
}

func (x *xmrEscrowEnv) deadlineOf(order string) string {
	return (&escrowEnv{testEnv: x.testEnv, t: x.t}).deadlineOf(order, "release_at")
}

func (x *xmrEscrowEnv) waitForLockWaits(n int) {
	(&escrowEnv{testEnv: x.testEnv, t: x.t}).waitForLockWaits(n)
}

func (x *xmrEscrowEnv) noticesOf(uid, order string) []string {
	return (&escrowEnv{testEnv: x.testEnv, t: x.t}).noticesOf(uid, order)
}

func (x *xmrEscrowEnv) events(order string) []string {
	return (&escrowEnv{testEnv: x.testEnv, t: x.t}).events(order)
}

func (x *xmrEscrowEnv) partyOf(order, viewer string) string {
	q := "SELECT o.buyer_id FROM orders o WHERE o.id=$1"
	if viewer == roleVendor {
		q = "SELECT p.vendor_id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1"
	}
	return agStrings(x.t, x.testEnv, q, order)[0]
}

// noBitcoinWords fails when s, a Monero escrow page, notice, note or desk view, uses Bitcoin escrow words.
func noBitcoinWords(t *testing.T, what, s string) {
	t.Helper()
	for _, banned := range []string{"Bitcoin", "BTC", "sign", "Sign", "PSBT", "regtest"} {
		if strings.Contains(s, banned) {
			t.Errorf("%s says %q:\n%s", what, banned, s)
		}
	}
}

// P3-R7, P3-R2: the vendor claims a completed digital Monero escrow order with a fresh Monero address, parsed offline,
// and a password step-up; the claim is one requested release (kind release, the destination, requested by the vendor),
// which the Monero escrow watcher builds. Integrated addresses, this market's escrow, slot and wallet addresses, the fee
// address and the buyer's refund address are refused, each with its own message, and change nothing.
func TestXMREscrowDigitalClaim(t *testing.T) {
	x := newXMREscrowEnv(t)
	fee := testXMRAddr("market fee", false)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
	x.restart()
	x.mustPoll()
	order, buyerSess := x.fundedXMR(x.product)
	if x.state(order) != stateCompleted {
		t.Fatalf("funded digital order is %s", x.state(order))
	}
	vendorPage := html.UnescapeString(x.page("/order?id="+order, x.vendorSess))
	if !containsAll(vendorPage, `action="/escrow-xmr/claim"`, `name="address"`, `name="password"`) {
		t.Fatalf("the vendor of a completed Monero escrow order is not offered the claim:\n%s", vendorPage)
	}
	if strings.Contains(x.page("/order?id="+order, buyerSess), `action="/escrow-xmr/claim"`) {
		t.Fatal("the buyer is offered the claim")
	}
	noBitcoinWords(t, "the vendor's Monero escrow panel", x.panel(order, x.vendorSess))

	// Refusals: each changes nothing.
	var primary, freeSlot string
	x.DB.QueryRow("SELECT primary_address FROM xmr_escrow_wallets ORDER BY id LIMIT 1").Scan(&primary)
	x.DB.QueryRow("SELECT address FROM xmr_escrow_slots WHERE state='free' ORDER BY id LIMIT 1").Scan(&freeSlot)
	escrowAddr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	other, _ := x.fundedXMR(x.product)
	fresh := testXMRAddr("claim "+order, true)
	for _, c := range []struct {
		name, addr, password string
		code                 int
		want                 string
	}{
		{"no password", fresh, "", 400, "Enter your current password"},
		{"a wrong password", fresh, "not-the-password", 401, "Password incorrect"},
		{"no address", "", testPassword, 400, "Monero"},
		{"an integrated address", testXMRAddressWith(xmrTagIntegrated, testXMRKey("s"), testXMRKey("v"), testXMRKey("p")[:8], false), testPassword, 400, errXMRAddressIntegrated.Error()},
		{"a bad checksum", testXMRAddressWith(xmrTagSubaddress, testXMRKey("s"), testXMRKey("v"), nil, true), testPassword, 400, errXMRAddressChecksum.Error()},
		{"a stagenet address", testXMRAddressWith(24, testXMRKey("s"), testXMRKey("v"), nil, false), testPassword, 400, xmrNets["fakechain"].refusal.Error()},
		{"an off-curve view key (A-417)", testXMRAddressWith(xmrTagSubaddress, testXMRKey("s"), testXMROffCurveKey(), nil, false), testPassword, 400, errXMRAddressKeys.Error()},
		{"a small-order spend key (A-417)", testXMRAddressWith(xmrTagSubaddress, testXMRSmallOrderKey(), testXMRKey("v"), nil, false), testPassword, 400, errXMRAddressKeys.Error()},
		{"this order's escrow address", escrowAddr, testPassword, 409, errXMRRefundEscrow.Error()},
		{"a free slot's address", freeSlot, testPassword, 409, errXMRRefundEscrow.Error()},
		{"an escrow wallet's primary address", primary, testPassword, 409, errXMRRefundWallet.Error()},
		{"the fee address", fee, testPassword, 409, errXMRRefundFee.Error()},
		{"the buyer's refund address", testXMRAddr("refund "+order, true), testPassword, 409, errXMRClaimRefund.Error()},
		{"another order's refund address", testXMRAddr("refund "+other, true), testPassword, 409, errClaimAddressUsed.Error()},
	} {
		code, body := x.xmrClaim(order, x.vendorSess, c.addr, c.password)
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, body := x.xmrClaim(order, buyerSess, fresh, testPassword); code != 403 || !strings.Contains(body, errClaimOnlyVendor.Error()) {
		t.Errorf("the buyer's claim: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); len(got) != 0 {
		t.Fatalf("a refused claim wrote %v", got)
	}

	if code, body := x.xmrClaim(order, x.vendorSess, fresh, testPassword); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	if got := x.xmrSettlements(order); len(got) != 1 || got[0] != "release requested "+fresh {
		t.Fatalf("after the claim: %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND requested_by=$2 AND tx_hash IS NULL", order, x.vendor); n != 1 {
		t.Fatal("the release is not requested by the vendor")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "Monero escrow") {
		t.Fatalf("the claim note does not name the coin:\n%s", ev)
	}
	noBitcoinWords(t, "the order history", ev)
	if page := x.page("/order?id="+order, x.vendorSess); strings.Contains(page, `action="/escrow-xmr/claim"`) {
		t.Fatal("the claim is offered again")
	}
	if code, body := x.xmrClaim(order, x.vendorSess, testXMRAddr("claim again "+order, true), testPassword); code != 409 || !strings.Contains(body, errClaimTwice.Error()) {
		t.Fatalf("a second claim: %d %s", code, body)
	}
	// The address a release pays is used: another order cannot claim to it.
	if code, body := x.xmrClaim(other, x.vendorSess, fresh, testPassword); code != 409 || !strings.Contains(body, errClaimAddressUsed.Error()) {
		t.Fatalf("a claim to another release's address: %d %s", code, body)
	}
	// Not before the order is completed: a paid digital order (its content could not be delivered automatically).
	paid, _ := x.undeliveredXMR()
	if x.state(paid) != statePaid {
		t.Fatalf("undelivered digital order is %s", x.state(paid))
	}
	if code, body := x.xmrClaim(paid, x.vendorSess, testXMRAddr("claim "+paid, true), testPassword); code != 409 || !strings.Contains(body, errClaimState.Error()) {
		t.Fatalf("a claim of a paid order: %d %s", code, body)
	}
	if notices["escrow-xmr-claim"].text == "" {
		t.Fatal("no notice for the Monero claim")
	}
	noBitcoinWords(t, "the claim notice", notices["escrow-xmr-claim"].text)
}

// P3-R7 (P2-R1 for Monero): a paid physical Monero escrow order is marked shipped only through the Monero escrow form,
// with a fresh claim address and the vendor's password; in one transaction the order is shipped, the release requested
// to that address, and release_at set from the escrow's release_days snapshot (a later change of the setting does not
// reach it). The vendor's ship and the buyer's no-ship cancel race on the order row: exactly one wins and the other is
// told which, with no Bitcoin wording.
func TestXMREscrowShipStepUp(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, buyerSess := x.paidXMRPhysical(physical)
	if page := html.UnescapeString(x.page("/order?id="+order, x.vendorSess)); !containsAll(page, `action="/escrow-xmr/ship"`, `name="address"`, `name="password"`) ||
		strings.Contains(page, `action="/orders/ship"`) {
		t.Fatalf("the vendor's ship form:\n%s", page)
	}
	if strings.Contains(x.page("/order?id="+order, buyerSess), `action="/escrow-xmr/ship"`) {
		t.Fatal("the buyer is offered the ship form")
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'21') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", escrowReleaseDaysSetting)
	claim := testXMRAddr("claim "+order, true)
	for _, c := range []struct {
		name, sess, addr, password string
		code                       int
		want                       string
	}{
		{"no password", x.vendorSess, claim, "", 400, ""},
		{"the buyer", buyerSess, claim, testPassword, 403, ""},
		{"an integrated address", x.vendorSess, testXMRAddressWith(xmrTagIntegrated, testXMRKey("s"), testXMRKey("v"), testXMRKey("p")[:8], false), testPassword, 400, ""},
		{"an off-curve spend key (A-417)", x.vendorSess, testXMRAddressWith(xmrTagSubaddress, testXMROffCurveKey(), testXMRKey("v"), nil, false), testPassword, 400, errXMRAddressKeys.Error()},
		{"a small-order view key (A-417)", x.vendorSess, testXMRAddressWith(xmrTagSubaddress, testXMRKey("s"), testXMRSmallOrderKey(), nil, false), testPassword, 400, errXMRAddressKeys.Error()},
		{"the buyer's refund address", x.vendorSess, testXMRAddr("refund "+order, true), testPassword, 409, ""},
	} {
		if code, body := x.xmrShip(order, c.sess, c.addr, c.password); code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if x.state(order) != statePaid || len(x.xmrSettlements(order)) != 0 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1 AND release_at IS NOT NULL", order) != 0 {
		t.Fatal("a refused ship changed the order")
	}
	if code, body := x.xmrShip(order, x.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("ship: %d %s", code, body)
	}
	if x.state(order) != stateShipped {
		t.Fatalf("after shipping: %s", x.state(order))
	}
	if got := x.xmrSettlements(order); len(got) != 1 || got[0] != "release requested "+claim {
		t.Fatalf("the release after shipping: %v", got)
	}
	// release_at is the snapshot (14 days, the default when the order was paid), not the setting changed since (21).
	if n := agInt(x.testEnv, `SELECT count(*) FROM escrows es WHERE order_id=$1 AND release_days=14
		AND abs(extract(epoch FROM es.release_at-(SELECT max(created) FROM order_events WHERE order_id=es.order_id AND to_state='shipped')-interval '14 days'))<5`, order); n != 1 {
		t.Fatal("release_at is not 14 days (the snapshot) from the shipment")
	}
	deadline := x.deadlineOf(order)
	notices := x.noticesOf(x.partyOf(order, roleBuyer), order)
	if !anyContains(notices, "Order "+shortID(order), "Shipped", "Release deadline: "+deadline, "Confirm receipt") {
		t.Fatalf("the buyer's shipped notice: %q", notices)
	}
	noBitcoinWords(t, "the buyer's notices", strings.Join(notices, "\n"))
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "Release requested at shipment (Monero escrow)") {
		t.Fatalf("the shipment note:\n%s", ev)
	}
	noBitcoinWords(t, "the order history", ev)
	if code, _ := x.xmrShip(order, x.vendorSess, testXMRAddr("claim again "+order, true), testPassword); code != 409 {
		t.Fatalf("a second ship: %d", code)
	}

	// The race with the buyer's no-ship cancel, from the ship-by time on.
	shippedAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='shipped' AND from_state='paid'`, order)
	}
	cancelledAt := func(order string) string {
		return physString(t, x.testEnv, `SELECT to_char(max(created) AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM order_events WHERE order_id=$1 AND to_state='cancelled'`, order)
	}
	count := func(order, kind string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind=$2", order, kind)
	}
	for i := range 3 {
		order, buyerSess := x.paidXMRPhysical(physical)
		x.forceDeadline(order, "ship_by=now()-interval '1 minute'")
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			t.Fatal(err)
		}
		var shipCode, cancelCode int
		var shipBody, cancelBody string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			shipCode, shipBody = x.xmrShip(order, x.vendorSess, testXMRAddr("race claim "+strconv.Itoa(i)+order, true), testPassword)
		}()
		go func() { defer wg.Done(); cancelCode, cancelBody = x.xmrBuyerCancel(order, buyerSess) }()
		x.waitForLockWaits(2)
		tx.Commit()
		wg.Wait()
		switch {
		case shipCode == 303 && cancelCode == 409:
			if x.state(order) != stateShipped || count(order, "release") != 1 || count(order, "refund") != 0 ||
				!containsAll(cancelBody, "marked this order shipped at "+shippedAt(order), "Confirm receipt") {
				t.Fatalf("ship won: order %s, cancel said %s", x.state(order), cancelBody)
			}
			noBitcoinWords(t, "the refused cancel", cancelBody)
		case cancelCode == 303 && shipCode == 409:
			if x.state(order) != stateCancelled || count(order, "release") != 0 || count(order, "refund") != 1 ||
				!strings.Contains(shipBody, "buyer cancelled this order at "+cancelledAt(order)) {
				t.Fatalf("cancel won: order %s, ship said %s", x.state(order), shipBody)
			}
		default:
			t.Fatalf("ship %d %s; cancel %d %s", shipCode, shipBody, cancelCode, cancelBody)
		}
	}
}

// P3-R7: nothing is built before the order is completed or resolved for the vendor. A release requested at shipment
// is refused its transaction by the database (escrow_market_may_sign, migration 081, through 084's
// xmr_settlements_market_fits) and is not buildable by settlementFits, the gate the Monero escrow builder reads; once
// the buyer confirms receipt it is both.
func TestXMREscrowNoBuildBeforeOutcome(t *testing.T) {
	for _, c := range []struct {
		kind, state, outcome string
		want                 bool
	}{
		{"release", statePaid, "", false}, {"release", stateShipped, "", false}, {"release", stateDisputed, "", false},
		{"release", stateCompleted, "", true}, {"release", stateResolved, "release", true}, {"release", stateResolved, "refund", false},
		{"refund", stateShipped, "", false}, {"refund", stateCancelled, "", true}, {"refund", stateResolved, "refund", true},
		{"stray_refund", stateShipped, "", false}, {"stray_refund", stateCompleted, "", true},
	} {
		if got := settlementFits(c.kind, c.state, c.outcome); got != c.want {
			t.Errorf("settlementFits(%s, %s, %q) = %v", c.kind, c.state, c.outcome, got)
		}
	}
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, buyerSess, _ := x.shippedXMR(x.physical(x.vendor))
	var id int64
	if err := x.DB.QueryRow("SELECT id FROM xmr_settlements WHERE order_id=$1 AND kind='release'", order).Scan(&id); err != nil {
		t.Fatal(err)
	}
	store := "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=500000000000,key_images=ARRAY[$2] WHERE id=$1"
	xmrRefused(t, x.testEnv, "a release built on a shipped order", "only once its order's state fits", store, id, txidOf("early"))
	var may bool
	x.DB.QueryRow("SELECT escrow_market_may_sign('release',$1)", order).Scan(&may)
	if may {
		t.Fatal("escrow_market_may_sign allows a release on a shipped order")
	}
	x.mustPoll()
	if got := x.xmrSettlements(order); len(got) != 1 || !strings.HasPrefix(got[0], "release requested ") {
		t.Fatalf("the watcher touched the release of a shipped order: %v", got)
	}
	if code, body := x.xmrConfirm(order, buyerSess, testPassword); code != 303 {
		t.Fatalf("confirm receipt: %d %s", code, body)
	}
	x.DB.QueryRow("SELECT escrow_market_may_sign('release',$1)", order).Scan(&may)
	if !may || !settlementFits("release", x.state(order), "") {
		t.Fatalf("a completed order's release is not buildable (%s)", x.state(order))
	}
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(store, id, txidOf("in time")); err != nil {
		t.Fatalf("the release of a completed order cannot be stored: %v", err)
	}
}

// P3-R7: the buyer of a shipped, undisputed Monero escrow order confirms receipt with a password step-up; the order
// row is locked and rechecked (shipped, no dispute), and the order moves to completed through the one escrow
// exception to buyerCompleteRefused, which applies only to a Monero escrow order and only through this action. Afterwards the
// buyer cannot dispute. The pooled Complete stays refused on a Monero escrow order.
func TestXMREscrowConfirmReceiptStepUp(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	physical := x.physical(x.vendor)
	order, buyerSess, _ := x.shippedXMR(physical)
	buyerPage := html.UnescapeString(x.page("/order?id="+order, buyerSess))
	if !containsAll(buyerPage, `action="/escrow-xmr/confirm"`, `name="password"`, "releases the coins in this escrow to the vendor", "cannot be undone") ||
		strings.Contains(buyerPage, `action="/orders/complete"`) {
		t.Fatalf("the buyer's Confirm receipt form:\n%s", buyerPage)
	}
	if strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow-xmr/confirm"`) {
		t.Fatal("the vendor is offered Confirm receipt")
	}
	noBitcoinWords(t, "the buyer's Monero escrow panel", x.panel(order, buyerSess))
	if code, body := x.xmrConfirm(order, buyerSess, ""); code != 400 || x.state(order) != stateShipped {
		t.Fatalf("Confirm receipt without a password: %d %s; order %s", code, body, x.state(order))
	}
	if code, _ := x.xmrConfirm(order, x.vendorSess, testPassword); code != 403 || x.state(order) != stateShipped {
		t.Fatalf("the vendor's Confirm receipt: %d; order %s", code, x.state(order))
	}
	// The pooled Complete is refused, with the Monero wording.
	code, body := x.xmrPost("/orders/complete", buyerSess, url.Values{"order_id": {order}, "payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}})
	if code != 409 || !strings.Contains(body, "Confirm receipt") || x.state(order) != stateShipped {
		t.Fatalf("the pooled Complete of a Monero escrow order: %d %s; order %s", code, body, x.state(order))
	}
	noBitcoinWords(t, "the pooled Complete's refusal", body)
	// The edge is only this action's: the buyer's move without it is refused as the pooled Complete is.
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.A.transition(context.Background(), tx, order, stateShipped, stateCompleted, x.loadUser(x.partyOf(order, roleBuyer)), ""); err != errEscrowCompleteXMR {
		t.Fatalf("the buyer's shipped -> completed outside Confirm receipt: %v", err)
	}
	tx.Rollback()

	// A dispute stops it: a disputed order, and a dispute row beside an order still shipped (rechecked under the lock).
	disputed, disputedSess, _ := x.shippedXMR(physical)
	if code, body := x.xmrDispute(disputed, disputedSess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	if code, body := x.xmrConfirm(disputed, disputedSess, testPassword); code != 409 || x.state(disputed) != stateDisputed {
		t.Fatalf("Confirm receipt of a disputed order: %d %s", code, body)
	}
	rowOnly, rowOnlySess, _ := x.shippedXMR(physical)
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'A dispute row written beside a shipped order.')", randomToken(), rowOnly)
	if code, body := x.xmrConfirm(rowOnly, rowOnlySess, testPassword); code != 409 || !strings.Contains(body, "disputed") || x.state(rowOnly) != stateShipped {
		t.Fatalf("Confirm receipt of a shipped order with a dispute row: %d %s; order %s", code, body, x.state(rowOnly))
	}

	if code, body := x.xmrConfirm(order, buyerSess, testPassword); code != 303 {
		t.Fatalf("Confirm receipt: %d %s", code, body)
	}
	if x.state(order) != stateCompleted || !anyContains(x.events(order), "shipped>completed "+x.partyOf(order, roleBuyer)+" Receipt confirmed by the buyer (Monero escrow)") {
		t.Fatalf("after Confirm receipt: %s %q", x.state(order), x.events(order))
	}
	if got := x.noticesOf(x.vendor, order); !anyContains(got, "Order "+shortID(order), "Completed", "confirmed receipt") {
		t.Fatalf("the vendor's notice: %q", got)
	}
	noBitcoinWords(t, "the vendor's notices", strings.Join(x.noticesOf(x.vendor, order), "\n"))
	noBitcoinWords(t, "the Confirm receipt notice", notices["escrow-xmr-confirmed"].text)
	for _, sess := range []string{buyerSess, x.vendorSess} {
		if code, _ := x.xmrDispute(order, sess); code != 409 || x.state(order) != stateCompleted {
			t.Fatalf("a dispute after Confirm receipt: %d; order %s", code, x.state(order))
		}
	}
	if code, _ := x.xmrConfirm(order, buyerSess, testPassword); code != 409 {
		t.Fatalf("a second Confirm receipt: %d", code)
	}
	if page := x.page("/order?id="+order, buyerSess); strings.Contains(page, `action="/escrow-xmr/confirm"`) {
		t.Fatal("Confirm receipt is offered on a completed order")
	}

	// Confirm receipt and a dispute race on the order row: exactly one wins.
	for range 2 {
		order, sess, _ := x.shippedXMR(physical)
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", order); err != nil {
			t.Fatal(err)
		}
		var confirmCode, disputeCode int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); confirmCode, _ = x.xmrConfirm(order, sess, testPassword) }()
		go func() { defer wg.Done(); disputeCode, _ = x.xmrDispute(order, x.vendorSess) }()
		x.waitForLockWaits(2)
		tx.Commit()
		wg.Wait()
		switch {
		case confirmCode == 303 && disputeCode == 409 && x.state(order) == stateCompleted:
		case disputeCode == 303 && confirmCode == 409 && x.state(order) == stateDisputed:
		default:
			t.Fatalf("confirm %d, dispute %d, order %s", confirmCode, disputeCode, x.state(order))
		}
	}
}
