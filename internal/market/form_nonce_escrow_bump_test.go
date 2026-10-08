package market

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type bumpNonceResponse struct {
	code             int
	location, notice string
}

func TestEscrowBumpSignatureFormNonceReplayAndCorrectedRetry(t *testing.T) {
	x := newEscrowEnv(t)
	order, settlementID := x.broadcastClaim("form-nonce-bump", 100000)
	bump := x.mustBump(order, x.vendorSess, settlementID, "5")
	if got := bumpSubmissionCount(t, x.testEnv); got != 0 {
		t.Fatalf("escrow setup wrote %d form receipts before the signing form", got)
	}

	page := x.page("/order?id="+order, x.vendorSess)
	nonce := renderedFormNonce(t, page, "/escrow/bump-sign")
	if got := bumpSubmissionCount(t, x.testEnv); got != 0 {
		t.Fatalf("GET /order wrote %d form receipts", got)
	}
	copyCode, copyText := x.bumpCopy(order, x.vendorSess, settlementID, bump.ID)
	if copyCode != 200 {
		t.Fatalf("vendor bump copy status = %d", copyCode)
	}
	_, vendorFingerprint := x.partySess(roleVendor)
	bad := walletSign(t, copyText, vendorFingerprint, 2) // SIGHASH_NONE is refused before any receipt is committed.
	form := url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bump.ID, 10)}, "psbt": {bad}, formNonceField: {nonce}}
	if w := x.do("POST", "/escrow/bump-sign", x.vendorSess, form); w.Code != 400 {
		t.Fatalf("invalid signature status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if got := bumpSubmissionCount(t, x.testEnv); got != 0 || x.bumpByID(bump.ID).State != "awaiting_signature" {
		t.Fatalf("invalid signature left %d receipts and bump state %q", got, x.bumpByID(bump.ID).State)
	}

	good := walletSign(t, copyText, vendorFingerprint, 1)
	form.Set("psbt", good) // Validation rollback leaves the same nonce available for the corrected form.
	start := make(chan struct{})
	results := make(chan bumpNonceResponse, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := x.do("POST", "/escrow/bump-sign", x.vendorSess, form)
			notice := ""
			if c := noticeSetCookie(w); c != nil {
				notice = c.Value
			}
			results <- bumpNonceResponse{code: w.Code, location: w.Header().Get("Location"), notice: notice}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first bumpNonceResponse
	firstSeen := false
	for got := range results {
		if got.code != 303 || got.notice != "escrow-bump-signed" {
			t.Fatalf("vendor signature response: %+v", got)
		}
		if !firstSeen {
			first = got
			firstSeen = true
		} else if got.location != first.location || got.notice != first.notice {
			t.Fatalf("concurrent exact replay changed response: first=%+v replay=%+v", first, got)
		}
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party='vendor'", bump.ID); got != 1 {
		t.Fatalf("vendor signature rows after concurrent replay = %d, want one", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note LIKE 'The vendor signed the fee bump of the escrow settlement%'", order, x.vendor); got != 1 {
		t.Fatalf("vendor fee-bump events = %d, want one", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%The vendor signed the fee bump of the escrow settlement%'", x.buyer); got != 1 {
		t.Fatalf("buyer notifications from vendor signature = %d, want one", got)
	}
	vendorAudit := "Signed a fee bump of the escrow settlement of order " + shortID(order) + " (" + x.A.escrowNetwork() + ")"
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", x.vendor, vendorAudit); got != 1 {
		t.Fatalf("vendor signature audits = %d, want one", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce); got != 1 {
		t.Fatalf("vendor signature receipts = %d, want one", got)
	}
	var receipt string
	if err := x.DB.QueryRow("SELECT concat_ws('|',nonce,user_id,session_hash,action_path,request_hmac,redirect,notice) FROM form_submissions WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, good) {
		t.Fatal("receipt retained the raw signed PSBT")
	}

	changed := url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bump.ID, 10)}, "psbt": {good + "changed"}, formNonceField: {nonce}}
	bodyMismatch := x.do("POST", "/escrow/bump-sign", x.vendorSess, changed)
	if bodyMismatch.Code != 409 {
		t.Fatalf("changed PSBT status = %d, want 409: %s", bodyMismatch.Code, bodyMismatch.Body.String())
	}
	otherSession := randomToken()
	if _, err := x.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), x.vendor); err != nil {
		t.Fatal(err)
	}
	sessionMismatch := x.do("POST", "/escrow/bump-sign", otherSession, form)
	if sessionMismatch.Code != 409 || sessionMismatch.Body.String() != bodyMismatch.Body.String() {
		t.Fatalf("different-session replay response = %d %q; body mismatch = %d %q", sessionMismatch.Code, sessionMismatch.Body.String(), bodyMismatch.Code, bodyMismatch.Body.String())
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1", bump.ID); got != 1 || x.bumpByID(bump.ID).State != "party_signed" {
		t.Fatalf("mismatch changed signature count/state: %d/%q", got, x.bumpByID(bump.ID).State)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note LIKE 'The vendor signed the fee bump of the escrow settlement%'", order, x.vendor); got != 1 {
		t.Fatalf("mismatch duplicated vendor event: %d", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%The vendor signed the fee bump of the escrow settlement%'", x.buyer); got != 1 {
		t.Fatalf("mismatch duplicated buyer notification: %d", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", x.vendor, vendorAudit); got != 1 {
		t.Fatalf("mismatch duplicated vendor audit: %d", got)
	}

	// The other party gets their own rendered receipt and can sign after the claimant. Its exact retry must return
	// the original both-signed notice instead of adding another signature, event, notification or audit.
	buyerPage := x.page("/order?id="+order, x.buyerSess)
	buyerNonce := renderedFormNonce(t, buyerPage, "/escrow/bump-sign")
	buyerCopyCode, buyerCopy := x.bumpCopy(order, x.buyerSess, settlementID, bump.ID)
	if buyerCopyCode != 200 {
		t.Fatalf("buyer bump copy status = %d", buyerCopyCode)
	}
	_, buyerFingerprint := x.partySess(roleBuyer)
	buyerSigned := walletSign(t, buyerCopy, buyerFingerprint, 1)
	buyerForm := url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bump.ID, 10)}, "psbt": {buyerSigned}, formNonceField: {buyerNonce}}
	buyerFirst := x.do("POST", "/escrow/bump-sign", x.buyerSess, buyerForm)
	if buyerFirst.Code != 303 || noticeValue(buyerFirst) != "escrow-bump-both" {
		t.Fatalf("counterparty signature response = %d %q", buyerFirst.Code, buyerFirst.Body.String())
	}
	buyerReplay := x.do("POST", "/escrow/bump-sign", x.buyerSess, buyerForm)
	if buyerReplay.Code != 303 || buyerReplay.Header().Get("Location") != buyerFirst.Header().Get("Location") || noticeValue(buyerReplay) != "escrow-bump-both" {
		t.Fatalf("counterparty exact replay changed result: %d %q", buyerReplay.Code, buyerReplay.Header().Get("Location"))
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party='buyer'", bump.ID); got != 1 {
		t.Fatalf("buyer signature rows after replay = %d, want one", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note LIKE 'The buyer also signed the fee bump of the escrow settlement%'", order, x.buyer); got != 1 {
		t.Fatalf("buyer fee-bump events = %d, want one", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%The buyer also signed the fee bump of the escrow settlement%'", x.vendor); got != 1 {
		t.Fatalf("vendor notifications from counterparty signature = %d, want one", got)
	}
	buyerAudit := "Signed a fee bump of the escrow settlement of order " + shortID(order) + " (" + x.A.escrowNetwork() + ")"
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", x.buyer, buyerAudit); got != 1 {
		t.Fatalf("buyer signature audits = %d, want one", got)
	}
}

func bumpSubmissionCount(t *testing.T, e *testEnv) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM form_submissions WHERE action_path='/escrow/bump-sign'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func noticeValue(w *httptest.ResponseRecorder) string {
	if c := noticeSetCookie(w); c != nil {
		return c.Value
	}
	return ""
}
