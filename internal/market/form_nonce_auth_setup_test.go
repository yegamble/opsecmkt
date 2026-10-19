package market

import (
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormNoncePGPChallengeAndVerificationReplay(t *testing.T) {
	e := newTestApp(t)
	signer, keyPub := testPGPKey(t, "nonce_pgp_owner")
	_, fingerprint, err := parsePublicKey(keyPub)
	if err != nil {
		t.Fatal(err)
	}
	uid, session := e.user("nonce_pgp_owner", "buyer")
	if _, err := e.DB.Exec("UPDATE users SET pgp=$2,pgp_fingerprint=$3 WHERE id=$1", uid, keyPub, fingerprint); err != nil {
		t.Fatal(err)
	}
	if submissionCount(t, e) != 0 {
		t.Fatal("user setup unexpectedly wrote form receipts")
	}
	page := e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	if submissionCount(t, e) != 0 {
		t.Fatal("GET /pgp wrote a form receipt")
	}
	challengeForm := url.Values{"kind": {"invalid"}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/pgp/challenge")}}
	if got := e.do("POST", "/pgp/challenge", session, challengeForm); got.Code != 400 {
		t.Fatalf("invalid challenge kind: %d %s", got.Code, got.Body.String())
	}
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/challenge'") != 0 {
		t.Fatal("refused challenge retained its nonce receipt")
	}
	challengeForm.Set("kind", "sign")
	first := e.do("POST", "/pgp/challenge", session, challengeForm)
	e.check(first, 303)
	if replay := e.do("POST", "/pgp/challenge", session, challengeForm); replay.Code != 303 || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("challenge replay: %d %q", replay.Code, replay.Header().Get("Location"))
	}
	var challenge string
	var created, expires time.Time
	if err := e.DB.QueryRow("SELECT challenge,created,expires FROM pgp_challenges WHERE user_id=$1", uid).Scan(&challenge, &created, &expires); err != nil {
		t.Fatal(err)
	}
	// A different request using a completed nonce is refused before it can rotate the open challenge.
	changedChallenge := url.Values{"kind": {"decrypt"}, formNonceField: {challengeForm.Get(formNonceField)}}
	if got := e.do("POST", "/pgp/challenge", session, changedChallenge); got.Code != 409 {
		t.Fatalf("changed challenge request: %d %s", got.Code, got.Body.String())
	}
	var challengeAfter string
	var createdAfter, expiresAfter time.Time
	if err := e.DB.QueryRow("SELECT challenge,created,expires FROM pgp_challenges WHERE user_id=$1", uid).Scan(&challengeAfter, &createdAfter, &expiresAfter); err != nil {
		t.Fatal(err)
	}
	if challengeAfter != challenge || !createdAfter.Equal(created) || !expiresAfter.Equal(expires) {
		t.Fatal("exact replay or changed-body refusal replaced the current PGP challenge")
	}
	page = e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/challenge'") != 1 || listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/verify'") != 0 {
		t.Fatal("GET /pgp after challenge changed its receipt count")
	}
	verifyForm := url.Values{"signature": {"not a signature"}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/pgp/verify")}}
	if got := e.do("POST", "/pgp/verify", session, verifyForm); got.Code != 400 {
		t.Fatalf("invalid proof: %d %s", got.Code, got.Body.String())
	}
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/verify'") != 0 {
		t.Fatal("refused proof retained its nonce receipt")
	}
	signed := testClearsign(t, signer, challenge)
	verifyForm.Set("signature", signed)
	verified := e.do("POST", "/pgp/verify", session, verifyForm)
	e.check(verified, 303)
	replayed := e.do("POST", "/pgp/verify", session, verifyForm)
	e.check(replayed, 303)
	if verified.Header().Get("Location") != replayed.Header().Get("Location") || noticeSetCookie(replayed) == nil || noticeSetCookie(replayed).Value != "pgp-verified" {
		t.Fatal("verification replay did not return its original success")
	}
	changedProof := url.Values{"signature": {"different signature"}, formNonceField: {verifyForm.Get(formNonceField)}}
	bodyMismatch := e.do("POST", "/pgp/verify", session, changedProof)
	e.check(bodyMismatch, 409)
	otherSession := randomToken()
	if _, err := e.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), uid); err != nil {
		t.Fatal(err)
	}
	if got := e.do("POST", "/pgp/verify", otherSession, verifyForm); got.Code != 409 || sessionNeutralBody(e, got, otherSession) != sessionNeutralBody(e, bodyMismatch, session) {
		t.Fatalf("same actor with a different session got a distinguishable response: %d %q", got.Code, got.Body.String())
	}
	if listingFormCount(t, e, "SELECT count(*) FROM pgp_challenges WHERE user_id=$1", uid) != 0 || listingFormCount(t, e, "SELECT count(*) FROM users WHERE id=$1 AND pgp_verified_at IS NOT NULL", uid) != 1 {
		t.Fatal("verification did not consume the challenge and record proof")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/challenge'") != 1 || listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/verify'") != 1 {
		t.Fatal("PGP replay or refused proof created an extra receipt")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", uid, "Verified PGP key ownership (fingerprint "+fingerprint+", sign challenge)") != 1 || listingFormCount(t, e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid) != 1 {
		t.Fatal("verification replay duplicated its audit or notification")
	}
	var receipts string
	if err := e.DB.QueryRow("SELECT string_agg(s::text, '') FROM form_submissions s").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipts, challenge) || strings.Contains(receipts, signed) {
		t.Fatal("PGP challenge or signature text was retained in a form receipt")
	}
}

func TestFormNoncePGPDecryptedResponseReplayKeepsAnswerPrivate(t *testing.T) {
	e := newTestApp(t)
	signer, keyPub := testPGPKey(t, "nonce_pgp_decrypt_owner")
	_, fingerprint, err := parsePublicKey(keyPub)
	if err != nil {
		t.Fatal(err)
	}
	uid, session := e.user("nonce_pgp_decrypt_owner", "buyer")
	if _, err := e.DB.Exec("UPDATE users SET pgp=$2,pgp_fingerprint=$3 WHERE id=$1", uid, keyPub, fingerprint); err != nil {
		t.Fatal(err)
	}
	if submissionCount(t, e) != 0 {
		t.Fatal("user setup unexpectedly wrote form receipts")
	}
	page := e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	if submissionCount(t, e) != 0 {
		t.Fatal("GET /pgp wrote a form receipt")
	}
	start := url.Values{"kind": {"decrypt"}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/pgp/challenge")}}
	started := e.do("POST", "/pgp/challenge", session, start)
	e.check(started, 303)
	page = e.do("GET", "/pgp", session, nil)
	e.check(page, 200)
	if listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/challenge'") != 1 || listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE action_path='/pgp/verify'") != 0 {
		t.Fatal("GET /pgp after encrypted challenge changed its receipt count")
	}
	if !strings.Contains(page.Body.String(), `name="response"`) {
		t.Fatal("decryption challenge page did not render its response form")
	}
	var encrypted string
	if err := e.DB.QueryRow("SELECT challenge FROM pgp_challenges WHERE user_id=$1 AND kind='decrypt'", uid).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	answer := testDecrypt(t, signer, encrypted)
	verify := url.Values{"response": {answer}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/pgp/verify")}}
	first := e.do("POST", "/pgp/verify", session, verify)
	e.check(first, 303)
	replay := e.do("POST", "/pgp/verify", session, verify)
	e.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "pgp-verified" {
		t.Fatal("decryption verification replay did not return its original success")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", uid, "Verified PGP key ownership (fingerprint "+fingerprint+", decrypt challenge)") != 1 || listingFormCount(t, e, "SELECT count(*) FROM notifications WHERE user_id=$1", uid) != 1 {
		t.Fatal("decryption verification replay duplicated its audit or notification")
	}
	var receipts string
	if err := e.DB.QueryRow("SELECT string_agg(s::text, '') FROM form_submissions s").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipts, encrypted) || strings.Contains(receipts, answer) {
		t.Fatal("encrypted challenge or decrypted answer was retained in a form receipt")
	}
}

func TestFormNonceTOTPEnrollmentReplayAndConcurrentSubmission(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("nonce_totp_owner", "buyer")
	page := e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	if submissionCount(t, e) != 0 {
		t.Fatal("GET /totp wrote a form receipt")
	}
	enroll := url.Values{formNonceField: {renderedFormNonce(t, page.Body.String(), "/totp/enroll")}}
	first := e.do("POST", "/totp/enroll", session, enroll)
	e.check(first, 303)
	var sealed string
	var enabled bool
	if err := e.DB.QueryRow("SELECT totp_pending,totp_enabled FROM users WHERE id=$1", uid).Scan(&sealed, &enabled); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || enabled {
		t.Fatalf("enrollment stored pending=%v enabled=%v", sealed != "", enabled)
	}
	secret, err := e.A.open("totp", sealed)
	if err != nil {
		t.Fatal(err)
	}
	replay := e.do("POST", "/totp/enroll", session, enroll)
	e.check(replay, 303)
	var replayedSealed string
	if err := e.DB.QueryRow("SELECT totp_pending FROM users WHERE id=$1", uid).Scan(&replayedSealed); err != nil {
		t.Fatal(err)
	}
	if replayedSealed != sealed {
		t.Fatal("exact enrollment replay replaced the pending authenticator secret")
	}
	changed := url.Values{"replace": {"yes"}, formNonceField: {enroll.Get(formNonceField)}}
	if got := e.do("POST", "/totp/enroll", session, changed); got.Code != 409 {
		t.Fatalf("changed enrollment request: %d %s", got.Code, got.Body.String())
	}
	page = e.do("GET", "/totp", session, nil)
	e.check(page, 200)
	if got := len(renderedActionNonces(t, page.Body.String(), "/totp/enroll")); got != 1 {
		t.Fatalf("pending TOTP page rendered %d replacement forms", got)
	}
	if submissionCount(t, e) != 1 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Started TOTP enrollment (inactive until a code is confirmed)'", uid) != 1 {
		t.Fatal("enrollment replay or changed body duplicated the receipt/audit")
	}
	var receipts string
	if err := e.DB.QueryRow("SELECT string_agg(s::text, '') FROM form_submissions s").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipts, secret) {
		t.Fatal("pending TOTP secret was retained in a form receipt")
	}

	// Two copies of the same rendered form serialize on the nonce and seal just one pending secret.
	concurrentID, concurrentSession := e.user("nonce_totp_concurrent", "buyer")
	page = e.do("GET", "/totp", concurrentSession, nil)
	e.check(page, 200)
	concurrent := url.Values{formNonceField: {renderedFormNonce(t, page.Body.String(), "/totp/enroll")}}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- e.do("POST", "/totp/enroll", concurrentSession, concurrent).Code
		}()
	}
	wg.Wait()
	close(results)
	for code := range results {
		if code != 303 {
			t.Fatalf("concurrent exact TOTP enrollment returned %d", code)
		}
	}
	var pending string
	var concurrentEnabled bool
	if err := e.DB.QueryRow("SELECT totp_pending,totp_enabled FROM users WHERE id=$1", concurrentID).Scan(&pending, &concurrentEnabled); err != nil {
		t.Fatal(err)
	}
	if pending == "" || concurrentEnabled || listingFormCount(t, e, "SELECT count(*) FROM form_submissions WHERE user_id=$1 AND action_path='/totp/enroll'", concurrentID) != 1 || listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action='Started TOTP enrollment (inactive until a code is confirmed)'", concurrentID) != 1 {
		t.Fatal("concurrent TOTP enrollment duplicated its audit or receipt")
	}
	if _, err := e.A.open("totp", pending); err != nil {
		t.Fatal("concurrent TOTP enrollment did not leave a decryptable pending secret")
	}
}
