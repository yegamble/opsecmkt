package market

import (
	"bytes"
	"context"
	"image/png"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-725: the setting's three meanings. Anything that is not exactly "false" or "under_attack", a missing row
// included, is the strictest mode.
func TestCaptchaModeOfFailsClosed(t *testing.T) {
	for value, want := range map[string]string{
		"false": captchaModeOff, "under_attack": captchaModeUnderAttack, "true": captchaModeAlways,
		"": captchaModeAlways, "FALSE": captchaModeAlways, "off": captchaModeAlways, "junk": captchaModeAlways,
	} {
		if got := captchaModeOf(value); got != want {
			t.Errorf("captchaModeOf(%q) = %q, want %q", value, got, want)
		}
	}
}

// A-725: the trigger's stated bounds, on an injected clock: 100 failed passwords in a ten-minute window or 10 refused
// password slots in a minute turn it on, for 30 minutes after the latest trip, and nothing else holds it on.
func TestSignInAttackTriggerThresholdsAndHold(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if signInAttackFailures != 100 || signInAttackFailureWindow != 10*time.Minute || signInAttackBusy != 10 ||
		signInAttackBusyWindow != time.Minute || signInAttackHold != 30*time.Minute {
		t.Fatal("the trigger's stated bounds changed: update the operator guide and the /admin/security copy with them")
	}
	t.Run("failed passwords", func(t *testing.T) {
		var s signInAttack
		for i := range signInAttackFailures - 1 {
			if s.noteFailure(now) || s.active(now) {
				t.Fatalf("on after %d failures", i+1)
			}
		}
		if !s.noteFailure(now) || !s.active(now) || s.trips != 1 || !s.since.Equal(now) {
			t.Fatalf("threshold did not turn it on: %+v", s)
		}
		if !s.active(now.Add(signInAttackHold-time.Second)) || s.active(now.Add(signInAttackHold)) {
			t.Fatal("the hold is not exactly bounded")
		}
	})
	t.Run("a window that ends starts the count again", func(t *testing.T) {
		var s signInAttack
		for range signInAttackFailures - 1 {
			s.noteFailure(now)
		}
		if s.noteFailure(now.Add(signInAttackFailureWindow)) || s.active(now.Add(signInAttackFailureWindow)) {
			t.Fatal("failures from an ended window still counted")
		}
		if s.failures != 1 {
			t.Fatalf("new window count %d", s.failures)
		}
	})
	t.Run("refused password slots", func(t *testing.T) {
		var s signInAttack
		for range signInAttackBusy - 1 {
			if s.noteBusy(now) {
				t.Fatal("on too early")
			}
		}
		if s.noteBusy(now.Add(signInAttackBusyWindow)) {
			t.Fatal("refusals from an ended window still counted")
		}
		for range signInAttackBusy - 2 {
			s.noteBusy(now.Add(signInAttackBusyWindow))
		}
		if !s.noteBusy(now.Add(signInAttackBusyWindow)) || !s.active(now.Add(signInAttackBusyWindow)) {
			t.Fatalf("threshold did not turn it on: %+v", s)
		}
	})
	t.Run("only a new trip extends it, and the view says how long it has been held", func(t *testing.T) {
		var s signInAttack
		for range signInAttackFailures {
			s.noteFailure(now)
		}
		later := now.Add(20 * time.Minute)
		for range signInAttackFailures - 1 {
			s.noteFailure(later)
		}
		if s.active(now.Add(signInAttackHold)) {
			t.Fatal("failures below the threshold extended the hold")
		}
		if turnedOn := s.noteFailure(later); turnedOn {
			t.Fatal("an extension was reported as turning on")
		}
		if !s.active(later.Add(signInAttackHold-time.Second)) || s.active(later.Add(signInAttackHold)) || s.trips != 1 || !s.since.Equal(now) {
			t.Fatalf("extension: %+v", s)
		}
		v := s.view(later)
		if !v.Active || v.Trips != 1 || v.Since != "2026-10-31 12:00 UTC" || v.Until != "2026-10-31 12:50 UTC" {
			t.Fatalf("view %+v", v)
		}
		// Off again, a later trip is a second one and a new period.
		after := later.Add(signInAttackHold)
		if v := s.view(after); v.Active || v.Since != "" || v.Until != "" || v.Trips != 1 {
			t.Fatalf("view after the hold %+v", v)
		}
		for range signInAttackFailures {
			s.noteFailure(after)
		}
		if s.trips != 2 || !s.since.Equal(after) {
			t.Fatalf("second trip: %+v", s)
		}
	})
}

// A-725: each ingress has its own state (as its own limiter budgets, A-570), so the public listener cannot put the
// image on the private administrator sign-in.
func TestSignInAttackKeepsIngressesApart(t *testing.T) {
	a := &App{}
	public, private := context.Background(), context.WithValue(context.Background(), adminIngressKey{}, true)
	for range signInAttackFailures {
		a.noteSignInFailure(public)
	}
	if !a.signInAttackOn(public) || a.signInAttackOn(private) {
		t.Fatal("public failures must trip only the public state")
	}
	b := &App{}
	for range signInAttackBusy {
		b.noteSignInBusy(private)
	}
	if b.signInAttackOn(public) || !b.signInAttackOn(private) {
		t.Fatal("private refusals must trip only the private state")
	}
}

// A-725: every question is plain words a screen reader can read, its answers are short, and it derives from the id
// under its own HMAC prefix: the same id always asks the same thing. Questions are checked against an independent
// reading of their own text.
func TestCaptchaTextChallenge(t *testing.T) {
	a := &App{key: []byte("captcha-test-key")}
	families, answers := map[string]int{}, map[string]int{}
	for range 5000 {
		id := randomToken()
		c := a.captchaTextChallenge(id)
		for _, answer := range c.Answers {
			answers[answer]++
		}
		if c.Question == "" || !strings.HasSuffix(c.Question, "?") || len(c.Answers) == 0 {
			t.Fatalf("challenge %+v", c)
		}
		if strings.ContainsAny(c.Question, "0123456789<>&\"'") {
			t.Fatalf("question must be plain words: %q", c.Question)
		}
		if got := a.captchaTextChallenge(id); got.Question != c.Question || strings.Join(got.Answers, ",") != strings.Join(c.Answers, ",") {
			t.Fatal("same id asked something else")
		}
		for _, answer := range c.Answers {
			if len(answer) == 0 || len(answer) > captchaTextMaxLen || !c.accepts(answer) || !c.accepts("  "+strings.ToUpper(answer)+". ") {
				t.Fatalf("answer %q of %q", answer, c.Question)
			}
		}
		if c.accepts("") || c.accepts("not an answer") || c.accepts(strings.Repeat(c.Answers[0], 2000)) {
			t.Fatalf("wrong answer accepted for %q", c.Question)
		}
		q := strings.TrimSuffix(c.Question, "?")
		switch {
		case strings.HasPrefix(q, "What is the day after "):
			families["day"]++
			days := []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
			i := slices.Index(days, strings.ToLower(strings.TrimPrefix(q, "What is the day after ")))
			if i < 0 || !slices.Equal(c.Answers, []string{days[(i+1)%7]}) {
				t.Fatalf("%q answers %v", q, c.Answers)
			}
		case strings.HasPrefix(q, "What is "):
			families["sum"]++
			f := strings.Fields(strings.TrimPrefix(q, "What is "))
			if len(f) != 3 {
				t.Fatalf("%q", q)
			}
			x, y := slices.Index(captchaNumberWords[:], f[0]), slices.Index(captchaNumberWords[:], f[2])
			want := x + y
			if f[1] == "minus" {
				want = x - y
			}
			if x < 0 || y < 0 || want < 0 || !slices.Contains(c.Answers, strconv.Itoa(want)) || !slices.Contains(c.Answers, captchaNumberWords[want]) {
				t.Fatalf("%q answers %v", q, c.Answers)
			}
		case strings.HasPrefix(q, "Which of these words is "):
			families["word"]++
			kind, list, ok := strings.Cut(strings.TrimPrefix(q, "Which of these words is "), ": ")
			words := strings.Split(list, ", ")
			if !ok || len(words) != 4 || len(c.Answers) != 1 || !slices.Contains(words, c.Answers[0]) {
				t.Fatalf("%q answers %v", q, c.Answers)
			}
			hits := 0
			for _, w := range words {
				if slices.Contains(captchaWordKinds[kind], w) {
					hits++
				}
			}
			if hits != 1 || !slices.Contains(captchaWordKinds[kind], c.Answers[0]) {
				t.Fatalf("%q must have one right word, has %d", q, hits)
			}
		default:
			t.Fatalf("unknown question %q", q)
		}
	}
	for _, f := range []string{"day", "sum", "word"} {
		if families[f] < 500 {
			t.Errorf("family %s asked only %d of 5000 times", f, families[f])
		}
	}
	// The operator is told a blind guess works about one time in twenty; a script that reads the page always answers.
	for answer, n := range answers {
		if n > 5000/10 {
			t.Errorf("the constant guess %q answers %d of 5000 questions", answer, n)
		}
	}
	// The question is under its own HMAC prefix (SEC-3, App.csrf): it is not derived as the image's text is.
	id := randomToken()
	if a.captchaTextSeed(id) == a.captchaSeed(id) {
		t.Error("the question and the image share a seed")
	}
	other := &App{key: []byte("another-captcha-test-key")}
	differs := false
	for range 20 {
		id := randomToken()
		differs = differs || other.captchaTextChallenge(id).Question != a.captchaTextChallenge(id).Question
	}
	if !differs {
		t.Error("the question does not depend on the key")
	}
}

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
