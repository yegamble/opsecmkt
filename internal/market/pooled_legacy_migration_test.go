package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func clearObsoletePooledEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BITCOIN_RPC_URL", "BITCOIN_WALLET", "MONERO_WALLET_RPC_URL", "MONERO_RPC_URL"} {
		t.Setenv(name, "")
	}
}

// newPooledHistoricalFixture builds the actual historical schema before adding
// test rows. Undoing a fixed tail of the current schema leaves newer migration
// markers and guards behind as soon as another migration is added.
func newPooledHistoricalFixture(t *testing.T, upto int) *testEnv {
	t.Helper()
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var historical []migration
	for _, m := range list {
		if m.Version <= upto {
			historical = append(historical, m)
		}
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), schema); err == nil {
		err = applyMigrations(t.Context(), tx, historical)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, DB: db}
	if version := agInt(e, "SELECT max(version) FROM schema_migrations"); version != upto {
		t.Fatalf("historical fixture version %d, want %d", version, upto)
	}
	return e
}

func applyLatestMigrations(t *testing.T, e *testEnv) error {
	t.Helper()
	tx, err := e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = migrate(context.Background(), tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func TestMigration107FreshInstallUsesEscrowAndHasNoPooledHistory(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newFreshTestApp(t)
	if got := agInt(e, "SELECT count(*) FROM schema_migrations WHERE version IN (107,108)"); got != 2 {
		t.Fatalf("fresh schema records %d of migrations 107 and 108, want both", got)
	}
	if btc, xmr := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"), agStr(e, "SELECT value FROM settings WHERE key='payment_intake_XMR'"); btc != "false" || xmr != "escrow" {
		t.Fatalf("fresh intake BTC=%q XMR=%q, want false/escrow", btc, xmr)
	}
	if n := agInt(e, "SELECT count(*) FROM pooled_drain_blockers()"); n != 0 {
		t.Fatalf("fresh database has %d pooled drain blockers", n)
	}
}

func TestFreshEscrowOnlyInstallCannotFallBackToPooledPayments(t *testing.T) {
	clearObsoletePooledEnv(t)
	t.Setenv("ESCROW_BTC", "")
	t.Setenv("ESCROW_XMR", "")
	e := newFreshTestApp(t)
	admin := e.setup()
	_, buyer := e.user("fresh107_buyer", "buyer")
	vendor, _ := e.user("fresh107_vendor", "vendor")
	product := e.product(vendor, "digital")
	for _, path := range []string{"/product?id=" + product, "/checkout?id=" + product} {
		page := e.body("GET", path, buyer, nil, 200)
		mustContain(t, page, "New payments paused")
		mustNotContain(t, page, `class="badge testnet">Bitcoin escrow`, `class="badge testnet">Monero escrow`)
	}
	for _, currency := range []string{"XMR"} {
		// Neither currency has an enabled escrow. A fresh install must refuse a new
		// order instead of silently falling back to the retired pooled payment path.
		e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {currency}}), 409)
	}
	for _, route := range []string{"/orders/pay", "/account/payout", "/admin/payout", "/admin/payment-address-collisions"} {
		e.check(e.do("POST", route, admin, url.Values{"currency": {"XMR"}, "address": {"bcrt1qretired"}}), 404)
	}
	for _, table := range []string{"orders", "payment_addresses", "payments", "payouts", "escrows"} {
		if n := agInt(e, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("fresh refusal wrote %d rows to %s", n, table)
		}
	}
}

func TestMigration107RefusesUndrainedLegacyWithoutChangesThenRestoredRerunPasses(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newPooledHistoricalFixture(t, 106)
	buyer, _ := e.keylessUser("migration107_buyer", "buyer")
	vendor, _ := e.keylessUser("migration107_vendor", "vendor")
	product := e.product(vendor, "digital")
	order := e.order(buyer, product, "XMR", stateAwaitingPayment)
	closed := e.order(buyer, e.product(vendor, "physical"), "XMR", stateCompleted)
	if _, err := e.DB.Exec(`UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1`, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider,created)
		VALUES($1,'XMR','bcrt1qlegacy107history00000000000000000000','btc-regtest',now()-interval '40 days')`, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited)
		VALUES($1,'XMR',$2,0,'bcrt1qlegacy107history00000000000000000000',100000,6,true)`, closed, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='true' WHERE key='payment_intake_BTC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='false' WHERE key='payment_intake_XMR'`); err != nil {
		t.Fatal(err)
	}
	before := []int{
		agInt(e, "SELECT count(*) FROM orders"),
		agInt(e, "SELECT count(*) FROM payment_addresses"),
		agInt(e, "SELECT count(*) FROM payments"),
		agInt(e, "SELECT count(*) FROM settings"),
		agInt(e, "SELECT max(version) FROM schema_migrations"),
	}
	err := applyLatestMigrations(t, e)
	if err == nil || !strings.Contains(err.Error(), "open_order") || !strings.Contains(err.Error(), "Nothing was changed") || strings.Contains(err.Error(), order) {
		t.Fatalf("migration refusal = %v, want a generic open_order blocker kind without references", err)
	}
	after := []int{
		agInt(e, "SELECT count(*) FROM orders"),
		agInt(e, "SELECT count(*) FROM payment_addresses"),
		agInt(e, "SELECT count(*) FROM payments"),
		agInt(e, "SELECT count(*) FROM settings"),
		agInt(e, "SELECT max(version) FROM schema_migrations"),
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("failed migration changed table snapshot %v to %v", before, after)
		}
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"); got != "true" {
		t.Fatalf("failed migration changed BTC intake to %q", got)
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_XMR'"); got != "false" {
		t.Fatalf("failed migration changed paused XMR intake to %q", got)
	}
	if got := agInt(e, "SELECT count(*) FROM payments WHERE txid=$1", strings.Repeat("a", 64)); got != 1 {
		t.Fatal("failed migration changed historical payment")
	}

	// This models restoring the pre-107 backup after operators drain it on an older drain-capable release. 107 then
	// passes, but the drained database still holds pooled history, which migration 108 (A-492) refuses unchanged.
	if _, err = e.DB.Exec(`UPDATE orders SET state='completed',updated=now()-interval '40 days' WHERE id=$1`, order); err != nil {
		t.Fatal(err)
	}
	if n := agInt(e, "SELECT count(*) FROM pooled_drain_blockers()"); n != 0 {
		t.Fatalf("drained fixture still has %d pooled drain blockers", n)
	}
	err = applyLatestMigrations(t, e)
	if err == nil || !strings.Contains(err.Error(), "migration 108 refused") || !strings.Contains(err.Error(), "UPGRADING.md section 59") {
		t.Fatalf("drained legacy database with pooled history = %v, want migration 108's refusal", err)
	}
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 106 {
		t.Fatalf("refused rerun recorded schema version %d, want 106", got)
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"); got != "true" {
		t.Fatalf("refused rerun changed BTC intake to %q", got)
	}
	if got := agInt(e, "SELECT count(*) FROM payment_addresses WHERE order_id=$1", closed); got != 1 {
		t.Fatal("refused rerun removed the historical address")
	}

	// A drained legacy database without pooled history (no payouts or payment_addresses row) upgrades through both.
	if _, err = e.DB.Exec("DELETE FROM payment_addresses WHERE order_id=$1", closed); err != nil {
		t.Fatal(err)
	}
	if err = applyLatestMigrations(t, e); err != nil {
		t.Fatalf("migration of a drained database without pooled history: %v", err)
	}
	if got := agInt(e, "SELECT count(*) FROM schema_migrations WHERE version IN (107,108)"); got != 2 {
		t.Fatalf("rerun recorded %d of migrations 107 and 108, want both", got)
	}
	if btc, xmr := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"), agStr(e, "SELECT value FROM settings WHERE key='payment_intake_XMR'"); btc != "false" || xmr != "false" {
		t.Fatalf("drained legacy intake BTC=%q XMR=%q, want false/false", btc, xmr)
	}
	if got := agInt(e, "SELECT count(*) FROM payments WHERE txid=$1", strings.Repeat("a", 64)); got != 1 {
		t.Fatal("successful migration changed historical payment")
	}
}

func TestMigration107RefusesMainnetAddressHistory(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newPooledHistoricalFixture(t, 106)
	buyer, _ := e.keylessUser("migration107_mainnet_buyer", "buyer")
	vendor, _ := e.keylessUser("migration107_mainnet_vendor", "vendor")
	order := e.order(buyer, e.product(vendor, "digital"), "XMR", "draft")
	if _, err := e.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider)
		VALUES($1,'XMR','bc1qlegacy107mainnet000000000000000000000','btc-main')`, order); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='true' WHERE key='payment_intake_BTC'`); err != nil {
		t.Fatal(err)
	}
	err := applyLatestMigrations(t, e)
	if err == nil || !strings.Contains(err.Error(), "mainnet_history") || strings.Contains(err.Error(), order) {
		t.Fatalf("migration error = %v, want generic mainnet_history refusal", err)
	}
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 106 {
		t.Fatalf("refused mainnet migration recorded version %d, want 106", got)
	}
	if got := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"); got != "true" {
		t.Fatalf("refused mainnet migration changed BTC intake to %q", got)
	}
}

func TestMigration107RefusesUnknownFutureBlockerKindsWithoutReferences(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newPooledHistoricalFixture(t, 106)
	if _, err := e.DB.Exec(`CREATE OR REPLACE FUNCTION pooled_drain_blockers()
		RETURNS TABLE(kind text, ref text, order_id text, currency text, detail text)
		LANGUAGE SQL STABLE AS $$
			SELECT 'future_kind'::text, 'private-reference'::text, NULL::text, NULL::text, 'private detail'::text
		$$`); err != nil {
		t.Fatal(err)
	}
	err := applyLatestMigrations(t, e)
	if err == nil || !strings.Contains(err.Error(), "future_kind") || strings.Contains(err.Error(), "private-reference") || strings.Contains(err.Error(), "private detail") {
		t.Fatalf("future blocker refusal = %v, want only its kind and no ref/detail", err)
	}
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 106 {
		t.Fatalf("refused future blocker migration recorded version %d, want 106", got)
	}
}

// newSelectedMigrationFixture applies the baseline and exactly the given migrations, in the given order and
// under the given recorded names, as an older or divergent build would have left the database.
func newSelectedMigrationFixture(t *testing.T, selected []migration) *testEnv {
	t.Helper()
	db, err := sql.Open("pgx", testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), schema); err == nil {
		err = applyMigrations(t.Context(), tx, selected)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, DB: db}
}

func embeddedMigrationsByVersion(t *testing.T) ([]migration, map[int]migration) {
	t.Helper()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	byVersion := map[int]migration{}
	for _, m := range list {
		byVersion[m.Version] = m
	}
	return list, byVersion
}

// MOD72-3: main gained migrations 128 and 131–133 before the moderation migrations merged. A database
// with every migration except 125, 126, 127 and 134 must take those four on upgrade, keeping the others' records.
func TestModerationMigrationsAfterMain128(t *testing.T) {
	list, byVersion := embeddedMigrationsByVersion(t)
	if byVersion[128].Name != "xmr_settlement_hand_settled" || byVersion[134].Name != "moderation_capacity" {
		t.Fatalf("embedded 128/134 are %q/%q", byVersion[128].Name, byVersion[134].Name)
	}
	var mainAfter613 []migration
	for _, m := range list {
		if m.Version != 125 && m.Version != 126 && m.Version != 127 && m.Version != 134 && m.Version != 143 {
			mainAfter613 = append(mainAfter613, m)
		}
	}
	e := newSelectedMigrationFixture(t, mainAfter613)
	applied128 := agStr(e, "SELECT applied::text FROM schema_migrations WHERE version=128")
	if err := applyLatestMigrations(t, e); err != nil {
		t.Fatalf("upgrade from main after A-613: %v", err)
	}
	var want []string
	for _, m := range list {
		want = append(want, fmt.Sprintf("%d:%s", m.Version, m.Name))
	}
	got := agStrings(t, e, "SELECT version || ':' || name FROM schema_migrations ORDER BY version")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recorded migrations %v, want %v", got, want)
	}
	if again := agStr(e, "SELECT applied::text FROM schema_migrations WHERE version=128"); again != applied128 {
		t.Fatalf("migration 128 re-recorded: %s, was %s", again, applied128)
	}
	for _, check := range []string{
		"SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='moderation_jobs' AND column_name='capture_version'",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='moderation_capacity'",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='moderation_backfill'",
		"SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='images' AND column_name='perceptual_hash'",
		"SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='xmr_settlement_hand_settled'",
	} {
		if agInt(e, check) != 1 {
			t.Errorf("upgraded schema lacks: %s", check)
		}
	}
	if err := applyLatestMigrations(t, e); err != nil {
		t.Fatalf("second start: %v", err)
	}
}

// MOD72-3: a database that an unmerged build recorded as 128_moderation_capacity is refused before any DDL,
// naming both migrations, instead of skipping this build's 128 and failing inside 134.
func TestMigrationNameMismatchRefused(t *testing.T) {
	list, byVersion := embeddedMigrationsByVersion(t)
	var divergent []migration
	for _, m := range list {
		if m.Version <= 127 {
			divergent = append(divergent, m)
		}
	}
	divergent = append(divergent, migration{128, "moderation_capacity", byVersion[134].SQL})
	e := newSelectedMigrationFixture(t, divergent)
	const recorded = "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations"
	before := agStr(e, recorded)
	err := applyLatestMigrations(t, e)
	if err == nil {
		t.Fatal("upgrade accepted a database whose migration 128 has a different name")
	}
	for _, want := range []string{"128_moderation_capacity", "128_xmr_settlement_hand_settled", "Nothing was changed", "Rolling back"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "already exists") {
		t.Errorf("refusal came from DDL, not the name check: %v", err)
	}
	if after := agStr(e, recorded); after != before {
		t.Fatalf("refused upgrade changed schema_migrations:\n%s\nwas\n%s", after, before)
	}
	if agInt(e, "SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='xmr_settlement_hand_settled'") != 0 {
		t.Fatal("refused upgrade applied migration 128")
	}
	// The host-side administrator commands use the same check.
	tx, err := e.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = refuseUnknownMigrations(t.Context(), tx, list); err == nil || !strings.Contains(err.Error(), "128_moderation_capacity") {
		t.Fatalf("refuseUnknownMigrations: %v", err)
	}
}
