package market

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"opsecmkt/internal/dependencies"
)

// DependencyStatusView is populated only after current administrator authorization.
// It contains service labels and version strings, never service endpoint identities.
type DependencyStatusView struct {
	Profile, ReportState, Checked string
	Stale                         bool
	NeedsAttention                bool
	Rows                          []DependencyStatusRow
	Warnings                      []string
}
type DependencyStatusRow struct {
	Service, Current, Latest, Evidence, Status string
	Update                                     bool
}

func init() { registerLoader("admin", loadDependencyStatus) }

func loadDependencyStatus(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	d.DependencyStatus = nil
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	if a.preview {
		d.DependencyStatus = &DependencyStatusView{Profile: "Preview", ReportState: "Version checks are unavailable in read-only preview.", Warnings: []string{"Preview does not inspect deployment configuration or dependency reports."}}
		return nil
	}
	if a.db == nil {
		return nil
	}
	var authorized bool
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL)`, d.User.ID).Scan(&authorized); err != nil {
		return err
	}
	if !authorized {
		return nil
	}
	view := &DependencyStatusView{Profile: os.Getenv("OPSECMKT_PROFILE"), Warnings: dependencyTopology(os.Getenv), NeedsAttention: dependencyTopologyNeedsAttention(os.Getenv)}
	if view.Profile == "" {
		view.Profile = "Not declared"
	}
	report, err := dependencies.ReadVerified(os.Getenv("DEPENDENCY_REPORT_FILE"), os.Getenv("DEPENDENCY_REPORT_PUBLIC_KEY"), time.Now())
	entries := map[string]dependencies.Entry{}
	if err != nil {
		view.NeedsAttention = true
		view.ReportState = "No authenticated dependency report is available. Configure the separate versions worker and its pinned public verification key."
	} else {
		view.ReportState = "Signature verified against the configured worker key."
		view.Checked = report.GeneratedAt.UTC().Format(time.RFC3339)
		view.Stale = time.Since(report.GeneratedAt) > 72*time.Hour
		if view.Stale {
			view.NeedsAttention = true
			view.Warnings = append(view.Warnings, "Dependency report is older than 72 hours. Restore the versions worker before relying on its release information.")
		}
		for _, entry := range report.Entries {
			entries[entry.Service] = entry
		}
	}
	observed := map[string]string{"go": dependencies.Normalize(runtime.Version())}
	var postgres string
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if a.db.QueryRowContext(queryCtx, `SHOW server_version`).Scan(&postgres) == nil {
		if fields := strings.Fields(postgres); len(fields) > 0 {
			observed["postgresql"] = dependencies.Normalize(fields[0])
		}
	}
	cancel()
	evidence := map[string]string{}
	if a.isolatedUploads { // A-552: the upload gateway's latest scanner report
		g, err := a.gatewayScanner(ctx)
		if err != nil {
			return err
		}
		if fields := strings.Fields(g.report.Engine); !g.received.IsZero() && len(fields) == 2 {
			observed["clamav"] = dependencies.Normalize(fields[1])
			evidence["clamav"] = "Reported by the upload gateway's scanner check"
			if !g.fresh {
				evidence["clamav"] += "; that report is stale"
			}
		}
	} else if a.scanner != nil {
		if fields := strings.Fields(a.scanner.State().Engine); len(fields) == 2 {
			observed["clamav"] = dependencies.Normalize(fields[1])
		}
	}
	for _, service := range []string{"postgresql", "monerod", "monero-wallet-rpc", "tor", "clamav", "go"} {
		entry := entries[service]
		row := DependencyStatusRow{Service: dependencyLabel(service), Current: entry.Current, Latest: entry.Latest, Evidence: "Reported by operator inventory", Status: "Unknown"}
		if current := observed[service]; current != "" {
			row.Current = current
			row.Evidence = "Observed on this app connection or process"
			if e := evidence[service]; e != "" {
				row.Evidence = e
			}
		}
		if row.Current == "" {
			row.Current = "Unknown"
			row.Evidence = "Installed version has not been reported"
		}
		if row.Latest == "" {
			row.Latest = "Unknown"
			row.Status = "Release check unavailable"
		} else if row.Current == "Unknown" {
			row.Status = "Installed version unknown"
		} else if dependencies.Newer(row.Latest, row.Current) {
			row.Update = true
			row.Status = "Newer upstream release available"
		} else if dependencies.Newer(row.Current, row.Latest) {
			row.Status = "Installed version is newer than report"
		} else {
			row.Status = "Matches reported upstream release"
		}
		if view.Stale {
			row.Status += "; report stale"
		}
		if row.Update || row.Current == "Unknown" || row.Latest == "Unknown" {
			view.NeedsAttention = true
		}
		view.Rows = append(view.Rows, row)
	}
	d.DependencyStatus = view
	return nil
}

func dependencyTopologyNeedsAttention(getenv func(string) string) bool {
	if getenv("OPSECMKT_PROFILE") != "prod" || getenv("UPLOAD_MODE") != "isolated" {
		return true
	}
	app := strings.ToLower(strings.TrimSuffix(getenv("OPSECMKT_APP_HOST"), "."))
	if app == "" {
		return true
	}
	keys := []string{"DATABASE_URL", "OPSECMKT_GATEWAY_URL"}
	if getenv("ESCROW_XMR") == "on" {
		keys = append(keys, "ESCROW_XMR_DAEMON_URL", "ESCROW_XMR_WALLET_RPC_URL")
	}
	for _, key := range keys {
		u, err := url.Parse(getenv(key))
		if err != nil {
			return true
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "" || dependencyLocalHost(host) || host == app {
			return true
		}
	}
	return false
}

func dependencyLabel(service string) string {
	switch service {
	case "postgresql":
		return "PostgreSQL"
	case "monerod":
		return "Monero daemon"
	case "monero-wallet-rpc":
		return "Monero wallet RPC"
	case "tor":
		return "Tor"
	case "clamav":
		return "ClamAV"
	case "go":
		return "Go application runtime"
	}
	return "Unknown service"
}

func dependencyTopology(getenv func(string) string) []string {
	var warnings []string
	switch getenv("OPSECMKT_PROFILE") {
	case "prod":
		warnings = append(warnings, "Production profile is configured. Different endpoint names declare separation; verify actual hosts, firewall rules and service identities operationally.")
	case "dev":
		warnings = append(warnings, "Development profile permits a single host. Switch to production checks before public deployment.")
	default:
		warnings = append(warnings, "Deployment profile is not declared. Production OPSEC checks are not enabled.")
	}
	app := strings.ToLower(strings.TrimSuffix(getenv("OPSECMKT_APP_HOST"), "."))
	classify := func(label, raw string, isAddress bool) {
		host := ""
		if isAddress {
			host, _, _ = net.SplitHostPort(raw)
		} else if parsed, err := url.Parse(raw); err == nil {
			host = parsed.Hostname()
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		switch {
		case host == "":
			warnings = append(warnings, label+": host separation is unknown or this service is not configured.")
		case dependencyLocalHost(host) || app != "" && host == app:
			warnings = append(warnings, label+": configured on the app host or a local address; separate this layer for production.")
		default:
			warnings = append(warnings, label+": a different endpoint is declared; physical host separation remains unverified.")
		}
	}
	classify("Database", getenv("DATABASE_URL"), false)
	if getenv("ESCROW_XMR") == "on" {
		classify("Monero daemon", getenv("ESCROW_XMR_DAEMON_URL"), false)
		classify("Monero wallet RPC", getenv("ESCROW_XMR_WALLET_RPC_URL"), false)
	} else {
		warnings = append(warnings, "Monero services: escrow is disabled; running service versions and topology are unknown.")
	}
	if getenv("UPLOAD_MODE") != "isolated" {
		warnings = append(warnings, "Upload processing: original images are handled by the main app. Use the isolated upload gateway for production.")
		classify("ClamAV", getenv("CLAMD_ADDRESS"), true)
	} else {
		classify("Upload gateway", getenv("OPSECMKT_GATEWAY_URL"), false)
		warnings = append(warnings, "ClamAV: scanning is delegated to the upload gateway. Verify it shares no database, wallet or application host access.")
		warnings = append(warnings, "The same-origin upload gateway proxies account traffic and is trusted with browser credentials and sessions; it must not hold database, wallet or app master keys.")
	}
	if getenv("APP_MODE") == "tor" {
		warnings = append(warnings, "Tor: verify public ingress placement and egress rules on the gateway host; endpoint declarations cannot prove anonymity or resist traffic correlation.")
		if strings.TrimSpace(getenv("ESCROW_XMR_TOR_RELAY")) == xmrTorRelayRemote {
			warnings = append(warnings, "Monero relay: remote Tor egress is declared by the operator. The application cannot verify the payment host's proxy or firewall; verify that direct daemon egress is blocked after every network change.")
		}
	}
	return warnings
}

func dependencyLocalHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "db" || host == "clamd" || host == "monero" || host == "monero-wallet" || host == "monero-escrow-wallet" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}
