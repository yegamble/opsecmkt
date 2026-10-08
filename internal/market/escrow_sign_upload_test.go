package market

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// A-456b: a signed settlement PSBT may be uploaded as a file beside the paste, with no JavaScript: the sign form is a
// multipart post, the only one besides the photo and avatar uploads. Its text parts (csrf, order_id, settlement_id, the
// paste) and a text file pass the same UTF-8 and NUL check as every form's fields (formTextValid, as uploads.go's
// readForm checks its parts); a binary PSBT file, as Electrum and Sparrow save one, is taken by its magic bytes; the body
// and each part are bounded. The uploaded PSBT then goes through every ES-2 check of the paste. DB integration.

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

func TestEscrowSignUploadRequiresExactFormNonce(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order, id := x.built(regtestAddr("upload-nonce-refund"), regtestAddr("upload-nonce-claim"), 60000, 40000)
	session, fingerprint := x.partySess(roleVendor)
	signed := walletSign(t, x.partyCopy(order, session, id), fingerprint, 1)
	for _, tc := range []struct {
		name  string
		extra []uploadPart
	}{
		{name: "missing"},
		{name: "non-hex 32 bytes", extra: []uploadPart{{formNonceField, "", []byte(strings.Repeat("g", 32))}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []uploadPart{{"order_id", "", []byte(order)}, {"settlement_id", "", []byte(strconv.FormatInt(id, 10))}}
			parts = append(parts, tc.extra...)
			parts = append(parts, uploadPart{"psbt_file", "signed.psbt", []byte(signed)})
			w := x.signUploadRaw(session, parts...)
			if w.Code != 400 {
				t.Fatalf("missing/invalid nonce status = %d %s", w.Code, w.Body.String())
			}
			if got := escrowFormReceiptCount(t, x.testEnv, "/escrow/sign"); got != 0 {
				t.Fatalf("refused signature nonce left %d signing receipts", got)
			}
			if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures"); got != 0 {
				t.Fatalf("refused nonce stored %d signatures", got)
			}
		})
	}
}

func partsNames(parts []uploadPart) string {
	var s string
	for _, p := range parts {
		s += p.name + ","
	}
	return s
}

func TestEscrowSignUpload(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	order, id := x.built(regtestAddr("upload-refund"), regtestAddr("upload-claim"), 60000, 40000)
	sid := strconv.FormatInt(id, 10)
	vsess, vfp := x.partySess(roleVendor)
	signed := walletSign(t, x.partyCopy(order, vsess, id), vfp, 1)
	// The sign form is a multipart form (no script) with the file field beside the paste, which is no longer required.
	page := x.page("/order?id="+order, vsess)
	if !strings.Contains(page, `action="/escrow/sign" enctype="multipart/form-data"`) || !strings.Contains(page, `type="file" name="psbt_file"`) ||
		strings.Contains(page, `name="psbt" required`) {
		t.Fatalf("the sign form:\n%s", page)
	}
	ids := func(extra ...uploadPart) []uploadPart {
		return append([]uploadPart{{"order_id", "", []byte(order)}, {"settlement_id", "", []byte(sid)}}, extra...)
	}
	nothingStored := func(what string) {
		t.Helper()
		if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures"); n != 0 {
			t.Fatalf("%s: %d signatures stored", what, n)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(signed)
	for _, c := range []struct {
		name  string
		parts []uploadPart
		code  int
		want  string
	}{
		{"invalid UTF-8 in a text field", ids(uploadPart{"psbt", "", []byte("cHNidP8\xff")}), 400, invalidFormText},
		{"a NUL in a text field", []uploadPart{{"order_id", "", []byte(order)}, {"settlement_id", "", []byte(sid + "\x00")}, {"psbt_file", "signed.psbt", []byte(signed)}}, 400, invalidFormText},
		{"invalid UTF-8 in a text file", ids(uploadPart{"psbt_file", "signed.txt", []byte(signed[:40] + "\xc3\x28")}), 400, invalidFormText},
		{"a NUL in a text file", ids(uploadPart{"psbt_file", "signed.txt", append([]byte(signed), 0)}), 400, invalidFormText},
		{"an oversized file", ids(uploadPart{"psbt_file", "big.psbt", bytes.Repeat([]byte("A"), psbtMaxBase64+1)}), 413, "too large"},
		{"an oversized body", ids(uploadPart{"psbt", "", bytes.Repeat([]byte("A"), psbtMaxBase64)}, uploadPart{"psbt_file", "big.psbt", bytes.Repeat([]byte("B"), psbtMaxBase64)}, uploadPart{"x", "", bytes.Repeat([]byte("C"), 20<<10)}), 413, "too large"},
		// QA5W5A-5: the body, read before Origin, CSRF and admission, holds one PSBT and the small fields, never two.
		{"two PSBTs of half the bound each", ids(uploadPart{"psbt", "", bytes.Repeat([]byte("A"), psbtMaxBase64/2+8<<10)}, uploadPart{"psbt_file", "big.psbt", bytes.Repeat([]byte("B"), psbtMaxBase64/2+8<<10)}), 413, "too large"},
		{"one PSBT of the largest size", ids(uploadPart{"psbt", "", nil}, uploadPart{"psbt_file", "big.psbt", bytes.Repeat([]byte("A"), psbtMaxBase64)}), 400, "could not be read"},
		{"an unknown field", ids(uploadPart{"psbt_file", "signed.psbt", raw}, uploadPart{"note", "", []byte("x")}), 400, "Invalid form"},
		{"a field twice", ids(uploadPart{"psbt_file", "signed.psbt", raw}, uploadPart{"order_id", "", []byte(order)}), 400, "Invalid form"},
		{"both a paste and a file", ids(uploadPart{"psbt", "", []byte(signed)}, uploadPart{"psbt_file", "signed.psbt", raw}), 400, "not both"},
		{"no CSRF token", []uploadPart{{"csrf", "", []byte("forged")}, {"order_id", "", []byte(order)}, {"settlement_id", "", []byte(sid)}, {"psbt_file", "signed.psbt", raw}}, 403, "Form expired"},
		{"a file signed with SIGHASH_NONE", ids(uploadPart{"psbt_file", "none.psbt", []byte(walletSign(t, x.partyCopy(order, vsess, id), vfp, 2))}), 400, "sighash type other than ALL"},
	} {
		w := x.signUpload(vsess, c.parts...)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
		nothingStored(c.name)
	}
	// A binary PSBT file (as Electrum and Sparrow save one), with an empty paste beside it: accepted.
	if w := x.signUpload(vsess, ids(uploadPart{"psbt", "", nil}, uploadPart{"psbt_file", "signed.psbt", raw})...); w.Code != 303 {
		t.Fatalf("a binary PSBT file: %d %s", w.Code, w.Body.String())
	}
	if r := x.settleByID(id); r.State != "party_signed" || r.Signer != roleVendor {
		t.Fatalf("after the upload: %+v", r)
	}
	assertNowhereInDB(t, x.testEnv, signed)
	// The buyer uploads a base64 text file with a trailing newline (the market's own download, signed): accepted too.
	bsess, bfp := x.partySess(roleBuyer)
	text := walletSign(t, x.partyCopy(order, bsess, id), bfp, 1) + "\n"
	if w := x.signUpload(bsess, ids(uploadPart{"psbt_file", "escrow-settlement.psbt.txt", []byte(text)})...); w.Code != 303 {
		t.Fatalf("a base64 text file: %d %s", w.Code, w.Body.String())
	}
	if n := agInt(x.testEnv, "SELECT count(DISTINCT party) FROM settlement_signatures WHERE settlement_id=$1", id); n != 2 {
		t.Fatalf("%d parties' signatures after both uploads", n)
	}
	// Any other route still refuses a multipart body before reading it.
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("csrf", x.A.csrf(vsess))
	mw.Close()
	r := httptest.NewRequest("POST", "/escrow/claim", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: vsess})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("a multipart post to /escrow/claim: %d", w.Code)
	}
}
