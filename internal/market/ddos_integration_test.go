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
	"sync"
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
// failover) answers the busy 503 with Retry-After, like the query one step earlier, never a 500.
func TestPageLoadConnectionLossIs503(t *testing.T) {
	e := newTestApp(t)
	_, buyer := e.user("lost_buyer", "buyer")
	admin, err := OpenDB(os.Getenv("DATABASE_URL")) // no timeouts: it holds the lock for as long as needed
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, c := range []struct {
		method, path, token, table string
	}{
		{"GET", "/?q=lamp", "", "products"},
		{"POST", "/notifications", buyer, "notifications"},
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
		go func() { done <- e.do(c.method, c.path, c.token, nil) }()
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
	// The pool replaces the lost connections.
	if w := e.do("GET", "/?q=lamp", "", nil); w.Code != 200 {
		t.Fatalf("page after the connections were lost: %d", w.Code)
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
	if c := post("/notifications", buyer, url.Values{}); c != 303 {
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
type slowWallet struct {
	hold    time.Duration
	mu      sync.Mutex
	slow    map[string]bool
	entered chan string
}

func (s *slowWallet) Currency() string   { return "BTC" }
func (s *slowWallet) Network() string    { return "regtest" }
func (s *slowWallet) Confirmations() int { return 1 }
func (s *slowWallet) NewAddress(ctx context.Context, orderID string) (string, error) {
	s.mu.Lock()
	slow := s.slow[orderID]
	s.mu.Unlock()
	if slow {
		s.entered <- orderID
		select {
		case <-time.After(s.hold):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "stub-" + orderID[:8], nil
}
func (s *slowWallet) Incoming(context.Context, []string) ([]Incoming, error) { return nil, nil }
func (s *slowWallet) Send(context.Context, string, int64) (string, error) {
	return "", errors.New("cannot send")
}
func (s *slowWallet) ValidAddress(addr string) bool { return strings.HasPrefix(addr, "stub-") }

// PR #15 review: a payment request holds the buyer, payment-intake and listing rows while the wallet is asked
// for an address (up to rpcTimeout, 10 s). A checkout, a stock edit or a takedown queued behind it waits for
// it instead of failing at the application pool's 3 s lock_timeout.
func TestSlowWalletDoesNotRefuseQueuedListingWriters(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("slow_wallet_admin", "admin")
	wallet := &slowWallet{hold: 4 * time.Second, slow: map[string]bool{}, entered: make(chan string, 4)}
	w.e.A.payments["BTC"] = wallet
	// behind runs the slow payment request for a fresh order of product and, once it is waiting on the wallet,
	// the competing request; it returns both statuses.
	behind := func(product string, compete func() int) (int, int) {
		t.Helper()
		order := w.e.order(w.buyerID, product, "BTC", stateDraft)
		wallet.mu.Lock()
		wallet.slow[order] = true
		wallet.mu.Unlock()
		first := make(chan int, 1)
		go func() { first <- w.post("/orders/pay", w.buyer, form("order_id", order)).Code }()
		select {
		case <-wallet.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the payment request never reached the wallet")
		}
		second := compete()
		return <-first, second
	}
	p := w.e.product(w.vendorID, "physical")
	other := w.e.order(w.otherID, p, "BTC", stateDraft)
	if a, b := behind(p, func() int { return w.post("/orders/pay", w.other, form("order_id", other)).Code }); a != 303 || b != 303 {
		t.Fatalf("two checkouts of one listing, the first waiting on the wallet: %d, %d", a, b)
	}
	q := w.e.product(w.vendorID, "physical")
	if a, b := behind(q, func() int {
		return w.post("/listings/update", w.vendor, inventoryForm(q, map[string]string{"stock": "9", "stock_seen": "4"})).Code
	}); a != 303 || b != 303 {
		t.Fatalf("a stock edit behind a checkout waiting on the wallet: %d, %d", a, b)
	}
	r := w.e.product(w.vendorID, "physical")
	take := withField(takedownForm(t, w.e.body("GET", "/listing-edit?id="+r, admin, nil, 200), "takedown", r), "reason", takedownReason)
	if a, b := behind(r, func() int { return w.post("/admin/listing", admin, take).Code }); a != 303 || b != 303 {
		t.Fatalf("a takedown behind a checkout waiting on the wallet: %d, %d", a, b)
	}
	if w.stock(p) != 3 || w.stock(q) != 9 || !func() bool { archived, _ := takenDown(w.e, r); return archived }() {
		t.Fatalf("after the queued writers: stock p=%d q=%d", w.stock(p), w.stock(q))
	}
}

// PR #15 re-review (NEW-3): while a checkout waits 5 s on a slow wallet, holding the buyer, payment-intake and
// listing rows, the requests that queue for those rows wait for it instead of failing at the 3 s lock_timeout:
// an administrator pausing intake, another buyer cancelling an order of the same listing (its stock comes
// back), administrators suspending the buyer or resetting their second factors, and one demoting the vendor
// (which archives the listing). A change that only notifies the buyer does not wait at all.
func TestSlowWalletDoesNotRefuseStaffOrCancel(t *testing.T) {
	w := newOrderWorld(t)
	// Two administrators: one administrator's confirmed actions queue on that administrator's own row.
	_, admin := w.e.user("slow_cancel_admin", "admin")
	_, admin2 := w.e.user("slow_cancel_admin2", "admin")
	_, admin3 := w.e.user("slow_cancel_admin3", "admin")
	wallet := &slowWallet{hold: 5 * time.Second, slow: map[string]bool{}, entered: make(chan string, 1)}
	w.e.A.payments["BTC"] = wallet
	p := w.e.product(w.vendorID, "physical")
	agExec(w.e, "UPDATE products SET stock=4 WHERE id=$1", p)
	cancelled := w.e.order(w.otherID, p, "BTC", stateAwaitingPayment) // its unit is already out of stock
	shipped := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", statePaid)
	order := w.e.order(w.buyerID, p, "BTC", stateDraft)
	wallet.slow[order] = true
	first := make(chan int, 1)
	go func() { first <- w.post("/orders/pay", w.buyer, form("order_id", order)).Code }()
	select {
	case <-wallet.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the payment request never reached the wallet")
	}
	started := time.Now()
	handle := func(id string) string { return agStr(w.e, "SELECT handle FROM users WHERE id=$1", id) }
	competitors := map[string]func() *httptest.ResponseRecorder{
		"pause payment intake": func() *httptest.ResponseRecorder {
			return w.post("/admin/payment-intake", admin, url.Values{"currency": {"BTC"}, "enabled": {"false"}, "password": {testPassword}})
		},
		"another buyer's cancel": func() *httptest.ResponseRecorder {
			return w.post("/orders/cancel", w.other, form("order_id", cancelled, "from", stateAwaitingPayment))
		},
		"suspend the buyer": func() *httptest.ResponseRecorder {
			return w.post("/admin/suspend", admin2, url.Values{"handle": {handle(w.buyerID)}, "action": {"suspend"}, "password": {testPassword}})
		},
		"reset the buyer's second factors": func() *httptest.ResponseRecorder { // none enrolled: 409 once it waited
			return w.post("/admin/reset-factors", admin3, url.Values{"handle": {handle(w.buyerID)}, "password": {testPassword}})
		},
		// Shipping notifies the buyer: an insert referencing the buyer's row, which the checkout's
		// FOR NO KEY UPDATE lets through at once.
		"ship the buyer's other order": func() *httptest.ResponseRecorder {
			return w.post("/orders/ship", w.vendor, form("order_id", shipped))
		},
		"demote the vendor": func() *httptest.ResponseRecorder {
			return w.post("/admin", admin, url.Values{"action": {"role"}, "handle": {handle(w.vendorID)}, "role": {"buyer"}})
		},
	}
	type result struct {
		name string
		code int
		body string
		took time.Duration
	}
	results := make(chan result, len(competitors))
	for name, run := range competitors {
		go func() {
			r := run()
			results <- result{name, r.Code, r.Body.String(), time.Since(started)}
		}()
	}
	for range competitors {
		r := <-results
		want := 303
		if strings.HasPrefix(r.name, "reset") {
			want = 409
		}
		if r.code != want {
			t.Errorf("%s behind a checkout waiting on the wallet: %d %q after %v", r.name, r.code, r.body, r.took)
		}
		if strings.HasPrefix(r.name, "ship") && r.took > 2*time.Second {
			t.Errorf("%s waited %v for the checkout", r.name, r.took)
		}
	}
	if code := <-first; code != 303 {
		t.Fatalf("the slow checkout: %d", code)
	}
	if t.Failed() {
		return
	}
	if w.state(cancelled) != stateCancelled || w.stock(p) != 4 || w.state(order) != stateAwaitingPayment {
		t.Fatalf("after the queued writers: cancelled order %s, stock %d, checkout %s", w.state(cancelled), w.stock(p), w.state(order))
	}
	if agStr(w.e, "SELECT value FROM settings WHERE key='payment_intake_BTC'") != "false" ||
		agInt(w.e, "SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", w.buyerID) != 1 ||
		agInt(w.e, "SELECT count(*) FROM products WHERE id=$1 AND archived", p) != 1 {
		t.Fatal("a queued staff change did not take effect")
	}
}

// PR #15 re-review (NEW-3): a moderator's refund queues a payout, which share-locks the buyer's row; a checkout
// by that buyer holds the row FOR UPDATE while it waits on the wallet (payAction), here for 5 s. The resolve
// waits for it instead of failing at the 3 s lock_timeout.
func TestResolveRefundWaitsForBuyersCheckout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	dispute, modSess := p.disputedOrder(100000)
	seen := amountSeen(t, p.page("/moderator", modSess))
	tx, err := p.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", p.buyer.ID); err != nil { // as payAction does
		t.Fatal(err)
	}
	go func() { time.Sleep(5 * time.Second); tx.Rollback() }()
	start := time.Now()
	decision := url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"No proof of shipment; refund the buyer."}}
	w := p.do("POST", "/resolve", modSess, payoutConfirmed(decision, seen[0]))
	if w.Code != 303 || time.Since(start) < 4*time.Second {
		t.Fatalf("refund behind the buyer's checkout: %d %q after %v", w.Code, w.Body.String(), time.Since(start))
	}
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-buyer" || amt != 100000 {
		t.Fatalf("payout %s %s %d", st, addr, amt)
	}
}

// The wait covers a wallet as slow as rpcTimeout allows: the application pool's 8 s statement_timeout would
// otherwise end a queued writer's lock wait at 8 s. Here the wallet takes 9 s and a stock edit queued behind
// the checkout still succeeds.
func TestWalletLockWaitOutlastsStatementTimeout(t *testing.T) {
	w := newOrderWorld(t)
	wallet := &slowWallet{hold: 9 * time.Second, slow: map[string]bool{}, entered: make(chan string, 1)}
	w.e.A.payments["BTC"] = wallet
	p := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, p, "BTC", stateDraft)
	wallet.slow[order] = true
	first := make(chan int, 1)
	go func() { first <- w.post("/orders/pay", w.buyer, form("order_id", order)).Code }()
	select {
	case <-wallet.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the payment request never reached the wallet")
	}
	edit := w.post("/listings/update", w.vendor, inventoryForm(p, map[string]string{"stock": "9", "stock_seen": "4"}))
	if code := <-first; code != 303 || edit.Code != 303 || w.stock(p) != 9 {
		t.Fatalf("a stock edit behind a 9 s wallet call: checkout %d, edit %d %q, stock %d", code, edit.Code, edit.Body.String(), w.stock(p))
	}
}

// PR #15 re-review (NEW-3, measured): with the wallet slow (5 s an address), one buyer firing 12 concurrent
// payment requests must not occupy the application pool's 12 connections while the requests queue on the
// buyer's row for up to walletLockWait. Other users' pages and posts stay fast.
func TestOneBuyersPayFloodLeavesPoolForOthers(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("pay_flood_admin", "admin")
	wallet := &slowWallet{hold: 5 * time.Second, slow: map[string]bool{}, entered: make(chan string, 16)}
	w.e.A.payments["BTC"] = wallet
	var orders []string
	for range 12 {
		o := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", stateDraft)
		wallet.slow[o] = true
		orders = append(orders, o)
	}
	type result struct {
		code       int
		retryAfter string
		took       time.Duration
	}
	results := make(chan result, len(orders))
	start := time.Now()
	for _, o := range orders {
		go func() {
			r := w.post("/orders/pay", w.buyer, form("order_id", o))
			results <- result{r.Code, r.Header().Get("Retry-After"), time.Since(start)}
		}()
	}
	select {
	case <-wallet.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no payment request reached the wallet")
	}
	time.Sleep(300 * time.Millisecond) // the rest are queued (or refused) by now
	measure := func(name string, run func() int, want int) {
		t.Helper()
		began := time.Now()
		code := run()
		took := time.Since(began)
		t.Logf("during the flood: %s -> %d in %v", name, code, took)
		if code != want || took > time.Second {
			t.Errorf("%s during one buyer's payment flood: %d after %v", name, code, took)
		}
	}
	measure("another buyer's orders page", func() int { return w.e.do("GET", "/orders", w.other, nil).Code }, 200)
	measure("the catalog", func() int { return w.e.do("GET", "/catalog", w.other, nil).Code }, 200)
	measure("another buyer's post", func() int { return w.e.do("POST", "/notifications", w.other, url.Values{}).Code }, 303)
	measure("an administrator's change", func() int {
		return w.e.do("POST", "/admin", admin, url.Values{"action": {"settings"}, "site_name": {"Flood Market"}}).Code
	}, 303)
	codes := map[int]int{}
	for range orders {
		r := <-results
		codes[r.code]++
		if r.code == 429 && (r.retryAfter != "5" || r.took > time.Second) {
			t.Errorf("a refused extra payment request: Retry-After=%q after %v", r.retryAfter, r.took)
		}
	}
	t.Logf("the buyer's 12 payment requests: %v", codes)
	if codes[303] != 1 || codes[429] != 11 {
		t.Errorf("one buyer's 12 concurrent payment requests: %v, want one served and 11 refused at once", codes)
	}
}

// PR #15 review: the application pool's session timeouts are startup parameters, which a connection pooler
// such as PgBouncer may refuse. The pool is opened and pinged before migrations, so such a database stops
// startup with a class-only error and an untouched schema. A server that refuses one of the parameters stands
// in for the pooler here.
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
