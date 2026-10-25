package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type bondBTCTestNode struct {
	*httptest.Server
	mu                     sync.Mutex
	labels                 map[string]string
	outputs                []map[string]any
	chain                  string
	private, scanning, ibd bool
	fault                  string
	calls                  []string
}

func newBondBTCTestProvider(t *testing.T) (*btcBondProvider, *bondBTCTestNode) {
	t.Helper()
	f := &bondBTCTestNode{labels: make(map[string]string), chain: "regtest"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Method string            `json:"method"`
			ID     json.RawMessage   `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, q.Method)
		res := any(map[string]any{})
		code, msg := 0, ""
		var arg string
		if len(q.Params) > 0 {
			_ = json.Unmarshal(q.Params[0], &arg)
		}
		switch q.Method {
		case "getblockchaininfo":
			res = map[string]any{"chain": f.chain, "blocks": 120, "headers": 120, "initialblockdownload": f.ibd}
		case "getnetworkinfo":
			res = map[string]any{"connections": 8}
		case "getwalletinfo":
			res = map[string]any{"walletname": "bond-watch", "private_keys_enabled": f.private, "descriptors": true, "scanning": f.scanning}
		case "getaddressesbylabel":
			if address := f.labels[arg]; address != "" {
				res = map[string]any{address: map[string]any{"purpose": "receive"}}
			} else {
				code, msg = -11, "No addresses with label"
			}
		case "getnewaddress":
			address := regtestAddr(arg)
			f.labels[arg] = address
			res = address
		case "getaddressinfo":
			res = map[string]any{"address": arg, "ismine": true, "iswatchonly": false, "solvable": true}
		case "listunspent":
			if len(q.Params) != 4 || string(q.Params[0]) != "0" || string(q.Params[3]) != "true" {
				t.Errorf("listunspent params: %s", q.Params)
			}
			if f.outputs == nil {
				res = []any{}
			} else {
				res = f.outputs
			}
		default:
			t.Errorf("unexpected bond RPC: %s", q.Method)
		}
		if q.Method == f.fault {
			code, msg = -4, "private descriptor and user:password@database"
		}
		envelope := map[string]any{"id": q.ID, "result": res, "error": nil}
		if code != 0 {
			envelope["result"], envelope["error"] = nil, map[string]any{"code": code, "message": msg}
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(f.Close)
	rpc, err := newRPCClient("bitcoin vendor bond", f.URL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	p := &btcBondProvider{rpc: rpc, wallet: "bond-watch", path: "/wallet/bond-watch", chain: "regtest", confirmations: 3, pin: func(context.Context) error { return nil }}
	return p, f
}

func bondBTCOutput(address, tag string, value string, confs int, safe bool) map[string]any {
	return map[string]any{"address": address, "txid": hex64(tag), "vout": 0, "amount": json.Number(value), "confirmations": confs, "safe": safe}
}

func TestVendorBondBTCAllocationRecoversLabelsAndNeverSends(t *testing.T) {
	p, f := newBondBTCTestProvider(t)
	ctx := context.Background()
	first, reference, err := p.Allocate(ctx, "quote0001")
	if err != nil || first == "" || reference != "quote0001" || p.Currency() != "BTC" {
		t.Fatalf("Allocate: %q %q %v", first, reference, err)
	}
	// The wallet kept a label but the request's caller lost its acknowledgment.
	second, ref2, err := p.Allocate(ctx, "quote0001")
	if err != nil || second != first || ref2 != reference {
		t.Fatalf("recovered: %q %q %v", second, ref2, err)
	}
	third, _, err := p.Allocate(ctx, "quote0002")
	if err != nil || third == first {
		t.Fatalf("distinct quote: %q %v", third, err)
	}
	created := 0
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, method := range f.calls {
		if method == "getnewaddress" {
			created++
		}
		if strings.Contains(method, "send") || strings.Contains(method, "sign") {
			t.Fatalf("bond provider used %s", method)
		}
	}
	if created != 2 {
		t.Fatalf("created %d addresses, want 2", created)
	}
}

func TestVendorBondBTCAllocationLostReplyRecoversAddress(t *testing.T) {
	p, f := newBondBTCTestProvider(t)
	ctx := context.Background()
	f.mu.Lock()
	f.fault = "getnewaddress"
	f.mu.Unlock()
	if address, ref, err := p.Allocate(ctx, "quote0010"); err == nil || address != "" || ref != "" {
		t.Fatalf("failed allocation exposed address: %q %q %v", address, ref, err)
	}
	f.mu.Lock()
	allocated := f.labels["bond:quote0010"]
	f.fault = ""
	f.mu.Unlock()
	address, _, err := p.Allocate(ctx, "quote0010")
	if err != nil || address != allocated {
		t.Fatalf("allocation recovery: %q want %q err=%v", address, allocated, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, method := range f.calls {
		if method == "getnewaddress" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ambiguous allocation created %d addresses", count)
	}
}

func TestVendorBondBTCConfirmedPendingAndWithdrawal(t *testing.T) {
	p, f := newBondBTCTestProvider(t)
	ctx := context.Background()
	address, ref, err := p.Allocate(ctx, "quote0003")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.outputs = []map[string]any{
		bondBTCOutput(address, "settled", "0.000001", 3, true),
		bondBTCOutput(address, "early", "0.000002", 2, true),
		bondBTCOutput(address, "unsafe", "0.000003", 0, false),
	}
	f.mu.Unlock()
	confirmed, pending, err := p.Observe(ctx, address, ref)
	if err != nil || confirmed != 100 || pending != 500 {
		t.Fatalf("totals %d %d %v", confirmed, pending, err)
	}
	// Spent/conflicted outputs disappear from listunspent; never retain historical credit.
	f.mu.Lock()
	f.outputs = []map[string]any{}
	f.mu.Unlock()
	confirmed, pending, err = p.Observe(ctx, address, ref)
	if err != nil || confirmed != 0 || pending != 0 {
		t.Fatalf("withdrawal totals %d %d %v", confirmed, pending, err)
	}
}

func TestVendorBondBTCRefusesUnsafeWalletNetworkAndReplies(t *testing.T) {
	for _, scenario := range []string{"private keys", "network", "syncing", "scanning", "reference", "duplicate", "negative confirmations", "unsafe missing", "amount overflow", "rpc error", "pin changed"} {
		t.Run(scenario, func(t *testing.T) {
			p, f := newBondBTCTestProvider(t)
			ctx := context.Background()
			address, ref, err := p.Allocate(ctx, "quote0004")
			if err != nil {
				t.Fatal(err)
			}
			output := bondBTCOutput(address, "coin", "0.000001", 3, true)
			f.mu.Lock()
			f.outputs = []map[string]any{output}
			switch scenario {
			case "private keys":
				f.private = true
			case "network":
				f.chain = "main"
			case "syncing":
				f.ibd = true
			case "scanning":
				f.scanning = true
			case "reference":
				ref = "quote9999"
			case "duplicate":
				f.outputs = append(f.outputs, output)
			case "negative confirmations":
				output["confirmations"] = -1
			case "unsafe missing":
				delete(output, "safe")
			case "amount overflow":
				output["amount"] = json.Number("92233720368.54775808")
			case "rpc error":
				f.fault = "listunspent"
			case "pin changed":
				p.pin = func(context.Context) error { return fmt.Errorf("wallet identity changed") }
			}
			f.mu.Unlock()
			confirmed, pending, err := p.Observe(ctx, address, ref)
			if err == nil || confirmed != 0 || pending != 0 {
				t.Fatalf("accepted %s: %d %d %v", scenario, confirmed, pending, err)
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "descriptor and") {
				t.Fatalf("unsanitized error: %v", err)
			}
		})
	}
}

func TestVendorBondBTCIdentityMarkerCannotBeReplaced(t *testing.T) {
	p, f := newBondBTCTestProvider(t)
	ctx := context.Background()
	if err := p.check(ctx); err != nil {
		t.Fatal(err)
	}
	var err error
	p.identityAddress, err = p.labelAddress(ctx, vendorBondBTCIdentityLabel, true)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.labels[vendorBondBTCIdentityLabel] = regtestAddr("different wallet")
	f.mu.Unlock()
	if _, _, err := p.Allocate(ctx, "quote0005"); err == nil || !strings.Contains(err.Error(), "identity-mismatch") {
		t.Fatalf("wallet swap: %v", err)
	}
}

type bondXMRTestFixture struct {
	mu                                     sync.Mutex
	p                                      *xmrBondProvider
	wallet, daemon                         *fakeXMRNode
	primary, address, txid, keyImage       string
	account                                int64
	created                                int
	confirmations                          int64
	unlocked, locked, spent, pool, missing bool
	balance                                int64
}

func (f *bondXMRTestFixture) set(node *fakeXMRNode, method string, reply func(json.RawMessage) (any, int, string)) {
	node.set(method, func(params json.RawMessage) (any, int, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return reply(params)
	})
}

func newBondXMRTestProvider(t *testing.T) *bondXMRTestFixture {
	t.Helper()
	f := &bondXMRTestFixture{wallet: newFakeXMRNode(t), daemon: newFakeXMRNode(t), primary: testXMRAddr("bond wallet", false),
		address: testXMRAddr("bond account", true), txid: hex64("bond transaction"), keyImage: hex64("bond output"), confirmations: 10, unlocked: true, balance: 700}
	w, err := newRPCClient("monero vendor bond wallet", f.wallet.URL, "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	d, err := newRPCClient("monero vendor bond daemon", f.daemon.URL, "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	f.p = &xmrBondProvider{rpc: &xmrEscrowRPC{wallet: w, daemon: d}, network: "fakechain", confirmations: 10, pin: func(context.Context) error { return nil }}
	f.daemon.result("get_info", map[string]any{"nettype": "fakechain", "height": 120, "target_height": 0, "synchronized": true, "status": "OK"})
	f.daemon.result("hard_fork_info", map[string]any{"version": 16})
	f.wallet.result("get_height", map[string]any{"height": 120})
	f.set(f.wallet, "get_address", func(json.RawMessage) (any, int, string) {
		return map[string]any{"addresses": []any{map[string]any{"address": f.primary, "address_index": 0}}}, 0, ""
	})
	f.set(f.wallet, "get_accounts", func(json.RawMessage) (any, int, string) {
		accounts := []any{map[string]any{"account_index": 0, "base_address": f.primary, "label": "primary"}}
		if f.account != 0 {
			accounts = append(accounts, map[string]any{"account_index": f.account, "base_address": f.address, "label": "bond:quote1001"})
		}
		return map[string]any{"subaddress_accounts": accounts}, 0, ""
	})
	f.set(f.wallet, "create_account", func(params json.RawMessage) (any, int, string) {
		if string(params) != `{"label":"bond:quote1001"}` {
			t.Errorf("create_account params %s", params)
		}
		f.account, f.created = 1, f.created+1
		return map[string]any{"account_index": f.account, "address": f.address}, 0, ""
	})
	f.set(f.wallet, "get_balance", func(params json.RawMessage) (any, int, string) {
		if string(params) != `{"account_index":1,"address_indices":[0]}` {
			t.Errorf("get_balance params %s", params)
		}
		unlocked := f.balance
		if !f.unlocked {
			unlocked = 0
		}
		return map[string]any{"balance": f.balance, "unlocked_balance": unlocked}, 0, ""
	})
	f.set(f.wallet, "get_transfers", func(params json.RawMessage) (any, int, string) {
		if string(params) != `{"account_index":1,"in":true,"pool":true,"subaddr_indices":[0]}` {
			t.Errorf("get_transfers params %s", params)
		}
		transfer := map[string]any{"txid": f.txid, "type": "in", "amount": 700, "confirmations": f.confirmations, "locked": f.locked,
			"unlock_time": 0, "subaddr_index": map[string]any{"major": 1, "minor": 0}}
		if f.pool {
			transfer["type"] = "pool"
			return map[string]any{"pool": []any{transfer}}, 0, ""
		}
		return map[string]any{"in": []any{transfer}}, 0, ""
	})
	f.set(f.wallet, "incoming_transfers", func(params json.RawMessage) (any, int, string) {
		if string(params) != `{"account_index":1,"transfer_type":"available","verbose":true}` {
			t.Errorf("incoming_transfers params %s", params)
		}
		return map[string]any{"transfers": []any{map[string]any{"amount": 700, "block_height": 111, "key_image": f.keyImage, "tx_hash": f.txid,
			"spent": false, "unlocked": f.unlocked, "frozen": false, "subaddr_index": map[string]any{"major": 1, "minor": 0}}}}, 0, ""
	})
	f.set(f.daemon, "/is_key_image_spent", func(json.RawMessage) (any, int, string) {
		state := 0
		if f.spent {
			state = 1
		}
		return map[string]any{"status": "OK", "spent_status": []int{state}}, 0, ""
	})
	f.set(f.daemon, "/get_transactions", func(json.RawMessage) (any, int, string) {
		if f.missing {
			return map[string]any{"status": "OK", "missed_tx": []string{f.txid}}, 0, ""
		}
		return map[string]any{"status": "OK", "txs": []any{map[string]any{"tx_hash": f.txid, "in_pool": f.pool, "block_height": 111,
			"confirmations": f.confirmations, "as_json": `{"rct_signatures":{"type":6}}`}}}, 0, ""
	})
	return f
}

func TestVendorBondXMRAllocationRecoversAndStoresAccount(t *testing.T) {
	f := newBondXMRTestProvider(t)
	ctx := context.Background()
	address, ref, err := f.p.Allocate(ctx, "quote1001")
	if err != nil || address != f.address || ref != "1:quote1001" || f.p.Currency() != "XMR" {
		t.Fatalf("Allocate %q %q %v", address, ref, err)
	}
	// Simulate a failed persistence acknowledgement after create_account succeeded.
	f.wallet.set("store", func(json.RawMessage) (any, int, string) { return nil, -4, "private wallet password" })
	if _, _, err := f.p.Allocate(ctx, "quote1001"); err == nil {
		t.Fatal("failed store exposed an address")
	}
	f.wallet.result("store", map[string]any{})
	address2, ref2, err := f.p.Allocate(ctx, "quote1001")
	f.mu.Lock()
	created := f.created
	f.mu.Unlock()
	if err != nil || address2 != address || ref2 != ref || created != 1 {
		t.Fatalf("recovery %q %q created=%d err=%v", address2, ref2, created, err)
	}
	f.wallet.mu.Lock()
	defer f.wallet.mu.Unlock()
	for _, call := range f.wallet.calls {
		if call.Method == "transfer" || call.Method == "relay_tx" || call.Method == "query_key" {
			t.Fatalf("bond provider used %s", call.Method)
		}
	}
}

func TestVendorBondXMRAllocationLostReplyRecoversAccount(t *testing.T) {
	f := newBondXMRTestProvider(t)
	ctx := context.Background()
	f.set(f.wallet, "create_account", func(json.RawMessage) (any, int, string) {
		f.account, f.created = 1, f.created+1
		return nil, -4, "reply lost after account was created"
	})
	if address, ref, err := f.p.Allocate(ctx, "quote1001"); err == nil || address != "" || ref != "" {
		t.Fatalf("failed allocation exposed account: %q %q %v", address, ref, err)
	}
	address, ref, err := f.p.Allocate(ctx, "quote1001")
	f.mu.Lock()
	created := f.created
	f.mu.Unlock()
	if err != nil || address != f.address || ref != "1:quote1001" || created != 1 {
		t.Fatalf("allocation recovery %q %q created=%d err=%v", address, ref, created, err)
	}
}

func TestVendorBondXMRConfirmedLockedReorgAndSpent(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		confirmed, pending int64
	}{
		{"confirmed", 700, 0}, {"too few confirmations", 0, 700}, {"locked", 0, 700}, {"timelocked", 0, 700},
		{"spent", 0, 0}, {"missing after reorg", 0, 0}, {"pool", 0, 700}, {"withdrawal", 200, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newBondXMRTestProvider(t)
			ctx := context.Background()
			address, ref, err := f.p.Allocate(ctx, "quote1001")
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			switch scenario.name {
			case "too few confirmations":
				f.confirmations = 9
			case "locked":
				f.unlocked = false
			case "timelocked":
				f.locked = true
			case "spent":
				f.spent = true
			case "missing after reorg":
				f.missing = true
			case "pool":
				f.pool = true
			case "withdrawal":
				f.balance = 200
			}
			f.mu.Unlock()
			confirmed, pending, err := f.p.Observe(ctx, address, ref)
			if err != nil || confirmed != scenario.confirmed || pending != scenario.pending {
				t.Fatalf("totals %d %d %v, want %d %d", confirmed, pending, err, scenario.confirmed, scenario.pending)
			}
		})
	}
}

func TestVendorBondXMRRefusesWalletSwapsLagAndInvalidReplies(t *testing.T) {
	for _, scenario := range []string{"wallet swap", "wallet lag", "daemon network", "view only", "zero key image", "wrong reference", "RPC error", "missing amount"} {
		t.Run(scenario, func(t *testing.T) {
			f := newBondXMRTestProvider(t)
			ctx := context.Background()
			address, ref, err := f.p.Allocate(ctx, "quote1001")
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			switch scenario {
			case "wallet swap":
				f.primary = testXMRAddr("swapped wallet", false)
			case "wallet lag":
				f.wallet.result("get_height", map[string]any{"height": 119})
			case "daemon network":
				f.daemon.result("get_info", map[string]any{"nettype": "mainnet", "mainnet": true, "height": 120, "synchronized": true})
			case "view only":
				f.keyImage = ""
			case "zero key image":
				f.keyImage = strings.Repeat("0", 64)
			case "wrong reference":
				ref = "2:quote1001"
			case "RPC error":
				f.wallet.set("get_balance", func(json.RawMessage) (any, int, string) { return nil, -4, "private wallet password" })
			case "missing amount":
				f.wallet.result("get_balance", map[string]any{"unlocked_balance": 700})
			}
			f.mu.Unlock()
			confirmed, pending, err := f.p.Observe(ctx, address, ref)
			if err == nil || confirmed != 0 || pending != 0 {
				t.Fatalf("accepted %s: %d %d %v", scenario, confirmed, pending, err)
			}
			if strings.Contains(err.Error(), "password") {
				t.Fatalf("unsanitized error: %v", err)
			}
		})
	}
}

func TestVendorBondProviderConfigIsExplicitAndSeparate(t *testing.T) {
	a := &App{}
	ctx := context.Background()
	empty := func(string) string { return "" }
	providers, err := a.vendorBondProvidersFromEnv(ctx, empty)
	if err != nil || len(providers) != 0 {
		t.Fatalf("disabled providers %v %v", providers, err)
	}
	for _, env := range []map[string]string{
		{"VENDOR_BOND_BTC_RPC_URL": "http://127.0.0.1"},
		{"VENDOR_BOND_XMR_DAEMON_URL": "http://127.0.0.1"},
		{"VENDOR_BOND_BTC_RPC_URL": "http://127.0.0.1", "VENDOR_BOND_BTC_WALLET": "opsecmkt-arbiter"},
		{"VENDOR_BOND_BTC_RPC_URL": "http://127.0.0.1", "VENDOR_BOND_BTC_WALLET": "bond-watch", "VENDOR_BOND_BTC_CONFIRMATIONS": "0"},
		{"VENDOR_BOND_XMR_WALLET_RPC_URL": "http://127.0.0.1:1234", "ESCROW_XMR_WALLET_RPC_URL": "http://other:password@127.0.0.1:1234", "VENDOR_BOND_XMR_DAEMON_URL": "http://127.0.0.1:1235"},
		{"VENDOR_BOND_XMR_WALLET_RPC_URL": "http://127.0.0.1:1234", "VENDOR_BOND_XMR_DAEMON_URL": "http://127.0.0.1:1235", "VENDOR_BOND_XMR_CONFIRMATIONS": "9"},
	} {
		if _, err := a.vendorBondProvidersFromEnv(ctx, func(key string) string { return env[key] }); err == nil {
			t.Fatalf("accepted config %v", env)
		}
	}
}

func TestVendorBondProviderWalletPinsSurviveRestart(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	_, btc := newBondBTCTestProvider(t)
	xmr := newBondXMRTestProvider(t)
	env := map[string]string{
		"VENDOR_BOND_BTC_RPC_URL": btc.URL, "VENDOR_BOND_BTC_WALLET": "bond-watch", "BITCOIN_CHAIN": "regtest",
		"VENDOR_BOND_XMR_WALLET_RPC_URL": xmr.wallet.URL, "VENDOR_BOND_XMR_DAEMON_URL": xmr.daemon.URL, "MONERO_NETWORK": "fakechain",
	}
	getenv := func(key string) string { return env[key] }
	providers, err := e.A.vendorBondProvidersFromEnv(ctx, getenv)
	if err != nil || len(providers) != 2 {
		t.Fatalf("configure: %v %v", providers, err)
	}
	if agStr(e, "SELECT value FROM settings WHERE key=$1", vendorBondBTCNetworkSetting) != "regtest" ||
		agStr(e, "SELECT value FROM settings WHERE key=$1", vendorBondXMRNetworkSetting) != "fakechain" {
		t.Fatal("bond networks were not pinned")
	}
	if _, err = e.A.vendorBondProvidersFromEnv(ctx, getenv); err != nil {
		t.Fatalf("unchanged restart: %v", err)
	}
	// A new descriptor wallet carrying the same configured walletname cannot
	// substitute another identity, even before any vendor bond has been paid.
	btc.mu.Lock()
	oldMarker := btc.labels[vendorBondBTCIdentityLabel]
	btc.labels[vendorBondBTCIdentityLabel] = regtestAddr("swapped bond watch wallet")
	btc.mu.Unlock()
	replaced, err := e.A.vendorBondProvidersFromEnv(ctx, getenv)
	if err != nil {
		t.Fatalf("wallet mismatch should pause bonds without taking the market offline: %v", err)
	}
	if address, _, err := replaced["BTC"].Allocate(ctx, "quote1002"); err == nil || address != "" {
		t.Fatal("same-name Bitcoin wallet replacement passed its permanent pin")
	}
	btc.mu.Lock()
	btc.labels[vendorBondBTCIdentityLabel] = oldMarker
	btc.mu.Unlock()
	if address, _, err := replaced["BTC"].Allocate(ctx, "quote1002"); err != nil || address == "" {
		t.Fatalf("restored original Bitcoin wallet did not recover without restart: %q %v", address, err)
	}
	xmr.mu.Lock()
	xmr.primary = testXMRAddr("swapped bond full wallet", false)
	xmr.mu.Unlock()
	replaced, err = e.A.vendorBondProvidersFromEnv(ctx, getenv)
	if err != nil {
		t.Fatalf("Monero wallet mismatch should pause bonds only: %v", err)
	}
	if address, _, err := replaced["XMR"].Allocate(ctx, "quote1001"); err == nil || address != "" {
		t.Fatal("Monero wallet replacement passed its permanent pin")
	}
	if _, err = e.A.vendorBondProvidersFromEnv(ctx, func(key string) string {
		if strings.HasPrefix(key, "VENDOR_BOND_") {
			return ""
		}
		return env[key]
	}); err != nil {
		t.Fatalf("disabled providers must preserve pins without RPC: %v", err)
	}
}

func TestVendorBondRPCEndpointComparison(t *testing.T) {
	for _, pair := range [][2]string{
		{"http://HOST.example", "http://host.example:80/"},
		{"https://user:password@host.example:443/rpc/", "https://other:secret@HOST.example/rpc"},
	} {
		if !sameBondRPCEndpoint(pair[0], pair[1]) {
			t.Fatalf("endpoint aliases differ: %v", pair)
		}
	}
	if sameBondRPCEndpoint("http://host:18082", "http://host:18083") {
		t.Fatal("separate services compare equal")
	}
}

func TestVendorBondProviderWalletQueueRespectsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	btc := &btcBondProvider{}
	xmr := &xmrBondProvider{}
	if err := btc.mu.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer btc.mu.unlock()
	if err := xmr.mu.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer xmr.mu.unlock()
	cancel()
	for _, p := range []bondProvider{btc, xmr} {
		result := make(chan error, 1)
		go func() { _, _, err := p.Allocate(ctx, "quote0001"); result <- err }()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled queue: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("provider kept a canceled request queued behind another wallet call")
		}
	}
}

func TestVendorBondProvidersRecoverFromStartupWalletOutage(t *testing.T) {
	e := newTestApp(t)
	ctx := context.Background()
	_, btc := newBondBTCTestProvider(t)
	xmr := newBondXMRTestProvider(t)
	btc.mu.Lock()
	btc.fault = "getwalletinfo"
	btc.mu.Unlock()
	xmr.wallet.set("refresh", func(json.RawMessage) (any, int, string) { return nil, -13, "No wallet open" })
	env := map[string]string{
		"VENDOR_BOND_BTC_RPC_URL": btc.URL, "VENDOR_BOND_BTC_WALLET": "bond-watch", "BITCOIN_CHAIN": "regtest",
		"VENDOR_BOND_XMR_WALLET_RPC_URL": xmr.wallet.URL, "VENDOR_BOND_XMR_DAEMON_URL": xmr.daemon.URL, "MONERO_NETWORK": "fakechain",
	}
	providers, err := e.A.vendorBondProvidersFromEnv(ctx, func(key string) string { return env[key] })
	if err != nil || len(providers) != 2 {
		t.Fatalf("valid configured outage stopped startup: %v %v", providers, err)
	}
	if agInt(e, "SELECT count(*) FROM settings WHERE key IN ($1,$2)", vendorBondBTCIdentitySetting, vendorBondXMRIdentitySetting) != 0 {
		t.Fatal("unverified wallets were pinned")
	}
	for _, p := range providers {
		if address, ref, err := p.Allocate(ctx, "quote1001"); err == nil || address != "" || ref != "" {
			t.Fatalf("unavailable %s wallet exposed instructions: %q %q %v", p.Currency(), address, ref, err)
		}
	}
	btc.mu.Lock()
	btc.fault = ""
	btc.mu.Unlock()
	xmr.wallet.result("refresh", map[string]any{})
	for _, coin := range []string{"BTC", "XMR"} {
		p := providers[coin]
		address, ref, err := p.Allocate(ctx, "quote1001")
		if err != nil || address == "" || ref == "" {
			t.Fatalf("%s did not recover: %q %q %v", coin, address, ref, err)
		}
		if _, _, err = p.Observe(ctx, address, ref); err != nil {
			t.Fatalf("recovered %s observation: %v", coin, err)
		}
	}
	if agInt(e, "SELECT count(*) FROM settings WHERE key IN ($1,$2,$3,$4)", vendorBondBTCIdentitySetting, vendorBondXMRIdentitySetting, vendorBondBTCNetworkSetting, vendorBondXMRNetworkSetting) != 4 {
		t.Fatal("recovered wallets lack permanent identity/network pins")
	}
}

func TestVendorBondUninitializedProvidersNeverExposeFunds(t *testing.T) {
	btc, _ := newBondBTCTestProvider(t)
	xmr := newBondXMRTestProvider(t)
	btc.pin, xmr.p.pin = nil, nil
	ctx := context.Background()
	for _, p := range []bondProvider{btc, xmr.p} {
		if address, ref, err := p.Allocate(ctx, "quote1001"); err == nil || address != "" || ref != "" {
			t.Fatalf("unchecked %s allocation: %q %q %v", p.Currency(), address, ref, err)
		}
		ref := "quote1001"
		if p.Currency() == "XMR" {
			ref = "1:quote1001"
		}
		if confirmed, pending, err := p.Observe(ctx, "unused", ref); err == nil || confirmed != 0 || pending != 0 {
			t.Fatalf("unchecked %s observation: %d %d %v", p.Currency(), confirmed, pending, err)
		}
	}
}
