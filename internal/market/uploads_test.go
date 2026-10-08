package market

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A-186: the upload routes over HTTP — the order of admission, the drain, deadlines, the form's parts and the
// refusal pages (ruling C3, C5; A-186's acceptance).

// part is one multipart form part.
type part struct {
	name, filename, ctype string
	body                  []byte
}

func multipartBody(parts ...part) ([]byte, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		cd := `form-data; name="` + p.name + `"`
		if p.filename != "" {
			cd += `; filename="` + p.filename + `"`
		}
		h.Set("Content-Disposition", cd)
		if p.ctype != "" {
			h.Set("Content-Type", p.ctype)
		}
		w, _ := mw.CreatePart(h)
		w.Write(p.body)
	}
	mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}

// readCounter is a request body that counts what the server read of it.
type readCounter struct {
	r  io.Reader
	mu sync.Mutex
	n  int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
	return n, err
}

func (c *readCounter) read() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// uploadReq is a multipart POST of parts to path as session ("" for none), with its body counted.
func uploadReq(path, session string, parts ...part) (*http.Request, *readCounter) {
	body, ct := multipartBody(parts...)
	rc := &readCounter{r: bytes.NewReader(body)}
	r := httptest.NewRequest("POST", path, rc)
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	r.Header.Set("Content-Type", ct)
	if session != "" {
		r.AddCookie(&http.Cookie{Name: "session", Value: session})
	}
	return r, rc
}

// upload posts parts to path as session through ServeHTTP.
func (e *testEnv) upload(path, session string, parts ...part) *httptest.ResponseRecorder {
	r, _ := uploadReq(path, session, parts...)
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	return w
}

func (e *testEnv) csrfPart(session string) part {
	return part{name: "csrf", body: []byte(e.A.csrf(session))}
}

// photoParts is the listing photo form: csrf, id, photo (with a file name and type the server must ignore).
func (e *testEnv) photoParts(session, listing string, photo []byte) []part {
	return []part{e.csrfPart(session), {name: "id", body: []byte(listing)}, {name: "photo", filename: "photo.jpg", ctype: "image/jpeg", body: photo}}
}

func (e *testEnv) avatarParts(session string, photo []byte) []part {
	return []part{e.csrfPart(session), {name: "photo", filename: "me.png", ctype: "image/png", body: photo}}
}

// uploadEnv is a database-backed App whose scanner is a Ready fake clamd.
func uploadEnv(t *testing.T) (*testEnv, *fakeClamd) {
	t.Helper()
	e := newTestApp(t)
	f := newFakeClamd(t)
	e.withScanner(f)
	return e, f
}

func (e *testEnv) imageCount() int {
	e.t.Helper()
	var n int
	if err := e.DB.QueryRow("SELECT count(*) FROM images").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// scansOf counts the INSTREAM commands f received.
func scansOf(f *fakeClamd) int {
	n := 0
	for _, c := range f.Commands() {
		if c == "zINSTREAM" {
			n++
		}
	}
	return n
}

// checkPage asserts an HTML refusal page with status code containing want.
func checkPage(t *testing.T, w *httptest.ResponseRecorder, code int, want string) {
	t.Helper()
	if w.Code != code || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "</html>") || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("status=%d type=%q, want %d page with %q:\n%s", w.Code, w.Header().Get("Content-Type"), code, want, w.Body.String())
	}
}

func smallJPEG(t testing.TB) []byte { return fixtureJPEG(t, fixturePicture(200, 150), 85) }

// The happy paths: a vendor adds a photo to a listing, a vendor and a moderator set an avatar.
func TestUploadStoresPhotoAndAvatar(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("photo_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, metadataJPEG(t, 400, 300))...)
	if w.Code != 303 || w.Header().Get("Location") != "/listing-edit?id="+listing+"#photos" {
		t.Fatalf("photo: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var kind, owner, product string
	var width, height int
	var full, thumb []byte
	if err := e.DB.QueryRow(`SELECT kind,owner_user_id,product_id,width,height,"full",thumb FROM images`).Scan(&kind, &owner, &product, &width, &height, &full, &thumb); err != nil {
		t.Fatal(err)
	}
	if kind != "photo" || owner != vendorID || product != listing || width != 400 || height != 300 {
		t.Fatalf("row: %s %s %s %dx%d", kind, owner, product, width, height)
	}
	assertCleanJPEG(t, "stored full", full)
	assertCleanJPEG(t, "stored thumb", thumb)
	if scansOf(f) != 1 || e.A.scanner.counts().Stored != 1 {
		t.Fatalf("scans %d, stored count %d", scansOf(f), e.A.scanner.counts().Stored)
	}
	var audit string
	e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", vendorID).Scan(&audit)
	if audit != "Added a photo to listing "+listing[:8] {
		t.Fatalf("audit %q", audit)
	}
	for _, role := range []string{"vendor", "moderator", "admin"} {
		id, s := e.user("avatar_"+role, role)
		w = e.upload("/account/avatar", s, e.avatarParts(s, fixturePNG(t, fixturePicture(300, 200)))...)
		if w.Code != 303 || w.Header().Get("Location") != "/account#avatar" {
			t.Fatalf("%s avatar: %d %s", role, w.Code, w.Body.String())
		}
		e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", id).Scan(&audit)
		if audit != "Added avatar" {
			t.Fatalf("%s avatar audit %q", role, audit)
		}
	}
	var avatars int
	e.DB.QueryRow("SELECT count(*) FROM images WHERE kind='avatar' AND product_id IS NULL AND width=200 AND height=200 AND description=''").Scan(&avatars)
	if avatars != 3 {
		t.Fatalf("avatars %d", avatars)
	}
}

// A multipart post to any other route is refused with a 415 page, its body unread.
func TestUploadRefusesMultipartElsewhere(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("elsewhere_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	for _, path := range []string{"/listings/update", "/login", "/account/avatar/remove", "/listings/photo/", "/LISTINGS/PHOTO", "/nowhere"} {
		r, body := uploadReq(path, vendor, append(e.photoParts(vendor, listing, smallJPEG(t)), part{name: "title", body: []byte("x")})...)
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		checkPage(t, w, 415, "This form cannot take files")
		if body.read() != 0 {
			t.Fatalf("%s: read %d bytes of the refused body", path, body.read())
		}
	}
	if scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatal("a refused multipart post was scanned or stored")
	}
}

// Every route but the two upload routes keeps the 64 KiB urlencoded form cap; an urlencoded post to an upload
// route is refused as an incomplete form.
func TestNonUploadRoutesKeep64KiBCap(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("cap_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	w := e.do("POST", "/listings/update", vendor, url.Values{"id": {listing}, "description": {strings.Repeat("x", 70<<10)}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Invalid or oversized form") {
		t.Fatalf("70 KiB form: %d %s", w.Code, w.Body.String())
	}
	form := url.Values{"csrf": {e.A.csrf(vendor)}, "id": {listing}}.Encode()
	r := httptest.NewRequest("POST", "/listings/photo", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Content-Length", fmt.Sprint(len(form)))
	r.AddCookie(&http.Cookie{Name: "session", Value: vendor})
	w = httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	checkPage(t, w, 400, "incomplete or out of order")
}

// The csrf token must be the first part and match the session.
func TestUploadCSRFFirstPart(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("csrf_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	good := e.photoParts(vendor, listing, smallJPEG(t))
	for name, parts := range map[string][]part{
		"csrf second":   {good[1], good[0], good[2]},
		"csrf last":     {good[1], good[2], good[0]},
		"no csrf":       {good[1], good[2]},
		"wrong csrf":    {{name: "csrf", body: []byte(e.A.csrf(randomToken()))}, good[1], good[2]},
		"oversize csrf": {{name: "csrf", body: bytes.Repeat([]byte("a"), 129)}, good[1], good[2]},
	} {
		r, body := uploadReq("/listings/photo", vendor, parts...)
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		if w.Code != 403 && !(name == "oversize csrf" && w.Code == 400) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if body.read() != int(r.ContentLength) {
			t.Fatalf("%s: refusal read %d of %d bytes (no drain)", name, body.read(), r.ContentLength)
		}
	}
	if scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatal("a refused form was scanned or stored")
	}
}

// Parts come in the order csrf, id, description, photo; anything else, or anything after the photo, is refused.
func TestUploadPartOrderAndExtraPartsRefused(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("order_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	csrf, id, desc, photo := e.csrfPart(vendor), part{name: "id", body: []byte(listing)}, part{name: "description", body: []byte("A red box")}, part{name: "photo", filename: "a.jpg", body: smallJPEG(t)}
	for name, c := range map[string]struct {
		parts []part
		want  string
	}{
		"photo before id":        {[]part{csrf, photo, id}, "incomplete or out of order"},
		"description before id":  {[]part{csrf, desc, id, photo}, "incomplete or out of order"},
		"duplicate id":           {[]part{csrf, id, id, photo}, "incomplete or out of order"},
		"unknown part":           {[]part{csrf, id, {name: "title", body: []byte("x")}, photo}, "incomplete or out of order"},
		"extra part after photo": {[]part{csrf, id, photo, desc}, "incomplete or out of order"},
		"second photo":           {[]part{csrf, id, photo, photo}, "incomplete or out of order"},
		"no photo":               {[]part{csrf, id, desc}, "choose a JPEG or PNG file"},
		"empty photo":            {[]part{csrf, id, {name: "photo", filename: "", body: nil}}, "choose a JPEG or PNG file"},
		"long description":       {[]part{csrf, id, {name: "description", body: []byte(strings.Repeat("é", 251))}, photo}, "longer than 250 characters"},
		"NUL in description":     {[]part{csrf, id, {name: "description", body: []byte("a\x00b")}, photo}, "not valid UTF-8 or contains a NUL character"},
		"0xff in description":    {[]part{csrf, id, {name: "description", body: []byte("a\xffb")}, photo}, "not valid UTF-8 or contains a NUL character"},
		"0xff in id":             {[]part{csrf, {name: "id", body: []byte("listing\xff")}, photo}, "not valid UTF-8 or contains a NUL character"},
		"NUL in id":              {[]part{csrf, {name: "id", body: []byte("listing\x00")}, photo}, "not valid UTF-8 or contains a NUL character"},
	} {
		w := e.upload("/listings/photo", vendor, c.parts...)
		if w.Code != 400 || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatal("a malformed form was scanned or stored")
	}
	// All four parts in order are accepted; a 250-character description is kept.
	w := e.upload("/listings/photo", vendor, csrf, id, part{name: "description", body: []byte(strings.Repeat("é", 250))}, photo)
	if w.Code != 303 {
		t.Fatalf("four parts: %d %s", w.Code, w.Body.String())
	}
	var d string
	e.DB.QueryRow("SELECT description FROM images").Scan(&d)
	if d != strings.Repeat("é", 250) {
		t.Fatalf("description %q", d)
	}
}

// A buyer is refused with a page after the body is read; nothing is scanned.
func TestUploadBuyerRefused(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, _ := e.user("buyer_case_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	_, buyer := e.user("upload_buyer", "buyer")
	for _, path := range []string{"/listings/photo", "/account/avatar"} {
		r, body := uploadReq(path, buyer, e.photoParts(buyer, listing, smallJPEG(t))...)
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		checkPage(t, w, 403, "not saved")
		if body.read() != int(r.ContentLength) {
			t.Fatalf("%s: read %d of %d", path, body.read(), r.ContentLength)
		}
	}
	// A moderator may set an avatar but not add a listing photo.
	_, moderator := e.user("upload_moderator", "moderator")
	checkPage(t, e.upload("/listings/photo", moderator, e.photoParts(moderator, listing, smallJPEG(t))...), 403, "only vendors and administrators")
	if scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatal("a refused sender was scanned or stored")
	}
}

// Anonymous or expired: the sign-in page, after the body is read.
func TestUploadSignedOutGetsSignInPage(t *testing.T) {
	e, _ := uploadEnv(t)
	for _, session := range []string{"", randomToken()} {
		r, body := uploadReq("/account/avatar", session, e.avatarParts(session, smallJPEG(t))...)
		w := httptest.NewRecorder()
		e.A.ServeHTTP(w, r)
		checkPage(t, w, 401, "Sign in again")
		if !strings.Contains(w.Body.String(), `action="/login"`) || body.read() != int(r.ContentLength) {
			t.Fatalf("not the sign-in page, or read %d of %d", body.read(), r.ContentLength)
		}
	}
}

// C3: Content-Length above the ceiling, or none (chunked), is refused unread.
func TestUploadContentLengthOverCeilingUnread(t *testing.T) {
	e, _ := uploadEnv(t)
	_, vendor := e.user("ceiling_vendor", "vendor")
	body := &readCounter{r: bytes.NewReader(make([]byte, 64))}
	r := httptest.NewRequest("POST", "/account/avatar", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.AddCookie(&http.Cookie{Name: "session", Value: vendor})
	r.ContentLength = uploadCeiling + 1
	r.Header.Set("Content-Length", fmt.Sprint(uploadCeiling+1))
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	checkPage(t, w, 413, "larger than 4 MB")
	if body.read() != 0 {
		t.Fatalf("read %d bytes", body.read())
	}
	r = httptest.NewRequest("POST", "/account/avatar", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	r.ContentLength, r.TransferEncoding = -1, []string{"chunked"}
	w = httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	checkPage(t, w, 411, "Content-Length")
	if body.read() != 0 {
		t.Fatalf("read %d bytes", body.read())
	}
}

// No code in the package calls ParseMultipartForm, FormFile, MultipartReader, ReadForm or FormValue (they
// buffer a multipart body in memory or spill it to temporary files), and the upload path never calls
// MaxBytesReader (C3). Calls are found in the syntax tree, so comments may name them.
func TestUploadDoesNotUseParseMultipartForm(t *testing.T) {
	files, err := filepath.Glob("internal/market/*.go")
	if err != nil || len(files) < 50 {
		t.Fatalf("package files: %d %v", len(files), err)
	}
	banned := map[string]bool{"ParseMultipartForm": true, "FormFile": true, "MultipartReader": true, "ReadForm": true, "FormValue": true}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		uploadPath := strings.HasSuffix(name, "uploads.go") || strings.HasSuffix(name, "images.go")
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (banned[sel.Sel.Name] || uploadPath && sel.Sel.Name == "MaxBytesReader") {
				t.Errorf("%s uses %s", name, sel.Sel.Name)
			}
			return true
		})
	}
}

// An upload writes nothing to the temporary directory.
func TestUploadNoTempFiles(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("tmp_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if os.TempDir() != dir {
		t.Skip("TMPDIR not honoured on this platform")
	}
	big := fixtureJPEG(t, fixturePicture(1600, 1200), 95)
	if w := e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, big)...); w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("temporary files: %v", entries)
	}
}

// The scanner's refusals store nothing: disabled, stale, down, hanging, a FOUND, an ERROR reply and garbage.
func TestUploadScannerRefusalsStoreNothing(t *testing.T) {
	shortScanTimeout(t, 300*time.Millisecond)
	e := newTestApp(t)
	vendorID, vendor := e.user("scan_refusal_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	photo := smallJPEG(t)
	post := func() *httptest.ResponseRecorder {
		return e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, photo)...)
	}
	// Disabled: no CLAMD_ADDRESS.
	w := post()
	checkPage(t, w, 503, "Photo uploads are off on this market")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
	f := newFakeClamd(t)
	f.SetVersion(fakeClamdVersion(31 * 24 * time.Hour))
	e.withScanner(f)
	checkPage(t, post(), 503, "older than 30 days")
	f.SetVersion(fakeClamdVersion(24 * time.Hour))
	for name, c := range map[string]struct {
		verdict string
		code    int
		want    string
	}{
		"found":   {"stream: Win.Test.Upload-Sig FOUND", 422, "the malware scanner flagged this file"},
		"error":   {"INSTREAM size limit exceeded. ERROR", 503, "the malware scanner did not respond"},
		"garbage": {"stream: OK trailing", 503, "the malware scanner did not respond"},
		"hang":    {fakeClamdHang, 503, "the malware scanner did not respond"},
		"close":   {fakeClamdClose, 503, "the malware scanner did not respond"},
	} {
		e.A.scanner.poll(t.Context())
		f.SetVerdict(func([]byte) string { return c.verdict })
		w = post()
		checkPage(t, w, c.code, c.want)
		if c.code == 503 && w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: 503 without Retry-After", name)
		}
		if strings.Contains(w.Body.String(), "Win.Test") {
			t.Fatalf("%s: signature name shown", name)
		}
	}
	// Down: the poll failed, so the upload is refused without a connection.
	f.SetHang(true)
	e.A.scanner.poll(t.Context())
	checkPage(t, post(), 503, "Photo uploads are paused: the malware scanner is not responding")
	if n := e.imageCount(); n != 0 {
		t.Fatalf("%d rows stored", n)
	}
	var audits []string
	rows, _ := e.DB.Query("SELECT action FROM audit_events WHERE user_id=$1", vendorID)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		audits = append(audits, s)
	}
	rows.Close()
	if len(audits) != 1 || audits[0] != "Upload refused by the malware scanner" {
		t.Fatalf("audit rows %q", audits)
	}
}

// C6: the CI test signature in a PNG tEXt chunk passes the caps and is refused by the scanner, so the original
// bytes are what is scanned; a 4097×1 PNG is refused before any connection to clamd.
func TestUploadCapsBeforeScan(t *testing.T) {
	e, f := uploadEnv(t)
	_, vendor := e.user("caps_vendor", "vendor")
	before := len(f.Commands())
	w := e.upload("/account/avatar", vendor, e.avatarParts(vendor, pngHeader(4097, 1, 8, 2, 0))...)
	checkPage(t, w, 422, "4097×1 pixels")
	checkPage(t, e.upload("/account/avatar", vendor, e.avatarParts(vendor, jpegHeader(0xC2, 16, 16, 4))...), 415, "CMYK")
	checkPage(t, e.upload("/account/avatar", vendor, e.avatarParts(vendor, []byte("<svg/>"))...), 415, "only JPEG and PNG")
	if len(f.Commands()) != before {
		t.Fatalf("clamd contacted for capped uploads: %q", f.Commands()[before:])
	}
	checkPage(t, e.upload("/account/avatar", vendor, e.avatarParts(vendor, markerPNG(t))...), 422, "the malware scanner flagged this file")
	if scans := f.Scans(); len(scans) != 1 || !bytes.Equal(scans[0], markerPNG(t)) {
		t.Fatal("clamd did not receive the original bytes")
	}
	if e.imageCount() != 0 {
		t.Fatal("stored")
	}
}

// uploadServer serves e.A over a real listener with the given read and write timeouts (production: 15 s, 20 s).
func uploadServer(t *testing.T, e *testEnv, read, write time.Duration) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(e.A)
	srv.Config.ReadHeaderTimeout, srv.Config.ReadTimeout, srv.Config.WriteTimeout = 5*time.Second, read, write
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// rawUpload sends a multipart POST on its own connection, the body written by send (all at once when nil) while
// the response is read; it returns the response and its whole body, or the error reading them.
func rawUpload(t *testing.T, addr, path, session string, body []byte, ct string, send func(c net.Conn, body []byte)) (*http.Response, string, error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cookie := ""
	if session != "" {
		cookie = "Cookie: session=" + session + "\r\n"
	}
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n%sConnection: close\r\n\r\n", path, addr, ct, len(body), cookie)
	if send == nil {
		send = func(c net.Conn, body []byte) { c.Write(body) }
	}
	go send(c, body)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return nil, "", err
	}
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// trickle writes body in n-byte pieces every d.
func trickle(n int, d time.Duration) func(c net.Conn, body []byte) {
	return func(c net.Conn, body []byte) {
		for len(body) > 0 {
			k := min(n, len(body))
			if _, err := c.Write(body[:k]); err != nil {
				return
			}
			body = body[k:]
			time.Sleep(d)
		}
	}
}

// browserUpload posts like a browser without JavaScript: it sends the whole body through a small send buffer,
// and only then reads the answer. A server that answers and closes without reading the rest of the body makes
// the send fail (the connection reset UI measured in Firefox, C3).
func browserUpload(t *testing.T, addr, path, session string, body []byte, ct string) (*http.Response, string, error) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.(*net.TCPConn).SetWriteBuffer(16 << 10)
	c.SetDeadline(time.Now().Add(30 * time.Second))
	cookie := ""
	if session != "" {
		cookie = "Cookie: session=" + session + "\r\n"
	}
	if _, err = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n%sConnection: close\r\n\r\n", path, addr, ct, len(body), cookie); err != nil {
		return nil, "", err
	}
	for rest := body; len(rest) > 0; {
		n, err := c.Write(rest[:min(len(rest), 64<<10)])
		if err != nil {
			return nil, "", fmt.Errorf("sending the body: %w", err)
		}
		rest = rest[n:]
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return nil, "", err
	}
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// C3: a vendor sending 1 MiB over the cap gets the whole 413 page naming the limit, not a reset; so does an
// anonymous sender, who gets the sign-in page within the unextended deadline, and a buyer.
func TestUploadRefusalReadsRestOfBody(t *testing.T) {
	e, _ := uploadEnv(t)
	_, vendor := e.user("drain_vendor", "vendor")
	addr := uploadServer(t, e, 15*time.Second, 20*time.Second)
	oversize := append(append([]byte(nil), smallJPEG(t)...), make([]byte, uploadCap+1<<20)...)
	body, ct := multipartBody(e.avatarParts(vendor, oversize)...)
	resp, page, err := browserUpload(t, addr, "/account/avatar", vendor, body, ct)
	if err != nil {
		t.Fatalf("vendor over the cap: %v (a reset instead of the page)", err)
	}
	if resp.StatusCode != 413 || !strings.Contains(page, "larger than 4 MB") || !strings.Contains(page, "</html>") {
		t.Fatalf("vendor: %d %s", resp.StatusCode, page)
	}
	body, ct = multipartBody(e.avatarParts(randomToken(), oversize)...)
	start := time.Now()
	resp, page, err = browserUpload(t, addr, "/account/avatar", "", body, ct)
	if err != nil || resp.StatusCode != 401 || !strings.Contains(page, "Sign in again") || !strings.Contains(page, "</html>") || time.Since(start) > 15*time.Second {
		t.Fatalf("anonymous: %v %v", err, resp)
	}
	_, buyer := e.user("drain_buyer", "buyer")
	body, ct = multipartBody(e.avatarParts(buyer, oversize)...)
	if resp, page, err = browserUpload(t, addr, "/account/avatar", buyer, body, ct); err != nil || resp.StatusCode != 403 || !strings.Contains(page, "</html>") {
		t.Fatalf("buyer: %v %v", err, resp)
	}
	if e.imageCount() != 0 {
		t.Fatal("stored")
	}
}

// C3 falsification: an anonymous sender's drain holds no admission slot, no upload slot and no account lock,
// keeps the server's unextended read deadline, and buffers nothing.
func TestUploadAnonDrainHoldsNoSlot(t *testing.T) {
	e, _ := uploadEnv(t)
	addr := uploadServer(t, e, 1500*time.Millisecond, 20*time.Second)
	body, ct := multipartBody(e.avatarParts(randomToken(), make([]byte, uploadCap))...)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	fmt.Fprintf(c, "POST /account/avatar HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", addr, ct, len(body))
	go trickle(1024, 20*time.Millisecond)(c, body)
	time.Sleep(500 * time.Millisecond) // draining now
	for name, sem := range map[string]chan struct{}{"page": pageWork, "anon post": anonPostWork, "user post": userPostWork, "lookup": sessionLookupWork, "upload": uploadWork, "decode": imageDecodeWork} {
		if len(sem) != 0 {
			t.Fatalf("an anonymous drain holds a %s slot", name)
		}
	}
	e.A.mu.Lock()
	inFlight := len(e.A.inFlight)
	e.A.mu.Unlock()
	if inFlight != 0 {
		t.Fatal("an anonymous drain holds an account lock")
	}
	// The trickle needs about 80 s; the unextended deadline cuts it at 1.5 s.
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	io.Copy(io.Discard, c)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("an anonymous drain lasted %s: the read deadline was extended", d)
	}
	// Memory: an anonymous 4 MiB body sent at full speed is discarded, not buffered.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, _, err := rawUpload(t, addr, "/account/avatar", "", body, ct, nil)
	runtime.ReadMemStats(&after)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous full-speed: %v %v", err, resp)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 2<<20 {
		t.Fatalf("draining a %d-byte anonymous body allocated %d bytes", len(body), grew)
	}
}

// C3/C5: a slow upload of an admitted vendor outlasts the server's ReadTimeout and WriteTimeout (scaled down here
// with the upload windows) and still gets its complete 303.
func TestSlowUploadResponseArrives(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("slow_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	setUploadWindows(t, 8*time.Second, 10*time.Second)
	addr := uploadServer(t, e, 1*time.Second, 1500*time.Millisecond)
	photo := fixtureJPEG(t, fixturePicture(640, 480), 95)
	body, ct := multipartBody(e.photoParts(vendor, listing, photo)...)
	start := time.Now()
	// About 3 s: past both server timeouts.
	resp, _, err := rawUpload(t, addr, "/listings/photo", vendor, body, ct, trickle(len(body)/20+1, 150*time.Millisecond))
	if err != nil {
		t.Fatalf("slow upload: %v after %s", err, time.Since(start))
	}
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/listing-edit?id="+listing) || time.Since(start) < 2*time.Second {
		t.Fatalf("slow upload: %d %q after %s", resp.StatusCode, resp.Header.Get("Location"), time.Since(start))
	}
	if e.imageCount() != 1 {
		t.Fatal("not stored")
	}
}

func setUploadWindows(t *testing.T, read, write time.Duration) {
	oldRead, oldWrite := uploadReadWindow, uploadWriteWindow
	uploadReadWindow, uploadWriteWindow = read, write
	t.Cleanup(func() { uploadReadWindow, uploadWriteWindow = oldRead, oldWrite })
}

// A client slower than the upload window gets a page saying so (while it is still listening), and nothing is
// stored.
func TestUploadSlowClientTimeoutMessage(t *testing.T) {
	e, _ := uploadEnv(t)
	_, vendor := e.user("too_slow_vendor", "vendor")
	setUploadWindows(t, 1500*time.Millisecond, 10*time.Second)
	addr := uploadServer(t, e, 1*time.Second, 1500*time.Millisecond)
	body, ct := multipartBody(e.avatarParts(vendor, fixtureJPEG(t, fixturePicture(640, 480), 95))...)
	resp, page, err := rawUpload(t, addr, "/account/avatar", vendor, body, ct, trickle(512, 100*time.Millisecond))
	if err != nil {
		t.Fatalf("timed-out upload: %v", err)
	}
	if resp.StatusCode != 408 || !strings.Contains(page, "did not finish within 90 seconds") {
		t.Fatalf("timed-out upload: %d %s", resp.StatusCode, page)
	}
	if e.imageCount() != 0 {
		t.Fatal("stored")
	}
}

// C5: a client that goes away mid-body frees its account's upload lock at once.
func TestUploadLockReleasedOnClientAbort(t *testing.T) {
	e, _ := uploadEnv(t)
	_, vendor := e.user("abort_vendor", "vendor")
	addr := uploadServer(t, e, 15*time.Second, 20*time.Second)
	body, ct := multipartBody(e.avatarParts(vendor, fixtureJPEG(t, fixturePicture(640, 480), 95))...)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "POST /account/avatar HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nCookie: session=%s\r\n\r\n", addr, ct, len(body), vendor)
	c.Write(body[:len(body)/2])
	held := func() bool {
		e.A.mu.Lock()
		defer e.A.mu.Unlock()
		return len(e.A.inFlight) > 0
	}
	for deadline := time.Now().Add(5 * time.Second); !held(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the upload never took its account lock")
		}
	}
	c.Close()
	start := time.Now()
	for held() {
		if time.Since(start) > time.Second {
			t.Fatal("the account lock outlived the client by a second")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(uploadWork) != 0 || len(userPostWork) != 0 {
		t.Fatal("slots held after the client went away")
	}
	body, ct = multipartBody(e.avatarParts(vendor, smallJPEG(t))...)
	if resp, _, err := rawUpload(t, addr, "/account/avatar", vendor, body, ct, nil); err != nil || resp.StatusCode != 303 {
		t.Fatalf("next upload: %v %v", err, resp)
	}
}

// Admission: a flood of anonymous multipart posts trickling their bodies holds no post, lookup or upload slot,
// and a vendor's upload goes through meanwhile.
func TestUploadAnonFloodHoldsNoPostSlot(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("flood_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	addr := uploadServer(t, e, 15*time.Second, 20*time.Second)
	body, ct := multipartBody(e.avatarParts(randomToken(), make([]byte, 1<<20))...)
	n := cap(anonPostWork) + cap(userPostWork) + cap(uploadWork) + 8
	for i := range n {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		path := "/account/avatar"
		if i%2 == 1 {
			path = "/listings/photo"
		}
		fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nCookie: session=%s\r\n\r\n", path, addr, ct, len(body), randomToken())
		go trickle(256, 100*time.Millisecond)(c, body)
	}
	time.Sleep(500 * time.Millisecond)
	for name, sem := range map[string]chan struct{}{"anon post": anonPostWork, "user post": userPostWork, "lookup": sessionLookupWork, "upload": uploadWork} {
		if len(sem) != 0 {
			t.Fatalf("%d trickling anonymous uploads hold %d %s slots", n, len(sem), name)
		}
	}
	photo, pct := multipartBody(e.photoParts(vendor, listing, smallJPEG(t))...)
	if resp, _, err := rawUpload(t, addr, "/listings/photo", vendor, photo, pct, nil); err != nil || resp.StatusCode != 303 {
		t.Fatalf("vendor upload during the flood: %v %v", err, resp)
	}
}
