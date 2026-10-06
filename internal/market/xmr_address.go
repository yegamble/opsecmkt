package market

import (
	"crypto/subtle"
	"errors"
	"math/big"
	"math/bits"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Offline Monero address parsing for Monero escrow (WR-escrow Phase 3, P3-R2; A-449, P4-R15). Monero escrow runs on a
// local fakechain (monerod --regtest) or on stagenet, and the address tags it accepts follow the network type the
// escrow daemon was verified on (xmrEscrowRPC.nettype, set by startEscrowXMR after checkDaemon and the network pin),
// never a configured or guessed one: on stagenet the standard (network byte 24, "5…") and subaddress (36, "7…") forms,
// on a fakechain, whose wallets use the mainnet address format, the mainnet standard (18, "4…") and subaddress (42,
// "8…") forms. Every other tag is refused: integrated addresses of the network as integrated, anything else (testnet,
// mainnet on stagenet, stagenet on a fakechain) as another network's, and before the network is verified ("") every
// address. Mainnet and testnet are no escrow network. It is for the escrow path only; the pooled adapter
// (payments_monero.go) asks its wallet and refuses fakechain. Both keys must be points of ed25519's prime-order
// subgroup (xmrPointValid, A-417): stricter than the wallet's own check, which accepts a point with a small-order
// component and then cannot build, or crashes building, a transfer to it.

// xmrBase58Alphabet is Monero's base58 alphabet (Bitcoin's).
const xmrBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// xmrBase58BlockSizes[n] is the encoded length of an n-byte block (Monero's encoded_block_sizes): Monero encodes
// 8-byte blocks as 11 characters each and the last, shorter block in the fewest characters that hold it.
var xmrBase58BlockSizes = [9]int{0, 2, 3, 5, 6, 7, 9, 10, 11}

const (
	xmrTagStandard   = 18 // mainnet-format standard address (fakechain uses the mainnet format)
	xmrTagIntegrated = 19
	xmrTagSubaddress = 42
	xmrAddressLength = 95 // characters: 1 tag byte, two 32-byte keys and a 4-byte checksum (69 bytes)

	xmrStandard   = "standard"
	xmrSubaddress = "subaddress"
)

// Fixed refusals: none quotes the address.
var (
	errXMRAddressLength     = errors.New("a Monero address here is 95 characters: a standard address or a subaddress (integrated addresses are not accepted)")
	errXMRAddressEncoding   = errors.New("not a Monero address: it has characters outside Monero's base58 alphabet (no spaces) or is not base58-encoded")
	errXMRAddressChecksum   = errors.New("not a Monero address: its checksum does not match (check for a typing error)")
	errXMRAddressIntegrated = errors.New("integrated Monero addresses are not accepted; use a standard address or a subaddress")
	errXMRAddressKeys       = errors.New("not a valid Monero address: its keys are not valid (check it and paste it again from your wallet)")
	// errXMRAddressNetwork matches (errors.Is) every refusal of another network's address (each xmrNets refusal); as
	// itself it refuses every address while Monero escrow has not verified its daemon's network.
	errXMRAddressNetwork = errors.New("not an address Monero escrow can check yet: it has not verified its daemon's network (the escrow daemon is unavailable); try again later")
)

// xmrNetRefusal is one escrow network's refusal of another network's address: fixed text naming its own prefixes,
// matched by errors.Is(err, errXMRAddressNetwork).
type xmrNetRefusal struct{ text string }

func (e *xmrNetRefusal) Error() string { return e.text }
func (e *xmrNetRefusal) Unwrap() error { return errXMRAddressNetwork }

// xmrNet is one escrow network type's address tags (Monero's cryptonote_config.h) and its refusal of other networks'.
type xmrNet struct {
	standard, integrated, subaddress byte
	refusal                          error
}

// xmrNets are the network types Monero escrow runs on (checkDaemon refuses the others), by get_info's nettype.
var xmrNets = map[string]xmrNet{
	"fakechain": {xmrTagStandard, xmrTagIntegrated, xmrTagSubaddress, &xmrNetRefusal{"not an address of this escrow's network: Monero escrow on this market runs on a local fakechain, whose addresses start with 4 or 8 (stagenet and testnet addresses are refused)"}},
	"stagenet":  {24, 25, 36, &xmrNetRefusal{"not an address of this escrow's network: Monero escrow on this market runs on Monero stagenet, whose addresses start with 5 or 7 (mainnet, fakechain and testnet addresses are refused)"}},
}

// xmrPrefixes names the first characters of nettype's standard addresses and subaddresses, for help text: "5 or 7" on
// stagenet, "4 or 8" on a fakechain (and for any other value: the only network Monero escrow ran on before A-449).
func xmrPrefixes(nettype string) string {
	if nettype == "stagenet" {
		return "5 or 7"
	}
	return "4 or 8"
}

// xmrAddressNet is the escrow network type an address's tag belongs to (xmrNets), or "" for none. For display only (an
// order page names the network of its escrow's address); no address is accepted by it.
func xmrAddressNet(addr string) string {
	data, ok := xmrBase58Decode(addr)
	if !ok || len(data) == 0 {
		return ""
	}
	for name, n := range xmrNets {
		if data[0] == n.standard || data[0] == n.subaddress || data[0] == n.integrated {
			return name
		}
	}
	return ""
}

// xmrAddress is a parsed escrow address: its kind (xmrStandard or xmrSubaddress) and its public spend and view keys.
type xmrAddress struct {
	Kind        string
	Spend, View [32]byte
}

// parseXMRAddress parses s exactly (no trimming) as an address of network type nettype (the verified one; see the file
// comment): 95 base58 characters decoding to the network byte, the spend and view keys and the first 4 bytes of their
// Keccak-256 (Monero's cn_fast_hash). Both keys must be valid (xmrPointValid).
func parseXMRAddress(s, nettype string) (xmrAddress, error) {
	if len(s) != xmrAddressLength {
		return xmrAddress{}, errXMRAddressLength
	}
	data, ok := xmrBase58Decode(s)
	if !ok || len(data) != 69 {
		return xmrAddress{}, errXMRAddressEncoding
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(data[:65])
	if subtle.ConstantTimeCompare(h.Sum(nil)[:4], data[65:]) != 1 {
		return xmrAddress{}, errXMRAddressChecksum
	}
	net, ok := xmrNets[nettype]
	if !ok {
		return xmrAddress{}, errXMRAddressNetwork
	}
	var a xmrAddress
	switch data[0] { // a one-byte varint; a multi-byte tag (high bit set) is no network this escrow accepts
	case net.standard:
		a.Kind = xmrStandard
	case net.subaddress:
		a.Kind = xmrSubaddress
	case net.integrated:
		return xmrAddress{}, errXMRAddressIntegrated
	default:
		return xmrAddress{}, net.refusal
	}
	copy(a.Spend[:], data[1:33])
	copy(a.View[:], data[33:65])
	if !xmrPointValid(a.Spend) || !xmrPointValid(a.View) {
		return xmrAddress{}, errXMRAddressKeys
	}
	return a, nil
}

// checkXMRFormAddress is parseXMRAddress for an address typed into a form (a refund or claim address) on network type
// nettype: an integrated address of that network, which parseXMRAddress refuses by its length alone (106 characters),
// is refused as integrated.
func checkXMRFormAddress(s, nettype string) error {
	_, err := parseXMRAddress(s, nettype)
	net, known := xmrNets[nettype]
	if data, ok := xmrBase58Decode(s); err != nil && known && ok && len(data) == 77 && data[0] == net.integrated {
		err = errXMRAddressIntegrated
	}
	return err
}

// xmrBase58Decode decodes Monero base58 (base58::decode): full 11-character blocks to 8 bytes, then a last block
// of a length xmrBase58BlockSizes allows. A block whose value does not fit its byte count is refused.
func xmrBase58Decode(s string) ([]byte, bool) {
	full, rest := len(s)/11, len(s)%11
	last := -1
	for n, size := range xmrBase58BlockSizes {
		if size == rest {
			last = n
		}
	}
	if last < 0 {
		return nil, false
	}
	out := make([]byte, 0, full*8+last)
	for i := 0; i <= full; i++ {
		block, n := s[min(i*11, len(s)):min(i*11+11, len(s))], 8
		if i == full {
			n = last
		}
		var ok bool
		if out, ok = xmrDecodeBlock(out, block, n); !ok {
			return nil, false
		}
	}
	return out, true
}

// xmrDecodeBlock appends the n bytes block encodes (big-endian), refusing a character outside the alphabet, a
// value above 64 bits, or, for a short block, a value of n bytes or more.
func xmrDecodeBlock(out []byte, block string, n int) ([]byte, bool) {
	var num, order uint64 = 0, 1
	for i := len(block) - 1; i >= 0; i-- {
		digit := strings.IndexByte(xmrBase58Alphabet, block[i])
		if digit < 0 {
			return nil, false
		}
		hi, lo := bits.Mul64(order, uint64(digit))
		var carry uint64
		if num, carry = bits.Add64(num, lo, 0); hi != 0 || carry != 0 {
			return nil, false
		}
		order *= 58 // overflows only after the last (most significant) digit has been used
	}
	if n < 8 && num>>(8*n) != 0 {
		return nil, false
	}
	for i := n - 1; i >= 0; i-- {
		out = append(out, byte(num>>(8*i)))
	}
	return out, true
}

// ed25519, the curve of Monero's keys (RFC 8032 §5.1): -x² + y² = 1 + d·x²·y² over the field of p = 2^255-19, with
// d = -121665/121666, √-1 = 2^((p-1)/4), the square-root exponent (p-5)/8, and l = 2^252 +
// 27742317777372353535851937790883648493, the prime order of the base point's subgroup.
var (
	edP      = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edD      = edMul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), edP))
	edD2     = edMul(big.NewInt(2), edD)
	edSqrtM1 = new(big.Int).Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(edP, big.NewInt(1)), 2), edP)
	edRootE  = new(big.Int).Rsh(new(big.Int).Sub(edP, big.NewInt(5)), 3)
	edL, _   = new(big.Int).SetString("1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed", 16)
)

// edPoint is a point in extended coordinates (X:Y:Z:T): x = X/Z, y = Y/Z, x·y = T/Z (RFC 8032 §5.1.4).
type edPoint struct{ x, y, z, t *big.Int }

// edMul is a·b mod p, in [0, p).
func edMul(a, b *big.Int) *big.Int { return new(big.Int).Mod(new(big.Int).Mul(a, b), edP) }

// edSub is a-b mod p, in [0, p).
func edSub(a, b *big.Int) *big.Int { return new(big.Int).Mod(new(big.Int).Sub(a, b), edP) }

// edMod reduces a mod p in place, to [0, p).
func edMod(a *big.Int) *big.Int { return a.Mod(a, edP) }

// xmrPointValid reports whether a public key is a point of ed25519's prime-order subgroup other than the identity:
// it decompresses (xmrDecompress, Monero's check_key) and l times it is the identity. A real Monero key is a scalar
// times the base point, so it always is. Monero's check_key does not check the subgroup, but monero-wallet-rpc
// 0.18.5.1 crashed building a transfer to keys that pass check_key with a small-order (torsion) component, so this is
// deliberately stricter. It runs once per address parsed; speed is irrelevant and nothing here is secret.
func xmrPointValid(key [32]byte) bool {
	p, ok := xmrDecompress(key)
	return ok && !edIdentity(p) && edIdentity(edScalarMult(edL, p))
}

// xmrDecompress decodes a key as a point exactly as Monero's crypto::check_key (ge_frombytes_vartime) does, which is
// RFC 8032 §5.1.3: y is the little-endian value with its top bit cleared, and must be below p; the top bit is the
// sign of x. x = u·v³·(u·v⁷)^((p-5)/8) for u = y²-1 and v = d·y²+1; if v·x² = u that is a square root of u/v, if
// v·x² = -u then x·√-1 is, and otherwise there is none and the key is no point. x = 0 with the sign bit set is refused.
func xmrDecompress(key [32]byte) (edPoint, bool) {
	var be [32]byte
	for i, b := range key {
		be[31-i] = b
	}
	sign := uint(be[0] >> 7)
	be[0] &= 0x7f
	y := new(big.Int).SetBytes(be[:])
	if y.Cmp(edP) >= 0 {
		return edPoint{}, false
	}
	one := big.NewInt(1)
	y2 := edMul(y, y)
	u := edSub(y2, one)
	v := edMod(new(big.Int).Add(edMul(edD, y2), one))
	v3 := edMul(edMul(v, v), v)
	x := edMul(edMul(u, v3), new(big.Int).Exp(edMul(u, edMul(edMul(v3, v3), v)), edRootE, edP))
	switch vx2 := edMul(v, edMul(x, x)); {
	case vx2.Cmp(u) == 0:
	case vx2.Cmp(edSub(big.NewInt(0), u)) == 0:
		x = edMul(x, edSqrtM1)
	default:
		return edPoint{}, false
	}
	if x.Sign() == 0 && sign == 1 {
		return edPoint{}, false
	}
	if x.Bit(0) != sign {
		x = edSub(big.NewInt(0), x)
	}
	return edPoint{x, y, one, edMul(x, y)}, true
}

// edAdd is p+q by RFC 8032 §5.1.4's addition formula, which is complete on ed25519: it also doubles.
func edAdd(p, q edPoint) edPoint {
	a := edMul(edSub(p.y, p.x), edSub(q.y, q.x))
	b := edMul(new(big.Int).Add(p.y, p.x), new(big.Int).Add(q.y, q.x))
	c := edMul(edMul(p.t, edD2), q.t)
	d := edMul(edMul(p.z, big.NewInt(2)), q.z)
	e, f, g, h := edSub(b, a), edSub(d, c), edMod(new(big.Int).Add(d, c)), edMod(new(big.Int).Add(b, a))
	return edPoint{edMul(e, f), edMul(g, h), edMul(f, g), edMul(e, h)}
}

// edScalarMult is k·p by double-and-add.
func edScalarMult(k *big.Int, p edPoint) edPoint {
	r := edPoint{big.NewInt(0), big.NewInt(1), big.NewInt(1), big.NewInt(0)}
	for i := k.BitLen() - 1; i >= 0; i-- {
		if r = edAdd(r, r); k.Bit(i) == 1 {
			r = edAdd(r, p)
		}
	}
	return r
}

// edIdentity reports whether p is the identity (0, 1): X = 0 and Y = Z.
func edIdentity(p edPoint) bool { return p.x.Sign() == 0 && p.y.Cmp(p.z) == 0 }
