package market

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

const (
	payStagenetPrimary = "5" + "6fNpjqGMn9o7n4bPdLjrkwWpCMZqYoT8RdMq3ALAPrWnUFrJvbC8JsbvsxUtaNPCNoxnfEYqBTFLaZSkqTcDFdS6KyAG1Y"
	payStagenetSub     = "7" + "4mcQgVQVd8PjSHtzMY7cRGsfKaYQXsvnRRfr2HbR9NhdV8kVTugcM7w4tvh1GVbKDnGHdpkrkHXcfaMM5zrPGq6UfaNeq8"
	payMainnetPrimary  = "4" + "4AFFq5kSiGBoZ4NMDwYtN18obc8AemS33DBLWs3H7otXft3XjrpDtQGv7SqSsaBYBb98uNbr2VBBEt7f2wfn3RVGQBEP3A"
	payTestnetPrimary  = "9" + "wviCeWe2D8XS82k2ovp5EUYLzBt9pYNW2LXUFsZiv8S3Mt21FZ5qQaAroko1enzw3eGr9qC7X1D7Geoo2RrAotYPwq9Gm8"
)

// payMoneroWallet is a scripted monero-wallet-rpc (and optionally monerod) JSON-RPC endpoint.
type payMoneroWallet struct {
	mu        sync.Mutex
	primary   string
	sub       string
	open      bool
	nettype   string // daemon get_info
	transfers map[string]any
	nextIndex int64
	valid     map[string]string // address -> nettype
}

func (m *payMoneroWallet) handle(path, method string, params json.RawMessage) (any, int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if path != "/json_rpc" {
		return nil, -32601, "bad path"
	}
	if !m.open && method != "open_wallet" && method != "get_info" {
		return nil, -13, "No wallet file"
	}
	switch method {
	case "get_info":
		return map[string]any{"nettype": m.nettype, "mainnet": m.nettype == "mainnet", "stagenet": m.nettype == "stagenet"}, 0, ""
	case "open_wallet":
		var p struct{ Filename string }
		json.Unmarshal(params, &p)
		if p.Filename != "opsecmkt" {
			return nil, -1, "Failed to open wallet"
		}
		m.open = true
		return map[string]any{}, 0, ""
	case "get_address":
		return map[string]any{"address": m.primary}, 0, ""
	case "create_address":
		m.nextIndex++
		return map[string]any{"address": m.sub, "address_index": m.nextIndex}, 0, ""
	case "get_address_index":
		var p struct{ Address string }
		json.Unmarshal(params, &p)
		if p.Address == m.sub {
			return map[string]any{"index": map[string]any{"major": 0, "minor": 7}}, 0, ""
		}
		return nil, -2, "Address doesn't belong to the wallet"
	case "get_transfers":
		return m.transfers, 0, ""
	case "transfer":
		return map[string]any{"tx_hash": "xmr-payout-1", "fee": 100}, 0, ""
	case "validate_address":
		var p struct{ Address string }
		json.Unmarshal(params, &p)
		nt, ok := m.valid[p.Address]
		return map[string]any{"valid": ok, "nettype": nt}, 0, ""
	}
	return nil, -32601, "Method not found"
}

func newPayMonero(t *testing.T, primary, sub string) (*payMoneroWallet, *payRPCServer) {
	m := &payMoneroWallet{primary: primary, sub: sub, open: true, nettype: "stagenet", valid: map[string]string{}}
	return m, newPayRPCServer(t, "wallet", "wallet-secret", true, m.handle)
}

func TestMoneroRefusesMainnet(t *testing.T) {
	ctx := context.Background()
	_, main := newPayMonero(t, payMainnetPrimary, payStagenetSub)
	_, err := newMoneroProvider(ctx, main.url(""), "", "", 10)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "mainnet") {
		t.Fatalf("mainnet wallet accepted: %v", err)
	}
	if strings.Contains(err.Error(), "wallet-secret") || strings.Contains(err.Error(), payMainnetPrimary) {
		t.Fatal("error leaks the password or the full wallet address")
	}
	w, stage := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.nettype = "mainnet"
	if _, err = newMoneroProvider(ctx, stage.url(""), stage.url(""), "", 10); err == nil || !strings.Contains(err.Error(), `monero daemon network "mainnet"`) {
		t.Fatalf("mainnet daemon accepted: %v", err)
	}
	w.nettype = "testnet"
	if _, err = newMoneroProvider(ctx, stage.url(""), stage.url(""), "", 10); err == nil || !strings.Contains(err.Error(), "daemon is on testnet") {
		t.Fatalf("daemon/wallet network mismatch accepted: %v", err)
	}
	w.nettype = "stagenet"
	if _, err = newMoneroProvider(ctx, stage.url(""), "", "testnet", 10); err == nil || !strings.Contains(err.Error(), `MONERO_NETWORK is "testnet"`) {
		t.Fatalf("MONERO_NETWORK mismatch accepted: %v", err)
	}
	if _, err = newMoneroProvider(ctx, stage.url(""), "", "mainnet", 10); err == nil || !strings.Contains(err.Error(), "not a test network") {
		t.Fatalf("MONERO_NETWORK=mainnet accepted: %v", err)
	}
	_, mixed := newPayMonero(t, payStagenetPrimary, "8"+payStagenetSub[1:])
	p, err := newMoneroProvider(ctx, mixed.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if addr, err := p.NewAddress(ctx, "order-1"); err == nil {
		t.Fatalf("mainnet subaddress accepted: %s", addr)
	}
	_, tn := newPayMonero(t, payTestnetPrimary, "B"+payStagenetSub[1:])
	tp, err := newMoneroProvider(ctx, tn.url(""), "", "", 10)
	if err != nil || tp.Network() != "testnet" {
		t.Fatalf("testnet wallet: %v", err)
	}
}

func TestMoneroHappyPath(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.open = false // wallet-rpc started with --wallet-dir: the adapter opens the conventional wallet once
	p, err := newMoneroProvider(ctx, s.url(""), s.url(""), "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.called("open_wallet") != 1 || p.Currency() != "XMR" || p.Network() != "stagenet" || p.Confirmations() != 10 {
		t.Fatal("provider setup")
	}
	addr, err := p.NewAddress(ctx, "order-xyz")
	if err != nil || addr != payStagenetSub {
		t.Fatalf("NewAddress %q %v", addr, err)
	}
	if got := string(s.lastParams("create_address")); got != `{"account_index":0,"label":"order:order-xyz"}` {
		t.Fatalf("create_address params %s", got)
	}
	w.transfers = map[string]any{
		"in": []map[string]any{
			{"txid": "in-1", "amount": json.Number("500000000000"), "confirmations": 12, "subaddr_index": map[string]any{"major": 0, "minor": 1}},
			{"txid": "in-2", "amount": json.Number("250000000000"), "confirmations": 12, "double_spend_seen": true, "subaddr_index": map[string]any{"major": 0, "minor": 1}},
			{"txid": "in-other", "amount": 5, "confirmations": 3, "subaddr_index": map[string]any{"major": 0, "minor": 99}},
		},
		"pool": []map[string]any{
			{"txid": "pool-1", "amount": json.Number("1000"), "subaddr_index": map[string]any{"major": 0, "minor": 1}},
		},
	}
	in, err := p.Incoming(ctx, []string{addr, "7unknownaddress"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Incoming{
		{Address: addr, TxID: "in-1", Index: 1, Amount: 500000000000, Confirmations: 12},
		{Address: addr, TxID: "in-2", Index: 1, Amount: 250000000000, Confirmations: -1},
		{Address: addr, TxID: "pool-1", Index: 1, Amount: 1000, Confirmations: 0},
	}
	if len(in) != len(want) {
		t.Fatalf("incoming %+v", in)
	}
	for i := range want {
		if in[i] != want[i] {
			t.Errorf("incoming[%d] = %+v want %+v", i, in[i], want[i])
		}
	}
	if got := string(s.lastParams("get_transfers")); got != `{"account_index":0,"in":true,"pool":true,"subaddr_indices":[1]}` {
		t.Fatalf("get_transfers params %s", got)
	}
	// After a restart the index cache is empty: get_address_index resolves it.
	p.index = map[string]int64{}
	w.transfers = map[string]any{"in": []map[string]any{{"txid": "in-9", "amount": 7, "confirmations": 1, "subaddr_index": map[string]any{"major": 0, "minor": 7}}}}
	if in, err = p.Incoming(ctx, []string{addr}); err != nil || len(in) != 1 || in[0].Index != 7 {
		t.Fatalf("index lookup: %+v %v", in, err)
	}
	txid, err := p.Send(ctx, payStagenetSub, 42)
	if err != nil || txid != "xmr-payout-1" {
		t.Fatalf("Send %q %v", txid, err)
	}
	if got := string(s.lastParams("transfer")); !strings.Contains(got, `"destinations":[{"address":"`+payStagenetSub+`","amount":42}]`) || !strings.Contains(got, `"account_index":0`) {
		t.Fatalf("transfer params %s", got)
	}
	w.valid[payStagenetSub] = "stagenet"
	w.valid[payTestnetPrimary] = "testnet"
	calls := s.called("validate_address")
	if p.ValidAddress(payMainnetPrimary) || p.ValidAddress("5short") || s.called("validate_address") != calls {
		t.Fatal("mainnet or malformed address accepted, or checked over RPC")
	}
	if !p.ValidAddress(payStagenetSub) || p.ValidAddress(payTestnetPrimary) || p.ValidAddress(payStagenetPrimary) {
		t.Fatal("validate_address nettype not enforced")
	}
}

func TestMoneroEnvConfiguration(t *testing.T) {
	t.Setenv("MONERO_WALLET_RPC_URL", "")
	t.Setenv("MONERO_RPC_URL", "http://monero:18081")
	if p, err := moneroFromEnv(context.Background()); p != nil || err != nil {
		t.Fatalf("a daemon URL alone must not enable XMR: %v %v", p, err)
	}
	_, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	t.Setenv("MONERO_WALLET_RPC_URL", s.url(""))
	t.Setenv("MONERO_RPC_URL", "")
	t.Setenv("MONERO_NETWORK", "stagenet")
	t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "abc")
	if _, err := moneroFromEnv(context.Background()); err == nil || !strings.Contains(err.Error(), "PAYMENT_CONFIRMATIONS_XMR") {
		t.Fatalf("bad confirmations accepted: %v", err)
	}
	t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "")
	p, err := moneroFromEnv(context.Background())
	if err != nil || p.Network() != "stagenet" || p.Confirmations() != 10 {
		t.Fatalf("defaults: %v %v", p, err)
	}
}
