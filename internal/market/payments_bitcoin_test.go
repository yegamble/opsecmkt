package market

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// payCore is a scripted Bitcoin Core wallet RPC.
type payCore struct {
	mu           sync.Mutex
	chain        string
	newAddr      string
	walletLoaded bool
	received     []map[string]any
	txs          map[string]map[string]any
	invalid      map[string]bool
}

func (c *payCore) handle(path, method string, params json.RawMessage) (any, int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	walletCall := strings.HasPrefix(path, "/wallet/")
	if walletCall && path != "/wallet/opsecmkt" {
		return nil, -18, "Requested wallet does not exist or is not loaded"
	}
	if walletCall && !c.walletLoaded {
		return nil, -18, "Requested wallet does not exist or is not loaded"
	}
	switch method {
	case "getblockchaininfo":
		return map[string]any{"chain": c.chain, "blocks": 100}, 0, ""
	case "loadwallet":
		var p []string
		json.Unmarshal(params, &p)
		if len(p) != 1 || p[0] != "opsecmkt" {
			return nil, -18, "Wallet file verification failed. Failed to load database path. Path does not exist."
		}
		c.walletLoaded = true
		return map[string]any{"name": "opsecmkt"}, 0, ""
	case "getwalletinfo":
		return map[string]any{"walletname": "opsecmkt"}, 0, ""
	case "getnewaddress":
		return c.newAddr, 0, ""
	case "listreceivedbyaddress":
		return c.received, 0, ""
	case "gettransaction":
		var p []any
		json.Unmarshal(params, &p)
		if tx, ok := c.txs[p[0].(string)]; ok {
			return tx, 0, ""
		}
		return nil, -5, "Invalid or non-wallet transaction id"
	case "sendtoaddress":
		return "payout-txid-1", 0, ""
	case "validateaddress":
		var p []string
		json.Unmarshal(params, &p)
		return map[string]any{"isvalid": !c.invalid[p[0]]}, 0, ""
	}
	return nil, -32601, "Method not found"
}

func newPayCore(t *testing.T, chain string) (*payCore, *payRPCServer) {
	c := &payCore{chain: chain, newAddr: "tb1qorderaddress000000000000000000000", walletLoaded: true, txs: map[string]map[string]any{}, invalid: map[string]bool{}}
	return c, newPayRPCServer(t, "marketplace", "rpc-secret", false, c.handle)
}

func TestBitcoinRefusesMainnetAndMismatches(t *testing.T) {
	ctx := context.Background()
	_, main := newPayCore(t, "main")
	_, err := newBitcoinProvider(ctx, main.url(""), "opsecmkt", "", 3)
	if err == nil || !strings.Contains(err.Error(), `refusing to start: bitcoin chain "main"`) {
		t.Fatalf("mainnet node accepted: %v", err)
	}
	if strings.Contains(err.Error(), "rpc-secret") {
		t.Fatal("error leaks the RPC password")
	}
	_, signet := newPayCore(t, "signet")
	if _, err = newBitcoinProvider(ctx, signet.url(""), "opsecmkt", "testnet4", 3); err == nil || !strings.Contains(err.Error(), `reports chain "signet" but BITCOIN_CHAIN is "testnet4"`) {
		t.Fatalf("chain mismatch accepted: %v", err)
	}
	if _, err = newBitcoinProvider(ctx, signet.url(""), "opsecmkt", "main", 3); err == nil || !strings.Contains(err.Error(), "refusing to start") || signet.called("getblockchaininfo") != 1 {
		t.Fatalf("BITCOIN_CHAIN=main must fail before connecting: %v", err)
	}
	if _, err = newBitcoinProvider(ctx, signet.url(""), "otherwallet", "", 3); err == nil || !strings.Contains(err.Error(), "createwallet") {
		t.Fatalf("missing wallet: %v", err)
	}
}

func TestBitcoinRefusesMainnetAddresses(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "testnet4")
	p, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "testnet4", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bc1qmainnetaddress00000000000000000000", "bcrt1qregtestaddress000000000000000000", "1BoatSLRHtKNngkdXEeobR76b53LETtpyT", ""} {
		core.mu.Lock()
		core.newAddr = bad
		core.mu.Unlock()
		if addr, err := p.NewAddress(ctx, "order-1"); err == nil {
			t.Errorf("testnet4 wallet address %q accepted as %q", bad, addr)
		}
	}
	_, rs := newPayCore(t, "regtest")
	r, err := newBitcoinProvider(ctx, rs.url(""), "opsecmkt", "regtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.NewAddress(ctx, "order-1"); err == nil {
		t.Fatal("regtest provider accepted a tb1 address")
	}
	if r.Network() != "regtest" || p.Network() != "testnet4" {
		t.Fatalf("networks %s %s", r.Network(), p.Network())
	}
}

func TestBitcoinHappyPath(t *testing.T) {
	ctx := context.Background()
	core, s := newPayCore(t, "testnet4")
	core.walletLoaded = false
	p, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if s.called("loadwallet") != 1 || p.Currency() != "BTC" || p.Confirmations() != 3 {
		t.Fatal("wallet was not loaded once")
	}
	addr, err := p.NewAddress(ctx, "abc123")
	if err != nil || addr != core.newAddr {
		t.Fatalf("NewAddress: %q %v", addr, err)
	}
	if got := string(s.lastParams("getnewaddress")); got != `["order:abc123","bech32"]` {
		t.Fatalf("getnewaddress params %s", got)
	}
	other := "tb1qsomeoneelse0000000000000000000000"
	core.received = []map[string]any{
		{"address": addr, "amount": json.Number("0.00150000"), "confirmations": 2, "txids": []string{"tx-a", "tx-b"}},
		{"address": other, "amount": json.Number("1.0"), "confirmations": 5, "txids": []string{"tx-other"}},
	}
	core.txs["tx-a"] = map[string]any{"confirmations": 2, "details": []map[string]any{
		{"address": addr, "category": "receive", "amount": json.Number("0.00100000"), "vout": 1},
		{"address": addr, "category": "receive", "amount": json.Number("0.00050000"), "vout": 3},
		{"address": other, "category": "receive", "amount": json.Number("0.50000000"), "vout": 0},
		{"address": addr, "category": "send", "amount": json.Number("-0.1"), "vout": 2},
	}}
	core.txs["tx-b"] = map[string]any{"confirmations": -4, "details": []map[string]any{
		{"address": addr, "category": "receive", "amount": json.Number("0.00020000"), "vout": 0},
	}}
	in, err := p.Incoming(ctx, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	want := []Incoming{
		{Address: addr, TxID: "tx-a", Index: 1, Amount: 100000, Confirmations: 2},
		{Address: addr, TxID: "tx-a", Index: 3, Amount: 50000, Confirmations: 2},
		{Address: addr, TxID: "tx-b", Index: 0, Amount: 20000, Confirmations: -4},
	}
	if len(in) != len(want) {
		t.Fatalf("incoming %+v", in)
	}
	for i := range want {
		if in[i] != want[i] {
			t.Errorf("incoming[%d] = %+v want %+v", i, in[i], want[i])
		}
	}
	if s.called("gettransaction") != 2 {
		t.Fatalf("gettransaction called %d times; unrelated txids must not be fetched", s.called("gettransaction"))
	}
	txid, err := p.Send(ctx, "tb1qvendorpayout000000000000000000000", 123456)
	if err != nil || txid != "payout-txid-1" {
		t.Fatalf("Send %q %v", txid, err)
	}
	if got := string(s.lastParams("sendtoaddress")); got != `["tb1qvendorpayout000000000000000000000",0.00123456,"","",true]` {
		t.Fatalf("sendtoaddress params %s", got)
	}
	if _, err = p.Send(ctx, "bc1qmainnet000000000000000000000000", 1); err == nil {
		t.Fatal("sent to a mainnet address")
	}
	calls := s.called("validateaddress")
	for _, bad := range []string{"bc1qmainnet000000000000000000000000", "TB1QUPPERCASE0000000000000000000000", "tb1", "mzBc4XEFSdzCDcTxAgf6EZXgsZWpztRhef"} {
		if p.ValidAddress(bad) {
			t.Errorf("ValidAddress(%q) = true", bad)
		}
	}
	if s.called("validateaddress") != calls {
		t.Fatal("format-invalid addresses must be rejected before any RPC")
	}
	core.invalid["tb1qbadchecksum00000000000000000000000"] = true
	if p.ValidAddress("tb1qbadchecksum00000000000000000000000") || !p.ValidAddress("tb1qvendorpayout000000000000000000000") {
		t.Fatal("validateaddress result ignored")
	}
}

func TestBitcoinEnvConfiguration(t *testing.T) {
	t.Setenv("BITCOIN_RPC_URL", "")
	if p, err := bitcoinFromEnv(context.Background()); p != nil || err != nil {
		t.Fatalf("blank URL must disable BTC: %v %v", p, err)
	}
	_, s := newPayCore(t, "signet")
	t.Setenv("BITCOIN_RPC_URL", s.url(""))
	t.Setenv("BITCOIN_CHAIN", "signet")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "0")
	if _, err := bitcoinFromEnv(context.Background()); err == nil || !strings.Contains(err.Error(), "PAYMENT_CONFIRMATIONS_BTC") {
		t.Fatalf("zero confirmations accepted: %v", err)
	}
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "")
	t.Setenv("PAYMENT_POLL_INTERVAL", "5ms")
	if _, err := bitcoinFromEnv(context.Background()); err == nil || !strings.Contains(err.Error(), "PAYMENT_POLL_INTERVAL") {
		t.Fatalf("bad poll interval accepted: %v", err)
	}
	t.Setenv("PAYMENT_POLL_INTERVAL", "")
	p, err := bitcoinFromEnv(context.Background())
	if err != nil || p.Network() != "signet" || p.Confirmations() != 3 {
		t.Fatalf("defaults: %v %v", p, err)
	}
}

// New() (the server's startup path) fails closed on a mainnet node; main.go turns that into exit status 1.
func TestStartupRefusesMainnet(t *testing.T) {
	_, s := newPayCore(t, "main")
	dsn := testSchemaDSN(t)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("BITCOIN_RPC_URL", s.url(""))
	t.Setenv("BITCOIN_CHAIN", "")
	a, err := New(context.Background(), false)
	if err == nil {
		a.Close()
		t.Fatal("New accepted a mainnet bitcoin node")
	}
	if !strings.Contains(err.Error(), `refusing to start: bitcoin chain "main"`) {
		t.Fatalf("unclear startup error: %v", err)
	}
	if testing.Short() {
		t.Skip("skipping the server-binary exit status check in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/server")
	cmd.Env = append(os.Environ(), "ADDR=127.0.0.1:0", "COOKIE_SECURE=false")
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() == 0 {
		t.Fatalf("server did not exit non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `refusing to start: bitcoin chain "main"`) || strings.Contains(string(out), "rpc-secret") {
		t.Fatalf("server output: %s", out)
	}
}

// TestRegtestSmoke runs against a real bitcoind -regtest (see scripts/regtest-smoke.sh); skipped otherwise.
func TestRegtestSmoke(t *testing.T) {
	raw := os.Getenv("BITCOIN_REGTEST_RPC_URL")
	if raw == "" {
		t.Skip("set BITCOIN_REGTEST_RPC_URL (scripts/regtest-smoke.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	p, err := newBitcoinProvider(ctx, raw, "opsecmkt", "regtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.NewAddress(ctx, "smoke"+randomToken()[:8])
	if err != nil || !p.ValidAddress(addr) {
		t.Fatalf("address %q %v", addr, err)
	}
	var mine string
	if err = p.rpc.call(ctx, p.wallet, "getnewaddress", []any{"smoke-miner", "bech32"}, &mine); err != nil {
		t.Fatal(err)
	}
	if err = p.rpc.call(ctx, "", "generatetoaddress", []any{101, mine}, nil); err != nil {
		t.Fatal(err)
	}
	txid, err := p.Send(ctx, addr, 150000)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.rpc.call(ctx, "", "generatetoaddress", []any{1, mine}, nil); err != nil {
		t.Fatal(err)
	}
	in, err := p.Incoming(ctx, []string{addr})
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 || in[0].TxID != txid || in[0].Confirmations < 1 || in[0].Amount <= 0 || in[0].Amount > 150000 {
		t.Fatalf("incoming %+v (txid %s)", in, txid)
	}
}
