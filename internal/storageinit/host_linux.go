//go:build linux

package storageinit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupTimeout = 15 * time.Second
const inheritedDevice = "/proc/self/fd/3"

var mapperName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

type commandRunner func(context.Context, string, []string, *os.File, console, bool) ([]byte, int, error)

type systemVolume struct {
	cfg                         options
	device, mapper              *os.File
	canonical, sysDevice, devID string
	uuid                        string
	rdev, inode, filesystemDev  uint64
	cryptsetup, wipefs, blkid   string
	mkfs                        string
	runner                      commandRunner
	opened, mounted             bool
	cleanupUnknown              bool
}

func openSystemVolume(ctx context.Context, cfg options, streams console) (volume, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("storage: Linux root privileges are required")
	}
	terminal, ok := streams.in.(*os.File)
	if !ok {
		return nil, errors.New("storage: an interactive terminal is required; piped passphrases are refused")
	}
	if _, err := unix.IoctlGetTermios(int(terminal.Fd()), unix.TCGETS); err != nil {
		return nil, errors.New("storage: an interactive terminal is required; piped passphrases are refused")
	}
	if !mapperName.MatchString(cfg.name) || strings.HasPrefix(cfg.name, "dm-") || cfg.name == "control" {
		return nil, errors.New("storage: use a safe mapper name of 1..63 letters, numbers, underscores or hyphens")
	}
	if !cleanAbsolute(cfg.device) || !strings.HasPrefix(cfg.device, "/dev/") || !cleanAbsolute(cfg.mount) || cfg.mount == "/" {
		return nil, errors.New("storage: require a clean absolute /dev path and a separate absolute data mount")
	}
	canonical, err := filepath.EvalSymlinks(cfg.device)
	if err != nil || !strings.HasPrefix(canonical, "/dev/") || !cleanAbsolute(canonical) {
		return nil, errors.New("storage: selected device must resolve to an existing /dev block device")
	}
	if cfg.prepare && cfg.confirmation != canonical {
		return nil, errors.New("storage prepare: --confirm-device must exactly match the resolved real block-device path")
	}
	var node unix.Stat_t
	if unix.Lstat(canonical, &node) != nil || node.Mode&unix.S_IFMT != unix.S_IFBLK {
		return nil, errors.New("storage: regular files and character devices are refused")
	}
	// Hold the validated device open and give children that descriptor, rather
	// than resolving the operator's path a second time during destructive work.
	// cryptsetup must obtain its own exclusive kernel claim. Holding O_EXCL in
	// this process would prevent cryptsetup from acquiring that claim itself.
	fd, err := unix.Open(canonical, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("storage: selected block device cannot be opened for validation")
	}
	v := &systemVolume{
		cfg: cfg, device: os.NewFile(uintptr(fd), "storage-data-device"), canonical: canonical,
		rdev: node.Rdev, inode: node.Ino, filesystemDev: uint64(node.Dev), runner: runCommand,
		devID: fmt.Sprintf("%d:%d", unix.Major(node.Rdev), unix.Minor(node.Rdev)),
	}
	keep := false
	defer func() {
		if !keep {
			v.Close()
		}
	}()
	if err := v.sameDevice(); err != nil {
		return nil, err
	}
	for name, dest := range map[string]*string{"cryptsetup": &v.cryptsetup, "wipefs": &v.wipefs, "blkid": &v.blkid} {
		*dest, err = trustedBinary(name)
		if err != nil {
			return nil, err
		}
	}
	if cfg.prepare {
		v.mkfs, err = trustedBinary("mkfs.ext4")
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Lstat(filepath.Join("/dev/mapper", cfg.name)); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("storage: mapper destination already exists or cannot be inspected")
	}
	if err := v.checkUnused(); err != nil {
		return nil, err
	}
	if cfg.prepare {
		if err := v.checkNoSignatures(ctx); err != nil {
			return nil, err
		}
	}
	if err := privateEmptyMount(cfg.mount); err != nil {
		return nil, err
	}
	keep = true
	return v, nil
}

func (v *systemVolume) Close() error {
	var err error
	if v.mapper != nil {
		err = v.mapper.Close()
		v.mapper = nil
	}
	if v.device != nil {
		if e := v.device.Close(); err == nil {
			err = e
		}
		v.device = nil
	}
	return err
}

func (v *systemVolume) sameDevice() error {
	var held, named unix.Stat_t
	if v.device == nil || unix.Fstat(int(v.device.Fd()), &held) != nil || unix.Lstat(v.canonical, &named) != nil ||
		held.Mode&unix.S_IFMT != unix.S_IFBLK || named.Mode&unix.S_IFMT != unix.S_IFBLK ||
		held.Rdev != v.rdev || named.Rdev != v.rdev || held.Ino != v.inode || named.Ino != v.inode ||
		uint64(held.Dev) != v.filesystemDev || uint64(named.Dev) != v.filesystemDev {
		return errors.New("storage: selected device changed after validation")
	}
	return nil
}

func (v *systemVolume) checkUnused() error {
	if err := v.sameDevice(); err != nil {
		return err
	}
	sysDevice, err := filepath.EvalSymlinks(filepath.Join("/sys/dev/block", v.devID))
	if err != nil || !strings.HasPrefix(sysDevice, "/sys/devices/") {
		return errors.New("storage: cannot prove selected device topology from Linux sysfs")
	}
	v.sysDevice = sysDevice
	var topology deviceTopology
	if _, err := os.Stat(filepath.Join(sysDevice, "partition")); err == nil {
		topology.partition = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("storage: cannot inspect selected device partition status")
	}
	entries, err := os.ReadDir(sysDevice)
	if err != nil {
		return errors.New("storage: cannot inspect device partition children")
	}
	for _, entry := range entries {
		_, err := os.Stat(filepath.Join(sysDevice, entry.Name(), "partition"))
		if err == nil {
			topology.partitions++
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return errors.New("storage: cannot inspect device partition children")
		}
	}
	for relation, count := range map[string]*int{"holders": &topology.holders, "slaves": &topology.slaves} {
		entries, err := os.ReadDir(filepath.Join(sysDevice, relation))
		if err != nil {
			return errors.New("storage: active holders or backing devices cannot be inspected")
		}
		*count = len(entries)
	}
	ro, err := os.ReadFile(filepath.Join(sysDevice, "ro"))
	if err != nil {
		return errors.New("storage: device write eligibility cannot be inspected")
	}
	topology.readOnly = strings.TrimSpace(string(ro))
	size, err := os.ReadFile(filepath.Join(sysDevice, "size"))
	if err != nil {
		return errors.New("storage: device capacity cannot be inspected")
	}
	topology.size = strings.TrimSpace(string(size))
	if err := validateWholeDevice(topology); err != nil {
		return err
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("storage: cannot inspect mounted devices")
	}
	if err := rejectMountedDevice(mounts, v.devID, ""); err != nil {
		return err
	}
	swaps, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return errors.New("storage: cannot inspect active swap")
	}
	return rejectSwap(swaps, v.rdev)
}

type deviceTopology struct {
	partition                   bool
	partitions, holders, slaves int
	readOnly, size              string
}

func validateWholeDevice(topology deviceTopology) error {
	if topology.partition {
		return errors.New("storage: select a fresh whole data device; partitions are refused")
	}
	if topology.partitions != 0 {
		return errors.New("storage: partitioned devices are refused, including unmounted partitions")
	}
	if topology.holders != 0 || topology.slaves != 0 {
		return errors.New("storage: devices with active holders or backing devices are refused")
	}
	if topology.readOnly != "0" {
		return errors.New("storage: device is read-only or write eligibility cannot be proved")
	}
	sectors, err := strconv.ParseUint(topology.size, 10, 64)
	if err != nil || sectors < 131072 {
		return errors.New("storage: require an observable data device of at least 64 MiB")
	}
	return nil
}

func rejectMountedDevice(data []byte, devID, mount string) error {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || len(lines) == 1 && lines[0] == "" {
		return errors.New("storage: empty mount inventory is not proof of an unused device")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.Contains(line, " - ") || !strings.Contains(fields[2], ":") {
			return errors.New("storage: malformed mount inventory; refusing storage changes")
		}
		if devID != "" && fields[2] == devID || mount != "" && unescapeProcPath(fields[4]) == mount {
			return errors.New("storage: selected device or destination is already mounted")
		}
	}
	return nil
}

func rejectSwap(data []byte, rdev uint64) error {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Filename\t") {
		return errors.New("storage: malformed swap inventory; refusing storage changes")
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			return errors.New("storage: malformed swap inventory; refusing storage changes")
		}
		var stat unix.Stat_t
		if unix.Stat(unescapeProcPath(fields[0]), &stat) != nil {
			return errors.New("storage: an active swap source cannot be inspected")
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFBLK && stat.Rdev == rdev || stat.Mode&unix.S_IFMT == unix.S_IFREG && uint64(stat.Dev) == rdev {
			return errors.New("storage: selected device is used for active swap")
		}
	}
	return nil
}

func (v *systemVolume) checkNoSignatures(ctx context.Context) error {
	data, status, err := v.runner(ctx, v.wipefs, []string{"--json", "--no-act", inheritedDevice}, v.device, console{}, false)
	if err != nil || status != 0 {
		return errors.New("storage prepare: signature probing failed; refusing to format")
	}
	var result struct {
		Signatures *[]json.RawMessage `json:"signatures"`
	}
	if json.Unmarshal(data, &result) != nil || result.Signatures == nil {
		return errors.New("storage prepare: invalid signature inventory; refusing to format")
	}
	if len(*result.Signatures) != 0 {
		return errors.New("storage prepare: existing signatures found; refusing to overwrite the selected device")
	}
	data, status, err = v.runner(ctx, v.blkid, []string{"--probe", "--output", "export", inheritedDevice}, v.device, console{}, false)
	// libblkid's documented status 2 means no identifiable signature. Status 0
	// identifies existing data; all other statuses and unexplained output fail.
	if err != nil || status != 2 || len(bytes.TrimSpace(data)) != 0 {
		return errors.New("storage prepare: independent signature probe did not return an unambiguous empty result")
	}
	return nil
}

func (v *systemVolume) Format(ctx context.Context, streams console) error {
	// Recheck immediately before the irreversible step; no force/batch/keyfile
	// options are permitted, and cryptsetup still asks its own confirmation.
	if err := v.checkUnused(); err != nil {
		return err
	}
	if err := v.checkNoSignatures(ctx); err != nil {
		return err
	}
	_, status, err := v.runner(ctx, v.cryptsetup, []string{"luksFormat", "--type", "luks2", "--verify-passphrase", "--cipher", "aes-xts-plain64", "--key-size", "512", "--pbkdf", "argon2id", "--iter-time", "2000", inheritedDevice}, v.device, streams, true)
	if err != nil || status != 0 {
		return errors.New("storage prepare: native LUKS creation did not complete; inspect the selected device before retrying")
	}
	return nil
}

func (v *systemVolume) Identity(ctx context.Context) (string, error) {
	if err := v.sameDevice(); err != nil {
		return "", err
	}
	_, status, err := v.runner(ctx, v.cryptsetup, []string{"isLuks", "--type", "luks2", inheritedDevice}, v.device, console{}, false)
	if err != nil || status != 0 {
		return "", errors.New("storage: selected device is not a readable LUKS2 volume")
	}
	data, status, err := v.runner(ctx, v.cryptsetup, []string{"luksUUID", inheritedDevice}, v.device, console{}, false)
	uuid := strings.TrimSpace(string(data))
	if err != nil || status != 0 || !validUUID(uuid) {
		return "", errors.New("storage: selected device has no valid observable LUKS UUID")
	}
	v.uuid = uuid
	return uuid, nil
}

func (v *systemVolume) Open(ctx context.Context, streams console) error {
	if err := v.sameDevice(); err != nil {
		return err
	}
	if !validUUID(v.uuid) {
		return errors.New("storage: a verified LUKS identity is required before unlock")
	}
	_, status, err := v.runner(ctx, v.cryptsetup, []string{"open", "--type", "luks2", "--tries", "3", inheritedDevice, v.cfg.name}, v.device, streams, true)
	// The process may register the mapping immediately before cancellation or
	// another command error. Prove ownership even on an unsuccessful exit so
	// deferred cleanup closes that mapping; never close an unrelated name.
	mapper, mappingErr := holdMapper(v.cfg.name, v.devID, v.uuid)
	_, statErr := os.Lstat(filepath.Join("/dev/mapper", v.cfg.name))
	v.recordMapping(mapper, mappingErr, !errors.Is(statErr, os.ErrNotExist))
	if err != nil || status != 0 {
		return errors.New("storage: native LUKS unlock failed; no filesystem was mounted")
	}
	if mappingErr != nil {
		return mappingErr
	}
	return nil
}

func (v *systemVolume) recordMapping(mapper *os.File, mappingErr error, presentOrUnknown bool) {
	if mappingErr == nil && mapper != nil {
		v.opened, v.mapper = true, mapper
	} else if presentOrUnknown {
		v.cleanupUnknown = true
	}
}

func holdMapper(name, devID, uuid string) (*os.File, error) {
	path := filepath.Join("/dev/mapper", name)
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.HasPrefix(canonical, "/dev/") {
		return nil, errors.New("storage: cannot inspect the newly opened mapping")
	}
	fd, err := unix.Open(canonical, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("storage: cannot hold the newly opened mapping")
	}
	file := os.NewFile(uintptr(fd), "storage-luks-mapping")
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		file.Close()
		return nil, errors.New("storage: the newly opened mapping is not a block device")
	}
	sysPath := filepath.Join("/sys/dev/block", fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev)))
	dmUUID, err := os.ReadFile(filepath.Join(sysPath, "dm/uuid"))
	if err != nil || strings.TrimSpace(string(dmUUID)) != "CRYPT-LUKS2-"+strings.ReplaceAll(uuid, "-", "")+"-"+name {
		file.Close()
		return nil, errors.New("storage: opened mapping does not match the selected LUKS2 UUID")
	}
	suspended, err := os.ReadFile(filepath.Join(sysPath, "dm/suspended"))
	if err != nil || strings.TrimSpace(string(suspended)) != "0" {
		file.Close()
		return nil, errors.New("storage: opened mapping is suspended or its active state cannot be proved")
	}
	children, err := os.ReadDir(filepath.Join(sysPath, "slaves"))
	if err != nil || len(children) != 1 {
		file.Close()
		return nil, errors.New("storage: cannot prove the opened mapping's sole backing device")
	}
	backing, err := os.ReadFile(filepath.Join(sysPath, "slaves", children[0].Name(), "dev"))
	if err != nil || strings.TrimSpace(string(backing)) != devID {
		file.Close()
		return nil, errors.New("storage: opened mapping is backed by a different device")
	}
	return file, nil
}

func (v *systemVolume) MakeFilesystem(ctx context.Context) error {
	if v.mapper == nil {
		return errors.New("storage: no verified mapping is available for a new filesystem")
	}
	// The trusted executable may resolve mkfs.ext4's symlink to mke2fs, whose
	// argv[0] default can be ext2. Select ext4 explicitly rather than relying on
	// the caller-visible symlink name or distribution mke2fs.conf defaults.
	_, status, err := v.runner(ctx, v.mkfs, []string{"-q", "-t", "ext4", "-L", "opsecmkt-state", inheritedDevice}, v.mapper, console{}, false)
	if err != nil || status != 0 {
		return errors.New("storage prepare: ext4 creation failed; existing LUKS header is preserved for inspection")
	}
	return v.CheckFilesystem(ctx)
}

func (v *systemVolume) CheckFilesystem(ctx context.Context) error {
	if v.mapper == nil {
		return errors.New("storage: no verified mapping is available")
	}
	data, status, err := v.runner(ctx, v.blkid, []string{"--probe", "--output", "value", "--match-tag", "TYPE", inheritedDevice}, v.mapper, console{}, false)
	if err != nil || status != 0 || strings.TrimSpace(string(data)) != "ext4" {
		return errors.New("storage: only an existing, unambiguous ext4 filesystem may be mounted")
	}
	return nil
}

func (v *systemVolume) Mount(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("storage: operation cancelled before mounting")
	}
	if v.mapper == nil {
		return errors.New("storage: no verified mapping is available")
	}
	if err := privateEmptyMount(v.cfg.mount); err != nil {
		return err
	}
	if err := unix.Mount(fmt.Sprintf("/proc/self/fd/%d", v.mapper.Fd()), v.cfg.mount, "ext4", unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		return errors.New("storage: mounting the verified encrypted filesystem failed")
	}
	v.mounted = true
	if os.Chmod(v.cfg.mount, 0700) != nil {
		return errors.New("storage: cannot make the mounted data root private")
	}
	return nil
}

func (v *systemVolume) WriteReceipt(uuid string) error {
	data, err := json.MarshalIndent(struct {
		Version  int    `json:"version"`
		Device   string `json:"device"`
		Resolved string `json:"resolved_device_at_creation"`
		Mapper   string `json:"mapper"`
		Mount    string `json:"mount"`
		LUKSUUID string `json:"luks_uuid"`
	}{1, v.cfg.device, v.canonical, v.cfg.name, v.cfg.mount, uuid}, "", "  ")
	if err != nil {
		return errors.New("storage: cannot encode non-secret storage receipt")
	}
	file, err := os.OpenFile(filepath.Join(v.cfg.mount, ".opsecmkt-storage.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("storage: cannot create the non-secret storage receipt exclusively")
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("storage: cannot durably write the non-secret storage receipt")
	}
	return nil
}

func (v *systemVolume) Cleanup(ctx context.Context) error {
	if v.mounted {
		if unix.Unmount(v.cfg.mount, 0) != nil {
			return errors.New("storage: mounted storage remains active")
		}
		v.mounted = false
	}
	if v.mapper != nil {
		v.mapper.Close()
		v.mapper = nil
	}
	if v.opened {
		_, status, err := v.runner(ctx, v.cryptsetup, []string{"close", v.cfg.name}, nil, console{}, false)
		if err != nil || status != 0 {
			return errors.New("storage: newly opened mapping remains active")
		}
		v.opened = false
	}
	if v.cleanupUnknown {
		return errors.New("storage: a mapping with the selected name could not be proved owned by this invocation; inspect it manually")
	}
	return nil
}

func privateEmptyMount(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		var stat unix.Stat_t
		if unix.Lstat(current, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || stat.Mode&0022 != 0 {
			return errors.New("storage: destination ancestors must be existing root-owned directories without symlinks or group/world write access")
		}
		if current == "/" {
			break
		}
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("storage: cannot create the private mount directory")
	}
	var stat unix.Stat_t
	if unix.Lstat(path, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || stat.Mode&0077 != 0 {
		return errors.New("storage: destination must be a root-owned private directory without symlinks")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return errors.New("storage: mount destination must be empty")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("storage: cannot inspect the mount destination")
	}
	return rejectMountedDevice(data, "", path)
}

func trustedBinary(name string) (string, error) {
	for _, directory := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		path, err := filepath.EvalSymlinks(filepath.Join(directory, name))
		if err != nil {
			continue
		}
		var stat unix.Stat_t
		if unix.Stat(path, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Uid == 0 && stat.Mode&0022 == 0 && stat.Mode&0111 != 0 && rootOwnedAncestors(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("storage: required root-owned system executable %s is unavailable; install it before preparing storage", name)
}

func runCommand(ctx context.Context, binary string, args []string, device *os.File, streams console, interactive bool) ([]byte, int, error) {
	if !interactive {
		var cancel context.CancelFunc
		limit := 30 * time.Second
		if filepath.Base(binary) == "mkfs.ext4" || filepath.Base(binary) == "mke2fs" {
			limit = 10 * time.Minute
		}
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	// Give native passphrase prompting a chance to restore terminal settings
	// on cancellation before WaitDelay forces termination.
	cmd.Cancel = func() error { return cmd.Process.Signal(unix.SIGTERM) }
	cmd.WaitDelay = time.Second
	if device != nil {
		cmd.ExtraFiles = []*os.File{device}
	}
	var output boundedOutput
	if interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = streams.in, streams.out, streams.errOut
	} else {
		cmd.Stdout, cmd.Stderr = &output, io.Discard
	}
	err := cmd.Run()
	if err == nil {
		return output.data, 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return output.data, exit.ExitCode(), nil
	}
	return nil, -1, errors.New("storage: native command could not complete")
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(b.data)+len(data) > 64<<10 {
		return 0, io.ErrShortBuffer
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func cleanAbsolute(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, r := range path {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func rootOwnedAncestors(path string) bool {
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		var stat unix.Stat_t
		if unix.Lstat(directory, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || stat.Mode&0022 != 0 {
			return false
		}
		if directory == "/" {
			return true
		}
	}
}

func unescapeProcPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}
