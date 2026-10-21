package operator

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testImage = "example.invalid/market@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func invoke(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := Run(context.Background(), args, strings.NewReader(""), &out, &errs)
	return out.String() + errs.String(), err
}

func initDev(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "install")
	if out, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--app-image", testImage); err != nil {
		t.Fatalf("init: %v (%s)", err, out)
	}
	return dir
}

func TestInitPrivateNoOverwrite(t *testing.T) {
	dir := initDev(t)
	key := filepath.Join(dir, "secrets", "app-secret-key")
	old, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(old))) != 64 {
		t.Fatalf("bad secret length")
	}
	for _, name := range []string{"operator.json", "secrets/app-secret-key", "secrets/setup-token"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode %o", name, info.Mode().Perm())
		}
	}
	if _, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--app-image", testImage); err == nil {
		t.Fatal("overwrote initialized directory")
	}
	now, _ := os.ReadFile(key)
	if !bytes.Equal(old, now) {
		t.Fatal("key rotated")
	}
}

func TestPlanRedactsAndNeverStartsDocker(t *testing.T) {
	dir := initDev(t)
	key, _ := os.ReadFile(filepath.Join(dir, "secrets/app-secret-key"))
	out, err := invoke(t, "plan", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, strings.TrimSpace(string(key))) {
		t.Fatal("plan leaked key")
	}
	if !strings.Contains(out, "development") || !strings.Contains(out, "db") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.json")); !os.IsNotExist(err) {
		t.Fatal("plan wrote runtime config")
	}
}

func TestInvalidInitWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bad")
	_, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--app-image", "example.invalid/market:latest")
	if err == nil {
		t.Fatal("floating image accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid init wrote state")
	}
}

func TestStrictConfigRejectsUnknownFields(t *testing.T) {
	dir := initDev(t)
	file := filepath.Join(dir, "operator.json")
	b, _ := os.ReadFile(file)
	b = bytes.Replace(b, []byte("{\n"), []byte("{\n\"unexpected\":true,"), 1)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "plan", "--dir", dir); err == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestProdRejectsDevDependenciesAndInsecureDatabase(t *testing.T) {
	for _, url := range []string{"postgres://user:secret@localhost/db?sslmode=verify-full", "postgres://user:secret@db.example/db?sslmode=require"} {
		t.Run(url, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "prod")
			f := filepath.Join(t.TempDir(), "dsn")
			_ = os.WriteFile(f, []byte(url), 0600)
			if _, err := invoke(t, "init", "--dir", dir, "--mode", "prod", "--app-image", testImage, "--database-url-file", f); err == nil {
				t.Fatal("unsafe production config accepted")
			}
		})
	}
}

func TestComposeDevHasNoPublishedDatabaseOrScanner(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	var compose map[string]any
	if err := json.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}
	services := compose["services"].(map[string]any)
	if _, ok := services["db"].(map[string]any)["ports"]; ok {
		t.Fatal("database published")
	}
	app := services["app"].(map[string]any)
	if app["read_only"] != true {
		t.Fatal("read-only app lost")
	}
	if !strings.Contains(string(data), "127.0.0.1:8080:8080") {
		t.Fatal("app isn't loopback")
	}
	env, _ := os.ReadFile(filepath.Join(dir, "secrets/database-url"))
	if strings.Contains(string(data), strings.TrimSpace(string(env))) {
		t.Fatal("compose embeds credentials")
	}
}

func TestConfigSymlinkAndBroadPermissionsRefused(t *testing.T) {
	dir := initDev(t)
	file := filepath.Join(dir, "operator.json")
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "plan", "--dir", dir); err == nil {
		t.Fatal("public config accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	other := filepath.Join(t.TempDir(), "config")
	_ = os.WriteFile(other, b, 0600)
	_ = os.Remove(file)
	_ = os.Symlink(other, file)
	if _, err := invoke(t, "plan", "--dir", dir); err == nil {
		t.Fatal("symlink config accepted")
	}
}

func TestDockerReceivesNoAmbientComposeAndRedactsCredentialFragments(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	dsn := "postgres://imported-user:secret%2Fpassword%21@database.example/market?sslmode=verify-full"
	if err := os.WriteFile(filepath.Join(dir, c.SecretFiles["DATABASE_URL"]), []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$@" > "$DOCKER_CONFIG"
if [ -n "$COMPOSE_FILE$DATABASE_URL$APP_SECRET_KEY$DOCKER_HOST$DOCKER_CONTEXT" ]; then echo 'ambient configuration leaked'; exit 22; fi
printf '%s\n' 'secret/password!' 'secret%2Fpassword%21' 'imported-user' 'secret%2Fpassword%21@database.example'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", argsFile)
	t.Setenv("COMPOSE_FILE", "untrusted.yaml")
	t.Setenv("DOCKER_HOST", "tcp://untrusted.example:2375")
	t.Setenv("DOCKER_CONTEXT", "untrusted-context")
	t.Setenv("DATABASE_URL", "ambient-secret")
	t.Setenv("APP_SECRET_KEY", "ambient-app-key")
	var out, errs bytes.Buffer
	if err := dockerCall(context.Background(), dir, c, &out, &errs, "logs", "--tail", "100"); err != nil {
		t.Fatalf("docker: %v (%s)", err, out.String())
	}
	for _, secret := range []string{"secret/password!", "secret%2Fpassword%21", "imported-user"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("credential fragment leaked: %s", out.String())
		}
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.HasPrefix(string(args), "--context\ndefault\n") {
		t.Fatalf("Docker context is implicit: %s", args)
	}
	if strings.Contains(string(args), "secret") || strings.Contains(string(args), "--volumes") {
		t.Fatalf("unsafe docker args: %s", args)
	}
}

func TestDockerTruncationDropsIncompleteSecretLine(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := `#!/bin/sh
printf 'safe first line\n'
i=0
while [ "$i" -lt 22000 ]; do printf 'secret-prefix-without-newline-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; i=$((i+1)); done
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errs bytes.Buffer
	if err := dockerCall(context.Background(), dir, c, &out, &errs, "logs"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "safe first line\n" {
		t.Fatalf("incomplete truncated line was exposed (%d bytes)", out.Len())
	}
	if !strings.Contains(errs.String(), "truncated") {
		t.Fatal("missing truncation notice")
	}
}

func TestPrivateMountedRuntimeFilesAndMaterializeKeepKeys(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, c.SecretFiles["APP_SECRET_KEY"]))
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, c.SecretFiles["APP_SECRET_KEY"]))
	if !bytes.Equal(key, after) {
		t.Fatal("resume rotated key")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.json"))
	if strings.Contains(string(b), string(bytes.TrimSpace(key))) {
		t.Fatal("Compose contains key value")
	}
	if !strings.Contains(string(b), "APP_SECRET_KEY_FILE") {
		t.Fatal("runtime file input missing")
	}
}

func TestInstallResumesWithoutRotatingKeysAndDownKeepsVolumes(t *testing.T) {
	dir := initDev(t)
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "commands")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_CONFIG"
if [ "$1" = --context ]; then shift 2; fi
case "$1" in ps|volume) exit 0;; esac
printf 'container operation complete\n'
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	before, _ := os.ReadFile(filepath.Join(dir, "secrets/app-secret-key"))
	for _, command := range []string{"install", "up", "status", "down"} {
		if out, err := invoke(t, command, "--dir", dir); err != nil {
			t.Fatalf("%s: %v (%s)", command, err, out)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, "secrets/app-secret-key"))
	if !bytes.Equal(before, after) {
		t.Fatal("resume rotated key")
	}
	commands, _ := os.ReadFile(record)
	if strings.Count(string(commands), "label=com.docker.compose.project=") != 2 {
		t.Fatalf("did not reuse durable project claim: %s", commands)
	}
	if strings.Contains(string(commands), "--volumes") || strings.Contains(string(commands), string(bytes.TrimSpace(before))) {
		t.Fatal("destructive flag or secret in command")
	}
}

func TestInstallRefusesExistingProjectVolumes(t *testing.T) {
	dir := initDev(t)
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = --context ]; then shift 2; fi
case "$1" in volume) echo existing_database_volume;; ps) exit 0;; *) exit 31;; esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := invoke(t, "install", "--dir", dir); err == nil {
		t.Fatal("existing volumes were reused")
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.json")); !os.IsNotExist(err) {
		t.Fatal("collision wrote runtime config")
	}
}

func TestProjectIdentityCannotChangeAfterInstall(t *testing.T) {
	dir := initDev(t)
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(receipt{Project: cfg.Project, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Project = "other-project"
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "plan", "--dir", dir); err == nil {
		t.Fatal("project identity change accepted")
	}
}

func TestMutationLockRefusesConcurrentCommands(t *testing.T) {
	dir := initDev(t)
	unlock, err := lockInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, "down", "--dir", dir); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("concurrent mutation accepted: %v", err)
	}
	unlock()
	unlock, err = lockInstallation(dir)
	if err != nil {
		t.Fatalf("released lock stayed stale: %v", err)
	}
	unlock()
}

func testTLSMaterial(t *testing.T, expired bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Hour)
	if expired {
		end = time.Now().Add(-time.Hour)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "app.internal"}, DNSNames: []string{"app.internal"}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
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

func TestExpiredTLSAllowsDiagnosticsAndStopButRefusesStart(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	cert, key := testTLSMaterial(t, true)
	for name, body := range map[string][]byte{"test-cert.pem": cert, "test-key.pem": key} {
		if err := os.WriteFile(filepath.Join(dir, "secrets", name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c.TLSFiles = map[string]string{"TLS_CERT": "secrets/test-cert.pem", "TLS_KEY": "secrets/test-key.pem"}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho diagnostic-or-stop\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"status", "logs", "down"} {
		if out, err := invoke(t, command, "--dir", dir); err != nil {
			t.Fatalf("expired TLS blocked %s: %v (%s)", command, err, out)
		}
	}
	for _, command := range []string{"plan", "install", "up", "doctor"} {
		if _, err := invoke(t, command, "--dir", dir); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("expired TLS accepted %s: %v", command, err)
		}
	}
}

func TestDependencyReportKeyIsPinnedOutsideReportDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "install")
	reports := t.TempDir()
	key := filepath.Join(t.TempDir(), "worker.pub")
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := invoke(t, "init", "--dir", dir, "--app-image", testImage, "--dependency-report-dir", reports, "--dependency-report-public-key", key); err != nil {
		t.Fatalf("init %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, env, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), reports+":/run/opsecmkt-dependencies:ro") {
		t.Fatal("report directory is not mounted read-only")
	}
	if env["DEPENDENCY_REPORT_PUBLIC_KEY"] == "/run/opsecmkt-dependencies/worker.pub" {
		t.Fatal("verification key trusted from writable report directory")
	}
	if err := os.WriteFile(key, []byte(strings.Repeat("cd", 32)), 0644); err != nil {
		t.Fatal(err)
	}
	pin, _ := os.ReadFile(filepath.Join(dir, c.PublicFiles["DEPENDENCY_REPORT_PUBLIC_KEY"]))
	if !strings.Contains(string(pin), strings.Repeat("ab", 32)) {
		t.Fatal("source-key replacement changed installed pin")
	}
}

func TestProductionPlanRejectsGatewayWalletCollocation(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Mode = "prod"
	c.PostgresImage = ""
	c.AppHost = "app.internal"
	c.GatewayURL = "https://gateway.internal"
	c.BackendBind = "10.0.0.10:8443"
	c.Settings["ESCROW_XMR"] = "on"
	for key, value := range map[string]string{"DATABASE_URL": "postgres://market:db-password@db.internal/market?sslmode=verify-full", "MONERO_WALLET_SECRET": strings.Repeat("0123456789abcdef", 4), "ESCROW_XMR_WALLET_RPC_URL": "https://escrow:password@gateway.internal/json_rpc", "ESCROW_XMR_DAEMON_URL": "https://daemon:password@monero.internal/json_rpc"} {
		name := "secrets/" + strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		c.SecretFiles[key] = name
	}
	c.PublicFiles = map[string]string{"ESCROW_XMR_SEED_KEY": "secrets/app-secret-key"}
	if err := validate(dir, c); err == nil || !strings.Contains(err.Error(), "separate") {
		t.Fatalf("gateway wallet collocation accepted: %v", err)
	}
}

func TestRedeployTargetsOnlyApplicationAndRetainsAllConfiguration(t *testing.T) {
	dir := initDev(t)
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "docker-args")
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_CONFIG"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	if out, err := invoke(t, "install", "--dir", dir); err != nil {
		t.Fatalf("install: %v (%s)", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, path := range c.SecretFiles {
		b, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		before[path] = b
	}
	for _, name := range []string{"installation.json", "postgres.env"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = b
	}
	if err := os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	nextImage := "example.invalid/market@sha256:" + strings.Repeat("abcdef01", 8)
	if out, err := invoke(t, "redeploy", "--dir", dir, "--app-image", nextImage); err != nil {
		t.Fatalf("redeploy: %v (%s)", err, out)
	}
	after, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.AppImage != nextImage {
		t.Fatal("desired app image was not persisted")
	}
	after.AppImage = c.AppImage
	want, _ := json.Marshal(c)
	got, _ := json.Marshal(after)
	if !bytes.Equal(want, got) {
		t.Fatal("redeploy changed endpoints, keys, project, settings or worker images")
	}
	for path, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("redeploy changed %s", path)
		}
	}
	args, _ := os.ReadFile(record)
	if strings.Count(string(args), "\n") != 1 || !strings.HasSuffix(string(args), "up -d --wait --wait-timeout 600 --no-deps app\n") {
		t.Fatalf("redeploy touched worker services: %s", args)
	}
	configBefore, _ := os.ReadFile(filepath.Join(dir, "operator.json"))
	composeBefore, _ := os.ReadFile(filepath.Join(dir, "compose.json"))
	if _, err := invoke(t, "redeploy", "--dir", dir, "--app-image", "example.invalid/market:latest"); err == nil {
		t.Fatal("floating redeploy image accepted")
	}
	configAfter, _ := os.ReadFile(filepath.Join(dir, "operator.json"))
	composeAfter, _ := os.ReadFile(filepath.Join(dir, "compose.json"))
	if !bytes.Equal(configBefore, configAfter) || !bytes.Equal(composeBefore, composeAfter) {
		t.Fatal("invalid redeploy changed persisted state")
	}
}

func TestRedeployFailureRetainsDesiredImageWithoutRollback(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	if err := os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 44\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	next := "example.invalid/market@sha256:" + strings.Repeat("12345678", 8)
	if _, err := invoke(t, "redeploy", "--dir", dir, "--app-image", next); err == nil {
		t.Fatal("failed restart reported success")
	}
	after, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.AppImage != next {
		t.Fatal("failed redeploy silently reverted desired image")
	}
}

func initProduction(t *testing.T) string {
	t.Helper()
	dir, args := productionInitArgs(t, "database-ca.pem")
	if out, err := invoke(t, args...); err != nil {
		t.Fatalf("init %v (%s)", err, out)
	}
	return dir
}

// productionInitArgs prepares production inputs. The database CA is its own
// certificate ("database-ca.pem"); passing "ca.pem" reuses the upload CA.
func productionInitArgs(t *testing.T, databaseCA string) (string, []string) {
	t.Helper()
	fakeEncryptedProductionHost(t)
	inputs := t.TempDir()
	cert, key := testTLSMaterial(t, false)
	dbCA, _ := testTLSMaterial(t, false)
	for name, body := range map[string][]byte{"database.url": []byte("postgres://market:dedicated-password@db.internal/market?sslmode=verify-full"), "ca.pem": cert, "database-ca.pem": dbCA, "server.pem": cert, "server.key": key} {
		if err := os.WriteFile(filepath.Join(inputs, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "production")
	return dir, []string{"init", "--dir", dir, "--mode", "prod", "--app-image", testImage, "--app-host", "app.internal", "--gateway-url", "https://gateway.internal", "--database-url-file", filepath.Join(inputs, "database.url"), "--database-ca", filepath.Join(inputs, databaseCA), "--tls-cert", filepath.Join(inputs, "server.pem"), "--tls-key", filepath.Join(inputs, "server.key"), "--upload-ca", filepath.Join(inputs, "ca.pem"), "--upload-cert", filepath.Join(inputs, "server.pem"), "--upload-key", filepath.Join(inputs, "server.key")}
}

// A-589: UPLOAD_CA must be dedicated to the upload backend and gateway. Setup
// refuses one that shares a certificate with the database CA, and validation
// refuses a share with MONERO_RPC_CA too (including inside a bundle), naming
// the keys and never the file contents.
func TestProductionRefusesUploadCASharedWithDatabaseOrMoneroCA(t *testing.T) {
	dir, args := productionInitArgs(t, "ca.pem")
	out, err := invoke(t, args...)
	if err == nil || err.Error() != errSharedUploadCA.Error() || strings.Contains(out+err.Error(), "BEGIN CERTIFICATE") {
		t.Fatalf("shared upload/database CA: %v (%s)", err, out)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("refused setup left an installation behind")
	}

	upload, _ := testTLSMaterial(t, false)
	other, _ := testTLSMaterial(t, false)
	unrelated, _ := testTLSMaterial(t, false)
	bundle := append(append([]byte{}, other...), upload...)
	for _, tc := range []struct {
		name            string
		upload, monero  []byte
		database        []byte
		wantSharedError bool
	}{
		{"monero shares a bundle member", bundle, upload, nil, true},
		{"database shares a bundle member", bundle, nil, other, true},
		{"database bundle shares one member", upload, nil, append(append([]byte{}, unrelated...), upload...), true},
		{"dedicated upload CA", bundle, unrelated, unrelated, false},
		{"no upload CA", nil, upload, upload, false},
	} {
		err := RefuseSharedUploadCA(tc.upload, tc.monero, tc.database)
		if tc.wantSharedError != (err == errSharedUploadCA) || !tc.wantSharedError && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestProductionInitializationAndPlanPreserveRemoteTLSAndPins(t *testing.T) {
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	compose, env, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if env["UPLOAD_MODE"] != "isolated" || env["OPSECMKT_PROFILE"] != "prod" || env["TLS_CERT"] == "" {
		t.Fatal("production runtime guards absent")
	}
	var object map[string]any
	if err := json.Unmarshal(compose, &object); err != nil {
		t.Fatal(err)
	}
	services := object["services"].(map[string]any)
	if len(services) != 1 {
		t.Fatal("production provisioned a dependency on the app host")
	}
	app := services["app"].(map[string]any)
	logging := app["logging"].(map[string]any)
	if logging["driver"] != "none" || len(logging) != 1 {
		t.Fatal("production application retains persistent container logging or invalid log options")
	}
	if _, ok := app["healthcheck"]; !ok {
		t.Fatal("TLS production has no readiness check")
	}
	dsn, err := secretValue(dir, c, "DATABASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "sslrootcert=%2Frun%2Fopsecmkt-secrets%2Fdatabase-ca.pem") {
		t.Fatal("database CA was not mapped to its runtime path")
	}
	if out, err := invoke(t, "plan", "--dir", dir); err != nil || strings.Contains(out, "dedicated-password") {
		t.Fatalf("plan %v (%s)", err, out)
	}
}

func TestProductionLogsExplainDisabledLoggingWithoutCallingDocker(t *testing.T) {
	dir := initProduction(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 44\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := invoke(t, "logs", "--dir", dir)
	if err != nil || !strings.Contains(out, "persistent container logs are disabled") || !strings.Contains(out, "administrator diagnostics") {
		t.Fatalf("production logs invoked Docker or omitted guidance: %v (%s)", err, out)
	}
}

func TestInitialAccessSettingsPersistRenderAndDescribeDefaults(t *testing.T) {
	for _, policy := range []struct {
		name, siteAccess, registrations string
		flags                           []string
	}{
		{"defaults", "public", "open", nil},
		{"members-open", "members", "open", []string{"--site-access", "members"}},
		{"public-closed", "public", "closed", []string{"--registrations", "closed"}},
		{"members-closed", "members", "closed", []string{"--site-access", "members", "--registrations", "closed"}},
	} {
		t.Run(policy.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "install")
			args := append([]string{"init", "--dir", dir, "--mode", "dev", "--app-image", testImage}, policy.flags...)
			if output, err := invoke(t, args...); err != nil {
				t.Fatalf("init: %v (%s)", err, output)
			}
			c, err := loadConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, env, err := render(dir, c)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"INITIAL_SITE_ACCESS": policy.siteAccess, "INITIAL_REGISTRATIONS": policy.registrations} {
				if c.Settings[key] != want || env[key] != want {
					t.Fatalf("%s: saved=%q rendered=%q want=%q", key, c.Settings[key], env[key], want)
				}
			}
			plan, err := invoke(t, "plan", "--dir", dir)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, "initial visitor access: "+policy.siteAccess+"; new registrations: "+policy.registrations+" (saved administrator settings take precedence)") {
				t.Fatal("plan did not describe first-start defaults:", plan)
			}
		})
	}
}

func TestInitialAccessSettingsRejectInvalidValuesWithoutPublishing(t *testing.T) {
	for _, setting := range []struct {
		flag, key, value string
	}{
		{"site-access", "INITIAL_SITE_ACCESS", "PUBLIC"},
		{"site-access", "INITIAL_SITE_ACCESS", ""},
		{"site-access", "INITIAL_SITE_ACCESS", "members "},
		{"registrations", "INITIAL_REGISTRATIONS", "enabled"},
		{"registrations", "INITIAL_REGISTRATIONS", ""},
		{"registrations", "INITIAL_REGISTRATIONS", "closed "},
	} {
		for _, useSet := range []bool{false, true} {
			name := setting.flag + "=" + setting.value
			if useSet {
				name += "/set"
			}
			t.Run(name, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "install")
				args := []string{"init", "--dir", dir, "--mode", "dev", "--app-image", testImage}
				if useSet {
					args = append(args, "--set", setting.key+"="+setting.value)
				} else {
					args = append(args, "--"+setting.flag, setting.value)
				}
				if _, err := invoke(t, args...); err == nil || !strings.Contains(err.Error(), setting.key+" must be") {
					t.Fatalf("invalid policy accepted or wrong error: %v", err)
				}
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Fatal("invalid policy published installation")
				}
			})
		}
	}
}

func TestOlderOperatorConfigWithoutInitialAccessSettingsRemainsValid(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	delete(c.Settings, "INITIAL_SITE_ACCESS")
	delete(c.Settings, "INITIAL_REGISTRATIONS")
	if _, _, err := render(dir, c); err != nil {
		t.Fatalf("existing configuration became invalid: %v", err)
	}
	var plan bytes.Buffer
	if err := printPlan(&plan, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "initial visitor access: public; new registrations: open") {
		t.Fatal(plan.String())
	}
}

func TestBrandingUploadOriginInitializationRenderingAndPlan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "install")
	if output, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--app-image", testImage, "--branding-upload-origin", "https://UPLOAD.MARKET.EXAMPLE:443"); err != nil {
		t.Fatalf("init: %v (%s)", err, output)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, env, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings["BRANDING_UPLOAD_ORIGIN"] != "https://upload.market.example" || env["BRANDING_UPLOAD_ORIGIN"] != "https://upload.market.example" {
		t.Fatal("fixed origin not normalized, persisted and forwarded")
	}
	plan, err := invoke(t, "plan", "--dir", dir)
	if err != nil || !strings.Contains(plan, "branding upload origin: https://upload.market.example") {
		t.Fatalf("origin missing from plan: %v %s", err, plan)
	}
	c.Settings["BRANDING_UPLOAD_ORIGIN"] = "http://127.0.0.1:8080"
	c.Mode = "prod"
	if err := validate(dir, c); err == nil || !strings.Contains(err.Error(), "BRANDING_UPLOAD_ORIGIN") {
		t.Fatalf("production accepted HTTP loopback: %v", err)
	}
}

func TestBrandingUploadOriginInitRejectsInvalidFlagsAndSetWithoutWriting(t *testing.T) {
	for _, flags := range [][]string{{"--branding-upload-origin", "http://public.example"}, {"--branding-upload-origin", "https://user:private-value@public.example"}, {"--set", "BRANDING_UPLOAD_ORIGIN=https://public.example/path"}} {
		dir := filepath.Join(t.TempDir(), "install")
		args := append([]string{"init", "--dir", dir, "--mode", "dev", "--app-image", testImage}, flags...)
		if output, err := invoke(t, args...); err == nil || !strings.Contains(err.Error(), "BRANDING_UPLOAD_ORIGIN") || strings.Contains(output+err.Error(), "private-value") {
			t.Fatalf("invalid origin not safely refused: %v %s", err, output)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid origin wrote installation")
		}
	}
}

// A-538: monerod and monero-wallet-rpc keep their own log files besides stdout, and Docker's json-file rotation does
// not reach them. The development nodes cap both: monerod's in its data volume at three 10 MiB files (its defaults
// keep 50 of 100 MB), the wallet's on its tmpfs at two 1 MiB files.
func TestDevMoneroNodeLogFilesAreCapped(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.MoneroImage, c.MoneroWalletImage = testImage, testImage
	c.Settings["ESCROW_XMR"] = "on"
	for key, value := range map[string]string{"MONERO_WALLET_SECRET": strings.Repeat("0123456789abcdef", 4), "ESCROW_XMR_WALLET_RPC_URL": "http://escrow:password@monero-wallet:18083/json_rpc", "ESCROW_XMR_DAEMON_URL": "http://monero:18081/json_rpc", "ESCROW_XMR_WALLET_RPC_PASSWORD": "wallet-password"} {
		name := "secrets/" + strings.ToLower(strings.ReplaceAll(key, "_", "-"))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		c.SecretFiles[key] = name
	}
	_, public := recoveryKey(t)
	seed, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets/escrow-xmr-seed-key.asc"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	c.PublicFiles = map[string]string{"ESCROW_XMR_SEED_KEY": "secrets/escrow-xmr-seed-key.asc"}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Command []string       `json:"command"`
			Logging map[string]any `json:"logging"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}
	daemon := compose.Services["monero"]
	counts := map[string]int{}
	for _, argument := range daemon.Command {
		name, _, _ := strings.Cut(argument, "=")
		counts[name]++
	}
	for _, flag := range []string{"--max-log-file-size=10485760", "--max-log-files=3"} {
		name, _, _ := strings.Cut(flag, "=")
		if !strings.Contains(" "+strings.Join(daemon.Command, " ")+" ", " "+flag+" ") || counts[name] != 1 {
			t.Fatalf("monerod command lacks exactly one %s: %q", flag, daemon.Command)
		}
	}
	if counts["--log-file"] != 0 || counts["--log-level"] != 0 {
		t.Fatalf("monerod log file or level moved from its capped default: %q", daemon.Command)
	}
	for _, name := range []string{"monero", "monero-wallet"} {
		if options, _ := compose.Services[name].Logging["options"].(map[string]any); options["max-size"] != "10m" || options["max-file"] != "3" {
			t.Fatalf("%s lost Docker log rotation: %v", name, compose.Services[name].Logging)
		}
	}
	wallet, err := os.ReadFile(filepath.Join(dir, "monero-wallet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"log-file=/tmp/monero-wallet-rpc.log", "max-log-file-size=1048576", "max-log-files=2"} {
		if !strings.Contains("\n"+string(wallet), "\n"+line+"\n") {
			t.Fatalf("wallet config lacks %s", line)
		}
	}
}
