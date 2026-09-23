package market

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// seal encrypts small secrets at rest (e.g. TOTP secrets, label "totp") with AES-256-GCM under
// sha256(label+":"+SETUP_TOKEN). Rotating SETUP_TOKEN makes previously sealed values unreadable.
func (a *App) seal(label, plaintext string) (string, error) {
	gcm, err := a.sealer(label)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), []byte(label))), nil
}

func (a *App) open(label, sealed string) (string, error) {
	gcm, err := a.sealer(label)
	if err != nil {
		return "", err
	}
	b, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil || len(b) < gcm.NonceSize() {
		return "", errors.New("sealed value malformed")
	}
	out, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], []byte(label))
	if err != nil {
		return "", errors.New("sealed value cannot be opened; was SETUP_TOKEN rotated?")
	}
	return string(out), nil
}

func (a *App) sealer(label string) (cipher.AEAD, error) {
	if len(a.setupToken) < 32 || label == "" {
		return nil, errors.New("sealing requires SETUP_TOKEN and a label")
	}
	key := sha256.Sum256([]byte(label + ":" + a.setupToken))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
