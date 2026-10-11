package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Owner directive 2026-09-26 (DDoS hardening Tier 1): A-176 stateless CAPTCHA, A-177 application-pool
// session timeouts, A-178 admission control.

// schemaWrites is the number of rows inserted, updated or deleted in the test schema, as the statistics
// collector counts them. The application pool's idle connections are closed first: a backend flushes its
// pending counts when it exits, instead of up to ten seconds later.
func schemaWrites(e *testEnv) int64 {
	e.t.Helper()
	e.A.db.SetMaxIdleConns(0)
	e.A.db.SetMaxIdleConns(4)
	time.Sleep(time.Second)
	stats, err := sql.Open("pgx", os.Getenv("DATABASE_URL")) // the test schema's DSN (newTestApp)
	if err != nil {
		e.t.Fatal(err)
	}
	defer stats.Close()
	var n int64
	if err = stats.QueryRow("SELECT coalesce(sum(n_tup_ins+n_tup_upd+n_tup_del),0) FROM pg_stat_user_tables WHERE schemaname=current_schema()").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A-176: cookieless GET /login, /register and /captcha write nothing; each still gets its own challenge,
// bound to the anon cookie it was issued with, and a posted answer is what records the id.
func TestCaptchaIssuanceWritesNothing(t *testing.T) {
	e := newTestApp(t)
	e.user("stateless_buyer", "buyer")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	if agInt(e, "SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND tablename='captchas'") != 0 {
		t.Fatal("the captchas table survived migration 061")
	}
	before := schemaWrites(e)
	seen := map[string]bool{}
	var anon, id string
	for i := range 1000 {
		page := "/login"
		if i%2 == 1 {
			page = "/register"
		}
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, httptest.NewRequest("GET", page, nil)) // no cookie at all
		m := captchaField.FindStringSubmatch(w.Body.String())
		if w.Code != 200 || m == nil || seen[m[1]] {
			t.Fatalf("GET %s #%d: status=%d challenge=%v repeated=%v", page, i, w.Code, m, m != nil && seen[m[1]])
		}
		seen[m[1]] = true
		anon, id = e.cookie(w, "anon"), m[1]
		if i%100 == 0 {
			r := httptest.NewRequest("GET", "/captcha?id="+id, nil)
			r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
			img := httptest.NewRecorder()
			e.A.ServeHTTP(img, r)
			if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("CAPTCHA image for a cookieless view: %d", img.Code)
			}
		}
	}
	if after := schemaWrites(e); after != before {
		t.Fatalf("1000 cookieless GETs changed %d rows", after-before)
	}
	if agInt(e, "SELECT count(*) FROM captcha_spent") != 0 {
		t.Fatal("issuing a challenge recorded it")
	}
	// The measurement sees writes: posting the last challenge's answer records its id.
	w := e.do("POST", "/login", anon, url.Values{"handle": {"stateless_buyer"}, "password": {testPassword}, "captcha_id": {id}, "captcha": {e.A.captchaAnswer(id)}})
	e.check(w, 303)
	if agInt(e, "SELECT count(*) FROM captcha_spent WHERE id=$1", id) != 1 || schemaWrites(e) == before {
		t.Fatal("a posted answer did not record its challenge id")
	}
	// Replayed by the same session, now signed in or not: refused.
	w = e.do("POST", "/login", anon, url.Values{"handle": {"stateless_buyer"}, "password": {testPassword}, "captcha_id": {id}, "captcha": {e.A.captchaAnswer(id)}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "expired or was already used") {
		t.Fatalf("replayed challenge: %d %q", w.Code, w.Body.String())
	}
}

// A-176: an id minted before the upgrade (random, stored in the dropped table) or for another session is
// refused without writing; the spent-id sweep deletes only rows past expiry.
func TestCaptchaForeignIDsAndSweep(t *testing.T) {
	e := newTestApp(t)
	e.user("sweep_buyer", "buyer")
	agExec(e, "UPDATE settings SET value='true' WHERE key='captcha_required'")
	anon := randomToken()
	login := func(id string) *httptest.ResponseRecorder {
		return e.do("POST", "/login", anon, url.Values{"handle": {"sweep_buyer"}, "password": {testPassword}, "captcha_id": {id}, "captcha": {e.A.captchaAnswer(id)}})
	}
	for name, id := range map[string]string{
		"pre-upgrade random id": randomToken(),
		"other session":         e.A.newCaptchaID(digest(randomToken()), time.Now().Add(captchaTTL)),
		"expired":               e.A.newCaptchaID(digest(anon), time.Now().Add(-time.Second)),
	} {
		if w := login(id); w.Code != 400 {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	if agInt(e, "SELECT count(*) FROM captcha_spent") != 0 {
		t.Fatal("a refused id was recorded")
	}
	agExec(e, "INSERT INTO captcha_spent(id,expires) VALUES('old',now()-interval '2 minutes'),('margin',now()-interval '10 seconds'),('live',now()+interval '5 minutes')")
	if err := e.A.sweepCaptchaSpent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := agStr(e, "SELECT string_agg(id,',' ORDER BY id) FROM captcha_spent"); got != "live,margin" {
		t.Fatalf("after the sweep: %q", got)
	}
	// Past expiry on the database clock, an id is refused even though its row was swept.
	id := e.A.newCaptchaID(digest(anon), time.Now().Add(2*time.Second))
	time.Sleep(2100 * time.Millisecond)
	if w := login(id); w.Code != 400 {
		t.Fatalf("id used after its expiry: %d", w.Code)
	}
}

// pgCode returns err's SQLSTATE, or "".
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// A-177: the application pool and the health connection carry the session timeouts; the host commands'
// OpenDB does not.
func TestAppPoolSessionTimeouts(t *testing.T) {
	e := newTestApp(t)
	want := map[string]string{"statement_timeout": "8s", "lock_timeout": "3s", "idle_in_transaction_session_timeout": "15s"}
	plain, err := OpenDB(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	for name, db := range map[string]*sql.DB{"app": e.A.db, "health": e.A.health, "plain": plain} {
		for k, v := range want {
			var got string
			if err := db.QueryRow("SHOW " + k).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if name == "plain" {
				v = "0"
			}
			if got != v {
				t.Errorf("%s pool: %s=%q, want %q", name, k, got, v)
			}
		}
	}
	// A DSN's own setting does not override the application pool's.
	override, err := OpenDB(os.Getenv("DATABASE_URL")+"&statement_timeout=60000", WithAppTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	defer override.Close()
	var got string
	if err := override.QueryRow("SHOW statement_timeout").Scan(&got); err != nil || got != "8s" {
		t.Fatalf("statement_timeout with a DSN override: %q %v", got, err)
	}
}

func TestAppPoolStatementTimeout(t *testing.T) {
	e := newTestApp(t)
	start := time.Now()
	_, err := e.A.db.ExecContext(context.Background(), "SELECT pg_sleep(20)")
	took := time.Since(start)
	if pgCode(err) != "57014" || took > 10*time.Second || took < 7*time.Second {
		t.Fatalf("pg_sleep(20) on the application pool: err=%v (SQLSTATE %q) after %v, want 57014 after about 8s", err, pgCode(err), took)
	}
	if !dbBusy(err) {
		t.Fatal("a statement timeout is not classed as busy (503)")
	}
}

func TestAppPoolLockTimeout(t *testing.T) {
	e := newTestApp(t)
	agExec(e, "INSERT INTO settings(key,value) VALUES('lock_probe','a')")
	holder, err := OpenDB(os.Getenv("DATABASE_URL")) // no timeouts: a migration or host command could hold it
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM settings WHERE key='lock_probe' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = e.A.db.Exec("UPDATE settings SET value='b' WHERE key='lock_probe'")
	took := time.Since(start)
	if pgCode(err) != "55P03" || took > 4*time.Second || took < 2500*time.Millisecond {
		t.Fatalf("competing UPDATE: err=%v (SQLSTATE %q) after %v, want 55P03 after about 3s", err, pgCode(err), took)
	}
	if !dbBusy(err) {
		t.Fatal("a lock timeout is not classed as busy (503)")
	}
}

// QA-178-2: a page load or a post whose pool connection is terminated mid-request (a database restart or
// failover) before Commit answers the busy 503 with Retry-After, like the query one step earlier.
func TestPageLoadConnectionLossIs503(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("lost_buyer", "buyer")
	_, staff := e.user("lost_admin", "admin")
	admin, err := OpenDB(os.Getenv("DATABASE_URL")) // no timeouts: it holds the lock for as long as needed
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, c := range []struct {
		method, path, token, table string
		form                       url.Values
	}{
		{"GET", "/?q=lamp", "", "products", nil},
		{"POST", "/notifications", buyer, "notifications", nil},
		// The action's own statement succeeds; the audit row after it loses the connection, so the whole
		// change rolls back and a retry is safe.
		{"POST", "/admin/captcha", staff, "audit_events", url.Values{"password": {testPassword}, "captcha_required": {"true"}}},
	} {
		// The request's query queues behind an exclusive lock on table; its backend is then terminated.
		tx, err := admin.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("LOCK TABLE " + c.table + " IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- e.do(c.method, c.path, c.token, c.form) }()
		var pid int
		for deadline := time.Now().Add(2 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s %s never queued on %s", c.method, c.path, c.table)
			}
			admin.QueryRow("SELECT coalesce(max(pid),0) FROM pg_locks WHERE relation=$1::regclass AND NOT granted", c.table).Scan(&pid)
		}
		if _, err = admin.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
			t.Fatal(err)
		}
		w := <-done
		tx.Rollback()
		if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.HasPrefix(w.Body.String(), errBusyMsg) {
			t.Errorf("%s %s with its connection terminated: %d Retry-After=%q %q", c.method, c.path, w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	var required string
	if err = admin.QueryRow("SELECT coalesce((SELECT value FROM settings WHERE key='captcha_required'),'')").Scan(&required); err != nil || required == "true" {
		t.Fatalf("setting after the audit row's connection was lost: %q %v", required, err)
	}
	// The pool replaces the lost connections.
	if w := e.do("GET", "/?q=lamp", "", nil); w.Code != 200 {
		t.Fatalf("page after the connections were lost: %d", w.Code)
	}
}

// QA-178-2 review: an OwnTx action commits its own change, and post() then writes the action's audit row in
// autocommit, so a connection lost at that row stays 500, never the "try again" 503: a retry could repeat the
// change. (The OwnTx actions registered today go through confirmedTx, which writes the row inside its own
// transaction; this pins post()'s path for any that does not.)
func TestAuditLostAfterOwnTxCommitIs500(t *testing.T) {
	e := newTestApp(t)
	_, staff := e.user("lost_owntx_admin", "admin")
	withAction(t, "/test/owntx-audit", actionSpec{Roles: []string{"admin"}, OwnTx: true, Run: func(c *actionCtx) (actionResult, error) {
		_, err := c.A.db.ExecContext(c.Ctx(), "INSERT INTO settings(key,value) VALUES('owntx_probe','done')")
		return actionResult{Redirect: "/admin", Audit: "OwnTx probe"}, err
	}})
	admin, err := OpenDB(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	tx, err := admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("LOCK TABLE audit_events IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.do("POST", "/test/owntx-audit", staff, nil) }()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the audit row never queued on audit_events")
		}
		admin.QueryRow("SELECT coalesce(max(pid),0) FROM pg_locks WHERE relation='audit_events'::regclass AND NOT granted").Scan(&pid)
	}
	if _, err = admin.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	w := <-done
	tx.Rollback()
	var n int
	if err = admin.QueryRow("SELECT count(*) FROM settings WHERE key='owntx_probe'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("the OwnTx change was not committed before its audit row: %d %v", n, err)
	}
	if w.Code != 500 || strings.HasPrefix(w.Body.String(), errBusyMsg) {
		t.Fatalf("audit row lost after an OwnTx commit: %d %q", w.Code, w.Body.String())
	}
}

// A-177: migrations run without the application pool's timeouts, so a long one is bounded only by
// MIGRATION_TIMEOUT.
func TestMigrationsRunWithoutSessionTimeouts(t *testing.T) {
	got := map[string]string{}
	defer func(f func(context.Context, *sql.Tx) error) { runMigrations = f }(runMigrations)
	runMigrations = func(ctx context.Context, tx *sql.Tx) error {
		for _, k := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
			var v string
			if err := tx.QueryRowContext(ctx, "SHOW "+k).Scan(&v); err != nil {
				return err
			}
			got[k] = v
		}
		if _, err := tx.ExecContext(ctx, "SELECT pg_sleep(9)"); err != nil { // longer than the app's 8s
			return err
		}
		return migrate(ctx, tx)
	}
	newTestApp(t)
	for k, v := range got {
		if v != "0" {
			t.Errorf("migration ran with %s=%s", k, v)
		}
	}
	if len(got) != 3 {
		t.Fatal("migrations did not run")
	}
}

// A-177: a watcher pass that cannot get a pool connection gives up at its deadline and logs, instead of
// waiting until the pool frees up.

func TestHealthzAndRetryAfterUnderLoad(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("retry_buyer", "buyer")
	var held []*sql.Conn
	for range 12 {
		c, err := e.A.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	start := time.Now()
	w := e.do("GET", "/healthz", "", nil)
	if w.Code != 200 || time.Since(start) > time.Second {
		t.Fatalf("/healthz with the application pool exhausted: %d after %v", w.Code, time.Since(start))
	}
	for _, c := range held {
		c.Close()
	}
	e.A.writeLimits[agStr(e, "SELECT id FROM users WHERE handle='retry_buyer'")] = bucket{Count: 100, Limit: 100, Until: time.Now().Add(time.Minute)}
	w = e.do("POST", "/notifications", buyer, url.Values{})
	if w.Code != 429 || w.Header().Get("Retry-After") != "600" {
		t.Fatalf("write budget spent: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	release := agHoldPasswordWork(t)
	w = e.do("POST", "/login", randomToken(), url.Values{"handle": {"retry_buyer"}, "password": {testPassword}})
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("password checks busy: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	release()
}

// PR #15 review (the blocker): posts that trickle their bodies hold no post slot, so with more of them in
// flight than both post classes hold, a real sign-in and a signed-in change still succeed.
func TestSlowBodyPostsLeaveSignInWorking(t *testing.T) {
	e := newTestApp(t)
	e.user("slow_body_user", "buyer")
	_, buyer := e.user("slow_body_buyer", "buyer")
	srv := httptest.NewServer(e.A)
	defer srv.Close()
	host := srv.Listener.Addr().String()
	n := cap(anonPostWork) + cap(userPostWork) + 8
	for i := range n {
		c, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		path, token := "/login", randomToken()
		if i%2 == 1 {
			path, token = "/notifications", buyer // signed in
		}
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nCookie: session=%s\r\nContent-Length: 4000\r\n\r\ncsrf=%s&handle=", path, host, token, e.A.csrf(token))
	}
	time.Sleep(500 * time.Millisecond) // every one is now waiting for the rest of its body
	if len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatalf("slow-body posts hold post slots: anonymous=%d signed-in=%d", len(anonPostWork), len(userPostWork))
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path, token string, form url.Values) int {
		form.Set("csrf", e.A.csrf(token))
		r, err := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "anon", Value: token})
		if path != "/login" {
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/login", randomToken(), url.Values{"handle": {"slow_body_user"}, "password": {testPassword}}); c != 303 {
		t.Fatalf("POST /login during %d slow-body posts: %d", n, c)
	}
	if c := post("/notifications", buyer, url.Values{formNonceField: {randomToken()[:32]}}); c != 303 {
		t.Fatalf("signed-in POST during %d slow-body posts: %d", n, c)
	}
}

// PR #15 re-review (NEW-2): a post's session lookup has its own small class, so an anonymous flood of pages
// that fills the page class refuses no signed-in or staff post. A lookup that finds its class full waits its
// turn (each holds its slot for one indexed read) for up to postLookupWait, then is refused as busy; a post
// without a session cookie needs no lookup.
func TestPostSessionLookupHasItsOwnClass(t *testing.T) {
	e := newTestApp(t)
	e.user("lookup_user", "buyer")
	_, buyer := e.user("lookup_buyer", "buyer")
	_, admin := e.user("lookup_admin", "admin")
	release := holdSlots(t, pageWork)
	if w := e.do("GET", "/catalog", buyer, nil); w.Code != 503 {
		t.Fatalf("page with the page class full: %d", w.Code)
	}
	e.check(e.do("POST", "/notifications", buyer, url.Values{}), 303)
	e.check(e.do("POST", "/admin", admin, url.Values{"action": {"settings"}, "site_name": {"Lookup Market"}}), 303)
	release()

	defer func(d time.Duration) { postLookupWait = d }(postLookupWait)
	postLookupWait = 3 * time.Second
	release = holdSlots(t, sessionLookupWork)
	result := make(chan int, 1)
	go func() { result <- e.do("POST", "/notifications", buyer, url.Values{}).Code }()
	time.Sleep(300 * time.Millisecond)
	select {
	case code := <-result:
		t.Fatalf("signed-in post answered %d while every lookup slot was held", code)
	default:
	}
	release()
	if code := <-result; code != 303 {
		t.Fatalf("signed-in post once a lookup slot came free: %d", code)
	}

	postLookupWait = 200 * time.Millisecond
	release = holdSlots(t, sessionLookupWork)
	if w := e.do("POST", "/notifications", buyer, url.Values{}); w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("signed-in post with the lookup class held past the wait: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	anon := randomToken()
	r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"handle": {"lookup_user"}, "password": {testPassword}, "csrf": {e.A.csrf(anon)}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "anon", Value: anon})
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 303) // no session cookie: no lookup
	release()
	if len(pageWork) != 0 || len(sessionLookupWork) != 0 || len(anonPostWork) != 0 || len(userPostWork) != 0 {
		t.Fatal("slots leaked")
	}
}

// slowWallet is a test-network wallet that takes hold to answer for the orders in slow (a wallet under load).

func TestAppPoolRefusedBeforeMigrations(t *testing.T) {
	dsn := testSchemaDSN(t)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("APP_SECRET_KEY", "")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("COOKIE_SECURE", "false")
	defer func(v string) { appSessionTimeouts["lock_timeout"] = v }(appSessionTimeouts["lock_timeout"])
	appSessionTimeouts["lock_timeout"] = "not-a-duration"
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("started with a database that refuses the session settings")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refused the application's session settings") || !strings.Contains(msg, "SQLSTATE 22023") || strings.Contains(msg, "not-a-duration") || strings.Contains(msg, "postgres://") {
		t.Fatalf("startup error: %q", msg)
	}
	plain, err := OpenDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	var tables int
	if err := plain.QueryRow("SELECT count(*) FROM pg_tables WHERE schemaname=current_schema()").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("the refused start migrated the schema: %d tables", tables)
	}
}

// With the session timeouts set on the database role instead (the documented PgBouncer set-up), migrations
// still run without them: the migration transaction lifts them itself.
func TestMigrationsLiftRoleLevelTimeouts(t *testing.T) {
	dsn := testSchemaDSN(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("statement_timeout", "1000") // as if set on the role: the plain migration connection carries it
	q.Set("lock_timeout", "1000")
	u.RawQuery = q.Encode()
	got := map[string]string{}
	defer func(f func(context.Context, *sql.Tx) error) { runMigrations = f }(runMigrations)
	runMigrations = func(ctx context.Context, tx *sql.Tx) error {
		for _, k := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"} {
			var v string
			if err := tx.QueryRowContext(ctx, "SHOW "+k).Scan(&v); err != nil {
				return err
			}
			got[k] = v
		}
		if _, err := tx.ExecContext(ctx, "SELECT pg_sleep(1.5)"); err != nil {
			return err
		}
		return migrate(ctx, tx)
	}
	db, err := OpenDB(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	timeouts, err := startupTimeoutsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDatabase(context.Background(), db, timeouts); err != nil {
		t.Fatalf("migration under role-level timeouts: %v", err)
	}
	for k, v := range got {
		if v != "0" {
			t.Errorf("migration ran with %s=%s", k, v)
		}
	}
}
