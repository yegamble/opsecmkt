package market

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// CheckDeploymentProfile enforces the production service contract before opening
// a database or wallet. Host identities are declarations: aliases, NAT and physical
// host placement still require operator verification and network policy.
func CheckDeploymentProfile(getenv func(string) string) error {
	profile := getenv("OPSECMKT_PROFILE")
	if strings.TrimSpace(getenv("ESCROW_XMR_TOR_RELAY")) == xmrTorRelayRemote &&
		(profile != "prod" || getenv("APP_MODE") != "tor" || getenv("ESCROW_XMR") != "on") {
		return errors.New("ESCROW_XMR_TOR_RELAY=remote-tor requires the production Tor profile with Monero escrow enabled")
	}
	if profile == "" || profile == "dev" {
		return nil
	}
	if profile != "prod" {
		return errors.New("OPSECMKT_PROFILE must be dev or prod")
	}
	app := profileHost(getenv("OPSECMKT_APP_HOST"))
	if !separateHost(app) {
		return errors.New("production requires OPSECMKT_APP_HOST identifying the private app host")
	}
	if getenv("APP_MODE") == "tor" && getenv("ADMIN_SOCKET") == "" {
		return errors.New("production Tor mode requires ADMIN_SOCKET and a separately client-authorized administrator onion")
	}
	if getenv("UPLOAD_MODE") != "isolated" || getenv("CLAMD_ADDRESS") != "" {
		return errors.New("production requires isolated uploads; raw uploads and ClamAV must run on the separate gateway host")
	}
	if getenv("TLS_CERT") == "" || getenv("TLS_KEY") == "" {
		return errors.New("production requires TLS_CERT and TLS_KEY for the gateway-to-application connection")
	}
	db, err := url.Parse(getenv("DATABASE_URL"))
	if err != nil || (db.Scheme != "postgres" && db.Scheme != "postgresql") || !separateHost(profileHost(db.Hostname())) || db.Query().Get("sslmode") != "verify-full" {
		return errors.New("production requires an external PostgreSQL URL with sslmode=verify-full")
	}
	hosts := map[string]bool{app: true}
	if hosts[profileHost(db.Hostname())] {
		return errors.New("production database must be separate from the application")
	}
	hosts[profileHost(db.Hostname())] = true
	gateway, err := url.Parse(getenv("OPSECMKT_GATEWAY_URL"))
	if err != nil || !separateHost(profileHost(gateway.Hostname())) || gateway.User != nil || gateway.RawQuery != "" || gateway.Fragment != "" || (gateway.Path != "" && gateway.Path != "/") || !(gateway.Scheme == "https" || gateway.Scheme == "http" && strings.HasSuffix(profileHost(gateway.Hostname()), ".onion")) {
		return errors.New("production requires OPSECMKT_GATEWAY_URL on a separate HTTPS or HTTP onion gateway host")
	}
	if hosts[profileHost(gateway.Hostname())] {
		return errors.New("production upload gateway must be separate from the application and database")
	}
	hosts[profileHost(gateway.Hostname())] = true
	if getenv("ESCROW_XMR") == "on" {
		for _, name := range []string{"ESCROW_XMR_WALLET_RPC_URL", "ESCROW_XMR_DAEMON_URL"} {
			u, err := url.Parse(getenv(name))
			if err != nil || u.Scheme != "https" || !separateHost(profileHost(u.Hostname())) || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("production " + name + " requires a separate remote HTTPS service")
			}
			if hosts[profileHost(u.Hostname())] {
				return errors.New("production Monero services must be separate from application, database and upload gateway hosts")
			}
			password, ok := "", false
			if u.User != nil {
				password, ok = u.User.Password()
				ok = ok && u.User.Username() != "" && password != ""
			}
			if !ok && (getenv("MONERO_RPC_CERT") == "" || getenv("MONERO_RPC_KEY") == "") {
				return errors.New("production Monero services require RPC credentials or a mutual TLS client identity")
			}
		}
	}
	return nil
}

func profileHost(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func separateHost(host string) bool {
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast()
}
