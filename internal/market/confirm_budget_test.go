package market

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

// A-274: the per-account password-confirmation budget (confirm:<user>, used by every confirmedTx change: /resolve,
// payment reviews, payout actions, settings, key and factor changes) is spent only by wrong passwords, as A-153 made
// the sign-in budget; a correct confirmation spends nothing, so a moderator can resolve any number of disputes.

const confirmPausedText = "Too many failed password confirmations"

// emptyDispute opens a dispute on a new disputed order with nothing paid, which /resolve closes without a payout.
func emptyDispute(p *payEnv) (order, dispute string) {
	p.t.Helper()
	order, _ = p.newOrder(stateDisputed)
	dispute = randomToken()
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Nothing arrived and nothing was paid either.')", dispute, order); err != nil {
		p.t.Fatal(err)
	}
	return order, dispute
}

// resolveWith is a /resolve refund of an empty dispute confirmed with password.
func resolveWith(dispute, password string) url.Values {
	v := resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Nothing was paid; close the order."}}, 0)
	v.Set("password", password)
	return v
}

func TestCorrectConfirmationsSpendNoBudget(t *testing.T) {
	p := newPayEnv(t)
	modID, mod := p.user("a274_mod", "moderator")
	for i := range 12 {
		order, dispute := emptyDispute(p)
		w := p.do("POST", "/resolve", mod, resolveWith(dispute, testPassword))
		if w.Code != 303 || p.state(order) != stateResolved {
			t.Fatalf("correct confirmation %d: status=%d state=%s body=%q", i+1, w.Code, p.state(order), w.Body.String())
		}
	}
	if agLimited(p.testEnv, "confirm:"+modID) {
		t.Fatal("correct confirmations left a limiter entry")
	}
}

func TestFailedConfirmationsPauseConfirmation(t *testing.T) {
	p := newPayEnv(t)
	e := p.testEnv
	modID, mod := p.user("a274_wrong", "moderator")
	order, dispute := emptyDispute(p)
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
	if p.state(order) != stateDisputed || e.auditHas(modID, "Resolved") {
		t.Fatal("a refused confirmation resolved the dispute")
	}
	// Another account is unaffected.
	_, other := p.user("a274_other", "moderator")
	agExpect(e, "/resolve", other, resolveWith(dispute, testPassword), 303, "")
	// Once the window has passed, the right password works again.
	e.A.mu.Lock()
	b := e.A.limits["confirm:"+modID]
	b.Until = time.Now().Add(-time.Second)
	e.A.limits["confirm:"+modID] = b
	e.A.mu.Unlock()
	order2, dispute2 := emptyDispute(p)
	agExpect(e, "/resolve", mod, resolveWith(dispute2, testPassword), 303, "")
	if p.state(order2) != stateResolved {
		t.Fatal("confirmation after the window did not resolve the dispute")
	}
}

// A correct confirmation between failures neither spends nor resets them: five wrong, twelve right, five wrong
// make ten failures, after which the right password is refused.
func TestMixedConfirmationsCountOnlyFailures(t *testing.T) {
	p := newPayEnv(t)
	e := p.testEnv
	_, mod := p.user("a274_mixed", "moderator")
	_, open := emptyDispute(p)
	wrong := func(n int) {
		t.Helper()
		for i := range n {
			agExpect(e, "/resolve", mod, resolveWith(open, "wrong-password-"+string(rune('a'+i))), 401, "Password incorrect")
		}
	}
	wrong(5)
	for i := range 12 {
		order, dispute := emptyDispute(p)
		if w := p.do("POST", "/resolve", mod, resolveWith(dispute, testPassword)); w.Code != 303 || p.state(order) != stateResolved {
			t.Fatalf("correct confirmation %d after failures: status=%d body=%q", i+1, w.Code, w.Body.String())
		}
	}
	wrong(5)
	agExpect(e, "/resolve", mod, resolveWith(open, testPassword), 429, confirmPausedText)
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
