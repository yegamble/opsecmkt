package market

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// A-492, migration 108: no pooled market exists, so a database holding any pooled payment history (a payouts or
// payment_addresses row) is refused before anything changes, and both tables refuse every new row.

const (
	pooledPayoutInsert  = "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'XMR',100000,'legacy-payout-address','sent')"
	pooledAddressInsert = "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR','bcrt1qlegacy108address0000000000000000000','btc-regtest')"
)

// restoreSchema109 puts the database back as migration 109 left it: 110's stamp and table removed.
func restoreSchema109(t *testing.T, e *testEnv) {
	t.Helper()
	restoreSchema110(t, e)
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version=110; DROP TABLE escrow_watch_unrepaired`); err != nil {
		t.Fatal(err)
	}
}

// restoreSchema108 puts the database back as migration 108 left it: 109's stamp, columns, constraints and sequence
// removed, and 074's fee destination check restored.
func restoreSchema108(t *testing.T, e *testEnv) {
	t.Helper()
	restoreSchema109(t, e)
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version=109;
		ALTER TABLE escrows DROP CONSTRAINT escrows_fee_destination_or_descriptor, DROP CONSTRAINT escrows_fee_descriptor_btc,
			DROP CONSTRAINT escrows_fee_index_descriptor, DROP COLUMN fee_descriptor, DROP COLUMN fee_index,
			ADD CONSTRAINT escrows_fee_destination_check CHECK (fee_bps = 0 OR fee_destination <> '');
		DROP SEQUENCE escrow_fee_index`); err != nil {
		t.Fatal(err)
	}
}

// restoreSchema107 puts the database back as migration 107 left it: 108's stamp, triggers and function removed.
func restoreSchema107(t *testing.T, e *testEnv) {
	t.Helper()
	restoreSchema108(t, e)
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version=108;
		DROP TRIGGER payouts_refuse_insert ON payouts;
		DROP TRIGGER payment_addresses_refuse_insert ON payment_addresses;
		DROP FUNCTION refuse_pooled_history()`); err != nil {
		t.Fatal(err)
	}
}

// withPooledHistoryWritable runs fn in a transaction with migration 108's triggers off, then rolls it back, so a test
// can check the older constraints that still guard the pooled tables (such as 074's payouts_not_escrow).
func withPooledHistoryWritable(t *testing.T, e *testEnv, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("ALTER TABLE payouts DISABLE TRIGGER payouts_refuse_insert; ALTER TABLE payment_addresses DISABLE TRIGGER payment_addresses_refuse_insert"); err != nil {
		t.Fatal(err)
	}
	fn(tx)
}

func pooledRefusalTriggers(t *testing.T, e *testEnv) int {
	t.Helper()
	return agInt(e, "SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname IN ('payouts_refuse_insert','payment_addresses_refuse_insert') AND tgenabled='O'")
}

func TestMigration108FreshDatabaseMigratesAndRefusesPooledRows(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newFreshTestApp(t)
	if n := agInt(e, "SELECT count(*) FROM schema_migrations WHERE version=108 AND name='refuse_pooled_history'"); n != 1 {
		t.Fatalf("fresh database recorded migration 108 %d times", n)
	}
	if n := pooledRefusalTriggers(t, e); n != 2 {
		t.Fatalf("fresh database has %d enabled pooled refusal triggers, want 2", n)
	}
	buyer, _ := e.user("m108_fresh_buyer", "buyer")
	vendor, _ := e.user("m108_fresh_vendor", "vendor")
	order := e.order(buyer, e.product(vendor, "digital"), "XMR", stateCompleted)
	for table, insert := range map[string]struct {
		q    string
		args []any
	}{
		"payouts":           {pooledPayoutInsert, []any{order, vendor}},
		"payment_addresses": {pooledAddressInsert, []any{order}},
	} {
		_, err := e.DB.Exec(insert.q, insert.args...)
		if err == nil || !strings.Contains(err.Error(), "pooled payments are removed: "+table+" takes no new rows") {
			t.Errorf("a new %s row: %v", table, err)
		}
		if n := agInt(e, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s holds %d rows after the refusal", table, n)
		}
	}
}

func TestMigration108RefusesPooledHistoryUnchanged(t *testing.T) {
	clearObsoletePooledEnv(t)
	for _, tc := range []struct{ table, count string }{
		{"payouts", "(1 payouts row(s), 0 payment_addresses row(s))"},
		{"payment_addresses", "(0 payouts row(s), 1 payment_addresses row(s))"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			e := newPooledHistoricalFixture(t, 107)
			buyer, _ := e.keylessUser("m108_"+tc.table+"_buyer", "buyer")
			vendor, _ := e.keylessUser("m108_"+tc.table+"_vendor", "vendor")
			// Closed long ago: no migration 107 drain blocker, so only 108 refuses it.
			order := e.order(buyer, e.product(vendor, "digital"), "XMR", stateCompleted)
			agExec(e, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", order)
			if tc.table == "payouts" {
				agExec(e, pooledPayoutInsert, order, vendor)
			} else {
				agExec(e, pooledAddressInsert, order)
			}
			snapshot := func() string {
				return strings.Join(agStrings(t, e, `SELECT (SELECT count(*) FROM payouts)||':'||(SELECT count(*) FROM payment_addresses)||':'||
					(SELECT max(version) FROM schema_migrations)||':'||(SELECT count(*) FROM pg_trigger WHERE tgname LIKE '%_refuse_insert')||':'||
					(SELECT count(*) FROM pg_proc WHERE proname='refuse_pooled_history')`), "")
			}
			before := snapshot()
			if before != map[string]string{"payouts": "1:0:107:0:0", "payment_addresses": "0:1:107:0:0"}[tc.table] {
				t.Fatalf("pre-108 fixture = %s", before)
			}
			err := applyLatestMigrations(t, e)
			for _, want := range []string{"migration 108 refused", tc.count, "Nothing was changed", "UPGRADING.md section 59"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("migration 108 over a %s row = %v, want %q", tc.table, err, want)
				}
			}
			if after := snapshot(); after != before {
				t.Fatalf("refused migration changed the database: %s, was %s", after, before)
			}
			// Without the pooled row the same database migrates.
			agExec(e, "DELETE FROM "+tc.table)
			if err = applyLatestMigrations(t, e); err != nil {
				t.Fatalf("migration 108 without pooled history: %v", err)
			}
			if got := snapshot(); got != "0:0:"+latestMigrationVersion(t)+":2:1" { // 108 and every later migration
				t.Fatalf("after migration 108: %s, want 0:0:%s:2:1", got, latestMigrationVersion(t))
			}
			if n := pooledRefusalTriggers(t, e); n != 2 {
				t.Fatalf("%d enabled pooled refusal triggers after migration 108, want 2", n)
			}
		})
	}
}

// latestMigrationVersion is the highest migration number embedded in this build, as schema_migrations records it.
func latestMigrationVersion(t *testing.T) string {
	t.Helper()
	list, err := loadMigrations(migrationFiles)
	if err != nil || len(list) == 0 {
		t.Fatal("no migrations", err)
	}
	return strconv.Itoa(list[len(list)-1].Version)
}
