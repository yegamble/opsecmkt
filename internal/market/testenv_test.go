package market

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
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
// DB-backed tests call newTestApp(t); each gets its own PostgreSQL database (or, without CREATEDB, its own
// schema) and must not use t.Parallel.

func TestMain(m *testing.M) {
	bcryptCost = bcrypt.MinCost // production cost 12 exceeds the request timeout under -race
	if err := os.Chdir("../.."); err != nil {
		panic(err)
	}
	code := m.Run()
	dropTestTemplate()
	os.Exit(code)
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

// testTemplate is a database holding exactly the baseline and migrations, built once per test binary by
// migrate (the code New runs) and copied for each newTestApp: a copy takes tens of milliseconds where
// migrating a fresh schema takes hundreds. Nothing environment-dependent goes into it; New still runs in full
// against each copy, under that test's environment. name is "" when the server refused CREATE DATABASE (no
// CREATEDB privilege) or the build failed: newTestApp then migrates a fresh schema, as before.
var testTemplate struct {
	once  sync.Once
	admin *sql.DB  // TEST_DATABASE_URL's database, for CREATE and DROP DATABASE
	base  *url.URL // TEST_DATABASE_URL
	name  string
}

// buildTestTemplate creates and migrates the template. Its name carries the process id and a random suffix:
// cmd/server and other agents' test processes share the server, and a run that is killed leaves a template
// no later run uses (drop leftover opsecmkt_tmpl_* and opsecmkt_copy_* databases that have no connections). It
// is created from template0, so it starts as empty as testSchemaDSN's schema whatever template1 holds.
func buildTestTemplate(dsn string) {
	base, err := url.Parse(dsn)
	if err != nil {
		return // newTestApp's schema path reports the unusable URL
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return
	}
	name := fmt.Sprintf("opsecmkt_tmpl_%d_%s", os.Getpid(), randomToken()[:8])
	if _, err = admin.Exec("CREATE DATABASE " + name + " ENCODING 'UTF8' TEMPLATE template0"); err != nil {
		admin.Close()
		fmt.Fprintf(os.Stderr, "internal/market tests: no template database (%v); each test migrates a schema of its own in TEST_DATABASE_URL's database instead (slower)\n", err)
		return
	}
	// The migration connection is opened as New opens its own (OpenDB: TimeZone pinned, no session timeouts).
	// migrate is called directly, not through runMigrations: the test that builds the template may have
	// replaced runMigrations (TestMigrationsRunWithoutSessionTimeouts). Messages go to stderr, not the log
	// package, which a test may be capturing.
	db, err := OpenDB(testDatabaseURL(base, name))
	if err == nil {
		var tx *sql.Tx
		if tx, err = db.Begin(); err == nil {
			if err = migrate(context.Background(), tx); err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		db.Close()
	}
	if err != nil {
		admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
		fmt.Fprintf(os.Stderr, "internal/market tests: template database not migrated (%v); each test migrates a schema of its own instead\n", err)
		return
	}
	testTemplate.admin, testTemplate.base, testTemplate.name = admin, base, name
}

// dropTestTemplate removes the template when the test binary ends (TestMain).
func dropTestTemplate() {
	if testTemplate.name == "" {
		return
	}
	if _, err := testTemplate.admin.Exec("DROP DATABASE " + testTemplate.name + " WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "internal/market tests: could not drop template database %s: %v\n", testTemplate.name, err)
	}
	testTemplate.admin.Close()
}

// testDatabaseURL is base pointing at database name, with search_path set to its public schema: the single
// schema a test's DSN names, as testSchemaDSN's does (tests append "&..." to it and read search_path from it).
func testDatabaseURL(base *url.URL, name string) string {
	u := *base
	u.Path = "/" + name
	q := u.Query()
	q.Set("search_path", "public")
	u.RawQuery = q.Encode()
	return u.String()
}

// testMigratedDSN returns a DSN for a fresh copy of the migrated template, dropped on cleanup (after the
// test's App, whose Close is registered later, has closed); or a fresh empty schema when there is no template.
func testMigratedDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	testTemplate.once.Do(func() { buildTestTemplate(dsn) })
	if testTemplate.name == "" {
		return testSchemaDSN(t)
	}
	// PostgreSQL 15 and later copy with STRATEGY WAL_LOG by default, the faster strategy for a small template.
	name := "opsecmkt_copy_" + randomToken()[:16]
	if _, err := testTemplate.admin.Exec("CREATE DATABASE " + name + " TEMPLATE " + testTemplate.name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := testTemplate.admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			t.Logf("could not drop test database %s: %v", name, err)
		}
	})
	return testDatabaseURL(testTemplate.base, name)
}

// newTestApp starts a real App against a fresh database holding the baseline schema and migrations (New
// re-runs them, finding nothing to apply). Not installed yet: call e.setup() for the real /setup flow or
// e.user() for fast fixtures.
func newTestApp(t *testing.T) *testEnv {
	t.Helper()
	e := startTestApp(t, testMigratedDSN(t))
	// Shared commerce fixtures exercise unfunded drafts even when no escrow node is
	// configured. A real fresh install still defaults to escrow; newFreshTestApp
	// leaves those migration settings untouched. Escrow fixtures configure their
	// nodes before startup and retain escrow intake for checkout coverage.
	if !e.A.escrowConfigured && e.A.escrowXMR == nil {
		if _, err := e.DB.Exec("UPDATE settings SET value=$1 WHERE key IN ($2,$3)", intakePaused, paymentIntakeKey("XMR"), paymentIntakeKey("XMR")); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// newFreshTestApp is newTestApp on an empty schema, where New itself applies the baseline and every migration.
func newFreshTestApp(t *testing.T) *testEnv {
	t.Helper()
	return startTestApp(t, testSchemaDSN(t))
}

func startTestApp(t *testing.T, dsn string) *testEnv {
	t.Helper()
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("APP_SECRET_KEY", "") // legacy key root: every DB-backed test also exercises the pre-split derivation
	t.Setenv("COMPOSE_FILE", "")
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
	} else {
		form = maps.Clone(form) // each call represents a newly rendered form unless it supplies its nonce
	}
	if method == "POST" {
		form.Set("csrf", e.A.csrf(token))
		if spec, ok := actions[path]; ok && (spec.FormNonce || spec.ConfirmedFormNonce || spec.OwnFormNonce) && !form.Has(formNonceField) {
			form.Set(formNonceField, formNonceFor(path))
		}
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

// keylessUser inserts an account without a PGP key or authentication proof. Use it for
// enrollment, browse-only login and access-refusal tests.
func (e *testEnv) keylessUser(handle, role string) (id, session string) {
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
	res, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,listing_title,listing_description,listing_kind)
		SELECT $1,$2,p.id,$3,100000,$4,
		CASE WHEN $4='draft' THEN NULL ELSE p.title END,
		CASE WHEN $4='draft' THEN NULL ELSE p.description END,
		CASE WHEN $4='draft' THEN NULL ELSE p.kind END FROM products p WHERE p.id=$5`, id, buyerID, currency, state, productID)
	if err != nil {
		e.t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		e.t.Fatalf("insert order: %d rows: %v", n, err)
	}
	return id
}

// loadUser reads a User as ServeHTTP would, for calling transition() directly.
func (e *testEnv) loadUser(id string) *User {
	e.t.Helper()
	u := &User{}
	if err := e.DB.QueryRow("SELECT id,handle,role,pgp FROM users WHERE id=$1", id).Scan(&u.ID, &u.Handle, &u.Role, &u.PGP); err != nil {
		e.t.Fatal(err)
	}
	return u
}

// accountPGPFixture is a throwaway public key shared by ordinary account fixtures. Its
// private half never leaves testPGPKey and is not needed for fixture insertion. Tests
// of decryption use their own private key and the real challenge endpoints.
var accountPGPFixture struct {
	once   sync.Once
	public string
}

// user represents an account that already completed PGP authentication in this
// session. Requests still run the production authorization checks. Tests of missing
// keys or proof use keylessUser, and changing a key requires explicit reauthentication.
func (e *testEnv) user(handle, role string) (id, session string) {
	e.t.Helper()
	id, session = e.keylessUser(handle, role)
	accountPGPFixture.once.Do(func() {
		_, accountPGPFixture.public = testPGPKey(e.t, "commerce-fixture")
	})
	e.pgpSession(session, accountPGPFixture.public)
	return id, session
}

// pgpSession explicitly seeds a saved public key and this session's access proof.
// It is fixture setup, not a request helper; tests of authentication must submit the
// actual challenge rather than use this method.
func (e *testEnv) pgpSession(token, public string) {
	e.t.Helper()
	_, fingerprint, canonical, err := parseCanonicalPublicKey(public)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.DB.Exec(`UPDATE users SET pgp=$2,pgp_fingerprint=$3
		WHERE id=(SELECT user_id FROM sessions WHERE token_hash=$1)`, digest(token), canonical, fingerprint)
	if err != nil {
		e.t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		e.t.Fatalf("seed PGP account: %d rows: %v", n, err)
	}
	e.pgpSessionKey(token)
}

// pgpSessionKey explicitly reauthenticates a fixture session after a test changed
// the saved key directly. It never changes that key or account-level ownership state.
func (e *testEnv) pgpSessionKey(token string) {
	e.t.Helper()
	var public string
	if err := e.DB.QueryRow(`SELECT u.pgp FROM users u JOIN sessions s ON s.user_id=u.id
		WHERE s.token_hash=$1`, digest(token)).Scan(&public); err != nil {
		e.t.Fatal(err)
	}
	_, _, canonical, err := parseCanonicalPublicKey(public)
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.DB.Exec("UPDATE sessions SET pgp_access_key=$2 WHERE token_hash=$1", digest(token), digest(canonical))
	if err != nil {
		e.t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		e.t.Fatalf("seed PGP session: %d rows: %v", n, err)
	}
}
