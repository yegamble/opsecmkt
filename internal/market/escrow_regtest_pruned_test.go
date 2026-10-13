package market

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEscrowRegtestPrunedRepair is the real-chain leg of A-527 (scripts/test-escrow-regtest.sh; in CI by the
// escrow-regtest job): the watch-only wallet repair on a pruned Bitcoin Core node. It needs a third regtest node started
// with -prune=550 -fastprune and the same users and whitelists (ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL,
// ESCROW_REGTEST_PRUNED_APP_RPC_URL). Block times are set with setmocktime. A per-order escrow is created through the
// checkout form (its creation set 10 days back in SQL) and paid its price; the node then mines 400 blocks a day later and
// 300 more 6 hours after those, and pruneblockchain prunes past the deposit's block. A newer escrow, created (in SQL)
// after the block at pruneheight, is paid too. First, Core's own behaviour, as the operator into a scratch watch-only wallet: importdescriptors of the
// old escrow's descriptor from its creation time answers success false with error code -1 ("Rescan failed …"), yet the
// wallet holds the address with its label and lists nothing it received; each is logged. Then the restore: the restore
// gate and the sweep marker are set as scripts/restore.sh sets them and the App restarts onto an empty watch-only
// wallet (one restored from a backup older than both escrows). One watcher pass imports only the newer escrow (its
// deposit counts and its order completes), lists the old escrow as one that cannot be rescanned on this pruned node, and
// leaves its order awaiting payment (not expired), the gate and the marker set; /admin names the order. A second pass
// imports nothing. The App reaches Core only as the rpcwhitelist user.
func TestEscrowRegtestPrunedRepair(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_PRUNED_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_PRUNED_APP_RPC_URL and ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest -prune=550 -fastprune")
	}
	ctx := context.Background()
	call, op := regtestOperator(t, opURL, map[string]string{"app": appURL})
	var chain struct {
		Pruned      bool  `json:"pruned"`
		PruneHeight int64 `json:"pruneheight"`
		Blocks      int64 `json:"blocks"`
		Time        int64 `json:"time"`
	}
	call("", "getblockchaininfo", &chain)
	if !chain.Pruned {
		t.Fatal("the node at ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL is not pruned: start it with -prune=550 -fastprune")
	}
	// The node's clock runs from after its tip: a rerun on the same node finds the tip a day or more ahead.
	clock := max(time.Now().Unix(), chain.Time) + 60
	call("", "setmocktime", nil, clock)
	t.Cleanup(func() { op.call(context.Background(), "", "setmocktime", []any{0}, nil) })
	withSum := func(d string) string {
		t.Helper()
		var info struct {
			Checksum string `json:"checksum"`
		}
		call("", "getdescriptorinfo", &info, d)
		return d + "#" + info.Checksum
	}
	s := randomToken()[:8]
	arbiter, watch, restored, scratch, funder := "pr-arbiter-"+s, "pr-watch-"+s, "pr-restored-"+s, "pr-scratch-"+s, "pr-funder-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, funder)
	for _, w := range []string{watch, restored, scratch} {
		call("", "createwallet", nil, w, true, true)
	}
	// A party's seed wallet's BIP48 account key with origin (as TestEscrowRegtestPerOrderRepair makes it).
	party := func(name string) string {
		t.Helper()
		call("", "createwallet", nil, name)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &priv, true)
		root := ""
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		var imported []struct {
			Success bool `json:"success"`
		}
		call(name, "importdescriptors", &imported, []map[string]any{{"desc": withSum("wpkh(" + root + "/48h/1h/0h/2h/0/*)"), "active": false, "range": []int{0, 10}, "timestamp": "now"}})
		if root == "" || len(imported) != 1 || !imported[0].Success {
			t.Fatalf("%s: no BIP48 account key (%v)", name, imported)
		}
		var pub struct {
			Descriptors []struct {
				Desc string `json:"desc"`
			} `json:"descriptors"`
		}
		call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				return strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		t.Fatalf("%s: no public BIP48 account key", name)
		return ""
	}
	buyerKey, vendorKey := party("pr-buyer-"+s), party("pr-vendor-"+s)

	f := newFakeSigner(t, signerKey(t, 7))
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", f.path)
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil || e.A.escrowSignerState() != signerReady {
		t.Fatalf("escrow did not start against the pruned node (signer %q)", e.A.escrowSignerState())
	}
	buyer, buyerSess := e.user("pr_b_"+s, "buyer")
	vendor, _ := e.user("pr_v_"+s, "vendor")
	_, adminSess := e.user("pr_a_"+s, "admin")
	for uid, key := range map[string]string{buyer: buyerKey, vendor: vendorKey} {
		k := mustParseKey(t, key)
		sealed, err := e.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			t.Fatal(err)
		}
		agExec(e, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout) VALUES($1,$2,$3,$4,$5,now(),'per_order')`,
			uid, sealed, e.A.escrowKeyHMAC(k), k.Fingerprint, k.PathText)
	}
	var mineTo string
	call(funder, "getnewaddress", &mineTo)
	matureFunds(t, ctx, op, mineTo)
	checkout := func(label string) (order, addr string) {
		t.Helper()
		product := e.product(vendor, "digital")
		agExec(e, "UPDATE products SET delivery_content='licence key PRUNED' WHERE id=$1", product)
		order = e.order(buyer, product, "BTC", stateDraft)
		var refund string
		call(funder, "getnewaddress", &refund)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
			t.Fatalf("%s checkout: %d %s", label, w.Code, w.Body.String())
		}
		return order, agStrings(t, e, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
	}
	pay := func(order, addr string) {
		t.Helper()
		price := agInt64(t, e, "SELECT amount FROM orders WHERE id=$1", order)
		var txid string
		call(funder, "sendtoaddress", &txid, addr, json.Number(strconv.FormatFloat(float64(price)/1e8, 'f', 8, 64)))
		mustMine(t, ctx, op, 3, mineTo)
		t.Logf("paid %d sat to the escrow of order %s: %s", price, shortID(order), txid)
	}
	state := func(order string) string { return agStrings(t, e, "SELECT state FROM orders WHERE id=$1", order)[0] }

	old, oldAddr := checkout("old")
	agExec(e, "UPDATE escrows SET created=now()-interval '10 days' WHERE order_id=$1", old)
	pay(old, oldAddr)
	var paidAt int64
	call("", "getblockcount", &paidAt)

	// A day later 400 blocks, and 6 hours after those 300 more; then everything below those 300 is pruned. Core rescans
	// even a "now" import (checkout's) from 2 hours before the tip's median time, so the last 2 hours of blocks must not
	// be pruned (on a real chain Core keeps at least 288 blocks, about two days).
	later := clock + 86400
	call("", "setmocktime", nil, later)
	mustMine(t, ctx, op, 400, mineTo)
	call("", "setmocktime", nil, later+6*3600)
	mustMine(t, ctx, op, 300, mineTo)
	var tip, prunedTo int64
	call("", "getblockcount", &tip)
	call("", "pruneblockchain", &prunedTo, tip-300)
	call("", "getblockchaininfo", &chain)
	var hash string
	call("", "getblockhash", &hash, chain.PruneHeight)
	var header struct {
		Time int64 `json:"time"`
	}
	call("", "getblockheader", &header, hash, true)
	t.Logf("pruned: pruneblockchain %d answered %d; pruneheight %d (block time %d), the old escrow's deposit at height %d", tip-300, prunedTo, chain.PruneHeight, header.Time, paidAt)
	if !chain.Pruned || chain.PruneHeight <= paidAt {
		t.Fatalf("pruneheight %d is not past the deposit's block %d", chain.PruneHeight, paidAt)
	}
	newer, newerAddr := checkout("newer")
	agExec(e, "UPDATE escrows SET created=to_timestamp($2) WHERE order_id=$1", newer, later+7*3600)
	call("", "setmocktime", nil, later+8*3600)
	pay(newer, newerAddr)

	// Core's own behaviour below pruneheight (the UNVERIFIED point of A-527, settled here): the operator imports the old
	// escrow's descriptor into a scratch watch-only wallet from its creation time.
	keys := agStrings(t, e, "SELECT pubkey FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 ORDER BY party='market',party='vendor'", old)
	created := agInt64(t, e, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", old)
	var res []struct {
		Success bool `json:"success"`
		Error   *struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	call(scratch, "importdescriptors", &res, []map[string]any{{"desc": withSum("wsh(sortedmulti(2," + strings.Join(keys, ",") + "))"), "timestamp": created, "label": "escrow:" + old, "active": false}})
	var held struct {
		IsMine bool     `json:"ismine"`
		Labels []string `json:"labels"`
	}
	call(scratch, "getaddressinfo", &held, oldAddr)
	var listed []struct {
		Address string   `json:"address"`
		TxIDs   []string `json:"txids"`
	}
	call(scratch, "listreceivedbyaddress", &listed, 0, true, true)
	seen := -1
	for _, l := range listed {
		if l.Address == oldAddr {
			seen = len(l.TxIDs)
		}
	}
	if len(res) == 1 && res[0].Error != nil {
		t.Logf("Core importdescriptors below pruneheight: success %v, error code %d: %s", res[0].Success, res[0].Error.Code, res[0].Error.Message)
	}
	t.Logf("Core after that import: getaddressinfo ismine %v labels %v; listreceivedbyaddress lists %d transaction(s) for it", held.IsMine, held.Labels, seen)
	if len(res) != 1 || res[0].Success || res[0].Error == nil || res[0].Error.Code != -1 || !held.IsMine || !slices.Contains(held.Labels, "escrow:"+old) || seen != 0 {
		t.Errorf("Core's import below pruneheight is not what A-527 records (failed, code -1, descriptor and label kept, nothing listed): %+v", res)
	}

	// The restore: the gate and the sweep marker as scripts/restore.sh sets them, and an empty watch-only wallet.
	agExec(e, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true'),('escrow_sweep_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	t.Setenv("ESCROW_BTC_WATCH_WALLET", restored)
	e.restart()
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow off after the restart onto the restored watch-only wallet")
	}
	for pass := 1; pass <= 2; pass++ {
		if err := e.A.escrowPollOnce(ctx); err != nil {
			t.Fatalf("watcher pass %d after the restore: %v", pass, err)
		}
	}
	rows := agStrings(t, e, "SELECT es.order_id||' pruned='||u.pruned||' import_failed='||u.import_failed FROM escrow_watch_unrepaired u JOIN escrows es ON es.id=u.escrow_id")
	var oldHeld struct {
		IsMine bool `json:"ismine"`
	}
	call(restored, "getaddressinfo", &oldHeld, oldAddr)
	gate := agStrings(t, e, "SELECT key FROM settings WHERE key IN ('payments_recovery_required','escrow_sweep_required') AND value='true' ORDER BY key")
	t.Logf("after two passes: old order %s, newer order %s; listed %v; old escrow held by the restored wallet %v; still set %v", state(old), state(newer), rows, oldHeld.IsMine, gate)
	if state(newer) != stateCompleted || state(old) != stateAwaitingPayment || !slices.Equal(rows, []string{old + " pruned=true import_failed=false"}) || oldHeld.IsMine ||
		!slices.Equal(gate, []string{"escrow_sweep_required", "payments_recovery_required"}) || e.A.escrowSiblingsChecked.Load() {
		t.Fatal("the repair on a pruned node did not keep the old escrow listed and the gate set while repairing the newer one")
	}
	page := html.UnescapeString(e.do("GET", "/admin", adminSess, nil).Body.String())
	for _, want := range []string{shortID(old), "cannot be rescanned on this pruned node"} {
		if !strings.Contains(page, want) {
			t.Fatalf("/admin lacks %q", want)
		}
	}
	t.Logf("/admin names order %s: its escrow cannot be rescanned on this pruned node", shortID(old))
}
