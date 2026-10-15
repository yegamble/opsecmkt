package market

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"strings"

	"testing"
)

func multipartSigningPost(x *escrowEnv, path, session, order, id, nonce, field, psbt string) *httptest.ResponseRecorder {
	x.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"csrf": x.A.csrf(session), formNonceField: nonce, "order_id": order,
	} {
		if err := mw.WriteField(name, value); err != nil {
			x.t.Fatal(err)
		}
	}
	if path == "/escrow/hardware-bump-sign" {
		if err := mw.WriteField("bump_id", id); err != nil {
			x.t.Fatal(err)
		}
	} else if err := mw.WriteField("settlement_id", id); err != nil {
		x.t.Fatal(err)
	}
	if field == psbtFileField {
		part, err := mw.CreateFormFile(psbtFileField, "signed.psbt")
		if err != nil {
			x.t.Fatal(err)
		}
		if _, err := part.Write([]byte(psbt)); err != nil {
			x.t.Fatal(err)
		}
	} else if err := mw.WriteField(field, psbt); err != nil {
		x.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		x.t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	return w
}

func TestPSBTMultipartFormNonceParserBounds(t *testing.T) {
	makeRequest := func(fields [][2]string) *http.Request {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for _, field := range fields {
			if err := mw.WriteField(field[0], field[1]); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/escrow/hardware-sign", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		return r
	}
	base := [][2]string{{"csrf", "token"}, {formNonceField, strings.Repeat("a", 32)}, {"order_id", "order"}, {"settlement_id", "1"}, {"psbt", ""}}
	for _, tc := range []struct {
		name   string
		fields [][2]string
		want   int
	}{
		{name: "known 32 byte field", fields: base, want: 0},
		{name: "unknown field", fields: append(append([][2]string{}, base...), [2]string{"unexpected", "x"}), want: 400},
		{name: "duplicate nonce", fields: append(append([][2]string{}, base...), [2]string{formNonceField, strings.Repeat("b", 32)}), want: 400},
		{name: "oversized nonce", fields: append(append([][2]string{}, base[:1]...), append([][2]string{{formNonceField, strings.Repeat("a", 33)}}, base[2:]...)...), want: 413},
		{name: "invalid text", fields: append(append([][2]string{}, base[:4]...), [2]string{"psbt", "bad\x00text"}), want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := makeRequest(tc.fields)
			_, code, _ := readPSBTUploadForm(httptest.NewRecorder(), r)
			if code != tc.want {
				t.Fatalf("parser status = %d, want %d", code, tc.want)
			}
		})
	}
}
