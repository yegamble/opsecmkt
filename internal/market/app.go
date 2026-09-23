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
	_ "github.com/jackc/pgx/v5/stdlib"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed schema.sql
var schema string

type bucket struct {
	Count int
	Until time.Time
}
type App struct {
	db               *sql.DB
	templates        *template.Template
	preview, secure  bool
	mode, setupToken string
	key              []byte
	mu               sync.Mutex
	limits           map[string]bucket
	payMu            sync.RWMutex                    // guards payments and unavailable once Start has run
	checkMu          sync.Mutex                      // serialises provider checks (refreshProviders)
	payments         map[string]PaymentProvider      // currency -> working provider; empty = payments disabled
	unavailable      map[string]*unavailableProvider // configured but failing its checks (retried each watcher pass)
	stop             context.CancelFunc
	background       sync.WaitGroup
}

// parseTemplates loads layout (web/templates/*.html), pages/<page>.html ("page:<page>") and partials/<hook>.html.
func parseTemplates() (*template.Template, error) {
	t := template.New("")
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
	a := &App{templates: t, preview: preview, secure: os.Getenv("COOKIE_SECURE") != "false", mode: os.Getenv("APP_MODE"), setupToken: os.Getenv("SETUP_TOKEN"), limits: make(map[string]bucket), payments: map[string]PaymentProvider{}, unavailable: map[string]*unavailableProvider{}}
	if a.mode == "" {
		a.mode = "clearnet"
	}
	if a.mode != "clearnet" && a.mode != "tor" {
		return nil, errors.New("APP_MODE must be clearnet or tor")
	}
	if preview {
		a.secure = false
		a.key = []byte(randomToken())
		return a, nil
	}
	if err := checkSetupToken(a.setupToken); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte("csrf:" + a.setupToken))
	a.key = sum[:]
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL required; use -preview for read-only sample UI")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("database configuration invalid")
	}
	a.db = db
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database connection failed; check DATABASE_URL and service health")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
		return nil, err
	}
	if err = migrate(ctx, tx); err != nil {
		return nil, fmt.Errorf("database migration failed: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = a.initPayments(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
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
func (a *App) allow(key string, n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.limits {
		if now.After(v.Until) {
			delete(a.limits, k)
		}
	}
	b := a.limits[key]
	if b.Count == 0 {
		if len(a.limits) >= 4096 { // full: evict the entry that expires first rather than refusing new keys
			oldest := ""
			for k, v := range a.limits {
				if oldest == "" || v.Until.Before(a.limits[oldest].Until) {
					oldest = k
				}
			}
			delete(a.limits, oldest)
		}
		b.Until = now.Add(10 * time.Minute)
	}
	b.Count++
	a.limits[key] = b
	return b.Count <= n
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if r.Method != "GET" && r.Method != "POST" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == "/healthz" {
		if a.db != nil {
			if err := a.db.PingContext(ctx); err != nil {
				http.Error(w, "unavailable", 503)
				return
			}
		}
		w.Write([]byte("ok"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/static/") {
		if r.Method == "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))).ServeHTTP(w, r)
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
	var user *User
	if token != "" && a.db != nil {
		u := &User{}
		err := a.db.QueryRowContext(ctx, `SELECT u.id,u.handle,u.role,u.pgp,u.xmpp FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=$1 AND s.expires>now()`, digest(token)).Scan(&u.ID, &u.Handle, &u.Role, &u.PGP, &u.XMPP)
		if err == nil {
			user = u
		} else if err != sql.ErrNoRows {
			http.Error(w, "Service unavailable", 503)
			return
		}
	}
	if token == "" {
		if anon == "" {
			anon = randomToken()
			a.cookie(w, "anon", anon, 86400)
		}
		token = anon
	}
	r = r.WithContext(context.WithValue(context.WithValue(ctx, sessionKey{}, token), anonKey{}, anon))
	ctx = r.Context()
	if r.Method == "POST" {
		if a.preview {
			http.Error(w, "Read-only preview. Start with PostgreSQL to save changes.", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid or oversized form", 400)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Cross-origin request rejected", 403)
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Form expired. Reload and try again.", 403)
			return
		}
		if sent := []byte(r.PostForm.Get("csrf")); subtle.ConstantTimeCompare(sent, []byte(a.csrf(token))) != 1 {
			// A page rendered while the browser withheld `session` (it followed a cross-site link) holds
			// a token derived from `anon`, and this same-site POST now sends both cookies. Serve it as the
			// anonymous request the form was rendered for: never as the signed-in user, so the anon token
			// grants nothing an anonymous visitor lacks, while that page's sign-in form and CAPTCHA work.
			// Origin and Sec-Fetch-Site are checked above either way. A sign-in served this way replaces
			// the `session` cookie; the previous session row lapses at expiry or with revoke-sessions.
			if anon == "" || anon == token || subtle.ConstantTimeCompare(sent, []byte(a.csrf(anon))) != 1 {
				http.Error(w, "Form expired. Reload and try again.", 403)
				return
			}
			token, user = anon, nil
			r = r.WithContext(context.WithValue(ctx, sessionKey{}, token))
		}
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
	if _, ok := pages[page]; !ok {
		http.NotFound(w, r)
		return
	}
	if a.preview {
		d := previewData(page)
		q := r.URL.Query()
		d.Query = q.Get("q")
		d.Category = q.Get("category")
		d.Region = q.Get("region")
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
			if page == "product" || page == "checkout" {
				if len(d.Products) == 0 {
					http.NotFound(w, r)
					return
				}
				d.Product = &d.Products[0]
			}
			if page == "vendor" && len(d.Products) > 0 {
				d.Users = []User{{ID: d.Products[0].VendorID, Handle: d.Products[0].Vendor, Role: "vendor"}}
			}
		}
		d.CSRF = a.csrf(token)
		d.Mode = a.mode
		applyPreviews(&d)
		a.render(w, d)
		return
	}
	var installed bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil {
		http.Error(w, "Service unavailable", 503)
		return
	}
	if !installed && page != "setup" {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if installed && page == "setup" {
		http.Error(w, "Installation is already complete", 403)
		return
	}
	if protected(page) && user == nil {
		http.Redirect(w, r, "/login", 303)
		return
	}
	if !authorized(page, user) {
		http.Error(w, "You do not have access to this page", 403)
		return
	}
	d := PageData{Page: page, Title: strings.ReplaceAll(strings.Title(page), "-", " "), CSRF: a.csrf(token), User: user, Mode: a.mode, Query: r.URL.Query().Get("q"), Category: r.URL.Query().Get("category"), Region: r.URL.Query().Get("region"), Currency: r.URL.Query().Get("currency"), Settings: map[string]string{}}
	if d.Currency != "XMR" {
		d.Currency = "BTC"
	}
	if r.URL.Query().Get("saved") == "1" {
		d.Notice = "Changes saved."
	}
	if err := a.load(r, &d); err != nil {
		var he *httpError
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
		} else if errors.As(err, &he) {
			http.Error(w, he.Msg, he.Code)
		} else {
			http.Error(w, "Unable to load this page", 500)
		}
		return
	}
	a.render(w, d)
}
func (a *App) render(w http.ResponseWriter, d PageData) {
	var b bytes.Buffer
	if err := a.templates.ExecuteTemplate(&b, "page:"+d.Page, d); err != nil {
		http.Error(w, "Unable to render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
