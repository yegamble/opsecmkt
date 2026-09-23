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
)

// payRPCServer is an httptest JSON-RPC server for adapter tests: Basic or Digest (MD5, qop=auth) auth,
// JSON-RPC 1.0 or 2.0 envelopes, and a per-method handler. It records every call.
type payRPCServer struct {
	*httptest.Server
	user, pass string
	digest     bool
	handle     func(path, method string, params json.RawMessage) (result any, code int, msg string)
	down       atomic.Bool // drop every connection, as an unreachable or restarting node does

	mu         sync.Mutex
	nonce      string
	calls      []string
	params     map[string]json.RawMessage
	challenges int
	lastNC     string
}

func newPayRPCServer(t *testing.T, user, pass string, digest bool, handle func(path, method string, params json.RawMessage) (any, int, string)) *payRPCServer {
	s := &payRPCServer{user: user, pass: pass, digest: digest, handle: handle, nonce: "nonce-1", params: map[string]json.RawMessage{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// url returns the server URL with credentials in userinfo, as operators configure it.
func (s *payRPCServer) url(path string) string {
	u := s.URL
	if s.user != "" {
		u = strings.Replace(u, "http://", "http://"+s.user+":"+s.pass+"@", 1)
	}
	return u + path
}

func (s *payRPCServer) called(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (s *payRPCServer) lastParams(method string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params[method]
}

func (s *payRPCServer) authorized(r *http.Request) bool {
	if s.user == "" {
		return true
	}
	if !s.digest {
		u, p, ok := r.BasicAuth()
		return ok && u == s.user && p == s.pass
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := parseAuthParams(h[7:])
	s.mu.Lock()
	nonce := s.nonce
	s.lastNC = p["nc"]
	s.mu.Unlock()
	if p["username"] != s.user || p["realm"] != "monero-rpc" || p["nonce"] != nonce || p["uri"] != r.URL.RequestURI() || p["qop"] != "auth" || p["algorithm"] != "MD5" {
		return false
	}
	ha1 := md5hex(s.user + ":monero-rpc:" + s.pass)
	ha2 := md5hex("POST:" + r.URL.RequestURI())
	return p["response"] == md5hex(ha1+":"+nonce+":"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2)
}

func (s *payRPCServer) serve(w http.ResponseWriter, r *http.Request) {
	if s.down.Load() {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	if !s.authorized(r) {
		s.mu.Lock()
		s.challenges++
		nonce := s.nonce
		s.mu.Unlock()
		if s.digest {
			w.Header().Add("WWW-Authenticate", `Digest qop="auth",algorithm=MD5-sess,realm="monero-rpc",nonce="`+nonce+`",stale=false`)
			w.Header().Add("WWW-Authenticate", `Digest qop="auth",algorithm=MD5,realm="monero-rpc",nonce="`+nonce+`",stale=false`)
		} else {
			w.Header().Set("WWW-Authenticate", `Basic realm="jsonrpc"`)
		}
		w.WriteHeader(401)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, req.Method)
	s.params[req.Method] = req.Params
	s.mu.Unlock()
	result, code, msg := s.handle(r.URL.Path, req.Method, req.Params)
	resp := map[string]any{"id": req.ID}
	if req.JSONRPC == "2.0" {
		resp["jsonrpc"] = "2.0"
	}
	if code != 0 {
		resp["error"] = map[string]any{"code": code, "message": msg}
		if req.JSONRPC == "1.0" {
			resp["result"] = nil
			w.WriteHeader(500) // Bitcoin Core reports RPC errors with HTTP 500 and a JSON body
		}
	} else {
		resp["result"] = result
		if req.JSONRPC == "1.0" {
			resp["error"] = nil
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func TestRPCBasicAuthNumbersAndErrors(t *testing.T) {
	s := newPayRPCServer(t, "rpcuser", "s3cret-pass", false, func(path, method string, params json.RawMessage) (any, int, string) {
		switch method {
		case "big":
			return map[string]any{"n": json.Number("0.00100000"), "i": json.Number("18446744073709551615")}, 0, ""
		case "fail":
			return nil, -18, "Requested wallet does not exist or is not loaded"
		}
		return nil, -32601, "Method not found"
	})
	c, err := newRPCClient("bitcoin", s.url("/"), "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.endpoint, "s3cret") || strings.Contains(c.endpoint, "@") {
		t.Fatalf("credentials kept in endpoint %q", c.endpoint)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	// Calls are bounded by their context (rpcTimeout, or payoutSendTimeout for a send); a client or transport
	// timeout would cut a slow payout send short after it was broadcast.
	if !ok || tr.Proxy != nil || c.http.Timeout != 0 || tr.ResponseHeaderTimeout != 0 || rpcTimeout.Seconds() != 10 || payoutSendTimeout.Seconds() != 30 {
		t.Fatal("RPC client must use no proxy and no client/transport timeout; calls default to 10 s and sends to 30 s")
	}
	var out struct{ N, I json.Number }
	if err = c.call(context.Background(), "", "big", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.N != "0.00100000" || out.I != "18446744073709551615" {
		t.Fatalf("numbers not preserved: %+v", out)
	}
	if n, err := parseAmount(string(out.N), 8); err != nil || n != 100000 {
		t.Fatalf("amount parse: %d %v", n, err)
	}
	err = c.call(context.Background(), "/wallet/x", "fail", []any{}, nil)
	var re *rpcError
	if !errors.As(err, &re) || re.Code != -18 || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("rpc error not surfaced: %v", err)
	}
	bad, _ := newRPCClient("bitcoin", strings.Replace(s.url("/"), "s3cret-pass", "wrong-pass", 1), "1.0", false)
	err = bad.call(context.Background(), "", "big", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "credentials rejected") || strings.Contains(err.Error(), "wrong-pass") {
		t.Fatalf("wrong password: %v", err)
	}
	s.Close()
	err = c.call(context.Background(), "", "big", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("unreachable: %v", err)
	}
	for _, raw := range []string{"", "ftp://h:1", "http://", "bitcoin:8332"} {
		if _, err := newRPCClient("bitcoin", raw, "1.0", false); err == nil {
			t.Errorf("accepted RPC URL %q", raw)
		}
	}
}

func TestRPCDigestAuth(t *testing.T) {
	s := newPayRPCServer(t, "wallet", "digest-pass", true, func(path, method string, params json.RawMessage) (any, int, string) {
		return map[string]any{"ok": true, "path": path}, 0, ""
	})
	c, err := newRPCClient("monero wallet", s.url(""), "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK   bool
		Path string
	}
	for i := 0; i < 3; i++ {
		if err = c.call(context.Background(), "/json_rpc", "get_version", nil, &out); err != nil || !out.OK || out.Path != "/json_rpc" {
			t.Fatalf("call %d: %v %+v", i, err, out)
		}
	}
	s.mu.Lock()
	challenges, nc := s.challenges, s.lastNC
	s.nonce = "nonce-2" // server rotates its nonce: the client must re-authenticate transparently
	s.mu.Unlock()
	if challenges != 1 || nc != "00000003" {
		t.Fatalf("digest challenge not reused: challenges=%d nc=%s", challenges, nc)
	}
	if err = c.call(context.Background(), "/json_rpc", "get_version", nil, &out); err != nil {
		t.Fatalf("after nonce rotation: %v", err)
	}
	bad, _ := newRPCClient("monero wallet", strings.Replace(s.url(""), "digest-pass", "nope", 1), "2.0", true)
	if err = bad.call(context.Background(), "/json_rpc", "get_version", nil, nil); err == nil || !strings.Contains(err.Error(), "credentials rejected") {
		t.Fatalf("wrong digest password: %v", err)
	}
	if ch := parseDigestChallenge([]string{`Basic realm="x"`, `Digest algorithm=MD5-sess, nonce="a", realm="r"`, `Digest realm="r", nonce="b", qop="auth", algorithm=MD5`}); ch == nil || ch["nonce"] != "b" {
		t.Fatalf("challenge selection: %v", ch)
	}
}

func TestRPCDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect followed; credentials could leak to another host")
	}))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, 307)
	}))
	defer s.Close()
	c, _ := newRPCClient("bitcoin", strings.Replace(s.URL, "http://", "http://u:p@", 1), "1.0", false)
	if err := c.call(context.Background(), "", "getblockchaininfo", nil, nil); err == nil {
		t.Fatal("redirect treated as success")
	}
}
