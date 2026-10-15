package market

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

const confirmPausedText = "Too many failed password confirmations"

func budgetDispute(x *xmrEscrowEnv) (order, dispute string) {
	x.t.Helper()
	order, buyer := x.undeliveredXMR()
	if code, body := x.xmrDispute(order, buyer); code != 303 {
		x.t.Fatalf("open funded Monero dispute: %d %s", code, body)
	}
	return order, agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
}

// resolveWith is a /resolve refund of an empty dispute confirmed with password.
func resolveWith(dispute, password string) url.Values {
	v := resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Refund the funded order to the buyer."}}, xmrListingPrice)
	v.Set("password", password)
	return v
}

// A wrong authenticator code after a correct password is a failed confirmation too: otherwise someone holding a
// session and the password could guess codes with no limit but the write limit. A right code spends nothing.
func TestWrongCodeAfterCorrectPasswordSpendsConfirmBudget(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("a274_code", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", id)
	key, _ := agEnableTOTP(e, id)
	off := func(code string) url.Values {
		return url.Values{"enable": {"0"}, "password": {testPassword}, "code": {code}}
	}
	wrong := e.wrongTOTP(id)
	for range 10 {
		agExpect(e, "/pgp/2fa", s, off(wrong), 401, "Verification code incorrect")
	}
	agExpect(e, "/pgp/2fa", s, off(agTOTPNow(key)), 429, confirmPausedText)
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", id) != "true" || agInt(e, "SELECT totp_last_step::int FROM users WHERE id=$1", id) != 0 {
		t.Fatal("a refused confirmation changed the setting or used the code")
	}
	// A correct password and code leave no limiter entry.
	otherID, other := e.user("a274_code_ok", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", otherID)
	otherKey, _ := agEnableTOTP(e, otherID)
	agExpect(e, "/pgp/2fa", other, off(agTOTPNow(otherKey)), 303, "")
	if agLimited(e, "confirm:"+otherID) {
		t.Fatal("a correct confirmation with a code left a limiter entry")
	}
}

// A wrong recovery code after a correct password spends the budget like a wrong authenticator code (QA19-3): the
// right code is then refused and stays unused.
func TestWrongRecoveryCodeSpendsConfirmBudget(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("a274_recovery", "buyer")
	_, code := agEnableTOTP(e, id)
	hash := passwordHash(e, id)
	change := func(c string) url.Values { return pwForm(testPassword, "a274-new-password", "code", c) }
	for range 10 {
		agExpect(e, "/account/password", s, change(newRecoveryCode()), 401, "Recovery code incorrect")
	}
	agExpect(e, "/account/password", s, change(code), 429, confirmPausedText)
	unused := agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL", id, recoveryHash(code))
	if unused != 1 || passwordHash(e, id) != hash {
		t.Fatal("a refused confirmation used the recovery code or changed the password")
	}
}

// /totp/disable checks its code itself, outside confirmedTxWith, and follows the same rule (QA19-2): after the right
// password a wrong authenticator or recovery code spends the budget every confirmed action shares, and a right one
// spends nothing.
func TestWrongCodeOnTOTPDisableSpendsConfirmBudget(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("a274_disable", "buyer")
	key, code := agEnableTOTP(e, id)
	wrong := e.wrongTOTP(id)
	for i := range 10 {
		c, msg := wrong, "Verification code incorrect"
		if i%2 == 1 {
			c, msg = newRecoveryCode(), "Recovery code incorrect"
		}
		agExpect(e, "/totp/disable", s, url.Values{"password": {testPassword}, "code": {c}}, 401, msg)
	}
	// The next confirmed action, another form with the right password and code, is paused.
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", id)
	agExpect(e, "/pgp/2fa", s, url.Values{"enable": {"0"}, "password": {testPassword}, "code": {agTOTPNow(key)}}, 429, confirmPausedText)
	if agStr(e, "SELECT (totp_enabled AND pgp_2fa)::text FROM users WHERE id=$1", id) != "true" || agInt(e, "SELECT totp_last_step::int FROM users WHERE id=$1", id) != 0 ||
		agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL", id, recoveryHash(code)) != 1 {
		t.Fatal("a wrong or refused code changed a setting or used a code")
	}
	// A right authenticator code, or a right recovery code, turns TOTP off and leaves no limiter entry.
	for _, recovery := range []bool{false, true} {
		otherID, other := e.user("a274_disable_ok_"+map[bool]string{false: "totp", true: "recovery"}[recovery], "buyer")
		otherKey, otherCode := agEnableTOTP(e, otherID)
		c := agTOTPNow(otherKey)
		if recovery {
			c = otherCode
		}
		agExpect(e, "/totp/disable", other, url.Values{"password": {testPassword}, "code": {c}}, 303, "")
		if agStr(e, "SELECT totp_enabled::text FROM users WHERE id=$1", otherID) != "false" || agLimited(e, "confirm:"+otherID) {
			t.Fatalf("recovery=%t: TOTP still on or the correct confirmation left a limiter entry", recovery)
		}
	}
}

// Concurrent wrong passwords cannot spend more than the budget: each reserves its attempt before bcrypt runs.
func TestConcurrentFailedConfirmationsStayWithinBudget(t *testing.T) {
	e := newTestApp(t)
	id, s := e.user("a274_burst", "buyer")
	agExec(e, "UPDATE users SET pgp_2fa=true WHERE id=$1", id)
	off := func(pw string) url.Values { return url.Values{"enable": {"0"}, "password": {pw}} }
	const n = 40
	start := make(chan struct{})
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- e.do("POST", "/pgp/2fa", s, off("wrong-password-"+randomToken()[:4]+string(rune('a'+i%26)))).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	failed := 0
	for c := range codes {
		switch c {
		case 401:
			failed++
		case 429, 503:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if failed > 10 {
		t.Fatalf("%d concurrent wrong passwords were checked, budget is 10", failed)
	}
	for ; failed < 10; failed++ {
		agExpect(e, "/pgp/2fa", s, off("wrong-password-seq"), 401, "Password incorrect")
	}
	agExpect(e, "/pgp/2fa", s, off(testPassword), 429, confirmPausedText)
	if agStr(e, "SELECT pgp_2fa::text FROM users WHERE id=$1", id) != "true" {
		t.Fatal("a refused confirmation changed the setting")
	}
}

func TestCorrectConfirmationsSpendNoBudget(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("a274_admin", "admin")
	for i := range 12 {
		order, dispute := budgetDispute(x)
		w := x.do("POST", "/resolve", mod, resolveWith(dispute, testPassword))
		if w.Code != 303 || x.state(order) != stateResolved {
			t.Fatalf("correct confirmation %d: status=%d state=%s body=%q", i+1, w.Code, x.state(order), w.Body.String())
		}
	}
	if agLimited(x.testEnv, "confirm:"+modID) {
		t.Fatal("correct confirmations left a limiter entry")
	}
}

func TestFailedConfirmationsPauseConfirmation(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	modID, mod := x.user("a274_wrong", "admin")
	order, dispute := budgetDispute(x)
	for i := range 10 {
		agExpect(e, "/resolve", mod, resolveWith(dispute, "wrong-password-"+string(rune('a'+i))), 401, "Password incorrect")
	}
	agExpect(e, "/resolve", mod, resolveWith(dispute, "wrong-password-k"), 429, confirmPausedText)
	// The right password is refused too for the rest of the window, and it runs no bcrypt: it is refused even while
	// every password slot is busy (429, not 503).
	agExpect(e, "/resolve", mod, resolveWith(dispute, testPassword), 429, confirmPausedText)
	release := agHoldPasswordWork(t)
	agExpect(e, "/resolve", mod, resolveWith(dispute, testPassword), 429, confirmPausedText)
	release()
	if x.state(order) != stateDisputed || e.auditHas(modID, "Resolved") {
		t.Fatal("a refused confirmation resolved the dispute")
	}
	// Another account is unaffected.
	_, other := x.user("a274_other", "admin")
	agExpect(e, "/resolve", other, resolveWith(dispute, testPassword), 303, "")
	// Once the window has passed, the right password works again.
	e.A.mu.Lock()
	b := e.A.limits["confirm:"+modID]
	b.Until = time.Now().Add(-time.Second)
	e.A.limits["confirm:"+modID] = b
	e.A.mu.Unlock()
	order2, dispute2 := budgetDispute(x)
	agExpect(e, "/resolve", mod, resolveWith(dispute2, testPassword), 303, "")
	if x.state(order2) != stateResolved {
		t.Fatal("confirmation after the window did not resolve the dispute")
	}
}

// A correct confirmation between failures neither spends nor resets them: five wrong, twelve right, five wrong
// make ten failures, after which the right password is refused.
func TestMixedConfirmationsCountOnlyFailures(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	e := x.testEnv
	_, mod := x.user("a274_mixed", "admin")
	_, open := budgetDispute(x)
	wrong := func(n int) {
		t.Helper()
		for i := range n {
			agExpect(e, "/resolve", mod, resolveWith(open, "wrong-password-"+string(rune('a'+i))), 401, "Password incorrect")
		}
	}
	wrong(5)
	for i := range 12 {
		order, dispute := budgetDispute(x)
		if w := x.do("POST", "/resolve", mod, resolveWith(dispute, testPassword)); w.Code != 303 || x.state(order) != stateResolved {
			t.Fatalf("correct confirmation %d after failures: status=%d body=%q", i+1, w.Code, w.Body.String())
		}
	}
	wrong(5)
	agExpect(e, "/resolve", mod, resolveWith(open, testPassword), 429, confirmPausedText)
}

// A wrong authenticator code after a correct password is a failed confirmation too: otherwise someone holding a
// session and the password could guess codes with no limit but the write limit. A right code spends nothing.
