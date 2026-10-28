package operator

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRenderInstallationsForDockerCompose writes the compose.json of three installations into OPSECMKT_RENDER_DIR
// (A-697): dev-full (internal database, public and administrator Tor, clamd, monerod and its wallet), prod (remote
// database, clearnet) and prod-tor (with the private administrator onion). Production storage is stubbed, as in the
// other production tests, so no LUKS host is needed. scripts/test-native-deploy.sh then runs `docker compose config
// --quiet` on each with a real Compose; the images are placeholders that are never pulled. The other tests check the
// rendered keys; this one only produces the files, and skips without OPSECMKT_RENDER_DIR.
func TestRenderInstallationsForDockerCompose(t *testing.T) {
	root := os.Getenv("OPSECMKT_RENDER_DIR")
	if root == "" {
		t.Skip("OPSECMKT_RENDER_DIR unset; scripts/test-native-deploy.sh sets it and validates the files with docker compose config")
	}
	_, seedKey := recoveryKey(t)
	installations := map[string][]string{
		"dev-full": {"init", "--mode", "dev", "--exposure", "tor", "--app-image", testImage, "--tor-image", testImage,
			"--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t), "--clamav-image", testImage,
			"--escrow-xmr", "--monero-image", testImage, "--monero-wallet-image", testImage, "--monero-seed-key", seedKey},
	}
	_, prod := productionInitArgs(t, "database-ca.pem")
	installations["prod"] = prod
	_, prodTor := productionInitArgs(t, "database-ca.pem")
	installations["prod-tor"] = append(prodTor, "--exposure", "tor", "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t))
	for name, args := range installations {
		dir := filepath.Join(root, name)
		// productionInitArgs names a temporary directory after "--dir"; init this one instead.
		args = append(args, "--dir", dir, "--project", "opsecmkt-render-"+name)
		if out, err := invoke(t, args...); err != nil {
			t.Fatalf("%s: init: %v (%s)", name, err, out)
		}
		c, err := loadConfig(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := materialize(dir, c); err != nil {
			t.Fatalf("%s: materialize: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "compose.json")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
