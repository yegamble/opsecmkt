package market

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	uploadTicketLimit    = 64
	uploadTicketLifetime = 3 * time.Minute
	uploadEnvelopeLimit  = 4 << 20 // bounded base64 JPEG pair plus text; never original bytes
	uploadResponseLimit  = 2 << 20
)

// UploadGatewayConfig configures a separate process which receives originals. It
// needs neither database access nor application, audit or wallet keys. TLSConfig
// authenticates this process to the application's private upload listener.
// AppURL must be a protected private app listener; never point it back at this gateway.
// The gateway sees browser sessions because it is the same-origin front door.
type UploadGatewayConfig struct {
	AppURL, BackendURL  string
	TLSConfig           *tls.Config
	AppTLSConfig        *tls.Config // optional application trust; nil uses system roots, never the backend identity
	ClamdAddress        string
	MaxSignatureAgeDays int
}

type uploadGateway struct {
	proxy   *httputil.ReverseProxy
	client  *http.Client
	backend string
	scanner *imageScanner
	work    chan struct{}
}

// NewUploadGateway builds the public same-origin front door. All image multipart
// parsing, header inspection, scanning, decoding and re-encoding stay here. Other
// requests are reverse proxied to the app without granting private admin ingress.
func NewUploadGateway(ctx context.Context, cfg UploadGatewayConfig) (http.Handler, error) {
	appURL, err := uploadServiceURL(cfg.AppURL, false)
	if err != nil {
		return nil, errors.New("upload gateway app URL must be an http(s) origin without credentials, path, query or fragment")
	}
	backendURL, err := uploadServiceURL(cfg.BackendURL, true)
	if err != nil {
		return nil, errors.New("upload gateway backend URL must be an https origin without credentials, path, query or fragment")
	}
	if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.RootCAs == nil || len(cfg.TLSConfig.Certificates) == 0 {
		return nil, errors.New("upload gateway requires a validated CA and client certificate for mutual TLS")
	}
	tc := cfg.TLSConfig.Clone()
	tc.MinVersion = max(tc.MinVersion, tls.VersionTLS13)
	tr := &http.Transport{TLSClientConfig: tc, DialContext: (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 30 * time.Second}
	appTLS := &tls.Config{MinVersion: tls.VersionTLS13}
	if cfg.AppTLSConfig != nil {
		if appURL.Scheme != "https" || cfg.AppTLSConfig.InsecureSkipVerify || len(cfg.AppTLSConfig.Certificates) != 0 || cfg.AppTLSConfig.GetClientCertificate != nil {
			return nil, errors.New("application proxy TLS requires an HTTPS origin, validated roots and no upload client identity")
		}
		appTLS = cfg.AppTLSConfig.Clone()
		appTLS.MinVersion = max(appTLS.MinVersion, tls.VersionTLS13)
	}
	proxyTransport := tr.Clone()
	proxyTransport.TLSClientConfig = appTLS
	sc, err := scannerConfigFromEnv(func(k string) string {
		switch k {
		case "CLAMD_ADDRESS":
			return cfg.ClamdAddress
		case "SCANNER_MAX_SIGNATURE_AGE_DAYS":
			if cfg.MaxSignatureAgeDays != 0 {
				return fmt.Sprint(cfg.MaxSignatureAgeDays)
			}
		}
		return ""
	})
	if err != nil {
		return nil, err
	}
	if sc.address == "" {
		return nil, errors.New("upload gateway requires CLAMD_ADDRESS; uploads cannot bypass scanning")
	}
	g := &uploadGateway{client: &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, backend: backendURL.String(), scanner: newImageScanner(sc), work: make(chan struct{}, 4)}
	g.proxy = &httputil.ReverseProxy{Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(appURL)
		p.Out.Host = p.In.Host // Origin checks use the browser's host, never forwarding headers
		p.Out.Header.Del("Forwarded")
		p.Out.Header.Del("X-Forwarded-For")
		p.Out.Header.Del("X-Forwarded-Host")
		p.Out.Header.Del("X-Forwarded-Proto")
	}, Transport: proxyTransport, ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
		gatewayRefusal(w, http.StatusServiceUnavailable, "The market is temporarily unavailable. Try again later.")
	}}
	go g.run(ctx) // polls clamd and reports its state to the app (gateway_scanner_report.go)
	go func() { <-ctx.Done(); tr.CloseIdleConnections(); proxyTransport.CloseIdleConnections() }()
	return g, nil
}

func uploadServiceURL(raw string, tlsOnly bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" || (u.Scheme != "https" && (tlsOnly || u.Scheme != "http")) {
		return nil, errors.New("invalid origin")
	}
	u.Path = ""
	return u, nil
}

func (g *uploadGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/uploads/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == brandingUploadPath && r.Method == http.MethodPost {
		g.serveBrandingUpload(w, r)
		return
	}
	route, isUpload := uploadRoutes[r.URL.Path]
	staff := r.URL.Path == staffUploadPath // A-551: the kind comes from the capability (staff_upload.go)
	if r.Method != http.MethodPost || !isUpload && !staff {
		if isMultipart(r) {
			gatewayRefusal(w, http.StatusUnsupportedMediaType, "This form cannot take files. Nothing was changed.")
			return
		}
		g.proxy.ServeHTTP(w, r)
		return
	}
	if len(r.TransferEncoding) > 0 || r.ContentLength < 0 || r.Header.Get("Content-Length") == "" {
		gatewayRefusal(w, http.StatusLengthRequired, "Photo not saved: the upload must include Content-Length. Nothing was stored.")
		return
	}
	if r.ContentLength > uploadCeiling {
		gatewayRefusal(w, http.StatusRequestEntityTooLarge, errImageTooLarge.Msg)
		return
	}
	select {
	case g.work <- struct{}{}:
		defer func() { <-g.work }()
	default:
		gatewayRefusal(w, http.StatusServiceUnavailable, "Photo uploads are busy. Try again shortly.")
		return
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(15 * time.Second)) // only admitted users receive the long Tor upload window
	rc.SetWriteDeadline(time.Now().Add(uploadWriteWindow))
	ctx, cancel := context.WithTimeout(r.Context(), uploadReadWindow+uploadWorkTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	var ticket string
	var refusal *gatewayReply
	u := &upload{r: r, route: route, ticketPart: staff}
	u.validateCSRF = func(csrf []byte) error {
		if staff {
			resp, err := g.call(ctx, "/v1/uploads/staff-admit", staffGatewayAdmission{Ticket: string(csrf)})
			if err != nil {
				return &httpError{Code: 503, Msg: "Photo not saved: the market is temporarily unavailable. Nothing was stored."}
			}
			if resp.status != http.StatusOK {
				refusal = &resp
				return errUploadCSRF
			}
			var admitted struct {
				Ticket string `json:"ticket"`
				Kind   string `json:"kind"`
			}
			if json.Unmarshal(resp.body, &admitted) != nil || len(admitted.Ticket) != 64 || staffUploadKinds[admitted.Kind] == "" {
				return &httpError{Code: 503, Msg: "Photo not saved: the market did not authorize this upload. Nothing was stored."}
			}
			ticket, u.route = admitted.Ticket, uploadRoutes[staffUploadKinds[admitted.Kind]]
			rc.SetReadDeadline(time.Now().Add(uploadReadWindow))
			return nil
		}
		session := ""
		if c, err := r.Cookie("session"); err == nil {
			session = c.Value
		}
		v := uploadAdmission{Path: r.URL.Path, Session: session, CSRF: string(csrf), Host: r.Host, Origin: r.Header.Get("Origin"), FetchSite: r.Header.Get("Sec-Fetch-Site")}
		resp, err := g.call(ctx, "/v1/uploads/admit", v)
		if err != nil {
			return &httpError{Code: 503, Msg: "Photo not saved: the market is temporarily unavailable. Nothing was stored."}
		}
		if resp.status != http.StatusOK {
			refusal = &resp
			return errUploadCSRF
		}
		var admitted struct {
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal(resp.body, &admitted) != nil || len(admitted.Ticket) != 64 {
			return &httpError{Code: 503, Msg: "Photo not saved: the market did not authorize this upload. Nothing was stored."}
		}
		ticket = admitted.Ticket
		rc.SetReadDeadline(time.Now().Add(uploadReadWindow))
		return nil
	}
	form, err := u.readForm()
	u.drain() // drain only on the isolated gateway, under its read deadline
	if refusal != nil {
		refusal.write(w)
		return
	}
	if ticket == "" {
		var he *httpError
		if !errors.As(err, &he) {
			he = errUploadForm
		}
		gatewayRefusal(w, he.Code, he.Msg)
		return
	}
	result := uploadFinish{Ticket: ticket, ID: form.id, Description: form.description}
	if err == nil {
		var info imageInfo
		info, err = inspectImage(form.photo)
		if err == nil {
			var clean scannedUpload
			clean, err = g.scanner.Scan(ctx, u.route.kind, form.photo)
			form.photo = nil
			if err == nil {
				var img encodedImage
				img, err = decodeInSlot(ctx, clean, info, u.route.kind)
				if err == nil {
					result.Full, result.Thumb, result.Width, result.Height = img.full, img.thumb, img.width, img.height
				}
			}
		}
	}
	if err != nil {
		var he *httpError
		var sr *scanRefusal
		switch {
		case errors.As(err, &he):
			result.Status, result.Message = he.Code, he.Msg
		case errors.As(err, &sr):
			result.Status, result.Message, result.ScanClass, result.Ref = sr.Status(), sr.Message(""), sr.Class, sr.Ref
		default:
			result.Status, result.Message = 503, "Photo not saved: picture processing did not finish. Nothing was stored. Try again later."
		}
	}
	// Once admitted, report even a cancelled browser's outcome to release its
	// bounded ticket. Never retry an ambiguous commit response.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer finishCancel()
	resp, callErr := g.call(finishCtx, "/v1/uploads/finish", result)
	if callErr != nil {
		gatewayRefusal(w, http.StatusBadGateway, "Photo status unknown: reload the listing or account to check before uploading again.")
		return
	}
	resp.write(w)
}

type gatewayReply struct {
	status int
	header http.Header
	body   []byte
}

func (v gatewayReply) write(w http.ResponseWriter) {
	for _, key := range []string{"Content-Type", "Location", "Set-Cookie", "Retry-After", "Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options", "Permissions-Policy", "Strict-Transport-Security"} {
		for _, value := range v.header.Values(key) {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(v.status)
	w.Write(v.body)
}
func (g *uploadGateway) call(ctx context.Context, path string, v any) (gatewayReply, error) {
	b, err := json.Marshal(v)
	if err != nil || len(b) > uploadEnvelopeLimit {
		return gatewayReply{}, errors.New("upload request exceeds service limit")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", g.backend+path, bytes.NewReader(b))
	if err != nil {
		return gatewayReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return gatewayReply{}, errors.New("upload service unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, uploadResponseLimit+1))
	if err != nil || len(data) > uploadResponseLimit {
		return gatewayReply{}, errors.New("invalid upload service response")
	}
	return gatewayReply{status: resp.StatusCode, header: resp.Header, body: data}, nil
}
func gatewayRefusal(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Connection", "close")
	if status == 503 {
		w.Header().Set("Retry-After", busyRetryAfter)
	}
	w.WriteHeader(status)
	fmt.Fprint(w, "<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Photo not saved</title><h1>Photo not saved</h1><p>"+html.EscapeString(message)+"</p><p><a href=\"/account\">Back to your account</a></p></html>")
}

type uploadAdmission struct{ Path, Session, CSRF, Host, Origin, FetchSite string }
type uploadFinish struct {
	Ticket, ID, Description string
	Full, Thumb             []byte
	Width, Height           int
	Status                  int
	Message                 string
	ScanClass               scanClass
	Ref                     string
}
type uploadTicket struct {
	admission  uploadAdmission
	actor      *User
	expires    time.Time
	release    func()
	timer      *time.Timer
	capability bool   // admitted with a staff upload capability (staff_upload.go): answered with standalone pages
	listing    string // a capability photo's bound listing; the gateway names none
}
type uploadTicketStore struct {
	mu      sync.Mutex
	pending map[string]uploadTicket
}

func (s *uploadTicketStore) prune(now time.Time) {
	for key, ticket := range s.pending {
		if !now.Before(ticket.expires) {
			delete(s.pending, key)
			if ticket.timer != nil {
				ticket.timer.Stop()
			}
			ticket.release()
		}
	}
}
func (s *uploadTicketStore) put(v uploadTicket) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	if len(s.pending) >= uploadTicketLimit {
		return "", false
	}
	if s.pending == nil {
		s.pending = map[string]uploadTicket{}
	}
	key := randomToken()
	// Expiry releases the per-account slot even when the gateway dies and no
	// later request arrives to prune the map. At most uploadTicketLimit timers.
	v.timer = time.AfterFunc(time.Until(v.expires), func() { s.mu.Lock(); defer s.mu.Unlock(); s.prune(time.Now()) })
	s.pending[key] = v
	return key, true
}
func (s *uploadTicketStore) take(key string) (uploadTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	v, ok := s.pending[key]
	if ok {
		delete(s.pending, key)
		if v.timer != nil {
			v.timer.Stop()
		}
	}
	return v, ok
}

// UploadBackendHandler must be bound on a separate tls.RequireAndVerifyClientCert
// listener. Its own certificate check remains mandatory: public Host/headers can
// never select this handler or impersonate the gateway.
func (a *App) UploadBackendHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.responseSecurityHeaders(w)
		if !a.isolatedUploads || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.NotFound(w, r)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		base := context.WithValue(r.Context(), serverErrorRingKey{}, &a.recentErrors)
		ctx, cancel := context.WithTimeout(base, uploadWorkTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		switch r.URL.Path {
		case "/v1/uploads/admit":
			a.admitGatewayUpload(w, r)
		case "/v1/uploads/finish":
			a.finishGatewayUpload(w, r)
		case "/v1/uploads/branding-admit":
			a.admitGatewayBranding(w, r)
		case "/v1/uploads/branding-finish":
			a.finishGatewayBranding(w, r)
		case "/v1/uploads/staff-admit":
			a.admitGatewayStaffUpload(w, r)
		case gatewayScannerReportPath:
			a.receiveGatewayScannerReport(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
func readUploadJSON(r *http.Request, limit int64, v any) error {
	if r.Header.Get("Content-Type") != "application/json" {
		return errors.New("invalid content type")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return errors.New("invalid upload service request")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid upload service request")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("invalid upload service request")
	}
	return nil
}
func uploadBrowserRequest(ctx context.Context, v uploadAdmission) *http.Request {
	r := &http.Request{Method: "POST", URL: &url.URL{Path: v.Path}, Host: v.Host, Header: make(http.Header)}
	r.Header.Set("Origin", v.Origin)
	r.Header.Set("Sec-Fetch-Site", v.FetchSite)
	r.AddCookie(&http.Cookie{Name: "session", Value: v.Session})
	return r.WithContext(context.WithValue(ctx, sessionKey{}, v.Session))
}
func (a *App) admitGatewayUpload(w http.ResponseWriter, r *http.Request) {
	var v uploadAdmission
	if readUploadJSON(r, 8<<10, &v) != nil {
		http.Error(w, "Invalid upload authorization", 400)
		return
	}
	route, ok := uploadRoutes[v.Path]
	if !ok || len(v.Host) > 512 || len(v.Origin) > 1024 || len(v.FetchSite) > 32 {
		http.Error(w, "Invalid upload authorization", 400)
		return
	}
	browser := uploadBrowserRequest(r.Context(), v)
	u := &upload{a: a, w: w, r: browser, route: route, token: v.Session, back: "/account#avatar"}
	if route.kind == uploadListing {
		u.back = "/vendor-dashboard"
	}
	if msg := postOriginError(browser, a.secure); msg != "" {
		u.render(403, msg)
		return
	}
	if len(v.Session) != 64 || len(v.CSRF) != 64 || subtle.ConstantTimeCompare([]byte(v.CSRF), []byte(a.csrf(v.Session))) != 1 {
		u.render(errUploadCSRF.Code, errUploadCSRF.Msg)
		return
	}
	releaseLookup, ok := takeLookupSlot(r.Context())
	if !ok {
		u.render(503, errBusyMsg)
		return
	}
	actor, err := a.sessionUser(r.Context(), v.Session)
	releaseLookup()
	if err != nil {
		u.serverError(503, errorCause(err))
		return
	}
	u.user = actor
	if a.publicAdministrator(browser.Context(), actor) {
		http.NotFound(w, browser)
		return
	}
	if actor == nil {
		u.render(errUploadSignedOut.Code, errUploadSignedOut.Msg)
		return
	}
	if actor.RetentionRestricted && route.kind != uploadEvidence {
		u.render(403, retentionRestrictedMessage)
		return
	}
	if !slices.Contains(route.roles, actor.Role) {
		u.render(403, "Photo not saved: your account cannot upload this picture. Nothing was stored.")
		return
	}
	if !a.allowWrite(actor.ID) || !a.allowWriteBudget("upload:"+actor.ID, uploadBudget) {
		u.render(429, "Photo not saved: too many uploads. Try again later. Nothing was stored.")
		return
	}
	release, ok := a.enterOneAtATime("upload", actor.ID)
	if !ok {
		u.render(429, "Photo not saved: another upload from your account is in progress. Nothing was stored.")
		return
	}
	ticket, ok := a.uploadTickets.put(uploadTicket{admission: v, actor: actor, expires: time.Now().Add(uploadTicketLifetime), release: release})
	if !ok {
		release()
		u.render(503, errBusyMsg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"ticket": ticket})
}
func (a *App) finishGatewayUpload(w http.ResponseWriter, r *http.Request) {
	var v uploadFinish
	if readUploadJSON(r, uploadEnvelopeLimit, &v) != nil {
		http.Error(w, "Invalid normalized upload", 400)
		return
	}
	ticket, ok := a.uploadTickets.take(v.Ticket)
	if !ok {
		http.Error(w, "Upload authorization expired or was already used", 409)
		return
	}
	defer ticket.release()
	browser := uploadBrowserRequest(r.Context(), ticket.admission)
	route := uploadRoutes[ticket.admission.Path]
	u := &upload{a: a, w: w, r: browser, route: route, token: ticket.admission.Session, user: ticket.actor, back: "/account#avatar", capability: ticket.capability}
	if ticket.capability {
		// A staff capability names its listing (staff_upload.go); the gateway names none.
		if v.ID != "" {
			http.Error(w, "Invalid normalized upload", 400)
			return
		}
		v.ID = ticket.listing
	}
	if route.kind == uploadListing {
		u.back = "/listing-edit?id=" + url.QueryEscape(v.ID) + "#photos"
	} else if route.kind == uploadEvidence {
		u.back = "/order?id=" + url.QueryEscape(v.ID) + "#dispute-evidence"
	}
	if v.Status != 0 {
		if !slices.Contains([]int{400, 403, 408, 413, 415, 422, 503}, v.Status) || len(v.Message) > 2048 || !utf8.ValidString(v.Message) {
			http.Error(w, "Invalid upload refusal", 400)
			return
		}
		if v.ScanClass == scanInfected {
			if ref, err := hex.DecodeString(v.Ref); err != nil || len(ref) != 4 || v.Status != http.StatusUnprocessableEntity {
				http.Error(w, "Invalid upload refusal", http.StatusBadRequest)
				return
			}
			if err := a.auditScanRefusal(r.Context(), ticket.actor.ID, &scanRefusal{Class: scanInfected}); err != nil {
				http5xxLog.Printf("upload audit failed cause=%s", errorCause(err))
			}
			contact, err := staffContactRefusal(r.Context(), a.db)
			if err != nil {
				http5xxLog.Printf("upload staff contact read failed cause=%s", errorCause(err))
			}
			v.Message = (&scanRefusal{Class: scanInfected, Ref: v.Ref}).Message(contact)
		}
		if v.Status == 503 {
			w.Header().Set("Retry-After", busyRetryAfter)
		}
		u.render(v.Status, v.Message)
		return
	}
	if !formTextValid(url.Values{"id": {v.ID}, "description": {v.Description}}) || len(v.ID) > int(idPartLimit) || utf8.RuneCountInString(v.Description) > descriptionLimit || !validGatewayJPEGs(v, route.kind) {
		u.render(422, "Photo not saved: the upload service returned an invalid normalized picture. Nothing was stored.")
		return
	}
	release, ok := tryTake(userPostWork)
	if !ok {
		u.render(503, errBusyMsg)
		return
	}
	defer release()
	img := encodedImage{full: v.Full, thumb: v.Thumb, width: v.Width, height: v.Height}
	redirect, err := a.storeImage(r.Context(), browser, ticket.admission.Session, ticket.actor, route, uploadForm{id: v.ID, description: v.Description}, img)
	var he *httpError
	switch {
	case err == nil && ticket.capability:
		title, msg := "Photo saved", "The photo is added to the listing. Reload the listing on the private administrator connection to see it."
		if route.kind == uploadAvatar {
			title, msg = "Avatar saved", "Your new avatar is saved. Reload your account on the private administrator connection to see it."
		} else if route.kind == uploadEvidence {
			title, msg = "Evidence saved", "Dispute image evidence is saved. Reload the order on the private administrator connection to see it."
		}
		brandingResultPage(w, http.StatusOK, title, msg)
	case err == nil:
		notice := "photo-saved"
		if route.kind == uploadAvatar {
			notice = "avatar-saved"
		} else if route.kind == uploadEvidence {
			notice = "evidence-saved"
		}
		http.Redirect(w, browser, a.setNotice(w, notice, redirect), http.StatusSeeOther)
	case errors.As(err, &he):
		u.render(he.Code, he.Msg)
	case isUploadCommitUnknown(err):
		u.serverErrorMessage(500, errorCause(err), "Photo may have been saved: reload the listing or account to check before uploading again.", "Photo status unknown")
	case dbBusy(err):
		u.serverError(503, errorCause(err))
	default:
		u.serverError(500, errorCause(err))
	}
}

// Validate the worker's narrow JPEG envelope without invoking a decoder in the
// app process. The trusted gateway's encoder emits baseline JPEG with one scan,
// no APP/COM metadata, no embedded image and exactly one final EOI marker.
func validGatewayJPEGs(v uploadFinish, kind uploadKind) bool {
	if len(v.Full) == 0 || len(v.Full) > maxFullBytes || len(v.Thumb) == 0 || len(v.Thumb) > maxThumbBytes {
		return false
	}
	fw, fh, ok := normalizedJPEGDimensions(v.Full)
	if !ok || fw != v.Width || fh != v.Height {
		return false
	}
	tw, th, ok := normalizedJPEGDimensions(v.Thumb)
	if !ok {
		return false
	}
	if kind == uploadAvatar {
		return fw == fh && fw <= avatarSide && tw == th && tw == min(fw, avatarThumbSide)
	}
	return fw <= photoLongEdge && fh <= photoLongEdge && tw == thumbWidth && th == thumbHeight
}
func normalizedJPEGDimensions(b []byte) (w, h int, ok bool) {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return
	}
	seenFrame := false
	for i, n := 2, 0; n < 32 && i+4 <= len(b); n++ {
		if b[i] != 0xff {
			return 0, 0, false
		}
		marker := b[i+1]
		length := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if length < 2 || i+2+length > len(b) {
			return 0, 0, false
		}
		segment := b[i+4 : i+2+length]
		switch marker {
		case 0xc0:
			if seenFrame || len(segment) != 15 || segment[0] != 8 || segment[5] != 3 {
				return 0, 0, false
			}
			seenFrame = true
			h = int(binary.BigEndian.Uint16(segment[1:3]))
			w = int(binary.BigEndian.Uint16(segment[3:5]))
			if w < 1 || h < 1 {
				return 0, 0, false
			}
		case 0xdb, 0xc4: // quantization and Huffman tables only
		case 0xda:
			if !seenFrame || len(segment) != 10 || segment[0] != 3 {
				return 0, 0, false
			}
			for j := i + 2 + length; j < len(b); j++ {
				if b[j] != 0xff {
					continue
				}
				if j+1 >= len(b) {
					return 0, 0, false
				}
				j++
				if b[j] == 0 {
					continue
				}
				if b[j] == 0xd9 && j == len(b)-1 {
					return w, h, true
				}
				return 0, 0, false
			}
			return 0, 0, false
		default:
			return 0, 0, false
		}
		i += 2 + length
	}
	return 0, 0, false
}
