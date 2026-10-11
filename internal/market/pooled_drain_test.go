package market

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// A-445 (P4-R9): pooled_drain_blockers() (migration 091) lists what still needs the pooled wallet, one row per blocker.
// Each kind is tested blocked and then clear, against the state the application writes (the watcher, the payout and
// review actions) where it can, and in SQL where only a restore or an older market writes it.

// drainRows returns the blockers of kind as "ref|order|currency|detail", sorted.
func drainRows(t *testing.T, e *testEnv, kind string) []string {
	t.Helper()
	rows, err := e.DB.Query("SELECT ref||'|'||COALESCE(order_id,'')||'|'||COALESCE(currency,'')||'|'||detail FROM pooled_drain_blockers() WHERE kind=$1 ORDER BY 1", kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
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
	return out
}

// listedKinds returns the kinds the function lists, each once, sorted.
func listedKinds(t *testing.T, e *testEnv) []string {
	t.Helper()
	return agStrings(t, e, "SELECT DISTINCT kind FROM pooled_drain_blockers() ORDER BY 1")
}

func wantRows(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: got %q, want %q", what, got, want)
	}
}

// drainFixture is a market with a buyer, a vendor, a listing and no order.
type drainFixture struct {
	*testEnv
	buyer, vendor, product string
}

func newDrainFixture(t *testing.T) *drainFixture {
	e := newTestApp(t)
	b, _ := e.user("dbuyer_"+randomToken()[:6], "buyer")
	v, _ := e.user("dvendor_"+randomToken()[:6], "vendor")
	return &drainFixture{testEnv: e, buyer: b, vendor: v, product: e.product(v, "physical")}
}

// closedOrder inserts an order in a terminal state that left the watch horizon long ago, with a pooled address issued
// on provider (network part main/mainnet for mainnet history), so it is not a blocker by itself.
func (f *drainFixture) closedOrder(cur, state, provider string) (order, addr string) {
	f.t.Helper()
	order = f.order(f.buyer, f.product, cur, state)
	addr = "fake-testnet-" + order[:12]
	agExec(f.testEnv, "UPDATE orders SET updated=now()-interval '40 days',created=now()-interval '41 days' WHERE id=$1", order)
	agExec(f.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES($1,$2,$3,$4,now()-interval '41 days')", order, cur, addr, provider)
	return
}

// escrowOrder marks order as an escrow order (an escrows row, as checkout writes one).
func (f *drainFixture) escrowOrder(order string) {
	f.t.Helper()
	agExec(f.testEnv, `INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination)
		VALUES($1,'bcrt1qdrain'||substr($1,1,12),nextval('escrow_market_index'),'bcrt1qdrainrefund'||substr($1,1,12),0,'')`, order)
}

func TestPooledDrainFunctionIsStableAndReadOnly(t *testing.T) {
	f := newDrainFixture(t)
	for _, fn := range []string{"pooled_drain_blockers", "pooled_watch_horizon"} {
		if v := agStr(f.testEnv, "SELECT provolatile::text FROM pg_proc WHERE proname=$1", fn); v != "s" {
			t.Fatalf("%s volatility %q, want STABLE", fn, v)
		}
	}
	// A market with no pooled order, payout, payment or address has nothing to drain.
	wantRows(t, "empty market", listedKinds(t, f.testEnv))
	// It runs in a read-only transaction (it writes nothing), with every kind present.
	f.closedOrder("BTC", stateCompleted, "btc-main")
	f.order(f.buyer, f.product, "BTC", statePaid)
	agExec(f.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	ctx := context.Background()
	tx, err := f.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SET TRANSACTION READ ONLY"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM pooled_drain_blockers()").Scan(&n); err != nil || n != 3 {
		t.Fatalf("read-only call: %d %v", n, err)
	}
}

// Kind 1: an order that is neither a draft nor closed, without an escrow.

func TestPooledDrainOpenOrders(t *testing.T) {
	f := newDrainFixture(t)
	f.order(f.buyer, f.product, "BTC", stateDraft) // no deposit address yet: not pooled, not a blocker
	var open []string
	for _, s := range []string{stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateDisputed} {
		open = append(open, f.order(f.buyer, f.product, "XMR", s))
	}
	escrow := f.order(f.buyer, f.product, "BTC", statePaid)
	f.escrowOrder(escrow) // settled from its escrow, never by the pooled wallet
	for _, s := range []string{stateCompleted, stateResolved, stateCancelled} {
		f.order(f.buyer, f.product, "BTC", s)
	}
	var want []string
	for i, s := range []string{stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateDisputed} {
		want = append(want, open[i]+"|"+open[i]+"|XMR|"+s)
	}
	wantRows(t, "open pooled orders", drainRows(t, f.testEnv, "open_order"), want...)
	wantRows(t, "kinds", listedKinds(t, f.testEnv), "open_order")
	agExec(f.testEnv, "UPDATE orders SET state='completed' WHERE id=ANY($1)", open)
	wantRows(t, "closed", drainRows(t, f.testEnv, "open_order"))
	wantRows(t, "kinds after closing", listedKinds(t, f.testEnv))
}

// Kind 2: every payout not sent, whatever its state (all payouts are pooled).

func TestPooledDrainUnsentPayouts(t *testing.T) {
	f := newDrainFixture(t)
	states := []string{"blocked", "held", "pending", "sending", "failed", "sent"}
	var ids []string
	for _, s := range states {
		order := f.order(f.buyer, f.product, "BTC", stateCompleted)
		addr := "fake-testnet-vendor"
		if s == "blocked" {
			addr = ""
		}
		ids = append(ids, agStr(f.testEnv, "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'BTC',100000,$3,$4) RETURNING id::text", order, f.vendor, addr, s))
	}
	var want []string
	for i, s := range states[:5] {
		want = append(want, ids[i]+"|"+agStr(f.testEnv, "SELECT order_id FROM payouts WHERE id=$1::bigint", ids[i])+"|BTC|release "+s)
	}
	wantRows(t, "unsent payouts", drainRows(t, f.testEnv, "unsent_payout"), want...)
	wantRows(t, "kinds", listedKinds(t, f.testEnv), "unsent_payout")
	agExec(f.testEnv, "UPDATE payouts SET state='sent',txid=repeat('a',64)")
	wantRows(t, "all sent", listedKinds(t, f.testEnv))
}

// Frozen predicates from migration 092. Runtime review now accepts only escrow flags;
// these checks protect the historical startup drain gate without reviving a writer.
const legacyPooledFlagOpenRule = `@.flagged AND @.disposition IN ('','escalated') AND (@.locked OR @.below_floor OR @.regress_notice OR @.spent_outside
    OR @.escrow_left_out OR NOT @.credited AND (@.after_close OR NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id=@.order_id)))`

func legacyPooledFlagOpen(alias string) string {
	return strings.ReplaceAll(legacyPooledFlagOpenRule, "@", alias)
}

const legacyPendingDepositSQL = "EXISTS (SELECT 1 FROM payments pm WHERE pm.currency = pa.currency AND pm.address = pa.address AND NOT pm.credited AND NOT pm.flagged AND pm.confirmations >= 0)"

// Kind 3: a pooled deposit whose review flag is open (paymentFlagOpenRule): recordable on the moderation desk, so the
// existing disposition closes every one. A flag the desk cannot record (a credited deposit that confirmed again) and an
// escrow deposit's flag are not blockers.

func TestPooledDrainFlaggedPaymentsFollowTheOpenFlagRule(t *testing.T) {
	f := newDrainFixture(t)
	type row struct {
		name, cols string
		escrow     bool
	}
	matrix := []row{
		{"undisposed after close", "flagged,after_close", false},
		{"escalated", "flagged,after_close,disposition=escalated", false},
		{"no action recorded", "flagged,after_close,disposition=no_action", false},
		{"refunded by hand", "flagged,after_close,disposition=refunded", false},
		{"not flagged", "after_close", false},
		{"locked", "flagged,locked", false},
		{"below floor", "flagged,credited,below_floor", false},
		{"regression open", "flagged,credited,regress_notice", false},
		{"regression confirmed again", "flagged,credited", false},
		{"after settlement", "flagged", false},
		{"escrow, counts for the order", "flagged,credited", true},
		{"escrow, after close", "flagged,after_close", true},
		{"escrow, left out", "flagged,escrow_left_out", true},
	}
	ids := map[string]string{}
	for i, m := range matrix {
		order := f.order(f.buyer, f.product, "BTC", stateCompleted)
		if m.escrow {
			f.escrowOrder(order)
		}
		set := []string{}
		for _, c := range strings.Split(m.cols, ",") {
			if k, v, ok := strings.Cut(c, "="); ok {
				set = append(set, k+"='"+v+"'")
			} else {
				set = append(set, c+"=true")
			}
		}
		id := agStr(f.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,$3,500,3) RETURNING id::text",
			order, strings.Repeat("0", 63)+string(rune('a'+i)), "fake-testnet-"+order[:12])
		agExec(f.testEnv, "UPDATE payments SET "+strings.Join(set, ",")+" WHERE id=$1::bigint", id)
		ids[m.name] = id
	}
	// The function's set is exactly the open-flag rule's over pooled orders, the rule the desk and the review read.
	rule := agStrings(t, f.testEnv, "SELECT pm.id::text FROM payments pm WHERE NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=pm.order_id) AND "+legacyPooledFlagOpen("pm")+" ORDER BY 1")
	got := agStrings(t, f.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='flagged_payment' ORDER BY 1")
	if !slices.Equal(got, rule) {
		t.Fatalf("flagged_payment %q, open-flag rule %q", got, rule)
	}
	var want []string
	for _, name := range []string{"undisposed after close", "escalated", "locked", "below floor", "regression open", "after settlement"} {
		want = append(want, ids[name])
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("flagged_payment %q, want %q", got, want)
	}
	// Recording a disposition closes each one.
	agExec(f.testEnv, "UPDATE payments SET disposition='no_action',disposition_note='checked by staff',disposed_at=now() WHERE id::text=ANY($1)", want)
	wantRows(t, "disposed", drainRows(t, f.testEnv, "flagged_payment"))
}

// The migration carries the open-flag rule verbatim (it cannot call Go): a change to paymentFlagOpenRule needs a new
// migration redefining pooled_drain_blockers, and this test says so.

func TestPooledDrainMigrationCarriesTheOpenFlagRule(t *testing.T) {
	b, err := fs.ReadFile(migrationFiles, "migrations/092_pooled_stalled_review.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), legacyPooledFlagOpen("pm")) {
		t.Fatal("latest drain migration's flagged_payment rule is not paymentFlagOpen(\"pm\"): redefine pooled_drain_blockers in a new migration")
	}
}

// Kind 4 and the watcher share one horizon (pooled_watch_horizon): an address of a closed order is a blocker exactly
// while the watcher reads it, 24 hours after the order closed or 30 days after the address was issued.

func TestPooledDrainRecoveryRequired(t *testing.T) {
	f := newDrainFixture(t)
	agExec(f.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	wantRows(t, "restore gate set", drainRows(t, f.testEnv, "recovery_required"), "payments_recovery_required|||")
	agExec(f.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	wantRows(t, "restore gate cleared", listedKinds(t, f.testEnv))
}

// Kind 6: an issued-address warning (migration 071) not yet dismissed; the administrator's dismissal clears it.

func TestPooledDrainMainnetHistory(t *testing.T) {
	f := newDrainFixture(t)
	f.closedOrder("BTC", stateCompleted, "btc-regtest")
	f.closedOrder("XMR", stateCompleted, "xmr-stagenet")
	f.closedOrder("BTC", stateCompleted, "btc-test")
	wantRows(t, "test networks only", listedKinds(t, f.testEnv))
	main1, _ := f.closedOrder("BTC", stateCompleted, "btc-main")
	main2, _ := f.closedOrder("BTC", stateCancelled, "btc-main")
	main3, _ := f.closedOrder("XMR", stateResolved, "xmr-mainnet")
	wantRows(t, "mainnet history", drainRows(t, f.testEnv, "mainnet_history"), "BTC||BTC|2", "XMR||XMR|1")
	wantRows(t, "kinds", listedKinds(t, f.testEnv), "mainnet_history")
	agExec(f.testEnv, "UPDATE payment_addresses SET provider='btc-regtest' WHERE order_id IN ($1,$2)", main1, main2)
	agExec(f.testEnv, "UPDATE payment_addresses SET provider='xmr-stagenet' WHERE order_id=$1", main3)
	wantRows(t, "no mainnet history", listedKinds(t, f.testEnv))
}

// R1 (A-486): a deposit recorded at a closed order's address inside the horizon but still below the threshold when the
// horizon ends keeps the address watched and listed (watched_address, naming the deposit) until it confirms; it is then
// flagged for review, which the existing disposition closes. QA445's P2 (TestQA445UnconfirmedLateDepositEscapesDrain).

func TestPooledDrainPendingRuleShared(t *testing.T) {
	b, err := fs.ReadFile(migrationFiles, "migrations/092_pooled_stalled_review.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "     AND "+legacyPendingDepositSQL) {
		t.Fatal("latest drain migration's pending rule is not legacyPendingDepositSQL: redefine pooled_drain_blockers in a new migration")
	}
	if strings.Contains(string(b), "pooled_pending_deposit(pa.currency, pa.address) IS NOT NULL") {
		t.Fatal("latest drain migration selects rows by the function call instead of the inline rule")
	}
	f := newDrainFixture(t)
	type dep struct {
		cur      string
		confs    int64
		credited bool
		flagged  bool
		locked   bool
		pending  bool // the expected answer
	}
	cases := []dep{
		{"BTC", 0, false, false, false, true}, {"BTC", 2, false, false, false, true}, {"BTC", -1, false, false, false, false},
		{"BTC", -5, false, false, false, false}, {"BTC", 6, true, false, false, false}, {"BTC", 6, false, true, false, false},
		{"BTC", 1, false, false, true, true}, {"XMR", 1, false, false, false, false},
	}
	for i, c := range cases {
		order, addr := f.closedOrder("BTC", stateCompleted, "btc-regtest")
		agExec(f.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,flagged,locked) VALUES($1,$2,$3,0,$4,500,$5,$6,$7,$8)",
			order, c.cur, fmt.Sprintf("rule-%d", i), addr, c.confs, c.credited, c.flagged, c.locked)
		var fn, frag bool
		if err := f.DB.QueryRow("SELECT pooled_pending_deposit(pa.currency,pa.address) IS NOT NULL, "+legacyPendingDepositSQL+" FROM payment_addresses pa WHERE pa.order_id=$1", order).Scan(&fn, &frag); err != nil {
			t.Fatal(err)
		}
		if fn != c.pending || frag != c.pending {
			t.Fatalf("case %d %+v: function %v, inline rule %v, want %v", i, c, fn, frag, c.pending)
		}
		listed := agInt(f.testEnv, "SELECT count(*) FROM pooled_drain_blockers() WHERE kind='watched_address' AND ref=$1", addr)
		if (listed == 1) != c.pending || listed > 1 {
			t.Fatalf("case %d %+v: listed %d times", i, c, listed)
		}
	}
}

// NIT-5 (A-488): an address is listed under watched_address once, whether inside the horizon with a pending deposit or
// past it; an open order's address is never listed there (it is an open order), pending deposit or not.

func TestPooledDrainWatchedAddressListedOnce(t *testing.T) {
	f := newDrainFixture(t)
	inside, insideAddr := f.closedOrder("BTC", stateCompleted, "btc-regtest")
	agExec(f.testEnv, "UPDATE orders SET updated=now()-interval '1 hour' WHERE id=$1", inside)
	open := f.order(f.buyer, f.product, "BTC", statePaid)
	openAddr := "fake-testnet-" + open[:12]
	agExec(f.testEnv, "UPDATE orders SET updated=now()-interval '3 days' WHERE id=$1", open)
	agExec(f.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES($1,'BTC',$2,'btc-regtest',now()-interval '40 days')", open, openAddr)
	for i, a := range []struct{ order, addr string }{{inside, insideAddr}, {open, openAddr}} {
		agExec(f.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,$3,500,1)", a.order, fmt.Sprintf("once-%d", i), a.addr)
	}
	wantRows(t, "watched addresses", agStrings(t, f.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='watched_address' ORDER BY 1"), insideAddr)
	wantRows(t, "open orders", agStrings(t, f.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='open_order' ORDER BY 1"), open)
}

// A closed historical address remains a blocker only inside the watch horizon or while a recorded deposit is pending.
func TestPooledDrainWatchedAddressHorizon(t *testing.T) {
	f := newDrainFixture(t)
	order, addr := f.closedOrder("BTC", stateCompleted, "btc-regtest")
	wantRows(t, "old closed address", drainRows(t, f.testEnv, "watched_address"))
	agExec(f.testEnv, "UPDATE orders SET updated=now()-interval '1 hour' WHERE id=$1", order)
	wantRows(t, "inside horizon", agStrings(t, f.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='watched_address' ORDER BY 1"), addr)
	agExec(f.testEnv, "UPDATE orders SET updated=now()-interval '40 days' WHERE id=$1", order)
	agExec(f.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC','historical-pending',0,$2,500,1)", order, addr)
	wantRows(t, "pending after horizon", agStrings(t, f.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='watched_address' ORDER BY 1"), addr)
	agExec(f.testEnv, "UPDATE payments SET confirmations=-1 WHERE order_id=$1", order)
	wantRows(t, "ended pending", drainRows(t, f.testEnv, "watched_address"))
}

func TestPooledDrainAddressCollisions(t *testing.T) {
	f := newDrainFixture(t)
	agExec(f.testEnv, "UPDATE payment_status SET address_collisions=2 WHERE currency='BTC'")
	wantRows(t, "historical collisions", drainRows(t, f.testEnv, "address_collision"), "BTC||BTC|2")
	agExec(f.testEnv, "UPDATE payment_status SET address_collisions=0 WHERE currency='BTC'")
	wantRows(t, "cleared legacy collision", drainRows(t, f.testEnv, "address_collision"))
}
