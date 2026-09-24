package market

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"image/png"
	"net/http"
	"regexp"
)

func init() {
	registerRaw("/captcha", captchaPNG)
	registerLoader("login", captchaLoader)
	registerLoader("register", captchaLoader)
	registerAction("/admin/captcha", actionSpec{Roles: []string{"admin"}, Run: captchaToggle})
	for _, page := range []string{"login", "register"} {
		registerPreview(page, func(d *PageData) { d.Captcha = &CaptchaView{ID: "preview"} })
	}
}

var captchaID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// captchaRequired is true unless an administrator turned the CAPTCHA off (fail closed on a missing row).
func captchaRequired(settings map[string]string) bool { return settings["captcha_required"] != "false" }

// captchaLoader issues a fresh single-use challenge bound to this browser session for every login/register view.
func captchaLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if !captchaRequired(d.Settings) {
		return nil
	}
	d.Captcha = &CaptchaView{}
	session := digest(sessionToken(r))
	// Counted in PostgreSQL rather than the in-memory limiter: anonymous GETs must not be able to fill
	// the limiter's shared key table (which would lock out sign-in for everyone).
	if _, err := a.db.ExecContext(ctx, "DELETE FROM captchas WHERE expires<now()"); err != nil {
		return err
	}
	var recent int
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM captchas WHERE session_hash=$1 AND created>now()-interval '10 minutes'", session).Scan(&recent); err != nil {
		return err
	}
	if recent >= 30 {
		d.Captcha.Error = "Too many CAPTCHA images were requested from this browser session. Wait ten minutes, then reload this page."
		return nil
	}
	id := randomToken()
	if _, err := a.db.ExecContext(ctx, "INSERT INTO captchas(id,answer_hash,session_hash,expires) VALUES($1,$2,$3,now()+interval '10 minutes')", id, captchaAnswerHash(id, a.captchaAnswer(id)), session); err != nil {
		return err
	}
	d.Captcha.ID = id
	return nil
}

// captchaPNG serves GET /captcha?id=<id>: only an unused, unexpired challenge issued to this session.
func captchaPNG(a *App, w http.ResponseWriter, r *http.Request, _ *User) {
	id := r.URL.Query().Get("id")
	if !(captchaID.MatchString(id) || a.preview && id == "preview") {
		http.NotFound(w, r)
		return
	}
	if !a.preview {
		var ok bool
		// Either cookie: a page rendered while `session` was withheld (cross-site landing) bound its
		// challenge to `anon`, and its image request then sends both cookies.
		anon := anonToken(r)
		if anon == "" {
			anon = sessionToken(r)
		}
		err := a.db.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM captchas WHERE id=$1 AND session_hash IN ($2,$3) AND NOT used AND expires>now())", id, digest(sessionToken(r)), digest(anon)).Scan(&ok)
		if err != nil {
			http.Error(w, "Service unavailable", 503)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, a.captchaImage(id)); err != nil {
		http.Error(w, "Unable to render image", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

// checkCaptcha consumes the submitted challenge (single use, even when the answer is wrong) and
// compares the answer. It is a no-op while the CAPTCHA is turned off.
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
	var hash string
	err = a.db.QueryRowContext(ctx, "UPDATE captchas SET used=true WHERE id=$1 AND session_hash=$2 AND NOT used AND expires>now() RETURNING answer_hash", id, digest(c.Token)).Scan(&hash)
	if err == sql.ErrNoRows {
		return fail(400, "The CAPTCHA expired or was already used. Go back, reload the page and enter the new image's characters.")
	}
	if err != nil {
		return fail(503, "Service unavailable")
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(captchaAnswerHash(id, c.Form.Get("captcha")))) != 1 {
		return fail(400, "CAPTCHA answer incorrect. Go back, reload the page and enter the new image's characters.")
	}
	return nil
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
	return actionResult{Redirect: "/admin?saved=1", Audit: audit}, err
}
