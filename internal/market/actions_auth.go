package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

func init() {
	for _, path := range []string{"/setup", "/login", "/register"} {
		registerAction(path, actionSpec{Public: true, OwnTx: true, Run: authAction})
	}
	registerAction("/challenge", actionSpec{Public: true, Run: challengeAction})
	registerPage("challenge", pageSpec{})
	registerLoader("challenge", challengeLoader)
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

// errSuspended refuses sign-in to an account an administrator suspended. It is only returned once the
// password is correct, so it reveals nothing to someone who does not know it.
var errSuspended = fail(403, "This account is suspended. Contact the market staff.")

// confirmPassword checks the signed-in user's current password for a sensitive change (per-user attempt
// limit, bounded bcrypt concurrency). Call it before opening a transaction.
func (a *App) confirmPassword(ctx context.Context, userID, password string) error {
	if password == "" || len(password) > 72 {
		return fail(400, "Enter your current password to confirm this change.")
	}
	if !a.allow("confirm:"+userID, 10) {
		return fail(429, "Too many attempts. Try again in ten minutes.")
	}
	select {
	case passwordWork <- struct{}{}:
	default:
		return fail(503, "Authentication is busy. Try again shortly.")
	}
	var hash string
	err := a.db.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash)
	if err == nil {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	}
	<-passwordWork
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
	if confirm {
		var totp bool
		if err = tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1 FOR NO KEY UPDATE", uid).Scan(&totp); err != nil {
			return actionResult{}, err
		}
		if totp {
			code := c.Form.Get("code")
			if opts.Recovery && normalizeRecoveryCode(code) != "" {
				_, err = a.useRecoveryCode(ctx, tx, uid, code)
				how = "password and recovery code"
			} else {
				err = a.checkTOTP(ctx, tx, uid, code)
				how = "password and authenticator code"
			}
			if err != nil {
				return actionResult{}, err
			}
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

// authGate admits one password-bearing request: bounded bcrypt concurrency, input bounds, CAPTCHA and
// per-handle rate limit, all before any expensive work. On success the caller must defer release().
func authGate(c *actionCtx) (handle, password string, release func(), err error) {
	select {
	case passwordWork <- struct{}{}:
	default:
		return "", "", nil, fail(503, "Authentication is busy. Try again shortly.")
	}
	release = func() { <-passwordWork }
	handle, password = strings.TrimSpace(c.Form.Get("handle")), c.Form.Get("password")
	// Validate the input and the CAPTCHA before the rate limit, so malformed handles and failed CAPTCHAs
	// never create or increment a limiter entry (and cannot spend a real user's sign-in budget).
	if !handlePattern.MatchString(handle) || !validPassword(password) {
		err = fail(400, "Use a 3–32 character handle (letters, digits, underscores) and a password of 12–72 bytes.")
	} else if c.R.URL.Path != "/setup" {
		err = c.A.checkCaptcha(c) // P1: single-use image CAPTCHA unless an administrator turned it off
	}
	if err == nil && !c.A.allow("auth:"+strings.ToLower(handle), 10) {
		err = fail(429, "Too many attempts. Try again in ten minutes.")
	}
	if err != nil {
		release()
		return "", "", nil, err
	}
	return handle, password, release, nil
}

// authAction serves /setup, /login and /register. bcrypt runs before any transaction is opened.
func authAction(c *actionCtx) (actionResult, error) {
	a, ctx, f, path, w := c.A, c.Ctx(), c.Form, c.R.URL.Path, c.W
	handle, password, release, err := authGate(c)
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
			return actionResult{}, fail(401, "Invalid handle or password")
		}
		if suspended {
			return actionResult{}, errSuspended
		}
		return a.secondFactor(c, id)
	}
	if path == "/setup" && (installed || subtle.ConstantTimeCompare([]byte(f.Get("token")), []byte(a.setupToken)) != 1) {
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
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	if path == "/setup" {
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil || installed {
			return actionResult{}, fail(403, "Setup already complete")
		}
	}
	role := "buyer"
	if path == "/setup" {
		role = "admin"
	}
	id := randomToken()
	if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,$4)", id, handle, string(hash), role); err != nil {
		return actionResult{}, fail(400, "Handle unavailable")
	}
	if path == "/setup" {
		name := strings.TrimSpace(f.Get("site_name"))
		if name == "" {
			name = "OPSMKT"
		}
		if len(name) > 80 {
			return actionResult{}, fail(400, "Site name too long")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('installed','true'),('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", name); err != nil {
			return actionResult{}, fail(500, "Setup failed")
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Account created: "+role); err != nil {
		return actionResult{}, fail(500, "Audit failed")
	}
	if err = tx.Commit(); err != nil {
		return actionResult{}, fail(500, "Unable to save account")
	}
	if err = a.login(ctx, w, id, c.Token); err != nil {
		return actionResult{}, fail(500, "Account created; sign in to continue")
	}
	if path == "/setup" {
		return actionResult{Redirect: "/admin?welcome=1"}, nil
	}
	return actionResult{Redirect: "/"}, nil
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

// secondFactor finishes a password login: a session when no factor is enrolled, otherwise a
// 10-minute pending login (cookie "pending") completed by POST /challenge.
func (a *App) secondFactor(c *actionCtx, userID string) (actionResult, error) {
	ctx := c.Ctx()
	names, err := a.enrolledFactors(ctx, userID)
	if err != nil {
		return actionResult{}, fail(503, "Service unavailable")
	}
	if len(names) == 0 {
		if err = a.login(ctx, c.W, userID, c.Token); err == errSuspended {
			return actionResult{}, err
		} else if err != nil {
			return actionResult{}, fail(500, "Unable to sign in")
		}
		return actionResult{Redirect: "/"}, nil
	}
	pending := randomToken()
	if _, err = a.db.ExecContext(ctx, "INSERT INTO pending_logins(token_hash,user_id,expires) VALUES($1,$2,now()+interval '10 minutes')", digest(pending), userID); err != nil {
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

func challengeAction(c *actionCtx) (actionResult, error) {
	a, ctx := c.A, c.Ctx()
	pending := pendingCookie(c.R)
	if pending == "" {
		return actionResult{}, fail(401, "Sign-in verification expired. Sign in again.")
	}
	// The pending login must exist before a limiter entry is created, so random cookies cost nothing.
	var userID string
	err := c.Tx.QueryRowContext(ctx, "SELECT user_id FROM pending_logins WHERE token_hash=$1 AND expires>now() FOR UPDATE", digest(pending)).Scan(&userID)
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
	if err = p.Verify(c, userID); err != nil {
		return actionResult{}, err
	}
	if _, err = c.Tx.ExecContext(ctx, "DELETE FROM pending_logins WHERE token_hash=$1 OR expires<now()", digest(pending)); err != nil {
		return actionResult{}, err
	}
	token, err := a.startSession(ctx, c.Tx, userID, c.Token)
	if err != nil {
		return actionResult{}, err
	}
	return actionResult{Redirect: "/", AfterCommit: func(w http.ResponseWriter) {
		a.cookie(w, "pending", "", -1)
		a.cookie(w, "session", token, 43200)
	}}, nil
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

// startSession rotates the caller's session inside tx and records the sign-in; the caller sets the cookie after commit.
// It refuses a suspended account with errSuspended. The account row is share-locked, so a suspension committing
// concurrently either is seen here or runs after this commit and deletes the new session.
func (a *App) startSession(ctx context.Context, tx *sql.Tx, id, old string) (string, error) {
	var suspended bool
	if err := tx.QueryRowContext(ctx, "SELECT suspended_at IS NOT NULL FROM users WHERE id=$1 FOR SHARE", id).Scan(&suspended); err != nil {
		return "", err
	}
	if suspended {
		return "", errSuspended
	}
	token := randomToken()
	if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1 OR expires<now()", digest(old)); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(token), id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,'Signed in')", id); err != nil {
		return "", err
	}
	return token, nil
}

func (a *App) login(ctx context.Context, w http.ResponseWriter, id, old string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	token, err := a.startSession(ctx, tx, id, old)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.cookie(w, "session", token, 43200)
	return nil
}
