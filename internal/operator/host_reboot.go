package operator

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"opsecmkt/internal/hostcontrol"
)

var installHostRebootService = ensureHostRebootService
var setHostRebootReady = writeHostRebootReadiness

func hostControlSocketDir(project string) string { return "/run/opsecmkt-host-control-" + project }
func hostControlStateDir(project string) string  { return "/var/lib/opsecmkt-host-control/" + project }
func hostControlPolicyFile(project string) string {
	return "/etc/opsecmkt-host-control/" + project + ".json"
}

func hostControlStoragePaths(c Config) []string {
	if c.HostControlDir == "" {
		return nil
	}
	return []string{hostControlStateDir(c.Project), hostControlPolicyFile(c.Project), filepath.Join(hostControlStateDir(c.Project), hostcontrol.LedgerFileName)}
}

func runHostReboot(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprintln(out, "usage: opsecmkt deploy host-reboot configure|ready --dir DIR\nconfigure installs a disabled private app-host agent on an encrypted Linux host.\nRun deploy up afterwards to attach its socket to the application.\nready requires --confirm-unlock-rehearsed after a real OS reboot/unlock recovery rehearsal.\nNeither command requests a reboot or changes disk keys.")
		return err
	}
	if args[0] != "configure" && args[0] != "ready" {
		return errors.New("host-reboot supports configure or ready; use --help")
	}
	f := flag.NewFlagSet("deploy host-reboot "+args[0], flag.ContinueOnError)
	f.SetOutput(errOut)
	directory := f.String("dir", "./opsecmkt-deployment", "preserved production installation")
	var confirmed bool
	if args[0] == "ready" {
		f.BoolVar(&confirmed, "confirm-unlock-rehearsed", false, "acknowledge this host's real OS reboot and off-host manual-unlock rehearsal")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("host-reboot accepts flags only")
	}
	if args[0] == "ready" && !confirmed {
		return errors.New("ready requires --confirm-unlock-rehearsed after a real OS reboot and manual-unlock rehearsal; synthetic tests are insufficient")
	}
	dir, err := installationPath(*directory)
	if err != nil {
		return err
	}
	if err := operatorBootConfigCheck(dir); err != nil {
		return err
	}
	unlock, err := lockInstallation(dir)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if c.Mode != "prod" || c.AdminTorImage == "" {
		return errors.New("host reboot requires a production installation with private administrator ingress")
	}
	if err := refuseEmergencyRestart(dir); err != nil {
		return err
	}
	if err := checkProductionStorage(ctx, dir, c); err != nil {
		return err
	}
	if args[0] == "ready" {
		if c.HostControlDir == "" {
			return errors.New("configure the private host agent before marking readiness")
		}
		if err := setHostRebootReady(dir, c); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "operator unlock-rehearsal acknowledgement recorded; encrypted storage and agent checks still apply to every request")
		return err
	}
	c.HostControlDir = hostControlSocketDir(c.Project)
	if err := validate(dir, c); err != nil {
		return err
	}
	bindings, err := productionStorageProbe(ctx, dir, c, nil)
	if err != nil {
		return err
	}
	if err := checkStoragePins(bindings, c.StorageBindings); err != nil {
		return err
	}
	c.StorageBindings = bindings
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, "operator.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	if err := installProductionBootGuard(ctx, dir, c); err != nil {
		return err
	}
	if err := installHostRebootService(ctx, dir, c); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "private application-host reboot agent configured; new policies remain disabled until unlock rehearsal is acknowledged; run deploy up to attach its socket")
	return err
}

func hostRebootUnit(binary, dir string, c Config) (string, error) {
	if c.HostControlDir != hostControlSocketDir(c.Project) || !projectPattern.MatchString(c.Project) || c.RuntimeUID < 1 || c.RuntimeGID < 1 {
		return "", errors.New("invalid private host-agent configuration")
	}
	// Reuse the storage unit's argv validation; systemd receives no shell command.
	if _, err := storageBootUnit(binary, dir); err != nil {
		return "", err
	}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	// Keep the host mount namespace: filesystem sandbox bind mounts would change
	// the exact mount identities checked by storage check-bound. This root agent
	// is trusted; the application receives only its narrow peer-checked socket.
	return fmt.Sprintf(`[Unit]
Description=OPSECMKT private application-host reboot agent (%s)
After=local-fs.target
Before=docker.service
[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=60
User=root
Group=root
UMask=0077
StateDirectory=opsecmkt-host-control/%s
StateDirectoryMode=0700
ExecStart=%s host-control-agent --socket %s --state-dir %s --deployment-dir %s --app-uid %d --app-gid %d --unlock-ready-file %s
Restart=on-failure
RestartSec=5
NoNewPrivileges=yes
RestrictAddressFamilies=AF_UNIX
LimitCORE=0
[Install]
WantedBy=multi-user.target
`, c.Project, c.Project, quote(binary), quote(filepath.Join(c.HostControlDir, "control.sock")), quote(hostControlStateDir(c.Project)), quote(dir), c.RuntimeUID, c.RuntimeGID, quote(hostControlPolicyFile(c.Project))), nil
}
