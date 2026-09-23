package market

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Real monero-wallet-rpc binds its nonce and exact increasing counter to the TCP
// connection, and returns a nonempty 401 body. A stateless Digest stub misses this.
func TestRPCDigestConnectionSession(t *testing.T) {
	type session struct {
		nonce   string
		counter int
	}
	var mu sync.Mutex
	sessions := map[string]*session{}
	accepted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		s := sessions[r.RemoteAddr]
		if s == nil {
			s = &session{nonce: fmt.Sprintf("session-%d", len(sessions)+1)}
			sessions[r.RemoteAddr] = s
		}
		p := parseAuthParams(strings.TrimPrefix(r.Header.Get("Authorization"), "Digest "))
		counter, _ := strconv.ParseInt(p["nc"], 16, 32)
		ha1 := md5hex("wallet:monero-rpc:session-password")
		response := md5hex(ha1 + ":" + s.nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + md5hex("POST:/json_rpc"))
		if p["nonce"] != s.nonce || p["response"] != response || int(counter) != s.counter+1 {
			w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest qop="auth",algorithm=MD5,realm="monero-rpc",nonce="%s",stale=false`, s.nonce))
			w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Digest qop="auth",algorithm=MD5-sess,realm="monero-rpc",nonce="%s",stale=false`, s.nonce))
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "<html><head><title>Unauthorized</title></head><body><h1>Unauthorized</h1></body></html>")
			return
		}
		s.counter++
		accepted++
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"opsecmkt","result":{"ok":true}}`)
	}))
	defer server.Close()
	client, err := newRPCClient("monero wallet", strings.Replace(server.URL, "http://", "http://wallet:session-password@", 1), "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	call := func() error {
		var response struct{ OK bool }
		err := client.call(context.Background(), "/json_rpc", "get_version", nil, &response)
		if err == nil && !response.OK {
			return fmt.Errorf("missing result")
		}
		return err
	}
	if err := call(); err != nil {
		t.Fatalf("first authenticated call: %v", err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := call(); err != nil {
				t.Errorf("parallel call: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	count := len(sessions)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("serialized digest calls used %d connections", count)
	}
	// Retire the idle connection on the client before reconnecting. Closing it
	// server-side can race the client's next write and produce an unrelated RST.
	client.http.CloseIdleConnections()
	if err := call(); err != nil {
		t.Fatalf("new TCP session did not renegotiate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("reconnect used %d TCP sessions, want 2", len(sessions))
	}
	if accepted != 14 {
		t.Fatalf("accepted %d calls, want 14", accepted)
	}
}

func TestRPCDigestQueueHonorsCancellation(t *testing.T) {
	c, err := newRPCClient("monero wallet", "http://wallet:password@127.0.0.1:1", "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	c.digestGate <- struct{}{}
	defer func() { <-c.digestGate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.call(ctx, "/json_rpc", "get_version", nil, nil); err == nil || !strings.Contains(err.Error(), "waiting for authentication session") {
		t.Fatalf("cancelled queued call: %v", err)
	}
}
