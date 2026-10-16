package market

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"time"
	"unicode/utf8"
)

// readBrandingForm admits a purpose-scoped ticket before reading file bytes.
// It accepts exactly ticket, logo, EOF; no cookie, password or administrator
// authentication code belongs in this public-gateway form.
func readBrandingForm(r *http.Request, admit func(string) error) (string, []byte, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return "", nil, errUploadForm
	}
	mr := multipart.NewReader(io.LimitReader(r.Body, uploadBodyLimit), params["boundary"])
	p, err := mr.NextRawPart()
	if err != nil || p.FormName() != "ticket" || p.FileName() != "" {
		return "", nil, errBrandingTicket
	}
	token, err := readSmallPart(p, 64)
	if err != nil || !brandingTokenPattern.Match(token) {
		return "", nil, errBrandingTicket
	}
	if err = admit(string(token)); err != nil {
		return "", nil, err
	}
	ticket := string(token)
	p, err = mr.NextRawPart()
	if err != nil || p.FormName() != "logo" {
		return ticket, nil, errUploadForm
	}
	data, err := io.ReadAll(io.LimitReader(p, uploadCap+1))
	if err != nil {
		return ticket, nil, errUploadForm
	}
	if len(data) > uploadCap {
		return ticket, nil, errImageTooLarge
	}
	if len(data) == 0 {
		return ticket, nil, errBrandingSVG
	}
	if _, err = mr.NextRawPart(); err != io.EOF {
		return ticket, nil, errUploadForm
	}
	return ticket, data, nil
}

func normalizeBranding(ctx context.Context, scanner *imageScanner, data []byte) (normalizedBranding, error) {
	if bytes.HasPrefix(data, jpegMagic) || bytes.HasPrefix(data, pngMagic) {
		info, err := inspectImage(data)
		if err != nil {
			return normalizedBranding{}, err
		}
		clean, err := scanner.Scan(ctx, uploadBranding, data)
		if err != nil {
			return normalizedBranding{}, err
		}
		img, err := decodeInSlot(ctx, clean, info, uploadBranding)
		if err != nil {
			return normalizedBranding{}, err
		}
		return normalizedBranding{ContentType: "image/jpeg", Body: img.full, Width: img.width, Height: img.height}, nil
	}
	if len(data) > brandingSVGCap {
		return normalizedBranding{}, errBrandingSVG
	}
	clean, err := scanner.Scan(ctx, uploadBranding, data)
	if err != nil {
		return normalizedBranding{}, err
	}
	body, width, height, err := normalizeBrandingSVG(clean.Bytes())
	if err != nil {
		return normalizedBranding{}, err
	}
	return normalizedBranding{ContentType: "image/svg+xml", Body: body, Width: width, Height: height}, nil
}

func brandingUploadSize(w http.ResponseWriter, r *http.Request) bool {
	if len(r.TransferEncoding) > 0 || r.ContentLength < 0 || r.Header.Get("Content-Length") == "" {
		brandingResultPage(w, 411, "Logo not saved", "The upload must include Content-Length. Nothing was stored.")
		return false
	}
	if r.ContentLength > uploadBodyLimit {
		brandingResultPage(w, 413, "Logo not saved", "The logo upload is too large. Maximum file size is 4 MiB; SVG files must be at most 256 KiB. Nothing was stored.")
		return false
	}
	return true
}

func brandingUploadRequest(r *http.Request, w http.ResponseWriter) (*http.Request, context.CancelFunc) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(15 * time.Second))
	rc.SetWriteDeadline(time.Now().Add(uploadWriteWindow))
	ctx, cancel := context.WithTimeout(r.Context(), uploadReadWindow+uploadWorkTimeout)
	return r.WithContext(ctx), cancel
}

func (a *App) serveBrandingUpload(w http.ResponseWriter, r *http.Request) {
	if a.isolatedUploads {
		// Do not drain: originals may only reach the separate gateway in this mode.
		w.Header().Set("Connection", "close")
		brandingResultPage(w, 415, "Logo not saved", "Use the separate upload gateway. Nothing was stored.")
		return
	}
	if a.preview {
		brandingResultPage(w, 403, "Logo not saved", "This preview cannot save changes.")
		return
	}
	if !brandingUploadSize(w, r) {
		return
	}
	release, ok := tryTake(uploadWork)
	if !ok {
		brandingResultPage(w, 503, "Logo not saved", errBusyMsg)
		return
	}
	defer release()
	r, cancel := brandingUploadRequest(r, w)
	defer cancel()
	ticket, data, err := readBrandingForm(r, func(token string) error {
		if err := a.admitBranding(r.Context(), token); err != nil {
			return err
		}
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadWindow))
		return nil
	})
	io.CopyN(io.Discard, r.Body, uploadCeiling)
	if ticket == "" {
		brandingError(w, r, err)
		return
	}
	var normalized normalizedBranding
	if err == nil {
		ctx, stop := context.WithTimeout(r.Context(), uploadWorkTimeout)
		normalized, err = normalizeBranding(ctx, a.scanner, data)
		stop()
	}
	status, message, infected := 0, "", false
	if err != nil {
		status, message, infected = brandingScannerError(err)
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer stop()
	if err := a.finishBranding(ctx, ticket, normalized, infected); err != nil {
		brandingError(w, r, err)
		return
	}
	if status != 0 {
		brandingResultPage(w, status, "Logo not saved", message)
		return
	}
	a.scanner.RecordStored()
	brandingResultPage(w, 200, "Logo saved", "The new logo is now used across the market. Check the result in private administrator settings.")
}

type brandingGatewayAdmission struct{ Ticket string }
type brandingGatewayFinish struct {
	Ticket     string
	Normalized normalizedBranding
	Status     int
	Message    string
	Infected   bool
}

func (g *uploadGateway) serveBrandingUpload(w http.ResponseWriter, r *http.Request) {
	if !brandingUploadSize(w, r) {
		return
	}
	release, ok := tryTake(g.work)
	if !ok {
		brandingResultPage(w, 503, "Logo not saved", errBusyMsg)
		return
	}
	defer release()
	r, cancel := brandingUploadRequest(r, w)
	defer cancel()
	var refusal *gatewayReply
	ticket, data, err := readBrandingForm(r, func(token string) error {
		resp, err := g.call(r.Context(), "/v1/uploads/branding-admit", brandingGatewayAdmission{Ticket: token})
		if err != nil {
			return fail(503, "Logo authorization is unavailable. Nothing was stored.")
		}
		if resp.status != http.StatusOK {
			refusal = &resp
			return errBrandingTicket
		}
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadWindow))
		return nil
	})
	io.CopyN(io.Discard, r.Body, uploadCeiling)
	if refusal != nil {
		refusal.write(w)
		return
	}
	if ticket == "" {
		brandingError(w, r, err)
		return
	}
	result := brandingGatewayFinish{Ticket: ticket}
	if err == nil {
		ctx, stop := context.WithTimeout(r.Context(), uploadWorkTimeout)
		result.Normalized, err = normalizeBranding(ctx, g.scanner, data)
		stop()
	}
	if err != nil {
		result.Status, result.Message, result.Infected = brandingScannerError(err)
	}
	ctx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer stop()
	resp, err := g.call(ctx, "/v1/uploads/branding-finish", result)
	if err != nil {
		brandingResultPage(w, 502, "Logo status unknown", "The logo may have been saved. Check the private administrator settings before authorizing another upload.")
		return
	}
	if resp.status == http.StatusOK && result.Status == 0 {
		g.scanner.RecordStored()
	}
	resp.write(w)
}

func (a *App) admitGatewayBranding(w http.ResponseWriter, r *http.Request) {
	var v brandingGatewayAdmission
	if readUploadJSON(r, 1024, &v) != nil {
		http.Error(w, "Invalid logo authorization", 400)
		return
	}
	if err := a.admitBranding(r.Context(), v.Ticket); err != nil {
		brandingError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) finishGatewayBranding(w http.ResponseWriter, r *http.Request) {
	var v brandingGatewayFinish
	if readUploadJSON(r, uploadEnvelopeLimit, &v) != nil {
		http.Error(w, "Invalid normalized logo", 400)
		return
	}
	if v.Status != 0 && (!slices.Contains([]int{400, 408, 413, 415, 422, 503}, v.Status) || len(v.Message) > 2048 || !utf8.ValidString(v.Message) || len(v.Normalized.Body) != 0 || v.Infected && v.Status != 422) || v.Status == 0 && (!validNormalizedBranding(v.Normalized) || v.Infected || v.Message != "") {
		http.Error(w, "Invalid logo outcome", 400)
		return
	}
	if err := a.finishBranding(r.Context(), v.Ticket, v.Normalized, v.Infected); err != nil {
		brandingError(w, r, err)
		return
	}
	if v.Status != 0 {
		brandingResultPage(w, v.Status, "Logo not saved", v.Message)
		return
	}
	brandingResultPage(w, http.StatusOK, "Logo saved", "The new logo is now used across the market. Check the result in private administrator settings.")
}
