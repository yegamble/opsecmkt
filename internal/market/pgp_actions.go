package market

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// P2 PGP identity: the profile key is parsed and fingerprinted on save; ownership is "Verified" only
// after the user signs a server nonce or decrypts a nonce encrypted to the key. Changing the key
// resets verification and PGP sign-in; renewing the same key may keep them (A-155, renewProfileKey). Adding, changing or removing the key and turning PGP sign-in on or off
// need the current password, plus a TOTP code when enrolled (A-47, A-152), and notify the owner. The server never
// sees or stores private keys.

const pgpChallengeTTL = "30 minutes"

func init() {
	registerPage("pgp", pageSpec{Protected: true})
	registerLoader("pgp", pgpLoader)
	registerLoader("account", pgpLoader)
	registerAction("/pgp/challenge", actionSpec{FormNonce: true, Run: pgpChallengeAction})
	registerAction("/pgp/verify", actionSpec{FormNonce: true, Run: pgpVerifyAction})
	registerAction("/pgp/2fa", actionSpec{Roles: []string{"buyer", "vendor", "moderator", "admin"}, OwnTx: true, ConfirmedFormNonce: true, FactorFormNonce: true, Run: pgpTwoFactorAction})
	registerPreview("pgp", previewPGP)
	registerPreview("account", func(d *PageData) { d.PGP = &PGPView{} })
}

// formatFingerprint groups a hex fingerprint in blocks of four for reading aloud and comparison.
func formatFingerprint(fp string) string {
	var b strings.Builder
	for i := 0; i < len(fp); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fp[i:min(i+4, len(fp))])
	}
	return b.String()
}

// sameProfileKey reports whether pasted is the stored key: the same text, or the same key packets once both
// are canonicalised (A-79), e.g. a legacy row with armor headers re-saved as the canonical text /account shows.
func sameProfileKey(stored, pasted string) bool {
	if stored == pasted {
		return true
	}
	_, _, a, err := parseCanonicalPublicKey(stored)
	if err != nil {
		return false
	}
	_, _, b, err := parseCanonicalPublicKey(pasted)
	return err == nil && a == b
}

// saveProfileKey is called by the /account action (inside its tx). An unchanged key is not re-parsed (a
// legacy key that no longer parses must not block the profile form). The same key re-saved (sameProfileKey)
// is stored in its canonical form and keeps its ownership proof and PGP sign-in (that rewrite is audited as a profile
// update; a byte-identical key writes nothing and returns no audit text). A changed key (added, replaced or
// removed) must parse and needs the confirmation (confirmed: password, plus a TOTP code if enrolled) on every
// account (A-152): buyers encrypt shipping addresses to it. A new version of the saved key (same fingerprint) is
// renewProfileKey's. A different key is stored in its canonical form with its fingerprint; ownership proof, PGP
// sign-in and any open challenge are cleared, the change time is recorded (A-158) and the owner is notified. It
// returns the audit text for the profile update and, for a new version of the saved key, its outcome: "renewed-kept"
// (the ownership proof kept), "renewed" (an unverified key) or the keyOutcomes reason; accountAction names its notice.
func saveProfileKey(c *actionCtx, armored string, confirmed bool) (string, string, error) {
	ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
	p, err := loadPGPAccount(ctx, tx, uid, true)
	if err != nil {
		return "", "", err
	}
	// loadPGPAccount has locked this account. Read independent selling approval
	// under that lock too: staff roles do not change when selling is granted.
	var selling bool
	if err := tx.QueryRowContext(ctx, "SELECT role IN ('vendor','admin') OR vendor_approved FROM users WHERE id=$1", uid).Scan(&selling); err != nil {
		return "", "", err
	}
	if selling {
		key, _, _, err := parseCanonicalPublicKey(armored)
		if err != nil || !canReceiveEncrypted(key, time.Now()) {
			return "", "", fail(409, "Vendors must keep a usable PGP public key. Save a public key with an active encryption subkey; never submit your private key.")
		}
	}
	old := p.Armored
	if old == armored { // nothing changed: no write and no audit row (A-159(h)); the form's receipt and notice still apply
		return "", "", nil
	}
	if armored != "" && sameProfileKey(old, armored) {
		_, _, canonical, _ := parseCanonicalPublicKey(armored)
		_, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1 WHERE id=$2", canonical, uid)
		return "Updated profile", "", err
	}
	if !confirmed { // the key changed since accountAction's unlocked read
		return "", "", fail(400, "Enter your current password to add, change or remove your PGP key.")
	}
	fp, canonical := "", ""
	if armored != "" {
		_, parsed, text, err := parseCanonicalPublicKey(armored)
		if err != nil {
			return "", "", fail(400, "PGP key rejected: "+err.Error()+".")
		}
		if p.Key != nil && parsed == p.Fingerprint {
			return renewProfileKey(c, p, text)
		}
		fp, canonical = parsed, text
	}
	if err := authorizeProfileKeyChange(c, p); err != nil {
		return "", "", err
	}
	if err := clearPGPAccess(ctx, tx, uid); err != nil {
		return "", "", err
	}
	if old == "" && fp != "" {
		// A pending secret shown before the first key binding was password-only
		// material; it must not become a trusted factor after the binding.
		if _, err := tx.ExecContext(ctx, "UPDATE users SET totp_pending='' WHERE id=$1 AND NOT totp_enabled", uid); err != nil {
			return "", "", err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1,pgp_fingerprint=$2,pgp_verified_at=NULL,pgp_2fa=false,pgp_changed_at=now() WHERE id=$3", canonical, fp, uid); err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1", uid); err != nil {
		return "", "", err
	}
	if fp == "" {
		if err := notifyOwner(ctx, tx, uid, "The PGP public key on your account was removed; its ownership proof and PGP sign-in verification were cleared."); err != nil {
			return "", "", err
		}
		return "Removed PGP key (ownership proof and PGP sign-in cleared)", "", nil
	}
	note := "The PGP public key on your account was changed; others now encrypt messages and shipping addresses to the new key."
	if old == "" {
		note = "A PGP public key was added to your account; others now encrypt messages and shipping addresses to it."
	}
	if err := notifyOwner(ctx, tx, uid, note+" Compare its fingerprint on your account page with your own PGP software."); err != nil {
		return "", "", err
	}
	return "Updated PGP key (fingerprint " + fp + "; ownership unverified)", "", nil
}

// keyOutcomes are why saving a new version of the saved key (renewProfileKey) cleared its ownership proof: the
// reason in the owner's notification and the key- notice (keyChangedAlert).
var keyOutcomes = map[string]string{
	"revocation": "the pasted key is missing a revocation signature your saved key had, or has it where PGP software ignores it, so a subkey or user ID you revoked could be used again",
	"subkey":     "with the pasted key others would encrypt to an older encryption subkey than the one your saved key uses",
}

// keyChangedAlert is the notice text (key-revocation, key-subkey) for a new version of the saved key saved as a
// changed key for the keyOutcomes reason why.
func keyChangedAlert(why string) string {
	return "Your key was saved as a changed key, not a renewal: " + keyOutcomes[why] + ". Its ownership is not verified and PGP sign-in is off until you prove ownership again on the PGP verification page. If you did not expect this, export your current key again and paste it."
}

// renewProfileKey saves a confirmed new version of the saved key p (A-155): the same fingerprint with different
// packets, as gpg --quick-set-expire, a new user ID or subkey, or someone's certification produce. Only the primary
// key's holder can make its self-signatures, so it keeps the ownership proof and PGP sign-in when every revocation
// in the saved key is still present and still applies to what it revoked (an old or rearranged copy of the key must
// not undo a revocation) and the encryption subkey it selects is the saved key's or one no older (no rollback to an
// older subkey). Otherwise it is saved as a changed key: proof, PGP sign-in and any open challenge are cleared, the
// change time is recorded (A-158) and the outcome names why. Either way the owner is notified.
func renewProfileKey(c *actionCtx, p *pgpAccount, canonical string) (string, string, error) {
	ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
	why := renewalRefusal(p, canonical, time.Now())
	if why != "" {
		if err := authorizeProfileKeyChange(c, p); err != nil {
			return "", "", err
		}
	}
	if err := clearPGPAccess(ctx, tx, uid); err != nil {
		return "", "", err
	}
	if why == "" {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1 WHERE id=$2", canonical, uid); err != nil {
			return "", "", err
		}
		note, audit, outcome := "The PGP public key on your account was updated with new signatures from the same key (for example a renewed expiry); its fingerprint is unchanged", "ownership unverified", "renewed"
		if p.Verified {
			note, audit, outcome = note+" and its ownership proof and PGP sign-in were kept", "ownership proof and PGP sign-in kept", "renewed-kept"
		}
		if err := notifyOwner(ctx, tx, uid, note+"."); err != nil {
			return "", "", err
		}
		return "Updated PGP key (same fingerprint " + p.Fingerprint + "; " + audit + ")", outcome, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1,pgp_verified_at=NULL,pgp_2fa=false,pgp_changed_at=now() WHERE id=$2", canonical, uid); err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1", uid); err != nil {
		return "", "", err
	}
	if err := notifyOwner(ctx, tx, uid, "The PGP public key on your account was updated, but not as a renewal of the same key: "+keyOutcomes[why]+". Its ownership proof and PGP sign-in verification were cleared."); err != nil {
		return "", "", err
	}
	return "Updated PGP key (same fingerprint " + p.Fingerprint + "; ownership proof and PGP sign-in cleared: " + why + ")", why, nil
}

// renewalRefusal returns why canonical, a new version of the saved key p, may not keep its ownership proof. It reads
// the key as it would be stored and used (canonical parsed again). "revocation": a revocation signature in the saved
// key (as published, parseCanonicalPublicKey) is missing, or a revocation of the saved key, of one of its subkeys or
// of one of its user IDs no longer applies to it (revocationsKept, A-261); "subkey": the encryption subkey it selects
// is neither the saved key's nor as new; "" when it may.
func renewalRefusal(p *pgpAccount, canonical string, now time.Time) string {
	key, _, _, err := parseCanonicalPublicKey(canonical)
	if err != nil {
		return "revocation"
	}
	had, err := revocationSignatures(p.Canonical)
	if err != nil {
		return "revocation"
	}
	has, err := revocationSignatures(canonical)
	if err != nil {
		return "revocation"
	}
	for sig := range had {
		if !has[sig] {
			return "revocation"
		}
	}
	if !revocationsKept(p.Key, key) {
		return "revocation"
	}
	was, ok := encryptionSubkey(p.Key, now)
	if !ok {
		return ""
	}
	is, ok := encryptionSubkey(key, now)
	if !ok || !bytes.Equal(is.Key.Fingerprint, was.Key.Fingerprint) && is.Key.CreationTime.Before(was.Key.CreationTime) {
		return "subkey"
	}
	return ""
}

type pgpAccount struct {
	Key          *openpgp.Entity
	Armored      string // users.pgp as stored
	Canonical    string // the parsed key re-armored without headers or surrounding text, shown to users (A-79)
	Fingerprint  string // parsed from the saved key
	Stored       string // users.pgp_fingerprint (set on save and on verification)
	Verified     bool   // proof recorded for exactly this key
	VerifiedAt   string
	ProofVersion string // exact ownership-proof timestamp used to bind pending sign-in codes
	KeyVersion   string // monotonic generation; replacing and restoring a key cannot revive a login code
	TwoFactor    bool   // PGP sign-in is on and usable: the setting is on and the proven key can be read
	TwoFactorSet bool   // users.pgp_2fa as stored; on while TwoFactor is off means PGP sign-in is lost (A-272)
}

// loadPGPAccount reads the saved key; q is a *sql.Tx (lock with forUpdate) or *sql.DB.
func loadPGPAccount(ctx context.Context, q rowQuerier, uid string, forUpdate bool) (*pgpAccount, error) {
	query := `SELECT pgp,pgp_fingerprint,pgp_verified_at IS NOT NULL,COALESCE(to_char(pgp_verified_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"'),''),pgp_2fa,COALESCE(extract(epoch FROM pgp_verified_at)::text,''),pgp_key_generation::text FROM users WHERE id=$1`
	if forUpdate {
		query += " FOR UPDATE"
	}
	p := &pgpAccount{}
	var verified bool
	if err := q.QueryRowContext(ctx, query, uid).Scan(&p.Armored, &p.Stored, &verified, &p.VerifiedAt, &p.TwoFactor, &p.ProofVersion, &p.KeyVersion); err != nil {
		return nil, err
	}
	if p.Armored != "" {
		if key, fp, canonical, err := parseCanonicalPublicKey(p.Armored); err == nil {
			p.Key, p.Fingerprint, p.Canonical = key, fp, canonical
		}
	}
	p.Verified = verified && p.Key != nil && p.Stored == p.Fingerprint
	p.TwoFactorSet = p.TwoFactor
	p.TwoFactor = p.TwoFactor && p.Verified
	if !p.Verified {
		p.VerifiedAt = ""
	}
	return p, nil
}

func pgpLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.Page == "pgp" {
		d.Title = "PGP verification"
	}
	if d.User == nil {
		return nil
	}
	p, err := loadPGPAccount(ctx, a.db, d.User.ID, false)
	if err != nil {
		return err
	}
	v := &PGPView{HasKey: p.Armored != "", Armored: p.Armored, Verified: p.Verified, VerifiedAt: p.VerifiedAt, TwoFactor: p.TwoFactor}
	v.LoginRequired = d.Settings[settingAllowLoginWithoutPGP] != "true" // as pgpLoginRequired reads it (A-702)
	d.PGP = v
	if p.Key == nil {
		if v.HasKey {
			_, _, perr := parsePublicKey(p.Armored)
			v.KeyError = "The saved key cannot be used: " + perr.Error() + ". Paste your current public key on the account page."
		}
		return nil
	}
	v.Fingerprint, v.Armored, v.FingerprintHex = formatFingerprint(p.Fingerprint), p.Canonical, p.Fingerprint
	for id := range p.Key.Identities {
		v.UserIDs = append(v.UserIDs, id)
	}
	slices.Sort(v.UserIDs)
	keyDates(v, p.Key, time.Now())
	if v.LoginRequired && (v.CannotEncrypt || v.StopsSoon != "") {
		v.SignInLockout = strictSignInLockout(v.StopsSoon, v.CannotEncrypt)
	}
	if d.Page != "pgp" {
		return nil
	}
	var kind, fp, challenge, expires string
	err = a.db.QueryRowContext(ctx, `SELECT kind,fingerprint,challenge,to_char(expires AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM pgp_challenges WHERE user_id=$1 AND session_hash=$2 AND key_digest=$3 AND expires>now()`, d.User.ID, digest(sessionToken(r)), digest(p.Canonical)).Scan(&kind, &fp, &challenge, &expires)
	if err == sql.ErrNoRows || err == nil && fp != p.Fingerprint {
		return nil
	}
	if err != nil {
		return err
	}
	v.ChallengeExpires = expires
	if kind == "sign" {
		v.Challenge = challenge
	} else {
		v.EncryptedChallenge = challenge
	}
	return nil
}

// keyExpiryWarning is how long before encryption to a key stops its owner is warned (A-155): on /account and /pgp,
// and once by the daily sweep.
const keyExpiryWarning = 30 * 24 * time.Hour

// keyExpiryText formats an expiry for the owner: "Never", "YYYY-MM-DD UTC", or "Expired YYYY-MM-DD UTC".
func keyExpiryText(t, now time.Time) string {
	switch {
	case t.IsZero():
		return "Never"
	case !t.After(now):
		return "Expired " + t.UTC().Format(time.DateOnly) + " UTC"
	}
	return t.UTC().Format(time.DateOnly) + " UTC"
}

// keyDates fills the owner's expiry fields of v for key (A-155) and whether anything can be encrypted to it (A-225).
func keyDates(v *PGPView, key *openpgp.Entity, now time.Time) {
	v.PrimaryExpires = keyExpiryText(primaryKeyExpiry(key), now)
	v.CannotEncrypt = !canReceiveEncrypted(key, now)
	enc, ok := encryptionSubkey(key, now)
	if !ok {
		return
	}
	v.SubkeyExpires = keyExpiryText(enc.Expires, now)
	if !v.CannotEncrypt && !enc.Until.IsZero() && enc.Until.Sub(now) <= keyExpiryWarning {
		v.StopsSoon = enc.Until.UTC().Format(time.DateOnly) + " UTC"
	}
}

func pgpChallengeAction(c *actionCtx) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	kind := c.Form.Get("kind")
	if kind != "sign" && kind != "decrypt" {
		return actionResult{}, fail(400, "Choose to sign a text or to decrypt a message")
	}
	if !c.A.allow(ingressKey(ctx, "pgp-challenge:"+u.ID), 20) {
		return actionResult{}, fail(429, "Too many challenges. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	if p.Key == nil {
		return actionResult{}, fail(409, "Save a valid public key on your account page first")
	}
	// Session-specific challenges are bounded by the issue budget and their TTL.
	// Retire this account's expired rows before creating another.
	if _, err = tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1 AND expires<=now()", u.ID); err != nil {
		return actionResult{}, err
	}
	nonce := randomToken()[:32]
	challenge, nonceHash := "OPSECMKT key ownership proof for "+u.Handle+": "+nonce, ""
	if kind == "decrypt" {
		var requested, expires time.Time
		var market string
		if err = tx.QueryRowContext(ctx, "SELECT now(),now()+interval '"+pgpChallengeTTL+"',COALESCE((SELECT value FROM settings WHERE key='site_name'),'')").Scan(&requested, &expires, &market); err != nil {
			return actionResult{}, err
		}
		text := pgpAccessChallengeText(u.Handle, market, c.A.officialAddresses, privateAdminIngress(ctx), requested, expires, nonce)
		if challenge, err = encryptTo(p.Key, text); err != nil {
			return actionResult{}, fail(409, "Your key has no usable encryption subkey (expired or sign-only). Use the signing challenge instead.")
		}
		nonceHash = digest(nonce)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pgp_challenges(user_id,kind,fingerprint,challenge,nonce_hash,expires,session_hash,key_digest) VALUES($1,$2,$3,$4,$5,now()+interval '`+pgpChallengeTTL+`',$6,$7)
ON CONFLICT(user_id,session_hash) DO UPDATE SET kind=excluded.kind,fingerprint=excluded.fingerprint,challenge=excluded.challenge,nonce_hash=excluded.nonce_hash,expires=excluded.expires,session_hash=excluded.session_hash,key_digest=excluded.key_digest,created=now()`, u.ID, kind, p.Fingerprint, challenge, nonceHash, digest(c.Token), digest(p.Canonical))
	return actionResult{Redirect: "/pgp"}, err
}

// checkSignedProof accepts a clearsigned message whose text is exactly challenge, or an armored
// detached signature over challenge (with or without one trailing line ending), made by key.
func checkSignedProof(challenge, submission string, key *openpgp.Entity) error {
	submission = strings.TrimSpace(submission)
	switch {
	case strings.HasPrefix(submission, "-----BEGIN PGP SIGNED MESSAGE-----"):
		text, _, err := verifyClearsigned(submission, key)
		if err != nil {
			return errors.New("the clearsigned message does not verify with your saved key")
		}
		if strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")) != challenge {
			return errors.New("the signed text is not the current challenge")
		}
		return nil
	case strings.HasPrefix(submission, "-----BEGIN PGP SIGNATURE-----"):
		for _, signed := range []string{challenge, challenge + "\n", challenge + "\r\n"} {
			if _, err := verifyDetached(signed, submission, key); err == nil {
				return nil
			}
		}
		return errors.New("the detached signature does not verify over the challenge with your saved key")
	}
	return errors.New("paste a clearsigned message or an armored detached signature")
}

func pgpVerifyAction(c *actionCtx) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	if !c.A.allow(ingressKey(ctx, "pgp-verify:"+u.ID), 10) {
		return actionResult{}, fail(429, "Too many attempts. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	var kind, fp, challenge, nonceHash string
	err = tx.QueryRowContext(ctx, "SELECT kind,fingerprint,challenge,nonce_hash FROM pgp_challenges WHERE user_id=$1 AND session_hash=$2 AND key_digest=$3 AND expires>now() FOR UPDATE", u.ID, digest(c.Token), digest(p.Canonical)).Scan(&kind, &fp, &challenge, &nonceHash)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(409, "No open challenge. Start a new one; challenges expire after 30 minutes.")
	}
	if err != nil {
		return actionResult{}, err
	}
	if p.Key == nil || fp != p.Fingerprint {
		return actionResult{}, fail(409, "Your saved key changed since this challenge was issued. Start a new challenge.")
	}
	if kind == "sign" {
		if err = checkSignedProof(challenge, c.Form.Get("signature"), p.Key); err != nil {
			return actionResult{}, fail(400, "Not verified: "+err.Error()+".")
		}
	} else {
		answer := strings.ToLower(strings.TrimSpace(c.Form.Get("response")))
		if answer == "" || subtle.ConstantTimeCompare([]byte(digest(answer)), []byte(nonceHash)) != 1 {
			return actionResult{}, fail(400, "Not verified: the decrypted text does not match the challenge.")
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET pgp_verified_at=now(),pgp_fingerprint=$1 WHERE id=$2 AND pgp=$3", p.Fingerprint, u.ID, p.Armored); err != nil {
		return actionResult{}, err
	}
	if kind == "decrypt" {
		if err = grantPGPAccess(ctx, tx, u.ID, c.Token); err != nil {
			return actionResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1 AND session_hash=$2", u.ID, digest(c.Token)); err != nil {
		return actionResult{}, err
	}
	if err = notifyOwner(ctx, tx, u.ID, "Ownership of the PGP key on your account was proved."); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/pgp", Notice: "pgp-verified", Audit: "Verified PGP key ownership (fingerprint " + p.Fingerprint + ", " + kind + " challenge)"}, nil
}

// pgpTwoFactorAction turns PGP sign-in on (verified key) or off. Either needs the current password plus a TOTP
// code if enrolled (A-152: also for the first factor, so a stolen session cannot lock the owner out) and notifies
// the owner.
func pgpTwoFactorAction(c *actionCtx) (actionResult, error) {
	enable := c.Form.Get("enable")
	if enable != "1" && enable != "0" {
		return actionResult{}, fail(400, "Choose to turn PGP sign-in verification on or off")
	}
	// Turning it on as the first factor ends pending sign-ins before the account lock (endOtherSessionsForFirstFactor).
	opts := &confirmation{}
	if enable == "1" {
		opts.Before = endPendingSignInsBeforeFirstFactor
	}
	return confirmedTxWith(c, opts, func(c *actionCtx) (actionResult, error) { return pgpTwoFactor(c, enable == "1") })
}

func pgpTwoFactor(c *actionCtx, enable bool) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	if !enable {
		if _, err = tx.ExecContext(ctx, "UPDATE users SET pgp_2fa=false WHERE id=$1", u.ID); err != nil {
			return actionResult{}, err
		}
		if err = notifyOwner(ctx, tx, u.ID, "PGP sign-in verification was turned off for your account."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/pgp", Notice: "pgp-2fa-off", Audit: "Turned off PGP sign-in verification"}, nil
	}
	if !p.Verified {
		return actionResult{}, fail(409, "Verify ownership of your saved key before turning on PGP sign-in verification.")
	}
	if _, err = encryptTo(p.Key, "probe"); err != nil {
		return actionResult{}, fail(409, "Your key has no usable encryption subkey (expired or sign-only), so sign-in codes cannot be encrypted to it.")
	}
	// SEC706-1: the account-wide ownership proof may come from another browser; a password alone (all a phisher
	// has) must not turn on a factor, so this browser must have decrypted a challenge to the saved key.
	proved, err := pgpBindingProven(c, p)
	if err != nil {
		return actionResult{}, err
	}
	if !proved {
		return actionResult{}, fail(http.StatusForbidden, "Decrypt a challenge with your PGP key in this browser before turning on PGP sign-in: start one below. Nothing was changed.")
	}
	var totp bool
	if err = tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1", u.ID).Scan(&totp); err != nil {
		return actionResult{}, err
	}
	if !totp && !p.TwoFactorSet {
		if err = endOtherSessionsForFirstFactor(c); err != nil {
			return actionResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET pgp_2fa=true WHERE id=$1", u.ID); err != nil {
		return actionResult{}, err
	}
	if err = notifyOwner(ctx, tx, u.ID, "PGP sign-in verification was turned on for your account: sign-in now also asks for a code encrypted to its key."); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/pgp", Notice: "pgp-2fa-on", Audit: "Turned on PGP sign-in verification (fingerprint " + p.Fingerprint + ")"}, nil
}

func previewPGP(d *PageData) {
	d.Title = "PGP verification"
	d.PGP = &PGPView{
		HasKey:           true,
		Fingerprint:      formatFingerprint("0000000000000000000000000000000000000000"),
		Challenge:        "OPSECMKT key ownership proof for preview_user: sample-preview-nonce",
		ChallengeExpires: "sample",
		PrimaryExpires:   "Never",
		SubkeyExpires:    "Never",
	}
}
