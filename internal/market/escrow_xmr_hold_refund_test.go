package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-652f (owner decisions 2026-10-26): a release held for a banned vendor (A-652c) that is still held after
// ban_hold_refund_days with the ban in place is turned, by an explicit confirmed administrator action per order, into a
// refund to the order's pinned buyer refund address: completed (also when the buyer confirmed), resolved for the vendor,
// or a completed digital order the vendor never claimed. DB integration tests against the simulated Monero wallet RPC
// (escrow_xmr_fake_test.go): not chain evidence.

const holdRefundTestReason = "The vendor stayed banned for the whole hold period, so the buyer is refunded."

// ageHold moves the vendor's ban and order's history back by days, as if the hold had begun that long ago.
func (x *xmrEscrowEnv) ageHold(order string, days int) {
	x.t.Helper()
	agExec(x.testEnv, "UPDATE users SET banned_at=banned_at-make_interval(days => $2) WHERE id=$1 AND banned_at IS NOT NULL", x.vendor, days)
	agExec(x.testEnv, "UPDATE order_events SET created=created-make_interval(days => $2) WHERE order_id=$1", order, days)
}

// orderPage is order's page as sess sees it, unescaped.
func (x *xmrEscrowEnv) orderPage(order, sess string) string {
	x.t.Helper()
	return html.UnescapeString(x.page("/order?id="+order, sess))
}

// holdRefundForm is the refund form on order's page as admin sees it now (digest and receipt nonce), with admin's
// password and a reason.
func (x *xmrEscrowEnv) holdRefundForm(admin, order string) url.Values {
	x.t.Helper()
	f := formOn(x.t, x.page("/order?id="+order, admin), "/admin/escrow-xmr-hold-refund")
	f.Set("password", testPassword)
	f.Set("reason", holdRefundTestReason)
	return f
}

// holdRefundPost is a refund form for order with the digest of what the action would check now, for orders whose page
// offers no form.
func (x *xmrEscrowEnv) holdRefundPost(order string) url.Values {
	x.t.Helper()
	r, err := readHoldRefund(context.Background(), x.DB, order, defaultBanHoldRefundDays)
	if err != nil {
		x.t.Fatal(err)
	}
	return url.Values{"order_id": {order}, "digest": {holdRefundDigest(order, r)}, "password": {testPassword}, "reason": {holdRefundTestReason}}
}

// heldConfirmed is a physical order whose buyer confirmed receipt after the vendor's ban (admin bans it), its release
// held by a pass: the order and the claim address its release pays.
func (x *xmrEscrowEnv) heldConfirmed(admin string) (order, claim string) {
	x.t.Helper()
	orders, claims := x.heldConfirmedMany(admin, 1)
	return orders[0], claims[0]
}

// heldConfirmedMany is n such orders, all shipped before the ban.
func (x *xmrEscrowEnv) heldConfirmedMany(admin string, n int) (orders, claims []string) {
	x.t.Helper()
	phys := x.physical(x.vendor)
	sessions := make([]string, n)
	orders, claims = make([]string, n), make([]string, n)
	for i := range n {
		orders[i], sessions[i], claims[i] = x.shippedXMR(phys)
	}
	x.banVendor(admin)
	for i, order := range orders {
		if code, body := x.xmrConfirm(order, sessions[i], testPassword); code != 303 {
			x.t.Fatalf("confirm receipt: %d %s", code, body)
		}
	}
	x.mustPoll()
	for _, order := range orders {
		if s := x.settlementOf(order, "release"); x.state(order) != stateCompleted || s.State != "requested" || x.waitClass(s.ID) != xmrVendorClosed {
			x.t.Fatalf("held release fixture: %s %+v %q", x.state(order), s, x.waitClass(s.ID))
		}
	}
	return orders, claims
}

// checkHoldRefunded fails unless order was cancelled by admin's hold refund: one refund requested to its pinned refund
// address, the release (if any) superseded, the order history and both parties' notices.
func (x *xmrEscrowEnv) checkHoldRefunded(order, adminID, from string) {
	x.t.Helper()
	if x.state(order) != stateCancelled || x.liveOutcomes(order) != 1 {
		x.t.Fatalf("order %s after the hold refund: %s, %d live outcomes", shortID(order), x.state(order), x.liveOutcomes(order))
	}
	if s := x.settlementOf(order, "refund"); s.Supersedes != 0 || agStr(x.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", s.ID) != x.refundAddressOf(order) {
		x.t.Fatalf("order %s refund %+v is not to its pinned refund address", shortID(order), s)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='release' AND state<>'superseded'", order); n != 0 {
		x.t.Fatalf("order %s keeps %d live release(s)", shortID(order), n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state=$2 AND to_state='cancelled' AND actor_id=$3 AND note=$4", order, from, adminID, holdRefundNote); n != 1 {
		x.t.Fatalf("order %s: %d hold refund events: %q", shortID(order), n, x.events(order))
	}
	for _, role := range []string{roleBuyer, roleVendor} {
		uid := x.partyOf(order, role)
		if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND order_id=$2 AND strpos(body,$3)>0", uid, order,
			"Order "+shortID(order)+" is now Cancelled — the vendor's account stayed closed"); n != 1 {
			x.t.Fatalf("%s of %s: %d notices of the hold refund: %q", role, shortID(order), n, x.noticesOf(uid, order))
		}
	}
}

// A release held for a banned vendor, after the buyer confirmed receipt, is refunded to the buyer once it has been held
// the period: the order page offers the form, the action cancels the order, supersedes the release and requests one
// refund to the pinned refund address, which the watcher then builds and sends; the release's address is never paid.
// Both accounts are audited with the reason, and an exact replay of the receipt changes nothing.
func TestHoldRefundAfterNDays(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_admin_"+randomToken()[:6], "admin")
	order, claim := x.heldConfirmed(admin)
	release := x.settlementOf(order, "release").ID
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	page := x.orderPage(order, admin)
	mustContain(t, page, `id="hold-refund"`, "Refund the held release to the buyer", "held for less than 90 days", "even though the order completed")
	if strings.Contains(page, `action="/admin/escrow-xmr-hold-refund"`) {
		t.Fatal("the hold refund form is offered before the period")
	}
	x.ageHold(order, 91)
	page = x.orderPage(order, admin)
	mustContain(t, page, `action="/admin/escrow-xmr-hold-refund"`, "Refund "+x.xmrText(xmrListingPrice)+" to the buyer", "refund the held release to the buyer under")
	f := x.holdRefundForm(admin, order)
	w := x.do("POST", "/admin/escrow-xmr-hold-refund", admin, f)
	if w.Code != 303 || w.Header().Get("Location") != adminUserURL(handle)+"#ban-orders" {
		t.Fatalf("hold refund: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	x.checkHoldRefunded(order, adminID, stateCompleted)
	if s := x.settlement(release); s.State != "superseded" || s.TxHash != "" {
		t.Fatalf("the held release after the refund: %+v", s)
	}
	if !anyContains(x.events(order), "held while the vendor's account is closed, is cancelled") || !anyContains(x.events(order), "Escrow refund requested") {
		t.Fatalf("order history: %q", x.events(order))
	}
	if p := x.panel(order, x.sessOf(order, roleBuyer)); !strings.Contains(p, "Superseded: the vendor's account stayed closed while this release was held, so an administrator refunded the buyer instead.") {
		t.Fatalf("the buyer's panel after the hold refund:\n%s", p)
	}
	for _, uid := range []string{adminID, x.vendor} {
		if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,$2)>0 AND strpos(action,$3)>0", uid, "order "+shortID(order), holdRefundTestReason); n != 1 {
			t.Fatalf("audit rows of %s: %q", uid, agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1", uid))
		}
	}
	if got := x.cookie(w, noticeCookie); got != "hold-refund" {
		t.Fatalf("notice %q", got)
	}
	// An exact replay of the receipt returns 303 and changes nothing.
	if w := x.do("POST", "/admin/escrow-xmr-hold-refund", admin, f); w.Code != 303 {
		t.Fatalf("exact replay: %d %s", w.Code, w.Body)
	}
	x.checkHoldRefunded(order, adminID, stateCompleted)
	x.mustPoll()
	x.mustPoll()
	if s := x.settlementOf(order, "refund"); s.State == "requested" || s.State == "failed" {
		t.Fatalf("the refund was not built and sent: %+v (%q)", s, x.waitClass(s.ID))
	}
	if outs := x.outputsOf(x.settlementOf(order, "refund").ID); len(outs) != 1 || !strings.HasPrefix(outs[0], "claimant:"+x.refundAddressOf(order)+":") {
		t.Fatalf("refund outputs %v", outs)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_outputs WHERE address=$1", claim); n != 0 || x.settlement(release).State != "superseded" {
		t.Fatalf("the vendor's claim address was paid (%d outputs) or the release moved: %+v", n, x.settlement(release))
	}
}

// Before the period the action is refused (409, nothing changed) and the page says from when it can be refunded; a
// shorter period set by an administrator applies at once.
func TestHoldRefundBeforeNDaysRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_soon_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 89)
	mustContain(t, x.orderPage(order, admin), "held for less than 90 days", "it can be refunded to the buyer from")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, x.holdRefundPost(order), 409, "Nothing was changed")
	if x.state(order) != stateCompleted || x.settlementOf(order, "release").State != "requested" || x.liveOutcomes(order) != 1 {
		t.Fatal("a refused hold refund changed the order")
	}
	x.check(x.do("POST", "/admin/ban-hold-refund-days", admin, url.Values{"hold_refund_days": {"60"}, "password": {testPassword}}), 303)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	if x.state(order) != stateCancelled {
		t.Fatalf("after the period was shortened the order is %s", x.state(order))
	}
}

// Lifting the ban ends the hold; banning the vendor again starts the period again.
func TestHoldRefundReBanRestartsClock(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_reban_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 120)
	stale := x.holdRefundForm(admin, order)
	x.unbanVendor(admin)
	x.banVendor(admin) // no pass ran in between, so the release is still requested
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, x.holdRefundPost(order), 409, "held for less than 90 days")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, stale, 409, "Nothing was changed")
	if x.state(order) != stateCompleted || x.liveOutcomes(order) != 1 {
		t.Fatal("a refused hold refund changed the order")
	}
	x.ageHold(order, 90)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	if x.state(order) != stateCancelled {
		t.Fatalf("after the new period the order is %s", x.state(order))
	}
}

// An order whose dispute was decided for the vendor, its release held, is refunded the same way: resolved -> cancelled,
// the decision left as recorded, and another administrator than the one who decided may act.
func TestHoldRefundResolvedForVendor(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_res_"+randomToken()[:6], "admin")
	order, _, _ := x.shippedXMR(x.physical(x.vendor))
	x.banVendor(admin)
	x.disputeAndResolve(order, "release")
	x.mustPoll()
	if x.state(order) != stateResolved || x.waitClass(x.settlementOf(order, "release").ID) != xmrVendorClosed {
		t.Fatalf("resolved fixture: %s %q", x.state(order), x.waitClass(x.settlementOf(order, "release").ID))
	}
	x.ageHold(order, 91)
	mustContain(t, x.orderPage(order, admin), "was decided for the vendor, whichever is later")
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	x.checkHoldRefunded(order, adminID, stateResolved)
	if got := agStr(x.testEnv, "SELECT outcome FROM disputes WHERE order_id=$1", order); got != "release" {
		t.Fatalf("the recorded decision changed to %q", got)
	}
}

// Owner decision 2026-10-26: a completed digital order of a banned vendor with no release row (the vendor never
// claimed) is covered by the same action and period, refunded only to the pinned refund address. Administrators read
// its page only once the vendor is banned.
func TestHoldRefundUnclaimedDigital(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_dig_"+randomToken()[:6], "admin")
	order, _, _ := x.completed()
	if w := x.do("GET", "/order?id="+order, admin, nil); w.Code != 404 {
		t.Fatalf("an administrator reads an unbanned vendor's completed order: %d", w.Code)
	}
	handle := x.banVendor(admin)
	x.mustPoll()
	page := x.orderPage(order, admin)
	mustContain(t, page, "the vendor cannot claim the coins in this escrow", "because its vendor's account is closed and the coins in its escrow were never sent", "held for less than 90 days")
	if strings.Contains(page, "Its digital delivery") {
		t.Fatal("the reviewer sees the delivery")
	}
	mustContain(t, banOrdersRow(t, x.banOrdersPage(admin, handle), order), `href="/order?id=`+order+`"`)
	x.ageHold(order, 91)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	x.checkHoldRefunded(order, adminID, stateCompleted)
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='release'", order); n != 0 {
		t.Fatalf("%d release rows", n)
	}
	x.mustPoll()
	if s := x.settlementOf(order, "refund"); s.State == "requested" || s.State == "failed" {
		t.Fatalf("the refund was not sent: %+v", s)
	}
}

// A release that was built or may be on the network is never superseded: built with its relay answer lost, relayed,
// confirmed, failed with a transaction, and one another transaction spent (external). Each is refused (409) and left as
// it was, and the page offers no form.
func TestHoldRefundRefusesBuiltRelayedUnknown(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_built_"+randomToken()[:6], "admin")
	failedOrder, failedID := x.failedRelease()
	confirmed, _, _ := x.completed()
	confirmedID, _ := x.release(confirmed)
	x.mustPoll()
	x.fake.mine(10)
	x.mustPoll()
	relayed, _, _ := x.completed()
	relayedID, _ := x.release(relayed)
	x.mustPoll()
	external, _, _ := x.completed()
	externalID, _ := x.release(external)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='external',updated=now() WHERE id=$1", externalID) // spent by another transaction
	unknown, _, _ := x.completed()
	unknownID, _ := x.release(unknown)
	x.fake.fault("relay_tx", fakeXMRDrop)
	_ = x.poll() // the relay took effect, its answer was lost: built, relay unknown (the pass reports it)
	for id, want := range map[int64]string{failedID: "failed", confirmedID: "confirmed", relayedID: "relayed", externalID: "external", unknownID: "built"} {
		if got := x.settlement(id).State; got != want {
			t.Fatalf("fixture release %d is %s, want %s", id, got, want)
		}
	}
	if x.waitClass(unknownID) != xmrRelayUnknown {
		t.Fatalf("fixture release %d class %q", unknownID, x.waitClass(unknownID))
	}
	x.banVendor(admin)
	states := map[string]string{}
	for _, o := range []string{unknown, relayed, confirmed, external, failedOrder} {
		x.ageHold(o, 91)
		states[o] = strings.Join(x.xmrSettlements(o), ",")
	}
	for _, o := range []string{unknown, relayed, confirmed, external, failedOrder} {
		agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, x.holdRefundPost(o), 409, "never replaced by a refund")
		// Staff read such an order only for another reason (an escrow alert); its page never offers the hold refund.
		if w := x.do("GET", "/order?id="+o, admin, nil); w.Code != 404 && (w.Code != 200 || strings.Contains(w.Body.String(), `id="hold-refund"`)) {
			t.Fatalf("order %s as an administrator: %d, or it offers the hold refund", shortID(o), w.Code)
		}
		if x.state(o) != stateCompleted || strings.Join(x.xmrSettlements(o), ",") != states[o] {
			t.Fatalf("a refused hold refund changed order %s: %s %v", shortID(o), x.state(o), x.xmrSettlements(o))
		}
	}
}

// A re-request of a failed release (a chain whose first settlement had a transaction) is never superseded.
func TestHoldRefundRefusesReRequestChain(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_chain_"+randomToken()[:6], "admin")
	order, failed := x.failedRelease()
	if code := x.reRequest(admin, order, failed, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.banVendor(admin)
	x.mustPoll()
	next := x.settlementOf(order, "release")
	if next.Supersedes != failed || next.State != "requested" {
		t.Fatalf("re-request fixture: %+v", next)
	}
	x.ageHold(order, 91)
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, x.holdRefundPost(order), 409, "never replaced by a refund")
	if x.state(order) != stateCompleted || x.settlement(next.ID).State != "requested" {
		t.Fatal("a refused hold refund changed the re-request")
	}
}

// A deposit spent outside the market, or a settlement of the order spent by another transaction, refuses the refund.
func TestHoldRefundRefusesSpentOrExternal(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_spent_"+randomToken()[:6], "admin")
	spent, _, _ := x.completed()
	stray, _, _ := x.completed()
	x.banVendor(admin)
	agExec(x.testEnv, "UPDATE payments SET spent_outside=true WHERE order_id=$1", spent)
	agExec(x.testEnv, "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1", stray)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='external' WHERE order_id=$1 AND kind='stray_refund'", stray)
	for _, o := range []string{spent, stray} {
		x.ageHold(o, 91)
		agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, x.holdRefundPost(o), 409, "spent outside the market")
		if x.state(o) != stateCompleted || x.liveOutcomes(o) != 0 {
			t.Fatalf("a refused hold refund changed order %s", shortID(o))
		}
	}
}

// An administrator who is the order's buyer (or vendor) is refused before the password (403), and a moderator by the
// role gate; the buyer's page offers no staff form.
func TestHoldRefundPartyRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_party_"+randomToken()[:6], "admin")
	_, moderator := x.user("hr_mod_"+randomToken()[:6], "moderator")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 91)
	f := x.holdRefundForm(admin, order)
	buyer := x.partyOf(order, roleBuyer)
	agExec(x.testEnv, "UPDATE users SET role='admin' WHERE id=$1", buyer)
	bsess := x.sessOf(order, roleBuyer)
	if strings.Contains(x.orderPage(order, bsess), `id="hold-refund"`) {
		t.Fatal("the buyer's page offers the hold refund")
	}
	wrong := url.Values{"order_id": {order}, "digest": {f.Get("digest")}, "password": {"not the password"}, "reason": {holdRefundTestReason}}
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", bsess, wrong, 403, "You are a party to this order")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", bsess, f, 403, "You are a party to this order")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", moderator, f, 403, "")
	for _, bad := range []string{"too short", holdRefundTestReason + "‮", strings.Repeat("x", 5001)} {
		f.Set("reason", bad)
		agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, f, 400, "20–5000 characters")
	}
	if x.state(order) != stateCompleted || x.liveOutcomes(order) != 1 {
		t.Fatal("a refused hold refund changed the order")
	}
}

// Once the ban is lifted the release is no longer held: the refund is refused (409) and the next pass pays the vendor.
func TestHoldRefundUnbannedRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_unban_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 91)
	f := x.holdRefundForm(admin, order)
	x.unbanVendor(admin)
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, f, 409, "The vendor's account is not banned")
	if x.state(order) != stateCompleted {
		t.Fatal("a refused hold refund changed the order")
	}
	x.mustPoll()
	if s := x.settlementOf(order, "release"); s.State == "requested" || x.liveOutcomes(order) != 1 {
		t.Fatalf("after the ban was lifted the release is %+v", s)
	}
}

// Lock order (A-652f): the refund share-locks the vendor's account before the order, so an unban waits for it and
// neither deadlocks; an unban committed first refuses the refund.
func TestHoldRefundConcurrentUnban(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_cu_"+randomToken()[:6], "admin")
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	both, _ := x.heldConfirmedMany(admin, 2)
	first, second := both[0], both[1]
	x.ageHold(first, 91)
	x.ageHold(second, 91)

	// 1. The refund holds the account's share lock (it waits for the order, held here) while the unban waits for it.
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR UPDATE", first); err != nil {
		t.Fatal(err)
	}
	refund, unban := make(chan int, 1), make(chan int, 1)
	f := x.holdRefundForm(admin, first)
	go func() { refund <- x.do("POST", "/admin/escrow-xmr-hold-refund", admin, f).Code }()
	x.waitForLockWaits(1)
	go func() { unban <- x.do("POST", "/admin/ban", admin, banForm("unban", handle, testLiftReason)).Code }()
	x.waitForLockWaits(2)
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if code := <-refund; code != 303 {
		t.Fatalf("the refund that waited for the order: %d", code)
	}
	if code := <-unban; code != 303 {
		t.Fatalf("the unban that waited for the refund: %d", code)
	}
	x.checkHoldRefunded(first, adminID, stateCompleted)

	// 2. An unban that commits first (it holds the account row) refuses the refund once it gets the lock.
	x.banVendor(admin)
	x.ageHold(second, 91)
	g := x.holdRefundForm(admin, second)
	tx2, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if _, err = tx2.Exec("UPDATE users SET suspended_at=ban_prior_suspended_at,banned_at=NULL,banned_by=NULL,ban_reason=NULL,ban_prior_suspended_at=NULL WHERE id=$1", x.vendor); err != nil {
		t.Fatal(err)
	}
	go func() { refund <- x.do("POST", "/admin/escrow-xmr-hold-refund", admin, g).Code }()
	x.waitForLockWaits(1)
	if err = tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-refund; code != 409 {
		t.Fatalf("the refund after the unban committed: %d", code)
	}
	if x.state(second) != stateCompleted || x.settlementOf(second, "release").State != "requested" {
		t.Fatal("the refused refund changed the order")
	}
}

// A watcher store racing the refund loses: one that holds the order first is refused by migration 149 (the vendor is
// banned) and the refund then goes through; after the refund the watcher's compare-and-set finds the release superseded.
func TestHoldRefundConcurrentWatcherStore(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_cw_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	release := x.settlementOf(order, "release").ID
	x.ageHold(order, 91)
	store := "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=500000000000,key_images=ARRAY[$2] WHERE id=$1 AND state='requested'"
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", order); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	f := x.holdRefundForm(admin, order)
	go func() { done <- x.do("POST", "/admin/escrow-xmr-hold-refund", admin, f).Code }()
	x.waitForLockWaits(1)
	_, err = tx.Exec(store, release, txidOf("racing store"))
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("the watcher's store of a held release: %v", err)
	}
	tx.Rollback()
	if code := <-done; code != 303 {
		t.Fatalf("the refund after the refused store: %d", code)
	}
	x.checkHoldRefunded(order, adminID, stateCompleted)
	res, err := x.DB.Exec(store, release, txidOf("late store"))
	if err != nil {
		t.Fatalf("the watcher's compare-and-set after the refund: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 0 || x.settlement(release).State != "superseded" {
		t.Fatalf("the late store took the superseded release: %d rows, %+v", n, x.settlement(release))
	}
}

// Nothing is refunded while the restore gate is set or a restore's sweep of the escrow wallets is pending (409, and the
// page says why); once both are clear the form goes through.
func TestHoldRefundRefusedUnderGateAndSweep(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_gate_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 91)
	f := x.holdRefundForm(admin, order)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, f, 409, restoreWhy)
	mustContain(t, x.orderPage(order, admin), "Refunds are paused: "+restoreWhy)
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES($1,'true') ON CONFLICT (key) DO UPDATE SET value='true'", xmrEscrowSweepKey)
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, f, 409, banSweepWhy)
	page := x.orderPage(order, admin)
	mustContain(t, page, "Refunds are paused: "+banSweepWhy)
	if strings.Contains(page, `action="/admin/escrow-xmr-hold-refund"`) {
		t.Fatal("the form is offered during the restore sweep")
	}
	if x.state(order) != stateCompleted || x.liveOutcomes(order) != 1 {
		t.Fatal("a paused hold refund changed the order")
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key=$1", xmrEscrowSweepKey)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	if x.state(order) != stateCancelled {
		t.Fatal("the refund after the gate and sweep cleared did not happen")
	}
}

// The form states a digest of the release, the order's state, the counted sum and when the hold began; a forged or
// missing digest, or one from before the hold began again, refuses it (409, nothing changed).
func TestHoldRefundStaleDigest409(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_stale_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	x.ageHold(order, 91)
	f := x.holdRefundForm(admin, order)
	forged := x.holdRefundForm(admin, order)
	forged.Set("digest", strings.Repeat("0", 64))
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, forged, 409, "changed since the page was shown")
	forged.Del("digest")
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, forged, 409, "changed since the page was shown")
	x.unbanVendor(admin)
	x.banVendor(admin)
	x.ageHold(order, 91)
	agExpect(x.testEnv, "/admin/escrow-xmr-hold-refund", admin, f, 409, "changed since the page was shown")
	if x.state(order) != stateCompleted || x.liveOutcomes(order) != 1 {
		t.Fatal("a stale form changed the order")
	}
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
}

// A deposit after the hold refund closed the order is not counted; it goes back by the stray refund to the refund
// address, never a release or a second outcome.
func TestHoldRefundLateDepositStray(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("hr_late_"+randomToken()[:6], "admin")
	order, addr, _ := x.completed()
	x.banVendor(admin)
	x.ageHold(order, 91)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	x.checkHoldRefunded(order, adminID, stateCompleted)
	x.mustPoll()
	x.fake.payAddress(addr, hex64("late "+order), xmrListingPrice, 12)
	x.mustPoll()
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND NOT credited", order) != 1 {
		t.Fatal("the late deposit was not recorded after the close")
	}
	strays := agStrings(t, x.testEnv, "SELECT destination FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund'", order)
	if len(strays) != 1 || strays[0] != x.refundAddressOf(order) {
		t.Fatalf("late deposit stray refunds %v", strays)
	}
	if x.liveOutcomes(order) != 1 || x.state(order) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='release'", order) != 0 {
		t.Fatal("the late deposit made a second outcome or a release")
	}
}

// The edge out of completed and resolved exists only for the order and administrator holdRefundAction marks: the
// generic cancel form, a party, a moderator, the system, another order or the ban refund's marker cannot take it.
func TestHoldRefundEdgeUnreachable(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	adminID, admin := e.user("hr_edge_"+randomToken()[:6], "admin")
	modID, _ := e.user("hr_edgemod_"+randomToken()[:6], "moderator")
	order, _, _ := x.completed()
	other, _, _ := x.completed()
	paid, _ := x.paidXMRPhysical(x.physical(x.vendor))
	x.banVendor(admin)
	for _, sess := range []string{admin, x.sessOf(order, roleBuyer), x.vendorSess} {
		if w := x.do("POST", "/orders/cancel", sess, url.Values{"order_id": {order}, "from": {stateCompleted}}); w.Code < 400 {
			t.Fatalf("the generic cancel form cancelled a completed order: %d", w.Code)
		}
	}
	adminUser, mod := x.loadUser(adminID), x.loadUser(modID)
	buyerAdmin := x.loadUser(x.partyOf(order, roleBuyer))
	buyerAdmin.Role = "admin"
	try := func(ctx context.Context, id, from string, actor *User) error {
		tx, err := x.DB.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = x.A.transition(ctx, tx, id, from, stateCancelled, actor, "test")
		return err
	}
	bg := context.Background()
	for name, err := range map[string]error{
		"no marker":           try(bg, order, stateCompleted, adminUser),
		"ban refund marker":   try(withBanRefund(bg, order, adminID), order, stateCompleted, adminUser),
		"marker, other order": try(withHoldRefund(bg, other, adminID), order, stateCompleted, adminUser),
		"marker, other actor": try(withHoldRefund(bg, order, modID), order, stateCompleted, adminUser),
		"marker, moderator":   try(withHoldRefund(bg, order, modID), order, stateCompleted, mod),
		"marker, system":      try(withHoldRefund(bg, order, adminID), order, stateCompleted, nil),
		"marker, buyer admin": try(withHoldRefund(bg, order, buyerAdmin.ID), order, stateCompleted, buyerAdmin),
		"marker, paid order":  try(withHoldRefund(bg, paid, adminID), paid, statePaid, adminUser),
	} {
		if !errors.Is(err, errForbidden) {
			t.Errorf("%s: %v, want errForbidden", name, err)
		}
	}
	pooled := e.order(x.buyer, x.product, "XMR", stateCompleted)
	if err := try(withHoldRefund(bg, pooled, adminID), pooled, stateCompleted, adminUser); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("marker on a pooled order: %v", err)
	}
	if err := try(withHoldRefund(bg, order, adminID), order, stateCompleted, adminUser); err != nil {
		t.Errorf("the marked edge itself: %v", err)
	}
	for _, s := range []string{stateCompleted, stateResolved} {
		for _, tr := range viewerTransitions(&Order{State: s, Escrow: true, Currency: "XMR", BuyerID: "b", VendorID: "v"}, adminUser) {
			t.Errorf("the order page offers an administrator %s -> %s", s, tr.To)
		}
	}
	if x.state(order) != stateCompleted || x.state(paid) != statePaid {
		t.Fatal("a refused move changed an order")
	}
}

// The period is an administrator setting on Admin → Payments: default 90, 30 to 3650 whole days, password step-up,
// audited old -> new; a moderator cannot change it.
func TestHoldRefundSettingBoundsAudit(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("hr_set_"+randomToken()[:6], "admin")
	_, moderator := e.user("hr_setmod_"+randomToken()[:6], "moderator")
	page := html.UnescapeString(e.body("GET", "/admin/payments", admin, nil, 200))
	mustContain(t, page, `id="ban-hold-refund-days"`, "In force: <strong>90 days</strong>", `action="/admin/ban-hold-refund-days"`, `min="30" max="3650"`)
	for _, bad := range []string{"29", "3651", "0", "-5", "90.5", "abc", ""} {
		agExpect(e, "/admin/ban-hold-refund-days", admin, url.Values{"hold_refund_days": {bad}, "password": {testPassword}}, 400, "from 30 to 3650")
	}
	agExpect(e, "/admin/ban-hold-refund-days", admin, url.Values{"hold_refund_days": {"90"}, "password": {testPassword}}, 409, "already that many days")
	agExpect(e, "/admin/ban-hold-refund-days", moderator, url.Values{"hold_refund_days": {"60"}, "password": {testPassword}}, 403, "")
	for _, c := range []struct{ from, to string }{{"90", "30"}, {"30", "3650"}, {"3650", "120"}} {
		e.check(e.do("POST", "/admin/ban-hold-refund-days", admin, url.Values{"hold_refund_days": {c.to}, "password": {testPassword}}), 303)
		if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", adminID,
			"Changed the held-release refund period for banned vendors: "+c.from+" → "+c.to+" days (confirmed with password)"); n != 1 {
			t.Fatalf("audit of %s -> %s: %q", c.from, c.to, agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1", adminID))
		}
	}
	if got, err := banHoldRefundDays(context.Background(), e.DB); err != nil || got != 120 {
		t.Fatalf("days in force: %d %v", got, err)
	}
	mustContain(t, html.UnescapeString(e.body("GET", "/admin/payments", admin, nil, 200)), "In force: <strong>120 days</strong>")
	agExec(e, "UPDATE settings SET value='12' WHERE key=$1", banHoldRefundDaysSetting)
	if _, err := banHoldRefundDays(context.Background(), e.DB); err == nil {
		t.Fatal("a stored value out of range was accepted")
	}
}

// A-653: the refunded order is its own class in the vendor's reputation figures (a held-release refund), never a
// no-ship cancel or a vendor cancellation; it no longer counts as completed, and its review stops counting.
func TestHoldRefundReputationClass(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_rep_"+randomToken()[:6], "admin")
	order, _ := x.heldConfirmed(admin)
	agExec(x.testEnv, "INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) SELECT $1,id,product_id,buyer_id,1,'Never arrived as described' FROM orders WHERE id=$2", randomToken(), order)
	ctx := context.Background()
	before, err := vendorReputationFigures(ctx, x.DB, x.vendor)
	if err != nil {
		t.Fatal(err)
	}
	if before.CompletedConfirmed != 1 || before.Reviews != 1 || before.LowReviews != 1 || before.HeldRefunds != 0 {
		t.Fatalf("figures before: %+v", before)
	}
	x.ageHold(order, 91)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	after, err := vendorReputationFigures(ctx, x.DB, x.vendor)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeldRefunds != 1 || after.CompletedConfirmed != 0 || after.NoShipCancels != 0 || after.VendorCancels != 0 || after.Reviews != 0 || after.LowReviews != 0 {
		t.Fatalf("figures after the hold refund: %+v", after)
	}
	if reputationDigest(x.vendor, before) == reputationDigest(x.vendor, after) {
		t.Fatal("the figures' digest did not change")
	}
	rules := reputationRules(after)
	if !strings.Contains(rules[0].Figure, "1 held releases refunded to the buyer after a ban") || !strings.Contains(rules[3].Figure, "0 non-delivery refunds") {
		t.Fatalf("rule figures: %q / %q", rules[0].Figure, rules[3].Figure)
	}
}

// Retention: the refund reopens the order's financial work, its retention clock restarts at the cancellation, and the
// requested refund keeps retention from closing it.
func TestHoldRefundRetention(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("hr_ret_"+randomToken()[:6], "admin")
	order, _, _ := x.completed()
	x.banVendor(admin)
	x.ageHold(order, 200)
	agExec(x.testEnv, "UPDATE orders SET financial_closed_at=now()-interval '10 days',financial_checked_at=now() WHERE id=$1", order)
	x.check(x.do("POST", "/admin/escrow-xmr-hold-refund", admin, x.holdRefundForm(admin, order)), 303)
	if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE id=$1 AND financial_closed_at IS NULL AND financial_checked_at IS NULL", order); n != 1 {
		t.Fatal("the refund did not reopen the order's financial closure")
	}
	if got := x.retentionReason(order); got != "settlement_pending" {
		t.Fatalf("retention reason after the refund: %q", got)
	}
	if n := agInt(x.testEnv, `SELECT count(*) FROM orders o WHERE o.id=$1 AND (SELECT max(e.created) FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>e.to_state)>now()-interval '1 hour'`, order); n != 1 {
		t.Fatal("the retention clock did not restart at the cancellation")
	}
}
