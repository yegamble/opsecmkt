package keymaterial

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha3"
	"encoding/base32"
	"errors"
	"strings"
)

var torBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func torClientOutputs(action string, o options) ([]outputFile, error) {
	var private *ecdh.PrivateKey
	var err error
	if action == "generate" {
		private, err = ecdh.X25519().GenerateKey(rand.Reader)
	} else {
		data, readErr := readFile(o.in, true)
		if readErr != nil {
			return nil, readErr
		}
		s := strings.TrimSuffix(string(data), "\n")
		if strings.Contains(s, ":") {
			parts := strings.Split(s, ":")
			if len(parts) != 4 || parts[1] != "descriptor" || parts[2] != "x25519" {
				return nil, errors.New("keys: invalid Tor client private authorization")
			}
			if _, err := torOnion(parts[0]); err != nil {
				return nil, err
			}
			s = parts[3]
		}
		raw, decodeErr := torBase32.DecodeString(s)
		if decodeErr != nil || len(raw) != 32 || torBase32.EncodeToString(raw) != s || bytes.Equal(raw, make([]byte, 32)) {
			return nil, errors.New("keys: Tor client private key must be a canonical 32-byte Base32 key")
		}
		private, err = ecdh.X25519().NewPrivateKey(raw)
	}
	if err != nil {
		return nil, errors.New("keys: X25519 key generation/import failed")
	}
	secret := torBase32.EncodeToString(private.Bytes())
	if o.onion != "" {
		onion, err := torOnion(o.onion)
		if err != nil {
			return nil, err
		}
		secret = onion + ":descriptor:x25519:" + secret
	}
	public := "descriptor:x25519:" + torBase32.EncodeToString(private.PublicKey().Bytes())
	return []outputFile{{o.public, []byte(public + "\n"), 0600}, {o.private, []byte(secret + "\n"), 0600}}, nil
}

func torOnion(address string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(address), ".onion")
	if len(name) != 56 {
		return "", errors.New("keys: onion must be a valid Tor v3 address")
	}
	raw, err := torBase32.DecodeString(strings.ToUpper(name))
	if err != nil || len(raw) != 35 || raw[34] != 3 {
		return "", errors.New("keys: onion must be a valid Tor v3 address")
	}
	material := append([]byte(".onion checksum"), raw[:32]...)
	material = append(material, 3)
	checksum := sha3.Sum256(material)
	if !bytes.Equal(checksum[:2], raw[32:34]) {
		return "", errors.New("keys: Tor v3 onion checksum is invalid")
	}
	return name, nil
}
