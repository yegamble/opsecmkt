package market

import (
	"maps"
	"strings"
	"testing"
)

func remoteTorRelayEnvironment() map[string]string {
	values := map[string]string{}
	for i, entries := 0, xmrOnEnv("/keys/recovery.asc"); i < len(entries); i += 2 {
		values[entries[i]] = entries[i+1]
	}
	for key, value := range map[string]string{
		"OPSECMKT_PROFILE": "prod", "APP_MODE": "tor", "ADMIN_SOCKET": "/run/market-admin/http.sock",
		"OPSECMKT_APP_HOST": "10.20.0.2", "OPSECMKT_GATEWAY_URL": "http://storefront.example.onion",
		"UPLOAD_MODE": "isolated", "TLS_CERT": "app.pem", "TLS_KEY": "app.key",
		"DATABASE_URL":              "postgres://market:private-db-password@db.example/market?sslmode=verify-full",
		"ESCROW_XMR_WALLET_RPC_URL": "https://wallet-user:private-wallet-password@10.20.0.4:18443",
		"ESCROW_XMR_DAEMON_URL":     "https://daemon-user:private-daemon-password@10.20.0.4:28443",
		"ESCROW_XMR_TOR_RELAY":      "remote-tor",
	} {
		values[key] = value
	}
	return values
}

func TestRemoteTorRelayRequiresFullProductionContract(t *testing.T) {
	base := remoteTorRelayEnvironment()
	for _, tc := range []struct{ key, value string }{
		{"OPSECMKT_PROFILE", ""}, {"OPSECMKT_PROFILE", "dev"}, {"APP_MODE", "clearnet"},
		{"ESCROW_XMR", "off"}, {"ADMIN_SOCKET", ""}, {"UPLOAD_MODE", "integrated"}, {"TLS_KEY", ""},
		{"DATABASE_URL", "postgres://market:password@db.example/market?sslmode=require"},
		{"OPSECMKT_GATEWAY_URL", "https://10.20.0.4"},
		{"ESCROW_XMR_DAEMON_URL", "http://u:p@10.20.0.4:28443"},
		{"ESCROW_XMR_WALLET_RPC_URL", "https://u:p@10.20.0.2:18443"},
		{"ESCROW_XMR_WALLET_RPC_URL", "https://10.20.0.4:18443"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			values := maps.Clone(base)
			values[tc.key] = tc.value
			get := func(key string) string { return values[key] }
			if err := CheckDeploymentProfile(get); err == nil {
				t.Fatal("remote relay accepted an incomplete production contract")
			}
			if tc.key != "ESCROW_XMR" {
				if _, err := escrowXMRConfigFromEnv(get); err == nil {
					t.Fatal("escrow configuration bypassed production relay checks")
				}
			}
		})
	}
}

func TestRemoteTorRelayDeclarationAndBlankHold(t *testing.T) {
	values := remoteTorRelayEnvironment()
	get := func(key string) string { return values[key] }
	for _, network := range []string{"stagenet", "mainnet"} {
		values["MONERO_NETWORK"] = network
		cfg, err := escrowXMRConfigFromEnv(get)
		if err != nil {
			t.Fatal(err)
		}
		app := &App{mode: "tor", escrowXMR: &escrowXMR{torRelayOK: cfg.torRelayOK}}
		if !cfg.torRelayOK || app.xmrRelayHold() != "" {
			t.Fatal("valid remote Tor declaration did not permit relay")
		}
	}
	values["ESCROW_XMR_TOR_RELAY"] = ""
	cfg, err := escrowXMRConfigFromEnv(get)
	if err != nil || cfg.torRelayOK || (&App{mode: "tor", escrowXMR: &escrowXMR{torRelayOK: cfg.torRelayOK}}).xmrRelayHold() == "" {
		t.Fatal("blank native production configuration did not retain its relay hold", err)
	}
	values["ESCROW_XMR_TOR_RELAY"] = "remote-tor"
	values["MONERO_RPC_CERT"], values["MONERO_RPC_KEY"] = "client.pem", "client.key"
	values["ESCROW_XMR_DAEMON_URL"] = "https://10.20.0.4:28443"
	values["ESCROW_XMR_WALLET_RPC_URL"] = "https://10.20.0.4:18443"
	if _, err := escrowXMRConfigFromEnv(get); err != nil {
		t.Fatal("mutual TLS production endpoints were refused", err)
	}
	warnings := strings.Join(dependencyTopology(get), " ")
	if !strings.Contains(warnings, "remote Tor egress is declared by the operator") || !strings.Contains(warnings, "cannot verify") {
		t.Fatal("administrator status omitted the remote routing limitation")
	}
	for _, secret := range []string{"10.20.0.4", "private-wallet-password", "private-daemon-password"} {
		if strings.Contains(warnings, secret) {
			t.Fatal("remote routing notice disclosed endpoint information")
		}
	}
}
