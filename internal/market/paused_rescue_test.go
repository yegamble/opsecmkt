package market

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A-571: while a handle's sign-in budget is paused, any account with TOTP signs in with its password and current
// authenticator code typed together on /login. The code is checked first: a wrong, used or absent code, an unknown
// handle or an account without TOTP is refused with the same 401 before any bcrypt and without spending a budget; only
// a right code reaches the password check, and that attempt spends the account's code budget (totp-verify:<id>).

// countSignInPasswordChecks counts /login's bcrypt comparisons until the test ends.
func countSignInPasswordChecks(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := compareSignInPassword
	compareSignInPassword = func(hash, password []byte) error {
		n.Add(1)
		return orig(hash, password)
	}
	t.Cleanup(func() { compareSignInPassword = orig })
	return &n
}

func TestPausedRescueSignsInEveryTOTPAccount(t *testing.T) {
	for _, tc := range []struct{ name, role string }{
		{"buyer", "buyer"}, {"vendor", "vendor"}, {"moderator", "moderator"},
		{"admin with another admin", "admin"}, {"TOTP and PGP sign-in", "buyer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("rescue_any", tc.role)
			key, _ := agEnableTOTP(e, id)
			switch tc.name {
			case "admin with another admin":
				e.user("rescue_other_admin", "admin")
			case "TOTP and PGP sign-in":
				_, pub := testPGPKey(t, "rescue_any")
				agEnablePGP(e, id, pub)
			}
			pauseSignIn(t, e, "rescue_any")
			agExpect(e, "/login", randomToken(), rescueForm("rescue_any", testPassword, ""), 429, signInPausedText)
			mine, other := agPending(e, id), agPending(e, id)
			form := rescueForm("RESCUE_ANY", testPassword, agTOTPNow(key))
			form.Set("next", "/orders")
			w := e.do("POST", "/login", randomToken(), form, mine)
			e.check(w, http.StatusSeeOther)
			e.session(w)
			if loc := w.Header().Get("Location"); loc != "/orders" {
				t.Fatalf("rescue landed on %q, want /orders", loc)
			}
			if agUserSessions(e, id) != 2 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) <= 0 ||
				agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Signed in with password and TOTP'", id) != 1 {
				t.Fatal("rescue did not record one session, one TOTP step and one sign-in audit row")
			}
			if factorBudgetCount(e, id) != 1 || !e.A.limited(signInKey("rescue_any"), 10) {
				t.Fatal("rescue was not charged exactly one code attempt, or lifted the sign-in pause")
			}
			if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(mine.Value)) != 0 ||
				agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(other.Value)) != 1 || e.cookie(w, "pending") != "" {
				t.Fatal("rescue did not end only this browser's pending sign-in")
			}
		})
	}
}

func TestPausedRescueRefusesBeforeBcryptWithoutBudget(t *testing.T) {
	e := newTestApp(t)
	const handle = "rescue_probe"
	pauseSignIn(t, e, handle) // the handle has no account yet
	checks := countSignInPasswordChecks(t)
	browser := randomToken() // one browser throughout, so the re-rendered form's CSRF token is the same
	probe := func(password, code string) *httptest.ResponseRecorder {
		return e.do("POST", "/login", browser, rescueForm(handle, password, code))
	}
	release := agHoldPasswordWork(t) // a request that needed bcrypt would be answered 503 busy
	refusals := map[string]*httptest.ResponseRecorder{}
	refusals["unknown handle"] = probe(testPassword, "123456")
	id, _ := e.user(handle, "buyer")
	refusals["account without TOTP"] = probe(testPassword, "123456")
	key, _ := agEnableTOTP(e, id)
	refusals["malformed code"] = probe(testPassword, "12345a")
	refusals["wrong code"] = probe(testPassword, e.wrongTOTP(id))
	agExec(e, "UPDATE users SET totp_last_step=$2 WHERE id=$1", id, totpStep(time.Now())+1)
	refusals["used code"] = probe(testPassword, agTOTPNow(key))
	agExec(e, "UPDATE users SET totp_last_step=0,suspended_at=now() WHERE id=$1", id)
	refusals["suspended account"] = probe(testPassword, agTOTPNow(key))
	agExec(e, "UPDATE users SET suspended_at=NULL WHERE id=$1", id)
	release()
	e.A.mu.Lock()
	entries := len(e.A.limits)
	e.A.mu.Unlock()
	if n := checks.Load(); n != 0 || entries != 1 || agLimited(e, "totp-verify:"+id) {
		t.Fatalf("refused codes ran bcrypt %d times or created limiter entries (%d entries)", n, entries)
	}
	// The right code with a wrong password reaches bcrypt once and spends one attempt of the code budget.
	wrongPassword := probe("wrong-long-password", agTOTPNow(key))
	if checks.Load() != 1 || factorBudgetCount(e, id) != 1 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 || agUserSessions(e, id) != 1 {
		t.Fatal("right code with a wrong password was not checked once and charged once, or spent the code")
	}
	refusals["right code, wrong password"] = wrongPassword
	want := refusals["unknown handle"]
	if want.Code != 401 || len(want.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("unknown handle: %d %q", want.Code, want.Header().Values("Set-Cookie"))
	}
	for name, w := range refusals {
		if w.Code != want.Code || !bytes.Equal(w.Body.Bytes(), want.Body.Bytes()) || !reflect.DeepEqual(w.Header(), want.Header()) {
			t.Fatalf("%s differs from the unknown-handle refusal: %d %v\n%s", name, w.Code, w.Header(), w.Body.String())
		}
	}
	// The right code and password still sign in, for the attempt already counted plus this one.
	w := probe(testPassword, agTOTPNow(key))
	e.check(w, http.StatusSeeOther)
	e.session(w)
	if checks.Load() != 2 || factorBudgetCount(e, id) != 2 || agUserSessions(e, id) != 2 {
		t.Fatal("rescue after a refused attempt did not sign in once")
	}
}

// The S-1/A-549 lock order holds for the rescue: this browser's live pending sign-in is taken before the account row,
// as /challenge takes them, so a /challenge from the same browser that holds the pending row is never deadlocked.
// A rescue whose pending sign-in was completed meanwhile is refused, so the two never make two sessions.
func TestPausedRescueWaitsForSameBrowserChallenge(t *testing.T) {
	for _, outcome := range []string{"challenge completes", "challenge fails"} {
		t.Run(outcome, func(t *testing.T) {
			e := newTestApp(t)
			id, _ := e.user("rescue_lock", "buyer")
			key, _ := agEnableTOTP(e, id)
			pauseSignIn(t, e, "rescue_lock")
			live := agPending(e, id)
			challenge, err := e.DB.Begin() // a /challenge from this browser, holding its pending sign-in
			if err != nil {
				t.Fatal(err)
			}
			defer challenge.Rollback()
			var uid string
			if err = challenge.QueryRow("SELECT user_id FROM pending_logins WHERE token_hash=$1 FOR UPDATE", digest(live.Value)).Scan(&uid); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- e.do("POST", "/login", randomToken(), rescueForm("rescue_lock", testPassword, agTOTPNow(key)), live)
			}()
			deadline := time.Now().Add(15 * time.Second)
			for agInt(e, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") == 0 {
				select {
				case w := <-done:
					t.Fatalf("rescue finished without waiting for this browser's pending sign-in: %d %q", w.Code, refusalText(w.Body.String()))
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("rescue never waited for this browser's pending sign-in")
				}
				time.Sleep(5 * time.Millisecond)
			}
			// The waiting rescue does not hold the account row, so the challenge can take it (no deadlock).
			if _, err = challenge.Exec("SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", id); err != nil {
				t.Fatalf("rescue held the account row while waiting for the pending sign-in: %v", err)
			}
			if outcome == "challenge completes" { // with another factor, as a PGP or recovery-code /challenge would
				if _, err = challenge.Exec("DELETE FROM pending_logins WHERE token_hash=$1", digest(live.Value)); err != nil {
					t.Fatal(err)
				}
				if _, err = challenge.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(randomToken()), id); err != nil {
					t.Fatal(err)
				}
				err = challenge.Commit()
			} else {
				err = challenge.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			w := <-done
			if outcome == "challenge completes" {
				if w.Code != 401 || e.cookie(w, "session") != "" || agUserSessions(e, id) != 2 || agInt(e, "SELECT totp_last_step FROM users WHERE id=$1", id) != 0 {
					t.Fatalf("rescue after the same browser's completed challenge: %d, %d sessions", w.Code, agUserSessions(e, id))
				}
				return
			}
			e.check(w, http.StatusSeeOther)
			e.session(w)
			if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(live.Value)) != 0 {
				t.Fatal("rescue after a failed challenge did not sign in once and end the pending sign-in")
			}
		})
	}
}

// A real race of the rescue and /challenge from one browser, with the same code and its live pending sign-in, at a
// sweep of offsets: never a 5xx (a cancelled deadlock) and never two sessions.
func TestPausedRescueRacesSameBrowserChallenge(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("rescue_race", "admin") // the sole administrator, so the rescue path is also eligible before A-571
	key, _ := agEnableTOTP(e, id)
	pauseSignIn(t, e, "rescue_race")
	start := time.Now()
	e.do("POST", "/login", randomToken(), rescueForm("rescue_race", "wrong-long-password", agTOTPNow(key)))
	rescueTime := time.Since(start)
	for i := range 40 {
		agExec(e, "UPDATE users SET totp_last_step=0 WHERE id=$1", id)
		e.A.mu.Lock()
		delete(e.A.limits, "totp-verify:"+id)
		e.A.mu.Unlock()
		before := agUserSessions(e, id)
		live := agPending(e, id)
		code := agTOTPNow(key)
		delay := time.Duration(i) * rescueTime / 20
		var wg sync.WaitGroup
		var rescue, challenge *httptest.ResponseRecorder
		wg.Add(2)
		go func() {
			defer wg.Done()
			rescue = e.do("POST", "/login", randomToken(), rescueForm("rescue_race", testPassword, code), live)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(delay)
			challenge = e.do("POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "code": {code}}, live)
		}()
		wg.Wait()
		if n := agUserSessions(e, id) - before; n != 1 || rescue.Code >= 500 || challenge.Code >= 500 {
			t.Fatalf("offset %d (%v): rescue %d %q, /challenge %d %q, %d new sessions", i, delay,
				rescue.Code, refusalText(rescue.Body.String()), challenge.Code, refusalText(challenge.Body.String()), n)
		}
		agExec(e, "DELETE FROM pending_logins WHERE user_id=$1", id)
	}
}
