package market

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A-31: a moderator or administrator who is not party to the order records one disposition per flagged payment
// on the moderation desk, confirmed with their password: no action (with a reason), refunded by hand (the
// transaction ID and amount; nothing is sent and no payout row is written) or escalated to an administrator
// (moderators only; an administrator then disposes). The flag leaves the payment-review list, the order history
// and the buyer's deposit label record the outcome (the refund's transaction ID only for the buyer and staff),
// and a later late deposit starts a new notice episode.

// flagPaymentID returns the payments.id of order's deposit txid.
func (p *payEnv) flagPaymentID(order, txid string) string {
	p.t.Helper()
	return p.str("SELECT id::text FROM payments WHERE order_id=$1 AND txid=$2", order, txid)
}

// settledLateFlag pays order through the watcher, cancels it (the refund is sent to the buyer) and records a late
// deposit txid of amt that is flagged for review; it returns the deposit's payments.id.
func (p *payEnv) settledLateFlag(order, addr, paid, txid string, amt int64) string {
	p.t.Helper()
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcher(order, addr, paid)
	p.move(order, statePaid, stateCancelled, p.vendor)
	p.poll()
	if st, _, _, _ := p.payout(order); st != "sent" {
		p.t.Fatalf("refund %s", st)
	}
	return p.lateDeposit(order, addr, txid, amt)
}

// lateDeposit records a confirmed deposit txid of amt to a settled order's address and returns its payments.id.
func (p *payEnv) lateDeposit(order, addr, txid string, amt int64) string {
	p.t.Helper()
	p.fake.Deposit(addr, txid, 0, amt, 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND flagged AND NOT credited", order, txid); n != 1 {
		p.t.Fatalf("late deposit %s not flagged", txid)
	}
	return p.flagPaymentID(order, txid)
}

// dispose posts a disposition as session (password testPassword unless kv sets one).
func (p *payEnv) dispose(session, paymentID, outcome string, kv ...string) (int, string) {
	p.t.Helper()
	f := form(append([]string{"payment_id", paymentID, "outcome", outcome, "password", testPassword}, kv...)...)
	w := p.do("POST", "/moderator/payment-review", session, f)
	if w.Code == 303 {
		return w.Code, w.Header().Get("Location")
	}
	return w.Code, w.Body.String()
}

func (p *payEnv) disposition(paymentID string) string {
	p.t.Helper()
	return p.str("SELECT disposition FROM payments WHERE id=$1::bigint", paymentID)
}

func (p *payEnv) disposedOn(paymentID string) string {
	p.t.Helper()
	return p.str("SELECT to_char(disposed_at,'YYYY-MM-DD') FROM payments WHERE id=$1::bigint", paymentID)
}

// staffEvents counts order events on order recorded by actor.
func (p *payEnv) staffEvents(order, actor string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2", order, actor)
}

func (p *payEnv) reviewAudits(actor string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Recorded payment review%'", actor)
}

func (p *payEnv) adminID() string {
	p.t.Helper()
	return p.str("SELECT user_id FROM sessions WHERE token_hash=$1", digest(p.adminSess))
}

func TestPaymentReviewDisposition(t *testing.T) {
	t.Run("no action", func(t *testing.T) {
		p := newPayEnv(t)
		modID, modSess := p.user("nmod_"+randomToken()[:6], "moderator")
		late := longTxid("f1")
		id := p.settledLateFlag(p.order, p.addr, longTxid("a1"), late, 20000)
		mustContain(t, p.page("/moderator", modSess), late+":0", `action="/moderator/payment-review"`, `name="payment_id" value="`+id+`"`)

		// Buyers and vendors never reach the action; a form without its CSRF token is refused.
		for name, sess := range map[string]string{"buyer": p.buyerSess, "vendor": p.vendorSess} {
			if code, body := p.dispose(sess, id, "no_action", "note", "Nothing to do here at all."); code != 403 {
				t.Fatalf("%s disposed a flag: %d %s", name, code, body)
			}
		}
		r := httptest.NewRequest("POST", "/moderator/payment-review", strings.NewReader(form("payment_id", id, "outcome", "no_action", "note", "No CSRF token sent here.", "password", testPassword).Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: modSess})
		w := httptest.NewRecorder()
		p.A.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("post without CSRF: %d", w.Code)
		}
		// Invalid input and a wrong password change nothing.
		for _, c := range []struct {
			outcome string
			kv      []string
			code    int
		}{
			{"no_action", nil, 400},
			{"no_action", []string{"note", "short"}, 400},
			{"ignore", []string{"note", "A reason that is long enough."}, 400},
			{"no_action", []string{"note", "A reason that is long enough.", "password", "wrong-password-123"}, 401},
			{"no_action", []string{"note", "A reason that is long enough.", "payment_id", "999999"}, 404},
		} {
			if code, body := p.dispose(modSess, id, c.outcome, c.kv...); code != c.code {
				t.Fatalf("%s %v: %d %s", c.outcome, c.kv, code, body)
			}
		}
		if d := p.disposition(id); d != "" || p.staffEvents(p.order, modID) != 0 || p.reviewAudits(modID) != 0 {
			t.Fatalf("refused dispositions changed something: %q", d)
		}

		vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
		reason := "The buyer confirmed this was a test transfer to keep."
		if code, loc := p.dispose(modSess, id, "no_action", "note", reason); code != 303 || loc != "/moderator?saved=1#payment-review" {
			t.Fatalf("no action: %d %s", code, loc)
		}
		if d := p.disposition(id); d != "no_action" {
			t.Fatalf("disposition %q", d)
		}
		if e, a := p.staffEvents(p.order, modID), p.reviewAudits(modID); e != 1 || a != 1 {
			t.Fatalf("events %d audit rows %d", e, a)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2 AND body LIKE $3", p.buyer.ID, "Order "+p.order[:8]+": market staff reviewed%", "%"+reason+"%"); n != 1 {
			t.Fatalf("buyer notifications of the review: %d", n)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); n != vendorBefore {
			t.Fatalf("vendor notified: %d, was %d", n, vendorBefore)
		}
		for _, sess := range []string{modSess, p.adminSess} {
			desk := p.page("/moderator", sess)
			mustContain(t, desk, "No payments are flagged for review.")
			mustNotContain(t, desk, late+":0")
		}
		day := p.disposedOn(id)
		for _, sess := range []string{p.buyerSess, p.vendorSess, p.adminSess} {
			body := p.page("/order?id="+p.order, sess)
			mustContain(t, body, "Arrived after the order closed — not paid out; reviewed by market staff on "+day+": no action", reason)
			mustNotContain(t, body, lateLabel)
		}
		// One disposition per flag.
		if code, body := p.dispose(p.adminSess, id, "no_action", "note", "Another reason, long enough."); code != 409 {
			t.Fatalf("second disposition: %d %s", code, body)
		}
		if e := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NOT NULL AND note LIKE '%review%'", p.order); e != 1 {
			t.Fatalf("review events after a refused second disposition: %d", e)
		}

		// Every late flag of the order is disposed: the next late deposit is announced again.
		staffBefore := p.allReviewNotices(p.order)
		p.lateDeposit(p.order, p.addr, longTxid("f2"), 30000)
		if n := p.lateNotices(p.buyer.ID, p.order); n != 2 {
			t.Fatalf("buyer late-deposit notices after the episode ended: %d", n)
		}
		if n := p.allReviewNotices(p.order) - staffBefore; n != 3 {
			t.Fatalf("staff review notices for the new episode: %d, want one each for three staff", n)
		}
		mustContain(t, p.page("/moderator", modSess), "1 open flag", longTxid("f2")+":0")
	})

	t.Run("refunded by hand", func(t *testing.T) {
		p := newPayEnv(t)
		modID, modSess := p.user("rmod_"+randomToken()[:6], "moderator")
		late := longTxid("f3")
		id := p.settledLateFlag(p.order, p.addr, longTxid("a3"), late, 20000)
		refund := longTxid("9f")
		for _, kv := range [][]string{
			{"amount", "0.00019"},
			{"txid", "not-a-txid", "amount", "0.00019"},
			{"txid", refund},
			{"txid", refund, "amount", "0"},
			{"txid", refund, "amount", "0.00021"},
			{"txid", refund, "amount", "0.000190001"},
		} {
			if code, body := p.dispose(modSess, id, "refunded", kv...); code != 400 {
				t.Fatalf("refunded %v: %d %s", kv, code, body)
			}
		}
		sends, payouts := len(p.fake.Sends()), p.count("SELECT count(*) FROM payouts")
		vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
		if code, loc := p.dispose(modSess, id, "refunded", "txid", strings.ToUpper(refund), "amount", "0.00019"); code != 303 {
			t.Fatalf("refunded: %d %s", code, loc)
		}
		// Nothing is sent and no payout row is written.
		if len(p.fake.Sends()) != sends || p.count("SELECT count(*) FROM payouts") != payouts {
			t.Fatal("a hand refund wrote a payout or called the wallet")
		}
		p.poll()
		if len(p.fake.Sends()) != sends || p.count("SELECT count(*) FROM payouts") != payouts {
			t.Fatal("the watcher paid a hand-refunded deposit")
		}
		if e, a := p.staffEvents(p.order, modID), p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", modID, "Recorded payment review%"+refund+"%"); e != 1 || a != 1 {
			t.Fatalf("events %d audit rows naming the refund %d", e, a)
		}
		day := p.disposedOn(id)
		label := "Refunded by hand on " + day + ": 0.00019 BTC"
		// The buyer (the recipient) and staff reading the order see the refund's transaction ID; the vendor does not.
		for _, sess := range []string{p.buyerSess, p.adminSess, modSess} {
			body := p.page("/order?id="+p.order, sess)
			mustContain(t, body, label, refund)
			mustNotContain(t, body, lateLabel)
		}
		vendorPage := p.page("/order?id="+p.order, p.vendorSess)
		mustContain(t, vendorPage, label, payoutTxidHidden)
		mustNotContain(t, vendorPage, refund, lateLabel)
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", p.buyer.ID, "Order "+p.order[:8]+": %refund%0.00019 BTC%"+refund+"%"); n != 1 {
			t.Fatalf("buyer notifications naming the refund transaction: %d", n)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); n != vendorBefore {
			t.Fatalf("vendor notified: %d, was %d", n, vendorBefore)
		}
		mustContain(t, p.page("/moderator", modSess), "No payments are flagged for review.")
		if code, body := p.dispose(p.adminSess, id, "refunded", "txid", longTxid("9e"), "amount", "0.00019"); code != 409 {
			t.Fatalf("second disposition: %d %s", code, body)
		}
	})

	t.Run("escalated", func(t *testing.T) {
		p := newPayEnv(t)
		adminID := p.adminID()
		modID, modSess := p.user("emod_"+randomToken()[:6], "moderator")
		first := longTxid("f5")
		id := p.settledLateFlag(p.order, p.addr, longTxid("a5"), first, 20000)
		if code, body := p.dispose(p.adminSess, id, "escalated"); code != 403 {
			t.Fatalf("administrator escalated: %d %s", code, body)
		}
		if code, loc := p.dispose(modSess, id, "escalated", "note", "Refund needs the wallet shell."); code != 303 {
			t.Fatalf("escalated: %d %s", code, loc)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", adminID, "Payment review escalated for order "+p.order[:8]+"%"); n != 1 {
			t.Fatalf("administrator escalation notices: %d", n)
		}
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", modID, "Payment review escalated%"); n != 0 {
			t.Fatalf("moderator escalation notices: %d", n)
		}
		// The escalated flag stays open; only an administrator is offered the form.
		desk := p.page("/moderator", modSess)
		mustContain(t, desk, "1 open flag", first+":0", "escalated to an administrator", "An administrator records the outcome")
		mustNotContain(t, desk, `name="payment_id" value="`+id+`"`)
		mustContain(t, p.page("/moderator", p.adminSess), first+":0", `name="payment_id" value="`+id+`"`)
		day := p.disposedOn(id)
		mustContain(t, p.page("/order?id="+p.order, p.buyerSess), "Arrived after the order closed — not paid out; escalated to an administrator on "+day)
		if code, body := p.dispose(modSess, id, "no_action", "note", "Changed my mind about this."); code != 409 {
			t.Fatalf("moderator disposed an escalated flag: %d %s", code, body)
		}
		// The episode lasts while a flag is escalated: another late deposit is not announced again.
		second := p.lateDeposit(p.order, p.addr, longTxid("f6"), 10000)
		if n := p.lateNotices(p.buyer.ID, p.order); n != 1 {
			t.Fatalf("buyer late-deposit notices during the episode: %d", n)
		}
		if code, loc := p.dispose(p.adminSess, id, "refunded", "txid", longTxid("9d"), "amount", "0.0002"); code != 303 {
			t.Fatalf("administrator disposed the escalated flag: %d %s", code, loc)
		}
		if d := p.disposition(id); d != "refunded" || p.staffEvents(p.order, modID) != 1 || p.staffEvents(p.order, adminID) != 1 {
			t.Fatalf("disposition %q", d)
		}
		if code, body := p.dispose(p.adminSess, id, "no_action", "note", "Once more, long enough."); code != 409 {
			t.Fatalf("third disposition: %d %s", code, body)
		}
		if code, _ := p.dispose(modSess, second, "no_action", "note", "Small top-up; the buyer keeps it as credit."); code != 303 {
			t.Fatal("second flag not disposed")
		}
		p.lateDeposit(p.order, p.addr, longTxid("f7"), 10000)
		if n := p.lateNotices(p.buyer.ID, p.order); n != 2 {
			t.Fatalf("buyer late-deposit notices after every flag was disposed: %d", n)
		}
	})

	t.Run("party staff and credited deposits", func(t *testing.T) {
		p := newPayEnv(t)
		partyID, partySess := p.user("ppmod_"+randomToken()[:6], "moderator")
		_, modSess := p.user("cmod_"+randomToken()[:6], "moderator")
		partyOrder, partyAddr := p.orderFor(partyID, p.productID)
		partyTx := longTxid("c1")
		p.paidByWatcher(partyOrder, partyAddr, partyTx)
		p.fake.SetConfirmations(partyTx, -1)
		p.poll()
		id := p.flagPaymentID(partyOrder, partyTx)
		if code, body := p.dispose(partySess, id, "no_action", "note", "I am the buyer of this order."); code != 403 {
			t.Fatalf("party moderator disposed: %d %s", code, body)
		}
		mustNotContain(t, p.page("/moderator", partySess), `name="payment_id" value="`+id+`"`)
		// A credited deposit is part of the order's payout: it is never refunded by hand.
		if code, body := p.dispose(modSess, id, "refunded", "txid", longTxid("9c"), "amount", "0.001"); code != 409 {
			t.Fatalf("hand refund of a credited deposit: %d %s", code, body)
		}
		if code, loc := p.dispose(modSess, id, "no_action", "note", "Double spend confirmed; the order is not shipped."); code != 303 {
			t.Fatalf("no action: %d %s", code, loc)
		}
		mustNotContain(t, p.page("/moderator", modSess), partyTx+":0")
		// Confirming again ends the episode; a later regression is a new, undisposed flag.
		p.fake.SetConfirmations(partyTx, 3)
		p.poll()
		p.fake.SetConfirmations(partyTx, -1)
		p.poll()
		if d := p.disposition(id); d != "" {
			t.Fatalf("new regression kept disposition %q", d)
		}
		mustContain(t, p.page("/moderator", modSess), "1 open flag", partyTx+":0", `name="payment_id" value="`+id+`"`)
	})

	t.Run("hand refund is never paid again", func(t *testing.T) {
		p := newPayEnv(t)
		_, modSess := p.user("fmod_"+randomToken()[:6], "moderator")
		p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
		dust, later := longTxid("d5"), longTxid("d6")
		p.fake.Deposit(p.addr, dust, 0, 1000, 3)
		p.fake.Deposit(p.addr, later, 0, 20000, 1)
		p.poll()
		p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
		p.poll()
		id := p.flagPaymentID(p.order, dust)
		if n := p.count("SELECT count(*) FROM payments WHERE id=$1::bigint AND flagged AND below_floor", id); n != 1 {
			t.Fatal("dust deposit not flagged below the floor")
		}
		if code, loc := p.dispose(modSess, id, "refunded", "txid", longTxid("9b"), "amount", "0.00001"); code != 303 {
			t.Fatalf("refunded: %d %s", code, loc)
		}
		// The deposit sent before the cancellation confirms and lifts the sum over the floor: only it is refunded.
		p.fake.SetConfirmations(later, 3)
		p.poll()
		if st, _, _, amt := p.payout(p.order); amt != 20000 {
			t.Fatalf("refund %s of %d, want 20000 (the hand-refunded dust is not paid again)", st, amt)
		}
		body := p.page("/order?id="+p.order, p.buyerSess)
		mustContain(t, body, "Refunded by hand on "+p.disposedOn(id)+": 0.00001 BTC")
		mustNotContain(t, body, "below the minimum automatic refund")
	})
}
