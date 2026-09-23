package market

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// Shared test helpers for every package. Tests run from the repository root (templates, static files).
// DB-backed tests call newTestApp(t); each gets its own PostgreSQL schema and must not use t.Parallel.

func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost // production cost 12 exceeds the request timeout under -race
	if err := os.Chdir("../.."); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

const testSetupToken = "integration-test-setup-token-5f2c9e7a1b4d8036cafe0123456789abcdef"

// testPassword is the password of users created by testEnv.user (hashed at bcrypt.MinCost for speed).
const testPassword = "a-long-test-password"

type testEnv struct {
	t  *testing.T
	A  *App
	DB *sql.DB // A.db, scoped to the test schema
}

// testSchemaDSN creates an isolated schema and returns a DSN whose search_path points at it (dropped on cleanup).
func testSchemaDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "test_" + randomToken()[:16]
	if _, err = db.Exec("CREATE SCHEMA " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec("DROP SCHEMA " + name + " CASCADE"); db.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", name)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

// newTestApp starts a real App (baseline schema + migrations) against a fresh schema. Not installed yet:
// call e.setup() for the real /setup flow or e.user() for fast fixtures.
func newTestApp(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("DATABASE_URL", testSchemaDSN(t))
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("COOKIE_SECURE", "false")
	e := &testEnv{t: t}
	e.restart()
	t.Cleanup(func() { e.A.Close() })
	// P1: fresh installs require the image CAPTCHA on /login and /register; tests opt back in explicitly.
	if _, err := e.DB.Exec("UPDATE settings SET value='false' WHERE key='captcha_required'"); err != nil {
		t.Fatal(err)
	}
	return e
}

// restart closes and re-creates the App against the same schema (persistence and migration re-runs).
func (e *testEnv) restart() {
	e.t.Helper()
	if e.A != nil {
		e.A.Close()
	}
	a, err := New(context.Background(), false)
	if err != nil {
		e.t.Fatal(err)
	}
	e.A, e.DB = a, a.db
}

// do sends a request as session token (CSRF added for POST); extra cookies (e.g. "pending") are appended.
func (e *testEnv) do(method, path, token string, form url.Values, extra ...*http.Cookie) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if method == "POST" {
		form.Set("csrf", e.A.csrf(token))
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: token})
	for _, c := range extra {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	return w
}

func (e *testEnv) check(w *httptest.ResponseRecorder, want int) {
	e.t.Helper()
	if w.Code != want {
		e.t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
	}
}

// cookie returns the value of a Set-Cookie on w, or "" when absent.
func (e *testEnv) cookie(w *httptest.ResponseRecorder, name string) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (e *testEnv) session(w *httptest.ResponseRecorder) string {
	e.t.Helper()
	s := e.cookie(w, "session")
	if s == "" {
		e.t.Fatal("no session")
	}
	return s
}

// setup runs the real /setup flow (at the lowered test bcryptCost) and returns the admin session.
func (e *testEnv) setup() string {
	e.t.Helper()
	w := e.do("POST", "/setup", randomToken(), url.Values{"token": {testSetupToken}, "handle": {"admin_user"}, "password": {"a-long-test-password"}, "site_name": {"Integration Market"}})
	e.check(w, 303)
	return e.session(w)
}

var testHash = sync.OnceValue(func() string {
	h, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(h)
})

// user inserts an account (password testPassword) with a live session, marking the market installed.
func (e *testEnv) user(handle, role string) (id, session string) {
	e.t.Helper()
	id, session = randomToken(), randomToken()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO settings(key,value) VALUES('installed','true') ON CONFLICT DO NOTHING", nil},
		{"INSERT INTO users(id,handle,password_hash,role) VALUES($1,$2,$3,$4)", []any{id, handle, testHash(), role}},
		{"INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", []any{digest(session), id}},
	} {
		if _, err := e.DB.Exec(q.sql, q.args...); err != nil {
			e.t.Fatal(err)
		}
	}
	return id, session
}

// product inserts a listing (kind physical|digital|service) priced 0.001 BTC / 0.5 XMR with stock 5.
func (e *testEnv) product(vendorID, kind string) string {
	e.t.Helper()
	id := randomToken()
	if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,'Test listing','Fixture','Hardware','Worldwide',$3,100000,500000000000,5)`, id, vendorID, kind); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// order inserts an order directly in the given state (bypassing transitions) and returns its id.
func (e *testEnv) order(buyerID, productID, currency, state string) string {
	e.t.Helper()
	id := randomToken()
	if _, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,$4,100000,$5)`, id, buyerID, productID, currency, state); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// loadUser reads a User as ServeHTTP would, for calling transition() directly.
func (e *testEnv) loadUser(id string) *User {
	e.t.Helper()
	u := &User{}
	if err := e.DB.QueryRow("SELECT id,handle,role,pgp,xmpp FROM users WHERE id=$1", id).Scan(&u.ID, &u.Handle, &u.Role, &u.PGP, &u.XMPP); err != nil {
		e.t.Fatal(err)
	}
	return u
}
