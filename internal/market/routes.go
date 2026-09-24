package market

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Registries are filled from init() in feature files and read-only afterwards.
// Within one registry, registration order is file-name order; do not rely on order across packages.

type pageSpec struct {
	Protected bool
	Roles     []string // empty = anyone who passes Protected; list "admin" explicitly when admins qualify
}

var pages = map[string]pageSpec{}

// registerPage makes GET /<name> render template "page:<name>" (web/templates/pages/<name>.html).
func registerPage(name string, s pageSpec) {
	if _, dup := pages[name]; dup {
		panic("duplicate page " + name)
	}
	pages[name] = s
}

func protected(page string) bool { return pages[page].Protected }
func authorized(page string, u *User) bool {
	s := pages[page]
	return len(s.Roles) == 0 || u != nil && slices.Contains(s.Roles, u.Role)
}

type loaderFunc func(ctx context.Context, a *App, r *http.Request, d *PageData) error

var loaders = map[string][]loaderFunc{}

// registerLoader runs f after load.go's generic loading; page "*" runs for every page (before page loaders).
// Return sql.ErrNoRows for 404 or fail(code, msg) for any other status.
func registerLoader(page string, f loaderFunc) { loaders[page] = append(loaders[page], f) }

func runLoaders(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	for _, f := range append(slices.Clone(loaders["*"]), loaders[d.Page]...) {
		if err := f(ctx, a, r, d); err != nil {
			return err
		}
	}
	return nil
}

type actionCtx struct {
	A     *App
	W     http.ResponseWriter
	R     *http.Request
	Tx    *sql.Tx // nil when the action uses OwnTx
	User  *User   // nil for anonymous Public actions
	Token string  // raw request token: session, or pre-login anon when anonymous (also sessionToken(R))
	Form  url.Values
}

func (c *actionCtx) Ctx() context.Context { return c.R.Context() }

type actionResult struct {
	Redirect    string // default "/"
	Audit       string // recorded in audit_events in the same transaction when non-empty
	AuditUserID string // default c.User.ID
	AfterCommit func(w http.ResponseWriter)
}

type actionSpec struct {
	Public bool     // allow anonymous; skips the write:<user> rate limit
	Roles  []string // required user roles (implies signed in)
	OwnTx  bool     // Run opens its own transactions (e.g. bcrypt before any tx); c.Tx is nil, Audit is written without a tx
	Run    func(c *actionCtx) (actionResult, error)
}

var actions = map[string]actionSpec{}

// registerAction handles POST <path> (CSRF and origin already verified by ServeHTTP).
func registerAction(path string, s actionSpec) {
	if _, dup := actions[path]; dup {
		panic("duplicate action " + path)
	}
	actions[path] = s
}

type httpError struct {
	Code int
	Msg  string
	// Render, when set on an action's error, writes the response (status Code) instead of the plain-text Msg,
	// after the action's transaction is rolled back.
	Render func(w http.ResponseWriter)
}

func (e *httpError) Error() string { return e.Msg }

// fail returns an error that post() and page loaders send as an HTTP status with a plain-text message.
func fail(code int, msg string) error { return &httpError{Code: code, Msg: msg} }

type rawHandler func(a *App, w http.ResponseWriter, r *http.Request, u *User)

var raws = map[string]rawHandler{}

// registerRaw serves GET/HEAD <path> without the HTML layout (PNG, downloads). It runs before the
// installed/page checks and also in preview mode (a.db is nil there); handlers enforce their own access.
func registerRaw(path string, h rawHandler) {
	if _, dup := raws[path]; dup {
		panic("duplicate raw handler " + path)
	}
	raws[path] = h
}

var previews = map[string][]func(d *PageData){}

// registerPreview adds sample data for the read-only -preview server; page "*" applies to every page.
func registerPreview(page string, f func(d *PageData)) { previews[page] = append(previews[page], f) }

func applyPreviews(d *PageData) {
	for _, f := range append(slices.Clone(previews["*"]), previews[d.Page]...) {
		f(d)
	}
}

type transitionHook func(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error

var transitionHooks []transitionHook

// registerTransitionHook runs f inside the transition's transaction after the state change is written.
// Returning an error aborts the whole action. Hooks may call a.transition again for the same order.
func registerTransitionHook(f transitionHook) { transitionHooks = append(transitionHooks, f) }

type factorProvider interface {
	Name() string // "totp", "pgp"
	Enrolled(ctx context.Context, db *sql.DB, userID string) (bool, error)
	Verify(c *actionCtx, userID string) error // return fail(401, ...) on a wrong answer; c.Tx is the /challenge tx
}

var factors []factorProvider

func registerFactor(p factorProvider) {
	if factorByName(p.Name()) != nil {
		panic("duplicate factor " + p.Name())
	}
	factors = append(factors, p)
}

func factorByName(name string) factorProvider {
	for _, p := range factors {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

type sessionKey struct{}

type anonKey struct{}

// sessionToken returns this request's token: the `session` cookie value, or else the pre-login `anon`
// cookie value (including one issued by this response). CSRF and CAPTCHA are bound to it.
func sessionToken(r *http.Request) string { s, _ := r.Context().Value(sessionKey{}).(string); return s }

// anonToken returns the pre-login `anon` cookie value ("" when the request sent none and needed none).
func anonToken(r *http.Request) string { s, _ := r.Context().Value(anonKey{}).(string); return s }

func roleRequired(roles []string) string {
	for _, r := range roles {
		if r != "admin" {
			return strings.ToUpper(r[:1]) + r[1:] + " access required"
		}
	}
	return "Administrator access required"
}

func (a *App) post(w http.ResponseWriter, r *http.Request, u *User, token string) {
	spec, ok := actions[r.URL.Path]
	if !ok {
		http.Error(w, "Unknown action", 404)
		return
	}
	if !spec.Public || len(spec.Roles) > 0 {
		if u == nil {
			http.Error(w, "Sign in required", 401)
			return
		}
		if !a.allow("write:"+u.ID, 100) {
			http.Error(w, "Too many changes. Try again later.", 429)
			return
		}
		if len(spec.Roles) > 0 && !slices.Contains(spec.Roles, u.Role) {
			http.Error(w, roleRequired(spec.Roles), 403)
			return
		}
	}
	ctx := r.Context()
	c := &actionCtx{A: a, W: w, R: r, User: u, Token: token, Form: r.PostForm}
	if !spec.OwnTx {
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			http.Error(w, "Service unavailable", 503)
			return
		}
		defer tx.Rollback()
		c.Tx = tx
	}
	res, err := spec.Run(c)
	var he *httpError
	if errors.As(err, &he) {
		if he.Render != nil {
			if c.Tx != nil {
				c.Tx.Rollback()
			}
			he.Render(w)
			return
		}
		http.Error(w, he.Msg, he.Code)
		return
	}
	if err != nil {
		http.Error(w, "Unable to save changes", 500)
		return
	}
	if res.Audit != "" {
		var uid any
		if res.AuditUserID != "" {
			uid = res.AuditUserID
		} else if u != nil {
			uid = u.ID
		}
		exec := a.db.ExecContext
		if c.Tx != nil {
			exec = c.Tx.ExecContext
		}
		if _, err = exec(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", uid, res.Audit); err != nil {
			http.Error(w, "Unable to record change", 500)
			return
		}
	}
	if c.Tx != nil {
		if err = c.Tx.Commit(); err != nil {
			http.Error(w, "Unable to save changes", 500)
			return
		}
	}
	if res.AfterCommit != nil {
		res.AfterCommit(w)
	}
	if res.Redirect == "" {
		res.Redirect = "/"
	}
	http.Redirect(w, r, res.Redirect, 303)
}

func init() {
	for name, s := range map[string]pageSpec{
		"catalog": {}, "product": {}, "vendor": {}, "canary": {}, "setup": {}, "login": {}, "register": {},
		"checkout": {Protected: true}, "orders": {Protected: true}, "order": {Protected: true}, "messages": {Protected: true},
		"notifications": {Protected: true}, "disputes": {Protected: true}, "account": {Protected: true},
		"vendor-dashboard": {Protected: true, Roles: []string{"vendor", "admin"}},
		"moderator":        {Protected: true, Roles: []string{"moderator", "admin"}},
		"admin":            {Protected: true, Roles: []string{"admin"}},
	} {
		registerPage(name, s)
	}
}
