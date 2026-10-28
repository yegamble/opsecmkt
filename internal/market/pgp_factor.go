package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// P2 PGP second factor. After the password step, the user asks for a one-time code (POST
// /challenge/pgp; GET never changes state); the server encrypts a random code to the user's verified
// key and stores only its sha256 in pending_logins. POST /challenge with method=pgp compares the
// decrypted code. The pending row, and with it the code, is deleted when sign-in completes. The encrypted
// message also names the account, the market and its official addresses (pgpSignInText, A-572).

type pgpFactor struct{}

func init() {
	registerFactor(pgpFactor{})
	registerAction("/challenge/pgp", actionSpec{Public: true, Precheck: pendingPrecheck, Run: pgpLoginCodeAction})
	registerLoader("challenge", pgpChallengeLoader)
	registerPreview("challenge", func(d *PageData) {
		if d.Auth == nil {
			d.Auth = &AuthView{}
		}
		d.Auth.PGPEnrolled = true
	})
}

func (pgpFactor) Name() string { return "pgp" }

// PGP is offered for an enrolled account, or required by the market login policy.
// Strict login can prove a newly saved key for the first time by decryption.
func (pgpFactor) Enrolled(ctx context.Context, q rowQuerier, userID string) (bool, error) {
	p, err := loadPGPAccount(ctx, q, userID, false)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	required, err := pgpLoginRequired(ctx, q)
	if err != nil {
		return false, err
	}
	return p.TwoFactor || required && p.Key != nil && canReceiveEncrypted(p.Key, time.Now()), nil
}

// Bind the code to the exact key ownership proof. Replacing and re-verifying a
// key invalidates old codes, including when the same key is later restored.
func pgpLoginDigest(code string, p *pgpAccount) string {
	return digest(code + "\x00" + p.Fingerprint + "\x00" + p.ProofVersion + "\x00" + digest(p.Canonical) + "\x00" + p.KeyVersion)
}

func (pgpFactor) Verify(c *actionCtx, userID string) error {
	p, err := loadPGPAccount(c.Ctx(), c.Tx, userID, true)
	if err != nil {
		return err
	}
	required, err := pgpLoginRequired(c.Ctx(), c.Tx)
	if err != nil {
		return err
	}
	if !p.TwoFactor && !required {
		return fail(401, "PGP sign-in verification is no longer enabled. Sign in again.")
	}
	if p.Key == nil || !canReceiveEncrypted(p.Key, time.Now()) {
		return fail(401, "Your saved key cannot receive an encrypted sign-in code. Renew your public key or contact staff for recovery.")
	}
	var hash sql.NullString
	err = c.Tx.QueryRowContext(c.Ctx(), "SELECT pgp_nonce FROM pending_logins WHERE token_hash=$1 AND user_id=$2", digest(pendingCookie(c.R)), userID).Scan(&hash)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if !hash.Valid {
		return fail(401, "Create an encrypted sign-in code first, then paste the code it contains.")
	}
	answer := strings.ToLower(strings.TrimSpace(c.Form.Get("pgp_code")))
	if answer == "" || subtle.ConstantTimeCompare([]byte(pgpLoginDigest(answer, p)), []byte(hash.String)) != 1 {
		return fail(401, "The decrypted code does not match.")
	}
	return nil
}

// pgpLoginCodeAction encrypts a fresh one-time code to the pending user's verified key.
func pgpLoginCodeAction(c *actionCtx) (actionResult, error) {
	ctx, tx := c.Ctx(), c.Tx
	pending := pendingCookie(c.R)
	if pending == "" {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	var userID, next string
	var requested, expires time.Time
	err := tx.QueryRowContext(ctx, "SELECT user_id,next,now(),expires FROM pending_logins WHERE token_hash=$1 AND expires>now() FOR UPDATE", digest(pending)).Scan(&userID, &next, &requested, &expires)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	if err != nil {
		return actionResult{}, err
	}
	if !c.A.allow(ingressKey(ctx, "pgp-login:"+digest(pending)), 10) { // only after the pending login exists
		return actionResult{}, fail(429, "Too many codes requested. Try again in ten minutes.")
	}
	p, err := loadPGPAccount(ctx, tx, userID, true)
	if err != nil {
		return actionResult{}, err
	}
	required, err := pgpLoginRequired(ctx, tx)
	if err != nil {
		return actionResult{}, err
	}
	if !p.TwoFactor && !required {
		return actionResult{}, fail(400, "PGP sign-in verification is not enabled for this account")
	}
	if p.Key == nil || !canReceiveEncrypted(p.Key, time.Now()) {
		return actionResult{}, expiredKeyRefusal(c, userID)
	}
	var handle, market string
	if err = tx.QueryRowContext(ctx, "SELECT handle,COALESCE((SELECT value FROM settings WHERE key='site_name'),'') FROM users WHERE id=$1", userID).Scan(&handle, &market); err != nil {
		return actionResult{}, err
	}
	code := randomToken()[:32]
	armored, err := encryptTo(p.Key, pgpSignInText(handle, market, c.A.officialAddresses, privateAdminIngress(ctx), requested, expires, code))
	if err != nil {
		return actionResult{}, expiredKeyRefusal(c, userID)
	}
	_, err = tx.ExecContext(ctx, "UPDATE pending_logins SET pgp_nonce=$1,pgp_challenge=$2 WHERE token_hash=$3", pgpLoginDigest(code, p), armored, digest(pending))
	return actionResult{Redirect: challengePath(c.A.signInNext(next))}, err
}

// pgpSignInText is the plaintext of an encrypted PGP sign-in code (A-572). A phishing site that relays this market
// through its own address can relay the ciphertext too, so the message names the account, the market, when the code
// was requested and until when it is valid (the pending sign-in's expiry, at most 10 minutes after the password), and
// the official addresses: OFFICIAL_ADDRESSES from the operator's configuration, never the request's Host or any
// header, so never the private administrator onion unless an operator lists it (the native installer refuses that).
// The market name is flattened to one line so it cannot forge another. A code requested over the private
// administrator listener (privateAdminIngress, set only by that socket's handler) says so instead of telling the
// administrator to look for a listed address, still without naming that onion. Only the code on the last line is
// accepted.
func pgpSignInText(handle, market string, addresses []string, privateAdmin bool, requested, expires time.Time, code string) string {
	market = strings.Join(strings.FieldsFunc(market, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}), " ")
	if market == "" {
		market = "OPSEC Market" // the layout's name when none is saved
	}
	list := "no official address configured"
	check := "This market's operator has not configured its official addresses, so this message cannot confirm the address you are using."
	if len(addresses) > 0 {
		list = strings.Join(addresses, ", ")
		check = "Enter the code only if your browser's address bar shows one of these addresses; on any other address, do not enter it."
	}
	if privateAdmin {
		check = "You requested this code on the market's private administrator connection, which is never listed here: enter it only on that connection's private address."
	}
	const layout = "2006-01-02 15:04:05 UTC"
	return "Sign-in code for " + handle + " at " + market + "\n" +
		"Requested: " + requested.UTC().Format(layout) + "; valid for at most 10 minutes, until " + expires.UTC().Format(layout) + "\n" +
		"Official addresses: " + list + "\n" +
		check + "\n" +
		"Code: " + code + "\n"
}

// expiredKeyRefusal is /challenge/pgp's answer when no code can be encrypted to the verified key (A-155): usually it,
// or its encryption subkey, expired.
func expiredKeyRefusal(c *actionCtx, userID string) error {
	return pgpKeyRefusal(c.Ctx(), c.Tx, userID,
		"A sign-in code cannot be encrypted to your key: it or its encryption subkey expired or was revoked, or it cannot encrypt. ",
		"your password, saved key and its verification stay, and ", "renew the key (gpg --quick-set-expire) and paste it on your account page")
}

// unreadableKeyRefusal is /login's answer, after a correct password, for an account whose only second factor is PGP
// sign-in and whose saved key can no longer be read (A-272): a password alone never opens such an account.
func unreadableKeyRefusal(ctx context.Context, q rowQuerier, userID string) error {
	return pgpKeyRefusal(ctx, q, userID,
		"PGP sign-in is on for this account, but your saved PGP key can no longer be read (it may have been saved before this market refused keys like it), so no sign-in code can be encrypted to it. ",
		"your password stays, and ", "paste a current export of your public key (gpg --armor --export followed by its fingerprint) on your account page and prove you own it on the PGP page again")
}

// pgpKeyRefusal refuses a PGP sign-in that cannot be finished because of the saved key (problem says why), naming what
// the account can still do: its other method when TOTP is enrolled, otherwise the administrator's reset of second
// factors (/admin; for an administrator only A-137's operator reset, -reset-admin-factors; keeps says what that reset
// leaves), then fix; and how to reach the staff (the staff contact, A-119, with A-702's signed-out lines). Under the
// strict policy (pgpLoginRequired) none of those helps, only replacement-key recovery through the staff (A-702).
func pgpKeyRefusal(ctx context.Context, q rowQuerier, userID, problem, keeps, fix string) error {
	var totp bool
	var role string
	if err := q.QueryRowContext(ctx, "SELECT totp_enabled,role FROM users WHERE id=$1", userID).Scan(&totp, &role); err != nil {
		return err
	}
	contact, err := signedOutStaffContactRefusal(ctx, q)
	if err != nil {
		return err
	}
	required, err := pgpLoginRequired(ctx, q)
	if err != nil {
		return err
	}
	if required {
		return strictKeyRefusal(problem, contact)
	}
	if totp {
		return fail(409, problem+"Sign in with your authenticator code or a recovery code instead, then "+fix+". Replacing the existing public-key binding also needs a fresh code from your previously enrolled authenticator or old-key decryption proof. If neither is available, ask for identity-verified replacement-key recovery. "+contact)
	}
	who := "A market administrator can turn it off for you with Reset a user's second factors"
	if role == "admin" {
		// The web reset refuses administrator targets (adminResetFactorsAction); only the host command remains (QA19-1).
		who = "The operator can turn it off on the server with -reset-admin-factors"
	}
	return fail(409, problem+"PGP sign-in is the only verification method on this account, so this sign-in cannot be finished. "+who+
		"; "+keeps+"you then sign in with your password. A reset alone cannot authorize replacing the protected key binding: renew a usable export of the same key where possible, or request identity-verified replacement-public-key recovery. "+contact)
}

// strictKeyRefusal refuses a sign-in under the strict policy (pgpLoginRequired) for want of a usable key (problem says
// why), naming replacement-key recovery and contact (signedOutStaffContactRefusal), A-702. The staff save the key with
// the factor reset's replacement public key (for an administrator, the operator with -reset-admin-factors and
// -reset-admin-pgp-key-file); sign-in then still needs decryption with it.
func strictKeyRefusal(problem, contact string) error {
	return fail(409, problem+"This market requires PGP decryption at every sign-in. An authenticator, recovery code or second-factor reset cannot replace it. "+
		"To recover, renew your key in your own PGP software (gpg --quick-set-expire keeps the same fingerprint) or, if its private key is lost or none is saved, make a new key, and send the public key to the market staff. "+
		"Once they have checked it is you, they save it (for an administrator account, the server operator does) and you sign in by decrypting with it; the policy stays in force. "+contact)
}

// pgpSignInLost reports that PGP sign-in is turned on but cannot be used: the saved key cannot be read (or no longer
// matches its ownership proof), so pgpFactor.Enrolled no longer counts it (A-272).
func pgpSignInLost(ctx context.Context, q rowQuerier, userID string) (bool, error) {
	p, err := loadPGPAccount(ctx, q, userID, false)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.TwoFactorSet && !p.TwoFactor, nil
}

// unreadableKeyNotice tells the owner, after a correct password, that PGP sign-in was lost (A-272).
var unreadableKeyNotice = ownerNotice("Someone entered your password correctly, but PGP sign-in could not be used because your saved PGP key can no longer be read. " +
	"With an authenticator enrolled, sign-in asks for its code or a recovery code instead; without one, sign-in is refused until your second factors are turned off (by a market administrator; for an administrator account, by the operator on the server). " +
	"The existing public-key binding remains protected. Renew the same key where possible; if it is lost or unreadable and no preexisting authenticator can confirm replacement, request identity-verified replacement-public-key recovery. A strict PGP login policy still requires decryption and is not weakened by a reset.")

// notifyUnreadableKey sends unreadableKeyNotice once per saved key: not again while one sent since the key last
// changed (users.pgp_changed_at; for a key saved before that was recorded, ever) exists. The account row is locked
// first, so concurrent sign-ins send one.
func (a *App) notifyUnreadableKey(ctx context.Context, userID string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var changed sql.NullTime
	if err = tx.QueryRowContext(ctx, "SELECT pgp_changed_at FROM users WHERE id=$1 FOR NO KEY UPDATE", userID).Scan(&changed); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) SELECT $1,$2,$3
		WHERE NOT EXISTS (SELECT 1 FROM notifications WHERE user_id=$2 AND body=$3 AND created>=COALESCE($4,'-infinity'::timestamptz))`,
		randomToken(), userID, unreadableKeyNotice, changed); err != nil {
		return err
	}
	return tx.Commit()
}

// pgpChallengeLoader shows the encrypted code for this pending login, if one was requested.
func pgpChallengeLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	pending := pendingCookie(r)
	if pending == "" {
		return nil
	}
	var armored sql.NullString
	err := a.db.QueryRowContext(ctx, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1 AND expires>now()", digest(pending)).Scan(&armored)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	d.PGP = &PGPView{EncryptedChallenge: armored.String}
	return nil
}
