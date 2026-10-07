package market

import (
	"context"
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
	rule := agStrings(t, f.testEnv, "SELECT pm.id::text FROM payments pm WHERE NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=pm.order_id) AND "+paymentFlagOpen("pm")+" ORDER BY 1")
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
	b, err := fs.ReadFile(migrationFiles, "migrations/091_pooled_drain_blockers.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), paymentFlagOpen("pm")) {
		t.Fatal("migration 091's flagged_payment rule is not paymentFlagOpen(\"pm\"): redefine pooled_drain_blockers in a new migration")
	}
}

// Kind 4 and the watcher share one horizon (pooled_watch_horizon): an address of a closed order is a blocker exactly
// while the watcher reads it, 24 hours after the order closed or 30 days after the address was issued.
func TestPooledDrainWatchedAddressIsTheWatcherHorizon(t *testing.T) {
	p := newPayEnv(t)
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer) // payEnv's own order: closed now, inside the horizon
	cases := []struct {
		state          string
		closed, issued string // ages
		watched        bool
		order, address string
	}{
		// A minute either side of each bound (the pass reads them within seconds of setting them).
		{state: stateCompleted, closed: "23 hours 59 minutes", issued: "31 days", watched: true},
		{state: stateCompleted, closed: "24 hours 1 minute", issued: "29 days 23 hours 59 minutes", watched: true},
		{state: stateCompleted, closed: "24 hours 1 minute", issued: "30 days 1 minute", watched: false},
		{state: stateCancelled, closed: "25 hours", issued: "31 days", watched: false},
		{state: stateResolved, closed: "1 hour", issued: "40 days", watched: true},
		{state: stateResolved, closed: "3 days", issued: "10 days", watched: true},
	}
	want := []string{p.addr}
	for i := range cases {
		c := &cases[i]
		c.order, c.address = p.newOrder(c.state)
		agExec(p.testEnv, "UPDATE orders SET updated=now()-$2::interval WHERE id=$1", c.order, c.closed)
		agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-$2::interval WHERE order_id=$1", c.order, c.issued)
		if c.watched {
			want = append(want, c.address)
		}
	}
	got := agStrings(t, p.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='watched_address' ORDER BY 1")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("watched_address %q, want %q", got, want)
	}
	// The watcher reads exactly those addresses: a deposit to each is recorded only where the function lists it.
	for i, c := range cases {
		p.fake.Deposit(c.address, "horizon-"+string(rune('a'+i)), 0, 700, 3)
	}
	p.fake.Deposit(p.addr, "horizon-own", 0, 700, 3)
	p.poll()
	recorded := agStrings(t, p.testEnv, "SELECT DISTINCT address FROM payments ORDER BY 1")
	if !slices.Equal(recorded, want) {
		t.Fatalf("watcher recorded deposits at %q, the drain gate lists %q", recorded, want)
	}
	// Past the horizon: clear.
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '25 hours' WHERE id IN (SELECT order_id FROM payment_addresses)")
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '31 days'")
	wantRows(t, "past the horizon", drainRows(t, p.testEnv, "watched_address"))
}

// Dust inside the horizon blocks until it is disposed through the existing disposition (also once the horizon has
// passed), and dust sent after the horizon is never read, so it never blocks.
func TestPooledDrainDustBlocksUntilDisposedNeverAfterHorizon(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "drain-paid")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	if st, _, _, _ := p.payout(p.order); st != "sent" {
		t.Fatalf("release %s", st)
	}
	// Completed, paid out, address inside the horizon: only the address blocks.
	wantRows(t, "settled inside the horizon", listedKinds(t, p.testEnv), "watched_address")

	// Dust inside the horizon: recorded, flagged, and a blocker of its own.
	p.fake.Deposit(p.addr, "drain-dust-1", 0, 546, 3)
	p.poll()
	dust := p.str("SELECT id::text FROM payments WHERE txid='drain-dust-1' AND flagged AND after_close AND disposition=''")
	wantRows(t, "dust inside the horizon", drainRows(t, p.testEnv, "flagged_payment"), dust+"|"+p.order+"|BTC|drain-dust-1:0")

	// The horizon passes with the dust undisposed: it still blocks, until staff dispose of it.
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '25 hours' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '31 days' WHERE order_id=$1", p.order)
	wantRows(t, "past the horizon, undisposed", listedKinds(t, p.testEnv), "flagged_payment")
	if code, body := p.dispose(p.adminSess, dust, "no_action", "note", "Dust below any useful amount; nothing to return."); code != 303 {
		t.Fatalf("disposition: %d %s", code, body)
	}
	wantRows(t, "disposed", listedKinds(t, p.testEnv))

	// Dust after the horizon: the watcher no longer reads the address, so it is never recorded and never blocks.
	p.fake.Deposit(p.addr, "drain-dust-2", 0, 546, 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE txid='drain-dust-2'"); n != 0 {
		t.Fatalf("dust after the horizon recorded: %d", n)
	}
	wantRows(t, "dust after the horizon", listedKinds(t, p.testEnv))
}

// Kind 5: the restore gate.
func TestPooledDrainRecoveryRequired(t *testing.T) {
	f := newDrainFixture(t)
	agExec(f.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	wantRows(t, "restore gate set", drainRows(t, f.testEnv, "recovery_required"), "payments_recovery_required|||")
	agExec(f.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")
	wantRows(t, "restore gate cleared", listedKinds(t, f.testEnv))
}

// Kind 6: an issued-address warning (migration 071) not yet dismissed; the administrator's dismissal clears it.
func TestPooledDrainAddressCollisions(t *testing.T) {
	p := newPayEnv(t)
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '2 days' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '40 days'")
	wantRows(t, "before", listedKinds(t, p.testEnv))
	p.A.recordAddressCollision(context.Background(), "BTC", "fake")
	p.A.recordAddressCollision(context.Background(), "BTC", "fake")
	wantRows(t, "collisions", drainRows(t, p.testEnv, "address_collision"), "BTC||BTC|2")
	p.check(p.do("POST", "/admin/payment-address-collisions", p.adminSess, form("currency", "BTC", "seen", "2")), 303)
	wantRows(t, "dismissed", listedKinds(t, p.testEnv))
}

// Kind 7: pooled addresses issued on mainnet, which no escrow in this release can replace.
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
