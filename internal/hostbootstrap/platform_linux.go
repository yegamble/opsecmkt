//go:build linux

package hostbootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const stateDir = "/var/lib/opsecmkt-host"
const firewallUnit = "opsecmkt-host-firewall.service"
const rollbackUnit = "opsecmkt-host-rollback.service"
const timerUnit = "opsecmkt-host-rollback.timer"
const ownedRules = stateDir + "/active.nft"
const pendingFile = stateDir + "/pending.json"
const inventoryFile = stateDir + "/host.json"
const binaryPath = stateDir + "/opsecmkt"
const updatePolicy = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\nUnattended-Upgrade::Automatic-Reboot \"false\";\n"

var controlledFiles = []string{ownedRules, inventoryFile, "/etc/systemd/system/" + firewallUnit, "/etc/systemd/system/docker.service.d/10-opsecmkt-host.conf", "/etc/systemd/system/containerd.service.d/10-opsecmkt-host.conf", "/etc/systemd/system/ssh.service.d/10-opsecmkt-host.conf", "/etc/systemd/system/ssh.socket.d/10-opsecmkt-host.conf", "/etc/sysctl.d/60-opsecmkt-host.conf", "/etc/systemd/coredump.conf.d/60-opsecmkt-host.conf", "/etc/apt/apt.conf.d/99-opsecmkt-host"}

type savedFile struct {
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}
type pending struct {
	RulesHash     string               `json:"rules_hash"`
	ExpectedFiles map[string]string    `json:"expected_files"`
	ConfigHash    string               `json:"config_hash"`
	PreviousRules string               `json:"previous_rules"`
	HadTable      bool                 `json:"had_table"`
	Session       string               `json:"session"`
	BootID        string               `json:"boot_id"`
	Deadline      time.Time            `json:"deadline"`
	Files         map[string]savedFile `json:"files"`
	Enabled       bool                 `json:"enabled"`
}

func command(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}
	cmd.Stdin = bytes.NewReader(input)
	var b limitedBuffer
	cmd.Stdout = &b
	cmd.Stderr = io.Discard
	if e := cmd.Run(); e != nil || b.overflow {
		return nil, fmt.Errorf("host prerequisite or command failed: %s (inspect its configuration locally)", filepath.Base(name))
	}
	return b.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		b.overflow = true
		return 0, errors.New("command output too large")
	}
	return b.Buffer.Write(p)
}
func rootOS() error {
	if os.Geteuid() != 0 {
		return errors.New("host mutation requires Linux root")
	}
	b, e := os.ReadFile("/etc/os-release")
	if e != nil {
		return errors.New("cannot verify supported OS")
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = strings.Trim(v, "\"")
		}
	}
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" {
		return errors.New("host mutation supports Ubuntu 24.04 only")
	}
	return nil
}

// Every persistent parent must be root-owned, non-writable to other users and
// real. This prevents root writes through attacker-controlled path components.
func secureParent(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("unsafe system path")
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		s, e := os.Lstat(current)
		if os.IsNotExist(e) && create {
			if e = os.Mkdir(current, 0755); e != nil {
				return e
			}
			s, e = os.Lstat(current)
		}
		if e != nil {
			return e
		}
		if !s.IsDir() || s.Mode()&os.ModeSymlink != 0 || s.Mode().Perm()&0022 != 0 {
			return errors.New("system parent must be a real root-owned directory without group/other write access")
		}
		st, ok := s.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return errors.New("system path parent is not root-owned")
		}
	}
	return nil
}
func safeRead(path string, limit int64) (savedFile, error) {
	var s savedFile
	if e := secureParent(path, false); e != nil {
		if os.IsNotExist(e) {
			return s, nil
		}
		return s, e
	}
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > limit {
		return s, errors.New("system file is unsafe or oversized")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return s, errors.New("system file is not root-owned")
	}
	f, e := os.Open(path)
	if e != nil {
		return s, e
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !os.SameFile(info, actual) {
		return s, errors.New("system file changed while reading")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return s, errors.New("cannot read bounded system file")
	}
	return savedFile{true, b, uint32(info.Mode().Perm())}, nil
}
func writeSystem(path string, data []byte, mode os.FileMode) error {
	if e := secureParent(path, true); e != nil {
		return e
	}
	if _, e := safeRead(path, 256<<20); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".opsecmkt-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}
func ensureState() (func(), error) {
	if e := secureParent(stateDir+"/lock", true); e != nil {
		return nil, e
	}
	if e := os.Chmod(stateDir, 0700); e != nil {
		return nil, e
	}
	if _, e := safeRead(stateDir+"/lock", 4096); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(stateDir+"/lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another host command is running")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func hashConfig(c Config) string {
	b, _ := marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func tableState(ctx context.Context) (bool, string, error) {
	b, e := command(ctx, nil, "/usr/sbin/nft", "list", "tables")
	if e != nil {
		return false, "", e
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "table inet "+table {
			rules, e := command(ctx, nil, "/usr/sbin/nft", "list", "table", "inet", table)
			return true, string(rules), e
		}
	}
	return false, "", nil
}
func replaceTable(ctx context.Context, rules string, check bool) error {
	exists, _, e := tableState(ctx)
	if e != nil {
		return e
	}
	if exists {
		rules = "delete table inet " + table + "\n" + rules
	}
	args := []string{"-f", "-"}
	if check {
		args = append([]string{"-c"}, args...)
	}
	_, e = command(ctx, []byte(rules), "/usr/sbin/nft", args...)
	return e
}
func bootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}
func preflight(ctx context.Context, c Config) error {
	if e := rootOS(); e != nil {
		return e
	}
	if bootID() == "" {
		return errors.New("cannot identify current boot")
	}
	for _, unit := range []string{"ufw.service", "firewalld.service", "nftables.service"} {
		b, e := command(ctx, nil, "/usr/bin/systemctl", "show", unit, "--property=ActiveState,UnitFileState")
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "ActiveState=active") || strings.Contains(string(b), "UnitFileState=enabled") {
			return errors.New("another persistent firewall manager is active/enabled; reconcile it before host apply")
		}
	}
	if _, e := command(ctx, nil, "/usr/bin/systemctl", "is-active", "ssh.service"); e != nil {
		return errors.New("ssh.service must already be running with tested key-only access")
	}
	account, e := user.Lookup(c.Operator)
	if e != nil || account.Uid == "0" {
		return errors.New("configured operator must exist and be nonroot")
	}
	// Check sshd effective policy; this tool deliberately never rewrites access.
	for _, source := range c.OperatorSources {
		p, _ := netip.ParsePrefix(source)
		args := []string{"-T", "-C", "user=" + c.Operator + ",addr=" + p.Addr().String() + ",host=" + p.Addr().String() + ",laddr=" + c.AppAddress + ",lport=" + strconv.Itoa(c.SSHPort)}
		b, e := command(ctx, nil, "/usr/sbin/sshd", args...)
		if e != nil {
			return e
		}
		settings := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, " ")
			if ok {
				settings[k] = strings.TrimSpace(v)
			}
		}
		for k, v := range map[string]string{"pubkeyauthentication": "yes", "authenticationmethods": "publickey", "passwordauthentication": "no", "kbdinteractiveauthentication": "no", "permitrootlogin": "no", "disableforwarding": "yes", "usepam": "yes"} {
			if settings[k] != v {
				return fmt.Errorf("SSH prerequisite %s=%s not met; follow server-hardening.md and test operator login first", k, v)
			}
		}
	}
	bridges, e := dockerBridges(ctx)
	if e != nil {
		return e
	}
	b, e := command(ctx, nil, "/usr/sbin/ip", "-details", "-json", "address", "show")
	if e != nil {
		return e
	}
	if e = validateInterfaces(c, b, bridges); e != nil {
		return e
	}
	// Networkd can retain a static lease address while DHCP still owns routes.
	for _, name := range c.Uplinks {
		b, e := command(ctx, nil, "/usr/bin/networkctl", "status", "--no-pager", "--no-legend", name)
		if e != nil {
			return errors.New("supported host requires systemd-networkd static network inventory")
		}
		if strings.Contains(strings.ToLower(string(b)), "dhcp") {
			return errors.New("DHCP managed interfaces are unsupported by this ingress profile")
		}
	}
	if e = replaceTable(ctx, renderRules(c), true); e != nil {
		return errors.New("nftables candidate validation failed; no host policy changed")
	}
	return nil
}
func applyHost(ctx context.Context, c Config, out io.Writer) error {
	if e := rootOS(); e != nil {
		return e
	}
	unlock, e := ensureState()
	if e != nil {
		return e
	}
	defer unlock()
	if s, e := safeRead(pendingFile, 1<<20); e != nil {
		return e
	} else if s.Exists {
		return errors.New("a host apply is pending; confirm its fresh SSH session or run host rollback first")
	}
	if e = preflight(ctx, c); e != nil {
		return e
	}
	p := pending{ConfigHash: hashConfig(c), Session: os.Getenv("SSH_CONNECTION"), BootID: bootID(), Deadline: time.Now().Add(5 * time.Minute), Files: map[string]savedFile{}}
	p.HadTable, p.PreviousRules, e = tableState(ctx)
	if e != nil {
		return e
	}
	for _, path := range controlledFiles {
		s, e := safeRead(path, 1<<20)
		if e != nil {
			return e
		}
		p.Files[path] = s
	}
	enabled, _ := command(ctx, nil, "/usr/bin/systemctl", "is-enabled", firewallUnit)
	p.Enabled = strings.TrimSpace(string(enabled)) == "enabled"
	// Retain the executable on protected storage; timers cannot depend on a
	// temporary upload path or a later replacement of the caller's executable.
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	f, e := os.Open(executable)
	if e != nil {
		return e
	}
	binary, e := io.ReadAll(io.LimitReader(f, (256<<20)+1))
	_ = f.Close()
	if e != nil || len(binary) > 256<<20 {
		return errors.New("cannot retain bounded bootstrap executable")
	}
	if e = writeSystem(binaryPath, binary, 0700); e != nil {
		return e
	}
	b, e := marshalPending(p)
	if e != nil {
		return e
	}
	if e = writeSystem(pendingFile, b, 0600); e != nil {
		return e
	}
	// Rollback is armed before firewall/persistence changes, and also runs on a
	// pending reboot before the candidate firewall or Docker may start.
	for path, data := range rollbackFiles() {
		if e = writeSystem(path, []byte(data), 0644); e != nil {
			return e
		}
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "daemon-reload"); e != nil {
		return e
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "enable", rollbackUnit); e != nil {
		return e
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "restart", timerUnit); e != nil {
		return e
	}
	fail := func(cause error) error {
		restoreErr := rollbackLocked(context.WithoutCancel(ctx), p)
		if restoreErr != nil {
			return fmt.Errorf("host apply failed; automatic rollback also failed: %w; use recovery console", restoreErr)
		}
		return cause
	}
	config, _ := marshal(c)
	files := map[string][]byte{ownedRules: []byte(renderRules(c)), inventoryFile: config, "/etc/systemd/system/" + firewallUnit: []byte(firewallService), "/etc/sysctl.d/60-opsecmkt-host.conf": []byte(sysctlDefaults), "/etc/systemd/coredump.conf.d/60-opsecmkt-host.conf": []byte("[Coredump]\nStorage=none\nProcessSizeMax=0\n"), "/etc/apt/apt.conf.d/99-opsecmkt-host": []byte(updatePolicy)}
	for _, name := range []string{"docker", "containerd", "ssh"} {
		files["/etc/systemd/system/"+name+".service.d/10-opsecmkt-host.conf"] = []byte("[Unit]\nRequires=" + firewallUnit + "\nAfter=" + firewallUnit + "\n")
	}
	files["/etc/systemd/system/ssh.socket.d/10-opsecmkt-host.conf"] = []byte("[Unit]\nRequires=" + firewallUnit + "\nAfter=" + firewallUnit + "\n")
	for path, data := range files {
		mode := os.FileMode(0644)
		if path == inventoryFile || path == ownedRules {
			mode = 0600
		}
		if e = writeSystem(path, data, mode); e != nil {
			return fail(e)
		}
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "daemon-reload"); e != nil {
		return fail(e)
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "enable", firewallUnit); e != nil {
		return fail(e)
	}
	if e = replaceTable(ctx, renderRules(c), false); e != nil {
		return fail(e)
	}
	p.RulesHash, e = liveRulesHash(ctx)
	if e != nil {
		return fail(e)
	}
	p.ExpectedFiles = map[string]string{}
	for path, data := range files {
		p.ExpectedFiles[path] = hashBytes(data)
	}
	b, e = marshalPending(p)
	if e != nil {
		return fail(e)
	}
	if e = writeSystem(pendingFile, b, 0600); e != nil {
		return fail(e)
	}
	_, e = fmt.Fprintln(out, "Owned ingress policy applied. Open a NEW approved operator SSH session and run host confirm with the same config within five minutes. Otherwise the previous policy returns. No application services started.")
	return e
}
func loadPending() (pending, error) {
	var p pending
	s, e := safeRead(pendingFile, 1<<20)
	if e != nil {
		return p, e
	}
	if !s.Exists {
		return p, errors.New("no host apply is pending")
	}
	d := json.NewDecoder(bytes.NewReader(s.Data))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || len(p.Files) != len(controlledFiles) {
		return p, errors.New("invalid pending host recovery state; use recovery console")
	}
	for _, path := range controlledFiles {
		if _, ok := p.Files[path]; !ok {
			return p, errors.New("pending host recovery inventory is incomplete")
		}
	}
	return p, nil
}
func confirmHost(ctx context.Context, c Config, out io.Writer) error {
	if e := rootOS(); e != nil {
		return e
	}
	unlock, e := ensureState()
	if e != nil {
		return e
	}
	defer unlock()
	p, e := loadPending()
	if e != nil {
		return e
	}
	if p.ConfigHash != hashConfig(c) || p.BootID != bootID() || !time.Now().Before(p.Deadline) {
		return errors.New("pending apply config, boot or deadline differs; rollback required")
	}
	if os.Getenv("SUDO_USER") != c.Operator {
		return errors.New("confirm through sudo from the configured named operator")
	}
	if e = checkSession(c, os.Getenv("SSH_CONNECTION"), p.Session); e != nil {
		return e
	}
	// Refuse to confirm drift or disappearance of the active table/files.
	s, e := safeRead(inventoryFile, 64<<10)
	if e != nil || !s.Exists {
		return errors.New("active inventory unavailable")
	}
	actual, e := decodeConfig(s.Data)
	if e != nil || hashConfig(actual) != p.ConfigHash {
		return errors.New("active host configuration changed")
	}
	rulesHash, e := liveRulesHash(ctx)
	if e != nil || p.RulesHash == "" || rulesHash != p.RulesHash {
		return errors.New("owned firewall rules changed or unavailable; cannot confirm")
	}
	if len(p.ExpectedFiles) != len(controlledFiles) {
		return errors.New("pending candidate file inventory incomplete")
	}
	for _, path := range controlledFiles {
		current, e := safeRead(path, 1<<20)
		if e != nil || !current.Exists || hashBytes(current.Data) != p.ExpectedFiles[path] {
			return errors.New("managed host files changed; cannot confirm")
		}
	}
	if _, e = command(ctx, nil, "/usr/bin/systemctl", "is-enabled", firewallUnit); e != nil {
		return errors.New("firewall persistence unavailable")
	}
	// Removing pending state commits. The timer may subsequently fire harmlessly;
	// it takes the same lock and sees no pending operation.
	if e = os.Remove(pendingFile); e != nil {
		return e
	}
	_, _ = command(ctx, nil, "/usr/bin/systemctl", "stop", timerUnit)
	_, e = fmt.Fprintln(out, "Fresh operator SSH confirmation recorded; host ingress policy persists before Docker startup. Security defaults are installed for subsequent boots/processes. Complete independent denied-source, gateway TLS and Tor checks.")
	return e
}
func rollbackHost(ctx context.Context, out io.Writer) error {
	if e := rootOS(); e != nil {
		return e
	}
	unlock, e := ensureState()
	if e != nil {
		return e
	}
	defer unlock()
	s, e := safeRead(pendingFile, 1<<20)
	if e != nil {
		return e
	}
	if !s.Exists {
		return nil
	}
	p, e := loadPending()
	if e != nil {
		return e
	}
	if e = rollbackLocked(ctx, p); e != nil {
		return e
	}
	_, e = fmt.Fprintln(out, "Previous owned ingress policy and managed files restored; unrelated firewall tables retained.")
	return e
}
func rollbackLocked(ctx context.Context, p pending) error {
	if !p.Enabled {
		// Disable before removing the unit, including a partially failed first apply.
		if _, e := command(ctx, nil, "/usr/bin/systemctl", "disable", firewallUnit); e != nil {
			return e
		}
	}
	rules := ""
	if p.HadTable {
		rules = p.PreviousRules
	}
	if e := replaceTable(ctx, rules, false); e != nil {
		return e
	}
	for _, path := range controlledFiles {
		s := p.Files[path]
		if s.Exists {
			if e := writeSystem(path, s.Data, os.FileMode(s.Mode)); e != nil {
				return e
			}
		} else {
			if e := secureParent(path, false); e != nil {
				if os.IsNotExist(e) {
					continue
				}
				return e
			}
			if _, e := safeRead(path, 1<<20); e != nil {
				return e
			}
			if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
	}
	if _, e := command(ctx, nil, "/usr/bin/systemctl", "daemon-reload"); e != nil {
		return e
	}
	return os.Remove(pendingFile)
}
func rollbackFiles() map[string]string {
	return map[string]string{
		"/etc/systemd/system/" + rollbackUnit: "[Unit]\nDescription=Restore pending OPSEC Market ingress policy\nDefaultDependencies=no\nAfter=local-fs.target\nBefore=network-pre.target " + firewallUnit + " docker.service containerd.service\nConditionPathExists=" + pendingFile + "\n[Service]\nType=oneshot\nRestart=on-failure\nRestartSec=2s\nExecStart=" + binaryPath + " host rollback\n[Install]\nWantedBy=sysinit.target\n",
		"/etc/systemd/system/" + timerUnit:    "[Unit]\nDescription=Rollback unconfirmed OPSEC Market ingress policy\n[Timer]\nOnActiveSec=5min\nAccuracySec=1s\nUnit=" + rollbackUnit + "\n",
	}
}

const firewallService = "[Unit]\nDescription=OPSEC Market application-host ingress\nDefaultDependencies=no\nRequires=" + rollbackUnit + "\nAfter=local-fs.target " + rollbackUnit + "\nBefore=network-pre.target docker.service containerd.service ssh.service ssh.socket\nWants=network-pre.target\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=" + binaryPath + " host-load-firewall\n[Install]\nWantedBy=multi-user.target\n"

// LoadFirewall is a fixed-purpose boot helper. Its only input is the protected
// inventory/rules, and it performs atomic table replacement rather than flush.
func LoadFirewall(ctx context.Context) error {
	if e := rootOS(); e != nil {
		return e
	}
	unlock, e := ensureState()
	if e != nil {
		return e
	}
	defer unlock()
	if p, e := safeRead(pendingFile, 1<<20); e != nil {
		return e
	} else if p.Exists {
		return errors.New("unconfirmed host policy must roll back before boot load")
	}
	s, e := safeRead(inventoryFile, 64<<10)
	if e != nil || !s.Exists {
		return errors.New("protected host inventory missing")
	}
	c, e := decodeConfig(s.Data)
	if e != nil {
		return e
	}
	s, e = safeRead(ownedRules, 1<<20)
	if e != nil || !s.Exists || string(s.Data) != renderRules(c) {
		return errors.New("protected host rules differ from inventory")
	}
	return replaceTable(ctx, renderRules(c), false)
}
func installPackages(ctx context.Context, c Config, out io.Writer) error {
	if e := rootOS(); e != nil {
		return e
	}
	unlock, e := ensureState()
	if e != nil {
		return e
	}
	defer unlock()
	if s, e := safeRead(pendingFile, 1<<20); e != nil {
		return e
	} else if s.Exists {
		return errors.New("finish pending host firewall change before package installation")
	}
	// Distro repositories only; never download a shell installer or add a trust key.
	for _, args := range [][]string{{"update"}, {"install", "-y", "--no-install-recommends", "docker.io", "docker-compose-v2", "nftables", "openssh-server", "unattended-upgrades", "iproute2"}} {
		packageCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(packageCtx, "/usr/bin/apt-get", args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		e := cmd.Run()
		cancel()
		if e != nil {
			return errors.New("distribution package installation failed; inspect apt locally before retrying")
		}
	}
	_, e = fmt.Fprintln(out, "Distribution Docker/Compose, nftables, OpenSSH and unattended-upgrades installed. Dependency daemons may have started. Prepare/test SSH and static networking, then run host apply. No application deployment started.")
	return e
}

func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func liveRulesHash(ctx context.Context) (string, error) {
	b, e := command(ctx, nil, "/usr/sbin/nft", "--json", "list", "table", "inet", table)
	if e != nil {
		return "", e
	}
	return normalizedRulesHash(b)
}
func normalizedRulesHash(b []byte) (string, error) {
	var value any
	if json.Unmarshal(b, &value) != nil {
		return "", errors.New("cannot verify live nftables JSON")
	}
	var clean func(any)
	clean = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, key := range []string{"handle", "packets", "bytes"} {
				delete(x, key)
			}
			for _, child := range x {
				clean(child)
			}
		case []any:
			for _, child := range x {
				clean(child)
			}
		}
	}
	clean(value)
	normalized, e := json.Marshal(value)
	if e != nil {
		return "", e
	}
	return hashBytes(normalized), nil
}

func marshalPending(p pending) ([]byte, error) {
	b, e := marshal(p)
	if e != nil || len(b) > 1<<20 {
		return nil, errors.New("previous host state exceeds 1 MiB recovery bound; no policy changed")
	}
	return b, nil
}
func dockerBridges(ctx context.Context) (map[string]bool, error) {
	b, e := command(ctx, nil, "/usr/bin/docker", "--host=unix:///var/run/docker.sock", "network", "ls", "--filter", "driver=bridge", "--format", "{{.ID}}")
	if e != nil {
		return nil, errors.New("local Docker bridge inventory unavailable; install/start the local engine first")
	}
	ids := strings.Fields(string(b))
	bridges := map[string]bool{}
	if len(ids) == 0 {
		return bridges, nil
	}
	args := append([]string{"--host=unix:///var/run/docker.sock", "network", "inspect"}, ids...)
	b, e = command(ctx, nil, "/usr/bin/docker", args...)
	if e != nil {
		return nil, e
	}
	var networks []struct {
		ID           string `json:"Id"`
		Name, Driver string
		Options      map[string]string
	}
	if json.Unmarshal(b, &networks) != nil {
		return nil, errors.New("invalid local Docker bridge inventory")
	}
	for _, network := range networks {
		if network.Driver != "bridge" || len(network.ID) < 12 {
			return nil, errors.New("unexpected Docker bridge inventory")
		}
		name := network.Options["com.docker.network.bridge.name"]
		if name == "" {
			name = "br-" + network.ID[:12]
		}
		if !interfaceName.MatchString(name) {
			return nil, errors.New("invalid Docker bridge interface")
		}
		bridges[name] = true
	}
	return bridges, nil
}
func validateInterfaces(c Config, b []byte, bridges map[string]bool) error {
	var links []struct {
		IfName   string `json:"ifname"`
		Master   string `json:"master"`
		LinkInfo struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
		AddrInfo []struct {
			Local, Scope string
			Dynamic      bool `json:"dynamic"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(b, &links) != nil {
		return errors.New("cannot inspect network interfaces")
	}
	found := map[string]bool{}
	local := false
	for _, link := range links {
		if link.IfName == "lo" {
			continue
		}
		listed := false
		for _, name := range c.Uplinks {
			if link.IfName == name {
				listed = true
				found[name] = true
			}
		}
		docker := bridges[link.IfName] && link.LinkInfo.Kind == "bridge" || bridges[link.Master] && link.LinkInfo.Kind == "veth"
		if bridges[link.Master] && link.LinkInfo.Kind != "veth" {
			return errors.New("non-veth link attached to Docker bridge; external bridge routing unsupported")
		}
		if !listed && !docker {
			return errors.New("every non-Docker network interface must appear in uplinks; names alone do not prove Docker ownership")
		}
		for _, addr := range link.AddrInfo {
			if addr.Local == c.AppAddress && listed {
				local = true
			}
			if addr.Dynamic && listed {
				return errors.New("dynamic addressing is unsupported; prepare static addresses and routes")
			}
		}
	}
	if !local || len(found) != len(c.Uplinks) {
		return errors.New("inventoried uplinks/private app address are not present locally")
	}
	return nil
}
