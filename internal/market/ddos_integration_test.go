package market

import (
	"context"
	"database/sql"
	"errors"
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
func TestWatcherPassIsBounded(t *testing.T) {
	e := newTestApp(t)
	e.A.payments["BTC"] = newFakeProvider("BTC", 1)
	defer func(d time.Duration) { watcherPassTimeout = d }(watcherPassTimeout)
	watcherPassTimeout = 500 * time.Millisecond
	logs := captureLog(t)
	var held []*sql.Conn
	for range 12 {
		c, err := e.A.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.A.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "payment watcher:") {
		if time.Now().After(deadline) {
			t.Fatalf("no watcher pass ended while the pool was held; log %q", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "timeout") {
		t.Fatalf("watcher log line: %q", logs.String())
	}
}

// A-178: /healthz answers on its own connection while the application pool is exhausted, and a 429 and a
// busy 503 from the application carry Retry-After.
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
	e.A.limits["write:"+agStr(e, "SELECT id FROM users WHERE handle='retry_buyer'")] = bucket{Count: 100, Limit: 100, Until: time.Now().Add(time.Minute)}
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
