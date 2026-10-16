package market

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A-185 (war room 2026-09-27, rulings C2 and C4): the ClamAV client that every photo and avatar upload passes
// through. The invariant is "nothing is stored or served without a clean stream: OK from clamd on the bytes
// received": only clamd's exact reply "stream: OK" to a zINSTREAM of the upload's bytes produces a
// scannedUpload, the one type the upload pipeline (A-186) decodes, and only this file constructs it
// (TestOnlyScannerBuildsScannedUpload). Everything else fails closed with a class: infected (a signature
// matched), unavailable, timeout or error (clamd did not answer, or answered anything but OK or FOUND),
// stale (signatures older than SCANNER_MAX_SIGNATURE_AGE_DAYS) or disabled (no CLAMD_ADDRESS). There is no
// switch that skips the scan (TestNoScannerSkipSwitch).
//
// A background poller asks clamd for PING and VERSION every 30 s and caches the result; pages (the admin
// panel, and the upload forms of A-188) read only that cache and never contact clamd, and /healthz ignores it.
//
// A matched signature name goes in exactly one log line, beside an 8-hex reference the uploader is shown,
// and into the admin page's 24-hour counts by signature; it is never shown to the uploader, never linked to an
// account and never written to audit_events, whose row is fixed copy (auditScanRefusal):
//
//	upload outcome=refused reason=infected sig=<name> ref=<8 hex> kind=listing|avatar
//	upload outcome=refused reason=unavailable|timeout|error ref=<8 hex> kind=listing|avatar

const (
	defaultSignatureAgeDays = 30
	clamdChunk              = 64 << 10 // zINSTREAM chunk size
	clamdReplyLimit         = 1024     // longest reply accepted, before its NUL
	scannerPollInterval     = 30 * time.Second
	scanSignatureNames      = 32 // distinct signature names counted per hour; further ones are counted together
	otherSignatures         = "(other signatures)"
)

// clamdDialTimeout bounds connecting to clamd; clamdTimeout bounds a whole exchange, connection included.
// Tests shorten clamdTimeout.
var (
	clamdDialTimeout = 3 * time.Second
	clamdTimeout     = 10 * time.Second
)

// scannerConfig is CLAMD_ADDRESS (host:port; empty = uploads off) and SCANNER_MAX_SIGNATURE_AGE_DAYS.
type scannerConfig struct {
	address    string
	maxAgeDays int
}

const errClamdAddress = "CLAMD_ADDRESS must be host:port of the clamd service on the scanner network (the installer writes clamd:3310), or empty to turn photo uploads off"

// scannerConfigFromEnv reads and checks the scanner settings; startup refuses a bad value (New).
func scannerConfigFromEnv(getenv func(string) string) (scannerConfig, error) {
	c := scannerConfig{address: strings.TrimSpace(getenv("CLAMD_ADDRESS")), maxAgeDays: defaultSignatureAgeDays}
	if c.address != "" {
		host, port, err := net.SplitHostPort(c.address)
		n, perr := strconv.Atoi(port)
		if err != nil || perr != nil || host == "" || strings.Contains(host, "/") || n < 1 || n > 65535 {
			return c, errors.New(errClamdAddress)
		}
	}
	if s := strings.TrimSpace(getenv("SCANNER_MAX_SIGNATURE_AGE_DAYS")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 7 || n > 90 {
			return c, errors.New("SCANNER_MAX_SIGNATURE_AGE_DAYS must be a whole number of days from 7 to 90 (default 30); photo uploads pause while the malware scanner's signatures are older than this")
		}
		c.maxAgeDays = n
	}
	return c, nil
}

// uploadKind names what an upload is for in the log line; it is never user data.
type uploadKind string

const (
	uploadListing  uploadKind = "listing"
	uploadAvatar   uploadKind = "avatar"
	uploadBranding uploadKind = "branding"
)

// scannedUpload holds bytes clamd answered exactly "stream: OK" for. Only Scan builds one; read the bytes
// with Bytes and do not modify them.
type scannedUpload struct{ cleanBytes []byte }

// Bytes returns the scanned bytes: exactly those sent to clamd.
func (u scannedUpload) Bytes() []byte { return u.cleanBytes }

// scanClass says why an upload was not scanned clean.
type scanClass string

const (
	scanInfected    scanClass = "infected"    // clamd replied "stream: <name> FOUND"
	scanUnavailable scanClass = "unavailable" // no connection, or it closed without a reply; or the scanner was already down
	scanTimeout     scanClass = "timeout"     // no reply within clamdTimeout, or the request ended first
	scanError       scanClass = "error"       // any reply other than OK or FOUND, or a stream the server cut short (this upload only)
	scanStale       scanClass = "stale"       // signatures older than the limit, or their date unreadable
	scanDisabled    scanClass = "disabled"    // no CLAMD_ADDRESS
)

// scanRefusal is Scan's error. Ref (8 hex) is shown to the uploader and logged once, never stored; stale and
// disabled refusals have none. The signature name is kept for the log line and the admin counts only.
type scanRefusal struct {
	Class  scanClass
	Ref    string
	sig    string
	notice string // stale and disabled: the scanner state's vendor notice
}

func (r *scanRefusal) Error() string {
	return "upload refused by the malware scanner check: " + string(r.Class)
}

// Status is the HTTP status of the refusal page: 422 for a matched signature, 503 (with Retry-After) otherwise.
func (r *scanRefusal) Status() int {
	if r.Class == scanInfected {
		return http.StatusUnprocessableEntity
	}
	return http.StatusServiceUnavailable
}

// Message is the uploader's copy. staffContact is staffContactSentence's line (A-119, A-279), shown with a
// flagged file; "" leaves it out.
func (r *scanRefusal) Message(staffContact string) string {
	switch r.Class {
	case scanInfected:
		if staffContact != "" {
			staffContact += " "
		}
		return "Photo refused: the malware scanner flagged this file. Nothing was stored. If you think this is a mistake, send the market staff this reference. " + staffContact + "Reference: " + r.Ref
	case scanStale, scanDisabled:
		return r.notice
	}
	return "Photo not saved: the malware scanner did not respond, so nothing was stored. Try again later. Reference: " + r.Ref
}

// auditUploadRefusedInfected is the audit row of an upload a signature matched: fixed copy, no name, no reference.
const auditUploadRefusedInfected = "Upload refused by the malware scanner"

// auditScanRefusal records an infected refusal on the uploader's account; other refusals record nothing.
func (a *App) auditScanRefusal(ctx context.Context, userID string, err error) error {
	var r *scanRefusal
	if !errors.As(err, &r) || r.Class != scanInfected {
		return nil
	}
	_, err = a.db.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", userID, auditUploadRefusedInfected)
	return err
}

// Scanner states, as the admin panel names them.
const (
	scannerDisabled    = "Disabled"
	scannerReady       = "Ready"
	scannerUnavailable = "Unavailable"
	scannerStale       = "Signatures stale"
)

// scannerState is the cached scanner status at one moment. Cause explains Unavailable ("not checked yet",
// "connection failed", "timed out", "unexpected reply") and an unreadable signature date.
type scannerState struct {
	Name, Cause        string
	Engine, Daily      string    // "ClamAV 1.4.3", "27999"; from the last VERSION read
	DailyDate          time.Time // zero when unreadable
	AgeDays, LimitDays int
	Checked, LastScan  time.Time // last poll; last clean scan in this process
}

// VendorNotice is the copy the upload forms show instead of (Disabled) or over (Unavailable, stale) the file
// input; "" when Ready.
func (s scannerState) VendorNotice() string {
	switch s.Name {
	case scannerDisabled:
		return "Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up."
	case scannerUnavailable:
		return "Photo uploads are paused: the malware scanner is not responding. Listing details can still be saved."
	case scannerStale:
		return fmt.Sprintf("Photo uploads are paused: the malware scanner's signatures are older than %d days. Listing details can still be saved.", s.LimitDays)
	}
	return ""
}

// imageScanner is the process's clamd client and its cached status. A nil *imageScanner is Disabled.
type imageScanner struct {
	cfg scannerConfig
	now func() time.Time

	mu       sync.Mutex
	checked  time.Time // last poll
	up       bool      // the last poll (or a later scan) reached clamd
	cause    string    // why not up
	engine   string
	daily    string
	date     time.Time
	lastScan time.Time
	buckets  []scanBucket // hourly counts, oldest first
}

func newImageScanner(cfg scannerConfig) *imageScanner {
	return &imageScanner{cfg: cfg, now: time.Now}
}

func init() {
	registerBackground(func(ctx context.Context, a *App) { a.scanner.run(ctx) })
}

// State returns the cached status; it never contacts clamd.
func (s *imageScanner) State() scannerState {
	if s == nil || s.cfg.address == "" {
		return scannerState{Name: scannerDisabled}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *imageScanner) stateLocked() scannerState {
	st := scannerState{Engine: s.engine, Daily: s.daily, DailyDate: s.date, LimitDays: s.cfg.maxAgeDays, Checked: s.checked, LastScan: s.lastScan}
	if !s.date.IsZero() {
		st.AgeDays = max(0, int(s.now().Sub(s.date)/(24*time.Hour)))
	}
	switch {
	case s.checked.IsZero():
		st.Name, st.Cause = scannerUnavailable, "not checked yet"
	case !s.up:
		st.Name, st.Cause = scannerUnavailable, s.cause
	case s.date.IsZero():
		st.Name, st.Cause = scannerStale, "signature date unreadable"
	case s.now().Sub(s.date) > time.Duration(st.LimitDays)*24*time.Hour: // exact, as "older than N days" says
		st.Name = scannerStale
		// Whole days rounded up, so a stale age never reads as within the limit ("30 days, limit 30").
		st.AgeDays = int((s.now().Sub(s.date) + 24*time.Hour - 1) / (24 * time.Hour))
	default:
		st.Name = scannerReady
	}
	return st
}

// update applies f to the status under the lock and logs a change of state (class only, never upload data):
//
//	image scanner: Unavailable (timed out)
func (s *imageScanner) update(f func()) {
	s.mu.Lock()
	before := s.stateLocked()
	f()
	after := s.stateLocked()
	s.mu.Unlock()
	if after.Name != before.Name || after.Cause != before.Cause {
		log.Print("image scanner: " + describeState(after))
	}
}

// describeState is the state with its detail: "Ready", "Unavailable (timed out)", "Signatures stale (31 days,
// limit 30)", "Signatures stale (signature date unreadable, limit 30)".
func describeState(st scannerState) string {
	switch {
	case st.Name == scannerStale && st.Cause != "":
		return fmt.Sprintf("%s (%s, limit %d)", st.Name, st.Cause, st.LimitDays)
	case st.Name == scannerStale:
		return fmt.Sprintf("%s (%d days, limit %d)", st.Name, st.AgeDays, st.LimitDays)
	case st.Cause != "":
		return st.Name + " (" + st.Cause + ")"
	}
	return st.Name
}

// run polls clamd at once and then every scannerPollInterval until ctx ends (a background task).
func (s *imageScanner) run(ctx context.Context) {
	if s == nil || s.cfg.address == "" {
		return
	}
	t := time.NewTicker(scannerPollInterval)
	defer t.Stop()
	for {
		s.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var errUnexpectedReply = errors.New("unexpected clamd reply")

// poll asks clamd for PING and VERSION and caches the answer.
func (s *imageScanner) poll(ctx context.Context) {
	if s == nil || s.cfg.address == "" {
		return
	}
	reply, err := clamdCommand(ctx, s.cfg.address, "PING", nil)
	if err == nil && reply != "PONG" {
		err = errUnexpectedReply
	}
	version := ""
	if err == nil {
		version, err = clamdCommand(ctx, s.cfg.address, "VERSION", nil)
	}
	if contextEnded(ctx) {
		return // shutting down: the failure says nothing about clamd
	}
	s.update(func() {
		s.checked = s.now()
		if err != nil {
			s.up, s.cause = false, failureCause(err)
			return
		}
		s.up, s.cause = true, ""
		s.engine, s.daily, s.date = parseClamdVersion(version)
	})
}

// failureCause is the admin panel's word for why clamd could not be used.
func failureCause(err error) string {
	switch {
	case errors.Is(err, errUnexpectedReply):
		return "unexpected reply"
	case clamdErrorClass(err) == scanTimeout:
		return "timed out"
	}
	return "connection failed"
}

// parseClamdVersion reads "ClamAV 1.4.3/27999/Thu Sep 25 07:57:02 2026" (the daily signature database's build
// date, ANSI C format, UTC in the clamav image). Anything else leaves the date zero, which counts as stale.
func parseClamdVersion(v string) (engine, daily string, date time.Time) {
	parts := strings.SplitN(v, "/", 3)
	if strings.HasPrefix(parts[0], "ClamAV ") && len(parts[0]) <= 64 {
		engine = strings.ToValidUTF8(parts[0], "?")
	}
	if len(parts) != 3 || parts[1] == "" || strings.Trim(parts[1], "0123456789") != "" || len(parts[1]) > 12 {
		return engine, "", time.Time{}
	}
	d, err := time.Parse(time.ANSIC, parts[2])
	if err != nil {
		return engine, parts[1], time.Time{}
	}
	return engine, parts[1], d.UTC()
}

// Scan sends data to clamd once, unmodified, and returns it as a scannedUpload only when clamd answers exactly
// "stream: OK". Any other outcome is a *scanRefusal; the Ready check uses the cached state, so a scanner that
// is disabled, stale or already known to be down is refused without a connection. kind is only logged.
func (s *imageScanner) Scan(ctx context.Context, kind uploadKind, data []byte) (scannedUpload, error) {
	st := s.State()
	switch st.Name {
	case scannerDisabled:
		return scannedUpload{}, &scanRefusal{Class: scanDisabled, notice: st.VendorNotice()}
	case scannerStale:
		return scannedUpload{}, &scanRefusal{Class: scanStale, notice: st.VendorNotice()}
	case scannerUnavailable:
		s.record(scanUnavailable, "")
		return scannedUpload{}, s.refuse(kind, scanUnavailable, "")
	}
	reply, err := clamdCommand(ctx, s.cfg.address, "INSTREAM", data)
	if err != nil {
		class := clamdErrorClass(err)
		// When the request ended first (the uploader went away), clamd is not at fault: neither pause everyone's
		// uploads nor count a scanner failure.
		if !contextEnded(ctx) {
			if class != scanError { // a bad reply to one stream is that upload's failure; the poller checks clamd itself
				s.update(func() { s.up, s.cause = false, failureCause(err) })
			}
			s.record(class, "")
		}
		return scannedUpload{}, s.refuse(kind, class, "")
	}
	if reply == "stream: OK" {
		s.mu.Lock()
		s.lastScan = s.now()
		s.mu.Unlock()
		return scannedUpload{cleanBytes: data}, nil
	}
	if name, ok := strings.CutPrefix(reply, "stream: "); ok {
		if name, ok = strings.CutSuffix(name, " FOUND"); ok && strings.TrimSpace(name) != "" {
			name = logSafe(name, 128)
			s.record(scanInfected, name)
			return scannedUpload{}, s.refuse(kind, scanInfected, name)
		}
	}
	s.record(scanError, "")
	return scannedUpload{}, s.refuse(kind, scanError, "")
}

// refuse logs a refusal with a new reference.
func (s *imageScanner) refuse(kind uploadKind, class scanClass, sig string) *scanRefusal {
	r := &scanRefusal{Class: class, Ref: randomToken()[:8], sig: sig}
	if class == scanInfected {
		log.Printf("upload outcome=refused reason=infected sig=%s ref=%s kind=%s", sig, r.Ref, kind)
	} else {
		log.Printf("upload outcome=refused reason=%s ref=%s kind=%s", class, r.Ref, kind)
	}
	return r
}

// logSafe keeps the characters ClamAV uses in signature names, replaces any other with "_" and cuts the result
// to limit bytes, so the name cannot break or forge a log line.
func logSafe(s string, limit int) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._-:+/(){}", c) >= 0) {
			b[i] = '_'
		}
	}
	return string(b[:min(len(b), limit)])
}

// clamdCommand sends "z<cmd>\0" to clamd on a new connection, then, when stream is not nil, stream as zINSTREAM
// chunks of at most 64 KiB (each after its 32-bit big-endian length) and a zero-length terminator, and reads the
// NUL-terminated reply. Connecting is bounded by clamdDialTimeout and the whole exchange by clamdTimeout, and
// the exchange ends as soon as ctx does.
func clamdCommand(ctx context.Context, addr, cmd string, stream []byte) (string, error) {
	deadline := time.Now().Add(clamdTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := net.Dialer{Timeout: clamdDialTimeout, Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err = conn.Write([]byte("z" + cmd + "\x00")); err != nil {
		return "", err
	}
	br := bufio.NewReaderSize(conn, clamdReplyLimit+1)
	if cmd != "INSTREAM" {
		return readClamdReply(br)
	}
	buf := make([]byte, 4+clamdChunk)
	for rest := stream; ; {
		n := min(len(rest), clamdChunk)
		binary.BigEndian.PutUint32(buf, uint32(n))
		copy(buf[4:], rest[:n])
		if _, err = conn.Write(buf[:4+n]); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return "", err
			}
			// The server closed before reading the whole stream, as clamd does after its error reply when a
			// stream exceeds StreamMaxLength. An error reply is reported as such. An early "stream: OK" is not a
			// scan of the bytes sent, and no reply (or one lost to the reset) fails this stream, not the scanner.
			reply, rerr := readClamdReply(br)
			if rerr == nil && reply != "stream: OK" {
				return reply, nil
			}
			return "", errStreamCut
		}
		if n == 0 {
			break
		}
		rest = rest[n:]
	}
	reply, err := readClamdReply(br)
	switch {
	case errors.Is(err, syscall.ECONNRESET):
		return "", errStreamCut // closed with part of the stream unread (a size overrun)
	case err != nil || reply != "stream: OK":
		return reply, err
	}
	// clamd closes the connection once it has answered (clamdscan reads to the end the same way). A reset
	// instead of that clean end means the server closed with part of the stream unread, so its OK does not
	// cover every byte sent; anything after the reply breaks the protocol. A server that has read every byte
	// but answers OK without scanning them cannot be told apart from here: clamd itself is the trust boundary.
	if _, err = br.ReadByte(); !errors.Is(err, io.EOF) {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", err
		}
		return "", errUnexpectedReply
	}
	return reply, nil
}

// errStreamCut: the server closed the connection before reading the whole zINSTREAM stream. It refuses that
// upload as an error reply would; it does not mark the scanner unavailable (the poller finds a real outage).
var errStreamCut = errors.New("clamd closed the connection before reading the whole stream")

// readClamdReply reads a NUL-terminated reply of at most clamdReplyLimit bytes.
func readClamdReply(br *bufio.Reader) (string, error) {
	reply, err := br.ReadSlice(0)
	switch {
	case err == nil:
		return string(reply[:len(reply)-1]), nil
	case errors.Is(err, bufio.ErrBufferFull), len(reply) > 0 && errors.Is(err, io.EOF):
		return "", errUnexpectedReply
	}
	return "", err
}

// contextEnded reports whether ctx is done or its deadline has passed: the connection deadline clamdCommand
// takes from ctx can fire a moment before ctx itself reports it.
func contextEnded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

// clamdErrorClass classifies a failed exchange: timeout (including the request ending), error (a reply that
// broke the protocol) or unavailable.
func clamdErrorClass(err error) scanClass {
	var ne net.Error
	switch {
	case errors.Is(err, errUnexpectedReply) || errors.Is(err, errStreamCut):
		return scanError
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.As(err, &ne) && ne.Timeout():
		return scanTimeout
	}
	return scanUnavailable
}

// scanBucket counts one hour's outcomes.
type scanBucket struct {
	hour                     time.Time
	stored, infected, failed int
	sigs                     map[string]int
}

// ScannerSignature is one signature's count of refused uploads.
type ScannerSignature struct {
	Name  string
	Count int
}

// scanCounts are the outcomes of about the last 24 hours in this process, by signature and never by account.
type scanCounts struct {
	Stored, Infected, Failed int
	Signatures               []ScannerSignature
}

// bucketLocked returns the current hour's bucket, dropping any that ended more than 24 hours ago.
func (s *imageScanner) bucketLocked() *scanBucket {
	now := s.now().UTC()
	s.buckets = slices.DeleteFunc(s.buckets, func(b scanBucket) bool { return !b.hour.Add(time.Hour).After(now.Add(-24 * time.Hour)) })
	hour := now.Truncate(time.Hour)
	if n := len(s.buckets); n == 0 || !s.buckets[n-1].hour.Equal(hour) {
		s.buckets = append(s.buckets, scanBucket{hour: hour, sigs: map[string]int{}})
	}
	return &s.buckets[len(s.buckets)-1]
}

// record counts a refusal: infected by signature, and unavailable, timeout or error as a scanner failure.
// Stale and disabled refusals are not counted (the state says why).
func (s *imageScanner) record(class scanClass, sig string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucketLocked()
	switch class {
	case scanInfected:
		b.infected++
		if _, seen := b.sigs[sig]; !seen && len(b.sigs) >= scanSignatureNames {
			sig = otherSignatures
		}
		b.sigs[sig]++
	case scanUnavailable, scanTimeout, scanError:
		b.failed++
	}
}

// RecordStored counts an upload stored after a clean scan; the upload pipeline calls it once its row commits.
func (s *imageScanner) RecordStored() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucketLocked().stored++
}

// counts sums the buckets of about the last 24 hours.
func (s *imageScanner) counts() scanCounts {
	var c scanCounts
	if s == nil {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	sigs := map[string]int{}
	for _, b := range s.buckets {
		if !b.hour.Add(time.Hour).After(now.Add(-24 * time.Hour)) {
			continue
		}
		c.Stored, c.Infected, c.Failed = c.Stored+b.stored, c.Infected+b.infected, c.Failed+b.failed
		for name, n := range b.sigs {
			sigs[name] += n
		}
	}
	for name, n := range sigs {
		c.Signatures = append(c.Signatures, ScannerSignature{Name: name, Count: n})
	}
	slices.SortFunc(c.Signatures, func(a, b ScannerSignature) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.Name, b.Name)
	})
	return c
}

// ScannerView is the admin page's Image scanner panel (/admin#image-scanner), built from the cached state.
type ScannerView struct {
	External                 bool   // isolated gateway owns scanner checks; local metrics do not apply
	State, Detail            string // "Signatures stale", "31 days, limit 30"
	Disabled, Ready, Stale   bool
	Engine, Daily, DailyDate string
	HasDate, Warn            bool // Warn: Ready, and at least half the age limit old
	AgeDays, LimitDays       int
	Checked, LastScan        string
	Counts                   scanCounts
	DatabaseSize             string
}

func init() {
	registerLoader("admin", func(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
		v := scannerView(a.scanner.State(), a.scanner.counts())
		if a.isolatedUploads {
			v.External, v.Disabled, v.State = true, false, "Separate upload gateway"
		}
		if err := a.db.QueryRowContext(ctx, "SELECT pg_size_pretty(pg_database_size(current_database()))").Scan(&v.DatabaseSize); err != nil {
			return err
		}
		d.Scanner = v
		return nil
	})
	registerPreview("admin", func(d *PageData) {
		st := scannerState{Name: scannerReady, Engine: "ClamAV 1.4.3", Daily: "27999", DailyDate: time.Date(2026, 1, 1, 7, 57, 0, 0, time.UTC), AgeDays: 16, LimitDays: 30,
			Checked: time.Date(2026, 1, 17, 9, 0, 0, 0, time.UTC), LastScan: time.Date(2026, 1, 17, 8, 41, 0, 0, time.UTC)}
		d.Scanner = scannerView(st, scanCounts{Stored: 3, Infected: 1, Signatures: []ScannerSignature{{Name: "Sample.Signature.UNOFFICIAL", Count: 1}}})
		d.Scanner.DatabaseSize = "preview sample"
	})
}

// scannerView renders st and the 24-hour counts for the admin panel.
func scannerView(st scannerState, c scanCounts) *ScannerView {
	const stamp = "2006-01-02 15:04 UTC"
	v := &ScannerView{State: st.Name, Disabled: st.Name == scannerDisabled, Ready: st.Name == scannerReady, Stale: st.Name == scannerStale,
		Engine: st.Engine, Daily: st.Daily, HasDate: !st.DailyDate.IsZero(), AgeDays: st.AgeDays, LimitDays: st.LimitDays, Counts: c}
	if d := describeState(st); d != st.Name {
		v.Detail = strings.TrimSuffix(strings.TrimPrefix(d, st.Name+" ("), ")")
	}
	if v.HasDate {
		v.DailyDate = st.DailyDate.UTC().Format(stamp)
	}
	v.Warn = v.Ready && v.HasDate && 2*st.AgeDays >= st.LimitDays
	if !st.Checked.IsZero() {
		v.Checked = st.Checked.UTC().Format(stamp)
	}
	if !st.LastScan.IsZero() {
		v.LastScan = st.LastScan.UTC().Format(stamp)
	}
	return v
}
