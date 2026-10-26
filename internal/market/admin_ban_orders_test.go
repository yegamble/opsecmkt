package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-652b (owner decision 2026-10-25, "Refund + hold"; PAY87-2, STAFF87-7): an administrator refunds a banned vendor's
// unpaid and unshipped Monero escrow orders to the buyers from the account's page. DB integration tests against the
// simulated Monero wallet RPC (escrow_xmr_fake_test.go): not chain evidence.

// banVendor bans x.vendor as the administrator session admin and returns the vendor's handle.
func (x *xmrEscrowEnv) banVendor(admin string) string {
	x.t.Helper()
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	agExpect(x.testEnv, "/admin/ban", admin, banForm("ban", handle, testBanReason), 303, "")
	return handle
}

// banOrdersPage is handle's account page as admin sees it, unescaped.
func (x *xmrEscrowEnv) banOrdersPage(admin, handle string) string {
	x.t.Helper()
	return html.UnescapeString(x.body("GET", adminUserURL(handle), admin, nil, 200))
}

// banOrdersForm is the refund form on handle's account page as admin sees it now (its digest and receipt nonce), with
// admin's password.
func (x *xmrEscrowEnv) banOrdersForm(admin, handle string) url.Values {
	x.t.Helper()
	f := formOn(x.t, x.body("GET", adminUserURL(handle), admin, nil, 200), "/admin/ban-orders")
	f.Set("password", testPassword)
	return f
}

// liveOutcomes counts order's live release or refund settlements; xmr_settlements_one_outcome allows at most one.
func (x *xmrEscrowEnv) liveOutcomes(order string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind IN ('release','refund') AND state<>'superseded'", order)
}

// refundAddressOf is the refund address order's buyer gave at checkout (escrows.refund_address).
func (x *xmrEscrowEnv) refundAddressOf(order string) string {
	return agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
}

// xmrText is n piconero as the admin page states it.
func (x *xmrEscrowEnv) xmrText(n int64) string { return x.A.escrowAmount(&Order{Currency: "XMR"}, n) }

func TestBanOrdersRefundsUnpaidAndUnshipped(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	adminID, admin := e.user("bo_admin_"+randomToken()[:6], "admin")
	unpaid, _ := x.freshCheckout(x.product)
	under, underAddr := x.freshCheckout(x.product)
	x.fake.payAddress(underAddr, hex64("under "+under), xmrListingPrice/5, 10)
	x.mustPoll()
	physical, _ := x.paidXMRPhysical(x.physical(x.vendor))
	digital, _ := x.undeliveredXMR()
	for order, want := range map[string]string{unpaid: stateAwaitingPayment, under: stateAwaitingPayment, physical: statePaid, digital: statePaid} {
		if got := x.state(order); got != want {
			t.Fatalf("fixture order %s is %s, want %s", shortID(order), got, want)
		}
	}
	handle := x.banVendor(admin)
	page := x.banOrdersPage(admin, handle)
	mustContain(t, page, `id="ban-orders"`, "Refund open orders to the buyers",
		"Awaiting payment, nothing received</th><td>1</td><td>"+x.xmrText(0)+"</td>",
		"Awaiting payment, deposit received</th><td>1</td><td>"+x.xmrText(xmrListingPrice/5)+"</td>",
		"Paid, not shipped</th><td>2</td><td>"+x.xmrText(2*xmrListingPrice)+"</td>",
		`name="digest"`, `action="/admin/ban-orders"`)
	f := x.banOrdersForm(admin, handle)
	w := x.do("POST", "/admin/ban-orders", admin, f)
	if w.Code != 303 || w.Header().Get("Location") != adminUserURL(handle)+"#ban-orders" {
		t.Fatalf("refund the banned vendor's orders: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	for _, order := range []string{unpaid, under, physical, digital} {
		if x.state(order) != stateCancelled {
			t.Fatalf("order %s is %s after the refund", shortID(order), x.state(order))
		}
		buyer := x.partyOf(order, roleBuyer)
		if !anyContains(x.noticesOf(buyer, order), "Cancelled", "the vendor's account is closed") {
			t.Fatalf("buyer of %s was not told: %q", shortID(order), x.noticesOf(buyer, order))
		}
		if !anyContains(x.events(order), "awaiting_payment>cancelled "+adminID, "the vendor's account is closed") &&
			!anyContains(x.events(order), "paid>cancelled "+adminID, "the vendor's account is closed") {
			t.Fatalf("order %s has no cancellation event by the administrator: %q", shortID(order), x.events(order))
		}
	}
	if got := x.xmrSettlements(unpaid); len(got) != 0 {
		t.Fatalf("an order with no deposit got a settlement: %v", got)
	}
	for _, order := range []string{under, physical, digital} {
		if got := x.xmrSettlements(order); len(got) != 1 || got[0] != "refund requested "+x.refundAddressOf(order) {
			t.Fatalf("order %s settlements %v, want one refund to its pinned refund address", shortID(order), got)
		}
	}
	// After reload the refunded orders are listed as cancelled while their escrow still holds coins (the unpaid one
	// holds none); nothing is left to refund, so the form is gone.
	page = x.banOrdersPage(admin, handle)
	mustContain(t, page, "No unpaid or unshipped order is left to refund")
	for _, order := range []string{under, physical, digital} {
		mustContain(t, banOrdersRow(t, page, order), "Cancelled", "refund requested")
	}
	if strings.Contains(page, ">"+shortID(unpaid)+"<") || strings.Contains(page, `action="/admin/ban-orders"`) {
		t.Fatal("the reloaded page lists an order holding nothing or still offers the refund form")
	}
	// The watcher builds and sends each refund once; xmr_settlements_one_outcome still holds. A replay of the same
	// receipt changes nothing.
	x.mustPoll()
	x.mustPoll()
	for _, order := range []string{under, physical, digital} {
		if n := x.liveOutcomes(order); n != 1 {
			t.Fatalf("order %s has %d live outcomes", shortID(order), n)
		}
		if s := x.settlementOf(order, "refund"); s.State == "requested" || s.State == "failed" {
			t.Fatalf("refund of %s not built: %+v", shortID(order), s)
		}
	}
	if agInt(e, "SELECT count(*) FROM xmr_settlements WHERE order_id=ANY($1) AND kind='release'", []string{unpaid, under, physical, digital}) != 0 {
		t.Fatal("a release was requested for a refunded order")
	}
	summary := "cancelled 4 order(s) (2 awaiting payment, 2 paid); requested 3 refund(s) of " + x.xmrText(xmrListingPrice/5+2*xmrListingPrice)
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,$2)>0 AND strpos(action,$3)>0", x.vendor, "Open orders refunded by administrator", summary) != 1 {
		t.Fatalf("vendor audit: %q", agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1", x.vendor))
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,$2)>0 AND strpos(action,$3)>0", adminID, "Refunded open orders of "+handle, summary) != 1 {
		t.Fatalf("administrator audit: %q", agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1", adminID))
	}
	if w := x.do("POST", "/admin/ban-orders", admin, f); w.Code != 303 {
		t.Fatalf("exact replay: %d %s", w.Code, w.Body)
	}
	for _, order := range []string{under, physical, digital} {
		if n := x.liveOutcomes(order); n != 1 {
			t.Fatalf("replay: order %s has %d live outcomes", shortID(order), n)
		}
	}
}

// An order cancelled with nothing received closes; a deposit that arrives later is flagged and goes back by the
// stray refund to the refund address, never a release or a second outcome.
func TestBanOrdersLateDepositStrayRefund(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_late_"+randomToken()[:6], "admin")
	order, addr := x.freshCheckout(x.product)
	handle := x.banVendor(admin)
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if x.state(order) != stateCancelled || len(x.xmrSettlements(order)) != 0 {
		t.Fatalf("unpaid order after the refund: %s %v", x.state(order), x.xmrSettlements(order))
	}
	x.fake.payAddress(addr, hex64("late "+order), xmrListingPrice, 12)
	x.mustPoll()
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND flagged AND NOT credited", order) != 1 {
		t.Fatal("the late deposit was not recorded after close and flagged")
	}
	got := x.xmrSettlements(order)
	if len(got) != 1 || !strings.HasPrefix(got[0], "stray_refund ") || !strings.HasSuffix(got[0], " "+x.refundAddressOf(order)) {
		t.Fatalf("late deposit settlements %v, want one stray refund to the refund address", got)
	}
	if x.liveOutcomes(order) != 0 || x.state(order) != stateCancelled {
		t.Fatal("the late deposit made a release or refund outcome or moved the order")
	}
}

// Shipped, disputed and completed orders are never touched; they stay listed (open or still holding coins).
func TestBanOrdersLeavesShippedDisputedCompleted(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_leave_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	shipped, _, claim := x.shippedXMR(physical)
	disputed, disputedSess := x.paidXMRPhysical(physical)
	if code, body := x.xmrDispute(disputed, disputedSess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	completed, _ := x.fundedXMR(x.product)
	if x.state(completed) != stateCompleted {
		t.Fatalf("funded digital order is %s", x.state(completed))
	}
	unpaid, _ := x.freshCheckout(x.product)
	before := map[string][]string{}
	for _, o := range []string{shipped, disputed, completed} {
		before[o] = x.xmrSettlements(o)
	}
	handle := x.banVendor(admin)
	page := x.banOrdersPage(admin, handle)
	for _, o := range []string{shipped, disputed, completed, unpaid} {
		banOrdersRow(t, page, o)
	}
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if x.state(unpaid) != stateCancelled {
		t.Fatalf("unpaid order is %s", x.state(unpaid))
	}
	for o, want := range map[string]string{shipped: stateShipped, disputed: stateDisputed, completed: stateCompleted} {
		if got := x.state(o); got != want {
			t.Fatalf("order %s moved to %s", shortID(o), got)
		}
		if got := x.xmrSettlements(o); strings.Join(got, ",") != strings.Join(before[o], ",") {
			t.Fatalf("order %s settlements changed: %v -> %v", shortID(o), before[o], got)
		}
	}
	if got := x.xmrSettlements(shipped); len(got) != 1 || got[0] != "release requested "+claim {
		t.Fatalf("shipped order's release: %v", got)
	}
}

// The form states a digest of what it refunds; a deposit, or any other change after the page was shown, refuses the
// whole submission with 409 and changes nothing. A forged or missing digest is refused too.
func TestBanOrdersStaleDigest409(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_stale_"+randomToken()[:6], "admin")
	order, addr := x.freshCheckout(x.product)
	other, _ := x.freshCheckout(x.product)
	handle := x.banVendor(admin)
	f := x.banOrdersForm(admin, handle)
	x.fake.payAddress(addr, hex64("stale "+order), xmrListingPrice/5, 10)
	x.mustPoll()
	agExpect(x.testEnv, "/admin/ban-orders", admin, f, 409, "Nothing was changed")
	forged := x.banOrdersForm(admin, handle)
	forged.Set("digest", strings.Repeat("0", 64))
	agExpect(x.testEnv, "/admin/ban-orders", admin, forged, 409, "Nothing was changed")
	forged.Del("digest")
	agExpect(x.testEnv, "/admin/ban-orders", admin, forged, 409, "Nothing was changed")
	for _, o := range []string{order, other} {
		if x.state(o) != stateAwaitingPayment || len(x.xmrSettlements(o)) != 0 {
			t.Fatalf("a refused submission changed order %s: %s %v", shortID(o), x.state(o), x.xmrSettlements(o))
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,'Open orders refunded')>0", x.vendor) != 0 {
		t.Fatal("a refused submission was audited")
	}
	// The form as shown now goes through.
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if x.state(order) != stateCancelled || x.state(other) != stateCancelled {
		t.Fatal("the current form did not refund")
	}
}

// An administrator who is the buyer of one of the orders does not refund it: it is listed for another
// administrator, who can.
func TestBanOrdersSkipsActingAdminAsBuyer(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	adminID, admin := e.user("bo_self_"+randomToken()[:6], "admin")
	_, admin2 := e.user("bo_other_"+randomToken()[:6], "admin")
	x.enableOrderPGP(adminID)
	mine := e.order(adminID, x.product, "XMR", stateDraft)
	e.check(x.checkout(admin, mine, testXMRAddrOn(x.refundNet(), "refund "+mine, true)), 303)
	theirs, _ := x.freshCheckout(x.product)
	handle := x.banVendor(admin)
	page := x.banOrdersPage(admin, handle)
	mustContain(t, page, "You are the buyer of 1 of these orders", "another administrator")
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if x.state(mine) != stateAwaitingPayment || x.state(theirs) != stateCancelled {
		t.Fatalf("after the first administrator: mine %s, theirs %s", x.state(mine), x.state(theirs))
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,$2)>0", x.vendor, "1 order(s) with the administrator as buyer left for another administrator") != 1 {
		t.Fatalf("vendor audit: %q", agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1", x.vendor))
	}
	// Only the administrator's own order is left: the first administrator gets no form for it.
	if strings.Contains(x.banOrdersPage(admin, handle), `action="/admin/ban-orders"`) {
		t.Fatal("the buyer administrator is offered a refund of their own order")
	}
	x.check(x.do("POST", "/admin/ban-orders", admin2, x.banOrdersForm(admin2, handle)), 303)
	if x.state(mine) != stateCancelled {
		t.Fatalf("another administrator's refund left the order %s", x.state(mine))
	}
}

// A ship that commits while the refund waits for the order row leaves that order out (compare-and-set), and the other
// orders are refunded. The vendor's move is made directly through the state machine: a banned vendor cannot sign in,
// so this stands for a move that was in flight. Run with -race.
func TestBanOrdersConcurrentShip(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_race_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	raced, _ := x.paidXMRPhysical(physical)
	other, _ := x.paidXMRPhysical(physical)
	handle := x.banVendor(admin)
	f := x.banOrdersForm(admin, handle)
	ctx := context.Background()
	tx, err := x.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = x.A.transition(ctx, tx, raced, statePaid, stateShipped, x.loadUser(x.vendor), "Marked shipped"); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- x.do("POST", "/admin/ban-orders", admin, f).Code }()
	x.waitForLockWaits(1)
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 303 {
		t.Fatalf("refund racing a ship: %d", code)
	}
	if x.state(raced) != stateShipped || len(x.xmrSettlements(raced)) != 0 {
		t.Fatalf("the shipped order was refunded: %s %v", x.state(raced), x.xmrSettlements(raced))
	}
	if x.state(other) != stateCancelled || x.liveOutcomes(other) != 1 {
		t.Fatalf("the other order: %s, %d outcomes", x.state(other), x.liveOutcomes(other))
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND strpos(action,$2)>0", x.vendor, "1 changed meanwhile and were left out") != 1 {
		t.Fatalf("vendor audit: %q", agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1", x.vendor))
	}
}

// While the restore gate is set, nothing is refunded and the page says why.
func TestBanOrdersRefusedUnderRestoreGate(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_gate_"+randomToken()[:6], "admin")
	order, _ := x.paidXMRPhysical(x.physical(x.vendor))
	handle := x.banVendor(admin)
	f := x.banOrdersForm(admin, handle)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	agExpect(x.testEnv, "/admin/ban-orders", admin, f, 409, restoreWhy)
	if x.state(order) != statePaid || len(x.xmrSettlements(order)) != 0 {
		t.Fatal("a refund under the restore gate changed the order")
	}
	page := x.banOrdersPage(admin, handle)
	mustContain(t, page, restoreWhy)
	if strings.Contains(page, `action="/admin/ban-orders"`) {
		t.Fatal("the refund form is offered under the restore gate")
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if x.state(order) != stateCancelled {
		t.Fatal("the refund after the gate was cleared did not happen")
	}
}

// Only a banned account's orders: an account never banned, or whose ban was lifted, is refused (409, nothing changed)
// and its page offers no form. A moderator is refused by the role gate.
func TestBanOrdersRefusedForUnbanned(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_unban_"+randomToken()[:6], "admin")
	_, moderator := x.user("bo_mod_"+randomToken()[:6], "moderator")
	order, _ := x.freshCheckout(x.product)
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	if page := x.banOrdersPage(admin, handle); strings.Contains(page, `action="/admin/ban-orders"`) || strings.Contains(page, `id="ban-orders"`) {
		t.Fatal("an account that is not banned shows the refund section")
	}
	forged := url.Values{"handle": {handle}, "from": {"user"}, "digest": {strings.Repeat("0", 64)}, "password": {testPassword}}
	agExpect(x.testEnv, "/admin/ban-orders", admin, forged, 409, handle+" is not banned")
	x.banVendor(admin)
	f := x.banOrdersForm(admin, handle)
	agExpect(x.testEnv, "/admin/ban", admin, banForm("unban", handle, testLiftReason), 303, "")
	agExpect(x.testEnv, "/admin/ban-orders", admin, f, 409, handle+" is not banned")
	agExpect(x.testEnv, "/admin/ban-orders", moderator, f, 403, "")
	agExpect(x.testEnv, "/admin/ban-orders", admin, url.Values{"handle": {"nobody_here"}, "password": {testPassword}}, 404, "")
	if x.state(order) != stateAwaitingPayment || len(x.xmrSettlements(order)) != 0 {
		t.Fatal("a refused submission changed the order")
	}
}

// The staff cancel edge exists only for the order, administrator and edge banOrdersAction marks: the generic cancel
// form, a moderator, another order, a party and a pooled order cannot take it.
func TestStaffCancelEdgeNotOnGenericForm(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	adminID, admin := e.user("bo_edge_"+randomToken()[:6], "admin")
	modID, _ := e.user("bo_edgemod_"+randomToken()[:6], "moderator")
	order, _ := x.freshCheckout(x.product)
	physical := x.physical(x.vendor)
	paid, _ := x.paidXMRPhysical(physical)
	shipped, _, _ := x.shippedXMR(physical)
	other, _ := x.freshCheckout(x.product)
	x.banVendor(admin)
	adminUser, mod := x.loadUser(adminID), x.loadUser(modID)
	for _, form := range []url.Values{
		{"order_id": {order}, "from": {stateAwaitingPayment}},
		{"order_id": {paid}, "from": {statePaid}, "payout_confirmed": {"confirmed"}, "amount_seen": {"500000000000"}},
	} {
		if w := x.do("POST", "/orders/cancel", admin, form); w.Code < 400 {
			t.Fatalf("the generic cancel form let an administrator cancel: %d", w.Code)
		}
	}
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
		"no marker":            try(bg, order, stateAwaitingPayment, adminUser),
		"no marker, paid":      try(bg, paid, statePaid, adminUser),
		"marker, other order":  try(withBanRefund(bg, other, adminID), order, stateAwaitingPayment, adminUser),
		"marker, other actor":  try(withBanRefund(bg, order, modID), order, stateAwaitingPayment, adminUser),
		"marker, moderator":    try(withBanRefund(bg, order, modID), order, stateAwaitingPayment, mod),
		"marker, system, paid": try(withBanRefund(bg, paid, adminID), paid, statePaid, nil),
	} {
		if !errors.Is(err, errForbidden) {
			t.Errorf("%s: %v, want errForbidden", name, err)
		}
	}
	// On a shipped order there is no cancel edge for anyone, marked or not.
	if err := try(withBanRefund(bg, shipped, adminID), shipped, stateShipped, adminUser); !errors.Is(err, errForbidden) {
		t.Errorf("marker on a shipped order: %v", err)
	}
	pooled := e.order(x.buyer, x.product, "XMR", stateAwaitingPayment)
	if err := try(withBanRefund(bg, pooled, adminID), pooled, stateAwaitingPayment, adminUser); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("marker on a pooled order: %v", err)
	}
	if err := try(withBanRefund(bg, order, adminID), order, stateAwaitingPayment, adminUser); err != nil {
		t.Errorf("the marked edge itself: %v", err)
	}
	for _, tr := range viewerTransitions(&Order{State: stateAwaitingPayment, Escrow: true, BuyerID: "b", VendorID: "v"}, adminUser) {
		if tr.To == stateCancelled {
			t.Error("the order page offers an administrator the staff cancel")
		}
	}
	if x.state(order) != stateAwaitingPayment || x.state(paid) != statePaid {
		t.Fatal("a refused move changed an order")
	}
}

// The banned account's page lists its open orders and those whose escrow still holds coins: link, state, escrow
// status and amount.
func TestBannedAccountOpenOrdersListed(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_list_"+randomToken()[:6], "admin")
	physical := x.physical(x.vendor)
	unpaid, _ := x.freshCheckout(x.product)
	paid, _ := x.paidXMRPhysical(physical)
	shipped, _, _ := x.shippedXMR(physical)
	completed, _ := x.fundedXMR(x.product)
	disputed, disputedSess := x.paidXMRPhysical(physical)
	if code, body := x.xmrDispute(disputed, disputedSess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	handle := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor)
	if strings.Contains(x.banOrdersPage(admin, handle), `id="ban-orders"`) {
		t.Fatal("an account that is not banned lists the ban orders section")
	}
	x.banVendor(admin)
	page := x.banOrdersPage(admin, handle)
	price := x.xmrText(xmrListingPrice)
	for order, parts := range map[string][]string{
		unpaid:    {"Awaiting payment", "Nothing received"},
		paid:      {"Paid", price + " counted; no settlement yet"},
		shipped:   {"Shipped", "release requested"},
		completed: {"Completed", price + " counted; no settlement yet"},
	} {
		row := banOrdersRow(t, page, order)
		mustContain(t, row, append(parts, "<td>"+price+"</td>")...)
		if strings.Contains(row, "href=") {
			t.Fatalf("order %s links to a page the administrator cannot read: %s", shortID(order), row)
		}
	}
	// An order the administrator can read (here: disputed) links to its page.
	mustContain(t, banOrdersRow(t, page, disputed), `href="/order?id=`+disputed+`"`, "Disputed")
}

// banOrdersRow is the table row of the ban orders list for order (its short id).
func banOrdersRow(t *testing.T, page, order string) string {
	t.Helper()
	at := strings.Index(page, ">"+shortID(order)+"</")
	if at < 0 {
		t.Fatalf("order %s is not listed:\n%s", shortID(order), page)
	}
	start := strings.LastIndex(page[:at], "<tr>")
	end := strings.Index(page[at:], "</tr>")
	return page[start : at+end]
}

// The overview flags the Users section while a banned account has open escrow orders, and not otherwise.
func TestUsersAlertBannedOpenEscrow(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("bo_alert_"+randomToken()[:6], "admin")
	alert := "1 banned account(s) have open escrow orders."
	x.freshCheckout(x.product)
	if strings.Contains(x.body("GET", "/admin", admin, nil, 200), "banned account(s) have open escrow orders") {
		t.Fatal("alert before any ban")
	}
	handle := x.banVendor(admin)
	mustContain(t, x.body("GET", "/admin", admin, nil, 200), "<strong>"+alert+"</strong>")
	x.check(x.do("POST", "/admin/ban-orders", admin, x.banOrdersForm(admin, handle)), 303)
	if strings.Contains(x.body("GET", "/admin", admin, nil, 200), "banned account(s) have open escrow orders") {
		t.Fatal("alert once the banned account has no open escrow order")
	}
}
