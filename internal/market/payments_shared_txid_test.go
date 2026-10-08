package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMarkSentSharedTxidNeedsConfirmation(t *testing.T) {
	p := newPayEnv(t)
	dispute, mod := p.disputedOrder(200000)
	p.check(p.do("POST", "/resolve", mod, resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"release"}, "resolution": {"Release the price; return the overpayment."}}, 200000)), 303)
	release := p.payoutID(p.order)
	var returned string
	if err := p.DB.QueryRow("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", p.order).Scan(&returned); err != nil {
		t.Fatal(err)
	}
	txid := strings.Repeat("ab", 32)
	if code, body := p.payoutAction(p.adminSess, release, "sent", strings.ToUpper(txid), testPassword); code != 303 {
		t.Fatalf("unique release transaction: %d %s", code, body)
	}
	type snapshot struct {
		state, txid, err      string
		ambiguous             bool
		updated               time.Time
		events, audits, notes int
	}
	state := func() (s snapshot) {
		t.Helper()
		if err := p.DB.QueryRow(`SELECT state,txid,error,send_ambiguous,updated,
   (SELECT count(*) FROM order_events),(SELECT count(*) FROM audit_events),(SELECT count(*) FROM notifications)
   FROM payouts WHERE id=$1`, returned).Scan(&s.state, &s.txid, &s.err, &s.ambiguous, &s.updated, &s.events, &s.audits, &s.notes); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	for name, fields := range map[string]url.Values{
		"neither":      {},
		"number only":  {"shared_payout_id": {release}},
		"tick only":    {"shared_txid": {"confirmed"}},
		"wrong payout": {"shared_payout_id": {returned}, "shared_txid": {"confirmed"}},
		"wrong tick":   {"shared_payout_id": {release}, "shared_txid": {"true"}},
	} {
		t.Run(name, func(t *testing.T) {
			fields.Set("payout_id", returned)
			fields.Set("op", "sent")
			fields.Set("txid", strings.ToUpper(txid))
			fields.Set("password", testPassword)
			w := p.do("POST", "/admin/payout", p.adminSess, fields)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "already recorded for payout "+release) {
				t.Fatalf("shared transaction without named confirmation: %d %s", w.Code, w.Body)
			}
			if after := state(); after != before {
				t.Fatalf("refused shared transaction changed payout/history: %+v -> %+v", before, after)
			}
		})
	}
	page := p.page("/admin", p.adminSess)
	mustContain(t, page, `name="shared_payout_id"`, `name="shared_txid" value="confirmed"`, "I checked both payments in the wallet")
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {returned}, "op": {"sent"}, "txid": {txid}, "password": {testPassword}, "shared_payout_id": {release}, "shared_txid": {"confirmed"}})
	p.check(w, 303)
	after := state()
	if after.state != "sent" || after.txid != txid || after.err != "" || after.ambiguous || after.events != before.events+1 || after.audits != before.audits+1 || after.notes != before.notes+1 {
		t.Fatalf("confirmed shared transaction: %+v -> %+v", before, after)
	}
	if p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "%administrator confirmed this transaction also pays payout "+release+"%") != 1 {
		t.Fatal("shared transaction confirmation not audited with other payout number")
	}
	if p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", p.buyer.ID, "%Transaction ID: "+txid) != 1 {
		t.Fatal("buyer not notified of the confirmed return transaction")
	}
	if p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE $2", p.order, "%"+txid+"%") != 0 {
		t.Fatal("shared transaction ID leaked into both parties' order history")
	}
	if code, _ := p.payoutAction(p.adminSess, returned, "sent", txid, testPassword); code != 409 {
		t.Fatalf("repeated mark sent: %d", code)
	}
	p.poll()
	if len(p.fake.Sends()) != 0 {
		t.Fatal("manually recorded transaction caused a wallet send")
	}
}

// Different administrators cannot both record first use of an ID before seeing each other's commit.
func TestMarkSentConcurrentSharedTxidCannotBypassConfirmation(t *testing.T) {
	p := newPayEnv(t)
	_, secondAdmin := p.user("shared_txid_admin_"+randomToken()[:6], "admin")
	orders := []string{p.completedWithPayout("shared-deposit-a"), p.completedWithPayout("shared-deposit-b")}
	ids := []string{p.payoutID(orders[0]), p.payoutID(orders[1])}
	admins := []string{p.adminSess, secondAdmin}
	txid := strings.Repeat("cd", 32)
	results := agRace(2, func(i int) (int, string) { return p.payoutAction(admins[i], ids[i], "sent", txid, testPassword) })
	counts := agTally(t, results, 303, 409, 503)
	if counts[303] != 1 || p.count("SELECT count(*) FROM payouts WHERE lower(txid)=$1 AND state='sent'", txid) != 1 {
		t.Fatalf("two first uses bypassed shared-ID confirmation: %+v", counts)
	}
	winner := 0
	if results[1].code == 303 {
		winner = 1
	}
	loser := 1 - winner
	if state, _, _, _ := p.payout(orders[loser]); state != "blocked" {
		t.Fatalf("unconfirmed payout moved to %s", state)
	}
	if code, body := p.payoutAction(admins[loser], ids[loser], "sent", txid, testPassword); code != 409 || !strings.Contains(body, "already recorded for payout "+ids[winner]) {
		t.Fatalf("retry bypassed confirmation: %d %s", code, body)
	}
	w := p.do("POST", "/admin/payout", admins[loser], url.Values{"payout_id": {ids[loser]}, "op": {"sent"}, "txid": {txid}, "password": {testPassword}, "shared_payout_id": {ids[winner]}, "shared_txid": {"confirmed"}})
	p.check(w, 303)
	if p.count("SELECT count(*) FROM payouts WHERE txid=$1 AND state='sent'", txid) != 2 || len(p.fake.Sends()) != 0 {
		t.Fatal("explicit shared-ID confirmation did not record both without sending")
	}
}

func TestMarkSentSharedTxidBusyLeavesPayoutUnchanged(t *testing.T) {
	p := newPayEnv(t)
	order := p.completedWithPayout("shared-lock-deposit")
	id := p.payoutID(order)
	txid := strings.Repeat("ef", 32)
	lock, err := p.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec("SELECT pg_advisory_xact_lock(hashtextextended('payout-txid:' || $1,0))", txid); err != nil {
		t.Fatal(err)
	}
	before := p.count("SELECT count(*) FROM audit_events")
	beforeNotes := p.count("SELECT count(*) FROM notifications")
	beforeEvents := p.count("SELECT count(*) FROM order_events")
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {id}, "op": {"sent"}, "txid": {txid}, "password": {testPassword}})
	if w.Code != 503 || w.Header().Get("Retry-After") != busyRetryAfter || !strings.HasPrefix(w.Body.String(), errBusyMsg) {
		t.Fatalf("busy shared-ID record: %d %s", w.Code, w.Body)
	}
	if state, got, _, _ := p.payout(order); state != "blocked" || got != "" || p.count("SELECT count(*) FROM audit_events") != before || p.count("SELECT count(*) FROM notifications") != beforeNotes || p.count("SELECT count(*) FROM order_events") != beforeEvents {
		t.Fatal("busy record changed payout or audit")
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if code, body := p.payoutAction(p.adminSess, id, "sent", txid, testPassword); code != 303 {
		t.Fatalf("record after busy lock released: %d %s", code, body)
	}
}
