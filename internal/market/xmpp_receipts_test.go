package market

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

const xmppReceiptTestFingerprint = "0123456789ABCDEF0123456789ABCDEF01234567"

func xmppReceiptRow(e *testEnv, id string) (destination, fingerprint, outcome string, finished bool) {
	e.t.Helper()
	err := e.DB.QueryRow(`SELECT destination,recipient_fingerprint,outcome,finished_at IS NOT NULL FROM xmpp_outbox WHERE notification_id=$1`, id).
		Scan(&destination, &fingerprint, &outcome, &finished)
	if err != nil {
		e.t.Fatal(err)
	}
	return destination, fingerprint, outcome, finished
}

// A-717 (PRIV98-1): a finished receipt keeps its outcome and time, not the recipient's address or key fingerprint.
func TestXMPPReceiptKeepsNoDestinationAfterFinish(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		prepare    func(e *testEnv, uid, id string)
		sendErr    error
		wantSends  int
	}{
		{name: "sent", want: "sent", wantSends: 1},
		{name: "cancelled", want: "cancelled", prepare: func(e *testEnv, uid, _ string) {
			agExec(e, `UPDATE users SET xmpp_enabled=false,xmpp_version=xmpp_version+1 WHERE id=$1`, uid)
		}},
		{name: "failed-attempts", want: "failed", sendErr: errors.New("refused"), wantSends: 1, prepare: func(e *testEnv, _, id string) {
			agExec(e, `UPDATE xmpp_outbox SET attempts=$2 WHERE notification_id=$1`, id, xmppMaxAttempts-1)
		}},
		{name: "failed-expired", want: "failed", prepare: func(e *testEnv, _, id string) {
			agExec(e, `UPDATE notifications SET created=now()-interval '25 hours' WHERE id=$1`, id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestApp(t)
			uid, _ := e.user("xmpp_receipt", "buyer")
			xmppOptIn(e, uid)
			enableXMPPDelivery(e)
			id := enqueueXMPPTestNotice(e, uid, "personal", "receipt "+tc.name)
			agExec(e, `UPDATE xmpp_outbox SET recipient_fingerprint=$2 WHERE notification_id=$1`, id, xmppReceiptTestFingerprint)
			if tc.prepare != nil {
				tc.prepare(e, uid, id)
			}
			sends := 0
			worked, err := e.A.deliverXMPPNotification(context.Background(), func(context.Context, xmppConfig, string, string, string) error {
				sends++
				return tc.sendErr
			})
			if err != nil || !worked || sends != tc.wantSends {
				t.Fatalf("worked=%v err=%v sends=%d want %d", worked, err, sends, tc.wantSends)
			}
			destination, fingerprint, outcome, finished := xmppReceiptRow(e, id)
			if outcome != tc.want || !finished {
				t.Fatalf("outcome=%q finished=%v, want %q finished", outcome, finished, tc.want)
			}
			if destination != "" || fingerprint != "" {
				t.Fatalf("finished %s receipt keeps destination=%q fingerprint=%q", outcome, destination, fingerprint)
			}
		})
	}
}

// A-717: one worker pass leaves no old address in the outbox, including on a row waiting out a retry delay (up to
// an hour) that the delivery query does not yet select. A row whose address still stands waits and delivers unchanged.
func TestXMPPAddressChangeLeavesNoOldDestination(t *testing.T) {
	for _, change := range []struct{ name, newAddress string }{
		{"changed", "replacement@example.test"},
		{"removed", ""},
	} {
		t.Run(change.name, func(t *testing.T) {
			e := newTestApp(t)
			ctx := context.Background()
			uid, _ := e.user("xmpp_mover", "buyer")
			keeper, _ := e.user("xmpp_keeper", "buyer")
			xmppOptIn(e, uid)
			xmppOptIn(e, keeper)
			enableXMPPDelivery(e)
			old := agStr(e, `SELECT xmpp FROM users WHERE id=$1`, uid)
			kept := agStr(e, `SELECT xmpp FROM users WHERE id=$1`, keeper)

			due := enqueueXMPPTestNotice(e, uid, "personal", "due before the change")
			waiting := enqueueXMPPTestNotice(e, uid, "personal", "waiting before the change")
			keptWaiting := enqueueXMPPTestNotice(e, keeper, "personal", "waiting, address unchanged")
			agExec(e, `UPDATE xmpp_outbox SET recipient_fingerprint=$1 WHERE notification_id IN ($2,$3,$4)`, xmppReceiptTestFingerprint, due, waiting, keptWaiting)
			agExec(e, `UPDATE xmpp_outbox SET attempts=3,next_attempt=now()+interval '1 hour' WHERE notification_id IN ($1,$2)`, waiting, keptWaiting)

			agExec(e, `UPDATE users SET xmpp=$2,xmpp_enabled=$3,xmpp_version=xmpp_version+1 WHERE id=$1`, uid, change.newAddress, change.newAddress != "")
			var replacementNotice string
			if change.newAddress != "" {
				replacementNotice = enqueueXMPPTestNotice(e, uid, "personal", "after the change")
			}

			var sentTo []string
			send := func(_ context.Context, _ xmppConfig, to, _, _ string) error {
				sentTo = append(sentTo, to)
				return nil
			}
			e.A.xmppDeliveryPass(ctx, send)

			if n := agInt(e, `SELECT count(*) FROM xmpp_outbox WHERE destination=$1`, old); n != 0 {
				t.Fatalf("%d outbox rows keep the old address after one pass", n)
			}
			for _, id := range []string{due, waiting} {
				if destination, fingerprint, outcome, finished := xmppReceiptRow(e, id); outcome != "cancelled" || !finished || destination != "" || fingerprint != "" {
					t.Errorf("row %s: outcome=%q finished=%v destination=%q fingerprint=%q", id, outcome, finished, destination, fingerprint)
				}
			}
			if replacementNotice != "" {
				if len(sentTo) != 1 || sentTo[0] != change.newAddress {
					t.Fatalf("sends after the change: %q, want only %q", sentTo, change.newAddress)
				}
				if _, _, outcome, _ := xmppReceiptRow(e, replacementNotice); outcome != "sent" {
					t.Fatalf("replacement notice outcome %q", outcome)
				}
			} else if len(sentTo) != 0 {
				t.Fatalf("sends after the address was removed: %q", sentTo)
			}

			// The other account's waiting row is untouched and later delivers to its address, keeping its
			// fingerprint until the send finishes it.
			destination, fingerprint, outcome, finished := xmppReceiptRow(e, keptWaiting)
			if destination != kept || fingerprint != xmppReceiptTestFingerprint || outcome != "pending" || finished {
				t.Fatalf("pending row changed: destination=%q fingerprint=%q outcome=%q finished=%v", destination, fingerprint, outcome, finished)
			}
			agExec(e, `UPDATE xmpp_outbox SET next_attempt=now() WHERE notification_id=$1`, keptWaiting)
			sentTo = nil
			e.A.xmppDeliveryPass(ctx, send)
			if len(sentTo) != 1 || sentTo[0] != kept {
				t.Fatalf("pending row sent to %q, want %q", sentTo, kept)
			}
			if destination, fingerprint, outcome, _ := xmppReceiptRow(e, keptWaiting); outcome != "sent" || destination != "" || fingerprint != "" {
				t.Fatalf("after send: outcome=%q destination=%q fingerprint=%q", outcome, destination, fingerprint)
			}
		})
	}
}

// A-717: migration 154 blanks the address and fingerprint on rows already finished and leaves pending rows deliverable.
func TestMigration154ClearsFinishedXMPPDestinations(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var before, only []migration
	for _, m := range list {
		switch {
		case m.Version < 154:
			before = append(before, m)
		case m.Version == 154:
			only = append(only, m)
		}
	}
	if len(only) != 1 {
		t.Fatalf("migration 154: %d files", len(only))
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, before)
	})
	for _, q := range []string{
		`INSERT INTO users(id,handle,password_hash,role) VALUES('u1','m154','hash','buyer')`,
		`INSERT INTO notifications(id,user_id,body) VALUES('sent','u1','a'),('cancelled','u1','b'),('failed','u1','c'),('pending','u1','d')`,
		`INSERT INTO xmpp_outbox(notification_id,destination,preference_version,attempts,finished_at,outcome,recipient_fingerprint) VALUES
			('sent','old@example.test',1,1,now(),'sent','FPR'),
			('cancelled','old@example.test',1,0,now(),'cancelled','FPR'),
			('failed','old@example.test',1,12,now(),'failed','FPR'),
			('pending','now@example.test',2,3,NULL,'pending','FPR')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, only) })

	rows, err := db.Query(`SELECT notification_id||' '||destination||' '||recipient_fingerprint||' '||outcome||' '||attempts||' '||preference_version||' '||(finished_at IS NOT NULL)
		FROM xmpp_outbox ORDER BY notification_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cancelled   cancelled 0 1 true",
		"failed   failed 12 1 true",
		"pending now@example.test FPR pending 3 2 false",
		"sent   sent 1 1 true",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("after migration 154:\n got %q\nwant %q", got, want)
	}
}

// A-717: /canary says what a finished receipt holds, in the XMPP entry, not only that pending records are kept.
func TestCanaryDescribesFinishedXMPPReceipts(t *testing.T) {
	raw, err := os.ReadFile("web/templates/pages/canary.html")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(raw), "<strong>Optional XMPP alerts.</strong>")
	if !ok {
		t.Fatal("no XMPP entry on /canary")
	}
	entry, _, _ := strings.Cut(after, "</li>")
	for _, want := range []string{
		"A finished delivery leaves a receipt with its outcome and time, without your address or key fingerprint",
		"With retention enabled, the receipt is removed with its notification after 30 days",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("/canary XMPP entry does not say %q", want)
		}
	}
}
