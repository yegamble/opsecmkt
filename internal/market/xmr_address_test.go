package market

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

// testXMREncode is Monero's base58 encoder (8-byte blocks to 11 characters, a shorter last block by
// xmrBase58BlockSizes). It lives in the tests only: the market never encodes an address. It is checked against
// Monero's own vectors below, so a decoder bug cannot hide behind a matching encoder bug.
func testXMREncode(data []byte) string {
	var b strings.Builder
	for len(data) > 0 {
		n := min(8, len(data))
		var num uint64
		for _, c := range data[:n] {
			num = num<<8 | uint64(c)
		}
		out := make([]byte, xmrBase58BlockSizes[n])
		for i := len(out) - 1; i >= 0; i-- {
			out[i] = xmrBase58Alphabet[num%58]
			num /= 58
		}
		b.Write(out)
		data = data[n:]
	}
	return b.String()
}

// testXMRAddressWith encodes tag, spend, view and extra (an integrated address's payment ID) with the Keccak
// checksum, or a wrong one.
func testXMRAddressWith(tag byte, spend, view, extra []byte, badChecksum bool) string {
	data := append(append(append([]byte{tag}, spend...), view...), extra...)
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	sum := h.Sum(nil)[:4]
	if badChecksum {
		sum[0] ^= 1
	}
	return testXMREncode(append(data, sum...))
}

// testXMRKey is a deterministic public key for a label: the ed25519 public key crypto/ed25519 derives from the
// label's SHA-256 as its seed. That is a clamped scalar times the base point, so it lies in the prime-order subgroup,
// as every real Monero key does, and parseXMRAddress accepts it (A-417).
func testXMRKey(label string) []byte {
	seed := sha256.Sum256([]byte(label))
	return ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
}

// testXMROffCurveKey is a key that is no curve point: y = 2, for which x² = (y²-1)/(dy²+1) has no square root (the
// smallest such y, found by search with Euler's criterion; TestXMRPointDecompress pins it).
func testXMROffCurveKey() []byte { return append([]byte{2}, make([]byte, 31)...) }

// testXMRSmallOrderKey is the point of order 2, (0, -1): y = p-1. Monero's check_key accepts it; parseXMRAddress
// does not.
func testXMRSmallOrderKey() []byte {
	return append([]byte{0xec}, append(bytes.Repeat([]byte{0xff}, 30), 0x7f)...)
}

// testXMRAddr is a valid fakechain escrow address (mainnet format): a subaddress, or a standard address.
func testXMRAddr(label string, sub bool) string {
	tag := byte(xmrTagStandard)
	if sub {
		tag = xmrTagSubaddress
	}
	return testXMRAddressWith(tag, testXMRKey(label+"/spend"), testXMRKey(label+"/view"), nil, false)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestXMRBase58MoneroVectors: block and whole-string vectors from Monero's tests/unit_tests/base58.cpp
// (monero-project/monero), decoded by the market and re-encoded by the test encoder.
func TestXMRBase58MoneroVectors(t *testing.T) {
	ff := func(n int) string { return strings.Repeat("ff", n) }
	pos := map[string]string{
		"":                       "",
		"11":                     "00",
		"5Q":                     ff(1),
		"LUv":                    ff(2),
		"2UzHL":                  ff(3),
		"7YXq9G":                 ff(4),
		"VtB5VXc":                ff(5),
		"3CUsUpv9t":              ff(6),
		"Ahg1opVcGW":             ff(7),
		"jpXCZedGfVQ":            ff(8),
		"jpXCZedGfVQ5Q":          ff(9),
		"jpXCZedGfVQVtB5VXc":     ff(13),
		"jpXCZedGfVQjpXCZedGfVQ": ff(16),
		"11111111111":            "0000000000000000",
		"11111111121":            "000000000000003a",
		"1Ahg1opVcGW":            "00" + ff(7),
		"1z111111111":            "05e022ba374b2a00",
		"22222222222VtB5VXc":     "06156013762879f7" + ff(5),
	}
	for enc, want := range pos {
		got, ok := xmrBase58Decode(enc)
		if !ok || hex.EncodeToString(got) != want {
			t.Errorf("decode %q = %x, %v; want %s", enc, got, ok, want)
		}
		if again := testXMREncode(mustHex(t, want)); again != enc {
			t.Errorf("encode %s = %q, want %q", want, again, enc)
		}
	}
	// Invalid lengths (1, 4 and 8 characters are no block's size), overflow, and characters outside the alphabet.
	for _, enc := range []string{"1", "z", "1111", "zzzz", "11111111", "123456789AB1", "123456789AB1111", "5R", "zz", "LUw",
		"2UzHM", "7YXq9H", "VtB5VXd", "3CUsUpv9u", "Ahg1opVcGX", "jpXCZedGfVR", "zzzzzzzzzzz", "123456789ABjpXCZedGfVR",
		"123456789AB5R", "10", "11I", "11O11", "11l111", "11_11111111", "1101111111111", "11 11111111", "11\n"} {
		if got, ok := xmrBase58Decode(enc); ok {
			t.Errorf("decode %q accepted: %x", enc, got)
		}
	}
}

// TestXMRAddressExternalVectors: two addresses this code did not produce. Monero's own unit-test address with its
// published keys (tests/unit_tests/base58.cpp, test_keys_addr_str; never a destination for funds), and the Monero
// project's general-fund address (getmonero.org, parse only).
func TestXMRAddressExternalVectors(t *testing.T) {
	const moneroTest = "4AzKEX4gXdJdNeM6dfiBFL7kqund3HYGvMBF3ttsNd9SfzgYB6L7ep1Yg1osYJzLdaKAYSLVh6e6jKnAuzj3bw1oGy9kXCb"
	spend := "f724bc5c6cfbb9d97602c300423a2f28641874513a035778a0c1778d833201e9"
	view := "220939689edf1abd5bc1d031f73ecd6c993add66d6808870456afeb8e7eeb68d"
	a, err := parseXMRAddress(moneroTest)
	if err != nil || a.Kind != xmrStandard || hex.EncodeToString(a.Spend[:]) != spend || hex.EncodeToString(a.View[:]) != view {
		t.Fatalf("Monero test address: %+v %v", a, err)
	}
	if got := testXMRAddressWith(xmrTagStandard, mustHex(t, spend), mustHex(t, view), nil, false); got != moneroTest {
		t.Fatalf("test encoder: %s", got)
	}
	const generalFund = "44AFFq5kSiGBoZ4NMDwYtN18obc8AemS33DBLWs3H7otXft3XjrpDtQGv7SqSsaBYBb98uNbr2VBBEt7f2wfn3RVGQBEP3A"
	if a, err := parseXMRAddress(generalFund); err != nil || a.Kind != xmrStandard {
		t.Fatalf("general fund address: %+v %v", a, err)
	}
}

// TestXMRAddressMoneroClassified (QA-XB-1): real addresses whose network and kind Monero itself states, so a wrong
// network-byte constant shared by the parser and the test encoder cannot pass. The chair's fakechain probe on
// monero-wallet-rpc 0.18.5.1 (its primary address and a create_account subaddress), and the table in Monero's
// tests/functional_tests/validate_address.py (nettype and kind as validate_address reports them; the openalias
// subaddress of donate@getmonero.org). Keys are not published for these, so only the kind is checked.
func TestXMRAddressMoneroClassified(t *testing.T) {
	cases := []struct {
		addr, kind string
		err        error
	}{
		// fakechain probe, 2026-10-03
		{"49JSufxNBTCLdByNX6iKuRQr3qQxrB75KCnGwtvtb28s5eE4fNJsWygBDFFMhxEAc5E2jk1o8bk8NZWMHGyR4M5ELetmAY8", xmrStandard, nil},
		{"8C9P9WWEG9GURMVjNQ7PKMLY87A77igVc33jYpHzxewGFw1F5nkrjd8Lb6i2RUXgimL12HTkbPDf7ZSmQG9wocmrTznxVeK", xmrSubaddress, nil},
		// validate_address.py: mainnet standard, mainnet subaddresses
		{"42ey1afDFnn4886T7196doS9GPMzexD9gXpsZJDwVjeRVdFCSoHnv7KPbBeGpzJBzHRCAs9UxqeoyFQMYbqSWYTfJJQAWDm", xmrStandard, nil},
		{"44Kbx4sJ7JDRDV5aAhLJzQCjDz2ViLRduE3ijDZu3osWKBjMGkV1XPk4pfDUMqt1Aiezvephdqm6YD19GKFD9ZcXVUTp6BW", xmrStandard, nil},
		{"8AsN91rznfkBGTY8psSNkJBg9SZgxxGGRUhGwRptBhgr5XSQ1XzmA9m8QAnoxydecSh5aLJXdrgXwTDMMZ1AuXsN1EX5Mtm", xmrSubaddress, nil},
		{"86kKnBKFqzCLxtK1Jmx2BkNBDBSMDEVaRYMMyVbeURYDWs8uNGDZURKCA5yRcyMxHzPcmCf1q2fSdhQVcaKsFrtGRsdGfNk", xmrSubaddress, nil},
		{"888tNkZrPN6JsEgekjMnABU4TBzc2Dt29EPAvkRxbANsAnjyPbb3iQ1YBRk1UXcdRsiKc9dhwMVgN5S9cQUiyoogDavup3H", xmrSubaddress, nil},
		// validate_address.py: the bad address (last character changed), testnet and stagenet standard and subaddresses
		{"42ey1afDFnn4886T7196doS9GPMzexD9gXpsZJDwVjeRVdFCSoHnv7KPbBeGpzJBzHRCAs9UxqeoyFQMYbqSWYTfJJQAWD9", "", errXMRAddressChecksum},
		{"9ujeXrjzf7bfeK3KZdCqnYaMwZVFuXemPU8Ubw335rj2FN1CdMiWNyFV3ksEfMFvRp9L9qum5UxkP5rN9aLcPxbH1au4WAB", "", errXMRAddressNetwork},
		{"53teqCAESLxeJ1REzGMAat1ZeHvuajvDiXqboEocPaDRRmqWoVPzy46GLo866qRFjbNhfkNckyhST3WEvBviDwpUDd7DSzB", "", errXMRAddressNetwork},
		{"BdKg9udkvckC5T58a8Nmtb6BNsgRAxs7uA2D49sWNNX5HPW5Us6Wxu8QMXrnSx3xPBQQ2iu9kwEcRGAoiz6EPmcZKbF62GS", "", errXMRAddressNetwork},
		{"BcFvPa3fT4gVt5QyRDe5Vv7VtUFao9ci8NFEy3r254KF7R1N2cNB5FYhGvrHbMStv4D6VDzZ5xtxeKV8vgEPMnDcNFuwZb9", "", errXMRAddressNetwork},
		{"73LhUiix4DVFMcKhsPRG51QmCsv8dYYbL6GcQoLwEEFvPvkVvc7BhebfA4pnEFF9Lq66hwvLqBvpHjTcqvpJMHmmNjPPBqa", "", errXMRAddressNetwork},
		{"7A1Hr63MfgUa8pkWxueD5xBqhQczkusYiCMYMnJGcGmuQxa7aDBxN1G7iCuLCNB3VPeb2TW7U9FdxB27xKkWKfJ8VhUZthF", "", errXMRAddressNetwork},
		// validate_address.py: integrated addresses on each network (106 characters)
		{"4BxSHvcgTwu25WooY4BVmgdcKwZu5EksVZSZkDd6ooxSVVqQ4ubxXkhLF6hEqtw96i9cf3cVfLw8UWe95bdDKfRQeYtPwLm1Jiw7AKt2LY", "", errXMRAddressLength},
		{"AApMA1VuhiCaHzr5X2KXi2Zc9oJ3VaGjkfChxxpRpxkyKf1NetvbRbQTbFMrGkr85DjnEH7JsBaoUFsgKwZnmtnVWnoB8MDotCsLb7eWwz", "", errXMRAddressLength},
		{"5K8mwfjumVseCcQEjNbf59Um6R9NfVUNkHTLhhPCmNvgDLVS88YW5tScnm83rw9mfgYtchtDDTW5jEfMhygi27j1QYphX38hg6m4VMtN29", "", errXMRAddressLength},
	}
	for _, c := range cases {
		a, err := parseXMRAddress(c.addr)
		if !errors.Is(err, c.err) || a.Kind != c.kind {
			t.Errorf("%s…: kind %q, %v; want %q, %v", c.addr[:8], a.Kind, err, c.kind, c.err)
		}
	}
	// The integrated address decodes with a valid checksum and the mainnet integrated byte: refused for its kind.
	if data, ok := xmrBase58Decode(cases[14].addr); !ok || len(data) != 77 || data[0] != 19 { // Monero's CRYPTONOTE_PUBLIC_INTEGRATED_ADDRESS_BASE58_PREFIX
		t.Fatalf("integrated vector: %v %d", ok, len(data))
	}
}

func TestXMRAddressRoundTrip(t *testing.T) {
	for _, c := range []struct {
		tag  byte
		kind string
	}{{xmrTagStandard, xmrStandard}, {xmrTagSubaddress, xmrSubaddress}} {
		for _, label := range []string{"a", "b", "escrow-202610-01/3"} {
			spend, view := testXMRKey(label+"/s"), testXMRKey(label+"/v")
			s := testXMRAddressWith(c.tag, spend, view, nil, false)
			a, err := parseXMRAddress(s)
			if err != nil || a.Kind != c.kind || string(a.Spend[:]) != string(spend) || string(a.View[:]) != string(view) {
				t.Fatalf("%d %s: %+v %v", c.tag, label, a, err)
			}
			if len(s) != 95 || (c.tag == xmrTagStandard) != (s[0] == '4') || (c.tag == xmrTagSubaddress) != (s[0] == '8') {
				t.Fatalf("unexpected form %s", s)
			}
		}
	}
}

// TestXMRAddressRefusals: every refusal has its fixed error and none quotes the address.
func TestXMRAddressRefusals(t *testing.T) {
	spend, view := testXMRKey("r/s"), testXMRKey("r/v")
	good := testXMRAddressWith(xmrTagStandard, spend, view, nil, false)
	cases := []struct {
		name, addr string
		want       error
	}{
		{"bad checksum", testXMRAddressWith(xmrTagStandard, spend, view, nil, true), errXMRAddressChecksum},
		{"bad checksum, subaddress", testXMRAddressWith(xmrTagSubaddress, spend, view, nil, true), errXMRAddressChecksum},
		{"one character short", good[:94], errXMRAddressLength},
		{"one character long", good + "1", errXMRAddressLength},
		{"empty", "", errXMRAddressLength},
		{"integrated (106 characters)", testXMRAddressWith(xmrTagIntegrated, spend, view, testXMRKey("pid")[:8], false), errXMRAddressLength},
		{"integrated network byte", testXMRAddressWith(xmrTagIntegrated, spend, view, nil, false), errXMRAddressIntegrated},
		{"testnet standard", testXMRAddressWith(53, spend, view, nil, false), errXMRAddressNetwork},
		{"testnet integrated byte", testXMRAddressWith(54, spend, view, nil, false), errXMRAddressNetwork},
		{"testnet subaddress", testXMRAddressWith(63, spend, view, nil, false), errXMRAddressNetwork},
		{"stagenet standard", testXMRAddressWith(24, spend, view, nil, false), errXMRAddressNetwork},
		{"stagenet integrated byte", testXMRAddressWith(25, spend, view, nil, false), errXMRAddressNetwork},
		{"stagenet subaddress", testXMRAddressWith(36, spend, view, nil, false), errXMRAddressNetwork},
		{"multi-byte network tag", testXMRAddressWith(0x92, spend, view, nil, false), errXMRAddressNetwork},
		{"zero (not base58)", "0" + good[1:], errXMRAddressEncoding},
		{"capital O", good[:40] + "O" + good[41:], errXMRAddressEncoding},
		{"capital I", good[:40] + "I" + good[41:], errXMRAddressEncoding},
		{"lower-case l", good[:40] + "l" + good[41:], errXMRAddressEncoding},
		{"leading space", " " + good[1:], errXMRAddressEncoding},
		{"trailing newline", good[:94] + "\n", errXMRAddressEncoding},
		{"inner space", good[:50] + " " + good[51:], errXMRAddressEncoding},
		{"padded with spaces", " " + good + " ", errXMRAddressLength},
		{"block overflow", good[:88] + "zzzzzzz", errXMRAddressEncoding},
	}
	for _, c := range cases {
		_, err := parseXMRAddress(c.addr)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if err != nil && len(c.addr) > 12 && strings.Contains(err.Error(), strings.TrimSpace(c.addr)[:12]) {
			t.Errorf("%s: error quotes the address: %v", c.name, err)
		}
	}
	if len(cases[5].addr) != 106 {
		t.Fatalf("integrated vector has %d characters", len(cases[5].addr))
	}
}

// testKey32 is a 32-byte key from its hex encoding.
func testKey32(t *testing.T, s string) (k [32]byte) {
	t.Helper()
	if copy(k[:], mustHex(t, s)) != 32 || len(s) != 64 {
		t.Fatalf("not a 32-byte key: %s", s)
	}
	return k
}

// testEdEncode encodes the affine point (x, y): y little-endian with the parity of x (its sign) in the top bit
// (RFC 8032 §5.1.2). The market never encodes a point.
func testEdEncode(x, y *big.Int) [32]byte {
	var k [32]byte
	y.FillBytes(k[:])
	slices.Reverse(k[:])
	k[31] |= byte(x.Bit(0)) << 7
	return k
}

// testEdAffine is p's affine coordinates (x, y) = (X/Z, Y/Z).
func testEdAffine(p edPoint) (x, y *big.Int) {
	zi := new(big.Int).ModInverse(p.z, edP)
	return edMul(p.x, zi), edMul(p.y, zi)
}

// testEdBase is the base point B: y = 4/5 (RFC 8032 §5.1), x positive.
const testEdBase = "5866666666666666666666666666666666666666666666666666666666666666"

// TestXMRPointValid (A-417): keys that are points of the prime-order subgroup are valid. The public keys of RFC 8032
// §7.1 (TEST 1, 2, 3, 1024 and SHA(abc)), each re-derived from its published secret key by crypto/ed25519 so a typing
// error in a vector cannot pass; the base point; the keys of Monero's unit-test address (base58.cpp); and 256 public
// keys crypto/ed25519 derives (each a clamped scalar times B, from an implementation independent of this one).
func TestXMRPointValid(t *testing.T) {
	rfc8032 := map[string]string{
		"9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60": "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
		"4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb": "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
		"c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7": "fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025",
		"f5e5767cf153319517630f226876b86c8160cc583bc013744c6bf255f5cc0ee5": "278117fc144c72340f67d0f2316e8386ceffbf2b2428c9c51fef7c597f1d426e",
		"833fe62409237b9d62ec77587520911e9a759cec1d19755b7da901b96dca3d42": "ec172b93ad5e563bf4932c70e1245034c35467ef2efd4d64ebf819683467e2bf",
	}
	for seed, pub := range rfc8032 {
		if got := hex.EncodeToString(ed25519.NewKeyFromSeed(mustHex(t, seed)).Public().(ed25519.PublicKey)); got != pub {
			t.Fatalf("RFC 8032 vector %s: crypto/ed25519 derives %s", pub, got)
		}
		if !xmrPointValid(testKey32(t, pub)) {
			t.Errorf("RFC 8032 public key %s refused", pub)
		}
	}
	for _, k := range []string{testEdBase, "f724bc5c6cfbb9d97602c300423a2f28641874513a035778a0c1778d833201e9", "220939689edf1abd5bc1d031f73ecd6c993add66d6808870456afeb8e7eeb68d"} {
		if !xmrPointValid(testKey32(t, k)) {
			t.Errorf("%s refused", k)
		}
	}
	for i := range 256 {
		seed := sha256.Sum256([]byte{byte(i)})
		if k := [32]byte(ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)); !xmrPointValid(k) {
			t.Errorf("crypto/ed25519 public key %x refused", k)
		}
	}
}

// TestXMRPointDecompress (A-417): xmrDecompress is Monero's crypto::check_key (ge_frombytes_vartime) exactly. Vectors:
// check_key lines of Monero's tests/crypto/tests.txt (monero-project/monero v0.18.4.0, the first eight of each
// result), and edge cases: y = p, y = p+1 and the largest 255-bit y (non-canonical), y = 2 (no square root; the
// smallest y >= 2 without one, by search), x = 0 with the sign bit set (y = 1, the identity's y, and y = p-1), and
// the identity and the two order-4 points (y = 0), which decompress.
func TestXMRPointDecompress(t *testing.T) {
	cases := map[string]bool{
		// tests.txt: check_key true
		"bd85a61bae0c101d826cbed54b1290f941d26e70607a07fc6f0ad611eb8f70a6": true,
		"6016a5463b9e5a58c3410d3f892b76278883473c3f0b69459172d3de49e85abe": true,
		"69393c25c3b50e177f81f20f852dd604e768eb30052e23108b3cfa1a73f2736e": true,
		"3d5a89b676cb84c2be3428d20a660dc6a37cae13912e127888a5132e8bac2163": true,
		"e03b2414e260580f86ee294cd4c636a5b153e617f704e81dad248fbf715b2ee4": true,
		"556381485df0d7d5a268ab5ecfb2984b060acc63471183fcf538bf273b0c0cb5": true,
		"c7f76d82ac64b1e7fdc32761ff00d6f0f7ada4cf223aa5a11187e3a02e1d5319": true,
		"cfa85d8bdb6f633fcf031adee3a299ac42eeb6bd707744049f652f6322f5aa47": true,
		// tests.txt: check_key false
		"c2cb3cf3840aa9893e00ec77093d3d44dba7da840b51c48462072d58d8efd183": false,
		"328f81cad4eba24ab2bad7c0e56b1e2e7346e625bcb06ae649aef3ffa0b8bef3": false,
		"4c71282b2add07cdc6898a2622553f1ca4eb851e5cb121181628be5f3814c5b1": false,
		"78cd665deb28cebc6208f307734c56fccdf5fa7e2933fadfcdd2b6246e9ae95c": false,
		"28c3503ce82d7cdc8e0d96c4553bcf0352bbcfc73925495dbe541e7e1df105fc": false,
		"06855c3c3e0d03fec354059bda319b39916bdc10b6581e3f41b335ee7b014fd5": false,
		"7338df494bc416cf5edcc02069e067f39cb269ce67bd9faba956021ce3b3de3a": false,
		"f9a1f27b1618342a558379f4815fa5039a8fe9d98a09f45c1af857ba99231dc1": false,
		// edge cases
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": false, // y = p
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": false, // y = p+1, the identity's y non-canonically
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": false, // y = 2^255-1
		"0200000000000000000000000000000000000000000000000000000000000000": false, // y = 2: no square root
		"0100000000000000000000000000000000000000000000000000000000000080": false, // y = 1, x = 0 with the sign bit
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff": false, // y = p-1, x = 0 with the sign bit
		"0100000000000000000000000000000000000000000000000000000000000000": true,  // the identity
		"0000000000000000000000000000000000000000000000000000000000000000": true,  // y = 0, x = √-1 (order 4)
		"0000000000000000000000000000000000000000000000000000000000000080": true,  // y = 0, x = -√-1 (order 4)
		testEdBase: true,
	}
	for k, want := range cases {
		if _, ok := xmrDecompress(testKey32(t, k)); ok != want {
			t.Errorf("%s: decompresses %v, want %v", k, ok, want)
		}
	}
	if hex.EncodeToString(testXMROffCurveKey()) != "0200000000000000000000000000000000000000000000000000000000000000" ||
		hex.EncodeToString(testXMRSmallOrderKey()) != "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f" {
		t.Fatal("the test helpers' bad keys changed")
	}
	// Decompression returns the point itself: B is (x, 4/5) with x even and re-encodes to its key; the sign bit gives -B.
	b, _ := xmrDecompress(testKey32(t, testEdBase))
	x, y := testEdAffine(b)
	if testEdEncode(x, y) != testKey32(t, testEdBase) || y.Cmp(edMul(big.NewInt(4), new(big.Int).ModInverse(big.NewInt(5), edP))) != 0 || x.Bit(0) != 0 {
		t.Fatalf("B decompressed to (%v, %v)", x, y)
	}
	neg := testKey32(t, testEdBase)
	neg[31] |= 0x80
	if nb, ok := xmrDecompress(neg); !ok || nb.x.Cmp(new(big.Int).Sub(edP, b.x)) != 0 || nb.y.Cmp(b.y) != 0 {
		t.Fatal("-B")
	}
}

// TestXMRPointPrimeOrder (A-417): a key is valid only in the prime-order subgroup (l·P is the identity and P is not):
// monero-wallet-rpc 0.18.5.1 crashed building a transfer to keys that pass check_key with a small-order component.
// The 8 points of small order are refused, and so is B plus each kind of small-order point: B+T2 = (-x, -y) and
// B+T4 = (√-1·y, √-1·x), both by the addition law worked by hand, and B+T8 by edAdd.
func TestXMRPointPrimeOrder(t *testing.T) {
	small := []string{
		"0100000000000000000000000000000000000000000000000000000000000000", // identity (order 1)
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // (0, -1), order 2
		"0000000000000000000000000000000000000000000000000000000000000000", // (√-1, 0), order 4
		"0000000000000000000000000000000000000000000000000000000000000080", // (-√-1, 0), order 4
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", // order 8
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85", // order 8
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a", // order 8
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa", // order 8
	}
	for _, k := range small {
		p, ok := xmrDecompress(testKey32(t, k))
		if !ok || !edIdentity(edScalarMult(big.NewInt(8), p)) {
			t.Fatalf("%s is not a point of small order", k)
		}
		if xmrPointValid(testKey32(t, k)) {
			t.Errorf("small-order point %s accepted", k)
		}
	}
	b, _ := xmrDecompress(testKey32(t, testEdBase))
	t8, _ := xmrDecompress(testKey32(t, small[4]))
	bx, by := testEdAffine(b)
	torsioned := map[string][32]byte{
		"B+T2": testEdEncode(new(big.Int).Sub(edP, bx), new(big.Int).Sub(edP, by)),
		"B+T4": testEdEncode(edMul(edSqrtM1, by), edMul(edSqrtM1, bx)),
		"B+T8": testEdEncode(testEdAffine(edAdd(b, t8))),
	}
	for name, k := range torsioned {
		if _, ok := xmrDecompress(k); !ok {
			t.Fatalf("%s is not a point", name)
		}
		if xmrPointValid(k) {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestXMRAddressKeysRefused (A-417): an address with a valid checksum whose spend or view key is not a point, or not
// a point of the prime-order subgroup, is refused with errXMRAddressKeys, which does not quote it.
func TestXMRAddressKeysRefused(t *testing.T) {
	good := testXMRKey("k")
	for _, tag := range []byte{xmrTagStandard, xmrTagSubaddress} {
		for name, keys := range map[string][2][]byte{
			"off-curve spend key":   {testXMROffCurveKey(), good},
			"off-curve view key":    {good, testXMROffCurveKey()},
			"small-order spend key": {testXMRSmallOrderKey(), good},
			"small-order view key":  {good, testXMRSmallOrderKey()},
		} {
			s := testXMRAddressWith(tag, keys[0], keys[1], nil, false)
			if _, err := parseXMRAddress(s); !errors.Is(err, errXMRAddressKeys) || strings.Contains(err.Error(), s[:12]) {
				t.Errorf("%d, %s: %v", tag, name, err)
			}
		}
		if _, err := parseXMRAddress(testXMRAddressWith(tag, good, good, nil, false)); err != nil {
			t.Fatalf("%d, valid keys: %v", tag, err)
		}
	}
}
