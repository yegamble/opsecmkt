package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// A-157: "Sign out everywhere" also ends the account's pending sign-ins, so an intruder who passed the password
// step and is parked at /challenge cannot finish signing in with the second factor afterwards.
func TestSignOutEverywhereEndsPendingSignIns(t *testing.T) {
	e := newTestApp(t)
	id, owner := e.user("a157_owner", "buyer")
	key, _ := agEnableTOTP(e, id)
	otherID, _ := e.user("a157_bystander", "buyer")
	agEnableTOTP(e, otherID)
	intruder, pc := e.pendingSignIn("a157_owner") // the intruder knows the password; TOTP is still pending
	_, bystanderPC := e.pendingSignIn("a157_bystander")

	w := e.do("POST", "/revoke-sessions", owner, nil)
	e.check(w, 303)
	if !agClearedCookie(w, "session") || agUserSessions(e, id) != 0 {
		t.Fatal("sign out everywhere left a session")
	}
	if n := agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id); n != 0 {
		t.Fatalf("sign out everywhere left %d pending sign-in(s)", n)
	}
	if !e.auditHas(id, "Revoked all sessions: ended 1 session(s) and 1 pending sign-in(s)") {
		t.Fatal("sign out everywhere not audited with what it ended")
	}
	agExpect(e, "/challenge", intruder, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 401, "Sign-in verification expired", pc)
	if agUserSessions(e, id) != 0 {
		t.Fatal("the intruder finished signing in after sign out everywhere")
	}
	// Another account's sign-in in progress is untouched.
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(bystanderPC.Value)) != 1 {
		t.Fatal("sign out everywhere ended another account's pending sign-in")
	}
}

// Sign out everywhere racing an intruder's /challenge, in both orders (a154Ordered holds the account row until
// both wait): neither deadlocks, and the account is left with no session either way — a sign-in that finished
// first loses its session, one that comes second finds its pending sign-in gone.
func TestSignOutEverywhereVsSignInLeavesNoSession(t *testing.T) {
	e := newTestApp(t)
	for i, challengeFirst := range []bool{true, false} {
		handle := "a157_race_" + string(rune('a'+i))
		id, owner := e.user(handle, "buyer")
		key, _ := agEnableTOTP(e, id)
		intruder, pc := e.pendingSignIn(handle)
		challenge := func() int {
			return e.do("POST", "/challenge", intruder, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, pc).Code
		}
		revoke := func() int { return e.do("POST", "/revoke-sessions", owner, nil).Code }
		var ch, rv int
		if challengeFirst {
			ch, rv = a154Ordered(t, e, id, challenge, revoke)
		} else {
			rv, ch = a154Ordered(t, e, id, revoke, challenge)
		}
		if rv != 303 || (challengeFirst && ch != 303) || (!challengeFirst && ch != 401) {
			t.Fatalf("challenge first %v: sign-in %d, sign out everywhere %d", challengeFirst, ch, rv)
		}
		if n := agUserSessions(e, id); n != 0 {
			t.Fatalf("challenge first %v: %d session(s) left", challengeFirst, n)
		}
	}
}

// accountSessionRows returns the rendered rows of the /account Sessions panel.
func accountSessionRows(t *testing.T, body string) (count string, rows []string) {
	t.Helper()
	panel := regexp.MustCompile(`(?s)<section class="panel" id="sessions".*?</section>`).FindString(body)
	if panel == "" {
		t.Fatalf("no sessions panel in:\n%s", body)
	}
	if m := regexp.MustCompile(`<p class="session-count">([^<]*)</p>`).FindStringSubmatch(panel); m != nil {
		count = m[1]
	}
	for _, m := range regexp.MustCompile(`(?s)<li class="session-row">(.*?)</li>`).FindAllStringSubmatch(panel, -1) {
		rows = append(rows, strings.Join(strings.Fields(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], " ")), " "))
	}
	return count, rows
}

// A-157: /account lists the account's live sessions (count, start time to the minute in UTC, this browser's
// marked) and nothing about the client: the market stores no IP address or user agent.
func TestAccountListsActiveSessions(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("a157_sessions", "buyer")
	agExec(e, "DELETE FROM sessions WHERE user_id=$1", id)
	// Sign in for real from a client that sends identifying headers; none of them may be stored or shown.
	signIn := func() string {
		t.Helper()
		anon := randomToken()
		f := url.Values{"handle": {"a157_sessions"}, "password": {testPassword}, "csrf": {e.A.csrf(anon)}}
		r := httptest.NewRequest("POST", "/login", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("User-Agent", "A157ProbeAgent/9.9")
		r.Header.Set("X-Forwarded-For", "198.51.100.157")
		r.RemoteAddr = "203.0.113.157:4157"
		r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		e.check(w, 303)
		return e.session(w)
	}
	mine, other := signIn(), signIn()
	agExec(e, "UPDATE sessions SET created='2026-03-04 05:06:59+00' WHERE token_hash=$1", digest(mine))
	agExec(e, "UPDATE sessions SET created='2026-03-05 23:59:01+00' WHERE token_hash=$1", digest(other))
	// An expired session is not active and is not listed.
	agExec(e, "INSERT INTO sessions(token_hash,user_id,expires,created) VALUES($1,$2,now()-interval '1 minute','2026-01-01 00:00+00')", digest(randomToken()), id)
	// Another account's session is not listed either.
	otherID, _ := e.user("a157_elsewhere", "buyer")
	agExec(e, "UPDATE sessions SET created='2026-03-06 12:00+00' WHERE user_id=$1", otherID)

	page := func(session string) string {
		t.Helper()
		w := e.do("GET", "/account", session, nil)
		e.check(w, 200)
		return w.Body.String()
	}
	body := page(mine)
	count, rows := accountSessionRows(t, body)
	if count != "2 active sessions" {
		t.Fatalf("count %q, want 2 active sessions", count)
	}
	// This browser's session first, then the others, newest first.
	want := []string{"Started 2026-03-04 05:06 UTC This session", "Started 2026-03-05 23:59 UTC"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("rows %q, want %q", rows, want)
	}
	// From the other browser the marker is on the other session.
	if _, rows = accountSessionRows(t, page(other)); len(rows) != 2 || rows[0] != "Started 2026-03-05 23:59 UTC This session" || rows[1] != "Started 2026-03-04 05:06 UTC" {
		t.Fatalf("other browser's rows %q", rows)
	}
	for _, leak := range []string{"A157ProbeAgent", "198.51.100.157", "203.0.113.157", "2026-03-06"} {
		if strings.Contains(body, leak) {
			t.Fatalf("account page shows %q", leak)
		}
	}
	for _, copy := range []string{
		"only when each session started and when it ends",
		"Only changing your password stops them",
		"ends every other session and every sign-in in progress",
	} {
		if !strings.Contains(body, copy) {
			t.Fatalf("sessions panel lacks %q", copy)
		}
	}
	// The market keeps no client data on a session: the table holds only these columns.
	if cols := agStr(e, "SELECT string_agg(column_name::text, ',' ORDER BY column_name) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sessions'"); cols != "created,expires,token_hash,user_id" {
		t.Fatalf("sessions columns %s", cols)
	}

	// The other browser signs out: one session remains, this one.
	e.check(e.do("POST", "/logout", other, nil), 303)
	count, rows = accountSessionRows(t, page(mine))
	if count != "1 active session" || len(rows) != 1 || rows[0] != "Started 2026-03-04 05:06 UTC This session" {
		t.Fatalf("after the other signed out: %q %q", count, rows)
	}
	if strings.Contains(page(mine), "session-more") {
		t.Fatal("a short list says it was cut")
	}

	// Someone who knows the password can open many sessions: the page lists this one and the newest others, at
	// most sessionsListed, and counts them all. This (oldest) session is still listed and marked.
	for i := range sessionsListed + 5 {
		agExec(e, "INSERT INTO sessions(token_hash,user_id,expires,created) VALUES($1,$2,now()+interval '1 hour',$3::timestamptz+make_interval(mins => $4))", digest(randomToken()), id, "2026-03-07 00:00+00", i)
	}
	body = page(mine)
	count, rows = accountSessionRows(t, body)
	if count != "26 active sessions" || len(rows) != sessionsListed || rows[0] != "Started 2026-03-04 05:06 UTC This session" || rows[1] != "Started 2026-03-07 00:24 UTC" {
		t.Fatalf("many sessions: %q, %d rows, first %q", count, len(rows), rows[:2])
	}
	if !strings.Contains(body, "this session and the newest others, 20 of 26") {
		t.Fatal("a cut list does not say so")
	}
}

func TestAccountSignOutIsNearTopAndKeepsItsCSRF(t *testing.T) {
	e := newTestApp(t)
	_, session := e.user("a129_signout", "buyer")
	w := e.do("GET", "/account", session, nil)
	e.check(w, 200)
	body := w.Body.String()
	logout := strings.Index(body, `class="account-signout" method="post" action="/logout"`) // the page's own, not the header's (A-548)
	sessions := strings.Index(body, `id="sessions"`)
	revoke := strings.Index(body, `action="/revoke-sessions"`)
	if logout < 0 || sessions < 0 || revoke < 0 || logout >= sessions || sessions >= revoke {
		t.Fatalf("ordinary sign-out is not near the top and separate from all-sessions revoke: logout=%d sessions=%d revoke=%d", logout, sessions, revoke)
	}
	formStart := strings.LastIndex(body[:logout], "<form")
	formEnd := strings.Index(body[logout:], "</form>")
	if formStart < 0 || formEnd < 0 {
		t.Fatal("ordinary sign-out form is incomplete")
	}
	form := body[formStart : logout+formEnd]
	if !strings.Contains(form, `method="post"`) || !strings.Contains(form, `name="csrf" value="`+e.A.csrf(session)+`"`) || !strings.Contains(form, `>Sign out</button>`) {
		t.Fatalf("ordinary sign-out lost POST, CSRF or button: %s", form)
	}
}

// A-157: a password change ends every sign-in in progress, including one whose old password was checked just
// before the change committed and that reaches /challenge (or, without a factor, its session) only afterwards.
func TestPasswordChangeEndsSignInCheckedBeforeIt(t *testing.T) {
	e := newTestApp(t)
	id, owner := e.user("a157_pw", "buyer")
	key, _ := agEnableTOTP(e, id)
	intruder, pc := e.pendingSignIn("a157_pw") // password accepted, second factor pending
	old := passwordHash(e, id)
	e.check(e.do("POST", "/account/password", owner, pwForm(testPassword, testPassword+"-changed", "code", agTOTPNow(key))), 303)
	// The race: a sign-in that read the old hash before the change and writes its pending row after the change
	// deleted the pending sign-ins.
	late := randomToken()
	agExec(e, "INSERT INTO pending_logins(token_hash,user_id,expires,password_digest) VALUES($1,$2,now()+interval '10 minutes',$3)", digest(late), id, digest(old))
	agExec(e, "UPDATE users SET totp_last_step=0 WHERE id=$1", id) // the code above may be reused by the probe
	for _, p := range []*http.Cookie{pc, {Name: "pending", Value: late}} {
		agExpect(e, "/challenge", intruder, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 401, "Sign-in verification expired", p)
	}
	if n := agUserSessions(e, id); n != 1 {
		t.Fatalf("%d sessions, want only the owner's", n)
	}
	// A pending sign-in written against the current password still works.
	w := e.do("POST", "/login", randomToken(), url.Values{"handle": {"a157_pw"}, "password": {testPassword + "-changed"}})
	e.check(w, 303)
	fresh := &http.Cookie{Name: "pending", Value: e.cookie(w, "pending")}
	agExec(e, "UPDATE users SET totp_last_step=0 WHERE id=$1", id)
	e.check(e.do("POST", "/challenge", intruder, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, fresh), 303)

	// Migration 013 added password_digest nullable so legacy pending rows survive upgrades. A valid second factor
	// cannot turn one of those rows into a session: without the password hash checked at /login, the challenge has
	// no proof that the password still matches the account.
	legacy := randomToken()
	agExec(e, "INSERT INTO pending_logins(token_hash,user_id,expires) VALUES($1,$2,now()+interval '10 minutes')", digest(legacy), id)
	agExec(e, "UPDATE users SET totp_last_step=0 WHERE id=$1", id)
	beforeSessions := agUserSessions(e, id)
	beforeAudits := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id)
	beforeNotices := agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", id)
	var beforeStep int64
	if err := e.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", id).Scan(&beforeStep); err != nil {
		t.Fatal(err)
	}
	agExpect(e, "/challenge", intruder, url.Values{"method": {"totp"}, "code": {agTOTPNow(key)}}, 401, "Sign-in verification expired", &http.Cookie{Name: "pending", Value: legacy})
	if agUserSessions(e, id) != beforeSessions {
		t.Fatal("legacy pending sign-in created a session")
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id) != beforeAudits || agInt(e, "SELECT count(*) FROM notifications WHERE user_id=$1", id) != beforeNotices {
		t.Fatal("legacy pending sign-in wrote an audit event or notification")
	}
	if agInt(e, "SELECT count(*) FROM pending_logins WHERE token_hash=$1", digest(legacy)) != 1 {
		t.Fatal("refused legacy pending sign-in changed its pending row")
	}
	var afterStep int64
	if err := e.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", id).Scan(&afterStep); err != nil {
		t.Fatal(err)
	}
	if afterStep != beforeStep {
		t.Fatalf("refused legacy pending sign-in consumed TOTP step: before=%d after=%d", beforeStep, afterStep)
	}

	// Without a second factor the session itself is refused when the password changed after it was checked.
	noFactor, _ := e.user("a157_nofactor", "buyer")
	stale := passwordHash(e, noFactor)
	changed, err := hashPassword(testPassword + "-again")
	if err != nil {
		t.Fatal(err)
	}
	agExec(e, "UPDATE users SET password_hash=$2 WHERE id=$1", noFactor, changed)
	w = httptest.NewRecorder()
	if err := e.A.login(context.Background(), w, noFactor, randomToken(), stale); err != errPasswordChanged {
		t.Fatalf("login with a stale password check: %v, want errPasswordChanged", err)
	}
	if agUserSessions(e, noFactor) != 1 || e.cookie(w, "session") != "" {
		t.Fatal("a stale password check started a session")
	}
}
