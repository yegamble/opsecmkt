package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testEscrowAdapter(t *testing.T) (*escrowBTC, *fakeEscrowCore, *payRPCServer) {
	t.Helper()
	core, srv := newFakeEscrowCore(t)
	e, err := newEscrowBTC(escrowConfig{on: true, rpcURL: srv.url(""), arbiterWallet: "opsecmkt-arbiter"})
	if err != nil {
		t.Fatal(err)
	}
	return e, core, srv
}

// lastCall is the path and exact JSON params of the last call of method.
func (c *fakeEscrowCore) lastCall(t *testing.T, method string) (string, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if c.calls[i].Method == method {
			return c.calls[i].Path, string(c.calls[i].Params)
		}
	}
	t.Fatalf("%s was not called", method)
	return "", ""
}

// TestEscrowRPCPinsParams pins the path and exact JSON-RPC params of every escrow adapter method (A-246 gate: "exact
// params, sighash pinned"): wallet calls go to the arbiter wallet only, the market signs with "ALL" only, fee rates
// are explicit, and createpsbt does no coin selection.
func TestEscrowRPCPinsParams(t *testing.T) {
	ctx := context.Background()
	e, core, _ := testEscrowAdapter(t)
	const arb = "/wallet/opsecmkt-arbiter"
	psbt := testCorePSBTUnsigned
	steps := []struct {
		method, path, params string
		call                 func() error
	}{
		{"getblockchaininfo", "/", `[]`, func() error { _, err := e.chain(ctx); return err }},
		{"getrawchangeaddress", arb, `["bech32"]`, func() error { return e.readArbiter(ctx) }},
		{"getaddressinfo", arb, `["bcrt1qarbiterchange000000000000000000000"]`, func() error { return e.readArbiter(ctx) }},
		{"createpsbt", "/", `[[{"txid":"` + testCorePSBTTxID + `","vout":1}],[{"bcrt1qdest":0.00099},{"bcrt1qfee":0.00000546}],0,true]`, func() error {
			_, err := e.createPSBT(ctx, []escrowOutpoint{{testCorePSBTTxID, 1}}, []escrowPayment{{"bcrt1qdest", 99000}, {"bcrt1qfee", 546}})
			return err
		}},
		{"utxoupdatepsbt", "/", `["` + psbt + `",["wsh(sortedmulti(2,a,b,c))#x"]]`, func() error {
			_, err := e.utxoUpdatePSBT(ctx, psbt, []string{"wsh(sortedmulti(2,a,b,c))#x"})
			return err
		}},
		{"importdescriptors", "/wallet/opsecmkt-escrow", `[[{"active":false,"desc":"wsh(x)#y","label":"escrow","timestamp":"now"}]]`, func() error {
			return e.importDescriptors(ctx, "opsecmkt-escrow", []escrowImport{{Desc: "wsh(x)#y", Timestamp: "now", Label: "escrow"}})
		}},
		{"getdescriptorinfo", "/", `["wsh(x)"]`, func() error { _, err := e.descriptorInfo(ctx, "wsh(x)"); return err }},
		{"deriveaddresses", "/", `["wsh(x)#y"]`, func() error { _, err := e.deriveAddresses(ctx, "wsh(x)#y", -1); return err }},
		{"deriveaddresses", "/", `["wpkh(x/0/*)#y",[7,7]]`, func() error { _, err := e.deriveAddresses(ctx, "wpkh(x/0/*)#y", 7); return err }},
		{"gettxout", "/", `["` + testCorePSBTTxID + `",3,true]`, func() error { _, _, _, err := e.txOut(ctx, testCorePSBTTxID, 3); return err }},
		{"keypoolrefill", arb, `[6000]`, func() error { return e.keypoolRefill(ctx, 6000) }},
		{"walletprocesspsbt", arb, `["` + psbt + `",true,"ALL",true,false]`, func() error { _, err := e.arbiterSign(ctx, psbt); return err }},
		{"descriptorprocesspsbt", "/", `["` + psbt + `",["wsh(tprvX)"],"ALL",true,false]`, func() error { _, err := e.throwawaySign(ctx, psbt, "wsh(tprvX)"); return err }},
		{"combinepsbt", "/", `[["m","p"]]`, func() error { _, err := e.combinePSBT(ctx, []string{"m", "p"}); return err }},
		{"finalizepsbt", "/", `["` + psbt + `",true]`, func() error { _, _, err := e.finalizePSBT(ctx, psbt); return err }},
		{"testmempoolaccept", "/", `[["0200"],0.001]`, func() error { _, _, err := e.testMempoolAccept(ctx, "0200", 100); return err }},
		{"sendrawtransaction", "/", `["0200",0.00025]`, func() error { _, err := e.sendRawTransaction(ctx, "0200", 25); return err }},
	}
	for _, s := range steps {
		if err := s.call(); err != nil && s.method != "descriptorprocesspsbt" && s.method != "finalizepsbt" {
			t.Fatalf("%s: %v", s.method, err)
		}
		path, params := core.lastCall(t, s.method)
		if path != s.path || params != s.params {
			t.Errorf("%s: path %q params %s\nwant path %q params %s", s.method, path, params, s.path, s.params)
		}
	}
	// The adapter's method list is the documented rpcwhitelist line, and it never includes a key-export method.
	called := map[string]bool{}
	for _, m := range core.methods() {
		called[m] = true
	}
	for _, m := range escrowRPCMethods {
		if !called[m] {
			t.Errorf("%s is on the whitelist list but no adapter method calls it", m)
		}
	}
	for m := range called {
		if !strings.Contains(" "+strings.Join(escrowRPCMethods, " ")+" ", " "+m+" ") {
			t.Errorf("%s is called but missing from escrowRPCMethods", m)
		}
	}
	for _, forbidden := range []string{"listdescriptors", "dumpprivkey", "dumpwallet", "backupwallet", "signrawtransactionwithwallet", "walletpassphrase", "sendtoaddress"} {
		if called[forbidden] || strings.Contains(strings.Join(escrowRPCMethods, ","), forbidden) {
			t.Errorf("%s must never be called or whitelisted", forbidden)
		}
	}
	if e.fingerprint != fakeArbiterFP || e.arbiterKey.PathText != "/84h/1h/0h" {
		t.Fatalf("arbiter %q %+v", e.fingerprint, e.arbiterKey)
	}
}

func TestEscrowRPCResults(t *testing.T) {
	ctx := context.Background()
	e, core, _ := testEscrowAdapter(t)
	if found, _, _, err := e.txOut(ctx, testCorePSBTTxID, 0); err != nil || found {
		t.Fatalf("gettxout null: %v %v", found, err)
	}
	core.prevoutExists = true
	if found, confs, sats, err := e.txOut(ctx, testCorePSBTTxID, 0); err != nil || !found || confs != 1 || sats != 100000 {
		t.Fatalf("gettxout: %v %d %d %v", found, confs, sats, err)
	}
	if allowed, reject, err := e.testMempoolAccept(ctx, "00", 10); err != nil || allowed || reject != "missing-inputs" {
		t.Fatalf("testmempoolaccept: %v %q %v", allowed, reject, err)
	}
	// The arbiter's parent descriptor must parse as a key with getaddressinfo's fingerprint.
	core.arbiterFP = "00000000"
	if err := e.readArbiter(ctx); err == nil {
		t.Fatal("accepted a parent descriptor whose fingerprint differs from hdmasterfingerprint")
	}
	core.arbiterFP, core.arbiterDesc = fakeArbiterFP, "wpkh(["+fakeArbiterFP+"/84h/1h/0h]"+testPrivDescriptor[5:20]+"/1/*)"
	if err := e.readArbiter(ctx); err == nil {
		t.Fatal("accepted an unparsable parent descriptor")
	}
}

// TestEscrowRPCErrorClasses: every failure becomes a fixed class that never carries Core's message (A-170, ES-10).
func TestEscrowRPCErrorClasses(t *testing.T) {
	ctx := context.Background()
	secret := "key 'tprv8ZgxMBicQKsPfPfXCY5DfrTFb4fTTnkGWSFHAxyYRduJfSPA4GyoTNszFsrDH4aur5ufqBJFghyp7JR8YFrsykLUcQbCVQ5GuWX1GnFKdDv' is not valid"
	rpcErr := newPayRPCServer(t, "u", "p", false, func(path, method string, params json.RawMessage) (any, int, string) {
		if method == "getdescriptorinfo" {
			return nil, -5, secret
		}
		return "not an object", 0, ""
	})
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(forbidden.Close)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(func() { hang.CloseClientConnections(); hang.Close() })
	down := newPayRPCServer(t, "u", "p", false, nil)
	down.down.Store(true)
	old := rpcTimeout
	rpcTimeout = 200 * time.Millisecond
	t.Cleanup(func() { rpcTimeout = old })
	cases := []struct {
		name, url, class string
		call             func(e *escrowBTC) error
	}{
		{"rpc error", rpcErr.url(""), "rpc:-5", func(e *escrowBTC) error { _, err := e.descriptorInfo(ctx, "x"); return err }},
		{"bad reply", rpcErr.url(""), "bad-reply", func(e *escrowBTC) error { _, _, err := e.finalizePSBT(ctx, "x"); return err }},
		{"wrong password", strings.Replace(rpcErr.url(""), ":p@", ":wrong@", 1), "credentials", func(e *escrowBTC) error { _, err := e.chain(ctx); return err }},
		{"not whitelisted", "http://u:p@" + strings.TrimPrefix(forbidden.URL, "http://"), "forbidden", func(e *escrowBTC) error { _, err := e.arbiterSign(ctx, "x"); return err }},
		{"timeout", "http://u:p@" + strings.TrimPrefix(hang.URL, "http://"), "timeout", func(e *escrowBTC) error { _, err := e.chain(ctx); return err }},
		{"unreachable", down.url(""), "unreachable", func(e *escrowBTC) error { _, err := e.chain(ctx); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := newEscrowBTC(escrowConfig{on: true, rpcURL: c.url, arbiterWallet: "arb"})
			if err != nil {
				t.Fatal(err)
			}
			err = c.call(e)
			var re *escrowRPCError
			if !errors.As(err, &re) || re.Class != c.class {
				t.Fatalf("got %v, want class %s", err, c.class)
			}
			if strings.Contains(err.Error(), "tprv") || strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error carries node text: %q", err)
			}
		})
	}
}

func TestEscrowConfigFromEnv(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, off := range []func(string) string{env(), env("ESCROW_BTC", "off"), env("ESCROW_BTC", " ")} {
		if c, err := escrowConfigFromEnv(off); err != nil || c.on {
			t.Fatalf("off: %+v %v", c, err)
		}
	}
	c, err := escrowConfigFromEnv(env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@127.0.0.1:18443"))
	if err != nil || !c.on || c.arbiterWallet != "opsecmkt-arbiter" {
		t.Fatalf("on: %+v %v", c, err)
	}
	for name, getenv := range map[string]func(string) string{
		"bad value":           env("ESCROW_BTC", "yes", "ESCROW_BTC_RPC_URL", "http://a:b@h:1"),
		"no URL":              env("ESCROW_BTC", "on"),
		"testnet4 chain":      env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@h:1", "BITCOIN_CHAIN", "testnet4"),
		"mainnet chain":       env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@h:1", "BITCOIN_CHAIN", "main"),
		"pooled wallet":       env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@h:1", "BITCOIN_RPC_URL", "http://x:y@h:2", "ESCROW_BTC_ARBITER_WALLET", "opsecmkt"),
		"named pooled wallet": env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@h:1", "BITCOIN_RPC_URL", "http://x:y@h:2", "BITCOIN_WALLET", "w", "ESCROW_BTC_ARBITER_WALLET", "w"),
	} {
		if _, err := escrowConfigFromEnv(getenv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := escrowConfigFromEnv(env("ESCROW_BTC", "on", "ESCROW_BTC_RPC_URL", "http://a:b@h:1", "BITCOIN_CHAIN", "regtest")); err != nil {
		t.Fatalf("regtest chain: %v", err)
	}
}

// The rpcwhitelist line the operator guide and scripts/test-escrow-regtest.sh give is exactly the adapter's methods.
func TestEscrowWhitelistDocumented(t *testing.T) {
	want := "rpcwhitelist=opsecmkt-escrow:" + strings.Join(escrowRPCMethods, ",")
	for _, f := range []string{"docs/operator-guide.md", "scripts/test-escrow-regtest.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "rpcwhitelist=opsecmkt-escrow:"); n != 1 || !strings.Contains(string(b), want+"\n") {
			t.Errorf("%s: want exactly one line %q (found %d)", f, want, n)
		}
	}
}
