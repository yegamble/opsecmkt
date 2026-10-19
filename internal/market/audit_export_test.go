package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAuditSeed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestAuditSigningKeyConfig(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", "")
	if v := auditExportStatus(); v.Available || !strings.Contains(v.Error, "not configured") {
		t.Fatalf("unset: %+v", v)
	}
	for _, bad := range []string{"abc", strings.Repeat("zz", 32), testAuditSeed + "00", testAuditSeed[:62]} {
		t.Setenv("AUDIT_SIGNING_KEY", bad)
		if k, err := auditSigningKey(); k != nil || err == nil {
			t.Fatalf("%q accepted", bad)
		}
		if v := auditExportStatus(); v.Available || !strings.Contains(v.Error, "invalid") {
			t.Fatalf("invalid: %+v", v)
		}
	}
	t.Setenv("AUDIT_SIGNING_KEY", " "+strings.ToUpper(testAuditSeed)+"\n")
	seed, _ := hex.DecodeString(testAuditSeed)
	want := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if v := auditExportStatus(); !v.Available || v.PublicKey != hex.EncodeToString(want) || v.Error != "" {
		t.Fatalf("valid: %+v", v)
	}
}

func TestAuditSignatureFormat(t *testing.T) {
	seed, _ := hex.DecodeString(testAuditSeed)
	key := ed25519.NewKeyFromSeed(seed)
	export := []byte("{\"id\":1}\n")
	sig := auditSignature(key, export)
	if !bytes.Equal(sig, auditSignature(key, export)) {
		t.Fatal("signature file is not deterministic")
	}
	f := parseSigFile(t, sig)
	sum := sha256.Sum256(export)
	s, _ := hex.DecodeString(f["signature"])
	pub, _ := hex.DecodeString(f["public_key"])
	if len(f) != 4 || f["algorithm"] != "ed25519" || f["sha256"] != hex.EncodeToString(sum[:]) || !ed25519.Verify(pub, export, s) {
		t.Fatalf("signature file: %q", sig)
	}
}

// A-139, A-251: the export's write window is 20 s plus one second per started 32 KiB, without a ceiling, so a large
// export over Tor (0.19 MB/s and up measured) is not cut by the server's 20 s WriteTimeout.
func TestAuditExportWriteWindowScalesWithSize(t *testing.T) {
	for _, c := range []struct {
		n    int
		want time.Duration
	}{
		{0, 21 * time.Second},
		{32<<10 - 1, 21 * time.Second},
		{32 << 10, 22 * time.Second},
		{1 << 20, 53 * time.Second},
		{50 << 20, 1621 * time.Second},
	} {
		if got := auditExportWriteWindow(c.n); got != c.want {
			t.Errorf("auditExportWriteWindow(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}

func parseSigFile(t *testing.T, sig []byte) map[string]string {
	t.Helper()
	f := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sig)), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		f[k] = v
	}
	return f
}

func TestAuditExportIntegration(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, admin := e.user("export_admin", "admin")
	_, buyer := e.user("export_buyer", "buyer")
	_, mod := e.user("export_mod", "moderator")
	for _, q := range []string{
		`INSERT INTO audit_events(user_id,action) VALUES($1,'First <event> & "quotes"')`,
		`INSERT INTO audit_events(user_id,action) VALUES($1,'Second event')`,
		`INSERT INTO audit_events(user_id,action) VALUES(NULL,'System event')`,
	} {
		args := []any{adminID}
		if strings.Contains(q, "NULL") {
			args = nil
		}
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var upto int64
	if err := e.DB.QueryRow("SELECT max(id) FROM audit_events").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/audit-export?upto=%d", upto)

	// Access control: anonymous -> sign in, non-admins -> 403.
	w := e.do("GET", path, randomToken(), nil)
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous export: %d %v", w.Code, w.Header())
	}
	for _, s := range []string{buyer, mod} {
		if w := e.do("GET", path, s, nil); w.Code != 403 || strings.Contains(w.Body.String(), "System event") {
			t.Fatalf("non-admin export: %d %s", w.Code, w.Body)
		}
	}

	w = e.do("GET", path, admin, nil)
	e.check(w, 200)
	if w.Header().Get("Content-Type") != "application/x-ndjson" || !strings.Contains(w.Header().Get("Content-Disposition"), fmt.Sprintf(`attachment; filename="audit-events-upto-%d.jsonl"`, upto)) {
		t.Fatalf("export headers: %v", w.Header())
	}
	export := w.Body.Bytes()
	lines := strings.Split(strings.TrimSuffix(string(export), "\n"), "\n")
	if int64(len(lines)) != upto {
		t.Fatalf("expected %d lines, got %q", upto, export)
	}
	var last struct {
		ID      int64   `json:"id"`
		UserID  *string `json:"user_id"`
		Handle  *string `json:"handle"`
		Action  string  `json:"action"`
		Created string  `json:"created"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.ID != upto || last.UserID != nil || last.Handle != nil || last.Action != "System event" || !strings.HasSuffix(last.Created, "Z") {
		t.Fatalf("last line %q: %+v %v", lines[len(lines)-1], last, err)
	}
	u := func(hex string) string { return `\` + "u00" + hex } // JSON escapes of < > & (encoding/json default)
	wantAction := "First " + u("3c") + "event" + u("3e") + " " + u("26") + ` \"quotes\"`
	if !strings.HasPrefix(lines[len(lines)-3], fmt.Sprintf(`{"id":%d,"user_id":"%s","handle":"export_admin","action":"%s","created":"`, upto-2, adminID, wantAction)) {
		t.Fatalf("field order/escaping changed: %s", lines[len(lines)-3])
	}

	w = e.do("GET", path+"&sig=1", admin, nil)
	e.check(w, 200)
	sig := w.Body.Bytes()
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Header().Get("Content-Disposition"), ".sig") {
		t.Fatalf("sig headers: %v", w.Header())
	}
	f := parseSigFile(t, sig)
	pub, _ := hex.DecodeString(f["public_key"])
	s, _ := hex.DecodeString(f["signature"])
	if f["public_key"] != auditExportStatus().PublicKey || !ed25519.Verify(pub, export, s) {
		t.Fatalf("signature does not verify: %q", sig)
	}

	// Later events and restarts do not change the bytes for the same upto.
	if _, err := e.DB.Exec(`INSERT INTO audit_events(user_id,action) VALUES($1,'Later event')`, adminID); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if again := e.do("GET", path, admin, nil).Body.Bytes(); !bytes.Equal(again, export) {
		t.Fatalf("export not byte-stable:\n%s\n%s", export, again)
	}
	if again := e.do("GET", path+"&sig=1", admin, nil).Body.Bytes(); !bytes.Equal(again, sig) {
		t.Fatal("signature not byte-stable")
	}
	earlier := e.do("GET", fmt.Sprintf("/admin/audit-export?upto=%d", upto-1), admin, nil).Body.String()
	if earlier != strings.Join(lines[:len(lines)-1], "\n")+"\n" {
		t.Fatalf("smaller upto is not a prefix: %q", earlier)
	}

	// Verifier CLI round trip with the pinned key; tampering fails.
	dir := t.TempDir()
	jsonl, sigPath, tampered := filepath.Join(dir, "export.jsonl"), filepath.Join(dir, "export.sig"), filepath.Join(dir, "tampered.jsonl")
	os.WriteFile(jsonl, export, 0o600)
	os.WriteFile(sigPath, sig, 0o600)
	os.WriteFile(tampered, bytes.Replace(export, []byte("Second event"), []byte("Second evenT"), 1), 0o600)
	bin := filepath.Join(dir, "verify-audit")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/verify-audit").CombinedOutput(); err != nil {
		t.Fatalf("build verifier: %v %s", err, out)
	}
	if out, err := exec.Command(bin, "-pub", f["public_key"], jsonl, sigPath).CombinedOutput(); err != nil || !strings.Contains(string(out), fmt.Sprintf("OK: %d events", upto)) {
		t.Fatalf("verifier rejected a genuine export: %v %s", err, out)
	}
	if out, err := exec.Command(bin, "-pub", f["public_key"], tampered, sigPath).CombinedOutput(); err == nil || !strings.Contains(string(out), "FAIL") {
		t.Fatalf("verifier accepted a tampered export: %s", out)
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if out, err := exec.Command(bin, "-pub", hex.EncodeToString(otherPub), jsonl, sigPath).CombinedOutput(); err == nil {
		t.Fatalf("verifier accepted an unpinned key: %s", out)
	}

	// Range validation and admin page.
	for _, bad := range []string{"", "upto=0", "upto=-1", "upto=abc", fmt.Sprintf("upto=%d", upto+100)} {
		if w := e.do("GET", "/admin/audit-export?"+bad, admin, nil); w.Code != 400 {
			t.Fatalf("%q: %d", bad, w.Code)
		}
	}
	page := e.do("GET", "/admin/transparency", admin, nil)
	e.check(page, 200)
	if body := page.Body.String(); !strings.Contains(body, f["public_key"]) || !strings.Contains(body, fmt.Sprintf(`value="%d"`, upto+1)) || !strings.Contains(body, "Download signature") {
		t.Fatal("admin page lacks export controls")
	}
	if body := e.do("GET", "/canary", randomToken(), nil).Body.String(); !strings.Contains(body, f["public_key"]) || !strings.Contains(body, "Signed exports enabled") {
		t.Fatal("public key not published on /canary")
	}

	// Rate limit: 10 exports per 10 minutes per admin (this test already used 9 successful + 5 rejected).
	e.A.limits = map[string]bucket{}
	for i := 0; i < 10; i++ {
		e.check(e.do("GET", path, admin, nil), 200)
	}
	e.check(e.do("GET", path, admin, nil), 429)
}

func TestAuditExportUnavailableWithoutKey(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", "")
	e := newTestApp(t)
	_, admin := e.user("nokey_admin", "admin")
	w := e.do("GET", "/admin/audit-export?upto=1", admin, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "AUDIT_SIGNING_KEY") || strings.Contains(w.Body.String(), "{") {
		t.Fatalf("export without key: %d %s", w.Code, w.Body)
	}
	page := e.do("GET", "/admin/transparency", admin, nil).Body.String()
	if !strings.Contains(page, "AUDIT_SIGNING_KEY is not configured") || strings.Contains(page, "Download export") {
		t.Fatal("admin page must show export as unavailable")
	}
	if body := e.do("GET", "/canary", randomToken(), nil).Body.String(); !strings.Contains(body, "Signed exports unavailable") {
		t.Fatal("canary page must show export as unavailable")
	}
	t.Setenv("AUDIT_SIGNING_KEY", "not-hex")
	if w := e.do("GET", "/admin/audit-export?upto=1", admin, nil); w.Code != 409 || !strings.Contains(w.Body.String(), "invalid") {
		t.Fatalf("invalid key: %d %s", w.Code, w.Body)
	}
}

// The export's upper bound is only accepted once every lower id is settled, so concurrent writers cannot
// later insert a row inside an already exported range.
func TestAuditExportSettledUnderConcurrentWrites(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, _ := e.user("race_admin", "admin")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				e.DB.Exec(`INSERT INTO audit_events(user_id,action) VALUES($1,'concurrent')`, adminID)
			}
		}()
	}
	var exports [][]byte
	var uptos []int64
	for i := 0; i < 5; i++ {
		settled, err := settledAuditMax(t.Context(), e.DB)
		if err != nil {
			t.Fatal(err)
		}
		b, err := auditExport(t.Context(), e.DB, settled)
		if err != nil {
			t.Fatal(err)
		}
		exports, uptos = append(exports, b), append(uptos, settled)
	}
	wg.Wait()
	for i, b := range exports {
		after, err := auditExport(t.Context(), e.DB, uptos[i])
		if err != nil || !bytes.Equal(after, b) {
			t.Fatalf("export up to %d changed after concurrent writers finished", uptos[i])
		}
	}
}

// smallBufferListener shrinks each accepted connection's send buffer, so a slow reader makes the server's
// writes wait on the network within a few kilobytes instead of disappearing into the kernel's buffers.
type smallBufferListener struct{ net.Listener }

func (l smallBufferListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(4096)
	}
	return c, err
}

// A-139: over Tor (0.19–0.45 MB/s measured) a large export outlasts the server's 20 s WriteTimeout, and its build
// may outlast the 12 s request bound and the 15 s ReadTimeout (which net/http clears for a body-less GET, so the
// export's own read deadline is defensive, A-251); a cut download fails verify-audit like tampering. Here the server's timeouts are shortened to 200 ms and 1 s: the export waits
// 1.5 s behind an in-flight audit writer, then a client reading 4 KB per 5 ms still receives every byte, the same
// bytes as for the same upto in-process (invariant 8), and they verify; a partial file fails with "sha256 does not
// match". Another route on the same server is still cut by its WriteTimeout.
func TestAuditExportOutlivesServerTimeouts(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, admin := e.user("slow_export_admin", "admin")
	e.mustExec(`INSERT INTO audit_events(user_id,action) SELECT $1, 'Slow download row ' || g FROM generate_series(1,4000) g`, adminID)
	var upto int64
	if err := e.DB.QueryRow("SELECT max(id) FROM audit_events").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	want, err := auditExport(t.Context(), e.DB, upto)
	if err != nil || len(want) < 300<<10 {
		t.Fatalf("export of %d bytes: %v", len(want), err)
	}

	const writeTimeout, readTimeout = 200 * time.Millisecond, time.Second
	const delay = 2 * writeTimeout
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "slow" {
			time.Sleep(delay) // a page answered after the server's WriteTimeout
		}
		e.A.ServeHTTP(w, r)
	}))
	srv.Config.WriteTimeout, srv.Config.ReadTimeout = writeTimeout, readTimeout
	srv.Listener = smallBufferListener{srv.Listener}
	srv.Start()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: admin})
		return client.Do(req)
	}
	slowRead := func(resp *http.Response) ([]byte, error) {
		defer resp.Body.Close()
		var b bytes.Buffer
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err == io.EOF {
				return b.Bytes(), nil
			}
			if err != nil {
				return b.Bytes(), err
			}
			// 5 ms per 4 KB: about 1 s for the export, well past the 200 ms WriteTimeout.
			time.Sleep(time.Duration(n) * 5 * time.Millisecond / 4096)
		}
	}
	path := fmt.Sprintf("/admin/audit-export?upto=%d", upto)

	// An in-flight audit writer holds off the export's SHARE lock for longer than both server timeouts.
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO audit_events(user_id,action) VALUES($1,'In-flight writer')`, adminID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code    int
		body    []byte
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		resp, err := get(path)
		if err != nil {
			done <- result{err: err}
			return
		}
		body, err := slowRead(resp)
		done <- result{resp.StatusCode, body, err, time.Since(start)}
	}()
	time.Sleep(3 * readTimeout / 2)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil || res.code != 200 || !bytes.Equal(res.body, want) {
		t.Fatalf("slow export: status %d, %d of %d bytes, %v", res.code, len(res.body), len(want), res.err)
	}
	if res.elapsed < 3*readTimeout/2 {
		t.Fatalf("export took %s: it did not wait for the in-flight writer", res.elapsed)
	}
	resp, err := get(path + "&sig=1")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := slowRead(resp)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("signature: %d %v", resp.StatusCode, err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "verify-audit")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/verify-audit").CombinedOutput(); err != nil {
		t.Fatalf("build verifier: %v %s", err, out)
	}
	jsonl, partial, sigPath := filepath.Join(dir, "export.jsonl"), filepath.Join(dir, "partial.jsonl"), filepath.Join(dir, "export.sig")
	os.WriteFile(jsonl, res.body, 0o600)
	os.WriteFile(partial, res.body[:len(res.body)/2], 0o600)
	os.WriteFile(sigPath, sig, 0o600)
	pub := parseSigFile(t, sig)["public_key"]
	if out, err := exec.Command(bin, "-pub", pub, jsonl, sigPath).CombinedOutput(); err != nil || !strings.Contains(string(out), fmt.Sprintf("OK: %d events", upto)) {
		t.Fatalf("verifier rejected the slow download: %v %s", err, out)
	}
	if out, err := exec.Command(bin, "-pub", pub, partial, sigPath).CombinedOutput(); err == nil || !strings.Contains(string(out), "sha256 does not match") {
		t.Fatalf("a partial download must fail with sha256 does not match: %v %s", err, out)
	}
	if page := e.do("GET", "/admin/transparency", admin, nil).Body.String(); !strings.Contains(page, `A download cut short (a dropped connection or Tor circuit) leaves a partial file that fails verification with "sha256 does not match"`) {
		t.Fatal("the admin export help does not say how a partial download fails")
	}

	// Other routes keep the server's timeouts: the same page is served in time and cut when it is answered
	// after the WriteTimeout (the client keeps one connection, reused after the export).
	if resp, err := get("/canary"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("canary in time: %v", err)
	} else {
		resp.Body.Close()
	}
	if resp, err := get("/canary?slow"); err == nil {
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr == nil {
			t.Fatalf("a page answered after the WriteTimeout arrived: %d, %d bytes", resp.StatusCode, len(body))
		}
	}
}

// A-139: the export's database work runs under its own bound, not the request's 12 s context, and still stops
// when the client goes away (ServeHTTP's arrival context).
func TestAuditExportBuildOutlivesRequestBound(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	adminID, _ := e.user("bound_admin", "admin")
	e.mustExec(`INSERT INTO audit_events(user_id,action) VALUES($1,'Bound event')`, adminID)
	var upto int64
	if err := e.DB.QueryRow("SELECT max(id) FROM audit_events").Scan(&upto); err != nil {
		t.Fatal(err)
	}
	want, err := auditExport(t.Context(), e.DB, upto)
	if err != nil {
		t.Fatal(err)
	}
	admin := &User{ID: adminID, Handle: "bound_admin", Role: "admin"}
	serve := func(arrival context.Context) *httptest.ResponseRecorder {
		// The request's own bound has already expired, as ServeHTTP's 12 s one would during a long build.
		ctx, cancel := context.WithDeadline(context.WithValue(arrival, arrivalKey{}, arrival), time.Now().Add(-time.Second))
		defer cancel()
		w := httptest.NewRecorder()
		auditExportHandler(e.A, w, httptest.NewRequest("GET", fmt.Sprintf("/admin/audit-export?upto=%d", upto), nil).WithContext(ctx), admin)
		return w
	}
	if w := serve(context.Background()); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("export after the request bound: %d %q", w.Code, w.Body)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if w := serve(gone); w.Code == 200 {
		t.Fatalf("export ran for a client that went away: %q", w.Body)
	}
}
