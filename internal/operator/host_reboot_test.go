package operator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func hostRebootFixture(t *testing.T) (string, Config) {
	t.Helper()
	dir := initProduction(t)
	c, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.AdminTorImage, c.AdminAuthDir, c.AdminSocketDir = testImage, "admin-clients", filepath.Join(dir, "admin")
	c.Exposure = "tor"
	if err := importAdminAuth(adminAuthInput(t), filepath.Join(dir, c.AdminAuthDir)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(c)
	if err := atomicWrite(filepath.Join(dir, "operator.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

func TestHostRebootConfigureChecksStorageAndNeverMarksReady(t *testing.T) {
	dir, c := hostRebootFixture(t)
	previousInstall, previousReady := installHostRebootService, setHostRebootReady
	t.Cleanup(func() { installHostRebootService, setHostRebootReady = previousInstall, previousReady })
	installed, ready := 0, 0
	installHostRebootService = func(_ context.Context, gotDir string, got Config) error {
		installed++
		if gotDir != dir || got.HostControlDir != hostControlSocketDir(c.Project) {
			t.Fatal("wrong host agent target")
		}
		return nil
	}
	setHostRebootReady = func(string, Config) error { ready++; return nil }
	productionStorageProbe = func(context.Context, string, Config, []string) ([]StorageBinding, error) {
		return nil, errors.New("changed encrypted root")
	}
	if _, err := invoke(t, "host-reboot", "configure", "--dir", dir); err == nil || installed != 0 {
		t.Fatal("configured on changed storage", err)
	}
	productionStorageProbe = func(_ context.Context, _ string, got Config, _ []string) ([]StorageBinding, error) {
		paths := append([]string{"/", dir, c.RetentionDir}, hostControlStoragePaths(got)...)
		var pins []StorageBinding
		for _, p := range paths {
			pins = append(pins, testStorageBinding(p))
		}
		return pins, nil
	}
	if _, err := invoke(t, "host-reboot", "configure", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	if installed != 1 || ready != 0 {
		t.Fatalf("configure installed=%d ready=%d", installed, ready)
	}
	after, err := loadConfig(dir)
	if err != nil || after.HostControlDir != hostControlSocketDir(c.Project) {
		t.Fatal(err)
	}
	for _, path := range hostControlStoragePaths(after) {
		found := false
		for _, pin := range after.StorageBindings {
			if pin.Path == path {
				found = true
			}
		}
		if !found {
			t.Fatalf("host ledger/policy not pinned: %s", path)
		}
	}
	if _, err := invoke(t, "host-reboot", "ready", "--dir", dir); err == nil || ready != 0 {
		t.Fatal("readiness lacked explicit rehearsal acknowledgement")
	}
	if _, err := invoke(t, "host-reboot", "ready", "--dir", dir, "--confirm-unlock-rehearsed"); err != nil || ready != 1 {
		t.Fatal("explicit readiness not recorded", err)
	}
}

func TestHostRebootSocketIsProjectScopedAndAppOnly(t *testing.T) {
	dir, c := hostRebootFixture(t)
	c.HostControlDir = hostControlSocketDir(c.Project)
	data, env, err := render(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if env["HOST_CONTROL_SOCKET"] != "/run/opsecmkt-host-control/control.sock" {
		t.Fatal("socket env absent")
	}
	if !strings.Contains(string(data), `"create_host_path": false`) {
		t.Fatal("Docker may fabricate a missing host-agent socket directory")
	}
	var obj struct {
		Services map[string]map[string]any `json:"services"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	for name, s := range obj.Services {
		b, _ := json.Marshal(s)
		if strings.Contains(string(b), c.HostControlDir) != (name == "app") {
			t.Fatalf("socket exposed to wrong service %s", name)
		}
		for _, bad := range []string{"docker.sock", "/run/systemd", "privileged"} {
			if strings.Contains(string(b), bad) {
				t.Fatalf("broad host authority: %s", bad)
			}
		}
	}
	for _, bad := range []string{"/run/opsecmkt-host-control-other", "/run", "/var/run/control", c.HostControlDir + "/../other"} {
		c.HostControlDir = bad
		if err := validate(dir, c); err == nil {
			t.Fatalf("accepted non-project socket %s", bad)
		}
	}
}

func TestHostRebootUnitHasFixedCapabilityAndSafeArguments(t *testing.T) {
	c := Config{Project: "market", RuntimeUID: 10001, RuntimeGID: 10001, HostControlDir: hostControlSocketDir("market")}
	unit, err := hostRebootUnit("/usr/local/bin/opsecmkt", "/srv/market with spaces", c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"host-control-agent --socket", `--deployment-dir "/srv/market with spaces"`, "Before=docker.service", "Type=notify", "NotifyAccess=main", "User=root", "Group=root", "--app-gid 10001", "StateDirectoryMode=0700", "RestrictAddressFamilies=AF_UNIX", "LimitCORE=0"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("missing %s", want)
		}
	}
	for _, bad := range []string{"RuntimeDirectory=", "PrivateDevices=", "ProtectSystem=", "ReadWritePaths=", "/bin/sh", "docker.sock", "ExecStart=/usr/bin/systemctl reboot"} {
		if strings.Contains(unit, bad) {
			t.Fatalf("unsafe service: %s", bad)
		}
	}
	for _, dir := range []string{"/srv/%H", "/srv/$USER", "/srv/market\nInjected=yes"} {
		if _, err := hostRebootUnit("/usr/local/bin/opsecmkt", dir, c); err == nil {
			t.Fatalf("accepted unit substitution %q", dir)
		}
	}
}
