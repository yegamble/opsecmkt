// Package btc holds the pure Bitcoin key code the app (internal/market) and the escrow signer (cmd/escrow-signer)
// share: secp256k1 point arithmetic on math/big, base58check, BIP32 extended public keys and public derivation, and
// the BIP 380 descriptor checksum. Standard library plus x/crypto's RIPEMD-160 only; no network, no database, no
// private-key code (the signer keeps its own, cmd/escrow-signer/keys.go). Moved out of internal/market for A-454 so the
// signer binary stays small and auditable; the behaviour is A-453's.
package btc

import "math/big"

// secp256k1 field prime and group order.
var (
	P, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)
	N, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
)

// OnCurve reports whether a 33-byte compressed key's x coordinate has a point on secp256k1 (x < p and x³+7 a
// quadratic residue mod p, by Euler's criterion). Bitcoin Core would refuse the key later; this refuses it before
// anything is stored.
func OnCurve(compressed []byte) bool {
	if len(compressed) != 33 || compressed[0] != 2 && compressed[0] != 3 {
		return false
	}
	x := new(big.Int).SetBytes(compressed[1:])
	if x.Cmp(P) >= 0 {
		return false
	}
	rhs := new(big.Int).Exp(x, big.NewInt(3), P)
	rhs.Add(rhs, big.NewInt(7)).Mod(rhs, P)
	if rhs.Sign() == 0 {
		return true
	}
	e := new(big.Int).Rsh(new(big.Int).Sub(P, big.NewInt(1)), 1)
	return new(big.Int).Exp(rhs, e, P).Cmp(big.NewInt(1)) == 0
}

// Point is an affine point on secp256k1; X == nil is the point at infinity. crypto/elliptic is never used: its
// CurveParams assume a = -3, and secp256k1 has a = 0.
type Point struct{ X, Y *big.Int }

// G is the secp256k1 generator.
var G = Point{
	X: func() *big.Int {
		x, _ := new(big.Int).SetString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 16)
		return x
	}(),
	Y: func() *big.Int {
		y, _ := new(big.Int).SetString("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8", 16)
		return y
	}(),
}

// Decompress is the point of a compressed key that OnCurve accepts: y = (x³+7)^((p+1)/4) mod p (p ≡ 3 mod 4), negated
// to the prefix's parity.
func Decompress(c []byte) (Point, bool) {
	if !OnCurve(c) {
		return Point{}, false
	}
	x := new(big.Int).SetBytes(c[1:])
	rhs := new(big.Int).Exp(x, big.NewInt(3), P)
	rhs.Add(rhs, big.NewInt(7)).Mod(rhs, P)
	y := new(big.Int).Exp(rhs, new(big.Int).Rsh(new(big.Int).Add(P, big.NewInt(1)), 2), P)
	if new(big.Int).Exp(y, big.NewInt(2), P).Cmp(rhs) != 0 || y.Sign() == 0 {
		return Point{}, false
	}
	if y.Bit(0) != uint(c[0]&1) {
		y.Sub(P, y)
	}
	return Point{x, y}, true
}

// Compress is pt's 33-byte compressed form; pt must not be the point at infinity.
func Compress(pt Point) (out [33]byte) {
	out[0] = 2 | byte(pt.Y.Bit(0))
	pt.X.FillBytes(out[1:])
	return out
}

// Add is a + b in affine coordinates (secp256k1: y² = x³ + 7, so the doubling slope is 3x²/2y).
func Add(a, b Point) Point {
	if a.X == nil {
		return b
	}
	if b.X == nil {
		return a
	}
	num, den := new(big.Int), new(big.Int)
	if a.X.Cmp(b.X) == 0 {
		if a.Y.Cmp(b.Y) != 0 || a.Y.Sign() == 0 {
			return Point{} // a = -b
		}
		num.Mul(a.X, a.X).Mul(num, big.NewInt(3))
		den.Lsh(a.Y, 1)
	} else {
		num.Sub(b.Y, a.Y)
		den.Sub(b.X, a.X)
	}
	den.Mod(den, P).ModInverse(den, P)
	l := num.Mul(num, den).Mod(num, P)
	x := new(big.Int).Mul(l, l)
	x.Sub(x, a.X).Sub(x, b.X).Mod(x, P)
	y := new(big.Int).Sub(a.X, x)
	y.Mul(y, l).Sub(y, a.Y).Mod(y, P)
	return Point{x, y}
}

// Mul is k·pt by double-and-add. It is not constant-time: the app uses it on public data only; the escrow signer uses
// it once at startup on its account private key (cmd/escrow-signer/keys.go says why that is acceptable there).
func Mul(k *big.Int, pt Point) Point {
	var r Point
	for i := k.BitLen() - 1; i >= 0; i-- {
		r = Add(r, r)
		if k.Bit(i) == 1 {
			r = Add(r, pt)
		}
	}
	return r
}
