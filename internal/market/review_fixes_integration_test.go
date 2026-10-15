package market

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// F2: attacker-chosen rate-limit keys neither fill the shared table nor lock out real users.
func TestLimiterFloodDoesNotLockOutUsers(t *testing.T) {
	a := &App{limits: map[string]bucket{}}
	for i := range 4096 {
		a.allow("junk:"+strconv.Itoa(i), 10)
	}
	if !a.allow("fresh", 10) || len(a.limits) > 4096 {
		t.Fatalf("full table refused a new key (size %d)", len(a.limits))
	}

	e := newTestApp(t)
	e.user("victim_user", "buyer")
	anon := randomToken()
	for i := range 4100 {
		pc := &http.Cookie{Name: "pending", Value: randomToken()}
		if w := e.do("POST", "/challenge", anon, url.Values{"method": {"totp"}}, pc); w.Code != 401 {
			t.Fatalf("challenge flood: %d", w.Code)
		}
		if i >= 50 {
			continue
		}
		if w := e.do("POST", "/challenge/pgp", anon, nil, pc); w.Code != 401 {
			t.Fatalf("pgp code flood: %d", w.Code)
		}
		if w := e.do("POST", "/login", anon, url.Values{"handle": {"bad handle " + randomToken()[:8]}, "password": {testPassword}}); w.Code != 400 {
			t.Fatalf("handle flood: %d", w.Code)
		}
	}
	if n := len(e.A.limits); n > 10 {
		t.Fatalf("unauthenticated junk created %d limiter keys", n)
	}
	e.check(e.do("POST", "/login", randomToken(), url.Values{"handle": {"victim_user"}, "password": {testPassword}}), 303)
}

// A-11: limiterFill fills the table with keys prefix0..prefix(n-1), each holding count requests against limit.
func limiterFill(a *App, prefix string, n, count, limit int) {
	for i := range n {
		k := prefix + strconv.Itoa(i)
		a.allow(k, limit)
		b := a.limits[k]
		b.Count = count
		a.limits[k] = b
	}
}

// A-11: a full table of blocking entries refuses a new key instead of evicting a blocked one.
func TestLimiterFullOfBlockingEntriesRefusesNewKey(t *testing.T) {
	a := &App{limits: map[string]bucket{}}
	limiterFill(a, "auth:blocked", 4096, 10, 10)
	if a.allow("auth:fresh", 10) {
		t.Fatal("full table of blocking entries admitted a new key")
	}
	if _, ok := a.limits["auth:fresh"]; ok || len(a.limits) != 4096 {
		t.Fatalf("refused key stored (table size %d)", len(a.limits))
	}
	for i := range 4096 {
		if b, ok := a.limits["auth:blocked"+strconv.Itoa(i)]; !ok || b.Count != 10 {
			t.Fatalf("blocking entry %d evicted or changed: %+v", i, b)
		}
	}
	if a.allow("auth:blocked0", 10) {
		t.Fatal("blocked key admitted")
	}
}

// A-11: junk keys evict each other, never a victim's counter, whether it is blocking or only partly used.
func TestLimiterJunkCannotResetVictim(t *testing.T) {
	a := &App{limits: map[string]bucket{}}
	for range 10 {
		a.allow("auth:blocked_victim", 10)
	}
	for range 5 {
		a.allow("auth:partial_victim", 10)
	}
	for i := range 4096 + 256 { // fills the table, then forces 256 evictions
		a.allow("auth:junk"+strconv.Itoa(i), 10)
	}
	if len(a.limits) > 4096 || a.limits["auth:blocked_victim"].Count != 10 || a.limits["auth:partial_victim"].Count != 5 {
		t.Fatalf("victim counters reset: size=%d blocked=%+v partial=%+v", len(a.limits), a.limits["auth:blocked_victim"], a.limits["auth:partial_victim"])
	}
	if a.allow("auth:blocked_victim", 10) {
		t.Fatal("blocked victim admitted after the flood")
	}
	for range 5 {
		if !a.allow("auth:partial_victim", 10) {
			t.Fatal("partial victim refused within its budget")
		}
	}
	if a.allow("auth:partial_victim", 10) {
		t.Fatal("partial victim's budget was reset by the flood")
	}
}

// A-11: when the table is full, the non-blocking entry with the lowest count is evicted.
func TestLimiterEvictsLowestCountNonBlockingEntry(t *testing.T) {
	a := &App{limits: map[string]bucket{}}
	limiterFill(a, "k", 4095, 2, 10)
	a.allow("low", 10)
	if !a.allow("fresh", 10) {
		t.Fatal("full table with non-blocking entries refused a new key")
	}
	if _, ok := a.limits["low"]; ok || len(a.limits) != 4096 {
		t.Fatalf("lowest-count entry not evicted (size %d)", len(a.limits))
	}
	for i := range 4095 {
		if _, ok := a.limits["k"+strconv.Itoa(i)]; !ok {
			t.Fatalf("entry k%d evicted before the lowest-count entry", i)
		}
	}
}

// A-11: a wrong or absent CAPTCHA does not consume the handle's sign-in budget; a passed CAPTCHA does.
func TestWrongCaptchaDoesNotConsumeSignInBudget(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("cap_budget", "buyer")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	anon := randomToken()
	login := func(cid, answer, password string) int {
		return e.do("POST", "/login", anon, url.Values{"handle": {"cap_budget"}, "password": {password}, "captcha_id": {cid}, "captcha": {answer}}).Code
	}
	for i := range 15 {
		cid, answer := "", ""
		if i%2 == 0 {
			cid, answer = e.captcha("/login", anon), "WRONG1"
		}
		if c := login(cid, answer, testPassword); c != 400 {
			t.Fatalf("attempt %d with a wrong CAPTCHA: %d", i, c)
		}
	}
	if agLimited(e, "auth:cap_budget") {
		t.Fatal("wrong CAPTCHA created a sign-in limiter entry")
	}
	for i := range 10 {
		cid := e.captcha("/login", anon)
		if c := login(cid, e.A.captchaAnswer(cid), "wrong-password-123"); c != 401 {
			t.Fatalf("attempt %d with a correct CAPTCHA and wrong password: %d", i, c)
		}
	}
	cid := e.captcha("/login", anon)
	if c := login(cid, e.A.captchaAnswer(cid), testPassword); c != 429 {
		t.Fatalf("eleventh attempt after a passed CAPTCHA: %d, want 429", c)
	}
	if agUserSessions(e, id) != 1 {
		t.Fatal("limited sign-in created a session")
	}
}

// F4's payout-address controls belonged to the retired pooled-payment flow. PGP sign-in changes remain sensitive.
func TestPGPChangesNeedPassword(t *testing.T) {
	p := newPayEnv(t)

	// PGP sign-in on (verified key), no TOTP.
	_, alicePub := testPGPKey(t, "alice")
	_, bobPub := testPGPKey(t, "bob")
	_, fp, _ := parsePublicKey(alicePub)
	v, vs := p.vendor.ID, p.vendorSess
	p.DB.Exec("UPDATE users SET pgp=$1,pgp_fingerprint=$2,pgp_verified_at=now(),pgp_2fa=true WHERE id=$3", strings.TrimSpace(alicePub), fp, v)
	twoFA := func() bool { return p.count("SELECT count(*) FROM users WHERE id=$1 AND pgp_2fa", v) == 1 }
	if !strings.Contains(p.page("/pgp", vs), `autocomplete="current-password"`) || !strings.Contains(p.page("/account", vs), "pgp-confirm-help") {
		t.Fatal("PGP forms lack the password field")
	}
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {alicePub}}), 303) // key unchanged
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {bobPub}}), 400)
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {""}, "password": {"wrong-password-123"}}), 401)
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}}), 400)
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}, "password": {"wrong-password-123"}}), 401)
	if !twoFA() {
		t.Fatal("PGP sign-in turned off without the password")
	}
	p.check(p.do("POST", "/pgp/2fa", vs, url.Values{"enable": {"0"}, "password": {testPassword}}), 303)
	if twoFA() {
		t.Fatal("PGP sign-in still on")
	}
	p.DB.Exec("UPDATE users SET pgp_2fa=true WHERE id=$1", v)
	p.check(p.do("POST", "/account", vs, url.Values{"pgp": {bobPub}, "password": {testPassword}}), 303)
	if twoFA() {
		t.Fatal("key change kept PGP sign-in")
	}
	if !p.testEnv.auditHas(v, "confirmed with password") {
		t.Fatal("confirmation not audited")
	}
}

// F8: an upgraded account whose stored key no longer parses can still save its profile unchanged.
func TestLegacyUnparseableKeySavesUnchanged(t *testing.T) {
	e := newTestApp(t)
	uid, s := e.user("legacy_user", "buyer")
	const legacy = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nlegacy key the parser rejects\n-----END PGP PUBLIC KEY BLOCK-----"
	e.DB.Exec("UPDATE users SET pgp=$1 WHERE id=$2", legacy, uid)
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {legacy}}), 303)
	e.check(e.do("POST", "/account", s, url.Values{"pgp": {legacy + "\nchanged"}, "password": {testPassword}}), 400)
}
