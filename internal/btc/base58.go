package btc

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"

	"golang.org/x/crypto/ripemd160" // BIP32 fingerprints (HASH160); x/crypto is already a dependency
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58Index = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(base58Alphabet); i++ {
		t[base58Alphabet[i]] = int8(i)
	}
	return t
}()

// Base58MaxLen bounds a decoded string: an extended key is 111 characters, a WIF 52.
const Base58MaxLen = 128

// IsBase58Char reports whether c is in the base58 alphabet.
func IsBase58Char(c byte) bool { return base58Index[c] >= 0 }

// Base58CheckDecode decodes s and checks its 4-byte double-SHA256 checksum, returning the payload.
func Base58CheckDecode(s string) ([]byte, error) {
	if len(s) == 0 || len(s) > Base58MaxLen {
		return nil, errors.New("base58: bad length")
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	// Big-endian base-256 accumulator; at most len(s)*log(58)/log(256)+1 bytes.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		v := int(base58Index[s[i]])
		if v < 0 {
			return nil, errors.New("base58: invalid character")
		}
		carry := v
		for j := len(out) - 1; j >= 0; j-- {
			carry += int(out[j]) * 58
			out[j] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			out = append([]byte{byte(carry)}, out...)
			carry >>= 8
		}
	}
	raw := append(make([]byte, zeros, zeros+len(out)), out...)
	if len(raw) < 4 {
		return nil, errors.New("base58: too short")
	}
	payload, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	want := DoubleSHA256(payload)
	if !bytes.Equal(sum, want[:4]) {
		return nil, errors.New("base58: checksum mismatch")
	}
	return payload, nil
}

// Base58CheckEncode appends the checksum to payload and encodes it.
func Base58CheckEncode(payload []byte) string {
	sum := DoubleSHA256(payload)
	raw := append(bytes.Clone(payload), sum[:4]...)
	zeros := 0
	for zeros < len(raw) && raw[zeros] == 0 {
		zeros++
	}
	digits := make([]byte, 0, len(raw)*2)
	for _, b := range raw[zeros:] {
		carry := int(b)
		for j := range digits {
			carry += int(digits[j]) << 8
			digits[j] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append(digits, byte(carry%58))
			carry /= 58
		}
	}
	var sb strings.Builder
	for range zeros {
		sb.WriteByte('1')
	}
	for i := len(digits) - 1; i >= 0; i-- {
		sb.WriteByte(base58Alphabet[digits[i]])
	}
	return sb.String()
}

// DoubleSHA256 is SHA-256 applied twice.
func DoubleSHA256(b []byte) [32]byte {
	first := sha256.Sum256(b)
	return sha256.Sum256(first[:])
}

// Hash160 is RIPEMD-160 of SHA-256: a BIP32 key identifier, whose first 4 bytes are the key's fingerprint.
func Hash160(b []byte) (out [20]byte) {
	h := sha256.Sum256(b)
	r := ripemd160.New()
	r.Write(h[:])
	copy(out[:], r.Sum(nil))
	return out
}
