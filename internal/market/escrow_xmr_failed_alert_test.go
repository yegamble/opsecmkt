package market

import (
	"html"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A-560 (STAFF66-4): a failed Monero escrow settlement that was not superseded, and one set aside because building it
// made the escrow wallet stop responding, are escrow staff alerts (escrowStaffAlert): listed under Escrow alerts on the
// moderation desk, counted in the staff badge, readable by staff and named in the order page's staff help. The text
// says whether the failed settlement can be re-requested (its transaction is stored) and, when it cannot, what to do;
// a payment-review row of a deposit that a failed settlement spends says that the settlement failed. A superseded,
// relayed or confirmed settlement raises no alert, and the parties never see the staff-only text. DB integration
// against the simulated wallet RPC (not chain evidence).
func TestXMREscrowFailedSettlementStaffAlert(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, modSess := x.user("failalert_mod_"+randomToken()[:6], "moderator")
	_, adminSess := x.user("failalert_admin_"+randomToken()[:6], "admin")

	// Re-requestable: the daemon refused the stored transaction xmrRelayAttempts times (relay-refused).
	stored, storedID := x.failedRelease()
	unbuilt, _, _ := x.completed()
	aside, _, _ := x.completed()
	sent, _, _ := x.completed()
	// Not re-requestable: failed before a transaction was built (here nothing-to-settle); and set aside. Both are
	// written before the next pass, which would otherwise build them.
	unbuiltID, _ := x.release(unbuilt)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", unbuiltID, xmrNothing)
	asideID, _ := x.release(aside)
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", asideID, xmrBuildSetAside)
	sentID, _ := x.release(sent)
	x.mustPoll()
	if s := x.settlement(sentID); s.State != "relayed" {
		t.Fatalf("the sent release: %s", s.State)
	}
	if x.settlement(asideID).State != "requested" || x.waitClass(asideID) != xmrBuildSetAside || x.settlement(unbuiltID).State != "failed" {
		t.Fatalf("fixtures: set aside %s %q, unbuilt %s", x.settlement(asideID).State, x.waitClass(asideID), x.settlement(unbuiltID).State)
	}
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	want := map[string]string{
		stored: "Monero escrow settlement " + id(storedID) + " failed and nothing was sent, but its transaction is stored: an administrator who is not a party can re-request it on the order page (Re-request settlement " + id(storedID) + ")",
		unbuilt: "Monero escrow settlement " + id(unbuiltID) + " failed before any transaction was built, so it cannot be re-requested: read why under the settlement on the order page, " +
			"and settle anything still owed by hand with the operator (a stray refund returns to the buyer any deposit it left out)",
		aside: "Monero escrow settlement " + id(asideID) + " is set aside because building it made the escrow wallet stop responding: an administrator who is not a party can ask for it to be built again on the order page (Build settlement " + id(asideID) + " again)",
	}
	staffOnly := []string{"who is not a party", "cannot be re-requested", "settle anything still owed", "Escrow alerts", "open staff alert"}
	badge := func() int {
		t.Helper()
		d := &PageData{User: &User{ID: modID, Role: "moderator"}}
		r := httptest.NewRequest("GET", "/", nil)
		if err := loadStaffAttention(r.Context(), x.A, r, d); err != nil || d.StaffAttention == nil {
			t.Fatalf("staff attention: %v", err)
		}
		return d.StaffAttention.EscrowAlerts
	}
	reviewReason := func(order string) string {
		t.Helper()
		d := &PageData{User: &User{ID: modID, Role: "moderator"}}
		r := httptest.NewRequest("GET", "/", nil)
		if err := loadPaymentReviews(r.Context(), x.A, r, d); err != nil {
			t.Fatal(err)
		}
		for _, v := range d.PaymentReviews {
			if v.OrderID == order {
				return v.Reason
			}
		}
		return ""
	}

	desk := html.UnescapeString(x.page("/moderator", modSess))
	for order, text := range want {
		if got := deskAlertIn(desk, order); !strings.Contains(got, text) {
			t.Errorf("desk alert of order %s:\n got %q\nwant %q", shortID(order), got, text)
		}
		for _, sess := range []string{modSess, adminSess} {
			if help := staffHelp(x.page("/order?id="+order, sess)); !strings.Contains(help, "because its escrow has an open staff alert: "+text+".") ||
				!strings.Contains(help, `href="/moderator#escrow-alerts"`) {
				t.Errorf("staff help of order %s: %q", shortID(order), help)
			}
		}
		for _, viewer := range []string{roleBuyer, roleVendor} {
			page := html.UnescapeString(x.page("/order?id="+order, x.sessOf(order, viewer)))
			mustNotContain(t, withoutSiteHeader(page), append(staffOnly, text)...)
		}
	}
	// The failed release's deposits are flagged as well; the help still says where they are listed.
	if help := staffHelp(x.page("/order?id="+stored, modSess)); !strings.Contains(help, `href="/moderator#payment-review"`) {
		t.Errorf("the help of the failed release with flagged deposits does not link Payment review: %q", help)
	}
	if got := deskAlertIn(desk, sent); got != "" {
		t.Errorf("a relayed settlement raised an alert: %q", got)
	}
	if n := badge(); n != 3 {
		t.Errorf("staff badge counts %d escrow alerts, want 3", n)
	}
	if got := reviewReason(stored); !strings.Contains(got, "Monero escrow settlement "+id(storedID)+", which spends it, failed and was not sent") {
		t.Errorf("payment-review reason of the failed release's deposit: %q", got)
	}
	// Data retention keeps the escrow records of an order with an open alert (UPGRADING.md, A-560).
	retention := func(order string) string {
		t.Helper()
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		reason, err := x.A.retentionEscrowBlocker(t.Context(), tx, retentionEscrowOrder{id: order})
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}
	if got := retention(unbuilt); got != "escrow_alert" {
		t.Errorf("retention blocker of the order whose settlement failed unbuilt: %q", got)
	}

	// Superseded by the administrator's re-request, then relayed and confirmed: no alert, and the reason no longer says
	// it failed. The confirmed release raises none either.
	if code := x.reRequest(adminSess, stored, storedID, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if s := x.settlement(storedID); s.State != "superseded" {
		t.Fatalf("after the re-request: %s", s.State)
	}
	x.fake.mine(10)
	x.mustPoll()
	if x.settlementOf(stored, "release").State != "confirmed" || x.settlement(sentID).State != "confirmed" {
		t.Fatalf("not confirmed: %s %s", x.settlementOf(stored, "release").State, x.settlement(sentID).State)
	}
	desk = html.UnescapeString(x.page("/moderator", modSess))
	for _, order := range []string{stored, sent} {
		if got := deskAlertIn(desk, order); got != "" {
			t.Errorf("order %s: superseded or confirmed settlement raised an alert: %q", shortID(order), got)
		}
	}
	if got := reviewReason(stored); strings.Contains(got, "failed") {
		t.Errorf("the reason still says the settlement failed once it was superseded: %q", got)
	}
	if n := badge(); n != 2 {
		t.Errorf("staff badge counts %d escrow alerts after the re-request, want 2", n)
	}
	if got := retention(sent); got == "escrow_alert" {
		t.Errorf("retention reports a confirmed settlement as an escrow alert")
	}
	if got := x.do("GET", "/order?id="+sent, modSess, nil); got.Code != 404 {
		t.Errorf("staff read an order with a confirmed settlement and no alert: %d", got.Code)
	}
}
