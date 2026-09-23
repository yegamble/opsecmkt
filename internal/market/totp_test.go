package market

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B, SHA-1 (8 digits; 6-digit codes are the last six digits).
var rfc6238 = []struct {
	unix int64
	code string
}{
	{59, "94287082"},
	{1111111109, "07081804"},
	{1111111111, "14050471"},
	{1234567890, "89005924"},
	{2000000000, "69279037"},
	{20000000000, "65353130"},
}

var rfcKey = []byte("12345678901234567890")

func TestTOTPRFC6238Vectors(t *testing.T) {
	for _, v := range rfc6238 {
		step := uint64(totpStep(time.Unix(v.unix, 0)))
		if got := hotp(rfcKey, step, 8); got != v.code {
			t.Errorf("T=%d: got %s want %s", v.unix, got, v.code)
		}
		if got := hotp(rfcKey, step, totpDigits); got != v.code[2:] {
			t.Errorf("T=%d 6-digit: got %s want %s", v.unix, got, v.code[2:])
		}
		if s, ok := totpMatch(rfcKey, v.code[2:], time.Unix(v.unix, 0), 0); !ok || s != int64(step) {
			t.Errorf("T=%d: match=%v step=%d", v.unix, ok, s)
		}
	}
}

func TestTOTPWindowAndReplay(t *testing.T) {
	now := time.Unix(1234567890, 0)
	cur := totpStep(now)
	code := func(s int64) string { return hotp(rfcKey, uint64(s), totpDigits) }
	for d, want := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		if _, ok := totpMatch(rfcKey, code(cur+d), now, 0); ok != want {
			t.Errorf("offset %d accepted=%v want %v", d, ok, want)
		}
	}
	if _, ok := totpMatch(rfcKey, code(cur), now, cur); ok {
		t.Error("code for the last accepted step was accepted again")
	}
	if _, ok := totpMatch(rfcKey, code(cur-1), now, cur); ok {
		t.Error("code older than the last accepted step was accepted")
	}
	if s, ok := totpMatch(rfcKey, code(cur+1), now, cur); !ok || s != cur+1 {
		t.Error("next step rejected after an accepted code")
	}
	if s, ok := totpMatch(rfcKey, " "+code(cur)[:3]+" "+code(cur)[3:]+" ", now, 0); !ok || s != cur {
		t.Error("spaces inside a code should be ignored")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := totpMatch(rfcKey, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTOTPSecretAndURI(t *testing.T) {
	s := newTOTPSecret()
	if len(s) != 32 || s == newTOTPSecret() {
		t.Fatalf("secret %q", s)
	}
	key, err := decodeTOTPSecret(groupSecret(strings.ToLower(s)))
	if err != nil || len(key) != 20 {
		t.Fatalf("decode grouped secret: %v %d", err, len(key))
	}
	if _, err = decodeTOTPSecret("not base32!"); err == nil {
		t.Error("garbage secret decoded")
	}
	if g := groupSecret("ABCDEFGHIJ"); g != "ABCD EFGH IJ" {
		t.Errorf("groupSecret: %q", g)
	}
	u, err := url.Parse(otpauthURI("My Market", "alice_1", s))
	if err != nil || u.Scheme != "otpauth" || u.Host != "totp" || u.Query().Get("secret") != s || u.Query().Get("issuer") != "My Market" || u.Query().Get("digits") != "6" || u.Query().Get("period") != "30" {
		t.Fatalf("otpauth URI: %v %v", u, err)
	}
	if label, _ := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/")); label != "My Market:alice_1" {
		t.Errorf("label %q", label)
	}
}

func TestRecoveryCodes(t *testing.T) {
	format := regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$`)
	seen := map[string]bool{}
	for range 50 {
		c := newRecoveryCode()
		if !format.MatchString(c) || seen[c] {
			t.Fatalf("recovery code %q", c)
		}
		seen[c] = true
		plain := strings.ReplaceAll(c, "-", "")
		for _, variant := range []string{strings.ToUpper(c), plain, " " + plain[:8] + " " + plain[8:] + " "} {
			if recoveryHash(variant) != recoveryHash(c) {
				t.Errorf("%q does not normalize like %q", variant, c)
			}
		}
		if recoveryHash(c) == c || len(recoveryHash(c)) != 64 {
			t.Error("recovery code not hashed")
		}
	}
	for _, bad := range []string{"", "abcd-efgh", "abcd-efgh-ijkl-mno1", "abcd-efgh-ijkl-mnopq"} {
		if normalizeRecoveryCode(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
