package market

import (
	"context"
	"encoding/json"
	"net/url"

	"strconv"
	"strings"
	"testing"
	"time"
)

const publicSignetChallenge = "512103ad5e0edad18cb1f0fc0d28a3d4f1f3e445640337489abb10404f2d1e086be430210359ef5021964fe22d6f8e05b2463c9540ce96883fe3b278760f048f5189f2e6c452ae"

// publicChainRefusal says why the smoke refuses a node reporting chain (and, on signet, challenge), or "" for the two
// public test networks escrow runs on: testnet4, and signet with public signet's challenge. The same table is in
// scripts/test-escrow-public.sh (chain_refusal); tests/test_escrow_public_script.py keeps the two equal.
func publicChainRefusal(chain, challenge string) string {
	switch chain {
	case "testnet4":
		return ""
	case "signet":
		if challenge == publicSignetChallenge {
			return ""
		}
		return "a private signet (its signet_challenge is not public signet's)"
	case "regtest":
		return "regtest is a local chain, not a public network"
	case "main":
		return "mainnet is never used for this smoke"
	case "test":
		return "testnet3 is not an escrow chain"
	}
	return "not a chain escrow runs on"
}

type escrowSmokeChain struct {
	op            *rpcClient
	funder        string // the wallet that pays the escrows and receives every settlement
	confirmations int
	feeRate       int                                 // sat/vB of the funding sends and of every settlement
	confirm       func(t *testing.T, txids ...string) // returns once each txid has confirmations
	passes        time.Duration                       // how long a stage may run watcher passes before it fails
	cleanup       func(wallets []string)              // after the smoke: what to do with the wallets it made
}

// runEscrowSmoke runs the three legs against an App started on the node c.op reaches as the operator, with the App
// reaching Core only as the rpcwhitelist user of appURL. It makes its own arbiter, watch-only and party wallets
// (named escrow-smoke-<random>-…), so it never touches the market's own. Each leg ends with a line
// "escrow-public: leg <name> passed" and each broadcast transaction is logged as "escrow-public: txid <leg> <role>
// <txid>", the lines scripts/test-escrow-public.sh reads into the manifest.
func runEscrowSmoke(t *testing.T, appURL string, c escrowSmokeChain) {
	ctx := context.Background()
	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := c.op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	s := "escrow-smoke-" + randomToken()[:8]
	arbiter, watch, buyerWallet, vendorWallet := s+"-arbiter", s+"-watch", s+"-buyer", s+"-vendor"
	made := []string{arbiter, watch}
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	bip48 := func(name string) string {
		t.Helper()
		seed := name + "-seed"
		made = append(made, seed, name)
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
	buyerKey, vendorKey := bip48(buyerWallet), bip48(vendorWallet)
	if c.cleanup != nil {
		t.Cleanup(func() { c.cleanup(made) })
	}
	newAddr := func() string {
		t.Helper()
		var a string
		call(c.funder, "getnewaddress", &a)
		return a
	}
	confs := strconv.Itoa(c.confirmations)
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("ESCROW_BTC_FEE_RATE", strconv.Itoa(c.feeRate))
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", confs)
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	t.Setenv("ESCROW_XMR", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the node (the escrow user's whitelist, the arbiter or watch-only wallet, or the node not synced?)")
	}
	t.Logf("escrow-public: escrow on; arbiter wallet %s, watch-only wallet %s, %s confirmation(s), fee rate %d sat/vB", arbiter, watch, confs, c.feeRate)
	// pass runs watcher passes until done reports true, for at most c.passes (a public node's sync guard can skip a
	// pass: a tip older than BITCOIN_MAX_TIP_AGE, or no peers for a moment).
	pass := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(c.passes)
		for {
			if err := e.A.escrowPollOnce(ctx); err != nil {
				t.Logf("escrow-public: watcher pass (%s): %v", what, err)
			}
			if done() {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not reached within %s of watcher passes", what, c.passes)
			}
			time.Sleep(10 * time.Second)
		}
	}

	// Leg 1: practice signing. Each party registers its BIP48 key and activates it by signing the practice
	// transaction in its own Core wallet; the practice transaction spends a made-up output and is never broadcast.
	activate := func(handle, role, key, wallet string) (string, string) {
		t.Helper()
		uid, sess := e.user(handle+"_"+s[13:], role)
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
		if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 1 {
			t.Fatalf("%s: key not active after practice signing", handle)
		}
		return uid, sess
	}
	buyer, buyerSess := activate("smoke_buyer", "buyer", buyerKey, buyerWallet)
	vendor, vendorSess := activate("smoke_vendor", "vendor", vendorKey, vendorWallet)
	t.Log("escrow-public: leg practice-sign passed (buyer and vendor keys activated by practice signatures from their own Core wallets; nothing broadcast)")

	// Legs 2 and 3 are funded together, so the owner's run waits for confirmations twice, not four times.
	checkout := func(refund string) (order, addr string) {
		t.Helper()
		product := e.product(vendor, "digital")
		if _, err := e.DB.Exec("UPDATE products SET delivery_content='public smoke licence' WHERE id=$1", product); err != nil {
			t.Fatal(err)
		}
		order = e.order(buyer, product, "BTC", stateDraft)
		if w := e.do("POST", "/orders/escrow", buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
			t.Fatalf("escrow checkout: %d %s", w.Code, w.Body.String())
		}
		if err := e.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
			t.Fatal(err)
		}
		return order, addr
	}
	send := func(addr, btc string) string {
		t.Helper()
		var txid string
		// sendtoaddress address amount comment comment_to subtractfeefromamount replaceable conf_target estimate_mode
		// avoid_reuse fee_rate: an explicit rate, since a public node may have no fee estimate yet.
		call(c.funder, "sendtoaddress", &txid, addr, json.Number(btc), "", "", false, true, nil, "unset", nil, c.feeRate)
		return txid
	}
	claimOrder, claimAddr := checkout(newAddr())
	refundTo := newAddr()
	refundOrder, refundAddr := checkout(refundTo)
	claimFund := send(claimAddr, "0.001")    // the listing's price: the order completes on funding
	refundFund := send(refundAddr, "0.0004") // short of the price: the buyer cancels and is refunded
	t.Logf("escrow-public: txid claim funding %s", claimFund)
	t.Logf("escrow-public: txid refund funding %s", refundFund)
	c.confirm(t, claimFund, refundFund)
	orderState := func(order string) string {
		var st string
		e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&st)
		return st
	}
	pass("the claim escrow's order completed on funding and the refund escrow's deposit confirmed", func() bool {
		return orderState(claimOrder) == stateCompleted &&
			agInt(e, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND confirmations>=$3", refundOrder, refundFund, c.confirmations) == 1
	})
	if st := orderState(refundOrder); st != stateAwaitingPayment {
		t.Fatalf("the underpaid order is %s", st)
	}

	settlementOf := func(order, kind string) (id int64, state, txid string) {
		t.Helper()
		e.DB.QueryRow(`SELECT id,state,COALESCE(txid,'') FROM settlements WHERE order_id=$1 AND kind=$2 AND state<>'superseded' ORDER BY id DESC LIMIT 1`,
			order, kind).Scan(&id, &state, &txid)
		return
	}
	partyCopy := func(order, sess string, id int64) string {
		t.Helper()
		w := e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
		if w.Code != 200 {
			t.Fatalf("copy: %d", w.Code)
		}
		return strings.TrimSpace(w.Body.String())
	}
	sign := func(order, sess, wallet string, id int64) {
		t.Helper()
		var signed struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call(wallet, "walletprocesspsbt", &signed, partyCopy(order, sess, id), true, "ALL", true, false)
		if signed.Complete {
			t.Fatal("one party's signature completed the escrow spend")
		}
		w := e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {signed.PSBT}})
		if w.Code != 303 {
			t.Fatalf("paste: %d %s", w.Code, w.Body.String())
		}
	}
	claimTo := newAddr()
	if w := e.do("POST", "/escrow/claim", vendorSess, url.Values{"order_id": {claimOrder}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if w := e.do("POST", "/orders/cancel", buyerSess, url.Values{"order_id": {refundOrder}, "from": {stateAwaitingPayment}, "amount_seen": {"40000"}, "payout_confirmed": {"confirmed"}}); w.Code != 303 {
		t.Fatalf("buyer cancel: %d %s", w.Code, w.Body.String())
	}
	pass("the claim and the refund built", func() bool {
		_, a, _ := settlementOf(claimOrder, "release")
		_, b, _ := settlementOf(refundOrder, "refund")
		return a == "awaiting_signature" && b == "awaiting_signature"
	})
	claimID, _, claimTx := settlementOf(claimOrder, "release")
	refundID, _, refundTx := settlementOf(refundOrder, "refund")
	sign(claimOrder, vendorSess, vendorWallet, claimID)
	sign(refundOrder, buyerSess, buyerWallet, refundID)
	pass("the claim and the refund co-signed and broadcast", func() bool {
		_, a, _ := settlementOf(claimOrder, "release")
		_, b, _ := settlementOf(refundOrder, "refund")
		return a == "broadcast" && b == "broadcast"
	})
	t.Logf("escrow-public: txid claim settlement %s", claimTx)
	t.Logf("escrow-public: txid refund settlement %s", refundTx)
	c.confirm(t, claimTx, refundTx)
	pass("the claim and the refund confirmed", func() bool {
		_, a, _ := settlementOf(claimOrder, "release")
		_, b, _ := settlementOf(refundOrder, "refund")
		return a == "confirmed" && b == "confirmed"
	})
	// Each planned output is an unspent output of its transaction on the chain, with the planned value and address,
	// and the claim pays the vendor's address, the refund the address pinned at checkout.
	onChain := func(leg string, id int64, txid, payee string) {
		t.Helper()
		paid := false
		for _, r := range agStrings(t, e, "SELECT n||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id) {
			f := strings.Fields(r)
			n, _ := strconv.Atoi(f[0])
			var out *struct {
				Confirmations int64       `json:"confirmations"`
				Value         json.Number `json:"value"`
				ScriptPubKey  struct {
					Address string `json:"address"`
				} `json:"scriptPubKey"`
			}
			call("", "gettxout", &out, txid, n, false)
			if out == nil {
				t.Fatalf("%s: output %d of %s is not an unspent output on the chain", leg, n, txid)
			}
			sats, _ := parseAmount(string(out.Value), 8)
			if out.ScriptPubKey.Address != f[1] || strconv.FormatInt(sats, 10) != f[2] || out.Confirmations < int64(c.confirmations) {
				t.Fatalf("%s: output %d of %s on chain: %+v, planned %s", leg, n, txid, out, r)
			}
			paid = paid || f[1] == payee
		}
		if !paid {
			t.Fatalf("%s: no output of %s pays %s", leg, txid, payee)
		}
	}
	onChain("claim", claimID, claimTx, claimTo)
	t.Logf("escrow-public: leg claim passed (funded at the price, completed on funding, the vendor's claim signed in its own wallet, co-signed, broadcast and confirmed with %s confirmation(s); outputs on chain = planned)", confs)
	onChain("refund", refundID, refundTx, refundTo)
	t.Logf("escrow-public: leg refund passed (underpaid, cancelled by the buyer, the refund signed in the buyer's wallet, co-signed, broadcast and confirmed with %s confirmation(s), paying the refund address pinned at checkout)", confs)
	if agInt(e, "SELECT count(*) FROM payouts") != 0 {
		t.Fatal("a payouts row for an escrow order")
	}
}
