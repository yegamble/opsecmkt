package operator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteTorRelayConfigurationFailsClosed(t *testing.T) {
	for _, value := range []string{"remote-tor", "remote", "verified"} {
		dir := filepath.Join(t.TempDir(), "invalid")
		if _, err := invoke(t, "init", "--dir", dir, "--app-image", testImage, "--set", "ESCROW_XMR_TOR_RELAY="+value); err == nil {
			t.Fatal("development or unknown remote Tor setting accepted")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid declaration left an installation")
		}
	}
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Settings["ESCROW_XMR_TOR_RELAY"] = "remote-tor"
	if err := validate(dir, c); err == nil {
		t.Fatal("clearnet production accepted remote Tor declaration")
	}
	c.Exposure = "tor"
	c.AdminTorImage, c.AdminAuthDir, c.AdminSocketDir = testImage, "admin-clients", filepath.Join(dir, "admin")
	if err := importAdminAuth(adminAuthInput(t), filepath.Join(dir, c.AdminAuthDir)); err != nil {
		t.Fatal(err)
	}
	if err := validate(dir, c); err == nil {
		t.Fatal("disabled escrow accepted remote Tor declaration")
	}
	c.Settings["ESCROW_XMR"] = "on"
	for key, value := range map[string]string{
		"MONERO_WALLET_SECRET":      strings.Repeat("abcdef01", 8),
		"ESCROW_XMR_DAEMON_URL":     "https://10.20.0.4:28443",
		"ESCROW_XMR_WALLET_RPC_URL": "https://10.20.0.4:18443",
	} {
		name := "secrets/" + strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c.SecretFiles[key] = name
	}
	// validate checks required paths; material validation checks actual key formats separately.
	if c.PublicFiles == nil {
		c.PublicFiles = map[string]string{}
	}
	c.PublicFiles["ESCROW_XMR_SEED_KEY"] = "secrets/recovery.asc"
	if err := os.WriteFile(filepath.Join(dir, c.PublicFiles["ESCROW_XMR_SEED_KEY"]), []byte("public key fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.TLSFiles["MONERO_RPC_CERT"], c.TLSFiles["MONERO_RPC_KEY"] = c.TLSFiles["TLS_CERT"], c.TLSFiles["TLS_KEY"]
	if err := validate(dir, c); err != nil {
		t.Fatal("valid separate mutual TLS endpoints refused", err)
	}
	var plan bytes.Buffer
	if err := printPlan(&plan, c); err != nil || !strings.Contains(plan.String(), "operator declares remote Tor egress") {
		t.Fatal("plan omitted the routing declaration", err)
	}
	for _, endpoint := range []string{"http://10.20.0.4:28443", "https://app.internal:28443", "https://db.internal:28443"} {
		if err := os.WriteFile(filepath.Join(dir, c.SecretFiles["ESCROW_XMR_DAEMON_URL"]), []byte(endpoint+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validate(dir, c); err == nil {
			t.Fatal("unsafe remote endpoint accepted")
		}
	}
}

// A-530: this controller renders its own development Monero daemon without the installer's node Tor client
// (compose.nodes-tor.yaml), so a Tor plan with that daemon says its peer-to-peer traffic leaves from the host
// and that Monero escrow relays stay held; a clearnet plan or one without a local daemon says nothing about it.
func TestTorPlanDisclosesLocalMoneroOutsideTor(t *testing.T) {
	const notice = "local Monero daemon: peer-to-peer traffic is not routed through Tor"
	settings := map[string]string{"MONERO_NETWORK": "stagenet", "ESCROW_XMR": "on"}
	for _, tc := range []struct {
		exposure, monero string
		want             bool
	}{{"tor", testImage, true}, {"clearnet", testImage, false}, {"tor", "", false}} {
		var plan bytes.Buffer
		c := Config{Mode: "dev", Exposure: tc.exposure, MoneroImage: tc.monero, Settings: settings}
		if err := printPlan(&plan, c); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(plan.String(), notice); got != tc.want {
			t.Fatalf("exposure %s, monero %q: notice shown %v, want %v:\n%s", tc.exposure, tc.monero, got, tc.want, plan.String())
		}
		if tc.want && !strings.Contains(plan.String(), "ESCROW_XMR_TOR_RELAY=clearnet-accepted") {
			t.Fatal("plan omitted the relay hold and its explicit opt-in")
		}
	}
}
