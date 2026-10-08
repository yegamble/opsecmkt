package market

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
)

// A signed PSBT uploaded as a file beside the paste (A-456b), with no JavaScript: the settlement sign form
// (/escrow/sign) is a multipart post, the only one besides uploads.go's photo and avatar routes, so a party can upload
// the file Electrum or Sparrow saves instead of pasting it. ServeHTTP reads its body here, before any slot, as it reads
// any other form's (at most psbtUploadBodyLimit bytes, through http.MaxBytesReader); the parts become the request's
// form and every later check is the paste's: the central text check, Origin, CSRF, the session, then escrowSignAction.
//
// Only the five known fields, each at most once and within its own bound: csrf, order_id, settlement_id, psbt (the
// paste) and psbt_file. A part's file name and Content-Type are never read. Every text part, and a text file (a base64
// PSBT, such as the market's own download signed), must be valid UTF-8 without a NUL byte (formTextValid, as uploads.go's
// readForm checks its text parts, QA5W3B-5); a binary PSBT file, starting with the PSBT magic bytes, is turned into its
// base64 text, which decodePSBT bounds and parses as it does a paste.

// psbtUploadRoutes are the form posts that take a multipart body for a signed PSBT file.
var psbtUploadRoutes = map[string]bool{"/escrow/sign": true}

const (
	psbtFileField = "psbt_file"
	// psbtUploadBodyLimit bounds the whole body, which is read before Origin, CSRF and admission (as any form's body is):
	// one PSBT of at most psbtMaxBase64 bytes, pasted or as a file (the action refuses both, so no legitimate form
	// carries two), and the small fields, an empty paste beside a file included, with the multipart framing around them
	// (QA5W5A-5). That is about as much as any other form's 64 KiB body, read at the same point; a first-part CSRF check,
	// as uploads.go makes, would duplicate app.go's token check (with its anonymous-token fallback) and rely on the
	// browser's part order.
	psbtUploadBodyLimit = psbtMaxBase64 + 16<<10
)

// psbtUploadFields is each field the sign form's multipart body may carry, with the most bytes it may hold.
var psbtUploadFields = map[string]int64{"csrf": csrfPartLimit, "order_id": idPartLimit, "settlement_id": 20, "psbt": psbtMaxBase64, psbtFileField: psbtMaxBase64}

const (
	errPSBTUploadForm  = "Invalid form: reload the order page and try again. Nothing was changed."
	errPSBTUploadLarge = "The signed PSBT, or the form around it, is too large: a settlement PSBT file is at most 64 KB. Nothing was changed."
)

// readPSBTUploadForm reads a sign form's multipart body into its fields (see the file comment), or answers the status
// and plain-text message of its refusal.
func readPSBTUploadForm(w http.ResponseWriter, r *http.Request) (url.Values, int, string) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, http.StatusBadRequest, errPSBTUploadForm
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, psbtUploadBodyLimit))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, http.StatusRequestEntityTooLarge, errPSBTUploadLarge
	case err != nil:
		return nil, http.StatusBadRequest, errPSBTUploadForm
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	form := url.Values{}
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return form, 0, ""
		}
		if err != nil {
			return nil, http.StatusBadRequest, errPSBTUploadForm
		}
		name := p.FormName()
		limit, known := psbtUploadFields[name]
		if !known || form.Has(name) {
			return nil, http.StatusBadRequest, errPSBTUploadForm
		}
		v, err := io.ReadAll(io.LimitReader(p, limit+1))
		switch {
		case err != nil:
			return nil, http.StatusBadRequest, errPSBTUploadForm
		case int64(len(v)) > limit:
			return nil, http.StatusRequestEntityTooLarge, errPSBTUploadLarge
		}
		s := string(v)
		if name == psbtFileField && bytes.HasPrefix(v, []byte("psbt\xff")) {
			s = base64.StdEncoding.EncodeToString(v) // a binary PSBT file
		}
		if !formTextValid(url.Values{name: {s}}) {
			return nil, http.StatusBadRequest, invalidFormText
		}
		form.Set(name, s)
	}
}
