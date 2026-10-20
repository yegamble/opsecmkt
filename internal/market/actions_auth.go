package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func init() {
	for _, path := range []string{"/setup", "/login", "/register"} {
		registerAction(path, actionSpec{Public: true, OwnTx: true, Run: authAction, Precheck: authPrecheck})
	}
	registerAction("/challenge", actionSpec{Public: true, Precheck: pendingPrecheck, Run: challengeAction})
	registerPage("challenge", pageSpec{})
	registerLoader("challenge", challengeLoader)
	registerLoader("login", signInNextLoader)
	registerLoader("register", signInNextLoader)
}

// signInNextID bounds the id query of a sign-in return target (listing, order and vendor ids are 64 hex digits).
var signInNextID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// signInNext returns the page a sign-in, registration or second sign-in step returns to (A-126), rebuilt from raw
// (a request URI or a "next" field), or "" to land on / as before. Only a registered GET page that is not switched
// off (pageSpec.Off), other than the sign-in pages themselves, is accepted, named exactly ("/orders"; never
// "//host", a scheme, a backslash, an encoded or extra character, or an action path), with at most its id query and
// /messages' recipient (to); other query parameters are dropped, and a malformed, repeated or out-of-pattern id or
// to refuses the whole target. The result is built here, never copied from raw, so it cannot lead off this market.
func (a *App) signInNext(raw string) string {
	if len(raw) > 512 {
		return ""
	}
	path, query, _ := strings.Cut(raw, "?")
	page, ok := strings.CutPrefix(path, "/")
	if !ok || !a.pageOn(page) || page == "login" || page == "register" || page == "setup" || page == "challenge" {
		return ""
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	keep := url.Values{}
	for key, pattern := range map[string]*regexp.Regexp{"id": signInNextID, "to": handlePattern} {
		switch v := q[key]; {
		case len(v) > 1 || len(v) == 1 && !pattern.MatchString(v[0]):
			return ""
		case len(v) == 1:
			keep.Set(key, v[0])
		}
	}
	if len(keep) == 0 {
		return "/" + page
	}
	return "/" + page + "?" + keep.Encode()
}

// signInNextLoader carries a valid ?next= on /login and /register into their forms and links (A-126). A form
// re-rendered from its POST (a wrong CAPTCHA answer, captchaRetry) keeps the posted next field instead (A-269).
func signInNextLoader(_ context.Context, a *App, r *http.Request, d *PageData) error {
	raw := r.URL.Query().Get("next")
	if r.Method == http.MethodPost {
		raw = r.PostForm.Get("next")
	}
	if d.Next = a.signInNext(raw); d.Next != "" {
		d.NextTitle = nextTitle(d.Next)
	}
	return nil
}

// nextTitle names the page of a signInNext target as its page title does ("Checkout", "Vendor Dashboard").
func nextTitle(next string) string {
	page, _, _ := strings.Cut(strings.TrimPrefix(next, "/"), "?")
	return pageTitle(page)
}

// orHome is next, or / when there is no return target.
func orHome(next string) string {
	if next == "" {
		return "/"
	}
	return next
}

// challengePath carries the already validated sign-in destination through the ten-minute challenge window. The
// pending row remains authoritative when a factor is verified; this URL copy only lets an expired challenge return
// to sign-in with the same destination after its cookie or row is gone.
func challengePath(next string) string {
	if next == "" {
		return "/challenge"
	}
	return "/challenge?next=" + url.QueryEscape(next)
}

var passwordWork = make(chan struct{}, 4)

// bcryptCost is the work factor for new password hashes. Tests lower it to bcrypt.MinCost (TestMain) so
// the -race suite stays within the request timeout; production keeps 12.
var bcryptCost = 12

// dummyHash is compared against when a login handle does not exist, so unknown and known handles cost the
// same bcrypt work. It is generated lazily at bcryptCost (after TestMain can lower it) from a random
// password, so it never matches any input.
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(randomToken()), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
})

// compareSignInPassword is /login's bcrypt comparison; tests count its calls to prove a refusal ran none.
var compareSignInPassword = bcrypt.CompareHashAndPassword

var handlePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

// errAccountChanged refuses a change whose signed-in account was given another role or suspended while the request
// was on its way (confirmedTx).
var errAccountChanged = fail(403, "Your account's role or status changed while this was being saved; nothing was changed. Reload the page.")

// errConfirmPaused refuses a password confirmation once the account's budget of failed confirmations is spent.
var errConfirmPaused = fail(429, "Too many failed password confirmations. Try again in ten minutes.")

// confirmPassword checks the signed-in user's current password for a sensitive change (bounded bcrypt concurrency).
// Only failed confirmations spend the account's budget of 10 per ten minutes (A-274, as A-153 did for sign-in): a
// wrong password here, or a wrong code after the right password (confirmCode); a correct confirmation spends nothing.
// Call it before opening a transaction.
func (a *App) confirmPassword(ctx context.Context, userID, password string) error {
	_, err := a.confirmPasswordHash(ctx, userID, password)
	return err
}

// confirmPasswordHash returns the hash that bcrypt actually verified. A confirmed form receipt
// compares it with the account's locked current hash before replaying or making a new change:
// a password replacement between this read and the transaction must not authorize either.
func (a *App) confirmPasswordHash(ctx context.Context, userID, password string) (string, error) {
	if password == "" || len(password) > 72 {
		return "", fail(400, "Enter your current password to confirm this change.")
	}
	// A spent budget is refused before a slot is taken, so it never runs bcrypt or keeps other checks waiting.
	key := ingressKey(ctx, "confirm:"+userID)
	if a.limited(key, 10) {
		return "", errConfirmPaused
	}
	// The slot is taken before the attempt is counted, so a busy refusal spends none of the user's budget.
	sem := workFor(ctx, passwordWork, adminPasswordWork)
	select {
	case sem <- struct{}{}:
	default:
		return "", fail(503, "Authentication is busy. Try again shortly.")
	}
	defer func() { <-sem }()
	// One attempt is reserved before bcrypt and refunded when the password is correct, so only failures spend the
	// budget and concurrent wrong passwords still cannot exceed it.
	if !a.allow(key, 10) {
		return "", errConfirmPaused
	}
	var hash string
	err := a.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash)
	if err == nil {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	}
	if err != nil {
		return "", fail(401, "Password incorrect")
	}
	a.refund(key)
	return hash, nil
}

// confirmCode runs check, the authenticator or recovery code of a confirmation whose password was correct. A code is
// a guess too (A-274): it reserves one attempt of the budget the correct password gave back, and only a wrong code
// (401) keeps it spent. Call it under the account row lock, so concurrent guesses cannot exceed the budget.
func (a *App) confirmCode(ctx context.Context, userID string, check func() error) error {
	key := ingressKey(ctx, "confirm:"+userID)
	if !a.allow(key, 10) {
		return errConfirmPaused
	}
	err := check()
	if he := (*httpError)(nil); err == nil || !errors.As(err, &he) || he.Code != 401 {
		a.refund(key)
	}
	return err
}

// validPassword is the password rule for registration, setup and password changes: 12–72 bytes (bcrypt
// ignores anything beyond 72).
func validPassword(p string) bool { return len(p) >= 12 && len(p) <= 72 }

// hashPassword hashes a new password at bcryptCost within the bounded bcrypt concurrency. Call it before
// opening a transaction.
func hashPassword(p string) (string, error) { return hashPasswordFor(context.Background(), p) }

// hashPasswordFor is hashPassword for a request: one through the private administrator ingress takes that
// ingress's own password slots (A-570).
func hashPasswordFor(ctx context.Context, p string) (string, error) {
	sem := workFor(ctx, passwordWork, adminPasswordWork)
	select {
	case sem <- struct{}{}:
	default:
		return "", fail(503, "Authentication is busy. Try again shortly.")
	}
	defer func() { <-sem }()
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcryptCost)
	return string(h), err
}

// confirmation selects extra behaviour for confirmedTxWith.
type confirmation struct {
	// Recovery also accepts one unused recovery code in "code" when TOTP is enrolled (and consumes it).
	Recovery bool
	// AfterPassword runs after a correct password and before the transaction opens (e.g. hashing a new password).
	AfterPassword func() error
	// Before runs in the transaction before the account row is locked. Every path takes an account's locks in the
	// order /challenge does — its pending sign-ins, then its recovery codes, then its row — so a change that also
	// deletes pending sign-ins or replaces recovery codes does that (or locks them) here (A-154, BE11-5).
	Before func(c *actionCtx) error
}

// lockRecoveryCodes locks the account's recovery codes, before its row, for a change that replaces or deletes them.
func lockRecoveryCodes(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, "SELECT 1 FROM recovery_codes WHERE user_id=$1 FOR UPDATE", userID)
	return err
}

// confirmedTx runs a sensitive change (an OwnTx action) in its own transaction. With confirm set, the current
// password is checked first (bcrypt outside any transaction) and, when TOTP is enrolled, a current
// authenticator code ("code") inside the transaction. run uses c.Tx; its Audit is written in the same
// transaction and notes how the change was confirmed.
func confirmedTx(c *actionCtx, confirm bool, run func(c *actionCtx) (actionResult, error)) (actionResult, error) {
	if !confirm {
		return confirmedTxWith(c, nil, run)
	}
	return confirmedTxWith(c, &confirmation{}, run)
}

// confirmedTxWith is confirmedTx with options; opts nil means no confirmation.
func confirmedTxWith(c *actionCtx, opts *confirmation, run func(c *actionCtx) (actionResult, error)) (actionResult, error) {
	a, ctx, uid := c.A, c.Ctx(), c.User.ID
	// A fixed confirmed receipt uses plain password/TOTP unless its route
	// explicitly owns factor-code locks. Dynamic OwnTx receipts never use
	// recovery, Before or AfterPassword hooks (notably session rotation).
	if c.ConfirmedFormNonce && (opts == nil || opts.AfterPassword != nil ||
		!c.FactorFormNonce && (opts.Recovery || opts.Before != nil)) {
		return actionResult{}, errors.New("unsupported confirmed form nonce options")
	}
	if c.OwnFormNonce && opts != nil && (opts.Recovery || opts.Before != nil || opts.AfterPassword != nil) {
		return actionResult{}, errors.New("unsupported own form nonce options")
	}
	confirm := opts != nil
	receipt := c.ConfirmedFormNonce || c.OwnFormNonce
	c.ReceiptConfirmRequired = c.OwnFormNonce && confirm
	var verifiedHash string
	if confirm {
		var err error
		if receipt {
			verifiedHash, err = a.confirmPasswordHash(ctx, uid, c.Form.Get("password"))
		} else {
			err = a.confirmPassword(ctx, uid, c.Form.Get("password"))
		}
		if err != nil {
			return actionResult{}, err
		}
		if opts.AfterPassword != nil {
			if err := opts.AfterPassword(); err != nil {
				return actionResult{}, err
			}
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	defer tx.Rollback()
	c.Tx = tx
	defer func() { c.Tx = nil }()
	how := "password"
	code := c.Form.Get("code")
	recovery := confirm && opts.Recovery && normalizeRecoveryCode(code) != ""
	var codeErr error
	if confirm {
		if opts.Before != nil {
			if err = opts.Before(c); err != nil {
				return actionResult{}, err
			}
		}
		// A recovery code is spent before the account row is locked (the lock order of confirmation.Before); it counts
		// only when TOTP turns out to be on, and otherwise matches nothing (codes exist only while TOTP is on).
		if recovery && !c.FactorFormNonce {
			var he *httpError
			if _, codeErr = a.useRecoveryCode(ctx, tx, uid, code); codeErr != nil && !errors.As(codeErr, &he) {
				return actionResult{}, codeErr
			}
		}
	}
	// The account row is locked, with or without a confirmation, and its role and suspension read again: a demotion
	// or suspension committed after this request's session was loaded refuses the change (A-156). Either one locks
	// this row too, so it is waited for.
	var totp, suspended, retentionRestricted bool
	var role, currentHash string
	if err = tx.QueryRowContext(ctx, "SELECT totp_enabled,role,suspended_at IS NOT NULL OR closed_at IS NOT NULL,password_hash,retention_restricted_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", uid).Scan(&totp, &role, &suspended, &currentHash, &retentionRestricted); err != nil {
		return actionResult{}, err
	}
	if role != c.User.Role || suspended {
		return actionResult{}, errAccountChanged
	}
	if retentionRestricted && (c.R == nil || !retentionActionAllowed(c.R.URL.Path)) {
		return actionResult{}, fail(403, retentionRestrictedMessage)
	}
	if receipt {
		if confirm && subtle.ConstantTimeCompare([]byte(verifiedHash), []byte(currentHash)) != 1 {
			return actionResult{}, fail(401, "Password changed while this was being saved. Sign in again; nothing was changed.")
		}
		// Role/password changes lock the actor before deleting sessions. A live session
		// must still exist in this transaction, and a concurrent revoke must serialize
		// with this read. clock_timestamp prevents a long transaction extending expiry.
		var live int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE token_hash=$1 AND user_id=$2
			AND expires>clock_timestamp() FOR SHARE`, digest(c.Token), uid).Scan(&live)
		if err == sql.ErrNoRows {
			return actionResult{}, fail(401, "Sign in again; nothing was changed.")
		}
		if err != nil {
			return actionResult{}, err
		}
		if c.ReceiptAuthorize != nil {
			if err = c.ReceiptAuthorize(c); err != nil {
				return actionResult{}, err
			}
		}
		var repeated bool
		var saved actionResult
		saved, repeated, err = claimFormSubmission(c)
		if err != nil || repeated {
			return saved, err
		}
	}
	if confirm && totp {
		if recovery {
			if c.FactorFormNonce {
				// Before locked the recovery rows before the user row. Consume
				// only for a NEW receipt; an exact completed replay returns above.
				err = a.confirmCode(ctx, uid, func() error {
					_, useErr := a.useRecoveryCode(ctx, tx, uid, code)
					return useErr
				})
			} else {
				err = a.confirmCode(ctx, uid, func() error { return codeErr })
			}
			how = "password and recovery code"
		} else {
			err = a.confirmCode(ctx, uid, func() error { return a.checkTOTP(ctx, tx, uid, code) })
			how = "password and authenticator code"
		}
		if err != nil {
			return actionResult{}, err
		}
	}
	res, err := run(c)
	if err != nil {
		return actionResult{}, err
	}
	if receipt && res.AfterCommit != nil {
		return actionResult{}, errors.New("confirmed form nonce cannot replay an after-commit effect")
	}
	if res.Audit != "" {
		if confirm {
			res.Audit += " (confirmed with " + how + ")"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid, res.Audit); err != nil {
			return actionResult{}, err
		}
		res.Audit = ""
	}
	if receipt {
		if res.Redirect == "" {
			res.Redirect = "/"
		}
		if err = finishFormSubmission(c, res); err != nil {
			return actionResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		// A lost acknowledgement does not tell us whether PostgreSQL committed. Do not
		// classify even a connection-looking error as safe to retry (A-209).
		return actionResult{}, commitOutcomeUnknown{err}
	}
	return res, nil
}

// commitOutcomeUnknown marks only an error returned by Commit, never a failure
// while preparing the transaction. The underlying error remains available for
// classification in server logs, but the HTTP response must not promise a retry.
type commitOutcomeUnknown struct{ cause error }

func (e commitOutcomeUnknown) Error() string { return e.cause.Error() }
func (e commitOutcomeUnknown) Unwrap() error { return e.cause }

// notifyOwner tells the account owner, inside tx, of a sign-in factor, key or recovery-code event (A-152). The text
// is link-free and never carries a code, secret, key fingerprint or address.
func notifyOwner(ctx context.Context, tx *sql.Tx, userID, event string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), userID, ownerNotice(event))
	return err
}

// ownerNotice is the text notifyOwner stores for event.
func ownerNotice(event string) string {
	return event + " If this was not you, someone else may know your password: change it and contact the market staff."
}

// authPrecheck refuses, before the request takes a post slot or reads the database, what authGate would
// refuse from the form alone: a malformed handle or password, and on /login and /register a CAPTCHA id that is
// not a live one issued to this session. A flood of junk sign-ins therefore holds no post slot a real sign-in
// needs (A-178). An id is checked only when one is sent: whether the CAPTCHA is on is a database setting, so a
// post without one goes on to authGate. A form shown while the CAPTCHA was on and posted after an
// administrator turned it off carries a live id, which passes; only an id that expired or was never issued
// to this browser is refused here even though the CAPTCHA is now off.
func authPrecheck(a *App, r *http.Request, token string) error {
	if !handlePattern.MatchString(strings.TrimSpace(r.PostForm.Get("handle"))) || !validPassword(r.PostForm.Get("password")) {
		return errAuthInput
	}
	if id := r.PostForm.Get("captcha_id"); id != "" && r.URL.Path != "/setup" {
		if _, ok := a.captchaIDValid(id, time.Now(), digest(token)); !ok {
			return errCaptchaExpired
		}
	}
	return nil
}

// authGate admits one password-bearing request: input bounds, CAPTCHA, a free handle for /register and the
// per-handle rate limit (or, for a paused sign-in rescued with an authenticator code, that code and the account's
// code budget), then bounded bcrypt concurrency, all before any expensive work. On success the caller must defer
// release().
func authGate(c *actionCtx) (handle, password, rescueID string, release func(), err error) {
	a, path := c.A, c.R.URL.Path
	handle, password = strings.TrimSpace(c.Form.Get("handle")), c.Form.Get("password")
	// Validate the input and the CAPTCHA before the rate limit, so malformed handles and failed CAPTCHAs
	// never create or increment a limiter entry (and cannot spend a real user's sign-in budget).
	if !handlePattern.MatchString(handle) || !validPassword(password) {
		err = errAuthInput
	} else if path != "/setup" {
		err = a.checkCaptcha(c) // P1: single-use image CAPTCHA unless an administrator turned it off
	}
	if err == nil && path == "/register" {
		// A taken handle is refused before any bcrypt, slot or budget. Recheck under the creation
		// advisory lock below, since this precheck alone cannot serialize case variants.
		var taken bool
		if qerr := a.db.QueryRowContext(c.Ctx(), "SELECT EXISTS(SELECT 1 FROM users WHERE lower(handle)=lower($1))", handle).Scan(&taken); qerr != nil {
			err = fail(503, "Service unavailable")
		} else if taken {
			err = fail(400, "Handle unavailable")
		}
	}
	// Each path has its own per-handle budget, so registering or setting up a handle never spends its sign-in
	// budget. Sign-in reserves one attempt here and authAction refunds it when the password is correct, so only
	// failed passwords spend "auth:" and concurrent guesses still cannot exceed it (A-153). A request through the
	// private administrator ingress spends that ingress's own budgets (ingressKey, A-570).
	key, refused := ingressKey(c.Ctx(), signInKey(handle)), errSignInPaused
	if path != "/login" {
		key, refused = ingressKey(c.Ctx(), strings.TrimPrefix(path, "/")+":"+strings.ToLower(handle)), fail(429, "Too many attempts. Try again in ten minutes.")
	}
	if err == nil && a.limited(key, 10) {
		err = refused
		if path == "/login" && c.Form.Get("rescue_code") != "" {
			// A-571: a paused handle's account with TOTP may still sign in with its current code and its password.
			// The code is checked first, so a wrong or used code, an unknown handle or an account without TOTP is
			// refused here, before bcrypt and without spending any budget, and only a right code spends an attempt of
			// the account's code budget, the one /challenge uses (bound to this account identity).
			rescueID, err = a.rescueAccount(c.Ctx(), handle, c.Form.Get("rescue_code"))
			if err == nil {
				key, refused = ingressKey(c.Ctx(), "totp-verify:"+rescueID), errRescueRefused
				if a.limited(key, 10) {
					err = refused
				}
			}
			if err == errRescueRefused {
				a.logRescueRefusal()
			}
		}
	}
	if err != nil {
		return "", "", "", nil, err
	}
	// A password-check slot is taken only now, so requests refused above never hold one while real
	// sign-ins wait. The attempt is counted after the slot, so a busy refusal spends no budget.
	sem := workFor(c.Ctx(), passwordWork, adminPasswordWork)
	select {
	case sem <- struct{}{}:
	default:
		return "", "", "", nil, fail(503, "Authentication is busy. Try again shortly.")
	}
	release = func() { <-sem }
	// Reserve atomically before bcrypt, including recovery attempts. The earlier limited check
	// only avoids taking a work slot for an already-exhausted budget; it cannot admit a race.
	if !a.allow(key, 10) {
		release()
		if rescueID != "" {
			a.logRescueRefusal()
		}
		return "", "", "", nil, refused
	}
	return handle, password, rescueID, release, nil
}

func sixDigitTOTP(code string) bool {
	if len(code) != totpDigits {
		return false
	}
	for i := range code {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// errRescueRefused is every refusal of a paused sign-in rescued with an authenticator code (A-571): the same 401 as a
// wrong password, so the rescue path never tells which of the handle, code or password was wrong.
var errRescueRefused = fail(401, "Invalid handle or password")

// rescueAccount returns the account a paused /login's authenticator code may sign in (A-571): the handle's active
// account with TOTP on, for which code is a current, unused code. It reads without locking and writes nothing (no
// bcrypt, no budget, no TOTP step): rescueSignIn checks the code again under the account lock, with its replay guard.
// Every refusal is errRescueRefused.
func (a *App) rescueAccount(ctx context.Context, handle, code string) (string, error) {
	if !sixDigitTOTP(code) {
		return "", errRescueRefused
	}
	account, err := lookupSignInAccount(ctx, a.db, handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errRescueRefused
	} else if err != nil {
		return "", fail(503, "Service unavailable")
	}
	if account.suspended || account.banned || !account.totpEnabled {
		return "", errRescueRefused
	}
	var sealed string
	var enabled bool
	var last int64
	err = a.db.QueryRowContext(ctx, "SELECT totp_secret,totp_enabled,totp_last_step FROM users WHERE id=$1", account.id).Scan(&sealed, &enabled, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errRescueRefused
	} else if err != nil {
		return "", fail(503, "Service unavailable")
	}
	if _, err = a.matchTOTP(sealed, enabled, last, code); err != nil {
		return "", errRescueRefused
	}
	return account.id, nil
}

type signInAccount struct {
	id, hash, role                 string
	suspended, banned, totpEnabled bool
}

// lookupSignInAccount preserves exact handles on legacy installations. A fallback may resolve only one
// account; two case variants are indistinguishable and therefore have no fallback sign-in identity.
func lookupSignInAccount(ctx context.Context, db *sql.DB, handle string) (signInAccount, error) {
	var account signInAccount
	err := db.QueryRowContext(ctx, `SELECT id,password_hash,role,suspended_at IS NOT NULL,banned_at IS NOT NULL,totp_enabled
		FROM users WHERE handle=$1 AND closed_at IS NULL`, handle).Scan(&account.id, &account.hash, &account.role, &account.suspended, &account.banned, &account.totpEnabled)
	if !errors.Is(err, sql.ErrNoRows) {
		return account, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,password_hash,role,suspended_at IS NOT NULL,banned_at IS NOT NULL,totp_enabled
		FROM users WHERE lower(handle)=lower($1) AND closed_at IS NULL LIMIT 2`, handle)
	if err != nil {
		return signInAccount{}, err
	}
	defer rows.Close()
	matches := 0
	for rows.Next() {
		matches++
		if err = rows.Scan(&account.id, &account.hash, &account.role, &account.suspended, &account.banned, &account.totpEnabled); err != nil {
			return signInAccount{}, err
		}
	}
	if err = rows.Err(); err != nil {
		return signInAccount{}, err
	}
	if matches != 1 {
		return signInAccount{}, sql.ErrNoRows
	}
	return account, nil
}

// errAuthInput refuses a malformed handle or password.
var errAuthInput = fail(400, "Use a 3–32 character handle (letters, digits, underscores) and a password of 12–72 bytes.")

// signInKey is the limiter key of a handle's sign-in budget (case-insensitive).
func signInKey(handle string) string { return "auth:" + strings.ToLower(handle) }

// errSignInPaused refuses sign-in to a handle whose budget of failed passwords is spent. Anyone can spend it
// (the budget is per handle: there is no per-client signal over Tor), so it does not blame the user.
var errSignInPaused = fail(429, "Sign-in for this handle is paused for up to 10 minutes after too many failed passwords. Someone else may have caused this, and your password may be fine. Try again later.")

// authAction serves /setup, /login and /register. bcrypt runs before any transaction is opened.
func authAction(c *actionCtx) (actionResult, error) {
	a, ctx, f, path, w := c.A, c.Ctx(), c.Form, c.R.URL.Path, c.W
	vendor := false
	if path == "/register" {
		// A-544: a vendor account is a buyer account with a vendor application under the current vendor policy.
		switch f.Get("account_type") {
		case "", "buyer":
		case "vendor":
			vendor = true
		default:
			return actionResult{}, fail(400, "Choose a buyer account or a vendor account")
		}
		policy, err := a.readSiteAccess(ctx)
		if err != nil {
			return actionResult{}, err
		}
		if !policy.RegistrationOpen {
			return actionResult{}, fail(http.StatusForbidden, registrationClosedMessage)
		}
	}
	handle, password, rescueID, release, err := authGate(c)
	if err == errSignInPaused {
		a.logSignInRefusals()
	}
	if err != nil {
		return actionResult{}, err
	}
	defer release()
	var installed bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	if path == "/login" {
		account, err := lookupSignInAccount(ctx, a.db, handle)
		hash := []byte(account.hash)
		if err != nil {
			hash = dummyHash()
		}
		var heldPending bool
		if rescueID != "" {
			if account.id != rescueID { // the handle now resolves to another account
				a.logRescueRefusal()
				return actionResult{}, errRescueRefused
			}
			// Whether this browser's pending sign-in is live now, before bcrypt: rescueSignIn refuses if it is gone by
			// then, as a /challenge from this browser completed it meanwhile.
			if pending := pendingCookie(c.R); pending != "" {
				if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pending_logins WHERE token_hash=$1 AND expires>now())", digest(pending)).Scan(&heldPending); err != nil {
					return actionResult{}, fail(503, "Service unavailable")
				}
			}
		}
		check := compareSignInPassword(hash, []byte(password))
		if err != nil || check != nil {
			a.logSignInRefusals()
			if rescueID != "" {
				a.logRescueRefusal()
			}
			return actionResult{}, fail(401, "Invalid handle or password")
		}
		if a.publicAdministrator(ctx, &User{Role: account.role}) {
			return actionResult{}, fail(404, pageNotFound)
		}
		if rescueID != "" {
			res, err := a.rescueSignIn(c, account.id, account.hash, f.Get("rescue_code"), a.signInNext(f.Get("next")), heldPending)
			if err == errRescueRefused {
				a.logRescueRefusal()
			}
			return res, err
		}
		a.refund(ingressKey(ctx, signInKey(handle))) // a correct password spends no sign-in budget
		if account.suspended {
			return actionResult{}, suspendedRefusal(ctx, a.db, account.banned)
		}
		return a.secondFactor(c, account.id, account.hash, strings.TrimSpace(f.Get("rescue_code")), a.signInNext(f.Get("next")))
	}
	if path == "/setup" && (installed || a.setupToken == "" || subtle.ConstantTimeCompare([]byte(f.Get("token")), []byte(a.setupToken)) != 1) {
		return actionResult{}, fail(403, "Setup unavailable or token incorrect")
	}
	if path == "/register" && !installed {
		return actionResult{}, fail(403, "Complete installation first")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return actionResult{}, fail(500, "Unable to create account")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	defer tx.Rollback()
	var id string
	if path == "/setup" {
		if id, err = installFirstAdmin(ctx, tx, handle, string(hash), f.Get("site_name")); err != nil {
			return actionResult{}, err
		}
	} else {
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
			return actionResult{}, fail(503, "Service unavailable")
		}
		if err = registrationAllowedTx(ctx, tx); err != nil {
			return actionResult{}, err
		}
		if err = refuseTakenHandle(ctx, tx, handle); err != nil {
			return actionResult{}, err
		}
		id = randomToken()
		if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,'buyer')", id, handle, string(hash)); err != nil {
			return actionResult{}, accountInsertError(err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Account created: buyer"); err != nil {
			return actionResult{}, fail(500, "Audit failed")
		}
		if vendor {
			if _, err = submitVendorApplication(ctx, tx, id); err != nil {
				return actionResult{}, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, fail(500, "Unable to save account")
	}
	if err = a.login(ctx, w, id, rotationSession(c), string(hash)); err != nil {
		return actionResult{}, fail(500, "Account created; sign in to continue")
	}
	if path == "/setup" {
		return actionResult{Redirect: "/admin?welcome=1"}, nil
	}
	if vendor {
		return actionResult{Redirect: "/account#vendor-application"}, nil // where the application's status is shown
	}
	return actionResult{Redirect: orHome(a.signInNext(f.Get("next")))}, nil
}

// rescueSignIn finishes a paused /login whose authenticator code (rescueAccount) and password were right: the A-159(a)
// escape from a per-handle password lockout, open to every account with TOTP since A-571 (before, only to a sole
// administrator). authGate has already charged the account's code budget. One transaction ends this browser's live
// pending sign-in, then checks the code again under the account lock with its replay guard (checkTOTP) and starts the
// session (startSession: suspension, closure, password change and public-administrator refusals), so the used step and
// the session commit together. The pending sign-in is taken before the account row, the order /challenge takes them
// in (S-1), so the two never deadlock; an expired one is left to /challenge's cleanup, as in passwordAndCodeSignIn.
// heldPending says it was live before bcrypt: if it is gone now, a /challenge from this browser completed it meanwhile
// and this request is refused rather than making a second session. Recovery codes are not accepted here, and another
// enrolled factor is not asked for: /challenge also accepts the authenticator code alone. Every refusal is
// errRescueRefused.
func (a *App) rescueSignIn(c *actionCtx, id, checkedHash, code, next string, heldPending bool) (actionResult, error) {
	ctx := c.Ctx()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	defer tx.Rollback()
	if pending := pendingCookie(c.R); pending != "" {
		res, err := tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE token_hash=$1 AND expires>now()", digest(pending))
		if err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		if n, _ := res.RowsAffected(); heldPending && n == 0 {
			return actionResult{}, errRescueRefused
		}
	}
	var he *httpError
	if err = a.checkTOTP(ctx, tx, id, code); errors.As(err, &he) || errors.Is(err, sql.ErrNoRows) {
		return actionResult{}, errRescueRefused // used meanwhile, TOTP turned off, or the account is gone
	} else if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	token, err := a.startSession(ctx, tx, id, rotationSession(c), "password and TOTP", digest(checkedHash))
	if errors.As(err, &he) {
		return actionResult{}, errRescueRefused
	} else if err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	a.cookie(c.W, "session", token, 43200)
	a.cookie(c.W, "pending", "", -1)
	return actionResult{Redirect: orHome(next)}, nil
}

// errPasswordChanged refuses a sign-in whose password was checked before the account's password changed (A-157).
var errPasswordChanged = fail(401, "Sign-in verification expired. Sign in again.")

// errFactorsChanged refuses password-only sign-in when a factor was enabled after
// secondFactor read the enrollment state. A new sign-in must use that factor.
var errFactorsChanged = fail(401, "Sign-in verification changed. Sign in again.")

// errSetupComplete refuses first-run setup once the market is installed.
var errSetupComplete = fail(403, "Setup already complete")

// setupSiteName applies first-run setup's marketplace-name rule: surrounding space trimmed, "OPSMKT" when
// blank, at most 80 characters, matching the form's maxlength.
func setupSiteName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "OPSMKT"
	}
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return "", fail(400, "Invalid site name")
	}
	if utf8.RuneCountInString(name) > 80 {
		return "", fail(400, "Site name too long")
	}
	return name, nil
}

// installFirstAdmin is first-run setup's write, shared by POST /setup and the host's -bootstrap-admin
// (BootstrapAdmin). In tx, under advisory lock 782493 (shared with startup migrations), it re-checks that the
// market is not installed, creates the one administrator with hash (bcrypt, made before tx opened), records
// installed and site_name and audits the new account. It returns the account's id; on an error the caller
// rolls tx back, so nothing is written.
func installFirstAdmin(ctx context.Context, tx *sql.Tx, handle, hash, siteName string) (string, error) {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		return "", fail(503, "Service unavailable")
	}
	var installed bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil || installed {
		return "", errSetupComplete
	}
	if err := refuseTakenHandle(ctx, tx, handle); err != nil {
		return "", err
	}
	id := randomToken()
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,'admin')", id, handle, hash); err != nil {
		return "", accountInsertError(err)
	}
	name, err := setupSiteName(siteName)
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('installed','true'),('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", name); err != nil {
		return "", fail(500, "Setup failed")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Account created: admin"); err != nil {
		return "", fail(500, "Audit failed")
	}
	return id, nil
}

// refuseTakenHandle runs under creation advisory lock 782493 for both HTTP and CLI account creation.
// The index is deliberately non-unique: a legacy database may already contain case variants. A banned account's
// handle stays taken after the account is closed and erased (banned_handles, A-546).
func refuseTakenHandle(ctx context.Context, tx *sql.Tx, handle string) error {
	var taken bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE lower(handle)=lower($1)) OR EXISTS(SELECT 1 FROM banned_handles WHERE handle=lower($1))", handle).Scan(&taken); err != nil {
		return fail(503, "Service unavailable")
	}
	if taken {
		return fail(400, "Handle unavailable")
	}
	return nil
}

// Only a uniqueness violation is a taken handle. Do not describe other database failures as a collision.
func accountInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fail(400, "Handle unavailable")
	}
	return fail(503, "Service unavailable")
}

// enrolledFactors lists the registered second factors the user has enrolled.
func (a *App) enrolledFactors(ctx context.Context, userID string) ([]string, error) {
	var names []string
	for _, p := range factors {
		ok, err := p.Enrolled(ctx, a.db, userID)
		if err != nil {
			return nil, err
		}
		if ok {
			names = append(names, p.Name())
		}
	}
	return names, nil
}

// secondFactor finishes a password login checked against passwordHash: a session when no factor is enrolled,
// otherwise a 10-minute pending login (cookie "pending") completed by POST /challenge. Either is refused once the
// password has changed since it was checked (startSession, A-157). next (signInNext, "" for /) is where the sign-in
// lands; a pending login keeps it for /challenge (A-126). code is the authenticator code typed on /login ("" for
// none): when TOTP is the only enrolled factor it completes the sign-in here (passwordAndCodeSignIn, A-549); with
// another factor enrolled it is ignored, so it never skips that factor.
func (a *App) secondFactor(c *actionCtx, userID, passwordHash, code, next string) (actionResult, error) {
	ctx := c.Ctx()
	names, err := a.enrolledFactors(ctx, userID)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	// PGP sign-in that is on but lost to an unreadable key is never dropped silently (A-272): the owner is told once,
	// and without another factor the sign-in is refused rather than finished with the password alone.
	lost, err := pgpSignInLost(ctx, a.db, userID)
	if err == nil && lost {
		err = a.notifyUnreadableKey(ctx, userID)
	}
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	if lost && len(names) == 0 {
		return actionResult{}, unreadableKeyRefusal(ctx, a.db, userID)
	}
	if len(names) == 0 {
		var he *httpError
		if err = a.login(ctx, c.W, userID, rotationSession(c), passwordHash); errors.As(err, &he) { // suspension, password or factor changed
			return actionResult{}, err
		} else if err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		return actionResult{Redirect: orHome(next)}, nil
	}
	if code != "" && len(names) == 1 && names[0] == "totp" {
		return a.passwordAndCodeSignIn(c, userID, passwordHash, code, next)
	}
	pending := randomToken()
	if _, err = a.db.ExecContext(ctx, "INSERT INTO pending_logins(token_hash,user_id,expires,password_digest,next) VALUES($1,$2,now()+interval '10 minutes',$3,$4)", digest(pending), userID, digest(passwordHash), next); err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	a.cookie(c.W, "pending", pending, 600)
	return actionResult{Redirect: challengePath(next)}, nil
}

// passwordAndCodeSignIn finishes a /login whose password was checked against passwordHash and which carried an
// authenticator code for an account whose only second factor is TOTP (A-549). It charges the same per-account factor
// budget as /challenge (totpFactor.Verify) and checks the code with checkTOTP, whose replay guard is committed with the
// session (startSession: rotation, suspension and password-change refusal, the "Signed in with password and TOTP"
// audit row). A wrong, expired or reused code opens the usual pending sign-in instead, recorded as its first failed
// second step (recordFactorFailure, A-152), and /challenge says the code was not accepted: a correct password reaches
// /challenge anyway, so this answers nothing a password guess could not already learn. Recovery codes are /challenge's.
func (a *App) passwordAndCodeSignIn(c *actionCtx, userID, passwordHash, code, next string) (actionResult, error) {
	ctx := c.Ctx()
	if !a.allow(ingressKey(ctx, "totp-verify:"+userID), 10) {
		return actionResult{}, errTOTPBudget
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	defer tx.Rollback()
	if pending := pendingCookie(c.R); pending != "" {
		// This browser's earlier two-step sign-in is replaced by this one either way (its cookie is overwritten);
		// other browsers' pending sign-ins remain. Deleted first: /challenge's lock order is its live pending sign-in,
		// then the account row (checkTOTP), then every expired pending sign-in (its cleanup). An expired row is left to
		// that cleanup: locking it here before the account row would deadlock with a /challenge holding the account.
		if _, err = tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE token_hash=$1 AND expires>now()", digest(pending)); err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
	}
	var he *httpError
	if err = a.checkTOTP(ctx, tx, userID, code); errors.As(err, &he) && he.Code == 401 {
		// checkTOTP wrote nothing; tx still holds the account row, so the record below cannot race a sign-in.
		pending := randomToken()
		if _, err = tx.ExecContext(ctx, "INSERT INTO pending_logins(token_hash,user_id,expires,password_digest,next,factor_failed) VALUES($1,$2,now()+interval '10 minutes',$3,$4,true)", digest(pending), userID, digest(passwordHash), next); err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		if err = recordFirstFactorFailure(ctx, tx, userID, "TOTP"); err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		if err = tx.Commit(); err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		a.cookie(c.W, "pending", pending, 600)
		notice := "signin-code-wrong"
		a.setNotice(c.W, notice, "")
		return actionResult{Redirect: challengePath(next)}, nil
	} else if he != nil {
		return actionResult{}, err // TOTP turned off meanwhile, or a secret this server cannot read
	} else if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	token, err := a.startSession(ctx, tx, userID, rotationSession(c), "password and TOTP", digest(passwordHash))
	if errors.As(err, &he) {
		return actionResult{}, err // suspension, password changed or public administrator
	} else if err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	a.cookie(c.W, "session", token, 43200)
	a.cookie(c.W, "pending", "", -1)
	return actionResult{Redirect: orHome(next)}, nil
}

func pendingCookie(r *http.Request) string {
	if c, err := r.Cookie("pending"); err == nil && len(c.Value) == 64 {
		return c.Value
	}
	return ""
}

// pendingPrecheck refuses a sign-in challenge without a pending cookie before it takes a post slot or opens a
// transaction (A-178); the action makes the same check.
func pendingPrecheck(_ *App, r *http.Request, _ string) error {
	if pendingCookie(r) == "" {
		return fail(401, "Sign-in verification expired. Sign in again.")
	}
	return nil
}

func challengeAction(c *actionCtx) (actionResult, error) {
	a, ctx := c.A, c.Ctx()
	pending := pendingCookie(c.R)
	if pending == "" {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	// The pending login must exist before a limiter entry is created, so random cookies cost nothing.
	var userID, stored string
	var checked sql.NullString
	err := c.Tx.QueryRowContext(ctx, "SELECT user_id,password_digest,next FROM pending_logins WHERE token_hash=$1 AND expires>now() FOR UPDATE", digest(pending)).Scan(&userID, &checked, &stored)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	if err != nil {
		return actionResult{}, err
	}
	if !a.allow(ingressKey(ctx, "challenge:"+digest(pending)), 10) {
		return actionResult{}, fail(429, "Too many attempts. Try again in ten minutes.")
	}
	p := factorByName(c.Form.Get("method"))
	if p == nil {
		return actionResult{}, fail(400, "Choose a verification method")
	}
	// Through c.Tx: this transaction holds a connection and the pending row's lock, so a pool read here could
	// wait for a connection that requests queued on the same lock hold (A-150).
	if ok, err := p.Enrolled(ctx, c.Tx, userID); err != nil {
		return actionResult{}, err
	} else if !ok {
		return actionResult{}, fail(400, "Choose a verification method")
	}
	method := signInMethod(p.Name(), c.Form)
	if err = p.Verify(c, userID); err != nil {
		return actionResult{}, recordFactorFailure(c, pending, userID, method, err)
	}
	if _, err = c.Tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE token_hash=$1 OR expires<now()", digest(pending)); err != nil {
		return actionResult{}, err
	}
	token, err := a.startSession(ctx, c.Tx, userID, rotationSession(c), "password and "+method, checked.String)
	if err != nil {
		return actionResult{}, err
	}
	// The sign-in lands on the page /login was sent from (A-126), kept by the pending sign-in and checked again; a
	// "next" posted here is ignored. A recovery-code sign-in lands on /totp with a one-shot notice (A-287; set here, as
	// the request had no session when it started), which says how many codes remain or that TOTP is now off (A-154)
	// and links on to that page: next stays in the URL, a link target shown only beside the notice.
	target := a.signInNext(stored)
	next, notice := orHome(target), ""
	if method == "recovery code" {
		next = "/totp"
		notice = "signin-recovery"
		if turnOffTOTPAtSignIn(c.Form) {
			notice = "signin-totp-off"
		}
		if target != "" {
			next += "?next=" + url.QueryEscape(target)
		}
	}
	return actionResult{Redirect: next, AfterCommit: func(w http.ResponseWriter) {
		a.cookie(w, "pending", "", -1)
		a.cookie(w, "session", token, 43200)
		a.setNotice(w, notice, "")
	}}, nil
}

// signInMethod names the second factor a /challenge request uses, as totpFactor.Verify and pgpFactor.Verify read it.
func signInMethod(factor string, f url.Values) string {
	switch {
	case factor == "pgp":
		return "PGP code"
	case f.Get("recovery") != "":
		return "recovery code"
	}
	return "TOTP"
}

// recordFactorFailure answers a failed second-factor check (err, a 401: wrong, used or missing code). The first
// failure of each pending sign-in writes an audit row and notifies the owner, who then knows that someone has the
// password (A-152); c.Tx is committed with that record (the failed check changed nothing else) and err returned.
func recordFactorFailure(c *actionCtx, pending, userID, method string, err error) error {
	var he *httpError
	if !errors.As(err, &he) || he.Code != 401 {
		return err
	}
	ctx, tx := c.Ctx(), c.Tx
	res, uerr := tx.ExecContext(ctx, "UPDATE pending_logins SET factor_failed=true WHERE token_hash=$1 AND NOT factor_failed", digest(pending))
	if uerr != nil {
		return uerr
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return err
	}
	if uerr = recordFirstFactorFailure(ctx, tx, userID, method); uerr != nil {
		return uerr
	}
	if uerr = tx.Commit(); uerr != nil {
		return uerr
	}
	return err
}

// recordFirstFactorFailure writes, in tx, the audit row and owner notice of a pending sign-in's first failed second
// step (A-152): at /challenge (recordFactorFailure) or with a code typed on /login (passwordAndCodeSignIn).
func recordFirstFactorFailure(ctx context.Context, tx *sql.Tx, userID, method string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, "Correct password but failed second sign-in step ("+method+")"); err != nil {
		return err
	}
	return notifyOwner(ctx, tx, userID, "Someone entered your password correctly but failed the second sign-in step ("+method+").")
}

func challengeLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.Title = "Verify sign-in"
	fromURL := a.signInNext(r.URL.Query().Get("next"))
	pending := pendingCookie(r)
	if pending == "" {
		d.Next = fromURL
		return nil
	}
	var userID, next string
	var unexpired bool
	err := a.db.QueryRowContext(ctx, "SELECT user_id,next,expires>now() FROM pending_logins WHERE token_hash=$1", digest(pending)).Scan(&userID, &next, &unexpired)
	if err == sql.ErrNoRows {
		d.Next = fromURL
		return nil
	}
	if err != nil {
		return err
	}
	d.Next = a.signInNext(next)
	if !unexpired {
		return nil
	}
	names, err := a.enrolledFactors(ctx, userID)
	if err != nil {
		return err
	}
	d.Auth = &AuthView{}
	for _, n := range names {
		d.Auth.TOTPEnrolled = d.Auth.TOTPEnrolled || n == "totp"
		d.Auth.PGPEnrolled = d.Auth.PGPEnrolled || n == "pgp"
	}
	return nil
}

// startSession rotates the caller's session inside tx and records the sign-in with how it was made (method, e.g.
// "password and TOTP"); the caller sets the cookie after commit.
// It refuses a suspended or banned account (suspendedRefusal), and with errPasswordChanged a sign-in whose password was
// checked against another hash than the account's current one (passwordDigest, the SHA-256 of that hash): a
// password change ends every sign-in in progress, including one whose password was checked just before the change
// committed (A-157). Password-only sign-in also rechecks both required factor flags: an enrollment may have
// committed after secondFactor's unlocked reads, without changing the password. Check the raw PGP flag so an
// enabled factor with an unreadable key cannot silently become password-only. The account row stays share-locked
// through session creation: earlier suspension, password changes and enrollment are seen; later changes wait.
func (a *App) startSession(ctx context.Context, tx *sql.Tx, id, old, method, passwordDigest string) (string, error) {
	var suspended, banned, totp, pgp bool
	var hash, role string
	if err := tx.QueryRowContext(ctx, "SELECT suspended_at IS NOT NULL OR closed_at IS NOT NULL,password_hash,totp_enabled,pgp_2fa,role,banned_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", id).Scan(&suspended, &hash, &totp, &pgp, &role, &banned); err != nil {
		return "", err
	}
	if a.publicAdministrator(ctx, &User{Role: role}) {
		return "", fail(404, pageNotFound)
	}
	if suspended {
		return "", suspendedRefusal(ctx, tx, banned)
	}
	if subtle.ConstantTimeCompare([]byte(digest(hash)), []byte(passwordDigest)) != 1 {
		return "", errPasswordChanged
	}
	if method == "password" && (totp || pgp) {
		return "", errFactorsChanged
	}
	token := randomToken()
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1 OR expires<now()", digest(old)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(token), id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET last_authenticated_at=now(),inactivity_notified_at=NULL WHERE id=$1 AND deletion_requested_at IS NULL", id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Signed in with "+method); err != nil {
		return "", err
	}
	return token, nil
}

// login starts a password-only session for a password checked against passwordHash (startSession).
func (a *App) login(ctx context.Context, w http.ResponseWriter, id, old, passwordHash string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	token, err := a.startSession(ctx, tx, id, old, "password", digest(passwordHash))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.cookie(w, "session", token, 43200)
	return nil
}
