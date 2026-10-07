package market

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A-456b's real-chain legs (scripts/test-escrow-regtest.sh; not run in CI): per-order (shape (b)) escrows settled end to
// end against Bitcoin Core, the market co-signing through the real escrow signer (cmd/escrow-signer, built from this
// tree, a fresh seed in a temporary directory, its own whitelisted RPC user). Two Core wallets' seeds stand in for the
// parties' BIP39 seeds; their BIP48 account keys are stored as active per-order wallet keys (A-455's registration and
// practice are their own leg). A party's order wallet is played by Core: descriptorprocesspsbt with the party's private
// root along its per-order path for the escrow's script (the other two keys as hex), on the copy the party downloads.

// shapeBChain is the set-up both legs share.
type shapeBChain struct {
	t                     *testing.T
	ctx                   context.Context
	e                     *testEnv
	op                    *rpcClient
	call                  func(wallet, method string, out any, params ...any)
	buyerRoot, vendorRoot string
	buyerSess, vendorSess string
	buyer, vendor         string
	product, funder       string
	mine                  string
	signerLog             string
	seed                  []byte
}

func newShapeBChain(t *testing.T, label string) *shapeBChain {
	appURL, opURL, signerURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL"), os.Getenv("ESCROW_REGTEST_SIGNER_RPC_URL")
	if appURL == "" || opURL == "" || signerURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL, ESCROW_REGTEST_OPERATOR_RPC_URL and ESCROW_REGTEST_SIGNER_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	c := &shapeBChain{t: t, ctx: context.Background()}
	var err error
	if c.op, err = newRPCClient("regtest operator", opURL, "1.0", false); err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, cl := range map[string]*rpcClient{"operator": c.op, "app": app} {
		checkRealChain(t, c.ctx, name, cl)
	}
	c.call = func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := c.op.callWithin(c.ctx, regtestOperatorTimeout, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	s := randomToken()[:8]
	arbiter, watch := label+"-arbiter-"+s, label+"-watch-"+s
	c.funder = label + "-funder-" + s
	c.call("", "createwallet", nil, arbiter)
	c.call("", "createwallet", nil, watch, true, true)
	c.call("", "createwallet", nil, c.funder)
	party := func(name string) (root, key string) {
		t.Helper()
		c.call("", "createwallet", nil, name)
		var priv struct {
			Descriptors []struct {
				Desc     string `json:"desc"`
				Internal bool   `json:"internal"`
			} `json:"descriptors"`
		}
		c.call(name, "listdescriptors", &priv, true)
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		var imported []struct {
			Success bool `json:"success"`
		}
		c.call(name, "importdescriptors", &imported, []map[string]any{{"desc": withChecksum("wpkh(" + root + "/48h/1h/0h/2h/0/*)"), "active": false, "range": []int{0, 2}, "timestamp": "now"}})
		var pub struct {
			Descriptors []struct {
				Desc string `json:"desc"`
			} `json:"descriptors"`
		}
		c.call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				key = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
			}
		}
		if k, err := parseEscrowKey(key); root == "" || err != nil || !k.BIP48 {
			t.Fatalf("%s: no BIP48 account (%v)", name, err)
		}
		return root, key
	}
	var buyerKey, vendorKey string
	c.buyerRoot, buyerKey = party(label + "-buyer-" + s)
	c.vendorRoot, vendorKey = party(label + "-vendor-" + s)

	// The real escrow signer: built from this tree, a fresh seed, its own RPC user, a socket in a temporary directory.
	dir := t.TempDir()
	bin := filepath.Join(dir, "escrow-signer")
	if out, err := exec.Command("go", "build", "-o", bin, "opsecmkt/cmd/escrow-signer").CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/escrow-signer: %v\n%s", err, out)
	}
	seedPath, socket := filepath.Join(dir, "seed", "seed"), filepath.Join(dir, "signer.sock")
	c.signerLog = filepath.Join(dir, "signer.log")
	signerCmd := func(args ...string) *exec.Cmd {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"ESCROW_SIGNER_RPC_URL=" + signerURL}
		return cmd
	}
	if out, err := signerCmd("init", "-seed", seedPath).CombinedOutput(); err != nil {
		t.Fatalf("escrow-signer init: %v\n%s", err, out)
	}
	if c.seed, err = os.ReadFile(seedPath); err != nil { // in memory only, for the log check at the end; never logged
		t.Fatal(err)
	}
	logf, err := os.Create(c.signerLog)
	if err != nil {
		t.Fatal(err)
	}
	serve := signerCmd("serve", "-seed", seedPath, "-socket", socket)
	serve.Stdout, serve.Stderr = logf, logf
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serve.Process.Signal(syscall.SIGTERM); serve.Wait(); logf.Close() })
	for i := 0; ; i++ {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			break
		}
		if i == 100 {
			b, _ := os.ReadFile(c.signerLog)
			t.Fatalf("escrow-signer serve did not start: %s", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("escrow-signer (cmd/escrow-signer, built from this tree): a fresh seed, serving on its socket as the RPC user whitelisted to decodepsbt, descriptorprocesspsbt and getdescriptorinfo")

	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", appURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", arbiter)
	t.Setenv("ESCROW_BTC_WATCH_WALLET", watch)
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", socket)
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	c.e = newTestApp(t)
	if c.e.A.escrow.Load() == nil || c.e.A.escrowSignerState() != signerReady {
		t.Fatalf("escrow did not start against the node (signer %q)", c.e.A.escrowSignerState())
	}
	c.buyer, c.buyerSess = c.e.user(label+"_b_"+s, "buyer")
	c.vendor, c.vendorSess = c.e.user(label+"_v_"+s, "vendor")
	for uid, key := range map[string]string{c.buyer: buyerKey, c.vendor: vendorKey} {
		k := mustParseKey(t, key)
		sealed, err := c.e.A.seal("escrow-xpub", k.XPub)
		if err != nil {
			t.Fatal(err)
		}
		agExec(c.e, `INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at,layout) VALUES($1,$2,$3,$4,$5,now(),'per_order')`,
			uid, sealed, c.e.A.escrowKeyHMAC(k), k.Fingerprint, k.PathText)
	}
	c.product = c.e.product(c.vendor, "digital")
	agExec(c.e, "UPDATE products SET delivery_content='licence key A456B-REAL-CHAIN' WHERE id=$1", c.product)
	c.call(c.funder, "getnewaddress", &c.mine)
	matureFunds(t, c.ctx, c.op, c.mine)
	return c
}

// pass runs one escrow watcher pass.
func (c *shapeBChain) pass() {
	c.t.Helper()
	if err := c.e.A.escrowPollOnce(c.ctx); err != nil {
		c.t.Fatalf("watcher pass: %v", err)
	}
}

// paidEscrow is a per-order escrow created through the checkout form and paid its price at its address, confirmed: the
// order (completed on funding) and the escrow address.
func (c *shapeBChain) paidEscrow() (string, string) {
	c.t.Helper()
	order := c.e.order(c.buyer, c.product, "BTC", stateDraft)
	var refund string
	c.call(c.funder, "getnewaddress", &refund)
	if w := c.e.do("POST", "/orders/escrow", c.buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}}); w.Code != 303 {
		c.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var addr, price string
	var m int64
	if err := c.e.DB.QueryRow("SELECT es.address,es.market_index,o.amount FROM escrows es JOIN orders o ON o.id=es.order_id WHERE es.order_id=$1", order).Scan(&addr, &m, &price); err != nil {
		c.t.Fatal(err)
	}
	if got := agStrings(c.t, c.e, "SELECT derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id WHERE es.order_id=$1 AND party='market'", order); len(got) != 1 || got[0] != "/"+strconv.FormatInt(m, 10)+"/0/0" {
		c.t.Fatalf("not a per-order escrow: market suffix %v", got)
	}
	sats, _ := strconv.ParseInt(price, 10, 64)
	var txid string
	c.call(c.funder, "sendtoaddress", &txid, addr, json.Number(amount(sats, 8)))
	mustMine(c.t, c.ctx, c.op, int(c.e.A.escrow.Load().confirmations), c.mine)
	c.pass()
	if st := c.state(order); st != stateCompleted {
		c.t.Fatalf("order %s after funding", st)
	}
	c.t.Logf("per-order escrow %s (market index %d) created through the checkout form, paid %s BTC by %s, confirmed: order completed", addr, m, amount(sats, 8), txid)
	return order, addr
}

func (c *shapeBChain) state(order string) string {
	c.t.Helper()
	var s string
	if err := c.e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s); err != nil {
		c.t.Fatal(err)
	}
	return s
}

// partySign is the party's order wallet: its copy of settlement id downloaded, signed by Core's descriptorprocesspsbt
// with the party's private root along its key's per-order path for the script the settlement spends (the other two
// keys as hex), SIGHASH_ALL, not finalized. It returns the signed PSBT.
func (c *shapeBChain) partySign(order string, id int64, party string) string {
	c.t.Helper()
	sess, root := c.buyerSess, c.buyerRoot
	if party == roleVendor {
		sess, root = c.vendorSess, c.vendorRoot
	}
	w := c.e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
	if w.Code != 200 {
		c.t.Fatalf("%s's copy: %d", party, w.Code)
	}
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, order, id)
	if err != nil {
		c.t.Fatal(err)
	}
	var exprs []string
	for _, p := range []string{roleBuyer, roleVendor, "market"} {
		if p == party {
			exprs = append(exprs, root+"/48h/1h/0h/2h"+keys[p].Suffix)
		} else {
			exprs = append(exprs, hex.EncodeToString(keys[p].PubKey))
		}
	}
	var signed struct {
		PSBT     string `json:"psbt"`
		Complete bool   `json:"complete"`
	}
	c.call("", "descriptorprocesspsbt", &signed, strings.TrimSpace(w.Body.String()), []string{withChecksum("wsh(sortedmulti(2," + strings.Join(exprs, ",") + "))")}, "ALL", false, false)
	p, err := decodePSBT(signed.PSBT)
	if err != nil || signed.Complete || len(p.Inputs) == 0 {
		c.t.Fatalf("%s's order wallet did not sign: %v", party, err)
	}
	for i, in := range p.Inputs {
		if len(in.PartialSigs) != 1 || !bytes.Equal(in.PartialSigs[0].PubKey, keys[party].PubKey) {
			c.t.Fatalf("input %d: not the %s's signature alone", i, party)
		}
	}
	return signed.PSBT
}

// settled waits, with watcher passes, until settlement id is broadcast, mines the confirmation threshold and confirms
// it: its transaction is then checked with gettxout (output 0, the claimant's, unspent at its value and address).
func (c *shapeBChain) settled(id int64, what string) string {
	c.t.Helper()
	var state, txid, cosigned, sendErr string
	for range 3 {
		c.pass()
		if err := c.e.DB.QueryRow("SELECT state,COALESCE(txid,''),COALESCE(cosigned,''),send_error FROM settlements WHERE id=$1", id).Scan(&state, &txid, &cosigned, &sendErr); err != nil {
			c.t.Fatal(err)
		}
		if state == "broadcast" {
			break
		}
	}
	if state != "broadcast" || cosigned != "market" {
		b, _ := os.ReadFile(c.signerLog)
		c.t.Fatalf("%s: %s (cosigned %q, send_error %q); signer log:\n%s", what, state, cosigned, sendErr, b)
	}
	c.t.Logf("%s: co-signed by the escrow signer (cosigned=market), checked, testmempoolaccept allowed, broadcast: %s", what, txid)
	mustMine(c.t, c.ctx, c.op, int(c.e.A.escrow.Load().confirmations), c.mine)
	c.pass()
	if err := c.e.DB.QueryRow("SELECT state FROM settlements WHERE id=$1", id).Scan(&state); err != nil || state != "confirmed" {
		c.t.Fatalf("%s after the confirmations: %s %v", what, state, err)
	}
	var out struct {
		Confirmations int64       `json:"confirmations"`
		Value         json.Number `json:"value"`
		ScriptPubKey  struct {
			Address string `json:"address"`
		} `json:"scriptPubKey"`
	}
	c.call("", "gettxout", &out, txid, 0, false)
	var addr string
	var sats int64
	if err := c.e.DB.QueryRow("SELECT address,sats FROM settlement_outputs WHERE settlement_id=$1 AND n=0", id).Scan(&addr, &sats); err != nil {
		c.t.Fatal(err)
	}
	if out.Confirmations < c.e.A.escrow.Load().confirmations || out.ScriptPubKey.Address != addr || out.Value.String() != amount(sats, 8) {
		c.t.Fatalf("%s: gettxout %s:0 = %+v, want %s BTC to %s", what, txid, out, amount(sats, 8), addr)
	}
	c.t.Logf("%s: confirmed; gettxout %s:0 = %s BTC to %s, %d confirmations", what, txid, out.Value, addr, out.Confirmations)
	return txid
}

// noSecrets checks that the signer's log names no tprv and does not hold the seed, nor does any database column.
func (c *shapeBChain) noSecrets() {
	c.t.Helper()
	b, _ := os.ReadFile(c.signerLog)
	if tprvPattern.Match(b) || bytes.Contains(b, []byte(hex.EncodeToString(c.seed))) || bytes.Contains(b, c.seed) {
		c.t.Fatal("the signer's log holds key material")
	}
	assertSecretNowhereInDB(c.t, c.e, c.seed, "signer seed")
	assertSecretNowhereInDB(c.t, c.e, []byte(hex.EncodeToString(c.seed)), "signer seed (hex)")
	c.t.Logf("no tprv or seed in the signer's log or the database; the signer logged:\n%s", strings.TrimSpace(string(b)))
}

// TestEscrowRegtestShapeBRelease: a per-order escrow's release end to end. The vendor claims the completed order; the
// watcher builds the claim; the vendor's order wallet (Core, the vendor's private root at its per-order path) signs its
// copy and the vendor uploads the signed PSBT as a binary file through the multipart sign form; the next pass co-signs
// it through the real escrow signer at the escrow's market index (never the Core arbiter wallet), checks it, broadcasts
// it; it confirms, and gettxout shows the vendor's output.
func TestEscrowRegtestShapeBRelease(t *testing.T) {
	c := newShapeBChain(t, "a456b-rel")
	order, _ := c.paidEscrow()
	var claimTo string
	c.call(c.funder, "getnewaddress", &claimTo)
	if w := c.e.do("POST", "/escrow/claim", c.vendorSess, url.Values{"order_id": {order}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	c.pass()
	var id int64
	var state string
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='release'", order).Scan(&id, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("claim built: %s %v", state, err)
	}
	t.Logf("claim to %s built by the watcher (gettxout, createpsbt, utxoupdatepsbt with the escrow's bare-key descriptor)", claimTo)
	signed := c.partySign(order, id, roleVendor)
	raw, _ := base64.StdEncoding.DecodeString(signed)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("csrf", c.e.A.csrf(c.vendorSess))
	mw.WriteField("order_id", order)
	mw.WriteField("settlement_id", strconv.FormatInt(id, 10))
	fw, _ := mw.CreateFormFile("psbt_file", "escrow-claim.psbt")
	fw.Write(raw)
	mw.Close()
	r := httptest.NewRequest("POST", "/escrow/sign", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: c.vendorSess})
	w := httptest.NewRecorder()
	c.e.A.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	t.Logf("the vendor's order wallet (Core descriptorprocesspsbt, the vendor's key at its per-order path) signed its copy; uploaded as a binary PSBT file (%d bytes) through the multipart sign form -> 303, party_signed", len(raw))
	c.settled(id, "release (the vendor's claim)")
	c.noSecrets()
}

// TestEscrowRegtestShapeBSiblingStray: QA5W3A-2 on a real node. After a per-order escrow's order completed, the funder
// pays the order wallet's sibling address /0/3; the watcher records it (left out) and requests and builds a stray refund
// spending it with the sibling's own witness script (the parties' keys at <i>/0/3 and the signer's at <m>/0/3, stored at
// checkout); the buyer's order wallet signs its copy at its /<i>/0/3 key and pastes it; the real escrow signer co-signs
// at the escrow's market index, finding its sibling key; it is broadcast and confirmed, and gettxout shows the buyer's
// refund output.
func TestEscrowRegtestShapeBSiblingStray(t *testing.T) {
	c := newShapeBChain(t, "a456b-sib")
	order, _ := c.paidEscrow()
	var sib string
	if err := c.e.DB.QueryRow("SELECT s.address FROM escrow_siblings s JOIN escrows es ON es.id=s.escrow_id WHERE es.order_id=$1 AND s.branch=0 AND s.idx=3", order).Scan(&sib); err != nil {
		t.Fatal(err)
	}
	var txid string
	c.call(c.funder, "sendtoaddress", &txid, sib, json.Number("0.0007"))
	mustMine(t, c.ctx, c.op, int(c.e.A.escrow.Load().confirmations), c.mine)
	c.pass()
	c.pass()
	var id int64
	var state string
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order).Scan(&id, &state); err != nil || state != "awaiting_signature" {
		var class string
		c.e.DB.QueryRow("SELECT build_error FROM settlements WHERE order_id=$1 AND kind='stray_refund'", order).Scan(&class)
		t.Fatalf("stray refund of the sibling deposit: %s (%s) %v", state, class, err)
	}
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, order, id)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	c.e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored)
	p, _ := decodePSBT(stored)
	ws := sortedMultisigScript(keys["buyer"].PubKey, keys["vendor"].PubKey, keys["market"].PubKey)
	if len(p.Inputs) != 1 || !bytes.Equal(p.Inputs[0].WitnessScript, ws) || keys["market"].Suffix[len(keys["market"].Suffix)-4:] != "/0/3" {
		t.Fatalf("the stray refund does not spend the sibling's script (market %s)", keys["market"].Suffix)
	}
	var fromCore []string
	c.call("", "deriveaddresses", &fromCore, withChecksum("wsh(sortedmulti(2,"+hex.EncodeToString(keys["buyer"].PubKey)+","+hex.EncodeToString(keys["vendor"].PubKey)+","+hex.EncodeToString(keys["market"].PubKey)+"))"))
	if len(fromCore) != 1 || fromCore[0] != sib {
		t.Fatalf("Core derives %v from the stored sibling keys; the sibling is %s", fromCore, sib)
	}
	t.Logf("sibling /0/3 %s paid 0.0007 BTC (%s), recorded left out; stray refund built with the sibling's witness script (buyer %s, vendor %s, market %s), which Core derives to that address",
		sib, txid, keys["buyer"].Suffix, keys["vendor"].Suffix, keys["market"].Suffix)
	signed := c.partySign(order, id, roleBuyer)
	if w := c.e.do("POST", "/escrow/sign", c.buyerSess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {signed}}); w.Code != 303 {
		t.Fatalf("buyer signs: %d %s", w.Code, w.Body.String())
	}
	t.Logf("the buyer's order wallet signed its copy at its /0/3 key; pasted -> 303, party_signed")
	c.settled(id, "stray refund of the sibling deposit")
	c.noSecrets()
}
