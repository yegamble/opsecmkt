package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func staffTargetForm(path, handle, operation, nonce string) url.Values {
	var f url.Values
	if path == "/admin/suspend" {
		f = suspendForm(operation, handle)
	} else {
		f = resetForm(handle)
	}
	f.Set(formNonceField, nonce)
	return f
}

func TestStaffTargetNonceRechecksCurrentActorAndSession(t *testing.T) {
	for _, tc := range []struct {
		name, path, change string
		want               int
	}{
		{"suspend demoted", "/admin/suspend", "UPDATE users SET role='buyer' WHERE id=$1", 403},
		{"suspend revoked", "/admin/suspend", "DELETE FROM sessions WHERE user_id=$1", 401},
		{"reset suspended", "/admin/reset-factors", "UPDATE users SET suspended_at=now() WHERE id=$1", 403},
		{"reset expired", "/admin/reset-factors", "UPDATE sessions SET expires=clock_timestamp()-interval '1 second' WHERE user_id=$1", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("nonce_staff_admin", "admin")
			targetID, _ := e.user("nonce_staff_target", "buyer")
			if tc.path == "/admin/reset-factors" {
				agEnableTOTP(e, targetID)
			}
			f := staffTargetForm(tc.path, "nonce_staff_target", "suspend", randomToken()[:32])
			e.check(e.do("POST", tc.path, admin, f), 303)
			w := besideAccountChange(t, e, adminID, []string{tc.change}, func() *httptest.ResponseRecorder {
				return e.do("POST", tc.path, admin, f)
			})
			e.check(w, tc.want)
			a, b, c, d := staffTargetCounts(t, e, tc.path, adminID, targetID)
			if [4]int{a, b, c, d} != [4]int{1, 1, 1, 1} {
				t.Fatalf("stale actor replay changed effects: %v", [4]int{a, b, c, d})
			}
		})
	}
}

func TestStaffTargetNonceOldTOTPReplaysWithoutSpendingAgain(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("nonce_staff_admin", "admin")
	e.user("nonce_staff_target", "buyer")
	key, _ := agEnableTOTP(e, adminID)
	f := staffTargetForm("/admin/suspend", "nonce_staff_target", "suspend", randomToken()[:32])
	f.Set("code", agTOTPNow(key))
	e.check(e.do("POST", "/admin/suspend", admin, f), 303)
	step := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", adminID)
	e.check(e.do("POST", "/admin/suspend", admin, f), 303)
	if got := agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", adminID); got != step {
		t.Fatalf("replay spent TOTP again: %d to %d", step, got)
	}
	f.Set(formNonceField, randomToken()[:32])
	e.check(e.do("POST", "/admin/suspend", admin, f), 401)
	if got := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path='/admin/suspend'"); got != 1 {
		t.Fatalf("fresh form with old code wrote %d receipts", got)
	}
}

func staffTargetCounts(t *testing.T, e *testEnv, path, actorID, targetID string) (receipts, actorAudits, targetAudits, notices int) {
	t.Helper()
	for _, q := range []struct {
		sql  string
		args []any
		out  *int
	}{
		{"SELECT count(*) FROM form_submissions WHERE action_path=$1", []any{path}, &receipts},
		{"SELECT count(*) FROM audit_events WHERE user_id=$1", []any{actorID}, &actorAudits},
		{"SELECT count(*) FROM audit_events WHERE user_id=$1", []any{targetID}, &targetAudits},
		{"SELECT count(*) FROM notifications WHERE user_id=$1", []any{targetID}, &notices},
	} {
		if err := e.DB.QueryRow(q.sql, q.args...).Scan(q.out); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestStaffTargetConfirmedFormsRenderDistinctNoncesWithoutWrites(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("nonce_staff_admin", "admin")
	page := e.body("GET", "/admin/users", admin, nil, 200)
	suspend := renderedFormNonce(t, page, "/admin/suspend")
	reset := renderedFormNonce(t, page, "/admin/reset-factors")
	if suspend == reset || submissionCount(t, e) != 0 {
		t.Fatal("staff forms reused a nonce or admin GET wrote a receipt")
	}
}

func TestStaffTargetNonceReplaysOnlyExactSuccessfulOperation(t *testing.T) {
	for _, tc := range []struct{ name, path, operation string }{
		{"suspend", "/admin/suspend", "suspend"},
		{"restore", "/admin/suspend", "restore"},
		{"reset factors", "/admin/reset-factors", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("nonce_staff_admin", "admin")
			targetID, _ := e.user("nonce_staff_target", "buyer")
			e.user("nonce_staff_other", "buyer")
			if tc.operation == "restore" {
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", targetID)
			}
			if tc.path == "/admin/reset-factors" {
				agEnableTOTP(e, targetID)
			}
			f := staffTargetForm(tc.path, "nonce_staff_target", tc.operation, randomToken()[:32])
			first := e.do("POST", tc.path, admin, f)
			e.check(first, 303)
			before := [4]int{}
			before[0], before[1], before[2], before[3] = staffTargetCounts(t, e, tc.path, adminID, targetID)
			if before[0] != 1 || before[1] != 1 || before[2] != 1 || before[3] != 1 {
				t.Fatalf("first staff change counts %v", before)
			}
			second := e.do("POST", tc.path, admin, f)
			e.check(second, 303)
			if first.Header().Get("Location") == "" || first.Header().Get("Location") != second.Header().Get("Location") {
				t.Fatalf("replay redirect %q vs %q", first.Header().Get("Location"), second.Header().Get("Location"))
			}
			changed := staffTargetForm(tc.path, "nonce_staff_other", tc.operation, f.Get(formNonceField))
			e.check(e.do("POST", tc.path, admin, changed), 409)
			if tc.path == "/admin/suspend" {
				changed = staffTargetForm(tc.path, "nonce_staff_target", map[string]string{"suspend": "restore", "restore": "suspend"}[tc.operation], f.Get(formNonceField))
				e.check(e.do("POST", tc.path, admin, changed), 409)
			}
			_, otherAdmin := e.user("nonce_staff_other_admin", "admin")
			e.check(e.do("POST", tc.path, otherAdmin, f), 409)
			wrong := staffTargetForm(tc.path, "nonce_staff_target", tc.operation, f.Get(formNonceField))
			wrong.Set("password", "wrong password")
			e.check(e.do("POST", tc.path, admin, wrong), 401)
			var saved string
			if err := e.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{testPassword, "nonce_staff_target", admin, f.Get("code")} {
				if secret != "" && strings.Contains(saved, secret) {
					t.Fatal("staff receipt retained raw form or session data")
				}
			}
			a, b, c, d := staffTargetCounts(t, e, tc.path, adminID, targetID)
			if [4]int{a, b, c, d} != before {
				t.Fatalf("refusals/replay added receipt, audit, or notice: %v vs %v", [4]int{a, b, c, d}, before)
			}
		})
	}
}

func TestStaffTargetNonceRollbackAndCurrentTargetEligibility(t *testing.T) {
	for _, path := range []string{"/admin/suspend", "/admin/reset-factors"} {
		t.Run(path, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("nonce_staff_admin", "admin")
			targetID, _ := e.user("nonce_staff_target", "buyer")
			operation := "restore"
			if path == "/admin/reset-factors" {
				operation = ""
			}
			f := staffTargetForm(path, "nonce_staff_target", operation, randomToken()[:32])
			e.check(e.do("POST", path, admin, f), 409) // active account or factorless target
			if n := agInt(e, "SELECT count(*) FROM form_submissions WHERE action_path=$1", path); n != 0 {
				t.Fatalf("refused staff action kept %d receipts", n)
			}
			if path == "/admin/suspend" {
				agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", targetID)
			} else {
				agEnableTOTP(e, targetID)
			}
			e.check(e.do("POST", path, admin, f), 303) // same nonce can recover after rollback
			agExec(e, "UPDATE users SET role='admin' WHERE id=$1", targetID)
			e.check(e.do("POST", path, admin, f), 403) // changed target eligibility blocks even an exact receipt
			a, b, c, d := staffTargetCounts(t, e, path, adminID, targetID)
			if [4]int{a, b, c, d} != [4]int{1, 1, 1, 1} {
				t.Fatalf("target eligibility changed effects: %v", [4]int{a, b, c, d})
			}
		})
	}
}

func TestStaffTargetNonceLostCommitAcknowledgementCanReplay(t *testing.T) {
	for _, path := range []string{"/admin/suspend", "/admin/reset-factors"} {
		t.Run(path, func(t *testing.T) {
			e := newTestApp(t)
			adminID, admin := e.user("nonce_staff_admin", "admin")
			targetID, _ := e.user("nonce_staff_target", "buyer")
			if path == "/admin/reset-factors" {
				agEnableTOTP(e, targetID)
			}
			f := staffTargetForm(path, "nonce_staff_target", "suspend", randomToken()[:32])
			lost := loseNextActionCommit(t, e)
			w := e.do("POST", path, admin, f)
			if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
				!strings.Contains(w.Body.String(), "Staff action outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
				t.Fatalf("lost acknowledgement: injected=%t status=%d retry=%q body=%q", !lost.Load(), w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}
			e.check(e.do("POST", path, admin, f), 303)
			a, b, c, d := staffTargetCounts(t, e, path, adminID, targetID)
			if [4]int{a, b, c, d} != [4]int{1, 1, 1, 1} {
				t.Fatalf("lost-ack replay counts: %v", [4]int{a, b, c, d})
			}
		})
	}
}
