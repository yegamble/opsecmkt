package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-652c (PAY87-3, owner decision 2026-10-25 "refund + hold"): a release to a banned vendor is held until the ban is
// lifted. The escrow watcher builds nothing for it and makes no wallet call for it; migration 149's
// escrow_market_may_sign refuses to store its transaction. Refunds and stray refunds are paid as usual, a release built
// before the ban is relayed and followed as before, and no other address is ever paid. DB integration tests against
// the simulated Monero wallet RPC, not chain evidence.

// heldReleaseText is how the order page and the staff alert begin to explain a release held for a banned vendor.
const heldReleaseText = "The vendor's account is closed, so the market holds this release"

// banVendor bans the escrow environment's vendor through the administrator's form.
func (x *xmrEscrowEnv) banVendor(adminSess string) {
	x.t.Helper()
	agExpect(x.testEnv, "/admin/ban", adminSess, banForm("ban", agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor), testBanReason), 303, "")
}

// unbanVendor lifts that ban through the same form.
func (x *xmrEscrowEnv) unbanVendor(adminSess string) {
	x.t.Helper()
	agExpect(x.testEnv, "/admin/ban", adminSess, banForm("unban", agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", x.vendor), testLiftReason), 303, "")
}

// spendCalls counts the fake wallet's calls that read, freeze, build or relay an escrow's coins.
func (x *xmrEscrowEnv) spendCalls() int {
	n := 0
	for _, m := range []string{"incoming_transfers", "freeze", "thaw", "transfer", "relay_tx"} {
		n += x.fake.countOf(m)
	}
	return n
}

// heldAlerts counts admin's notifications that tell of the hold on order.
func (x *xmrEscrowEnv) heldAlerts(admin, order string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", admin, shortID(order), heldReleaseText)
}

// A banned vendor's shipped orders complete (by the deadline or the buyer's confirmation) but their release, requested
// at shipment, stays requested across passes with the fixed class vendor-closed: nothing is built, no wallet call is made
// for it, administrators are told once, and the parties read why. A re-request of a failed release is held the same way.
func TestBanHoldsRequestedRelease(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	adminID, admin := x.user("banrel_admin", "admin")
	phys := x.physical(x.vendor)
	byDeadline, buyerA, _ := x.shippedXMR(phys)
	byConfirm, buyerB, _ := x.shippedXMR(phys)
	failedOrder, failed := x.failedRelease()

	x.banVendor(admin)
	x.forceDeadline(byDeadline, "release_at=now()-interval '1 minute'")
	if code, body := x.xmrConfirm(byConfirm, buyerB, testPassword); code != 303 {
		t.Fatalf("confirm receipt after the ban: %d %s", code, body)
	}
	if code := x.reRequest(admin, failedOrder, failed, testPassword); code != 303 {
		t.Fatalf("re-request after the ban: %d", code)
	}
	calls := x.spendCalls()
	x.mustPoll() // completes the order whose deadline passed, after the pass's settlements
	for pass := range 3 {
		x.mustPoll()
		for _, order := range []string{byDeadline, byConfirm} {
			s := x.settlementOf(order, "release")
			if x.state(order) != stateCompleted || s.State != "requested" || s.TxHash != "" || x.waitClass(s.ID) != xmrVendorClosed {
				t.Fatalf("pass %d: order %s, release %+v, class %q", pass, x.state(order), s, x.waitClass(s.ID))
			}
			if n := x.heldAlerts(adminID, order); n != 1 {
				t.Fatalf("pass %d: administrators told %d times of the hold on %s", pass, n, shortID(order))
			}
		}
		next := x.settlementOf(failedOrder, "release")
		if next.Supersedes != failed || next.State != "requested" || x.waitClass(next.ID) != xmrVendorClosed || x.heldAlerts(adminID, failedOrder) != 1 {
			t.Fatalf("pass %d: re-request %+v, class %q", pass, next, x.waitClass(next.ID))
		}
	}
	if got := x.spendCalls(); got != calls {
		t.Fatalf("the wallet was asked about held releases: %d spend calls, was %d", got, calls)
	}
	for _, c := range []struct{ order, sess string }{{byDeadline, buyerA}, {byConfirm, buyerB}} {
		if p := x.panel(c.order, c.sess); !strings.Contains(p, "Not built yet. "+heldReleaseText) || !strings.Contains(p, "No other address is ever paid") {
			t.Fatalf("the buyer's panel does not say the release is held:\n%s", p)
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND strpos(note,$2)>0", byDeadline, "not sent: "+heldReleaseText); n != 1 {
		t.Fatalf("%d order notes of the hold", n)
	}
	// A refused claim address keeps its class while the release is held, so it is still never tried again afterwards.
	refused := x.settlementOf(byConfirm, "release").ID
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", refused, xmrDestRefused+"-claimant")
	x.mustPoll()
	if x.waitClass(refused) != xmrDestRefused+"-claimant" || x.settlement(refused).State != "requested" || x.spendCalls() != calls {
		t.Fatalf("a held release with a refused claim address: class %q, %+v", x.waitClass(refused), x.settlement(refused))
	}
}

// A release built before the ban is not held: one whose relay answer was lost (it may be on the network: never treated
// as unsent) is looked up and relayed again with its stored metadata, and followed to its confirmations, with no new
// build.
func TestBanDoesNotStrandBuiltRelease(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banbuilt_admin", "admin")
	inPool, _, _ := x.completed()
	flushed, _, _ := x.completed()
	inPoolID, _ := x.release(inPool)
	flushedID, _ := x.release(flushed)
	x.fake.fault("relay_tx", fakeXMRDrop)
	x.fake.fault("relay_tx", fakeXMRDrop)
	x.poll()                                         // both relays took effect, their answers were lost
	x.fake.flushPool(x.settlement(flushedID).TxHash) // and the daemon dropped one of them
	for _, id := range []int64{inPoolID, flushedID} {
		if s := x.settlement(id); s.State != "built" || x.waitClass(id) != xmrRelayUnknown {
			t.Fatalf("before the ban: %+v, class %q", s, x.waitClass(id))
		}
	}
	hashes := []string{x.settlement(inPoolID).TxHash, x.settlement(flushedID).TxHash}
	transfers := x.fake.countOf("transfer")

	x.banVendor(admin)
	x.mustPoll()
	for i, id := range []int64{inPoolID, flushedID} {
		if s := x.settlement(id); s.State != "relayed" || s.TxHash != hashes[i] || x.waitClass(id) != "" {
			t.Fatalf("after the ban: %+v, class %q", s, x.waitClass(id))
		}
	}
	x.fake.mine(10)
	x.mustPoll()
	for _, id := range []int64{inPoolID, flushedID} {
		if s := x.settlement(id); s.State != "confirmed" {
			t.Fatalf("10 confirmations after the ban: %+v", s)
		}
	}
	if got := x.fake.countOf("transfer"); got != transfers {
		t.Fatalf("a built release was built again after the ban: transfer %d, was %d", got, transfers)
	}
}

// The database half (migration 149): while the vendor is banned, a release's transaction is never stored, whatever
// stores it, and a ban committed before the storing statement refuses it even inside a transaction begun earlier.
// A refund and a stray refund of the same vendor's orders are still allowed, and lifting the ban allows the release.
func TestBanHoldBackstopRefusesStore(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banstore_admin", "admin")
	cancelled, _ := x.fundedXMR(x.physical(x.vendor)) // cancelled by its buyer once the vendor is banned
	order, _, _ := x.completed()
	id, _ := x.release(order) // no pass builds it before the ban
	store := "UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd02',fee=30000000,input_total=500000000000,key_images=ARRAY[$2] WHERE id=$1"
	may := func(kind, order string) bool {
		t.Helper()
		var ok bool
		if err := x.DB.QueryRow("SELECT escrow_market_may_sign($1,$2)", kind, order).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !may("release", order) {
		t.Fatal("a completed order's release is refused before the ban")
	}

	// Begun (the order row locked, as storeXMRBuilt does) before the ban commits; the store runs after it.
	tx, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT 1 FROM orders WHERE id=$1 FOR NO KEY UPDATE", order); err != nil {
		t.Fatal(err)
	}
	x.banVendor(admin)
	_, err = tx.Exec(store, id, txidOf("banned store"))
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || !strings.Contains(pe.Message, "only once its order's state fits") {
		t.Fatalf("a held release stored after the ban committed: %v", err)
	}
	tx.Rollback()
	xmrRefused(t, x.testEnv, "a held release stored", "only once its order's state fits", store, id, txidOf("banned store 2"))
	if may("release", order) || !may("stray_refund", order) {
		t.Fatal("escrow_market_may_sign: a banned vendor's release allowed, or its stray refund refused")
	}
	x.forceDeadline(cancelled, "ship_by=now()-interval '1 minute'") // the vendor did not ship in time
	if code, body := x.xmrBuyerCancel(cancelled, x.sessOf(cancelled, roleBuyer)); code != 303 || x.state(cancelled) != stateCancelled {
		t.Fatalf("buyer's cancellation: %d %s (%s)", code, body, x.state(cancelled))
	}
	if !may("refund", cancelled) {
		t.Fatal("escrow_market_may_sign refuses the refund of a banned vendor's cancelled order")
	}

	x.unbanVendor(admin)
	if !may("release", order) {
		t.Fatal("the release is still refused after the ban was lifted")
	}
	tx2, err := x.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if _, err = tx2.Exec(store, id, txidOf("unbanned store")); err != nil {
		t.Fatalf("the release cannot be stored after the ban was lifted: %v", err)
	}
}

// Lifting the ban is the only step: the next pass builds the held release to the vendor's claim address and relays it.
func TestUnbanLiftsReleaseHold(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banlift_admin", "admin")
	order, buyerSess, claim := x.shippedXMR(x.physical(x.vendor))
	x.banVendor(admin)
	x.forceDeadline(order, "release_at=now()-interval '1 minute'")
	x.mustPoll()
	x.mustPoll()
	s := x.settlementOf(order, "release")
	if x.state(order) != stateCompleted || s.State != "requested" || x.waitClass(s.ID) != xmrVendorClosed {
		t.Fatalf("held: %s %+v %q", x.state(order), s, x.waitClass(s.ID))
	}
	transfers := x.fake.countOf("transfer")

	x.unbanVendor(admin)
	x.mustPoll()
	s = x.settlement(s.ID)
	if s.State != "relayed" || x.waitClass(s.ID) != "" || x.fake.countOf("transfer") != transfers+1 {
		t.Fatalf("after the ban was lifted: %+v, class %q, transfer %d", s, x.waitClass(s.ID), x.fake.countOf("transfer")-transfers)
	}
	if outs := x.outputsOf(s.ID); len(outs) != 1 || !strings.HasPrefix(outs[0], "claimant:"+claim+":") {
		t.Fatalf("the release pays %v, not the vendor's claim address %s", outs, claim)
	}
	if p := x.panel(order, buyerSess); strings.Contains(p, heldReleaseText) {
		t.Fatalf("the panel still says the release is held:\n%s", p)
	}
	x.fake.mine(10)
	x.mustPoll()
	if x.settlement(s.ID).State != "confirmed" {
		t.Fatalf("10 confirmations: %+v", x.settlement(s.ID))
	}
}

// A ban never holds money owed to buyers: a refund requested after the ban (the buyer's cancellation, a dispute decided
// for the buyer) and a stray refund beside a held release are built and relayed to the buyer's refund address.
func TestBanNeverHoldsRefunds(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("banrefund_admin", "admin")
	phys := x.physical(x.vendor)
	cancelled, _ := x.paidXMRPhysical(phys)
	disputed, _ := x.paidXMRPhysical(phys)
	held, addr, _ := x.completed()
	release, _ := x.release(held)
	x.banVendor(admin)

	x.forceDeadline(cancelled, "ship_by=now()-interval '1 minute'") // the vendor did not ship in time
	if code, body := x.xmrBuyerCancel(cancelled, x.sessOf(cancelled, roleBuyer)); code != 303 || x.state(cancelled) != stateCancelled {
		t.Fatalf("buyer's cancellation: %d %s (%s)", code, body, x.state(cancelled))
	}
	x.disputeAndResolve(disputed, "refund")
	x.fake.payAddress(addr, hex64("late "+held), 7000000000, 12)
	x.mustPoll()
	x.mustPoll()
	refundOf := func(order string) string {
		return agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	}
	for _, c := range []struct {
		order, kind string
		amount      int64
	}{{cancelled, "refund", xmrListingPrice}, {disputed, "refund", xmrListingPrice}, {held, "stray_refund", 7000000000}} {
		s := x.settlementOf(c.order, c.kind)
		if want := []string{"claimant:" + refundOf(c.order) + ":" + strconv.FormatInt(c.amount-s.Fee, 10)}; s.State != "relayed" || !slices.Equal(x.outputsOf(s.ID), want) {
			t.Fatalf("%s of %s: %+v outputs %v, want %v", c.kind, shortID(c.order), s, x.outputsOf(s.ID), want)
		}
	}
	if s := x.settlement(release); s.State != "requested" || x.waitClass(release) != xmrVendorClosed {
		t.Fatalf("the release beside the stray refund: %+v, class %q", s, x.waitClass(release))
	}
}

// Migration 149 replaces escrow_market_may_sign and nothing else: the catalog (relations, columns, constraints,
// indexes, triggers, every other function, types) and every row are as migration 148 left them, the function keeps its
// signature, language and volatility, and the one-outcome and one-built indexes are unchanged.
func TestMigration149HoldIsReplaceOnly(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var before, through []migration
	for _, m := range list {
		if m.Version < 149 {
			before = append(before, m)
		}
		if m.Version <= 149 {
			through = append(through, m)
		}
	}
	if through[len(through)-1].Version != 149 || through[len(through)-1].Name != "ban_holds_release" {
		t.Fatalf("migration 149 is %03d_%s", through[len(through)-1].Version, through[len(through)-1].Name)
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, before)
	})
	const fn = "escrow_market_may_sign"
	snapshot := func() (rest []string, def, header string) {
		t.Helper()
		const in = "= current_schema()::regnamespace"
		for _, q := range []string{
			"SELECT 'relation ' || relname || ' ' || relkind::text FROM pg_class WHERE relnamespace " + in,
			"SELECT 'column ' || table_name || '.' || column_name || ' #' || ordinal_position || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '') FROM information_schema.columns WHERE table_schema = current_schema()",
			"SELECT 'constraint ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace " + in,
			"SELECT 'index ' || indexdef FROM pg_indexes WHERE schemaname = current_schema()",
			"SELECT 'trigger ' || pg_get_triggerdef(t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid WHERE NOT t.tgisinternal AND c.relnamespace " + in,
			"SELECT 'function ' || pg_get_functiondef(oid) FROM pg_proc WHERE prokind IN ('f', 'p') AND proname <> '" + fn + "' AND pronamespace " + in,
			"SELECT 'type ' || typname || ' ' || typtype::text FROM pg_type WHERE typtype IN ('d', 'e') AND typnamespace " + in,
		} {
			rest = append(rest, querySorted(t, db, q)...)
		}
		for _, table := range querySorted(t, db, "SELECT relname FROM pg_class WHERE relkind = 'r' AND relname <> 'schema_migrations' AND relnamespace "+in) {
			rest = append(rest, querySorted(t, db, "SELECT 'row "+table+" ' || row_to_json(r) FROM "+table+" r")...)
		}
		ns := querySorted(t, db, "SELECT current_schema()")[0] + "."
		for i := range rest {
			rest[i] = strings.ReplaceAll(rest[i], ns, "")
		}
		slices.Sort(rest)
		defs := querySorted(t, db, "SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = '"+fn+"' AND pronamespace "+in)
		headers := querySorted(t, db, "SELECT pg_get_function_identity_arguments(p.oid) || ' -> ' || pg_get_function_result(p.oid) || ' ' || l.lanname || ' ' || provolatile::text || ' ' || proisstrict::text || ' ' || prosecdef::text FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang WHERE proname = '"+fn+"' AND pronamespace "+in)
		if len(defs) != 1 || len(headers) != 1 {
			t.Fatalf("%d definitions of %s", len(defs), fn)
		}
		return rest, defs[0], headers[0]
	}
	rest148, def148, header148 := snapshot()
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, through) })
	rest149, def149, header149 := snapshot()
	if header149 != header148 || header148 != "kind text, order_id text -> boolean sql s false false" {
		t.Fatalf("the function's signature changed: %q -> %q", header148, header149)
	}
	if def149 == def148 || !strings.Contains(def149, "v.banned_at IS NOT NULL") {
		t.Fatalf("migration 149 did not change the release arm:\n%s", def149)
	}
	if !slices.Equal(rest149, rest148) {
		for _, l := range rest149 {
			if !slices.Contains(rest148, l) {
				t.Errorf("added or changed: %s", l)
			}
		}
		for _, l := range rest148 {
			if !slices.Contains(rest149, l) {
				t.Errorf("removed or changed: %s", l)
			}
		}
		t.Fatal("migration 149 changed more than escrow_market_may_sign")
	}
	for _, want := range []string{
		"CREATE UNIQUE INDEX xmr_settlements_one_outcome ON xmr_settlements USING btree (order_id) WHERE ((kind = ANY (ARRAY['release'::text, 'refund'::text])) AND (state <> 'superseded'::text))",
		"CREATE UNIQUE INDEX xmr_settlements_one_built ON xmr_settlements USING btree (order_id) WHERE (state = 'built'::text)",
	} {
		if !slices.Contains(rest149, "index "+want) {
			t.Fatalf("missing after 149: %s", want)
		}
	}
}

// querySorted runs q on db (one text column) and returns the rows sorted.
func querySorted(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
