package market

import (
	"testing"
)

// altEscrowPubKey is key with its public key's parity byte flipped (02 <-> 03) and the same chain code, under origin fp.
func altEscrowPubKey(t *testing.T, key, fp string) string {
	t.Helper()
	k, err := parseEscrowKey(key, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base58CheckDecode(k.XPub)
	if err != nil {
		t.Fatal(err)
	}
	raw[45] ^= 1
	return "[" + fp + k.PathText + "]" + base58CheckEncode(raw)
}
