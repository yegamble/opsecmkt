package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	texttemplate "text/template"

	"github.com/jackc/pgx/v5/pgconn"
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
	// Precheck, when set, runs in ServeHTTP after the CSRF check and before the session lookup and the post
	// slot: a check of the form alone (no database), so a flood of posts it refuses holds no slot (A-178).
	// Run must still make the same checks; it is also called without Precheck (tests call post directly).
	Precheck func(a *App, r *http.Request, token string) error
	// OneAtATime admits at most oneAtATimeLimit requests of this action per signed-in account at once; another
	// is refused at once with 429 and Retry-After: 5, before it takes a post slot or a database connection. For
	// an action that may wait on a slow wallet while holding the account's row (payAction), so one account's
	// burst cannot hold the application pool's connections queued on its own row lock (PR #15 re-review).
	OneAtATime bool
	Run        func(c *actionCtx) (actionResult, error)
}

// oneAtATimeLimit is the per-account limit of OneAtATime actions (tests of the database's own guards lift it).
var oneAtATimeLimit = 1

// enterOneAtATime counts a request of path by userID in, unless oneAtATimeLimit are already in progress; done
// counts it out.
func (a *App) enterOneAtATime(path, userID string) (done func(), ok bool) {
	key := path + "\x00" + userID
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight[key] >= oneAtATimeLimit {
		return nil, false
	}
	if a.inFlight == nil {
		a.inFlight = map[string]int{}
	}
	a.inFlight[key]++
	return sync.OnceFunc(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.inFlight[key]--; a.inFlight[key] <= 0 {
			delete(a.inFlight, key)
		}
	}), true
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
// A 5xx code is also logged (serverError).
func fail(code int, msg string) error { return &httpError{Code: code, Msg: msg} }

// serverError answers with a 5xx status and msg followed by a reference, and logs one line with the same
// reference so the operator can find what a user reports (A-141):
//
//	http 5xx kind=page|action name=<registered route key> status=<code> ref=<8 hex> cause=<class>
//
// cause is a class from errorCause, never the error's text, and the line names nothing about the request or
// its sender: no URL path or query, form value, cookie, header, address, handle or user id. At most 10 such
// lines are written a second; the rest are counted in one "http 5xx suppressed=N" line (A-178). A 503 carries
// Retry-After.
func serverError(w http.ResponseWriter, r *http.Request, code int, msg, cause string) {
	ref := randomToken()[:8]
	kind, name := routeOf(r)
	http5xxLog.Printf("http 5xx kind=%s name=%s status=%d ref=%s cause=%s", kind, name, code, ref, cause)
	if code == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", busyRetryAfter)
	}
	http.Error(w, msg+"\nReference: "+ref, code)
}

// writeHTTPError answers err from a check made before the action runs (actionSpec.Precheck).
func writeHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	if errors.As(err, &he) && he.Code < 500 {
		http.Error(w, he.Msg, he.Code)
		return
	}
	serverError(w, r, 503, "Service unavailable", errorCause(err))
}

// dbBusy reports a statement cancelled by the application pool's statement_timeout or lock_timeout (A-177),
// or a database that is unavailable (QA-178-2): shutting down or refusing connections (57P01-57P03, 53300),
// or a lost or failed connection (08000, 08001, 08003, 08004, 08006 or dbConnLost; the other class 08 codes,
// such as 08P01 protocol_violation, point at configuration, which retrying does not fix). The database is
// overloaded, restarting or a row is held too long, so the request is answered 503 (retry), not 500. A client
// that went away or a request past its deadline is not.
func dbBusy(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "57014", "55P03", "57P01", "57P02", "57P03", "53300", "08000", "08001", "08003", "08004", "08006":
			return true
		}
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && dbConnLost(err)
}

// dbConnLost reports a failed or lost database connection: errorCause's db-connection class.
func dbConnLost(err error) bool {
	var ce *pgconn.ConnectError
	var ne net.Error
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.As(err, &ce) || errors.As(err, &ne) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// errBusyMsg answers a request refused because of dbBusy.
const errBusyMsg = "The market is busy. Try again in a few seconds."

// routeOf names r's registered route: an action path, a page name or a raw handler path, else "unregistered".
func routeOf(r *http.Request) (kind, name string) {
	p := r.URL.Path
	if p == "/healthz" {
		return "page", p
	}
	if r.Method == "POST" {
		if _, ok := uploadRoutes[p]; ok {
			return "action", p
		}
		if _, ok := actions[p]; ok {
			return "action", p
		}
		return "action", "unregistered"
	}
	if _, ok := raws[p]; ok {
		return "page", p
	}
	if route := imageRoute(p); route != "" {
		return "page", route // never the image id
	}
	page := strings.TrimPrefix(p, "/")
	if page == "" {
		page = "catalog"
	}
	if _, ok := pages[page]; ok {
		return "page", page
	}
	return "page", "unregistered"
}

// errorCause classifies err for the log without its text, which can quote user input or configuration:
// the SQLSTATE (with the table and constraint when PostgreSQL names them), timeout, canceled, db-connection,
// template:<name>, or else the type of the innermost wrapped error.
func errorCause(err error) string {
	var pe *pgconn.PgError
	var ne net.Error
	var ee texttemplate.ExecError
	var te *template.Error
	switch {
	case err == nil:
		return "none"
	case errors.As(err, &pe):
		return strings.Join(slices.DeleteFunc([]string{pe.Code, pe.TableName, pe.ConstraintName}, func(s string) bool { return s == "" }), " ")
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case dbConnLost(err):
		return "db-connection"
	case errors.As(err, &ee):
		return "template:" + ee.Name
	case errors.As(err, &te):
		return "template:" + te.Name
	}
	for u := errors.Unwrap(err); u != nil; u = errors.Unwrap(err) {
		err = u
	}
	return fmt.Sprintf("%T", err)
}

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

// rowQuerier is a *sql.Tx or *sql.DB. Code that runs while a transaction is open reads through that
// transaction: a second pool connection there can wait on the pool while holding one (A-150).
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type factorProvider interface {
	Name() string // "totp", "pgp"
	// Enrolled reports whether userID has this factor on. /challenge passes its transaction (c.Tx).
	Enrolled(ctx context.Context, q rowQuerier, userID string) (bool, error)
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

// arrivalKey holds the request's context as the server created it, without ServeHTTP's 12 s bound.
type arrivalKey struct{}

// arrivalContext returns the request's context without ServeHTTP's 12 s bound: it ends only when the client
// goes away (or the connection's read deadline passes). A handler that sets its own bound (the signed audit
// export, A-139) derives it from here. Without ServeHTTP (tests) it is r's own context.
func arrivalContext(r *http.Request) context.Context {
	if c, ok := r.Context().Value(arrivalKey{}).(context.Context); ok {
		return c
	}
	return r.Context()
}

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
	// Only a post that is charged to its account's write budget takes a signed-in slot: a public action (sign-in,
	// registration, the sign-in challenge) is not, so a signed-in account posting one uses the anonymous class and
	// cannot flood the class that staff changes use (PR #15 re-review).
	class := anonPostWork
	if !spec.Public || len(spec.Roles) > 0 {
		class = userPostWork
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
	if spec.OneAtATime && u != nil {
		done, ok := a.enterOneAtATime(r.URL.Path, u.ID)
		if !ok {
			w.Header().Set("Retry-After", busyRetryAfter)
			http.Error(w, "Your previous request is still being processed. Wait a few seconds, then reload the page to see its result.", http.StatusTooManyRequests)
			return
		}
		defer done()
	}
	// The post's admission slot (A-178), taken only now: its body has been read and its CSRF, sign-in, role
	// and write-budget checks have passed.
	release, ok := admitPost(w, class)
	if !ok {
		return
	}
	defer release()
	ctx := r.Context()
	c := &actionCtx{A: a, W: w, R: r, User: u, Token: token, Form: r.PostForm}
	if !spec.OwnTx {
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			serverError(w, r, 503, "Service unavailable", errorCause(err))
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
		if he.Code >= 500 {
			serverError(w, r, he.Code, he.Msg, errorCause(he))
			return
		}
		http.Error(w, he.Msg, he.Code)
		return
	}
	if err != nil {
		if dbBusy(err) {
			serverError(w, r, 503, errBusyMsg, errorCause(err))
			return
		}
		serverError(w, r, 500, "Unable to save changes", errorCause(err))
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
			// Inside the action's transaction nothing is committed yet, so a busy or lost database answers
			// the retryable 503 like the action itself. An OwnTx action has already committed its change
			// (the row is written in autocommit), so a retry could repeat it: that stays 500.
			if c.Tx != nil && dbBusy(err) {
				serverError(w, r, 503, errBusyMsg, errorCause(err))
				return
			}
			serverError(w, r, 500, "Unable to record change", errorCause(err))
			return
		}
	}
	if c.Tx != nil {
		if err = c.Tx.Commit(); err != nil {
			serverError(w, r, 500, "Unable to save changes", errorCause(err))
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
