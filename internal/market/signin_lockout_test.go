package market

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-153: the per-handle sign-in budget (auth:<handle>) is spent only by failed passwords. Registration and
// setup keep their own per-handle budgets, a correct sign-in spends nothing, the refusal says someone else
// may have caused it, and a count-only log line shows that the limiter is refusing sign-ins.

const signInPausedText = "Sign-in for this handle is paused for up to 10 minutes"

// caseVariant upper-cases the letters of handle selected by the bits of mask.
func caseVariant(handle string, mask int) string {
	b, bit := []byte(handle), 0
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			if mask&(1<<bit) != 0 {
				b[i] = c - 'a' + 'A'
			}
			bit++
		}
	}
	return string(b)
}

func TestRegisterAndSetupNeverSpendSignInBudget(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("lock_victim", "admin")
	login := url.Values{"handle": {"lock_victim"}, "password": {testPassword}}
	for range 10 {
		agExpect(e, "/register", randomToken(), url.Values{"handle": {"lock_victim"}, "password": {"a-new-long-password"}}, 400, "Handle unavailable")
	}
	// Handles are unique case-sensitively, so a case variant of the victim's handle is free: registering it
	// used to spend the victim's (case-insensitive) sign-in budget.
	for i := 1; i <= 10; i++ {
		agExpect(e, "/register", randomToken(), url.Values{"handle": {caseVariant("lock_victim", i)}, "password": {"a-new-long-password"}}, 303, "")
	}
	// Setup after installation needs no CAPTCHA and is refused only after the limiter.
	for range 10 {
		agExpect(e, "/setup", randomToken(), url.Values{"handle": {"lock_victim"}, "password": {"a-new-long-password"}, "token": {"wrong-token"}}, 403, "Setup unavailable")
	}
	if agLimited(e, "auth:lock_victim") {
		t.Fatal("registration or setup spent the sign-in budget")
	}
	w := e.do("POST", "/login", randomToken(), login)
	e.check(w, 303)
	e.session(w)
	if agUserSessions(e, id) != 2 {
		t.Fatal("sign-in after registrations did not create a session")
	}
	// Registration keeps its own per-handle budget (10 per ten minutes, case-insensitive), separate from
	// sign-in: the eleventh case variant is refused and nothing is created.
	agExpect(e, "/register", randomToken(), url.Values{"handle": {caseVariant("lock_victim", 11)}, "password": {"a-new-long-password"}}, 429, "Too many attempts")
	if n := agInt(e, "SELECT count(*) FROM users"); n != 11 {
		t.Fatalf("users=%d, want 11", n)
	}
	agExpect(e, "/setup", randomToken(), url.Values{"handle": {"lock_victim"}, "password": {"a-new-long-password"}, "token": {"wrong-token"}}, 429, "Too many attempts")
	w = e.do("POST", "/login", randomToken(), login)
	e.check(w, 303)
}

func TestCorrectSignInsSpendNoBudget(t *testing.T) {
	e := newTestApp(t)
	e.user("often_in", "buyer")
	for i := range 11 {
		w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"often_in"}, "password": {testPassword}})
		if w.Code != 303 || e.cookie(w, "session") == "" {
			t.Fatalf("correct sign-in %d: status=%d body=%q", i+1, w.Code, w.Body.String())
		}
	}
	if agLimited(e, "auth:often_in") {
		t.Fatal("correct sign-ins left a limiter entry")
	}
	// A correct sign-in between failures neither spends nor resets the failures: nine wrong, one right, one
	// wrong is the tenth failure, and the right password is then refused.
	for i := range 9 {
		agExpect(e, "/login", randomToken(), url.Values{"handle": {"often_in"}, "password": {"wrong-password-" + string(rune('a'+i))}}, 401, "Invalid handle or password")
	}
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"often_in"}, "password": {testPassword}}, 303, "")
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"often_in"}, "password": {"wrong-password-z"}}, 401, "Invalid handle or password")
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"often_in"}, "password": {testPassword}}, 429, signInPausedText)
}

func TestFailedPasswordsPauseSignInWithExplanation(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("paused_user", "admin")
	for i := range 10 {
		agExpect(e, "/login", randomToken(), url.Values{"handle": {"paused_user"}, "password": {"wrong-password-" + string(rune('a'+i))}}, 401, "Invalid handle or password")
	}
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"Paused_User"}, "password": {testPassword}})
	e.check(w, 429)
	body := w.Body.String()
	for _, want := range []string{signInPausedText, "Someone else may have caused this", "your password may be fine"} {
		if !strings.Contains(body, want) {
			t.Fatalf("429 body lacks %q: %q", want, body)
		}
	}
	if e.cookie(w, "session") != "" || e.cookie(w, "pending") != "" || agUserSessions(e, id) != 1 {
		t.Fatal("paused sign-in issued credentials")
	}
	// Registration's budget is separate, so the paused sign-in does not refuse a free handle's registration.
	agExpect(e, "/register", randomToken(), url.Values{"handle": {"paused_new"}, "password": {"a-new-long-password"}}, 303, "")
	// Break-glass (operator guide): restarting the application empties the in-memory limiter.
	e.restart()
	w = e.do("POST", "/login", randomToken(), url.Values{"handle": {"paused_user"}, "password": {testPassword}})
	e.check(w, 303)
	e.session(w)
}

func TestSignInLimiterLogsCountOnly(t *testing.T) {
	e := newTestApp(t)
	e.user("logged_victim", "buyer")
	e.user("other_victim", "buyer")
	logs := captureLog(t)
	failN := func(handle string, n int) {
		t.Helper()
		for i := range n {
			agExpect(e, "/login", randomToken(), url.Values{"handle": {handle}, "password": {"wrong-password-" + string(rune('a'+i))}}, 401, "Invalid handle or password")
		}
	}
	const line = "sign-in limiter refusing handles="
	failN("logged_victim", 9)
	if got := logs.lines(line); len(got) != 0 {
		t.Fatalf("logged before any handle was refused: %q", got)
	}
	failN("logged_victim", 1)
	if got := logs.lines(line); len(got) != 1 || got[0] != line+"1" {
		t.Fatalf("after the tenth failure: %q", got)
	}
	// Refusals within the same minute add no line.
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"logged_victim"}, "password": {testPassword}}, 429, signInPausedText)
	failN("other_victim", 10)
	if got := logs.lines(line); len(got) != 1 {
		t.Fatalf("more than one line within a minute: %q", got)
	}
	// A minute later, the next refusal logs the current count again.
	e.A.mu.Lock()
	e.A.signInLogAt = time.Now().Add(-61 * time.Second)
	e.A.mu.Unlock()
	agExpect(e, "/login", randomToken(), url.Values{"handle": {"other_victim"}, "password": {testPassword}}, 429, signInPausedText)
	if got := logs.lines(line); len(got) != 2 || got[1] != line+"2" {
		t.Fatalf("after a minute: %q", got)
	}
	all := strings.ToLower(logs.String())
	for _, h := range []string{"logged_victim", "other_victim"} {
		if strings.Contains(all, h) {
			t.Fatalf("log names a handle: %q", logs.String())
		}
	}
}
