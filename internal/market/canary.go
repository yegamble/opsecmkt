package market

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

// P6 Transparency: the warrant canary is operator-authored text, clearsigned offline with the operator's PGP key.
// The server stores the submission verbatim and re-verifies it against the configured operator key on every
// render. It never writes, edits or signs canary text itself.

const (
	settingOperatorKey         = "operator_pgp_key"
	settingOperatorFingerprint = "operator_pgp_fingerprint"
	maxCanaryBytes             = 16384
	clearsignHeader            = "-----BEGIN PGP SIGNED MESSAGE-----"
)

// verifyCanary checks that text is exactly one clearsigned message (nothing before or after it) signed by the
// armored operator key. It returns the signed statement, the signature creation time and the key fingerprint.
func verifyCanary(operatorKey, text string) (statement string, signedAt time.Time, fingerprint string, err error) {
	if strings.TrimSpace(operatorKey) == "" {
		return "", time.Time{}, "", errors.New("no operator PGP key is configured")
	}
	key, fp, err := parsePublicKey(operatorKey)
	if err != nil {
		return "", time.Time{}, "", errors.New("the configured operator key is unusable: " + err.Error())
	}
	text = strings.TrimSpace(text)
	if len(text) > maxCanaryBytes {
		return "", time.Time{}, fp, errors.New("the statement is longer than 16384 bytes")
	}
	if !strings.HasPrefix(text, clearsignHeader) || strings.Count(text, clearsignHeader) != 1 {
		return "", time.Time{}, fp, errors.New("paste exactly one PGP clearsigned message with nothing before it")
	}
	block, rest := clearsign.Decode([]byte(text))
	if block == nil {
		return "", time.Time{}, fp, errors.New("not a well-formed PGP clearsigned message")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return "", time.Time{}, fp, errors.New("text after the signature block is not allowed")
	}
	statement, signedAt, err = verifyClearsigned(text, key)
	if err != nil {
		return "", time.Time{}, fp, err
	}
	if strings.TrimSpace(statement) == "" {
		return "", time.Time{}, fp, errors.New("the signed statement is empty")
	}
	return statement, signedAt, fp, nil
}

// groupFingerprint formats a hex fingerprint in blocks of four, as gpg prints it.
func groupFingerprint(fp string) string {
	var b strings.Builder
	for i := 0; i < len(fp); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fp[i:min(i+4, len(fp))])
	}
	return b.String()
}

func formatUTC(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// canaryView builds the canary status from the configured key and the stored submission. The result is exactly
// one of: no statement (Clearsigned == ""), Verified, or invalid (Error set).
func canaryView(operatorKey, clearsigned string, posted time.Time) *CanaryView {
	v := &CanaryView{}
	if strings.TrimSpace(operatorKey) != "" {
		v.OperatorKey = operatorKey
		if _, fp, err := parsePublicKey(operatorKey); err != nil {
			v.KeyError = "The configured operator key is unusable: " + err.Error()
		} else {
			v.Fingerprint = groupFingerprint(fp)
		}
	}
	if v.OperatorKey == "" || clearsigned == "" {
		return v // "No signed canary published": nothing is shown that the configured key could vouch for.
	}
	v.Clearsigned = clearsigned
	v.Posted = formatUTC(posted)
	statement, signedAt, _, err := verifyCanary(operatorKey, clearsigned)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Verified, v.Statement, v.SignedAt = true, statement, formatUTC(signedAt)
	return v
}

// loadCanary reads the stored canary and returns its freshly verified status.
func loadCanary(ctx context.Context, db *sql.DB, settings map[string]string) (*CanaryView, error) {
	var text string
	var posted time.Time
	err := db.QueryRowContext(ctx, "SELECT clearsigned, posted FROM canary WHERE id=1").Scan(&text, &posted)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return canaryView(settings[settingOperatorKey], text, posted), nil
}
