package market

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"
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
	// The time shown is the later bound: for the first case (closed 23 h 59 min ago, issued 31 days ago) the order's
	// close plus 24 hours, not the address's issue plus 30 days.
	var closed time.Time
	if err := p.DB.QueryRow("SELECT updated FROM orders WHERE id=$1", cases[0].order).Scan(&closed); err != nil {
		t.Fatal(err)
	}
	until := "until " + closed.Add(24*time.Hour).UTC().Format("2006-01-02 15:04") + " UTC"
	if d := p.str("SELECT detail FROM pooled_drain_blockers() WHERE kind='watched_address' AND ref=$1", cases[0].address); d != until {
		t.Fatalf("watched-until %q, want %q", d, until)
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

// R1 (A-486): a deposit recorded at a closed order's address inside the horizon but still below the threshold when the
// horizon ends keeps the address watched and listed (watched_address, naming the deposit) until it confirms; it is then
// flagged for review, which the existing disposition closes. QA445's P2 (TestQA445UnconfirmedLateDepositEscapesDrain).
func TestPooledDrainPendingDepositFlaggedOnConfirmation(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcher(p.order, p.addr, "pending-paid")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	p.poll()
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '23 hours 59 minutes' WHERE id=$1", p.order)
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '31 days' WHERE order_id=$1", p.order)
	p.fake.Deposit(p.addr, "pending-late", 0, 50000, 1) // 1 of 3 confirmations
	p.poll()
	row := func() string {
		return p.str("SELECT credited::text||','||flagged::text||','||after_close::text||','||confirmations::text FROM payments WHERE txid='pending-late'")
	}
	if r := row(); r != "false,false,true,1" {
		t.Fatalf("inside the horizon: %s", r)
	}
	wantRows(t, "inside the horizon", listedKinds(t, p.testEnv), "watched_address")
	if d := p.str("SELECT detail FROM pooled_drain_blockers() WHERE kind='watched_address'"); !strings.HasSuffix(d, ", and after that while recorded deposit pending-late:0 awaits confirmation") {
		t.Fatalf("detail inside the horizon: %q", d)
	}

	// The horizon ends with the deposit still unconfirmed: still watched (a pass reads it) and still listed.
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '25 hours' WHERE id=$1", p.order)
	p.fake.SetConfirmations("pending-late", 2)
	p.poll()
	if r := row(); r != "false,false,true,2" {
		t.Fatalf("past the horizon, the watcher did not read the deposit: %s", r)
	}
	wantRows(t, "past the horizon, pending", drainRows(t, p.testEnv, "watched_address"),
		p.addr+"|"+p.order+"|BTC|until recorded deposit pending-late:0 confirms (it is then flagged for review) or leaves the wallet; its watch horizon ended "+
			p.str("SELECT to_char((updated+interval '24 hours') AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI \"UTC\"') FROM orders WHERE id=$1", p.order))

	// It confirms: flagged for review (kind 3) and no longer a watched address; the desk disposition clears it.
	p.fake.SetConfirmations("pending-late", 6)
	p.poll()
	if r := row(); r != "false,true,true,6" {
		t.Fatalf("after it confirmed: %s", r)
	}
	id := p.str("SELECT id::text FROM payments WHERE txid='pending-late'")
	wantRows(t, "flagged once confirmed", drainRows(t, p.testEnv, "flagged_payment"), id+"|"+p.order+"|BTC|pending-late:0")
	wantRows(t, "flagged once confirmed (kinds)", listedKinds(t, p.testEnv), "flagged_payment")
	if code, body := p.dispose(p.adminSess, id, "no_action", "note", "Late payment reviewed with the buyer; returned outside the market."); code != 303 {
		t.Fatalf("disposition: %d %s", code, body)
	}
	wantRows(t, "disposed", listedKinds(t, p.testEnv))
	// No longer read: a later deposit is never recorded.
	p.fake.Deposit(p.addr, "pending-after", 0, 546, 3)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE txid='pending-after'"); n != 0 {
		t.Fatalf("address still read after the pending deposit closed: %d", n)
	}
}

// R1's exit: a pending deposit past the horizon that the wallet stops reporting (dropped or replaced) is marked
// missing (negative confirmations) by the pass that reads it, and one the wallet reports conflicted (a reorganisation
// or a double spend) is recorded with negative confirmations; either ends it, so the address leaves the watched set
// and the drain list with no staff action.
func TestPooledDrainPendingDepositEndsWhenTheWalletDropsIt(t *testing.T) {
	p := newPayEnv(t)
	p.move(p.order, stateAwaitingPayment, stateCancelled, p.buyer)
	other, otherAddr := p.newOrder(stateCancelled)
	for _, o := range []string{p.order, other} {
		agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '23 hours 59 minutes' WHERE id=$1", o)
	}
	agExec(p.testEnv, "UPDATE payment_addresses SET created=now()-interval '31 days'")
	p.fake.Deposit(p.addr, "drop-me", 0, 40000, 0)
	p.fake.Deposit(otherAddr, "conflict-me", 0, 40000, 1)
	p.poll()
	agExec(p.testEnv, "UPDATE orders SET updated=now()-interval '25 hours'")
	p.poll()
	wantRows(t, "both pending past the horizon", agStrings(t, p.testEnv, "SELECT ref FROM pooled_drain_blockers() WHERE kind='watched_address' ORDER BY 1"),
		p.addr, otherAddr)

	p.fake.Drop("drop-me")                     // evicted or replaced: the wallet no longer reports it
	p.fake.SetConfirmations("conflict-me", -1) // conflicted
	p.poll()
	for txid, want := range map[string]string{"drop-me": "-1,false,false", "conflict-me": "-1,false,false"} {
		if got := p.str("SELECT confirmations::text||','||credited::text||','||flagged::text FROM payments WHERE txid=$1", txid); got != want {
			t.Fatalf("%s: %s, want %s", txid, got, want)
		}
	}
	wantRows(t, "ended", listedKinds(t, p.testEnv))
	// Not read any more: neither reappearing nor confirming is recorded.
	p.fake.Deposit(p.addr, "drop-me", 0, 40000, 6)
	p.fake.SetConfirmations("conflict-me", 6)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE confirmations>=0"); n != 0 {
		t.Fatalf("an ended deposit's address is still read: %d", n)
	}
}

// S1 (adopted from QA445's TestQA445WatcherPredicateEquivalence): the pooled watcher's address rule (pooledWatchSQL)
// selects, for every order state at both time bounds (exact, a microsecond either side, in the future) and under
// several session time zones, the addresses the rule before A-445 selected (frozen below), plus exactly the addresses
// with a pending deposit (pooled_pending_deposit, A-486): nothing else changed. Seeded and compared in one transaction
// (one now()) per zone.
func TestPooledDrainWatcherPredicateEquivalence(t *testing.T) {
	p := newPayEnv(t)
	states := []string{"draft", "awaiting_payment", "paid", "shipped", "delivered", "completed", "disputed", "resolved", "cancelled"}
	upd := []string{"0", "24 hours", "24 hours 1 microsecond", "23 hours 59 minutes 59.999999 seconds", "-1 hour", "48 hours", "1 day", "1 day 1 microsecond", "40 days"}
	cre := []string{"0", "30 days", "30 days 1 microsecond", "29 days 23 hours 59 minutes 59.999999 seconds", "720 hours", "721 hours", "719 hours", "-1 hour", "40 days"}
	before := `(o.state IN ('awaiting_payment','paid','shipped','delivered','disputed') OR (o.state<>'draft' AND o.updated > now()-interval '24 hours')
		OR (o.state IN ('cancelled','resolved','completed') AND pa.created > now()-interval '30 days')
		OR EXISTS (SELECT 1 FROM payouts po WHERE po.order_id=o.id AND po.state IN ('pending','held')))`
	for _, tz := range []string{"UTC", "America/New_York", "Australia/Lord_Howe", "Pacific/Kiritimati", "America/St_Johns", "Europe/London"} {
		tx, err := p.DB.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(q, args...); err != nil {
				tx.Rollback()
				t.Fatalf("%s: %v", q, err)
			}
		}
		exec("SET LOCAL TimeZone='" + tz + "'")
		exec("DROP INDEX IF EXISTS orders_one_draft")
		for _, q := range []string{"payouts", "payments", "payment_addresses", "order_events", "orders"} {
			exec("DELETE FROM " + q)
		}
		n := 0
		for _, st := range states {
			for _, u := range upd {
				for _, c := range cre {
					id := fmt.Sprintf("eq-%d", n)
					exec("INSERT INTO orders(id,buyer_id,product_id,currency,amount,state,updated) VALUES($1,$2,$3,'BTC',1000,$4,now()-$5::interval)", id, p.buyer.ID, p.productID, st, u)
					exec("INSERT INTO payment_addresses(order_id,currency,address,provider,created) VALUES($1,'BTC',$2,'fake',now()-$3::interval)", id, "addr-"+id, c)
					n++
				}
			}
		}
		count := func(where string) int {
			t.Helper()
			var k int
			if err := tx.QueryRow("SELECT count(*) FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id WHERE " + where).Scan(&k); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			return k
		}
		// No deposit recorded: the rule is the old one, three-valued results included.
		if k := count("(" + before + ") IS DISTINCT FROM (" + pooledWatchSQL + ")"); k != 0 {
			tx.Rollback()
			t.Fatalf("tz %s: %d of %d addresses watched differently with no deposit recorded", tz, k, n)
		}
		// Deposits of every kind at every address: the rule adds exactly the addresses with a pending one (not
		// credited, not flagged, not ended), and only those.
		exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,flagged)
			SELECT pa.order_id,'BTC','tx-'||pa.order_id||'-'||k,0,pa.address,500,
				CASE k WHEN 0 THEN 0 WHEN 1 THEN 2 WHEN 2 THEN -1 WHEN 3 THEN 3 ELSE 1 END, k=3, k=4
			FROM payment_addresses pa JOIN orders o ON o.id=pa.order_id, generate_series(0,4) k
			WHERE (hashtext(pa.order_id) & 7) = k OR (k=2 AND (hashtext(pa.order_id) & 7) > 4)`)
		pending := "EXISTS(SELECT 1 FROM payments pm WHERE pm.address=pa.address AND NOT pm.credited AND NOT pm.flagged AND pm.confirmations>=0)"
		if k := count("(" + pooledWatchSQL + ") IS DISTINCT FROM ((" + before + ") OR " + pending + ")"); k != 0 {
			tx.Rollback()
			t.Fatalf("tz %s: %d addresses watched other than the old rule plus pending deposits", tz, k)
		}
		added := count("(" + pooledWatchSQL + ") AND NOT (" + before + ")")
		if added == 0 {
			tx.Rollback()
			t.Fatalf("tz %s: no address added by a pending deposit; the fixture tests nothing", tz)
		}
		tx.Rollback()
		t.Logf("tz %-20s: %d addresses, equal with no deposit; %d added by a pending deposit only", tz, n, added)
	}
}

// SHOULD-2 (A-488): the pending rule is the watcher's inline pendingDepositSQL; migration 091 carries it verbatim in
// pooled_drain_blockers, and pooled_pending_deposit (which only names the deposit in the detail) agrees with it on every
// recorded state, another currency's row at the same address included.
func TestPooledDrainPendingRuleShared(t *testing.T) {
	b, err := fs.ReadFile(migrationFiles, "migrations/091_pooled_drain_blockers.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "     AND "+pendingDepositSQL) {
		t.Fatal("migration 091's pending rule is not pendingDepositSQL: redefine pooled_drain_blockers in a new migration")
	}
	if strings.Contains(string(b), "pooled_pending_deposit(pa.currency, pa.address) IS NOT NULL") {
		t.Fatal("migration 091 selects rows by the function call instead of the inline rule")
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
		if err := f.DB.QueryRow("SELECT pooled_pending_deposit(pa.currency,pa.address) IS NOT NULL, "+pendingDepositSQL+" FROM payment_addresses pa WHERE pa.order_id=$1", order).Scan(&fn, &frag); err != nil {
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
