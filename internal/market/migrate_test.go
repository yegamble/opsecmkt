package market

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrationFilesAreWellFormed(t *testing.T) {
	list, err := loadMigrations(migrationFiles)
	if err != nil || len(list) == 0 || list[0].Version != 1 {
		t.Fatalf("migrations=%v err=%v", list, err)
	}
	for _, bad := range []fstest.MapFS{
		{"migrations/1_x.sql": {}},
		{"migrations/001_a.sql": {}, "migrations/001_b.sql": {}},
		{"migrations/002_Upper.sql": {}},
	} {
		if _, err := loadMigrations(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	list, err = loadMigrations(fstest.MapFS{"migrations/010_b.sql": {}, "migrations/002_a.sql": {}, "migrations/100_c.sql": {}})
	if err != nil || list[0].Version != 2 || list[1].Version != 10 || list[2].Version != 100 {
		t.Fatalf("not numerically sorted: %v %v", list, err)
	}
}

func schemaDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // keep search_path on one session
	t.Cleanup(func() { db.Close() })
	return db
}

func runInTx(t *testing.T, db *sql.DB, f func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = f(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRunnerAppliesMissingVersionsOnce(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	m := func(v int, table string) migration {
		return migration{v, table, "CREATE TABLE " + table + " (id int)"}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, []migration{m(1, "m_one"), m(3, "m_three")}) })
	// A lower version merged later is still applied; applied ones are not re-run (CREATE TABLE would fail).
	runInTx(t, db, func(tx *sql.Tx) error {
		return applyMigrations(ctx, tx, []migration{m(1, "m_one"), m(2, "m_two"), m(3, "m_three")})
	})
	runInTx(t, db, func(tx *sql.Tx) error {
		return applyMigrations(ctx, tx, []migration{m(1, "m_one"), m(2, "m_two"), m(3, "m_three")})
	})
	var n int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version IN (1,2,3)").Scan(&n); err != nil || n != 3 {
		t.Fatalf("recorded=%d err=%v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM m_two").Scan(&n); err != nil {
		t.Fatalf("version 2 not applied: %v", err)
	}
	tx, _ := db.Begin()
	defer tx.Rollback()
	if err := applyMigrations(ctx, tx, []migration{{4, "broken", "SELECT * FROM missing_table"}}); err == nil {
		t.Fatal("failing migration reported success")
	}
}

func TestFreshInstallAppliesBaselineAndMigrations(t *testing.T) {
	e := newFreshTestApp(t)
	list, _ := loadMigrations(migrationFiles)
	count := func() (n int) {
		if err := e.DB.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != len(list) {
		t.Fatalf("applied %d of %d migrations", got, len(list))
	}
	e.restart()
	if got := count(); got != len(list) {
		t.Fatalf("restart changed migrations: %d", got)
	}
}

// A-400: compare a copy of the migrated template before newTestApp's intentional paused-intake fixture adjustment
// with a fresh migration run. They must have the same relations, columns, constraints, indexes, triggers, functions,
// types, sequences and rows. A fresh migration run also writes one startup audit event; that intentional runtime row
// is excluded from the template comparison.
const freshStartAuditActionSQL = `(SELECT 'Startup applied ' || count(*) || ' database migrations; schema version ' || lpad(max(version)::text,3,'0') FROM schema_migrations)`

func TestNewTestAppMatchesAFreshStart(t *testing.T) {
	copied := startTestApp(t, testMigratedDSN(t))
	fresh := newFreshTestApp(t) // creates an empty schema and executes the baseline plus every migration
	if got := agInt(fresh, "SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action="+freshStartAuditActionSQL); got != 1 {
		t.Fatalf("fresh start has %d migration startup audit rows, want exactly one", got)
	}
	if got := agInt(copied, "SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action="+freshStartAuditActionSQL); got != 0 {
		t.Fatalf("template snapshot has %d migration startup audit rows, want none", got)
	}
	want, got := schemaSnapshot(fresh), schemaSnapshot(copied)
	if len(got) != len(want) {
		t.Errorf("snapshot has %d lines, a fresh start %d", len(got), len(want))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Fatalf("first difference at line %d:\n  migrated template copy (before shared-fixture intake pause): %s\n  fresh migration: %s", i, got[i], want[i])
		}
	}
}

// schemaSnapshot describes e's schema (its search_path) and every row in it, with the schema's own name removed
// from the server's definitions. It excludes schema_migrations and normalizes only the per-start health updated_at.
func schemaSnapshot(e *testEnv) []string {
	e.t.Helper()
	ns := agStr(e, "SELECT current_schema()")
	const in = "= current_schema()::regnamespace"
	var lines []string
	sequenceSnapshot := "SELECT 'sequence ' || sequencename || ' ' || CASE WHEN sequencename='audit_events_id_seq' THEN " +
		"(COALESCE(last_value,0) - (SELECT count(*) FROM audit_events WHERE user_id IS NULL AND action=" + freshStartAuditActionSQL + "))::text " +
		"ELSE COALESCE(last_value::text,'unused') END FROM pg_sequences WHERE schemaname = current_schema()"
	for _, q := range []string{
		"SELECT 'relation ' || relname || ' ' || relkind::text FROM pg_class WHERE relnamespace " + in,
		"SELECT 'column ' || table_name || '.' || column_name || ' #' || ordinal_position || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '') FROM information_schema.columns WHERE table_schema = current_schema()",
		"SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace " + in,
		"SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = current_schema()",
		"SELECT 'trigger ' || pg_get_triggerdef(t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE NOT t.tgisinternal AND c.relnamespace " + in,
		"SELECT 'function ' || pg_get_functiondef(oid) FROM pg_proc WHERE prokind IN ('f', 'p') AND pronamespace " + in,
		"SELECT 'type ' || typname || ' ' || typtype::text FROM pg_type WHERE typtype IN ('d', 'e') AND typnamespace " + in,
		sequenceSnapshot,
		"SELECT 'migration ' || version || ' ' || name FROM schema_migrations",
	} {
		lines = append(lines, agStrings(e.t, e, q)...)
	}
	for _, table := range agStrings(e.t, e, "SELECT relname FROM pg_class WHERE relkind = 'r' AND relname <> 'schema_migrations' AND relnamespace "+in) {
		rowJSON := "row_to_json(r)"
		if table == "bitcoin_escrow_health" {
			// This operational timestamp is initialized with now() on each app start; compare every health/config field.
			rowJSON = "(row_to_json(r)::jsonb - 'updated_at')::text"
		}
		query := "SELECT 'row " + table + " ' || " + rowJSON + " FROM " + table + " r"
		if table == "audit_events" {
			query += " WHERE NOT (r.user_id IS NULL AND r.action=" + freshStartAuditActionSQL + ")"
		}
		lines = append(lines, agStrings(e.t, e, query)...)
	}
	for i := range lines {
		lines[i] = strings.ReplaceAll(lines[i], ns+".", "")
	}
	slices.Sort(lines)
	return lines
}

// An existing v0 database (baseline schema with free-text orders.status) upgrades in place.
func TestUpgradeFromBaselineSchema(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		for _, q := range []string{
			"INSERT INTO users(id,handle,password_hash,role) VALUES('b','buyer_x','x','buyer'),('v','vendor_x','x','vendor')",
			"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES('p','v','T','D','Hardware','Worldwide','physical',1,1,1)",
			"INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES('o','b','p','BTC',1)",
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	runInTx(t, db, func(tx *sql.Tx) error { return migrate(ctx, tx) })
	var state string
	if err := db.QueryRow("SELECT state FROM orders WHERE id='o'").Scan(&state); err != nil || state != stateDraft {
		t.Fatalf("existing draft state=%q err=%v", state, err)
	}
	var n int
	db.QueryRow("SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='orders' AND column_name='status'").Scan(&n)
	if n != 0 {
		t.Fatal("orders.status still present")
	}
	db.QueryRow("SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='orders_one_draft' AND indexdef LIKE '%WHERE (state = ''draft''::text)%'").Scan(&n)
	if n != 1 {
		t.Fatal("partial one-draft index missing")
	}
	if _, err := db.Exec("INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES('o2','b','p','BTC',1)"); err == nil {
		t.Fatal("second draft for the same buyer/product/currency accepted")
	}
	if _, err := db.Exec("UPDATE orders SET state='cancelled' WHERE id='o'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO orders(id,buyer_id,product_id,currency,amount) VALUES('o2','b','p','BTC',1)"); err != nil {
		t.Fatalf("new draft after cancellation rejected: %v", err)
	}
	if _, err := db.Exec("UPDATE orders SET state='bogus' WHERE id='o'"); err == nil {
		t.Fatal("unknown state accepted")
	}
	runInTx(t, db, func(tx *sql.Tx) error { return migrate(ctx, tx) })
}

// A-30: on a database upgraded by a newer release (a migration recorded that this binary does not embed) the
// server refuses to start, names every unknown version and changes nothing, instead of serving a schema it
// does not know.
func TestStartupRefusesUnknownAppliedMigration(t *testing.T) {
	e := newTestApp(t)
	list, _ := loadMigrations(migrationFiles)
	last := list[len(list)-1]
	// Also forget the newest embedded migration: a refused start must not apply it.
	if _, err := e.DB.Exec("DELETE FROM schema_migrations WHERE version=$1", last.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("INSERT INTO schema_migrations(version,name) VALUES (998,'future_one'),(999,'future_two')"); err != nil {
		t.Fatal(err)
	}
	e.A.Close()
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("server started on a database with migrations it does not include")
	}
	for _, want := range []string{"998_future_one", "999_future_two", "UPGRADING.md", "Rolling back"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err = db.QueryRow("SELECT string_agg(version::text, ',' ORDER BY version) FROM schema_migrations WHERE version=$1 OR version>=998", last.Version).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "998,999" {
		t.Fatalf("refused start changed schema_migrations: %q", got)
	}
}
