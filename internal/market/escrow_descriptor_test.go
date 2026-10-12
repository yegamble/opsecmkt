package market

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// Escrow P1-b pure checks: descriptor checksums (BIP 380), the party origin rule (the chair's ruling (b) on QA25's
// carry-over: origin_path is the exact account origin, derivation_suffix the child path below it, and no split
// reaches the /0/0 practice child) and the witness-script check (ES-3).

// Checksums Bitcoin Core 31.1 printed on regtest (getdescriptorinfo, listdescriptors).
func TestDescriptorChecksum(t *testing.T) {
	for _, c := range []string{
		"addr(bcrt1qw508d6qejxtdg4y5r3zarvary0c5xw7kygt080)#8pk5s7ya",
		"addr(mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn)#eccnnjdy",
		"pkh([4aa624d3/44h/1h/0h]tpubDCdf9GxSRppCMRo8xj99cBsoMHNf1vft2zA2TjhsQhsFkPuWuEXsrUMJM6vXrwUAxoJtdL4mhFA2NZFaFYVE1GFGV3LbvLTrLadXARMjHD6/0/*)#6zwlnu2x",
		"wpkh([4aa624d3/84h/1h/0h]tpubDDFM72yuCRKHokD6cnMvGZAf2vqAWTvLj8Wbi2tNPGbV1JgD6c2hUc2Xh7PoPbeRtfuhMFEqF5TGpn3sYUyHULdzEFXYQDkDc1xBEUyJabL/1/*)#qp7lsjne",
		"tr([4aa624d3/86h/1h/0h]tpubDCYLRGkQnm2Z8acvCtJFF89e8g9wxS6XHbULKWzYmRWbKREKKBVhXEhQbJcE1DrC5UGdLtJhtrev1rhfAi5h7ijmUjeGcL1ydoZEuRwQqTE/0/*)#2dykdagw",
	} {
		desc, want, _ := strings.Cut(c, "#")
		if got, ok := descChecksum(desc); !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", desc, got, ok, want)
		}
	}
	if _, ok := descChecksum("addr(é)"); ok {
		t.Error("a character outside the descriptor alphabet got a checksum")
	}
}

// The party child path (ruling (b)): origin is exactly the registered account path, the suffix is /0/<i> with
// 1 <= i < 2^31, and nothing reaches the practice child <account>/0/0 (ES-9).
func TestEscrowPartySuffixBoundaries(t *testing.T) {
	k, err := parseEscrowKey(testBIP48Key, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		origin, suffix string
		ok             bool
	}{
		{"/48h/1h/0h/2h", "/0/1", true},
		{"/48h/1h/0h/2h", "/0/2147483647", true},
		{"/48h/1h/0h/2h", "/0/0", false},              // the practice child
		{"/48h/1h/0h/2h", "/0/00", false},             // /0/0 spelled otherwise
		{"/48h/1h/0h/2h", "/0/2147483648", false},     // hardened range
		{"/48h/1h/0h/2h", "/0/1h", false},             // hardened: not derivable from a public key
		{"/48h/1h/0h/2h", "/1/1", false},              // change branch
		{"/48h/1h/0h/2h", "/0", false},                // one step
		{"/48h/1h/0h/2h", "/0/1/0", false},            // three steps
		{"/48h/1h/0h/2h", "", false},                  // none
		{"/48h/1h/0h/2h/0", "/0", false},              // origin + suffix = <account>/0/0 split differently
		{"/48h/1h/0h/2h/0", "/1", false},              // origin is not the registered account path
		{"/48h/1h/0h", "/2h/0/1", false},              // shorter origin, hardened step in the suffix
		{"/48h/1h/0h/2h", "/0/-1", false},             // not a number
		{"/48'/1'/0'/2'", "/0/1", false},              // origin not canonical
		{"/48h/1h/1h/2h", "/0/1", false},              // another account
		{"/48h/1h/0h/2h", "/0/1 ", false},             // trailing space
		{"/48h/1h/0h/2h", "/0/99999999999999", false}, // overflow
	} {
		err := checkEscrowOrigin(k, c.origin, c.suffix)
		if (err == nil) != c.ok {
			t.Errorf("origin %q suffix %q: %v, want ok=%v", c.origin, c.suffix, err, c.ok)
		}
	}
	if s := escrowChildSuffix(1); s != "/0/1" || checkEscrowOrigin(k, k.PathText, s) != nil {
		t.Fatalf("escrowChildSuffix(1) = %q", s)
	}
	for _, i := range []int64{0, -1, 1 << 31} {
		if checkEscrowOrigin(k, k.PathText, escrowChildSuffix(i)) == nil {
			t.Errorf("index %d accepted", i)
		}
	}
}

func testPub(b byte) []byte {
	for i := 0; ; i++ {
		h := sha256.Sum256([]byte{b, byte(i)})
		pub := append([]byte{2 + b%2}, h[:]...)
		if onCurve(pub) {
			return pub
		}
	}
}

func multisigScript(keys ...[]byte) []byte {
	ws := []byte{0x52}
	for _, k := range keys {
		ws = append(append(ws, byte(len(k))), k...)
	}
	return append(ws, 0x53, 0xae)
}

func p2wsh(ws []byte) []byte {
	h := sha256.Sum256(ws)
	return append([]byte{0, 32}, h[:]...)
}

// The witness-script check (ES-3): three distinct compressed keys in sorted order, each with a derivation, exactly
// one derived from the pinned arbiter fingerprint, every party key at its expected origin, and the script is the
// one the address pays.
func TestEscrowWitnessCheck(t *testing.T) {
	buyer, vendor, market := testPub(1), testPub(2), testPub(3)
	fpB, fpV, fpM := "11111111", "22222222", fakeArbiterFP
	pathB, pathV, pathM := []uint32{48 | 1<<31, 1 | 1<<31, 0 | 1<<31, 2 | 1<<31, 0, 1}, []uint32{48 | 1<<31, 1 | 1<<31, 0 | 1<<31, 2 | 1<<31, 0, 4}, []uint32{84 | 1<<31, 1 | 1<<31, 0 | 1<<31, 0, 9}
	deriv := func(pub []byte, fp string, path []uint32) psbtDerivation {
		d := psbtDerivation{PubKey: pub, Path: path}
		fpb, _ := hex.DecodeString(fp)
		copy(d.Fingerprint[:], fpb)
		return d
	}
	want := []escrowExpectKey{{Party: "buyer", Fingerprint: fpB, Path: pathB}, {Party: "vendor", Fingerprint: fpV, Path: pathV}, {Party: "market", Fingerprint: fpM, Path: pathM}}
	sorted := [][]byte{buyer, vendor, market}
	slices.SortFunc(sorted, bytes.Compare)
	good := psbtInput{WitnessScript: multisigScript(sorted...), Derivations: []psbtDerivation{deriv(buyer, fpB, pathB), deriv(vendor, fpV, pathV), deriv(market, fpM, pathM)}}
	keys, err := checkEscrowWitness(good, p2wsh(good.WitnessScript), fpM, want)
	if err != nil || !bytes.Equal(keys["buyer"], buyer) || !bytes.Equal(keys["vendor"], vendor) || !bytes.Equal(keys["market"], market) {
		t.Fatalf("good script: %v %x", err, keys)
	}
	mutate := func(f func(in *psbtInput, spk *[]byte)) error {
		in := psbtInput{WitnessScript: slices.Clone(good.WitnessScript), Derivations: slices.Clone(good.Derivations)}
		spk := p2wsh(in.WitnessScript)
		f(&in, &spk)
		_, err := checkEscrowWitness(in, spk, fpM, want)
		return err
	}
	// Each case fails one guard and names it: dropping a guard makes its case fail later, on another guard.
	wantErr := map[string]error{
		"duplicate key": errEscrowScriptKeys, "unsorted keys": errEscrowScriptKeys, "1-of-3": errEscrowScriptShape,
		"uncompressed key": errEscrowScriptShape, "key not on the curve": errEscrowScriptShape, "2-of-4": errEscrowScriptShape,
		"other script paid": errEscrowScriptPays, "no arbiter key": errEscrowArbiterCount, "two arbiter keys": errEscrowArbiterCount,
		"arbiter key at another index": errEscrowPartyKey, "buyer key at the practice child": errEscrowPartyKey,
		"vendor derivation missing": errEscrowScriptDerivs, "extra derivation": errEscrowScriptDerivs,
		"derivation for a key outside the script": errEscrowScriptDerivs, "one key named twice": errEscrowScriptDerivs,
	}
	for name, f := range map[string]func(in *psbtInput, spk *[]byte){
		"duplicate key": func(in *psbtInput, spk *[]byte) {
			in.WitnessScript = multisigScript(sorted[0], sorted[0], sorted[2])
			*spk = p2wsh(in.WitnessScript)
		},
		"unsorted keys": func(in *psbtInput, spk *[]byte) {
			in.WitnessScript = multisigScript(sorted[2], sorted[1], sorted[0])
			*spk = p2wsh(in.WitnessScript)
		},
		"1-of-3": func(in *psbtInput, spk *[]byte) {
			in.WitnessScript[0] = 0x51
			*spk = p2wsh(in.WitnessScript)
		},
		"uncompressed key": func(in *psbtInput, spk *[]byte) {
			in.WitnessScript[2] = 0x04
			*spk = p2wsh(in.WitnessScript)
		},
		"key not on the curve": func(in *psbtInput, spk *[]byte) {
			bad := append([]byte{2}, bytes.Repeat([]byte{0xff}, 32)...)
			ks := [][]byte{bad, sorted[1], sorted[2]}
			slices.SortFunc(ks, bytes.Compare)
			in.WitnessScript = multisigScript(ks...)
			*spk = p2wsh(in.WitnessScript)
		},
		"2-of-4": func(in *psbtInput, spk *[]byte) {
			ks := append(slices.Clone(sorted), testPub(4))
			slices.SortFunc(ks, bytes.Compare)
			in.WitnessScript = multisigScript(ks...)
			in.WitnessScript[len(in.WitnessScript)-2] = 0x54
			*spk = p2wsh(in.WitnessScript)
		},
		"other script paid": func(in *psbtInput, spk *[]byte) { (*spk)[5] ^= 1 },
		"no arbiter key": func(in *psbtInput, spk *[]byte) {
			in.Derivations[2] = deriv(market, "33333333", pathM)
		},
		"two arbiter keys": func(in *psbtInput, spk *[]byte) {
			in.Derivations[1] = deriv(vendor, fpM, pathV)
		},
		"arbiter key at another index": func(in *psbtInput, spk *[]byte) {
			in.Derivations[2] = deriv(market, fpM, []uint32{84 | 1<<31, 1 | 1<<31, 0 | 1<<31, 0, 8})
		},
		"buyer key at the practice child": func(in *psbtInput, spk *[]byte) {
			in.Derivations[0] = deriv(buyer, fpB, []uint32{48 | 1<<31, 1 | 1<<31, 0 | 1<<31, 2 | 1<<31, 0, 0})
		},
		"vendor derivation missing": func(in *psbtInput, spk *[]byte) { in.Derivations = in.Derivations[:2] },
		"extra derivation": func(in *psbtInput, spk *[]byte) {
			in.Derivations = append(in.Derivations, deriv(testPub(5), "44444444", pathB))
		},
		"derivation for a key outside the script": func(in *psbtInput, spk *[]byte) {
			in.Derivations[0] = deriv(testPub(6), fpB, pathB)
		},
		"one key named twice": func(in *psbtInput, spk *[]byte) {
			in.Derivations[1] = deriv(buyer, fpV, pathV)
		},
	} {
		if err := mutate(f); err != wantErr[name] {
			t.Errorf("%s: %v, want %v", name, err, wantErr[name])
		}
	}
	// A node that names a party key twice and adds a second key under the arbiter's fingerprint: every party key is
	// found at its origin, and only the arbiter count refuses it.
	twice := []escrowExpectKey{want[0], {Party: "vendor", Fingerprint: fpB, Path: pathB}, want[2]}
	other := testPub(7)
	ks := [][]byte{buyer, other, market}
	slices.SortFunc(ks, bytes.Compare)
	in := psbtInput{WitnessScript: multisigScript(ks...), Derivations: []psbtDerivation{deriv(buyer, fpB, pathB), deriv(other, fpM, pathV), deriv(market, fpM, pathM)}}
	if _, err := checkEscrowWitness(in, p2wsh(in.WitnessScript), fpM, twice); err != errEscrowArbiterCount {
		t.Fatalf("second arbiter-fingerprint key: %v", err)
	}
}
