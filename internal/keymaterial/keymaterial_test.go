package keymaterial

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Run(args, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestGenerateSecretsAndNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "app")
	b := filepath.Join(dir, "wallet")
	out := run(t, "generate", "secret", "--out", a)
	run(t, "generate", "secret", "--out", b)
	first, _ := os.ReadFile(a)
	second, _ := os.ReadFile(b)
	decoded, err := hex.DecodeString(strings.TrimSpace(string(first)))
	if err != nil || len(decoded) != 32 || bytes.Equal(first, second) {
		t.Fatal("random independent 32-byte secrets required")
	}
	if strings.Contains(out, strings.TrimSpace(string(first))) {
		t.Fatal("printed secret")
	}
	info, _ := os.Stat(a)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private permissions")
	}
	if err := Run([]string{"generate", "secret", "--out", a}, &bytes.Buffer{}); err == nil {
		t.Fatal("overwrote existing key")
	}
	after, _ := os.ReadFile(a)
	if !bytes.Equal(first, after) {
		t.Fatal("modified original")
	}
}

func TestAuditSeedAndImport(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	public := filepath.Join(dir, "public")
	run(t, "generate", "audit", "--out", seed, "--public-out", public)
	data, _ := os.ReadFile(seed)
	raw, _ := hex.DecodeString(strings.TrimSpace(string(data)))
	pub, _ := os.ReadFile(public)
	expected := hex.EncodeToString(ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey))
	if strings.TrimSpace(string(pub)) != expected {
		t.Fatal("wrong audit public key")
	}
	copy := filepath.Join(dir, "imported")
	run(t, "import", "audit", "--in", seed, "--out", copy)
	got, _ := os.ReadFile(copy)
	if !bytes.Equal(data, got) {
		t.Fatal("changed imported seed")
	}
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("do-not-disclose"), 0600)
	if err := Run([]string{"import", "secret", "--in", bad, "--out", filepath.Join(dir, "refused")}, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "do-not-disclose") {
		t.Fatal("bad import accepted or disclosed")
	}
}

func TestPGPRecoveryIsSeparatePublicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "recovery.asc")
	priv := filepath.Join(dir, "offline.asc")
	out := run(t, "generate", "pgp", "--name", "Offline recovery", "--public-out", pub, "--private-out", priv)
	public, _ := os.ReadFile(pub)
	private, _ := os.ReadFile(priv)
	if bytes.Contains(public, []byte("PRIVATE KEY")) || !bytes.Contains(private, []byte("PRIVATE KEY")) {
		t.Fatal("key custody outputs mixed")
	}
	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(public))
	if err != nil || len(ring) != 1 || len(ring[0].Subkeys) == 0 {
		t.Fatal("unusable recovery key")
	}
	if strings.Contains(out, "BEGIN PGP") {
		t.Fatal("printed key")
	}
	run(t, "import", "pgp", "--in", pub, "--public-out", filepath.Join(dir, "imported.asc"))
	if err := Run([]string{"import", "pgp", "--in", priv, "--public-out", filepath.Join(dir, "refused.asc")}, &bytes.Buffer{}); err == nil {
		t.Fatal("private input requires explicit private destination")
	}
}

func TestTLSCertificateRolesAndPairImport(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	cakey := filepath.Join(dir, "ca.key")
	run(t, "generate", "tls-ca", "--name", "Test CA", "--cert-out", ca, "--key-out", cakey)
	server := filepath.Join(dir, "server.pem")
	key := filepath.Join(dir, "server.key")
	run(t, "generate", "tls-server", "--name", "upload.test", "--host", "upload.test,127.0.0.1", "--ca-cert", ca, "--ca-key", cakey, "--cert-out", server, "--key-out", key)
	certPEM, _ := os.ReadFile(server)
	keyPEM, _ := os.ReadFile(key)
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	capem, _ := os.ReadFile(ca)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(capem)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "upload.test", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("server key also grants client role")
	}
	client := filepath.Join(dir, "client.pem")
	clientkey := filepath.Join(dir, "client.key")
	run(t, "generate", "tls-client", "--name", "application", "--ca-cert", ca, "--ca-key", cakey, "--cert-out", client, "--key-out", clientkey)
	run(t, "import", "tls", "--cert-in", server, "--key-in", key, "--cert-out", filepath.Join(dir, "copy.pem"), "--key-out", filepath.Join(dir, "copy.key"))
	if err := Run([]string{"import", "tls", "--cert-in", server, "--key-in", clientkey, "--cert-out", filepath.Join(dir, "bad.pem"), "--key-out", filepath.Join(dir, "bad.key")}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted mismatched key")
	}
}

func TestMultiOutputExistingDestinationChangesNothing(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	fresh := filepath.Join(dir, "fresh")
	os.WriteFile(existing, []byte("preserve"), 0600)
	if err := Run([]string{"generate", "pgp", "--name", "test", "--public-out", fresh, "--private-out", existing}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted existing destination")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("left partial output")
	}
	if err := Run([]string{"generate", "pgp", "--name", "test", "--public-out", fresh, "--private-out", fresh}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted duplicate paths")
	}
}

func TestKeyInputsAndOutputsRefuseSymlinks(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	run(t, "generate", "secret", "--out", original)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(original, link); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"import", "secret", "--in", link, "--out", filepath.Join(dir, "copy")}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted symlink input")
	}
	before, _ := os.ReadFile(original)
	if err := Run([]string{"generate", "secret", "--out", link}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted symlink output")
	}
	after, _ := os.ReadFile(original)
	if !bytes.Equal(before, after) {
		t.Fatal("modified symlink target")
	}
}

func TestInvalidKeyCommandHasNoOutputs(t *testing.T) {
	for _, args := range [][]string{
		{"generate", "pgp", "--public-out", "unused"},
		{"generate", "tls-server", "--name", "server", "--host", "localhost"},
		{"generate", "secret", "--out", "unused", "--unknown", "do-not-disclose"},
		{"generate", "tls-ca", "--name", "CA", "--days", "0", "--cert-out", "unused", "--key-out", "unused-key"},
	} {
		var out bytes.Buffer
		if err := Run(args, &out); err == nil || strings.Contains(err.Error(), "do-not-disclose") {
			t.Fatalf("invalid flags accepted or disclosed: %v", args[:2])
		}
		if out.Len() != 0 {
			t.Fatal("invalid invocation wrote output")
		}
	}
}

func TestPGPImportRejectsJunkAndMultipleKeys(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "pub")
	priv := filepath.Join(dir, "priv")
	run(t, "generate", "pgp", "--name", "offline", "--public-out", pub, "--private-out", priv)
	body, _ := os.ReadFile(pub)
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"prefix", append([]byte("junk\n"), body...)},
		{"suffix", append(append([]byte{}, body...), []byte("junk\n")...)},
		{"multiple", append(append([]byte{}, body...), body...)},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, tc.body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := Run([]string{"import", "pgp", "--in", p, "--public-out", filepath.Join(dir, tc.name+"-output")}, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %s", tc.name)
		}
	}
}
