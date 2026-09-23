package market

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// RFC 4226 HOTP / RFC 6238 TOTP with the parameters every common authenticator app supports:
// HMAC-SHA-1, 30-second steps, 6 digits, a 20-byte secret, and acceptance of one step either side.

const (
	totpPeriod = 30
	totpDigits = 6
	totpSkew   = 1
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a random 160-bit secret in unpadded base32 (32 characters).
func newTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return totpEncoding.EncodeToString(b)
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	b, err := totpEncoding.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil || len(b) < 10 {
		return nil, errors.New("invalid TOTP secret")
	}
	return b, nil
}

// hotp computes the RFC 4226 value for counter with the given number of digits.
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, v%mod)
}

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// totpMatch returns the step whose code equals code, searching now±totpSkew, and only accepting
// steps strictly after lastStep so a code can never be used twice.
func totpMatch(key []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	found, ok := int64(0), false
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		// Compare every candidate so timing does not reveal which step matched.
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s), totpDigits)), []byte(code)) == 1 && s > lastStep && !ok {
			found, ok = s, true
		}
	}
	return found, ok
}

// otpauthURI is the key-URI format authenticator apps accept when typed or pasted (no QR code is rendered).
func otpauthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// groupSecret spaces a base32 secret in blocks of four for manual entry.
func groupSecret(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Recovery codes: 80 random bits as 16 base32 characters, shown as xxxx-xxxx-xxxx-xxxx and stored as sha256.

const recoveryCodeCount = 10

var recoveryEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newRecoveryCode() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := recoveryEncoding.EncodeToString(b)
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

// normalizeRecoveryCode accepts any case, spaces and dashes; it returns "" for malformed input.
func normalizeRecoveryCode(s string) string {
	s = strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	if len(s) != 16 {
		return ""
	}
	if _, err := recoveryEncoding.DecodeString(s); err != nil {
		return ""
	}
	return s
}

func recoveryHash(s string) string { return digest("recovery:" + normalizeRecoveryCode(s)) }
