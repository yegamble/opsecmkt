package market

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// QA5W4A (rulings on QA5W4A-1 to -5 and the N17 NIT): the watch-only wallet's repair of shape-(b) escrows after a restart
// waits for the escrow signer instead of importing the siblings without their ranged descriptors, imports every lost
// escrow in one call with its own long timeout and never on top of a rescan, and rebuilds the order wallets from the keys
// checkout stored; the orphan sweep reads an unsolvable orphan only as a lost per-order escrow's sibling. DB integration
// with the simulated Core of escrow_fake_test.go and the fake signer: simulated RPC, not chain evidence
// (TestEscrowRegtestPerOrderRepair is the real Core leg).

// signerDown stops the fake signer (nothing listens on its socket: errSignerUnreachable) or starts it again.
func signerDown(f *fakeSigner, down bool) {
	if down {
		f.stop()
	} else {
		f.start()
	}
}

// signerGarbled makes the fake signer answer what the app does not understand (a refusal it has no class for), or
// answer properly again: like a signer that is down, a signer error that is not a pin mismatch.
func signerGarbled(f *fakeSigner, garbled bool) {
	f.set(func(f *fakeSigner) {
		f.raw = ""
		if garbled {
			f.raw = `{"error":"unavailable"}`
		}
	})
}

// solvable reports whether the fake watch wallet holds addr with its keys (a ranged import held it first).
func (x *escrowEnv) solvable(addr string) bool {
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	return x.core.rangedPubkeys[addr] != nil || x.core.watchPubkeys[addr] != nil
}

// QA5W4A-1: a signer that does not answer at the first watcher pass after a restart (with a watch-only wallet restored
// from before the escrow) makes the repair import only the escrow's own descriptor and wait: the pass reports it, the
// check stays to be done, and no sibling is imported as an address alone (Core would keep it unsolvable for good). A
// second pass, the signer now up but answering what the app does not understand, imports nothing more. Once the signer
// answers, the next pass imports the ranged descriptors and then the siblings, which are solvable; a later restart
// imports nothing.
func TestEscrowShapeBRepairWaitsForSigner(t *testing.T) {
	x, f := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("wait-signer"))
	created := agInt64(t, x.testEnv, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", x.order)
	first := x.core.importRequests()
	sib := x.sibling(x.order, 0, 3)
	x.core.forgetWatched()
	signerDown(f, true)
	x.restart()
	ctx := context.Background()
	for pass := range 2 {
		if pass == 1 {
			signerGarbled(f, true)
			signerDown(f, false)
		}
		err := x.A.escrowPollOnce(ctx)
		if err == nil || !strings.Contains(watcherCause(err), "waits for the escrow signer") {
			t.Fatalf("pass %d with the signer down: %v", pass, err)
		}
		if x.A.escrowSiblingsChecked.Load() {
			t.Fatalf("pass %d: the sibling check counted as done while the signer is down", pass)
		}
		reqs := x.core.importRequests()[len(first):]
		if len(reqs) != 1 || reqs[0]["desc"] != first[0]["desc"] || reqs[0]["label"] != "escrow:"+x.order || reqs[0]["timestamp"] != float64(created) {
			t.Fatalf("pass %d with the signer down imported %v, want the escrow's own descriptor once", pass, reqs)
		}
	}
	x.core.mu.Lock()
	held := x.core.watched[sib]
	x.core.mu.Unlock()
	if held {
		t.Fatal("a sibling was imported while the signer was down")
	}
	signerGarbled(f, false)
	x.poll()
	reqs := x.core.importRequests()[len(first)+1:]
	if len(reqs) != 31 {
		t.Fatalf("%d imports once the signer answered, want the two ranged descriptors and the 29 siblings: %v", len(reqs), reqs)
	}
	for i, r := range reqs {
		want := maps.Clone(first[i+1])
		want["timestamp"] = float64(created)
		if !mapsEqualJSON(r, want) {
			t.Fatalf("import %d %v, want checkout's %v from the escrow's creation time", i, r, want)
		}
	}
	if !x.solvable(sib) || !x.A.escrowSiblingsChecked.Load() {
		t.Fatalf("sibling solvable %v, check done %v", x.solvable(sib), x.A.escrowSiblingsChecked.Load())
	}
	n := len(x.core.importRequests())
	x.restart()
	x.poll()
	if got := len(x.core.importRequests()); got != n {
		t.Fatalf("%d imports after a restart with everything held", got-n)
	}
}

// QA5W4A-1: a signer whose key differs from the pins cannot rebuild the market's key: the repair imports the escrow's
// own descriptor, leaves the siblings out (never as addresses alone) and logs it, and the check is done for this
// process; the next start with the pinned signer back imports the ranged descriptors and the siblings.
func TestEscrowShapeBRepairSignerMismatch(t *testing.T) {
	logs := captureLog(t)
	x, f := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("repair-mismatch"))
	n := len(x.core.importRequests())
	sib := x.sibling(x.order, 1, 9)
	x.core.forgetWatched()
	pinned := f.key
	f.set(func(f *fakeSigner) { f.key = signerKey(t, 8) })
	x.restart()
	x.poll()
	reqs := x.core.importRequests()[n:]
	if len(reqs) != 1 || reqs[0]["label"] != "escrow:"+x.order {
		t.Fatalf("repair with a signer that differs from the pins: %v", reqs)
	}
	if !strings.Contains(logs.String(), "sibling addresses of 1 per-order escrow(s) were not imported again") {
		t.Fatalf("log: %s", logs.String())
	}
	f.set(func(f *fakeSigner) { f.key = pinned })
	x.restart()
	x.poll()
	if reqs = x.core.importRequests()[n+1:]; len(reqs) != 31 || !x.solvable(sib) {
		t.Fatalf("after the pinned signer came back: %d imports, sibling solvable %v", len(reqs), x.solvable(sib))
	}
}

// QA5W4A-3: the repair imports every lost escrow in one importdescriptors call (Core rescans once, from the earliest
// creation time), with its own timeout: an import that outlives rpcTimeout but not escrowRescanTimeout succeeds, and a
// deposit made meanwhile is credited in the same pass. Checkout's own import ("now") keeps rpcTimeout: the same slow
// node fails it and nothing is stored.
func TestEscrowShapeBRepairOneRescan(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	var orders []string
	var created []float64
	for i := range 3 {
		o := x.order
		if i < 2 {
			o = x.checkoutAside("one-rescan-" + strconv.Itoa(i))
		} else {
			x.mustCheckout(o, regtestAddr("one-rescan-2"))
		}
		orders = append(orders, o)
		created = append(created, float64(agInt64(t, x.testEnv, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", o)))
	}
	addr := agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", orders[2])[0]
	price := agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", orders[2])
	x.poll()
	x.core.forgetWatched()
	x.core.deposit(addr, txidOf("one-rescan-main"), 0, price, 6)
	defer func(r, s time.Duration) { rpcTimeout, escrowRescanTimeout = r, s }(rpcTimeout, escrowRescanTimeout)
	rpcTimeout, escrowRescanTimeout = 6*time.Second, 40*time.Second
	x.core.mu.Lock()
	x.core.beforeImport = func() { time.Sleep(7 * time.Second) }
	x.core.mu.Unlock()
	x.restart()
	calls, n := x.core.count("importdescriptors"), len(x.core.importRequests())
	x.poll()
	if got := x.core.count("importdescriptors") - calls; got != 1 {
		t.Fatalf("%d importdescriptors calls for three lost escrows, want one", got)
	}
	reqs := x.core.importRequests()[n:]
	if len(reqs) != 96 {
		t.Fatalf("%d requests, want 32 for each of three escrows", len(reqs))
	}
	for i, r := range reqs {
		if r["timestamp"] != created[i/32] {
			t.Fatalf("request %d timestamp %v, want its escrow's creation %v", i, r["timestamp"], created[i/32])
		}
	}
	if got := agStrings(t, x.testEnv, "SELECT credited::text FROM payments WHERE order_id=$1", orders[2]); strings.Join(got, ",") != "true" {
		t.Fatalf("deposit made while the wallet lacked the escrow: %v", got)
	}
	draft := x.draft()
	calls = x.core.count("importdescriptors")
	if w := x.checkout(draft, regtestAddr("one-rescan-checkout")); w.Code == 303 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", draft) != 0 {
		t.Fatalf("a checkout import outliving rpcTimeout: %d", w.Code)
	}
	if got := x.core.count("importdescriptors") - calls; got != 1 {
		t.Fatalf("checkout made %d imports, want the one that timed out", got)
	}
}

// QA5W4A-3: no second rescan on top of one. While the watch-only wallet rescans (the operator's rescanblockchain), the
// repair imports nothing and waits; an import Core refuses as rescanning, and one whose call timed out while Core goes
// on importing, are waits too: the next pass asks getwalletinfo first and imports nothing while Core still scans, and
// once it is done finds everything held.
func TestEscrowShapeBRepairWaitsForRescan(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("wait-rescan"))
	ctx := context.Background()
	wait := func(want string) {
		t.Helper()
		err := x.A.escrowPollOnce(ctx)
		if err == nil || !strings.Contains(watcherCause(err), want) || x.A.escrowSiblingsChecked.Load() {
			t.Fatalf("pass: %v (check done %v), want a wait: %s", err, x.A.escrowSiblingsChecked.Load(), want)
		}
	}
	set := func(fn func(c *fakeEscrowCore)) {
		x.core.mu.Lock()
		defer x.core.mu.Unlock()
		fn(x.core)
	}

	x.core.forgetWatched()
	set(func(c *fakeEscrowCore) { c.scanning = true })
	x.restart()
	calls := x.core.count("importdescriptors")
	wait("the wallet is rescanning")
	if got := x.core.count("importdescriptors"); got != calls {
		t.Fatalf("%d imports started while the wallet was rescanning", got-calls)
	}
	set(func(c *fakeEscrowCore) { c.scanning = false })
	x.poll()
	if got := x.core.count("importdescriptors") - calls; got != 1 || !x.A.escrowSiblingsChecked.Load() {
		t.Fatalf("%d imports once the rescan ended", got)
	}

	x.core.forgetWatched()
	set(func(c *fakeEscrowCore) { c.rescanRefuses = true })
	x.restart()
	wait("import answered importdescriptors rescanning")
	set(func(c *fakeEscrowCore) { c.rescanRefuses = false })
	x.poll()

	defer func(d time.Duration) { escrowRescanTimeout = d }(escrowRescanTimeout)
	escrowRescanTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	set(func(c *fakeEscrowCore) { c.beforeImport = func() { <-release } })
	x.core.forgetWatched()
	x.restart()
	calls = x.core.count("importdescriptors")
	wait("import answered importdescriptors timeout")
	wait("the wallet is rescanning")
	if got := x.core.count("importdescriptors") - calls; got != 1 {
		t.Fatalf("%d imports, want the one Core is still running", got)
	}
	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; {
		x.core.mu.Lock()
		busy := x.core.importing
		x.core.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake import did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	x.poll()
	if got := x.core.count("importdescriptors") - calls; got != 1 || !x.A.escrowSiblingsChecked.Load() {
		t.Fatalf("%d imports after the timed-out one finished, want none more", got-1)
	}
}

// QA5W4A-5: checkout stores the buyer's and the vendor's order wallet key (the account key's child <i>, rooted) beside
// their keys, and none for the market; perOrderXpub gives each party's order wallet key, the market's from the escrow
// signer, and refuses a stored key that does not give the escrow's. Migration 089 allows per_order_xpub only on a
// buyer's or a vendor's per-order key, as a tpub.
func TestEscrowShapeBOrderXpubStored(t *testing.T) {
	x, f := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("xpub-stored"))
	m := uint32(x.marketIndex(x.order))
	id := agInt64(t, x.testEnv, "SELECT id FROM escrows WHERE order_id=$1", x.order)
	ctx := context.Background()
	for party, want := range map[string]bip32Key{"buyer": accountKey(t, x.buyerKey), "vendor": accountKey(t, x.vendorKey), "market": f.key} {
		i := uint32(2)
		if party == "market" {
			i = m
		}
		child, _ := perOrderChild(t, want, i)
		stored := agStrings(t, x.testEnv, "SELECT coalesce(per_order_xpub,'-') FROM escrow_pubkeys WHERE escrow_id=$1 AND party=$2", id, party)
		if wantStored := child.Rooted(versionTpub); party == "market" && stored[0] != "-" || party != "market" && stored[0] != wantStored {
			t.Fatalf("%s per_order_xpub %q, want %q", party, stored[0], wantStored)
		}
		got, err := x.A.perOrderXpub(ctx, x.DB, id, party)
		if err != nil || got.Rooted(versionTpub) != child.Rooted(versionTpub) {
			t.Fatalf("perOrderXpub %s: %v", party, err)
		}
	}
	other, _ := perOrderChild(t, accountKey(t, x.buyerKey), 3)
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET per_order_xpub=$2 WHERE escrow_id=$1 AND party='vendor'", id, other.Rooted(versionTpub))
	if _, err := x.A.perOrderXpub(ctx, x.DB, id, "vendor"); !errors.Is(err, errPerOrderStored) {
		t.Fatalf("a stored order wallet key that does not give the stored key: %v", err)
	}
	if _, err := x.A.perOrderXpub(ctx, x.DB, id+1000, "buyer"); !errors.Is(err, errPerOrderStored) {
		t.Fatalf("no such escrow: %v", err)
	}
	signerDown(f, true)
	if _, err := x.A.perOrderXpub(ctx, x.DB, id, "market"); !errors.Is(err, errSignerUnreachable) {
		t.Fatalf("market key with the signer down: %v", err)
	}
	signerDown(f, false)
	f.set(func(f *fakeSigner) { f.key = signerKey(t, 8) })
	var refused *signerRefusal
	if _, err := x.A.perOrderXpub(ctx, x.DB, id, "market"); !errors.As(err, &refused) || refused.Class != signerMismatch {
		t.Fatalf("market key with a signer whose key differs from the pins: %v", err)
	}
	for _, c := range []struct{ party, xpub string }{
		{"market", other.Rooted(versionTpub)},
		{"buyer", "tpub-not-a-key"},
		{"buyer", other.Rooted(versionVpub)},
	} {
		if _, err := x.DB.Exec("UPDATE escrow_pubkeys SET per_order_xpub=$2 WHERE escrow_id=$1 AND party=$3", id, c.xpub, c.party); err == nil || !strings.Contains(err.Error(), "escrow_pubkeys_per_order_xpub") {
			t.Fatalf("%s per_order_xpub %q: %v", c.party, c.xpub, err)
		}
	}
	core := x.draft()
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	x.mustCheckout(core, regtestAddr("xpub-core"))
	if _, err := x.DB.Exec("UPDATE escrow_pubkeys SET per_order_xpub=$2 WHERE escrow_id=(SELECT id FROM escrows WHERE order_id=$1) AND party='buyer'", core, other.Rooted(versionTpub)); err == nil {
		t.Fatal("per_order_xpub accepted on a Bitcoin Core escrow key")
	}
}

// QA5W4A-5: after the vendor registers another key, a watch-only wallet restored from before the escrow is still
// repaired with the order wallet's ranged descriptors (rebuilt from the order wallet keys stored at checkout, not the
// vendor's current key), so the siblings are solvable.
func TestEscrowShapeBRepairAfterKeyChange(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("repair-key-changed"))
	first := x.core.importRequests()
	sib := x.sibling(x.order, 0, 1)
	x.activeKey(x.vendor, altEscrowKey(t, x.vendorKey, "5eed5eed", 3))
	x.core.forgetWatched()
	x.restart()
	x.poll()
	reqs := x.core.importRequests()[len(first):]
	if len(reqs) != 32 {
		t.Fatalf("%d repair requests, want the escrow's descriptor, the two ranged ones and the 29 siblings: %v", len(reqs), reqs)
	}
	for i := 1; i <= 2; i++ {
		if reqs[i]["desc"] != first[i]["desc"] {
			t.Fatalf("ranged import %v, want checkout's %v", reqs[i]["desc"], first[i]["desc"])
		}
	}
	if !x.solvable(sib) {
		t.Fatal("sibling not solvable after the repair")
	}
}

// N17 (QA5W4A): the repair rebuilds the escrow's own descriptor only when the stored keys give its address. With a stored
// key corrupted, nothing of that escrow is imported (the order wallet of the party no longer gives its stored key either),
// and the log says so.
func TestEscrowShapeBRepairBareAddressCheck(t *testing.T) {
	logs := captureLog(t)
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("repair-bare-check"))
	n := len(x.core.importRequests())
	id := agInt64(t, x.testEnv, "SELECT id FROM escrows WHERE order_id=$1", x.order)
	agExec(x.testEnv, "UPDATE escrow_pubkeys SET pubkey='02'||repeat('ab',32) WHERE escrow_id=$1 AND party='buyer'", id)
	x.core.forgetWatched()
	x.restart()
	x.poll()
	if reqs := x.core.importRequests()[n:]; len(reqs) != 0 {
		t.Fatalf("imports for an escrow whose stored keys do not give its address: %v", reqs)
	}
	for _, want := range []string{"per-order escrow of order " + shortID(x.order) + ": its stored keys do not give its address", "sibling addresses of 1 per-order escrow(s) were not imported again"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log lacks %q: %s", want, logs.String())
		}
	}
}

// QA5W4A-2: a Bitcoin Core (shape (a)) escrow the watch-only wallet holds only as a labelled addr(…) (imported again by
// hand after a watch-only wallet restore; Core 31.1 reports it solvable false) and lost by a database restore has no keys
// the sweep can read: it is no per-order escrow's sibling (no other orphan of its label gave three keys; another lost
// escrow's, of another order, does not count), so the sweep stays incomplete, the restore's marker stays set and
// checkout stays refused. Re-imported with its descriptor, as the runbook says, the sweep reserves its keys and finishes.
func TestEscrowSweepLoneUnsolvableOrphanKeepsGate(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	lost := x.escrowPubkeys(x.order)
	other := x.draft()
	x.mustCheckout(other, "bcrt1qbuyerrefund00000000000000000000002")
	reqs := x.core.importRequests()
	x.core.forgetWatched()
	x.core.mu.Lock()
	x.core.importOne(fakeWatchWallet, map[string]any{"desc": withChecksum("addr(" + addr + ")"), "timestamp": float64(0), "label": "escrow:" + x.order})
	x.core.importOne(fakeWatchWallet, reqs[1]) // the other escrow, imported again with its descriptor
	x.core.mu.Unlock()
	last, called := x.marketCounter()
	x.lose(last, called, x.order, other)
	for range 2 {
		err := x.A.escrowPollOnce(context.Background())
		if err == nil || !strings.Contains(watcherCause(err), "orphan sweep incomplete") || !x.sweepRequired() {
			t.Fatalf("pass over a lone unsolvable orphan: %v, marker set %v", err, x.sweepRequired())
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_reserved_keys WHERE address=$1", addr); n != 0 {
		t.Fatalf("%d keys reserved at the unsolvable address", n)
	}
	if w := x.checkout(x.draft(), regtestAddr("lone-orphan")); w.Code != 503 {
		t.Fatalf("checkout while the sweep is incomplete: %d", w.Code)
	}
	x.core.forgetWatched() // the watch-only wallet restored again: Core keeps an address an addr(…) import held first unsolvable
	x.core.mu.Lock()
	x.core.importOne(fakeWatchWallet, reqs[0])
	x.core.importOne(fakeWatchWallet, reqs[1])
	x.core.mu.Unlock()
	x.poll()
	if x.sweepRequired() {
		t.Fatal("the sweep did not finish once the escrow was imported with its descriptor")
	}
	if got := agStrings(t, x.testEnv, "SELECT pubkey FROM escrow_reserved_keys WHERE address=$1 ORDER BY pubkey", addr); !slices.Equal(got, lost) {
		t.Fatalf("reserved %v, want %v", got, lost)
	}
}
