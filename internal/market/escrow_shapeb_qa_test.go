package market

import (
	"encoding/hex"
	"encoding/json"

	"testing"

	"opsecmkt/internal/btc"
)

// siblingInUse is an env with one per-order escrow (whose siblings the market stores) and both parties' keys Bitcoin
// Core again, and that escrow's sibling /0/5.
func siblingInUse(t *testing.T) (*escrowEnv, string) {
	t.Helper()
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("sibling-owner"))
	sib := x.sibling(x.order, 0, 5)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	return x, sib
}

// signerAsAccount is the escrow signer's rooted account key re-serialized as a BIP48 account key at depth 4
// (/48h/1h/0h/2h) under the declared fingerprint fp: the same public key and chain code, so the same children.
func signerAsAccount(k btc.ExtKey, fp string) string {
	k.Depth, k.ParentFP, k.Child = 4, [4]byte{0xde, 0xad, 0xbe, 0xef}, 0x80000002
	return "[" + fp + "/48h/1h/0h/2h]" + k.Serialize(btc.VersionTpub)
}

// declaringSignerFP is a BIP48 account key no account holds (testBIP48Key with its chain code changed) declaring the
// signer's fingerprint as its origin.
func declaringSignerFP(t *testing.T, k btc.ExtKey) string {
	fp := k.Fingerprint()
	return altEscrowKey(t, testBIP48Key, hex.EncodeToString(fp[:]), 7)
}

// mapsEqualJSON compares two decoded JSON objects (import requests) by their JSON text.
func mapsEqualJSON(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
