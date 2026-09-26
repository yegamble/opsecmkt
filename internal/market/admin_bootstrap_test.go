package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// BootstrapAdmin is first-run setup from the host (cmd/server -bootstrap-admin): the same transaction as
// POST /setup, with no setup token and no HTTP path (D-26).

const bootstrapPassword = "operator-chosen-password"

// notInstalled fails when anything first-run setup writes is present.
func notInstalled(t *testing.T, e *testEnv, when string) {
	t.Helper()
	if agInt(e, "SELECT count(*) FROM users") != 0 || agInt(e, "SELECT count(*) FROM settings WHERE key='installed'") != 0 ||
		agStr(e, "SELECT value FROM settings WHERE key='site_name'") != "OPSMKT" || agInt(e, "SELECT count(*) FROM audit_events") != 0 {
		t.Fatalf("%s: refused bootstrap left a partial installation", when)
	}
}

func TestBootstrapAdminCreatesFirstAdmin(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	if err := BootstrapAdmin(ctx, e.DB, "first_admin", bootstrapPassword, "  Host Market  "); err != nil {
		t.Fatal(err)
	}
	var id, role, hash string
	if err := e.DB.QueryRow("SELECT id,role,password_hash FROM users WHERE handle='first_admin'").Scan(&id, &role, &hash); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || agInt(e, "SELECT count(*) FROM users") != 1 {
		t.Fatalf("role=%q users=%d", role, agInt(e, "SELECT count(*) FROM users"))
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(bootstrapPassword)) != nil {
		t.Fatal("stored hash does not match the password")
	}
	if agStr(e, "SELECT value FROM settings WHERE key='installed'") != "true" || agStr(e, "SELECT value FROM settings WHERE key='site_name'") != "Host Market" {
		t.Fatal("installed or site_name not recorded (site name is trimmed like /setup)")
	}
	if !e.auditExact(id, "Account created: admin") || agInt(e, "SELECT count(*) FROM audit_events") != 1 {
		t.Fatal("setup audit row missing or extra rows written")
	}
	// No session is created on the host; the administrator signs in through the site.
	if agInt(e, "SELECT count(*) FROM sessions") != 0 {
		t.Fatal("bootstrap created a session")
	}
	// The running site sees the installation at once: /setup is gone, even with the right token.
	e.check(e.do("GET", "/setup", randomToken(), nil), 403)
	agExpect(e, "/setup", randomToken(), url.Values{"token": {testSetupToken}, "handle": {"second_admin"}, "password": {testPassword}}, 403, "Setup unavailable")
	if code, loc := loginStatus(e, "first_admin", bootstrapPassword); code != 303 || loc != "/" {
		t.Fatalf("sign-in with the bootstrapped password: %d %q", code, loc)
	}

	// A blank marketplace name gets /setup's default.
	e2 := newTestApp(t)
	if err := BootstrapAdmin(ctx, e2.DB, "first_admin", bootstrapPassword, " "); err != nil {
		t.Fatal(err)
	}
	if agStr(e2, "SELECT value FROM settings WHERE key='site_name'") != "OPSMKT" {
		t.Fatal("blank site name did not default to OPSMKT")
	}
}

func TestBootstrapAdminRefusesWhenInstalled(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, e *testEnv, admin, site string, audits int) {
		t.Helper()
		err := BootstrapAdmin(ctx, e.DB, "late_admin", bootstrapPassword, "Late Market")
		if err == nil || !strings.Contains(err.Error(), "already set up") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("bootstrap on an installed market: %v", err)
		}
		if agInt(e, "SELECT count(*) FROM users") != 1 || agStr(e, "SELECT handle FROM users WHERE role='admin'") != admin ||
			agStr(e, "SELECT value FROM settings WHERE key='site_name'") != site || agInt(e, "SELECT count(*) FROM audit_events") != audits {
			t.Fatal("refused bootstrap changed the installation")
		}
	}
	t.Run("after /setup", func(t *testing.T) {
		e := newTestApp(t)
		e.setup()
		check(t, e, "admin_user", "Integration Market", agInt(e, "SELECT count(*) FROM audit_events"))
	})
	t.Run("after bootstrap", func(t *testing.T) {
		e := newTestApp(t)
		if err := BootstrapAdmin(ctx, e.DB, "first_admin", bootstrapPassword, "First Market"); err != nil {
			t.Fatal(err)
		}
		check(t, e, "first_admin", "First Market", 1)
	})
}

// One advisory lock (782493) serialises POST /setup and host bootstraps; whichever commits first installs the
// market and every other attempt is refused with nothing written.
func TestBootstrapAdminRacingSetupCreatesOneAdmin(t *testing.T) {
	ctx := context.Background()
	// Every request fits in the bcrypt gate (a /setup holds its slot for the whole action), so all race for the lock.
	n := cap(passwordWork)
	for round := range 3 {
		for _, mode := range []string{"mixed", "bootstrap only"} {
			t.Run(fmt.Sprintf("%s %d", mode, round), func(t *testing.T) {
				e := newTestApp(t)
				viaSetup := func(i int) bool { return mode == "mixed" && i%2 == 0 }
				rs := agRace(n, func(i int) (int, string) {
					handle, site := fmt.Sprintf("admin_%d", i), fmt.Sprintf("Market %d", i)
					if viaSetup(i) {
						w := e.do("POST", "/setup", randomToken(), url.Values{"token": {testSetupToken}, "handle": {handle}, "password": {testPassword}, "site_name": {site}})
						return w.Code, w.Body.String()
					}
					if err := BootstrapAdmin(ctx, e.DB, handle, bootstrapPassword, site); err != nil {
						if !strings.Contains(err.Error(), "already set up") {
							return 500, err.Error()
						}
						return 403, err.Error()
					}
					return 303, ""
				})
				if got := agTally(t, rs, 303, 403); got[303] != 1 {
					t.Fatalf("%d concurrent setups succeeded", got[303])
				}
				winner := -1
				for i, r := range rs {
					if r.code == 303 {
						winner = i
					}
				}
				if agInt(e, "SELECT count(*) FROM users") != 1 || agStr(e, "SELECT handle FROM users WHERE role='admin'") != fmt.Sprintf("admin_%d", winner) ||
					agStr(e, "SELECT value FROM settings WHERE key='site_name'") != fmt.Sprintf("Market %d", winner) ||
					agInt(e, "SELECT count(*) FROM settings WHERE key='installed'") != 1 ||
					agInt(e, "SELECT count(*) FROM audit_events WHERE action LIKE 'Account created%'") != 1 ||
					agInt(e, "SELECT count(*) FROM audit_events WHERE action='Account created: admin' AND user_id=(SELECT id FROM users)") != 1 {
					t.Fatalf("installation state does not belong to the single winner %d: users=%d admin=%q site=%q audits=%q",
						winner, agInt(e, "SELECT count(*) FROM users"), agStr(e, "SELECT string_agg(handle,',') FROM users"),
						agStr(e, "SELECT value FROM settings WHERE key='site_name'"), agStr(e, "SELECT string_agg(action,'|') FROM audit_events"))
				}
			})
		}
	}
}

func TestBootstrapAdminRejectsBadPasswordAndHandle(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	for _, tc := range []struct{ handle, password, site, msg string }{
		{"first_admin", "short-pass1", "", "12–72 bytes"},
		{"first_admin", strings.Repeat("x", 73), "", "12–72 bytes"},
		{"first_admin", "", "", "12–72 bytes"},
		{"ab", bootstrapPassword, "", "3–32 character handle"},
		{strings.Repeat("a", 33), bootstrapPassword, "", "3–32 character handle"},
		{"bad-handle", bootstrapPassword, "", "3–32 character handle"},
		{"has space", bootstrapPassword, "", "3–32 character handle"},
		{" first_admin", bootstrapPassword, "", "3–32 character handle"}, // exact, as the operator will sign in with it
		{"", bootstrapPassword, "", "3–32 character handle"},
		{"first_admin", bootstrapPassword, strings.Repeat("n", 81), "at most 80 bytes"},
	} {
		err := BootstrapAdmin(ctx, e.DB, tc.handle, tc.password, tc.site)
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("BootstrapAdmin(%q, %d bytes, %d-byte site) = %v, want %q", tc.handle, len(tc.password), len(tc.site), err, tc.msg)
		}
		if tc.password != "" && strings.Contains(err.Error(), tc.password) {
			t.Fatalf("error repeats the password: %v", err)
		}
		notInstalled(t, e, tc.msg)
		// CheckBootstrapInput refuses the same handle and site-name mistakes before any password is read.
		if !strings.Contains(tc.msg, "12–72") {
			if err := CheckBootstrapInput(tc.handle, tc.site); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("CheckBootstrapInput(%q, %d-byte site) = %v", tc.handle, len(tc.site), err)
			}
		}
	}
	if err := CheckBootstrapInput("first_admin", strings.Repeat("n", 80)); err != nil {
		t.Fatalf("valid input refused: %v", err)
	}

	// A database upgraded by a newer release is refused, like server startup and -reset-admin-password.
	agExec(e, "INSERT INTO schema_migrations(version,name) VALUES(999,'from_the_future')")
	if err := BootstrapAdmin(ctx, e.DB, "first_admin", bootstrapPassword, ""); err == nil || !strings.Contains(err.Error(), "999_from_the_future") {
		t.Fatalf("unknown migration not refused: %v", err)
	}
	notInstalled(t, e, "unknown migration")
	agExec(e, "DELETE FROM schema_migrations WHERE version=999")

	// An 80-byte name is accepted, as by /setup.
	if err := BootstrapAdmin(ctx, e.DB, "first_admin", bootstrapPassword, strings.Repeat("n", 80)); err != nil {
		t.Fatal(err)
	}
}

// A connection lost after the tool's first check (the pool reconnects) is reported by class: pgconn's own text
// names the DATABASE_URL user and database.
func TestBootstrapAdminConnectErrorIsClassOnly(t *testing.T) {
	cause := fmt.Errorf("begin: %w", &pgconn.ConnectError{Config: &pgconn.Config{User: "dsn_user_name", Database: "dsn_db_name"}})
	err := bootstrapFailed(context.Background(), cause)
	if err == nil || !strings.Contains(err.Error(), "database connection failed") || !strings.Contains(err.Error(), "nothing was changed") ||
		strings.Contains(err.Error(), "dsn_user_name") || strings.Contains(err.Error(), "dsn_db_name") {
		t.Fatalf("connect error: %v", err)
	}
}

// A database the server has never set up is refused with a clear message instead of a relation error.
func TestBootstrapAdminNeedsInstalledSchema(t *testing.T) {
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = BootstrapAdmin(context.Background(), db, "first_admin", bootstrapPassword, "")
	if err == nil || !strings.Contains(err.Error(), "start the server") {
		t.Fatalf("empty database: %v", err)
	}
}
