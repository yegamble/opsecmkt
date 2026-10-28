package operator

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// replacementDocker installs a fake docker that records each call. A file
// named RECORD.ps supplies `docker ps` output (an existing app container) and
// RECORD.fail makes every call fail.
func replacementDocker(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "docker-args")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_CONFIG"
if [ -f "$DOCKER_CONFIG.fail" ]; then exit 9; fi
if [ "$1" = --context ]; then shift 2; fi
if [ "$1" = ps ] && [ -f "$DOCKER_CONFIG.ps" ]; then cat "$DOCKER_CONFIG.ps"; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	return record
}

// installationFiles reads every regular file of the installation except the
// process lock, keyed by its relative path.
func installationFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "operator.lock" {
			return nil
		}
		b, err := os.ReadFile(path)
		files[rel] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// assertOnlyChanged fails unless exactly the named files differ and no file
// appeared or disappeared.
func assertOnlyChanged(t *testing.T, before, after map[string][]byte, changed ...string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, name := range changed {
		allowed[name] = true
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Fatalf("%s disappeared", name)
		}
		if !allowed[name] && !bytes.Equal(got, want) {
			t.Fatalf("%s changed", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Fatalf("unexpected new file %s", name)
		}
	}
}

func writeClaimedReceipt(t *testing.T, dir string) {
	t.Helper()
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeInput(t *testing.T, name string, body []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEndpointSetReplacesDatabaseURLAndCAOnly(t *testing.T) {
	dir := initProduction(t)
	writeClaimedReceipt(t, dir)
	record := replacementDocker(t)
	before := installationFiles(t, dir)
	urlFile := writeInput(t, "database-rollback.url", []byte("postgres://market:rollback-password@db-rollback.internal:5432/market_rollback?sslmode=verify-full&sslrootcert=/srv/opsecmkt-inputs/database-ca.pem\n"), 0600)
	newCA, _ := testTLSMaterial(t, false)
	caFile := writeInput(t, "database-ca.pem", newCA, 0644)
	out, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", urlFile, "--database-ca", caFile)
	if err != nil {
		t.Fatalf("endpoint set: %v (%s)", err, out)
	}
	if strings.Contains(out, "rollback-password") || strings.Contains(out, "market_rollback") {
		t.Fatalf("endpoint set printed the database URL: %s", out)
	}
	after := installationFiles(t, dir)
	assertOnlyChanged(t, before, after, "secrets/database-url", "secrets/database-ca.pem")
	if !bytes.Equal(after["secrets/database-ca.pem"], newCA) {
		t.Fatal("replacement database CA was not installed")
	}
	installed, err := url.Parse(strings.TrimSpace(string(after["secrets/database-url"])))
	if err != nil {
		t.Fatal(err)
	}
	password, _ := installed.User.Password()
	if installed.Hostname() != "db-rollback.internal" || installed.Path != "/market_rollback" || password != "rollback-password" || installed.Query().Get("sslmode") != "verify-full" || installed.Query().Get("sslrootcert") != "/run/opsecmkt-secrets/database-ca.pem" {
		t.Fatalf("installed URL does not name the replacement database with the mounted CA: %s", installed.Redacted())
	}
	if info, err := os.Lstat(filepath.Join(dir, "secrets/database-url")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement URL file mode: %v %v", info, err)
	}
	args, _ := os.ReadFile(record)
	if strings.Count(string(args), "\n") != 1 || !strings.Contains(string(args), "ps -aq --filter label=com.docker.compose.project=") || !strings.Contains(string(args), "label=com.docker.compose.service=app") {
		t.Fatalf("endpoint set must only check for an application container: %s", args)
	}
	if out, err := invoke(t, "plan", "--dir", dir); err != nil {
		t.Fatalf("replaced endpoint fails plan: %v (%s)", err, out)
	}

	// Without --database-ca the imported CA is kept and still named by the URL.
	before = installationFiles(t, dir)
	urlFile = writeInput(t, "database-pitr.url", []byte("postgresql://market:pitr-password@db-pitr.internal/market?sslmode=verify-full"), 0600)
	if out, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", urlFile); err != nil {
		t.Fatalf("endpoint set without CA: %v (%s)", err, out)
	}
	after = installationFiles(t, dir)
	assertOnlyChanged(t, before, after, "secrets/database-url")
	installed, _ = url.Parse(strings.TrimSpace(string(after["secrets/database-url"])))
	if installed.Hostname() != "db-pitr.internal" || installed.Query().Get("sslrootcert") != "/run/opsecmkt-secrets/database-ca.pem" {
		t.Fatalf("kept CA is not named by the replacement URL: %s", installed.Redacted())
	}

	// An external database imported without a CA gains exactly that one entry.
	external := filepath.Join(t.TempDir(), "external")
	initial := writeInput(t, "initial.url", []byte("postgres://market:initial-password@db.internal/market?sslmode=verify-full"), 0600)
	if out, err := invoke(t, "init", "--dir", external, "--app-image", testImage, "--database-url-file", initial); err != nil {
		t.Fatalf("init external: %v (%s)", err, out)
	}
	c, err := loadConfig(external)
	if err != nil {
		t.Fatal(err)
	}
	before = installationFiles(t, external)
	if out, err := invoke(t, "endpoint", "set", "--dir", external, "--database-url-file", urlFile, "--database-ca", caFile); err != nil {
		t.Fatalf("endpoint set adding a CA: %v (%s)", err, out)
	}
	after = installationFiles(t, external)
	delete(after, "secrets/database-ca.pem")
	assertOnlyChanged(t, before, after, "secrets/database-url", "operator.json")
	got, err := loadConfig(external)
	if err != nil {
		t.Fatal(err)
	}
	if got.TLSFiles["DATABASE_CA"] != "secrets/database-ca.pem" {
		t.Fatalf("new CA not recorded: %#v", got.TLSFiles)
	}
	delete(got.TLSFiles, "DATABASE_CA")
	if len(c.TLSFiles) == 0 {
		c.TLSFiles = got.TLSFiles
	}
	want, _ := json.Marshal(c)
	have, _ := json.Marshal(got)
	if !bytes.Equal(want, have) {
		t.Fatal("endpoint set changed configuration beyond the database CA")
	}
}

func TestEndpointSetRefusesInsecureURLAndUnsafeFile(t *testing.T) {
	dir := initProduction(t)
	writeClaimedReceipt(t, dir)
	record := replacementDocker(t)
	before := installationFiles(t, dir)
	good := writeInput(t, "good.url", []byte("postgres://market:hidden-password@db2.internal/market?sslmode=verify-full"), 0600)
	link := filepath.Join(t.TempDir(), "link.url")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	caCert, caKey := testTLSMaterial(t, false)
	verifyFull := "sslmode=verify-full"
	unsafeFile := "mode 0600 or stricter"
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"require":       {[]string{"--database-url-file", writeInput(t, "a.url", []byte("postgres://market:hidden-password@db2.internal/market?sslmode=require"), 0600)}, verifyFull},
		"no sslmode":    {[]string{"--database-url-file", writeInput(t, "b.url", []byte("postgres://market:hidden-password@db2.internal/market"), 0600)}, verifyFull},
		"scheme":        {[]string{"--database-url-file", writeInput(t, "c.url", []byte("mysql://market:hidden-password@db2.internal/market?sslmode=verify-full"), 0600)}, verifyFull},
		"app host":      {[]string{"--database-url-file", writeInput(t, "d.url", []byte("postgres://market:hidden-password@app.internal/market?sslmode=verify-full"), 0600)}, "separate remote host"},
		"control char":  {[]string{"--database-url-file", writeInput(t, "e.url", []byte("postgres://market:hidden-password@db2.internal/market?sslmode=verify-full\x00"), 0600)}, verifyFull},
		"group":         {[]string{"--database-url-file", writeInput(t, "f.url", []byte("postgres://market:hidden-password@db2.internal/market?sslmode=verify-full"), 0640)}, unsafeFile},
		"world":         {[]string{"--database-url-file", writeInput(t, "g.url", []byte("postgres://market:hidden-password@db2.internal/market?sslmode=verify-full"), 0604)}, unsafeFile},
		"symlink":       {[]string{"--database-url-file", link}, unsafeFile},
		"missing":       {nil, "requires --database-url-file"},
		"positional":    {[]string{"--database-url-file", good, "extra"}, "positional"},
		"private CA":    {[]string{"--database-url-file", good, "--database-ca", writeInput(t, "ca.pem", append(append([]byte{}, caCert...), caKey...), 0600)}, "without private keys"},
		"not a CA file": {[]string{"--database-url-file", good, "--database-ca", writeInput(t, "junk.pem", []byte("not a certificate"), 0644)}, "public PEM certificates"},
	} {
		out, err := invoke(t, append([]string{"endpoint", "set", "--dir", dir}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want refusal %q, got %v (%s)", name, tc.want, err, out)
		}
		if strings.Contains(out+err.Error(), "hidden") {
			t.Fatalf("%s: refusal printed the URL: %v %s", name, err, out)
		}
		assertOnlyChanged(t, before, installationFiles(t, dir))
	}

	unlock, err := lockInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", good); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("endpoint set ignored the installation lock: %v", err)
	}
	unlock()

	if err := os.WriteFile(record+".ps", []byte("0123456789ab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", good); err == nil || !strings.Contains(err.Error(), "deploy down") {
		t.Fatalf("endpoint set ran beside an application container: %v", err)
	}
	os.Remove(record + ".ps")
	if err := os.WriteFile(record+".fail", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", good); err == nil || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("endpoint set ran without proof the application is stopped: %v", err)
	}
	os.Remove(record + ".fail")
	assertOnlyChanged(t, before, installationFiles(t, dir))

	// A development installation's own Compose database is not an imported endpoint.
	dev := initDev(t)
	devBefore := installationFiles(t, dev)
	if _, err := invoke(t, "endpoint", "set", "--dir", dev, "--database-url-file", good); err == nil || !strings.Contains(err.Error(), "Compose PostgreSQL") {
		t.Fatalf("endpoint set replaced a development database: %v", err)
	}
	assertOnlyChanged(t, devBefore, installationFiles(t, dev))

	// The accepted path still works once every refusal has passed.
	if out, err := invoke(t, "endpoint", "set", "--dir", dir, "--database-url-file", good); err != nil {
		t.Fatalf("valid replacement refused: %v (%s)", err, out)
	}
}

// serverLeaf issues a self-signed leaf for app.internal with the given usages.
func serverLeaf(t *testing.T, usage []x509.ExtKeyUsage) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "app.internal"}, DNSNames: []string{"app.internal"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
}

func TestTLSReplaceAcceptsRenewedLeafAndRefusesMismatchedKey(t *testing.T) {
	dir := initProduction(t)
	record := replacementDocker(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	expiredCert, expiredKey := testTLSMaterial(t, true)
	for key, body := range map[string][]byte{"TLS_CERT": expiredCert, "TLS_KEY": expiredKey} {
		if err := os.WriteFile(filepath.Join(dir, c.TLSFiles[key]), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := invoke(t, "up", "--dir", dir); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired application leaf started: %v", err)
	}
	before := installationFiles(t, dir)

	renewedCert, renewedKey := serverLeaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	_, otherKey := serverLeaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	clientCert, clientKey := serverLeaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	certFile := writeInput(t, "app.pem", renewedCert, 0644)
	keyFile := writeInput(t, "app.key", renewedKey, 0600)
	caCert, caKey := testTLSMaterial(t, false)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"mismatched key":   {[]string{"--tls-cert", certFile, "--tls-key", writeInput(t, "other.key", otherKey, 0600)}, "valid pair"},
		"client-only leaf": {[]string{"--tls-cert", writeInput(t, "client.pem", clientCert, 0644), "--tls-key", writeInput(t, "client.key", clientKey, 0600)}, "serverAuth"},
		"still expired":    {[]string{"--tls-cert", writeInput(t, "old.pem", expiredCert, 0644), "--tls-key", writeInput(t, "old.key", expiredKey, 0600)}, "expired"},
		"readable key":     {[]string{"--tls-cert", certFile, "--tls-key", writeInput(t, "open.key", renewedKey, 0644)}, "mode 0600 or stricter"},
		"cert only":        {[]string{"--tls-cert", certFile}, "supplied together"},
		"upload key only":  {[]string{"--upload-key", keyFile}, "supplied together"},
		"nothing named":    {nil, "needs --tls-cert"},
		"private CA":       {[]string{"--tls-cert", certFile, "--tls-key", keyFile, "--upload-ca", writeInput(t, "upload-ca.pem", append(append([]byte{}, caCert...), caKey...), 0600)}, "without private keys"},
	} {
		out, err := invoke(t, append([]string{"tls", "replace", "--dir", dir}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want refusal %q, got %v (%s)", name, tc.want, err, out)
		}
		if strings.Contains(out+err.Error(), "PRIVATE KEY") {
			t.Fatalf("%s: refusal printed key material", name)
		}
		assertOnlyChanged(t, before, installationFiles(t, dir))
	}
	if err := os.WriteFile(record+".ps", []byte("0123456789ab\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "tls", "replace", "--dir", dir, "--tls-cert", certFile, "--tls-key", keyFile); err == nil || !strings.Contains(err.Error(), "deploy down") {
		t.Fatalf("TLS replaced beside an application container: %v", err)
	}
	os.Remove(record + ".ps")
	assertOnlyChanged(t, before, installationFiles(t, dir))

	out, err := invoke(t, "tls", "replace", "--dir", dir, "--tls-cert", certFile, "--tls-key", keyFile)
	if err != nil {
		t.Fatalf("renewed leaf refused: %v (%s)", err, out)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatal("tls replace printed key material")
	}
	after := installationFiles(t, dir)
	assertOnlyChanged(t, before, after, c.TLSFiles["TLS_CERT"], c.TLSFiles["TLS_KEY"])
	if !bytes.Equal(after[c.TLSFiles["TLS_CERT"]], renewedCert) || !bytes.Equal(after[c.TLSFiles["TLS_KEY"]], renewedKey) {
		t.Fatal("renewed pair was not installed")
	}
	if info, err := os.Lstat(filepath.Join(dir, c.TLSFiles["TLS_KEY"])); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement key mode: %v %v", info, err)
	}
	if out, err := invoke(t, "up", "--dir", dir); err != nil {
		t.Fatalf("up after renewal: %v (%s)", err, out)
	}
	if args, _ := os.ReadFile(record); !strings.Contains(string(args), "up -d --wait --wait-timeout 600") {
		t.Fatalf("up did not start the installation: %s", args)
	}

	// Replacement never adds material the installation does not already use.
	dev := initDev(t)
	devBefore := installationFiles(t, dev)
	if _, err := invoke(t, "tls", "replace", "--dir", dev, "--tls-cert", certFile, "--tls-key", keyFile); err == nil || !strings.Contains(err.Error(), "no TLS_CERT") {
		t.Fatalf("tls replace added TLS material: %v", err)
	}
	assertOnlyChanged(t, devBefore, installationFiles(t, dev))
}

func composeServices(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services
}

// assertOnlyServiceImage checks that every Compose service except target is
// byte-identical and that target differs only in its image.
func assertOnlyServiceImage(t *testing.T, before, after []byte, target, image string) {
	t.Helper()
	was, now := composeServices(t, before), composeServices(t, after)
	if len(was) != len(now) {
		t.Fatal("redeploy added or removed a service")
	}
	for name, body := range was {
		if name == target {
			var old, current map[string]any
			_ = json.Unmarshal(body, &old)
			_ = json.Unmarshal(now[name], &current)
			if current["image"] != image {
				t.Fatalf("%s image not replaced", name)
			}
			old["image"] = image
			a, _ := json.Marshal(old)
			b, _ := json.Marshal(current)
			if !bytes.Equal(a, b) {
				t.Fatalf("%s changed beyond its image", name)
			}
			continue
		}
		if !bytes.Equal(body, now[name]) {
			t.Fatalf("redeploy changed service %s", name)
		}
	}
}

func TestRedeployAdminTorImageOnly(t *testing.T) {
	dir := initAdminTor(t)
	record := replacementDocker(t)
	if out, err := invoke(t, "install", "--dir", dir); err != nil {
		t.Fatalf("install: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := installationFiles(t, dir)
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	next := "example.invalid/tor-admin@sha256:" + strings.Repeat("fedcba98", 8)
	for name, args := range map[string][]string{
		"both images": {"--app-image", testImage, "--admin-tor-image", next},
		"neither":     {},
		"floating":    {"--admin-tor-image", "example.invalid/tor-admin:latest"},
	} {
		if _, err := invoke(t, append([]string{"redeploy", "--dir", dir}, args...)...); err == nil {
			t.Fatalf("%s: ambiguous redeploy accepted", name)
		}
		assertOnlyChanged(t, before, installationFiles(t, dir))
	}
	out, err := invoke(t, "redeploy", "--dir", dir, "--admin-tor-image", next)
	if err != nil {
		t.Fatalf("redeploy admin Tor: %v (%s)", err, out)
	}
	after := installationFiles(t, dir)
	assertOnlyChanged(t, before, after, "operator.json", "compose.json")
	got, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.AdminTorImage != next {
		t.Fatal("administrator Tor image was not persisted")
	}
	got.AdminTorImage = c.AdminTorImage
	want, _ := json.Marshal(c)
	have, _ := json.Marshal(got)
	if !bytes.Equal(want, have) {
		t.Fatal("redeploy changed configuration beyond the administrator Tor image")
	}
	assertOnlyServiceImage(t, before["compose.json"], after["compose.json"], "tor-admin", next)
	args, _ := os.ReadFile(record)
	if strings.Count(string(args), "\n") != 1 || !strings.HasSuffix(string(args), "up -d --wait --wait-timeout 600 --no-deps tor-admin\n") {
		t.Fatalf("admin Tor redeploy touched other services: %s", args)
	}

	// An installation without private administrator Tor has nothing to replace.
	dev := initDev(t)
	if out, err := invoke(t, "install", "--dir", dev); err != nil {
		t.Fatalf("install: %v (%s)", err, out)
	}
	devBefore := installationFiles(t, dev)
	if _, err := invoke(t, "redeploy", "--dir", dev, "--admin-tor-image", next); err == nil || !strings.Contains(err.Error(), "administrator Tor") {
		t.Fatalf("admin Tor image added by redeploy: %v", err)
	}
	assertOnlyChanged(t, devBefore, installationFiles(t, dev))
}

func TestRedeployProductionTargetsOnlyApplication(t *testing.T) {
	dir := initProduction(t)
	record := replacementDocker(t)
	if out, err := invoke(t, "install", "--dir", dir); err != nil {
		t.Fatalf("install: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := installationFiles(t, dir)
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	next := "example.invalid/market@sha256:" + strings.Repeat("abcdef01", 8)
	if out, err := invoke(t, "redeploy", "--dir", dir, "--app-image", next); err != nil {
		t.Fatalf("redeploy: %v (%s)", err, out)
	}
	after := installationFiles(t, dir)
	assertOnlyChanged(t, before, after, "operator.json", "compose.json")
	got, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppImage != next {
		t.Fatal("desired app image was not persisted")
	}
	got.AppImage = c.AppImage
	want, _ := json.Marshal(c)
	have, _ := json.Marshal(got)
	if !bytes.Equal(want, have) {
		t.Fatal("production redeploy changed endpoints, keys, pins or storage bindings")
	}
	assertOnlyServiceImage(t, before["compose.json"], after["compose.json"], "app", next)
	args, _ := os.ReadFile(record)
	if strings.Count(string(args), "\n") != 1 || !strings.HasSuffix(string(args), "up -d --wait --wait-timeout 600 --no-deps app\n") {
		t.Fatalf("production redeploy touched other services: %s", args)
	}
}
