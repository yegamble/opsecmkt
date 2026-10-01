package market

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"html"
	"slices"
	"strings"
	"testing"
)

// Reconciliation (P1-d1, ES-6): an escrow deposit spent by a transaction the market did not make, with no settlement
// covering it, is flagged for staff with the spending transaction when the watch-only wallet names it; an address the
// watch-only wallet holds with no escrow row is listed for administrators. Fake escrow Core (simulated RPC, not chain
// evidence).

// coSpend is a segwit transaction spending txid:vout to one output, as the parties' own co-spend would be, recorded
// in the fake node and the watch-only wallet with confs confirmations (its input spent).
func (x *escrowEnv) coSpend(txid string, vout uint32, confs int64) string {
	x.t.Helper()
	id, _ := hex.DecodeString(txid)
	slices.Reverse(id)
	raw := []byte{2, 0, 0, 0, 0, 1, 1}
	raw = append(raw, id...)
	raw = binary.LittleEndian.AppendUint32(raw, vout)
	raw = append(raw, 0, 0xfd, 0xff, 0xff, 0xff, 1)
	raw = binary.LittleEndian.AppendUint64(raw, 90000)
	raw = append(raw, 22, 0, 20)
	raw = append(raw, bytes32(7)[:20]...)
	raw = append(raw, 2, 0, 1, 1) // witness: two items
	raw = append(raw, 0, 0, 0, 0)
	t, err := parseSignedTx(raw)
	if err != nil {
		x.t.Fatal(err)
	}
	spender := t.TxID()
	x.core.mu.Lock()
	if x.core.broadcast == nil {
		x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
	}
	x.core.broadcast[spender], x.core.txConfs[spender] = hex.EncodeToString(raw), confs
	x.core.mu.Unlock()
	x.core.spend(txid, int64(vout))
	return spender
}

func TestEscrowOutsideSpendFlagged(t *testing.T) {
	x := newEscrowEnv(t)
	_, mod := x.user("recmod", "moderator")
	// A completed order whose vendor has not claimed: its deposit has no settlement. The parties co-spend it directly.
	order := x.completed(regtestAddr("refund-cospend"), 100000)
	spender := x.coSpend(txidOf(order), 0, 1)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by=$2 AND flagged", order, spender) != 1 {
		t.Fatal("the co-spent deposit was not marked with its spending transaction and flagged")
	}
	ev := strings.Join(x.events(order), "\n")
	if !strings.Contains(ev, "were spent by a transaction this market did not make (transaction "+spender+"), with no settlement of the market covering them") {
		t.Fatalf("order notes:\n%s", ev)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE body LIKE $1", "Payment review needed for order "+shortID(order)+"%"+spender+"%") == 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%coins at its escrow were spent by a transaction this market did not make%'", x.buyer) != 1 {
		t.Fatal("staff or parties not told")
	}
	if page := html.UnescapeString(x.page("/order?id="+order, mod)); !strings.Contains(page, "Spent outside the market by transaction "+spender) {
		t.Fatalf("staff order page:\n%s", page)
	}
	// The moderation desk lists it as an open payment review with the honest reason.
	if desk := html.UnescapeString(x.page("/moderator", mod)); !strings.Contains(desk, "Escrow deposit spent by a transaction this market did not make, with no settlement of the market covering it (transaction "+spender+")") {
		t.Fatalf("moderation desk:\n%s", desk)
	}
	// Repeated passes flag it once.
	x.poll()
	x.poll()
	if strings.Count(strings.Join(x.events(order), "\n"), "did not make") != 1 {
		t.Fatal("flagged more than once")
	}
	// A claim requested now never spends the co-spent deposit.
	creates := x.core.count("createpsbt")
	if code, body := x.claim(order, x.vendorSess, regtestAddr("claim-cospend")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	if x.core.count("createpsbt") != creates {
		t.Fatal("a settlement was built over a deposit spent outside the market")
	}
	if _, _, state, class := x.settlement(order); state != "failed" {
		t.Fatalf("the claim of an escrow whose only deposit was spent outside: %s %s", state, class)
	}

	// An order awaiting payment whose deposit the parties co-spend never counts it as funding, even once it is deep.
	open := x.draft()
	addrO := x.mustCheckout(open, regtestAddr("refund-cospend-open"))
	x.core.deposit(addrO, txidOf("open-dep"), 0, 100000, 1)
	x.poll()
	x.coSpend(txidOf("open-dep"), 0, 1)
	x.poll()
	x.core.setConfs(txidOf("open-dep"), 6)
	x.poll()
	if st := x.state(open); st != stateAwaitingPayment || agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND NOT credited", open) != 1 {
		t.Fatalf("an awaiting order whose deposit was co-spent: %s", st)
	}

	// A deposit after the order closed that the parties co-spend gets no stray refund.
	closed := x.completed(regtestAddr("refund-cospend-late"), 100000)
	var addrL string
	x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", closed).Scan(&addrL)
	x.core.deposit(addrL, txidOf("late-dep"), 0, 30000, 1)
	x.poll()
	x.coSpend(txidOf("late-dep"), 0, 1)
	x.core.setConfs(txidOf("late-dep"), 6)
	x.poll()
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1 AND kind='stray_refund'", closed) != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND after_close AND spent_outside", closed) != 1 {
		t.Fatal("a co-spent late deposit was given a stray refund, or not marked")
	}

	// A spend the wallet does not name (and listtransactions refused): flagged without a transaction ID.
	order2 := x.completed(regtestAddr("refund-cospend2"), 100000)
	x.core.mu.Lock()
	x.core.listTxErr = -32601
	x.core.mu.Unlock()
	x.core.spend(txidOf(order2), 0)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by IS NULL AND flagged", order2) != 1 {
		t.Fatal("an unnamed outside spend was not flagged")
	}
	if ev := strings.Join(x.events(order2), "\n"); !strings.Contains(ev, "the market's watch-only wallet does not name it") {
		t.Fatalf("order notes:\n%s", ev)
	}
	x.core.mu.Lock()
	x.core.listTxErr = 0
	x.core.mu.Unlock()

	// A deposit claimed by a settlement is the settlement's business (external there), not flagged twice here.
	order3, id3 := x.built(regtestAddr("refund-cospend3"), regtestAddr("claim-cospend3"), 100000)
	x.signAs(order3, roleVendor, id3)
	other := x.coSpend(txidOf(order3), 0, 1)
	x.poll()
	if r := x.settleByID(id3); r.State != "external" || r.SpentBy != other {
		t.Fatalf("settlement whose deposit was co-spent: %+v (the wallet names %s)", r, other)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside", order3) != 0 {
		t.Fatal("a settlement's deposit was also marked as spent with no settlement")
	}
	x.noPayouts(order)
}

func TestEscrowOrphanAddressListed(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("orphanadmin", "admin")
	escrowAddr := x.mustCheckout(x.order, regtestAddr("refund-orphan"))
	orphan, unlabelled := regtestAddr("orphan-1"), regtestAddr("orphan-2")
	draft := x.draft()
	x.core.mu.Lock()
	x.core.labels[orphan] = "escrow:" + draft // a descriptor imported for a checkout that rolled back
	x.core.labels[unlabelled] = "not an escrow label"
	// A-365 (O3): an entry that is not in the address form the market stores is skipped, never written; it does not
	// stop the others being listed (escrow_orphans.address refuses it, which would roll the whole list back).
	x.core.labels["bcrt1q-not-an-address"] = "escrow:" + draft
	x.core.mu.Unlock()
	daily := func() {
		x.t.Helper()
		x.DB.Exec("INSERT INTO settings(key,value) VALUES('escrow_btc_stray_check','2000-01-01T00:00:00Z') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
		x.poll()
	}
	x.poll() // an ordinary pass reads no address book
	if _, params := x.core.lastCall(t, "listreceivedbyaddress"); params != `[0,false,true]` || agInt(x.testEnv, "SELECT count(*) FROM escrow_orphans") != 0 {
		t.Fatalf("an ordinary pass: %s", params)
	}
	daily()
	got := agStrings(t, x.testEnv, "SELECT address||' '||order_id FROM escrow_orphans ORDER BY address")
	want := []string{orphan + " " + draft, unlabelled + " "}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("orphans %q, want %q (escrow %s)", got, want, escrowAddr)
	}
	page := html.UnescapeString(x.page("/admin", admin))
	if !strings.Contains(page, "Escrow addresses with no escrow record") || !strings.Contains(page, orphan) || !strings.Contains(page, draft) || strings.Contains(page, ">"+escrowAddr+"<") {
		t.Fatalf("/admin orphan list:\n%s", page)
	}
	// Gone from the wallet: removed at the next daily check; still there: kept.
	x.core.mu.Lock()
	delete(x.core.labels, unlabelled)
	x.core.mu.Unlock()
	daily()
	if got := agStrings(t, x.testEnv, "SELECT address FROM escrow_orphans"); !slices.Equal(got, []string{orphan}) {
		t.Fatalf("after the wallet dropped one: %q", got)
	}
	// A-358: the last orphan gone from the wallet empties the list (the malformed entry is not one).
	x.core.mu.Lock()
	delete(x.core.labels, orphan)
	x.core.mu.Unlock()
	daily()
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_orphans"); n != 0 {
		t.Fatalf("after the wallet dropped the last orphan: %d rows remain", n)
	}
	if page := html.UnescapeString(x.page("/admin", admin)); strings.Contains(page, orphan) {
		t.Fatalf("/admin still lists the orphan the wallet dropped:\n%s", page)
	}
	// The adapter reads the address book with include_empty on the watch-only wallet, never listdescriptors.
	if path, params := x.core.lastCall(t, "listreceivedbyaddress"); path != fakeWatchWallet || params != `[0,true,true]` {
		t.Fatalf("listreceivedbyaddress %s %s", path, params)
	}
	if slices.Contains(x.core.methods(), "listdescriptors") {
		t.Fatal("listdescriptors called")
	}
}

// The reconciliation RPCs: exact params, and replies read into fixed shapes (a malformed one is a bad-reply class).
func TestEscrowRPCReconciliation(t *testing.T) {
	ctx := context.Background()
	e, core, _ := testEscrowAdapter(t)
	core.deposit("bcrt1qwatcheda", txidOf("fund"), 1, 70000, 3)
	core.labels = map[string]string{"bcrt1qwatcheda": "escrow:abc", "bcrt1qemptyaddress0": "escrow:def"}
	ws, err := e.watchedAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if path, params := core.lastCall(t, "listreceivedbyaddress"); path != fakeWatchWallet || params != `[0,true,true]` {
		t.Fatalf("listreceivedbyaddress %s %s", path, params)
	}
	if len(ws) != 2 || ws[0] != (escrowWatched{"bcrt1qwatcheda", "escrow:abc", 100000}) || ws[1] != (escrowWatched{"bcrt1qemptyaddress0", "escrow:def", 0}) {
		t.Fatalf("watched %+v", ws)
	}
	x := &escrowEnv{t: t, core: core}
	spender := x.coSpend(txidOf("fund"), 1, 0)
	got, err := e.walletSpenders(ctx, map[string]bool{txidOf("fund") + ":1": true, txidOf("other") + ":0": true})
	if err != nil {
		t.Fatal(err)
	}
	if path, params := core.lastCall(t, "listtransactions"); path != fakeWatchWallet || params != `["*",100,0,true]` {
		t.Fatalf("listtransactions %s %s", path, params)
	}
	if path, params := core.lastCall(t, "gettransaction"); path != fakeWatchWallet || params != `["`+spender+`",true]` {
		t.Fatalf("gettransaction %s %s", path, params)
	}
	if len(got) != 1 || got[txidOf("fund")+":1"] != spender {
		t.Fatalf("spenders %v", got)
	}
	core.listTxErr = -1
	if _, err := e.walletSpenders(ctx, map[string]bool{"x:0": true}); errorClassOf(err) != "listtransactions rpc:-1" {
		t.Fatalf("listtransactions error class %q", errorClassOf(err))
	}
}

// A-365 (F2, F6): findOutsideSpends checks only deposits in a block, and only of orders whose deposits were read in this
// pass. A deposit still in the mempool that gettxout no longer finds (evicted, or replaced before a block) is not
// flagged; an order whose wallet read failed in this pass is not checked, and is flagged on the next pass that reads it.
func TestEscrowOutsideSpendSkipsMempoolAndUnread(t *testing.T) {
	x := newEscrowEnv(t)
	// F2: a 0-confirmation deposit to an order awaiting payment, then gone from gettxout's view.
	open := x.draft()
	addr := x.mustCheckout(open, regtestAddr("refund-mempool"))
	x.core.deposit(addr, txidOf("mempool-dep"), 0, 100000, 0)
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND confirmations=0", open) != 1 {
		t.Fatal("the mempool deposit was not recorded")
	}
	x.core.spend(txidOf("mempool-dep"), 0)
	x.poll()
	x.poll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND (spent_outside OR flagged)", open); n != 0 {
		t.Fatalf("a deposit still in the mempool was flagged as spent outside the market (%d)", n)
	}
	// F6: a completed order's unclaimed deposit, co-spent while the wallet read fails.
	order := x.completed(regtestAddr("refund-unread"), 100000)
	spender := x.coSpend(txidOf(order), 0, 1)
	x.core.mu.Lock()
	x.core.txErr = -1
	x.core.mu.Unlock()
	if err := x.A.escrowPollOnce(context.Background()); err == nil {
		t.Fatal("the pass did not report the failed wallet read")
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside", order); n != 0 {
		t.Fatal("an order whose deposits were not read in this pass was checked for outside spends")
	}
	x.core.mu.Lock()
	x.core.txErr = 0
	x.core.mu.Unlock()
	x.poll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND spent_outside AND spent_by=$2 AND flagged", order, spender) != 1 {
		t.Fatal("the co-spent deposit was not flagged once its order was read again")
	}
}
