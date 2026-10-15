package market

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type rebroadcastReceiptCase struct {
	e               *testEnv
	path            string
	settlementID    string
	orderID         string
	adminID         string
	adminSession    string
	auditLike       string
	eventLike       string
	lostAckText     string
	savedRedirect   string
	settlementTable string
	renderedForms   int
	rpcCount        func() int
	clearAndRehold  func()
	heldValue       string
}

func rebroadcastReceiptCounts(t *testing.T, c rebroadcastReceiptCase) (receipts, audits, events int) {
	t.Helper()
	for _, q := range []struct {
		query string
		arg   any
		out   *int
	}{
		{"SELECT count(*) FROM form_submissions WHERE action_path=$1", c.path, &receipts},
		{"SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", c.adminID, &audits},
		{"SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE $2", c.orderID, &events},
	} {
		var err error
		if q.out == &audits {
			err = c.e.DB.QueryRow(q.query, q.arg, c.auditLike).Scan(q.out)
		} else if q.out == &events {
			err = c.e.DB.QueryRow(q.query, q.arg, c.eventLike).Scan(q.out)
		} else {
			err = c.e.DB.QueryRow(q.query, q.arg).Scan(q.out)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return
}

func rebroadcastReceiptRows(t *testing.T, c rebroadcastReceiptCase) [3]string {
	t.Helper()
	queries := []struct {
		query string
		args  []any
	}{
		{"SELECT COALESCE(string_agg(row_to_json(f)::text,E'\\n' ORDER BY f.nonce),'') FROM form_submissions f WHERE action_path=$1", []any{c.path}},
		{"SELECT COALESCE(string_agg(row_to_json(a)::text,E'\\n' ORDER BY a.id),'') FROM audit_events a WHERE user_id=$1 AND action LIKE $2", []any{c.adminID, c.auditLike}},
		{"SELECT COALESCE(string_agg(row_to_json(o)::text,E'\\n' ORDER BY o.id),'') FROM order_events o WHERE order_id=$1 AND note LIKE $2", []any{c.orderID, c.eventLike}},
	}
	var rows [3]string
	for i, q := range queries {
		if err := c.e.DB.QueryRow(q.query, q.args...).Scan(&rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func rebroadcastSettlementState(t *testing.T, c rebroadcastReceiptCase) string {
	t.Helper()
	var state string
	query := "SELECT state||'|'||send_error||'|'||updated::text FROM " + c.settlementTable + " WHERE id=$1"
	if err := c.e.DB.QueryRow(query, c.settlementID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func runRebroadcastReceiptContract(t *testing.T, c rebroadcastReceiptCase) {
	t.Helper()
	totpKey, _ := agEnableTOTP(c.e, c.adminID)
	page := c.e.do("GET", "/admin", c.adminSession, nil)
	if page.Code != 200 {
		t.Fatalf("admin GET: %d %s", page.Code, page.Body.String())
	}
	nonces := renderedActionNonces(t, page.Body.String(), c.path)
	if len(nonces) != c.renderedForms {
		t.Fatalf("rendered %s forms=%d, want %d", c.path, len(nonces), c.renderedForms)
	}
	seen := map[string]bool{}
	for _, n := range nonces {
		if seen[n] {
			t.Fatalf("rendered forms for %s reused nonce %s", c.path, n)
		}
		seen[n] = true
	}
	nonce := renderedFormNonce(t, page.Body.String(), c.path)
	form := url.Values{
		"settlement_id": {c.settlementID},
		"not_sent":      {"confirmed"},
		"password":      {testPassword},
		"code":          {agTOTPNow(totpKey)},
		formNonceField:  {nonce},
	}
	if got := agInt(c.e, "SELECT count(*) FROM form_submissions WHERE action_path=$1", c.path); got != 0 {
		t.Fatalf("admin GET wrote %d receipts", got)
	}
	rpcBaseline := c.rpcCount()
	before := [3]int{}
	before[0], before[1], before[2] = rebroadcastReceiptCounts(t, c)

	lost := loseNextActionCommit(t, c.e)
	w := c.e.do("POST", c.path, c.adminSession, form)
	if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), c.lostAckText) || !strings.Contains(w.Body.String(), "verify") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	committed := [3]int{}
	committed[0], committed[1], committed[2] = rebroadcastReceiptCounts(t, c)
	if committed[0] != before[0]+1 || committed[1] != before[1]+1 || committed[2] != before[2]+1 || c.rpcCount() != rpcBaseline {
		t.Fatalf("lost acknowledgement did not commit one database-only action: before=%v committed=%v rpc=%d baseline=%d", before, committed, c.rpcCount(), rpcBaseline)
	}
	committedRows := rebroadcastReceiptRows(t, c)
	if got := agInt(c.e, "SELECT count(*) FROM users WHERE id=$1 AND totp_last_step>0", c.adminID); got != 1 {
		t.Fatal("confirmed form did not consume its current authenticator step")
	}

	// An exact receipt stays bound to its original actor, session and canonical form body.
	changed := cloneValues(form)
	changed.Set("extra", "different saved transaction check")
	if got := c.e.do("POST", c.path, c.adminSession, changed); got.Code != 409 {
		t.Fatalf("changed body replay: %d %s", got.Code, got.Body.String())
	}
	if got := c.e.do("POST", c.path, agSession(c.e, c.adminID), form); got.Code != 409 {
		t.Fatalf("another session replay: %d %s", got.Code, got.Body.String())
	}
	otherID, other := c.e.user("rebroadcast_other_admin_"+randomToken()[:6], "admin")
	_ = otherID
	if got := c.e.do("POST", c.path, other, form); got.Code != 409 {
		t.Fatalf("another administrator replay: %d %s", got.Code, got.Body.String())
	}
	wrongPassword := cloneValues(form)
	wrongPassword.Set("password", "incorrect administrator password")
	if got := c.e.do("POST", c.path, c.adminSession, wrongPassword); got.Code != 401 {
		t.Fatalf("wrong password replay: %d %s", got.Code, got.Body.String())
	}
	if got, err := hashPassword("a-different-long-password"); err != nil {
		t.Fatal(err)
	} else {
		agExec(c.e, "UPDATE users SET password_hash=$2 WHERE id=$1", c.adminID, got)
	}
	if got := c.e.do("POST", c.path, c.adminSession, form); got.Code != 401 {
		t.Fatalf("saved response bypassed a changed current password: %d %s", got.Code, got.Body.String())
	}
	// Restore the original password hash and demote the actor, which must revoke route eligibility too.
	originalHash, err := hashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	agExec(c.e, "UPDATE users SET password_hash=$2,role='moderator' WHERE id=$1", c.adminID, originalHash)
	if got := c.e.do("POST", c.path, c.adminSession, form); got.Code != 403 {
		t.Fatalf("demoted administrator replay: %d %s", got.Code, got.Body.String())
	}
	agExec(c.e, "UPDATE users SET role='admin' WHERE id=$1", c.adminID)

	// The saved response can be read without spending the already-used TOTP step a second time.
	replay := c.e.do("POST", c.path, c.adminSession, form)
	if replay.Code != 303 || replay.Header().Get("Location") != c.savedRedirect {
		t.Fatalf("lost-ack replay status/location: %d %q, want %q", replay.Code, replay.Header().Get("Location"), c.savedRedirect)
	}
	if beforeStep := agInt(c.e, "SELECT totp_last_step FROM users WHERE id=$1", c.adminID); beforeStep <= 0 {
		t.Fatalf("TOTP step after exact replay = %d", beforeStep)
	}
	fresh := cloneValues(form)
	fresh.Set(formNonceField, randomToken()[:32])
	if got := c.e.do("POST", c.path, c.adminSession, fresh); got.Code != 401 {
		t.Fatalf("fresh form accepted a spent authenticator code: %d %s", got.Code, got.Body.String())
	}
	if r, a, e := rebroadcastReceiptCounts(t, c); [3]int{r, a, e} != committed {
		t.Fatalf("replay or refusals changed saved effects: committed=%v after=%v", committed, [3]int{r, a, e})
	}
	if rows := rebroadcastReceiptRows(t, c); rows != committedRows {
		t.Fatalf("replay or refusals changed exact receipt, audit or event rows: committed=%q after=%q", committedRows, rows)
	}
	if c.rpcCount() != rpcBaseline {
		t.Fatalf("confirmation, replay or refusal made %d provider calls", c.rpcCount()-rpcBaseline)
	}
	var saved string
	if err := c.e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, form.Get("code"), c.adminSession} {
		if strings.Contains(saved, secret) {
			t.Fatal("rebroadcast receipt retained a password, authenticator code or session token")
		}
	}

	// Let the watcher clear the confirmation, then simulate a restored/reheld settlement. The old receipt must return
	// its original response without changing the new hold or scheduling another network call.
	c.clearAndRehold()
	heldBefore := agStrings(t, c.e, "SELECT send_error FROM "+c.settlementTable+" WHERE id=$1", c.settlementID)
	if len(heldBefore) != 1 || heldBefore[0] != c.heldValue {
		t.Fatalf("watcher/rehold fixture: %v", heldBefore)
	}
	stateBeforeReplay := rebroadcastSettlementState(t, c)
	rowsBeforeReplay := rebroadcastReceiptRows(t, c)
	rpcBefore := c.rpcCount()
	reheld := c.e.do("POST", c.path, c.adminSession, form)
	if reheld.Code != 303 || reheld.Header().Get("Location") != c.savedRedirect {
		t.Fatalf("old receipt after watcher clear/rehold: %d %s", reheld.Code, reheld.Body.String())
	}
	stateAfterReplay := rebroadcastSettlementState(t, c)
	if stateAfterReplay != stateBeforeReplay || c.rpcCount() != rpcBefore {
		t.Fatalf("old receipt changed settlement state/timestamp or made an RPC: before=%q after=%q rpc=%d to %d", stateBeforeReplay, stateAfterReplay, rpcBefore, c.rpcCount())
	}
	if r, a, e := rebroadcastReceiptCounts(t, c); [3]int{r, a, e} != committed {
		t.Fatalf("old receipt after rehold duplicated effects: committed=%v after=%v", committed, [3]int{r, a, e})
	}
	if rows := rebroadcastReceiptRows(t, c); rows != rowsBeforeReplay {
		t.Fatalf("old receipt after rehold changed exact receipt, audit or event rows: before=%q after=%q", rowsBeforeReplay, rows)
	}
}

func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, values := range in {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func TestMoneroEscrowRebroadcastConfirmedReceipt(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("nonce_xmr_rebroadcast_admin_"+randomToken()[:6], "admin")
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	if err := x.poll(); err == nil {
		t.Fatal("expected the first relay attempt to be refused")
	}
	if got := x.settlement(id); got.State != "built" || got.TxHash == "" {
		t.Fatalf("built fixture: %+v", got)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", id, restoredSettlementHold)
	caseInfo := rebroadcastReceiptCase{
		e: x.testEnv, path: "/admin/escrow-xmr-rebroadcast", settlementID: fmtInt(id), orderID: order, adminID: adminID, adminSession: admin, settlementTable: "xmr_settlements", renderedForms: 1, savedRedirect: "/admin#xmr-escrow-recovery",
		auditLike:   "Confirmed the relay of Monero escrow settlement " + fmtInt(id) + "%",
		eventLike:   "%checked that this Monero escrow settlement's transaction was not already relayed or replaced after the market restored%",
		lostAckText: "Monero escrow relay outcome unknown", rpcCount: func() int { return x.fake.countOf("relay_tx") }, heldValue: restoredSettlementHold,
		clearAndRehold: func() {
			agExec(x.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
			x.mustPoll()
			if got := x.settlement(id); got.State != "relayed" || got.TxHash == "" || x.fake.countOf("relay_tx") != 2 {
				t.Fatalf("watcher did not relay confirmed settlement exactly once: %+v relays=%d", got, x.fake.countOf("relay_tx"))
			}
			agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", id, restoredSettlementHold)
		},
	}
	runRebroadcastReceiptContract(t, caseInfo)
}

func fmtInt(id int64) string { return strconv.FormatInt(id, 10) }
