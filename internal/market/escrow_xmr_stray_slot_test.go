package market

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-616 (PAY72-1/-3, STAFF72-3, DA72-N1): a re-request (A-401) of a failed Monero stray refund never holds the order's
// one stray refund slot (migration 135, xmrStrayOpenSQL). Before, the re-request's carry-out broke
// xmr_settlements_one_open_stray on every pass while the first-generation stray refund of a later deposit was in
// progress, and a re-request waiting deposit-missing (A-614) held back the stray refund of every later deposit. Their
// coins stay disjoint: a first-generation stray refund claims only deposits no settlement claims, a re-request spends
// exactly the stored key images, and one built transaction per order is relayed before another is built. DB integration
// against the simulated wallet RPC (not chain evidence).

// strayTwoOpen is the buyer's status line of a cancelled order whose refund was too few while two stray refunds are in
// progress (xmrStrayStatus).
const strayTwoOpen = "Cancelled; the market could not send the refund because the coins were too few. The escrowed amount is returned, less the network fee, to the buyer's refund address by the 2 stray refunds below, which are in progress at once: each returns its own deposits and is sent on its own, so one may be sent while another still waits, as each says under it."

// strayFailed is a cancelled Monero escrow order whose refund was too few (A-437; its deposit d0 left out), then a
// deposit d1 after close, whose stray refund (spending d0 and d1) the daemon refused relay for good: failed, its
// transaction stored, claiming both. It returns the order, its escrow address, the buyer's session, d1 and the stray refund.
func (x *xmrEscrowEnv) strayFailed() (order, addr, bsess, d1 string, first int64) {
	x.t.Helper()
	order, addr = x.freshCheckout(x.digital(x.vendor))
	bsess = x.sessOf(order, roleBuyer)
	x.fake.payAddress(addr, hex64("stray slot tiny "+order), minPayoutXMR/2, 20)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.mustPoll()
	x.mustPoll()
	if x.state(order) != stateCancelled || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 1 {
		x.t.Fatalf("cancelled, its refund too few: %s; settlements %v", x.state(order), x.xmrSettlements(order))
	}
	for range xmrRelayAttempts {
		x.fake.fault("relay_tx", fakeXMRRejected)
	}
	d1 = hex64("stray slot one " + order)
	x.fake.payAddress(addr, d1, xmrListingPrice, 20)
	for i := 0; i < 3*xmrRelayAttempts && agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund' AND state='failed'", order) == 0; i++ {
		x.mustPoll()
	}
	s := x.settlementOf(order, "stray_refund")
	if s.State != "failed" || s.TxHash == "" || s.Supersedes != 0 || x.claimsOf(s.ID) != 2 {
		x.t.Fatalf("refused stray refund: %+v, %d claimed; settlements %v", s, x.claimsOf(s.ID), x.xmrSettlements(order))
	}
	// Nothing else is eligible (every deposit is claimed by the failed one), so no stray refund is requested beside it.
	x.mustPoll()
	if n := x.strays(order); n != 1 {
		x.t.Fatalf("%d stray refunds while every deposit is claimed by the failed one; settlements %v", n, x.xmrSettlements(order))
	}
	return order, addr, bsess, d1, s.ID
}

// claimsOf counts the deposits settlement id claims.
func (x *xmrEscrowEnv) claimsOf(id int64) int {
	return agInt(x.testEnv, "SELECT count(*) FROM payments WHERE xmr_settlement_id=$1", id)
}

// strays counts order's stray refunds.
func (x *xmrEscrowEnv) strays(order string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund'", order)
}

// laterStray pays a later deposit d2 of the price to order's escrow address and returns its first-generation stray
// refund, which the next passes request (claiming only d2, named in its note) and, with gate off, build and relay.
func (x *xmrEscrowEnv) laterStray(order, addr string, built bool) (string, int64) {
	x.t.Helper()
	d2 := hex64("stray slot two " + order)
	x.fake.payAddress(addr, d2, xmrListingPrice, 20)
	x.mustPoll()
	x.mustPoll()
	id := int64(agInt(x.testEnv, "SELECT COALESCE(max(id),0) FROM xmr_settlements WHERE order_id=$1 AND kind='stray_refund' AND supersedes IS NULL AND state NOT IN ('failed','superseded')", order))
	if id == 0 {
		x.t.Fatalf("no stray refund of the later deposit; settlements %v", x.xmrSettlements(order))
	}
	s := x.settlement(id)
	want := "requested"
	if built {
		want = "relayed"
	}
	if s.State != want {
		x.t.Fatalf("the later deposit's stray refund: %+v, want %s; settlements %v", s, want, x.xmrSettlements(order))
	}
	note := "Stray refund requested: " + amount(xmrListingPrice, 12) + " XMR in 1 deposit(s)"
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, note); n != 1 {
		x.t.Fatalf("%d notes %q; events %q", n, note, x.events(order))
	}
	return d2, id
}

// confirmedOnce checks that each deposit of order is claimed by exactly one settlement, which is confirmed, and that the
// key images of the two stray refunds a and b are disjoint.
func (x *xmrEscrowEnv) confirmedOnce(order string, a, b int64) {
	x.t.Helper()
	thr := x.A.escrowXMR.confirmations
	for i := 0; i < 3 && (x.settlement(a).State != "confirmed" || x.settlement(b).State != "confirmed"); i++ {
		x.fake.mine(int(thr) + 1)
		x.mustPoll()
	}
	sa, sb := x.settlement(a), x.settlement(b)
	if sa.State != "confirmed" || sb.State != "confirmed" {
		x.t.Fatalf("not confirmed: %+v %+v; settlements %v", sa, sb, x.xmrSettlements(order))
	}
	if got := agStrings(x.t, x.testEnv, `SELECT p.txid||' '||COALESCE(s.state,'none') FROM payments p LEFT JOIN xmr_settlements s ON s.id=p.xmr_settlement_id
		WHERE p.order_id=$1 AND (s.state IS DISTINCT FROM 'confirmed')`, order); len(got) != 0 {
		x.t.Fatalf("deposits without one confirmed claim: %v", got)
	}
	for _, k := range sa.KeyImages {
		if slices.Contains(sb.KeyImages, k) {
			x.t.Fatalf("key image %s spent by both %d and %d", k, a, b)
		}
	}
	if len(sa.KeyImages) == 0 || len(sb.KeyImages) == 0 {
		x.t.Fatalf("key images: %v %v", sa.KeyImages, sb.KeyImages)
	}
}

// Test 1 (PAY72-3, the collision): a stray refund refused relay for good, then a later deposit's first-generation stray
// refund in progress, then an administrator's re-request. Its carry-out used to break xmr_settlements_one_open_stray on
// every pass; it now supersedes the failed one, builds exactly its stored key images and relays beside the other. A
// second first-generation stray refund is still refused (test 4), and both confirm with disjoint claims.
func TestXMREscrowStrayReRequestBesideLaterStray(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a616_admin_"+randomToken()[:6], "admin")
	order, addr, bsess, _, first := x.strayFailed()
	_, second := x.laterStray(order, addr, true)
	if code := x.reRequest(admin, order, first, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	if err := x.poll(); err != nil {
		t.Fatalf("the pass that carries out the re-request: %v", err)
	}
	again := x.settlementOf(order, "stray_refund")
	old := x.settlement(first)
	if again.Supersedes != first || again.State != "relayed" || old.State != "superseded" ||
		!slices.Equal(slices.Sorted(slices.Values(again.KeyImages)), slices.Sorted(slices.Values(old.KeyImages))) ||
		x.claimsOf(again.ID) != 2 || x.claimsOf(second) != 1 || x.settlement(second).State != "relayed" {
		t.Fatalf("after the re-request: %+v (superseded %+v), claims %d/%d; re-requests %v; settlements %v",
			again, old, x.claimsOf(again.ID), x.claimsOf(second), x.reRequests(first), x.xmrSettlements(order))
	}
	if got := x.reRequests(first); !slices.Equal(got, []string{"done::" + strconv.FormatInt(again.ID, 10)}) {
		t.Fatalf("re-requests: %v", got)
	}
	// Test 4: a second first-generation stray refund in progress is still refused. The watcher requests none beside the
	// relayed one (xmrStrayOpenSQL); the database refuses a second one requested or built (since migration 144 its index
	// no longer counts a relayed one, PAY566-1), checked in a transaction rolled back.
	x.mustPoll()
	if x.strays(order) != 3 {
		t.Fatalf("a stray refund requested beside a relayed one: settlements %v", x.xmrSettlements(order))
	}
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	const insertStray = "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1"
	if _, err = tx.Exec(insertStray, order); err != nil {
		t.Fatalf("one first-generation stray refund beside a relayed one: %v", err)
	}
	if _, err = tx.Exec(insertStray, order); err == nil || !strings.Contains(err.Error(), "xmr_settlements_one_open_stray") {
		t.Fatalf("a second first-generation stray refund requested: %v", err)
	}
	tx.Rollback()
	if panel := x.panel(order, bsess); !strings.Contains(panel, strayTwoOpen) {
		t.Fatalf("buyer's panel with two stray refunds in progress:\n%s", panel)
	}
	x.confirmedOnce(order, again.ID, second)
	if x.strays(order) != 3 {
		t.Fatalf("settlements %v", x.xmrSettlements(order))
	}
}

// waitingStrayReRequest carries out an administrator's re-request of failed stray refund first while the wallet refuses
// to build it, so it stays requested claiming the deposits, and returns it.
func (x *xmrEscrowEnv) waitingStrayReRequest(admin, order string, first int64) int64 {
	x.t.Helper()
	restore := x.refuseTransfer(-4, "transaction was rejected by daemon")
	if code := x.reRequest(admin, order, first, testPassword); code != 303 {
		x.t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	restore()
	again := x.settlementOf(order, "stray_refund")
	if again.Supersedes != first || again.State != "requested" || x.claimsOf(again.ID) != 2 {
		x.t.Fatalf("the re-request: %+v; settlements %v", again, x.xmrSettlements(order))
	}
	return again.ID
}

// Test 2 (PAY72-1): a re-request of a stray refund waits deposit-missing (A-614: d1 read as double-spent) while a later
// deposit arrives. The later deposit's stray refund no longer waits behind it: it is requested, built from that deposit
// alone and relayed while the re-request waits; the order page says two are in progress. Once d1 is listed again the
// re-request builds its stored key images and relays; both confirm, each deposit with one confirmed claim.
func TestXMREscrowStrayBesideWaitingReRequest(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a616w_admin_"+randomToken()[:6], "admin")
	order, addr, bsess, d1, first := x.strayFailed()
	again := x.waitingStrayReRequest(admin, order, first)
	x.fake.setTransfer(d1, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	if s := x.settlement(again); s.State != "requested" || x.waitClass(again) != xmrWaitMissing {
		t.Fatalf("the re-request with d1 double-spent: %+v %q", s, x.waitClass(again))
	}
	transfers := x.fake.countOf("transfer")
	d2, second := x.laterStray(order, addr, true)
	if x.fake.countOf("transfer") != transfers+1 || x.settlement(again).State != "requested" || x.waitClass(again) != xmrWaitMissing || x.claimsOf(second) != 1 ||
		agStr(x.testEnv, "SELECT txid FROM payments WHERE xmr_settlement_id=$1", second) != d2 {
		t.Fatalf("beside the waiting re-request: transfers %d (want %d), %+v %q, claims %d; settlements %v",
			x.fake.countOf("transfer"), transfers+1, x.settlement(again), x.waitClass(again), x.claimsOf(second), x.xmrSettlements(order))
	}
	if panel := x.panel(order, bsess); !strings.Contains(panel, strayTwoOpen) {
		t.Fatalf("buyer's panel with two stray refunds in progress:\n%s", panel)
	}
	x.fake.setTransfer(d1, func(t *fakeXMRTransfer) { t.DoubleSpend = false })
	x.mustPoll()
	x.mustPoll()
	if s, old := x.settlement(again), x.settlement(first); s.State != "relayed" ||
		!slices.Equal(slices.Sorted(slices.Values(s.KeyImages)), slices.Sorted(slices.Values(old.KeyImages))) || x.claimsOf(again) != 2 {
		t.Fatalf("once d1 is listed again: %+v (stored %+v); settlements %v", s, old, x.xmrSettlements(order))
	}
	x.confirmedOnce(order, again, second)
}

// Test 3 (PAY72-1 premise): confirmations<0 sticks without any reorganisation when the wallet simply no longer lists a
// deposit (a restore, rescan or file swap; markMissing). Such a row, claimed by the waiting re-request (d1) or by none
// (d3, after close, never confirmed), never enters the plan of a later deposit's stray refund: it spends d2 alone.
func TestXMREscrowStrayMissingNeverInLaterPlan(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a616m_admin_"+randomToken()[:6], "admin")
	order, addr, _, d1, first := x.strayFailed()
	again := x.waitingStrayReRequest(admin, order, first)
	x.dropTransfer(d1)
	d3 := hex64("stray slot unconfirmed " + order)
	x.fake.payAddress(addr, d3, xmrListingPrice, 0)
	x.mustPoll()
	x.dropTransfer(d3)
	x.mustPoll()
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||COALESCE(xmr_settlement_id,0) FROM payments WHERE order_id=$1 AND txid=ANY($2) ORDER BY txid=$3 DESC", order, []string{d1, d3}, d1); !slices.Equal(got,
		[]string{"-1 " + strconv.FormatInt(again, 10), "-1 0"}) || x.waitClass(again) != xmrWaitMissing {
		t.Fatalf("no longer listed: %v, wait %q %+v; settlements %v; events %q", got, x.waitClass(again), x.settlement(again), x.xmrSettlements(order), x.events(order))
	}
	d2, second := x.laterStray(order, addr, true)
	if s := x.settlement(second); s.InputTotal != xmrListingPrice || x.claimsOf(second) != 1 ||
		agStr(x.testEnv, "SELECT txid FROM payments WHERE xmr_settlement_id=$1", second) != d2 || len(s.KeyImages) != 1 {
		t.Fatalf("the later stray refund's plan: %+v, claims %d", s, x.claimsOf(second))
	}
	if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||COALESCE(xmr_settlement_id,0) FROM payments WHERE order_id=$1 AND txid=$2", order, d3); !slices.Equal(got, []string{"-1 0"}) {
		t.Fatalf("d3: %v", got)
	}
}

// Test 5: with the restore gate set, the later deposit's stray refund and the administrator's re-request are recorded
// and nothing is built or relayed; once the gate is cleared, the re-request is carried out beside the stray refund and
// both are built and relayed.
func TestXMREscrowStraySlotRestoreGate(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a616g_admin_"+randomToken()[:6], "admin")
	order, addr, _, _, first := x.strayFailed()
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
	transfers, relays := x.fake.countOf("transfer"), x.fake.countOf("relay_tx")
	_, second := x.laterStray(order, addr, false)
	if code := x.reRequest(admin, order, first, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	x.mustPoll()
	if got := x.reRequests(first); !slices.Equal(got, []string{"requested::0"}) || x.settlement(second).State != "requested" ||
		x.fake.countOf("transfer") != transfers || x.fake.countOf("relay_tx") != relays || x.strays(order) != 2 {
		t.Fatalf("under the restore gate: re-requests %v, transfers %d (want %d), relays %d (want %d); settlements %v",
			got, x.fake.countOf("transfer"), transfers, x.fake.countOf("relay_tx"), relays, x.xmrSettlements(order))
	}
	x.clearGate()
	if err := x.poll(); err != nil {
		t.Fatalf("the first pass after the gate: %v", err)
	}
	x.mustPoll()
	again := x.settlementOf(order, "stray_refund")
	if again.Supersedes != first || again.State != "relayed" || x.settlement(second).State != "relayed" {
		t.Fatalf("after the gate: %+v, the later stray refund %+v; settlements %v", again, x.settlement(second), x.xmrSettlements(order))
	}
	x.confirmedOnce(order, again.ID, second)
}

// Test 6, migration 135: on a database that the migrations before it left (084's index, one first-generation stray refund in
// progress), it applies once, keeps the row, and the index then refuses a second first-generation stray refund in
// progress but accepts a re-request of a failed one beside it.
func TestMigration135StraySlotFirstGeneration(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM schema_migrations WHERE version=135 AND name='xmr_stray_slot_first_generation'"); n != 1 {
		t.Fatalf("migration 135 recorded %d times", n)
	}
	def := func() string {
		return agStr(x.testEnv, "SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='xmr_settlements_one_open_stray'")
	}
	if d := def(); !strings.Contains(d, "supersedes IS NULL") {
		t.Fatalf("index: %s", d)
	}
	order, _, _, _, first := x.strayFailed()
	agExec(x.testEnv, `DELETE FROM schema_migrations WHERE version=135; DROP INDEX xmr_settlements_one_open_stray;
		CREATE UNIQUE INDEX xmr_settlements_one_open_stray ON xmr_settlements (order_id) WHERE kind = 'stray_refund' AND state IN ('requested','built','relayed')`)
	stray := "INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'stray_refund',refund_address FROM escrows WHERE order_id=$1 RETURNING id"
	var open int64
	if err := x.DB.QueryRow(stray, order).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if d := def(); strings.Contains(d, "supersedes") {
		t.Fatalf("restored 084 index: %s", d)
	}
	if err := applyLatestMigrations(t, x.testEnv); err != nil {
		t.Fatalf("migration 135: %v", err)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM schema_migrations WHERE version=135"); n != 1 || !strings.Contains(def(), "supersedes IS NULL") || x.settlement(open).State != "requested" {
		t.Fatalf("after migration 135: %d rows, index %s, %+v", n, def(), x.settlement(open))
	}
	if _, err := x.DB.Exec(stray, order); err == nil || !strings.Contains(err.Error(), "xmr_settlements_one_open_stray") {
		t.Fatalf("a second first-generation stray refund after 135: %v", err)
	}
	// A re-request beside it, as xmrReRequest writes one.
	agExec(x.testEnv, "UPDATE xmr_settlements SET state='superseded',updated=now() WHERE id=$1", first)
	if _, err := x.DB.Exec(`INSERT INTO xmr_settlements(order_id,kind,destination,supersedes) SELECT order_id,kind,destination,id FROM xmr_settlements WHERE id=$1`, first); err != nil {
		t.Fatalf("a re-request beside a first-generation stray refund in progress: %v", err)
	}
	if err := applyLatestMigrations(t, x.testEnv); err != nil || agInt(x.testEnv, "SELECT count(*) FROM schema_migrations WHERE version=135") != 1 {
		t.Fatalf("migration 135 again: %v", err)
	}
}

// A-617, the stray refund variant: a re-request of a stray refund waiting deposit-missing keeps each deposit's base
// payment-review reason (left out, or after the close and now missing) and appends which re-request claims it; only
// the missing deposit's reason says it waits for that deposit to be listed again.
func TestXMREscrowStrayReRequestReviewReasons(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a617s_admin_"+randomToken()[:6], "admin")
	order, _, _, d1, first := x.strayFailed()
	again := x.waitingStrayReRequest(admin, order, first)
	x.fake.setTransfer(d1, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	if x.waitClass(again) != xmrWaitMissing {
		t.Fatalf("the re-request with d1 double-spent: %+v %q", x.settlement(again), x.waitClass(again))
	}
	want := []string{
		"Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer; now conflicted or missing" + waitingClaim(again, first, true),
		"Escrow deposit left out of its settlement, not counted; a stray refund from the escrow returns it to the buyer" + waitingClaim(again, first, false),
	}
	got := x.reviewReasons(order)
	if !slices.Equal(got, want) {
		t.Fatalf("payment-review reasons:\n got %q\nwant %q", got, want)
	}
	// A-640: only the missing deposit's reason carries the stale-ring caveat, and neither promises a send on its own.
	if !strings.Contains(got[0], staleRingClause) || strings.Contains(got[1], staleRingClause) {
		t.Fatalf("stale-ring caveat: %q", got)
	}
	for _, r := range got {
		mustNotContain(t, r, oldReRequestPromise...)
	}
}
