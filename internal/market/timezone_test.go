package market

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// withDatabaseTimeZone sets the default TimeZone of the test's own database (DATABASE_URL, from newTestApp), as an
// operator's host PostgreSQL might have it, for connections opened from now on and restores the previous
// database-level setting on cleanup.
func withDatabaseTimeZone(t *testing.T, zone string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
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
	e := newTestApp(t)
	withDatabaseTimeZone(t, "Pacific/Chatham") // UTC+12:45 or +13:45: never equal to UTC wall-clock time
	e.restart()                                // the App's connections open under the database's new zone
	var zone string
	if err := e.DB.QueryRow("SHOW timezone").Scan(&zone); err != nil || zone != "UTC" {
		t.Errorf("app connection TimeZone=%q err=%v, want UTC", zone, err)
	}
	// pgx stores runtime parameters in a map. Repeated fresh pools catch a case-variant DSN pin being
	// nondeterministically applied after OpenDB's UTC value, including a timezone supplied through options.
	for i := 0; i < 20; i++ {
		dsn := os.Getenv("DATABASE_URL") + "&TimeZone=Asia%2FTokyo&timezone=Pacific%2FChatham&options=-c%20timezone%3DAmerica%2FNew_York"
		override, openErr := OpenDB(dsn)
		if openErr != nil {
			t.Fatalf("open hostile timezone DSN on attempt %d: %v", i+1, openErr)
		}
		if queryErr := override.QueryRow("SHOW timezone").Scan(&zone); queryErr != nil || zone != "UTC" {
			override.Close()
			t.Fatalf("hostile timezone DSN attempt %d gave TimeZone=%q err=%v, want UTC", i+1, zone, queryErr)
		}
		if closeErr := override.Close(); closeErr != nil {
			t.Fatalf("close hostile timezone DSN attempt %d: %v", i+1, closeErr)
		}
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

// A-111 pins the rendered labels as well as the connection default. Each marker comes from a real PostgreSQL row;
// none is computed by repeating the production formatting query in the assertion.
func TestRenderedTimestampSurfacesAreUTCWhateverTheDatabaseZone(t *testing.T) {
	e := newTestApp(t)
	withDatabaseTimeZone(t, "Pacific/Chatham")
	e.restart()

	buyerID, buyer := e.user("tz_surface_buyer", "buyer")
	vendorID, vendor := e.user("tz_surface_vendor", "vendor")
	_, admin := e.user("tz_surface_admin", "admin")
	_, moderator := e.user("tz_surface_mod", "moderator")
	product := e.product(vendorID, "digital")
	order := e.order(buyerID, product, "BTC", stateDisputed)
	const created = "2025-04-05 06:07 UTC"
	const changed = "2025-04-06 07:08 UTC"
	const notificationAt = "2025-04-07 08:09 UTC"
	const auditAt = "2025-04-08 09:10 UTC"
	const disputeAt = "2025-04-09 10:11 UTC"
	const replyAt = "2025-04-10 11:12 UTC"
	const deliveryAt = "2025-04-11 12:13 UTC"
	const listingAt = "2025-04-12 13:14 UTC"
	const payoutAt = "2025-04-13 14:15 UTC"
	const pollAt = "2025-04-14 15:16:17 UTC"
	const reviewAt = "2025-04-15 16:17 UTC"

	if _, err := e.DB.Exec(`UPDATE orders SET created=$2,updated=$3 WHERE id=$1`, order, "2025-04-05 06:07:00+00", "2025-04-06 07:08:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO notifications(id,user_id,body,created) VALUES($1,$2,'Timezone fixture notification',$3)`, randomToken(), buyerID, "2025-04-07 08:09:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO audit_events(user_id,action,created) VALUES($1,'Timezone fixture audit',$2)`, buyerID, "2025-04-08 09:10:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO disputes(id,order_id,reason,created) VALUES($1,$2,'The package never arrived and the seller has not replied.',$3)`, randomToken(), order, "2025-04-09 10:11:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created)
		VALUES($1,'disputed','disputed',$2,'Dispute reply: The parcel was shipped with tracking and the carrier confirms delivery.',$3)`, order, vendorID, "2025-04-10 11:12:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`UPDATE products SET updated=$2 WHERE id=$1`, product, "2025-04-12 13:14:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO deliveries(order_id,content,created) VALUES($1,'Timezone fixture delivery',$2)`, order, "2025-04-11 12:13:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,updated)
		VALUES($1,'release',$2,'BTC',100000,'fixture-payout-address','sent',$3)`, order, vendorID, "2025-04-13 14:15:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO payment_status(currency,network,last_poll) VALUES('BTC','testnet3',$1)
		ON CONFLICT(currency) DO UPDATE SET network=EXCLUDED.network,last_poll=EXCLUDED.last_poll`, "2025-04-14 15:16:17+00"); err != nil {
		t.Fatal(err)
	}
	e.A.payMu.Lock()
	if e.A.payments == nil {
		e.A.payments = make(map[string]PaymentProvider)
	}
	e.A.payments["BTC"] = &stubProvider{currency: "BTC"}
	e.A.payMu.Unlock()
	if _, err := e.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged,after_close,created)
		VALUES($1,'BTC','timezone-review-tx',0,'fixture-payment-address',5000,1,true,true,$2)`, order, "2025-04-15 16:17:00+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO order_events(order_id,from_state,to_state,note,created)
		VALUES($1,'disputed','disputed','Late deposit timezone-review-tx output 0 (0.00005000 BTC) arrived after the order closed. Moderator review required.',$2)`, order, "2025-04-15 16:17:00+00"); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		name, path, token, marker string
	}{
		{"notifications", "/notifications", buyer, notificationAt},
		{"audit activity", "/account", buyer, auditAt},
		{"dispute list", "/disputes", buyer, disputeAt},
		{"dispute reply", "/order?id=" + order, buyer, replyAt},
		{"delivery and order events", "/order?id=" + order, buyer, deliveryAt},
		{"listing edit", "/listing-edit?id=" + product, vendor, listingAt},
		{"payout updated", "/admin", admin, payoutAt},
		{"provider poll", "/admin", admin, pollAt},
		{"payment review", "/moderator", moderator, reviewAt},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			page := e.body("GET", check.path, check.token, nil, 200)
			if !strings.Contains(page, check.marker) {
				t.Fatalf("rendered %s page missing UTC timestamp %q", check.name, check.marker)
			}
		})
	}

	page := e.body("GET", "/order?id="+order, buyer, nil, 200)
	createdLabel := "<dt>Created</dt>\n<dd>" + created + "</dd>"
	changedLabel := "<dt>Last change</dt>\n<dd>" + changed + "</dd>"
	if !strings.Contains(page, createdLabel) || !strings.Contains(page, changedLabel) {
		t.Fatalf("order page did not preserve distinct Created (%q) and Last change (%q) labels", created, changed)
	}
}
