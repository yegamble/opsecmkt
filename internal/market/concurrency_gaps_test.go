package market

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// True concurrency for single-use and capped operations (audit finding B7, non-payout part). Each test
// releases all goroutines at once and then checks the database, not just how many requests returned 303.

type agResult struct {
	code int
	body string
}

// agRace runs n requests at once (released together) and returns their results in index order.
func agRace(n int, req func(i int) (int, string)) []agResult {
	out := make([]agResult, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i].code, out[i].body = req(i)
		}()
	}
	close(start)
	wg.Wait()
	return out
}

// agTally counts results by status and fails on any status outside allowed.
func agTally(t *testing.T, rs []agResult, allowed ...int) map[int]int {
	t.Helper()
	m := map[int]int{}
	for _, r := range rs {
		ok := false
		for _, a := range allowed {
			ok = ok || r.code == a
		}
		if !ok {
			t.Fatalf("unexpected status %d: %s", r.code, r.body)
		}
		m[r.code]++
	}
	return m
}

func TestConcurrentSetupCreatesExactlyOneAdmin(t *testing.T) {
	e := newTestApp(t)
	n := cap(passwordWork) // every request passes the bcrypt gate, so all race for installation
	rs := agRace(n, func(i int) (int, string) {
		w := e.do("POST", "/setup", randomToken(), url.Values{"token": {testSetupToken}, "handle": {fmt.Sprintf("admin_%d", i)}, "password": {testPassword}, "site_name": {fmt.Sprintf("Market %d", i)}})
		return w.Code, w.Body.String()
	})
	if got := agTally(t, rs, 303, 403); got[303] != 1 {
		t.Fatalf("%d concurrent setups succeeded", got[303])
	}
	if n := agInt(e, "SELECT count(*) FROM users"); n != 1 {
		t.Fatalf("users=%d", n)
	}
	var winner int
	for i, r := range rs {
		if r.code == 303 {
			winner = i
		}
	}
	if agStr(e, "SELECT handle FROM users WHERE role='admin'") != fmt.Sprintf("admin_%d", winner) ||
		agStr(e, "SELECT value FROM settings WHERE key='site_name'") != fmt.Sprintf("Market %d", winner) ||
		agInt(e, "SELECT count(*) FROM settings WHERE key='installed'") != 1 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE action='Account created: admin'") != 1 {
		t.Fatal("installation state does not belong to the single winner")
	}
}

func TestConcurrentPayRespectsPerBuyerCap(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("cap_buyer", "buyer")
	vendorID, _ := e.user("cap_vendor", "vendor")
	e.A.payments["BTC"] = newFakeProvider("BTC", 1)
	const n = 8
	orders, products := make([]string, n), make([]string, n)
	for i := range n {
		products[i] = e.product(vendorID, "physical")
		orders[i] = e.order(buyerID, products[i], "BTC", stateDraft)
	}
	rs := agRace(n, func(i int) (int, string) {
		w := e.do("POST", "/orders/pay", buyer, url.Values{"order_id": {orders[i]}})
		return w.Code, w.Body.String()
	})
	got := agTally(t, rs, 303, 409)
	open := agInt(e, "SELECT count(*) FROM orders WHERE buyer_id=$1 AND state='awaiting_payment'", buyerID)
	if got[303] != maxAwaitingPayment || open != maxAwaitingPayment {
		t.Fatalf("%d payment requests succeeded, %d orders awaiting payment; cap is %d", got[303], open, maxAwaitingPayment)
	}
	for i, r := range rs {
		if r.code == 409 && !strings.Contains(r.body, "awaiting payment") {
			t.Fatalf("refusal %d: %s", i, r.body)
		}
	}
	if agInt(e, "SELECT count(*) FROM payment_addresses") != maxAwaitingPayment ||
		agInt(e, "SELECT count(*) FROM products WHERE stock=4") != maxAwaitingPayment ||
		agInt(e, "SELECT count(*) FROM orders WHERE state='draft'") != n-maxAwaitingPayment {
		t.Fatal("refused requests reserved stock, issued addresses or moved orders")
	}
}

func TestConcurrentRecoveryCodeIsSingleUse(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("race_recovery", "buyer")
	_, code := agEnableTOTP(e, id)
	const n = 8
	pending := make([]*http.Cookie, n)
	for i := range pending {
		pending[i] = agPending(e, id) // separate sign-in attempts, so only the code itself is shared
	}
	rs := agRace(n, func(i int) (int, string) {
		w := e.do("POST", "/challenge", randomToken(), url.Values{"method": {"totp"}, "recovery": {code}}, pending[i])
		return w.Code, w.Body.String()
	})
	if got := agTally(t, rs, 303, 401); got[303] != 1 {
		t.Fatalf("one recovery code signed in %d times", got[303])
	}
	if agInt(e, "SELECT count(*) FROM recovery_codes WHERE user_id=$1 AND used_at IS NOT NULL", id) != 1 ||
		agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != n-1 ||
		agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Used a recovery code%'", id) != 1 {
		t.Fatal("recovery code consumption not recorded exactly once")
	}
}

func TestConcurrentCaptchaIsSingleUse(t *testing.T) {
	e := newTestApp(t)
	id, _ := e.user("race_captcha", "buyer")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	anon := randomToken()
	cid := e.captcha("/login", anon)
	answer := e.A.captchaAnswer(cid)
	rs := agRace(cap(passwordWork), func(int) (int, string) {
		w := e.do("POST", "/login", anon, url.Values{"handle": {"race_captcha"}, "password": {testPassword}, "captcha_id": {cid}, "captcha": {answer}})
		return w.Code, w.Body.String()
	})
	if got := agTally(t, rs, 303, 400); got[303] != 1 {
		t.Fatalf("one CAPTCHA admitted %d sign-ins", got[303])
	}
	if agUserSessions(e, id) != 2 || agStr(e, "SELECT used::text FROM captchas WHERE id=$1", cid) != "true" {
		t.Fatal("CAPTCHA not consumed exactly once")
	}
}

func TestConcurrentPGPLoginCodeIsSingleUse(t *testing.T) {
	e := newTestApp(t)
	key, pub := testPGPKey(t, "racer")
	id, _ := e.user("race_pgp", "buyer")
	agEnablePGP(e, id, pub)
	p := agPending(e, id)
	e.check(e.do("POST", "/challenge/pgp", randomToken(), nil, p), 303)
	code := testDecrypt(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(p.Value)))
	const n = 8
	rs := agRace(n, func(int) (int, string) {
		w := e.do("POST", "/challenge", randomToken(), url.Values{"method": {"pgp"}, "pgp_code": {code}}, p)
		return w.Code, w.Body.String()
	})
	if got := agTally(t, rs, 303, 401); got[303] != 1 {
		t.Fatalf("one PGP sign-in code signed in %d times", got[303])
	}
	if agUserSessions(e, id) != 2 || agInt(e, "SELECT count(*) FROM pending_logins WHERE user_id=$1", id) != 0 {
		t.Fatal("PGP code not consumed exactly once")
	}
}
