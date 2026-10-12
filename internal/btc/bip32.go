package btc

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"math/big"
	"slices"
)

// BIP32 extended public keys and public derivation (A-453): CKDpub on math/big over secp256k1, refusing hardened
// indexes and moving to the next index on IL >= n or the point at infinity, a strict parser (test vector 5's rules)
// and a rooted serializer (depth 0, parent fingerprint 0, child 0).

// Extended-key version bytes (BIP32 and SLIP-132) escrow uses: on test networks tpub, and Vpub (a P2WSH multisig
// key); on mainnet xpub and Zpub (A-460). The private versions are what the practice transaction's throwaway key uses.
const (
	VersionTpub = 0x043587cf
	VersionVpub = 0x02575483 // SLIP-132 multisig P2WSH, test networks
	VersionTprv = 0x04358394
	VersionXpub = 0x0488b21e
	VersionZpub = 0x02aa7ed3 // SLIP-132 multisig P2WSH, mainnet
	VersionXprv = 0x0488ade4
	// Hardened is the first hardened child number (2^31).
	Hardened = 1 << 31
)

var (
	ErrEncoding = errors.New("bip32: not a base58check extended key of 78 bytes")
	ErrVersion  = errors.New("bip32: version bytes not accepted here")
	ErrPrefix   = errors.New("bip32: public key prefix is not 02 or 03")
	ErrPoint    = errors.New("bip32: public key is not a point on secp256k1")
	ErrRoot     = errors.New("bip32: depth 0 with a parent fingerprint or child number")
	ErrHardened = errors.New("bip32: a public key cannot derive a hardened child")
	ErrNoIndex  = errors.New("bip32: no valid unhardened child at or after this index")
	ErrDepth    = errors.New("bip32: depth 255 has no serializable child")
)

// ExtKey is an extended public key: BIP32's serialization without its version bytes.
type ExtKey struct {
	Depth     byte
	ParentFP  [4]byte
	Child     uint32
	ChainCode [32]byte
	PubKey    [33]byte
}

// ParseExtPub decodes a bare extended public key strictly (BIP32 "Serialization format" and test vector 5):
// base58check of exactly 78 bytes, version bytes among versions, a compressed key (02 or 03) whose x has a point on
// the curve, and at depth 0 a zero parent fingerprint and child number. Escrow passes only one network's plain and
// multisig versions (tpub and Vpub, or xpub and Zpub).
func ParseExtPub(s string, versions ...uint32) (ExtKey, error) {
	raw, err := Base58CheckDecode(s)
	if err != nil || len(raw) != 78 {
		return ExtKey{}, ErrEncoding
	}
	if !slices.Contains(versions, binary.BigEndian.Uint32(raw[:4])) {
		return ExtKey{}, ErrVersion
	}
	if raw[45] != 2 && raw[45] != 3 {
		return ExtKey{}, ErrPrefix
	}
	if !OnCurve(raw[45:78]) {
		return ExtKey{}, ErrPoint
	}
	k := ExtKey{Depth: raw[4], Child: binary.BigEndian.Uint32(raw[9:13])}
	copy(k.ParentFP[:], raw[5:9])
	copy(k.ChainCode[:], raw[13:45])
	copy(k.PubKey[:], raw[45:78])
	if k.Depth == 0 && (k.ParentFP != [4]byte{} || k.Child != 0) {
		return ExtKey{}, ErrRoot
	}
	return k, nil
}

// Serialize is the key's BIP32 serialization under version, base58check-encoded.
func (k ExtKey) Serialize(version uint32) string {
	raw := binary.BigEndian.AppendUint32(make([]byte, 0, 78), version)
	raw = append(raw, k.Depth)
	raw = append(raw, k.ParentFP[:]...)
	raw = binary.BigEndian.AppendUint32(raw, k.Child)
	raw = append(raw, k.ChainCode[:]...)
	return Base58CheckEncode(append(raw, k.PubKey[:]...))
}

// Rooted is the key serialized as a root (depth 0, parent fingerprint 0, child number 0) under version: VersionTpub
// or VersionXpub for Bitcoin Core descriptors, VersionVpub or VersionZpub (SLIP-132 P2WSH multisig) for wallets. Only the chain code
// and key are kept, so the result names no parent.
func (k ExtKey) Rooted(version uint32) string {
	return ExtKey{ChainCode: k.ChainCode, PubKey: k.PubKey}.Serialize(version)
}

// Fingerprint is the key's own BIP32 fingerprint: the first 4 bytes of HASH160 of its public key. It is what a
// descriptor names as the origin of a key derived from the rooted form ([fingerprint]tpub…/i/0/0).
func (k ExtKey) Fingerprint() (fp [4]byte) {
	h := Hash160(k.PubKey[:])
	copy(fp[:], h[:4])
	return fp
}

// CKDPub is BIP32's CKDpub from k at index i, returning the child and the index used: when i gives an invalid child
// (IL >= n or the point at infinity) BIP32 says to proceed with the next index, so the used index can exceed i.
// Hardened indexes are refused, and so is a step past 2^31-1.
func CKDPub(k ExtKey, i uint32) (ExtKey, uint32, error) {
	return ckdPubWith(k, i, func(key, data []byte) (out [64]byte) {
		m := hmac.New(sha512.New, key)
		m.Write(data)
		copy(out[:], m.Sum(nil))
		return out
	})
}

// ckdPubWith is CKDPub with HMAC-SHA512 passed in, so a test can force an invalid child at one index.
func ckdPubWith(k ExtKey, i uint32, mac func(key, data []byte) [64]byte) (ExtKey, uint32, error) {
	if i >= Hardened {
		return ExtKey{}, 0, ErrHardened
	}
	if k.Depth == 255 {
		return ExtKey{}, 0, ErrDepth
	}
	parent, ok := Decompress(k.PubKey[:])
	if !ok {
		return ExtKey{}, 0, ErrPoint
	}
	data := make([]byte, 37)
	copy(data, k.PubKey[:])
	for ; i < Hardened; i++ {
		binary.BigEndian.PutUint32(data[33:], i)
		sum := mac(k.ChainCode[:], data)
		il := new(big.Int).SetBytes(sum[:32])
		if il.Cmp(N) >= 0 {
			continue // BIP32: invalid, proceed with the next index
		}
		child := Add(Mul(il, G), parent)
		if child.X == nil {
			continue // BIP32: the point at infinity, proceed with the next index
		}
		out := ExtKey{Depth: k.Depth + 1, Child: i, PubKey: Compress(child), ParentFP: k.Fingerprint()}
		copy(out.ChainCode[:], sum[32:])
		return out, i, nil
	}
	return ExtKey{}, 0, ErrNoIndex
}
