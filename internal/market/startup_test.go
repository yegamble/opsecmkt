package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-9: startup bounds the database connection, the migration lock wait, the migrations and payment
// provider startup separately; each timeout names its phase and the setting that raises it.

func TestStartupTimeoutsFromEnvironment(t *testing.T) {
	for _, k := range []string{"DATABASE_CONNECT_TIMEOUT", "MIGRATION_LOCK_TIMEOUT", "MIGRATION_TIMEOUT"} {
		t.Setenv(k, "")
	}
	got, err := startupTimeoutsFromEnv()
	if err != nil || got.connect != 15*time.Second || got.lock != 10*time.Minute || got.migrate != 10*time.Minute {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	t.Setenv("DATABASE_CONNECT_TIMEOUT", "30s")
	t.Setenv("MIGRATION_LOCK_TIMEOUT", " 2m ")
	t.Setenv("MIGRATION_TIMEOUT", "1h30m")
	got, err = startupTimeoutsFromEnv()
	if err != nil || got.connect != 30*time.Second || got.lock != 2*time.Minute || got.migrate != 90*time.Minute {
		t.Fatalf("configured = %+v, %v", got, err)
	}
	for _, k := range []string{"DATABASE_CONNECT_TIMEOUT", "MIGRATION_LOCK_TIMEOUT", "MIGRATION_TIMEOUT"} {
		for _, bad := range []string{"10", "abc", "0s", "-5m", "500ms", "25h"} {
			t.Run(k+"="+bad, func(t *testing.T) {
				t.Setenv(k, bad)
				if _, err := startupTimeoutsFromEnv(); err == nil || !strings.Contains(err.Error(), k+" must be a duration") {
					t.Fatalf("%s=%s: err = %v", k, bad, err)
				}
			})
		}
	}
	t.Setenv("DATABASE_CONNECT_TIMEOUT", "11m") // the connection bound stops at 10m
	if _, err := startupTimeoutsFromEnv(); err == nil {
		t.Fatal("DATABASE_CONNECT_TIMEOUT=11m accepted")
	}
}

// An invalid value stops startup before any connection is attempted (no database needed).
func TestNewRefusesInvalidStartupTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("MIGRATION_TIMEOUT", "forever")
	if a, err := New(context.Background(), false); err == nil || !strings.Contains(err.Error(), "MIGRATION_TIMEOUT must be a duration from 1s to 24h") {
		if a != nil {
			a.Close()
		}
		t.Fatalf("New with MIGRATION_TIMEOUT=forever: %v", err)
	}
}

// startupEnv points New at a fresh schema and returns a connection scoped to it for inspection.
func startupEnv(t *testing.T) *sql.DB {
	t.Helper()
	dsn := testSchemaDSN(t)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("COOKIE_SECURE", "false")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// assertNothingApplied fails when the schema holds any table (baseline or migration) after a failed start.
func assertNothingApplied(t *testing.T, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a failed start left %d tables behind (err %v)", n, err)
	}
}

// assertLockReleased checks that the startup lock is free again (the timed-out session did not linger).
func assertLockReleased(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got bool
	if err = conn.QueryRowContext(context.Background(), "SELECT pg_try_advisory_lock(782493)").Scan(&got); err != nil || !got {
		t.Fatalf("startup lock still held after the timed-out start (err %v)", err)
	}
	conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(782493)")
}

// Another session holding the startup lock (an instance migrating the same database) is waited for only
// MIGRATION_LOCK_TIMEOUT; the error names that phase and the setting, and nothing is applied.
func TestStartupLockWaitTimesOutNamingThePhase(t *testing.T) {
	db := startupEnv(t)
	holderDB, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer holderDB.Close()
	holder, err := holderDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err = holder.ExecContext(context.Background(), "SELECT pg_advisory_lock(782493)"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATION_LOCK_TIMEOUT", "1s")
	started := time.Now()
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("New succeeded while another session held the migration lock")
	}
	if !strings.Contains(err.Error(), "waiting for the database migration lock timed out after 1s") || !strings.Contains(err.Error(), "MIGRATION_LOCK_TIMEOUT") {
		t.Fatalf("error does not name the lock phase and setting: %v", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("lock wait took %s with a 1s bound", took)
	}
	assertNothingApplied(t, db)
	if _, err = holder.ExecContext(context.Background(), "SELECT pg_advisory_unlock(782493)"); err != nil {
		t.Fatal(err)
	}
	// Once the other session lets go, a normal start migrates as before.
	t.Setenv("MIGRATION_LOCK_TIMEOUT", "")
	a, err = New(context.Background(), false)
	if err != nil {
		t.Fatalf("start after the lock was released: %v", err)
	}
	a.Close()
}

// A migration outlasting MIGRATION_TIMEOUT fails with a phase-named error, the whole migration transaction
// is rolled back (not even the baseline remains) and the lock is released for the next start.
func TestStartupMigrationTimeoutRollsBackAndNamesThePhase(t *testing.T) {
	db := startupEnv(t)
	saved := runMigrations
	runMigrations = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrate(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "SELECT pg_sleep(60)") // a slow data migration on a large database
		return err
	}
	t.Cleanup(func() { runMigrations = saved })
	t.Setenv("MIGRATION_TIMEOUT", "2s")
	started := time.Now()
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("New succeeded although the migration outlasted MIGRATION_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "database migration timed out after 2s") || !strings.Contains(err.Error(), "MIGRATION_TIMEOUT") {
		t.Fatalf("error does not name the migration phase and setting: %v", err)
	}
	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("migration phase took %s with a 2s bound", took)
	}
	assertLockReleased(t)
	assertNothingApplied(t, db)
	runMigrations = saved
	t.Setenv("MIGRATION_TIMEOUT", "")
	a, err = New(context.Background(), false)
	if err != nil {
		t.Fatalf("normal start after the timed-out one: %v", err)
	}
	defer a.Close()
	list, _ := loadMigrations(migrationFiles)
	var n int
	if err = a.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != len(list) {
		t.Fatalf("applied %d of %d migrations (err %v)", n, len(list), err)
	}
}

// Payment provider startup has its own bound: a slow migration no longer eats into it, and a provider
// that hangs past it fails startup with an error naming the payment phase.
func TestStartupPaymentPhaseTimeoutNamesThePhase(t *testing.T) {
	startupEnv(t)
	saved := paymentStartupTimeout
	paymentStartupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { paymentStartupTimeout = saved })
	withProviderFactory(t, func(ctx context.Context) (PaymentProvider, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("New succeeded with a hanging payment provider")
	}
	if !strings.Contains(err.Error(), "payment provider startup timed out after 200ms") {
		t.Fatalf("error does not name the payment phase: %v", err)
	}
}

// A-141: a failed startup connection names its class (authentication, missing database, unknown host,
// refused) so the operator can act on it, without printing the DSN or the driver's error text (which carries
// the user, database and host).
func TestStartupDatabaseErrorNamesItsClass(t *testing.T) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("DATABASE_CONNECT_TIMEOUT", "10s")
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	user := u.User.Username()
	password := "wrong-pw-" + randomToken()[:12]
	wrongPassword := *u
	wrongPassword.User = url.UserPassword(user, password)
	missing := "missing_db_" + randomToken()[:12]
	missingDB := *u
	missingDB.Path = "/" + missing
	unknownHost := *u
	unknownHost.Host = "no-such-db-host.invalid:5432"
	refused := *u
	refused.Host = "127.0.0.1:1"
	// A trust-authenticated server accepts any password: that case then proves nothing.
	if probe, err := OpenDB(wrongPassword.String()); err == nil {
		perr := probe.PingContext(context.Background())
		probe.Close()
		if perr == nil {
			t.Skip("TEST_DATABASE_URL's server accepts a wrong password (trust authentication)")
		}
	}
	for _, c := range []struct {
		name, dsn, want string
	}{
		{"wrong password", wrongPassword.String(), "authentication failed (SQLSTATE 28P01)"},
		{"unknown database", missingDB.String(), "database missing (SQLSTATE 3D000)"},
		{"unknown host", unknownHost.String(), "host not found"},
		{"refused", refused.String(), "connection refused"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", c.dsn)
			a, err := New(context.Background(), false)
			if a != nil {
				a.Close()
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New: %v, want %q", err, c.want)
			}
			for _, secret := range []string{c.dsn, password, missing, "no-such-db-host", "user=" + user, "127.0.0.1", "failed to connect"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("startup error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestConnectErrorClassTLSRequired(t *testing.T) {
	err := &pgconn.PgError{Severity: "FATAL", Code: "28000", Message: `no pg_hba.conf entry for host "10.0.0.5", user "market", database "market", no encryption`}
	if got := connectErrorClass(fmt.Errorf("connect: %w", err)); !strings.Contains(got, "TLS required") || strings.Contains(got, "10.0.0.5") {
		t.Fatalf("class %q", got)
	}
	err.Message = `pg_hba.conf rejects connection for host "10.0.0.5", user "market", database "market", SSL encryption`
	if got := connectErrorClass(err); !strings.Contains(got, "authentication failed (SQLSTATE 28000)") {
		t.Fatalf("class %q", got)
	}
}
