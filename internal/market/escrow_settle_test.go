package market

import (
	"crypto/sha256"
	"encoding/hex"
)

// txidOf is a 64-hex transaction ID for label, as Core prints one.
func txidOf(label string) string {
	h := sha256.Sum256([]byte("tx:" + label))
	return hex.EncodeToString(h[:])
}
