package main

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"opsecmkt/internal/btc"
)

// BIP32 test vectors 1-3 (https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki, "Test Vectors"): the seeds,
// the master keys and the first hardened child, with mainnet version bytes (no network is involved).
const (
	xprvVersion = 0x0488ade4
	xpubVersion = 0x0488b21e
)

var bip32PrivVectors = []struct {
	seed, masterPrv, masterPub, child0HPub string
}{
	{"000102030405060708090a0b0c0d0e0f",
		"xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHi",
		"xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8",
		"xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw"},
	{"fffcf9f6f3f0edeae7e4e1dedbd8d5d2cfccc9c6c3c0bdbab7b4b1aeaba8a5a29f9c999693908d8a8784817e7b7875726f6c696663605d5a5754514e4b484542",
		"xprv9s21ZrQH143K31xYSDQpPDxsXRTUcvj2iNHm5NUtrGiGG5e2DtALGdso3pGz6ssrdK4PFmM8NSpSBHNqPqm55Qn3LqFtT2emdEXVYsCzC2U",
		"xpub661MyMwAqRbcFW31YEwpkMuc5THy2PSt5bDMsktWQcFF8syAmRUapSCGu8ED9W6oDMSgv6Zz8idoc4a6mr8BDzTJY47LJhkJ8UB7WEGuduB",
		""},
	{"4b381541583be4423346c643850da4b320e46a87ae3d2a4e6da11eba819cd4acba45d239319ac14f863b8d5ab5a0d0c64d2e8a1e7d1457df2e5a3c51c73235be",
		"xprv9s21ZrQH143K25QhxbucbDDuQ4naNntJRi4KUfWT7xo4EKsHt2QJDu7KXp1A3u7Bi1j8ph3EGsZ9Xvz9dGuVrtHHs7pXeTzjuxBrCmmhgC6",
		"xpub661MyMwAqRbcEZVB4dScxMAdx6d4nFc9nvyvH3v4gJL378CSRZiYmhRoP7mBy6gSPSCYk6SzXPTf3ND1cZAceL7SfJ1Z3GC8vBgp2epUt13",
		"xpub68NZiKmJWnxxS6aaHmn81bvJeTESw724CRDs6HbuccFQN9Ku14VQrADWgqbhhTHBaohPX4CjNLf9fq9MYo6oDaPPLPxSb7gwQN3ih19Zm4Y"},
}

// TestSignerBIP32PrivateVectors: the master key from each vector's seed serializes to the vector's xprv and xpub, and
// the hardened child m/0H (vectors 1 and 3: leading zeros kept) to its xpub, so the seed handling, the hardened private
// step and the public key computed from a private one are BIP32's.
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
		if got := m.public().Serialize(xpubVersion); got != v.masterPub {
			t.Fatalf("vector %d: master xpub %s", i+1, got)
		}
		if v.child0HPub == "" {
			continue
		}
		c, err := m.hardenedChild(0)
		if err != nil {
			t.Fatal(err)
		}
		pub := c.public() // rooted: give it m/0H's metadata to compare with the vector
		pub.Depth, pub.ParentFP, pub.Child = 1, m.public().Fingerprint(), btc.Hardened
		if got := pub.Serialize(xpubVersion); got != v.child0HPub {
			t.Fatalf("vector %d: m/0H xpub %s", i+1, got)
		}
	}
}

// TestSignerKeysFromSeed: the account is m/48h/1h/0h/2h of the seed's master key, published rooted (depth 0,
// fingerprint 0, child 0) with its own fingerprint; the per-order keys are account/i/0/k and account/i/1/k, the
// descriptor key hands Core the master tprv with the full path for Core to derive, and 16- or 33-byte seeds are refused.
func TestSignerKeysFromSeed(t *testing.T) {
	seed, _ := hex.DecodeString(bip32PrivVectors[0].seed + bip32PrivVectors[0].seed)
	k, err := keysFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := masterKey(seed)
	acct := m
	for _, step := range []uint32{48, 1, 0, 2} {
		if acct, err = acct.hardenedChild(step); err != nil {
			t.Fatal(err)
		}
	}
	want := acct.public()
	got, err := btc.ParseExtPub(k.accountTpub(), btc.VersionTpub)
	if err != nil || got.Depth != 0 || got.Child != 0 || got.ParentFP != [4]byte{} || got.PubKey != want.PubKey || got.ChainCode != want.ChainCode {
		t.Fatalf("account tpub %+v, %v", got, err)
	}
	if fp := want.Fingerprint(); k.fingerprint() != hex.EncodeToString(fp[:]) || len(k.fingerprint()) != 8 {
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
		if !strings.HasPrefix(d, "tprv") || !strings.HasSuffix(d, "/48h/1h/0h/2h/"+itoa(c.index)+"/"+itoa(c.chain)+"/"+itoa(c.i)) {
			t.Fatalf("descriptor key path for %v is wrong", c)
		}
		if strings.Split(d, "/")[0] != m.serialize(tprvVersion) {
			t.Fatal("descriptor key is not the master tprv")
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
