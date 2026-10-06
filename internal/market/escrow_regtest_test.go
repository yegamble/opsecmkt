package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestEscrowRegtestPracticeSign is escrow slice P1-a's real-chain leg (scripts/test-escrow-regtest.sh; not run in
// CI): against a Bitcoin Core regtest node it creates the arbiter wallet and a buyer's Core wallet holding a BIP48
// account, registers the buyer's tpub on a real App, fetches the practice PSBT, signs it in the buyer's wallet with
// walletprocesspsbt and pastes it back. Adversarial pastes are refused first. The App reaches Core only as the
// rpcwhitelist user of ESCROW_REGTEST_APP_RPC_URL; wallets are set up with ESCROW_REGTEST_OPERATOR_RPC_URL (full
// rights, standing in for the buyer's own node). Both must report regtest before any wallet call.
func TestEscrowRegtestPracticeSign(t *testing.T) {
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
		if err := op.call(ctx, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	s := randomToken()[:8]
	arbiter, seed, buyer, other, watch := "p1a-arbiter-"+s, "p1a-seed-"+s, "p1a-buyer-"+s, "p1a-other-"+s, "p1a-watch-"+s
	for _, w := range []string{arbiter, seed, other} {
		call("", "createwallet", nil, w)
	}
	call("", "createwallet", nil, watch, true, true)  // the watch-only wallet escrow startup requires (P1-b)
	call("", "createwallet", nil, buyer, false, true) // blank: it will hold only the BIP48 account
	// The buyer's BIP48 account (m/48h/1h/0h/2h), imported as a private descriptor as a Core user would.
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
	var imported []struct {
		Success bool `json:"success"`
	}
	call(buyer, "importdescriptors", &imported, imports)
	var pub struct {
		Descriptors []struct {
			Desc     string `json:"desc"`
			Internal bool   `json:"internal"`
		} `json:"descriptors"`
	}
	call(buyer, "listdescriptors", &pub)
	var key string
	for _, d := range pub.Descriptors {
		if !d.Internal {
			key = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
		}
	}
	parsed, err := parseEscrowKey(key)
	if err != nil || !parsed.BIP48 {
		t.Fatalf("buyer key %q: %v", key, err)
	}
	t.Logf("buyer wallet %s exports %s… (BIP48, origin [%s%s])", buyer, key[:40], parsed.Fingerprint, parsed.PathText)

	// Whitelist: the app user may not export or back up keys, and may refill the arbiter keypool.
	for method, params := range map[string][]any{"listdescriptors": {true}, "dumpwallet": {"/tmp/p1a-dump"}, "backupwallet": {"/tmp/p1a-backup"}} {
		err := classifyEscrowRPC(method, app.call(ctx, "/wallet/"+arbiter, method, params, nil))
		var re *escrowRPCError
		if !errors.As(err, &re) || re.Class != "forbidden" {
			t.Fatalf("app user %s on the arbiter wallet: %v (want forbidden)", method, err)
		}
		t.Logf("whitelist: app user %s -> %s", method, re.Class)
	}

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	t.Logf("escrow on: arbiter fingerprint pinned (%d hex digits)", len(e.A.escrow.Load().fingerprint))
	if err := e.A.escrow.Load().keypoolRefill(ctx, 1100); err != nil {
		t.Fatalf("keypoolrefill as the whitelisted app user: %v", err)
	}
	t.Log("whitelist: app user keypoolrefill 1100 on the arbiter wallet -> ok")

	uid, sess := e.user("regtest_buyer_"+s, "buyer")
	if code, body := registerKey(e, sess, key); code != 303 {
		t.Fatalf("register: %d %s", code, body)
	}
	psbt := practicePSBT(t, e, sess)
	var dec struct {
		Tx struct {
			TxID string `json:"txid"`
			Vin  []struct {
				TxID string `json:"txid"`
				Vout int    `json:"vout"`
			} `json:"vin"`
		} `json:"tx"`
		Inputs []struct {
			Derivs []struct {
				Fingerprint string `json:"master_fingerprint"`
				Path        string `json:"path"`
			} `json:"bip32_derivs"`
		} `json:"inputs"`
	}
	call("", "decodepsbt", &dec, psbt)
	t.Logf("practice PSBT: %d base64 chars, txid %s, spends %s:%d", len(psbt), dec.Tx.TxID, dec.Tx.Vin[0].TxID, dec.Tx.Vin[0].Vout)
	for _, d := range dec.Inputs[0].Derivs {
		t.Logf("practice key derivation: %s %s", d.Fingerprint, d.Path)
		if d.Fingerprint == e.A.escrow.Load().fingerprint {
			t.Fatal("the market's key is in the practice transaction")
		}
	}
	var txout any
	call("", "gettxout", &txout, dec.Tx.Vin[0].TxID, dec.Tx.Vin[0].Vout, true)
	if txout != nil {
		t.Fatal("the practice outpoint exists")
	}
	t.Log("gettxout on the practice outpoint -> null (it does not exist)")

	sign := func(wallet, sighash string) string {
		var out struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call(wallet, "walletprocesspsbt", &out, psbt, true, sighash, true, false)
		if out.Complete {
			t.Fatalf("%s: one signature completed the 2-of-3 practice transaction", wallet)
		}
		return out.PSBT
	}
	refuse := func(name, paste, want string) {
		t.Helper()
		code, body := submitPractice(e, sess, paste)
		if code != 400 || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		t.Logf("refused %-28s -> %d %q", name, code, want)
	}
	refuse("unsigned paste", psbt, "no signature from your registered key")
	refuse("signed by another wallet", sign(other, "ALL"), "no signature from your registered key")
	refuse("buyer SIGHASH_NONE", sign(buyer, "NONE"), "sighash type other than ALL")
	refuse("buyer SIGHASH_SINGLE|ACP", sign(buyer, "SINGLE|ANYONECANPAY"), "sighash type other than ALL")
	good := sign(buyer, "ALL")
	gp, err := decodePSBT(good)
	if err != nil || len(gp.Inputs[0].PartialSigs) != 1 || gp.Inputs[0].PartialSigs[0].SighashByte() != 1 {
		t.Fatalf("buyer signature: %v", err)
	}
	corrupt := append([]byte(nil), gp.Inputs[0].PartialSigs[0].Sig...)
	corrupt[12] ^= 1
	bad, err := decodePSBT(psbt)
	if err != nil {
		t.Fatal(err)
	}
	badRaw, err := bad.withInputField(0, append([]byte{0x02}, gp.Inputs[0].PartialSigs[0].PubKey...), corrupt)
	if err != nil {
		t.Fatal(err)
	}
	refuse("corrupted buyer signature", encodePSBT(badRaw), "did not verify")
	finRaw, err := gp.withInputField(0, []byte{0x08}, []byte{0})
	if err != nil {
		t.Fatal(err)
	}
	refuse("forged final witness", encodePSBT(finRaw), "already finalized")

	// Control: the server's throwaway signature alone never completes the practice transaction.
	pt, err := e.A.practiceFor(ctx, uid, mustStoredKey(t, e, uid))
	if err != nil {
		t.Fatal(err)
	}
	srvSigned, err := e.A.escrow.Load().throwawaySign(ctx, pt.PSBT, pt.signDesc)
	if err != nil || srvSigned.Complete {
		t.Fatalf("throwaway sign: %v %v", srvSigned.Complete, err)
	}
	if _, complete, err := e.A.escrow.Load().finalizePSBT(ctx, srvSigned.PSBT); err != nil || complete {
		t.Fatalf("server signature alone finalized: %v %v", complete, err)
	}
	t.Log("control: throwaway server signature alone -> finalizepsbt complete=false")

	code, body := submitPractice(e, sess, good)
	if code != 303 {
		t.Fatalf("practice: %d %s", code, body)
	}
	var activeAt string
	e.DB.QueryRow("SELECT coalesce(to_char(active_at,'YYYY-MM-DD HH24:MI:SS'),'') FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&activeAt)
	if activeAt == "" {
		t.Fatal("not activated")
	}
	t.Logf("buyer-signed practice PSBT accepted -> 303; active_at %s UTC", activeAt)
	// What Core says about the finalized practice transaction (the check verifyPractice made).
	withSig, _ := decodePSBT(srvSigned.PSBT)
	sig := gp.Inputs[0].PartialSigs[0]
	full, _ := withSig.withInputField(0, append([]byte{0x02}, sig.PubKey...), sig.Sig)
	rawHex, complete, err := e.A.escrow.Load().finalizePSBT(ctx, encodePSBT(full))
	allowed, reject, err2 := e.A.escrow.Load().testMempoolAccept(ctx, rawHex, practiceMaxFeeRate)
	if err != nil || err2 != nil || !complete || allowed || reject != "missing-inputs" {
		t.Fatalf("finalize complete=%v, testmempoolaccept allowed=%v %q: %v %v", complete, allowed, reject, err, err2)
	}
	t.Logf("finalizepsbt complete=%v; testmempoolaccept allowed=%v reject-reason=%q (never broadcastable)", complete, allowed, reject)
	if code, _ := submitPractice(e, sess, good); code != 409 {
		t.Fatalf("second practice: %d", code)
	}
	t.Log("practice after activation -> 409 (never offered again)")
}

// The real-chain legs run on regtest (the default) or, with ESCROW_REGTEST_CHAIN=signet, on a private signet
// (scripts/test-escrow-regtest.sh --private-signet, A-443): two Bitcoin Core nodes started with -signetchallenge=51 on
// a Docker network of their own, a chain anyone can mine with generatetoaddress and nobody else can reach. Its genesis
// block is public signet's, so a node counts as the private signet only when getblockchaininfo also reports
// signet_challenge 51 (public signet's challenge is a 1-of-2 multisig). It is private signet, not public: no leg here
// is public-network evidence.
const (
	privateSignetChallenge   = "51"
	privateSignetLabel       = "private signet, not public"
	privateSignetMinerWallet = "private-signet-miner" // node A's wallet holding mature coins (TestEscrowPrivateSignetPrepare)
	signetMaxTries           = 2000000000             // generatetoaddress maxtries on signet: one int32 budget for the call's blocks
	signetMineTimeout        = 15 * time.Minute       // one generatetoaddress call on signet
)

// realChainNet is the chain the real-chain legs run on: "regtest", or "signet" (the private signet).
func realChainNet() string {
	if os.Getenv("ESCROW_REGTEST_CHAIN") == "signet" {
		return "signet"
	}
	return "regtest"
}

// realChainName names it in refusals and log lines.
func realChainName() string {
	if realChainNet() == "signet" {
		return "the private signet (signet_challenge 51; " + privateSignetLabel + ")"
	}
	return "regtest"
}

// checkRealChain refuses, before any wallet call, a node that is not on the legs' chain: on regtest the chain name, on
// the private signet the chain name and signet_challenge 51 (so public signet is refused).
func checkRealChain(t *testing.T, ctx context.Context, name string, c *rpcClient) {
	t.Helper()
	var info struct {
		Chain     string `json:"chain"`
		Challenge string `json:"signet_challenge"`
	}
	err := c.call(ctx, "", "getblockchaininfo", []any{}, &info)
	if err != nil || info.Chain != realChainNet() || realChainNet() == "signet" && info.Challenge != privateSignetChallenge {
		t.Fatalf("refusing: the %s node URL is not on %s (or is unreachable)", name, realChainName())
	}
}

// Why three legs do not run on the private signet (scripts/test-escrow-regtest.sh lists the same reasons).
const (
	privateSignetSkipReorg = "it fakes a reorganisation with invalidateblock and reconsiderblock, test-only RPCs this harness treats as " +
		"regtest-only: on two connected nodes the invalidated block stays on node A, so what runs would be a split network, not the " +
		"single-node reorganisation the leg is written for"
	privateSignetSkipPrivate = "it needs a node started with -privatebroadcast behind a dead proxy and mines the queued transaction " +
		"with generateblock, which takes no maxtries: Core's fixed 1000000 tries do not find a signet block (\"Failed to make block.\")"
)

// skipOnPrivateSignet skips a leg that cannot run on the private signet, with the reason; the script lists it as not
// run, never as a pass.
func skipOnPrivateSignet(t *testing.T, reason string) {
	t.Helper()
	if realChainNet() == "signet" {
		t.Skip("not run on the " + privateSignetLabel + ": " + reason)
	}
}

// mineBlocks mines n blocks to addr with generatetoaddress on the node c reaches, on chain ("regtest" or "signet"), and
// returns their hashes; it is an error unless exactly n were mined. Regtest needs one call. On signet every block needs
// real proof of work: Core's default maxtries (1000000) returns [] without an error, and maxtries is one budget for
// all of a call's blocks, so each call passes signetMaxTries and asks for the blocks still missing, and each must mine
// at least one block and no more than it asked for: an empty answer is never taken as a block. A signet call may take
// minutes (seconds per block), so it waits up to signetMineTimeout instead of rpcTimeout.
func mineBlocks(ctx context.Context, c *rpcClient, chain string, n int, addr string) ([]string, error) {
	var mined []string
	for len(mined) < n {
		want := n - len(mined)
		params, timeout := []any{want, addr}, rpcTimeout
		if chain == "signet" {
			params, timeout = append(params, signetMaxTries), signetMineTimeout
		}
		var got []string
		if err := c.callWithin(ctx, timeout, "", "generatetoaddress", params, &got); err != nil {
			return mined, err
		}
		if len(got) == 0 || len(got) > want {
			return mined, fmt.Errorf("generatetoaddress %d on %s mined %d blocks", want, chain, len(got))
		}
		mined = append(mined, got...)
		if chain != "signet" && len(mined) != n {
			return mined, fmt.Errorf("generatetoaddress %d on %s mined %d blocks", n, chain, len(mined))
		}
	}
	return mined, nil
}

// mustMine is mineBlocks on the legs' chain, failing the leg on any error.
func mustMine(t *testing.T, ctx context.Context, c *rpcClient, n int, addr string) []string {
	t.Helper()
	mined, err := mineBlocks(ctx, c, realChainNet(), n, addr)
	if err != nil {
		t.Fatalf("mining %d blocks: %v", n, err)
	}
	return mined
}

// matureFunds gives the wallet owning addr spendable coins. On regtest it mines 101 blocks to addr, so the first
// coinbase matures (as every leg did before A-443). On the private signet, where each block takes seconds, node A's
// miner wallet (ESCROW_REGTEST_MINER_RPC_URL, matured once by TestEscrowPrivateSignetPrepare) pays addr 5 BTC, the
// payment is awaited in the mempool of c's node (node B) and one block confirms it.
func matureFunds(t *testing.T, ctx context.Context, c *rpcClient, addr string) {
	t.Helper()
	if realChainNet() != "signet" {
		mustMine(t, ctx, c, 101, addr)
		return
	}
	miner := privateSignetMiner(t, ctx)
	var txid string
	if err := miner.call(ctx, "/wallet/"+privateSignetMinerWallet, "sendtoaddress", []any{addr, json.Number("5")}, &txid); err != nil {
		t.Fatalf("%s: the miner wallet's sendtoaddress: %v", privateSignetLabel, err)
	}
	waitInMempool(t, ctx, c, txid)
	mustMine(t, ctx, c, 1, addr)
	t.Logf("%s: the miner wallet on node A paid 5 BTC (%s) to the leg's wallet on node B; 1 block mined", privateSignetLabel, txid)
}

// privateSignetMiner is the operator client of node A, the private signet's other node.
func privateSignetMiner(t *testing.T, ctx context.Context) *rpcClient {
	t.Helper()
	miner, err := newRPCClient("private signet miner", os.Getenv("ESCROW_REGTEST_MINER_RPC_URL"), "1.0", false)
	if err != nil {
		t.Fatalf("%s: set ESCROW_REGTEST_MINER_RPC_URL (scripts/test-escrow-regtest.sh --private-signet)", privateSignetLabel)
	}
	checkRealChain(t, ctx, "miner", miner)
	return miner
}

// waitInMempool waits up to 90 seconds for txid to reach the mempool of c's node (relayed from the other node).
func waitInMempool(t *testing.T, ctx context.Context, c *rpcClient, txid string) {
	t.Helper()
	for range 90 {
		if c.call(ctx, "", "getmempoolentry", []any{txid}, nil) == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: %s did not reach node B's mempool within 90 seconds", privateSignetLabel, txid)
}

func mustStoredKey(t *testing.T, e *testEnv, uid string) *storedEscrowKey {
	t.Helper()
	k, err := loadEscrowKey(context.Background(), e.DB, uid)
	if err != nil || k == nil {
		t.Fatalf("stored key: %v", err)
	}
	return k
}
