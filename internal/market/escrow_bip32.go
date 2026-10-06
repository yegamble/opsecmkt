package market

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ripemd160" // BIP32 fingerprints (HASH160); x/crypto is already a dependency
)

// Per-order key derivation for escrow shape (b) (A-453, ruling P4-R2; no product caller yet: A-454 and A-456). A
// per-order key is the unhardened child <i> of a party's registered account key (BIP32 CKDpub), to be served to
// wallets rooted: depth 0, parent fingerprint 0, child number 0, so the order wallet holds no link to the account. The
// arithmetic is math/big over secp256k1 with escrow_keys.go's constants (secpP, secpN, onCurve). crypto/elliptic is
// never used: its CurveParams assume a = -3, and secp256k1 has a = 0. A caller must check each derived key against
// Bitcoin Core (checkPerOrderDerivation) before relying on it (P4-R2).

var (
	errBIP32Encoding = errors.New("bip32: not a base58check extended key of 78 bytes")
	errBIP32Version  = errors.New("bip32: version bytes not accepted here")
	errBIP32Prefix   = errors.New("bip32: public key prefix is not 02 or 03")
	errBIP32Point    = errors.New("bip32: public key is not a point on secp256k1")
	errBIP32Root     = errors.New("bip32: depth 0 with a parent fingerprint or child number")
	errBIP32Hardened = errors.New("bip32: a public key cannot derive a hardened child")
	errBIP32NoIndex  = errors.New("bip32: no valid unhardened child at or after this index")
	errBIP32Depth    = errors.New("bip32: depth 255 has no serializable child")
)

// bip32Key is an extended public key: BIP32's serialization without its version bytes.
type bip32Key struct {
	Depth     byte
	ParentFP  [4]byte
	Child     uint32
	ChainCode [32]byte
	PubKey    [33]byte
}

// parseBIP32Pub decodes a bare extended public key strictly (BIP32 "Serialization format" and test vector 5):
// base58check of exactly 78 bytes, version bytes among versions, a compressed key (02 or 03) whose x has a point on
// the curve, and at depth 0 a zero parent fingerprint and child number. Escrow passes only tpub and Vpub.
func parseBIP32Pub(s string, versions ...uint32) (bip32Key, error) {
	raw, err := base58CheckDecode(s)
	if err != nil || len(raw) != 78 {
		return bip32Key{}, errBIP32Encoding
	}
	if !slices.Contains(versions, binary.BigEndian.Uint32(raw[:4])) {
		return bip32Key{}, errBIP32Version
	}
	if raw[45] != 2 && raw[45] != 3 {
		return bip32Key{}, errBIP32Prefix
	}
	if !onCurve(raw[45:78]) {
		return bip32Key{}, errBIP32Point
	}
	k := bip32Key{Depth: raw[4], Child: binary.BigEndian.Uint32(raw[9:13])}
	copy(k.ParentFP[:], raw[5:9])
	copy(k.ChainCode[:], raw[13:45])
	copy(k.PubKey[:], raw[45:78])
	if k.Depth == 0 && (k.ParentFP != [4]byte{} || k.Child != 0) {
		return bip32Key{}, errBIP32Root
	}
	return k, nil
}

// serialize is the key's BIP32 serialization under version, base58check-encoded.
func (k bip32Key) serialize(version uint32) string {
	raw := binary.BigEndian.AppendUint32(make([]byte, 0, 78), version)
	raw = append(raw, k.Depth)
	raw = append(raw, k.ParentFP[:]...)
	raw = binary.BigEndian.AppendUint32(raw, k.Child)
	raw = append(raw, k.ChainCode[:]...)
	return base58CheckEncode(append(raw, k.PubKey[:]...))
}

// rooted is the key serialized as a root (depth 0, parent fingerprint 0, child number 0) under version: versionTpub
// for Bitcoin Core descriptors, versionVpub (SLIP-132 test-network P2WSH multisig) for wallets. Only the chain code
// and key are kept, so the result names no parent.
func (k bip32Key) rooted(version uint32) string {
	return bip32Key{ChainCode: k.ChainCode, PubKey: k.PubKey}.serialize(version)
}

// ckdPub is BIP32's CKDpub from k at index i, returning the child and the index used: when i gives an invalid child
// (IL >= n or the point at infinity) BIP32 says to proceed with the next index, so the used index can exceed i.
// Hardened indexes are refused, and so is a step past 2^31-1.
func ckdPub(k bip32Key, i uint32) (bip32Key, uint32, error) {
	return ckdPubWith(k, i, func(key, data []byte) (out [64]byte) {
		m := hmac.New(sha512.New, key)
		m.Write(data)
		copy(out[:], m.Sum(nil))
		return out
	})
}

// ckdPubWith is ckdPub with HMAC-SHA512 passed in, so a test can force an invalid child at one index.
func ckdPubWith(k bip32Key, i uint32, mac func(key, data []byte) [64]byte) (bip32Key, uint32, error) {
	if i >= 1<<31 {
		return bip32Key{}, 0, errBIP32Hardened
	}
	if k.Depth == 255 {
		return bip32Key{}, 0, errBIP32Depth
	}
	parent, ok := secpDecompress(k.PubKey[:])
	if !ok {
		return bip32Key{}, 0, errBIP32Point
	}
	data := make([]byte, 37)
	copy(data, k.PubKey[:])
	for ; i < 1<<31; i++ {
		binary.BigEndian.PutUint32(data[33:], i)
		sum := mac(k.ChainCode[:], data)
		il := new(big.Int).SetBytes(sum[:32])
		if il.Cmp(secpN) >= 0 {
			continue // BIP32: invalid, proceed with the next index
		}
		child := secpAdd(secpMul(il, secpG), parent)
		if child.x == nil {
			continue // BIP32: the point at infinity, proceed with the next index
		}
		h := sha256.Sum256(k.PubKey[:])
		r := ripemd160.New()
		r.Write(h[:])
		out := bip32Key{Depth: k.Depth + 1, Child: i, PubKey: secpCompress(child)}
		copy(out.ParentFP[:], r.Sum(nil))
		copy(out.ChainCode[:], sum[32:])
		return out, i, nil
	}
	return bip32Key{}, 0, errBIP32NoIndex
}

// deriveOrderKey is the per-order child <i> of a registered account key (escrowKey, parsed by parseEscrowKey), with
// the index used (ckdPub).
func deriveOrderKey(account escrowKey, i uint32) (bip32Key, uint32, error) {
	k := bip32Key{Depth: byte(len(account.Path)), ChainCode: account.ChainCode, PubKey: account.PubKey}
	if len(account.Path) > 0 {
		k.Child = account.Path[len(account.Path)-1]
	}
	return ckdPub(k, i)
}

// secpPoint is an affine point on secp256k1; x == nil is the point at infinity.
type secpPoint struct{ x, y *big.Int }

var secpG = secpPoint{
	x: func() *big.Int {
		x, _ := new(big.Int).SetString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 16)
		return x
	}(),
	y: func() *big.Int {
		y, _ := new(big.Int).SetString("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8", 16)
		return y
	}(),
}

// secpDecompress is the point of a compressed key that onCurve accepts: y = (x³+7)^((p+1)/4) mod p (p ≡ 3 mod 4),
// negated to the prefix's parity.
func secpDecompress(c []byte) (secpPoint, bool) {
	if !onCurve(c) {
		return secpPoint{}, false
	}
	x := new(big.Int).SetBytes(c[1:])
	rhs := new(big.Int).Exp(x, big.NewInt(3), secpP)
	rhs.Add(rhs, big.NewInt(7)).Mod(rhs, secpP)
	y := new(big.Int).Exp(rhs, new(big.Int).Rsh(new(big.Int).Add(secpP, big.NewInt(1)), 2), secpP)
	if new(big.Int).Exp(y, big.NewInt(2), secpP).Cmp(rhs) != 0 || y.Sign() == 0 {
		return secpPoint{}, false
	}
	if y.Bit(0) != uint(c[0]&1) {
		y.Sub(secpP, y)
	}
	return secpPoint{x, y}, true
}

func secpCompress(pt secpPoint) (out [33]byte) {
	out[0] = 2 | byte(pt.y.Bit(0))
	pt.x.FillBytes(out[1:])
	return out
}

// secpAdd is a + b in affine coordinates (secp256k1: y² = x³ + 7, so the doubling slope is 3x²/2y).
func secpAdd(a, b secpPoint) secpPoint {
	if a.x == nil {
		return b
	}
	if b.x == nil {
		return a
	}
	num, den := new(big.Int), new(big.Int)
	if a.x.Cmp(b.x) == 0 {
		if a.y.Cmp(b.y) != 0 || a.y.Sign() == 0 {
			return secpPoint{} // a = -b
		}
		num.Mul(a.x, a.x).Mul(num, big.NewInt(3))
		den.Lsh(a.y, 1)
	} else {
		num.Sub(b.y, a.y)
		den.Sub(b.x, a.x)
	}
	den.Mod(den, secpP).ModInverse(den, secpP)
	l := num.Mul(num, den).Mod(num, secpP)
	x := new(big.Int).Mul(l, l)
	x.Sub(x, a.x).Sub(x, b.x).Mod(x, secpP)
	y := new(big.Int).Sub(a.x, x)
	y.Mul(y, l).Sub(y, a.y).Mod(y, secpP)
	return secpPoint{x, y}
}

// secpMul is k·pt by double-and-add (public data only: no constant-time requirement).
func secpMul(k *big.Int, pt secpPoint) secpPoint {
	var r secpPoint
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = secpAdd(r, r)
		if k.Bit(i) == 1 {
			r = secpAdd(r, pt)
		}
	}
	return r
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
		fromRooted = append(fromRooted, r.serialize(versionTpub)+"/0/0")
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
