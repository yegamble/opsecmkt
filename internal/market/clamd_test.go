package market

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-185: the clamd client and its fail-closed behaviour. Only clamd's exact "stream: OK" for the bytes received
// yields a scannedUpload; everything else refuses with a class.

// readyScanner is a scanner pointed at f that has polled it once.
func readyScanner(t *testing.T, f *fakeClamd) *imageScanner {
	t.Helper()
	s := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: defaultSignatureAgeDays})
	s.poll(context.Background())
	if st := s.State(); st.Name != scannerReady {
		t.Fatalf("state after poll = %+v, want Ready", st)
	}
	return s
}

// shortScanTimeout bounds a scan to d for the rest of the test.
func shortScanTimeout(t *testing.T, d time.Duration) {
	old := clamdTimeout
	clamdTimeout = d
	t.Cleanup(func() { clamdTimeout = old })
}

func refusalOf(t *testing.T, err error) *scanRefusal {
	t.Helper()
	var r *scanRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want *scanRefusal", err)
	}
	return r
}

func TestClamdOnlyExactOK(t *testing.T) {
	shortScanTimeout(t, 500*time.Millisecond)
	data := bytes.Repeat([]byte("0123456789abcdef"), 200<<10/16) // 200 KiB: four chunks, the last short
	for _, c := range []struct {
		name, reply string
		class       scanClass
	}{
		{"ok", "stream: OK", ""},
		{"found", "stream: Heuristics.Limits.Exceeded.MaxScanSize FOUND", scanInfected},
		{"size limit", "INSTREAM size limit exceeded. ERROR", scanError},
		{"ok with suffix", "stream: OKAY", scanError},
		{"ok with space", "stream: OK ", scanError},
		{"bare ok", "OK", scanError},
		{"lower case", "stream: ok", scanError},
		{"found without name", "stream:  FOUND", scanError},
		{"error reply", "stream: Can't allocate memory ERROR", scanError},
		{"oversized reply", "stream: " + strings.Repeat("x", 2000), scanError},
		{"garbage", "\xff\xfe binary", scanError},
		{"hang", fakeClamdHang, scanTimeout},
		{"close", fakeClamdClose, scanUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeClamd(t)
			f.SetVerdict(func([]byte) string { return c.reply })
			s := readyScanner(t, f)
			got, err := s.Scan(context.Background(), uploadListing, data)
			checkRefusal(t, c.class, got, err, data)
			if scans := f.Scans(); len(scans) != 1 || !bytes.Equal(scans[0], data) {
				t.Fatalf("clamd received %d streams, want the upload's bytes once, unmodified", len(scans))
			}
			if n := f.MaxChunk(); n != 64<<10 {
				t.Fatalf("largest chunk = %d, want 65536", n)
			}
		})
	}
	// A server that answers before it has read the whole stream and closes the connection with the rest unread:
	// real clamd does this with its size error when a stream exceeds StreamMaxLength. An early "stream: OK" is
	// refused whenever the client can see the rest went unread: a failed write, or a reset in place of a clean
	// close (the fake forces a reset, as a close with unread data does). Either way the upload is refused as that
	// request's error, not as a scanner outage: the scanner stays Ready.
	for _, c := range []struct {
		name, reply string
		size        int
	}{
		{"early ok, 8 MiB", "stream: OK", 8 << 20},
		{"early ok, 200 KiB", "stream: OK", 200 << 10},
		{"early size error, 8 MiB", "INSTREAM size limit exceeded. ERROR", 8 << 20},
		{"early size error, 200 KiB", "INSTREAM size limit exceeded. ERROR", 200 << 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeClamd(t)
			f.SetEarlyReply(64<<10, c.reply)
			s := readyScanner(t, f)
			for range 5 {
				data := bytes.Repeat([]byte{0xa5}, c.size)
				got, err := s.Scan(context.Background(), uploadListing, data)
				checkRefusal(t, scanError, got, err, data)
				if st := s.State(); st.Name != scannerReady {
					t.Fatalf("state after the server cut the stream short = %+v", st)
				}
			}
		})
	}
}

// checkRefusal checks a scan's outcome: clean (class "") returns exactly data; otherwise a refusal of class and
// no bytes.
func checkRefusal(t *testing.T, class scanClass, got scannedUpload, err error, data []byte) {
	t.Helper()
	if class == "" {
		if err != nil || !bytes.Equal(got.Bytes(), data) {
			t.Fatalf("clean scan: err=%v, %d bytes returned", err, len(got.Bytes()))
		}
		return
	}
	if r := refusalOf(t, err); r.Class != class {
		t.Fatalf("class = %q, want %q", r.Class, class)
	}
	if got.Bytes() != nil {
		t.Fatalf("a refused scan returned %d bytes", len(got.Bytes()))
	}
}

func TestClamdTimeoutRefuses(t *testing.T) {
	shortScanTimeout(t, 300*time.Millisecond)
	f := newFakeClamd(t)
	s := readyScanner(t, f)
	f.SetVerdict(func([]byte) string { return fakeClamdHang })
	start := time.Now()
	_, err := s.Scan(context.Background(), uploadListing, []byte("photo"))
	if r := refusalOf(t, err); r.Class != scanTimeout || r.Status() != http.StatusServiceUnavailable || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(r.Ref) {
		t.Fatalf("refusal = %+v status %d", r, r.Status())
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hung scan took %s, want about the 300ms bound", d)
	}
	if msg := refusalOf(t, err).Message(askOperator); !strings.HasPrefix(msg, "Photo not saved: the malware scanner did not respond, so nothing was stored. Try again later. Reference: ") {
		t.Fatalf("message = %q", msg)
	}
	// A failed scan marks the scanner unavailable until the next successful poll, and later scans are refused
	// without contacting clamd.
	if st := s.State(); st.Name != scannerUnavailable {
		t.Fatalf("state after a timed-out scan = %+v", st)
	}
	before := len(f.Commands())
	if _, err = s.Scan(context.Background(), uploadListing, []byte("photo")); refusalOf(t, err).Class != scanUnavailable {
		t.Fatalf("scan while unavailable: %v", err)
	}
	if len(f.Commands()) != before {
		t.Fatal("a scan while unavailable contacted clamd")
	}
	f.SetVerdict(func([]byte) string { return "stream: OK" })
	s.poll(context.Background())
	if _, err = s.Scan(context.Background(), uploadListing, []byte("photo")); err != nil {
		t.Fatalf("scan after recovery: %v", err)
	}

	// The request's own context ends the scan too (a client that went away).
	f.SetVerdict(func([]byte) string { return fakeClamdHang })
	shortScanTimeout(t, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	failedBefore := s.counts().Failed
	start = time.Now()
	if _, err = s.Scan(ctx, uploadListing, []byte("photo")); refusalOf(t, err).Class != scanTimeout {
		t.Fatalf("cancelled scan: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancelled scan took %s", d)
	}
	// ... but clamd was not at fault: uploads stay available and no scanner failure is counted.
	if st, c := s.State(), s.counts(); st.Name != scannerReady || c.Failed != failedBefore {
		t.Fatalf("after a request that ended first: state %+v, %d failures counted, want %d", st, c.Failed, failedBefore)
	}

	// clamd refusing a stream it finds too long, before the client finished sending, is an error reply: the
	// upload is refused and the scanner stays Ready.
	f.SetVerdict(func([]byte) string { return "stream: OK" })
	f.SetStreamLimit(128 << 10)
	if _, err = s.Scan(context.Background(), uploadListing, make([]byte, 4<<20)); refusalOf(t, err).Class != scanError {
		t.Fatalf("stream over clamd's limit: %v", err)
	}
	if st := s.State(); st.Name != scannerReady {
		t.Fatalf("state after a size error = %+v", st)
	}

	// A poll cut short by shutdown records nothing.
	stopped, cancelPoll := context.WithCancel(context.Background())
	cancelPoll()
	fresh := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: 30})
	fresh.poll(stopped)
	if st := fresh.State(); st.Cause != "not checked yet" {
		t.Fatalf("poll during shutdown changed the state: %+v", st)
	}

	// Nothing listening: refused as unavailable, within the dial bound.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	down := newImageScanner(scannerConfig{address: addr, maxAgeDays: 30})
	down.poll(context.Background())
	if st := down.State(); st.Name != scannerUnavailable || st.Cause != "connection failed" {
		t.Fatalf("state with nothing listening = %+v", st)
	}
}

func TestClamdStaleRefuses(t *testing.T) {
	f := newFakeClamd(t)
	f.SetVersion(fakeClamdVersion(31*24*time.Hour - time.Hour))
	s := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: 30})
	s.poll(context.Background())
	st := s.State()
	if st.Name != scannerStale || st.AgeDays != 31 || st.LimitDays != 30 || st.Engine != "ClamAV 1.4.3" || st.Daily != "27999" {
		t.Fatalf("state = %+v", st)
	}
	if want := "Photo uploads are paused: the malware scanner's signatures are older than 30 days. Listing details can still be saved."; st.VendorNotice() != want {
		t.Fatalf("vendor notice = %q", st.VendorNotice())
	}
	_, err := s.Scan(context.Background(), uploadListing, []byte("photo"))
	r := refusalOf(t, err)
	if r.Class != scanStale || r.Status() != http.StatusServiceUnavailable || r.Ref != "" || r.Message(askOperator) != st.VendorNotice() {
		t.Fatalf("stale refusal = %+v %q", r, r.Message(askOperator))
	}
	if len(f.Scans()) != 0 {
		t.Fatal("stale signatures: the upload was sent to clamd")
	}
	f.SetVersion(fakeClamdVersion(29 * 24 * time.Hour))
	s.poll(context.Background())
	if _, err = s.Scan(context.Background(), uploadListing, []byte("photo")); err != nil {
		t.Fatalf("29 days old: %v", err)
	}
	// The limit is exact, as the copy "older than N days" says: an hour under 7 days passes, an hour over does not.
	f.SetVersion(fakeClamdVersion(7*24*time.Hour - time.Hour))
	s7 := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: 7})
	s7.poll(context.Background())
	if st := s7.State(); st.Name != scannerReady || st.AgeDays != 6 {
		t.Fatalf("7 days less an hour at limit 7 = %+v", st)
	}
	f.SetVersion(fakeClamdVersion(7*24*time.Hour + time.Hour))
	s7.poll(context.Background())
	if st := s7.State(); st.Name != scannerStale || st.AgeDays != 8 || describeState(st) != "Signatures stale (8 days, limit 7)" ||
		!strings.Contains(st.VendorNotice(), "older than 7 days") {
		t.Fatalf("7 days and an hour at limit 7 = %+v", st)
	}
}

func TestClamdVersionUnparsableIsStale(t *testing.T) {
	f := newFakeClamd(t)
	for _, v := range []string{"ClamAV 1.4.3", "ClamAV 1.4.3/27999", "ClamAV 1.4.3/27999/not a date", "garbage", "", "ClamAV 1.4.3/27999/Thu Sep 25 07:57:02 2026 extra"} {
		f.SetVersion(v)
		s := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: 30})
		s.poll(context.Background())
		st := s.State()
		if st.Name != scannerStale || st.Cause != "signature date unreadable" || !st.DailyDate.IsZero() {
			t.Fatalf("VERSION %q: state = %+v", v, st)
		}
		if _, err := s.Scan(context.Background(), uploadListing, []byte("photo")); refusalOf(t, err).Class != scanStale {
			t.Fatalf("VERSION %q: scan %v", v, err)
		}
	}
	f.SetVersion("ClamAV 1.4.3")
	s := newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: 30})
	s.poll(context.Background())
	if st := s.State(); st.Engine != "ClamAV 1.4.3" {
		t.Fatalf("engine of a VERSION without signatures = %q", st.Engine)
	}
	// clamd pads a one-digit day with a space (ANSI C asctime).
	f.SetVersion("ClamAV 1.4.3/27999/" + time.Now().UTC().AddDate(0, 0, -2).Format("Mon Jan _2 15:04:05 2006"))
	s.poll(context.Background())
	if st := s.State(); st.Name != scannerReady || st.AgeDays != 2 {
		t.Fatalf("padded date: %+v", st)
	}
}

func TestSignatureAgeRange(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := scannerConfigFromEnv(env(nil))
	if err != nil || c.address != "" || c.maxAgeDays != 30 {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	for _, ok := range []string{"7", "30", "90", " 45 "} {
		if c, err = scannerConfigFromEnv(env(map[string]string{"SCANNER_MAX_SIGNATURE_AGE_DAYS": ok})); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"3", "6", "91", "0", "-30", "abc", "30d", "30.5", "1e2"} {
		_, err := scannerConfigFromEnv(env(map[string]string{"SCANNER_MAX_SIGNATURE_AGE_DAYS": bad}))
		if err == nil || !strings.Contains(err.Error(), "SCANNER_MAX_SIGNATURE_AGE_DAYS must be a whole number of days from 7 to 90") {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"clamd:3310", "127.0.0.1:3310", "[::1]:3310", " clamd:3310 "} {
		if c, err = scannerConfigFromEnv(env(map[string]string{"CLAMD_ADDRESS": ok})); err != nil || c.address != strings.TrimSpace(ok) {
			t.Fatalf("CLAMD_ADDRESS=%q: %+v %v", ok, c, err)
		}
	}
	for _, bad := range []string{"clamd", "clamd:", ":3310", "clamd:abc", "clamd:0", "clamd:70000", "tcp://clamd:3310", "/run/clamav/clamd.sock"} {
		_, err := scannerConfigFromEnv(env(map[string]string{"CLAMD_ADDRESS": bad}))
		if err == nil || err.Error() != errClamdAddress {
			t.Fatalf("CLAMD_ADDRESS=%q: err = %v", bad, err)
		}
	}
	// Startup refuses it before any database connection (none is reachable here).
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("SCANNER_MAX_SIGNATURE_AGE_DAYS", "3")
	if a, err := New(context.Background(), false); err == nil || !strings.Contains(err.Error(), "from 7 to 90") {
		if a != nil {
			a.Close()
		}
		t.Fatalf("New with SCANNER_MAX_SIGNATURE_AGE_DAYS=3: %v", err)
	}
}

// Without CLAMD_ADDRESS the scanner is Disabled: every upload is refused with the "off" copy and a 503, and
// nothing is dialled. (The upload routes and forms that show this copy arrive with A-186 and A-188.)
func TestClamdUnsetDisablesUploads(t *testing.T) {
	for _, s := range []*imageScanner{nil, newImageScanner(scannerConfig{maxAgeDays: 30})} {
		s.poll(context.Background()) // a no-op
		st := s.State()
		const off = "Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up."
		if st.Name != scannerDisabled || st.VendorNotice() != off {
			t.Fatalf("state = %+v notice %q", st, st.VendorNotice())
		}
		_, err := s.Scan(context.Background(), uploadAvatar, []byte("photo"))
		if r := refusalOf(t, err); r.Class != scanDisabled || r.Status() != http.StatusServiceUnavailable || r.Message(askOperator) != off || r.Ref != "" {
			t.Fatalf("refusal = %+v", r)
		}
		s.RecordStored() // nil-safe
	}
	st := newImageScanner(scannerConfig{address: "127.0.0.1:1", maxAgeDays: 30}).State()
	if st.Name != scannerUnavailable || st.Cause != "not checked yet" || st.VendorNotice() != "Photo uploads are paused: the malware scanner is not responding. Listing details can still be saved." {
		t.Fatalf("configured but never polled = %+v", st)
	}
}

// Only clamd.go constructs a scannedUpload or touches its bytes; everything else gets one from Scan and reads it
// with Bytes. Test files included: a test needing a clean upload scans through fakeClamd.
func TestOnlyScannerBuildsScannedUpload(t *testing.T) {
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) < 50 {
		t.Fatalf("package files: %d, %v", len(files), err)
	}
	constructions := 0
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		inScanner := filepath.Base(name) == "clamd.go"
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "scannedUpload" {
					if !inScanner {
						t.Errorf("%s constructs a scannedUpload", name)
					} else if len(n.Elts) > 0 { // clamd.go's empty literals are the refusals' zero value
						constructions++
					}
				}
			case *ast.CallExpr:
				// new(scannedUpload) or a conversion scannedUpload(x)
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "scannedUpload" && !inScanner {
					t.Errorf("%s converts to scannedUpload", name)
				}
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && len(n.Args) == 1 {
					if a, ok := n.Args[0].(*ast.Ident); ok && a.Name == "scannedUpload" && !inScanner {
						t.Errorf("%s allocates a scannedUpload", name)
					}
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "cleanBytes" && !inScanner {
					t.Errorf("%s touches scannedUpload.cleanBytes; use Bytes()", name)
				}
			}
			return true
		})
	}
	if constructions != 1 {
		t.Fatalf("clamd.go constructs scannedUpload %d times, want once (the exact stream: OK branch)", constructions)
	}
}

// The upload log line carries the class, the signature name (infected only), the reference and the kind of
// upload, and nothing else: no handle, user id, listing id, filename or size.
func TestUploadLogLineHasNoUserData(t *testing.T) {
	shortScanTimeout(t, 300*time.Millisecond)
	logs := captureLog(t)
	f := newFakeClamd(t)
	s := readyScanner(t, f)
	f.SetVerdict(func([]byte) string { return "stream: Win.Test.Distinct Sig\x01-9 FOUND" })
	_, err := s.Scan(context.Background(), uploadListing, []byte("secret-listing-bytes"))
	infected := refusalOf(t, err)
	if infected.Class != scanInfected || infected.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("refusal = %+v", infected)
	}
	want := "Photo refused: the malware scanner flagged this file. Nothing was stored. If you think this is a mistake, send the market staff this reference. Ask the operator of this market. Reference: " + infected.Ref
	if infected.Message(askOperator) != want || strings.Contains(infected.Message(askOperator), "Win.Test") || strings.Contains(infected.Error(), "Win.Test") {
		t.Fatalf("uploader copy = %q / error %q", infected.Message(askOperator), infected.Error())
	}
	f.SetVerdict(func([]byte) string { return fakeClamdHang })
	_, err = s.Scan(context.Background(), uploadAvatar, []byte("secret-avatar-bytes"))
	timeout := refusalOf(t, err)
	lines := logs.lines("upload ")
	if len(lines) != 2 {
		t.Fatalf("upload log lines = %q", lines)
	}
	if lines[0] != "upload outcome=refused reason=infected sig=Win.Test.Distinct_Sig_-9 ref="+infected.Ref+" kind=listing" {
		t.Fatalf("infected line = %q", lines[0])
	}
	if lines[1] != "upload outcome=refused reason=timeout ref="+timeout.Ref+" kind=avatar" {
		t.Fatalf("timeout line = %q", lines[1])
	}
	if infected.Ref == timeout.Ref {
		t.Fatal("references repeat")
	}
	if strings.Contains(logs.String(), "secret-") {
		t.Fatal("the log quotes upload bytes")
	}
}

// A refused upload leaves one audit row with fixed copy on the uploader: no signature name and no reference.
func TestRefusedUploadAuditHasNoSignatureName(t *testing.T) {
	e := newTestApp(t)
	uid, _ := e.user("scan_vendor", "vendor")
	f := newFakeClamd(t)
	e.withScanner(f)
	f.SetVerdict(func([]byte) string { return "stream: Win.Test.Distinct-Sig-9 FOUND" })
	ctx := context.Background()
	_, err := e.A.scanner.Scan(ctx, uploadListing, []byte("photo"))
	r := refusalOf(t, err)
	if err = e.A.auditScanRefusal(ctx, uid, err); err != nil {
		t.Fatal(err)
	}
	// A refusal because the scanner failed records nothing: nothing happened to the account.
	f.SetVerdict(func([]byte) string { return fakeClamdClose })
	_, err = e.A.scanner.Scan(ctx, uploadListing, []byte("photo"))
	if err = e.A.auditScanRefusal(ctx, uid, err); err != nil {
		t.Fatal(err)
	}
	var rows []string
	res, err := e.DB.Query("SELECT action FROM audit_events WHERE user_id=$1", uid)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		var s string
		res.Scan(&s)
		rows = append(rows, s)
	}
	res.Close()
	if len(rows) != 1 || rows[0] != "Upload refused by the malware scanner" {
		t.Fatalf("audit rows = %q", rows)
	}
	var leaks int
	if err = e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE action LIKE '%Win.Test%' OR action LIKE '%'||$1||'%'", r.Ref).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("audit rows naming the signature or reference: %d %v", leaks, err)
	}
}

// /healthz never contacts clamd, and answers at once while clamd hangs.
func TestHealthzIgnoresScanner(t *testing.T) {
	f := newFakeClamd(t)
	f.SetHang(true)
	t.Setenv("CLAMD_ADDRESS", f.Addr)
	e := newTestApp(t)
	start := time.Now()
	w := e.do("GET", "/healthz", "", nil)
	if w.Code != 200 || w.Body.String() != "ok" || time.Since(start) > time.Second {
		t.Fatalf("healthz = %d %q after %s", w.Code, w.Body.String(), time.Since(start))
	}
	if cmds := f.Commands(); len(cmds) != 0 {
		t.Fatalf("healthz contacted clamd: %q", cmds)
	}
}

// With the background poller stuck on a clamd that never answers, the market still serves pages, /healthz and
// the admin page (which reads only the cached state and says Unavailable).
func TestMarketHealthyWhileScannerDown(t *testing.T) {
	shortScanTimeout(t, 400*time.Millisecond)
	f := newFakeClamd(t)
	f.SetHang(true)
	t.Setenv("CLAMD_ADDRESS", f.Addr)
	e := newTestApp(t)
	_, admin := e.user("scan_admin", "admin")
	e.A.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for len(f.Commands()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.Commands()) == 0 {
		t.Fatal("the poller never contacted clamd")
	}
	for _, path := range []string{"/healthz", "/"} {
		start := time.Now()
		if w := e.do("GET", path, "", nil); w.Code != 200 || time.Since(start) > time.Second {
			t.Fatalf("%s = %d after %s while clamd hangs", path, w.Code, time.Since(start))
		}
	}
	for time.Now().Before(deadline) && e.A.scanner.State().Cause == "not checked yet" {
		time.Sleep(20 * time.Millisecond)
	}
	before := len(f.Commands())
	w := e.do("GET", "/admin/system", admin, nil)
	e.check(w, 200)
	if body := w.Body.String(); !strings.Contains(body, "Unavailable (timed out)") || !strings.Contains(body, "Signatures: not read yet.") || !strings.Contains(body, "docker compose logs clamd") {
		t.Fatalf("admin page does not show the scanner unavailable:\n%s", scannerPanel(w.Body.String()))
	}
	if len(f.Commands()) != before {
		t.Fatal("rendering /admin contacted clamd")
	}
}

// scannerPanel cuts the Image scanner section out of an admin page, for failure messages and assertions.
func scannerPanel(page string) string {
	i := strings.Index(page, `id="image-scanner"`)
	if i < 0 {
		return "(no image scanner panel)"
	}
	j := strings.Index(page[i:], "</section>")
	if j < 0 {
		return page[i:]
	}
	return page[i : i+j]
}

func TestAdminScannerPanel(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("panel_admin", "admin")
	_, vendor := e.user("panel_vendor", "vendor")
	// Disabled (no CLAMD_ADDRESS in the test environment).
	w := e.do("GET", "/admin/system", admin, nil)
	e.check(w, 200)
	panel := scannerPanel(w.Body.String())
	if !strings.Contains(panel, "Disabled") || !strings.Contains(panel, "CLAMD_ADDRESS") {
		t.Fatalf("disabled panel:\n%s", panel)
	}
	if w = e.do("GET", "/admin/system", vendor, nil); w.Code != 403 {
		t.Fatalf("vendor /admin = %d", w.Code)
	}

	f := newFakeClamd(t)
	f.SetVersion(fakeClamdVersion(31*24*time.Hour - time.Hour))
	e.withScanner(f)
	st := e.A.scanner.State()
	panel = scannerPanel(e.do("GET", "/admin/system", admin, nil).Body.String())
	for _, want := range []string{"Signatures stale (31 days, limit 30)", "ClamAV 1.4.3", "27999", st.DailyDate.Format("2006-01-02 15:04 UTC"), "install.sh refresh-scanner", "No successful scan since the app started"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("stale panel lacks %q:\n%s", want, panel)
		}
	}

	// Ready, past half the limit: a warning with the refresh hint. Counts by signature, never by account.
	f.SetVersion(fakeClamdVersion(16*24*time.Hour + time.Hour))
	e.A.scanner.poll(context.Background())
	ctx := context.Background()
	for _, v := range []string{"stream: OK", "stream: Win.Test.Panel-Sig-1 FOUND", "stream: Win.Test.Panel-Sig-1 FOUND", "stream: Other.Panel-Sig FOUND"} {
		f.SetVerdict(func([]byte) string { return v })
		e.A.scanner.Scan(ctx, uploadListing, []byte("photo"))
	}
	e.A.scanner.RecordStored()
	f.SetVerdict(func([]byte) string { return "INSTREAM size limit exceeded. ERROR" })
	e.A.scanner.Scan(ctx, uploadListing, []byte("photo"))
	e.A.scanner.poll(ctx) // back to Ready after the failed scan
	before := len(f.Commands())
	panel = scannerPanel(e.do("GET", "/admin/system", admin, nil).Body.String())
	if len(f.Commands()) != before {
		t.Fatal("rendering /admin contacted clamd")
	}
	for _, want := range []string{">Ready<", "16 days old", "limit 30", "install.sh refresh-scanner", "1 stored", "3 refused as infected", "1 refused because the scanner failed",
		"<td>Win.Test.Panel-Sig-1</td>\n<td>2</td>", "<td>Other.Panel-Sig</td>\n<td>1</td>"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("ready panel lacks %q:\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "panel_vendor") || strings.Contains(panel, "panel_admin") || strings.Contains(panel, "Signatures stale") {
		t.Fatalf("ready panel:\n%s", panel)
	}
	// Fresh signatures: no warning.
	f.SetVersion(fakeClamdVersion(2 * 24 * time.Hour))
	e.A.scanner.poll(ctx)
	if panel = scannerPanel(e.do("GET", "/admin/system", admin, nil).Body.String()); strings.Contains(panel, "refresh-scanner") || !strings.Contains(panel, "2 days old") {
		t.Fatalf("fresh panel:\n%s", panel)
	}
}

// The 24-hour counts drop events older than a day and keep a bounded number of signature names.
func TestScanCountsWindow(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 30, 0, 0, time.UTC)
	s := newImageScanner(scannerConfig{address: "127.0.0.1:1", maxAgeDays: 30})
	s.now = func() time.Time { return now }
	s.record(scanInfected, "Old.Sig")
	s.RecordStored()
	now = now.Add(2 * time.Hour)
	for i := range scanSignatureNames + 5 {
		s.record(scanInfected, "Sig."+strings.Repeat("x", i))
	}
	s.record(scanTimeout, "")
	c := s.counts()
	if c.Stored != 1 || c.Infected != scanSignatureNames+6 || c.Failed != 1 {
		t.Fatalf("counts = %+v", c)
	}
	now = now.Add(23 * time.Hour) // the first hour is now more than a day old
	c = s.counts()
	if c.Stored != 0 || c.Infected != scanSignatureNames+5 || c.Failed != 1 {
		t.Fatalf("counts a day later = %+v", c)
	}
	named, other := 0, 0
	for _, sig := range c.Signatures {
		if sig.Name == "Old.Sig" {
			t.Fatal("expired signature still counted")
		}
		if sig.Name == otherSignatures {
			other = sig.Count
		} else {
			named++
		}
	}
	if named != scanSignatureNames || other != 5 {
		t.Fatalf("named %d, other %d", named, other)
	}
}

// There is no switch that lets an upload skip the scan. The extra upload
// settings only select the isolated service and authenticate its transport.
func TestNoScannerSkipSwitch(t *testing.T) {
	// BRANDING_UPLOAD_ORIGIN selects the file-handoff destination; it cannot bypass scanning.
	allowed := map[string]bool{"CLAMD_ADDRESS": true, "SCANNER_MAX_SIGNATURE_AGE_DAYS": true, "UPLOAD_MODE": true, "UPLOAD_BACKEND_ADDR": true, "UPLOAD_CA": true, "UPLOAD_CERT": true, "UPLOAD_KEY": true, "BRANDING_UPLOAD_ORIGIN": true}
	files, _ := filepath.Glob("internal/market/*.go")
	main, _ := filepath.Glob("cmd/server/*.go")
	getenv := regexp.MustCompile(`Getenv\("([A-Z0-9_]+)"\)|getenv\("([A-Z0-9_]+)"\)`)
	for _, name := range append(files, main...) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(name)
		for _, m := range getenv.FindAllStringSubmatch(string(src), -1) {
			v := m[1] + m[2]
			if (strings.Contains(v, "SCAN") || strings.Contains(v, "CLAM") || strings.Contains(v, "UPLOAD")) && !allowed[v] {
				t.Errorf("%s reads %s", name, v)
			}
		}
	}
}

// TestRealClamd runs the client against a real clamd (opt-in: CLAMD_TEST_ADDRESS=host:port), for example
//
//	docker run -d --rm -p 127.0.0.1:3310:3310 -e CLAMAV_NO_FRESHCLAMD=true clamav/clamav-debian:stable
//
// once clamd has loaded its signatures. It proves the real VERSION reply parses; that a clean stream passes,
// which also proves clamd closes the connection cleanly after answering (the client requires that before it
// accepts "stream: OK"); and that the EICAR test file is FOUND. Fake-clamd tests do not prove these.
func TestRealClamd(t *testing.T) {
	addr := os.Getenv("CLAMD_TEST_ADDRESS")
	if addr == "" {
		t.Skip("set CLAMD_TEST_ADDRESS=host:port of a running clamd to check the client against it")
	}
	ctx := context.Background()
	s := newImageScanner(scannerConfig{address: addr, maxAgeDays: 90})
	s.poll(ctx)
	st := s.State()
	if st.Name != scannerReady || !strings.HasPrefix(st.Engine, "ClamAV ") || st.Daily == "" || st.DailyDate.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	t.Logf("%s daily %s built %s (%d days old)", st.Engine, st.Daily, st.DailyDate.Format(time.RFC3339), st.AgeDays)
	clean := bytes.Repeat([]byte("an ordinary upload with nothing in it\n"), 8000) // about 300 KiB: several chunks
	for range 3 {
		got, err := s.Scan(ctx, uploadListing, clean)
		if err != nil || !bytes.Equal(got.Bytes(), clean) {
			t.Fatalf("clean stream: %v", err)
		}
	}
	// The EICAR anti-virus test string, assembled at run time so this source file is not itself detected.
	eicar := []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$` + `EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
	_, err := s.Scan(ctx, uploadListing, eicar)
	if r := refusalOf(t, err); r.Class != scanInfected || !strings.Contains(strings.ToLower(r.sig), "eicar") {
		t.Fatalf("EICAR: %+v", r)
	}
	if st := s.State(); st.Name != scannerReady {
		t.Fatalf("state after scans = %+v", st)
	}
	// CLAMD_TEST_OVERRUN_BYTES, above that clamd's StreamMaxLength: clamd answers with its size error and closes
	// with the rest unread. The upload is refused as an error, the scanner stays Ready, and a clean scan follows.
	if n, _ := strconv.Atoi(os.Getenv("CLAMD_TEST_OVERRUN_BYTES")); n > 0 {
		for range 5 {
			_, err := s.Scan(ctx, uploadListing, bytes.Repeat([]byte{0xa5}, n))
			if r := refusalOf(t, err); r.Class != scanError {
				t.Fatalf("overrun of %d bytes: %+v", n, r)
			}
		}
		if _, err := s.Scan(ctx, uploadListing, clean); err != nil || s.State().Name != scannerReady {
			t.Fatalf("after overruns: %v, %+v", err, s.State())
		}
	}
}
