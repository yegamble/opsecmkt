package operator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validAdminAuth = "descriptor:x25519:N2NU7BSRL6YODZCYPN4CREB54TYLKGIE2KYOQWLFYC23ZJVCE5DQ\n"

func adminAuthInput(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "operator.auth"), []byte(validAdminAuth), 0600); err != nil {
		t.Fatal(err)
	}
	return d
}
func initAdminTor(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "admin-install")
	if out, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--exposure", "tor", "--app-image", testImage, "--tor-image", testImage, "--admin-tor-image", testImage, "--admin-client-auth-dir", adminAuthInput(t)); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	return dir
}
func TestAdminTorUsesValidatedPublicSnapshotAndPrivateSocket(t *testing.T) {
	dir := initAdminTor(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionDir != filepath.Join(dir, "retention") || c.AdminSocketDir != filepath.Join(dir, "admin") || c.AdminAuthDir != "admin-clients" {
		t.Fatalf("unexpected managed paths: %#v", c)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	b, _, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil {
		t.Fatal("invalid compose")
	}
	services := obj["services"].(map[string]any)
	admin := services["tor-admin"].(map[string]any)
	app := services["app"].(map[string]any)
	if app["environment"].(map[string]any)["ADMIN_SOCKET"] != "/run/market-admin/http.sock" {
		t.Fatal("private admin listener absent")
	}
	if _, ok := admin["ports"]; ok {
		t.Fatal("admin service published a TCP port")
	}
	if got := admin["networks"].([]any); len(got) != 1 || got[0] != "admin_tor_egress" {
		t.Fatalf("admin joined application networks: %#v", got)
	}
	if got := admin["user"]; got != numericID(c.RuntimeUID, c.RuntimeGID) {
		t.Fatalf("Tor runtime IDs differ from socket owner: %#v", got)
	}
	mounts, _ := json.Marshal(admin["volumes"])
	for _, bad := range []string{"secrets", "postgres", "monero", "retention"} {
		if strings.Contains(string(mounts), bad) {
			t.Fatalf("admin mount exposes %s: %s", bad, mounts)
		}
	}
	for _, want := range []string{"/run/market-admin:ro", "tor_admin_identity:/var/lib/tor"} {
		if !strings.Contains(string(mounts), want) {
			t.Fatalf("missing mount %q: %s", want, mounts)
		}
	}
	if strings.Contains(string(mounts), "admin-clients") {
		t.Fatal("Tor receives mutable authorization source rather than its prepared volume")
	}
	prepare := services["admin-tor-prepare"].(map[string]any)
	if prepare["network_mode"] != "none" || prepare["user"] != "0:0" || prepare["restart"] != "no" || prepare["read_only"] != true {
		t.Fatalf("unsafe prepare privileges: %#v", prepare)
	}
	if _, exists := prepare["networks"]; exists {
		t.Fatal("prepare helper joined a network")
	}
	prepareMounts := prepare["volumes"].([]any)
	if len(prepareMounts) != 3 || prepareMounts[0] != filepath.Join(dir, c.AdminAuthDir)+":/run/tor-admin-clients:ro" || prepareMounts[1] != "tor_admin_identity:/var/lib/tor" || prepareMounts[2] != filepath.Join(dir, c.AdminOnionDir)+":/run/tor-admin-identity:ro" {
		t.Fatalf("prepare helper sees more than authorization, selected identity and destination: %#v", prepareMounts)
	}
	if got := admin["depends_on"].(map[string]any)["admin-tor-prepare"].(map[string]any)["condition"]; got != "service_completed_successfully" {
		t.Fatal("Tor can start before validated authorization is prepared")
	}
	config, err := os.ReadFile(filepath.Join(dir, "torrc-admin"))
	if err != nil || !strings.Contains(string(config), "HiddenServicePort 80 unix:/run/market-admin/http.sock") || strings.Contains(string(config), "app:8080") {
		t.Fatalf("unsafe admin config: %s %v", config, err)
	}
	// Control access stays in Tor's private DataDirectory, with no TCP listener
	// or group-readable authentication material.
	settings := map[string]string{}
	for _, line := range strings.Split(string(config), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			if _, duplicate := settings[fields[0]]; duplicate {
				t.Fatalf("duplicate Tor setting: %s", fields[0])
			}
			settings[fields[0]] = fields[1]
		}
	}
	for option, want := range map[string]string{
		"ControlPort": "0", "ControlSocket": "/tmp/tor/control.sock",
		"ControlSocketsGroupWritable": "0", "CookieAuthentication": "1",
		"CookieAuthFile": "/tmp/tor/control_auth_cookie", "CookieAuthFileGroupReadable": "0",
	} {
		if settings[option] != want {
			t.Fatalf("unsafe or missing control option %s", option)
		}
	}
	info, err := os.Stat(c.AdminSocketDir)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("socket directory mode: %v %v", info, err)
	}
}
func TestAdminTorImportRefusesMalformedAllowlist(t *testing.T) {
	for _, name := range []string{"empty", "malformed", "private", "noncanonical", "symlink", "hidden", "extra"} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			body := []byte(validAdminAuth)
			filename := "operator.auth"
			switch name {
			case "empty":
				body = nil
			case "malformed":
				body = []byte("descriptor:x25519:invalid\n")
			case "private":
				body = []byte("CLIENT_PRIVATE_KEY_DO_NOT_IMPORT")
			case "noncanonical":
				body = []byte(strings.TrimSuffix(validAdminAuth, "Q\n") + "R\n")
			case "hidden":
				filename = ".auth"
			case "extra":
				filename = "notes.txt"
			}
			if body != nil {
				if err := os.WriteFile(filepath.Join(source, filename), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "symlink" {
				target := filepath.Join(t.TempDir(), "key")
				os.WriteFile(target, body, 0600)
				os.Remove(filepath.Join(source, filename))
				if err := os.Symlink(target, filepath.Join(source, filename)); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(t.TempDir(), "bad")
			if _, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--exposure", "tor", "--app-image", testImage, "--tor-image", testImage, "--admin-tor-image", testImage, "--admin-client-auth-dir", source); err == nil {
				t.Fatal("invalid auth accepted")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid auth published installation")
			}
		})
	}
}
func TestTamperedAdminSnapshotPreventsInstall(t *testing.T) {
	dir := initAdminTor(t)
	if err := os.Chmod(filepath.Join(dir, "admin-clients", "operator.auth"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "admin-clients", "operator.auth"), []byte("bad record"), 0444); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "docker-called")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf called > \"$DOCKER_CONFIG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	if _, err := invoke(t, "install", "--dir", dir); err == nil {
		t.Fatal("started with invalid admin auth")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("invalid auth reached Docker")
	}
}
func TestRetentionDirectoryCanRemainOutsideInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "install")
	ledger := filepath.Join(t.TempDir(), "ledger")
	if out, err := invoke(t, "init", "--dir", dir, "--mode", "dev", "--app-image", testImage, "--retention-dir", ledger); err != nil {
		t.Fatalf("init %v %s", err, out)
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionDir != ledger {
		t.Fatal("external retention lost")
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	b, _, err := render(dir, c)
	if err != nil || !strings.Contains(string(b), ledger+":/var/lib/opsecmkt-retention") {
		t.Fatalf("retention mount: %s %v", b, err)
	}
}
func TestEmergencyStopPersistsMarkerAndKeepsVolumes(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	receiptBytes, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	os.WriteFile(filepath.Join(dir, "installation.json"), receiptBytes, 0600)
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "commands")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CONFIG\"\n"
	os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	if err := EmergencyStop(context.Background(), dir, EmergencyTarget{Project: c.Project, DockerContext: c.DockerContext}, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(record)
	if !strings.Contains(string(b), " kill\n") || !strings.Contains(string(b), " down --timeout 0 --remove-orphans\n") || !strings.Contains(string(b), " ps -aq --filter label=com.docker.compose.project="+c.Project) || strings.Contains(string(b), "--volumes") {
		t.Fatalf("unsafe emergency commands: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "emergency.json")); err != nil {
		t.Fatal("emergency stop not durable")
	}
	if _, err := invoke(t, "up", "--dir", dir); err == nil || !strings.Contains(err.Error(), "emergency") {
		t.Fatalf("restart permitted: %v", err)
	}
}

func TestProductionTorRequiresProtectedAdminAndKeepsItOffBackend(t *testing.T) {
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Exposure = "tor"
	if err := validate(dir, c); err == nil || !strings.Contains(err.Error(), "protected private administrator") {
		t.Fatalf("production Tor accepted unprotected admin: %v", err)
	}
	c.AdminTorImage, c.AdminAuthDir, c.AdminSocketDir = testImage, "admin-clients", filepath.Join(dir, "admin")
	if err := importAdminAuth(adminAuthInput(t), filepath.Join(dir, c.AdminAuthDir)); err != nil {
		t.Fatal(err)
	}
	b, env, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if env["ADMIN_SOCKET"] == "" || env["APP_MODE"] != "tor" {
		t.Fatal("private application ingress absent")
	}
	var object map[string]any
	json.Unmarshal(b, &object)
	services := object["services"].(map[string]any)
	if len(services) != 3 {
		t.Fatalf("production provisioned other workers: %#v", services)
	}
	admin := services["tor-admin"].(map[string]any)
	if admin["logging"].(map[string]any)["driver"] != "none" {
		t.Fatal("private admin retains persistent Docker logs")
	}
}
func TestUnpackedPublicAdminSnapshotGetsTorReadablePermissions(t *testing.T) {
	dir := initAdminTor(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(dir, c.AdminAuthDir)
	if err := os.Chmod(auth, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(auth, "operator.auth"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{auth: 0755, filepath.Join(auth, "operator.auth"): 0444} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("public Tor auth not readable: %s %v %v", path, info, err)
		}
	}
}
func TestRAMInstallationRequiresExternalPersistentRetention(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := operatorIsTmpfs
	defer func() { operatorIsTmpfs = saved }()
	operatorIsTmpfs = func(path string) (bool, error) { return path == dir, nil }
	if checkRetentionStorage(dir, c) == nil {
		t.Fatal("RAM installation retained ledger inside RAM")
	}
	c.RetentionDir = filepath.Join(t.TempDir(), "durable-ledger")
	if err := checkRetentionStorage(dir, c); err != nil {
		t.Fatal(err)
	}
	operatorIsTmpfs = func(string) (bool, error) { return true, nil }
	if checkRetentionStorage(dir, c) == nil {
		t.Fatal("tmpfs ledger accepted")
	}
}
func TestEmergencyStopWorksWithMissingKeysAndFailureBlocksRestart(t *testing.T) {
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
	key := filepath.Join(dir, c.SecretFiles["APP_SECRET_KEY"])
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if project, err := InstallationProject(dir); err != nil || project != c.Project {
		t.Fatalf("missing key blocked identity: %q %v", project, err)
	}
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "commands")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_CONFIG\"\nexit 44\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", record)
	if EmergencyStop(context.Background(), dir, EmergencyTarget{Project: c.Project, DockerContext: c.DockerContext}, os.Stdout, os.Stderr) == nil {
		t.Fatal("failed emergency stop reported success")
	}
	b, err := os.ReadFile(record)
	if err != nil || !strings.HasSuffix(string(b), " kill\n") || strings.Contains(string(b), " down ") {
		t.Fatalf("unexpected stop sequence: %s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "emergency.json")); err != nil {
		t.Fatal("failed stop lost restart marker")
	}
	if err := refuseEmergencyRestart(dir); err == nil {
		t.Fatal("marker permits restart")
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("emergency stop regenerated missing key")
	}
}
func TestEmergencyIdentityRefusesMissingOrMismatchedReceipt(t *testing.T) {
	dir := initDev(t)
	if _, err := InstallationProject(dir); err == nil {
		t.Fatal("unclaimed installation accepted")
	}
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(receipt{Project: c.Project + "-different", DockerContext: c.DockerContext, Version: 1})
	os.WriteFile(filepath.Join(dir, "installation.json"), b, 0600)
	if _, err := InstallationProject(dir); err == nil {
		t.Fatal("mismatched project receipt accepted")
	}
}

func TestEmergencyStopChecksConfirmedTargetUnderLockAndResidualContainers(t *testing.T) {
	dir := initDev(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(dir, c); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(receipt{Project: c.Project, DockerContext: c.DockerContext, Version: 1})
	os.WriteFile(filepath.Join(dir, "installation.json"), b, 0600)
	target, err := InstallationTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrong := target
	wrong.Project += "-changed"
	if err := EmergencyStop(context.Background(), dir, wrong, os.Stdout, os.Stderr); err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("changed target accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "emergency.json")); !os.IsNotExist(err) {
		t.Fatal("target mismatch mutated state")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *' ps -aq '*) printf 'lingering-project-container\\n';; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := EmergencyStop(context.Background(), dir, target, os.Stdout, os.Stderr); err == nil || !strings.Contains(err.Error(), "every container") {
		t.Fatalf("residual containers accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "emergency.json")); err != nil {
		t.Fatal("failed residual check removed stop marker")
	}
}
