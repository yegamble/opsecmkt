package market

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// A-552: in the separated topology the upload gateway reports its scanner state to the app over the private
// mutual-TLS upload backend; the app keeps the latest report in PostgreSQL with the database's receipt time, and
// the admin panel, the upload forms and the dependency table read it with a staleness rule.

// postScannerReport sends body to the report path of the backend at base with tc (nil: no TLS client config).
func postScannerReport(t *testing.T, base string, tc *tls.Config, body []byte) (int, error) {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: tc}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Post(base+gatewayScannerReportPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func scannerReportRow(t *testing.T, e *testEnv) string {
	t.Helper()
	var v string
	if err := e.DB.QueryRow(`SELECT coalesce((SELECT value FROM settings WHERE key=$1),'')`, settingGatewayScanner).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// pageSection returns the HTML of the element with id, up to the end of its section.
func pageSection(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no #%s on the page", id)
	}
	end := strings.Index(page[i:], "</section>")
	if end < 0 {
		t.Fatalf("#%s has no end", id)
	}
	return page[i : i+end]
}

const wantGatewayPaused = "Uploads paused: the virus scanner is unavailable."

func TestGatewayScannerReport(t *testing.T) {
	t.Setenv("UPLOAD_MODE", "isolated")
	e := newTestApp(t)
	_, admin := e.user("gateway_report_admin", "admin")
	vendorID, vendor := e.user("gateway_report_vendor", "vendor")
	listing := e.product(vendorID, "physical")

	get := func(a *App, path, session string) string {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: session})
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	// pausedForms checks both upload forms of a: paused before a file is picked (true) or ready (false).
	pausedForms := func(a *App, paused bool) {
		t.Helper()
		avatar := pageSection(t, get(a, "/account", vendor), "avatar")
		photos := pageSection(t, get(a, "/listing-edit?id="+url.QueryEscape(listing), vendor), "photos")
		for name, s := range map[string]string{"avatar": avatar, "photos": photos} {
			disabled := strings.Contains(s, "<fieldset disabled")
			notice := strings.Contains(s, wantGatewayPaused)
			if disabled != paused || notice != paused || !strings.Contains(s, `type="file"`) {
				t.Fatalf("%s form paused=%v: disabled=%v notice=%v\n%s", name, paused, disabled, notice, s)
			}
			if paused && strings.Index(s, wantGatewayPaused) > strings.Index(s, `type="file"`) {
				t.Fatalf("%s: the paused notice must come before the file input", name)
			}
		}
	}
	const unknown = "Scanner status unknown — gateway not reporting"

	// No report yet: unknown, paused, and the overview names it beside the System & audit link.
	page := get(e.A, "/admin/system", admin)
	if panel := pageSection(t, page, "image-scanner"); !strings.Contains(panel, unknown) {
		t.Fatalf("no report: panel\n%s", panel)
	}
	if overview := get(e.A, "/admin", admin); !strings.Contains(overview, unknown) {
		t.Fatal("no report: the admin overview does not flag the scanner")
	}
	pausedForms(e.A, true)

	backend, tc := uploadTestBackend(t, e.A.UploadBackendHandler())
	valid := gatewayScannerReport{State: scannerReady, Reachable: true, Engine: "ClamAV 1.4.3", Daily: "27999", DailyDate: time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second), AgeDays: 2, LimitDays: 30, Checked: time.Now().UTC().Truncate(time.Second)}
	body, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("gateway identity required", func(t *testing.T) {
		_, otherClient, _ := uploadTestPKI(t) // a client certificate from another CA
		for name, cfg := range map[string]*tls.Config{"no client certificate": {RootCAs: tc.RootCAs, MinVersion: tls.VersionTLS13}, "other CA": {RootCAs: tc.RootCAs, Certificates: otherClient.Certificates, MinVersion: tls.VersionTLS13}} {
			if code, err := postScannerReport(t, backend.URL, cfg, body); err == nil {
				t.Errorf("%s: accepted with status %d", name, code)
			}
		}
		// The public handler never takes a report, whatever the caller's headers say.
		r := httptest.NewRequest("POST", gatewayScannerReportPath, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Upload-Authenticated", "true")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Errorf("public handler: %d", w.Code)
		}
		// The backend handler without a verified client chain answers 404.
		for _, state := range []*tls.ConnectionState{nil, {}} {
			r = httptest.NewRequest("POST", gatewayScannerReportPath, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.TLS = state
			w = httptest.NewRecorder()
			e.A.UploadBackendHandler().ServeHTTP(w, r)
			if w.Code != 404 {
				t.Errorf("unverified backend request: %d", w.Code)
			}
		}
		if row := scannerReportRow(t, e); row != "" {
			t.Fatalf("a refused report was stored: %s", row)
		}
	})

	t.Run("malformed or oversized report refused", func(t *testing.T) {
		mutate := func(f func(*gatewayScannerReport)) []byte {
			v := valid
			f(&v)
			b, _ := json.Marshal(v)
			return b
		}
		withField := func(k, v string) []byte {
			return append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"`+k+`":`+v+`}`)...)
		}
		cases := map[string][]byte{
			"not json":              []byte("state=Ready"),
			"unknown field":         withField("hostname", `"clamd.internal"`),
			"received from gateway": withField("received", `"2026-01-01T00:00:00Z"`),
			"oversized":             withField("pad", `"`+strings.Repeat("x", gatewayScannerReportLimit)+`"`),
			"two documents":         append(append([]byte(nil), body...), body...),
			"unknown state":         mutate(func(v *gatewayScannerReport) { v.State = "Fine" }),
			"free-text cause": mutate(func(v *gatewayScannerReport) {
				v.State, v.Reachable, v.Cause = scannerUnavailable, false, "dial tcp 10.0.0.5:3310: refused"
			}),
			"engine with address":    mutate(func(v *gatewayScannerReport) { v.Engine = "ClamAV 1.4.3 clamd.internal:3310" }),
			"engine url":             mutate(func(v *gatewayScannerReport) { v.Engine = "http://scanner.example/" }),
			"daily not digits":       mutate(func(v *gatewayScannerReport) { v.Daily = "27999a" }),
			"reachable but down":     mutate(func(v *gatewayScannerReport) { v.State = scannerUnavailable; v.Cause = "timed out" }),
			"limit out of range":     mutate(func(v *gatewayScannerReport) { v.LimitDays = 365 }),
			"negative refusal count": mutate(func(v *gatewayScannerReport) { v.Infected = -1 }),
		}
		for name, b := range cases {
			code, err := postScannerReport(t, backend.URL, tc, b)
			if err != nil || code != http.StatusBadRequest {
				t.Errorf("%s: status %d err %v", name, code, err)
			}
		}
		if row := scannerReportRow(t, e); row != "" {
			t.Fatalf("a malformed report was stored: %s", row)
		}
	})

	// A healthy report from a real gateway sender: forms ready, the panel shows the gateway's state, the dependency
	// table's ClamAV row the reported engine, and nothing names a host, address or URL.
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f) // a gateway with its own mutual-TLS backend listener on e.A
	g.scanner.record(scanInfected, "Opsecmkt.Test.Marker.UNOFFICIAL")
	g.scanner.record(scanTimeout, "")
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatalf("report: %v", err)
	}
	row := scannerReportRow(t, e)
	for _, private := range []string{f.Addr, "127.0.0.1", "Opsecmkt.Test.Marker"} {
		if strings.Contains(row, private) {
			t.Fatalf("stored report holds %q: %s", private, row)
		}
	}
	pausedForms(e.A, false)
	page = get(e.A, "/admin/system", admin)
	panel := pageSection(t, page, "image-scanner")
	for _, want := range []string{"<strong>Ready</strong>", "ClamAV 1.4.3, daily database 27999", "Report received:", "1 refused as infected · 1 refused because the scanner failed"} {
		if !strings.Contains(panel, html.EscapeString(want)) && !strings.Contains(panel, want) {
			t.Fatalf("healthy report: panel lacks %q\n%s", want, panel)
		}
	}
	if strings.Contains(panel, unknown) {
		t.Fatal("healthy report still unknown")
	}
	deps := pageSection(t, page, "dependency-status")
	clam := deps[strings.Index(deps, ">ClamAV<"):]
	clam = clam[:strings.Index(clam, "</tr>")]
	if !strings.Contains(clam, "<td>1.4.3<br>") || !strings.Contains(clam, "Reported by the upload gateway") {
		t.Fatalf("ClamAV dependency row: %s", clam)
	}
	for _, private := range []string{f.Addr, strings.TrimPrefix(g.backend, "https://")} {
		if private != "" && strings.Contains(page, private) {
			t.Fatalf("admin page names %q", private)
		}
	}

	// A second app process on the same database shows the same state.
	second, err := New(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	if !second.isolatedUploads {
		t.Fatal("second app not isolated")
	}
	pausedForms(second, false)
	if p := pageSection(t, get(second, "/admin/system", admin), "image-scanner"); !strings.Contains(p, "<strong>Ready</strong>") {
		t.Fatalf("second app panel:\n%s", p)
	}

	// The report goes stale (no report for longer than the limit): both processes show unknown and pause the forms.
	if _, err := e.DB.Exec(`UPDATE settings SET value=jsonb_set(value::jsonb,'{received}',to_jsonb(clock_timestamp()-$2::interval))::text WHERE key=$1`, settingGatewayScanner, (gatewayScannerStaleAfter + time.Minute).String()); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*App{e.A, second} {
		pausedForms(a, true)
		p := pageSection(t, get(a, "/admin/system", admin), "image-scanner")
		if !strings.Contains(p, unknown) || !strings.Contains(p, "Last report received:") {
			t.Fatalf("stale report: panel\n%s", p)
		}
		if overview := get(a, "/admin", admin); !strings.Contains(overview, unknown) {
			t.Fatal("stale report: overview does not flag the scanner")
		}
	}

	// An unhealthy report (the gateway's signatures are stale) pauses the forms and alerts on the panel.
	f.SetVersion(fakeClamdVersion(40 * 24 * time.Hour))
	g.scanner.poll(context.Background())
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*App{e.A, second} {
		pausedForms(a, true)
		p := pageSection(t, get(a, "/admin/system", admin), "image-scanner")
		if !strings.Contains(p, "<strong>Signatures stale") || !strings.Contains(p, `class="alert error"`) || strings.Contains(p, unknown) {
			t.Fatalf("unhealthy report: panel\n%s", p)
		}
		if overview := get(a, "/admin", admin); !strings.Contains(overview, "Photo uploads are paused by the image scanner.") {
			t.Fatal("unhealthy report: overview does not flag the scanner")
		}
	}

	// A healthy report again resumes both.
	f.SetVersion(fakeClamdVersion(24 * time.Hour))
	g.scanner.poll(context.Background())
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatal(err)
	}
	pausedForms(e.A, false)
	pausedForms(second, false)
}

// The gateway's sender: what it posts, over which identity, and that the gateway sends one at start without
// waiting for the first poll interval.
func TestUploadGatewayScannerReportSender(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	var paths []string
	backend, tc := uploadTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("report sent without the gateway client identity")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, paths = append(got, b), append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	f := newFakeClamd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := NewUploadGateway(ctx, UploadGatewayConfig{AppURL: "http://127.0.0.1:1", BackendURL: backend.URL, TLSConfig: tc, ClamdAddress: f.Addr})
	if err != nil {
		t.Fatal(err)
	}
	g := h.(*uploadGateway)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the gateway sent no report at start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	g.scanner.record(scanInfected, "Some.Signature")
	g.scanner.record(scanUnavailable, "")
	if err := g.reportScanner(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	last, path := got[len(got)-1], paths[len(paths)-1]
	mu.Unlock()
	if path != gatewayScannerReportPath {
		t.Fatalf("path %q", path)
	}
	if len(last) > gatewayScannerReportLimit {
		t.Fatalf("report of %d bytes exceeds the app's limit", len(last))
	}
	for _, private := range []string{f.Addr, "127.0.0.1", "3310", "Some.Signature", backend.URL} {
		if strings.Contains(string(last), private) {
			t.Fatalf("report holds %q: %s", private, last)
		}
	}
	var v gatewayScannerReport
	d := json.NewDecoder(bytes.NewReader(last))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil || !v.valid() {
		t.Fatalf("report the app would refuse: %v %s", err, last)
	}
	if v.State != scannerReady || !v.Reachable || v.Engine != "ClamAV 1.4.3" || v.Daily != "27999" || v.LimitDays != defaultSignatureAgeDays || v.AgeDays != 1 || v.Checked.IsZero() || v.Infected != 1 || v.Failed != 1 {
		t.Fatalf("report: %+v", v)
	}

	// An engine string the app would refuse is left out, not sent.
	f.SetVersion("ClamAV 1.4.3 build host.example/27999/" + time.Now().UTC().Add(-time.Hour).Format(time.ANSIC))
	g.scanner.poll(context.Background())
	r := newGatewayScannerReport(g.scanner.State(), g.scanner.counts())
	if r.Engine != "" || !r.valid() {
		t.Fatalf("unsafe engine kept: %+v", r)
	}

	// A refused report is an error for the sender, so it can log the class.
	refusing, rtc := uploadTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 400) }))
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2() // no background reports
	h2, err := NewUploadGateway(ctx2, UploadGatewayConfig{AppURL: "http://127.0.0.1:1", BackendURL: refusing.URL, TLSConfig: rtc, ClamdAddress: f.Addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.(*uploadGateway).reportScanner(context.Background()); err == nil {
		t.Fatal("refused report treated as delivered")
	}
}

// The integrated (single-host) mode keeps its direct clamd checks and ignores any stored gateway report.
func TestIntegratedModeIgnoresGatewayScannerReport(t *testing.T) {
	e, f := uploadEnv(t)
	_ = f
	if e.A.isolatedUploads {
		t.Fatal("integrated test app is isolated")
	}
	stale := `{"state":"Unavailable","reachable":false,"cause":"timed out","limit_days":30,"checked":"2026-01-01T00:00:00Z","received":"2026-01-01T00:00:00Z"}`
	if _, err := e.DB.Exec(`INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, settingGatewayScanner, stale); err != nil {
		t.Fatal(err)
	}
	st, err := e.A.uploadScannerState(context.Background())
	if err != nil || st.Name != scannerReady {
		t.Fatalf("integrated state %+v %v", st, err)
	}
	_, admin := e.user("integrated_report_admin", "admin")
	w := e.do("GET", "/admin/system", admin, nil)
	e.check(w, 200)
	if p := pageSection(t, w.Body.String(), "image-scanner"); strings.Contains(p, "gateway not reporting") || !strings.Contains(p, "<strong>Ready</strong>") {
		t.Fatalf("integrated panel:\n%s", p)
	}
}
