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
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed schema.sql
var schema string

type User struct{ ID, Handle, Role, PGP, XMPP string }
type Product struct {
	ID, Title, Description, Category, Region, Kind, Vendor, VendorID, PriceBTC, PriceXMR string
	Stock                                                                                int
}
type Order struct{ ID, ProductID, Title, Buyer, Vendor, Currency, Amount, Status, Created string }
type Message struct{ ID, Sender, Recipient, Body, Created string }
type Notification struct {
	ID, Body, Created string
	Read              bool
}
type Dispute struct{ ID, OrderID, Reason, Status, Resolution, Created string }
type Event struct{ Action, Created string }
type PageData struct {
	Page, Title, CSRF, Error, Notice, Query, Category, Region, Currency, Mode string
	User                                                                      *User
	Products                                                                  []Product
	Product                                                                   *Product
	Orders                                                                    []Order
	Order                                                                     *Order
	Messages                                                                  []Message
	Notifications                                                             []Notification
	Disputes                                                                  []Dispute
	Users                                                                     []User
	Events                                                                    []Event
	Settings                                                                  map[string]string
	Preview                                                                   bool
}
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
}

func New(ctx context.Context, preview bool) (*App, error) {
	t, err := template.ParseFiles("web/templates/pages.html")
	if err != nil {
		return nil, err
	}
	a := &App{templates: t, preview: preview, secure: os.Getenv("COOKIE_SECURE") != "false", mode: os.Getenv("APP_MODE"), setupToken: os.Getenv("SETUP_TOKEN"), limits: make(map[string]bucket)}
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
	if len(a.setupToken) < 32 {
		return nil, errors.New("SETUP_TOKEN must contain at least 32 random characters")
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
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return nil, errors.New("database migration failed")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return a, nil
}
func (a *App) Close() {
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
		if len(a.limits) >= 4096 {
			return false
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
	w.Header().Set("Referrer-Policy", "no-referrer")
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
	token := ""
	if c, err := r.Cookie("session"); err == nil && len(c.Value) == 64 {
		token = c.Value
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
		token = randomToken()
		a.cookie(w, "session", token, 86400)
	}
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
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(a.csrf(token))) != 1 {
			http.Error(w, "Form expired. Reload and try again.", 403)
			return
		}
		a.post(w, r, user, token)
		return
	}
	page := strings.TrimPrefix(r.URL.Path, "/")
	if page == "" {
		page = "catalog"
	}
	allowed := map[string]bool{"catalog": true, "product": true, "vendor": true, "checkout": true, "orders": true, "order": true, "messages": true, "notifications": true, "disputes": true, "account": true, "vendor-dashboard": true, "moderator": true, "admin": true, "canary": true, "setup": true, "login": true, "register": true}
	if !allowed[page] {
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
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unable to load this page", 500)
		}
		return
	}
	a.render(w, d)
}
func protected(page string) bool {
	switch page {
	case "checkout", "orders", "order", "messages", "notifications", "disputes", "account", "vendor-dashboard", "moderator", "admin":
		return true
	}
	return false
}
func authorized(page string, u *User) bool {
	switch page {
	case "admin":
		return u != nil && u.Role == "admin"
	case "moderator":
		return u != nil && (u.Role == "admin" || u.Role == "moderator")
	case "vendor-dashboard":
		return u != nil && (u.Role == "admin" || u.Role == "vendor")
	}
	return true
}
func (a *App) render(w http.ResponseWriter, d PageData) {
	var b bytes.Buffer
	if err := a.templates.ExecuteTemplate(&b, "page", d); err != nil {
		http.Error(w, "Unable to render page", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b.Bytes())
}

var passwordWork = make(chan struct{}, 4)

var handlePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

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
func (a *App) post(w http.ResponseWriter, r *http.Request, u *User, token string) {
	ctx := r.Context()
	f := r.PostForm
	path := r.URL.Path
	fail := func(s string, code int) { http.Error(w, s, code) }
	if path == "/setup" || path == "/login" || path == "/register" {
		select {
		case passwordWork <- struct{}{}:
			defer func() { <-passwordWork }()
		default:
			fail("Authentication is busy. Try again shortly.", 503)
			return
		}
		handle := strings.TrimSpace(f.Get("handle"))
		password := f.Get("password")
		if !a.allow("auth:"+strings.ToLower(handle), 10) {
			fail("Too many attempts. Try again in ten minutes.", 429)
			return
		}
		if !handlePattern.MatchString(handle) || len(password) < 12 || len(password) > 72 {
			fail("Use a 3–32 character handle (letters, digits, underscores) and a password of 12–72 bytes.", 400)
			return
		}
		var installed bool
		if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil {
			fail("Service unavailable", 503)
			return
		}
		if path == "/login" {
			var id, hash string
			err := a.db.QueryRowContext(ctx, "SELECT id,password_hash FROM users WHERE handle=$1", handle).Scan(&id, &hash)
			if err != nil {
				hash = "$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW"
			}
			check := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
			if err != nil || check != nil {
				fail("Invalid handle or password", 401)
				return
			}
			if err = a.login(ctx, w, id, token); err != nil {
				fail("Unable to sign in", 500)
				return
			}
			http.Redirect(w, r, "/", 303)
			return
		}
		if path == "/setup" && (installed || subtle.ConstantTimeCompare([]byte(f.Get("token")), []byte(a.setupToken)) != 1) {
			fail("Setup unavailable or token incorrect", 403)
			return
		}
		if path == "/register" && !installed {
			fail("Complete installation first", 403)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			fail("Unable to create account", 500)
			return
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			fail("Service unavailable", 503)
			return
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(782493)"); err != nil {
			fail("Service unavailable", 503)
			return
		}
		if path == "/setup" {
			if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='installed')").Scan(&installed); err != nil || installed {
				fail("Setup already complete", 403)
				return
			}
		}
		role := "buyer"
		if path == "/setup" {
			role = "admin"
		}
		id := randomToken()
		if _, err = tx.ExecContext(ctx, "INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,$4)", id, handle, string(hash), role); err != nil {
			fail("Handle unavailable", 400)
			return
		}
		if path == "/setup" {
			name := strings.TrimSpace(f.Get("site_name"))
			if name == "" {
				name = "OPSMKT"
			}
			if len(name) > 80 {
				fail("Site name too long", 400)
				return
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('installed','true'),('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value", name); err != nil {
				fail("Setup failed", 500)
				return
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", id, "Account created: "+role); err != nil {
			fail("Audit failed", 500)
			return
		}
		if err = tx.Commit(); err != nil {
			fail("Unable to save account", 500)
			return
		}
		if err = a.login(ctx, w, id, token); err != nil {
			fail("Account created; sign in to continue", 500)
			return
		}
		http.Redirect(w, r, "/", 303)
		return
	}
	if u == nil {
		fail("Sign in required", 401)
		return
	}
	if !a.allow("write:"+u.ID, 100) {
		fail("Too many changes. Try again later.", 429)
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		fail("Service unavailable", 503)
		return
	}
	defer tx.Rollback()
	redirect := "/"
	action := ""
	switch path {
	case "/logout":
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1", digest(token))
		a.cookie(w, "session", "", -1)
		redirect = "/login"
		action = "Signed out"
	case "/revoke-sessions":
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=$1", u.ID)
		a.cookie(w, "session", "", -1)
		redirect = "/login"
		action = "Revoked all sessions"
	case "/account":
		pgp, xmpp := strings.TrimSpace(f.Get("pgp")), strings.TrimSpace(f.Get("xmpp"))
		if len(pgp) > 16384 || len(xmpp) > 254 {
			fail("Profile fields too long", 400)
			return
		}
		if pgp != "" && (!strings.HasPrefix(pgp, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || !strings.HasSuffix(pgp, "-----END PGP PUBLIC KEY BLOCK-----")) {
			fail("Paste an armored public key only. Never upload your private key.", 400)
			return
		}
		_, err = tx.ExecContext(ctx, "UPDATE users SET pgp=$1,xmpp=$2 WHERE id=$3", pgp, xmpp, u.ID)
		redirect = "/account?saved=1"
		action = "Updated profile (key not cryptographically verified)"
	case "/orders":
		currency := f.Get("currency")
		if currency != "BTC" && currency != "XMR" {
			fail("Choose BTC or XMR", 400)
			return
		}
		var btc, xmr int64
		var vendor string
		var stock int
		err = tx.QueryRowContext(ctx, "SELECT btc,xmr,vendor_id,stock FROM products WHERE id=$1", f.Get("product_id")).Scan(&btc, &xmr, &vendor, &stock)
		if err != nil || stock < 1 {
			fail("Product unavailable", 400)
			return
		}
		if vendor == u.ID {
			fail("You cannot order your own listing", 400)
			return
		}
		total := btc
		if currency == "XMR" {
			total = xmr
		}
		id := randomToken()
		err = tx.QueryRowContext(ctx, `INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES($1,$2,$3,$4,$5) ON CONFLICT(buyer_id,product_id,currency) DO UPDATE SET buyer_id=excluded.buyer_id RETURNING id`, id, u.ID, f.Get("product_id"), currency, total).Scan(&id)
		redirect = "/order?id=" + id
		action = "Saved unfunded order draft"
	case "/messages":
		body := strings.TrimSpace(f.Get("body"))
		if len(body) < 60 || len(body) > 20000 || !strings.HasPrefix(body, "-----BEGIN PGP MESSAGE-----") || !strings.HasSuffix(body, "-----END PGP MESSAGE-----") {
			fail("Encrypt the message locally and paste the complete armored PGP message (up to 20 KB).", 400)
			return
		}
		var recipient string
		err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1", f.Get("recipient")).Scan(&recipient)
		if err != nil {
			fail("Recipient not found", 400)
			return
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO messages(id,sender_id,recipient_id,body) VALUES($1,$2,$3,$4)", randomToken(), u.ID, recipient, body)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), recipient, "New message from "+u.Handle)
		}
		redirect = "/messages?saved=1"
		action = "Sent armored message (contents not verified)"
	case "/notifications":
		_, err = tx.ExecContext(ctx, "UPDATE notifications SET is_read=true WHERE id=$1 AND user_id=$2", f.Get("id"), u.ID)
		redirect = "/notifications"
		action = "Marked notification read"
	case "/listings":
		if u.Role != "vendor" && u.Role != "admin" {
			fail("Vendor access required", 403)
			return
		}
		btc, e1 := parseAmount(f.Get("price_btc"), 8)
		xmr, e2 := parseAmount(f.Get("price_xmr"), 12)
		stock, e3 := strconv.Atoi(f.Get("stock"))
		kind := f.Get("kind")
		title := strings.TrimSpace(f.Get("title"))
		desc := strings.TrimSpace(f.Get("description"))
		cat := f.Get("category")
		region := f.Get("region")
		if e1 != nil || e2 != nil || e3 != nil || stock < 0 || stock > 1000000 || len(title) < 3 || len(title) > 140 || len(desc) > 10000 || (kind != "digital" && kind != "physical") || (cat != "Hardware" && cat != "Digital" && cat != "Services") || len(region) < 2 || len(region) > 80 {
			fail("Invalid listing. Check price precision, stock, category, and required fields.", 400)
			return
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, randomToken(), u.ID, title, desc, cat, region, kind, btc, xmr, stock)
		redirect = "/vendor-dashboard?saved=1"
		action = "Created listing"
	case "/disputes":
		reason := strings.TrimSpace(f.Get("reason"))
		if len(reason) < 20 || len(reason) > 5000 {
			fail("Describe the issue in 20–5000 characters", 400)
			return
		}
		var status string
		err = tx.QueryRowContext(ctx, `SELECT o.status FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)`, f.Get("order_id"), u.ID).Scan(&status)
		if err != nil {
			fail("Order not found", 404)
			return
		}
		if status != "In escrow" && status != "Shipped" && status != "Delivered" {
			fail("Only funded orders in escrow, shipped, or delivered can be disputed. Draft orders contain no funds.", 409)
			return
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,$3)", randomToken(), f.Get("order_id"), reason)
		redirect = "/disputes?saved=1"
		action = "Opened dispute"
	case "/resolve":
		if u.Role != "moderator" && u.Role != "admin" {
			fail("Moderator access required", 403)
			return
		}
		resolution := strings.TrimSpace(f.Get("resolution"))
		if len(resolution) < 20 || len(resolution) > 5000 {
			fail("Provide a decision of 20–5000 characters", 400)
			return
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE disputes SET resolution=$1,status='Decision recorded — settlement pending' WHERE id=$2 AND status='Open'", resolution, f.Get("id"))
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				fail("Open dispute not found", 409)
				return
			}
		}
		redirect = "/moderator?saved=1"
		action = "Recorded dispute decision; no fund transfer"
	case "/admin":
		if u.Role != "admin" {
			fail("Administrator access required", 403)
			return
		}
		switch f.Get("action") {
		case "role":
			role := f.Get("role")
			if role != "buyer" && role != "vendor" && role != "moderator" {
				fail("Choose buyer, vendor, or moderator", 400)
				return
			}
			if f.Get("user_id") == u.ID {
				fail("Cannot change your own administrator role", 400)
				return
			}
			var result sql.Result
			result, err = tx.ExecContext(ctx, "UPDATE users SET role=$1 WHERE id=$2 AND role<>'admin'", role, f.Get("user_id"))
			if err == nil {
				if n, _ := result.RowsAffected(); n != 1 {
					fail("Eligible user not found", 404)
					return
				}
			}
			action = "Changed user role"
		case "settings":
			name := strings.TrimSpace(f.Get("site_name"))
			btc, xmr := f.Get("bitcoin_mode"), f.Get("monero_mode")
			valid := func(s string) bool { return s == "disabled" || s == "local" || s == "external" }
			if name == "" || len(name) > 80 || !valid(btc) || !valid(xmr) {
				fail("Invalid settings", 400)
				return
			}
			for k, v := range map[string]string{"site_name": name, "bitcoin_mode": btc, "monero_mode": xmr} {
				if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); err != nil {
					break
				}
			}
			action = "Saved desired node configuration; operator apply required"
		default:
			fail("Unknown action", 400)
			return
		}
		redirect = "/admin?saved=1"
	default:
		fail("Unknown action", 404)
		return
	}
	if err != nil {
		fail("Unable to save changes", 500)
		return
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", u.ID, action); err != nil {
		fail("Unable to record change", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		fail("Unable to save changes", 500)
		return
	}
	http.Redirect(w, r, redirect, 303)
}
func (a *App) login(ctx context.Context, w http.ResponseWriter, id, old string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	token := randomToken()
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=$1 OR expires<now()", digest(old)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '12 hours')", digest(token), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,'Signed in')", id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.cookie(w, "session", token, 43200)
	return nil
}
