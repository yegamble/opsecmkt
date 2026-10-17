package operator

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func readHostMetadata(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("host storage metadata unavailable or oversized")
	}
	return b, nil
}

func checkNoActiveSwap() error {
	b, err := readHostMetadata("/proc/swaps", 64<<10)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if err != nil || len(lines) != 1 || !strings.HasPrefix(lines[0], "Filename") {
		return errors.New("production storage requires no active swap; disable swap in the reviewed host setup and recheck before startup")
	}
	return nil
}

// Containerd's content store can be outside DockerRootDir. Inspect each local
// daemon's actual argv/config rather than assuming /var/lib/docker covers it.
func discoverContainerdRoots() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, errors.New("cannot inspect local containerd storage")
	}
	seen := map[string]bool{"/var/lib/containerd": true}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil || !entry.IsDir() {
			continue
		}
		b, err := readHostMetadata(filepath.Join("/proc", entry.Name(), "cmdline"), 64<<10)
		if os.IsNotExist(err) { // Process ended while reading the snapshot.
			continue
		}
		if err != nil {
			return nil, errors.New("cannot inspect local process storage arguments")
		}
		argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		if len(argv) == 0 || filepath.Base(argv[0]) != "containerd" {
			continue
		}
		root, err := containerdRoot(argv, readHostMetadata)
		if err != nil {
			return nil, err
		}
		seen[root] = true
	}
	result := make([]string, 0, len(seen))
	for root := range seen {
		result = append(result, root)
	}
	sort.Strings(result)
	return result, nil
}

func containerdRoot(argv []string, read func(string, int64) ([]byte, error)) (string, error) {
	root, config := "", "/etc/containerd/config.toml"
	for i := 1; i < len(argv); i++ {
		key, value, hasValue := strings.Cut(argv[i], "=")
		if key != "--root" && key != "--config" && key != "-c" {
			continue
		}
		if !hasValue {
			i++
			if i >= len(argv) {
				return "", errors.New("containerd storage argument is incomplete")
			}
			value = argv[i]
		}
		if key == "--root" {
			root = value
		} else {
			config = value
		}
	}
	if root == "" {
		if !filepath.IsAbs(config) || config != filepath.Clean(config) || strings.ContainsAny(config, "\x00\r\n") {
			return "", errors.New("containerd config must use a reviewed absolute path")
		}
		b, err := read(config, 1<<20)
		if err != nil && !os.IsNotExist(err) {
			return "", errors.New("cannot verify containerd storage configuration")
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				break // Only the top-level root field identifies the daemon root.
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if strings.HasPrefix(key, `"`) {
				key, err = strconv.Unquote(key)
				if err != nil {
					return "", errors.New("cannot parse containerd top-level storage configuration")
				}
			} else if strings.HasPrefix(key, "'") && strings.HasSuffix(key, "'") {
				key = strings.Trim(key, "'")
			}
			if key == "imports" && strings.TrimSpace(value) != "[]" {
				return "", errors.New("containerd configuration imports require separate storage review; automatic root verification refused")
			}
			if key != "root" {
				continue
			}
			value = strings.TrimSpace(value)
			// Normal generated containerd configs use a quoted absolute root.
			// Reject unfamiliar TOML syntax instead of guessing its semantics.
			parsed, err := strconv.Unquote(value)
			if err != nil {
				return "", errors.New("containerd root requires a reviewed quoted absolute path")
			}
			root = parsed
		}
		if root == "" {
			root = "/var/lib/containerd"
		}
	}
	if !filepath.IsAbs(root) || root != filepath.Clean(root) || strings.ContainsAny(root, "\x00\r\n") {
		return "", errors.New("containerd root must be an explicit clean absolute path")
	}
	return root, nil
}
