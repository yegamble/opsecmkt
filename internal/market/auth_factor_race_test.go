package market

import (
	"context"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"
)

// enrollmentReadGate pauses after a real provider's initial enrollment read.
// Enrollment itself uses the normal HTTP actions, without holding a database
// lock or changing the value returned by the provider.
type enrollmentReadGate struct {
	factorProvider
	userID  string
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (g *enrollmentReadGate) Enrolled(ctx context.Context, q rowQuerier, userID string) (bool, error) {
	enrolled, err := g.factorProvider.Enrolled(ctx, q, userID)
	if userID == g.userID && err == nil {
		g.once.Do(func() {
			close(g.reached)
			select {
			case <-g.resume:
			case <-ctx.Done():
			}
		})
	}
	return enrolled, err
}

// A password-only login must recheck the required factors while holding the
// account lock that protects session creation. Both real enrollment actions can
// commit after the initial factor reads without changing the password hash.
func TestPasswordOnlySignInRefusesConcurrentFactorEnrollment(t *testing.T) {
	for _, method := range []string{"totp", "pgp"} {
		t.Run(method, func(t *testing.T) {
			e := newTestApp(t)
			handle := "factor_race_" + method
			uid, owner := e.user(handle, "buyer")
			var enable func()
			if method == "totp" {
				e.check(e.do("POST", "/totp/enroll", owner, nil), 303)
				code, _ := e.totpCodeFor(uid, 0)
				enable = func() {
					e.check(e.do("POST", "/totp/activate", owner, url.Values{"password": {testPassword}, "code": {code}}), 303)
				}
			} else {
				key, public := testPGPKey(t, handle)
				e.check(e.do("POST", "/account", owner, url.Values{"password": {testPassword}, "pgp": {public}}), 303)
				e.check(e.do("POST", "/pgp/challenge", owner, url.Values{"kind": {"sign"}}), 303)
				challenge := agStr(e, "SELECT challenge FROM pgp_challenges WHERE user_id=$1", uid)
				e.check(e.do("POST", "/pgp/verify", owner, url.Values{"signature": {testClearsign(t, key, challenge)}}), 303)
				e.pgpSessionKey(owner) // and decrypted one in this browser, which turning PGP sign-in on needs (SEC706-1)
				enable = func() {
					e.check(e.do("POST", "/pgp/2fa", owner, url.Values{"password": {testPassword}, "enable": {"1"}}), 303)
				}
			}

			before := agUserSessions(e, uid)
			oldFactors := slices.Clone(factors)
			t.Cleanup(func() { factors = oldFactors })
			// Hold the last read, so every initial provider has observed no factor.
			gate := &enrollmentReadGate{factorProvider: factors[len(factors)-1], userID: uid,
				reached: make(chan struct{}), resume: make(chan struct{})}
			factors = slices.Clone(factors)
			factors[len(factors)-1] = gate
			resume := sync.OnceFunc(func() { close(gate.resume) })
			finished := make(chan *httptest.ResponseRecorder, 1)
			done := make(chan struct{})
			t.Cleanup(func() { resume(); <-done })
			go func() {
				defer close(done)
				finished <- e.do("POST", "/login", randomToken(), url.Values{"handle": {handle}, "password": {testPassword}})
			}()
			select {
			case <-gate.reached:
			case <-time.After(5 * time.Second):
				t.Fatal("sign-in did not reach its initial factor reads")
			}
			enable()
			resume()
			w := <-finished
			if w.Code != 401 || e.cookie(w, "session") != "" || agUserSessions(e, uid) != before {
				t.Fatalf("password-only sign-in after %s enrollment: status=%d new cookie=%t sessions=%d, want refusal and %d unchanged sessions",
					method, w.Code, e.cookie(w, "session") != "", agUserSessions(e, uid), before)
			}
			if e.auditHas(uid, "Signed in with password") {
				t.Fatal("refused sign-in recorded a successful password-only sign-in")
			}
			// The owner keeps their session, and a fresh sign-in asks for the factor.
			e.check(e.do("GET", "/account", owner, nil), 200)
			fresh := e.do("POST", "/login", randomToken(), url.Values{"handle": {handle}, "password": {testPassword}})
			e.check(fresh, 303)
			if fresh.Header().Get("Location") != "/challenge" || e.cookie(fresh, "pending") == "" || e.cookie(fresh, "session") != "" {
				t.Fatal("fresh sign-in did not ask for the newly enrolled factor")
			}
		})
	}
}

// Session creation is the final authorization boundary even when a required
// PGP factor's saved key cannot be read. Do not treat it as an absent factor.
func TestPasswordOnlySessionRefusesUnreadableRequiredPGP(t *testing.T) {
	e := newTestApp(t)
	uid, owner := e.user("factor_unreadable_pgp", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true,pgp='unreadable legacy key' WHERE id=$1", uid)
	before := agUserSessions(e, uid)
	w := httptest.NewRecorder()
	if err := e.A.login(context.Background(), w, uid, owner, passwordHash(e, uid)); err != errFactorsChanged {
		t.Fatalf("password-only session with required unreadable PGP: %v, want errFactorsChanged", err)
	}
	if e.cookie(w, "session") != "" || agUserSessions(e, uid) != before || e.auditHas(uid, "Signed in with password") {
		t.Fatal("refused session start changed sessions, issued a cookie or recorded a successful sign-in")
	}
	e.check(e.do("GET", "/account", owner, nil), 200)
}
