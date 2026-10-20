package market

import (
	"cmp"
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
// It contains service labels, version strings, fixed health text, error classes and times, never service endpoint
// identities.
type DependencyStatusView struct {
	Profile, ReportState, Checked string
	Stale                         bool
	NeedsAttention                bool
	WorkerMissing                 bool // no authenticated report: the panel names the operator step (A-553)
	Rows                          []DependencyStatusRow
	Warnings                      []string
}
type DependencyStatusRow struct {
	Service, Current, Latest, Evidence, Status string
	Update                                     bool
	// Health (A-553): reachable, synced, last successful check and last error class, as this app observed them; no
	// URL, host, address or credential. HealthAttention marks a failing service.
	Health          []string
	HealthAttention bool
}

const (
	// marketServiceLabel is the dependency table's row for this market's own build (A-553).
	marketServiceLabel = "OPSEC Market (this app)"
	dependencyNoReport = "No authenticated dependency report is available, so installed versions are not compared with upstream releases."
)

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
		view.NeedsAttention, view.WorkerMissing = true, true
		view.ReportState = dependencyNoReport
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
	now := time.Now()
	observed := map[string]string{"go": dependencies.Normalize(runtime.Version())}
	evidence := map[string]string{"go": "Observed by this app (its own process)", "postgresql": "Observed by this app (its database connection)"}
	health := map[string][]string{"go": {"Running in this process"}, "tor": {"Not observed by this app: it has no connection to Tor"}}
	attention := map[string]bool{}
	var postgres string
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	pgErr := a.db.QueryRowContext(queryCtx, `SHOW server_version`).Scan(&postgres)
	cancel()
	if fields := strings.Fields(postgres); pgErr == nil && len(fields) > 0 {
		observed["postgresql"] = dependencies.Normalize(fields[0])
	}
	health["postgresql"] = []string{"Reachable: answered this page's queries, " + now.UTC().Format(scannerStamp)}
	if pgErr != nil {
		health["postgresql"] = append(health["postgresql"], "Version query failed ("+errorCause(pgErr)+")")
		attention["postgresql"] = true
	}
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
		if g.received.IsZero() {
			health["clamav"], attention["clamav"] = []string{"No scanner report from the upload gateway yet"}, true
		} else {
			state := g.report.State
			if g.report.Cause != "" {
				state += " (" + g.report.Cause + ")"
			}
			health["clamav"] = []string{"Upload gateway reports: " + state, "Report received: " + healthStamp(g.received, now)}
			attention["clamav"] = !g.fresh || g.report.State != scannerReady
			if !g.fresh {
				health["clamav"] = append(health["clamav"], "Report stale: none received for over 3 minutes")
			}
		}
	} else if a.scanner != nil {
		st := a.scanner.State()
		if fields := strings.Fields(st.Engine); len(fields) == 2 {
			observed["clamav"] = dependencies.Normalize(fields[1])
			evidence["clamav"] = "Observed by this app (its clamd connection)"
		}
		health["clamav"] = []string{"Scanner: " + describeState(st)}
		if !st.Checked.IsZero() {
			health["clamav"] = append(health["clamav"], "Last checked by this app: "+healthStamp(st.Checked, now))
		}
		attention["clamav"] = st.Name != scannerReady && st.Name != scannerDisabled
	} else {
		health["clamav"] = []string{"No scanner on this app"}
	}
	// A-553: the Monero nodes as this app's watcher last saw them over their RPC (escrow_xmr_health.go). monerod
	// reports its release in get_info except on a restricted RPC; get_version gives only the RPC interface version,
	// the only version monero-wallet-rpc reports.
	rpcOnly := map[string]string{}
	if a.escrowXMR == nil {
		for _, service := range []string{"monerod", "monero-wallet-rpc"} {
			health[service] = []string{"Monero escrow is off on this app (ESCROW_XMR): not checked"}
		}
	} else {
		h, err := a.xmrHealth(ctx)
		if err != nil {
			return err
		}
		if s := h.rec.Daemon; s.Release != "" {
			observed["monerod"], evidence["monerod"] = s.Release, "Observed by this app (monerod get_info)"
		} else if s.RPC != "" {
			rpcOnly["monerod"] = "Observed by this app: RPC interface " + s.RPC + "; this daemon's restricted RPC does not report its release number"
		}
		if s := h.rec.Wallet; s.RPC != "" {
			rpcOnly["monero-wallet-rpc"] = "Observed by this app: wallet RPC interface " + s.RPC + "; monero-wallet-rpc does not report its release number over RPC"
		}
		health["monerod"], attention["monerod"] = h.nodeHealth(true)
		health["monero-wallet-rpc"], attention["monero-wallet-rpc"] = h.nodeHealth(false)
	}
	view.Rows = append(view.Rows, marketBuildRow())
	services := []string{"postgresql", "monerod", "monero-wallet-rpc", "tor", "clamav", "go"}
	if _, inventoried := entries[dependencies.InferenceRuntimeService]; inventoried {
		services = append(services, dependencies.InferenceRuntimeService)
		health[dependencies.InferenceRuntimeService] = []string{"Not observed by this app: inference runs in a separate worker"}
	}
	for _, service := range services {
		entry := entries[service]
		row := DependencyStatusRow{Service: dependencyLabel(service), Current: entry.Current, Latest: entry.Latest, Evidence: "Reported by operator inventory", Status: "Unknown",
			Health: health[service], HealthAttention: attention[service]}
		if current := observed[service]; current != "" {
			row.Current = current
			row.Evidence = cmp.Or(evidence[service], "Observed by this app")
		}
		if row.Current == "" {
			row.Current = "Unknown"
			row.Evidence = "Installed version has not been reported"
		}
		if note := rpcOnly[service]; note != "" && observed[service] == "" {
			row.Evidence = note
			if entry.Current != "" {
				row.Evidence += "; the release shown is reported by operator inventory"
			}
		}
		if row.Latest == "" && view.WorkerMissing {
			row.Latest = "Not checked"
			row.Status = "Not compared: versions worker not configured"
		} else if row.Latest == "" {
			row.Latest = "Unknown"
			row.Status = "Release check unavailable"
		} else if row.Current == "Unknown" {
			row.Status = "Installed version unknown"
		} else if dependencies.NewerFor(service, row.Latest, row.Current) {
			row.Update = true
			row.Status = "Newer upstream release available"
		} else if dependencies.NewerFor(service, row.Current, row.Latest) {
			row.Status = "Installed version is newer than report"
		} else {
			row.Status = "Matches reported upstream release"
		}
		if view.Stale {
			row.Status += "; report stale"
		}
		if row.Update || row.Current == "Unknown" || entry.Latest == "" || row.HealthAttention {
			view.NeedsAttention = true
		}
		view.Rows = append(view.Rows, row)
	}
	d.DependencyStatus = view
	return nil
}

// previewStatusPanels gives the read-only preview's System and Payments panels a labelled sample (A-553): ?status=sample
// healthy services and watcher, ?status=attention a failing watcher pass with an unreachable daemon, wallet creation
// stopped and no versions worker. Layout and copy only: TestDependencyStatusObservesMonero and
// TestXMREscrowAdminShowsPassFailures prove the observations against PostgreSQL and simulated Monero RPC.
func previewStatusPanels(d *PageData, status string) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 17, h, m, 0, 0, time.UTC) }
	h := xmrHealthStatus{found: true, now: at(9, 2), rec: xmrHealthRecord{Checked: at(9, 1), Good: at(9, 1),
		Daemon: xmrSeen{OK: at(9, 1), Release: "0.18.5.1", InfoAt: at(9, 1), Height: 3312480, Synchronized: true},
		Wallet: xmrSeen{OK: at(9, 1), RPC: "1.31"}}}
	attention := status == "attention"
	switch {
	case attention:
		h.rec.Good, h.rec.CreationStopped = at(8, 31), true
		h.rec.Cause = "monero escrow: daemon check failed: get_info timeout"
		h.rec.Daemon.OK, h.rec.Daemon.Error, h.rec.Daemon.ErrorAt = at(8, 31), "get_info timeout", at(9, 1)
	case status != "sample":
		return
	}
	d.XMREscrowHealth = h.view()
	v := &DependencyStatusView{Profile: "Preview sample", ReportState: "Preview sample: no dependency report is read.", NeedsAttention: attention, WorkerMissing: attention,
		Warnings: []string{"Preview sample: the read-only preview inspects no deployment configuration, service or dependency report."}}
	if attention {
		v.ReportState = dependencyNoReport
	}
	row := func(service, current, evidence, latest string, health []string, bad bool) DependencyStatusRow {
		r := DependencyStatusRow{Service: service, Current: current, Evidence: evidence, Latest: latest, Status: "Matches reported upstream release", Health: health, HealthAttention: bad}
		if attention {
			r.Latest, r.Status = "Not checked", "Not compared: versions worker not configured"
		}
		return r
	}
	market := marketBuildRow()
	market.Current, market.Evidence = "v0.0.0-preview", "Preview sample build; commit 0123456789ab"
	daemonHealth, daemonBad := h.nodeHealth(true)
	walletHealth, walletBad := h.nodeHealth(false)
	v.Rows = []DependencyStatusRow{market,
		row("PostgreSQL", "17.6", "Observed by this app (its database connection)", "17.6", []string{"Reachable: answered this page's queries, 2026-01-17 09:02 UTC"}, false),
		row("Monero daemon", "0.18.5.1", "Observed by this app (monerod get_info)", "0.18.5.1", daemonHealth, daemonBad),
		row("Monero wallet RPC", "0.18.5.1", "Observed by this app: wallet RPC interface 1.31; monero-wallet-rpc does not report its release number over RPC; the release shown is reported by operator inventory", "0.18.5.1", walletHealth, walletBad),
		row("Tor", "0.4.8.17", "Reported by operator inventory", "0.4.8.17", []string{"Not observed by this app: it has no connection to Tor"}, false),
		row("ClamAV", "1.4.3", "Reported by the upload gateway's scanner check", "1.4.3", []string{"Upload gateway reports: Ready", "Report received: 2026-01-17 09:01 UTC (1 minute ago)"}, false),
		row("Go application runtime", "1.27.2", "Observed by this app (its own process)", "1.27.2", []string{"Running in this process"}, false),
	}
	d.DependencyStatus = v
}

// marketBuildRow is the dependency table's row for this market's own build: its version and commit (A-553). The
// versions worker does not track the market's releases.
func marketBuildRow() DependencyStatusRow {
	version, commit, modified := marketBuild()
	row := DependencyStatusRow{Service: marketServiceLabel, Current: version, Latest: "Not checked", Status: "Not tracked by the versions worker",
		Evidence: "This running build; commit not recorded in it", Health: []string{"Serving this page"}}
	if len(commit) >= 12 && strings.Trim(commit, "0123456789abcdef") == "" {
		row.Evidence = "This running build; commit " + commit[:12]
		if modified {
			row.Evidence += ", built with uncommitted changes"
		}
	}
	return row
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
	case dependencies.InferenceRuntimeService:
		return "llama.cpp inference runtime"
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
