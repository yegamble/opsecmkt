package market

import (
	"html"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-613 (PAY560-2): a Monero settlement that failed before any transaction was built is never re-requested, so the
// A-560 escrow staff alert it raised never cleared: the staff badge never returned to zero, the desk listed it for
// good and retention kept the order under escrow_alert. A refund too few (whose deposits are left out, flagged for
// payment review and returned by a stray refund) is no longer an alert, as Bitcoin raises none for a refund too few;
// any other unbuilt failure (nothing-to-settle) stays an alert until an administrator who is not a party marks it
// settled by hand on the order page (POST /admin/escrow-xmr-settled-by-hand, migration 125). DB integration against
// the simulated wallet RPC (not chain evidence).

// settledByHand posts the administrator's "settled by hand" of settlement id as session (password testPassword unless
// kv sets one), returning the status and the Location or body.
func (x *xmrEscrowEnv) settledByHand(session string, id int64, note string, kv ...string) (int, string) {
	x.t.Helper()
	w := x.do("POST", "/admin/escrow-xmr-settled-by-hand", session, form(append([]string{"settlement_id", strconv.FormatInt(id, 10), "note", note, "password", testPassword}, kv...)...))
	if w.Code == 303 {
		return w.Code, w.Header().Get("Location")
	}
	return w.Code, w.Body.String()
}

// settledByHandAudits counts the audit rows of marking a Monero settlement settled by hand (of actor, or of anyone for "").
func (x *xmrEscrowEnv) settledByHandAudits(actor string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE ($1='' OR user_id=$1) AND action LIKE 'Marked the failed Monero escrow settlement % settled by hand%'", actor)
}

// alerting reports whether order has an open escrow staff alert (escrowStaffAlert).
func (x *xmrEscrowEnv) alerting(order string) bool {
	x.t.Helper()
	var open bool
	if err := x.DB.QueryRow("SELECT "+escrowStaffAlert("$1"), order).Scan(&open); err != nil {
		x.t.Fatal(err)
	}
	return open
}

// escrowAlertBadge is the staff badge's count of escrow alerts for staff member uid.
func (x *xmrEscrowEnv) escrowAlertBadge(uid string) int {
	x.t.Helper()
	d := &PageData{User: &User{ID: uid, Role: "moderator"}}
	r := httptest.NewRequest("GET", "/", nil)
	if err := loadStaffAttention(r.Context(), x.A, r, d); err != nil || d.StaffAttention == nil {
		x.t.Fatalf("staff attention: %v", err)
	}
	return d.StaffAttention.EscrowAlerts
}

// retentionReason is retention's escrow blocker for order now.
func (x *xmrEscrowEnv) retentionReason(order string) string {
	x.t.Helper()
	tx, err := x.DB.Begin()
	if err != nil {
		x.t.Fatal(err)
	}
	defer tx.Rollback()
	reason, err := x.A.retentionEscrowBlocker(x.t.Context(), tx, retentionEscrowOrder{id: order})
	if err != nil {
		x.t.Fatal(err)
	}
	return reason
}

// A refund too few (A-437) is not an escrow alert: at no point, while its stray refund waits or once it confirms, does
// the desk list it, the badge count it or retention keep the order under escrow_alert. Its deposit is flagged and left
// out as before, so staff read the order and review the deposit under Payment review; nothing offers to mark the
// refund settled by hand, and the action refuses it.
func TestXMREscrowRefundTooFewNoStaffAlert(t *testing.T) {
	x := newXMREscrowEnv(t)
	modID, mod := x.user("tfmod_"+randomToken()[:6], "moderator")
	_, admin := x.user("tfadmin_"+randomToken()[:6], "admin")
	x.mustPoll()
	tiny := int64(minPayoutXMR / 2)
	order, addr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(addr, hex64("too few "+order), tiny, 20)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.mustPoll()
	if x.state(order) != stateCancelled {
		t.Fatalf("underpaid: %s", x.state(order))
	}
	x.mustPoll()
	refund := x.settlementOf(order, "refund")
	if refund.State != "failed" || x.waitClass(refund.ID) != xmrTooSmall || refund.TxHash != "" {
		t.Fatalf("the refund: %+v %q", refund, x.waitClass(refund.ID))
	}
	reviewed := func(stage string) {
		t.Helper()
		if got := agStrings(t, x.testEnv, "SELECT flagged||' '||escrow_left_out FROM payments WHERE order_id=$1 AND NOT after_close", order); !slices.Equal(got, []string{"true true"}) {
			t.Fatalf("%s: the deposit: %v", stage, got)
		}
		d := &PageData{User: &User{ID: modID, Role: "moderator"}}
		r := httptest.NewRequest("GET", "/", nil)
		if err := loadPaymentReviews(r.Context(), x.A, r, d); err != nil {
			t.Fatal(err)
		}
		var listed []bool
		for _, v := range d.PaymentReviews {
			if v.OrderID == order && v.TxID == hex64("too few "+order) {
				listed = append(listed, v.Open && v.Form)
			}
		}
		if !slices.Equal(listed, []bool{true}) {
			t.Fatalf("%s: payment review of the deposit left out (open with its form): %v", stage, listed)
		}
		if code := x.do("GET", "/order?id="+order, mod, nil).Code; code != 200 {
			t.Fatalf("%s: staff cannot read the order with a flagged deposit: %d", stage, code)
		}
	}
	noAlert := func(stage string) {
		t.Helper()
		if x.alerting(order) {
			t.Errorf("%s: the refund too few is an escrow alert", stage)
		}
		if e := deskAlertIn(html.UnescapeString(x.page("/moderator", mod)), order); e != "" {
			t.Errorf("%s: the desk lists it: %q", stage, e)
		}
		if n := x.escrowAlertBadge(modID); n != 0 {
			t.Errorf("%s: the badge counts %d escrow alerts", stage, n)
		}
		if got := x.retentionReason(order); got == "escrow_alert" {
			t.Errorf("%s: retention keeps the order as an escrow alert", stage)
		}
		page := html.UnescapeString(x.page("/order?id="+order, admin))
		mustNotContain(t, staffHelp(page), "open staff alert")
		mustNotContain(t, page, `action="/admin/escrow-xmr-settled-by-hand"`)
	}
	reviewed("refund ended")
	noAlert("refund ended")
	if code, body := x.settledByHand(admin, refund.ID, "Settled by hand with the operator."); code != 409 {
		t.Fatalf("settled by hand on a refund too few: %d %s", code, body)
	}

	// The stray refund waits (too few on their own), then a later deposit lets it return both, and it confirms.
	x.mustPoll()
	x.mustPoll()
	stray := x.settlementOf(order, "stray_refund")
	if stray.State != "requested" || x.waitClass(stray.ID) != xmrTooSmall {
		t.Fatalf("the stray refund: %+v %q", stray, x.waitClass(stray.ID))
	}
	noAlert("stray refund waiting")
	x.fake.payAddress(addr, hex64("later "+order), 4*minPayoutXMR, 20)
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(stray.ID); s.State != "relayed" {
		t.Fatalf("the stray refund was not sent: %+v %q", s, x.waitClass(s.ID))
	}
	x.fake.mine(10)
	x.mustPoll()
	if s := x.settlement(stray.ID); s.State != "confirmed" {
		t.Fatalf("the stray refund did not confirm: %+v", s)
	}
	reviewed("stray refund confirmed")
	noAlert("stray refund confirmed")
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled"); n != 0 {
		t.Fatalf("%d settled-by-hand records", n)
	}
}

// An unbuilt failure other than a refund too few (here nothing-to-settle) alerts until an administrator who is not a
// party marks it settled by hand: the desk entry and the staff help name the action, only that administrator is offered
// its form, and every refusal writes nothing. Recorded once (audit row, no order event, no party notice), the desk,
// the badge, the order-page help and retention's escrow_alert all clear; a repeat is a clean 409. The parties never
// see any of it.
func TestXMREscrowSettledByHand(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("sbhmod_"+randomToken()[:6], "moderator")
	adminID, admin := x.user("sbhadmin_"+randomToken()[:6], "admin")
	_, wrongPw := x.user("sbhwrong_"+randomToken()[:6], "admin") // spends its own confirmation budget only

	order, _, _ := x.completed()
	id, _ := x.release(order)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", id, xmrNothing)
	stored, storedID := x.failedRelease()
	sent, _, _ := x.completed()
	sentID, _ := x.release(sent)
	x.mustPoll()
	if x.settlement(sentID).State != "relayed" || x.settlement(id).State != "failed" {
		t.Fatalf("fixtures: sent %s, unbuilt %s", x.settlement(sentID).State, x.settlement(id).State)
	}
	buyerID := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
	parties := []string{x.sessOf(order, roleBuyer), x.sessOf(order, roleVendor)}
	n := strconv.FormatInt(id, 10)
	text := "Monero escrow settlement " + n + " failed before any transaction was built, so it cannot be re-requested: read why under the settlement on the order page, " +
		"settle anything still owed by hand with the operator, then an administrator who is not a party marks it settled on the order page (Mark settlement " + n + " settled by hand)"
	formAction := `action="/admin/escrow-xmr-settled-by-hand"`
	if !x.alerting(order) || x.retentionReason(order) != "escrow_alert" {
		t.Fatalf("before: alert %v, retention %q", x.alerting(order), x.retentionReason(order))
	}
	if got := deskAlertIn(html.UnescapeString(x.page("/moderator", mod)), order); !strings.Contains(got, text) {
		t.Fatalf("desk alert:\n got %q\nwant %q", got, text)
	}
	adminPage := html.UnescapeString(x.page("/order?id="+order, admin))
	if help := staffHelp(adminPage); !strings.Contains(help, "because its escrow has an open staff alert: "+text+".") {
		t.Fatalf("the administrator's staff help: %q", help)
	}
	mustContain(t, adminPage, formAction, `name="settlement_id" value="`+n+`"`, `name="note"`, `name="password"`, "Mark settlement "+n+" settled by hand")
	mustContain(t, staffHelp(adminPage), `href="#xmr-settlement-`+n+`"`)
	modPage := html.UnescapeString(x.page("/order?id="+order, mod))
	mustNotContain(t, modPage, formAction)
	for _, sess := range parties {
		mustNotContain(t, withoutSiteHeader(html.UnescapeString(x.page("/order?id="+order, sess))), formAction, "settled by hand", "open staff alert", text)
	}
	// A failed settlement with a stored transaction gets no such form (a relayed one raises no alert, so staff cannot
	// open its order at all).
	mustNotContain(t, html.UnescapeString(x.page("/order?id="+stored, admin)), formAction)
	// With Monero escrow off the form is still offered, as the refused-deposit disposition is (it needs no wallet).
	saved := x.A.escrowXMR
	x.A.escrowXMR = nil
	mustContain(t, html.UnescapeString(x.page("/order?id="+order, admin)), formAction)
	x.A.escrowXMR = saved

	note := "Paid the vendor by hand with the operator in " + hex64("hand release") + "."
	events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
	partyNotices := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2) AND (order_id=$3 OR strpos(body,$4)>0)", buyerID, x.vendor, order, shortID(order))
	for _, c := range []struct {
		name, sess string
		id         int64
		note       string
		kv         []string
		code       int
	}{
		{"a moderator", mod, id, note, nil, 403},
		{"the buyer", parties[0], id, note, nil, 403},
		{"the vendor", parties[1], id, note, nil, 403},
		{"an empty note", admin, id, "", nil, 400},
		{"a short note", admin, id, "too short", nil, 400},
		{"a long note", admin, id, strings.Repeat("x", 501), nil, 400},
		{"a wrong password", wrongPw, id, note, []string{"password", "wrong-password-123"}, 401},
		{"an unknown settlement", admin, 999999, note, nil, 404},
		{"a settlement with a stored transaction", admin, storedID, note, nil, 409},
		{"a relayed settlement", admin, sentID, note, nil, 409},
	} {
		if code, body := x.settledByHand(c.sess, c.id, c.note, c.kv...); code != c.code {
			t.Fatalf("%s: %d, want %d: %s", c.name, code, c.code, body)
		}
	}
	// An administrator who is a party to the order (here its vendor) is refused too.
	agExec(x.testEnv, "UPDATE users SET role='admin' WHERE id=$1", x.vendor)
	if code, body := x.settledByHand(parties[1], id, note); code != 403 || !strings.Contains(body, "party") {
		t.Fatalf("an administrator party: %d %s", code, body)
	}
	agExec(x.testEnv, "UPDATE users SET role='vendor' WHERE id=$1", x.vendor)
	// A superseded settlement (the stored one, once re-requested) is refused.
	if code := x.reRequest(admin, stored, storedID, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if x.settlement(storedID).State != "superseded" {
		t.Fatalf("not superseded: %s", x.settlement(storedID).State)
	}
	if code, body := x.settledByHand(admin, storedID, note); code != 409 {
		t.Fatalf("a superseded settlement: %d %s", code, body)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled"); n != 0 || x.settledByHandAudits("") != 0 || !x.alerting(order) {
		t.Fatalf("refusals wrote %d records, %d audit rows; alert %v", n, x.settledByHandAudits(""), x.alerting(order))
	}

	badge := x.escrowAlertBadge(modID)
	if badge != 1 {
		t.Fatalf("the badge counts %d escrow alerts before the record, want 1 (this order's)", badge)
	}
	if code, loc := x.settledByHand(admin, id, "  "+note+"  "); code != 303 || !strings.HasPrefix(loc, "/moderator") {
		t.Fatalf("settled by hand: %d %s", code, loc)
	}
	if got := agStrings(t, x.testEnv, "SELECT note||'|'||settled_by||'|'||(settled_at IS NOT NULL) FROM xmr_settlement_hand_settled WHERE settlement_id=$1", id); !slices.Equal(got, []string{note + "|" + adminID + "|true"}) {
		t.Fatalf("the record: %q", got)
	}
	if got := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", adminID)[0]; x.settledByHandAudits(adminID) != 1 ||
		!strings.Contains(got, "settlement "+n+" (") || !strings.Contains(got, shortID(order)) || !strings.Contains(got, "(confirmed with password)") {
		t.Fatalf("the audit row (%d): %q", x.settledByHandAudits(adminID), got)
	}
	if e, p := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order), agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2) AND (order_id=$3 OR strpos(body,$4)>0)", buyerID, x.vendor, order, shortID(order)); e != events || p != partyNotices {
		t.Fatalf("order events %d (was %d), party notices %d (was %d)", e, events, p, partyNotices)
	}
	if x.alerting(order) {
		t.Fatal("still an escrow alert")
	}
	if e := deskAlertIn(html.UnescapeString(x.page("/moderator", mod)), order); e != "" {
		t.Fatalf("the desk still lists it: %q", e)
	}
	if got := x.escrowAlertBadge(modID); got != badge-1 {
		t.Fatalf("the badge counts %d escrow alerts, want %d", got, badge-1)
	}
	if got := x.retentionReason(order); got == "escrow_alert" {
		t.Fatalf("retention still keeps the order as an escrow alert")
	}
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{404, 404}) {
		t.Fatalf("staff still read the order: %v", got)
	}
	if code, body := x.settledByHand(admin, id, "A second record, long enough."); code != 409 || !strings.Contains(body, "already") || x.settledByHandAudits("") != 1 {
		t.Fatalf("a repeat: %d %s (%d audit rows)", code, body, x.settledByHandAudits(""))
	}
	// Migration 125 keeps the record as written, and accepts none for a settlement the action refuses.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE xmr_settlement_hand_settled SET note='Rewritten note, long enough.' WHERE settlement_id=$1", []any{id}},
		{"DELETE FROM xmr_settlement_hand_settled WHERE settlement_id=$1", []any{id}},
		{"INSERT INTO xmr_settlement_hand_settled(settlement_id,note,settled_by) VALUES($1,'Direct insert, long enough.',$2)", []any{storedID, adminID}},
		{"INSERT INTO xmr_settlement_hand_settled(settlement_id,note,settled_by) VALUES($1,'Direct insert, long enough.',$2)", []any{sentID, adminID}},
	} {
		if _, err := x.DB.Exec(q.sql, q.args...); err == nil || !strings.Contains(err.Error(), "xmr_settlement_hand_settled") {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	// Staff who read the order for another reason (a dispute) see the record, with no form; the parties see none of it.
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parties referred this order for staff review.')", randomToken(), order)
	adminPage = html.UnescapeString(x.page("/order?id="+order, admin))
	mustContain(t, adminPage, "Marked settled by hand", note)
	mustNotContain(t, adminPage, formAction)
	for _, sess := range parties {
		mustNotContain(t, withoutSiteHeader(html.UnescapeString(x.page("/order?id="+order, sess))), formAction, "settled by hand", note)
	}
}

// Three administrators mark one settlement settled by hand at once: one record and one audit row, the others 409.
func TestXMREscrowSettledByHandConcurrent(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", id, xmrNothing)
	var sessions []string
	for i := range 3 {
		_, s := x.user("sbhc"+strconv.Itoa(i)+"_"+randomToken()[:6], "admin")
		sessions = append(sessions, s)
	}
	var wg sync.WaitGroup
	codes, bodies := make([]int, 3), make([]string, 3)
	start := make(chan struct{})
	for i, sess := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], bodies[i] = x.settledByHand(sess, id, "Concurrent record, note long enough.")
		}()
	}
	close(start)
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{303, 409, 409}) {
		t.Fatalf("concurrent records: %v %q", codes, bodies)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled"); n != 1 || x.settledByHandAudits("") != 1 {
		t.Fatalf("%d records, %d audit rows", n, x.settledByHandAudits(""))
	}
	if x.alerting(order) {
		t.Fatal("still an escrow alert")
	}
}

// An exact retry of a completed form (the same form nonce) returns the saved response and records nothing more; the
// same nonce with a changed note is refused, as the refused-deposit disposition's is.
func TestXMREscrowSettledByHandReplay(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("sbhr_"+randomToken()[:6], "admin")
	order, _, _ := x.completed()
	id, _ := x.release(order)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", id, xmrNothing)
	path := "/admin/escrow-xmr-settled-by-hand"
	nonce := renderedFormNonce(t, x.page("/order?id="+order, admin), path)
	f := form("settlement_id", strconv.FormatInt(id, 10), "note", "Nothing was owed: checked with the operator.", "password", testPassword, formNonceField, nonce)
	first := x.do("POST", path, admin, f)
	x.check(first, 303)
	replay := x.do("POST", path, admin, f)
	x.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") || x.settledByHandAudits(adminID) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled") != 1 {
		t.Fatalf("the replay: %q vs %q, %d audit rows", replay.Header().Get("Location"), first.Header().Get("Location"), x.settledByHandAudits(adminID))
	}
	f.Set("note", "A different note, long enough.")
	x.check(x.do("POST", path, admin, f), 409)
	if x.settledByHandAudits(adminID) != 1 {
		t.Fatal("the changed replay wrote an audit row")
	}
}
