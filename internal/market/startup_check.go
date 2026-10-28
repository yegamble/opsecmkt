package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
)

// CheckStartup is serve --check-startup (A-694). A production app container keeps no logs, so a start that New
// refuses shows only as unhealthy or restarting; the operator tools run this instead to show why. It makes New's
// refusals that need no wallet, scanner or listener, in New's order and with New's messages: the configuration
// (configure, the escrow configuration), the connection and its encoding, the serving lock, the recorded
// migrations (refuseUnknownMigrations), the install state, the network class and the retention ledger's presence
// (checkRetentionPresence). It writes one line per check to out and returns the first refusal.
//
// It changes nothing: no migration, no audit row, no setting, no retention file or lock, and it takes no advisory
// lock, so neither a restore gate host command nor an application mistakes it for a serving application. A
// database no server has migrated passes, saying how many migrations startup would apply; one that an application
// is serving passes, saying so. Lines and errors never contain DATABASE_URL or another secret.
func CheckStartup(ctx context.Context, out io.Writer) error {
	say := func(format string, args ...any) { fmt.Fprintf(out, "startup check: "+format+"\n", args...) }
	mode, err := appModeFromEnv()
	if err != nil {
		return err
	}
	a := &App{mode: mode}
	cfg, legacy, timeouts, err := a.configure()
	for _, note := range cfg.notes {
		say("note: %s", note)
	}
	if err != nil {
		return err
	}
	if _, _, _, err = escrowXMRSetup(a.live); err != nil {
		return err
	}
	say("configuration: valid")

	db, err := openAppPool(ctx, os.Getenv("DATABASE_URL"), timeouts.connect)
	if err != nil {
		return err
	}
	defer db.Close()
	a.db = db
	connectCtx, cancel := context.WithTimeout(ctx, timeouts.connect)
	err = requireUTF8Database(connectCtx, db)
	cancel()
	if err != nil {
		return err
	}
	say("database: connected (UTF8)")

	line, err := checkServingLock(ctx, db, timeouts)
	if err != nil {
		return err
	}
	say("%s", line)

	list, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return errors.New("database connection failed: " + connectErrorClass(err))
	}
	defer tx.Rollback()
	if err = refuseUnknownMigrations(ctx, tx, list); err != nil {
		return fmt.Errorf("database migration failed: %w", err) // prepareDatabase's wrapping, so the line is the same
	}
	pending, recorded, err := pendingMigrations(ctx, tx, list)
	if err != nil {
		return err
	}
	if err = tx.Rollback(); err != nil {
		return err
	}
	if pending == 0 {
		say("migrations: schema current at %03d; nothing to apply", recorded)
	} else {
		say("migrations: startup would apply %d migration(s) (recorded schema %03d, this server's %03d) within MIGRATION_TIMEOUT (%s); none were applied, so their own failure or timeout is not checked", pending, recorded, list[len(list)-1].Version, timeouts.migrate)
	}

	// The state checks New makes after migrating, against the schema as it is now. Those that read tables a
	// pending migration creates run only once those tables exist.
	var settings, escrows bool
	if err = db.QueryRowContext(ctx, `SELECT to_regclass('settings') IS NOT NULL, to_regclass('xmr_escrow_wallets') IS NOT NULL
		AND EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('escrows') AND attname='coin' AND NOT attisdropped)`).Scan(&settings, &escrows); err != nil {
		return errors.New("database status query failed: " + connectErrorClass(err))
	}
	if settings {
		err = a.checkInstallState(ctx, legacy)
	} else if a.setupToken == "" {
		err = errSetupTokenRequired // a database no server has migrated is not installed
	}
	if err != nil {
		return err
	}
	if escrows {
		if err = a.checkNetworkClass(ctx); err != nil {
			return err
		}
	}
	if line, err = a.checkRetentionPresence(ctx, cfg.retention, settings); err != nil {
		return err
	}
	say("%s", line)
	say("passed; nothing was changed (wallet RPC, the scanner and the listeners are checked only by a real start)")
	return nil
}

// checkServingLock reads, from pg_locks and without taking a lock, who holds servingLock and the migration lock
// (782493) in this database, and refuses a connection that does not keep one session (errServingPooled), as
// takeServingLock would. A holder is reported, not refused: startup shares the serving lock with a running
// application and waits, within MIGRATION_LOCK_TIMEOUT, for a restore gate host command or another instance's
// migration.
func checkServingLock(ctx context.Context, db *sql.DB, t startupTimeouts) (string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", errors.New("database connection failed: " + connectErrorClass(err))
	}
	defer conn.Close()
	var before, after, serving, gate, migrating int
	if err = conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&before); err == nil {
		err = conn.QueryRowContext(ctx, `SELECT pg_backend_pid(),
			count(*) FILTER (WHERE objid=782499 AND mode='ShareLock'), count(*) FILTER (WHERE objid=782499 AND mode='ExclusiveLock'),
			count(*) FILTER (WHERE objid=782493)
			FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid IN (782493, 782499) AND objsubid=1 AND granted
			AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&after, &serving, &gate, &migrating)
	}
	if err != nil {
		return "", errors.New("database status query failed: " + connectErrorClass(err))
	}
	if after != before {
		return "", errServingPooled
	}
	switch {
	case gate > 0:
		return fmt.Sprintf("serving lock: a restore gate host command holds it; startup would wait for it to finish, at most MIGRATION_LOCK_TIMEOUT (%s)", t.lock), nil
	case migrating > 0:
		return fmt.Sprintf("serving lock: free; another instance holds the migration lock (migrating or setting up); startup would wait at most MIGRATION_LOCK_TIMEOUT (%s)", t.lock), nil
	case serving > 0:
		return fmt.Sprintf("serving lock: held by %d session(s): an application is already serving this database (or starting); this is not a refusal", serving), nil
	}
	return "serving lock: free (no application is serving this database)", nil
}
