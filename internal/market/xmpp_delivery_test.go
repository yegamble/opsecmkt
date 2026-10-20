package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func enableXMPPDelivery(e *testEnv) {
	e.t.Helper()
	sealed, err := e.A.seal("xmpp-password", "disposable-xmpp-test-password")
	if err != nil {
		e.t.Fatal(err)
	}
	value, err := json.Marshal(map[string]any{"enabled": true, "sender": "market@example.test", "host": "xmpp.example.test", "port": 5222, "tls_mode": "starttls", "password_sealed": sealed})
	if err != nil {
		e.t.Fatal(err)
	}
	agExec(e, `INSERT INTO settings(key,value) VALUES('xmpp_config',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(value))
}

func xmppOptIn(e *testEnv, id string) {
	e.t.Helper()
	agExec(e, `UPDATE users SET xmpp='recipient-'||substr(id,1,16)||'@example.test',xmpp_enabled=true,xmpp_version=xmpp_version+1 WHERE id=$1`, id)
}

func enqueueXMPPTestNotice(e *testEnv, id, audience, body string) string {
	e.t.Helper()
	_, err := insertScopedNotification(context.Background(), e.DB, id, body, "", audience)
	if err != nil {
		e.t.Fatal(err)
	}
	return agStr(e, `SELECT id FROM notifications WHERE user_id=$1 AND body=$2`, id, body)
}

func TestXMPPOutboxAtomicOptInAndAudienceMatrix(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		uid, _ := e.user("xmpp_matrix_"+role, role)
		enqueueXMPPTestNotice(e, uid, "personal", "before opt-in "+role)
		xmppOptIn(e, uid)
		enqueueXMPPTestNotice(e, uid, "personal", "sender disabled "+role)
	}
	if got := agInt(e, `SELECT count(*) FROM xmpp_outbox`); got != 0 {
		t.Fatalf("queued before sender enable: %d", got)
	}
	enableXMPPDelivery(e)
	for _, role := range []string{"buyer", "vendor", "moderator", "admin"} {
		uid := agStr(e, `SELECT id FROM users WHERE handle=$1`, "xmpp_matrix_"+role)
		for _, audience := range []string{"personal", "dispute", "admin"} {
			id := enqueueXMPPTestNotice(e, uid, audience, "matrix "+role+" "+audience)
			want := 0
			if audience == "personal" || audience == "dispute" && (role == "moderator" || role == "admin") || audience == "admin" && role == "admin" {
				want = 1
			}
			if got := agInt(e, `SELECT count(*) FROM xmpp_outbox WHERE notification_id=$1`, id); got != want {
				t.Errorf("%s/%s queued %d want %d", role, audience, got, want)
			}
		}
		tx, err := e.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = insertNotification(ctx, tx, uid, "rolled back "+role, ""); err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	if got := agInt(e, `SELECT count(*) FROM xmpp_outbox`); got != 7 {
		t.Fatalf("outbox count %d want 7; rollback or audience leaked", got)
	}
}

func TestXMPPDeliveryRechecksRolePreferencesAndAccountState(t *testing.T) {
	for _, change := range []string{"demoted-admin", "demoted-moderator", "opted-out", "destination-changed", "version-changed", "suspended", "closed", "restricted", "sender-disabled", "expired"} {
		t.Run(change, func(t *testing.T) {
			e := newTestApp(t)
			uid, _ := e.user("xmpp_recheck", "admin")
			xmppOptIn(e, uid)
			enableXMPPDelivery(e)
			audience := "admin"
			if change == "demoted-moderator" {
				agExec(e, `UPDATE users SET role='moderator' WHERE id=$1`, uid)
				audience = "dispute"
			}
			id := enqueueXMPPTestNotice(e, uid, audience, "must stay private "+change)
			switch change {
			case "demoted-admin":
				agExec(e, `UPDATE users SET role='moderator' WHERE id=$1`, uid)
			case "demoted-moderator":
				agExec(e, `UPDATE users SET role='buyer' WHERE id=$1`, uid)
			case "opted-out":
				agExec(e, `UPDATE users SET xmpp_enabled=false WHERE id=$1`, uid)
			case "destination-changed":
				agExec(e, `UPDATE users SET xmpp='another@example.test' WHERE id=$1`, uid)
			case "version-changed":
				agExec(e, `UPDATE users SET xmpp_version=xmpp_version+1 WHERE id=$1`, uid)
			case "suspended":
				agExec(e, `UPDATE users SET suspended_at=now() WHERE id=$1`, uid)
			case "closed":
				agExec(e, `UPDATE users SET closed_at=now() WHERE id=$1`, uid)
			case "restricted":
				agExec(e, `UPDATE users SET retention_restricted_at=now() WHERE id=$1`, uid)
			case "sender-disabled":
				agExec(e, `UPDATE settings SET value=jsonb_set(value::jsonb,'{enabled}','false')::text WHERE key='xmpp_config'`)
			case "expired":
				agExec(e, `UPDATE notifications SET created=now()-interval '25 hours' WHERE id=$1`, id)
			}
			worked, err := e.A.deliverXMPPNotification(context.Background(), func(context.Context, xmppConfig, string, string, string) error {
				t.Fatal("unauthorized XMPP delivery invoked")
				return nil
			})
			if err != nil || !worked {
				t.Fatalf("recheck failed: worked=%v err=%v", worked, err)
			}
			want := "cancelled"
			if change == "expired" {
				want = "failed"
			}
			if got := agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, id); got != want {
				t.Fatalf("outcome=%s want %s", got, want)
			}
		})
	}
}

func TestXMPPDeliveryMatchesFrontendAndRetriesStableID(t *testing.T) {
	e := newTestApp(t)
	uid, session := e.user("xmpp_body", "moderator")
	xmppOptIn(e, uid)
	enableXMPPDelivery(e)
	body := "Order 12345678: payment confirmed & ready to ship."
	id := enqueueXMPPTestNotice(e, uid, "personal", body)
	var called int
	send := func(ctx context.Context, cfg xmppConfig, to, messageID, text string) error {
		called++
		if cfg.Sender != "market@example.test" || cfg.Password != "disposable-xmpp-test-password" || to != "recipient-"+uid[:16]+"@example.test" || messageID != "market-"+id || text != body {
			t.Fatal("XMPP body, destination, ID or sender differed from committed frontend notification")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("send has no timeout")
		}
		if called == 1 {
			return errors.New("transient connection failure")
		}
		return nil
	}
	for attempt := 1; attempt <= 2; attempt++ {
		worked, err := e.A.deliverXMPPNotification(context.Background(), send)
		if err != nil || !worked {
			t.Fatalf("attempt %d: worked=%v err=%v", attempt, worked, err)
		}
		if attempt == 1 {
			if agInt(e, `SELECT attempts FROM xmpp_outbox WHERE notification_id=$1`, id) != 1 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, id) != "pending" {
				t.Fatal("transient failure not scheduled for retry")
			}
			if worked, err = e.A.deliverXMPPNotification(context.Background(), send); err != nil || worked {
				t.Fatal("retried before backoff elapsed")
			}
			agExec(e, `UPDATE xmpp_outbox SET next_attempt=now() WHERE notification_id=$1`, id)
		}
	}
	if called != 2 || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, id) != "sent" {
		t.Fatal("send receipt not persisted")
	}
	page := e.body("GET", "/notifications", session, nil, 200)
	if !strings.Contains(page, "payment confirmed &amp; ready to ship.") {
		t.Fatal("frontend does not show matching body")
	}
	agExec(e, `DELETE FROM notifications WHERE id=$1`, id)
	if agInt(e, `SELECT count(*) FROM xmpp_outbox`) != 0 {
		t.Fatal("retention deletion left XMPP receipt")
	}
}

func TestXMPPDeliveryCompetingWorkersAndMaximumRetries(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("xmpp_workers", "buyer")
	xmppOptIn(e, uid)
	enableXMPPDelivery(e)
	enqueueXMPPTestNotice(e, uid, "personal", "competing workers")
	entered, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worked, err := e.A.deliverXMPPNotification(context.Background(), func(context.Context, xmppConfig, string, string, string) error {
			close(entered)
			<-release
			return nil
		})
		if err != nil || !worked {
			t.Errorf("first worker: %v %v", worked, err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("worker did not reach sender")
	}
	worked, err := e.A.deliverXMPPNotification(context.Background(), func(context.Context, xmppConfig, string, string, string) error {
		t.Error("duplicate send from competing worker")
		return nil
	})
	close(release)
	wg.Wait()
	if err != nil || worked {
		t.Fatalf("competing worker did not skip claimed row: %v %v", worked, err)
	}
	id := enqueueXMPPTestNotice(e, uid, "personal", "retry exhaustion")
	agExec(e, `UPDATE xmpp_outbox SET attempts=$2 WHERE notification_id=$1`, id, xmppMaxAttempts-1)
	worked, err = e.A.deliverXMPPNotification(context.Background(), func(context.Context, xmppConfig, string, string, string) error {
		return fmt.Errorf("do not persist this server's private error text")
	})
	if err != nil || !worked || agStr(e, `SELECT outcome FROM xmpp_outbox WHERE notification_id=$1`, id) != "failed" || agInt(e, `SELECT attempts FROM xmpp_outbox WHERE notification_id=$1`, id) != xmppMaxAttempts {
		t.Fatalf("retry exhaustion: %v %v", worked, err)
	}
}

func TestXMPPInvalidOptionalConfigDoesNotBlockFrontend(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("xmpp_corrupt_config", "buyer")
	xmppOptIn(e, uid)
	agExec(e, `INSERT INTO settings(key,value) VALUES('xmpp_config','broken-json')`)
	id := enqueueXMPPTestNotice(e, uid, "personal", "frontend notice survives broken optional delivery configuration")
	if agInt(e, `SELECT count(*) FROM notifications WHERE id=$1`, id) != 1 || agInt(e, `SELECT count(*) FROM xmpp_outbox`) != 0 {
		t.Fatal("invalid optional delivery config disrupted frontend notice")
	}
}

func TestXMPPUpgradeRestrictsHistoricalStaffNoticesWithoutSending(t *testing.T) {
	db, err := OpenDB(testSchemaDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var beforeXMPP []migration
	for _, m := range migrations {
		if m.Version < 131 {
			beforeXMPP = append(beforeXMPP, m)
		}
	}
	if err = applyMigrations(ctx, tx, beforeXMPP); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "moderator", "buyer"} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,handle,password_hash,role,xmpp) VALUES($1,$1,'unused',$1,'old@example.test')`, role); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) VALUES($1,$1,'Old personal or staff notice'),($1||'-payment',$1,'Payment review needed for order 12345678: private operator details')`, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body) VALUES('former-admin-wallet','buyer','Monero escrow wallet private-wallet: operator detail')`); err != nil {
		t.Fatal(err)
	}
	if err = applyMigrations(ctx, tx, migrations); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM xmpp_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("upgrade queued historical notices: %d %v", count, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM users WHERE xmpp_enabled`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("upgrade implicitly opted legacy addresses in: %d %v", count, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM notifications WHERE audience='admin'`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("legacy staff boundary missing: %d %v", count, err)
	}
}
