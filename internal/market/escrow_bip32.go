package market

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"opsecmkt/internal/btc"
)

// Per-order key derivation for escrow shape (b) (A-453, ruling P4-R2; no product caller yet: A-456). A per-order key is
// the unhardened child <i> of a party's registered account key (BIP32 CKDpub), to be served to wallets rooted: depth
// 0, parent fingerprint 0, child number 0, so the order wallet holds no link to the account. The arithmetic (math/big
// over secp256k1, never crypto/elliptic) lives in internal/btc, shared with the escrow signer since A-454. A caller must
// check each derived key against Bitcoin Core (checkPerOrderDerivation) before relying on it (P4-R2).

// The internal/btc names this package uses.
type bip32Key = btc.ExtKey

var (
	parseBIP32Pub = btc.ParseExtPub
	ckdPub        = btc.CKDPub

	errBIP32Encoding = btc.ErrEncoding
	errBIP32Version  = btc.ErrVersion
	errBIP32Prefix   = btc.ErrPrefix
	errBIP32Point    = btc.ErrPoint
	errBIP32Root     = btc.ErrRoot
	errBIP32Hardened = btc.ErrHardened
	errBIP32NoIndex  = btc.ErrNoIndex
	errBIP32Depth    = btc.ErrDepth
)

// deriveOrderKey is the per-order child <i> of a registered account key (escrowKey, parsed by parseEscrowKey), with
// the index used (ckdPub).
func deriveOrderKey(account escrowKey, i uint32) (bip32Key, uint32, error) {
	k := bip32Key{Depth: byte(len(account.Path)), ChainCode: account.ChainCode, PubKey: account.PubKey}
	if len(account.Path) > 0 {
		k.Child = account.Path[len(account.Path)-1]
	}
	return ckdPub(k, i)
}

// perOrderKey is one party's per-order key for the Core cross-check: the registered account key, the per-order index
// the Go derivation used, and the rooted serialization of that derived child (tpub or Vpub).
type perOrderKey struct {
	Account escrowKey
	Index   uint32
	Rooted  string
}

var (
	errPerOrderInput    = &escrowRPCError{Method: "deriveaddresses", Class: "per-order-input"}
	errPerOrderMismatch = &escrowRPCError{Method: "deriveaddresses", Class: "per-order-derivation-mismatch"}
)

// checkPerOrderDerivation checks Go's per-order derivation against Bitcoin Core: deriveaddresses of the descriptor
// built from each account key with its child path (<index>/0/0, Core deriving) and of the one built from each rooted
// key at /0/0 must return one and the same address. One key is checked as wpkh(…), three as the escrow's
// wsh(sortedmulti(2,…)). It refuses (errPerOrderInput, before any RPC) any other number of keys, a hardened index and
// a rooted key that is not a depth-0 tpub or Vpub, and (errPerOrderMismatch) any disagreement.
func (e *escrowBTC) checkPerOrderDerivation(ctx context.Context, keys []perOrderKey) error {
	if len(keys) != 1 && len(keys) != 3 {
		return errPerOrderInput
	}
	var fromAccount, fromRooted []string
	for _, k := range keys {
		r, err := parseBIP32Pub(k.Rooted, versionTpub, versionVpub)
		if k.Index >= 1<<31 || err != nil || r.Depth != 0 {
			return errPerOrderInput
		}
		fromAccount = append(fromAccount, k.Account.Expression()+"/"+strconv.FormatUint(uint64(k.Index), 10)+"/0/0")
		fromRooted = append(fromRooted, r.Serialize(versionTpub)+"/0/0")
	}
	desc := func(ks []string) string {
		if len(ks) == 1 {
			return withChecksum("wpkh(" + ks[0] + ")")
		}
		return withChecksum("wsh(sortedmulti(2," + strings.Join(ks, ",") + "))")
	}
	a, err := e.deriveAddresses(ctx, desc(fromAccount), -1)
	if err != nil {
		return err
	}
	b, err := e.deriveAddresses(ctx, desc(fromRooted), -1)
	if err != nil {
		return err
	}
	if len(a) != 1 || !slices.Equal(a, b) {
		return errPerOrderMismatch
	}
	return nil
}
