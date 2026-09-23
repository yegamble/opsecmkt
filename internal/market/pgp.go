package market

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// OpenPGP helpers shared by the PGP identity (P2) and transparency (P6) packages. The server only
// verifies and encrypts to public keys; it never holds user private keys.

// parsePublicKey accepts exactly one armored, unrevoked public key and returns it with its uppercase hex fingerprint.
func parsePublicKey(armored string) (*openpgp.Entity, string, error) {
	if strings.Count(armored, "-----BEGIN PGP") != 1 {
		return nil, "", errors.New("paste exactly one armored public key block")
	}
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armored)))
	if err != nil {
		return nil, "", errors.New("not an ASCII-armored OpenPGP key")
	}
	if block.Type != openpgp.PublicKeyType {
		return nil, "", errors.New("paste an armored public key block only; never upload a private key")
	}
	list, err := openpgp.ReadKeyRing(block.Body)
	if err != nil {
		return nil, "", errors.New("the public key could not be parsed")
	}
	if len(list) != 1 {
		return nil, "", errors.New("paste exactly one public key")
	}
	e := list[0]
	if e.PrivateKey != nil {
		return nil, "", errors.New("private key material rejected; paste only your public key")
	}
	if e.Revoked(time.Now()) {
		return nil, "", errors.New("this key is revoked")
	}
	return e, strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint)), nil
}

// entityKeyIDs lists the primary and subkey IDs, for matching encrypted-message recipients.
func entityKeyIDs(e *openpgp.Entity) []uint64 {
	ids := []uint64{e.PrimaryKey.KeyId}
	for _, s := range e.Subkeys {
		ids = append(ids, s.PublicKey.KeyId)
	}
	return ids
}

// verifyClearsigned checks a cleartext-signed message against key and returns the signed text and signature time.
func verifyClearsigned(armored string, key *openpgp.Entity) (string, time.Time, error) {
	b, _ := clearsign.Decode([]byte(strings.TrimSpace(armored)))
	if b == nil {
		return "", time.Time{}, errors.New("not a clearsigned OpenPGP message")
	}
	sig, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{key}, bytes.NewReader(b.Bytes), b.ArmoredSignature.Body, nil)
	if err != nil {
		return "", time.Time{}, errors.New("signature does not verify with the configured key")
	}
	return string(b.Plaintext), sig.CreationTime, nil
}

// verifyDetached checks an armored detached signature over signed and returns the signature time.
func verifyDetached(signed, armoredSig string, key *openpgp.Entity) (time.Time, error) {
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armoredSig)))
	if err != nil || block.Type != openpgp.SignatureType {
		return time.Time{}, errors.New("not an armored OpenPGP signature")
	}
	sig, _, err := openpgp.VerifyDetachedSignature(openpgp.EntityList{key}, strings.NewReader(signed), block.Body, nil)
	if err != nil {
		return time.Time{}, errors.New("signature does not verify with the configured key")
	}
	return sig.CreationTime, nil
}

// encryptTo returns plaintext encrypted to key as an armored PGP MESSAGE.
func encryptTo(key *openpgp.Entity, plaintext string) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		return "", err
	}
	pt, err := openpgp.Encrypt(w, []*openpgp.Entity{key}, nil, nil, nil)
	if err != nil {
		return "", err
	}
	if _, err = io.WriteString(pt, plaintext); err != nil {
		return "", err
	}
	if err = pt.Close(); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// inspectEncrypted reports whether an armored message is OpenPGP-encrypted, and the recipient key IDs
// of its public-key session packets (0 = hidden recipient). It never decrypts.
func inspectEncrypted(armored string) (recipientKeyIDs []uint64, isEncrypted bool, err error) {
	block, err := armor.Decode(strings.NewReader(strings.TrimSpace(armored)))
	if err != nil {
		return nil, false, errors.New("not ASCII-armored OpenPGP data")
	}
	if block.Type != "PGP MESSAGE" {
		return nil, false, nil
	}
	r := packet.NewReader(block.Body)
	for {
		p, err := r.Next()
		if err == io.EOF {
			return recipientKeyIDs, false, nil
		}
		if err != nil {
			return nil, false, errors.New("malformed OpenPGP message")
		}
		switch p := p.(type) {
		case *packet.EncryptedKey:
			recipientKeyIDs = append(recipientKeyIDs, p.KeyId)
		case *packet.SymmetricKeyEncrypted:
		case *packet.SymmetricallyEncrypted, *packet.AEADEncrypted:
			return recipientKeyIDs, true, nil
		default:
			return recipientKeyIDs, false, nil
		}
	}
}
