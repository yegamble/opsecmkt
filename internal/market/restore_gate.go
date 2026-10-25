package market

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"time"
)

// The restore gate from the host (A-505). After a restore from backup the restore gate (settings
// payments_recovery_required) pauses Monero escrow checkout, settlement building and relay until the operator
// asserts that the restored records were reconciled with the wallets (docs/testnet-runbook.md). That assertion stays
// on the host, never a web action (A-29, D-5): -set-restore-gate applies the protection of UPGRADING.md section 6 to a
// database restored without scripts/restore.sh, and -clear-restore-gate HANDLE clears the gate in the name of an
// administrator, with an audit row. Both refuse while an application is serving the database: every serving App holds
// servingLock (shared) for as long as it runs, and these commands take it exclusively for their transaction.

// servingLock is the session advisory lock every serving App holds, shared, from before its migrations until Close
// (holdServingLock). The restore gate host commands take it exclusively with pg_try_advisory_xact_lock and refuse
// when they cannot, so they never change the gate under a running or starting application. Like the other advisory
// locks (782493 to 782498) it belongs to the database, not the schema.
const servingLock = 782499

// servingLockHeld asks whether this session holds servingLock (a single bigint key is classid 0, objid the key,
// objsubid 1 in pg_locks; 782499 is servingLock, written out so the query is fixed text).
const servingLockHeld = "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND classid=0 AND objid=782499 AND objsubid=1 AND granted)"

// errAppServing is the refusal while an application serves (or is starting against) the database; lockRestoreGate
// wraps it with how many sessions hold the serving lock and when each started.
var errAppServing = errors.New("the application is running against this database (or is starting)")

// errServingPooled refuses a start whose serving connection does not keep one database session: a connection pooler
// in transaction mode hands each statement to any server session, which keeps neither the lock nor its settings.
var errServingPooled = errors.New("the database connection does not keep one session (a connection pooler in transaction mode?), " +
	"so the serving lock cannot be held; connect DATABASE_URL to PostgreSQL directly or to a session-mode endpoint (docs/operator-guide.md, Connection poolers); nothing was changed")

// servingKeepEvery is how often keepServingLock checks the serving connection.
const servingKeepEvery = 10 * time.Second

// holdServingLock opens one connection of its own and takes servingLock shared on it, waiting at most wait for a
// restore gate host command to finish. The connection stays open, holding the lock, until closeDB.
func (a *App) holdServingLock(ctx context.Context, dsn string, wait time.Duration) error {
	db, err := OpenDB(dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	conn, err := takeServingLock(ctx, db)
	if err != nil {
		db.Close()
		switch {
		case errors.Is(err, errServingPooled):
			return err
		case ctx.Err() != nil:
			return fmt.Errorf("waiting for a restore gate host command to finish timed out after %s; nothing was changed. Wait for it, or raise MIGRATION_LOCK_TIMEOUT", wait)
		}
		return errors.New("database connection failed: " + connectErrorClass(err))
	}
	a.servingDB, a.serving = db, conn
	return nil
}

// takeServingLock takes servingLock shared on a new connection from db. The session first turns off the statement,
// lock, idle-in-transaction and (PostgreSQL 14 and later) idle-session limits a role or database may set, as the
// migration connection does, so it waits for a host command as long as ctx allows and is never ended for being idle,
// and names itself opsecmkt-serving in pg_stat_activity. The session's process ID is read before and after taking the
// lock: a difference means a pooler is handing statements to different sessions (errServingPooled).
func takeServingLock(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	discard := func(err error) (*sql.Conn, error) {
		conn.Raw(func(any) error { return driver.ErrBadConn }) // a cancelled wait may leave the session holding it
		conn.Close()
		return nil, err
	}
	var version, before, after int
	if err = conn.QueryRowContext(ctx, `SELECT set_config('statement_timeout','0',false), set_config('lock_timeout','0',false),
		set_config('idle_in_transaction_session_timeout','0',false), set_config('application_name','opsecmkt-serving',false),
		current_setting('server_version_num')::int, pg_backend_pid()`).Scan(new(string), new(string), new(string), new(string), &version, &before); err != nil {
		return discard(err)
	}
	if version >= 140000 {
		if _, err = conn.ExecContext(ctx, "SELECT set_config('idle_session_timeout','0',false)"); err != nil {
			return discard(err)
		}
	}
	if _, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock_shared($1)", servingLock); err != nil {
		return discard(err)
	}
	if err = conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&after); err != nil {
		return discard(err)
	}
	if after != before {
		return discard(errServingPooled)
	}
	return conn, nil
}

// keepServingLock checks every servingKeepEvery that the serving connection still holds servingLock (a database
// restart or a terminated session ends the session and with it the lock) and takes it again on a new connection if
// not. It narrows, but cannot close, the window in which a host command could run beside this application.
func (a *App) keepServingLock(ctx context.Context) {
	t := time.NewTicker(servingKeepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.refreshServingLock(ctx)
	}
}

// refreshServingLock is one keepServingLock check.
func (a *App) refreshServingLock(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	held := false
	if a.serving != nil {
		if err := a.serving.QueryRowContext(ctx, servingLockHeld).Scan(&held); err != nil {
			if ctx.Err() != nil {
				return // shutting down: Close releases the connection
			}
			held = false
		}
		if held {
			return
		}
		a.serving.Raw(func(any) error { return driver.ErrBadConn })
		a.serving.Close()
		a.serving = nil
	}
	conn, err := takeServingLock(ctx, a.servingDB)
	if err != nil {
		log.Printf("serving lock: not held (%s); restore gate host commands cannot see this application until it is taken again", errorCause(err))
		return
	}
	a.serving = conn
	log.Print("serving lock: taken again after its database session ended")
}

// lockRestoreGate starts a restore gate host command's transaction: it refuses while an application holds
// servingLock, naming how many sessions hold it and when each started (never an address), then takes the startup
// lock and refuses a database this binary does not know (lockKnownSchema).
func lockRestoreGate(ctx context.Context, tx *sql.Tx) error {
	var free bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", servingLock).Scan(&free); err != nil {
		return err
	}
	if !free {
		var n int
		var started sql.NullString
		if err := tx.QueryRowContext(ctx, servingHolders).Scan(&n, &started); err != nil {
			return err
		}
		if !started.Valid {
			started.String = "unknown"
		}
		return fmt.Errorf("%w: %d database session(s) hold the serving lock (session start, UTC: %s); stop the application first "+
			"(docker compose stop app, or opsecmkt deploy down --dir DIR, or stop opsecmkt serve in its service manager) and run this again; "+
			"if no application runs, see \"Clear the gate\" in docs/testnet-runbook.md for finding a stale holder; nothing was changed", errAppServing, n, started.String)
	}
	return lockKnownSchema(ctx, tx)
}

// servingHolders counts the sessions holding servingLock in this database and lists their start times (pg_locks
// joined to pg_stat_activity; the same lookup docs/testnet-runbook.md gives the operator).
const servingHolders = "SELECT count(*), string_agg(to_char(a.backend_start AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), ', ' ORDER BY a.backend_start) " +
	"FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid WHERE l.locktype='advisory' AND l.classid=0 AND l.objid=782499 " +
	"AND l.objsubid=1 AND l.granted AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database())"

// SetRestoreGate is the host command -set-restore-gate: the protection of UPGRADING.md section 6, for a database
// restored any way other than scripts/restore.sh, applied as scripts/restore.sh applies it. In one transaction it sets
// the restore gate (payments_recovery_required), retention_restore_required, escrow_xmr_release_recheck and
// xmr_escrow_sweep_required; holds every built or relayed Monero escrow settlement (restoredSettlementHold), retires
// every free escrow account and moves every active or sealed escrow wallet to closing (when the database has
// xmr_settlements.send_error, migration 085); closes the vendor bond wallet in use (when it has xmr_bond_wallets,
// migration 124); and adds one audit row with no user. Running it again applies it again. It refuses while an
// application serves the database, and a database recording migrations this binary does not include. It returns a
// summary with counts only.
func SetRestoreGate(ctx context.Context, db *sql.DB) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = lockRestoreGate(ctx, tx); err != nil {
		return "", err
	}
	for _, key := range []string{"payments_recovery_required", "retention_restore_required", "escrow_xmr_release_recheck", xmrEscrowSweepKey} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", key); err != nil {
			return "", err
		}
	}
	var hasXMR, hasBond bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('xmr_settlements') AND attname='send_error' AND NOT attisdropped),
		to_regclass('xmr_bond_wallets') IS NOT NULL`).Scan(&hasXMR, &hasBond); err != nil {
		return "", err
	}
	var held, retired, closing, bondClosed int64
	affected := func(n *int64, r sql.Result, execErr error) {
		if err == nil {
			if err = execErr; err == nil {
				*n, err = r.RowsAffected()
			}
		}
	}
	if hasXMR {
		r, e := tx.ExecContext(ctx, "UPDATE xmr_settlements SET send_error=$1, updated=now() WHERE state IN ('built','relayed')", restoredSettlementHold)
		affected(&held, r, e)
		r, e = tx.ExecContext(ctx, "UPDATE xmr_escrow_slots SET state='retired', updated=now() WHERE state='free'")
		affected(&retired, r, e)
		r, e = tx.ExecContext(ctx, "UPDATE xmr_escrow_wallets SET state='closing', updated=now() WHERE state IN ('active','sealed')")
		affected(&closing, r, e)
	}
	if hasBond {
		r, e := tx.ExecContext(ctx, "UPDATE xmr_bond_wallets SET state='closed', updated=now() WHERE state='sealed'")
		affected(&bondClosed, r, e)
	}
	if err != nil {
		return "", err
	}
	action := "Payout recovery gate set by host command (-set-restore-gate)"
	if held > 0 {
		action += fmt.Sprintf("; held %d restored Monero escrow settlement(s) with a stored transaction until an administrator confirms each relay", held)
	}
	action += "; all outbound payouts paused until an administrator clears the gate"
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES (NULL,$1)", action); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Restore gate set: Monero escrow checkout, settlement building and relay are paused; held %d restored Monero escrow settlement(s), "+
		"retired %d free escrow account(s), moved %d escrow wallet(s) to closing and closed %d vendor bond wallet(s); audited", held, retired, closing, bondClosed), nil
}

// ClearRestoreGate is the host command -clear-restore-gate HANDLE: the operator's assertion, in the name of the
// administrator handle, that the restored database was reconciled with the wallets (docs/testnet-runbook.md). In one
// transaction it refuses while an application serves the database, a database this binary does not know, a handle
// that is not an administrator's (exact and case-sensitive), a gate that is not set and a restored database no
// application has started on yet (retention_restore_required still set); otherwise it sets payments_recovery_required
// to false and audits the clear on that administrator. With apply false it makes the same checks and changes nothing
// (the command's check before it asks for the confirmations), returning a note when the restore sweep has not
// completed (never a refusal). With apply true it returns a summary.
func ClearRestoreGate(ctx context.Context, db *sql.DB, handle string, apply bool) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = lockRestoreGate(ctx, tx); err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE handle=$1 AND role='admin'", handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no administrator has the handle %q (handles are exact and case-sensitive); nothing was changed", handle)
	}
	if err != nil {
		return "", err
	}
	var set bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true')").Scan(&set); err != nil {
		return "", err
	}
	if !set {
		return "", errors.New("the restore gate is not set; nothing was changed")
	}
	// The restored database must have been served once, so startup replayed the current retention state (it clears
	// retention_restore_required) and the watcher could run its sweep: reconciliation happens on that running app.
	var replayPending, sweepPending bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM settings WHERE key='retention_restore_required' AND value='true'), "+
		"EXISTS(SELECT 1 FROM settings WHERE key='xmr_escrow_sweep_required' AND value='true')").Scan(&replayPending, &sweepPending); err != nil {
		return "", err
	}
	if replayPending {
		return "", errors.New("the application has not started on this restored database yet (runbook steps 3 to 4: start it with the current retention state and reconcile first); nothing was changed")
	}
	if !apply {
		if sweepPending {
			return "Note: the Monero escrow restore sweep has not completed (xmr_escrow_sweep_required is set); Monero escrow checkout stays paused until it does, even with the gate cleared.", nil
		}
		return "", nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'"); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES ($1,$2)", id,
		"Payout recovery gate cleared by "+handle+" by host command after reconciling the restored database with the wallets"); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return "Restore gate cleared by administrator " + handle + "; audited", nil
}
