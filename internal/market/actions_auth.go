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
// (a request URI or a "next" field), or "" to land on / as before. Only a registered GET page other than the sign-in
// pages themselves is accepted, named exactly ("/orders"; never "//host", a scheme, a backslash, an encoded or extra
// character, or an action path), with at most its id query and /messages' recipient (to); other query parameters
// are dropped, and a malformed, repeated or out-of-pattern id or to refuses the whole target. The result is built
// here, never copied from raw, so it cannot lead off this market.
func signInNext(raw string) string {
	if len(raw) > 512 {
		return ""
	}
	path, query, _ := strings.Cut(raw, "?")
	page, ok := strings.CutPrefix(path, "/")
	if _, known := pages[page]; !ok || !known || page == "login" || page == "register" || page == "setup" || page == "challenge" {
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
func signInNextLoader(_ context.Context, _ *App, r *http.Request, d *PageData) error {
	raw := r.URL.Query().Get("next")
	if r.Method == http.MethodPost {
		raw = r.PostForm.Get("next")
	}
	if d.Next = signInNext(raw); d.Next != "" {
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

var handlePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

// errAccountChanged refuses a change whose signed-in account was given another role or suspended while the request
// was on its way (confirmedTx).
var errAccountChanged = fail(403, "Your account's role or status changed while this was being saved; nothing was changed. Reload the page.")

// confirmPassword checks the signed-in user's current password for a sensitive change (per-user attempt
// limit, bounded bcrypt concurrency). Call it before opening a transaction.
func (a *App) confirmPassword(ctx context.Context, userID, password string) error {
	if password == "" || len(password) > 72 {
		return fail(400, "Enter your current password to confirm this change.")
	}
	// The slot is taken before the attempt is counted, so a busy refusal spends none of the user's budget.
	select {
	case passwordWork <- struct{}{}:
	default:
		return fail(503, "Authentication is busy. Try again shortly.")
	}
	defer func() { <-passwordWork }()
	if !a.allow("confirm:"+userID, 10) {
		return fail(429, "Too many attempts. Try again in ten minutes.")
	}
	var hash string
	err := a.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash)
	if err == nil {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	}
	if err != nil {
		return fail(401, "Password incorrect")
	}
	return nil
}

// validPassword is the password rule for registration, setup and password changes: 12–72 bytes (bcrypt
// ignores anything beyond 72).
func validPassword(p string) bool { return len(p) >= 12 && len(p) <= 72 }

// hashPassword hashes a new password at bcryptCost within the bounded bcrypt concurrency. Call it before
// opening a transaction.
func hashPassword(p string) (string, error) {
	select {
	case passwordWork <- struct{}{}:
	default:
		return "", fail(503, "Authentication is busy. Try again shortly.")
	}
	defer func() { <-passwordWork }()
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
	confirm := opts != nil
	if confirm {
		if err := a.confirmPassword(ctx, uid, c.Form.Get("password")); err != nil {
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
		if recovery {
			var he *httpError
			if _, codeErr = a.useRecoveryCode(ctx, tx, uid, code); codeErr != nil && !errors.As(codeErr, &he) {
				return actionResult{}, codeErr
			}
		}
	}
	// The account row is locked, with or without a confirmation, and its role and suspension read again: a demotion
	// or suspension committed after this request's session was loaded refuses the change (A-156). Either one locks
	// this row too, so it is waited for.
	var totp, suspended bool
	var role string
	if err = tx.QueryRowContext(ctx, "SELECT totp_enabled,role,suspended_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", uid).Scan(&totp, &role, &suspended); err != nil {
		return actionResult{}, err
	}
	if role != c.User.Role || suspended {
		return actionResult{}, errAccountChanged
	}
	if confirm && totp {
		if recovery {
			err = codeErr
			how = "password and recovery code"
		} else {
			err = a.checkTOTP(ctx, tx, uid, code)
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
	if res.Audit != "" {
		if confirm {
			res.Audit += " (confirmed with " + how + ")"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid, res.Audit); err != nil {
			return actionResult{}, err
		}
		res.Audit = ""
	}
	return res, tx.Commit()
}

// notifyOwner tells the account owner, inside tx, of a sign-in factor, key or recovery-code event (A-152). The text
// is link-free and never carries a code, secret, key fingerprint or address.
func notifyOwner(ctx context.Context, tx *sql.Tx, userID, event string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), userID,
		event+" If this was not you, someone else may know your password: change it and contact the market staff.")
	return err
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
// per-handle rate limit, then bounded bcrypt concurrency, all before any expensive work. On success the
// caller must defer release().
func authGate(c *actionCtx) (handle, password string, release func(), err error) {
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
		// A taken handle is refused before any bcrypt, slot or budget. The INSERT's unique key still
		// refuses a handle registered concurrently after this check.
		var taken bool
		if qerr := a.db.QueryRowContext(c.Ctx(), "SELECT EXISTS(SELECT 1 FROM users WHERE handle=$1)", handle).Scan(&taken); qerr != nil {
			err = fail(503, "Service unavailable")
		} else if taken {
			err = fail(400, "Handle unavailable")
		}
	}
	// Each path has its own per-handle budget, so registering or setting up a handle never spends its sign-in
	// budget. Sign-in reserves one attempt here and authAction refunds it when the password is correct, so only
	// failed passwords spend "auth:" and concurrent guesses still cannot exceed it (A-153).
	key, refused := signInKey(handle), errSignInPaused
	if path != "/login" {
		key, refused = strings.TrimPrefix(path, "/")+":"+strings.ToLower(handle), fail(429, "Too many attempts. Try again in ten minutes.")
	}
	if err == nil && a.limited(key, 10) {
		err = refused
	}
	if err != nil {
		return "", "", nil, err
	}
	// A password-check slot is taken only now, so requests refused above never hold one while real
	// sign-ins wait. The attempt is counted after the slot, so a busy refusal spends no budget.
	select {
	case passwordWork <- struct{}{}:
	default:
		return "", "", nil, fail(503, "Authentication is busy. Try again shortly.")
	}
	release = func() { <-passwordWork }
	if !a.allow(key, 10) {
		release()
		return "", "", nil, refused
	}
	return handle, password, release, nil
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
	handle, password, release, err := authGate(c)
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
		var id, stored string
		var suspended bool
		err := a.db.QueryRowContext(ctx, "SELECT id,password_hash,suspended_at IS NOT NULL FROM users WHERE handle=$1", handle).Scan(&id, &stored, &suspended)
		hash := []byte(stored)
		if err != nil {
			hash = dummyHash()
		}
		check := bcrypt.CompareHashAndPassword(hash, []byte(password))
		if err != nil || check != nil {
			a.logSignInRefusals()
			return actionResult{}, fail(401, "Invalid handle or password")
		}
		a.refund(signInKey(handle)) // a correct password spends no sign-in budget
		if suspended {
			return actionResult{}, suspendedRefusal(ctx, a.db)
		}
		return a.secondFactor(c, id, stored, signInNext(f.Get("next")))
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
		id = randomToken()
		if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,'buyer')", id, handle, string(hash)); err != nil {
			return actionResult{}, fail(400, "Handle unavailable")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Account created: buyer"); err != nil {
			return actionResult{}, fail(500, "Audit failed")
		}
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, fail(500, "Unable to save account")
	}
	if err = a.login(ctx, w, id, c.Token, string(hash)); err != nil {
		return actionResult{}, fail(500, "Account created; sign in to continue")
	}
	if path == "/setup" {
		return actionResult{Redirect: "/admin?welcome=1"}, nil
	}
	return actionResult{Redirect: orHome(signInNext(f.Get("next")))}, nil
}

// errPasswordChanged refuses a sign-in whose password was checked before the account's password changed (A-157).
var errPasswordChanged = fail(401, "Sign-in verification expired. Sign in again.")

// errSetupComplete refuses first-run setup once the market is installed.
var errSetupComplete = fail(403, "Setup already complete")

// setupSiteName applies first-run setup's marketplace-name rule: surrounding space trimmed, "OPSMKT" when
// blank, at most 80 bytes.
func setupSiteName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "OPSMKT"
	}
	if len(name) > 80 {
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
	id := randomToken()
	if _, err := tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,'admin')", id, handle, hash); err != nil {
		return "", fail(400, "Handle unavailable")
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
// lands; a pending login keeps it for /challenge (A-126).
func (a *App) secondFactor(c *actionCtx, userID, passwordHash, next string) (actionResult, error) {
	ctx := c.Ctx()
	names, err := a.enrolledFactors(ctx, userID)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	if len(names) == 0 {
		var he *httpError
		if err = a.login(ctx, c.W, userID, c.Token, passwordHash); errors.As(err, &he) { // suspended or password changed
			return actionResult{}, err
		} else if err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		return actionResult{Redirect: orHome(next)}, nil
	}
	pending := randomToken()
	if _, err = a.db.ExecContext(ctx, "INSERT INTO pending_logins(token_hash,user_id,expires,password_digest,next) VALUES($1,$2,now()+interval '10 minutes',$3,$4)", digest(pending), userID, digest(passwordHash), next); err != nil {
		return actionResult{}, fail(500, "Unable to sign in")
	}
	a.cookie(c.W, "pending", pending, 600)
	return actionResult{Redirect: "/challenge"}, nil
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
	if !a.allow("challenge:"+digest(pending), 10) {
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
	token, err := a.startSession(ctx, c.Tx, userID, c.Token, "password and "+method, checked.String)
	if err != nil {
		return actionResult{}, err
	}
	// The sign-in lands on the page /login was sent from (A-126), kept by the pending sign-in and checked again; a
	// "next" posted here is ignored. A recovery-code sign-in lands on /totp, which says how many codes remain or that
	// TOTP is now off (A-154), and links on to that page.
	target := signInNext(stored)
	next := orHome(target)
	if method == "recovery code" {
		next = "/totp?signin=recovery"
		if turnOffTOTPAtSignIn(c.Form) {
			next = "/totp?signin=totp-off"
		}
		if target != "" {
			next += "&next=" + url.QueryEscape(target)
		}
	}
	return actionResult{Redirect: next, AfterCommit: func(w http.ResponseWriter) {
		a.cookie(w, "pending", "", -1)
		a.cookie(w, "session", token, 43200)
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
	if _, uerr = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, "Correct password but failed second sign-in step ("+method+")"); uerr != nil {
		return uerr
	}
	if uerr = notifyOwner(ctx, tx, userID, "Someone entered your password correctly but failed the second sign-in step ("+method+")."); uerr != nil {
		return uerr
	}
	if uerr = tx.Commit(); uerr != nil {
		return uerr
	}
	return err
}

func challengeLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	d.Title = "Verify sign-in"
	pending := pendingCookie(r)
	if pending == "" {
		return nil
	}
	var userID string
	err := a.db.QueryRowContext(ctx, "SELECT user_id FROM pending_logins WHERE token_hash=$1 AND expires>now()", digest(pending)).Scan(&userID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
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
// It refuses a suspended account (suspendedRefusal), and with errPasswordChanged a sign-in whose password was
// checked against another hash than the account's current one (passwordDigest, the SHA-256 of that hash): a
// password change ends every sign-in in progress, including one whose password was checked just before the change
// committed (A-157). The account row is share-locked, so a suspension or password change committing concurrently
// either is seen here or runs after this commit and deletes the new session.
func (a *App) startSession(ctx context.Context, tx *sql.Tx, id, old, method, passwordDigest string) (string, error) {
	var suspended bool
	var hash string
	if err := tx.QueryRowContext(ctx, "SELECT suspended_at IS NOT NULL,password_hash FROM users WHERE id=$1 FOR SHARE", id).Scan(&suspended, &hash); err != nil {
		return "", err
	}
	if suspended {
		return "", suspendedRefusal(ctx, tx)
	}
	if subtle.ConstantTimeCompare([]byte(digest(hash)), []byte(passwordDigest)) != 1 {
		return "", errPasswordChanged
	}
	token := randomToken()
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1 OR expires<now()", digest(old)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(token), id); err != nil {
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
