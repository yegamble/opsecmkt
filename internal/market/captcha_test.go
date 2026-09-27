package market

import (
	"bytes"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	if !a.captchaMatches(id, strings.ToLower(a.captchaAnswer(id))+" ") || !a.captchaMatches(id, a.captchaAnswer(id)) {
		t.Error("answers should be case-insensitive")
	}
	if other := randomToken(); a.captchaAnswer(other) != a.captchaAnswer(id) && a.captchaMatches(other, a.captchaAnswer(id)) {
		t.Error("answer not bound to the challenge id")
	}
}

// A-176: a challenge id is expiry ‖ nonce ‖ MAC bound to the issuing session; nothing is stored to check it.
func TestCaptchaIDIsStatelessAndBound(t *testing.T) {
	a := &App{key: []byte("captcha-test-key")}
	a.useKeyRoot("captcha-test-root-0123456789abcdef0123456789abcdef")
	now := time.Now()
	s1, s2 := digest("session-one"), digest("session-two")
	id := a.newCaptchaID(s1, now.Add(captchaTTL))
	if !captchaID.MatchString(id) {
		t.Fatalf("id %q is not 64 lowercase hex characters", id)
	}
	if other := a.newCaptchaID(s1, now.Add(captchaTTL)); other == id {
		t.Fatal("two challenges share an id")
	}
	exp, ok := a.captchaIDValid(id, now, s1)
	if !ok || exp.Unix() != now.Add(captchaTTL).Unix() {
		t.Fatalf("valid id refused: ok=%v expires=%v", ok, exp)
	}
	if _, ok := a.captchaIDValid(id, now, s2); ok {
		t.Error("id accepted for another session")
	}
	if _, ok := a.captchaIDValid(id, now, s2, s1); !ok {
		t.Error("id refused when its session is one of the candidates")
	}
	if _, ok := a.captchaIDValid(id, now.Add(captchaTTL), s1); ok {
		t.Error("id accepted at its expiry")
	}
	if _, ok := a.captchaIDValid(randomToken(), now, s1); ok {
		t.Error("random (pre-upgrade) id accepted")
	}
	// Any altered byte (a later expiry, another nonce, a forged MAC) is refused.
	for i := 0; i < len(id); i += 2 {
		b := []byte(id)
		if b[i] == 'f' {
			b[i] = '0'
		} else {
			b[i] = 'f'
		}
		if _, ok := a.captchaIDValid(string(b), now, s1); ok {
			t.Fatalf("id altered at %d accepted", i)
		}
	}
	other := &App{key: a.key}
	other.useKeyRoot("another-root-0123456789abcdef0123456789abcdef0123")
	if _, ok := other.captchaIDValid(id, now, s1); ok {
		t.Error("id accepted under another key root")
	}
	if (&App{}).newCaptchaID(s1, now.Add(captchaTTL)) != "" {
		t.Error("id minted without a key")
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
