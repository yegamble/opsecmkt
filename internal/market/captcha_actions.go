package market

import (
	"bytes"
	"context"
	"database/sql"
	"image/png"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func init() {
	registerRaw("/captcha", captchaPNG)
	registerLoader("login", captchaLoader)
	registerLoader("register", captchaLoader)
	registerAction("/admin/captcha", actionSpec{Roles: []string{"admin"}, Run: captchaToggle})
	registerBackground(captchaSpentSweeper)
	for _, page := range []string{"login", "register"} {
		registerPreview(page, func(d *PageData) { d.Captcha = &CaptchaView{ID: "preview"} })
	}
}

var captchaID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// captchaRequired is true unless an administrator turned the CAPTCHA off (fail closed on a missing row).
func captchaRequired(settings map[string]string) bool { return settings["captcha_required"] != "false" }

// captchaLoader issues a fresh single-use challenge bound to this browser session for every login/register
// view. Issuing is stateless (captcha.go) and writes nothing, so anonymous page views cannot grow a table or
// hold a pool connection for it (A-176).
func captchaLoader(_ context.Context, a *App, r *http.Request, d *PageData) error {
	if !captchaRequired(d.Settings) {
		return nil
	}
	d.Captcha = &CaptchaView{ID: a.newCaptchaID(digest(sessionToken(r)), time.Now().Add(captchaTTL))}
	if d.Captcha.ID == "" {
		return fail(503, "Service unavailable")
	}
	return nil
}

// captchaWork bounds concurrent CAPTCHA image renders; with every slot taken a request is refused at once
// (503 with Retry-After) instead of queueing CPU work (A-176).
var captchaWork = make(chan struct{}, 8)

// captchaPNG serves GET /captcha?id=<id>: only an unexpired challenge issued to this session. It reads no
// database: an id already used still renders its same image, which gains nothing, since POST refuses it.
func captchaPNG(a *App, w http.ResponseWriter, r *http.Request, _ *User) {
	id := r.URL.Query().Get("id")
	if !(captchaID.MatchString(id) || a.preview && id == "preview") {
		http.NotFound(w, r)
		return
	}
	if !a.preview {
		// Either cookie: a page rendered while `session` was withheld (cross-site landing) bound its
		// challenge to `anon`, and its image request then sends both cookies.
		anon := anonToken(r)
		if anon == "" {
			anon = sessionToken(r)
		}
		if _, ok := a.captchaIDValid(id, time.Now(), digest(sessionToken(r)), digest(anon)); !ok {
			http.NotFound(w, r)
			return
		}
	}
	select {
	case captchaWork <- struct{}{}:
		defer func() { <-captchaWork }()
	default:
		busy(w)
		return
	}
	var b bytes.Buffer
	if err := png.Encode(&b, a.captchaImage(id)); err != nil {
		serverError(w, r, 500, "Unable to render image", errorCause(err))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// errCaptchaExpired refuses a challenge id that expired, was already used or was not issued to this session.
var errCaptchaExpired = fail(400, "The CAPTCHA expired or was already used. Go back, reload the page and enter the new image's characters.")

// checkCaptcha consumes the submitted challenge (single use, even when the answer is wrong) and compares the
// answer. It is a no-op while the CAPTCHA is turned off. An id this server did not issue to this session, or
// one that has expired, is refused before anything is written.
func (a *App) checkCaptcha(c *actionCtx) error {
	ctx := c.Ctx()
	var v string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='captcha_required'").Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return fail(503, "Service unavailable")
	}
	if !captchaRequired(map[string]string{"captcha_required": v}) {
		return nil
	}
	id := c.Form.Get("captcha_id")
	expires, ok := a.captchaIDValid(id, time.Now(), digest(c.Token))
	if !ok {
		return errCaptchaExpired
	}
	// The primary key makes a replayed or concurrent second use insert nothing. Expiry is checked again on the
	// database clock, which the sweep deletes by, so an id whose row was swept can never be inserted afresh.
	res, err := a.db.ExecContext(ctx, "INSERT INTO captcha_spent(id,expires) SELECT $1,$2::timestamptz WHERE $2::timestamptz>now() ON CONFLICT(id) DO NOTHING", id, expires)
	if err != nil {
		return fail(503, "Service unavailable")
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errCaptchaExpired
	}
	if !a.captchaMatches(id, c.Form.Get("captcha")) {
		return captchaRetry(c)
	}
	return nil
}

// captchaRetry answers a wrong CAPTCHA answer with 400 and the login or register form again, the error in its
// alert and a fresh challenge, so a browser without JavaScript needs no reload (QA-3). The used id stays spent.
// When the page cannot be loaded, the answer is plain text asking for a reload.
func captchaRetry(c *actionCtx) error {
	const msg = "CAPTCHA answer incorrect. Enter the characters of the new image below."
	return &httpError{Code: 400, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		d := a.pageData(c.R, strings.TrimPrefix(c.R.URL.Path, "/"), c.Token, c.User)
		if err := a.load(c.R, &d); err != nil {
			http.Error(w, "CAPTCHA answer incorrect. Go back, reload the page and enter the new image's characters.", 400)
			return
		}
		d.Error = msg
		a.renderStatus(w, c.R, d, 400)
	}}
}

// captchaSpentSweepInterval is how often spent challenge ids past their expiry are deleted.
var captchaSpentSweepInterval = time.Minute

// captchaSpentSweeper deletes spent ids once they have expired. checkCaptcha refuses an expired id before
// inserting (on the database clock), so deleting its row cannot reopen it; the one-minute margin absorbs a
// database clock step.
func captchaSpentSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepCaptchaSpent(ctx); err != nil && ctx.Err() == nil {
			log.Printf("captcha spent-id sweep: %s", errorCause(err)) // a class, never the driver's text (A-170)
		}
		t.Reset(captchaSpentSweepInterval)
	}
}

func (a *App) sweepCaptchaSpent(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := a.db.ExecContext(ctx, "DELETE FROM captcha_spent WHERE expires<now()-interval '1 minute'")
	return err
}

func captchaToggle(c *actionCtx) (actionResult, error) {
	v := c.Form.Get("captcha_required")
	if v != "true" && v != "false" {
		return actionResult{}, fail(400, "Choose whether the CAPTCHA is required")
	}
	_, err := c.Tx.ExecContext(c.Ctx(), "INSERT INTO settings(key,value) VALUES('captcha_required',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", v)
	audit := "Turned on CAPTCHA for sign-in and registration"
	if v == "false" {
		audit = "Turned off CAPTCHA for sign-in and registration"
	}
	return actionResult{Redirect: "/admin", Notice: "captcha", Audit: audit}, err
}
