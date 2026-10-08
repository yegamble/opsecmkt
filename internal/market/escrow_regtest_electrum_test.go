package market

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This manual leg is invoked only by scripts/test-escrow-wallets.sh with a verified Electrum source and a real
// isolated Core regtest. The bridge keeps both BIP39 roots in its memory; Core only funds and observes the chain.
type electrumBridge struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *bytes.Buffer
}

func startElectrumBridge(t *testing.T) *electrumBridge {
	t.Helper()
	python, src := os.Getenv("ESCROW_WALLETS_BRIDGE_PYTHON"), os.Getenv("ESCROW_WALLETS_ELECTRUM_SRC")
	if python == "" || src == "" || os.Getenv("ESCROW_WALLETS_ELECTRUM_VERSION") == "" {
		t.Skip("run scripts/test-escrow-wallets.sh for the verified Electrum market journey")
	}
	_, testFile, _, _ := runtime.Caller(0)
	cmd := exec.Command(python, filepath.Join(filepath.Dir(testFile), "..", "..", "scripts", "escrow-electrum-bridge.py"))
	cmd.Env = []string{"ESCROW_WALLETS_ELECTRUM_SRC=" + src, "ESCROW_WALLETS_ELECTRUM_VERSION=" + os.Getenv("ESCROW_WALLETS_ELECTRUM_VERSION"), "HOME=" + t.TempDir()}
	startup := new(bytes.Buffer)
	cmd.Stderr = startup
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	b := &electrumBridge{cmd: cmd, in: in, out: bufio.NewReader(out), stderr: startup}
	t.Cleanup(func() { in.Close(); cmd.Wait() })
	return b
}

func (b *electrumBridge) request(t *testing.T, req any, dst any) {
	t.Helper()
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.in.Write(append(line, '\n')); err != nil {
		t.Fatalf("Electrum bridge write: %v", err)
	}
	line, err = b.out.ReadBytes('\n')
	if err != nil {
		t.Fatalf("Electrum bridge read: %v; startup diagnostic: %s", err, b.stderr.String())
	}
	var result struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(line, &result); err != nil {
		t.Fatalf("Electrum bridge response: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("Electrum bridge: %s", result.Error)
	}
	if err := json.Unmarshal(line, dst); err != nil {
		t.Fatalf("Electrum bridge decode: %v", err)
	}
}

var (
	setupPath  = regexp.MustCompile(`<pre class="code-block escrow-setup-path"[^>]*>([^<]+)</pre>`)
	setupAddr  = regexp.MustCompile(`<pre class="code-block escrow-setup-address"[^>]*>([^<]+)</pre>`)
	orderIndex = regexp.MustCompile(`/([0-9]+)\]$`)
)

func (b *electrumBridge) sign(t *testing.T, party string, index int, child int, cosigners []string, address, psbt string) string {
	t.Helper()
	var response struct {
		PSBT    string `json:"psbt"`
		Address string `json:"address"`
	}
	b.request(t, map[string]any{"op": "sign", "party": party, "index": index, "branch": 0, "child": child, "cosigners": cosigners, "address": address, "psbt": psbt}, &response)
	if response.Address != address || response.PSBT == "" {
		t.Fatal("Electrum did not sign the market's address and PSBT")
	}
	return response.PSBT
}

func electrumSetup(t *testing.T, c *shapeBChain, order, sess string) (int, []string, string) {
	t.Helper()
	w := c.e.do("GET", "/escrow-setup?id="+order, sess, nil)
	if w.Code != 200 {
		t.Fatalf("order-wallet setup: %d", w.Code)
	}
	body := w.Body.String()
	path, addr := setupPath.FindStringSubmatch(body), setupAddr.FindStringSubmatch(body)
	var cosigners []string
	for _, m := range cosignerArea.FindAllStringSubmatch(body, -1) {
		cosigners = append(cosigners, html.UnescapeString(m[1]))
	}
	if path == nil || addr == nil || len(cosigners) != 2 {
		t.Fatal("order-wallet setup lacks the path, address or two rooted cosigners")
	}
	match := orderIndex.FindStringSubmatch(path[1])
	if match == nil {
		t.Fatalf("order-wallet path %q", path[1])
	}
	index, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return index, cosigners, html.UnescapeString(addr[1])
}

func electrumSettlement(t *testing.T, c *shapeBChain, b *electrumBridge, order string, id int64, party string, child int, wantAddr string) {
	t.Helper()
	sess := c.buyerSess
	if party == roleVendor {
		sess = c.vendorSess
	}
	index, cosigners, address := electrumSetup(t, c, order, sess)
	if child == 0 && address != wantAddr {
		t.Fatalf("order-wallet address %s, want %s", address, wantAddr)
	}
	w := c.e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
	if w.Code != 200 {
		t.Fatalf("%s PSBT download: %d", party, w.Code)
	}
	signed := b.sign(t, party, index, child, cosigners, wantAddr, strings.TrimSpace(w.Body.String()))
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, order, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodePSBT(signed)
	if err != nil || len(p.Inputs) == 0 {
		t.Fatalf("Electrum PSBT: %v", err)
	}
	for i, in := range p.Inputs {
		if len(in.PartialSigs) != 1 || !bytes.Equal(in.PartialSigs[0].PubKey, keys[party].PubKey) {
			t.Fatalf("input %d: expected only %s's Electrum signature", i, party)
		}
	}
	if w := c.e.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {signed}}); w.Code != 303 {
		t.Fatalf("%s signed paste: %d %s", party, w.Code, w.Body.String())
	}
	t.Logf("%s Electrum order wallet at index %d /0/%d signed downloaded PSBT; paste accepted", party, index, child)
}

func TestEscrowRegtestElectrumMarketJourney(t *testing.T) {
	b := startElectrumBridge(t)
	var account struct {
		Keys    map[string]string `json:"keys"`
		Version string            `json:"version"`
	}
	b.request(t, map[string]any{"op": "keys"}, &account)
	if account.Version != os.Getenv("ESCROW_WALLETS_ELECTRUM_VERSION") || len(account.Keys) != 2 {
		t.Fatal("Electrum bridge did not supply both expected public keys")
	}
	c := newShapeBChainWithPartyKeys(t, "a457", account.Keys, func(c *shapeBChain) {
		for _, party := range []struct{ name, uid, sess string }{{roleBuyer, c.buyer, c.buyerSess}, {roleVendor, c.vendor, c.vendorSess}} {
			key := account.Keys[party.name]
			if code, body := registerKey(c.e, party.sess, key, "layout", "per_order"); code != 303 {
				t.Fatalf("%s register: %d %s", party.name, code, body)
			}
			psbt, cosigners, address, path := perOrderPractice(t, c.e, party.sess)
			parsed := mustParseKey(t, key)
			if path != fmt.Sprintf("[%s/48h/1h/0h/2h/2147483647]", parsed.Fingerprint) {
				t.Fatalf("%s practice path %s", party.name, path)
			}
			signed := b.sign(t, party.name, 2147483647, 0, cosigners, address, psbt)
			if code, body := submitPractice(c.e, party.sess, signed); code != 303 {
				t.Fatalf("%s practice: %d %s", party.name, code, body)
			}
			if agInt(c.e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL AND layout='per_order'", party.uid) != 1 {
				t.Fatalf("%s practice did not activate", party.name)
			}
			t.Logf("%s registered public BIP39/BIP48 key and Electrum practice signature activated it", party.name)
		}
	})
	order, addr := c.paidEscrow()
	var claimTo string
	c.call(c.funder, "getnewaddress", &claimTo)
	if w := c.e.do("POST", "/escrow/claim", c.vendorSess, url.Values{"order_id": {order}, "address": {claimTo}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("vendor claim: %d %s", w.Code, w.Body.String())
	}
	c.pass()
	var release int64
	var state string
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='release'", order).Scan(&release, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("release: %s %v", state, err)
	}
	// A-459: the real watch wallet supplies the funding transaction on the optional hardware download. No physical
	// signing device is involved in this leg; Electrum signs its ordinary copy below.
	hw := c.e.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(release, 10)+"&hardware=1", c.vendorSess, nil)
	if hw.Code != 200 || hw.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("hardware release copy: %d %s", hw.Code, hw.Body.String())
	}
	hp, err := decodeHardwarePSBT(base64.StdEncoding.EncodeToString(hw.Body.Bytes()))
	if err != nil || len(hp.Inputs) == 0 {
		t.Fatalf("hardware release PSBT: %v", err)
	}
	for i, input := range hp.Inputs {
		if len(input.NonWitnessUTXO) == 0 || input.WitnessUTXO == nil || len(input.Derivations) != 1 {
			t.Fatalf("hardware input %d missing verified funding or the reader's origin", i)
		}
		id := hp.Tx.In[i].TxID
		slices.Reverse(id[:])
		txid := hex.EncodeToString(id[:])
		out, err := fundingTxOutput(hex.EncodeToString(input.NonWitnessUTXO), txid, hp.Tx.In[i].Vout)
		if err != nil || out.Value != input.WitnessUTXO.Value || !bytes.Equal(out.Script, input.WitnessUTXO.Script) {
			t.Fatalf("hardware input %d does not match its Core funding transaction: %v", i, err)
		}
	}
	t.Logf("vendor hardware release copy: %d real Core funding transaction(s) verified as non_witness_utxo", len(hp.Inputs))
	electrumSettlement(t, c, b, order, release, roleVendor, 0, addr)
	c.settled(release, "Electrum vendor release")

	siblingOrder, _ := c.paidEscrow()
	var sibling string
	if err := c.e.DB.QueryRow("SELECT s.address FROM escrow_siblings s JOIN escrows es ON es.id=s.escrow_id WHERE es.order_id=$1 AND s.branch=0 AND s.idx=3", siblingOrder).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	var deposit string
	c.call(c.funder, "sendtoaddress", &deposit, sibling, json.Number("0.0007"))
	mustMine(t, c.ctx, c.op, int(c.e.A.escrow.Load().confirmations), c.mine)
	c.pass()
	c.pass()
	var refund int64
	if err := c.e.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund'", siblingOrder).Scan(&refund, &state); err != nil || state != "awaiting_signature" {
		t.Fatalf("sibling refund: %s %v", state, err)
	}
	keys, err := c.e.A.settlementKeys(c.ctx, c.e.DB, siblingOrder, refund)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	c.e.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", refund).Scan(&stored)
	p, err := decodePSBT(stored)
	if err != nil || len(p.Inputs) != 1 || !bytes.Equal(p.Inputs[0].WitnessScript, sortedMultisigScript(keys[roleBuyer].PubKey, keys[roleVendor].PubKey, keys["market"].PubKey)) || !strings.HasSuffix(keys["market"].Suffix, "/0/3") {
		t.Fatal("refund PSBT does not spend the funded sibling's script")
	}
	electrumSettlement(t, c, b, siblingOrder, refund, roleBuyer, 3, sibling)
	c.settled(refund, "Electrum buyer sibling refund")
	t.Logf("sibling /0/3 deposit %s refunded on chain", deposit)

	// A mixed pair must not create an escrow, even after both Electrum keys proved usable above.
	if _, err := c.e.DB.Exec("UPDATE user_escrow_keys SET layout='core' WHERE user_id=$1", c.vendor); err != nil {
		t.Fatal(err)
	}
	mixed := c.e.order(c.buyer, c.product, "BTC", stateDraft)
	var refundAddr string
	c.call(c.funder, "getnewaddress", &refundAddr)
	w := c.e.do("POST", "/orders/escrow", c.buyerSess, url.Values{"order_id": {mixed}, "refund_address": {refundAddr}})
	if w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), "per-order wallet") || agInt(c.e, "SELECT count(*) FROM escrows WHERE order_id=$1", mixed) != 0 {
		t.Fatalf("mixed pair: %d, escrow rows %d", w.Code, agInt(c.e, "SELECT count(*) FROM escrows WHERE order_id=$1", mixed))
	}
	c.noSecrets()
	t.Log("mixed per-order/Core pair refused before escrow creation; no Core party key signed either settlement")
}
