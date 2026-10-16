package market

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func uploadTestPKI(t *testing.T) (tls.Certificate, *tls.Config, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "upload-test-root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	leaf := func(n int64, usage x509.ExtKeyUsage) tls.Certificate {
		lp, lk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c := &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: "upload-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		b, err := x509.CreateCertificate(rand.Reader, c, ca, lp, key)
		if err != nil {
			t.Fatal(err)
		}
		kb, err := x509.MarshalPKCS8PrivateKey(lk)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	server := leaf(2, x509.ExtKeyUsageServerAuth)
	return server, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{leaf(3, x509.ExtKeyUsageClientAuth)}, MinVersion: tls.VersionTLS13}, roots
}
func uploadTestBackend(t *testing.T, h http.Handler) (*httptest.Server, *tls.Config) {
	t.Helper()
	cert, client, roots := uploadTestPKI(t)
	s := httptest.NewUnstartedServer(h)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s, client
}
func uploadTestGateway(t *testing.T, e *testEnv, f *fakeClamd) *uploadGateway {
	t.Helper()
	e.A.isolatedUploads = true
	app := httptest.NewServer(e.A)
	t.Cleanup(app.Close)
	backend, tc := uploadTestBackend(t, e.A.UploadBackendHandler())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := NewUploadGateway(ctx, UploadGatewayConfig{AppURL: app.URL, BackendURL: backend.URL, TLSConfig: tc, ClamdAddress: f.Addr})
	if err != nil {
		t.Fatal(err)
	}
	g := h.(*uploadGateway)
	g.scanner.poll(context.Background())
	return g
}
func uploadThroughGateway(g *uploadGateway, path, session string, parts ...part) *httptest.ResponseRecorder {
	r, _ := uploadReq(path, session, parts...)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestIsolatedUploadGatewayRoundTrip(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	owner, session := e.user("isolated_vendor", "vendor")
	listing := e.product(owner, "physical")
	original := metadataJPEG(t, 400, 300)
	w := uploadThroughGateway(g, "/listings/photo", session, e.photoParts(session, listing, original)...)
	if w.Code != 303 || w.Header().Get("Location") != "/listing-edit?id="+listing+"#photos" {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("upload response lost browser security headers")
	}
	var full, thumb []byte
	var width, height int
	if err := e.DB.QueryRow(`SELECT "full",thumb,width,height FROM images`).Scan(&full, &thumb, &width, &height); err != nil {
		t.Fatal(err)
	}
	assertCleanJPEG(t, "gateway full", full)
	assertCleanJPEG(t, "gateway thumb", thumb)
	if width != 400 || height != 300 || !bytes.Equal(f.Scans()[0], original) || scansOf(f) != 1 {
		t.Fatalf("dimensions/scans: %dx%d scans=%d", width, height, scansOf(f))
	}
	if e.A.scanner != nil && e.A.scanner.State().Name != scannerDisabled {
		t.Fatal("main app scanner must remain unused")
	}
	w = uploadThroughGateway(g, "/account/avatar", session, e.avatarParts(session, fixturePNG(t, fixturePicture(200, 300)))...)
	if w.Code != 303 || w.Header().Get("Location") != "/account#avatar" {
		t.Fatalf("avatar: %d %s", w.Code, w.Body.String())
	}
}
func TestIsolatedUploadGatewayAuthorizationAndFailures(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	owner, session := e.user("isolated_denial", "vendor")
	listing := e.product(owner, "physical")
	r, body := uploadReq("/listings/photo", session, e.photoParts(session, listing, smallJPEG(t))...)
	r.Header.Set("X-Upload-Authenticated", "true")
	r.Header.Set("X-Forwarded-Host", "private")
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	if w.Code != 415 || body.read() != 0 || e.imageCount() != 0 {
		t.Fatalf("direct raw request: status=%d body=%d", w.Code, body.read())
	}
	bad := e.photoParts(session, listing, smallJPEG(t))
	bad[0].body = []byte(strings.Repeat("0", 64))
	w = uploadThroughGateway(g, "/listings/photo", session, bad...)
	if w.Code != 403 || scansOf(f) != 0 {
		t.Fatalf("CSRF: %d scans %d", w.Code, scansOf(f))
	}
	_, buyer := e.user("isolated_buyer", "buyer")
	w = uploadThroughGateway(g, "/listings/photo", buyer, e.photoParts(buyer, listing, smallJPEG(t))...)
	if w.Code != 403 || scansOf(f) != 0 {
		t.Fatalf("buyer: %d scans %d", w.Code, scansOf(f))
	}
	otherID, other := e.user("isolated_other", "vendor")
	_ = otherID
	w = uploadThroughGateway(g, "/listings/photo", other, e.photoParts(other, listing, smallJPEG(t))...)
	if w.Code != 404 || e.imageCount() != 0 {
		t.Fatalf("ownership: %d %s", w.Code, w.Body.String())
	}
	infected := append(smallJPEG(t), []byte(fakeClamdMarker)...)
	w = uploadThroughGateway(g, "/listings/photo", session, e.photoParts(session, listing, infected)...)
	if w.Code != 422 || e.imageCount() != 0 {
		t.Fatalf("infected: %d %s", w.Code, w.Body.String())
	}
	var audits int
	if err := e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", owner, auditUploadRefusedInfected).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("infected audit count=%d err=%v", audits, err)
	}
	before := scansOf(f)
	f.SetVersion(fakeClamdVersion(40 * 24 * time.Hour))
	g.scanner.poll(context.Background())
	w = uploadThroughGateway(g, "/listings/photo", session, e.photoParts(session, listing, smallJPEG(t))...)
	if w.Code != 503 || scansOf(f) != before || e.imageCount() != 0 {
		t.Fatalf("stale: %d scans %d", w.Code, scansOf(f))
	}
	f.SetVersion(fakeClamdVersion(24 * time.Hour))
	g.scanner.poll(context.Background())
	f.SetVerdict(func([]byte) string {
		e.DB.Exec("DELETE FROM sessions WHERE token_hash=$1", digest(session))
		return "stream: OK"
	})
	w = uploadThroughGateway(g, "/listings/photo", session, e.photoParts(session, listing, smallJPEG(t))...)
	if w.Code != 401 || e.imageCount() != 0 {
		t.Fatalf("revoked while scanning: %d %s", w.Code, w.Body.String())
	}
}
func TestIsolatedUploadGatewayTicketAndEnvelope(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	_, session := e.user("isolated_ticket", "vendor")
	admit := func() string {
		resp, err := g.call(context.Background(), "/v1/uploads/admit", uploadAdmission{Path: "/account/avatar", Session: session, CSRF: e.A.csrf(session), Host: "example.com"})
		if err != nil || resp.status != 200 {
			t.Fatalf("admit %v %d %s", err, resp.status, resp.body)
		}
		var v struct{ Ticket string }
		if json.Unmarshal(resp.body, &v) != nil || len(v.Ticket) != 64 {
			t.Fatal("invalid ticket")
		}
		return v.Ticket
	}
	ticket := admit()
	second, err := g.call(context.Background(), "/v1/uploads/admit", uploadAdmission{Path: "/account/avatar", Session: session, CSRF: e.A.csrf(session), Host: "example.com"})
	if err != nil || second.status != 429 {
		t.Fatalf("concurrent admission: %d %v", second.status, err)
	}
	result := uploadFinish{Ticket: ticket, Full: smallJPEG(t), Thumb: smallJPEG(t), Width: 200, Height: 150}
	resp, err := g.call(context.Background(), "/v1/uploads/finish", result)
	if err != nil || resp.status != 422 || e.imageCount() != 0 {
		t.Fatalf("bad avatar envelope: %d %v", resp.status, err)
	}
	resp, err = g.call(context.Background(), "/v1/uploads/finish", result)
	if err != nil || resp.status != 409 {
		t.Fatalf("replayed ticket: %d %v", resp.status, err)
	}
	result.Ticket = admit()
	result.Full = append(result.Full, []byte("trailing-original")...)
	resp, err = g.call(context.Background(), "/v1/uploads/finish", result)
	if err != nil || resp.status != 422 || e.imageCount() != 0 {
		t.Fatalf("trailing bytes: %d %v", resp.status, err)
	}
}
func TestIsolatedUploadGatewayMutualTLS(t *testing.T) {
	a := &App{isolatedUploads: true}
	s, tc := uploadTestBackend(t, a.UploadBackendHandler())
	for _, name := range []string{"missing-client", "untrusted-ca", "wrong-host", "wrong-role"} {
		cfg := tc.Clone()
		switch name {
		case "missing-client":
			cfg.Certificates = nil
		case "untrusted-ca":
			cfg.RootCAs = x509.NewCertPool()
		case "wrong-host":
			cfg.ServerName = "different.example"
		case "wrong-role":
			cfg.Certificates = s.TLS.Certificates
		}
		tr := &http.Transport{TLSClientConfig: cfg}
		client := &http.Client{Transport: tr, Timeout: time.Second}
		resp, err := client.Post(s.URL+"/v1/uploads/admit", "application/json", strings.NewReader("{}"))
		tr.CloseIdleConnections()
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s accepted without valid mutual TLS", name)
		}
	}
	for _, tlsState := range []*tls.ConnectionState{nil, {PeerCertificates: []*x509.Certificate{{}}}} {
		r := httptest.NewRequest("POST", "/v1/uploads/admit", strings.NewReader("{}"))
		r.TLS = tlsState
		r.Header.Set("X-Upload-Authenticated", "true")
		w := httptest.NewRecorder()
		a.UploadBackendHandler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("unverified handler access: %d", w.Code)
		}
	}
}
func TestUploadGatewayNormalizedJPEGValidation(t *testing.T) {
	photo := fixtureJPEG(t, fixturePicture(200, 150), 82)
	w, h, ok := normalizedJPEGDimensions(photo)
	if !ok || w != 200 || h != 150 {
		t.Fatalf("encoder output rejected: %dx%d %v", w, h, ok)
	}
	for name, data := range map[string][]byte{"metadata": metadataJPEG(t, 200, 150), "trailing": append(append([]byte(nil), photo...), 1), "png": fixturePNG(t, fixturePicture(10, 10)), "truncated": photo[:len(photo)-2], "embedded": append(append([]byte(nil), photo...), photo...)} {
		if _, _, ok := normalizedJPEGDimensions(data); ok {
			t.Errorf("%s accepted", name)
		}
	}
}
func TestUploadGatewayBoundedExpiringTickets(t *testing.T) {
	var released atomic.Int32
	s := uploadTicketStore{}
	for i := 0; i < uploadTicketLimit; i++ {
		if _, ok := s.put(uploadTicket{expires: time.Now().Add(time.Hour), release: func() { released.Add(1) }}); !ok {
			t.Fatal("capacity refused early")
		}
	}
	if _, ok := s.put(uploadTicket{expires: time.Now().Add(time.Hour), release: func() {}}); ok {
		t.Fatal("ticket cap exceeded")
	}
	s.mu.Lock()
	for key, v := range s.pending {
		v.expires = time.Now().Add(-time.Second)
		s.pending[key] = v
	}
	s.mu.Unlock()
	if _, ok := s.take("unknown"); ok || released.Load() != uploadTicketLimit {
		t.Fatalf("expiry released %d", released.Load())
	}
}
func TestUploadGatewayRejectsUnsafeConfigurationAndMultipartProxy(t *testing.T) {
	cert, tc, _ := uploadTestPKI(t)
	_ = cert
	base := UploadGatewayConfig{AppURL: "http://127.0.0.1:1234", BackendURL: "https://127.0.0.1:1235", TLSConfig: tc, ClamdAddress: "127.0.0.1:3310"}
	for _, change := range []func(*UploadGatewayConfig){func(c *UploadGatewayConfig) { c.BackendURL = "http://127.0.0.1:1235" }, func(c *UploadGatewayConfig) { c.AppURL = "http://user:secret@example.com" }, func(c *UploadGatewayConfig) { c.BackendURL = "https://example.com/path" }, func(c *UploadGatewayConfig) { c.TLSConfig = nil }, func(c *UploadGatewayConfig) { c.ClamdAddress = "" }, func(c *UploadGatewayConfig) { c.TLSConfig = c.TLSConfig.Clone(); c.TLSConfig.InsecureSkipVerify = true }} {
		cfg := base
		change(&cfg)
		if _, err := NewUploadGateway(context.Background(), cfg); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}

// Keep the multipart boundary assertion separate from the database journey.
func TestUploadGatewayNeverProxiesOtherMultipart(t *testing.T) {
	g := &uploadGateway{}
	r := httptest.NewRequest("POST", "/login", strings.NewReader("raw-original"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("other multipart: %d", w.Code)
	}
	if b, err := io.ReadAll(r.Body); err != nil || string(b) != "raw-original" {
		t.Fatalf("raw bytes read: %q %v", b, err)
	}
}

func TestIsolatedUploadGatewayKeepsAdminIngressAndStaffPrivacy(t *testing.T) {
	e := newTestApp(t)
	f := newFakeClamd(t)
	g := uploadTestGateway(t, e, f)
	_, admin := e.user("isolated_private_admin", "admin")
	e.A.adminSocket = "/private/upload-test-admin.sock"
	r, _ := uploadReq("/account/avatar", admin, e.avatarParts(admin, smallJPEG(t))...)
	r.Host = "private-admin.onion"
	r.Header.Set("Host", "private-admin.onion")
	r.Header.Set("X-Admin-Authenticated", "true")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 404 || scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatalf("spoofed private admin: %d scans=%d", w.Code, scansOf(f))
	}
	id, moderator := e.user("isolated_demoted_staff", "moderator")
	f.SetVerdict(func([]byte) string {
		if _, err := e.DB.Exec("UPDATE users SET role='vendor' WHERE id=$1", id); err != nil {
			t.Error(err)
		}
		return "stream: OK"
	})
	w = uploadThroughGateway(g, "/account/avatar", moderator, e.avatarParts(moderator, smallJPEG(t))...)
	if w.Code != 403 || e.imageCount() != 0 {
		t.Fatalf("staff demotion during scan: %d %s", w.Code, w.Body.String())
	}
}

func TestIsolatedUploadModeStartsWithoutAppScanner(t *testing.T) {
	t.Setenv("UPLOAD_MODE", "isolated")
	e := newTestApp(t)
	if !e.A.isolatedUploads || e.A.scanner != nil {
		t.Fatal("isolated application must not have a scanner connection")
	}
	_, admin := e.user("isolated_scanner_admin", "admin")
	w := e.do("GET", "/admin", admin, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Separate upload gateway") || strings.Contains(w.Body.String(), "Photo uploads are off:") {
		t.Fatalf("isolated admin scanner state: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadGatewaySeparatesAppAndBackendTLSAuthority(t *testing.T) {
	backend, backendTLS := uploadTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("backend lost mutual TLS identity")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ticket":"verified-backend"}`))
	}))
	appCert, _, appRoots := uploadTestPKI(t)
	var sawClientIdentity atomic.Bool
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			sawClientIdentity.Store(true)
		}
		if r.Host != "public-market.example" {
			t.Errorf("proxy changed browser host: %q", r.Host)
		}
		w.Write([]byte("application reached"))
	}))
	app.TLS = &tls.Config{Certificates: []tls.Certificate{appCert}, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}
	app.StartTLS()
	t.Cleanup(app.Close)
	f := newFakeClamd(t)
	cfg := UploadGatewayConfig{AppURL: app.URL, BackendURL: backend.URL, TLSConfig: backendTLS, AppTLSConfig: &tls.Config{RootCAs: appRoots}, ClamdAddress: f.Addr}
	newGateway := func(c UploadGatewayConfig) *uploadGateway {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		h, err := NewUploadGateway(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return h.(*uploadGateway)
	}
	request := func(g *uploadGateway) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://public-market.example/", nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	g := newGateway(cfg)
	if w := request(g); w.Code != 200 || w.Body.String() != "application reached" {
		t.Fatalf("distinct application CA failed: %d %s", w.Code, w.Body.String())
	}
	if sawClientIdentity.Load() {
		t.Fatal("upload client identity leaked to ordinary app proxy")
	}
	resp, err := g.call(context.Background(), "/v1/uploads/admit", map[string]string{})
	if err != nil || resp.status != 200 || !strings.Contains(string(resp.body), "verified-backend") {
		t.Fatalf("dedicated backend mTLS failed: %v %d %s", err, resp.status, resp.body)
	}
	defaults := cfg
	defaults.AppTLSConfig = nil
	defaultGateway := newGateway(defaults)
	defaultTLS := defaultGateway.proxy.Transport.(*http.Transport).TLSClientConfig
	if defaultTLS.RootCAs != nil || len(defaultTLS.Certificates) != 0 || defaultTLS.GetClientCertificate != nil {
		t.Fatal("default application proxy inherited upload trust or credentials")
	}
	wrong := cfg
	wrong.AppTLSConfig = &tls.Config{RootCAs: backendTLS.RootCAs}
	if w := request(newGateway(wrong)); w.Code != 503 || strings.Contains(w.Body.String(), app.URL) || strings.Contains(w.Body.String(), "x509") {
		t.Fatalf("wrong application CA accepted or endpoint leaked: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []*tls.Config{{InsecureSkipVerify: true}, {Certificates: backendTLS.Certificates}} {
		invalid := cfg
		invalid.AppTLSConfig = bad
		if _, err := NewUploadGateway(context.Background(), invalid); err == nil {
			t.Fatal("unsafe application TLS configuration accepted")
		}
	}
}

func TestIsolatedUploadGatewayErrorsReachSafeAdminDiagnostics(t *testing.T) {
	for _, phase := range []string{"preflight", "store"} {
		t.Run(phase, func(t *testing.T) {
			e := newTestApp(t)
			f := newFakeClamd(t)
			g := uploadTestGateway(t, e, f)
			owner, session := e.user("private_upload_diagnostic", "vendor")
			listing := e.product(owner, "physical")
			wantStatus := 500
			if phase == "preflight" {
				original := e.A.db
				bad, err := OpenDB("postgres://synthetic@127.0.0.1:1/unavailable?sslmode=disable&connect_timeout=1")
				if err != nil {
					t.Fatal(err)
				}
				e.A.db = bad
				t.Cleanup(func() { e.A.db = original; bad.Close() })
				wantStatus = 503
			} else {
				f.SetVerdict(func([]byte) string {
					if _, err := e.DB.Exec("ALTER TABLE images RENAME TO images_unavailable"); err != nil {
						t.Error(err)
					}
					return "stream: OK"
				})
			}
			parts := e.photoParts(session, listing, metadataJPEG(t, 100, 100))
			parts = append(parts[:2], append([]part{{name: "description", body: []byte("private picture description")}}, parts[2:]...)...)
			r, _ := uploadReq("/listings/photo?private-query=synthetic-secret", session, parts...)
			r.Header.Set("User-Agent", "private diagnostic agent")
			r.RemoteAddr = "192.0.2.25:1234"
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != wantStatus {
				t.Fatalf("%s: status=%d body=%s", phase, w.Code, w.Body.String())
			}
			entries := e.A.recentErrors.snapshot()
			if len(entries) != 1 || entries[0].Kind != "action" || entries[0].Route != "/listings/photo" || entries[0].Status != wantStatus || len(entries[0].Reference) != 8 || !strings.Contains(w.Body.String(), "Reference: "+entries[0].Reference) {
				t.Fatalf("missing diagnostic: %#v body=%s", entries, w.Body.String())
			}
			encoded, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{owner, session, listing, "private_upload_diagnostic", "private picture description", "private-query", "synthetic-secret", "private diagnostic agent", "192.0.2.25", "FIXTURE"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("diagnostic retained private request data: %q", private)
				}
			}
		})
	}
}
