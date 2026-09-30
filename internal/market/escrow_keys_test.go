package market

import (
	"bytes"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// Real Bitcoin Core 31.1 regtest exports (listdescriptors on a throwaway wallet, scratch p1a/fixtures.sh): the BIP48
// account imported as a private descriptor, and the wallet's own BIP84 account. Regtest keys, never funded.
const (
	testBIP48Key = "[7450f935/48h/1h/0h/2h]tpubDExDu3ca3r7v6yDdZqFrmxZbmiTxr3YE4TbAyNdFUiATJdM4q7Rwo1uud32w7vPpAn6hRgxJEWEK3iFjvMHZyFoZyn5EJvTwtMqNRRXXAaL"
	testBIP84Key = "[7450f935/84h/1h/0h]tpubDCxERQzA2G3N1UEa799jFh1tPw7S7aV4YnmUkQGdzkuiVoiShNw2FWuSrVKcydfNMd5P9GoLpHJKAiiV7qHn6HX5BQ179QdtMj2ogSotGBR"
	// The same regtest wallet's private BIP84 descriptor as `listdescriptors true` prints it.
	testPrivDescriptor = "wpkh(tprv8ZgxMBicQKsPfPfXCY5DfrTFb4fTTnkGWSFHAxyYRduJfSPA4GyoTNszFsrDH4aur5ufqBJFghyp7JR8YFrsykLUcQbCVQ5GuWX1GnFKdDv/84h/1h/0h/0/*)#zxgy0fr9"
)

// reversion re-encodes an extended key under another version (the SLIP-132 variants of the same key).
func reversion(t *testing.T, key string, version uint32) string {
	t.Helper()
	raw, err := base58CheckDecode(key)
	if err != nil || len(raw) != 78 {
		t.Fatalf("decode %q: %v", key, err)
	}
	raw[0], raw[1], raw[2], raw[3] = byte(version>>24), byte(version>>16), byte(version>>8), byte(version)
	return base58CheckEncode(raw)
}

func testWIF(testnet, compressed bool) string {
	payload := []byte{0x80}
	if testnet {
		payload[0] = 0xef
	}
	payload = append(payload, bytes.Repeat([]byte{0x11}, 32)...)
	if compressed {
		payload = append(payload, 0x01)
	}
	return base58CheckEncode(payload)
}

func TestBase58CheckRoundTrip(t *testing.T) {
	for _, h := range []string{"", "00", "0000ff", "00010203", "ffffffffffffffff"} {
		b, _ := hex.DecodeString(h)
		enc := base58CheckEncode(b)
		got, err := base58CheckDecode(enc)
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s: %q -> %x %v", h, enc, got, err)
		}
	}
	// BIP32 test vector 1, chain m: a known string decodes to 78 bytes with the xpub version.
	raw, err := base58CheckDecode("xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8")
	if err != nil || len(raw) != 78 || hex.EncodeToString(raw[:4]) != "0488b21e" {
		t.Fatalf("BIP32 vector: %x %v", raw, err)
	}
	for _, bad := range []string{"0", "O", "I", "l", "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet9", strings.Repeat("z", 200)} {
		if _, err := base58CheckDecode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseEscrowKeyAccepts(t *testing.T) {
	k, err := parseEscrowKey(testBIP48Key)
	if err != nil {
		t.Fatal(err)
	}
	if k.Fingerprint != "7450f935" || k.PathText != "/48h/1h/0h/2h" || !k.BIP48 || k.XPub != testBIP48Key[23:] {
		t.Fatalf("parsed %+v", k)
	}
	if k.Expression() != testBIP48Key {
		t.Fatalf("expression %q", k.Expression())
	}
	// Outer whitespace (a copied line break) is trimmed; "'" and "H" read as "h"; an upper-case fingerprint is
	// lower-cased; Core's "/0/*" suffix is accepted and dropped; a Vpub is stored as the same key's tpub.
	vpub := reversion(t, testBIP48Key[23:], 0x02575483)
	for _, in := range []string{
		"  " + testBIP48Key + "\n",
		"[7450F935/48'/1'/0'/2']" + testBIP48Key[23:],
		"[7450f935/48H/1H/0H/2H]" + testBIP48Key[23:],
		testBIP48Key + "/0/*",
		"[7450f935/48h/1h/0h/2h]" + vpub,
	} {
		got, err := parseEscrowKey(in)
		if err != nil || got.Expression() != testBIP48Key || got.KeyMaterial() != k.KeyMaterial() {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	// A Core-native BIP84 account parses but is not BIP48: registering it needs the acknowledgement.
	k84, err := parseEscrowKey(testBIP84Key)
	if err != nil || k84.BIP48 || k84.PathText != "/84h/1h/0h" {
		t.Fatalf("bip84: %+v %v", k84, err)
	}
	// Other BIP48 accounts are plain BIP48; the nested-SegWit script type (1h) and mainnet coin type are not.
	for path, bip48 := range map[string]bool{"/48h/1h/7h/2h": true, "/48h/1h/0h/1h": false, "/48h/0h/0h/2h": false, "/48h/1h/0/2h": false} {
		if got := isBIP48Path(mustPath(t, path)); got != bip48 {
			t.Errorf("isBIP48Path(%s)=%v", path, got)
		}
	}
}

// The Go parser accepts exactly the canonical paths migration 074's CHECK constraints accept (QA-P1a-3): whatever
// parseOriginPath reads, formatOriginPath writes in a form the SQL pattern matches, and a leading zero is refused
// by both.
func TestOriginPathMatchesMigration(t *testing.T) {
	sql, err := migrationFiles.ReadFile("migrations/074_escrow_btc.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`origin_path ~ '([^']+)'`).FindSubmatch(sql)
	if m == nil || !bytes.Contains(sql, []byte("derivation_suffix ~ '"+string(m[1])+"'")) {
		t.Fatal("074's origin_path and derivation_suffix patterns not found or not the same")
	}
	pattern := regexp.MustCompile(string(m[1]))
	for _, in := range []string{"/0", "/48h/1h/0h/2h", "/48'/1'/10'/2'", "/84H/1H/0H", "/2147483647h/0/9", "/1/2/3/4/5/6/7/8/9/10"} {
		p, err := parseOriginPath(in)
		if err != nil || !pattern.MatchString(formatOriginPath(p)) {
			t.Errorf("%s: %q %v", in, formatOriginPath(p), err)
		}
	}
	for _, in := range []string{"/00", "/0/00", "/00/0", "/048h", "/48h/01h", "/0h/007"} {
		if _, err := parseOriginPath(in); err == nil {
			t.Errorf("parseOriginPath accepted %s", in)
		}
		if pattern.MatchString(in) {
			t.Errorf("074's pattern accepts %s", in)
		}
	}
}

func mustPath(t *testing.T, s string) []uint32 {
	t.Helper()
	p, err := parseOriginPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestParseEscrowKeyRefusals is ES-3's refusal table: each input is refused before any RPC with its fixed message,
// which never repeats the input.
func TestParseEscrowKeyRefusals(t *testing.T) {
	tpub := testBIP48Key[23:]
	origin := testBIP48Key[:23]
	tprv := strings.TrimPrefix(strings.Split(testPrivDescriptor, "/")[0], "wpkh(")
	cases := []struct {
		name, in, want string
	}{
		// Private material, detected anywhere in the input and before any syntax rule.
		{"tprv alone", tprv, errKeyExposed.Error()},
		{"tprv with origin", origin + tprv, errKeyExposed.Error()},
		{"private descriptor", testPrivDescriptor, errKeyExposed.Error()},
		{"listdescriptors true JSON", `{"desc":"` + testPrivDescriptor + `","active":true}`, errKeyExposed.Error()},
		{"xprv prefix, bad checksum", "xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHj", errKeyExposed.Error()},
		{"truncated tprv", tprv[:60], errKeyExposed.Error()},
		{"vprv", reversion(t, tprv, 0x045f18bc), errKeyExposed.Error()},
		{"Vprv", reversion(t, tprv, 0x02575048), errKeyExposed.Error()},
		{"zprv", reversion(t, tprv, 0x04b2430c), errKeyExposed.Error()},
		{"WIF testnet compressed", testWIF(true, true), errKeyExposed.Error()},
		{"WIF testnet uncompressed", testWIF(true, false), errKeyExposed.Error()},
		{"WIF mainnet", testWIF(false, true), errKeyExposed.Error()},
		{"12 recovery words", "abandon ability able about above absent absorb abstract absurd abuse access accident", errKeyExposed.Error()},
		{"24 words, commas", strings.Repeat("zoo,", 23) + "wrong", errKeyExposed.Error()},
		// Recovery words as wallets and people write them down (QA-P1a-2): case, numbering, punctuation, hyphens.
		{"capitalised words", "Abandon Ability Able About Above Absent Absorb Abstract Absurd Abuse Access Accident", errKeyExposed.Error()},
		{"all caps words", "ABANDON ABILITY ABLE ABOUT ABOVE ABSENT ABSORB ABSTRACT ABSURD ABUSE ACCESS ACCIDENT", errKeyExposed.Error()},
		{"numbered words", "1. abandon 2. ability 3. able 4. about 5. above 6. absent 7. absorb 8. abstract 9. absurd 10. abuse 11. access 12. accident", errKeyExposed.Error()},
		{"numbered words, no dots", "1 abandon 2 ability 3 able 4 about 5 above 6 absent 7 absorb 8 abstract 9 absurd 10 abuse 11 access 12 accident", errKeyExposed.Error()},
		{"numbered words, one per line", "1) abandon\n2) ability\n3) able\n4) about\n5) above\n6) absent\n7) absorb\n8) abstract\n9) absurd\n10) abuse\n11) access\n12) accident", errKeyExposed.Error()},
		{"words, trailing period", "abandon ability able about above absent absorb abstract absurd abuse access accident.", errKeyExposed.Error()},
		{"words, hyphen joined", "abandon-ability-able-about-above-absent-absorb-abstract-absurd-abuse-access-accident", errKeyExposed.Error()},
		{"11 numbered words stay under the threshold", "1. Abandon 2. Ability 3. Able 4. About 5. Above 6. Absent 7. Absorb 8. Abstract 9. Absurd 10. Abuse 11. Access", errKeySpace.Error()},
		{"numbers alone are not words", "1 2 3 4 5 6 7 8 9 10 11 12 13", errKeySpace.Error()},
		// One key, one line, no descriptor syntax.
		{"inner space", origin + tpub[:20] + " " + tpub[20:], errKeySpace.Error()},
		{"inner newline", origin + "\n" + tpub, errKeySpace.Error()},
		{"tab", origin + "\t" + tpub, errKeySpace.Error()},
		{"comma, two keys", testBIP48Key + "," + testBIP84Key, errKeyComma.Error()},
		{"comma alone", testBIP48Key + ",", errKeyComma.Error()},
		{"parentheses", "wpkh(" + testBIP48Key + "/0/*)", errKeyDescriptor.Error()},
		{"closing parenthesis", testBIP48Key + ")", errKeyDescriptor.Error()},
		{"checksum", testBIP48Key + "#20gdjfqv", errKeyDescriptor.Error()},
		// The origin.
		{"no origin", tpub, errKeyOrigin.Error()},
		{"empty", "", errKeyOrigin.Error()},
		{"short fingerprint", "[7450f93/48h/1h/0h/2h]" + tpub, errKeyOriginForm.Error()},
		{"non-hex fingerprint", "[7450f93z/48h/1h/0h/2h]" + tpub, errKeyOriginForm.Error()},
		{"no path", "[7450f935]" + tpub, errKeyOriginForm.Error()},
		{"empty step", "[7450f935/48h//0h/2h]" + tpub, errKeyOriginForm.Error()},
		{"step too large", "[7450f935/48h/1h/0h/2147483648h]" + tpub, errKeyOriginForm.Error()},
		{"leading zero step", "[7450f935/48h/01h/0h/2h]" + tpub, errKeyOriginForm.Error()},
		{"zero step spelled 00", "[7450f935/48h/1h/00h/2h]" + tpub, errKeyOriginForm.Error()},
		{"unclosed origin", "[7450f935/48h/1h/0h/2h" + tpub, errKeyOriginForm.Error()},
		{"two origins", origin + origin + tpub, errKeyOriginForm.Error()},
		{"path too deep", "[7450f935" + strings.Repeat("/1", 11) + "]" + tpub, errKeyOriginForm.Error()},
		{"path depth differs", "[7450f935/48h/1h/0h]" + tpub, errKeyDepth.Error()},
		{"last step differs", "[7450f935/48h/1h/0h/3h]" + tpub, errKeyDepth.Error()},
		{"last step unhardened", "[7450f935/48h/1h/0h/2]" + tpub, errKeyDepth.Error()},
		// What follows the key.
		{"change chain", testBIP48Key + "/1/*", errKeySuffix.Error()},
		{"fixed child", testBIP48Key + "/0/5", errKeySuffix.Error()},
		{"hardened wildcard", testBIP48Key + "/0/*h", errKeySuffix.Error()},
		// The key itself.
		{"bad checksum", origin + tpub[:len(tpub)-1] + "M", errKeyInvalid.Error()},
		{"truncated", origin + tpub[:60], errKeyInvalid.Error()},
		{"non-base58", origin + tpub[:50] + "0" + tpub[51:], errKeyInvalid.Error()},
		{"mainnet xpub", origin + reversion(t, tpub, 0x0488b21e), errKeyMainnet.Error()},
		{"mainnet Zpub", origin + reversion(t, tpub, 0x02aa7ed3), errKeyMainnet.Error()},
		{"mainnet zpub", origin + reversion(t, tpub, 0x04b24746), errKeyMainnet.Error()},
		{"SLIP-132 vpub", origin + reversion(t, tpub, 0x045f1cf6), errKeySingleSig.Error()},
		{"SLIP-132 upub", origin + reversion(t, tpub, 0x044a5262), errKeySingleSig.Error()},
		{"SLIP-132 Upub", origin + reversion(t, tpub, 0x024289ef), errKeyNested.Error()},
		{"unknown version", origin + reversion(t, tpub, 0x01020304), errKeyVersion.Error()},
		{"too long", origin + strings.Repeat("A", 400), errKeyLong.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseEscrowKey(c.in)
			var r *keyRefusal
			if err == nil || !errors.As(err, &r) || err.Error() != c.want {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if len(c.in) > 12 && strings.Contains(err.Error(), c.in[len(c.in)-12:]) {
				t.Fatalf("message repeats the input: %q", err)
			}
		})
	}
}

// A master key (depth 0), a key whose point is not on the curve and a key with an invalid prefix byte are refused.
func TestParseEscrowKeyRefusesBadKeyMaterial(t *testing.T) {
	raw, err := base58CheckDecode(testBIP48Key[23:])
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(b []byte)) string {
		b := bytes.Clone(raw)
		f(b)
		return base58CheckEncode(b)
	}
	offCurve := mutate(func(b []byte) {
		for x := byte(0); ; x++ { // find an x with no point: about half of all x values
			b[77] = x
			if !onCurve(b[45:78]) {
				return
			}
		}
	})
	if _, err := parseEscrowKey(testBIP48Key[:23] + offCurve); err == nil || err.Error() != errKeyInvalid.Error() {
		t.Fatalf("off-curve: %v", err)
	}
	badPrefix := mutate(func(b []byte) { b[45] = 0x04 })
	if _, err := parseEscrowKey(testBIP48Key[:23] + badPrefix); err == nil || err.Error() != errKeyInvalid.Error() {
		t.Fatalf("prefix 04: %v", err)
	}
	master := mutate(func(b []byte) { b[4] = 0; copy(b[5:13], make([]byte, 8)) })
	if _, err := parseEscrowKey("[7450f935]" + master); err == nil || err.Error() != errKeyOriginForm.Error() {
		t.Fatalf("master: %v", err)
	}
}

// FuzzParseEscrowKey: no input panics, and whatever is accepted round-trips to a canonical expression that parses to
// the same key.
func FuzzParseEscrowKey(f *testing.F) {
	for _, s := range []string{testBIP48Key, testBIP84Key, testPrivDescriptor, testBIP48Key + "/0/*", "[", "[]", "[00000000/0]", testWIF(true, true)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		k, err := parseEscrowKey(s)
		if err != nil {
			return
		}
		again, err := parseEscrowKey(k.Expression())
		if err != nil || again.KeyMaterial() != k.KeyMaterial() || again.Expression() != k.Expression() {
			t.Fatalf("%q: canonical %q does not round-trip: %v", s, k.Expression(), err)
		}
	})
}
