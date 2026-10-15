package market

import (
	"bufio"
	"bytes"

	"encoding/json"

	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"

	"strconv"
	"strings"
	"testing"
)

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
