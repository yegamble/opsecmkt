package market

import (
	"context"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestReserve is escrow slice P2-e's real-chain leg (A-366, rule P2-R10; scripts/test-escrow-regtest.sh,
// not run in CI), against a Bitcoin Core regtest node with the App reaching Core only as the rpcwhitelist user:
//
//   - database-only restore: escrows are created, the backup point is taken, three more escrows (two buyers, one
//     vendor) are created and lost by a restore simulated in SQL (their rows removed, the market counter set back, the
//     restore gate and escrow_sweep_required set as scripts/restore.sh sets them; the margin is left out here, so the
//     exact layer alone carries the result). Checkout answers 503 until the orphan sweep has run; the sweep reserves
//     exactly the lost escrows' nine keys, read with getaddressinfo from the real watch-only wallet; the next escrow of
//     each buyer shares no public key with any lost escrow;
//   - the watch-only wallet restored too: at a second backup point the watch-only wallet is backed up (backupwallet,
//     operator), two more escrows are created and lost, and the wallet is restored from that backup (restorewallet), so
//     it does not know them either. scripts/restore.sh's own margin SQL runs; after the sweep the next escrow's party
//     indices are at or above their floors, its market index at least 50 past the backup point, and it shares no key
//     with any lost escrow.
func TestEscrowRegtestReserve(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL and ESCROW_REGTEST_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*rpcClient{"operator": op, "app": app} {
		checkRealChain(t, ctx, name, c)
	}
	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.call(ctx, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	s := randomToken()[:8]
	arbiter, watch, funder := "p2e-arbiter-"+s, "p2e-watch-"+s, "p2e-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, funder)
	bip48Wallet := func(name string) string {
		t.Helper()
		seed := name + "-seed"
		call("", "createwallet", nil, seed)
		call("", "createwallet", nil, name, false, true)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(seed, "listdescriptors", &priv, true)
		var root string
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		var imports []map[string]any
		for chain, internal := range map[string]bool{"0": false, "1": true} {
			var info struct {
				Checksum string `json:"checksum"`
			}
			d := "wpkh(" + root + "/48h/1h/0h/2h/" + chain + "/*)"
			call("", "getdescriptorinfo", &info, d)
			imports = append(imports, map[string]any{"desc": d + "#" + info.Checksum, "active": true, "internal": internal, "range": []int{0, 1000}, "timestamp": "now"})
		}
		call(name, "importdescriptors", nil, imports)
		var pub struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if !d.Internal {
				return strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		t.Fatalf("no BIP48 account in %s", name)
		return ""
	}
	buyer1Wallet, buyer2Wallet, vendorWallet := "p2e-buyer1-"+s, "p2e-buyer2-"+s, "p2e-vendor-"+s
	buyer1Key, buyer2Key, vendorKey := bip48Wallet(buyer1Wallet), bip48Wallet(buyer2Wallet), bip48Wallet(vendorWallet)
	newAddr := func() string {
		t.Helper()
		var a string
		call(funder, "getnewaddress", &a)
		return a
	}
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("ESCROW_BTC_FEE_RATE", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "3")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	esc := e.A.escrow.Load()
	if esc == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	t.Logf("escrow on: arbiter wallet %s, watch-only wallet %s", arbiter, watch)
	activate := func(handle, role, key, wallet string) (string, string) {
		t.Helper()
		uid, sess := e.user(handle+"_"+s, role)
		if code, body := registerKey(e, sess, key); code != 303 {
			t.Fatalf("%s register: %d %s", handle, code, body)
		}
		var signed struct {
			PSBT string `json:"psbt"`
		}
		call(wallet, "walletprocesspsbt", &signed, practicePSBT(t, e, sess), true, "ALL", true, false)
		if code, body := submitPractice(e, sess, signed.PSBT); code != 303 {
			t.Fatalf("%s practice: %d %s", handle, code, body)
		}
		return uid, sess
	}
	buyer1, buyer1Sess := activate("p2e_buyer1", "buyer", buyer1Key, buyer1Wallet)
	buyer2, buyer2Sess := activate("p2e_buyer2", "buyer", buyer2Key, buyer2Wallet)
	vendor, _ := activate("p2e_vendor", "vendor", vendorKey, vendorWallet)
	poll := func() {
		t.Helper()
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass: %v", err)
		}
	}
	draft := func(buyer string) string {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='regtest licence p2e' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		return e.order(buyer, product, "BTC", stateDraft)
	}
	checkoutCode := func(order, sess string) (int, string) {
		w := e.do("POST", "/orders/escrow", sess, url.Values{"order_id": {order}, "refund_address": {newAddr()}})
		return w.Code, w.Body.String()
	}
	escrowOrder := func(buyer, sess string) (order, addr string) {
		t.Helper()
		order = draft(buyer)
		if code, body := checkoutCode(order, sess); code != 303 {
			t.Fatalf("escrow checkout: %d %s", code, body)
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		return order, addr
	}
	// The cap of three orders awaiting payment: the legs create more, so finished ones are set aside.
	setAside := func(orders ...string) {
		t.Helper()
		for _, o := range orders {
			if _, err := e.DB.Exec("UPDATE orders SET state='cancelled' WHERE id=$1 AND state='awaiting_payment'", o); err != nil {
				t.Fatal(err)
			}
		}
	}
	keysOf := func(orders ...string) []string {
		return agStrings(t, e, "SELECT pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=ANY($1) ORDER BY pubkey", orders)
	}
	indicesOf := func(order string) string {
		t.Helper()
		var b, v string
		var m int64
		if err := e.DB.QueryRow(`SELECT max(derivation_suffix) FILTER (WHERE party='buyer'),max(derivation_suffix) FILTER (WHERE party='vendor'),max(es.market_index)
			FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1`, order).Scan(&b, &v, &m); err != nil {
			t.Fatal(err)
		}
		return "buyer " + b + ", vendor " + v + ", market /0/" + strconv.FormatInt(m, 10)
	}
	childOf := func(order, party string) int64 {
		t.Helper()
		var suffix string
		if err := e.DB.QueryRow("SELECT derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND p.party=$2", order, party).Scan(&suffix); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(suffix, "/0/"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	marketOf := func(order string) int64 {
		var n int64
		e.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", order).Scan(&n)
		return n
	}
	counter := func() (int64, bool) {
		t.Helper()
		var last int64
		var called bool
		if err := e.DB.QueryRow("SELECT last_value,is_called FROM escrow_market_index").Scan(&last, &called); err != nil {
			t.Fatal(err)
		}
		return last, called
	}
	// restore simulates a database restored from the backup point (last, called): the orders' escrow rows are gone and
	// the orders set aside, the market counter is back, and the restore gate and the sweep marker are set as
	// scripts/restore.sh sets them.
	restore := func(last int64, called bool, orders ...string) {
		t.Helper()
		for _, o := range orders {
			forgetEscrow(t, e.DB, o)
			agExec(e, "UPDATE orders SET state='cancelled' WHERE id=$1", o)
		}
		if _, err := e.DB.Exec("SELECT setval('escrow_market_index',$1,$2)", last, called); err != nil {
			t.Fatal(err)
		}
		agExec(e, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true'),('escrow_sweep_required','true') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value")
	}
	clearGate := func() { agExec(e, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'") }
	shared := func(a, b []string) []string {
		var out []string
		for _, k := range a {
			if slices.Contains(b, k) {
				out = append(out, k)
			}
		}
		return out
	}

	// Leg 1: database-only restore.
	e1, _ := escrowOrder(buyer1, buyer1Sess)
	e2, _ := escrowOrder(buyer2, buyer2Sess)
	setAside(e1, e2)
	last, called := counter()
	l1, a1 := escrowOrder(buyer2, buyer2Sess)
	l2, a2 := escrowOrder(buyer1, buyer1Sess)
	l3, a3 := escrowOrder(buyer2, buyer2Sess)
	lost := keysOf(l1, l2, l3)
	if len(lost) != 9 {
		t.Fatalf("the lost escrows hold %d keys", len(lost))
	}
	t.Logf("restore leg: lost escrows %s (%s), %s (%s), %s (%s)", a1, indicesOf(l1), a2, indicesOf(l2), a3, indicesOf(l3))
	restore(last, called, l1, l2, l3)
	clearGate()
	n1 := draft(buyer1)
	if code, body := checkoutCode(n1, buyer1Sess); code != 503 || !strings.Contains(body, "Bitcoin escrow is paused until this market has checked its escrow wallet") {
		t.Fatalf("checkout before the sweep: %d %s", code, body)
	}
	poll()
	if got := agStrings(t, e, "SELECT pubkey FROM escrow_reserved_keys ORDER BY pubkey"); !slices.Equal(got, lost) {
		t.Fatalf("reserved keys %v, want the lost escrows' %v", got, lost)
	}
	if agInt(e, "SELECT count(*) FROM settings WHERE key='escrow_sweep_required'") != 0 {
		t.Fatal("the sweep did not clear escrow_sweep_required")
	}
	if code, body := checkoutCode(n1, buyer1Sess); code != 303 {
		t.Fatalf("checkout after the sweep: %d %s", code, body)
	}
	n2, _ := escrowOrder(buyer2, buyer2Sess)
	if k := shared(keysOf(n1, n2), lost); len(k) != 0 {
		t.Fatalf("escrows after the restore share keys with the lost ones: %v", k)
	}
	t.Logf("restore leg: checkout -> 503 until the sweep; the sweep reserved the 9 lost keys from getaddressinfo on the real watch-only wallet; new escrows %s and %s share no key with the lost escrows", indicesOf(n1), indicesOf(n2))
	setAside(n1, n2)

	// Leg 2: the watch-only wallet restored from the same backup.
	last, called = counter()
	backup := "/tmp/" + watch + ".bak"
	call(watch, "backupwallet", nil, backup)
	f1, fa1 := escrowOrder(buyer1, buyer1Sess)
	f2, fa2 := escrowOrder(buyer2, buyer2Sess)
	forgotten := keysOf(f1, f2)
	t.Logf("restore-with-wallet leg: lost escrows %s (%s), %s (%s)", fa1, indicesOf(f1), fa2, indicesOf(f2))
	restore(last, called, f1, f2)
	restored := watch + "-restored"
	call("", "restorewallet", nil, restored, backup)
	esc.watchName, esc.watchWallet = restored, "/wallet/"+restored // the App now reads the restored wallet
	for _, a := range []string{fa1, fa2} {
		var info struct {
			IsMine bool `json:"ismine"`
		}
		call(restored, "getaddressinfo", &info, a)
		if info.IsMine {
			t.Fatalf("the restored wallet knows %s", a)
		}
	}
	agExec(e, restoreMarginSQL(t))
	floor := func(uid string) int64 {
		return int64(agInt(e, "SELECT next_index FROM escrow_index_floors WHERE user_id=$1", uid))
	}
	clearGate()
	poll()
	if agInt(e, "SELECT count(*) FROM settings WHERE key='escrow_sweep_required'") != 0 || !slices.Equal(agStrings(t, e, "SELECT pubkey FROM escrow_reserved_keys ORDER BY pubkey"), lost) {
		t.Fatal("the sweep of the restored wallet")
	}
	g, _ := escrowOrder(buyer1, buyer1Sess)
	if childOf(g, "buyer") < floor(buyer1) || childOf(g, "vendor") < floor(vendor) || marketOf(g) < last+51 {
		t.Fatalf("the escrow after the restore with the wallet: %s, floors buyer %d vendor %d, market at least %d", indicesOf(g), floor(buyer1), floor(vendor), last+51)
	}
	if k := shared(keysOf(g), append(slices.Clone(forgotten), lost...)); len(k) != 0 {
		t.Fatalf("the escrow after the restore with the wallet shares keys: %v", k)
	}
	t.Logf("restore-with-wallet leg: the restored wallet does not know the lost escrows; floors buyer /0/%d, vendor /0/%d, market from /0/%d; the new escrow %s shares no key with any lost escrow",
		floor(buyer1), floor(vendor), last+51, indicesOf(g))
}
