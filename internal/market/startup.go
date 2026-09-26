package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Startup runs in separately bounded phases so a slow migration on a large database does not share (and
// exhaust) the connection check's budget and crash-loop the server: connecting (DATABASE_CONNECT_TIMEOUT),
// waiting for the migration lock another instance may hold (MIGRATION_LOCK_TIMEOUT), migrating
// (MIGRATION_TIMEOUT) and payment provider startup (paymentStartupTimeout). A timeout names its phase.

// runMigrations is migrate; tests replace it to simulate a slow migration.
var runMigrations = migrate

// paymentStartupTimeout bounds initPayments (provider construction and first checks); tests shorten it.
var paymentStartupTimeout = 15 * time.Second

type startupTimeouts struct{ connect, lock, migrate time.Duration }

// startupTimeoutsFromEnv reads DATABASE_CONNECT_TIMEOUT (1s to 10m; default 15s), MIGRATION_LOCK_TIMEOUT
// and MIGRATION_TIMEOUT (1s to 24h; default 10m each), as Go durations.
func startupTimeoutsFromEnv() (t startupTimeouts, err error) {
	if t.connect, err = durationEnv("DATABASE_CONNECT_TIMEOUT", 15*time.Second, time.Second, 10*time.Minute, "1s to 10m, e.g. 15s"); err != nil {
		return
	}
	if t.lock, err = durationEnv("MIGRATION_LOCK_TIMEOUT", 10*time.Minute, time.Second, 24*time.Hour, "1s to 24h, e.g. 10m"); err != nil {
		return
	}
	t.migrate, err = durationEnv("MIGRATION_TIMEOUT", 10*time.Minute, time.Second, 24*time.Hour, "1s to 24h, e.g. 10m")
	return
}

// durationEnv reads a Go duration from name: blank means def; outside [lo, hi] is refused with a message
// naming the variable and its accepted range.
func durationEnv(name string, def, lo, hi time.Duration, accepted string) (time.Duration, error) {
	s := strings.TrimSpace(os.Getenv(name))
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < lo || d > hi {
		return 0, fmt.Errorf("%s must be a duration from %s", name, accepted)
	}
	return d, nil
}

// timedOut reports whether phase (not the caller's ctx) ran out of time.
func timedOut(parent, phase context.Context) bool {
	return parent.Err() == nil && errors.Is(phase.Err(), context.DeadlineExceeded)
}

// connectErrorClass says why the startup connection failed, for the operator to act on, without the driver's
// error text: pgx's names the user, database and host from DATABASE_URL (A-141).
func connectErrorClass(err error) string {
	var pe *pgconn.PgError
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &pe) && pe.Code == "28000" && strings.HasSuffix(pe.Message, "no encryption"):
		return "TLS required: the server accepts only encrypted connections (SQLSTATE 28000); set sslmode=require or verify-full in DATABASE_URL"
	case errors.As(err, &pe) && (pe.Code == "28P01" || pe.Code == "28000"):
		return "authentication failed (SQLSTATE " + pe.Code + "); check the user and password in DATABASE_URL and the server's pg_hba.conf"
	case errors.As(err, &pe) && pe.Code == "3D000":
		return "database missing (SQLSTATE 3D000); check the database name in DATABASE_URL"
	case errors.As(err, &pe):
		return "refused by the server (SQLSTATE " + pe.Code + ")"
	case errors.As(err, &dns) && !dns.IsTimeout:
		return "host not found; check the host name in DATABASE_URL"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused; check the host and port in DATABASE_URL and that PostgreSQL is running"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		return "timed out; check the host and port in DATABASE_URL and the network path to PostgreSQL"
	}
	return "check DATABASE_URL and service health"
}

// prepareDatabase checks the connection, then applies the baseline and migrations in one transaction under
// the startup advisory lock (shared with first-run setup). A failure or timeout leaves nothing applied.
func prepareDatabase(ctx context.Context, db *sql.DB, t startupTimeouts) error {
	connectCtx, cancel := context.WithTimeout(ctx, t.connect)
	defer cancel()
	if err := db.PingContext(connectCtx); err != nil {
		if timedOut(ctx, connectCtx) {
			return fmt.Errorf("database connection timed out after %s; check DATABASE_URL and service health, or raise DATABASE_CONNECT_TIMEOUT", t.connect)
		}
		return errors.New("database connection failed: " + connectErrorClass(err))
	}
	// The transaction outlives each phase context, so it is bound to ctx; the phases bound its statements.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("database connection failed: " + connectErrorClass(err))
	}
	defer tx.Rollback()
	// On a timeout the driver cancels the statement from a background goroutine, which a process exiting
	// straight after New fails (log.Fatal) never gets to run: the server session would keep migrating, or
	// queue for the lock, while holding it. End the session before returning, so the transaction has rolled
	// back and the lock is free when the next start begins.
	var pid int
	abandon := func() {
		endCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if pid != 0 {
			db.ExecContext(endCtx, "SELECT pg_terminate_backend($1, 5000)", pid)
		}
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, t.lock)
	defer cancelLock()
	if err = tx.QueryRowContext(lockCtx, "SELECT pg_backend_pid()").Scan(&pid); err == nil {
		_, err = tx.ExecContext(lockCtx, "SELECT pg_advisory_xact_lock(782493)")
	}
	if err != nil {
		if timedOut(ctx, lockCtx) {
			abandon()
			return fmt.Errorf("waiting for the database migration lock timed out after %s; another server instance may be migrating or setting up the same database. Nothing was changed. Wait for it, or raise MIGRATION_LOCK_TIMEOUT", t.lock)
		}
		return err
	}
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, t.migrate)
	defer cancelMigrate()
	if err = runMigrations(migrateCtx, tx); err != nil {
		if timedOut(ctx, migrateCtx) {
			abandon()
			return fmt.Errorf("database migration timed out after %s and was rolled back; nothing was changed. Raise MIGRATION_TIMEOUT (a large database can need longer) and start again", t.migrate)
		}
		return fmt.Errorf("database migration failed: %w", err)
	}
	return tx.Commit()
}
