package market

import (
	"context"
	"database/sql"
	"html"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-480: a Monero escrow deposit the database refused as another order's escrow account (payments_xmr_escrow_idx) is
// recorded once per order and transaction (xmr_escrow_collisions, migration 090) with the order note and the staff
// notice of before; while its record is open it is an escrow staff alert (escrowStaffAlert): non-party moderators and
// administrators read the order, the moderation desk lists it, and a staff member records what was done on the order
// page (POST /moderator/xmr-collision), which clears it. DB integration against the fake monero-wallet-rpc (simulated
// RPC, not chain evidence).

const collisionLine = "was not recorded: the database refused it as another order's escrow account"

// refusePass runs one Monero escrow pass in which the database refuses every ledger row of order's escrow account as
// another order's (payments_xmr_escrow_idx), as TestXMREscrowCollisionFlaggedPerRow makes it: the watcher reads the
// slot as order's, and before it writes the ledger the slot comes to name another order, as no code path can do. The
// slot names order again afterwards.
func (x *xmrEscrowEnv) refusePass(order string) {
	x.t.Helper()
	other := x.draft(x.digital(x.vendor))
	agExec(x.testEnv, "ALTER TABLE escrows DROP CONSTRAINT IF EXISTS escrows_xmr_slot")
	flip := func(from, to string) {
		agExec(x.testEnv, "ALTER TABLE xmr_escrow_slots DISABLE TRIGGER xmr_escrow_slots_kept")
		agExec(x.testEnv, "UPDATE xmr_escrow_slots SET order_id=$2 WHERE order_id=$1", from, to)
		agExec(x.testEnv, "ALTER TABLE xmr_escrow_slots ENABLE TRIGGER xmr_escrow_slots_kept")
	}
	x.fake.hook("get_transfers", func() { x.fake.hook("get_transfers", nil); flip(order, other) })
	err := x.poll()
	flip(other, order)
	if err != nil {
		x.t.Fatalf("the refused pass: %v", err)
	}
}

// collisions lists order's refused-deposit records as "txid open|disposed", oldest first.
func (x *xmrEscrowEnv) collisions(order string) []string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT txid||' '||CASE WHEN disposed_at IS NULL THEN 'open' ELSE 'disposed' END FROM xmr_escrow_collisions WHERE order_id=$1 ORDER BY id", order)
}

// collisionNotes counts the order notes of order's refused deposits and the staff notices of them to the environment's
// administrator (every staff member who is not a party gets one per note).
func (x *xmrEscrowEnv) collisionNotes(order string) (notes, notices int) {
	x.t.Helper()
	notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, collisionLine)
	notices = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0",
		x.admin, "Payment review needed for order "+shortID(order), collisionLine)
	return
}

// orderCodes is the status of GET /order?id=order for each session.
func (x *xmrEscrowEnv) orderCodes(order string, sessions ...string) []int {
	x.t.Helper()
	var out []int
	for _, s := range sessions {
		out = append(out, x.do("GET", "/order?id="+order, s, nil).Code)
	}
	return out
}

// deskAlertIn is the Escrow alerts entry for order in the moderation desk page desk, or "" when it is not listed.
func deskAlertIn(desk, order string) string {
	_, section, ok := strings.Cut(desk, `<section id="escrow-alerts"`)
	if !ok {
		return ""
	}
	section, _, _ = strings.Cut(section, "</section>")
	_, entry, ok := strings.Cut(section, `<li><a class="text-link mono" href="/order?id=`+order+`">`)
	if !ok {
		return ""
	}
	entry, _, _ = strings.Cut(entry, "</li>")
	return entry
}

// refusedSection is the order page's staff-only Refused Monero deposits section, or "" when it has none.
func refusedSection(page string) string {
	_, s, ok := strings.Cut(page, `<section class="panel" id="refused-deposits"`)
	if !ok {
		return ""
	}
	s, _, _ = strings.Cut(s, "</section>")
	return s
}

// recordCollision posts the disposition of record id as session (password testPassword unless kv sets one).
func (x *xmrEscrowEnv) recordCollision(session, id, note string, kv ...string) (int, string) {
	x.t.Helper()
	w := x.do("POST", "/moderator/xmr-collision", session, form(append([]string{"collision_id", id, "note", note, "password", testPassword}, kv...)...))
	if w.Code == 303 {
		return w.Code, w.Header().Get("Location")
	}
	return w.Code, w.Body.String()
}

// collisionAudits counts the audit rows of actor recording a refused deposit's disposition.
func (x *xmrEscrowEnv) collisionAudits(actor string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Recorded what was done about refused Monero escrow deposit %'", actor)
}

// One record and one note and notice per refused deposit, however many passes meet it; a second transaction refused
// for the same order is a second record with its own note and notice. Nothing is written to the ledger.
func TestXMREscrowCollisionRecordedOnce(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	_, account := x.fake.accountOf(addr)
	first, second := hex64("refused one "+order), hex64("refused two "+order)
	x.fake.payAddress(addr, first, xmrListingPrice, 10)
	for range 3 {
		x.refusePass(order)
	}
	if got := x.collisions(order); !slices.Equal(got, []string{first + " open"}) {
		t.Fatalf("records after three passes: %q", got)
	}
	if notes, notices := x.collisionNotes(order); notes != 1 || notices != 1 || x.notified(x.admin, collisionLine) != 1 {
		t.Fatalf("after three passes: %d notes, %d staff notices, admin %d", notes, notices, x.notified(x.admin, collisionLine))
	}
	want := "escrow-202610-01 " + strconv.FormatInt(account, 10) + " false"
	if got := agStrings(t, x.testEnv, "SELECT wallet_name||' '||account||' '||txid_truncated FROM xmr_escrow_collisions WHERE order_id=$1", order); !slices.Equal(got, []string{want}) {
		t.Fatalf("the record: %q, want %q", got, want)
	}
	x.fake.payAddress(addr, second, xmrListingPrice, 10)
	for range 2 {
		x.refusePass(order)
	}
	if got := x.collisions(order); !slices.Equal(got, []string{first + " open", second + " open"}) {
		t.Fatalf("records after a second refused transaction: %q", got)
	}
	if notes, notices := x.collisionNotes(order); notes != 2 || notices != 2 {
		t.Fatalf("after a second refused transaction: %d notes, %d staff notices", notes, notices)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", order); n != 0 || x.state(order) != stateAwaitingPayment {
		t.Fatalf("the refused deposits were recorded: %d rows, order %s", n, x.state(order))
	}
}

// While a record is open, moderators and administrators who are not parties read the order and see the record with its
// form; the parties see no record; an outsider is refused. The desk lists the order with its reason. The disposition is
// refused to a non-staff user, a staff party, a short or long reason, a wrong password, an unknown record and a post
// without its CSRF token, all writing nothing; recorded once (audit row, no order event, no party notice), it clears
// the alert: staff are refused the order again, the desk no longer lists it, and a repeat is 409. Passes that meet the
// same deposit again open nothing.
func TestXMREscrowCollisionStaffAlert(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("collmod_"+randomToken()[:6], "moderator")
	adminID, admin := x.user("colladmin_"+randomToken()[:6], "admin")
	_, stranger := x.user("collstranger_"+randomToken()[:6], "buyer")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	tx := hex64("refused alert " + order)
	x.fake.payAddress(addr, tx, xmrListingPrice, 10)
	if got := x.orderCodes(order, mod, admin, stranger); !slices.Equal(got, []int{404, 404, 404}) {
		t.Fatalf("before the refusal: %v", got)
	}
	x.refusePass(order)
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order); n != 0 {
		t.Fatalf("%d flagged deposits: the test needs an order no flag opens", n)
	}
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	if got := x.orderCodes(order, mod, admin, stranger); !slices.Equal(got, []int{200, 200, 404}) {
		t.Fatalf("while the record is open (moderator, administrator, outsider): %v", got)
	}
	for _, sess := range []string{mod, admin} {
		page := x.page("/order?id="+order, sess)
		sec := refusedSection(page)
		mustContain(t, sec, tx, "escrow-202610-01", "Open", `action="/moderator/xmr-collision"`, `name="collision_id" value="`+id+`"`, `name="note"`, `name="password"`)
		help := staffHelp(page)
		mustContain(t, help, "because its escrow has an open staff alert", "a Monero deposit was refused as another order", `href="/moderator#escrow-alerts"`)
		if e := deskAlertIn(x.page("/moderator", sess), order); !strings.Contains(e, "a Monero deposit was refused as another order") {
			t.Fatalf("the desk's entry: %q", e)
		}
	}
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		if sec := refusedSection(x.page("/order?id="+order, sess)); sec != "" {
			t.Fatalf("a party sees the record: %s", sec)
		}
	}

	reason := "Checked the wallet: refunded by hand in " + hex64("hand refund") + "."
	r := httptest.NewRequest("POST", "/moderator/xmr-collision", strings.NewReader(form("collision_id", id, "note", reason, "password", testPassword).Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: mod})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("post without CSRF: %d", w.Code)
	}
	for name, sess := range map[string]string{"an outsider": stranger, "the buyer": x.buyerSess, "the vendor": x.vendorSess} {
		if code, body := x.recordCollision(sess, id, reason); code != 403 {
			t.Fatalf("%s recorded the disposition: %d %s", name, code, body)
		}
	}
	for _, c := range []struct {
		note string
		kv   []string
		code int
	}{
		{"too short", nil, 400},
		{strings.Repeat("x", 501), nil, 400},
		{reason, []string{"password", "wrong-password-123"}, 401},
		{reason, []string{"collision_id", "999999"}, 404},
		{reason, []string{"collision_id", "nope"}, 404},
	} {
		if code, body := x.recordCollision(mod, id, c.note, c.kv...); code != c.code {
			t.Fatalf("%.20q %v: %d, want %d: %s", c.note, c.kv, code, c.code, body)
		}
	}
	// A moderator who is a party to the order (here its vendor) is refused too.
	agExec(x.testEnv, "UPDATE users SET role='moderator' WHERE id=$1", x.vendor)
	if code, body := x.recordCollision(x.vendorSess, id, reason); code != 403 || !strings.Contains(body, "party") {
		t.Fatalf("a staff party: %d %s", code, body)
	}
	agExec(x.testEnv, "UPDATE users SET role='vendor' WHERE id=$1", x.vendor)
	if got := x.collisions(order); !slices.Equal(got, []string{tx + " open"}) || x.collisionAudits(modID)+x.collisionAudits(x.vendor) != 0 {
		t.Fatalf("refused dispositions changed something: %q", got)
	}

	events := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order)
	partyNotices := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	if code, loc := x.recordCollision(mod, id, "  "+reason+"  "); code != 303 || loc != "/moderator" {
		t.Fatalf("the disposition: %d %s", code, loc)
	}
	if got := agStrings(t, x.testEnv, "SELECT disposition_note||'|'||disposed_by||'|'||(disposed_at IS NOT NULL) FROM xmr_escrow_collisions WHERE id=$1::bigint", id); !slices.Equal(got, []string{reason + "|" + modID + "|true"}) {
		t.Fatalf("the recorded disposition: %q", got)
	}
	if n := x.collisionAudits(modID); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	if got := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", modID)[0]; !strings.Contains(got, tx) || !strings.Contains(got, shortID(order)) || !strings.Contains(got, "(confirmed with password)") {
		t.Fatalf("the audit row: %q", got)
	}
	if e, n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", order), agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id IN ($1,$2)", x.buyer, x.vendor); e != events || n != partyNotices {
		t.Fatalf("the disposition wrote order events %d (was %d) or party notices %d (was %d)", e, events, n, partyNotices)
	}
	if got := x.orderCodes(order, mod, admin); !slices.Equal(got, []int{404, 404}) {
		t.Fatalf("staff after the disposition: %v", got)
	}
	if e := deskAlertIn(x.page("/moderator", mod), order); e != "" {
		t.Fatalf("the desk still lists the order: %q", e)
	}
	if code, body := x.recordCollision(admin, id, "A second disposition, long enough."); code != 409 || !strings.Contains(body, "already recorded") {
		t.Fatalf("a second disposition: %d %s", code, body)
	}
	if n := x.collisionAudits(adminID); n != 0 {
		t.Fatalf("the refused repeat wrote %d audit rows", n)
	}
	// The same deposit met again opens nothing; staff who read the order for another reason (a dispute) see the record
	// and its disposition, with no form.
	x.refusePass(order)
	if got, codes := x.collisions(order), x.orderCodes(order, mod); !slices.Equal(got, []string{tx + " disposed"}) || codes[0] != 404 {
		t.Fatalf("after the same deposit was met again: %q, staff %v", got, codes)
	}
	if notes, notices := x.collisionNotes(order); notes != 1 || notices != 1 {
		t.Fatalf("after the disposition and another pass: %d notes, %d notices", notes, notices)
	}
	agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", order)
	sec := refusedSection(x.page("/order?id="+order, admin))
	mustContain(t, sec, tx, "Recorded", reason)
	mustNotContain(t, sec, `method="post"`)
}

// Three staff record one refused deposit at once: one disposition is recorded and the others are refused with 409,
// writing no second audit row.
func TestXMREscrowCollisionConcurrentDisposition(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, m1 := x.user("collc1_"+randomToken()[:6], "moderator")
	_, m2 := x.user("collc2_"+randomToken()[:6], "moderator")
	_, a1 := x.user("collc3_"+randomToken()[:6], "admin")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("refused concurrent "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	var wg sync.WaitGroup
	codes, bodies := make([]int, 3), make([]string, 3)
	start := make(chan struct{})
	for i, sess := range []string{m1, m2, a1} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], bodies[i] = x.recordCollision(sess, id, "Concurrent disposition, reason long enough.")
		}()
	}
	close(start)
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{303, 409, 409}) {
		t.Fatalf("concurrent dispositions: %v %q", codes, bodies)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE 'Recorded what was done about refused Monero escrow deposit %'"); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
}

// The desk's Escrow alerts list counts an open record, on an order with no settlement, in its "N more" line, and lists
// such orders by when the record was noted among the other alerts, newest first, whatever the orders' age (QA5W2-3):
// the oldest alert is on the newest order and is the one cut; the newest is on the oldest order and is listed first.
func TestXMREscrowCollisionDeskCounted(t *testing.T) {
	x := newXMREscrowEnv(t)
	_, mod := x.user("colldesk_"+randomToken()[:6], "moderator")
	refused := func(order, noted string) {
		agExec(x.testEnv, "INSERT INTO xmr_escrow_collisions(order_id,wallet_name,account,txid,noted) VALUES($1,'escrow-202610-01',1,$2,now()+$3::interval)", order, hex64("desk "+order), noted)
	}
	oldest := x.testEnv.order(x.buyer, x.product, "XMR", stateAwaitingPayment)
	agExec(x.testEnv, "UPDATE orders SET created=now()+interval '30 days' WHERE id=$1", oldest)
	refused(oldest, "-1 day")
	for i := range escrowAlertLimit {
		o := x.testEnv.order(x.buyer, x.product, "BTC", stateCompleted)
		agExec(x.testEnv, "UPDATE orders SET created=now()+make_interval(secs => $2) WHERE id=$1", o, i)
		agExec(x.testEnv, "INSERT INTO settlements(order_id,kind,state,build_error,updated) VALUES($1,'release','failed',$2,now()+make_interval(secs => $3))", o, settleTooSmall, i)
	}
	section := func() string {
		_, sec, _ := strings.Cut(x.page("/moderator", mod), `<section id="escrow-alerts"`)
		sec, _, _ = strings.Cut(sec, "</section>")
		return sec
	}
	if sec := section(); !strings.Contains(sec, "1 more order has an open alert") || strings.Contains(sec, oldest) {
		t.Fatalf("the oldest alert, a refused deposit, is not counted:\n%s", sec)
	}
	newest := x.testEnv.order(x.buyer, x.product, "XMR", stateAwaitingPayment)
	agExec(x.testEnv, "UPDATE orders SET created=now()-interval '30 days' WHERE id=$1", newest)
	refused(newest, "1 day")
	sec := section()
	if !strings.Contains(sec, "2 more orders have an open alert") {
		t.Fatalf("two refused deposits among 102 alerts:\n%s", sec)
	}
	if i := strings.Index(sec, "<li>"); i < 0 || !strings.HasPrefix(sec[i:], `<li><a class="text-link mono" href="/order?id=`+newest+`">`) {
		t.Fatalf("the newest alert, a refused deposit, is not listed first:\n%s", sec)
	}
	if e := deskAlertIn(x.page("/moderator", mod), newest); !strings.Contains(e, "refused as another order") || strings.Contains(e, "too few") {
		t.Fatalf("the refused deposit's entry: %q", e)
	}
}

// A refused deposit noted before migration 090 is backfilled as an open record holding the first 20 hex digits of its
// transaction (txid_truncated); the first pass after the upgrade that meets the same deposit completes its transaction
// ID and opens nothing new (no second note or notice).
func TestXMREscrowCollisionBackfilledRecordCompleted(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	tx := hex64("refused before the upgrade " + order)
	x.fake.payAddress(addr, tx, xmrListingPrice, 10)
	x.refusePass(order)
	agExec(x.testEnv, "UPDATE xmr_escrow_collisions SET txid=left(txid,20),txid_truncated=true WHERE order_id=$1", order)
	x.refusePass(order)
	if got := agStrings(t, x.testEnv, "SELECT txid||' '||txid_truncated FROM xmr_escrow_collisions WHERE order_id=$1", order); !slices.Equal(got, []string{tx + " false"}) {
		t.Fatalf("the backfilled record after a pass: %q", got)
	}
	if notes, notices := x.collisionNotes(order); notes != 1 || notices != 1 {
		t.Fatalf("after the pass: %d notes, %d notices", notes, notices)
	}
}

// Migration 090 on a database holding refused-deposit notes written before it: each note of the fixed form becomes one
// open record (its order, wallet, account, the 20 hex digits of the transaction it names, txid_truncated, noted at the
// note's time); a note by a user that merely quotes the form, and other notes, do not.
func TestXMREscrowMigration090Backfill(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var upTo089 []migration
	for _, m := range list {
		if m.Version < 90 {
			upTo089 = append(upTo089, m)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, upTo089)
	})
	tx1, tx2 := hex64("m090 one"), hex64("m090 two")
	note := func(tx, wallet string, account int) string {
		return "TESTNET stagenet Monero escrow deposit " + truncate(tx, 20) + " to wallet " + wallet + " account " + strconv.Itoa(account) +
			" was not recorded: the database refused it as another order's escrow account. Moderator review required."
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO users(id,handle,password_hash,role) VALUES('ub','m90_buyer','x','buyer'),('uv','m90_vendor','x','vendor')", nil},
		{"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES('pd','uv','D','D','Hardware','Worldwide','digital',100000,1,5)", nil},
		{"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o1','ub','pd','XMR',1,'awaiting_payment'),('o2','ub','pd','XMR',1,'awaiting_payment')", nil},
		{"INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created) VALUES('o1','awaiting_payment','awaiting_payment',NULL,$1,'2026-09-01 10:00Z')", []any{note(tx1, "escrow-202609-01", 3)}},
		{"INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created) VALUES('o1','awaiting_payment','awaiting_payment',NULL,$1,'2026-09-02 10:00Z')", []any{note(tx2, "escrow-202609-01", 3)}},
		{"INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES('o2','awaiting_payment','awaiting_payment','ub',$1)", []any{note(tx1, "escrow-202609-01", 4)}},
		{"INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES('o2','awaiting_payment','awaiting_payment',NULL,'Payment review required.')", nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, list) })
	rows, err := db.Query(`SELECT order_id||' '||wallet_name||' '||account||' '||txid||' '||txid_truncated||' '||to_char(noted AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI')||' '||(disposed_at IS NULL)
		FROM xmr_escrow_collisions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	want := []string{"o1 escrow-202609-01 3 " + tx1[:20] + " true 2026-09-01 10:00 true", "o1 escrow-202609-01 3 " + tx2[:20] + " true 2026-09-02 10:00 true"}
	if !slices.Equal(got, want) {
		t.Fatalf("backfilled records:\n got %q\nwant %q", got, want)
	}
}

// QA5W3B-6: on a disputed escrow order the staff help says nothing can be changed from the page only when it shows no
// Refused Monero deposits form; while a record is open it names that form as the one thing staff change there.
func TestXMREscrowCollisionDisputedHelp(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, mod := x.user("colldisp_"+randomToken()[:6], "moderator")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("refused disputed "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", order)
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The content was never delivered to me at all.')", randomToken(), order)
	page := html.UnescapeString(x.page("/order?id="+order, mod))
	if !strings.Contains(refusedSection(page), `action="/moderator/xmr-collision"`) {
		t.Fatal("the disputed order shows staff no disposition form")
	}
	help := staffHelp(page)
	mustContain(t, help, "market staff", `href="#refused-deposits"`, "Only an administrator decides this escrow dispute")
	mustNotContain(t, help, "Nothing can be changed from this page")
	mustNotContain(t, page, "The order page is read-only for market staff.")
	// QA5W4B-7 (mutant MM): once the dispute is resolved, the moderator's help (its other branch) still names the open
	// record's form, and the page still offers it.
	agExec(x.testEnv, "UPDATE orders SET state='resolved' WHERE id=$1", order)
	page = html.UnescapeString(x.page("/order?id="+order, mod))
	if !strings.Contains(refusedSection(page), `action="/moderator/xmr-collision"`) {
		t.Fatal("the resolved order shows staff no disposition form")
	}
	help = staffHelp(page)
	mustContain(t, help, "a moderator.", `href="#refused-deposits"`, "nothing else can be changed from this page")
	mustNotContain(t, help, "Nothing can be changed from this page")
	// QA5W5B-4: nor does the line under the closed order's state say no change is possible beside that form.
	mustContain(t, page, `<p class="muted">This order is closed. Staff can still record what was done about a refused deposit on this page.</p>`)
	mustNotContain(t, page, "No further changes are possible.")
	agExec(x.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", order)
	if code, loc := x.recordCollision(mod, id, "Refunded to the buyer by hand in the wallet."); code != 303 {
		t.Fatalf("the disposition: %d %s", code, loc)
	}
	page = html.UnescapeString(x.page("/order?id="+order, mod))
	mustNotContain(t, refusedSection(page), `method="post"`)
	mustContain(t, staffHelp(page), "Nothing can be changed from this page")
	mustNotContain(t, staffHelp(page), `href="#refused-deposits"`)
	mustContain(t, page, "The order page is read-only for market staff.")
}
