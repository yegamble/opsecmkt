package market

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A-440 (P4-R6): the market key protection self-check. Through the pooled Bitcoin client (BITCOIN_RPC_URL) escrow
// calls listdescriptors (private=false) on the arbiter wallet's path; only HTTP 403 (the method is not on the pooled
// user's rpcwhitelist) or wallet-not-found (RPC -18: the pooled node is another node) lets escrow start. Against the
// fake escrow Core and a fake pooled Core (httptest; simulated RPC, not chain evidence: scripts/test-escrow-regtest.sh
// has the real regtest legs).

// pooledCore is a fake pooled Bitcoin Core: enough for the pooled provider's startup check, and listdescriptors
// answered as reply (a string) says: "forbidden" (HTTP 403, as Core's rpcwhitelist refuses), "notfound", "answered",
// "error" or "slow" (one second: longer than the rpcTimeout the timeout case sets).
func pooledCore(t *testing.T, reply *atomic.Value) *payRPCServer {
	t.Helper()
	srv := newPayRPCServer(t, "pooled", "pooled-pass", false, func(path, method string, params json.RawMessage) (any, int, string) {
		switch method {
		case "getblockchaininfo":
			return map[string]any{"chain": "regtest", "blocks": 200, "headers": 200, "initialblockdownload": false}, 0, ""
		case "getwalletinfo":
			return map[string]any{"walletname": "opsecmkt"}, 0, ""
		case "getnetworkinfo":
			return map[string]any{"version": 310100, "connections": 0}, 0, ""
		case "listdescriptors":
			switch r, _ := reply.Load().(string); r {
			case "notfound":
				return nil, -18, "Requested wallet does not exist or is not loaded"
			case "error":
				return nil, -4, "core-said-secret wpkh(tpubCORETEXT/0/*)"
			case "slow":
				time.Sleep(time.Second)
			}
			return map[string]any{"wallet_name": "opsecmkt-arbiter", "descriptors": []any{map[string]any{"desc": "wpkh([d34db33f/84h/1h/0h]tpubPOOLEDLEAK/0/*)#leakleak"}}}, 0, ""
		}
		return nil, -32601, "Method not found"
	})
	srv.mu.Lock()
	srv.forbid = func(method string) bool {
		r, _ := reply.Load().(string)
		return method == "listdescriptors" && r == "forbidden"
	}
	srv.mu.Unlock()
	return srv
}

func TestEscrowPooledKeyCheck(t *testing.T) {
	x := newEscrowEnv(t) // BITCOIN_RPC_URL blank: no pooled client, nothing to check, escrow on
	var reply atomic.Value
	reply.Store("")
	pooled := pooledCore(t, &reply)
	for _, tc := range []struct {
		name, reply string
		badPass     bool
		pass        bool
		class       string // the error class the disabled state is logged with
	}{
		{"refused 403", "forbidden", false, true, ""},
		{"wallet not found -18", "notfound", false, true, ""},
		{"answered", "answered", false, false, "listdescriptors answered"},
		{"another error", "error", false, false, "listdescriptors rpc:-4"},
		{"credentials rejected 401", "forbidden", true, false, "listdescriptors credentials"},
		{"timeout", "slow", false, false, "listdescriptors timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply.Store(tc.reply)
			url := pooled.url("")
			if tc.badPass {
				url = strings.Replace(url, ":pooled-pass@", ":wrong-pass@", 1)
			}
			t.Setenv("BITCOIN_RPC_URL", url)
			if tc.reply == "slow" {
				old := rpcTimeout
				rpcTimeout = 300 * time.Millisecond
				t.Cleanup(func() { rpcTimeout = old })
			}
			before := pooled.called("listdescriptors")
			logs := captureLog(t)
			x.restart()
			if n := pooled.called("listdescriptors"); n != before+1 && !tc.badPass {
				t.Fatalf("listdescriptors through the pooled client called %d times, want 1", n-before)
			}
			if !tc.badPass {
				if got := string(pooled.lastParams("listdescriptors")); got != "[false]" {
					t.Fatalf("listdescriptors params %s, want [false] (never private=true)", got)
				}
			}
			page := x.adminPage()
			if tc.pass {
				if x.A.escrow.Load() == nil || strings.Contains(logs.String(), escrowLogPooledKey) || strings.Contains(page, `id="escrow-node"`) {
					t.Fatalf("escrow on %v\nlog:\n%s", x.A.escrow.Load() != nil, logs.String())
				}
				return
			}
			if x.A.escrow.Load() != nil || !x.A.escrowConfigured {
				t.Fatal("escrow enabled although the pooled user was not refused listdescriptors on the arbiter wallet")
			}
			if strings.Count(logs.String(), escrowLogPooledKey+" ("+tc.class+")\n") != 1 {
				t.Fatalf("want one line %q\nlog:\n%s", escrowLogPooledKey+" ("+tc.class+")", logs.String())
			}
			for _, leak := range []string{"tpub", "core-said-secret", "pooled-pass", "wrong-pass"} {
				if strings.Contains(logs.String(), "escrow: ") && strings.Contains(escrowLines(logs.String()), leak) {
					t.Fatalf("an escrow log line holds %q:\n%s", leak, escrowLines(logs.String()))
				}
				if strings.Contains(page, leak) {
					t.Fatalf("the admin page holds %q", leak)
				}
			}
			want := "Bitcoin escrow disabled: the pooled Bitcoin RPC user (BITCOIN_RPC_URL) was not refused listdescriptors on the arbiter wallet (" + strings.TrimPrefix(tc.class, "listdescriptors ") + ")."
			if !strings.Contains(page, want) || !strings.Contains(page, "rpcwhitelist="+"USER:"+strings.Join(pooledRPCMethods, ",")) {
				t.Fatalf("admin page lacks %q and the whitelist line:\n%s", want, page)
			}
		})
	}

	// Re-checked like the other escrow checks: still answered, a retry keeps escrow off without another line; once
	// the operator whitelists the pooled user (403), the next retry starts escrow and the admin line goes.
	reply.Store("answered")
	t.Setenv("BITCOIN_RPC_URL", pooled.url(""))
	logs := captureLog(t)
	x.restart()
	retry := &escrowRetry{last: x.A.escrowStartState}
	x.A.retryEscrow(context.Background(), retry)
	if x.A.escrow.Load() != nil || strings.Count(logs.String(), escrowLogPooledKey) != 1 {
		t.Fatalf("retry while answered: on %v\n%s", x.A.escrow.Load() != nil, logs.String())
	}
	reply.Store("forbidden")
	x.A.retryEscrow(context.Background(), retry)
	if x.A.escrow.Load() == nil || !strings.Contains(logs.String(), escrowLogOn) {
		t.Fatalf("escrow did not start once the pooled user was refused:\n%s", logs.String())
	}
	if page := x.adminPage(); strings.Contains(page, "was not refused listdescriptors") {
		t.Fatal("the admin line outlived the exposure")
	}
}

// escrowLines is the escrow: lines of a log.
func escrowLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "escrow: ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// pooledRPCMethods is every method the pooled Bitcoin adapter (payments_bitcoin.go) calls: each provider method run
// against a fake Core calls exactly the list, never a key-export method, and the operator guide and the regtest script
// document the list as the pooled user's rpcwhitelist line.
func TestBitcoinPooledRPCMethods(t *testing.T) {
	var loaded atomic.Bool
	srv := newPayRPCServer(t, "pooled", "pooled-pass", false, func(path, method string, params json.RawMessage) (any, int, string) {
		switch method {
		case "getblockchaininfo":
			return map[string]any{"chain": "regtest", "blocks": 200, "headers": 200}, 0, ""
		case "getwalletinfo":
			if !loaded.Load() {
				return nil, -18, "Requested wallet does not exist or is not loaded"
			}
			return map[string]any{"walletname": "opsecmkt"}, 0, ""
		case "loadwallet":
			loaded.Store(true)
			return map[string]any{"name": "opsecmkt"}, 0, ""
		case "getnetworkinfo":
			return map[string]any{"version": 310100, "connections": 0}, 0, ""
		case "getnewaddress":
			return "bcrt1qpooledaddress000000000000000000000", 0, ""
		case "listreceivedbyaddress":
			return []any{map[string]any{"address": "bcrt1qpooledaddress000000000000000000000", "txids": []string{strings.Repeat("ab", 32)}}}, 0, ""
		case "gettransaction":
			return map[string]any{"confirmations": 1, "details": []any{map[string]any{"address": "bcrt1qpooledaddress000000000000000000000", "category": "receive", "amount": json.Number("0.001"), "vout": 0}}}, 0, ""
		case "sendtoaddress":
			return strings.Repeat("cd", 32), 0, ""
		case "validateaddress":
			return map[string]any{"isvalid": true}, 0, ""
		}
		return nil, -32601, "Method not found"
	})
	ctx := context.Background()
	p, err := newBitcoinProvider(ctx, srv.url(""), "opsecmkt", "regtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.NewAddress(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if in, err := p.Incoming(ctx, []string{"bcrt1qpooledaddress000000000000000000000"}); err != nil || len(in) != 1 {
		t.Fatalf("incoming %v %v", in, err)
	}
	if _, err := p.Send(ctx, "bcrt1qpooledaddress000000000000000000000", 50000); err != nil {
		t.Fatal(err)
	}
	if !p.ValidAddress("bcrt1qpooledaddress000000000000000000000") {
		t.Fatal("validateaddress")
	}
	srv.mu.Lock()
	called := slices.Clone(srv.calls)
	srv.mu.Unlock()
	for _, m := range pooledRPCMethods {
		if !slices.Contains(called, m) {
			t.Errorf("%s is on the pooled whitelist list but the pooled adapter never calls it", m)
		}
	}
	for _, m := range called {
		if !slices.Contains(pooledRPCMethods, m) {
			t.Errorf("%s is called by the pooled adapter but missing from pooledRPCMethods", m)
		}
	}
	for _, forbidden := range []string{"listdescriptors", "dumpprivkey", "dumpwallet", "backupwallet", "importdescriptors", "walletprocesspsbt", "signrawtransactionwithwallet", "unloadwallet", "createwallet"} {
		if slices.Contains(pooledRPCMethods, forbidden) {
			t.Errorf("%s must never be on the pooled user's whitelist", forbidden)
		}
	}
	want := "rpcwhitelist=opsecmkt-pooled:" + strings.Join(pooledRPCMethods, ",")
	for f, line := range map[string]string{"docs/operator-guide.md": want + "\n", "scripts/test-escrow-regtest.sh": "-" + want + " \\\n"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "rpcwhitelist=opsecmkt-pooled:"); n != 1 || !strings.Contains(string(b), line) {
			t.Errorf("%s: want exactly one line %q (found %d)", f, strings.TrimSpace(line), n)
		}
	}
}
