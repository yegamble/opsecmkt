package market

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// withDatabaseTimeZone sets the test database's default TimeZone (as an operator's host PostgreSQL might have
// it) for connections opened from now on and restores the previous database-level setting on cleanup.
func withDatabaseTimeZone(t *testing.T, zone string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for isolated PostgreSQL integration tests")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var name, prior string
	if err = admin.QueryRow(`SELECT current_database(),COALESCE((SELECT split_part(c,'=',2) FROM pg_db_role_setting s,unnest(s.setconfig) c
		WHERE s.setrole=0 AND s.setdatabase=(SELECT oid FROM pg_database WHERE datname=current_database()) AND lower(c) LIKE 'timezone=%'),'')`).Scan(&name, &prior); err != nil {
		t.Fatal(err)
	}
	db := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec("ALTER DATABASE " + db + " SET timezone TO " + pgx.Identifier{zone}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		restore := "ALTER DATABASE " + db + " RESET timezone"
		if prior != "" {
			restore = "ALTER DATABASE " + db + " SET timezone TO " + pgx.Identifier{prior}.Sanitize()
		}
		if _, err := admin.Exec(restore); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	// Prove the setting reaches a connection that does not pin its own zone, so the test cannot pass vacuously.
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var got string
	if err = raw.QueryRow("SHOW timezone").Scan(&got); err != nil || got != zone {
		t.Fatalf("unpinned connection TimeZone=%q err=%v, want %q", got, err, zone)
	}
}

// A-82: rendered times must not reveal the database server's zone (and so the operator's UTC offset). The app
// pins TimeZone=UTC on every connection and labels displayed times " UTC".
func TestDisplayedTimesAreUTCWhateverTheDatabaseZone(t *testing.T) {
	withDatabaseTimeZone(t, "Pacific/Chatham") // UTC+12:45 or +13:45: never equal to UTC wall-clock time
	e := newTestApp(t)
	var zone string
	if err := e.DB.QueryRow("SHOW timezone").Scan(&zone); err != nil || zone != "UTC" {
		t.Errorf("app connection TimeZone=%q err=%v, want UTC", zone, err)
	}
	// A zone named in DATABASE_URL (either spelling, or as a -c option) does not override the pin either.
	override, err := OpenDB(os.Getenv("DATABASE_URL") + "&TimeZone=Asia%2FTokyo&options=-c%20timezone%3DAsia%2FTokyo")
	if err != nil {
		t.Fatal(err)
	}
	defer override.Close()
	if err = override.QueryRow("SHOW timezone").Scan(&zone); err != nil || zone != "UTC" {
		t.Errorf("DSN TimeZone=Asia/Tokyo gave connection TimeZone=%q err=%v, want UTC", zone, err)
	}
	buyerID, buyer := e.user("tz_buyer", "buyer")
	vendorID, _ := e.user("tz_vendor", "vendor")
	order := e.order(buyerID, e.product(vendorID, "digital"), "BTC", stateDraft)
	if _, err := e.DB.Exec("INSERT INTO messages(id,sender_id,recipient_id,body) VALUES($1,$2,$3,'tz message')", randomToken(), vendorID, buyerID); err != nil {
		t.Fatal(err)
	}
	// Expected strings are computed with explicit AT TIME ZONE conversions, independent of the session zone.
	var orderUTC, orderChatham, msgUTC, msgChatham string
	const utc, chatham = `'YYYY-MM-DD HH24:MI "UTC"'`, `'YYYY-MM-DD HH24:MI'`
	if err := e.DB.QueryRow(`SELECT to_char(created AT TIME ZONE 'UTC',`+utc+`),to_char(created AT TIME ZONE 'Pacific/Chatham',`+chatham+`) FROM orders WHERE id=$1`, order).Scan(&orderUTC, &orderChatham); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.QueryRow(`SELECT to_char(created AT TIME ZONE 'UTC',`+utc+`),to_char(created AT TIME ZONE 'Pacific/Chatham',`+chatham+`) FROM messages WHERE recipient_id=$1`, buyerID).Scan(&msgUTC, &msgChatham); err != nil {
		t.Fatal(err)
	}
	page := e.body("GET", "/order?id="+order, buyer, nil, 200)
	if !strings.Contains(page, "<dd>"+orderUTC+"</dd>") || strings.Contains(page, orderChatham) {
		t.Fatalf("order page does not show created time %q (database-zone time %q)", orderUTC, orderChatham)
	}
	page = e.body("GET", "/messages", buyer, nil, 200)
	if !strings.Contains(page, "<time>"+msgUTC+"</time>") || strings.Contains(page, msgChatham) {
		t.Fatalf("messages page does not show sent time %q (database-zone time %q)", msgUTC, msgChatham)
	}
}
