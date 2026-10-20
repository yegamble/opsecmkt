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
// any other unbuilt failure (nothing-to-settle) stays an alert until an administrator who is not a party dismisses it as
// nothing owed on the order page (POST /admin/escrow-xmr-settled-by-hand, migration 128; never paid by hand, PAY613-1),
// and a stored-transaction failure whose re-request was refused for good is marked settled by hand there. DB
// integration against the simulated wallet RPC (not chain evidence).

// settledByHand posts the administrator's dismissal or "settled by hand" of settlement id as session (password
// testPassword unless kv sets one), returning the status and the Location or body.
func (x *xmrEscrowEnv) settledByHand(session string, id int64, note string, kv ...string) (int, string) {
	x.t.Helper()
	w := x.do("POST", "/admin/escrow-xmr-settled-by-hand", session, form(append([]string{"settlement_id", strconv.FormatInt(id, 10), "note", note, "password", testPassword}, kv...)...))
	if w.Code == 303 {
		return w.Code, w.Header().Get("Location")
	}
	return w.Code, w.Body.String()
}

// settledByHandAudits counts the audit rows of dismissing a Monero settlement or marking one settled by hand (of actor,
// or of anyone for "").
func (x *xmrEscrowEnv) settledByHandAudits(actor string) int {
	return agInt(x.testEnv, `SELECT count(*) FROM audit_events WHERE ($1='' OR user_id=$1) AND (action LIKE 'Marked the failed Monero escrow settlement % settled by hand%'
		OR action LIKE 'Dismissed the failed Monero escrow settlement %: nothing owed now%')`, actor)
}

// unbuiltAlert is the escrow alert text of settlement n that failed before any transaction was built and claims no deposit (PAY613-1).
func unbuiltAlert(n string) string {
	return "Monero escrow settlement " + n + " failed before any transaction was built because no deposit counted at that time, so nothing was sent and it cannot be re-requested: " +
		"read why under the settlement on the order page. Do not pay anything by hand for it: if a deposit of this escrow is listed again, the market handles it automatically as usual " +
		"(a stray refund returns it when the order's coins go back to the buyer) and it is flagged for review or raises its own alert. " +
		"An administrator who is not a party dismisses it on the order page (Dismiss settlement " + n + " (nothing owed now))"
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
// party dismisses it as nothing owed: the desk entry, the staff help and the form say nothing was sent and must not be
// paid by hand (PAY613-1), only that administrator is offered the form, and every refusal writes nothing. Recorded once
// as nothing-owed (audit row, no order event, no party notice), the desk, the badge, the order-page help and
// retention's escrow_alert all clear; a repeat is a clean 409. The parties never see any of it.
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
	text := unbuiltAlert(n)
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
	mustContain(t, adminPage, formAction, `name="settlement_id" value="`+n+`"`, `name="note"`, `name="password"`, "Dismiss settlement "+n+" (nothing owed now)",
		"so nothing was sent and nothing is owed for it now. Do not pay anything by hand for it.", "dismissing this settlement does not stop that", "Why nothing is owed now")
	mustNotContain(t, adminPage, "settle anything still owed", "Settle anything it still owed", "Mark settlement "+n+" settled by hand")
	mustContain(t, staffHelp(adminPage), `href="#xmr-settlement-`+n+`"`, "As an administrator, act on a Monero escrow settlement (dismiss it as nothing owed) under")
	modPage := html.UnescapeString(x.page("/order?id="+order, mod))
	mustNotContain(t, modPage, formAction)
	for _, sess := range parties {
		mustNotContain(t, withoutSiteHeader(html.UnescapeString(x.page("/order?id="+order, sess))), formAction, "settled by hand", "Dismiss", "open staff alert", text)
	}
	// A failed settlement with a stored transaction gets no such form (a relayed one raises no alert, so staff cannot
	// open its order at all).
	mustNotContain(t, html.UnescapeString(x.page("/order?id="+stored, admin)), formAction)
	// With Monero escrow off the form is still offered, as the refused-deposit disposition is (it needs no wallet).
	saved := x.A.escrowXMR
	x.A.escrowXMR = nil
	mustContain(t, html.UnescapeString(x.page("/order?id="+order, admin)), formAction)
	x.A.escrowXMR = saved

	note := "No deposit counted when it was built; nothing is owed now, checked with the operator."
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
	if got := agStrings(t, x.testEnv, "SELECT kind||'|'||note||'|'||settled_by||'|'||(settled_at IS NOT NULL) FROM xmr_settlement_hand_settled WHERE settlement_id=$1", id); !slices.Equal(got, []string{xmrNothingOwed + "|" + note + "|" + adminID + "|true"}) {
		t.Fatalf("the record: %q", got)
	}
	if got := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", adminID)[0]; x.settledByHandAudits(adminID) != 1 ||
		!strings.HasPrefix(got, "Dismissed the failed Monero escrow settlement "+n+" (") || !strings.Contains(got, shortID(order)+": nothing owed now") || !strings.Contains(got, "(confirmed with password)") {
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
	// Migration 128 keeps the record as written, and accepts none for a settlement the action refuses.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE xmr_settlement_hand_settled SET note='Rewritten note, long enough.' WHERE settlement_id=$1", []any{id}},
		{"DELETE FROM xmr_settlement_hand_settled WHERE settlement_id=$1", []any{id}},
		{"INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'settled-by-hand','Direct insert, long enough.',$2)", []any{storedID, adminID}},
		{"INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'settled-by-hand','Direct insert, long enough.',$2)", []any{sentID, adminID}},
		{"INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'nothing-owed','Direct insert, long enough.',$2)", []any{sentID, adminID}},
	} {
		if _, err := x.DB.Exec(q.sql, q.args...); err == nil || !strings.Contains(err.Error(), "is never changed") && !strings.Contains(err.Error(), "is recorded") {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	// Staff who read the order for another reason (a dispute) see the record, with no form; the parties see none of it.
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parties referred this order for staff review.')", randomToken(), order)
	adminPage = html.UnescapeString(x.page("/order?id="+order, admin))
	mustContain(t, adminPage, "Dismissed (nothing owed now)", note)
	mustNotContain(t, adminPage, formAction, "Marked settled by hand")
	for _, sess := range parties {
		mustNotContain(t, withoutSiteHeader(html.UnescapeString(x.page("/order?id="+order, sess))), formAction, "settled by hand", "Dismissed", note)
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

// A failed settlement with a stored transaction is re-requested, not marked settled by hand: refused (409) before any
// re-request and while one waits. Its transaction relayed after all and mined, the re-request is refused (on-chain) and
// the settlement can never be superseded: the alert tells staff to check the wallet and the chain and then mark it
// settled by hand, which only a non-party administrator can. Recorded, the alert, the badge, the staff help and
// retention's escrow_alert clear, its payment-review reason says so, and it is never re-requested again. With Monero
// escrow off the staff help names only the action that works then.
func TestXMREscrowSettledByHandRefusedReRequest(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("sbhrr_mod_"+randomToken()[:6], "moderator")
	adminID, admin := x.user("sbhrr_admin_"+randomToken()[:6], "admin")
	order, id := x.failedRelease()
	n := strconv.FormatInt(id, 10)
	formAction := `action="/admin/escrow-xmr-settled-by-hand"`
	note := "Checked the wallet and the chain: the release was mined, nothing more is owed."
	reviewReason := func() string {
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

	// No re-request yet: it is re-requested first, and no form offers otherwise.
	if code, body := x.settledByHand(admin, id, note); code != 409 || !strings.Contains(body, "Re-request a failed settlement with a stored transaction first") {
		t.Fatalf("no re-request yet: %d %.300s", code, body)
	}
	mustNotContain(t, html.UnescapeString(x.page("/order?id="+order, admin)), formAction)
	old := x.settlement(id)
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	x.fake.mine(1)
	if code := x.reRequest(admin, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	if code, body := x.settledByHand(admin, id, note); code != 409 || !strings.Contains(body, "already waiting") {
		t.Fatalf("a re-request waiting: %d %.300s", code, body)
	}
	mustNotContain(t, html.UnescapeString(x.page("/order?id="+order, admin)), formAction)
	x.mustPoll()
	if got := x.reRequests(id); len(got) != 1 || got[0] != "refused:"+xmrReRequestOnChain+":0" {
		t.Fatalf("re-request of a mined transaction: %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled"); n != 0 || x.settledByHandAudits("") != 0 {
		t.Fatalf("refusals wrote %d records, %d audit rows", n, x.settledByHandAudits(""))
	}

	text := "Monero escrow settlement " + n + " failed with its transaction stored, and its last re-request was refused (its stored transaction is in the chain, so it was sent after all): " +
		"check the escrow wallet and the chain, because that transaction, or another spending the same coins, may have been sent; once checked, mark it settled by hand (Mark settlement " + n + " settled by hand)"
	if got := deskAlertIn(html.UnescapeString(x.page("/moderator", mod)), order); !strings.Contains(got, text) {
		t.Fatalf("desk alert:\n got %q\nwant %q", got, text)
	}
	if !x.alerting(order) || x.escrowAlertBadge(modID) != 1 || x.retentionReason(order) != "escrow_alert" {
		t.Fatalf("before: alert %v, badge %d, retention %q", x.alerting(order), x.escrowAlertBadge(modID), x.retentionReason(order))
	}
	adminPage := html.UnescapeString(x.page("/order?id="+order, admin))
	mustContain(t, staffHelp(adminPage), "because its escrow has an open staff alert: "+text+".", `href="#xmr-settlement-`+n+`"`,
		"As an administrator, act on a Monero escrow settlement (re-request it or mark it settled by hand) under")
	mustContain(t, adminPage, formAction, `name="settlement_id" value="`+n+`"`, "Mark settlement "+n+" settled by hand",
		"may have been sent: check the escrow wallet and the chain first", `action="/admin/escrow-xmr-rerequest"`)
	mustNotContain(t, html.UnescapeString(x.page("/order?id="+order, mod)), formAction)
	for _, sess := range []string{x.sessOf(order, roleBuyer), x.sessOf(order, roleVendor)} {
		mustNotContain(t, withoutSiteHeader(html.UnescapeString(x.page("/order?id="+order, sess))), formAction, "settled by hand", "open staff alert")
	}
	if got := reviewReason(); !strings.Contains(got, "once checked, mark it settled by hand (Mark settlement "+n+" settled by hand)") {
		t.Fatalf("payment-review reason: %q", got)
	}
	// With Monero escrow off the re-request is unavailable: the staff help and the closed-order line name only what
	// works, and the form is still offered.
	saved := x.A.escrowXMR
	x.A.escrowXMR = nil
	off := html.UnescapeString(x.page("/order?id="+order, admin))
	x.A.escrowXMR = saved
	mustContain(t, off, formAction, "This order is closed. An administrator can still act on a Monero escrow settlement (mark it settled by hand) on this page.")
	mustContain(t, staffHelp(off), "As an administrator, act on a Monero escrow settlement (mark it settled by hand) under")
	mustNotContain(t, staffHelp(off), "re-request it or", "build a Monero escrow settlement again")
	mustNotContain(t, off, `action="/admin/escrow-xmr-rerequest"`)

	if code, _ := x.settledByHand(mod, id, note); code != 403 {
		t.Fatalf("a moderator: %d", code)
	}
	if code, loc := x.settledByHand(admin, id, note); code != 303 || !strings.HasPrefix(loc, "/moderator") {
		t.Fatalf("settled by hand: %d %s", code, loc)
	}
	if got := agStrings(t, x.testEnv, "SELECT note||'|'||settled_by FROM xmr_settlement_hand_settled WHERE settlement_id=$1", id); !slices.Equal(got, []string{note + "|" + adminID}) || x.settledByHandAudits(adminID) != 1 {
		t.Fatalf("the record %q, %d audit rows", got, x.settledByHandAudits(adminID))
	}
	if x.alerting(order) || x.escrowAlertBadge(modID) != 0 || x.retentionReason(order) == "escrow_alert" {
		t.Fatalf("after: alert %v, badge %d, retention %q", x.alerting(order), x.escrowAlertBadge(modID), x.retentionReason(order))
	}
	if e := deskAlertIn(html.UnescapeString(x.page("/moderator", mod)), order); e != "" {
		t.Fatalf("the desk still lists it: %q", e)
	}
	// Its deposit stays flagged, so staff still read the order: no alert help, the record, and neither form.
	adminPage = html.UnescapeString(x.page("/order?id="+order, admin))
	mustNotContain(t, staffHelp(adminPage), "open staff alert")
	mustContain(t, adminPage, "Marked settled by hand", note)
	mustNotContain(t, adminPage, formAction, `action="/admin/escrow-xmr-rerequest"`)
	if got := reviewReason(); !strings.Contains(got, "an administrator marked it settled by hand after its re-request was refused") || strings.Contains(got, "once checked") {
		t.Fatalf("payment-review reason after: %q", got)
	}
	// Never re-requested again: the action refuses it, and so does migration 128.
	if code := x.reRequest(admin, order, id, testPassword); code != 409 {
		t.Fatalf("re-request after settled by hand: %d", code)
	}
	if _, err := x.DB.Exec("INSERT INTO xmr_settlement_rerequests(settlement_id,requested_by) VALUES($1,$2)", id, adminID); err == nil || !strings.Contains(err.Error(), "settled by hand") {
		t.Fatalf("a direct re-request row: %v", err)
	}
	if got := x.reRequests(id); len(got) != 1 {
		t.Fatalf("re-requests: %v", got)
	}
}

// PAY613-1, the reviewer's probe: a stray refund fails nothing-to-settle because the deposit it was requested for is
// missing (the wallet reads it as double-spent), and an administrator dismisses it as nothing owed. The deposit is
// then listed again: the market requests and sends exactly one stray refund of it automatically, as before A-613, and
// flags the deposit for review; the dismissal does not block that, and nothing told staff to pay it by hand.
func TestXMREscrowDismissedDepositReturnsAutomatically(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("sbhd_admin_"+randomToken()[:6], "admin")
	order, addr := x.settled()
	late := hex64("late " + order)
	strays := func() []string {
		return agStrings(t, x.testEnv, "SELECT id||' '||state||' '||send_error FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id", order)
	}
	x.fake.payAddress(addr, late, 4*minPayoutXMR, 0)
	x.mustPoll()
	x.fake.setTransfer(late, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	// The stray refund the watcher requests for it, here written directly: the window between its request and its
	// build is too short to place the double spend in from a test.
	var id int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1 RETURNING id", order).Scan(&id); err != nil {
		t.Fatal(err)
	}
	x.mustPoll()
	n := strconv.FormatInt(id, 10)
	if got := strays(); !slices.Equal(got, []string{n + " failed " + xmrNothing}) {
		t.Fatalf("the stray refund: %v", got)
	}
	if got := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order); !strings.Contains(got, unbuiltAlert(n)) {
		t.Fatalf("desk alert: %q", got)
	}
	page := html.UnescapeString(x.page("/order?id="+order, admin))
	mustContain(t, page, "Do not pay anything by hand for it.", "Dismiss settlement "+n+" (nothing owed now)")
	mustNotContain(t, page, "Settle anything it still owed", "settle anything still owed")
	if code, loc := x.settledByHand(admin, id, "The deposit was double-spent; nothing is owed now."); code != 303 || !strings.HasPrefix(loc, "/moderator") {
		t.Fatalf("dismiss: %d %s", code, loc)
	}
	if x.alerting(order) || agStr(x.testEnv, "SELECT kind FROM xmr_settlement_hand_settled WHERE settlement_id=$1", id) != xmrNothingOwed {
		t.Fatalf("after the dismissal: alert %v", x.alerting(order))
	}

	// Listed again: one automatic stray refund to the buyer's refund address, and the deposit flagged for review.
	x.fake.setTransfer(late, func(t *fakeXMRTransfer) { t.DoubleSpend, t.Confs, t.Pool = false, 20, false })
	x.mustPoll()
	x.mustPoll()
	got := strays()
	if len(got) != 2 || got[0] != n+" failed "+xmrNothing || !strings.HasSuffix(got[1], " relayed ") {
		t.Fatalf("stray refunds after the deposit came back: %v", got)
	}
	next := x.settlementOf(order, "stray_refund")
	refund := agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
	if outs := x.outputsOf(next.ID); len(outs) != 1 || !strings.HasPrefix(outs[0], "claimant:"+refund+":") {
		t.Fatalf("the stray refund's outputs: %v", outs)
	}
	if got := agStrings(t, x.testEnv, "SELECT flagged||' '||(xmr_settlement_id=$2) FROM payments WHERE order_id=$1 AND txid=$3", order, next.ID, late); !slices.Equal(got, []string{"true true"}) {
		t.Fatalf("the deposit: %v", got)
	}
}

// PAY613-3: a stored-transaction failure is marked settled by hand only once a re-request was refused for good. Refused
// in-pool (the transaction may still be mined or dropped), the action refuses it (409, telling staff to re-request it
// again later), no form is offered, the alert says to re-request it later, and migration 128 refuses the row; refused
// key-image-spent (a coin it spends is spent elsewhere), it is accepted and the alert clears.
func TestXMREscrowSettledByHandFinalRefusalsOnly(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("sbhf_admin_"+randomToken()[:6], "admin")
	order, id := x.failedRelease()
	n := strconv.FormatInt(id, 10)
	formAction := `action="/admin/escrow-xmr-settled-by-hand"`
	note := "Checked the wallet and the chain: its coins went elsewhere; settled with the operator."
	old := x.settlement(id)
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	if code := x.reRequest(admin, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if got := x.reRequests(id); !slices.Equal(got, []string{"refused:" + xmrReRequestInPool + ":0"}) {
		t.Fatalf("re-requests: %v", got)
	}
	if code, body := x.settledByHand(admin, id, note); code != 409 || !strings.Contains(body, "Re-request it again later") {
		t.Fatalf("refused in-pool: %d %.300s", code, body)
	}
	page := html.UnescapeString(x.page("/order?id="+order, admin))
	mustNotContain(t, page, formAction)
	mustContain(t, page, `action="/admin/escrow-xmr-rerequest"`)
	if got := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order); !strings.Contains(got, "re-request it again later (Re-request settlement "+n+")") || strings.Contains(got, "Mark settlement") {
		t.Fatalf("in-pool desk alert: %q", got)
	}
	if _, err := x.DB.Exec("INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'settled-by-hand','Direct insert, long enough.',$2)", id, adminID); err == nil {
		t.Fatal("migration 128 accepted a record after an in-pool refusal")
	}

	// Out of the pool, and a coin it spends spent by another transaction: refused key-image-spent, which is final.
	x.fake.flushPool(old.TxHash)
	outside := x.fake.spendOutside(old.KeyImages...)
	if code := x.reRequest(admin, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	x.fake.flushPool(outside)
	if got := x.reRequests(id); len(got) != 2 || got[1] != "refused:"+xmrReRequestSpent+":0" {
		t.Fatalf("re-requests: %v", got)
	}
	mustContain(t, html.UnescapeString(x.page("/order?id="+order, admin)), formAction, "Mark settlement "+n+" settled by hand")
	if code, loc := x.settledByHand(admin, id, note); code != 303 || !strings.HasPrefix(loc, "/moderator") {
		t.Fatalf("settled by hand after key-image-spent: %d %s", code, loc)
	}
	if x.alerting(order) || agStr(x.testEnv, "SELECT kind FROM xmr_settlement_hand_settled WHERE settlement_id=$1", id) != xmrSettledByHand {
		t.Fatalf("after: alert %v", x.alerting(order))
	}
}

// strandedAlert is the escrow alert text of settlement n that failed before any transaction was built while it still
// claims deposits (PAY613-4).
func strandedAlert(n string) string {
	return "Monero escrow settlement " + n + " failed before any transaction was built while it still claims deposits of this escrow: their coins are in the escrow wallet and may still be owed, " +
		"and the market does not settle them on its own. Settle them with the operator; it cannot be dismissed while it claims them"
}

// reviewReasons lists the payment-review reasons of order's flagged deposits, sorted, as an administrator's desk reads them.
func (x *xmrEscrowEnv) reviewReasons(order string) []string {
	x.t.Helper()
	d := &PageData{User: &User{ID: x.admin, Role: "admin"}}
	r := httptest.NewRequest("GET", "/", nil)
	if err := loadPaymentReviews(r.Context(), x.A, r, d); err != nil {
		x.t.Fatal(err)
	}
	var reasons []string
	for _, v := range d.PaymentReviews {
		if v.OrderID == order {
			reasons = append(reasons, v.Reason)
		}
	}
	slices.Sort(reasons)
	return reasons
}

// waitingClaim is the clause the payment-review reason of a deposit claimed by re-request n of settlement m, still
// requested, ends with (A-617): with the wait when the deposit is missing and n waits for it.
func waitingClaim(n, m int64, missing bool) string {
	s := "; Monero escrow settlement " + strconv.FormatInt(n, 10) + ", which re-requests settlement " + strconv.FormatInt(m, 10) + ", claims it"
	if missing {
		s += " and waits: the escrow wallet no longer lists this deposit, and the market sends settlement " + strconv.FormatInt(n, 10) + " on its own once it is listed again"
	}
	return s + ". Do not pay anything by hand for it, or it is paid twice"
}

// waitingAlert is the desk's line for a re-request n of settlement m requested for more than 24 hours while it waits
// for a missing deposit (A-617).
func waitingAlert(n, m int64) string {
	return "Monero escrow settlement has been requested for more than 24 hours: settlement " + strconv.FormatInt(n, 10) + ", which re-requests settlement " + strconv.FormatInt(m, 10) +
		", waits because the escrow wallet no longer lists a deposit it spends, and the market sends it on its own once that deposit is listed again. Do not pay anything by hand for it, or it is paid twice"
}

// supersededWaiting is the PAY613-4 probe up to its double spend: a release that failed relay-refused is re-requested,
// the new settlement's first build is refused so it stays requested while it claims the deposit, and the wallet then
// reads that deposit as double-spent. It returns the order, the deposit, and the failed and new settlements.
func (x *xmrEscrowEnv) supersededWaiting(admin string) (order, deposit string, first, second int64) {
	x.t.Helper()
	order, first = x.failedRelease()
	deposit = hex64("deposit " + order)
	restore := x.refuseTransfer(-4, "transaction was rejected by daemon")
	if code := x.reRequest(admin, order, first, testPassword); code != 303 {
		x.t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	restore()
	if x.settlement(first).State != "superseded" {
		x.t.Fatalf("not superseded: %s", x.settlement(first).State)
	}
	second = int64(agInt(x.testEnv, "SELECT id FROM xmr_settlements WHERE supersedes=$1", first))
	x.fake.setTransfer(deposit, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	return order, deposit, first, second
}

// A-614 (PAY613-4/-5), the reviewer's probe: a re-request supersedes a release that failed relay-refused; the new
// settlement claims the deposit, its first build is refused, and the deposit is then read as double-spent. The new
// settlement no longer fails nothing-to-settle while it claims that deposit (which left an alert nothing could clear):
// it stays requested and waits (deposit-missing), builds nothing, staff are told once, and no "nothing owed" dismissal
// or stranded alert appears. It never builds from other coins, however long it waits (a re-request spends exactly the
// stored key images, so A-554's bounded wait has nothing to build from: it waits as a release whose credited deposits
// are all missing does). Once the wallet lists the deposit again it is built from exactly the stored key images to the
// stored plan, and relayed.
func TestXMREscrowReRequestWaitsForMissingDeposit(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a614_admin_"+randomToken()[:6], "admin")
	transfers := x.fake.countOf("transfer")
	order, deposit, first, second := x.supersededWaiting(admin)
	transfers = x.fake.countOf("transfer") - transfers // the refused first build of the re-request
	claimed := func() int {
		return agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, second)
	}
	s := x.settlement(second)
	if s.State != "requested" || x.waitClass(second) != xmrWaitMissing || s.TxHash != "" || claimed() != 1 {
		t.Fatalf("after the double spend: settlement %d %+v %q, %d claimed; settlements %v", second, s, x.waitClass(second), claimed(), x.xmrSettlements(order))
	}
	n := strconv.FormatInt(second, 10)
	notes := strings.Join(x.events(order), "\n")
	if !strings.Contains(notes, "Monero escrow settlement "+n+" ("+settlementKind("release")+") not sent: A deposit this settlement spends is no longer listed by the escrow wallet") ||
		strings.Count(notes, "A deposit this settlement spends is no longer listed") != 1 || strings.Contains(notes, "Monero escrow settlement "+n+" ("+settlementKind("release")+") failed") {
		t.Fatalf("order notes:\n%s", notes)
	}
	flags := x.staffFlags(order)
	panel := x.panel(order, x.sessOf(order, roleVendor))
	if !strings.Contains(panel, "Not built yet. A deposit this settlement spends is no longer listed by the escrow wallet") ||
		strings.Contains(panel, "from the deposits that remain") || strings.Contains(panel, "nothing to settle") {
		t.Fatalf("vendor's panel:\n%s", panel)
	}
	page := html.UnescapeString(x.page("/order?id="+order, admin))
	mustNotContain(t, page, `action="/admin/escrow-xmr-settled-by-hand"`, "Dismiss settlement", "still claims deposits of this escrow")
	// No failure alert: like any settlement waiting, it is an escrow alert once requested for more than 24 hours.
	if x.alerting(order) {
		t.Fatal("a waiting re-request alerts before 24 hours")
	}
	// A-617: the deposit's payment-review reason keeps its base reason and says which re-request claims it, that it
	// waits for this deposit to be listed again, and not to pay it by hand.
	if got := x.reviewReasons(order); len(got) != 1 || !strings.HasSuffix(got[0], waitingClaim(second, first, true)) || strings.Count(got[0], "re-requests") != 1 {
		t.Fatalf("payment-review reason while it waits:\n got %q\nwant suffix %q", got, waitingClaim(second, first, true))
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET created=now()-interval '25 hours' WHERE id=$1", second)
	if desk := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order); !x.alerting(order) ||
		!strings.Contains(desk, waitingAlert(second, first)) || strings.Contains(desk, "failed before any transaction was built") {
		t.Fatalf("desk alert after 24 hours:\n got %v %q\nwant %q", x.alerting(order), desk, waitingAlert(second, first))
	}
	// Past twice the threshold in blocks it still waits: no other coins, no new notice, nothing built.
	x.fake.mine(int(3 * x.A.escrowXMR.confirmations))
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(second); s.State != "requested" || x.waitClass(second) != xmrWaitMissing || x.fake.countOf("transfer") != transfers || x.staffFlags(order) != flags {
		t.Fatalf("past the bounded wait: %+v %q, transfer %d (want %d), staff flags %d (want %d)", s, x.waitClass(second), x.fake.countOf("transfer"), transfers, x.staffFlags(order), flags)
	}
	// Listed again: built from exactly the stored key images to the stored plan, and relayed. While a build is refused
	// once more it is still requested: the reason drops the wait (the deposit is listed) and keeps the rest, and the
	// desk's line is the generic one.
	x.fake.setTransfer(deposit, func(t *fakeXMRTransfer) { t.DoubleSpend = false })
	restore := x.refuseTransfer(-4, "transaction was rejected by daemon")
	x.mustPoll()
	restore()
	if s := x.settlement(second); s.State != "requested" || x.waitClass(second) == xmrWaitMissing {
		t.Fatalf("listed again, its build refused: %+v %q", s, x.waitClass(second))
	}
	if got := x.reviewReasons(order); len(got) != 1 || !strings.HasSuffix(got[0], waitingClaim(second, first, false)) || strings.Contains(got[0], "no longer lists") {
		t.Fatalf("payment-review reason once listed again:\n got %q\nwant suffix %q", got, waitingClaim(second, first, false))
	}
	if desk := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order); !strings.Contains(desk, "requested for more than 24 hours") || strings.Contains(desk, "re-requests") {
		t.Fatalf("desk alert once listed again: %q", desk)
	}
	x.mustPoll()
	x.mustPoll()
	old, s := x.settlement(first), x.settlement(second)
	if s.State != "relayed" || s.TxHash == "" || s.TxHash == old.TxHash || !slices.Equal(slices.Sorted(slices.Values(s.KeyImages)), slices.Sorted(slices.Values(old.KeyImages))) ||
		s.InputTotal != old.InputTotal || claimed() != 1 || old.State != "superseded" {
		t.Fatalf("once the deposit is listed again: %+v (superseded %+v); settlements %v", s, old, x.xmrSettlements(order))
	}
	if got, want := x.outputsOf(second), x.outputsOf(first); len(got) != len(want) || strings.SplitN(got[0], ":", 3)[1] != strings.SplitN(want[0], ":", 3)[1] {
		t.Fatalf("outputs %v, stored plan %v", got, want)
	}
	if x.alerting(order) {
		t.Fatal("still alerting once the re-request is relayed")
	}
	if got := x.reviewReasons(order); len(got) != 1 || strings.Contains(got[0], "re-requests") || strings.Contains(got[0], "Do not pay") {
		t.Fatalf("payment-review reason once relayed: %q", got)
	}
}

// A-617 control (A-554): a first release waiting for its missing credited deposit keeps its reasons: the deposit's
// payment-review reason and the desk's 24-hour line say nothing of a re-request.
func TestXMREscrowFirstReleaseWaitReasonUnchanged(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a617c_admin_"+randomToken()[:6], "admin")
	order, _, txid := x.completed()
	id := x.claimed(order)
	x.dropTransfer(txid)
	x.mustPoll()
	x.mustPoll()
	if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrWaitMissing {
		t.Fatalf("its deposit missing: %+v %q", s, x.waitClass(id))
	}
	if got := x.reviewReasons(order); !slices.Equal(got, []string{"Credited deposit conflicted or missing"}) {
		t.Fatalf("payment-review reason: %q", got)
	}
	agExec(x.testEnv, "UPDATE xmr_settlements SET created=now()-interval '25 hours' WHERE id=$1", id)
	if desk := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order); !strings.Contains(desk, "Monero escrow settlement has been requested for more than 24 hours") ||
		strings.Contains(desk, "re-requests") || strings.Contains(desk, "Do not pay") {
		t.Fatalf("desk alert: %q", desk)
	}
}

// PAY613-4: a failed settlement with no transaction built that still claims deposits. The market no longer leaves one
// (A-614: TestXMREscrowReRequestWaitsForMissingDeposit), but a database that ran a build from A-613 up to A-614 may hold
// one; the fixture writes that row as such a build did (the probe's re-request failed nothing-to-settle). No "nothing
// owed" dismissal is offered or accepted for it (the action's 409 and migration 128's trigger): the alert stays, saying
// its coins are in the escrow wallet and to settle with the operator, and the deposit's payment-review reason says the
// same rather than "transaction stored". The watcher leaves a failed settlement alone, so nothing in the market clears it.
func TestXMREscrowClaimingUnbuiltNotDismissed(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, _ := x.user("sbhc4_mod_"+randomToken()[:6], "moderator")
	adminID, admin := x.user("sbhc4_admin_"+randomToken()[:6], "admin")
	order, _, _, second := x.supersededWaiting(admin)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2,updated=now() WHERE id=$1 AND state='requested'", second, xmrNothing)
	x.mustPoll()
	s := x.settlement(second)
	if s.State != "failed" || x.waitClass(s.ID) != xmrNothing || s.TxHash != "" ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, s.ID) != 1 {
		t.Fatalf("fixture: settlement %d %+v %q; settlements %v", second, s, x.waitClass(s.ID), x.xmrSettlements(order))
	}
	n := strconv.FormatInt(s.ID, 10)
	if code, body := x.settledByHand(admin, s.ID, "Nothing is owed now, the deposit was double-spent."); code != 409 || !strings.Contains(body, "still claimed by this settlement") {
		t.Fatalf("dismissal of a settlement still claiming a deposit: %d %.300s", code, body)
	}
	if _, err := x.DB.Exec("INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,'nothing-owed','Direct insert, long enough.',$2)", s.ID, adminID); err == nil {
		t.Fatal("migration 128 accepted a dismissal of a settlement still claiming a deposit")
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_hand_settled"); n != 0 || x.settledByHandAudits("") != 0 {
		t.Fatalf("%d records, %d audit rows", n, x.settledByHandAudits(""))
	}
	page := html.UnescapeString(x.page("/order?id="+order, admin))
	mustNotContain(t, page, `action="/admin/escrow-xmr-settled-by-hand"`, "nothing owed now", "Dismiss settlement")
	if !x.alerting(order) || x.retentionReason(order) != "escrow_alert" || x.escrowAlertBadge(modID) != 1 {
		t.Fatalf("the alert: %v, retention %q, badge %d", x.alerting(order), x.retentionReason(order), x.escrowAlertBadge(modID))
	}
	desk := deskAlertIn(html.UnescapeString(x.page("/moderator", admin)), order)
	if !strings.Contains(desk, strandedAlert(n)) || strings.Contains(desk, "nothing owed") || strings.Contains(desk, "Do not pay") {
		t.Fatalf("desk alert:\n got %q\nwant %q", desk, strandedAlert(n))
	}
	if help := staffHelp(page); !strings.Contains(help, strandedAlert(n)) {
		t.Fatalf("staff help: %q", help)
	}
	d := &PageData{User: &User{ID: modID, Role: "moderator"}}
	r := httptest.NewRequest("GET", "/", nil)
	if err := loadPaymentReviews(r.Context(), x.A, r, d); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, v := range d.PaymentReviews {
		if v.OrderID == order {
			reasons = append(reasons, v.Reason)
		}
	}
	want := "Escrow deposit counted for the order; Monero escrow settlement " + n + ", which claims it, failed before any transaction was built, so its coins are still in the escrow wallet: settle with the operator, because the market does not settle them on its own"
	if len(reasons) != 1 || !strings.Contains(reasons[0], want) || strings.Contains(reasons[0], "stored") || strings.Contains(reasons[0], "re-request") {
		t.Fatalf("payment-review reason:\n got %q\nwant %q", reasons, want)
	}
}

// NIT-B: migration 128 refuses a record whose kind does not match its settlement: "nothing-owed" for one with a stored
// transaction refused for good, "settled-by-hand" for one never built.
func TestXMREscrowHandSettledKindMatchesSettlement(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("sbhk_admin_"+randomToken()[:6], "admin")
	order, stored := x.failedRelease()
	old := x.settlement(stored)
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	x.fake.mine(1)
	if code := x.reRequest(admin, order, stored, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if got := x.reRequests(stored); !slices.Equal(got, []string{"refused:" + xmrReRequestOnChain + ":0"}) {
		t.Fatalf("re-requests: %v", got)
	}
	unbuiltOrder, _, _ := x.completed()
	unbuilt, _ := x.release(unbuiltOrder)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", unbuilt, xmrNothing)
	for _, c := range []struct {
		id   int64
		kind string
	}{{stored, xmrNothingOwed}, {unbuilt, xmrSettledByHand}} {
		if _, err := x.DB.Exec("INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,$2,'Direct insert, long enough.',$3)", c.id, c.kind, adminID); err == nil || !strings.Contains(err.Error(), "is recorded") {
			t.Fatalf("a %s record of settlement %d: %v", c.kind, c.id, err)
		}
	}
	// The matching kinds are accepted.
	for _, c := range []struct {
		id   int64
		kind string
	}{{stored, xmrSettledByHand}, {unbuilt, xmrNothingOwed}} {
		if _, err := x.DB.Exec("INSERT INTO xmr_settlement_hand_settled(settlement_id,kind,note,settled_by) VALUES($1,$2,'Direct insert, long enough.',$3)", c.id, c.kind, adminID); err != nil {
			t.Fatalf("a %s record of settlement %d: %v", c.kind, c.id, err)
		}
	}
}

// NIT-A: the closing rule (xmrFailedClosedSQL) reads the record alone, so what keeps it right is that migration 128
// accepts a record only for a settlement that qualifies (TestXMREscrowSettledByHandFinalRefusalsOnly,
// TestXMREscrowClaimingUnbuiltNotDismissed, TestXMREscrowHandSettledKindMatchesSettlement) and then refuses the two
// changes that would stop it qualifying: a new re-request of a recorded settlement (which could leave an in-pool refusal
// newest, or supersede it), and a deposit claim moved onto one (which would make a dismissed one owe money). Both are
// refused here with the triggers on, and the alerts stay closed. (Re-checking those conditions in the rule itself, as
// correlated sub-queries, made the desk's queries cross PostgreSQL's JIT thresholds on tables without statistics.)
func TestXMREscrowHandSettledClosingNeedsFinalRefusal(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("sbhn_admin_"+randomToken()[:6], "admin")
	order, id := x.failedRelease()
	old := x.settlement(id)
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	x.fake.mine(1)
	if code := x.reRequest(admin, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if code, loc := x.settledByHand(admin, id, "Mined on chain: checked the wallet and the chain."); code != 303 {
		t.Fatalf("settled by hand: %d %s", code, loc)
	}
	dismissedOrder, _, _ := x.completed()
	dismissed, _ := x.release(dismissedOrder)
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='failed',send_error=$2 WHERE id=$1", dismissed, xmrNothing)
	if code, loc := x.settledByHand(admin, dismissed, "No deposit counted; nothing is owed now."); code != 303 {
		t.Fatalf("dismiss: %d %s", code, loc)
	}
	if x.alerting(order) || x.alerting(dismissedOrder) {
		t.Fatalf("alerting after the records: %v %v", x.alerting(order), x.alerting(dismissedOrder))
	}
	if _, err := x.DB.Exec("INSERT INTO xmr_settlement_rerequests(settlement_id,requested_by) VALUES($1,$2)", id, adminID); err == nil || !strings.Contains(err.Error(), "never re-requested") {
		t.Fatalf("a re-request of a recorded settlement: %v", err)
	}
	for _, c := range []struct {
		order string
		id    int64
	}{{dismissedOrder, dismissed}, {order, id}} {
		if _, err := x.DB.Exec("UPDATE payments SET xmr_settlement_id=$2 WHERE order_id=$1", c.order, c.id); err == nil || !strings.Contains(err.Error(), "claims no deposit") {
			t.Fatalf("a deposit claim moved onto recorded settlement %d: %v", c.id, err)
		}
	}
	if x.alerting(order) || x.alerting(dismissedOrder) || len(x.reRequests(id)) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE xmr_settlement_id=$1", dismissed) != 0 {
		t.Fatal("a refused change took effect")
	}
}
