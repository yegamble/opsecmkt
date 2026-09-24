package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// P2 PGP identity: the profile key is parsed and fingerprinted on save; ownership is "Verified" only
// after the user signs a server nonce or decrypts a nonce encrypted to the key. Changing the key
// resets verification and PGP sign-in. Once a second factor is enrolled, changing the key or adding
// PGP sign-in needs re-authentication (A-47). The server never sees or stores private keys.

const pgpChallengeTTL = "30 minutes"

func init() {
	registerPage("pgp", pageSpec{Protected: true})
	registerLoader("pgp", pgpLoader)
	registerLoader("account", pgpLoader)
	registerAction("/pgp/challenge", actionSpec{Run: pgpChallengeAction})
	registerAction("/pgp/verify", actionSpec{Run: pgpVerifyAction})
	registerAction("/pgp/2fa", actionSpec{OwnTx: true, Run: pgpTwoFactorAction})
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
// is stored in its canonical form and keeps its ownership proof and PGP sign-in. A changed key must parse, and
// needs the confirmation (confirmed: password, plus a TOTP code if enrolled) while PGP sign-in or TOTP is on; it
// stores the canonical form and new fingerprint, clears ownership proof, PGP sign-in and any open challenge. It
// returns the audit text for the profile update.
func saveProfileKey(c *actionCtx, armored string, confirmed bool) (string, error) {
	ctx, tx, uid := c.Ctx(), c.Tx, c.User.ID
	var old string
	var twoFA, totp bool
	if err := tx.QueryRowContext(ctx, "SELECT pgp,pgp_2fa,totp_enabled FROM users WHERE id=$1 FOR UPDATE", uid).Scan(&old, &twoFA, &totp); err != nil {
		return "", err
	}
	if old == armored {
		return "Updated profile", nil
	}
	if armored != "" && sameProfileKey(old, armored) {
		_, _, canonical, _ := parseCanonicalPublicKey(armored)
		_, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1 WHERE id=$2", canonical, uid)
		return "Updated profile", err
	}
	if (twoFA || totp) && !confirmed {
		return "", fail(400, "Enter your current password to change or remove your key while sign-in verification is on.")
	}
	fp, canonical := "", ""
	if armored != "" {
		_, parsed, text, err := parseCanonicalPublicKey(armored)
		if err != nil {
			return "", fail(400, "PGP key rejected: "+err.Error()+".")
		}
		fp, canonical = parsed, text
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET pgp=$1,pgp_fingerprint=$2,pgp_verified_at=NULL,pgp_2fa=false WHERE id=$3", canonical, fp, uid); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1", uid); err != nil {
		return "", err
	}
	if fp == "" {
		return "Removed PGP key (ownership proof and PGP sign-in cleared)", nil
	}
	return "Updated PGP key (fingerprint " + fp + "; ownership unverified)", nil
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
	TwoFactor    bool
}

// loadPGPAccount reads the saved key; q is a *sql.Tx (lock with forUpdate) or *sql.DB.
func loadPGPAccount(ctx context.Context, q rowQuerier, uid string, forUpdate bool) (*pgpAccount, error) {
	query := `SELECT pgp,pgp_fingerprint,pgp_verified_at IS NOT NULL,COALESCE(to_char(pgp_verified_at AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"'),''),pgp_2fa,COALESCE(extract(epoch FROM pgp_verified_at)::text,'') FROM users WHERE id=$1`
	if forUpdate {
		query += " FOR UPDATE"
	}
	p := &pgpAccount{}
	var verified bool
	if err := q.QueryRowContext(ctx, query, uid).Scan(&p.Armored, &p.Stored, &verified, &p.VerifiedAt, &p.TwoFactor, &p.ProofVersion); err != nil {
		return nil, err
	}
	if p.Armored != "" {
		if key, fp, canonical, err := parseCanonicalPublicKey(p.Armored); err == nil {
			p.Key, p.Fingerprint, p.Canonical = key, fp, canonical
		}
	}
	p.Verified = verified && p.Key != nil && p.Stored == p.Fingerprint
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
	d.PGP = v
	if p.Key == nil {
		if v.HasKey {
			_, _, perr := parsePublicKey(p.Armored)
			v.KeyError = "The saved key cannot be used: " + perr.Error() + ". Paste your current public key on the account page."
		}
		return nil
	}
	v.Fingerprint, v.Armored = formatFingerprint(p.Fingerprint), p.Canonical
	for id := range p.Key.Identities {
		v.UserIDs = append(v.UserIDs, id)
	}
	slices.Sort(v.UserIDs)
	if d.Page != "pgp" {
		return nil
	}
	var kind, fp, challenge, expires string
	err = a.db.QueryRowContext(ctx, `SELECT kind,fingerprint,challenge,to_char(expires AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI "UTC"') FROM pgp_challenges WHERE user_id=$1 AND expires>now()`, d.User.ID).Scan(&kind, &fp, &challenge, &expires)
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

func pgpChallengeAction(c *actionCtx) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	kind := c.Form.Get("kind")
	if kind != "sign" && kind != "decrypt" {
		return actionResult{}, fail(400, "Choose to sign a text or to decrypt a message")
	}
	if !c.A.allow("pgp-challenge:"+u.ID, 20) {
		return actionResult{}, fail(429, "Too many challenges. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	if p.Key == nil {
		return actionResult{}, fail(409, "Save a valid public key on your account page first")
	}
	nonce := randomToken()[:32]
	challenge, nonceHash := "OPSECMKT key ownership proof for "+u.Handle+": "+nonce, ""
	if kind == "decrypt" {
		if challenge, err = encryptTo(p.Key, nonce); err != nil {
			return actionResult{}, fail(409, "Your key has no usable encryption subkey (expired or sign-only). Use the signing challenge instead.")
		}
		nonceHash = digest(nonce)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pgp_challenges(user_id,kind,fingerprint,challenge,nonce_hash,expires) VALUES($1,$2,$3,$4,$5,now()+interval '`+pgpChallengeTTL+`')
ON CONFLICT(user_id) DO UPDATE SET kind=excluded.kind,fingerprint=excluded.fingerprint,challenge=excluded.challenge,nonce_hash=excluded.nonce_hash,expires=excluded.expires,created=now()`, u.ID, kind, p.Fingerprint, challenge, nonceHash)
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
	if !c.A.allow("pgp-verify:"+u.ID, 10) {
		return actionResult{}, fail(429, "Too many attempts. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	var kind, fp, challenge, nonceHash string
	err = tx.QueryRowContext(ctx, "SELECT kind,fingerprint,challenge,nonce_hash FROM pgp_challenges WHERE user_id=$1 AND expires>now() FOR UPDATE", u.ID).Scan(&kind, &fp, &challenge, &nonceHash)
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
	if _, err = tx.ExecContext(ctx, "DELETE FROM pgp_challenges WHERE user_id=$1", u.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/pgp?saved=1", Audit: "Verified PGP key ownership (fingerprint " + p.Fingerprint + ", " + kind + " challenge)"}, nil
}

// pgpTwoFactorAction turns PGP sign-in on (verified key) or off. Turning it off, or on while TOTP is enrolled
// (adding a second factor), needs the current password plus a TOTP code if enrolled.
func pgpTwoFactorAction(c *actionCtx) (actionResult, error) {
	enable := c.Form.Get("enable")
	if enable != "1" && enable != "0" {
		return actionResult{}, fail(400, "Choose to turn PGP sign-in verification on or off")
	}
	var totp bool
	if err := c.A.db.QueryRowContext(c.Ctx(), "SELECT totp_enabled FROM users WHERE id=$1", c.User.ID).Scan(&totp); err != nil {
		return actionResult{}, err
	}
	confirm := enable == "0" || totp
	return confirmedTx(c, confirm, func(c *actionCtx) (actionResult, error) { return pgpTwoFactor(c, enable, confirm) })
}

func pgpTwoFactor(c *actionCtx, enable string, confirmed bool) (actionResult, error) {
	ctx, tx, u := c.Ctx(), c.Tx, c.User
	p, err := loadPGPAccount(ctx, tx, u.ID, true)
	if err != nil {
		return actionResult{}, err
	}
	if enable == "0" {
		_, err = tx.ExecContext(ctx, "UPDATE users SET pgp_2fa=false WHERE id=$1", u.ID)
		return actionResult{Redirect: "/pgp?saved=1", Audit: "Turned off PGP sign-in verification"}, err
	}
	// TOTP may have been turned on since the unlocked read above; the row is locked now.
	var totp bool
	if err = tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1", u.ID).Scan(&totp); err != nil {
		return actionResult{}, err
	}
	if totp && !confirmed {
		return actionResult{}, fail(400, "Enter your current password and an authenticator code to add PGP sign-in verification.")
	}
	if !p.Verified {
		return actionResult{}, fail(409, "Verify ownership of your saved key before turning on PGP sign-in verification.")
	}
	if _, err = encryptTo(p.Key, "probe"); err != nil {
		return actionResult{}, fail(409, "Your key has no usable encryption subkey (expired or sign-only), so sign-in codes cannot be encrypted to it.")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET pgp_2fa=true WHERE id=$1", u.ID); err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/pgp?saved=1", Audit: "Turned on PGP sign-in verification (fingerprint " + p.Fingerprint + ")"}, nil
}

func previewPGP(d *PageData) {
	d.Title = "PGP verification"
	d.PGP = &PGPView{
		HasKey:           true,
		Fingerprint:      formatFingerprint("0000000000000000000000000000000000000000"),
		Challenge:        "OPSECMKT key ownership proof for preview_user: sample-preview-nonce",
		ChallengeExpires: "sample",
	}
}
