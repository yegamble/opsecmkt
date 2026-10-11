package market

import (
	"context"
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

func restoreSchema106(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := e.DB.Exec("DELETE FROM schema_migrations WHERE version=107"); err != nil {
		t.Fatal(err)
	}
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
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 107 {
		t.Fatalf("fresh schema version = %d, want 107", got)
	}
	if btc, xmr := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"), agStr(e, "SELECT value FROM settings WHERE key='payment_intake_XMR'"); btc != "escrow" || xmr != "escrow" {
		t.Fatalf("fresh intake BTC=%q XMR=%q, want escrow/escrow", btc, xmr)
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
	for _, currency := range []string{"BTC", "XMR"} {
		// Neither currency has an enabled escrow. A fresh install must refuse a new
		// order instead of silently falling back to the retired pooled payment path.
		e.check(e.do("POST", "/orders", buyer, url.Values{"product_id": {product}, "currency": {currency}}), 409)
	}
	for _, route := range []string{"/orders/pay", "/account/payout", "/admin/payout", "/admin/payment-address-collisions"} {
		e.check(e.do("POST", route, admin, url.Values{"currency": {"BTC"}, "address": {"bcrt1qretired"}}), 404)
	}
	for _, table := range []string{"orders", "payment_addresses", "payments", "payouts", "escrows"} {
		if n := agInt(e, "SELECT count(*) FROM "+table); n != 0 {
			t.Fatalf("fresh refusal wrote %d rows to %s", n, table)
		}
	}
}

func TestMigration107RefusesUndrainedLegacyWithoutChangesThenRestoredRerunPasses(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newTestApp(t)
	buyer, _ := e.user("migration107_buyer", "buyer")
	vendor, _ := e.user("migration107_vendor", "vendor")
	product := e.product(vendor, "digital")
	order := e.order(buyer, product, "BTC", stateAwaitingPayment)
	closed := e.order(buyer, e.product(vendor, "physical"), "BTC", stateCompleted)
	if _, err := e.DB.Exec(`UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1`, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider,created)
		VALUES($1,'BTC','bcrt1qlegacy107history00000000000000000000','btc-regtest',now()-interval '40 days')`, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited)
		VALUES($1,'BTC',$2,0,'bcrt1qlegacy107history00000000000000000000',100000,6,true)`, closed, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='true' WHERE key='payment_intake_BTC'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE settings SET value='false' WHERE key='payment_intake_XMR'`); err != nil {
		t.Fatal(err)
	}
	restoreSchema106(t, e)
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

	// This models restoring the pre-107 backup after operators drain it on an older drain-capable release.
	if _, err = e.DB.Exec(`UPDATE orders SET state='completed',updated=now()-interval '40 days' WHERE id=$1`, order); err != nil {
		t.Fatal(err)
	}
	if err = applyLatestMigrations(t, e); err != nil {
		t.Fatalf("migration after the restored database was drained: %v", err)
	}
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 107 {
		t.Fatalf("rerun schema version = %d, want 107", got)
	}
	if btc, xmr := agStr(e, "SELECT value FROM settings WHERE key='payment_intake_BTC'"), agStr(e, "SELECT value FROM settings WHERE key='payment_intake_XMR'"); btc != "escrow" || xmr != "false" {
		t.Fatalf("drained legacy intake BTC=%q XMR=%q, want escrow/false", btc, xmr)
	}
	if got := agInt(e, "SELECT count(*) FROM payments WHERE txid=$1", strings.Repeat("a", 64)); got != 1 {
		t.Fatal("successful migration changed historical payment")
	}
	if got := agInt(e, "SELECT count(*) FROM payment_addresses WHERE order_id=$1", closed); got != 1 {
		t.Fatal("successful migration removed historical address")
	}
}

func TestMigration107RefusesMainnetAddressHistory(t *testing.T) {
	clearObsoletePooledEnv(t)
	e := newTestApp(t)
	buyer, _ := e.user("migration107_mainnet_buyer", "buyer")
	vendor, _ := e.user("migration107_mainnet_vendor", "vendor")
	order := e.order(buyer, e.product(vendor, "digital"), "BTC", "draft")
	if _, err := e.DB.Exec(`INSERT INTO payment_addresses(order_id,currency,address,provider)
		VALUES($1,'BTC','bc1qlegacy107mainnet000000000000000000000','btc-main')`, order); err != nil {
		t.Fatal(err)
	}
	restoreSchema106(t, e)
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
	e := newTestApp(t)
	if _, err := e.DB.Exec(`CREATE OR REPLACE FUNCTION pooled_drain_blockers()
		RETURNS TABLE(kind text, ref text, order_id text, currency text, detail text)
		LANGUAGE SQL STABLE AS $$
			SELECT 'future_kind'::text, 'private-reference'::text, NULL::text, NULL::text, 'private detail'::text
		$$`); err != nil {
		t.Fatal(err)
	}
	restoreSchema106(t, e)
	err := applyLatestMigrations(t, e)
	if err == nil || !strings.Contains(err.Error(), "future_kind") || strings.Contains(err.Error(), "private-reference") || strings.Contains(err.Error(), "private detail") {
		t.Fatalf("future blocker refusal = %v, want only its kind and no ref/detail", err)
	}
	if got := agInt(e, "SELECT max(version) FROM schema_migrations"); got != 106 {
		t.Fatalf("refused future blocker migration recorded version %d, want 106", got)
	}
}

func TestMigration107PreservesHistoricalPooledFeeAddressRefusal(t *testing.T) {
	clearObsoletePooledEnv(t)
	x := newEscrowEnv(t)
	_, admin := x.user("legacy107_fee_admin", "admin")
	old := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateCompleted)
	address := regtestAddr("legacy107 pooled fee refusal")
	agExec(x.testEnv, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", old)
	agExec(x.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES($1,'BTC',$2,'btc-regtest',now()-interval '40 days')", old, address)
	w := x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {address}, "password": {testPassword}})
	x.check(w, 409)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key LIKE 'escrow_btc_fee_%'"); n != 0 {
		t.Fatalf("refused historical fee address changed %d settings", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payment_addresses WHERE order_id=$1 AND address=$2", old, address); n != 1 {
		t.Fatal("historical address was changed")
	}
}
