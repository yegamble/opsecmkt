package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adapter negatives against the scripted nodes (no database): wallet send failures, RPC errors in the middle
// of a wallet read, timeouts, non-JSON / oversized / 503 responses, Digest variants, the Bitcoin "test"
// chain name and monerod's boolean network flags.

type rpcHandler = func(path, method string, params json.RawMessage) (any, int, string)

// rpcOverride sits in front of a scripted node: a method can be made to fail with an RPC error, answered by
// a function, or made to hang until the test ends (as a stuck node does).
type rpcOverride struct {
	mu      sync.Mutex
	errs    map[string]rpcFailure
	funcs   map[string]rpcHandler
	hang    map[string]bool
	release chan struct{}
}

type rpcFailure struct {
	code int
	msg  string
}

func newRPCOverride() *rpcOverride {
	return &rpcOverride{errs: map[string]rpcFailure{}, funcs: map[string]rpcHandler{}, hang: map[string]bool{}, release: make(chan struct{})}
}

// releaseAtEnd must be registered after the server so it runs first (cleanups run last-in first-out):
// a hanging handler would otherwise block httptest.Server.Close.
func (o *rpcOverride) releaseAtEnd(t *testing.T) {
	t.Cleanup(func() { close(o.release) })
}

func (o *rpcOverride) fail(method string, code int, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.errs[method] = rpcFailure{code, msg}
}

func (o *rpcOverride) answer(method string, f rpcHandler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.funcs[method] = f
}

func (o *rpcOverride) stall(method string, on bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hang[method] = on
}

func (o *rpcOverride) clear() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.errs, o.funcs, o.hang = map[string]rpcFailure{}, map[string]rpcHandler{}, map[string]bool{}
}

func (o *rpcOverride) wrap(next rpcHandler) rpcHandler {
	return func(path, method string, params json.RawMessage) (any, int, string) {
		o.mu.Lock()
		f, failing := o.errs[method]
		fn := o.funcs[method]
		hang := o.hang[method]
		o.mu.Unlock()
		if hang {
			<-o.release
			return nil, -1, "released"
		}
		if failing {
			return nil, f.code, f.msg
		}
		if fn != nil {
			return fn(path, method, params)
		}
		return next(path, method, params)
	}
}

// newGapCore is newPayCore with an override in front of the scripted Bitcoin Core.
func newGapCore(t *testing.T, chain string) (*payCore, *rpcOverride, *payRPCServer) {
	c := &payCore{chain: chain, newAddr: "tb1qorderaddress000000000000000000000", walletLoaded: true, txs: map[string]map[string]any{}, invalid: map[string]bool{}}
	o := newRPCOverride()
	s := newPayRPCServer(t, "marketplace", "rpc-secret", false, o.wrap(c.handle))
	o.releaseAtEnd(t)
	return c, o, s
}

// newGapMonero is newPayMonero with an override in front of the scripted monero-wallet-rpc / monerod.
func newGapMonero(t *testing.T) (*payMoneroWallet, *rpcOverride, *payRPCServer) {
	m := &payMoneroWallet{primary: payStagenetPrimary, sub: payStagenetSub, open: true, nettype: "stagenet", valid: map[string]string{}}
	o := newRPCOverride()
	s := newPayRPCServer(t, "wallet", "wallet-secret", true, o.wrap(m.handle))
	o.releaseAtEnd(t)
	return m, o, s
}

// gapReceive makes the scripted Bitcoin wallet report one received output (listreceivedbyaddress + gettransaction).
func (c *payCore) gapReceive(addr, txid string, vout int, sats int64, confs int) {
	c.set(func(c *payCore) {
		found := false
		for _, r := range c.received {
			if r["address"] == addr {
				r["txids"] = append(r["txids"].([]string), txid)
				found = true
			}
		}
		if !found {
			c.received = append(c.received, map[string]any{"address": addr, "txids": []string{txid}})
		}
		c.txs[txid] = map[string]any{"confirmations": confs, "details": []map[string]any{
			{"address": addr, "category": "receive", "amount": json.Number(amount(sats, 8)), "vout": vout},
		}}
	})
}

func TestGapAdapterSendFailuresNeverReturnATxid(t *testing.T) {
	ctx := context.Background()
	_, bov, bs := newGapCore(t, "testnet4")
	bp, err := newBitcoinProvider(ctx, bs.url(""), "opsecmkt", "testnet4", 1)
	if err != nil {
		t.Fatal(err)
	}
	to := "tb1qvendorpayout000000000000000000000"
	for _, f := range []rpcFailure{{-6, "Insufficient funds"}, {-13, "Error: Please enter the wallet passphrase with walletpassphrase first."}} {
		bov.fail("sendtoaddress", f.code, f.msg)
		txid, err := bp.Send(ctx, to, 100000)
		var re *rpcError
		if txid != "" || !errors.As(err, &re) || re.Code != int64(f.code) || !strings.Contains(err.Error(), f.msg) || strings.Contains(err.Error(), "rpc-secret") {
			t.Fatalf("bitcoin send %d: txid=%q err=%v", f.code, txid, err)
		}
	}
	bov.clear()
	bs.down.Store(true) // node gone mid-send: an error, never a txid
	if txid, err := bp.Send(ctx, to, 100000); txid != "" || err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("bitcoin send while unreachable: %q %v", txid, err)
	}

	_, xov, xs := newGapMonero(t)
	xp, err := newMoneroProvider(ctx, xs.url(""), "", "stagenet", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []rpcFailure{{-17, "not enough money"}, {-37, "not enough unlocked money"}} {
		xov.fail("transfer", f.code, f.msg)
		txid, err := xp.Send(ctx, payStagenetSub, 42)
		var re *rpcError
		if txid != "" || !errors.As(err, &re) || re.Code != int64(f.code) || !strings.Contains(err.Error(), f.msg) || strings.Contains(err.Error(), "wallet-secret") {
			t.Fatalf("monero send %d: txid=%q err=%v", f.code, txid, err)
		}
	}
	xov.clear()
	xov.answer("transfer", func(string, string, json.RawMessage) (any, int, string) {
		return map[string]any{"tx_hash": "", "fee": 100}, 0, ""
	})
	if txid, err := xp.Send(ctx, payStagenetSub, 42); txid != "" || err == nil || !strings.Contains(err.Error(), "no transaction hash") {
		t.Fatalf("monero send with empty tx_hash: %q %v", txid, err)
	}
}

func TestGapAdapterIncomingErrorsMidRead(t *testing.T) {
	ctx := context.Background()
	core, bov, bs := newGapCore(t, "testnet4")
	bp, err := newBitcoinProvider(ctx, bs.url(""), "opsecmkt", "testnet4", 1)
	if err != nil {
		t.Fatal(err)
	}
	addr := "tb1qdepositaddress0000000000000000000"
	core.gapReceive(addr, "tx-1", 0, 5000, 1)
	core.gapReceive(addr, "tx-2", 1, 7000, 1)
	bov.fail("listreceivedbyaddress", -4, "Wallet is currently rescanning. Abort existing rescan or wait.")
	if in, err := bp.Incoming(ctx, []string{addr}); in != nil || err == nil || !strings.Contains(err.Error(), "rescanning") {
		t.Fatalf("listreceivedbyaddress failure: %+v %v", in, err)
	}
	bov.clear()
	// gettransaction fails for the second txid after the first succeeded: nothing partial is returned.
	bov.answer("gettransaction", func(path, method string, params json.RawMessage) (any, int, string) {
		if strings.Contains(string(params), "tx-2") {
			return nil, -5, "Invalid or non-wallet transaction id"
		}
		return core.handle(path, method, params)
	})
	if in, err := bp.Incoming(ctx, []string{addr}); in != nil || err == nil || !strings.Contains(err.Error(), "error -5") {
		t.Fatalf("gettransaction failure mid-read: %+v %v", in, err)
	}
	bov.clear()
	if in, err := bp.Incoming(ctx, []string{addr}); err != nil || len(in) != 2 {
		t.Fatalf("recovered read: %+v %v", in, err)
	}

	w, xov, xs := newGapMonero(t)
	xp, err := newMoneroProvider(ctx, xs.url(""), "", "stagenet", 1)
	if err != nil {
		t.Fatal(err)
	}
	w.set(func(m *payMoneroWallet) {
		m.transfers = map[string]any{"in": []map[string]any{{"txid": "x-1", "amount": 5, "confirmations": 2, "subaddr_index": map[string]any{"major": 0, "minor": 7}}}}
	})
	xov.fail("get_transfers", -1, "Failed to get transfers")
	if in, err := xp.Incoming(ctx, []string{payStagenetSub}); in != nil || err == nil || !strings.Contains(err.Error(), "get_transfers") {
		t.Fatalf("get_transfers failure: %+v %v", in, err)
	}
	xov.clear()
	// A fresh provider (after an app restart) has no subaddress index cache. A wallet error while resolving the
	// index is a failed read, not "this address is not ours".
	fresh, err := newMoneroProvider(ctx, xs.url(""), "", "stagenet", 1)
	if err != nil {
		t.Fatal(err)
	}
	xov.fail("get_address_index", -13, "No wallet file")
	if in, err := fresh.Incoming(ctx, []string{payStagenetSub}); in != nil || err == nil || !strings.Contains(err.Error(), "No wallet file") {
		t.Fatalf("get_address_index wallet error treated as a foreign address: %+v %v", in, err)
	}
	// An address that does not belong to the wallet (WRONG_ADDRESS, -2) is skipped without an error.
	xov.clear()
	if in, err := fresh.Incoming(ctx, []string{"7notours"}); in != nil || err != nil {
		t.Fatalf("foreign address: %+v %v", in, err)
	}
	if in, err := fresh.Incoming(ctx, []string{payStagenetSub}); err != nil || len(in) != 1 || in[0].Index != 7 {
		t.Fatalf("recovered monero read: %+v %v", in, err)
	}
}

// rawNode serves a fixed HTTP status, headers and body for every request.
func rawNode(t *testing.T, status int, body string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestGapRPCNonJSONUnavailableAndOversizedResponses(t *testing.T) {
	ctx := context.Background()
	client := func(s *httptest.Server) *rpcClient {
		c, err := newRPCClient("bitcoin", strings.Replace(s.URL, "http://", "http://u:rpc-secret@", 1), "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// A reverse proxy in front of a restarting node answers 503 with an HTML page.
	html := rawNode(t, 503, "<html><body>503 Service Unavailable - upstream internal-node-7 down</body></html>")
	err := client(html).call(ctx, "", "getblockchaininfo", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid response (HTTP 503)") || strings.Contains(err.Error(), "internal-node-7") {
		t.Fatalf("503 HTML: %v", err)
	}
	// A JSON envelope with a non-200 status and no error object is still a failure.
	jsonErr := rawNode(t, 503, `{"result":{"chain":"testnet4"},"error":null,"id":"x"}`)
	var out struct{ Chain string }
	if err = client(jsonErr).call(ctx, "", "getblockchaininfo", nil, &out); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("503 JSON without error: %v", err)
	}
	empty := rawNode(t, 200, "")
	if err = client(empty).call(ctx, "", "getblockchaininfo", nil, &out); err == nil || !strings.Contains(err.Error(), "invalid response (HTTP 200)") {
		t.Fatalf("empty body: %v", err)
	}
	shape := rawNode(t, 200, `{"result":"not-an-object","error":null,"id":"x"}`)
	if err = client(shape).call(ctx, "", "getblockchaininfo", nil, &out); err == nil || !strings.Contains(err.Error(), "unexpected result shape") {
		t.Fatalf("wrong result shape: %v", err)
	}
	// A response over the 16 MiB bound is rejected without decoding it.
	var served atomic.Int64
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("a", 1<<16))
		w.Write([]byte(`{"result":"`))
		for n := 0; n <= rpcMaxBody; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			served.Add(int64(len(chunk)))
		}
		w.Write([]byte(`"}`))
	}))
	t.Cleanup(big.Close)
	var s string
	if err = client(big).call(ctx, "", "getnewaddress", nil, &s); err == nil || !strings.Contains(err.Error(), "response too large") || s != "" {
		t.Fatalf("oversized response: %v", err)
	}
	// Through an adapter the same failures surface as check errors, never as a working provider.
	if _, err = newBitcoinProvider(ctx, strings.Replace(html.URL, "http://", "http://u:p@", 1), "opsecmkt", "", 1); err == nil || isRefusal(err) || !strings.Contains(err.Error(), "bitcoin node check failed") {
		t.Fatalf("503 node accepted: %v", err)
	}
}

func TestGapRPCTimeouts(t *testing.T) {
	_, ov, s := newGapCore(t, "testnet4")
	ov.stall("listreceivedbyaddress", true)
	bp, err := newBitcoinProvider(context.Background(), s.url(""), "opsecmkt", "testnet4", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The caller's deadline (a watcher pass being cancelled) ends the call with "timed out".
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	in, err := bp.Incoming(ctx, []string{"tb1qdepositaddress0000000000000000000"})
	if in != nil || err == nil || !strings.Contains(err.Error(), "bitcoin RPC unreachable: timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("stalled node with caller deadline: %v after %s", err, time.Since(start))
	}
	// The per-call timeout (a node that accepts the connection and never answers) is also an error.
	defer func(d time.Duration) { rpcTimeout = d }(rpcTimeout)
	rpcTimeout = 200 * time.Millisecond
	start = time.Now()
	in, err = bp.Incoming(context.Background(), []string{"tb1qdepositaddress0000000000000000000"})
	if in != nil || err == nil || !strings.Contains(err.Error(), "bitcoin RPC unreachable") || strings.Contains(err.Error(), "rpc-secret") || time.Since(start) > 5*time.Second {
		t.Fatalf("stalled node with client timeout: %v after %s", err, time.Since(start))
	}
	// A node that is down at startup is an error, not a refusal.
	s.down.Store(true)
	if _, err = newBitcoinProvider(context.Background(), s.url(""), "opsecmkt", "", 1); err == nil || isRefusal(err) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("down node: %v", err)
	}
}

// digestNode is a Digest-auth server with a configurable challenge; verify checks the Authorization header.
type digestNode struct {
	*httptest.Server
	challenges []string
	verify     func(p map[string]string, uri string) bool
	requests   atomic.Int64
	lastAuth   atomic.Value
}

func newDigestNode(t *testing.T, challenges []string, verify func(p map[string]string, uri string) bool) *digestNode {
	d := &digestNode{challenges: challenges, verify: verify}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		h := r.Header.Get("Authorization")
		d.lastAuth.Store(h)
		if strings.HasPrefix(h, "Digest ") && d.verify(parseAuthParams(h[7:]), r.URL.RequestURI()) {
			w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"version":65562}}`))
			return
		}
		for _, c := range d.challenges {
			w.Header().Add("WWW-Authenticate", c)
		}
		w.WriteHeader(401)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *digestNode) client(t *testing.T) *rpcClient {
	c, err := newRPCClient("monero wallet", strings.Replace(d.URL, "http://", "http://wallet:digest-pass@", 1), "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGapDigestChallengeVariants(t *testing.T) {
	ctx := context.Background()
	ha1 := md5hex("wallet:monero-rpc:digest-pass")
	// RFC 2069 (no qop): response = MD5(HA1:nonce:HA2), no nc/cnonce, opaque echoed.
	legacy := newDigestNode(t, []string{`Digest realm="monero-rpc", nonce="legacy-nonce", opaque="op-1", algorithm=MD5`}, func(p map[string]string, uri string) bool {
		if p["qop"] != "" || p["nc"] != "" || p["cnonce"] != "" || p["opaque"] != "op-1" || p["uri"] != uri {
			return false
		}
		return p["response"] == md5hex(ha1+":legacy-nonce:"+md5hex("POST:"+uri))
	})
	c := legacy.client(t)
	var out struct{ Version int }
	for i := 0; i < 2; i++ {
		if err := c.call(ctx, "/json_rpc", "get_version", nil, &out); err != nil || out.Version != 65562 {
			t.Fatalf("no-qop digest call %d: %v (auth %v)", i, err, legacy.lastAuth.Load())
		}
	}
	if n := legacy.requests.Load(); n != 3 {
		t.Fatalf("no-qop challenge not reused: %d requests for 2 calls", n)
	}
	// A server offering several qop values (auth-int first) and unquoted parameters: the client picks auth.
	multi := newDigestNode(t, []string{`Digest realm=monero-rpc, nonce=multi-nonce, qop="auth-int,auth"`}, func(p map[string]string, uri string) bool {
		if p["qop"] != "auth" || p["nc"] != "00000001" || p["cnonce"] == "" {
			return false
		}
		ha2 := md5hex("POST:" + uri)
		return p["response"] == md5hex(ha1+":multi-nonce:"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2)
	})
	if err := multi.client(t).call(ctx, "/json_rpc", "get_version", nil, &out); err != nil {
		t.Fatalf("qop list digest: %v (auth %v)", err, multi.lastAuth.Load())
	}
	// Only MD5-sess offered (unsupported) or no challenge at all: one request, a clear error, no loop.
	sess := newDigestNode(t, []string{`Digest realm="monero-rpc", nonce="n", qop="auth", algorithm=MD5-sess`}, func(map[string]string, string) bool { return false })
	if err := sess.client(t).call(ctx, "/json_rpc", "get_version", nil, nil); err == nil || !strings.Contains(err.Error(), "authentication required") || sess.requests.Load() != 1 {
		t.Fatalf("MD5-sess only: %v requests=%d", err, sess.requests.Load())
	}
	none := newDigestNode(t, nil, func(map[string]string, string) bool { return false })
	if err := none.client(t).call(ctx, "/json_rpc", "get_version", nil, nil); err == nil || !strings.Contains(err.Error(), "authentication required") || none.requests.Load() != 1 {
		t.Fatalf("401 without challenge: %v requests=%d", err, none.requests.Load())
	}
	// A server that keeps rejecting the answered challenge: credentials rejected after exactly one retry.
	reject := newDigestNode(t, []string{`Digest realm="monero-rpc", nonce="r", qop="auth", algorithm=MD5`}, func(map[string]string, string) bool { return false })
	if err := reject.client(t).call(ctx, "/json_rpc", "get_version", nil, nil); err == nil || !strings.Contains(err.Error(), "credentials rejected (HTTP 401)") || strings.Contains(err.Error(), "digest-pass") || reject.requests.Load() != 2 {
		t.Fatalf("rejected digest: %v requests=%d", err, reject.requests.Load())
	}
}

func TestGapBitcoinTestnet3ChainName(t *testing.T) {
	ctx := context.Background()
	core, _, s := newGapCore(t, "test") // Bitcoin Core reports testnet3 as "test"
	p, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Network() != "testnet3" || p.prefix != "tb1" {
		t.Fatalf("chain test: network %q prefix %q", p.Network(), p.prefix)
	}
	if pinned, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "test", 1); err != nil || pinned.Network() != "testnet3" {
		t.Fatalf("BITCOIN_CHAIN=test: %v", err)
	}
	if addr, err := p.NewAddress(ctx, "o1"); err != nil || addr != core.newAddr {
		t.Fatalf("tb1 address on testnet3: %q %v", addr, err)
	}
	core.set(func(c *payCore) { c.newAddr = "bcrt1qregtestaddress000000000000000000" })
	if _, err = p.NewAddress(ctx, "o2"); err == nil {
		t.Fatal("regtest address accepted on testnet3")
	}
	if !p.ValidAddress("tb1qvendorpayout000000000000000000000") || p.ValidAddress("bc1qmainnet000000000000000000000000") {
		t.Fatal("testnet3 address validation")
	}
	// The node later reports another test chain: refused (never silently switched).
	core.set(func(c *payCore) { c.chain = "testnet4" })
	if _, err = p.Check(ctx); err == nil || !isRefusal(err) || !strings.Contains(err.Error(), `now reports chain "testnet4"; it was "test"`) {
		t.Fatalf("chain switch: %v", err)
	}
}

func TestGapMonerodGetInfoWithoutNettype(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		info   map[string]any
		refuse string // "" = accepted
		sync   bool
	}{
		{"stagenet flag", map[string]any{"stagenet": true, "height": 10, "target_height": 0}, "", false},
		{"stagenet flag syncing", map[string]any{"stagenet": true, "height": 10, "target_height": 20}, "", true},
		{"no height fields", map[string]any{"stagenet": true}, "", false},
		{"testnet flag vs stagenet wallet", map[string]any{"testnet": true}, "daemon is on testnet but the wallet is on stagenet", false},
		{"no flags means mainnet", map[string]any{"height": 10}, `monero daemon network "mainnet" is not a test network`, false},
		{"mainnet flag wins over nettype", map[string]any{"nettype": "stagenet", "mainnet": true}, `monero daemon network "stagenet" is not a test network`, false},
		{"fakechain", map[string]any{"nettype": "fakechain"}, `monero daemon network "fakechain" is not a test network`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ov, s := newGapMonero(t)
			ov.answer("get_info", func(string, string, json.RawMessage) (any, int, string) { return tc.info, 0, "" })
			p, err := buildMoneroProvider(s.url(""), s.url(""), "", 1)
			if err != nil {
				t.Fatal(err)
			}
			syncing, err := p.Check(ctx)
			if tc.refuse == "" {
				if err != nil || syncing != tc.sync || p.Network() != "stagenet" {
					t.Fatalf("accepted daemon: syncing=%v err=%v", syncing, err)
				}
				return
			}
			if err == nil || !isRefusal(err) || !strings.Contains(err.Error(), tc.refuse) {
				t.Fatalf("daemon %v: %v", tc.info, err)
			}
		})
	}
}
