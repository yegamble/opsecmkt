package operator

import (
	"bytes"
	"encoding/base32"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var adminClientName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,63}\.auth$`)

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n$")
}

// readAdminAuth captures every public record once. Tor ignores invalid records
// and serves a public onion if none load, so one invalid/extra file refuses the
// entire snapshot. Only the captured values are copied, never the source files.
func readAdminAuth(dir string) (map[string][]byte, error) {
	fail := func() (map[string][]byte, error) {
		return nil, errors.New("private administrator onion requires 1 through 256 regular public .auth files with canonical descriptor:x25519:base32 records")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fail()
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) < 1 || len(entries) > 256 {
		return fail()
	}
	result := map[string][]byte{}
	for _, entry := range entries {
		if !adminClientName.MatchString(entry.Name()) {
			return fail()
		}
		path := filepath.Join(dir, entry.Name())
		before, err := os.Lstat(path)
		if err != nil || !before.Mode().IsRegular() {
			return fail()
		}
		file, err := os.Open(path)
		if err != nil {
			return fail()
		}
		after, err := file.Stat()
		if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
			file.Close()
			return fail()
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 72))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) < 70 || len(raw) > 71 {
			return fail()
		}
		record := bytes.TrimSuffix(raw, []byte{'\n'})
		if len(record) != 70 || !bytes.HasPrefix(record, []byte("descriptor:x25519:")) {
			return fail()
		}
		encoded := string(record[len("descriptor:x25519:"):])
		key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(encoded)
		if err != nil || len(key) != 32 || base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key) != encoded || bytes.Equal(key, make([]byte, 32)) {
			return fail()
		}
		result[entry.Name()] = append(append([]byte(nil), record...), '\n')
	}
	return result, nil
}
func importAdminAuth(source, destination string) error {
	records, err := readAdminAuth(source)
	if err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		return err
	}
	for name, record := range records {
		if err := writeExclusive(filepath.Join(destination, name), record, 0444); err != nil {
			return err
		}
	}
	return syncDirectory(destination)
}

// RAM bundle extraction starts private. These records are public keys and must
// be readable by the separate Tor user; the containing installation remains
// private and Tor receives this snapshot alone, read-only.
func prepareAdminAuthSnapshot(dir string, c Config) error {
	path, err := managedPath(dir, c.AdminAuthDir)
	if err != nil {
		return err
	}
	records, err := readAdminAuth(path)
	if err != nil {
		return err
	}
	for name := range records {
		entry := filepath.Join(path, name)
		before, err := os.Lstat(entry)
		if err != nil || !before.Mode().IsRegular() {
			return errors.New("administrator snapshot record changed")
		}
		file, err := os.Open(entry)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err == nil && (!info.Mode().IsRegular() || !os.SameFile(before, info)) {
			err = errors.New("administrator snapshot record changed")
		}
		if err == nil {
			err = file.Chmod(0444)
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return os.Chmod(path, 0755)
}

func renderAdminTor(dir string, c Config, app service, services map[string]service, volumes, networks map[string]any) {
	env := app["environment"].(map[string]string)
	env["ADMIN_SOCKET"] = "/run/market-admin/http.sock"
	app["volumes"] = append(app["volumes"].([]string), c.AdminSocketDir+":/run/market-admin")
	admin := hardened(c.AdminTorImage, []string{"admin_tor_egress"}, "256m")
	admin["entrypoint"] = []string{"tor"}
	admin["command"] = []string{"-f", "/etc/tor/torrc-admin"}
	admin["user"] = numericID(c.RuntimeUID, c.RuntimeGID)
	admin["pids_limit"] = 64
	admin["volumes"] = []string{"tor_admin_identity:/var/lib/tor", c.AdminSocketDir + ":/run/market-admin:ro", filepath.Join(dir, "torrc-admin") + ":/etc/tor/torrc-admin:ro"}
	admin["depends_on"] = map[string]any{"app": map[string]string{"condition": "service_healthy"}, "admin-tor-prepare": map[string]string{"condition": "service_completed_successfully"}}
	admin["healthcheck"] = map[string]any{"test": []string{"CMD-SHELL", "test -s /var/lib/tor/hostname"}, "interval": "10s", "timeout": "5s", "retries": 3, "start_period": "1m"}
	prepare := hardened(c.AppImage, nil, "64m")
	delete(prepare, "networks")
	prepare["network_mode"] = "none"
	prepare["restart"] = "no"
	prepare["user"] = "0:0"
	prepare["cap_add"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
	prepare["pids_limit"] = 16
	prepare["command"] = []string{"admin-tor-prepare", "--source", "/run/tor-admin-clients", "--destination", "/var/lib/tor", "--uid", strconv.Itoa(c.RuntimeUID), "--gid", strconv.Itoa(c.RuntimeGID)}
	prepare["volumes"] = []string{filepath.Join(dir, c.AdminAuthDir) + ":/run/tor-admin-clients:ro", "tor_admin_identity:/var/lib/tor"}
	if c.AdminOnionDir != "" {
		prepare["command"] = append(prepare["command"].([]string), "--identity-source", "/run/tor-admin-identity")
		prepare["volumes"] = append(prepare["volumes"].([]string), filepath.Join(dir, c.AdminOnionDir)+":/run/tor-admin-identity:ro")
	}
	if c.Mode == "prod" {
		admin["logging"] = map[string]any{"driver": "none"}
		prepare["logging"] = map[string]any{"driver": "none"}
	}
	services["admin-tor-prepare"] = prepare
	services["tor-admin"] = admin
	volumes["tor_admin_identity"] = map[string]any{}
	networks["admin_tor_egress"] = map[string]any{}
}

// A separate networkless helper owns the onion volume and its public client
// authorization for the configured runtime user before Tor can start.
const adminTorConfig = `DataDirectory /tmp/tor
SocksPort 0
ControlPort 0
ControlSocket /tmp/tor/control.sock
ControlSocketsGroupWritable 0
CookieAuthentication 1
CookieAuthFile /tmp/tor/control_auth_cookie
CookieAuthFileGroupReadable 0
Log notice stdout
HeartbeatPeriod 30 minutes
MaxMemInQueues 128 MB
HiddenServiceDir /var/lib/tor
HiddenServiceVersion 3
HiddenServicePort 80 unix:/run/market-admin/http.sock
HiddenServiceMaxStreams 16
HiddenServiceMaxStreamsCloseCircuit 1
`
