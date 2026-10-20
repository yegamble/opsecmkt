package market

import (
	"encoding/hex"

	"strings"

	"testing"
)

type escrowEnv struct {
	*testEnv
	t    *testing.T
	core *fakeEscrowCore
}

// altEscrowKey is key with another chain code (so another key) under origin fingerprint fp.
func altEscrowKey(t *testing.T, key, fp string, n byte) string {
	t.Helper()
	k, err := parseEscrowKey(key, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base58CheckDecode(k.XPub)
	if err != nil {
		t.Fatal(err)
	}
	raw[13] ^= n
	return "[" + fp + k.PathText + "]" + base58CheckEncode(raw)
}

func childHex(expr, suffix string) string {
	return hex.EncodeToString(fakeChild(expr[strings.IndexByte(expr, ']')+1:] + suffix))
}

func fakeArbiterExpr() string { return "[" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:] }
