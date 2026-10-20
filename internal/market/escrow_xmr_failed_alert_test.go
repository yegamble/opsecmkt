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
	// PAY560-1: the stored transaction may have reached the network (relay_tx's -4 after a lost connection), so the text
	// never says nothing was sent, and it offers the re-request only when the action would record one.
	const storedFailed = "failed with its transaction stored, which the market has not seen mined"
	storedOffer := storedFailed + ": an administrator who is not a party can re-request it on the order page (Re-request settlement " + id(storedID) +
		"), and the market rebuilds it only if that transaction is in neither the daemon's pool nor the chain and none of its coins is spent"
	want := map[string]string{
		stored: "Monero escrow settlement " + id(storedID) + " " + storedOffer,
		unbuilt: "Monero escrow settlement " + id(unbuiltID) + " failed before any transaction was built, so it cannot be re-requested: read why under the settlement on the order page, " +
			"settle anything still owed by hand with the operator, then an administrator who is not a party marks it settled on the order page (Mark settlement " + id(unbuiltID) + " settled by hand)",
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
	if got := reviewReason(stored); !strings.Contains(got, "Monero escrow settlement "+id(storedID)+", which spends it, "+storedOffer) {
		t.Errorf("payment-review reason of the failed release's deposit: %q", got)
	}
	for _, text := range []string{deskAlertIn(desk, stored), reviewReason(stored), staffHelp(x.page("/order?id="+stored, adminSess))} {
		mustNotContain(t, text, "nothing was sent", "was not sent")
	}
	// storedSays fails unless the desk entry and the staff help of the failed release say "Monero escrow settlement N
	// "+clause and its payment-review reason "Monero escrow settlement N, which spends it, "+clause, and none of them says
	// any of unwanted (the order page's own re-request form, A-401, may still be offered after a refusal: the watcher
	// decides each request against the daemon).
	storedSays := func(stage, clause string, unwanted ...string) {
		t.Helper()
		desk := html.UnescapeString(x.page("/moderator", modSess))
		alert := "Monero escrow settlement " + id(storedID) + " " + clause
		for name, text := range map[string]string{"desk": deskAlertIn(desk, stored), "help": staffHelp(x.page("/order?id="+stored, adminSess)), "reason": reviewReason(stored)} {
			want := alert
			if name == "reason" {
				want = "Monero escrow settlement " + id(storedID) + ", which spends it, " + clause
			}
			if !strings.Contains(text, want) {
				t.Errorf("%s: the %s does not say %q: %q", stage, name, want, text)
			}
			for _, u := range unwanted {
				if strings.Contains(text, u) {
					t.Errorf("%s: the %s says %q: %q", stage, name, u, text)
				}
			}
		}
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

	// NIT-1: with Monero escrow off the re-request action and its form are disabled, so the text says so.
	saved := x.A.escrowXMR
	x.A.escrowXMR = nil
	storedSays("escrow off", storedFailed+": Monero escrow (ESCROW_XMR) is off on this market, so it can be re-requested on the order page only once the operator turns it back on",
		"(Re-request settlement")
	x.A.escrowXMR = saved

	// PAY560-1 (i): the stored transaction was relayed after all and mined; a re-request is refused (on-chain). The alert
	// and the reason no longer offer the re-request; they name the refusal and tell staff to check the wallet and the chain.
	old := x.settlement(storedID)
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	x.fake.mine(1)
	if code := x.reRequest(adminSess, stored, storedID, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	// PAY560-1 (ii): while it waits for the watcher's pass, the text says so and offers nothing (a second one is a 409).
	storedSays("re-request waiting", storedFailed+": an administrator's re-request waits for the market's next pass over the escrow wallet",
		"can re-request it", "(Re-request settlement")
	saved = x.A.escrowXMR
	x.A.escrowXMR = nil
	storedSays("re-request waiting, escrow off", storedFailed+": an administrator's re-request waits for the escrow watcher, which runs only once the operator turns Monero escrow (ESCROW_XMR) back on", "can re-request it")
	x.A.escrowXMR = saved
	x.mustPoll()
	if got := x.reRequests(storedID); len(got) != 1 || got[0] != "refused:"+xmrReRequestOnChain+":0" {
		t.Fatalf("re-request of a mined transaction: %v", got)
	}
	storedSays("re-request refused on-chain", "failed with its transaction stored, and its last re-request was refused (its stored transaction is in the chain, so it was sent after all): "+
		"check the escrow wallet and the chain, because that transaction, or another spending the same coins, may have been sent", "can re-request it", "(Re-request settlement", "not seen mined")
	// A reorganisation and a flushed pool put it back to nowhere, as in TestXMREscrowReRequestGuards.
	x.fake.popBlocks(1)
	x.fake.flushPool(old.TxHash)

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
