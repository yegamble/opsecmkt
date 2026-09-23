package market

import (
	"bytes"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaptchaFontCoversAlphabet(t *testing.T) {
	if strings.ContainsAny(captchaAlphabet, "018IOQ") || len(captchaAlphabet) != 30 {
		t.Fatalf("alphabet %q", captchaAlphabet)
	}
	shapes := map[[7]string]rune{}
	for _, r := range captchaAlphabet {
		g, ok := captchaFont[r]
		if !ok {
			t.Fatalf("no glyph for %c", r)
		}
		for _, row := range g {
			if len(row) != 5 || strings.Trim(row, "#.") != "" {
				t.Fatalf("glyph %c row %q", r, row)
			}
		}
		if other, dup := shapes[g]; dup {
			t.Errorf("glyphs %c and %c are identical", r, other)
		}
		shapes[g] = r
	}
	if len(captchaFont) != len(captchaAlphabet) {
		t.Error("font has glyphs outside the alphabet")
	}
}

func TestCaptchaTextAndImage(t *testing.T) {
	a := &App{key: []byte("captcha-test-key")}
	used := map[rune]bool{}
	for range 500 {
		id := randomToken()
		text := a.captchaAnswer(id)
		if len(text) != captchaLen || strings.Trim(text, captchaAlphabet) != "" {
			t.Fatalf("answer %q", text)
		}
		for _, r := range text {
			used[r] = true
		}
	}
	if len(used) != len(captchaAlphabet) {
		t.Errorf("only %d of %d characters used", len(used), len(captchaAlphabet))
	}
	id := randomToken()
	encode := func(id string) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, a.captchaImage(id)); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	first := encode(id)
	img, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != captchaWidth || b.Dy() != captchaHeight {
		t.Fatalf("size %v", b)
	}
	ink := 0
	for y := range captchaHeight {
		for x := range captchaWidth {
			if r, _, _, _ := img.At(x, y).RGBA(); r > 0xc000 {
				ink++
			}
		}
	}
	if ink < 500 {
		t.Errorf("only %d ink pixels", ink)
	}
	if !bytes.Equal(first, encode(id)) {
		t.Error("same id rendered differently")
	}
	if bytes.Equal(first, encode(randomToken())) {
		t.Error("different ids rendered identically")
	}
	if captchaAnswerHash(id, strings.ToLower(a.captchaAnswer(id))+" ") != captchaAnswerHash(id, a.captchaAnswer(id)) {
		t.Error("answers should be case-insensitive")
	}
	if captchaAnswerHash(id, a.captchaAnswer(id)) == captchaAnswerHash(randomToken(), a.captchaAnswer(id)) {
		t.Error("answer hash not bound to the challenge id")
	}
}

func TestCaptchaPreviewImage(t *testing.T) {
	a := testHTTPApp(true)
	for path, want := range map[string]int{"/captcha?id=preview": 200, "/captcha?id=../x": 404, "/captcha": 404} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: %d", path, w.Code)
		}
		if want == 200 {
			if w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("headers %v", w.Header())
			}
			if _, err := png.Decode(w.Body); err != nil {
				t.Error(err)
			}
		}
	}
}
