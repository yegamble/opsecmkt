package market

import (
	"html"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-359: a restored database knows nothing of the escrows created after its backup, so it would hand out their child
// keys again and compose the same 2-of-3 address for the same buyer and vendor. Escrow checkout therefore waits while
// the restore gate is set (a), and every checkout first asks the watch-only wallet whether it already watches the
// address it composed (b). Fake escrow Core (simulated RPC, not chain evidence; TestEscrowRegtestGate has the real
// Core leg).

const escrowRestoringText = "Bitcoin escrow is paused while this market restores its records from a backup. Nothing was charged and your order is still a draft; try again later."

// (a) While the restore gate is set, escrow checkout answers a fixed 503 before any node call and the order stays a
// draft; the order page says why instead of offering the form. Once the gate is cleared, checkout works again.
func TestEscrowCheckoutPausedByRestoreGate(t *testing.T) {
	x := newEscrowEnv(t)
	x.setGate(true)
	calls := len(x.core.methods())
	page := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
	if !strings.Contains(page, "Unavailable.") || !strings.Contains(page, "Bitcoin escrow is paused while this market restores its records from a backup") || strings.Contains(page, `action="/orders/escrow"`) {
		t.Fatalf("the escrow offer under the restore gate:\n%s", page)
	}
	w := x.checkout(x.order, regtestAddr("refund-gate"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), escrowRestoringText) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("checkout under the restore gate: %d %q %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	x.nothingWritten(x.order)
	if got := x.core.methods()[calls:]; len(got) != 0 {
		t.Fatalf("checkout under the restore gate called the node: %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", x.order); n != 0 {
		t.Fatalf("checkout under the restore gate wrote %d order events", n)
	}
	x.setGate(false)
	if addr := x.mustCheckout(x.order, regtestAddr("refund-gate")); addr == "" {
		t.Fatal("no escrow after the gate was cleared")
	}
}

// watchInfoCalls is the params of every getaddressinfo call on the watch-only wallet, in order.
func (x *escrowEnv) watchInfoCalls() []string {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	var out []string
	for _, c := range x.core.calls {
		if c.Method == "getaddressinfo" && c.Path == fakeWatchWallet {
			out = append(out, string(c.Params))
		}
	}
	return out
}

// escrowKeys is the market index and each party's derivation suffix of order's escrow.
func (x *escrowEnv) escrowKeys(order string) (int64, []string) {
	x.t.Helper()
	var idx int64
	if err := x.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", order).Scan(&idx); err != nil {
		x.t.Fatal(err)
	}
	return idx, agStrings(x.t, x.testEnv, "SELECT party||' '||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 ORDER BY party", order)
}

// (b) An escrow created after the backup a database was restored from: the restored database has no row for it and
// its market counter is back where the backup left it, but the watch-only wallet still watches its address. The next
// checkout for the same buyer and vendor composes that very address first; it is skipped, with every index it used,
// and the escrow gets the next child keys and a fresh address. The lost escrow's label is not overwritten, and the
// skipped indices are never handed out later.
func TestEscrowCheckoutSkipsAddressTheWalletWatches(t *testing.T) {
	logs := captureLog(t)
	x := newEscrowEnv(t)
	want := func(party, market int64) []string {
		p := "/0/" + strconv.FormatInt(party, 10)
		return []string{"buyer " + p, "market /0/" + strconv.FormatInt(market, 10), "vendor " + p}
	}
	lost := x.mustCheckout(x.order, regtestAddr("refund-lost"))
	lostIdx, lostKeys := x.escrowKeys(x.order)
	if !slices.Equal(lostKeys, want(1, lostIdx)) {
		t.Fatalf("the escrow created after the backup: %v", lostKeys)
	}
	// The restore: the database from before that escrow. Its rows are gone and the market counter is back; the order
	// is set aside so nothing else reads it. The watch-only wallet keeps the escrow, labelled with its order.
	for _, q := range []string{
		"DELETE FROM escrow_pubkeys WHERE escrow_id IN (SELECT id FROM escrows WHERE order_id=$1)",
		"DELETE FROM escrows WHERE order_id=$1",
		"UPDATE orders SET state='cancelled' WHERE id=$1",
	} {
		if _, err := x.DB.Exec(q, x.order); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.DB.Exec("SELECT setval('escrow_market_index',$1,false)", lostIdx); err != nil {
		t.Fatal(err)
	}
	asked := len(x.watchInfoCalls())
	imports := len(x.core.importedDescriptors())

	order2 := x.draft()
	fresh := x.mustCheckout(order2, regtestAddr("refund-after-restore"))
	idx2, keys2 := x.escrowKeys(order2)
	if fresh == lost || idx2 != lostIdx+1 || !slices.Equal(keys2, want(2, lostIdx+1)) {
		t.Fatalf("the escrow after the restore: %s (lost %s), market index %d, keys %v", fresh, lost, idx2, keys2)
	}
	if got := x.watchInfoCalls()[asked:]; !slices.Equal(got, []string{`["` + lost + `"]`, `["` + fresh + `"]`}) {
		t.Fatalf("getaddressinfo on the watch-only wallet: %v", got)
	}
	got := x.core.importedDescriptors()[imports:]
	x.core.mu.Lock()
	label := x.core.labels[lost]
	importedAddr := ""
	if len(got) == 1 {
		importedAddr = x.core.address(strings.Split(got[0], "#")[0])
	}
	x.core.mu.Unlock()
	if importedAddr != fresh {
		t.Fatalf("imports after the restore: %v", got)
	}
	if label != "escrow:"+x.order {
		t.Fatalf("the lost escrow's label was overwritten: %q", label)
	}
	line := "escrow: escrow for order " + shortID(order2) + ": its address is already in the watch-only wallet (a restored database?); trying the next child keys"
	if !strings.Contains(logs.String(), line) || strings.Contains(logs.String(), lost) {
		t.Fatalf("log:\n%s", logs.String())
	}
	// The skipped indices are never handed out: the next escrow continues after the ones used.
	order3 := x.draft()
	x.mustCheckout(order3, regtestAddr("refund-after-restore-3"))
	if idx3, keys3 := x.escrowKeys(order3); idx3 != lostIdx+2 || !slices.Equal(keys3, want(3, lostIdx+2)) {
		t.Fatalf("the next escrow: market index %d, keys %v", idx3, keys3)
	}
}

// (b) When every address tried is already watched, the checkout gives up after escrowWatchedTries with a fixed 503 and
// a fixed log line, writes nothing and imports nothing; a failing wallet read is the node's refusal (503), nothing
// written either.
func TestEscrowCheckoutWatchedAddressRetriesBounded(t *testing.T) {
	logs := captureLog(t)
	x := newEscrowEnv(t)
	var before int64
	x.DB.QueryRow("SELECT nextval('escrow_market_index')").Scan(&before)
	x.core.mu.Lock()
	x.core.watchAll = true
	x.core.mu.Unlock()
	imports := len(x.core.importedDescriptors())
	w := x.checkout(x.order, regtestAddr("refund-exhausted"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "every escrow address the market composed for this order is already watched by its escrow wallet") ||
		strings.Contains(w.Body.String(), "bcrt1q") {
		t.Fatalf("exhausted: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if n := len(x.watchInfoCalls()); n != escrowWatchedTries || escrowWatchedTries != 20 {
		t.Fatalf("%d getaddressinfo calls, want %d", n, escrowWatchedTries)
	}
	if len(x.core.importedDescriptors()) != imports {
		t.Fatal("an escrow was imported although every address was watched")
	}
	// Every market index tried is skipped, never reused.
	var after int64
	x.DB.QueryRow("SELECT nextval('escrow_market_index')").Scan(&after)
	if after != before+escrowWatchedTries+1 {
		t.Fatalf("market index %d after %d, want %d", after, before, before+escrowWatchedTries+1)
	}
	line := "escrow: escrow for order " + shortID(x.order) + " not built: every address tried is already in the watch-only wallet (20 tries)"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log:\n%s", logs.String())
	}
	// The wallet read fails: the node did not confirm it.
	x.core.mu.Lock()
	x.core.watchAll, x.core.watchInfoErr = false, -1
	x.core.mu.Unlock()
	w = x.checkout(x.order, regtestAddr("refund-exhausted"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "did not confirm it") {
		t.Fatalf("wallet read failed: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if !strings.Contains(logs.String(), "escrow: escrow for order "+shortID(x.order)+" not built: getaddressinfo rpc:-1") {
		t.Fatalf("log:\n%s", logs.String())
	}
	x.core.mu.Lock()
	x.core.watchInfoErr = 0
	x.core.mu.Unlock()
	x.mustCheckout(x.order, regtestAddr("refund-exhausted"))
}
