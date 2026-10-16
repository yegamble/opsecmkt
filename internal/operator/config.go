// Package operator provisions an explicit, persistent Docker Compose installation.
// It never installs Docker, runs a shell, rotates an existing key, or removes data volumes.
package operator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const configVersion = 1
const postgresImage = "postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24"

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,48}$`)

// Config contains topology and paths only. Secret values live in private files.
type Config struct {
	DockerContext       string            `json:"docker_context"`
	DependencyReportDir string            `json:"dependency_report_dir,omitempty"`
	RetentionDir        string            `json:"retention_dir"`
	AdminTorImage       string            `json:"admin_tor_image,omitempty"`
	AdminAuthDir        string            `json:"admin_auth_dir,omitempty"`
	AdminSocketDir      string            `json:"admin_socket_dir,omitempty"`
	OnionDir            string            `json:"onion_dir,omitempty"`
	OnionAddress        string            `json:"onion_address,omitempty"`
	AdminOnionDir       string            `json:"admin_onion_dir,omitempty"`
	AdminOnionAddress   string            `json:"admin_onion_address,omitempty"`
	Version             int               `json:"version"`
	Mode                string            `json:"mode"`
	Project             string            `json:"project"`
	AppImage            string            `json:"app_image"`
	PostgresImage       string            `json:"postgres_image,omitempty"`
	TorImage            string            `json:"tor_image,omitempty"`
	ClamavImage         string            `json:"clamav_image,omitempty"`
	MoneroImage         string            `json:"monero_image,omitempty"`
	MoneroWalletImage   string            `json:"monero_wallet_image,omitempty"`
	Exposure            string            `json:"exposure"`
	AppBind             string            `json:"app_bind"`
	BackendBind         string            `json:"backend_bind,omitempty"`
	GatewayURL          string            `json:"gateway_url,omitempty"`
	AppHost             string            `json:"app_host,omitempty"`
	RuntimeUID          int               `json:"runtime_uid"`
	RuntimeGID          int               `json:"runtime_gid"`
	Settings            map[string]string `json:"settings"`
	SecretFiles         map[string]string `json:"secret_files"`
	PublicFiles         map[string]string `json:"public_files,omitempty"`
	TLSFiles            map[string]string `json:"tls_files,omitempty"`
}

var settingKeys = map[string]bool{
	"INITIAL_SITE_ACCESS": true, "INITIAL_REGISTRATIONS": true,
	"MONERO_NETWORK": true, "ESCROW_XMR": true, "PAYMENT_CONFIRMATIONS_XMR": true,
	"PAYMENT_POLL_INTERVAL": true, "PAYMENT_EXPIRY": true, "MONERO_MAX_TIP_AGE": true,
	"ESCROW_XMR_FEE_BPS": true, "ESCROW_XMR_FEE_ADDRESS": true, "ESCROW_XMR_SLOTS": true,
	"ESCROW_XMR_MAX_FEE": true, "ESCROW_XMR_TOR_RELAY": true, "IMAGE_STORAGE_LIMIT_MB": true,
	"SCANNER_MAX_SIGNATURE_AGE_DAYS": true, "DATABASE_CONNECT_TIMEOUT": true,
	"MIGRATION_LOCK_TIMEOUT": true, "MIGRATION_TIMEOUT": true, "RETENTION_ENABLED": true,
}
var secretKeys = map[string]bool{
	"DATABASE_URL": true, "APP_SECRET_KEY": true, "SETUP_TOKEN": true, "AUDIT_SIGNING_KEY": true,
	"MONERO_WALLET_SECRET": true, "ESCROW_XMR_WALLET_RPC_URL": true, "ESCROW_XMR_DAEMON_URL": true,
	"POSTGRES_PASSWORD": true, "ESCROW_XMR_WALLET_RPC_PASSWORD": true,
}
var publicKeys = map[string]bool{"ESCROW_XMR_SEED_KEY": true, "AUDIT_EXPORT_RECIPIENT": true, "DEPENDENCY_REPORT_PUBLIC_KEY": true}
var tlsKeys = map[string]bool{"DATABASE_CA": true, "UPLOAD_CA": true, "UPLOAD_CERT": true, "UPLOAD_KEY": true, "TLS_CERT": true, "TLS_KEY": true, "MONERO_RPC_CA": true, "MONERO_RPC_CERT": true, "MONERO_RPC_KEY": true}

func privateFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("required private file is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private files must be regular, non-symlink files with mode 0600 or stricter")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read required private file")
	}
	defer f.Close()
	actual, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private file changed while being read")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("private file is unreadable or exceeds its size limit")
	}
	return b, nil
}

func loadConfig(dir string) (Config, error) {
	var c Config
	info, err := os.Lstat(dir)
	if err != nil {
		return c, errors.New("installation directory is unavailable; run deploy init first")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return c, errors.New("installation directory must be a real directory with mode 0700 or stricter")
	}
	b, err := privateFile(filepath.Join(dir, "operator.json"), 1<<20)
	if err != nil {
		return c, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil {
		return c, errors.New("operator.json is invalid or contains unknown fields")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return c, errors.New("operator.json must contain one JSON object")
	}
	if c.RetentionDir == "" {
		c.RetentionDir = filepath.Join(dir, "retention")
	}
	if err := validate(dir, c); err != nil {
		return c, err
	}
	if err := checkReceipt(dir, c); err != nil {
		return c, err
	}
	return c, nil
}

func managedPath(dir, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n$") || strings.HasPrefix(path, "..") {
		return "", errors.New("managed file paths must remain within the installation directory")
	}
	parts := strings.Split(path, string(filepath.Separator))
	current := dir
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", errors.New("managed file is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("managed paths cannot contain symlinks")
		}
	}
	return current, nil
}

func secretValue(dir string, c Config, key string) (string, error) {
	path, ok := c.SecretFiles[key]
	if !ok {
		return "", fmt.Errorf("%s needs a managed private file", key)
	}
	path, err := managedPath(dir, path)
	if err != nil {
		return "", err
	}
	b, err := privateFile(path, 64<<10)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if value == "" || strings.ContainsAny(value, "\x00\r\n'") {
		return "", fmt.Errorf("%s contains an empty value or unsupported control/quote characters", key)
	}
	return value, nil
}

func validate(dir string, c Config) error {
	if !projectPattern.MatchString(c.DockerContext) {
		return errors.New("Docker context must be an explicit context name (default: default)")
	}
	if c.Version != configVersion {
		return errors.New("unsupported operator.json version")
	}
	if c.Mode != "dev" && c.Mode != "prod" {
		return errors.New("mode must be dev or prod")
	}
	if !projectPattern.MatchString(c.Project) {
		return errors.New("invalid project name")
	}
	if !imagePattern.MatchString(c.AppImage) {
		return errors.New("--app-image must include an explicit sha256 digest")
	}
	for _, image := range []string{c.PostgresImage, c.TorImage, c.AdminTorImage, c.ClamavImage, c.MoneroImage, c.MoneroWalletImage} {
		if image != "" && !imagePattern.MatchString(image) {
			return errors.New("every selected container image needs an explicit sha256 digest")
		}
	}
	if c.Exposure != "clearnet" && c.Exposure != "tor" {
		return errors.New("exposure must be clearnet or tor")
	}
	if c.RuntimeUID < 1 || c.RuntimeGID < 1 {
		return errors.New("runtime UID and GID must be positive nonroot IDs")
	}
	if !cleanAbsolutePath(c.RetentionDir) {
		return errors.New("retention directory must be an absolute clean persistent directory path")
	}
	if (c.AdminTorImage == "") != (c.AdminAuthDir == "") || (c.AdminTorImage == "") != (c.AdminSocketDir == "") {
		return errors.New("private administrator onion requires both --admin-tor-image and --admin-client-auth-dir")
	}
	if c.AdminTorImage != "" {
		if c.Exposure != "tor" {
			return errors.New("private administrator onion requires Tor exposure")
		}
		if !cleanAbsolutePath(c.AdminSocketDir) {
			return errors.New("administrator socket directory must be an absolute clean path")
		}
		path, err := managedPath(dir, c.AdminAuthDir)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return errors.New("administrator client snapshot must be a real directory without group or other writes")
		}
	} else if c.Mode == "prod" && c.Exposure == "tor" {
		return errors.New("production Tor requires a protected private administrator onion: set --admin-tor-image and --admin-client-auth-dir")
	}
	if err := validateOnionIdentities(dir, c); err != nil {
		return err
	}
	if err := privateBind(c.AppBind); err != nil {
		return err
	}
	if c.BackendBind != "" {
		if err := privateBind(c.BackendBind); err != nil {
			return err
		}
	}
	for key, value := range c.Settings {
		if !settingKeys[key] || strings.ContainsAny(value, "\x00\r\n'") {
			return fmt.Errorf("unsupported or invalid setting %s", key)
		}
	}
	if value, ok := c.Settings["INITIAL_SITE_ACCESS"]; ok && value != "public" && value != "members" {
		return errors.New("INITIAL_SITE_ACCESS must be public or members")
	}
	if value, ok := c.Settings["INITIAL_REGISTRATIONS"]; ok && value != "open" && value != "closed" {
		return errors.New("INITIAL_REGISTRATIONS must be open or closed")
	}
	if c.Settings["MONERO_NETWORK"] != "stagenet" && c.Settings["MONERO_NETWORK"] != "mainnet" {
		return errors.New("MONERO_NETWORK must be stagenet or mainnet")
	}
	if c.Mode == "dev" && c.Settings["MONERO_NETWORK"] != "stagenet" {
		return errors.New("development deployments require Monero stagenet")
	}
	for key := range c.SecretFiles {
		if !secretKeys[key] {
			return errors.New("unknown secret file setting")
		}
		if _, err := secretValue(dir, c, key); err != nil {
			return err
		}
	}
	for key, path := range c.PublicFiles {
		if !publicKeys[key] {
			return errors.New("unknown public-key setting")
		}
		file, err := managedPath(dir, path)
		if err != nil {
			return err
		}
		if _, err := privateFile(file, 64<<10); err != nil {
			return err
		}
	}
	for key, path := range c.TLSFiles {
		if !tlsKeys[key] {
			return errors.New("unknown upload TLS setting")
		}
		file, err := managedPath(dir, path)
		if err != nil {
			return err
		}
		if _, err := privateFile(file, 64<<10); err != nil {
			return err
		}
	}
	for _, key := range []string{"DATABASE_URL", "APP_SECRET_KEY", "SETUP_TOKEN", "AUDIT_SIGNING_KEY"} {
		if _, err := secretValue(dir, c, key); err != nil {
			return err
		}
	}
	if (c.MoneroImage == "") != (c.MoneroWalletImage == "") {
		return errors.New("local Monero requires both daemon and wallet images")
	}
	if c.MoneroImage != "" && c.Settings["ESCROW_XMR"] != "on" {
		return errors.New("local Monero images require --escrow-xmr")
	}
	if c.Settings["ESCROW_XMR"] != "off" && c.Settings["ESCROW_XMR"] != "on" {
		return errors.New("ESCROW_XMR must be off or on")
	}
	if c.Settings["ESCROW_XMR"] == "on" {
		for _, key := range []string{"MONERO_WALLET_SECRET", "ESCROW_XMR_WALLET_RPC_URL", "ESCROW_XMR_DAEMON_URL"} {
			if _, err := secretValue(dir, c, key); err != nil {
				return err
			}
		}
		if c.PublicFiles["ESCROW_XMR_SEED_KEY"] == "" {
			return errors.New("Monero escrow needs an imported public seed-encryption key")
		}
	}
	if c.Exposure == "tor" && c.Mode == "dev" && c.TorImage == "" {
		return errors.New("development Tor deployment needs --tor-image with an explicit digest")
	}
	if c.DependencyReportDir != "" {
		if !filepath.IsAbs(c.DependencyReportDir) || strings.ContainsAny(c.DependencyReportDir, "\x00\r\n$") {
			return errors.New("dependency report directory must be an absolute directory path")
		}
		info, err := os.Lstat(c.DependencyReportDir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("dependency report directory must exist and cannot be a symlink")
		}
		if c.PublicFiles["DEPENDENCY_REPORT_PUBLIC_KEY"] == "" {
			return errors.New("dependency reporting needs an independently imported pinned public key")
		}
	} else if c.PublicFiles["DEPENDENCY_REPORT_PUBLIC_KEY"] != "" {
		return errors.New("dependency report public key requires a report directory")
	}
	if c.Mode == "prod" {
		if c.PostgresImage != "" || c.MoneroImage != "" || c.MoneroWalletImage != "" || c.ClamavImage != "" || c.TorImage != "" {
			return errors.New("production manages the app host only; database, Monero, upload gateway, ClamAV and public Tor ingress must run on separate hosts")
		}
		if !remoteHost(c.AppHost) {
			return errors.New("production requires --app-host naming the private application host")
		}
		dsn, err := secretValue(dir, c, "DATABASE_URL")
		if err != nil {
			return err
		}
		db, err := url.Parse(dsn)
		if err != nil || !(db.Scheme == "postgres" || db.Scheme == "postgresql") || !remoteHost(db.Hostname()) || db.Query().Get("sslmode") != "verify-full" || strings.EqualFold(db.Hostname(), c.AppHost) {
			return errors.New("production database requires a separate remote host and sslmode=verify-full")
		}
		if c.Settings["ESCROW_XMR"] == "on" {
			for _, key := range []string{"ESCROW_XMR_WALLET_RPC_URL", "ESCROW_XMR_DAEMON_URL"} {
				s, _ := secretValue(dir, c, key)
				u, err := url.Parse(s)
				if err != nil || u.Scheme != "https" || !remoteHost(u.Hostname()) || u.RawQuery != "" || u.Fragment != "" || sameHost(u.Hostname(), c.AppHost) || sameHost(u.Hostname(), db.Hostname()) || sameHost(u.Hostname(), gatewayHost(c.GatewayURL)) {
					return fmt.Errorf("%s requires verified HTTPS on a host separate from the app and database", key)
				}
				authenticated := c.TLSFiles["MONERO_RPC_CERT"] != "" && c.TLSFiles["MONERO_RPC_KEY"] != ""
				if u.User != nil {
					password, ok := u.User.Password()
					authenticated = authenticated || (ok && password != "" && u.User.Username() != "")
				}
				if !authenticated {
					return errors.New("production Monero RPC services need credentials or a mutual TLS client identity")
				}

			}
		}
		gateway, err := url.Parse(c.GatewayURL)
		if err != nil || !remoteHost(gateway.Hostname()) || sameHost(gateway.Hostname(), c.AppHost) || sameHost(gateway.Hostname(), db.Hostname()) || gateway.User != nil || gateway.RawQuery != "" || gateway.Fragment != "" || (gateway.Path != "" && gateway.Path != "/") || !(gateway.Scheme == "https" || (gateway.Scheme == "http" && strings.HasSuffix(gateway.Hostname(), ".onion"))) {
			return errors.New("production needs a separate --gateway-url (HTTPS or an HTTP onion)")
		}
		if c.BackendBind == "" {
			return errors.New("production requires a private upload backend bind")
		}
		for _, key := range []string{"UPLOAD_CA", "UPLOAD_CERT", "UPLOAD_KEY", "TLS_CERT", "TLS_KEY"} {
			if c.TLSFiles[key] == "" {
				return errors.New("production requires upload mutual TLS materials and application TLS certificate/key")
			}
		}
	}
	return nil
}

func remoteHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil && !strings.Contains(host, ".") {
		return false
	}
	return ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast())
}
func privateBind(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if err != nil || port == "" || ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
		return errors.New("listener bindings must use an explicit loopback or private IP and port")
	}
	parsed, err := net.LookupPort("tcp", port)
	if err != nil || parsed < 1 || parsed > 65535 {
		return errors.New("invalid listener port")
	}
	return nil
}
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
func gatewayHost(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
