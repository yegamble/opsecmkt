package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"opsecmkt/internal/btc"
)

// Per-order key derivation for escrow shape (b) (A-453, ruling P4-R2; its caller is escrow checkout since A-456a). A per-order key is
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

// perOrderKeys is the order wallet key of a per-order escrow at index i (the account key's child <i>, ckdPub) and its
// first receiving key /0/0, the escrow's key; ok is false when BIP32 calls one of them invalid (the index it would move
// to is then not i: the caller takes the next index instead).
func perOrderKeys(account escrowKey, i uint32) (order, leaf bip32Key, ok bool) {
	order, used, err := deriveOrderKey(account, i)
	if err != nil || used != i {
		return order, leaf, false
	}
	leaf, ok = perOrderSibling(order, 0, 0)
	return order, leaf, ok
}

// perOrderSibling is an order wallet's key /branch/k (receiving 0, change 1), or false when BIP32 calls a step invalid.
// A caller deriving many keys of one branch derives the branch key once (perOrderBranch) and each key from it
// (perOrderAt): every CKDpub step is a point multiplication in math/big (QA5W4A-4).
func perOrderSibling(order bip32Key, branch, k uint32) (bip32Key, bool) {
	c, ok := perOrderBranch(order, branch)
	if !ok {
		return c, false
	}
	return perOrderAt(c, k)
}

// perOrderBranch is an order wallet's branch key /branch (receiving 0, change 1), or false when BIP32 calls the step
// invalid.
func perOrderBranch(order bip32Key, branch uint32) (bip32Key, bool) {
	c, used, err := ckdPub(order, branch)
	return c, err == nil && used == branch
}

// perOrderAt is the key /k below an order wallet's branch key (perOrderBranch), or false when BIP32 calls the step
// invalid.
func perOrderAt(branchKey bip32Key, k uint32) (bip32Key, bool) {
	leaf, used, err := ckdPub(branchKey, k)
	return leaf, err == nil && used == k
}

// errPerOrderStored is a party of a shape-(b) escrow whose order wallet key cannot be rebuilt from what is stored: no
// per_order_xpub (the market's row, a row from before QA5W4A-5, or no such party), or one whose /0/0 child is not the
// stored key.
var errPerOrderStored = errors.New("escrow: no stored per-order key gives this escrow's key")

// perOrderStored is one party's escrow_pubkeys row of a shape-(b) escrow: its stored key (the order wallet's /0/0),
// derivation suffix (/<i>/0/0) and, for the buyer and the vendor, the order wallet key checkout stored (QA5W4A-5).
type perOrderStored struct {
	Party, PubKey, Suffix string
	Xpub                  sql.NullString
}

// orderKey is the party's order wallet key (its account key's child <i>, rooted), checked: its /0/0 child must be the
// stored key. The buyer's and the vendor's is the per_order_xpub checkout stored, never their current account key, which
// a key registered since gives no more (QA5W4A-5); the market's is the escrow signer's account key's child <i>, so
// signer must be the signer's pinned account key (signerAccount) and nil leaves the market's key unbuilt.
func (s perOrderStored) orderKey(signer *escrowKey) (bip32Key, error) {
	steps, err := parseOriginPath(s.Suffix)
	if err != nil || len(steps) != 3 {
		return bip32Key{}, errPerOrderStored
	}
	var order bip32Key
	switch {
	case s.Party == "market" && signer != nil:
		var ok bool
		if order, _, ok = perOrderKeys(*signer, steps[0]); !ok {
			return bip32Key{}, errPerOrderStored
		}
	case s.Party != "market" && s.Xpub.Valid:
		if order, err = parseBIP32Pub(s.Xpub.String, versionTpub); err != nil || order.Depth != 0 {
			return bip32Key{}, errPerOrderStored
		}
	default:
		return bip32Key{}, errPerOrderStored
	}
	if leaf, ok := perOrderSibling(order, 0, 0); !ok || hex.EncodeToString(leaf.PubKey[:]) != s.PubKey {
		return bip32Key{}, errPerOrderStored
	}
	return order, nil
}

// perOrderXpub is the order wallet key of party ("buyer", "vendor" or "market") in the shape-(b) escrow escrowID, read
// through q and checked against the stored key (perOrderStored.orderKey): the buyer's and the vendor's from what checkout
// stored, the market's from the escrow signer (signerAccount, whose error, a signer that is down or one whose key differs
// from the pins, is returned as it is). Its branch keys and sibling keys are perOrderBranch and perOrderAt of it; served
// to a wallet or a descriptor it is written rooted (Rooted(versionTpub)), as checkout imported it. errPerOrderStored: no
// such party, or nothing stored gives its key.
func (a *App) perOrderXpub(ctx context.Context, q rowQuerier, escrowID int64, party string) (bip32Key, error) {
	s := perOrderStored{Party: party}
	err := q.QueryRowContext(ctx, "SELECT pubkey,derivation_suffix,per_order_xpub FROM escrow_pubkeys WHERE escrow_id=$1 AND party=$2",
		escrowID, party).Scan(&s.PubKey, &s.Suffix, &s.Xpub)
	if err == sql.ErrNoRows {
		return bip32Key{}, errPerOrderStored
	}
	if err != nil {
		return bip32Key{}, err
	}
	var signer *escrowKey
	if party == "market" {
		k, err := a.signerAccount(ctx, q)
		if err != nil {
			return bip32Key{}, err
		}
		signer = &k
	}
	return s.orderKey(signer)
}

// The order wallet addresses a shape-(b) escrow covers (P4-R2): receiving /0/0../0/19 (/0/0 is the escrow's address) and
// change /1/0../1/9, the siblings cmd/escrow-signer also signs for (k < 20).
const (
	perOrderReceive = 20
	perOrderChange  = 10
)

// escrowSibling is one sibling address of a shape-(b) escrow's order wallet: /Branch/Index (escrow_siblings).
type escrowSibling struct {
	Branch, Index int
	Address       string
}

// siblingImports is one labelled addr(…) import per sibling address of order's shape-(b) escrow, with the escrow's own
// label. Bitcoin Core's listreceivedbyaddress, which the watcher reads deposits with (escrowBTC.incoming), reports only
// addresses in the wallet's address book, and the unlabelled ranged descriptors (Core refuses a label beside a range) put
// none there (probed on Core 31.1 regtest, TestEscrowRegtestPerOrderEscrow); the label puts each sibling there, and
// lists it, should its escrow be lost to a restore, as an orphan of that order.
func siblingImports(orderID string, addrs []string, timestamp any) []escrowImport {
	out := make([]escrowImport, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, escrowImport{Desc: withChecksum("addr(" + a + ")"), Timestamp: timestamp, Label: "escrow:" + orderID})
	}
	return out
}

// checkPerOrderEscrow is the P4-R2 cross-check of a shape-(b) escrow Core built (buildEscrowWith) against Go's
// derivation (escrowParty.Order and Leaf), before anything is stored or imported: each key Core put in the witness
// script is the one Go derived; Core derives one and the same address from the account keys' paths and from the rooted
// per-order keys (checkPerOrderDerivation); and every address of the order wallet's two ranged descriptors (rooted keys
// only, so the watch-only wallet holds no account key) over the covered range is the 2-of-3 of Go's keys at that index,
// /0/0 being the escrow's address. It fills in b's sibling addresses and the ranged imports. Any disagreement is
// errPerOrderMismatch.
func (e *escrowBTC) checkPerOrderEscrow(ctx context.Context, b *builtEscrow) error {
	var keys []perOrderKey
	for _, p := range b.Parties {
		if p.Layout != layoutPerOrder || !bytes.Equal(b.Keys[p.Party], p.Leaf.PubKey[:]) {
			return errPerOrderMismatch
		}
		keys = append(keys, perOrderKey{Account: p.Key, Index: uint32(p.Index), Rooted: p.Order.Rooted(versionTpub)})
	}
	if err := e.checkPerOrderDerivation(ctx, keys); err != nil {
		return err
	}
	b.Siblings, b.Ranged = nil, nil
	for branch, n := range []int{perOrderReceive, perOrderChange} {
		var exprs []string
		for _, k := range keys {
			exprs = append(exprs, k.Rooted+"/"+strconv.Itoa(branch)+"/*")
		}
		desc := withChecksum("wsh(sortedmulti(2," + strings.Join(exprs, ",") + "))")
		addrs, err := e.deriveAddressRange(ctx, desc, 0, int64(n-1))
		if err != nil {
			return err
		}
		if len(addrs) != n {
			return errPerOrderMismatch
		}
		// Each party's branch key once, each address's keys from it (QA5W4A-4).
		var branchKeys []bip32Key
		for _, p := range b.Parties {
			bk, ok := perOrderBranch(p.Order, uint32(branch))
			if !ok {
				return errPerOrderMismatch
			}
			branchKeys = append(branchKeys, bk)
		}
		for k, addr := range addrs {
			var leaves [][]byte
			for _, bk := range branchKeys {
				leaf, ok := perOrderAt(bk, uint32(k))
				if !ok {
					return errPerOrderMismatch
				}
				leaves = append(leaves, leaf.PubKey[:])
			}
			sum := sha256.Sum256(sortedMultisigScript(leaves...))
			if spk, err := escrowAddressScript(e.network, addr); err != nil || !bytes.Equal(spk, append([]byte{0, 32}, sum[:]...)) {
				return errPerOrderMismatch
			}
			if branch == 0 && k == 0 {
				if addr != b.Address {
					return errPerOrderMismatch
				}
				continue
			}
			b.Siblings = append(b.Siblings, escrowSibling{Branch: branch, Index: k, Address: addr})
		}
		b.Ranged = append(b.Ranged, escrowImport{Desc: desc, Timestamp: "now", Range: []int64{0, int64(n - 1)}})
	}
	return nil
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
