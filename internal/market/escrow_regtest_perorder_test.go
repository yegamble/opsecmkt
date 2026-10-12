package market

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestEscrowRegtestPerOrderPractice is A-455's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): the
// practice signature of a per-order wallet key (shape (b), ES-9) against Bitcoin Core on regtest. A Core wallet's seed
// stands in for the vendor's BIP39 seed: its BIP48 account key (m/48h/1h/0h/2h) is registered on a real App as a
// per-order wallet key. From the practice page alone (the derivation path, the two rooted cosigner Vpubs, the first
// address and the PSBT), the operator builds the per-order practice wallet as a private descriptor from the seed
// (root/48h/1h/0h/2h/2147483647/0/0 with the cosigners at /0/0): Core derives the address the page shows and signs the
// practice PSBT with descriptorprocesspsbt; that signature activates the key. Refused first: the unsigned PSBT, the
// vendor's Core wallet holding the account's shape-(a) range (it cannot sign the per-order key), and the per-order
// wallet's SIGHASH_NONE signature. The App reaches Core only as the rpcwhitelist user; no private key is logged.
func TestEscrowRegtestPerOrderPractice(t *testing.T) {
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
		if err := op.callWithin(ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	type descList struct {
		Descriptors []struct {
			Desc     string `json:"desc"`
			Internal bool   `json:"internal"`
		} `json:"descriptors"`
	}
	withSum := func(d string) string {
		t.Helper()
		var info struct {
			Checksum string `json:"checksum"`
		}
		call("", "getdescriptorinfo", &info, d)
		return d + "#" + info.Checksum
	}
	s := randomToken()[:8]
	arbiter, watch, vendor := "a455-arbiter-"+s, "a455-watch-"+s, "a455-vendor-"+s
	call("", "createwallet", nil, arbiter)
	call("", "createwallet", nil, watch, true, true)
	call("", "createwallet", nil, vendor)
	// The vendor's seed (root key kept in memory, never logged) and its BIP48 account key, imported into the vendor's
	// Core wallet with the shape-(a) receive range a Core user would have.
	var priv descList
	call(vendor, "listdescriptors", &priv, true)
	var root string
	for _, d := range priv.Descriptors {
		if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
			root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
		}
	}
	if root == "" {
		t.Fatal("vendor wallet: no wpkh root key")
	}
	var imported []struct {
		Success bool `json:"success"`
	}
	call(vendor, "importdescriptors", &imported, []map[string]any{{"desc": withSum("wpkh(" + root + "/48h/1h/0h/2h/0/*)"), "active": false, "range": []int{0, 1000}, "timestamp": "now"}})
	if len(imported) != 1 || !imported[0].Success {
		t.Fatalf("importdescriptors: %+v", imported)
	}
	var pub descList
	call(vendor, "listdescriptors", &pub)
	var key string
	for _, d := range pub.Descriptors {
		if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
			key = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
		}
	}
	parsed, err := parseEscrowKey(key, keyNetTest)
	if err != nil || !parsed.BIP48 {
		t.Fatalf("vendor key %q: %v", key, err)
	}
	t.Logf("vendor seed wallet %s: account [%s%s]%s… (BIP48)", vendor, parsed.Fingerprint, parsed.PathText, parsed.XPub[:24])

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the regtest node")
	}
	uid, sess := e.user("regtest_po_vendor_"+s, "vendor")
	if code, body := registerKey(e, sess, key, "layout", "per_order"); code != 303 {
		t.Fatalf("register: %d %s", code, body)
	}
	psbt, cosigners, addr, path := perOrderPractice(t, e, sess)
	t.Logf("practice page: path %s, cosigners …%s and …%s (rooted Vpubs), first address %s, PSBT %d base64 chars", path, cosigners[0][len(cosigners[0])-16:], cosigners[1][len(cosigners[1])-16:], addr, len(psbt))
	if path != "["+parsed.Fingerprint+"/48h/1h/0h/2h/2147483647]" {
		t.Fatalf("practice path %q", path)
	}
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
	owner := 0
	for _, d := range dec.Inputs[0].Derivs {
		t.Logf("practice key derivation (Core's decodepsbt): %s %s", d.Fingerprint, d.Path)
		if d.Fingerprint == e.A.escrow.Load().fingerprint {
			t.Fatal("the market's key is in the practice transaction")
		}
		if d.Fingerprint == parsed.Fingerprint {
			if d.Path != "m/48h/1h/0h/2h/2147483647/0/0" {
				t.Fatalf("owner derivation %s", d.Path)
			}
			owner++
		} else if d.Path != "m/0/0" {
			t.Fatalf("cosigner derivation %s %s (want the rooted key's own fingerprint at m/0/0)", d.Fingerprint, d.Path)
		}
	}
	if owner != 1 || len(dec.Inputs[0].Derivs) != 3 {
		t.Fatalf("derivations: %d owner of %d", owner, len(dec.Inputs[0].Derivs))
	}
	var txout any
	call("", "gettxout", &txout, dec.Tx.Vin[0].TxID, dec.Tx.Vin[0].Vout, true)
	if txout != nil {
		t.Fatal("the practice outpoint exists")
	}
	t.Logf("practice txid %s spends %s:%d; gettxout -> null (it does not exist)", dec.Tx.TxID, dec.Tx.Vin[0].TxID, dec.Tx.Vin[0].Vout)

	// The per-order practice wallet, built from the seed and what the page shows (Vpubs as Core's tpub form).
	var tpubs []string
	for _, c := range cosigners {
		k, err := parseBIP32Pub(c, versionVpub)
		if err != nil || k.Depth != 0 || k.ParentFP != [4]byte{} || k.Child != 0 {
			t.Fatalf("cosigner not rooted: %+v %v", k, err)
		}
		tpubs = append(tpubs, k.Serialize(versionTpub))
	}
	wallet := "wsh(sortedmulti(2," + root + "/48h/1h/0h/2h/2147483647/0/0," + tpubs[0] + "/0/0," + tpubs[1] + "/0/0))"
	var addrs []string
	call("", "deriveaddresses", &addrs, withSum(wallet))
	if len(addrs) != 1 || addrs[0] != addr {
		t.Fatalf("the per-order wallet built from the seed derives %q, the page shows %s", addrs, addr)
	}
	t.Logf("operator: the per-order practice wallet from the seed (root/48h/1h/0h/2h/2147483647/0/0 + both rooted cosigners /0/0) derives %s, the address shown", addrs[0])

	sign := func(sighash string) string {
		t.Helper()
		var out struct {
			PSBT     string `json:"psbt"`
			Complete bool   `json:"complete"`
		}
		call("", "descriptorprocesspsbt", &out, psbt, []string{withSum(wallet)}, sighash, true, false)
		if out.Complete {
			t.Fatal("one signature completed the 2-of-3 practice transaction")
		}
		return out.PSBT
	}
	refuse := func(name, paste, want string) {
		t.Helper()
		code, body := submitPractice(e, sess, paste)
		if code != 400 || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		t.Logf("refused %-36s -> %d %q", name, code, want)
	}
	refuse("unsigned paste", psbt, "no signature from your registered key")
	var coreSigned struct {
		PSBT string `json:"psbt"`
	}
	call(vendor, "walletprocesspsbt", &coreSigned, psbt, true, "ALL", true, false)
	refuse("vendor's Core wallet (shape-(a) range)", coreSigned.PSBT, "no signature from your registered key")
	refuse("per-order wallet SIGHASH_NONE", sign("NONE"), "sighash type other than ALL")
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 0 {
		t.Fatal("activated by a refused paste")
	}

	good := sign("ALL")
	gp, err := decodePSBT(good)
	if err != nil || len(gp.Inputs[0].PartialSigs) != 1 || gp.Inputs[0].PartialSigs[0].SighashByte() != 1 {
		t.Fatalf("per-order wallet signature: %v", err)
	}
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
	t.Log("control: throwaway server signature (its child at 2147483647/0/0, signed by Core) alone -> finalizepsbt complete=false")
	if code, body := submitPractice(e, sess, good); code != 303 {
		t.Fatalf("practice: %d %s", code, body)
	}
	var activeAt, layout string
	e.DB.QueryRow("SELECT coalesce(to_char(active_at,'YYYY-MM-DD HH24:MI:SS'),''),layout FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&activeAt, &layout)
	if activeAt == "" || layout != "per_order" {
		t.Fatalf("not activated: %q %s", activeAt, layout)
	}
	t.Logf("per-order wallet's practice signature accepted -> 303; active_at %s UTC, layout %s", activeAt, layout)
	withSig, _ := decodePSBT(srvSigned.PSBT)
	sig := gp.Inputs[0].PartialSigs[0]
	full, _ := withSig.withInputField(0, append([]byte{0x02}, sig.PubKey...), sig.Sig)
	rawHex, complete, err := e.A.escrow.Load().finalizePSBT(ctx, encodePSBT(full))
	allowed, reject, err2 := e.A.escrow.Load().testMempoolAccept(ctx, rawHex, practiceMaxFeeRate)
	if err != nil || err2 != nil || !complete || allowed || reject != "missing-inputs" {
		t.Fatalf("finalize complete=%v, testmempoolaccept allowed=%v %q: %v %v", complete, allowed, reject, err, err2)
	}
	t.Logf("finalizepsbt complete=%v (Core verified both signatures); testmempoolaccept allowed=%v reject-reason=%q (never broadcastable)", complete, allowed, reject)
	if code, _ := submitPractice(e, sess, good); code != 409 {
		t.Fatalf("second practice: %d", code)
	}
	t.Log("practice after activation -> 409 (never offered again)")
}
