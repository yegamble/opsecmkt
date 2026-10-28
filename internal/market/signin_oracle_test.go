package market

import (
	"bytes"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-611: on the public listener an administrator's correct password must be indistinguishable from a wrong one: the
// same response (status, headers, cookies, body), the same refusal logging and the same budget spent, on the plain
// sign-in and on the paused sign-in's authenticator-code rescue (A-571), with or without the one-step code (A-549).

// oracleProbe is what one public /login shows: the full response and what it changed on the server.
type oracleProbe struct {
	status          int
	header, body    string
	log             string
	signIn, rescue  int // spent attempts of the handle's sign-in budget and the account's code budget
	rescueCode      int // spent attempts of the account's rescue-code budget
	sessions, steps int // sessions and pending sign-ins in the database, and the account's used TOTP step
}

func TestPublicAdminSignInIsNoPasswordOracle(t *testing.T) {
	e := newTestApp(t)
	e.A.adminSocket = "/run/market-admin/http.sock"
	id, _ := e.factorlessStaff("oracle_admin", "admin") // TOTP below is its only factor
	agEnableTOTP(e, id)
	token := randomToken() // one browser for every probe, so its CSRF and cookies are the same
	count := func(key string) int {
		e.A.mu.Lock()
		defer e.A.mu.Unlock()
		return e.A.limits[key].Count
	}
	probe := func(password, code string) oracleProbe {
		t.Helper()
		var logs bytes.Buffer
		out, flags := log.Writer(), log.Flags()
		log.SetOutput(&logs)
		log.SetFlags(0)
		defer func() { log.SetOutput(out); log.SetFlags(flags) }()
		e.A.mu.Lock()
		e.A.signInLogAt, e.A.rescueLogAt, e.A.rescueRefused = time.Time{}, time.Time{}, 0
		e.A.mu.Unlock()
		before := oracleProbe{signIn: count(signInKey("oracle_admin")), rescue: count("totp-verify:" + id), rescueCode: count(rescueCodeKey(id))}
		w := ingressDo(e, false, "POST", "/login", token, url.Values{"handle": {"oracle_admin"}, "password": {password}, "rescue_code": {code}})
		p := oracleProbe{status: w.Code, header: headerDump(w), body: w.Body.String(), log: logs.String(),
			signIn: count(signInKey("oracle_admin")) - before.signIn, rescue: count("totp-verify:"+id) - before.rescue,
			rescueCode: count(rescueCodeKey(id)) - before.rescueCode}
		var last int64
		if err := e.DB.QueryRow("SELECT (SELECT count(*) FROM sessions WHERE user_id=$1)+(SELECT count(*) FROM pending_logins WHERE user_id=$1),totp_last_step FROM users WHERE id=$1", id).Scan(&p.sessions, &last); err != nil {
			t.Fatal(err)
		}
		p.steps = int(last)
		return p
	}
	same := func(leg string, right, wrong oracleProbe) {
		t.Helper()
		if right.status != 401 || wrong.status != 401 {
			t.Fatalf("%s: status right=%d wrong=%d, want 401 both; right body=%q", leg, right.status, wrong.status, refusalText(right.body))
		}
		if right != wrong {
			t.Fatalf("%s: a correct administrator password is distinguishable from a wrong one:\nright=%+v\nwrong=%+v", leg, right, wrong)
		}
	}
	baseline, _ := countSessions(e, id)

	// Plain sign-in, blank code and with a current code (A-549's one-step field): both spend one sign-in attempt.
	for _, code := range []string{"", "current"} {
		c := code
		if c == "current" {
			c, _ = e.totpCodeFor(id, 0)
		}
		right, wrong := probe(testPassword, c), probe("wrong-password-x", c)
		same("plain sign-in code="+code, right, wrong)
		if right.signIn != 1 || right.sessions != baseline {
			t.Fatalf("plain sign-in code=%s: spent %d sign-in attempts and left %d sessions, want 1 and %d", code, right.signIn, right.sessions, baseline)
		}
	}

	// Pause the handle with wrong passwords, then try the paused sign-in's rescue with the right code: the right password
	// must spend the account's code budget and log a rescue refusal exactly as a wrong one does.
	for range 10 {
		probe("wrong-password-y", "")
	}
	if !e.A.limited(signInKey("oracle_admin"), 10) {
		t.Fatal("the handle is not paused")
	}
	code, _ := e.totpCodeFor(id, 0)
	right, wrong := probe(testPassword, code), probe("wrong-password-z", code)
	same("paused sign-in rescue", right, wrong)
	if right.rescue != 1 || !strings.Contains(right.log, "paused sign-in rescue refused attempts=1") || right.sessions != baseline {
		t.Fatalf("paused sign-in rescue: spent %d code attempts, logged %q, left %d sessions; want 1, a rescue refusal, %d", right.rescue, right.log, right.sessions, baseline)
	}

	// Regression: the same administrator still signs in on the private admin listener.
	e.A.limits = map[string]bucket{}
	w := ingressDo(e, true, "POST", "/login", randomToken(), url.Values{"handle": {"oracle_admin"}, "password": {testPassword}, "rescue_code": {code}})
	if w.Code != 303 || e.cookie(w, "session") == "" {
		t.Fatalf("private administrator sign-in failed: status=%d body=%q", w.Code, refusalText(w.Body.String()))
	}
}

func countSessions(e *testEnv, id string) (int, error) {
	var n int
	err := e.DB.QueryRow("SELECT (SELECT count(*) FROM sessions WHERE user_id=$1)+(SELECT count(*) FROM pending_logins WHERE user_id=$1)", id).Scan(&n)
	return n, err
}

// headerDump is the response's headers in a stable order, Set-Cookie included.
func headerDump(w *httptest.ResponseRecorder) string {
	var b strings.Builder
	if err := w.Result().Header.Write(&b); err != nil {
		return err.Error()
	}
	return b.String()
}
