package market

import (
	"database/sql"
	"testing"
)

// Rehearse the deployed main schema before the marketplace branch's six
// migrations. In particular, preserve main's PGP, availability and ticket schema
// while upgrading existing orders and reusable delivery listings.
func TestMarketplaceUpgradeFromMain135(t *testing.T) {
	db := schemaDB(t)
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var deployed []migration
	for _, m := range list {
		if m.Version <= 135 {
			deployed = append(deployed, m)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(t.Context(), tx, deployed)
	})
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role,vendor_vacation) VALUES('ub','upgrade_buyer','x','buyer',false),('uv','upgrade_vendor','x','vendor',true),('um','upgrade_moderator','x','moderator',false)",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,dispatch_days,delivery_content) VALUES('up','uv','Upgrade listing','Preserve these terms','Digital','Worldwide','digital',100000,500000000000,3,NULL,'retained reusable source')",
		"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('uo','ub','up','XMR',500000000000,'paid')",
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(t.Context(), tx, list) })
	var qty, stock int64
	var mode, content string
	var vacation bool
	var approved, moderatorApproved bool
	if err = db.QueryRow(`SELECT o.quantity,p.stock,p.digital_delivery_mode,p.delivery_content,u.vendor_vacation,u.vendor_approved,
 (SELECT vendor_approved FROM users WHERE id='um') FROM orders o JOIN products p ON p.id=o.product_id JOIN users u ON u.id=p.vendor_id WHERE o.id='uo'`).Scan(&qty, &stock, &mode, &content, &vacation, &approved, &moderatorApproved); err != nil {
		t.Fatal(err)
	}
	if qty != 1 || stock != 3 || mode != "reusable" || content != "retained reusable source" || !vacation || !approved || moderatorApproved {
		t.Fatalf("upgrade changed existing behavior: qty=%d stock=%d mode=%s content=%q vacation=%t approved=%t moderatorApproved=%t", qty, stock, mode, content, vacation, approved, moderatorApproved)
	}
	// Main's tables and metadata must coexist with the new private case tables.
	for _, q := range []string{
		"SELECT pgp_access_key FROM sessions LIMIT 0",
		"SELECT include_contents FROM xmpp_outbox LIMIT 0",
		"SELECT id FROM tickets LIMIT 0",
		"SELECT encrypted,recipient_fingerprint FROM deliveries LIMIT 0",
		"SELECT encrypted,recipient_fingerprint FROM order_delivery_updates LIMIT 0",
		"SELECT order_id FROM order_messages LIMIT 0",
		"SELECT order_id FROM images LIMIT 0",
		"SELECT order_id FROM digital_units LIMIT 0",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(q, err)
		}
		rows.Close()
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(t.Context(), tx, list) })
}
