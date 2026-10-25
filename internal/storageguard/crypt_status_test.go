package storageguard

import (
	"strings"
	"testing"
)

const activeCryptStatus = "/dev/dm-0 is active and is in use.\n  type: LUKS2\n  cipher: aes-xts-plain64\n  keysize: 512 bits\n  key location: keyring\n  device: /dev/loop0\n  sector size: 512\n  mode: read/write\n"

func TestCryptStatusConfirmsActualTargetWithoutKeyOutput(t *testing.T) {
	for _, status := range []string{activeCryptStatus, strings.Replace(activeCryptStatus, "is active and is in use.", "is active.", 1), strings.Replace(activeCryptStatus, "512 bits", "256 bits", 1)} {
		if err := validateCryptStatus([]byte(status)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCryptStatusRejectsInactiveLinearNullAndMalformedTargets(t *testing.T) {
	for name, status := range map[string]string{
		"empty":            "",
		"inactive":         strings.Replace(activeCryptStatus, "is active and is in use.", "is inactive.", 1),
		"linear":           strings.Replace(activeCryptStatus, "type: LUKS2", "type: n/a", 1),
		"LUKS1":            strings.Replace(activeCryptStatus, "type: LUKS2", "type: LUKS1", 1),
		"missing type":     strings.Replace(activeCryptStatus, "type: LUKS2", "other: LUKS2", 1),
		"missing cipher":   strings.Replace(activeCryptStatus, "cipher: aes-xts-plain64", "cipher:", 1),
		"null cipher":      strings.Replace(activeCryptStatus, "aes-xts-plain64", "cipher_null-ecb", 1),
		"capital null":     strings.Replace(activeCryptStatus, "aes-xts-plain64", "NULL", 1),
		"invalid cipher":   strings.Replace(activeCryptStatus, "aes-xts-plain64", "cipher with spaces", 1),
		"weak keysize":     strings.Replace(activeCryptStatus, "512 bits", "128 bits", 1),
		"invalid keysize":  strings.Replace(activeCryptStatus, "512 bits", "unknown bits", 1),
		"missing keysize":  strings.Replace(activeCryptStatus, "keysize: 512 bits", "keysize:", 1),
		"duplicate type":   activeCryptStatus + "  type: LUKS2\n",
		"oversized output": strings.Repeat("x", (16<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCryptStatus([]byte(status)); err == nil {
				t.Fatal("unverified crypt target accepted")
			}
		})
	}
}
