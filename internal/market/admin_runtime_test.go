package market

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartupMigrationAuditIsAtomicAndWrittenOnlyForAppliedWork(t *testing.T) {
	e := newTestApp(t)
	original := agInt(e, "SELECT count(*) FROM audit_events")
	version := agInt(e, "SELECT max(version) FROM schema_migrations")
	timeouts := startupTimeouts{connect: time.Second, lock: time.Second, migrate: time.Second}
	if err := prepareDatabase(t.Context(), e.DB, timeouts); err != nil {
		t.Fatal(err)
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != original {
		t.Fatal("ordinary restart audited migrations")
	}
	// A gap below the maximum still counts as applied work.
	agExec(e, "DELETE FROM schema_migrations WHERE version=99")
	if err := prepareDatabase(t.Context(), e.DB, timeouts); err != nil {
		t.Fatal(err)
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action=$1", fmt.Sprintf("Startup applied 1 database migrations; schema version %03d", version)) != 1 {
		t.Fatal("missing migration work audit")
	}
	if err := prepareDatabase(t.Context(), e.DB, timeouts); err != nil {
		t.Fatal(err)
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != original+1 {
		t.Fatal("restart repeated migration work audit")
	}
	// Failure after applying changes still rolls the schema and audit back together.
	agExec(e, "DELETE FROM schema_migrations WHERE version=99")
	saved := runMigrations
	t.Cleanup(func() { runMigrations = saved })
	runMigrations = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrate(ctx, tx); err != nil {
			return err
		}
		return errors.New("test after migration refusal")
	}
	if err := prepareDatabase(t.Context(), e.DB, timeouts); err == nil {
		t.Fatal("accepted failed migration")
	}
	if agInt(e, "SELECT count(*) FROM schema_migrations WHERE version=99") != 0 || agInt(e, "SELECT count(*) FROM audit_events") != original+1 {
		t.Fatal("failed startup committed schema or audit")
	}
}

func TestAdminRuntimeUsesCurrentRoleAndProcessTime(t *testing.T) {
	e := newTestApp(t)
	id, token := e.user("runtime_admin", "admin")
	e.A.startedAt = time.Date(2026, 10, 8, 12, 34, 56, 0, time.UTC)
	w := e.do("GET", "/admin", token, nil)
	e.check(w, 200)
	if !strings.Contains(w.Body.String(), fmt.Sprintf("Database schema version %d", agInt(e, "SELECT max(version) FROM schema_migrations"))) || !strings.Contains(w.Body.String(), "2026-10-08T12:34:56Z") {
		t.Fatal("missing runtime metadata")
	}
	for _, update := range []string{"role='buyer'", "role='admin',suspended_at=now()"} {
		agExec(e, "UPDATE users SET "+update+" WHERE id=$1", id)
		d := &PageData{User: &User{ID: id, Role: "admin"}}
		if err := loadAdminRuntime(t.Context(), e.A, httptest.NewRequest("GET", "/admin", nil), d); err != nil || d.ServerStarted != "" || d.SchemaVersion != 0 {
			t.Fatal("stale role received runtime metadata")
		}
	}
}

func TestFreshStartupRecordsAppliedSchemaAndFailedAuditRollsBackMigration(t *testing.T) {
	db, err := OpenDB(testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	timeouts := startupTimeouts{connect: time.Second, lock: time.Second, migrate: 2 * time.Second}
	if err = prepareDatabase(t.Context(), db, timeouts); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action LIKE 'Startup applied % database migrations; schema version %'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("fresh audit: %d %v", count, err)
	}
	if _, err = db.Exec(`DELETE FROM schema_migrations WHERE version=99;
 CREATE FUNCTION refuse_startup_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Startup applied %' THEN RAISE EXCEPTION 'test refusal'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER refuse_startup_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_startup_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = prepareDatabase(t.Context(), db, timeouts); err == nil {
		t.Fatal("accepted refused migration audit")
	}
	if err = db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=99").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed audit committed migration")
	}
	if err = db.QueryRow("SELECT count(*) FROM audit_events").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed audit changed history")
	}
}
