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
	"syscall"
	texttemplate "text/template"

	"github.com/jackc/pgx/v5/pgconn"
)

// Registries are filled from init() in feature files and read-only afterwards.
// Within one registry, registration order is file-name order; do not rely on order across packages.

type pageSpec struct {
	Protected bool
	Roles     []string // empty = anyone who passes Protected; list "admin" explicitly when admins qualify
	// Off, when set and true, makes the page answer exactly as an unknown page does, before any sign-in check, and
	// never a sign-in return target: a feature switched off by configuration leaves no trace of its route.
	Off func(a *App) bool
}

var pages = map[string]pageSpec{}

// registerPage makes GET /<name> render template "page:<name>" (web/templates/pages/<name>.html).
func registerPage(name string, s pageSpec) {
	if _, dup := pages[name]; dup {
		panic("duplicate page " + name)
	}
	pages[name] = s
}

// pageOn reports whether page is registered and not switched off (pageSpec.Off).
func (a *App) pageOn(page string) bool {
	s, ok := pages[page]
	return ok && (s.Off == nil || !s.Off(a))
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
	// ConfirmedFormNonce is set only for an opted-in OwnTx action whose confirmedTx
	// transaction owns both its mutation and the durable submission receipt.
	ConfirmedFormNonce bool
	// FactorFormNonce explicitly permits the recovery-code lock and consumption
	// order used by TOTP activation/replacement/disable. Other confirmed forms
	// still reject those options.
	FactorFormNonce bool
	// OwnFormNonce records the original confirmation mode for dynamic OwnTx
	// routes such as profile/key changes. It is mutually exclusive with the
	// fixed confirmed and normal transaction flags.
	OwnFormNonce           bool
	ReceiptConfirmRequired bool
	// ReceiptAuthorize rechecks route-specific eligibility before a confirmed receipt
	// can be claimed or replayed. It must be read-only and must not require the
	// original transaction's pre-change state.
	ReceiptAuthorize func(*actionCtx) error
}

func (c *actionCtx) Ctx() context.Context { return c.R.Context() }

func rotationSession(c *actionCtx) string {
	if old, ok := c.R.Context().Value(rotationSessionKey{}).(string); ok {
		return old
	}
	return c.Token // action contexts built directly by tests keep their existing semantics
}

type actionResult struct {
	Redirect    string // default "/"
	Audit       string // recorded in audit_events in the same transaction when non-empty
	AuditUserID string // default c.User.ID
	AfterCommit func(w http.ResponseWriter)
	Notice      string // one-shot notice code (notice.go), set on the 303 of a signed-in post; its section becomes the fragment
}

type actionSpec struct {
	Public             bool                   // allow anonymous; skips the write:<user> rate limit
	Roles              []string               // required user roles (implies signed in)
	OwnTx              bool                   // Run opens its own transactions (e.g. bcrypt before any tx); c.Tx is nil, Audit is written without a tx
	FormNonce          bool                   // this normal-transaction form records a successful submission and replays its original 303
	ConfirmedFormNonce bool                   // this confirmed OwnTx form records its receipt in confirmedTx's transaction
	FactorFormNonce    bool                   // fixed confirmed receipt whose factor-code rows are locked before the actor
	OwnFormNonce       bool                   // dynamic OwnTx receipt remembers whether the original request needed confirmation
	ReceiptAuthorize   func(*actionCtx) error // optional current eligibility check before a confirmed claim/replay
	// Precheck, when set, runs in ServeHTTP after the CSRF check and before the session lookup and the post
	// slot: a check of the form alone (no database), so a flood of posts it refuses holds no slot (A-178).
	// Run must still make the same checks; it is also called without Precheck (tests call post directly).
	Precheck func(a *App, r *http.Request, token string) error
	// OneAtATime admits at most oneAtATimeLimit requests of this action per signed-in account at once; another
	// is refused at once with 429 and Retry-After: 5, before it takes a post slot or a database connection. For
	// an action that may wait on a slow wallet while holding the account's row (payAction), so one account's
	// burst cannot hold the application pool's connections queued on its own row lock (PR #15 re-review).
	OneAtATime bool
	// Off, when set and true, makes the action answer exactly as an unknown action does (pageSpec.Off).
	Off func(a *App) bool
	Run func(c *actionCtx) (actionResult, error)
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
	if s.FormNonce && (s.OwnTx || s.Public) {
		panic("form nonce requires an authenticated shared-transaction action: " + path)
	}
	if s.ConfirmedFormNonce && (!s.OwnTx || s.Public || s.FormNonce || len(s.Roles) == 0) {
		panic("confirmed form nonce requires a role-gated authenticated OwnTx action: " + path)
	}
	if s.OwnFormNonce && (!s.OwnTx || s.Public || s.FormNonce || s.ConfirmedFormNonce || len(s.Roles) == 0) {
		panic("own form nonce requires a role-gated authenticated OwnTx action: " + path)
	}
	if s.FactorFormNonce && (!s.ConfirmedFormNonce || !s.OwnTx) {
		panic("factor form nonce requires a confirmed OwnTx action: " + path)
	}
	if s.ReceiptAuthorize != nil && !s.ConfirmedFormNonce {
		panic("receipt authorization requires a confirmed form nonce: " + path)
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
// lines are written a second; the rest are counted in one "http 5xx suppressed=N" line (A-178).
// Retryable 503 responses carry Retry-After.
func serverErrorReference(r *http.Request, code int, cause string) string {
	ref := randomToken()[:8]
	kind, name := routeOf(r)
	recordServerError(r, code, ref)
	http5xxLog.Printf("http 5xx kind=%s name=%s status=%d ref=%s cause=%s", kind, name, code, ref, cause)
	return ref
}

func serverError(w http.ResponseWriter, r *http.Request, code int, msg, cause string) {
	ref := serverErrorReference(r, code, cause)
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
	writeDBFailure(w, r, err)
}

// writeDBFailure reserves Retry-After for transient database faults. A certificate or configuration failure
// may need operator repair; repeatedly retrying the same request will not make it safe or successful.
func writeDBFailure(w http.ResponseWriter, r *http.Request, err error) {
	writeDBFailureContext(r.Context(), w, r, err)
}

func writeDBFailureContext(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	if dbBusyForContext(ctx, err) {
		serverError(w, r, 503, "Service unavailable", errorCause(err))
		return
	}
	serverError(w, r, 500, "Unable to access database", errorCause(err))
}

// Raw escrow downloads depend on both the database and the wallet. Preserve their existing temporary
// wallet-failure response while refusing to promise retries for a persistent database TLS configuration error.
func writeEscrowDownloadFailure(w http.ResponseWriter, r *http.Request, err error) {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) && !dbBusyForContext(r.Context(), err) {
		writeDBFailure(w, r, err)
		return
	}
	serverError(w, r, 503, "Service unavailable", errorCause(err))
}

// dbBusy reports a statement cancelled by the application pool's statement_timeout or lock_timeout (A-177),
// or a database that is unavailable (QA-178-2): shutting down or refusing connections (57P01-57P03, 53300),
// or a lost or failed transport (08000, 08001, 08003, 08004, 08006 or dbConnLost; the other class 08 codes,
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
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		// A failed handshake or certificate validation is a configuration problem even though pgx wraps it
		// in ConnectError. Retry only identifiable transport failures from an attempted connection.
		return dbConnectTransportFailure(err)
	}
	return !errors.Is(err, context.DeadlineExceeded) && dbConnLost(err)
}

// A configured pgx connect_timeout can surface as context.DeadlineExceeded inside ConnectError while the
// HTTP request is still live. A caller whose own request has expired must not receive a retry promise.
func dbBusyForContext(ctx context.Context, err error) bool {
	return ctx.Err() == nil && dbBusy(err)
}

func dbConnectTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	// A multi-host ConnectError joins every attempt. One permanent TLS/configuration failure must not
	// become retryable merely because another host also refused a TCP connection.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !dbConnectTransportFailure(child) {
				return false
			}
		}
		return true
	}
	if op, ok := err.(*net.OpError); ok {
		return dbConnectTransportFailure(op.Err) // a TLS handshake error can sit inside net.OpError
	}
	if dns, ok := err.(*net.DNSError); ok {
		return dns.IsTimeout || dns.IsTemporary
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return dbConnectTransportFailure(wrapped.Unwrap())
	}
	for _, target := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, syscall.ECONNREFUSED,
		syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ETIMEDOUT,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		if errors.Is(err, target) {
			return true
		}
	}
	var ne net.Error
	return errors.As(err, &ne) && (ne.Timeout() || ne.Temporary())
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
	case errors.Is(err, errPerOrderXpub):
		return "per-order-key-unavailable" // QA5W5A-1: a fixed class, never a Go type name
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

// rotationSessionKey retains the original session cookie even if a matching anonymous CSRF
// token makes this request anonymous. Only successful sign-in may revoke that session.
type rotationSessionKey struct{}

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
	if !ok || spec.Off != nil && spec.Off(a) {
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
		if !a.allowWrite(u.ID) {
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
			msg := "Your previous request is still being processed. Wait a few seconds, then reload the page to see its result."
			if spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce {
				msg = "Your previous request is still being processed. Wait a few seconds, then check its result. If the original form is still open, resubmit it unchanged to retrieve the result."
			}
			http.Error(w, msg, http.StatusTooManyRequests)
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
	c := &actionCtx{A: a, W: w, R: r, User: u, Token: token, Form: r.PostForm,
		ConfirmedFormNonce: spec.ConfirmedFormNonce, FactorFormNonce: spec.FactorFormNonce,
		OwnFormNonce: spec.OwnFormNonce, ReceiptAuthorize: spec.ReceiptAuthorize}
	if !spec.OwnTx {
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			writeDBFailure(w, r, err)
			return
		}
		defer tx.Rollback()
		c.Tx = tx
	}
	// A-222: the session/route-role check above may have run before a staff member was
	// demoted or suspended. Lock and re-read the caller in the action's transaction
	// before any role-gated staff change. OwnTx actions use confirmedTx or their own
	// locked recheck; keeping this first avoids target-row/actor-row lock inversions.
	var res actionResult
	var repeated bool
	var err error
	if c.Tx != nil && u != nil && len(spec.Roles) > 0 && (u.Role == "admin" || u.Role == "moderator") {
		var role string
		var suspended bool
		err = c.Tx.QueryRowContext(ctx, "SELECT role,suspended_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", u.ID).Scan(&role, &suspended)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (role != u.Role || suspended) {
			err = errAccountChanged
		}
	}
	if err == nil && spec.FormNonce {
		res, repeated, err = claimFormSubmission(c)
	}
	if err == nil && !repeated {
		res, err = spec.Run(c)
	}
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
		var unknown commitOutcomeUnknown
		if errors.As(err, &unknown) {
			msg := "Change outcome unknown: it may have been saved. Reload the relevant page and check the recorded state before taking further action."
			switch r.URL.Path {
			case "/admin/escrow-xmr-destroyed":
				msg = "Escrow wallet destruction outcome unknown: the confirmation may have been saved. Check the wallet record and sealed-seed status on the admin page, and verify the wallet files and backups before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/review":
				msg = "Review change outcome unknown: it may have been saved. Check the review on the product or vendor page before starting another form. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/listing":
				msg = "Listing change outcome unknown: it may have been saved. Check the listing's status and activity before starting another form. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/escrow/decision", "/admin/escrow-xmr-rerequest", "/admin/escrow-xmr-retry-build":
				msg = "Escrow administration outcome unknown: it may have been saved. Check the order decision, settlement and activity before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/escrow-fee":
				msg = "Market fee outcome unknown: it may have been saved. Check the fee setting and audit trail on the admin page before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/resolve":
				msg = "Resolution outcome unknown: it may have been saved. Check the dispute, order and payout state before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/disputes/concede":
				msg = "Concession outcome unknown: it may have been saved. Check the dispute, order and payout or escrow settlement before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/suspend", "/admin/reset-factors":
				msg = "Staff action outcome unknown: it may have been saved. Check the account's current state and audit trail before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/captcha", "/admin/staff-contact", "/admin/staff-list", "/admin/operator-key", "/admin/canary":
				msg = "Admin setting outcome unknown: it may have been saved. Reload the admin page and check the setting and audit trail before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/escrow-release-days":
				msg = "Escrow release deadline setting outcome unknown: it may have been saved. Reload the admin page and check the current deadline and audit trail before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/payment-intake":
				msg = "Payment intake setting outcome unknown: it may have been saved. Reload the admin page and check the currency's current intake and audit trail before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/moderator/payment-review", "/moderator/xmr-collision":
				msg = "Staff disposition outcome unknown: it may have been saved. Check the moderation desk and order record before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/pgp/2fa":
				msg = "PGP sign-in setting outcome unknown: it may have been saved. Reload PGP verification and check whether PGP sign-in is on or off before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/delivery-cleanup":
				msg = "Delivery cleanup outcome unknown: it may have been saved. Reload archived listing cleanup and check whether the stored delivery text is present before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/escrow-rebroadcast":
				msg = "Bitcoin escrow rebroadcast outcome unknown: the confirmation may have been saved. Check the settlement on the admin page and verify its transaction against the chain and the watch-only wallet before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/admin/escrow-xmr-rebroadcast":
				msg = "Monero escrow relay outcome unknown: the confirmation may have been saved. Check the settlement on the admin page and verify its transaction with the escrow daemon and wallet before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/escrow/claim", "/escrow/ship", "/escrow/recover", "/escrow/bump", "/escrow-xmr/claim", "/escrow-xmr/ship", "/escrow-xmr/confirm":
				msg = "Escrow action outcome unknown: it may have been saved. Check the order and settlement state before taking further action. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/totp/disable", "/totp/activate", "/totp/recovery":
				msg = "Authenticator change outcome unknown: it may have been saved. Check your authenticator settings and recovery-code state before making another change. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			case "/account", "/admin", "/escrow-key", "/escrow-key/practice":
				msg = "Change outcome unknown: it may have been saved. Check the current account, admin or escrow-key state before making another change. If the original form is still open, resubmit it unchanged to retrieve the saved response."
			}
			serverError(w, r, 500, msg, errorCause(err))
			return
		}
		if dbBusyForContext(ctx, err) {
			serverError(w, r, 503, errBusyMsg, errorCause(err))
			return
		}
		serverError(w, r, 500, "Unable to save changes", errorCause(err))
		return
	}
	if res.Audit != "" && !repeated {
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
			if c.Tx != nil && dbBusyForContext(ctx, err) {
				serverError(w, r, 503, errBusyMsg, errorCause(err))
				return
			}
			serverError(w, r, 500, "Unable to record change", errorCause(err))
			return
		}
	}
	if res.Redirect == "" {
		res.Redirect = "/"
	}
	if spec.FormNonce && !repeated {
		if err = finishFormSubmission(c, res); err != nil {
			writeDBFailure(w, r, err)
			return
		}
	}
	if c.Tx != nil {
		if err = c.Tx.Commit(); err != nil {
			if spec.FormNonce {
				serverError(w, r, 500, "Submission outcome unknown: it may have been saved. Check the result on the relevant page before starting a new form. If the original form is still open, resubmit it unchanged to retrieve the saved response.", errorCause(err))
				return
			}
			serverError(w, r, 500, "Unable to save changes", errorCause(err))
			return
		}
	}
	if res.AfterCommit != nil {
		res.AfterCommit(w)
	}
	if res.Notice != "" && u != nil {
		res.Redirect = a.setNotice(w, res.Notice, res.Redirect)
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
