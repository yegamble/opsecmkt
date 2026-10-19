package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"time"
)

// A-552 (owner request 2026-10-18): in the separated topology (UPLOAD_MODE=isolated) the app never contacts clamd,
// so the upload gateway reports its scanner's state to the app after each 30-second poll, over the private
// mutual-TLS upload backend and its existing client identity (UploadBackendHandler refuses any other caller). The
// report holds only fixed classes, version strings, times and counts: no file contents or names, no signature
// names, no account, and no host, address or URL (valid refuses anything else). The app keeps the latest report
// in one settings row with the database's receipt time, so every app process shows the same state, and reads it
// with a staleness rule: no report for gatewayScannerStaleAfter means the scanner state is unknown, and the upload
// forms pause before anyone picks a file, as they do for an unhealthy report. The gateway still checks its own
// scanner before every upload; this state only saves a wasted upload. Integrated (single-host) mode keeps the
// app's own clamd poller and ignores the row.

const (
	gatewayScannerReportPath  = "/v1/uploads/scanner-report"
	gatewayScannerReportLimit = 2 << 10
	gatewayScannerStaleAfter  = 3 * time.Minute // six missed reports
	settingGatewayScanner     = "upload_gateway_scanner"
	gatewayScannerUnknown     = "Scanner status unknown — gateway not reporting"
	gatewayPausedNotice       = "Uploads paused: the virus scanner is unavailable." + listingNoticeTail
)

// gatewayScannerReport is what the gateway sends and the app stores (with "received" added by the database).
type gatewayScannerReport struct {
	State     string    `json:"state"` // scannerReady, scannerUnavailable or scannerStale
	Reachable bool      `json:"reachable"`
	Cause     string    `json:"cause,omitempty"`  // a fixed class from failureCause or stateLocked
	Engine    string    `json:"engine,omitempty"` // "ClamAV 1.4.3"
	Daily     string    `json:"daily,omitempty"`  // daily signature database version, digits
	DailyDate time.Time `json:"daily_date"`       // its build date; zero when unreadable
	AgeDays   int       `json:"age_days"`
	LimitDays int       `json:"limit_days"`
	Checked   time.Time `json:"checked"`   // the gateway's last poll
	LastScan  time.Time `json:"last_scan"` // its last clean scan
	Infected  int       `json:"infected_24h"`
	Failed    int       `json:"failed_24h"`
}

var (
	gatewayEngine = regexp.MustCompile(`^ClamAV [0-9A-Za-z._+~-]{1,40}$`)
	gatewayDaily  = regexp.MustCompile(`^[0-9]{1,12}$`)
	// gatewayCauses are the causes each state may carry: the scanner's own fixed classes.
	gatewayCauses = map[string][]string{
		scannerReady:       {""},
		scannerUnavailable: {"not checked yet", "connection failed", "timed out", "unexpected reply"},
		scannerStale:       {"", "signature date unreadable"},
	}
)

func gatewayTimeValid(t time.Time) bool {
	return t.IsZero() || t.Year() >= 2000 && t.Year() <= 2200
}

// valid reports whether every field is one the gateway's scanner can produce.
func (r gatewayScannerReport) valid() bool {
	causes, ok := gatewayCauses[r.State]
	return ok && slices.Contains(causes, r.Cause) && r.Reachable == (r.State != scannerUnavailable) &&
		(r.Engine == "" || gatewayEngine.MatchString(r.Engine)) && (r.Daily == "" || gatewayDaily.MatchString(r.Daily)) &&
		r.AgeDays >= 0 && r.AgeDays <= 36500 && r.LimitDays >= 7 && r.LimitDays <= 90 &&
		r.Infected >= 0 && r.Infected <= 1e9 && r.Failed >= 0 && r.Failed <= 1e9 &&
		gatewayTimeValid(r.DailyDate) && gatewayTimeValid(r.Checked) && gatewayTimeValid(r.LastScan)
}

// newGatewayScannerReport is the gateway's report of its cached scanner state and 24-hour refusal counts. An
// engine or daily string the app would refuse is left out rather than sent.
func newGatewayScannerReport(st scannerState, c scanCounts) gatewayScannerReport {
	r := gatewayScannerReport{State: st.Name, Reachable: st.Name != scannerUnavailable, Cause: st.Cause, Engine: st.Engine, Daily: st.Daily,
		DailyDate: st.DailyDate.UTC(), AgeDays: st.AgeDays, LimitDays: st.LimitDays, Checked: st.Checked.UTC(), LastScan: st.LastScan.UTC(),
		Infected: min(c.Infected, 1e9), Failed: min(c.Failed, 1e9)}
	if !gatewayEngine.MatchString(r.Engine) {
		r.Engine = ""
	}
	if !gatewayDaily.MatchString(r.Daily) {
		r.Daily = ""
	}
	return r
}

// reportScanner sends the current report once; an error names only its class.
func (g *uploadGateway) reportScanner(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := g.call(ctx, gatewayScannerReportPath, newGatewayScannerReport(g.scanner.State(), g.scanner.counts()))
	if err != nil {
		return err
	}
	if resp.status != http.StatusNoContent {
		return fmt.Errorf("the app refused the scanner report (status %d)", resp.status)
	}
	return nil
}

// run polls the gateway's clamd at once and then every scannerPollInterval, reporting after each poll, until ctx
// ends. A change between delivered and not delivered is logged once, by class:
//
//	upload gateway: scanner report not delivered (upload service unavailable)
func (g *uploadGateway) run(ctx context.Context) {
	t := time.NewTicker(scannerPollInterval)
	defer t.Stop()
	delivered := true
	for {
		g.scanner.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		err := g.reportScanner(ctx)
		if ctx.Err() != nil {
			return
		}
		if (err == nil) != delivered {
			delivered = err == nil
			if delivered {
				log.Print("upload gateway: scanner report delivered")
			} else {
				log.Print("upload gateway: scanner report not delivered (" + err.Error() + ")")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// receiveGatewayScannerReport stores a valid report with the database's receipt time (UploadBackendHandler has
// checked the gateway's client identity).
func (a *App) receiveGatewayScannerReport(w http.ResponseWriter, r *http.Request) {
	var v gatewayScannerReport
	if readUploadJSON(r, gatewayScannerReportLimit, &v) != nil || !v.valid() {
		http.Error(w, "Invalid scanner report", http.StatusBadRequest)
		return
	}
	b, err := json.Marshal(v)
	if err == nil {
		_, err = a.db.ExecContext(r.Context(), `INSERT INTO settings(key,value) VALUES($1,jsonb_set($2::jsonb,'{received}',to_jsonb(clock_timestamp()))::text)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, settingGatewayScanner, string(b))
	}
	if err != nil {
		http5xxLog.Printf("scanner report not stored cause=%s", errorCause(err))
		http.Error(w, "Scanner report not stored", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gatewayScannerStatus is the stored report as one page reads it.
type gatewayScannerStatus struct {
	report   gatewayScannerReport
	received time.Time // zero: no readable report
	fresh    bool      // received within gatewayScannerStaleAfter, by the database's clock
}

// gatewayScanner reads the latest report. A missing or unreadable row is no report.
func (a *App) gatewayScanner(ctx context.Context) (gatewayScannerStatus, error) {
	var raw sql.NullString
	var now time.Time
	if err := a.db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1),clock_timestamp()`, settingGatewayScanner).Scan(&raw, &now); err != nil {
		return gatewayScannerStatus{}, err
	}
	var stored struct {
		gatewayScannerReport
		Received time.Time `json:"received"`
	}
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &stored) != nil || !stored.valid() || stored.Received.IsZero() {
		return gatewayScannerStatus{}, nil
	}
	age := now.Sub(stored.Received)
	return gatewayScannerStatus{report: stored.gatewayScannerReport, received: stored.Received, fresh: age > -time.Minute && age <= gatewayScannerStaleAfter}, nil
}

// state is the scanner state the report stands for; without a fresh report, Unavailable.
func (g gatewayScannerStatus) state() scannerState {
	if !g.fresh {
		return scannerState{Name: scannerUnavailable, Cause: "gateway not reporting", LimitDays: g.report.LimitDays, gateway: true}
	}
	r := g.report
	return scannerState{Name: r.State, Cause: r.Cause, Engine: r.Engine, Daily: r.Daily, DailyDate: r.DailyDate, AgeDays: r.AgeDays, LimitDays: r.LimitDays,
		Checked: r.Checked, LastScan: r.LastScan, gateway: true}
}

// gatewayScannerView is the admin panel for the separated topology.
func gatewayScannerView(g gatewayScannerStatus) *ScannerView {
	v := scannerView(g.state(), scanCounts{Infected: g.report.Infected, Failed: g.report.Failed})
	v.External = true
	if !g.received.IsZero() {
		v.Received = g.received.UTC().Format(scannerStamp)
	}
	if !g.fresh {
		v.Unknown, v.State, v.Detail = true, gatewayScannerUnknown, ""
	}
	return v
}

// previewGatewayScanner replaces the read-only preview's admin scanner panel with the separated topology's:
// ?scanner=gateway a sample healthy report, ?scanner=gateway-unknown a report older than the staleness limit.
func previewGatewayScanner(d *PageData, scanner string) {
	sample := func(h, m int) time.Time { return time.Date(2026, 1, 17, h, m, 0, 0, time.UTC) }
	var g gatewayScannerStatus
	switch scanner {
	case "gateway":
		g = gatewayScannerStatus{fresh: true, received: sample(9, 0), report: gatewayScannerReport{State: scannerReady, Reachable: true, Engine: "ClamAV 1.4.3", Daily: "27999",
			DailyDate: time.Date(2026, 1, 1, 7, 57, 0, 0, time.UTC), AgeDays: 16, LimitDays: 30, Checked: sample(9, 0), LastScan: sample(8, 41), Infected: 1}}
	case "gateway-unknown":
		g = gatewayScannerStatus{received: sample(8, 40), report: gatewayScannerReport{State: scannerReady, Reachable: true, LimitDays: 30}}
	default:
		return
	}
	d.Scanner = gatewayScannerView(g)
	d.Scanner.DatabaseSize = "preview sample"
}

// uploadScannerState is the state the upload forms follow: the app's own cached clamd state, or in the separated
// topology the gateway's latest report.
func (a *App) uploadScannerState(ctx context.Context) (scannerState, error) {
	if !a.isolatedUploads {
		return a.scanner.State(), nil
	}
	g, err := a.gatewayScanner(ctx)
	if err != nil {
		return scannerState{}, err
	}
	return g.state(), nil
}
