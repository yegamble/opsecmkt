package market

import (
	"bytes"

	"mime/multipart"
	"net/http"
	"net/http/httptest"

	"strings"
)

type uploadPart struct {
	name, file string // file: a file part's name ("" for a text field)
	value      []byte
}

// signUpload posts the sign form as multipart/form-data as session, the CSRF token first unless parts carry one.
func (x *escrowEnv) signUpload(session string, parts ...uploadPart) *httptest.ResponseRecorder {
	return x.signUploadWithNonceDefault(session, true, parts...)
}

func (x *escrowEnv) signUploadRaw(session string, parts ...uploadPart) *httptest.ResponseRecorder {
	return x.signUploadWithNonceDefault(session, false, parts...)
}

func (x *escrowEnv) signUploadWithNonceDefault(session string, generateNonce bool, parts ...uploadPart) *httptest.ResponseRecorder {
	x.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if !strings.Contains(partsNames(parts), "csrf,") {
		mw.WriteField("csrf", x.A.csrf(session))
	}
	if generateNonce && !strings.Contains(partsNames(parts), formNonceField+",") {
		mw.WriteField(formNonceField, randomToken()[:32])
	}
	for _, p := range parts {
		var w interface{ Write([]byte) (int, error) }
		var err error
		if p.file != "" {
			w, err = mw.CreateFormFile(p.name, p.file)
		} else {
			w, err = mw.CreateFormField(p.name)
		}
		if err != nil {
			x.t.Fatal(err)
		}
		w.Write(p.value)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/escrow/sign", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	return w
}

func partsNames(parts []uploadPart) string {
	var s string
	for _, p := range parts {
		s += p.name + ","
	}
	return s
}
