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
