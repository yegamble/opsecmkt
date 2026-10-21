package ramsecrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// CollectInstallation returns the same bounded, validated plaintext format used
// by secrets pack. It includes managed configuration only, never retained data.
func CollectInstallation(dir string) ([]byte, error) { return collect(dir) }

// ParseInstallation validates a bounded plaintext bundle without writing files.
// Callers must separately validate their complete operator configuration.
func ParseInstallation(data []byte) (Bundle, error) {
	var bundle Bundle
	if len(data) == 0 || len(data) > maxBundle {
		return bundle, errors.New("secrets: plaintext bundle exceeds bounds")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&bundle) != nil || d.Decode(new(any)) != io.EOF {
		return bundle, errors.New("secrets: invalid plaintext bundle JSON")
	}
	_, err := validate(bundle)
	return bundle, err
}

// EncryptInstallation encrypts an already bounded recovery document to exactly
// one public OpenPGP recipient. Private decryption keys are always refused.
func EncryptInstallation(public, plain []byte) ([]byte, error) {
	if len(public) > maxFile || len(plain) == 0 || len(plain) > 2*maxBundle {
		return nil, errors.New("secrets: recovery encryption input exceeds bounds")
	}
	return encrypt(public, plain)
}
