package market

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"regexp"
	"strings"

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

var handlePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

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

// confirmedTx runs a sensitive change (an OwnTx action) in its own transaction. With confirm set, the current
// password is checked first (bcrypt outside any transaction) and, when TOTP is enrolled, a current
// authenticator code ("code") inside the transaction. run uses c.Tx; its Audit is written in the same
// transaction and notes how the change was confirmed.
func confirmedTx(c *actionCtx, confirm bool, run func(c *actionCtx) (actionResult, error)) (actionResult, error) {
	a, ctx, uid := c.A, c.Ctx(), c.User.ID
	if confirm {
		if err := a.confirmPassword(ctx, uid, c.Form.Get("password")); err != nil {
			return actionResult{}, err
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
		if err = tx.QueryRowContext(ctx, "SELECT totp_enabled FROM users WHERE id=$1", uid).Scan(&totp); err != nil {
			return actionResult{}, err
		}
		if totp {
			if err = a.checkTOTP(ctx, tx, uid, c.Form.Get("code")); err != nil {
				return actionResult{}, err
			}
			how = "password and authenticator code"
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

// authGate admits one password-bearing request: bounded bcrypt concurrency, per-handle rate limit and
// input bounds, all before any expensive work. On success the caller must defer release().
func authGate(c *actionCtx) (handle, password string, release func(), err error) {
	select {
	case passwordWork <- struct{}{}:
	default:
		return "", "", nil, fail(503, "Authentication is busy. Try again shortly.")
	}
	release = func() { <-passwordWork }
	handle, password = strings.TrimSpace(c.Form.Get("handle")), c.Form.Get("password")
	// Validate before the rate limit so malformed handles never create limiter entries.
	if !handlePattern.MatchString(handle) || len(password) < 12 || len(password) > 72 {
		err = fail(400, "Use a 3–32 character handle (letters, digits, underscores) and a password of 12–72 bytes.")
	} else if !c.A.allow("auth:"+strings.ToLower(handle), 10) {
		err = fail(429, "Too many attempts. Try again in ten minutes.")
	} else if c.R.URL.Path != "/setup" {
		err = c.A.checkCaptcha(c) // P1: single-use image CAPTCHA unless an administrator turned it off
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
		var id, hash string
		err := a.db.QueryRowContext(ctx, "SELECT id,password_hash FROM users WHERE handle=$1", handle).Scan(&id, &hash)
		if err != nil {
			hash = "$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW"
		}
		check := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
		if err != nil || check != nil {
			return actionResult{}, fail(401, "Invalid handle or password")
		}
		return a.secondFactor(c, id)
	}
	if path == "/setup" && (installed || subtle.ConstantTimeCompare([]byte(f.Get("token")), []byte(a.setupToken)) != 1) {
		return actionResult{}, fail(403, "Setup unavailable or token incorrect")
	}
	if path == "/register" && !installed {
		return actionResult{}, fail(403, "Complete installation first")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
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
		if err = a.login(ctx, c.W, userID, c.Token); err != nil {
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
	if ok, err := p.Enrolled(ctx, a.db, userID); err != nil || !ok {
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
func (a *App) startSession(ctx context.Context, tx *sql.Tx, id, old string) (string, error) {
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
