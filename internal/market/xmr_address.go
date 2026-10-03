package market

import (
	"crypto/subtle"
	"errors"
	"math/bits"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Offline Monero address parsing for Monero escrow (WR-escrow Phase 3, P3-R2). Monero escrow runs on a local
// fakechain (monerod --regtest), whose wallets use the mainnet address format, so this parser accepts the mainnet
// standard (network byte 18, "4…") and subaddress (42, "8…") forms and nothing else: integrated addresses (19) are
// refused, and so are testnet and stagenet addresses. It is for the fakechain escrow path only; the pooled adapter
// (payments_monero.go) asks its wallet and refuses fakechain. Curve points are not checked: the wallet refuses an
// address whose keys are not points before it builds anything.

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
	errXMRAddressNetwork    = errors.New("not an address of this escrow's network: Monero escrow runs on a local fakechain, whose addresses start with 4 or 8 (testnet and stagenet addresses are refused)")
)

// xmrAddress is a parsed escrow address: its kind (xmrStandard or xmrSubaddress) and its public spend and view keys.
type xmrAddress struct {
	Kind        string
	Spend, View [32]byte
}

// parseXMRAddress parses s exactly (no trimming): 95 base58 characters decoding to the network byte, the spend and
// view keys and the first 4 bytes of their Keccak-256 (Monero's cn_fast_hash).
func parseXMRAddress(s string) (xmrAddress, error) {
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
	var a xmrAddress
	switch data[0] { // a one-byte varint; a multi-byte tag (high bit set) is no network this escrow accepts
	case xmrTagStandard:
		a.Kind = xmrStandard
	case xmrTagSubaddress:
		a.Kind = xmrSubaddress
	case xmrTagIntegrated:
		return xmrAddress{}, errXMRAddressIntegrated
	default:
		return xmrAddress{}, errXMRAddressNetwork
	}
	copy(a.Spend[:], data[1:33])
	copy(a.View[:], data[33:65])
	return a, nil
}

// checkXMRFormAddress is parseXMRAddress for an address typed into a form (a refund or claim address): an integrated
// address, which parseXMRAddress refuses by its length alone (106 characters), is refused as integrated.
func checkXMRFormAddress(s string) error {
	_, err := parseXMRAddress(s)
	if data, ok := xmrBase58Decode(s); err != nil && ok && len(data) == 77 && data[0] == xmrTagIntegrated {
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
