package market

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

//go:embed schema.sql
var schema string

type bucket struct {
	Count int
	Limit int // requests admitted per window; an entry with Count >= Limit is blocking and is never evicted
	Until time.Time
	Seen  []string // allowLookup: the distinct handles this window admitted (A-516)
}
type App struct {
	startedAt    time.Time       // this process instance, never a persisted heartbeat
	recentErrors serverErrorRing // A-143: last 20 referenced HTTP errors, in memory only

	db               *sql.DB
	health           *sql.DB // one connection for /healthz, apart from the application pool (A-178)
	templates        *template.Template
	preview, secure  bool
	mode, setupToken string // setupToken authorizes /setup only, and only until installed
	keyRoot          string // APP_SECRET_KEY, or SETUP_TOKEN (legacy): the root of key and the sealing keys
	key              []byte // CSRF/CAPTCHA HMAC key, sha256("csrf:"+keyRoot)
	captchaIDKey     []byte // CAPTCHA challenge-id MAC key, sha256("captcha-id:"+keyRoot); empty = no challenges
	mu               sync.Mutex
	limits           map[string]bucket         // authentication and factor budgets
	writeLimits      map[string]bucket         // independent per-account write budgets (A-75)
	inFlight         map[string]int            // OneAtATime actions in progress, by path and account (enterOneAtATime)
	signInLogAt      time.Time                 // last "sign-in limiter refusing" log line (guarded by mu)
	live             bool                      // escrow currently supports test networks only
	escrowConfigured bool                      // ESCROW_BTC=on (escrow_rpc.go); escrow may still be disabled
	escrowCfg        escrowConfig              // the ESCROW_BTC settings read at startup, for the escrow watcher's retries
	escrowStartState string                    // the escrow state line logged at startup (escrowRetry starts from it)
	escrow           atomic.Pointer[escrowBTC] // the Bitcoin escrow adapter once ESCROW_BTC=on passed its checks, at startup or on a watcher retry (A-316); nil = no escrow; never cleared once set
	escrowXMR        *escrowXMR                // Monero escrow while ESCROW_XMR=on (escrow_xmr_config.go), ready or not; nil = off; set once in New
	scanner          *imageScanner             // clamd client for uploads (clamd.go); nil = Disabled
	storageLimit     int64                     // IMAGE_STORAGE_LIMIT_MB in bytes (uploads.go); 0 = the default
	stop             context.CancelFunc
	background       sync.WaitGroup

	// The Bitcoin node sync guard and escrow chain pin (A-439, bitcoin_sync.go), in memory only.
	escrowSync     atomic.Value                        // string: the reason class the escrow watcher last found its node not synced for ("" or unset = synced)
	escrowMismatch atomic.Pointer[EscrowChainMismatch] // set while startEscrow refuses the node's chain against the escrow chain pin; nil = none
	escrowNet      atomic.Pointer[string]              // the escrow chain escrowNetwork names while escrow is not enabled (A-443); nil = not known yet
	// The arbiter wallet check (QA4W4A-1, checkArbiterWallet), in memory only; the checkpoint is in settings.
	escrowArbiterRead atomic.Pointer[string] // set while startEscrow could not finish reading the arbiter wallet's entries: "waiting" (the read goes on) or the read failure's class; nil = none
	escrowScanMu      sync.Mutex             // guards escrowScan and escrowPace
	escrowScan        *escrowArbiterScan     // the arbiter wallet read in progress, or finished and not yet taken (arbiterScan); nil = none
	escrowPace        escrowArbiterPace      // when the next arbiter wallet read may start (A-479, paceArbiter); guarded by escrowScanMu
	// Broadcast honesty (A-441, escrow_stall.go), in memory only.
	escrowTip               atomic.Int64                            // the escrow node's tip height the last escrow watcher pass read (0 = none yet)
	escrowHealthObservation atomic.Pointer[escrowHealthObservation] // allowlisted metadata from the last Bitcoin escrow startup check
	escrowUnseen            escrowUnseen                            // per broadcast settlement not seen by the watch-only wallet, the height it went unseen from
	escrowTorGate           atomic.Value                            // string: why the Tor-mode gate holds escrow broadcasts ("" = they may be sent; unset = not checked yet)
	// The escrow signer (A-454, escrow_signer.go): nil without ESCROW_SIGNER_SOCKET; its state is in memory only.
	escrowSigner       *escrowSignerClient
	escrowSignerStatus atomic.Value // string: the state checkEscrowSigner last found (signerReady, signerWaiting, …)
	// escrowSiblingsChecked: this process's escrow watcher has checked that the watch-only wallet holds every shape-(b)
	// escrow's sibling addresses (ensureSiblingWatch, A-456a).
	escrowSiblingsChecked atomic.Bool
}

// parseTemplates loads layout (web/templates/*.html), pages/<page>.html ("page:<page>") and partials/<hook>.html.
func parseTemplates() (*template.Template, error) {
	t := template.New("").Funcs(template.FuncMap{"formNonce": formNonceFor})
	for _, pattern := range []string{"web/templates/*.html", "web/templates/pages/*.html", "web/templates/partials/*.html"} {
		var err error
		if t, err = t.ParseGlob(pattern); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func New(ctx context.Context, preview bool) (*App, error) {
	t, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	a := &App{startedAt: time.Now().UTC(), templates: t, preview: preview, mode: os.Getenv("APP_MODE"), limits: make(map[string]bucket), writeLimits: make(map[string]bucket)}
	if a.mode == "" {
		a.mode = "clearnet"
	}
	if a.mode != "clearnet" && a.mode != "tor" {
		return nil, errors.New("APP_MODE must be clearnet or tor")
	}
	if preview {
		a.key = []byte(randomToken())
		return a, nil
	}
	// Everything checkable without the database is checked before it is opened (config.go).
	cfg, err := loadStartupConfig(os.Getenv, a.mode)
	if err != nil {
		return nil, err
	}
	a.secure, a.setupToken = cfg.secure, cfg.setupToken
	root, legacy := cfg.keyRoot()
	a.useKeyRoot(root)
	for _, note := range cfg.notes {
		log.Print(note)
	}
	dsn := os.Getenv("DATABASE_URL")
	timeouts, err := startupTimeoutsFromEnv()
	if err != nil {
		return nil, err
	}
	scanCfg, err := scannerConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, err
	}
	a.scanner = newImageScanner(scanCfg)
	if a.storageLimit, err = imageStorageLimitFromEnv(os.Getenv); err != nil {
		return nil, err
	}
	// The application pool is opened and pinged first: its session timeouts (A-177) are startup parameters,
	// which a connection pooler may refuse, and such a database must stop startup before the schema moves.
	db, err := openAppPool(ctx, dsn, timeouts.connect)
	if err != nil {
		return nil, err
	}
	// Migrations run on their own connection without the application pool's session timeouts (A-177): a
	// large migration, or the wait for another instance's migration lock, is bounded by MIGRATION_TIMEOUT
	// and MIGRATION_LOCK_TIMEOUT instead.
	mdb, err := OpenDB(dsn)
	if err != nil {
		db.Close()
		return nil, err
	}
	mdb.SetMaxOpenConns(2) // the migration transaction, plus pg_terminate_backend when a phase times out
	err = prepareDatabase(ctx, mdb, timeouts)
	mdb.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	a.db = db
	// /healthz pings on its own connection, so a saturated application pool does not make the probe wait.
	if a.health, err = OpenDB(dsn, WithAppTimeouts()); err != nil {
		db.Close()
		return nil, err
	}
	a.health.SetMaxOpenConns(1)
	a.health.SetMaxIdleConns(1)
	a.health.SetConnMaxLifetime(30 * time.Minute)
	if err = a.checkInstallState(ctx, legacy); err != nil {
		a.closeDB()
		return nil, err
	}
	escrowCtx, cancelEscrow := context.WithTimeout(ctx, paymentStartupTimeout)
	defer cancelEscrow()
	if err = a.initEscrow(escrowCtx); err != nil {
		a.closeDB()
		return nil, err
	}
	xmrCtx, cancelXMR := context.WithTimeout(ctx, paymentStartupTimeout)
	defer cancelXMR()
	if err = a.initEscrowXMR(xmrCtx); err != nil {
		a.closeDB()
		return nil, err
	}
	return a, nil
}

// openAppPool opens the application pool (WithAppTimeouts) and pings it within connectTimeout. When the
// server refuses it but accepts a plain connection, the error says the session settings were refused (a
// connection pooler such as PgBouncer rejects startup parameters it does not know; operator-guide.md). Errors
// name a class, never DATABASE_URL or the server's text.
func openAppPool(ctx context.Context, dsn string, connectTimeout time.Duration) (*sql.DB, error) {
	db, err := OpenDB(dsn, WithAppTimeouts())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	err = db.PingContext(connectCtx)
	if err == nil {
		return db, nil
	}
	db.Close()
	if timedOut(ctx, connectCtx) {
		return nil, fmt.Errorf("database connection timed out after %s; check DATABASE_URL and service health, or raise DATABASE_CONNECT_TIMEOUT", connectTimeout)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if plain, perr := OpenDB(dsn); perr == nil {
			perr = plain.PingContext(connectCtx)
			plain.Close()
			if perr == nil {
				return nil, fmt.Errorf("database refused the application's session settings (SQLSTATE %s): statement_timeout, lock_timeout and idle_in_transaction_session_timeout are sent when each connection starts, and a connection pooler such as PgBouncer refuses parameters it does not know; see \"Connection poolers\" in docs/operator-guide.md. Nothing was changed", pe.Code)
			}
		}
	}
	return nil, errors.New("database connection failed: " + connectErrorClass(err))
}

// DBOption adjusts the connection settings OpenDB uses.
type DBOption func(runtimeParams map[string]string)

// appSessionTimeouts are the per-connection limits of the application pool (A-177): no statement runs longer
// than 8 s, no lock is waited for longer than 3 s and no transaction sits idle longer than 15 s. A longer
// operation on that pool needs a scoped SET LOCAL. Migrations and the host commands never get them.
var appSessionTimeouts = map[string]string{
	"statement_timeout":                   "8s",
	"lock_timeout":                        "3s",
	"idle_in_transaction_session_timeout": "15s",
}

// WithAppTimeouts sets appSessionTimeouts on every connection (startup parameters, beside timezone). Only the
// server's application pool and its health probe connection use it.
func WithAppTimeouts() DBOption {
	return func(p map[string]string) {
		for k, v := range appSessionTimeouts {
			setRuntimeParam(p, k, v)
		}
	}
}

// setRuntimeParam sets k, replacing any spelling of it that dsn carried (setting names are case-insensitive).
func setRuntimeParam(p map[string]string, k, v string) {
	for existing := range p {
		if strings.EqualFold(existing, k) {
			delete(p, existing)
		}
	}
	p[k] = v
}

// OpenDB opens the database in dsn with TimeZone pinned to UTC and client_encoding to UTF8 on every connection
// (startup parameters, which outrank database and role defaults), so displayed times never follow or reveal
// the server's zone. The
// application pool, its migrations and the -bootstrap-admin, -reset-admin-password, -reset-admin-factors,
// -set-role and -status commands all open the database here; only the application pool passes WithAppTimeouts.
func OpenDB(dsn string, opts ...DBOption) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("database configuration invalid")
	}
	setRuntimeParam(cfg.RuntimeParams, "timezone", "UTC")
	// pgx sends no client_encoding, so a session would use the database's own encoding (or the role's setting).
	// Text crosses the wire as UTF-8 only if the session says so (A-481); startup separately refuses a
	// database that is not UTF8.
	setRuntimeParam(cfg.RuntimeParams, "client_encoding", "UTF8")
	for _, o := range opts {
		o(cfg.RuntimeParams)
	}
	return stdlib.OpenDB(*cfg), nil
}

// closeDB closes the application pool and the health probe connection.
func (a *App) closeDB() {
	if a.health != nil {
		a.health.Close()
	}
	if a.db != nil {
		a.db.Close()
	}
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (a *App) csrf(token string) string {
	h := hmac.New(sha256.New, a.key)
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}
func (a *App) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: age})
}

// allow counts one request against key's budget of n per ten minutes. When the table is full it evicts
// the non-blocking entry with the lowest count (earliest expiry breaks ties); if every entry is blocking
// it refuses the new key instead, so junk keys can never reset a counter that is refusing requests.
func (a *App) allow(key string, n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return allowIn(a.limits, key, n, time.Now())
}

// allowWrite reserves a separate bounded table for signed-in writes. Saturated authentication
// counters must not refuse a legitimate account's write, and saturated write counters must
// neither reset themselves nor evict blocking entries from either table.
func (a *App) allowWrite(userID string) bool {
	return a.allowWriteBudget(userID, 100)
}

func (a *App) allowWriteBudget(key string, n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeLimits == nil {
		a.writeLimits = make(map[string]bucket)
	}
	return allowIn(a.writeLimits, key, n, time.Now())
}

// allowLookup counts a recipient lookup on /messages?to= against the account's budget of lookupBudget distinct
// handles per ten minutes, in the write table (A-516, A-92). A handle the window already admitted is admitted again
// without spending, so reloading the compose page never runs the budget down.
func (a *App) allowLookup(userID, handle string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writeLimits == nil {
		a.writeLimits = make(map[string]bucket)
	}
	key, now := "lookup:"+userID, time.Now()
	if b, ok := a.writeLimits[key]; ok && !now.After(b.Until) && slices.Contains(b.Seen, handle) {
		return true
	}
	if !allowIn(a.writeLimits, key, lookupBudget, now) {
		return false
	}
	b := a.writeLimits[key]
	b.Seen = append(b.Seen, handle)
	a.writeLimits[key] = b
	return true
}

func allowIn(limits map[string]bucket, key string, n int, now time.Time) bool {
	for k, v := range limits {
		if now.After(v.Until) {
			delete(limits, k)
		}
	}
	b := limits[key]
	if b.Count == 0 {
		if len(limits) >= 4096 {
			victim, found := "", false
			for k, v := range limits {
				if v.Count >= v.Limit {
					continue
				}
				if w := limits[victim]; !found || v.Count < w.Count || (v.Count == w.Count && v.Until.Before(w.Until)) {
					victim, found = k, true
				}
			}
			if !found {
				return false
			}
			delete(limits, victim)
		}
		b.Until = now.Add(10 * time.Minute)
	}
	b.Count++
	b.Limit = n
	limits[key] = b
	return b.Count <= n
}

// refund takes back one request that allow counted against key (a sign-in whose password was correct). An
// entry left with no count is removed.
func (a *App) refund(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.limits[key]
	if !ok {
		return
	}
	if b.Count <= 1 {
		delete(a.limits, key)
		return
	}
	b.Count--
	a.limits[key] = b
}

// logSignInRefusals writes "sign-in limiter refusing handles=N", at most once a minute, while N > 0 handles
// have spent their sign-in budget, so the operator can see a lockout without the log naming anyone.
func (a *App) logSignInRefusals() {
	a.mu.Lock()
	now, n := time.Now(), 0
	if now.Sub(a.signInLogAt) >= time.Minute {
		for k, b := range a.limits {
			if strings.HasPrefix(k, "auth:") && b.Count >= b.Limit && !now.After(b.Until) {
				n++
			}
		}
		if n > 0 {
			a.signInLogAt = now
		}
	}
	a.mu.Unlock()
	if n > 0 {
		log.Printf("sign-in limiter refusing handles=%d", n)
	}
}

// limited reports whether key has already spent its budget of n, without counting a request.
func (a *App) limited(key string, n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.limits[key]
	return ok && !time.Now().After(b.Until) && b.Count >= n
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if a.secure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if r.Method != "GET" && r.Method != "POST" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD, POST")
		a.refuse(w, r, nil, 405, "Method not allowed")
		return
	}
	// No page, raw route or image route meets query text the database would refuse: the loaders passed it to
	// PostgreSQL, so /product?id=%ff answered 500 (QA5W4B-5). A post's query string is in r.Form, checked below.
	if r.Method != "POST" && !formTextValid(r.URL.Query()) {
		a.refuse(w, r, nil, 400, invalidQueryText)
		return
	}
	// Admission control (admission.go): a page finding its class full is refused here with 503, before any
	// database work; a form post takes its slot in a.post, once its body has been read and checked and its
	// session looked up.
	aw, release, ok := admit(w, r)
	if !ok {
		return
	}
	defer release()
	w = aw
	// An upload's work after its body has arrived (uploads.go) and the signed audit export (audit_export.go,
	// through arrivalContext) get their own bounds.
	base := context.WithValue(r.Context(), serverErrorRingKey{}, &a.recentErrors)
	ctx, cancel := context.WithTimeout(context.WithValue(base, arrivalKey{}, base), 12*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == "/healthz" {
		probe := a.health
		if probe == nil {
			probe = a.db // an App built without New (tests)
		}
		if probe != nil {
			if err := probe.PingContext(ctx); err != nil {
				var ce *pgconn.ConnectError
				if errors.As(err, &ce) && !dbBusyForContext(ctx, err) {
					// A persistent TLS/certificate fault is unhealthy but does not warrant Retry-After.
					serverError(w, r, 500, "unavailable", errorCause(err))
				} else {
					serverError(w, r, 503, "unavailable", errorCause(err))
				}
				return
			}
		}
		w.Write([]byte("ok"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		if r.Method == "POST" {
			a.refuse(w, r, nil, 405, "Method not allowed")
			return
		}
		a.serveStatic(w, r)
		return
	}
	// The image routes (image_routes.go) branch here, before any cookie is read or set: the public one reads no
	// session at all, the desk one only the session cookie.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/img/"); ok {
		a.servePublicImage(w, r, rest)
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/desk/img/"); ok {
		a.serveDeskImage(w, r, rest)
		return
	}
	// Two SameSite=Strict cookies: `session` holds a signed-in session and is set only at sign-in (and
	// cleared at sign-out); `anon` holds the pre-login token that backs CSRF and CAPTCHA for anonymous
	// requests. A cross-site link arrives with neither, so the response issues only a new `anon` and can
	// never overwrite the browser's real `session`. A `session` value that is not (or no longer) a live
	// session, such as a pre-upgrade anonymous one, still serves as this request's token.
	token, anon := "", ""
	if c, err := r.Cookie("session"); err == nil && len(c.Value) == 64 {
		token = c.Value
	}
	if c, err := r.Cookie("anon"); err == nil && len(c.Value) == 64 {
		anon = c.Value
	}
	session := token // looked up below, unless a post's form turns out to have been rendered for `anon`
	if token == "" {
		if anon == "" {
			anon = randomToken()
			a.cookie(w, "anon", anon, 86400)
		}
		token = anon
	}
	r = r.WithContext(context.WithValue(context.WithValue(context.WithValue(ctx, sessionKey{}, token), anonKey{}, anon), rotationSessionKey{}, session))
	ctx = r.Context()
	if r.Method == "POST" {
		// Everything here reads no database and holds no slot: a post takes its slot in a.post, after its
		// body has been read and checked, so a client trickling a body holds only its connection (A-178).
		// The two upload routes read their multipart body themselves, after the sender is admitted (uploads.go);
		// any other multipart post is refused before its body is read.
		if route, ok := uploadRoutes[r.URL.Path]; ok {
			a.serveUpload(w, r.WithContext(context.WithValue(context.WithValue(base, sessionKey{}, token), anonKey{}, anon)), route, session, token)
			return
		}
		// The sign form's signed PSBT file (A-456b) is the one other multipart post; it is read below.
		if isMultipart(r) && !psbtUploadRoutes[r.URL.Path] {
			a.refuseMultipartElsewhere(w, r, token)
			return
		}
		if a.preview {
			a.refuse(w, r, nil, 403, "Read-only preview. Start with PostgreSQL to save changes.")
			return
		}
		if isMultipart(r) {
			if isHardwarePSBTUpload(r.URL.Path) {
				select {
				case hardwarePSBTReadSlots <- struct{}{}:
					defer func() { <-hardwarePSBTReadSlots }()
				default:
					a.refuse(w, r, nil, http.StatusServiceUnavailable, "Hardware PSBT uploads are busy. Try again shortly. Nothing was changed.")
					return
				}
			}
			// Its six known fields, bounded, become the form; every check below applies to them (psbt_upload.go).
			form, code, msg := readPSBTUploadForm(w, r)
			if form == nil {
				a.refuse(w, r, nil, code, msg)
				return
			}
			r.Form, r.PostForm = form, form
		} else {
			// The body is read first (at most 64 KiB) and its fields counted before it is parsed: parsing runs before
			// any slot, on as many connections as the listener admits, and a body of thousands of distinct empty
			// fields cost about 3.7 MB of allocation to parse. No form has more than maxFormFields (PR #15 re-review).
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
			if err != nil || bytes.Count(body, []byte("&")) >= maxFormFields {
				a.refuse(w, r, nil, 400, "Invalid or oversized form")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if err := r.ParseForm(); err != nil {
				r.PostForm = nil // nothing of a form that did not parse is shown back
				a.refuse(w, r, nil, 400, "Invalid or oversized form")
				return
			}
		}
		// No handler meets text the database would refuse: on a UTF8 database invalid UTF-8 or a NUL byte made the
		// write fail with a 500, after a confirmation had accepted the password (QA5W3B-5). The upload routes check
		// their text parts themselves (uploads.go readForm), and so does the sign form's multipart reader.
		if !formTextValid(r.Form) {
			a.refuse(w, r, nil, 400, invalidFormText)
			return
		}
		if msg := postOriginError(r, a.secure); msg != "" {
			a.refuse(w, r, nil, 403, msg)
			return
		}
		if sent := []byte(r.PostForm.Get("csrf")); subtle.ConstantTimeCompare(sent, []byte(a.csrf(token))) != 1 {
			// A page rendered while the browser withheld `session` (it followed a cross-site link) holds
			// a token derived from `anon`, and this same-site POST now sends both cookies. Serve it as the
			// anonymous request the form was rendered for: never as the signed-in user, so the anon token
			// grants nothing an anonymous visitor lacks, while that page's sign-in form and CAPTCHA work.
			// Origin and Sec-Fetch-Site are checked above either way. Successful sign-in
			// rotates the original session cookie retained separately for that purpose; a refusal leaves it live.
			if anon == "" || anon == token || subtle.ConstantTimeCompare(sent, []byte(a.csrf(anon))) != 1 {
				a.refuse(w, r, nil, 403, formExpired)
				return
			}
			token, session = anon, ""
			r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, token)) // keeps anonKey
		}
		if spec, ok := actions[r.URL.Path]; ok && spec.Precheck != nil {
			if err := spec.Precheck(a, r, token); err != nil {
				a.writeHTTPError(w, r, err)
				return
			}
		}
	}
	var user *User
	if session != "" && a.db != nil {
		lookupDone := func() {}
		if r.Method == "POST" {
			// A page holds its page slot already. A post's lookup takes a slot of its own class for this
			// query only (takeLookupSlot), so a flood of posts with made-up session cookies is bounded, and a
			// flood of pages, which never takes that class, cannot refuse a signed-in or staff post.
			done, ok := takeLookupSlot(ctx)
			if !ok {
				busy(w)
				return
			}
			defer done() // on a panic; released below as soon as the query is done
			lookupDone = done
		}
		u, err := a.sessionUser(ctx, session)
		lookupDone()
		if err != nil {
			writeDBFailure(w, r, err)
			return
		}
		user = u
	}
	if r.Method == "POST" {
		a.post(w, r, user, token)
		return
	}
	if h, ok := raws[r.URL.Path]; ok {
		h(a, w, r, user)
		return
	}
	page := strings.TrimPrefix(r.URL.Path, "/")
	if page == "" {
		page = "catalog"
	}
	if !a.pageOn(page) {
		a.refuse(w, r, user, 404, pageNotFound)
		return
	}
	if a.preview {
		d := previewData(page)
		q := r.URL.Query()
		d.Query = q.Get("q")
		d.Category = q.Get("category")
		d.Region = q.Get("region")
		normaliseCatalogFilters(&d)
		if q.Get("currency") == "XMR" {
			d.Currency = "XMR"
		}
		if page == "catalog" || page == "vendor" || page == "product" || page == "checkout" {
			all := d.Products
			d.Products = nil
			for _, p := range all {
				match := true
				if page == "catalog" {
					match = (d.Category == "" || p.Category == d.Category) && (d.Region == "" || p.Region == d.Region || p.Region == "Worldwide") && strings.Contains(strings.ToLower(p.Title+" "+p.Description), strings.ToLower(d.Query))
				}
				if page == "vendor" {
					match = p.VendorID == q.Get("id")
				}
				if page == "product" || page == "checkout" {
					match = p.ID == q.Get("id")
				}
				if match {
					d.Products = append(d.Products, p)
				}
			}
			if page == "catalog" {
				d.Products = pageSlice(&d, d.Products, q.Get("page"))
			}
			if page == "product" || page == "checkout" {
				if len(d.Products) == 0 {
					a.refuse(w, r, user, 404, pageNotFound)
					return
				}
				d.Product = &d.Products[0]
			}
			if page == "vendor" && len(d.Products) > 0 {
				d.Users = []User{{ID: d.Products[0].VendorID, Handle: d.Products[0].Vendor, Role: "vendor"}}
			}
		}
		if page == "order" && q.Get("id") == previewPaidID {
			previewPaidOrder(&d)
		}
		if page == "listing-edit" || page == "account" {
			previewUploads(&d, q.Get("scanner"))
		}
		d.CSRF = a.csrf(token)
		d.Mode = a.mode
		applyPreviews(&d)
		objectTitle(&d)
		a.render(w, r, d)
		return
	}
	var installed bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil {
		writeDBFailure(w, r, err)
		return
	}
	if !installed && page != "setup" {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if installed && page == "setup" {
		a.refuse(w, r, user, 403, "Installation is already complete")
		return
	}
	if protected(page) && user == nil {
		login := "/login"
		if next := a.signInNext(r.URL.RequestURI()); next != "" {
			login += "?next=" + url.QueryEscape(next) // signing in returns here (A-126)
		}
		http.Redirect(w, r, login, 303)
		return
	}
	if !authorized(page, user) {
		a.refuse(w, r, user, 403, "You do not have access to this page")
		return
	}
	d := a.pageData(r, page, token, user)
	a.takeNotice(w, r, &d)
	if err := a.load(r, &d); err != nil {
		var he *httpError
		switch {
		case err == sql.ErrNoRows:
			a.refuse(w, r, user, 404, pageNotFound)
		case dbBusyForContext(ctx, err):
			serverError(w, r, 503, errBusyMsg, errorCause(err))
		case !errors.As(err, &he):
			serverError(w, r, 500, "Unable to load this page", errorCause(err))
		case he.Code >= 500:
			serverError(w, r, he.Code, he.Msg, errorCause(he))
		default:
			a.refuse(w, r, user, he.Code, he.Msg)
		}
		return
	}
	objectTitle(&d)
	a.render(w, r, d)
}

// formExpired refuses a post whose CSRF token is not this browser's, or a cross-site post.
const formExpired = "Form expired. Reload and try again."

// pageNotFound answers an unknown page, a page switched off (pageSpec.Off) and a page whose object does not exist alike.
const pageNotFound = "Page not found."

// invalidFormText refuses a form whose text formTextValid rejects.
const invalidFormText = "Invalid form: a field is not valid UTF-8 or contains a NUL character. Nothing was changed."

// invalidQueryText refuses a GET or HEAD whose query string formTextValid rejects (QA5W4B-5).
const invalidQueryText = "Invalid address: a query parameter is not valid UTF-8 or contains a NUL character."

// formTextValid reports whether every name and value in form is valid UTF-8 without a NUL byte, as PostgreSQL's
// UTF8 text must be.
func formTextValid(form url.Values) bool {
	ok := func(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
	for k, vs := range form {
		if !ok(k) {
			return false
		}
		for _, v := range vs {
			if !ok(v) {
				return false
			}
		}
	}
	return true
}

// postOriginError refuses a different Origin host, an HTTP Origin when secure cookies are required, or a
// cross-site fetch. Forwarded headers do not weaken this check. The fixed copy names no header value; a
// common host mismatch is a reverse proxy rewriting Host (A-138).
func postOriginError(r *http.Request, secure bool) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") || (secure && u.Scheme != "https") {
			return "Origin does not match Host; if this market runs behind a reverse proxy, the proxy must forward the Host header."
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return formExpired
	}
	return ""
}

// pageTitle is a page's default title, from its name ("vendor-dashboard" → "Vendor Dashboard"), or the name the
// navigation gives it; loaders may replace it.
func pageTitle(page string) string {
	switch page {
	case "catalog":
		return "Marketplace"
	case "canary":
		return "Transparency"
	}
	return strings.ReplaceAll(strings.Title(page), "-", " ")
}

// objectTitle names the listing, order or vendor a page shows in its title (A-117), once the page has loaded.
func objectTitle(d *PageData) {
	switch {
	case d.Page == "product" && d.Product != nil:
		d.Title = d.Product.Title
	case d.Page == "order" && d.Order != nil:
		d.Title = "Order " + shortID(d.Order.ID) + " – " + d.Order.Title
	case d.Page == "vendor" && len(d.Users) == 1:
		d.Title = "Vendor " + d.Users[0].Handle
	}
}

// pageData is the state a page render starts from, before a.load fills it.
func (a *App) pageData(r *http.Request, page, token string, user *User) PageData {
	d := PageData{Page: page, Title: pageTitle(page), CSRF: a.csrf(token), User: user, Mode: a.mode, Query: r.URL.Query().Get("q"), Category: r.URL.Query().Get("category"), Region: r.URL.Query().Get("region"), Currency: r.URL.Query().Get("currency"), BitcoinPriceLabel: "Price (BTC, network not configured)", Settings: map[string]string{}}
	if d.Currency != "XMR" {
		d.Currency = "BTC"
	}
	return d
}

func (a *App) render(w http.ResponseWriter, r *http.Request, d PageData) {
	a.renderStatus(w, r, d, http.StatusOK)
}

func (a *App) renderStatus(w http.ResponseWriter, r *http.Request, d PageData, code int) {
	var b bytes.Buffer
	if err := a.templates.ExecuteTemplate(&b, "page:"+d.Page, d); err != nil {
		cause := errorCause(err)
		if !strings.HasPrefix(cause, "template:") {
			cause = "template:page:" + d.Page
		}
		serverError(w, r, 500, "Unable to render page", cause)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(b.Bytes())
}

func parseAmount(s string, decimals int) (int64, error) {
	if s == "" || strings.TrimSpace(s) != s {
		return 0, errors.New("invalid amount")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, errors.New("invalid amount")
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > decimals {
		return 0, errors.New("too many decimals")
	}
	all := parts[0] + frac + strings.Repeat("0", decimals-len(frac))
	if len(parts[0]) == 0 {
		return 0, errors.New("invalid amount")
	}
	for _, c := range all {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid amount")
		}
	}
	n, e := strconv.ParseInt(all, 10, 64)
	if e != nil || n <= 0 {
		return 0, errors.New("amount out of range")
	}
	return n, nil
}
func amount(n int64, d int) string {
	s := strconv.FormatInt(n, 10)
	for len(s) <= d {
		s = "0" + s
	}
	return strings.TrimRight(strings.TrimRight(s[:len(s)-d]+"."+s[len(s)-d:], "0"), ".")
}
