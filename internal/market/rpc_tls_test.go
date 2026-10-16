package market

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoneroRPCPrivateCA(t *testing.T) {
	for _, name := range []string{"MONERO_RPC_CA", "MONERO_RPC_CERT", "MONERO_RPC_KEY"} {
		t.Setenv(name, "")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":"opsecmkt","result":{"ready":true}}`))
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := newRPCClient("monero wallet", srv.URL, "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]bool
	if err := client.call(context.Background(), "", "get_info", nil, &result); err == nil {
		t.Fatal("trusted unknown CA")
	}
	t.Setenv("MONERO_RPC_CA", ca)
	client, err = newRPCClient("monero wallet", srv.URL, "2.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.call(context.Background(), "", "get_info", nil, &result); err != nil || !result["ready"] {
		t.Fatalf("private CA: %v %v", err, result)
	}
	if _, err := newRPCClient("monero wallet", strings.Replace(srv.URL, "https:", "http:", 1), "2.0", true); err == nil {
		t.Fatal("silently ignored TLS for HTTP")
	}
	t.Setenv("MONERO_RPC_CERT", ca)
	if _, err := newRPCClient("monero wallet", srv.URL, "2.0", true); err == nil {
		t.Fatal("accepted incomplete client identity")
	}
	t.Setenv("MONERO_RPC_CERT", "")
	t.Setenv("MONERO_RPC_CA", filepath.Join(t.TempDir(), "missing"))
	if _, err := newRPCClient("monero wallet", srv.URL, "2.0", true); err == nil {
		t.Fatal("silently ignored missing CA")
	}
}
