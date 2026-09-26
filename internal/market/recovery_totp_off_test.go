package market

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A-154: a user who lost the authenticator can turn TOTP off at sign-in with a recovery code, and every path that
// changes an account's pending sign-ins, recovery codes and row takes the locks in one order (pending sign-in →
// recovery codes → account row), so none deadlocks with /challenge (BE11-5).

// a154Codes adds n more unused recovery codes for userID and returns them.
func a154Codes(e *testEnv, userID string, n int) []string {
	e.t.Helper()
	codes := make([]string, n)
	for i := range codes {
		codes[i] = newRecoveryCode()
		agExec(e, "INSERT INTO recovery_codes(code_hash,user_id) VALUES($1,$2)", recoveryHash(codes[i]), userID)
	}
	return codes
}

// a154TOTP describes the account's TOTP state: enabled, secret/pending/reveal set, last step, recovery codes left.
func a154TOTP(e *testEnv, userID string) (on bool, secret, pending, reveal string, last int64, codes int) {
	e.t.Helper()
	err := e.DB.QueryRow(`SELECT totp_enabled,totp_secret,totp_pending,recovery_reveal,totp_last_step,
		(SELECT count(*) FROM recovery_codes WHERE user_id=$1) FROM users WHERE id=$1`, userID).Scan(&on, &secret, &pending, &reveal, &last, &codes)
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func TestRecoveryCodeSignInTurnsTOTPOff(t *testing.T) {
	for _, role := range []string{"buyer", "admin"} { // applies to administrators too
		t.Run(role, func(t *testing.T) {
			e := newTestApp(t)
			handle := "a154_off_" + role
			id, _ := e.user(handle, role)
			_, recovery := agEnableTOTP(e, id)
			a154Codes(e, id, 2)
			// A pending secret, an unrevealed code list and a used step are all cleared.
			agExec(e, "UPDATE users SET totp_pending='x',recovery_reveal='y',recovery_reveal_until=now()+interval '5 minutes',totp_last_step=7 WHERE id=$1", id)
			other := agSession(e, id)
			otherPending := agPending(e, id)
			notes := a152Notes(e, id, "")

			anon, pc := e.passwordLogin(handle)
			w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc)
			e.check(w, 303)
			if loc := w.Header().Get("Location"); loc != "/totp?signin=totp-off" {
				t.Fatalf("landed on %q, want /totp?signin=totp-off", loc)
			}
			sess := e.session(w)
			on, secret, pending, reveal, last, codes := a154TOTP(e, id)
			if on || secret != "" || pending != "" || reveal != "" || last != 0 || codes != 0 {
				t.Fatalf("TOTP not fully off: on=%v secret=%v pending=%v reveal=%v last=%d codes=%d", on, secret != "", pending != "", reveal != "", last, codes)
			}
			a152Once(e, id, notes, "TOTP turned off at sign-in with a recovery code (2 remaining)", "two-step sign-in with an authenticator app (TOTP) was turned off", recovery)
			if a152Audits(e, id, "Used a recovery code") != 0 || a152Audits(e, id, "Signed in with password and recovery code") != 1 {
				t.Fatal("want the turn-off row in place of the recovery-code row, plus the sign-in row")
			}
			// Like /totp/disable, other sessions and pending sign-ins are left as they are.
			if agInt(e, "SELECT count(*) FROM sessions WHERE token_hash=$1", digest(other)) != 1 ||
				agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(otherPending.Value)) != 1 {
				t.Fatal("other sessions or pending sign-ins changed")
			}
			body := e.body("GET", "/totp?signin=totp-off", sess, nil, 200)
			if !strings.Contains(body, "Not enrolled") || !strings.Contains(body, "turned off two-step sign-in with an authenticator app (TOTP)") {
				t.Fatal("/totp does not show TOTP off with the sign-in notice")
			}
			// The old authenticator no longer matters: the next sign-in is password only.
			if code, loc := loginStatus(e, handle, testPassword); code != 303 || loc != "/" {
				t.Fatalf("password sign-in after turning TOTP off: %d %q", code, loc)
			}
		})
	}
}

// The checkbox is honoured only with a recovery code: with an authenticator code (or a wrong recovery code) nothing
// but the usual sign-in or failure happens.
func TestTurnOffTOTPCheckboxNeedsRecoveryCode(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a154_ignored", "buyer")
	key, recovery := agEnableTOTP(e, id)

	anon, pc := e.passwordLogin("a154_ignored")
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {"aaaa-bbbb-cccc-dddd"}, "turn_off_totp": {"1"}}, 401, "Recovery code incorrect", pc)
	w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}, "turn_off_totp": {"1"}}, pc)
	e.check(w, 303)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("TOTP sign-in landed on %q, want /", loc)
	}
	if on, _, _, _, _, codes := a154TOTP(e, id); !on || codes != 1 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL", id, recoveryHash(recovery)) != 1 {
		t.Fatal("the checkbox changed TOTP without a recovery code")
	}
	if a152Audits(e, id, "TOTP turned off") != 0 || a152Audits(e, id, "Signed in with password and TOTP") != 1 {
		t.Fatal("unexpected audit rows")
	}
}

// A recovery-code sign-in without the checkbox lands on /totp and says how many codes remain.
func TestRecoveryCodeSignInLandsOnTOTP(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a154_land", "buyer")
	_, recovery := agEnableTOTP(e, id)
	extra := a154Codes(e, id, 2)
	for i, tc := range []struct {
		code, remain string
	}{{recovery, "2 recovery codes remain"}, {extra[0], "1 recovery code remains"}, {extra[1], "0 recovery codes remain"}} {
		anon, pc := e.passwordLogin("a154_land")
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {tc.code}}, pc)
		e.check(w, 303)
		if loc := w.Header().Get("Location"); loc != "/totp?signin=recovery" {
			t.Fatalf("sign-in %d landed on %q", i, loc)
		}
		body := e.body("GET", "/totp?signin=recovery", e.session(w), nil, 200)
		if !strings.Contains(body, "You signed in with a recovery code. "+tc.remain) {
			t.Fatalf("sign-in %d: /totp lacks %q", i, tc.remain)
		}
	}
	if on, _, _, _, _, _ := a154TOTP(e, id); !on {
		t.Fatal("TOTP turned off without the checkbox")
	}
}

// A suspended account's sign-in is refused and the whole transaction rolls back: TOTP stays on, the code unused.
func TestTurnOffTOTPAtSignInSuspendedRollsBack(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a154_susp", "buyer")
	_, recovery := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_susp")
	agExec(e, "UPDATE users SET suspended_at=now() WHERE id=$1", id)
	audits, notes := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id), a152Notes(e, id, "")
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, 403, "suspended", pc)
	on, secret, _, _, _, codes := a154TOTP(e, id)
	if !on || secret == "" || codes != 1 || agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 1 {
		t.Fatal("a suspended account's sign-in changed TOTP or spent the code")
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id) != audits || a152Notes(e, id, "") != notes || agUserSessions(e, id) != 1 {
		t.Fatal("a refused sign-in left audit rows, notifications or a session")
	}
}

// The checkbox turns TOTP off only: PGP sign-in, when also on, stays on and is asked for at the next sign-in.
func TestTurnOffTOTPAtSignInKeepsPGPSignIn(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a154_pgp", "buyer")
	_, recovery := agEnableTOTP(e, id)
	_, pub := testPGPKey(t, "a154")
	agEnablePGP(e, id, pub)
	anon, pc := e.passwordLogin("a154_pgp")
	e.check(e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc), 303)
	if on, _, _, _, _, _ := a154TOTP(e, id); on || agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", id) != "true" {
		t.Fatal("want TOTP off and PGP sign-in still on")
	}
	e.passwordLogin("a154_pgp") // still asks for the second factor
}

// a154Blocked counts sessions waiting, directly or through another waiter, on the backend pid.
func a154Blocked(e *testEnv, pid int) int {
	return agInt(e, `WITH RECURSIVE w(pid) AS (
		SELECT pid FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))
		UNION SELECT a.pid FROM pg_stat_activity a JOIN w ON w.pid = ANY(pg_blocking_pids(a.pid)))
		SELECT count(*) FROM w`, pid)
}

// a154Ordered holds the account row in an outside transaction, starts first until it waits, then second until it
// waits too, and releases the row. Before A-154 each pair then deadlocked (PostgreSQL aborted one side: 500).
func a154Ordered(t *testing.T, e *testEnv, userID string, first, second func() int) (int, int) {
	t.Helper()
	t0, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer t0.Rollback()
	var pid int
	if err = t0.QueryRow("SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = t0.Exec("SELECT 1 FROM users WHERE id=$1 FOR UPDATE", userID); err != nil {
		t.Fatal(err)
	}
	wait := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); a154Blocked(e, pid) < n; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %d waiting request(s)", n)
			}
		}
	}
	a, b := make(chan int, 1), make(chan int, 1)
	go func() { a <- first() }()
	wait(1)
	go func() { b <- second() }()
	wait(2)
	if err = t0.Commit(); err != nil {
		t.Fatal(err)
	}
	return <-a, <-b
}

func TestPasswordChangeVsRecoverySignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("a154_pw", "buyer")
	key, recovery := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_pw")
	pw, ch := a154Ordered(t, e, id, func() int {
		return e.do("POST", "/account/password", sess, pwForm(testPassword, "a154-new-password", "code", agTOTPNow(key))).Code
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc).Code
	})
	// The password change ended the pending sign-in first, so the sign-in finds it gone.
	if pw != 303 || ch != 401 {
		t.Fatalf("password change %d, sign-in %d; want 303 and 401", pw, ch)
	}
	if on, _, _, _, _, codes := a154TOTP(e, id); !on || codes != 1 {
		t.Fatal("the refused sign-in changed TOTP")
	}
}

func TestPasswordChangeVsTOTPSignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("a154_pw2", "buyer")
	key, recovery := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_pw2")
	// The password change is confirmed with the recovery code: consumed before the account row is locked.
	pw, ch := a154Ordered(t, e, id, func() int {
		return e.do("POST", "/account/password", sess, pwForm(testPassword, "a154-new-password", "code", recovery)).Code
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc).Code
	})
	if pw != 303 || ch != 401 {
		t.Fatalf("password change %d, sign-in %d; want 303 and 401", pw, ch)
	}
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != 0 || !e.auditHas(id, "(confirmed with password and recovery code)") {
		t.Fatal("the recovery code did not confirm the password change")
	}
}

func TestFactorResetVsSignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("a154_boss", "admin")
	id, _ := e.user("a154_reset", "buyer")
	key, _ := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_reset")
	reset, ch := a154Ordered(t, e, id, func() int {
		return e.do("POST", "/admin/reset-factors", admin, url.Values{"handle": {"a154_reset"}, "password": {testPassword}}).Code
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc).Code
	})
	if reset != 303 || ch != 401 {
		t.Fatalf("factor reset %d, sign-in %d; want 303 and 401", reset, ch)
	}
	if on, _, _, _, _, codes := a154TOTP(e, id); on || codes != 0 || agUserSessions(e, id) != 0 {
		t.Fatal("factor reset incomplete")
	}
}

func TestReplaceRecoveryCodesVsRecoverySignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("a154_regen", "buyer")
	key, recovery := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_regen")
	regen, ch := a154Ordered(t, e, id, func() int {
		return e.do("POST", "/totp/recovery", sess, url.Values{"password": {testPassword}, "code": {agTOTPNow(key)}}).Code
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, pc).Code
	})
	// The codes were replaced first, so the old code no longer signs in.
	if regen != 303 || ch != 401 {
		t.Fatalf("replace codes %d, sign-in %d; want 303 and 401", regen, ch)
	}
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NULL", id) != recoveryCodeCount {
		t.Fatal("codes not replaced")
	}
}

func TestTurnOffTOTPVsRecoverySignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, sess := e.user("a154_disable", "buyer")
	key, recovery := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_disable")
	off, ch := a154Ordered(t, e, id, func() int {
		return e.do("POST", "/totp/disable", sess, url.Values{"password": {testPassword}, "code": {agTOTPNow(key)}}).Code
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}}, pc).Code
	})
	if off != 303 || ch != 401 {
		t.Fatalf("turn off %d, sign-in %d; want 303 and 401", off, ch)
	}
	if on, _, _, _, _, codes := a154TOTP(e, id); on || codes != 0 {
		t.Fatal("TOTP not off")
	}
}

func TestResetAdminPasswordVsSignInNoDeadlock(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a154_root", "admin")
	key, _ := agEnableTOTP(e, id)
	anon, pc := e.passwordLogin("a154_root")
	var cliErr error
	cli, ch := a154Ordered(t, e, id, func() int {
		if cliErr = ResetAdminPassword(context.Background(), e.DB, "a154_root", "operator-chosen-password"); cliErr != nil {
			return 500
		}
		return 0
	}, func() int {
		return e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc).Code
	})
	if cli != 0 || ch != 401 {
		t.Fatalf("reset-admin-password %v, sign-in %d; want success and 401", cliErr, ch)
	}
	if agUserSessions(e, id) != 0 {
		t.Fatal("the reset left a session")
	}
}

// Natural race (run with -race -count=5): a password change and a recovery sign-in that turns TOTP off, started
// together on fresh accounts, never deadlock and always leave a consistent account.
func TestPasswordChangeRacesRecoverySignIn(t *testing.T) {
	e := newTestApp(t)
	for i := range 20 {
		handle := "a154_race_" + string(rune('a'+i))
		id, sess := e.user(handle, "buyer")
		key, recovery := agEnableTOTP(e, id)
		anon, pc := e.passwordLogin(handle)
		var wg sync.WaitGroup
		var pw, ch int
		wg.Add(2)
		go func() {
			defer wg.Done()
			pw = e.do("POST", "/account/password", sess, pwForm(testPassword, "a154-new-password", "code", agTOTPNow(key))).Code
		}()
		go func() {
			defer wg.Done()
			ch = e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}, "recovery": {recovery}, "turn_off_totp": {"1"}}, pc).Code
		}()
		wg.Wait()
		on, _, _, _, _, codes := a154TOTP(e, id)
		switch {
		case pw != 303:
			t.Fatalf("race %d: password change %d (sign-in %d)", i, pw, ch)
		case ch == 303 && (on || codes != 0): // the sign-in ran first and turned TOTP off
			t.Fatalf("race %d: sign-in succeeded but TOTP on=%v codes=%d", i, on, codes)
		case ch == 401 && (!on || codes != 1): // the password change ended the pending sign-in first
			t.Fatalf("race %d: sign-in refused but TOTP on=%v codes=%d", i, on, codes)
		case ch != 303 && ch != 401:
			t.Fatalf("race %d: sign-in %d", i, ch)
		}
	}
}
