package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"opsecmkt/internal/btc"
)

// TestSignerNoPointMultiplication (QA4W2-1): no non-test file of the package does elliptic-curve point arithmetic
// (internal/btc's Mul, Add or G, or any ScalarMult or ScalarBaseMult), whose math/big code is not constant-time. The
// signer's private keys only get HMAC-SHA512 and an addition mod n (hardened CKDpriv); Core computes the account's
// public key (getdescriptorinfo) and derives every private child. btc.CKDPub, which multiplies by a scalar computed
// from the public account key and chain code, is public data only and allowed.
func TestSignerNoPointMultiplication(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		local := ""
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "crypto/elliptic" || path == "crypto/ecdh" {
				t.Errorf("%s imports %s", name, path)
			}
			if path != "opsecmkt/internal/btc" {
				continue
			}
			local = "btc"
			if imp.Name != nil {
				local = imp.Name.Name
			}
			if local == "." || local == "_" {
				t.Errorf("%s imports internal/btc as %q, which this check cannot follow", name, local)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && local != "" && x.Name == local {
				switch sel.Sel.Name {
				case "Mul", "Add", "G":
					t.Errorf("%s: %s.%s: point arithmetic in the signer", fset.Position(sel.Pos()), local, sel.Sel.Name)
				}
			}
			if sel.Sel.Name == "ScalarMult" || sel.Sel.Name == "ScalarBaseMult" {
				t.Errorf("%s: %s: point multiplication in the signer", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("only %d source files checked", checked)
	}
}

// BIP32 test vectors 1-3 (https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki, "Test Vectors"): the seeds,
// the master keys and the first hardened child, with mainnet version bytes (no network is involved).
const (
	xprvVersion = 0x0488ade4
	xpubVersion = 0x0488b21e
)

var bip32PrivVectors = []struct {
	seed, masterPrv, child0HPrv string
}{
	{"000102030405060708090a0b0c0d0e0f",
		"xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi",
		"xprv9uHRZZhk6KAJC1avXpDAp4MDc3sQKNxDiPvvkX8Br5ngLNv1TxvUxt4cV1rGL5hj6KCesnDYUhd7oWgT11eZG7XnxHrnYeSvkzY7d2bhkJ7"},
	{"fffcf9f6f3f0edeae7e4e1dedbd8d5d2cfccc9c6c3c0bdbab7b4b1aeaba8a5a29f9c999693908d8a8784817e7b7875726f6c696663605d5a5754514e4b484542",
		"xprv9s21ZrQH143K31xYSDQpPDxsXRTUcvj2iNHm5NUtrGiGG5e2DtALGdso3pGz6ssrdK4PFmM8NSpSBHNqPqm55Qn3LqFtT2emdEXVYsCzC2U",
		""},
	{"4b381541583be4423346c643850da4b320e46a87ae3d2a4e6da11eba819cd4acba45d239319ac14f863b8d5ab5a0d0c64d2e8a1e7d1457df2e5a3c51c73235be",
		"xprv9s21ZrQH143K25QhxbucbDDuQ4naNntJRi4KUfWT7xo4EKsHt2QJDu7KXp1A3u7Bi1j8ph3EGsZ9Xvz9dGuVrtHHs7pXeTzjuxBrCmmhgC6",
		"xprv9uPDJpEQgRQfDcW7BkF7eTya6RPxXeJCqCJGHuCJ4GiRVLzkTXBAJMu2qaMWPrS7AANYqdq6vcBcBUdJCVVFceUvJFjaPdGZ2y9WACViL4L"},
}

// TestSignerBIP32PrivateVectors: the master key from each vector's seed serializes to the vector's xprv, and the
// hardened child m/0H (vectors 1 and 3: leading zeros kept) has the chain code and private key of the vector's m/0H
// xprv, so the seed handling and the hardened private step are BIP32's (no public key is computed in Go).
func TestSignerBIP32PrivateVectors(t *testing.T) {
	for i, v := range bip32PrivVectors {
		seed, _ := hex.DecodeString(v.seed)
		m, err := masterKey(seed)
		if err != nil {
			t.Fatalf("vector %d: %v", i+1, err)
		}
		if got := m.serialize(xprvVersion); got != v.masterPrv {
			t.Fatalf("vector %d: master xprv mismatch", i+1)
		}
		if v.child0HPrv == "" {
			continue
		}
		c, err := m.hardenedChild(0)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := btc.Base58CheckDecode(v.child0HPrv)
		if err != nil || len(raw) != 78 || raw[4] != 1 || raw[45] != 0 {
			t.Fatalf("vector %d: m/0H xprv does not decode: %v", i+1, err)
		}
		if !bytes.Equal(raw[13:45], c.chain[:]) || !bytes.Equal(raw[46:], c.key.FillBytes(make([]byte, 32))) {
			t.Fatalf("vector %d: m/0H chain code or key mismatch", i+1)
		}
	}
}

// coreAccountTpub is what Bitcoin Core 31.1 (regtest, the signer's RPC user) answered for getdescriptorinfo of
// wpkh(<testSeed's account tprv>/0/*), the tprv computed by an independent Python implementation of BIP32 (QA4W2-1
// fix, 2026-10-06): the descriptor wpkh(<this>/0/*)#wf6k6mvt.
const coreAccountTpub = "tpubD6NzVbkrYhZ4WWQMNd7tbyfWYEqSRRiwxd6xyQzzAmDUBqnZuAqjPAAgRDFgyNxVTCtiZQb23hjntrhsemD8LpcTt56jAzbXqkaMJszi3sV"

// testKeys is keysFromSeed(seed) with its account loaded from the fake Core f.
func testKeys(t *testing.T, f *fakeCore, seed []byte) *signerKeys {
	t.Helper()
	k, err := keysFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCore(f.url())
	if err != nil {
		t.Fatal(err)
	}
	if err := k.loadAccount(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestSignerAccountFromCore (QA4W2-1, fake Core): the signer's account key is the one Core's getdescriptorinfo gives
// for wpkh(<account tprv>/0/*), sent in one call whose body holds the account tprv (m/48h/1h/0h/2h of the seed, from
// hardened CKDpriv) and never the master tprv. For testSeed it equals real Core 31.1's answer (coreAccountTpub), and so
// does the fake's simulation. An answer that is not a rooted tpub with Go's chain code is refused, as are a node that
// does not answer and a signer user not whitelisted for getdescriptorinfo; no error names key material.
func TestSignerAccountFromCore(t *testing.T) {
	f := newFakeCore(t)
	k := testKeys(t, f, testSeed)
	if got := k.accountTpub(); got != coreAccountTpub {
		t.Fatalf("account tpub %s, Core 31.1 answered %s", got, coreAccountTpub)
	}
	m, _ := masterKey(testSeed)
	acct := m
	for _, step := range []uint32{48, 1, 0, 2} {
		acct, _ = acct.hardenedChild(step)
	}
	if k.accountTprv != acct.serialize(tprvVersion) || k.chain != acct.chain {
		t.Fatal("the account tprv is not m/48h/1h/0h/2h of the seed, rooted")
	}
	if m := f.methods(); len(m) != 1 || m[0] != "getdescriptorinfo" {
		t.Fatalf("calls %v", m)
	}
	var desc string
	json.Unmarshal(f.calls[0].params[0], &desc)
	if len(f.calls[0].params) != 1 || desc != "wpkh("+k.accountTprv+"/0/*)" {
		t.Fatal("getdescriptorinfo was not asked for wpkh(<account tprv>/0/*)")
	}
	f.noBodyHolds(t, masterTprv(t, testSeed), "master tprv")
	fp := mustParse(t, coreAccountTpub).Fingerprint()
	if k.fingerprint() != hex.EncodeToString(fp[:]) {
		t.Fatalf("fingerprint %s", k.fingerprint())
	}

	other := testKeys(t, newFakeCore(t), bytes.Repeat([]byte{0x11}, 32)).accountTpub()
	deeper := mustParse(t, coreAccountTpub)
	deeper.Depth, deeper.Child = 1, 0
	answer := func(d string) func(string) any {
		return func(string) any { return map[string]any{"descriptor": d, "checksum": "00000000"} }
	}
	for name, setup := range map[string]func(f *fakeCore){
		"another seed's account": func(f *fakeCore) { f.descInfo = answer(btc.WithChecksum("wpkh(" + other + "/0/*)")) },
		"the private descriptor": func(f *fakeCore) {
			f.descInfo = func(d string) any { return map[string]any{"descriptor": btc.WithChecksum(d)} }
		},
		"a depth-1 key": func(f *fakeCore) {
			f.descInfo = answer(btc.WithChecksum("wpkh(" + deeper.Serialize(btc.VersionTpub) + "/0/*)"))
		},
		"no checksum":               func(f *fakeCore) { f.descInfo = answer("wpkh(" + coreAccountTpub + "/0/*)") },
		"another script":            func(f *fakeCore) { f.descInfo = answer(btc.WithChecksum("pkh(" + coreAccountTpub + "/0/*)")) },
		"a key off the curve":       func(f *fakeCore) { f.descInfo = answer(btc.WithChecksum("wpkh(" + offCurveTpub(t) + "/0/*)")) },
		"no descriptor":             func(f *fakeCore) { f.descInfo = func(string) any { return map[string]any{} } },
		"getdescriptorinfo refused": func(f *fakeCore) { f.refuse["getdescriptorinfo"] = true },
		"not on the whitelist":      func(f *fakeCore) { f.forbidden = map[string]bool{"getdescriptorinfo": true} },
		"node down":                 func(f *fakeCore) { f.downAt = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCore(t)
			setup(f)
			k, _ := keysFromSeed(testSeed)
			c, _ := newCore(f.url())
			err := k.loadAccount(context.Background(), c)
			if err == nil || k.account != (btc.ExtKey{}) {
				t.Fatalf("accepted: %v", err)
			}
			if strings.Contains(err.Error(), "tprv") || strings.Contains(err.Error(), "tpub") {
				t.Fatalf("error names key material: %v", err)
			}
			if name == "not on the whitelist" && !strings.Contains(err.Error(), "getdescriptorinfo") {
				t.Fatalf("error does not name the method: %v", err)
			}
			f.noBodyHolds(t, masterTprv(t, testSeed), "master tprv")
		})
	}
}

func mustParse(t *testing.T, s string) btc.ExtKey {
	t.Helper()
	k, err := btc.ParseExtPub(s, btc.VersionTpub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// offCurveTpub is a rooted tpub (valid checksum) whose key is 02 and an x with no point on the curve.
func offCurveTpub(t *testing.T) string {
	t.Helper()
	k := mustParse(t, coreAccountTpub)
	k.PubKey = [33]byte{2}
	for i := 1; ; i++ {
		k.PubKey[32] = byte(i)
		if !btc.OnCurve(k.PubKey[:]) {
			return k.Serialize(btc.VersionTpub)
		}
	}
}

// TestSignerKeysFromSeed: the account is m/48h/1h/0h/2h of the seed's master key, published rooted (depth 0,
// fingerprint 0, child 0) with Go's chain code and its own fingerprint; the per-order keys are account/i/0/k and
// account/i/1/k, the descriptor key hands Core the account tprv with the per-order path (never the master tprv), and
// 16- or 33-byte seeds are refused.
func TestSignerKeysFromSeed(t *testing.T) {
	seed, _ := hex.DecodeString(bip32PrivVectors[0].seed + bip32PrivVectors[0].seed)
	k := testKeys(t, newFakeCore(t), seed)
	m, _ := masterKey(seed)
	acct := m
	var err error
	for _, step := range []uint32{48, 1, 0, 2} {
		if acct, err = acct.hardenedChild(step); err != nil {
			t.Fatal(err)
		}
	}
	got, err := btc.ParseExtPub(k.accountTpub(), btc.VersionTpub)
	if err != nil || got.Depth != 0 || got.Child != 0 || got.ParentFP != [4]byte{} || got.ChainCode != acct.chain || got.PubKey != k.account.PubKey {
		t.Fatalf("account tpub %+v, %v", got, err)
	}
	if fp := got.Fingerprint(); k.fingerprint() != hex.EncodeToString(fp[:]) || len(k.fingerprint()) != 8 {
		t.Fatalf("fingerprint %s", k.fingerprint())
	}
	for _, c := range []struct{ index, chain, i uint32 }{{2, 0, 0}, {2, 1, 19}, {1000, 0, 7}} {
		step, _, _ := btc.CKDPub(got, c.index)
		step, _, _ = btc.CKDPub(step, c.chain)
		leaf, _, _ := btc.CKDPub(step, c.i)
		pub, err := k.orderKey(c.index, c.chain, c.i)
		if err != nil || pub != leaf.PubKey {
			t.Fatalf("order key %v: %x, %v", c, pub, err)
		}
		d := k.descriptorKey(c.index, c.chain, c.i)
		if d != acct.serialize(tprvVersion)+"/"+itoa(c.index)+"/"+itoa(c.chain)+"/"+itoa(c.i) || !strings.HasPrefix(d, "tprv") {
			t.Fatalf("descriptor key for %v is not the account tprv and the per-order path", c)
		}
		if strings.Contains(d, m.serialize(tprvVersion)) || strings.Contains(d, "h/") {
			t.Fatal("descriptor key holds the master tprv or a hardened step")
		}
	}
	sib := k.siblingKeys(5, 20)
	for _, c := range [][2]uint32{{0, 0}, {0, 19}, {1, 0}, {1, 19}} {
		pub, _ := k.orderKey(5, c[0], c[1])
		if sib[pub] != c || len(sib) != 40 {
			t.Fatalf("sibling keys of index 5: %d keys, %v at %v", len(sib), sib[pub], c)
		}
	}
	if len(k.siblingKeys(btc.Hardened, 20)) != 0 {
		t.Fatal("sibling keys of a hardened index")
	}
	if _, err := k.orderKey(btc.Hardened, 0, 0); err == nil {
		t.Fatal("a hardened order index was accepted")
	}
	for _, n := range []int{0, 16, 31, 33, 63, 65} {
		if _, err := keysFromSeed(make([]byte, n)); !errors.Is(err, errSeedSize) {
			t.Fatalf("%d-byte seed: %v", n, err)
		}
	}
}
