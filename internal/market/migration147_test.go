package market

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// A-520 (PRIV60-4): migration 147 clears every account's saved pooled-payout addresses and the times they changed,
// keeps the columns, and leaves the rest of the account alone. DB integration.
func TestMigration147ClearsLegacyPayoutAddresses(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var before, only []migration
	for _, m := range list {
		switch {
		case m.Version < 147:
			before = append(before, m)
		case m.Version == 147:
			only = append(only, m)
		}
	}
	if len(only) != 1 {
		t.Fatalf("migration 147: %d files", len(only))
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, before)
	})
	for _, q := range []string{
		`INSERT INTO users(id,handle,password_hash,role,payout_btc,payout_xmr,payout_btc_changed,payout_xmr_changed,pgp)
			VALUES('u1','p147a','hash-a','vendor','bc1-saved','4-saved',now(),now(),'key a'),
			('u2','p147b','hash-b','buyer','','',now(),NULL,''),
			('u3','p147c','hash-c','buyer','','',NULL,NULL,'')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, only) })

	var left int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE payout_btc<>'' OR payout_xmr<>'' OR payout_btc_changed IS NOT NULL
		OR payout_xmr_changed IS NOT NULL`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after migration 147: %d accounts keep a payout value (%v)", left, err)
	}
	var kept []string
	rows, err := db.Query("SELECT id||' '||handle||' '||password_hash||' '||role||' '||pgp FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, s)
	}
	rows.Close()
	if want := []string{"u1 p147a hash-a vendor key a", "u2 p147b hash-b buyer ", "u3 p147c hash-c buyer "}; !slices.Equal(kept, want) {
		t.Fatalf("other account values changed: %q, want %q", kept, want)
	}
	var columns int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='users'
		AND column_name IN ('payout_btc','payout_xmr','payout_btc_changed','payout_xmr_changed')`).Scan(&columns); err != nil || columns != 4 {
		t.Fatalf("payout columns after migration 147: %d (%v), want all 4 kept", columns, err)
	}
}
