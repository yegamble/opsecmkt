package market

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	registerAction("/admin/captcha", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: captchaToggle})
	registerBackground(expiredChallengeSweeper)
	// The preview shows the default mode with no attack signal: /login has no human check.
	registerPreview("register", func(d *PageData) {
		d.Captcha = &CaptchaView{ID: "preview", Question: "What is seven plus five?"}
	})
}

var captchaID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The values of the captcha_required setting (A-725). The old "true" keeps its meaning, the image on both forms.
const (
	captchaModeOff         = "false"        // no check on sign-in or registration
	captchaModeUnderAttack = "under_attack" // the default: image on sign-in under the attack signal, question on registration
	captchaModeAlways      = "true"         // the operator forces the image on sign-in and registration
)

// captchaModeOf reads the setting's value: anything but the two weaker modes, a missing row included, is the strictest.
func captchaModeOf(value string) string {
	if value == captchaModeOff || value == captchaModeUnderAttack {
		return value
	}
	return captchaModeAlways
}

// captchaKind is what a form asks for.
type captchaKind int

const (
	captchaKindNone captchaKind = iota
	captchaKindImage
	captchaKindText
)

// captchaKindFor decides the check of /register or /login in mode; ctx selects the ingress whose signal counts.
func (a *App) captchaKindFor(ctx context.Context, mode string, register bool) captchaKind {
	switch {
	case mode == captchaModeOff:
		return captchaKindNone
	case mode == captchaModeAlways:
		return captchaKindImage
	case register:
		return captchaKindText
	case a.signInAttackOn(ctx):
		return captchaKindImage
	}
	return captchaKindNone
}

// captchaLoader issues a fresh single-use challenge bound to this browser session for every login/register
// view that asks for one. Issuing is stateless (captcha.go) and writes nothing, so anonymous page views cannot
// grow a table or hold a pool connection for it (A-176).
func captchaLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.Page == "register" && d.Access != nil && !d.Access.RegistrationOpen {
		return nil
	}
	mode := captchaModeOf(d.Settings["captcha_required"])
	kind := a.captchaKindFor(ctx, mode, d.Page == "register")
	if kind == captchaKindNone {
		return nil
	}
	d.Captcha = &CaptchaView{ID: a.newCaptchaID(digest(sessionToken(r)), time.Now().Add(captchaTTL))}
	if d.Captcha.ID == "" {
		return fail(503, "Service unavailable")
	}
	if kind == captchaKindText {
		d.Captcha.Question = a.captchaTextChallenge(d.Captcha.ID).Question
	} else {
		d.Captcha.UnderAttack = mode == captchaModeUnderAttack
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
	sem := workFor(r.Context(), captchaWork, adminCaptchaWork)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
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
var errCaptchaExpired = fail(400, "This CAPTCHA or question expired or was already used. Go back, reload the page and answer the new one.")

// Messages of a refused answer, shown over the form with a fresh challenge (captchaRetry).
const (
	captchaRetryImage = "CAPTCHA answer incorrect. Enter the characters of the new image below."
	captchaRetryText  = "That answer is not right. Answer the new question below."
	captchaRetryNewly = "Because of many failed sign-ins, signing in needs an image for now. Enter its characters below."
)

// checkCaptcha consumes the submitted challenge (single use, even when the answer is wrong) and compares the
// answer. It is a no-op while the form asks for no check (captchaKindFor). An id this server did not issue to this
// session, or one that has expired, is refused before anything is written. In the default mode a post with no id is a
// form shown before the attack signal or a mode change, so it gets the form again with the check to answer.
func (a *App) checkCaptcha(c *actionCtx) error {
	ctx := c.Ctx()
	var v string
	err := a.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='captcha_required'").Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return fail(503, "Service unavailable")
	}
	mode := captchaModeOf(v)
	kind := a.captchaKindFor(ctx, mode, c.R.URL.Path == "/register")
	if kind == captchaKindNone {
		return nil
	}
	retry := captchaRetryImage
	if kind == captchaKindText {
		retry = captchaRetryText
	}
	id := c.Form.Get("captcha_id")
	if id == "" && mode == captchaModeUnderAttack {
		if kind == captchaKindImage {
			retry = captchaRetryNewly
		}
		return captchaRetry(c, retry)
	}
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
	right := a.captchaMatches(id, c.Form.Get("captcha"))
	if kind == captchaKindText {
		right = a.captchaTextChallenge(id).accepts(c.Form.Get("captcha"))
	}
	if !right {
		return captchaRetry(c, retry)
	}
	return nil
}

// captchaRetry answers a wrong or missing answer with 400 and the login or register form again, msg in its
// alert, the typed handle kept (A-514) and a fresh challenge, so a browser without JavaScript needs no reload (QA-3).
// The used id stays spent. When the page cannot be loaded, the shared refusal page asks for a reload.
func captchaRetry(c *actionCtx, msg string) error {
	return &httpError{Code: 400, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		d := a.pageData(c.R, strings.TrimPrefix(c.R.URL.Path, "/"), c.Token, c.User)
		if err := a.load(c.R, &d); err != nil {
			a.refuse(w, c.R, c.User, 400, "That answer was not accepted. Go back, reload the page and answer the new human check.")
			return
		}
		d.Error, d.FormHandle, d.FormVendor = msg, a.typedHandle(c.R), c.R.PostForm.Get("account_type") == "vendor"
		a.renderStatus(w, c.R, d, 400)
	}}
}

// challengeSweepInterval is how often expired challenge records are deleted.
var challengeSweepInterval = time.Minute

// expiredChallengeSweeper deletes spent CAPTCHA ids and PGP challenges once they have expired. checkCaptcha refuses
// an expired id before inserting (on the database clock), and every read of a PGP challenge requires expires>now(),
// so deleting a row cannot reopen it or change the refusal; the one-minute margin absorbs a database clock step.
func expiredChallengeSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepExpiredChallenges(ctx); err != nil && ctx.Err() == nil {
			log.Printf("expired challenge sweep: %s", errorCause(err)) // a class, never the driver's text (A-170)
		}
		t.Reset(challengeSweepInterval)
	}
}

// sweepExpiredChallenges attempts each table on its own time bound, so one that fails or stalls does not stop the other.
func (a *App) sweepExpiredChallenges(ctx context.Context) error {
	sweep := func(query string) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := a.db.ExecContext(ctx, query)
		return err
	}
	return errors.Join(
		sweep("DELETE FROM captcha_spent WHERE expires<now()-interval '1 minute'"),
		sweep("DELETE FROM pgp_challenges WHERE expires<now()-interval '1 minute'"),
	)
}

func captchaToggle(c *actionCtx) (actionResult, error) {
	v := c.Form.Get("captcha_required")
	if v != captchaModeAlways && v != captchaModeOff && v != captchaModeUnderAttack {
		return actionResult{}, fail(400, "Choose only under attack, always the image, or off")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		var previous string
		err := c.Tx.QueryRowContext(c.Ctx(), "SELECT value FROM settings WHERE key='captcha_required' FOR UPDATE").Scan(&previous)
		if err != nil && err != sql.ErrNoRows {
			return actionResult{}, err
		}
		if err == sql.ErrNoRows {
			previous = "true" // a missing setting fails closed
		}
		if _, err = c.Tx.ExecContext(c.Ctx(), "INSERT INTO settings(key,value) VALUES('captcha_required',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", v); err != nil {
			return actionResult{}, err
		}
		audit := "Turned on CAPTCHA for sign-in and registration"
		switch v {
		case captchaModeOff:
			audit = "Turned off CAPTCHA for sign-in and registration"
		case captchaModeUnderAttack:
			audit = "Set CAPTCHA to only under attack: the image on sign-in under the attack signal, a written question on registration"
		}
		audit += "; required changed from " + previous + " to " + v
		return actionResult{Redirect: "/admin/security", Notice: "captcha", Audit: audit}, nil
	})
}
