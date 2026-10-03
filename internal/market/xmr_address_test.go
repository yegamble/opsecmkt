package market

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// testXMRKey is a deterministic 32-byte key for a label (not a curve point; the parser does not check points).
func testXMRKey(label string) []byte { s := sha256.Sum256([]byte(label)); return s[:] }

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
