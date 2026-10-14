package market

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const auditExportRecipientMaxBytes = 64 << 10

var (
	errAuditExportRecipientFile     = errors.New("AUDIT_EXPORT_RECIPIENT must name a readable file of at most 64 KiB holding one armored OpenPGP public key")
	errAuditExportRecipientPrivate  = errors.New("AUDIT_EXPORT_RECIPIENT contains private key material; provide only the public key and keep the private key offline")
	errAuditExportRecipientInvalid  = errors.New("AUDIT_EXPORT_RECIPIENT must hold exactly one unrevoked armored OpenPGP public key")
	errAuditExportRecipientUnusable = errors.New("AUDIT_EXPORT_RECIPIENT has no encryption key usable now; provide an unexpired encryption key")
	errAuditExportEncryption        = errors.New("encryption to AUDIT_EXPORT_RECIPIENT failed; audit export is unavailable")
)

// AuditExportRecipient holds only the public key used to seal downloaded audit files.
// Its nil value preserves the existing unencrypted export behavior.
type AuditExportRecipient struct {
	key *openpgp.Entity
}

// LoadAuditExportRecipient reads AUDIT_EXPORT_RECIPIENT's public-key file. An empty
// path means encryption is unset. Configured failures never reveal paths or key data.
func LoadAuditExportRecipient(path string) (*AuditExportRecipient, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errAuditExportRecipientFile
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, auditExportRecipientMaxBytes+1))
	if err != nil || len(data) > auditExportRecipientMaxBytes {
		return nil, errAuditExportRecipientFile
	}
	armored := string(data)
	if strings.Contains(armored, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		return nil, errAuditExportRecipientPrivate
	}
	// A public armor label must not disguise private primary or subkey packets.
	block, err := armor.Decode(strings.NewReader(armored))
	if err != nil || block.Type != openpgp.PublicKeyType {
		return nil, errAuditExportRecipientInvalid
	}
	packets := packet.NewOpaqueReader(block.Body)
	for {
		p, err := packets.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errAuditExportRecipientInvalid
		}
		if p.Tag == 5 || p.Tag == 7 { // secret-key and secret-subkey packets
			return nil, errAuditExportRecipientPrivate
		}
	}
	e, _, err := parsePublicKey(armored)
	if err != nil {
		return nil, errAuditExportRecipientInvalid
	}
	if !canReceiveEncrypted(e, time.Now()) {
		return nil, errAuditExportRecipientUnusable
	}
	return &AuditExportRecipient{key: e}, nil
}

// Suffix marks encrypted files without changing the existing plaintext names.
func (r *AuditExportRecipient) Suffix() string {
	if r == nil {
		return ""
	}
	return ".asc"
}

// Encrypt seals the exact bytes as an armored binary OpenPGP message. A nil
// recipient returns the bytes unchanged; an unusable configured key fails closed.
func (r *AuditExportRecipient) Encrypt(plaintext []byte) ([]byte, error) {
	if r == nil {
		return plaintext, nil
	}
	if !canReceiveEncrypted(r.key, time.Now()) {
		return nil, errAuditExportEncryption
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return nil, errAuditExportEncryption
	}
	pt, err := openpgp.Encrypt(w, []*openpgp.Entity{r.key}, nil, nil, nil)
	if err != nil {
		return nil, errAuditExportEncryption
	}
	if _, err = pt.Write(plaintext); err != nil {
		return nil, errAuditExportEncryption
	}
	if err = pt.Close(); err != nil {
		return nil, errAuditExportEncryption
	}
	if err = w.Close(); err != nil {
		return nil, errAuditExportEncryption
	}
	return buf.Bytes(), nil
}
