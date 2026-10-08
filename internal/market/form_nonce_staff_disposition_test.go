package market

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func paymentReviewNonceForm(id, outcome, note, nonce string) url.Values {
	return url.Values{"payment_id": {id}, "outcome": {outcome}, "note": {note},
		"password": {testPassword}, formNonceField: {nonce}}
}

func paymentReviewNonceFixture(t *testing.T) (*payEnv, string, string, string) {
	t.Helper()
	p := newPayEnv(t)
	modID, mod := p.user("review_nonce_"+randomToken()[:6], "moderator")
	id := p.settledLateFlag(p.order, p.addr, txidOf("review-nonce-paid"), txidOf("review-nonce-late"), 20000)
	return p, modID, mod, id
}

type reviewNonceEffects struct{ receipts, audits, events, buyerNotices int }

func reviewEffects(p *payEnv, modID string) reviewNonceEffects {
	return reviewNonceEffects{
		p.count("SELECT count(*) FROM form_submissions WHERE action_path='/moderator/payment-review'"),
		p.reviewAudits(modID), p.staffEvents(p.order, modID),
		p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.buyer.ID),
	}
}

func TestPaymentReviewConfirmedNonceReplayClearedAndReopened(t *testing.T) {
	p, modID, mod, id := paymentReviewNonceFixture(t)
	page := p.page("/moderator", mod)
	forms := regexp.MustCompile(`(?s)<form class="stack" method="post" action="/moderator/payment-review">.*?<input type="hidden" name="form_nonce" value="([0-9a-f]{32})">`).FindAllStringSubmatch(page, -1)
	if len(forms) != 3 || forms[0][1] == forms[1][1] || forms[0][1] == forms[2][1] || forms[1][1] == forms[2][1] {
		t.Fatalf("three rendered review outcomes need distinct nonces; got %d forms", len(forms))
	}
	if n := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/moderator/payment-review'"); n != 0 {
		t.Fatalf("GET wrote %d receipts", n)
	}
	f := paymentReviewNonceForm(id, "no_action", "Confirmed this late deposit needs no wallet action.", forms[0][1])
	before := reviewEffects(p, modID)
	first := p.do("POST", "/moderator/payment-review", mod, f)
	p.check(first, 303)
	after := reviewEffects(p, modID)
	if after.receipts != before.receipts+1 || after.audits != before.audits+1 || after.events != before.events+1 || after.buyerNotices != before.buyerNotices+1 {
		t.Fatalf("first review effects: before=%+v after=%+v", before, after)
	}
	var disposed time.Time
	if err := p.DB.QueryRow("SELECT disposed_at FROM payments WHERE id=$1", id).Scan(&disposed); err != nil {
		t.Fatal(err)
	}
	replay := p.do("POST", "/moderator/payment-review", mod, f)
	p.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") || reviewEffects(p, modID) != after {
		t.Fatal("exact review replay changed redirect or effects")
	}
	changed := paymentReviewNonceForm(id, "no_action", "A different but valid review reason was entered.", f.Get(formNonceField))
	p.check(p.do("POST", "/moderator/payment-review", mod, changed), 409)
	p.check(p.do("POST", "/moderator/payment-review", agSession(p.testEnv, modID), f), 409)
	f.Set("password", "wrong password")
	p.check(p.do("POST", "/moderator/payment-review", mod, f), 401)
	f.Set("password", testPassword)
	if reviewEffects(p, modID) != after {
		t.Fatal("review refusals added effects")
	}
	var still time.Time
	if err := p.DB.QueryRow("SELECT disposed_at FROM payments WHERE id=$1", id).Scan(&still); err != nil || !still.Equal(disposed) {
		t.Fatalf("review refusal changed disposition timestamp: %s / %s: %v", disposed, still, err)
	}
	var saved string
	if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, f.Get("note"), mod} {
		if strings.Contains(saved, secret) {
			t.Fatal("review receipt retained raw form or session data")
		}
	}
	// A watcher countAgain can clear a reviewed flag. Only the full exact saved
	// request passes the pre-password check in that state; a new form does not.
	agExec(p.testEnv, "UPDATE payments SET flagged=false,disposition='',disposition_note='',disposed_at=NULL WHERE id=$1", id)
	p.check(p.do("POST", "/moderator/payment-review", mod, f), 303)
	p.check(p.do("POST", "/moderator/payment-review", mod, changed), 404)
	fresh := paymentReviewNonceForm(id, "no_action", f.Get("note"), randomToken()[:32])
	fresh.Set("password", "wrong password")
	confirmCount := func() int {
		p.A.mu.Lock()
		defer p.A.mu.Unlock()
		return p.A.limits["confirm:"+modID].Count
	}
	budget := confirmCount()
	p.check(p.do("POST", "/moderator/payment-review", mod, fresh), 404)
	agExec(p.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", modID, p.order)
	p.check(p.do("POST", "/moderator/payment-review", mod, fresh), 404)
	p.check(p.do("POST", "/moderator/payment-review", mod, f), 403)
	agExec(p.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", p.buyer.ID, p.order)
	if got := confirmCount(); got != budget {
		t.Fatalf("unflagged fresh/party refusals spent confirmation budget: %d to %d", budget, got)
	}
	fresh.Set("password", testPassword)
	if reviewEffects(p, modID) != after {
		t.Fatal("cleared flag old/new forms changed review effects")
	}
	// A later new flag is a different episode: the old receipt remains a saved
	// result, while a fresh form records a second disposition under the callback.
	agExec(p.testEnv, "UPDATE payments SET flagged=true,disposition='',disposition_note='',disposed_at=NULL WHERE id=$1", id)
	p.check(p.do("POST", "/moderator/payment-review", mod, f), 303)
	fresh.Set("note", "The newly flagged episode was checked separately.")
	p.check(p.do("POST", "/moderator/payment-review", mod, fresh), 303)
	second := reviewEffects(p, modID)
	if second.receipts != after.receipts+1 || second.audits != after.audits+1 || second.events != after.events+1 || second.buyerNotices != after.buyerNotices+1 {
		t.Fatalf("reopened review effects: after=%+v second=%+v", after, second)
	}
	agExec(p.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", modID, p.order)
	p.check(p.do("POST", "/moderator/payment-review", mod, f), 403)
	if reviewEffects(p, modID) != second {
		t.Fatal("current-party refusal changed a reviewed episode")
	}
}

func TestPaymentReviewConfirmedNonceTOTPAndLostAck(t *testing.T) {
	t.Run("current factor and actor", func(t *testing.T) {
		p, modID, mod, id := paymentReviewNonceFixture(t)
		key, _ := agEnableTOTP(p.testEnv, modID)
		f := paymentReviewNonceForm(id, "no_action", "Staff checked this late payment manually.", randomToken()[:32])
		f.Set("code", agTOTPNow(key))
		p.check(p.do("POST", "/moderator/payment-review", mod, f), 303)
		step := agInt(p.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID)
		p.check(p.do("POST", "/moderator/payment-review", mod, f), 303)
		if got := agInt(p.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID); got != step {
			t.Fatalf("review replay spent TOTP again: %d to %d", step, got)
		}
		f.Set(formNonceField, randomToken()[:32])
		p.check(p.do("POST", "/moderator/payment-review", mod, f), 401)
		f.Set(formNonceField, agStrings(t, p.testEnv, "SELECT nonce FROM form_submissions WHERE action_path='/moderator/payment-review'")[0])
		agExec(p.testEnv, "UPDATE users SET role='buyer' WHERE id=$1", modID)
		if w := p.do("POST", "/moderator/payment-review", mod, f); w.Code == 303 {
			t.Fatal("demoted moderator replayed review receipt")
		}
		agExec(p.testEnv, "UPDATE users SET role='moderator',suspended_at=now() WHERE id=$1", modID)
		if w := p.do("POST", "/moderator/payment-review", mod, f); w.Code == 303 {
			t.Fatal("suspended moderator replayed review receipt")
		}
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		p, modID, mod, id := paymentReviewNonceFixture(t)
		f := paymentReviewNonceForm(id, "refunded", "Refund was checked in the wallet.", randomToken()[:32])
		f.Set("txid", txidOf("review-nonce-refund"))
		f.Set("amount", "0.0002")
		before := reviewEffects(p, modID)
		lost := loseNextActionCommit(t, p.testEnv)
		w := p.do("POST", "/moderator/payment-review", mod, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Staff disposition outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost review ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		committed := reviewEffects(p, modID)
		if committed.receipts != before.receipts+1 || committed.audits != before.audits+1 || committed.events != before.events+1 ||
			committed.buyerNotices != before.buyerNotices+1 || p.disposition(id) != "refunded" {
			t.Fatalf("lost ACK did not persist one review: before=%+v committed=%+v", before, committed)
		}
		p.check(p.do("POST", "/moderator/payment-review", mod, f), 303)
		if reviewEffects(p, modID) != committed {
			t.Fatal("lost-ack replay duplicated review")
		}
		var saved string
		if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{testPassword, f.Get("txid"), f.Get("note"), mod} {
			if strings.Contains(saved, secret) {
				t.Fatal("refund receipt retained secret form fields")
			}
		}
	})
}

func collisionNonceFixture(t *testing.T) (*xmrEscrowEnv, string, string, string, string) {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("collision_nonce_"+randomToken()[:6], "moderator")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("nonce collision "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	return x, modID, mod, order, id
}

func TestXMRCollisionConfirmedNonceReplayAndLostAck(t *testing.T) {
	t.Run("exact replay and current eligibility", func(t *testing.T) {
		x, modID, mod, order, id := collisionNonceFixture(t)
		nonce := renderedFormNonce(t, x.page("/order?id="+order, mod), "/moderator/xmr-collision")
		if n := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'"); n != 0 {
			t.Fatalf("GET wrote %d collision receipts", n)
		}
		f := url.Values{"collision_id": {id}, "note": {"Checked the refused deposit in the wallet."},
			"password": {testPassword}, formNonceField: {nonce}}
		events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
		notices := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
		first := x.do("POST", "/moderator/xmr-collision", mod, f)
		x.check(first, 303)
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'") != 1 {
			t.Fatal("first collision disposition did not persist one audit and receipt")
		}
		var disposed time.Time
		if err := x.DB.QueryRow("SELECT disposed_at FROM xmr_escrow_collisions WHERE id=$1", id).Scan(&disposed); err != nil {
			t.Fatal(err)
		}
		replay := x.do("POST", "/moderator/xmr-collision", mod, f)
		x.check(replay, 303)
		if replay.Header().Get("Location") != first.Header().Get("Location") || x.collisionAudits(modID) != 1 ||
			agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order) != events ||
			agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor) != notices {
			t.Fatal("collision replay changed redirect/audit/event/notices")
		}
		var unchanged time.Time
		if err := x.DB.QueryRow("SELECT disposed_at FROM xmr_escrow_collisions WHERE id=$1", id).Scan(&unchanged); err != nil || !unchanged.Equal(disposed) {
			t.Fatal("collision replay changed disposition time")
		}
		changed := url.Values{"collision_id": {id}, "note": {"A different valid action was checked in the wallet."},
			"password": {testPassword}, formNonceField: {nonce}}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, changed), 409)
		x.check(x.do("POST", "/moderator/xmr-collision", agSession(x.testEnv, modID), f), 409)
		f.Set("password", "wrong password")
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 401)
		f.Set("password", testPassword)
		f.Set(formNonceField, randomToken()[:32])
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 409)
		f.Set(formNonceField, nonce)
		agExec(x.testEnv, "UPDATE orders SET buyer_id=$1 WHERE id=$2", modID, order)
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 403)
		if x.collisionAudits(modID) != 1 {
			t.Fatal("current-party refusal added audit")
		}
	})
	t.Run("TOTP and current staff role", func(t *testing.T) {
		x, modID, mod, _, id := collisionNonceFixture(t)
		key, _ := agEnableTOTP(x.testEnv, modID)
		f := url.Values{"collision_id": {id}, "note": {"Wallet ownership of this deposit was checked."},
			"password": {testPassword}, "code": {agTOTPNow(key)}, formNonceField: {randomToken()[:32]}}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		step := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID)
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		if got := agInt(x.testEnv, "SELECT totp_last_step FROM users WHERE id=$1", modID); got != step {
			t.Fatalf("collision replay spent TOTP again: %d to %d", step, got)
		}
		original := f.Get(formNonceField)
		f.Set(formNonceField, randomToken()[:32])
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 401)
		f.Set(formNonceField, original)
		agExec(x.testEnv, "UPDATE users SET role='buyer' WHERE id=$1", modID)
		if w := x.do("POST", "/moderator/xmr-collision", mod, f); w.Code == 303 {
			t.Fatal("demoted moderator replayed collision disposition")
		}
		agExec(x.testEnv, "UPDATE users SET role='moderator',suspended_at=now() WHERE id=$1", modID)
		if w := x.do("POST", "/moderator/xmr-collision", mod, f); w.Code == 303 {
			t.Fatal("suspended moderator replayed collision disposition")
		}
	})
	t.Run("lost commit acknowledgement", func(t *testing.T) {
		x, modID, mod, order, id := collisionNonceFixture(t)
		f := url.Values{"collision_id": {id}, "note": {"Wallet and refused deposit were checked carefully."},
			"password": {testPassword}, formNonceField: {randomToken()[:32]}}
		lost := loseNextActionCommit(t, x.testEnv)
		w := x.do("POST", "/moderator/xmr-collision", mod, f)
		if lost.Load() || w.Code != 500 || w.Header().Get("Retry-After") != "" ||
			!strings.Contains(w.Body.String(), "Staff disposition outcome unknown") || !strings.Contains(w.Body.String(), "resubmit it unchanged") {
			t.Fatalf("lost collision ack status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_collisions WHERE id=$1 AND disposed_at IS NOT NULL", id) != 1 {
			t.Fatal("lost ACK did not persist the collision disposition")
		}
		x.check(x.do("POST", "/moderator/xmr-collision", mod, f), 303)
		if x.collisionAudits(modID) != 1 || agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE action_path='/moderator/xmr-collision'") != 1 {
			t.Fatal("lost ACK replay duplicated the collision disposition")
		}
		var saved string
		if err := x.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", f.Get(formNonceField)).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{testPassword, f.Get("note"), mod} {
			if strings.Contains(saved, secret) {
				t.Fatal("collision receipt retained raw form or session")
			}
		}
		_ = order
	})
}
