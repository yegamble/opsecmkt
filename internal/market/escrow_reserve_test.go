package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"html"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow P2-e (A-366, rule P2-R10): after a restore from backup no escrow child key is handed out again, not even inside
// another escrow address. The exact layer: the orphan sweep reserves the three keys of every address the watch-only
// wallet holds with no escrow row (escrow_reserved_keys), checkout skips any child key reserved or on record, moving
// only the colliding party's index (or the market index), and checkout waits until a sweep has run after the restore
// (escrow_sweep_required). The margin layer, for a watch-only wallet restored from the same backup: scripts/restore.sh
// moves each account's next child index (escrow_index_floors) and the market index 50 past the restored records. DB
// integration with the simulated Core (not chain evidence; TestEscrowRegtestReserve is the real Core leg).

const escrowSweepText = "Bitcoin escrow is paused until this market has checked its escrow wallet after restoring its records from a backup. Nothing was charged and your order is still a draft; try again later."

// restoreMarginSQL is the escrow margin block of scripts/restore.sh (between its escrow-key-margin markers), run here as
// the script runs it, so these tests exercise the script's own SQL.
func restoreMarginSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("scripts/restore.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(b), "-- escrow-key-margin: begin\n")
	block, _, ok2 := strings.Cut(block, "-- escrow-key-margin: end\n")
	if !ok || !ok2 || strings.Contains(block, `\`) {
		t.Fatal("scripts/restore.sh has no plain-SQL escrow-key-margin block")
	}
	return block
}

// lose simulates a database restored from a backup taken before orders' escrows were created: their rows are gone, the
// orders are set aside, the market counter is back at (last, called), and the restore's sweep marker is set as
// scripts/restore.sh sets it. The watch-only wallet keeps every escrow it imported.
func (x *escrowEnv) lose(last int64, called bool, orders ...string) {
	x.t.Helper()
	for _, o := range orders {
		for _, q := range []string{
			"DELETE FROM escrow_pubkeys WHERE escrow_id IN (SELECT id FROM escrows WHERE order_id=$1)",
			"DELETE FROM escrows WHERE order_id=$1",
			"UPDATE orders SET state='cancelled' WHERE id=$1",
		} {
			if _, err := x.DB.Exec(q, o); err != nil {
				x.t.Fatal(err)
			}
		}
	}
	if _, err := x.DB.Exec("SELECT setval('escrow_market_index',$1,$2)", last, called); err != nil {
		x.t.Fatal(err)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_sweep_required','true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
}

// marketCounter is the escrow_market_index sequence's state.
func (x *escrowEnv) marketCounter() (int64, bool) {
	x.t.Helper()
	var last int64
	var called bool
	if err := x.DB.QueryRow("SELECT last_value,is_called FROM escrow_market_index").Scan(&last, &called); err != nil {
		x.t.Fatal(err)
	}
	return last, called
}

// escrowPubkeys is the three stored public keys of each order's escrow.
func (x *escrowEnv) escrowPubkeys(orders ...string) []string {
	return agStrings(x.t, x.testEnv, "SELECT pubkey FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=ANY($1) ORDER BY pubkey", orders)
}

func (x *escrowEnv) sweepRequired() bool {
	return agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key='escrow_sweep_required'") != 0
}

// Migration 083: escrow_reserved_keys holds compressed public keys once each, with the orphan address they were read
// from; escrow_index_floors one next child index per account, at least 1.
func TestEscrowMigration083(t *testing.T) {
	x := newEscrowEnv(t)
	var last int
	if err := x.DB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&last); err != nil || last < 83 {
		t.Fatalf("schema_migrations: %d %v", last, err)
	}
	key := "02" + strings.Repeat("ab", 32)
	try := func(q string, args ...any) bool {
		t.Helper()
		_, err := x.DB.Exec(q, args...)
		return err == nil
	}
	if !try("INSERT INTO escrow_reserved_keys(pubkey,address) VALUES($1,$2)", key, regtestAddr("m83")) {
		t.Fatal("a reserved key was refused")
	}
	for name, q := range map[string]string{
		"a key twice":        "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES('" + key + "','" + regtestAddr("m83b") + "')",
		"uncompressed":       "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES('04" + strings.Repeat("ab", 64) + "','" + regtestAddr("m83") + "')",
		"upper-case hex":     "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES('02" + strings.Repeat("AB", 32) + "','" + regtestAddr("m83") + "')",
		"short":              "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES('02ab','" + regtestAddr("m83") + "')",
		"address not a form": "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES('03" + strings.Repeat("cd", 32) + "','bcrt1q-not)')",
		"floor 0":            "INSERT INTO escrow_index_floors(user_id,next_index) VALUES('" + x.buyer + "',0)",
		"floor no user":      "INSERT INTO escrow_index_floors(user_id,next_index) VALUES('no-such-user',5)",
	} {
		if try(q) {
			t.Errorf("%s: allowed", name)
		}
	}
	if !try("INSERT INTO escrow_index_floors(user_id,next_index) VALUES($1,51)", x.buyer) || try("INSERT INTO escrow_index_floors(user_id,next_index) VALUES($1,52)", x.buyer) {
		t.Fatal("one floor per account")
	}
}

// The exact layer, on the A-366 reproduction: an escrow created after the backup for another buyer and the same vendor
// is lost by a database-only restore. Checkout waits for the sweep; the sweep reserves the lost escrow's three keys
// (read with getaddressinfo); the next escrow, for the first buyer, composes another address (so the A-359 address
// check alone would pass it) but its vendor key and market key would be the lost escrow's: each is skipped, only the
// vendor's index and the market index move, the buyer's stays, and no key is shared. A key on record in escrow_pubkeys
// is skipped the same way (it used to refuse the checkout). Normal operation is unchanged: the same indices and the same
// node calls as before.
func TestEscrowRestoreSkipsReservedKeys(t *testing.T) {
	logs := captureLog(t)
	x := newEscrowEnv(t)
	b2, sess2 := x.user("ebuyer2_"+randomToken()[:6], "buyer")
	x.activeKey(b2, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 3))
	checkout2 := func(refund string) string {
		t.Helper()
		o := x.testEnv.order(b2, x.autoListing(x.vendor), "BTC", stateDraft)
		if w := x.do("POST", "/orders/escrow", sess2, map[string][]string{"order_id": {o}, "refund_address": {refund}}); w.Code != 303 {
			t.Fatalf("checkout of the second buyer: %d %s", w.Code, w.Body.String())
		}
		return o
	}
	// Normal operation: the first escrow takes /0/1 for both parties and the next market index, with the same node calls
	// as before P2-e (no extra RPC on the checkout path).
	calls := len(x.core.methods())
	x.mustCheckout(x.order, regtestAddr("refund-e0"))
	m0, keys0 := x.escrowKeys(x.order)
	if !slices.Equal(keys0, []string{"buyer /0/1", "market /0/" + strconv.FormatInt(m0, 10), "vendor /0/1"}) {
		t.Fatalf("the first escrow: %v", keys0)
	}
	if got := x.core.methods()[calls:]; !slices.Equal(got, []string{"getdescriptorinfo", "deriveaddresses", "createpsbt", "utxoupdatepsbt", "getdescriptorinfo", "deriveaddresses", "getaddressinfo", "importdescriptors"}) {
		t.Fatalf("node calls of a checkout: %v", got)
	}
	// The backup point; then an escrow for the second buyer and the same vendor, lost by the restore.
	last, called := x.marketCounter()
	lost := checkout2(regtestAddr("refund-lost"))
	mLost, keysLost := x.escrowKeys(lost)
	if !slices.Equal(keysLost, []string{"buyer /0/1", "market /0/" + strconv.FormatInt(mLost, 10), "vendor /0/2"}) {
		t.Fatalf("the lost escrow: %v", keysLost)
	}
	lostKeys := x.escrowPubkeys(lost)
	var lostAddr string
	x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", lost).Scan(&lostAddr)
	x.lose(last, called, lost)

	// Until the sweep has run, checkout waits.
	o1 := x.draft()
	if w := x.checkout(o1, regtestAddr("refund-n1")); w.Code != 503 || !strings.Contains(w.Body.String(), escrowSweepText) {
		t.Fatalf("checkout before the sweep: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(o1)
	x.poll() // the sweep runs on the next pass while the marker is set, not only on the daily check
	if got := agStrings(t, x.testEnv, "SELECT pubkey FROM escrow_reserved_keys WHERE address=$1 ORDER BY pubkey", lostAddr); !slices.Equal(got, lostKeys) {
		t.Fatalf("reserved keys %v, want the lost escrow's %v", got, lostKeys)
	}
	if x.sweepRequired() {
		t.Fatal("a full sweep did not clear escrow_sweep_required")
	}
	if got := agStrings(t, x.testEnv, "SELECT address FROM escrow_orphans"); !slices.Equal(got, []string{lostAddr}) {
		t.Fatalf("orphans %v", got)
	}

	// The next escrow for the first buyer: the buyer's /0/2 is free; the vendor's /0/2 and the market index are the lost
	// escrow's, so only those move on. The skipped build asks the watch-only wallet nothing.
	asked := len(x.watchInfoCalls())
	x.mustCheckout(o1, regtestAddr("refund-n1"))
	m1, keys1 := x.escrowKeys(o1)
	if !slices.Equal(keys1, []string{"buyer /0/2", "market /0/" + strconv.FormatInt(mLost+1, 10), "vendor /0/3"}) || m1 != mLost+1 {
		t.Fatalf("the escrow after the restore: %v (lost %v)", keys1, keysLost)
	}
	if n := len(x.watchInfoCalls()) - asked; n != 1 {
		t.Fatalf("%d getaddressinfo calls on the watch-only wallet, want 1 (the built address only)", n)
	}
	for _, k := range x.escrowPubkeys(o1) {
		if slices.Contains(lostKeys, k) {
			t.Fatalf("the new escrow shares key %s with the lost one", k)
		}
	}
	line := "escrow: escrow for order " + shortID(o1) + ": a child key it composed is already held by an escrow or reserved by the orphan sweep (a restored database?); trying that key's next index"
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("log:\n%s", logs.String())
	}
	for _, k := range lostKeys {
		if strings.Contains(logs.String(), k) {
			t.Fatal("a key in the log")
		}
	}

	// A key on record: the market counter set back to the first escrow's index. The second buyer's /0/1 is reserved and
	// the market's m0 and m0+1.. are held or reserved: each is skipped, the checkout succeeds, no key is shared.
	if _, err := x.DB.Exec("SELECT setval('escrow_market_index',$1,false)", m0); err != nil {
		t.Fatal(err)
	}
	o2 := checkout2(regtestAddr("refund-n2"))
	_, keys2 := x.escrowKeys(o2)
	if keys2[0] != "buyer /0/2" || keys2[2] != "vendor /0/4" {
		t.Fatalf("the escrow past a key on record: %v", keys2)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_pubkeys JOIN escrow_reserved_keys USING (pubkey)"); n != 0 {
		t.Fatalf("%d reserved keys handed out", n)
	}

	// Bounded: when every key tried for a party is reserved, the checkout is refused (409, nothing written).
	next := int64(3)
	for i := range escrowReservedTries {
		agExec(x.testEnv, "INSERT INTO escrow_reserved_keys(pubkey,address) VALUES($1,$2)", childHex(x.buyerKey, "/0/"+strconv.FormatInt(next+int64(i), 10)), regtestAddr("bound"))
	}
	o3 := x.draft()
	if w := x.checkout(o3, regtestAddr("refund-n3")); w.Code != 409 || !strings.Contains(w.Body.String(), "already holds one of its keys") {
		t.Fatalf("every key reserved: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(o3)
}

// The margin layer: scripts/restore.sh's own SQL moves each account with an escrow key to its highest child index on
// record + 1 + 50 (an account with no escrow to 51) and the market index 50 on. With the watch-only wallet restored
// from the same backup (it knows no lost escrow, so the sweep finds nothing to reserve), the next escrow starts at the
// floors; the one after continues from the escrow on record. A second restore adds another 50 past where the market
// would issue next. The escrow key page advises keypoolrefill once the next child index passes 900.
func TestEscrowRestoreIndexFloors(t *testing.T) {
	x := newEscrowEnv(t)
	b3, _ := x.user("ekeyonly_"+randomToken()[:6], "buyer")
	x.activeKey(b3, altEscrowKey(t, testBIP48Key, "0ee0ee0e", 5))
	x.mustCheckout(x.order, regtestAddr("refund-f0"))
	m0, _ := x.escrowKeys(x.order)
	agExec(x.testEnv, restoreMarginSQL(t))
	floors := agStrings(t, x.testEnv, "SELECT user_id||' '||next_index FROM escrow_index_floors ORDER BY user_id")
	want := []string{x.buyer + " 52", x.vendor + " 52", b3 + " 51"}
	slices.Sort(want)
	if !slices.Equal(floors, want) {
		t.Fatalf("floors %v, want %v", floors, want)
	}
	if last, called := x.marketCounter(); last != m0+50 || !called {
		t.Fatalf("market counter %d %v after %d, want the next index %d", last, called, m0, m0+51)
	}
	// The wallet restored too: the sweep finds no orphan, and clears the marker.
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_sweep_required','true')")
	x.poll()
	if x.sweepRequired() || agInt(x.testEnv, "SELECT count(*) FROM escrow_reserved_keys") != 0 {
		t.Fatal("the sweep with no orphan")
	}
	o1 := x.draft()
	x.mustCheckout(o1, regtestAddr("refund-f1"))
	if m, keys := x.escrowKeys(o1); !slices.Equal(keys, []string{"buyer /0/52", "market /0/" + strconv.FormatInt(m0+51, 10), "vendor /0/52"}) || m != m0+51 {
		t.Fatalf("the escrow after the restore: %v", keys)
	}
	o2 := x.draft()
	x.mustCheckout(o2, regtestAddr("refund-f2"))
	if _, keys := x.escrowKeys(o2); keys[0] != "buyer /0/53" || keys[2] != "vendor /0/53" {
		t.Fatalf("the next escrow: %v", keys)
	}
	// A second restore: 50 past where the market would issue next (the escrow on record, or a higher floor).
	agExec(x.testEnv, "UPDATE escrow_index_floors SET next_index=80 WHERE user_id=$1", b3)
	agExec(x.testEnv, restoreMarginSQL(t))
	floors = agStrings(t, x.testEnv, "SELECT user_id||' '||next_index FROM escrow_index_floors ORDER BY user_id")
	want = []string{x.buyer + " 104", x.vendor + " 104", b3 + " 130"}
	slices.Sort(want)
	if !slices.Equal(floors, want) {
		t.Fatalf("floors after a second restore %v, want %v", floors, want)
	}
	// The keypoolrefill advice on the escrow key page, once the next child index passes 900.
	advice := "Your Bitcoin Core wallet signs only keys inside its descriptor range"
	for _, c := range []struct {
		floor int64
		shown bool
	}{{900, false}, {901, true}} {
		agExec(x.testEnv, "UPDATE escrow_index_floors SET next_index=$2 WHERE user_id=$1", x.buyer, c.floor)
		page := strings.NewReplacer("<code>", "", "</code>", "").Replace(html.UnescapeString(oneSpace(x.page("/escrow-key", x.buyerSess))))
		if strings.Contains(page, advice) != c.shown {
			t.Fatalf("next index %d: advice shown %v:\n%s", c.floor, !c.shown, page)
		}
		if c.shown && !strings.Contains(page, "Your next escrow uses child key /0/901 of this key. "+advice+" (1000 keys by default), so run keypoolrefill 1901 in that wallet") {
			t.Fatalf("advice text:\n%s", page)
		}
	}
}

// Checkout waits while escrow_sweep_required is set: a fixed 503 before any node call, the order still a draft, and the
// order page says why. A sweep that cannot read an orphan's keys leaves the marker set (and checkout waiting); the next
// pass retries, and a full sweep clears it, gate or not.
func TestEscrowCheckoutWaitsForSweep(t *testing.T) {
	x := newEscrowEnv(t)
	last, called := x.marketCounter()
	lost := x.mustCheckout(x.order, regtestAddr("refund-w0"))
	lostKeys := x.escrowPubkeys(x.order)
	x.lose(last, called, x.order)
	x.setGate(true) // scripts/restore.sh sets the gate too; the sweep runs under it
	o := x.draft()
	page := html.UnescapeString(x.page("/order?id="+o, x.buyerSess))
	if !strings.Contains(page, "Unavailable.") || strings.Contains(page, `action="/orders/escrow"`) {
		t.Fatalf("the escrow offer:\n%s", page)
	}
	x.setGate(false)
	page = html.UnescapeString(x.page("/order?id="+o, x.buyerSess))
	if !strings.Contains(page, "Bitcoin escrow is paused until this market has checked its escrow wallet after restoring its records from a backup") || strings.Contains(page, `action="/orders/escrow"`) {
		t.Fatalf("the escrow offer while the sweep is pending:\n%s", page)
	}
	calls := len(x.core.methods())
	w := x.checkout(o, regtestAddr("refund-w1"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), escrowSweepText) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("checkout while the sweep is pending: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(o)
	if got := x.core.methods()[calls:]; len(got) != 0 {
		t.Fatalf("checkout while the sweep is pending called the node: %v", got)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1", o); n != 0 {
		t.Fatalf("%d order events", n)
	}
	// The wallet cannot be read: the sweep is not complete, nothing is reserved, the marker stays.
	x.setGate(true)
	x.core.mu.Lock()
	x.core.watchInfoErr = -1
	x.core.mu.Unlock()
	if err := x.A.escrowPollOnce(context.Background()); err == nil || watcherCause(err) != "escrow: orphan sweep incomplete: getaddressinfo rpc:-1" {
		t.Fatalf("a failed sweep: %v (%s)", err, watcherCause(err))
	}
	if !x.sweepRequired() || agInt(x.testEnv, "SELECT count(*) FROM escrow_reserved_keys") != 0 {
		t.Fatal("a failed sweep cleared the marker or reserved keys")
	}
	if w := x.checkout(o, regtestAddr("refund-w1")); w.Code != 503 {
		t.Fatalf("checkout after a failed sweep: %d", w.Code)
	}
	x.core.mu.Lock()
	x.core.watchInfoErr = 0
	x.core.mu.Unlock()
	x.poll()
	if x.sweepRequired() || !slices.Equal(agStrings(t, x.testEnv, "SELECT pubkey FROM escrow_reserved_keys WHERE address=$1 ORDER BY pubkey", lost), lostKeys) {
		t.Fatal("the sweep after the wallet answered again")
	}
	x.setGate(false)
	x.mustCheckout(o, regtestAddr("refund-w1"))
	for _, k := range x.escrowPubkeys(o) {
		if slices.Contains(lostKeys, k) {
			t.Fatalf("shared key %s", k)
		}
	}
	// Once reserved, an orphan's keys are not read again on later daily checks.
	asked := len(x.watchInfoCalls())
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_btc_stray_check','2000-01-01T00:00:00Z') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
	x.poll()
	if n := len(x.watchInfoCalls()) - asked; n != 0 {
		t.Fatalf("%d getaddressinfo calls for an orphan already reserved", n)
	}
}

// The real Core 31.1 reply of getaddressinfo on a watch-only wallet for an imported bare-key 2-of-3 (probed on regtest,
// p2e-regtest, 2026-10-03): pubkeys in script order, desc as multi(…) with each key's own fingerprint, scriptPubKey.
var (
	coreWatchPubkeys = []string{
		"020d6954cb5a10e733fa52f54b1401f189a94e63ce18ca0936c97445d995179bc9",
		"039c1397dfd77ba25923cc2061bf33543d964569809a22a65c67f906776cd3f48b",
		"03f1b81ece6804840a9736dd9a484492161bcec92c1416e3c092e89110f4a5e48d",
	}
	coreWatchSPK  = "002031f7bcd5ab2c15ddad87e881fa4ec220f395eff2b8ac2e131434adface70d2a3"
	coreWatchDesc = "wsh(multi(2,[504df747]020d6954cb5a10e733fa52f54b1401f189a94e63ce18ca0936c97445d995179bc9,[ba61cd7f]039c1397dfd77ba25923cc2061bf33543d964569809a22a65c67f906776cd3f48b,[87eaaddf]03f1b81ece6804840a9736dd9a484492161bcec92c1416e3c092e89110f4a5e48d))#csv246nh"
)

// The adapter's reading of getaddressinfo for the orphan sweep (escrowBTC.watchKeys), over HTTP: the three keys from
// pubkeys, or else from desc (multi or sortedmulti, origins stripped), each a compressed key on the curve, and only when
// their 2-of-3 witness script hashes to the reply's scriptPubKey. An address the wallet does not hold (ismine false) and
// one that is not P2WSH (never an escrow) have no keys; any other shape is a bad-reply class, never "no keys".
func TestEscrowWatchKeysReply(t *testing.T) {
	ctx := context.Background()
	unsorted := "wsh(sortedmulti(2," + coreWatchPubkeys[2] + "," + coreWatchPubkeys[0] + "," + coreWatchPubkeys[1] + "))"
	without := func(drop ...string) map[string]any {
		r := map[string]any{"ismine": true, "iswatchonly": false, "solvable": true, "scriptPubKey": coreWatchSPK, "pubkeys": coreWatchPubkeys,
			"desc": coreWatchDesc, "labels": []string{"escrow:o1"}, "script": "multisig", "sigsrequired": 2}
		for _, d := range drop {
			delete(r, d)
		}
		return r
	}
	with := func(r map[string]any, k string, v any) map[string]any { r[k] = v; return r }
	for _, c := range []struct {
		name  string
		reply map[string]any
		keys  []string
		class string
	}{
		{"Core 31.1", without(), coreWatchPubkeys, ""},
		{"desc only", without("pubkeys"), coreWatchPubkeys, ""},
		{"sortedmulti desc, keys in another order", with(without("pubkeys"), "desc", unsorted), coreWatchPubkeys, ""},
		{"not held", map[string]any{"ismine": false, "labels": []string{}, "scriptPubKey": coreWatchSPK}, nil, ""},
		{"labelled, not held", map[string]any{"ismine": false, "labels": []string{"escrow:o2"}}, nil, ""},
		{"held, not P2WSH", map[string]any{"ismine": true, "labels": []string{"x"}, "scriptPubKey": "0014" + strings.Repeat("ab", 20)}, nil, ""},
		{"no ismine", without("ismine"), nil, "getaddressinfo bad-reply"},
		{"no scriptPubKey", without("scriptPubKey"), nil, "getaddressinfo bad-reply"},
		{"neither pubkeys nor desc", without("pubkeys", "desc"), nil, "getaddressinfo bad-reply"},
		{"two pubkeys", with(without(), "pubkeys", coreWatchPubkeys[:2]), nil, "getaddressinfo bad-reply"},
		{"a key repeated", with(without(), "pubkeys", []string{coreWatchPubkeys[0], coreWatchPubkeys[0], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply"},
		{"not hex", with(without(), "pubkeys", []string{"zz", coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply"},
		{"uncompressed", with(without(), "pubkeys", []string{"04" + strings.Repeat("ab", 64), coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply"},
		{"off the curve", with(without(), "pubkeys", []string{"02" + strings.Repeat("00", 32), coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply"},
		{"keys of another address", with(without(), "scriptPubKey", "0020"+strings.Repeat("ab", 32)), nil, "getaddressinfo bad-reply"},
		{"keys out of script order", with(without(), "pubkeys", []string{coreWatchPubkeys[1], coreWatchPubkeys[0], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply"},
		{"desc not a 2-of-3", with(without("pubkeys"), "desc", "wsh(multi(1,"+strings.Join(coreWatchPubkeys, ",")+"))"), nil, "getaddressinfo bad-reply"},
		{"desc malformed", with(without("pubkeys"), "desc", "wsh(multi(2,"+coreWatchPubkeys[0]+"))"), nil, "getaddressinfo bad-reply"},
		{"pubkeys not a list", with(without(), "pubkeys", "02ab"), nil, "getaddressinfo bad-reply"},
	} {
		srv := newPayRPCServer(t, "u", "p", false, func(path, method string, params json.RawMessage) (any, int, string) {
			if path != fakeWatchWallet || method != "getaddressinfo" || string(params) != `["bcrt1qorphan00000000"]` {
				return nil, -32601, "unexpected call"
			}
			return c.reply, 0, ""
		})
		e, err := newEscrowBTC(escrowConfig{on: true, rpcURL: srv.url(""), arbiterWallet: "opsecmkt-arbiter", watchWallet: "opsecmkt-escrow-watch", confirmations: 3})
		if err != nil {
			t.Fatal(err)
		}
		keys, err := e.watchKeys(ctx, "bcrt1qorphan00000000")
		class := ""
		if err != nil {
			class = errorClassOf(err)
		}
		var got []string
		for _, k := range keys {
			got = append(got, hex.EncodeToString(k))
		}
		if !slices.Equal(got, c.keys) || class != c.class {
			t.Errorf("%s: keys %v, error %v", c.name, got, err)
		}
	}
}
