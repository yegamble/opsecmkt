package operator

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type service map[string]any

func hardened(image string, networks []string, memory string) service {
	return service{"image": image, "restart": "unless-stopped", "networks": networks, "read_only": true, "tmpfs": []string{"/tmp"}, "cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"}, "mem_limit": memory, "pids_limit": 256, "cpu_period": 100000, "cpu_quota": 200000,
		"logging": map[string]any{"driver": "json-file", "options": map[string]string{"max-size": "10m", "max-file": "3"}}}
}

// render returns Compose JSON and non-secret application environment values.
func render(dir string, c Config) ([]byte, map[string]string, error) {
	if err := validate(dir, c); err != nil {
		return nil, nil, err
	}
	if err := validateMaterial(dir, c); err != nil {
		return nil, nil, err
	}
	env := map[string]string{"OPSECMKT_PROFILE": c.Mode, "OPSECMKT_APP_HOST": c.AppHost, "OPSECMKT_GATEWAY_URL": c.GatewayURL, "ADDR": ":8080", "APP_MODE": c.Exposure, "COOKIE_SECURE": "true", "UPLOAD_MODE": "integrated", "GOMEMLIMIT": "400MiB", "RETENTION_STATE_DIR": "/var/lib/opsecmkt-retention"}
	if c.Mode == "dev" || c.Exposure == "tor" {
		env["COOKIE_SECURE"] = "false"
	}
	for key, value := range c.Settings {
		env[key] = value
	}
	for key, path := range c.SecretFiles {
		if key == "POSTGRES_PASSWORD" || key == "ESCROW_XMR_WALLET_RPC_PASSWORD" {
			continue
		}
		env[key+"_FILE"] = "/run/opsecmkt-secrets/" + filepath.Base(path)
	}
	for key, path := range c.PublicFiles {
		env[key] = "/run/opsecmkt-secrets/" + filepath.Base(path)
	}
	for key, path := range c.TLSFiles {
		env[key] = "/run/opsecmkt-secrets/" + filepath.Base(path)
	}
	app := hardened(c.AppImage, []string{"backend"}, "512m")
	app["user"] = numericID(c.RuntimeUID, c.RuntimeGID)
	app["command"] = []string{"serve"}
	app["environment"] = env
	app["volumes"] = []string{filepath.Join(dir, "secrets") + ":/run/opsecmkt-secrets:ro", c.RetentionDir + ":/var/lib/opsecmkt-retention"}
	if c.DependencyReportDir != "" {
		app["volumes"] = append(app["volumes"].([]string), c.DependencyReportDir+":/run/opsecmkt-dependencies:ro")
		env["DEPENDENCY_REPORT_FILE"] = "/run/opsecmkt-dependencies/report.json"
	}
	app["stop_grace_period"] = "60s"
	app["healthcheck"] = map[string]any{"test": []string{"CMD", "/app/server", "-healthcheck"}, "interval": "10s", "timeout": "5s", "retries": 3, "start_period": "2m"}

	app["ports"] = []string{portMapping(c.AppBind, "8080")}
	services := map[string]service{"app": app}
	volumes := map[string]any{}
	networks := map[string]any{"backend": map[string]any{}}
	if c.Mode == "prod" {
		app["logging"] = map[string]any{"driver": "none"}
		env["UPLOAD_MODE"] = "isolated"
		env["UPLOAD_BACKEND_ADDR"] = ":8443"
		app["ports"] = []string{portMapping(c.AppBind, "8080"), portMapping(c.BackendBind, "8443")}
	}
	if c.PostgresImage != "" {
		db := hardened(c.PostgresImage, []string{"backend"}, "1g")
		db["user"] = "70:70"
		db["tmpfs"] = []string{"/tmp", "/var/run/postgresql"}
		db["env_file"] = []string{filepath.Join(dir, "postgres.env")}
		db["environment"] = map[string]string{"POSTGRES_DB": "opsecmkt", "POSTGRES_USER": "opsecmkt", "POSTGRES_INITDB_ARGS": "--encoding=UTF8"}
		db["volumes"] = []string{"postgres_data:/var/lib/postgresql/data"}
		db["healthcheck"] = map[string]any{"test": []string{"CMD", "pg_isready", "-h", "127.0.0.1", "-U", "opsecmkt", "-d", "opsecmkt"}, "interval": "5s", "timeout": "5s", "retries": 20}
		services["db"] = db
		volumes["postgres_data"] = map[string]any{}
		app["depends_on"] = map[string]any{"db": map[string]string{"condition": "service_healthy"}}
	}
	if c.ClamavImage != "" {
		env["CLAMD_ADDRESS"] = "clamd:3310"
		app["networks"] = []string{"backend", "scanner"}
		networks["scanner"] = map[string]any{"internal": true}
		clamd := hardened(c.ClamavImage, []string{"scanner"}, "3g")
		clamd["entrypoint"] = []string{"clamd"}
		clamd["command"] = []string{"--foreground", "--config-file=/etc/clamav/clamd.conf"}
		clamd["user"] = "clamav"
		clamd["tmpfs"] = []string{"/tmp:size=64m"}
		clamd["volumes"] = []string{filepath.Join(dir, "clamd.conf") + ":/etc/clamav/clamd.conf:ro"}
		clamd["healthcheck"] = map[string]any{"test": []string{"CMD", "clamdcheck.sh"}, "interval": "30s", "timeout": "10s", "retries": 3, "start_period": "10m"}
		services["clamd"] = clamd
	}
	if c.MoneroImage != "" {
		networks["node_egress"] = map[string]any{}
		daemon := hardened(c.MoneroImage, []string{"backend", "node_egress"}, "2g")
		daemon["user"] = "1000:1000"
		daemon["pids_limit"] = 512
		daemon["command"] = []string{"--stagenet", "--data-dir=/home/monero/.bitmonero", "--restricted-rpc", "--rpc-bind-ip=127.0.0.1", "--rpc-bind-port=18089", "--rpc-restricted-bind-ip=0.0.0.0", "--rpc-restricted-bind-port=18081", "--confirm-external-bind", "--no-igd", "--no-zmq", "--prune-blockchain", "--sync-pruned-blocks"}
		daemon["volumes"] = []string{"monero_data:/home/monero/.bitmonero"}
		services["monero"] = daemon
		volumes["monero_data"] = map[string]any{}
		wallet := hardened(c.MoneroWalletImage, []string{"backend"}, "1g")
		wallet["user"] = "1000:1000"
		wallet["command"] = []string{"--config-file=/etc/opsecmkt-wallet.conf"}
		wallet["volumes"] = []string{"monero_wallet:/home/monero/wallet", filepath.Join(dir, "monero-wallet.conf") + ":/etc/opsecmkt-wallet.conf:ro"}
		services["monero-wallet"] = wallet
		volumes["monero_wallet"] = map[string]any{}
	}
	if c.TorImage != "" {
		networks["tor_egress"] = map[string]any{}
		// External dependencies need explicit egress. This controller never claims Tor-routed outbound traffic.
		if c.PostgresImage != "" && c.Settings["ESCROW_XMR"] == "off" {
			networks["backend"] = map[string]any{"internal": true}
		}
		tor := hardened(c.TorImage, []string{"backend", "tor_egress"}, "256m")
		tor["user"] = numericID(c.RuntimeUID, c.RuntimeGID)
		tor["volumes"] = []string{"tor_identity:/var/lib/tor", filepath.Join(dir, "torrc") + ":/etc/tor/torrc:ro"}
		tor["depends_on"] = map[string]any{"app": map[string]any{"condition": "service_started", "restart": true}, "tor-identity-prepare": map[string]string{"condition": "service_completed_successfully"}}
		prepare := hardened(c.AppImage, nil, "64m")
		delete(prepare, "networks")
		prepare["network_mode"], prepare["restart"], prepare["user"] = "none", "no", "0:0"
		prepare["cap_add"] = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
		prepare["pids_limit"] = 16
		prepare["volumes"] = []string{"tor_identity:/var/lib/tor"}
		prepare["command"] = []string{"tor-identity-prepare", "--destination", "/var/lib/tor", "--subdir", "marketplace", "--uid", strconv.Itoa(c.RuntimeUID), "--gid", strconv.Itoa(c.RuntimeGID)}
		if c.OnionDir != "" {
			prepare["volumes"] = append(prepare["volumes"].([]string), filepath.Join(dir, c.OnionDir)+":/run/tor-public-identity:ro")
			prepare["command"] = append(prepare["command"].([]string), "--identity-source", "/run/tor-public-identity")
		}
		services["tor-identity-prepare"] = prepare
		services["tor"] = tor
		volumes["tor_identity"] = map[string]any{}
		delete(app, "ports")
	}
	if c.AdminTorImage != "" {
		renderAdminTor(dir, c, app, services, volumes, networks)
	}
	data, err := json.MarshalIndent(map[string]any{"name": c.Project, "services": services, "volumes": volumes, "networks": networks}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(data, '\n'), env, nil
}
func portMapping(bind, target string) string {
	host, port, _ := net.SplitHostPort(bind)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port + ":" + target
}

func materialize(dir string, c Config) error {
	if err := checkRetentionStorage(dir, c); err != nil {
		return err
	}
	compose, _, err := render(dir, c)
	if err != nil {
		return err
	}
	retention := c.RetentionDir
	if err := os.Mkdir(retention, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(retention)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("retention state must remain a private real directory")
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(retention, c.RuntimeUID, c.RuntimeGID); err != nil {
			return err
		}
	}
	if c.PostgresImage != "" {
		password, err := secretValue(dir, c, "POSTGRES_PASSWORD")
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "postgres.env"), []byte("POSTGRES_PASSWORD='"+password+"'\n"), 0600); err != nil {
			return err
		}
	}
	if c.MoneroImage != "" {
		password, err := secretValue(dir, c, "ESCROW_XMR_WALLET_RPC_PASSWORD")
		if err != nil {
			return err
		}
		walletConfig := "stagenet=1\ndaemon-address=monero:18081\ntrusted-daemon=1\nwallet-dir=/home/monero/wallet\nshared-ringdb-dir=/home/monero/wallet/.shared-ringdb\nrpc-bind-ip=0.0.0.0\nrpc-bind-port=18083\nconfirm-external-bind=1\nlog-file=/tmp/monero-wallet-rpc.log\nmax-log-file-size=1048576\nmax-log-files=2\nrpc-login=escrow:" + password + "\n"
		// The file is inside the operator's 0700 directory. Its container sees just this one read-only file.
		if err := atomicWrite(filepath.Join(dir, "monero-wallet.conf"), []byte(walletConfig), 0644); err != nil {
			return err
		}
	}
	if c.ClamavImage != "" {
		if err := atomicWrite(filepath.Join(dir, "clamd.conf"), []byte(clamdConfig), 0644); err != nil {
			return err
		}
	}
	if c.TorImage != "" {
		if err := atomicWrite(filepath.Join(dir, "torrc"), []byte(torConfig), 0644); err != nil {
			return err
		}
	}
	if c.AdminTorImage != "" {
		if err := prepareAdminAuthSnapshot(dir, c); err != nil {
			return err
		}
		if err := prepareAdminSocket(c); err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(dir, "torrc-admin"), []byte(adminTorConfig), 0644); err != nil {
			return err
		}
	}
	return atomicWrite(filepath.Join(dir, "compose.json"), compose, 0600)
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing to overwrite a symlink or nonregular runtime file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".operator-write-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

const torConfig = `DataDirectory /tmp/tor
SocksPort 0
Log notice stdout
HeartbeatPeriod 30 minutes
MaxMemInQueues 128 MB
HiddenServiceDir /var/lib/tor/marketplace
HiddenServiceVersion 3
HiddenServicePort 80 app:8080
HiddenServicePoWDefensesEnabled 1
HiddenServicePoWQueueRate 60
HiddenServicePoWQueueBurst 600
HiddenServiceEnableIntroDoSDefense 1
HiddenServiceEnableIntroDoSRatePerSec 25
HiddenServiceEnableIntroDoSBurstPerSec 200
HiddenServiceMaxStreams 32
HiddenServiceMaxStreamsCloseCircuit 1
`
const clamdConfig = `TCPSocket 3310
TCPAddr 0.0.0.0
Foreground yes
EnableShutdownCommand no
EnableReloadCommand no
EnableStatsCommand no
DatabaseDirectory /var/lib/clamav
SelfCheck 600
ConcurrentDatabaseReload no
TemporaryDirectory /tmp
LeaveTemporaryFiles no
LogTime yes
LogClean no
StreamMaxLength 9M
MaxFileSize 9M
MaxScanSize 18M
AlertExceedsMax yes
ScanImage yes
ScanImageFuzzyHash no
AlertBrokenMedia no
`

// SortedSettings is useful to documentation and inspection tools without exposing values.
func SortedSettings() []string {
	keys := make([]string, 0, len(settingKeys))
	for key := range settingKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
