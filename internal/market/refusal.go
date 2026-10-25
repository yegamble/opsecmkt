package market

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// RefusalView is the shared refusal page's content besides its reason, which is PageData.Error (A-514).
type RefusalView struct {
	Back, BackLabel string      // the page the refused form or address was reached from; "/" at worst
	SignIn          bool        // a 401 or an expired form to a browser not signed in: a sign-in that returns to Back
	Echo            []EchoField // a refused post's long text fields, shown read-only so they can be copied
}

// EchoField is one long text field of a refused post, as it was sent (bounded by its form's limit).
type EchoField struct{ Label, Text string }

// refusalEchoFields is the allowlist of fields a refused post shows back, in display order, each bounded in characters
// by its form's own limit. Only free text the sender typed is listed: never a password, an authenticator, recovery,
// PGP sign-in or CAPTCHA code, a setup token, a CSRF token or a form nonce (none of those names is here).
var refusalEchoFields = []struct {
	name, label string
	max         int
}{
	{"title", "Listing title", 140},
	{"description", "Description", 10000},
	{"delivery_content", "Automatic delivery content", maxDeliveryContent},
	{"content", "Delivery content", 32000},
	{"body", "Message or review", 65536},
	{"reply", "Dispute reply", 5000},
	{"reason", "Reason", 5000},
	{"resolution", "Decision", 5000},
	{"note", "Note", 1000},
	{"psbt", "Signed PSBT", hardwareMaxBase64},
	{"pgp", "PGP public key", 16384},
	{"operator_key", "Operator key", maxCanaryBytes},
	{"canary", "Canary statement", maxCanaryBytes},
	{"address", "Address", 110},
	{"refund_address", "Refund address", 110},
}

// refusalEcho returns form's allowlisted long text fields (refusalEchoFields) that hold more than white space. The text
// is made valid UTF-8 without NUL characters and cut to the field's limit; html/template escapes it where it is shown.
// It is neither stored nor logged.
func refusalEcho(form url.Values) []EchoField {
	var echo []EchoField
	for _, f := range refusalEchoFields {
		v := form.Get(f.name)
		if strings.TrimSpace(v) == "" {
			continue
		}
		echo = append(echo, EchoField{Label: f.label, Text: typedText(v, f.max)})
	}
	return echo
}

// mayEcho reports whether a refusal may show r's posted text: only a post whose Origin and Sec-Fetch-Site checks
// passed, so a cross-site form cannot make this market display text of its choosing.
func (a *App) mayEcho(r *http.Request) bool {
	return r.Method == http.MethodPost && r.PostForm != nil && postOriginError(r, a.secure) == ""
}

// typedHandle is the handle a refused /login or /register post sent, at most 32 characters, to type into the form
// again (never the password); "" when the post may not be echoed (mayEcho).
func (a *App) typedHandle(r *http.Request) string {
	if !a.mayEcho(r) {
		return ""
	}
	h := strings.TrimSpace(r.PostForm.Get("handle"))
	if utf8.RuneCountInString(h) > 32 {
		h = string([]rune(h)[:32])
	}
	return h
}

// refusalBack is the page to link back to from a refusal: the registered page a registered action's path names
// (/messages, /disputes/reply → /disputes), else, or when it is the same page with an id or recipient, the same-origin
// Referer when it is a registered page (rebuilt by signInNext, never copied), else /.
func (a *App) refusalBack(r *http.Request) string {
	page := ""
	// Only an action that exists and is on names a page, so an action switched off answers exactly as an unknown
	// action does (actionSpec.Off).
	if spec, ok := actions[r.URL.Path]; ok && r.Method == http.MethodPost && (spec.Off == nil || !spec.Off(a)) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		first, _, _ := strings.Cut(path, "/")
		for _, name := range []string{path, first} {
			if name != "" && a.pageOn(name) {
				page = name
				break
			}
		}
	}
	ref := ""
	if u, err := url.Parse(r.Referer()); err == nil && u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil {
		ref = a.signInNext(u.RequestURI())
	}
	refPage, _, _ := strings.Cut(strings.TrimPrefix(ref, "/"), "?")
	switch {
	case refPage == "ticket" && strings.HasPrefix(r.URL.Path, "/tickets/"):
		return ref
	// An admin action's form is on an administration section (A-542): /admin/suspend refused from /admin/users
	// links back to that section.
	case ref != "" && (page == "" || refPage == page || page == "admin" && strings.HasPrefix(refPage, "admin/")):
		return ref
	case page != "" && page != "catalog":
		return "/" + page
	}
	return "/"
}

// refusalTitle is the refusal page's title and heading.
func refusalTitle(code int, post bool) string {
	switch {
	case code == http.StatusUnauthorized:
		return "Sign in required"
	case code == http.StatusTooManyRequests:
		return "Too many requests"
	case code == http.StatusServiceUnavailable:
		return "Busy"
	case post:
		return "Form not accepted"
	case code == http.StatusNotFound:
		return "Page not found"
	case code == http.StatusForbidden:
		return "Not allowed"
	}
	return "Request not accepted"
}

// refuse answers a refused request with status code and msg on the shared refusal page
// (web/templates/partials/refusal.html, A-514): the layout, msg in the error alert, a link back (refusalBack), a sign-in
// link on a 401 or an expired form to a browser that is not signed in, and a refused post's long text fields read-only
// (refusalEcho). A refused /login or /register post gets its form again instead, with the typed handle kept and a fresh
// CAPTCHA, never the password. user is the signed-in account when the caller knows it; otherwise the session cookie is
// looked up for the header (loadRefusalPage). Nothing the request sent is written or logged. The response keeps
// ServeHTTP's headers (Cache-Control: no-store, the CSP); a 503 caller sets its own Retry-After.
func (a *App) refuse(w http.ResponseWriter, r *http.Request, user *User, code int, msg string) {
	post := r.Method == http.MethodPost
	if post && (r.URL.Path == "/login" || r.URL.Path == "/register") {
		d := a.pageData(r, strings.TrimPrefix(r.URL.Path, "/"), sessionToken(r), user)
		if a.loadRefusalPage(r, &d) {
			d.Error, d.FormHandle, d.FormVendor = msg, a.typedHandle(r), r.PostForm.Get("account_type") == "vendor"
			a.renderRefusal(w, r, d, code, msg)
			return
		}
	}
	d := a.pageData(r, "refusal", sessionToken(r), user)
	d.Title, d.Preview = refusalTitle(code, post), a.preview
	d.Query, d.Category, d.Region = "", "", "" // the header's search box never carries a refused query
	v := &RefusalView{Back: a.refusalBack(r)}
	v.BackLabel = "Back to the market"
	if v.Back != "/" {
		v.BackLabel = "Back to " + nextTitle(v.Back)
	}
	if a.mayEcho(r) {
		v.Echo = refusalEcho(r.PostForm)
	}
	d.Refusal = v
	a.loadRefusalPage(r, &d)
	d.Error = msg
	// A browser that is not signed in is offered a sign-in on a 401, and on an expired form, which is how a post
	// arrives once the session cookie has gone (after 12 hours or a sign-out elsewhere); not on the sign-in pages.
	first, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	v.SignIn = d.User == nil && (code == http.StatusUnauthorized || msg == formExpired) &&
		first != "login" && first != "register" && first != "setup" && first != "challenge"
	if d.User == nil {
		d.Next = a.signInNext(v.Back) // the header's sign-in link returns here too
	}
	a.renderRefusal(w, r, d, code, msg)
}

// renderRefusal writes d with status code. Should the page fail to render, the refusal is still answered with its
// own status, in plain text: a refusal never becomes a 500.
func (a *App) renderRefusal(w http.ResponseWriter, r *http.Request, d PageData, code int, msg string) {
	var b bytes.Buffer
	if a.templates == nil || a.templates.ExecuteTemplate(&b, "page:"+d.Page, d) != nil {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(b.Bytes())
}
