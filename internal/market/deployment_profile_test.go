package market

import "testing"

func TestProductionServiceSeparation(t *testing.T) {
	base := map[string]string{
		"OPSECMKT_PROFILE": "prod", "OPSECMKT_APP_HOST": "10.0.0.2", "OPSECMKT_GATEWAY_URL": "https://upload.example.test",
		"UPLOAD_MODE": "isolated", "TLS_CERT": "server.pem", "TLS_KEY": "server.key",
		"DATABASE_URL": "postgres://u:p@10.0.0.3/market?sslmode=verify-full", "ESCROW_XMR": "on",
		"ESCROW_XMR_WALLET_RPC_URL": "https://u:p@10.0.0.4:18083", "ESCROW_XMR_DAEMON_URL": "https://u:p@10.0.0.4:18081",
	}
	get := func(k string) string { return base[k] }
	if err := CheckDeploymentProfile(get); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"UPLOAD_MODE", "integrated"}, {"CLAMD_ADDRESS", "clamd:3310"}, {"TLS_KEY", ""},
		{"APP_MODE", "tor"},
		{"OPSECMKT_APP_HOST", "127.0.0.1"}, {"OPSECMKT_PROFILE", "production"},
		{"DATABASE_URL", "postgres://u:p@10.0.0.2/market?sslmode=verify-full"},
		{"DATABASE_URL", "postgres://u:p@10.0.0.3/market?sslmode=require"},
		{"DATABASE_URL", "postgres://u:p@localhost/market?sslmode=verify-full"},
		{"OPSECMKT_GATEWAY_URL", "https://10.0.0.3"}, {"OPSECMKT_GATEWAY_URL", "http://upload.example.test"},
		{"ESCROW_XMR_WALLET_RPC_URL", "http://u:p@10.0.0.4:18083"},
		{"ESCROW_XMR_DAEMON_URL", "https://10.0.0.4:18081"},
		{"ESCROW_XMR_DAEMON_URL", "https://u:p@10.0.0.3:18081"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			old := base[tc.key]
			base[tc.key] = tc.value
			defer func() { base[tc.key] = old }()
			if CheckDeploymentProfile(get) == nil {
				t.Fatal("accepted unsafe production service topology")
			}
		})
	}
	base["OPSECMKT_PROFILE"] = "dev"
	base["UPLOAD_MODE"] = "integrated"
	if err := CheckDeploymentProfile(get); err != nil {
		t.Fatal("refused self-contained development", err)
	}
}
