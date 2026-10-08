package market

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var accountPayoutFormRE = regexp.MustCompile(`(?s)<form class="stack" method="post" action="/account/payout">(.*?)</form>`)

func accountPayoutNonceForCurrency(t *testing.T, body, currency string) string {
	t.Helper()
	for _, form := range accountPayoutFormRE.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(form[1], `name="currency" value="`+currency+`"`) {
			continue
		}
		nonce := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`).FindStringSubmatch(form[1])
		if nonce == nil {
			t.Fatalf("rendered %s payout form has no valid receipt nonce", currency)
		}
		return nonce[1]
	}
	t.Fatalf("no /account/payout form for %s in page", currency)
	return ""
}

func accountPayoutForm(currency, address, nonce string) url.Values {
	return url.Values{
		"currency": {currency}, "address": {address}, "password": {testPassword}, formNonceField: {nonce},
	}
}

func TestAccountPayoutFormsHaveDistinctNoncesAndExactReplayDoesNotRestoreOldAddress(t *testing.T) {
	p := newPayEnv(t)
	p.A.payments["XMR"] = newFakeProvider("XMR", 10)
	page := p.page("/account", p.vendorSess)
	btcNonce := accountPayoutNonceForCurrency(t, page, "BTC")
	xmrNonce := accountPayoutNonceForCurrency(t, page, "XMR")
	if btcNonce == xmrNonce {
		t.Fatal("BTC and XMR payout forms shared a nonce")
	}
	if got := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/account/payout'"); got != 0 {
		t.Fatalf("account GET wrote %d payout receipts", got)
	}

	blockedOrder := p.insertPayout(stateCompleted, "release", p.vendor.ID, "blocked", "", "No payout address was saved", false, "")
	heldOrder := p.insertPayout(stateCompleted, "release", p.vendor.ID, "held", "", heldReason, false, "")
	queuedOrder := p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-prior-queue", "", false, "")
	btcFirst := accountPayoutForm("BTC", "fake-testnet-first-btc-address", btcNonce)
	first := p.do("POST", "/account/payout", p.vendorSess, btcFirst)
	p.check(first, 303)
	var blockedState, blockedAddress, heldState, heldAddress, queuedAddress string
	if err := p.DB.QueryRow("SELECT state,address FROM payouts WHERE order_id=$1", blockedOrder).Scan(&blockedState, &blockedAddress); err != nil {
		t.Fatal(err)
	}
	if err := p.DB.QueryRow("SELECT state,address FROM payouts WHERE order_id=$1", heldOrder).Scan(&heldState, &heldAddress); err != nil {
		t.Fatal(err)
	}
	if err := p.DB.QueryRow("SELECT address FROM payouts WHERE order_id=$1", queuedOrder).Scan(&queuedAddress); err != nil {
		t.Fatal(err)
	}
	if blockedState != "pending" || blockedAddress != "fake-testnet-first-btc-address" || heldState != "held" || heldAddress != "fake-testnet-first-btc-address" || queuedAddress != "fake-testnet-prior-queue" {
		t.Fatalf("payout address save changed the wrong queue rows: blocked=%s/%q held=%s/%q queued=%q", blockedState, blockedAddress, heldState, heldAddress, queuedAddress)
	}
	xmr := p.do("POST", "/account/payout", p.vendorSess, accountPayoutForm("XMR", "fake-testnet-xmr-address", xmrNonce))
	p.check(xmr, 303)
	if got := p.count("SELECT count(*) FROM form_submissions WHERE action_path='/account/payout'"); got != 2 {
		t.Fatalf("saved BTC/XMR forms produced %d receipts", got)
	}

	var firstChanged string
	if err := p.DB.QueryRow("SELECT to_char(payout_btc_changed,'YYYY-MM-DD HH24:MI:SS.US') FROM users WHERE id=$1", p.vendor.ID).Scan(&firstChanged); err != nil {
		t.Fatal(err)
	}
	if got := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Your % payout address was changed%'", p.vendor.ID); got != 2 {
		t.Fatalf("initial BTC/XMR saves wrote %d address notifications", got)
	}
	if got := p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Saved % payout address%'", p.vendor.ID); got != 2 {
		t.Fatalf("initial BTC/XMR saves wrote %d address audits", got)
	}

	// A separate legitimate save moves the address forward. Replaying the old form
	// must return its receipt without restoring its earlier destination or timestamp.
	secondPage := p.page("/account", p.vendorSess)
	secondNonce := accountPayoutNonceForCurrency(t, secondPage, "BTC")
	second := p.do("POST", "/account/payout", p.vendorSess, accountPayoutForm("BTC", "fake-testnet-second-btc-address", secondNonce))
	p.check(second, 303)
	var latestAddress, latestChanged string
	if err := p.DB.QueryRow("SELECT payout_btc,to_char(payout_btc_changed,'YYYY-MM-DD HH24:MI:SS.US') FROM users WHERE id=$1", p.vendor.ID).Scan(&latestAddress, &latestChanged); err != nil {
		t.Fatal(err)
	}
	if latestAddress != "fake-testnet-second-btc-address" || latestChanged == firstChanged {
		t.Fatalf("second save did not advance the address timestamp: address=%q time=%q first=%q", latestAddress, latestChanged, firstChanged)
	}
	secondChanged := latestChanged
	beforeNotes := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
	beforeAudits := p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Saved % payout address%'", p.vendor.ID)
	replay := p.do("POST", "/account/payout", p.vendorSess, btcFirst)
	p.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("saved payout replay redirect changed: %q vs %q", replay.Header().Get("Location"), first.Header().Get("Location"))
	}
	if err := p.DB.QueryRow("SELECT payout_btc,to_char(payout_btc_changed,'YYYY-MM-DD HH24:MI:SS.US') FROM users WHERE id=$1", p.vendor.ID).Scan(&latestAddress, &latestChanged); err != nil {
		t.Fatal(err)
	}
	if latestAddress != "fake-testnet-second-btc-address" || latestChanged != secondChanged {
		t.Fatalf("old receipt changed the newer address or timestamp: address=%q time=%q want=%q", latestAddress, latestChanged, secondChanged)
	}
	if got := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); got != beforeNotes {
		t.Fatalf("saved payout replay wrote another notification: %d -> %d", beforeNotes, got)
	}
	if got := p.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Saved % payout address%'", p.vendor.ID); got != beforeAudits {
		t.Fatalf("saved payout replay wrote another audit: %d -> %d", beforeAudits, got)
	}
	if got := p.str("SELECT state FROM payouts WHERE order_id=$1", heldOrder); got != "held" {
		t.Fatalf("address replay released a held payout: %s", got)
	}
	if got := p.str("SELECT address FROM payouts WHERE order_id=$1", queuedOrder); got != "fake-testnet-prior-queue" {
		t.Fatalf("address replay moved a queued payout: %q", got)
	}

	var receipt string
	if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", btcNonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testPassword, "fake-testnet-first-btc-address", p.vendorSess} {
		if strings.Contains(receipt, secret) {
			t.Fatalf("payout receipt retained submitted secret or raw address")
		}
	}

	changed := accountPayoutForm("BTC", "fake-testnet-changed-address", btcNonce)
	p.check(p.do("POST", "/account/payout", p.vendorSess, changed), 409)
	secondVendorSession := agSession(p.testEnv, p.vendor.ID)
	p.check(p.do("POST", "/account/payout", secondVendorSession, btcFirst), 409)
	_, otherSession := p.user("payout_other_session", "vendor")
	p.check(p.do("POST", "/account/payout", otherSession, btcFirst), 409)
	wrongPassword := accountPayoutForm("BTC", "fake-testnet-first-btc-address", randomToken()[:32])
	wrongPassword.Set("password", "incorrect")
	p.check(p.do("POST", "/account/payout", p.vendorSess, wrongPassword), 401)
}

func TestAccountPayoutUnavailableProviderRefusalRollsBackAndSavedReplayWorksOffline(t *testing.T) {
	p := newPayEnv(t)
	page := p.page("/account", p.vendorSess)
	nonce := accountPayoutNonceForCurrency(t, page, "BTC")
	f := accountPayoutForm("BTC", "fake-testnet-offline-retry", nonce)
	p.A.payMu.Lock()
	delete(p.A.payments, "BTC")
	p.A.payMu.Unlock()
	refused := p.do("POST", "/account/payout", p.vendorSess, f)
	p.check(refused, 409)
	if got := p.count("SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce); got != 0 {
		t.Fatalf("offline provider refusal retained %d receipt rows", got)
	}

	p.A.payMu.Lock()
	p.A.payments["BTC"] = p.fake
	p.A.payMu.Unlock()
	first := p.do("POST", "/account/payout", p.vendorSess, f)
	p.check(first, 303)
	p.A.payMu.Lock()
	delete(p.A.payments, "BTC")
	p.A.payMu.Unlock()
	replay := p.do("POST", "/account/payout", p.vendorSess, f)
	p.check(replay, 303)
	if replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("offline saved replay redirect changed: %q vs %q", replay.Header().Get("Location"), first.Header().Get("Location"))
	}
	if got := p.str("SELECT payout_btc FROM users WHERE id=$1", p.vendor.ID); got != f.Get("address") {
		t.Fatalf("saved address after offline replay = %q", got)
	}
	if got := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Your BTC payout address was changed%'", p.vendor.ID); got != 1 {
		t.Fatalf("offline exact replay wrote %d payout notices", got)
	}
}

func TestAccountPayoutConfirmedReplayRechecksActorAndTOTPWithoutResaving(t *testing.T) {
	p := newPayEnv(t)
	key, _ := agEnableTOTP(p.testEnv, p.vendor.ID)
	page := p.page("/account", p.vendorSess)
	nonce := accountPayoutNonceForCurrency(t, page, "BTC")
	f := accountPayoutForm("BTC", "fake-testnet-totp-address", nonce)
	f.Set("code", agTOTPNow(key))
	p.check(p.do("POST", "/account/payout", p.vendorSess, f), 303)
	var step int64
	if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", p.vendor.ID).Scan(&step); err != nil {
		t.Fatal(err)
	}
	var receipt string
	if err := p.DB.QueryRow("SELECT row_to_json(f)::text FROM form_submissions f WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, f.Get("code")) {
		t.Fatal("payout receipt retained the authenticator code")
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, f), 303)
	var after int64
	if err := p.DB.QueryRow("SELECT totp_last_step FROM users WHERE id=$1", p.vendor.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != step {
		t.Fatalf("exact receipt replay spent TOTP again: %d -> %d", step, after)
	}
	fresh := accountPayoutForm("BTC", "fake-testnet-totp-address", randomToken()[:32])
	fresh.Set("code", f.Get("code"))
	p.check(p.do("POST", "/account/payout", p.vendorSess, fresh), 401)
	if got := p.str("SELECT payout_btc FROM users WHERE id=$1", p.vendor.ID); got != f.Get("address") {
		t.Fatalf("fresh form with spent code changed address to %q", got)
	}
}

func TestAccountPayoutLostCommitAcknowledgementExplainsSafeReplay(t *testing.T) {
	p := newPayEnv(t)
	page := p.page("/account", p.vendorSess)
	nonce := accountPayoutNonceForCurrency(t, page, "BTC")
	f := accountPayoutForm("BTC", "fake-testnet-lost-ack", nonce)
	lost := loseNextActionCommit(t, p.testEnv)
	w := p.do("POST", "/account/payout", p.vendorSess, f)
	if lost.Load() {
		t.Fatal("Commit did not reach the injected acknowledgement loss")
	}
	if w.Code != 500 || w.Header().Get("Retry-After") != "" ||
		!strings.Contains(w.Body.String(), "Payout address outcome unknown") ||
		!strings.Contains(w.Body.String(), "saved address and payout states") ||
		!strings.Contains(w.Body.String(), "resubmit it unchanged") {
		t.Fatalf("lost payout acknowledgement: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if got := p.str("SELECT payout_btc FROM users WHERE id=$1", p.vendor.ID); got != f.Get("address") {
		t.Fatalf("lost acknowledgement did not commit address: %q", got)
	}
	// The browser keeps the original form open after the 500. Its unchanged retry
	// returns the recorded success without repeating the address change or notice.
	replay := p.do("POST", "/account/payout", p.vendorSess, f)
	p.check(replay, 303)
	if got := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Your BTC payout address was changed%'", p.vendor.ID); got != 1 {
		t.Fatalf("lost-ack replay wrote %d address notices", got)
	}
}
