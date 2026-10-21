package moderation

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"regexp"
	"strings"
)

// Bitcoin is not accepted here, so a Bitcoin address can only be an
// off-platform destination. It only blocks automatic history (reviewOnlySignal).
// A token counts only when its Base58Check (P2PKH/P2SH) or BIP173/BIP350
// checksum verifies, so random identifiers, key blocks and SKUs do not.
var addressToken = regexp.MustCompile(`[A-Za-z0-9]+`)

const (
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	bech32Alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

func bitcoinAddressDestination(value string) bool {
	for _, token := range addressToken.FindAllString(value, -1) {
		if base58CheckAddress(token) || segwitAddress(token) {
			return true
		}
	}
	return false
}

// base58CheckAddress accepts mainnet P2PKH ("1", version 0) and P2SH ("3",
// version 5): 25 bytes whose last four are the double-SHA256 checksum.
func base58CheckAddress(s string) bool {
	if len(s) < 26 || len(s) > 35 || (s[0] != '1' && s[0] != '3') {
		return false
	}
	n, radix := new(big.Int), big.NewInt(58)
	for i := 0; i < len(s); i++ {
		digit := strings.IndexByte(base58Alphabet, s[i])
		if digit < 0 {
			return false
		}
		n.Mul(n, radix).Add(n, big.NewInt(int64(digit)))
	}
	raw := append(make([]byte, len(s)-len(strings.TrimLeft(s, "1"))), n.Bytes()...)
	if len(raw) != 25 || !(s[0] == '1' && raw[0] == 0 || s[0] == '3' && raw[0] == 5) {
		return false
	}
	first := sha256.Sum256(raw[:21])
	second := sha256.Sum256(first[:])
	return bytes.Equal(second[:4], raw[21:])
}

// segwitAddress accepts a mainnet "bc1" address: single case, a valid bech32
// checksum for witness version 0 or bech32m for versions 1–16, and a 2–40 byte
// program (20 or 32 bytes for version 0).
func segwitAddress(s string) bool {
	if len(s) < 14 || len(s) > 74 || s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return false
	}
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, "bc1") {
		return false
	}
	values := []int{3, 3, 0, 2, 3} // the human-readable part "bc", expanded
	for i := 3; i < len(s); i++ {
		digit := strings.IndexByte(bech32Alphabet, s[i])
		if digit < 0 {
			return false
		}
		values = append(values, digit)
	}
	data := values[5:]
	version := data[0]
	want := 0x2bc830a3
	if version == 0 {
		want = 1
	}
	if version > 16 || bech32Polymod(values) != want {
		return false
	}
	acc, bits, length := 0, 0, 0
	for _, v := range data[1 : len(data)-6] {
		acc, bits = acc<<5|v, bits+5
		for bits >= 8 {
			bits -= 8
			length++
		}
		acc &= 1<<bits - 1
	}
	if bits >= 5 || acc != 0 || length < 2 || length > 40 {
		return false
	}
	return version != 0 || length == 20 || length == 32
}

func bech32Polymod(values []int) int {
	generator := [5]int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	checksum := 1
	for _, v := range values {
		top := checksum >> 25
		checksum = (checksum&0x1ffffff)<<5 ^ v
		for i, g := range generator {
			if top>>i&1 == 1 {
				checksum ^= g
			}
		}
	}
	return checksum
}
