package market

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"strings"

	"testing"
)

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
