package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

type coreRoundTripFunc func(*http.Request) (*http.Response, error)

func (f coreRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A node or its front proxy must not forward a key-bearing RPC to another endpoint, including a plaintext
// endpoint after HTTPS. Exercise the actual client's redirect handling without opening network sockets.
func TestSignerRPCRejectsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, err := newCore("https://signer:synthetic-password@escrow-node.invalid:8332")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c.client.Transport = coreRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					body, err := io.ReadAll(r.Body)
					if err != nil || !strings.Contains(string(body), "SYNTHETIC_ACCOUNT_PRIVATE_KEY") {
						t.Error("configured node did not receive the synthetic key-bearing RPC")
					}
					return &http.Response{StatusCode: status, Header: http.Header{
						"Location": {"http://unrelated-target.invalid:8000"},
					}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(`{"result":{"descriptor":"public"},"error":null}`)), Request: r}, nil
			})
			var out map[string]any
			err = c.call(context.Background(), "getdescriptorinfo", []any{"wpkh(SYNTHETIC_ACCOUNT_PRIVATE_KEY/0/*)"}, &out)
			if !errors.Is(err, errCoreDown) {
				t.Errorf("redirect error = %v, want fixed Core unavailable error", err)
			}
			if calls != 1 {
				t.Errorf("sent %d requests; redirect destination must receive none", calls)
			}
		})
	}
}

// ProxyFromEnvironment caches its first environment snapshot. A child test process makes this regression
// independent of any earlier HTTP tests. The real transport runs over net.Pipe, recording where it dials and
// which endpoint receives the synthetic key-bearing body, without DNS, external requests or listening sockets.
func TestSignerRPCNoEnvironmentProxy(t *testing.T) {
	const child = "OPSECMKT_SIGNER_PROXY_TEST_CHILD"
	if os.Getenv(child) != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestSignerRPCNoEnvironmentProxy$", "-test.v")
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			switch strings.ToUpper(name) {
			case child, "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUEST_METHOD":
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		cmd.Env = append(cmd.Env, child+"=1", "HTTP_PROXY=http://ambient-proxy.invalid:8888", "NO_PROXY=")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated proxy regression failed: %v\n%s", err, output)
		}
		return
	}

	c, err := newCore("http://signer:synthetic-password@escrow-node.invalid:8332")
	if err != nil {
		t.Fatal(err)
	}
	transport := c.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	tr, ok := transport.(*http.Transport)
	if !ok {
		t.Fatal("signer RPC transport cannot be instrumented")
	}
	tr = tr.Clone()
	t.Cleanup(tr.CloseIdleConnections)
	type received struct{ address, body, auth string }
	requests := make(chan received, 1)
	serverErrors := make(chan error, 1)
	tr.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		client, server := net.Pipe()
		server.SetDeadline(time.Now().Add(5 * time.Second))
		go func() {
			defer server.Close()
			r, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				serverErrors <- err
				return
			}
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				serverErrors <- err
				return
			}
			requests <- received{address, string(body), r.Header.Get("Authorization")}
			const result = `{"result":{"descriptor":"public"},"error":null}`
			_, err = fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(result), result)
			serverErrors <- err
		}()
		return client, nil
	}
	c.client.Transport = tr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out map[string]any
	if err := c.call(ctx, "getdescriptorinfo", []any{"wpkh(SYNTHETIC_ACCOUNT_PRIVATE_KEY/0/*)"}, &out); err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.address != "escrow-node.invalid:8332" {
		t.Errorf("key-bearing request dialed %s instead of the configured node", got.address)
	}
	if !strings.Contains(got.body, "SYNTHETIC_ACCOUNT_PRIVATE_KEY") || got.auth == "" {
		t.Error("configured node must receive the RPC body and authentication")
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
}
