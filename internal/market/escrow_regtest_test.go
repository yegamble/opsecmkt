package market

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
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
		var info struct {
			Chain string `json:"chain"`
		}
		if err := c.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.Chain != "regtest" {
			t.Fatalf("refusing: the %s node URL is not on regtest (or is unreachable)", name)
		}
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
	if e.A.escrow == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	t.Logf("escrow on: arbiter fingerprint pinned (%d hex digits)", len(e.A.escrow.fingerprint))
	if err := e.A.escrow.keypoolRefill(ctx, 1100); err != nil {
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
		if d.Fingerprint == e.A.escrow.fingerprint {
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
	srvSigned, err := e.A.escrow.throwawaySign(ctx, pt.PSBT, pt.signDesc)
	if err != nil || srvSigned.Complete {
		t.Fatalf("throwaway sign: %v %v", srvSigned.Complete, err)
	}
	if _, complete, err := e.A.escrow.finalizePSBT(ctx, srvSigned.PSBT); err != nil || complete {
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
	rawHex, complete, err := e.A.escrow.finalizePSBT(ctx, encodePSBT(full))
	allowed, reject, err2 := e.A.escrow.testMempoolAccept(ctx, rawHex, practiceMaxFeeRate)
	if err != nil || err2 != nil || !complete || allowed || reject != "missing-inputs" {
		t.Fatalf("finalize complete=%v, testmempoolaccept allowed=%v %q: %v %v", complete, allowed, reject, err, err2)
	}
	t.Logf("finalizepsbt complete=%v; testmempoolaccept allowed=%v reject-reason=%q (never broadcastable)", complete, allowed, reject)
	if code, _ := submitPractice(e, sess, good); code != 409 {
		t.Fatalf("second practice: %d", code)
	}
	t.Log("practice after activation -> 409 (never offered again)")
}

func mustStoredKey(t *testing.T, e *testEnv, uid string) *storedEscrowKey {
	t.Helper()
	k, err := loadEscrowKey(context.Background(), e.DB, uid)
	if err != nil || k == nil {
		t.Fatalf("stored key: %v", err)
	}
	return k
}
