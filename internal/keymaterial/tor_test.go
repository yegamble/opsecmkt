package keymaterial

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTorClientKeysGenerateAndBindOffline(t *testing.T) {
	d := t.TempDir()
	pub, priv := filepath.Join(d, "operator.auth"), filepath.Join(d, "operator.key")
	run(t, "generate", "tor-client", "--public-out", pub, "--private-out", priv)
	public, _ := os.ReadFile(pub)
	secret, _ := os.ReadFile(priv)
	raw, err := torBase32.DecodeString(strings.TrimSpace(string(secret)))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(public) != "descriptor:x25519:"+torBase32.EncodeToString(key.PublicKey().Bytes())+"\n" {
		t.Fatal("public/private mismatch")
	}
	for _, p := range []string{pub, priv} {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0600 {
			t.Fatal("Tor auth permissions")
		}
	}
	// Construct a valid synthetic v3 onion, independent of any real service.
	onionBytes := append([]byte(nil), bytes.Repeat([]byte{7}, 32)...)
	material := append([]byte(".onion checksum"), onionBytes...)
	material = append(material, 3)
	sum := sha3.Sum256(material)
	onionBytes = append(onionBytes, sum[0], sum[1], 3)
	onion := strings.ToLower(torBase32.EncodeToString(onionBytes))
	bound := filepath.Join(d, "operator.auth_private")
	run(t, "import", "tor-client", "--in", priv, "--public-out", filepath.Join(d, "bound.auth"), "--private-out", bound, "--onion", onion+".onion")
	result, _ := os.ReadFile(bound)
	if string(result) != onion+":descriptor:x25519:"+string(secret) {
		t.Fatal("invalid bound authorization")
	}
	if err := Run([]string{"generate", "tor-client", "--public-out", filepath.Join(d, "bad.auth"), "--private-out", filepath.Join(d, "bad.key"), "--onion", strings.Repeat("a", 56)}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted bad onion checksum")
	}
}
