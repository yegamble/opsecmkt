package storageguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type inspector struct {
	mountInfo      string
	sysRoot        string
	device         func(string) (string, error)
	cryptStatus    func(string, string) error
	filesystemUUID func(string, string) (string, error)
}

func inspect(path string, host inspector) (Evidence, error) {
	var result Evidence
	existing, before, err := existingPath(path)
	if err != nil {
		return result, err
	}
	device, err := host.device(existing)
	if err != nil || !validDevice(device) {
		return result, errors.New("cannot inspect actual storage device")
	}
	data, err := boundedRead(host.mountInfo, 2<<20)
	if err != nil {
		return result, errors.New("cannot read actual mount information")
	}
	mount, err := containingMount(existing, device, string(data))
	if err != nil {
		return result, err
	}
	// A Btrfs mount can represent several devices while mountinfo reports an
	// anonymous major:minor. Do not claim proof from only one of those devices.
	if mount.filesystem != "ext4" && mount.filesystem != "xfs" {
		return result, errors.New("storage verification supports persistent ext4 or XFS backed by LUKS2; volatile, overlay, remote and other filesystems are refused")
	}
	trace := ancestry{sysRoot: host.sysRoot, active: map[string]bool{}, verified: map[string][]string{}, cryptStatus: host.cryptStatus}
	uuids, err := trace.protectingMappings(device, 0)
	if err != nil {
		return result, err
	}
	node, err := trace.node(device)
	if err != nil || host.filesystemUUID == nil {
		return result, errors.New("cannot inspect encrypted filesystem identity")
	}
	filesystemUUID, err := host.filesystemUUID(device, node)
	if err != nil || !canonicalUUID(filesystemUUID) {
		return result, errors.New("cannot verify actual encrypted filesystem UUID")
	}
	// Detect ordinary path/mount replacement during inspection. This is not a
	// security boundary against a root process controlling this kernel.
	afterPath, after, err := existingPath(path)
	afterDevice, deviceErr := host.device(existing)
	if err != nil || deviceErr != nil || afterPath != existing || !os.SameFile(before, after) || afterDevice != device {
		return result, errors.New("storage path changed during verification")
	}
	result = Evidence{Path: path, ExistingPath: existing, MountPoint: mount.point, MountRoot: mount.root, Device: device, Filesystem: mount.filesystem, FilesystemUUID: filesystemUUID, LUKSUUIDs: uniqueSorted(uuids)}
	return result, nil
}

// existingPath checks every component with Lstat before accepting a parent for
// a future directory. A symlink in any existing ancestor is never followed.
func existingPath(path string) (string, os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return "", nil, errors.New("storage path must be a clean absolute path")
	}
	current := string(filepath.Separator)
	last, err := os.Lstat(current)
	if err != nil {
		return "", nil, errors.New("cannot inspect storage path")
	}
	parts := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	if path == current {
		return current, last, nil
	}
	for i, part := range parts {
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return current, last, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || i != len(parts)-1 && !info.IsDir() {
			return "", nil, errors.New("storage path must have accessible real ancestors without symlinks")
		}
		current, last = next, info
	}
	return current, last, nil
}

type mountRecord struct{ point, root, device, filesystem string }

func containingMount(path, device, data string) (mountRecord, error) {
	var selected mountRecord
	longest := -1
	matched := false
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+3 >= len(fields) || !validDevice(fields[2]) {
			continue
		}
		point, err := unescapeMountPath(fields[4])
		if err != nil || !filepath.IsAbs(point) || filepath.Clean(point) != point || !within(path, point) {
			continue
		}
		root, err := unescapeMountPath(fields[3])
		if err != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return mountRecord{}, errors.New("cannot decode containing mount root")
		}
		if len(point) > longest {
			longest, matched = len(point), false
			selected = mountRecord{}
		}
		if len(point) != longest || fields[2] != device {
			continue
		}
		candidate := mountRecord{point: point, root: root, device: fields[2], filesystem: fields[separator+1]}
		if matched && selected != candidate {
			return mountRecord{}, errors.New("ambiguous containing mount information")
		}
		selected, matched = candidate, true
	}
	if !matched {
		return mountRecord{}, errors.New("actual device does not match its containing mount")
	}
	return selected, nil
}

func within(path, parent string) bool {
	return parent == string(filepath.Separator) || path == parent || strings.HasPrefix(path, parent+string(filepath.Separator))
}

func unescapeMountPath(path string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '\\' {
			result.WriteByte(path[i])
			continue
		}
		if i+3 >= len(path) {
			return "", errors.New("invalid mount escape")
		}
		escape := path[i+1 : i+4]
		var decoded byte
		switch escape {
		case "040":
			decoded = ' '
		case "011":
			decoded = '\t'
		case "012":
			decoded = '\n'
		case "134":
			decoded = '\\'
		default:
			return "", errors.New("invalid mount escape")
		}
		result.WriteByte(decoded)
		i += 3
	}
	return result.String(), nil
}

type ancestry struct {
	sysRoot     string
	active      map[string]bool
	verified    map[string][]string
	visited     int
	cryptStatus func(string, string) error
}

func (a *ancestry) protectingMappings(device string, depth int) ([]string, error) {
	if a.active[device] || depth > 64 || a.visited >= 1024 {
		return nil, errors.New("storage device ancestry is cyclic or exceeds verification limits")
	}
	if previous, ok := a.verified[device]; ok {
		return previous, nil
	}
	a.visited++
	a.active[device] = true
	defer delete(a.active, device)
	node, err := a.node(device)
	if err != nil {
		return nil, err
	}
	children, err := a.slaves(node)
	if err != nil {
		return nil, err
	}
	identity, err := boundedRead(filepath.Join(node, "dm", "uuid"), 256)
	if err == nil {
		if uuid, ok := luks2UUID(strings.TrimSpace(string(identity))); ok {
			suspended, err := boundedRead(filepath.Join(node, "dm", "suspended"), 16)
			if err != nil || strings.TrimSpace(string(suspended)) != "0" || len(children) == 0 {
				return nil, errors.New("LUKS2 storage mapping is not active with an observable backing device")
			}
			// A device-mapper UUID is administrator-assigned metadata. Confirm
			// its actual target rather than trusting a label on a linear map.
			if a.cryptStatus == nil || a.cryptStatus(device, node) != nil {
				return nil, errors.New("cannot verify an active LUKS2 crypt target with a non-null cipher and adequate key size; run as root with cryptsetup installed")
			}
			result := []string{uuid}
			a.verified[device] = result
			return result, nil
		}
		if strings.HasPrefix(string(identity), "CRYPT-") {
			return nil, errors.New("storage mapping must identify active LUKS2 encryption")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot inspect storage mapping identity")
	}
	partition, err := boundedRead(filepath.Join(node, "partition"), 32)
	if err == nil {
		if _, err := strconv.ParseUint(strings.TrimSpace(string(partition)), 10, 32); err != nil || strings.TrimSpace(string(partition)) == "0" || len(children) != 0 {
			return nil, errors.New("invalid storage partition ancestry")
		}
		parent, err := readDevice(filepath.Dir(node))
		if err != nil {
			return nil, err
		}
		children = []string{parent}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("cannot inspect storage partition ancestry")
	}
	if len(children) == 0 {
		return nil, errors.New("persistent storage has an unencrypted or unverifiable backing branch")
	}
	var protected []string
	for _, child := range children {
		uuids, err := a.protectingMappings(child, depth+1)
		if err != nil {
			return nil, err
		}
		protected = append(protected, uuids...)
	}
	protected = uniqueSorted(protected)
	a.verified[device] = protected
	return protected, nil
}

func (a *ancestry) node(device string) (string, error) {
	if !validDevice(device) {
		return "", errors.New("invalid storage device identity")
	}
	path, err := filepath.EvalSymlinks(filepath.Join(a.sysRoot, "dev", "block", device))
	if err != nil || !within(path, a.sysRoot) || path == a.sysRoot {
		return "", errors.New("cannot resolve storage device ancestry in sysfs")
	}
	actual, err := readDevice(path)
	if err != nil || actual != device {
		return "", errors.New("storage device ancestry identity mismatch")
	}
	return path, nil
}

func (a *ancestry) slaves(node string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(node, "slaves"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || len(entries) > 1024 {
		return nil, errors.New("cannot inspect storage backing branches")
	}
	var children []string
	for _, entry := range entries {
		path, err := filepath.EvalSymlinks(filepath.Join(node, "slaves", entry.Name()))
		if err != nil || !within(path, a.sysRoot) || path == a.sysRoot {
			return nil, errors.New("storage backing branch escaped sysfs")
		}
		child, err := readDevice(path)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return uniqueSorted(children), nil
}

func readDevice(path string) (string, error) {
	data, err := boundedRead(filepath.Join(path, "dev"), 64)
	device := strings.TrimSpace(string(data))
	if err != nil || !validDevice(device) {
		return "", errors.New("cannot read storage block device identity")
	}
	return device, nil
}

func validDevice(device string) bool {
	parts := strings.Split(device, ":")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		value, err := strconv.ParseUint(part, 10, 32)
		if err != nil || strconv.FormatUint(value, 10) != part {
			return false
		}
	}
	return true
}

func canonicalUUID(uuid string) bool {
	if len(uuid) != 36 {
		return false
	}
	for i, char := range uuid {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
		} else if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func luks2UUID(identity string) (string, bool) {
	const prefix = "CRYPT-LUKS2-"
	if !strings.HasPrefix(identity, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(identity, prefix)
	if len(value) < 34 || value[32] != '-' {
		return "", false
	}
	uuid := strings.ToLower(value[:32])
	if strings.Trim(uuid, "0123456789abcdef") != "" {
		return "", false
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", uuid[:8], uuid[8:12], uuid[12:16], uuid[16:20], uuid[20:]), true
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func boundedRead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("storage metadata unavailable or oversized")
	}
	return data, nil
}
