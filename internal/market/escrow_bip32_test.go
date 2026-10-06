package market

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
)

// BIP32 test vectors 1–3 (public chain steps) and 5 (invalid extended keys), copied verbatim from the BIP's own text:
// https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki, section "Test Vectors" (fetched 2026-10-06; file
// sha256 e5e00a8289db2f681052cf24a745320afc225e66b25d1e489a7c884d2fc7f11f). They use mainnet xpub version bytes;
// the tests parse them with that version only (no network is involved).
const bip32VectorVersion = 0x0488b21e

// bip32Chain is one vector's "ext pub" lines in order; steps[i] is the child number from chain i to chain i+1.
type bip32Chain struct {
	pubs  []string
	steps []uint32
}

const bip32H = 1 << 31

var bip32Vectors = []bip32Chain{
	{ // Test vector 1: m, m/0H, m/0H/1, m/0H/1/2H, m/0H/1/2H/2, m/0H/1/2H/2/1000000000
		pubs: []string{
			"xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8",
			"xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw",
			"xpub6ASuArnXKPbfEwhqN6e3mwBcDTgzisQN1wXN9BJcM47sSikHjJf3UFHKkNAWbWMiGj7Wf5uMash7SyYq527Hqck2AxYysAA7xmALppuCkwQ",
			"xpub6D4BDPcP2GT577Vvch3R8wDkScZWzQzMMUm3PWbmWvVJrZwQY4VUNgqFJPMM3No2dFDFGTsxxpG5uJh7n7epu4trkrX7x7DogT5Uv6fcLW5",
			"xpub6FHa3pjLCk84BayeJxFW2SP4XRrFd1JYnxeLeU8EqN3vDfZmbqBqaGJAyiLjTAwm6ZLRQUMv1ZACTj37sR62cfN7fe5JnJ7dh8zL4fiyLHV",
			"xpub6H1LXWLaKsWFhvm6RVpEL9P4KfRZSW7abD2ttkWP3SSQvnyA8FSVqNTEcYFgJS2UaFcxupHiYkro49S8yGasTvXEYBVPamhGW6cFJodrTHy",
		},
		steps: []uint32{0 | bip32H, 1, 2 | bip32H, 2, 1000000000},
	},
	{ // Test vector 2: m, m/0, m/0/2147483647H, m/0/2147483647H/1, m/0/2147483647H/1/2147483646H, …/2
		pubs: []string{
			"xpub661MyMwAqRbcFW31YEwpkMuc5THy2PSt5bDMsktWQcFF8syAmRUapSCGu8ED9W6oDMSgv6Zz8idoc4a6mr8BDzTJY47LJhkJ8UB7WEGuduB",
			"xpub69H7F5d8KSRgmmdJg2KhpAK8SR3DjMwAdkxj3ZuxV27CprR9LgpeyGmXUbC6wb7ERfvrnKZjXoUmmDznezpbZb7ap6r1D3tgFxHmwMkQTPH",
			"xpub6ASAVgeehLbnwdqV6UKMHVzgqAG8Gr6riv3Fxxpj8ksbH9ebxaEyBLZ85ySDhKiLDBrQSARLq1uNRts8RuJiHjaDMBU4Zn9h8LZNnBC5y4a",
			"xpub6DF8uhdarytz3FWdA8TvFSvvAh8dP3283MY7p2V4SeE2wyWmG5mg5EwVvmdMVCQcoNJxGoWaU9DCWh89LojfZ537wTfunKau47EL2dhHKon",
			"xpub6ERApfZwUNrhLCkDtcHTcxd75RbzS1ed54G1LkBUHQVHQKqhMkhgbmJbZRkrgZw4koxb5JaHWkY4ALHY2grBGRjaDMzQLcgJvLJuZZvRcEL",
			"xpub6FnCn6nSzZAw5Tw7cgR9bi15UV96gLZhjDstkXXxvCLsUXBGXPdSnLFbdpq8p9HmGsApME5hQTZ3emM2rnY5agb9rXpVGyy3bdW6EEgAtqt",
		},
		steps: []uint32{0, 2147483647 | bip32H, 1, 2147483646 | bip32H, 2},
	},
	{ // Test vector 3 (retention of leading zeros): m, m/0H
		pubs: []string{
			"xpub661MyMwAqRbcEZVB4dScxMAdx6d4nFc9nvyvH3v4gJL378CSRZiYmhRoP7mBy6gSPSCYk6SzXPTf3ND1cZAceL7SfJ1Z3GC8vBgp2epUt13",
			"xpub68NZiKmJWnxxS6aaHmn81bvJeTESw724CRDs6HbuccFQN9Ku14VQrADWgqbhhTHBaohPX4CjNLf9fq9MYo6oDaPPLPxSb7gwQN3ih19Zm4Y",
		},
		steps: []uint32{0 | bip32H},
	},
}

// TestBIP32PublicDerivationVectors: every unhardened step of vectors 1–3 derives the vector's next extended public key
// exactly (depth, parent fingerprint, child number, chain code and key), every hardened step is refused, and every
// vector key parses and re-serializes to itself (vector 3: leading zeros kept).
func TestBIP32PublicDerivationVectors(t *testing.T) {
	public, hardened := 0, 0
	for vi, v := range bip32Vectors {
		for i, s := range v.pubs {
			k, err := parseBIP32Pub(s, bip32VectorVersion)
			if err != nil {
				t.Fatalf("vector %d chain %d: parse: %v", vi+1, i, err)
			}
			if got := k.serialize(bip32VectorVersion); got != s {
				t.Fatalf("vector %d chain %d: re-serialized %s", vi+1, i, got)
			}
			if i == len(v.steps) {
				continue
			}
			step := v.steps[i]
			child, used, err := ckdPub(k, step)
			if step&bip32H != 0 {
				if !errors.Is(err, errBIP32Hardened) {
					t.Fatalf("vector %d chain %d: hardened step %#x: %v (want refused)", vi+1, i, step, err)
				}
				hardened++
				continue
			}
			if err != nil || used != step {
				t.Fatalf("vector %d chain %d: step %d: used %d, %v", vi+1, i, step, used, err)
			}
			if got := child.serialize(bip32VectorVersion); got != v.pubs[i+1] {
				t.Fatalf("vector %d chain %d -> %d: got %s want %s", vi+1, i, step, got, v.pubs[i+1])
			}
			public++
		}
	}
	if public != 6 || hardened != 5 {
		t.Fatalf("checked %d public and %d hardened steps (want 6 and 5)", public, hardened)
	}
}

// TestBIP32InvalidKeysVector5: every invalid key of test vector 5 is refused, the public ones for the vector's stated
// reason (private-version keys are refused by their version).
func TestBIP32InvalidKeysVector5(t *testing.T) {
	for _, c := range []struct {
		key  string
		want error // nil: any refusal
	}{
		{"xpub661MyMwAqRbcEYS8w7XLSVeEsBXy79zSzH1J8vCdxAZningWLdN3zgtU6LBpB85b3D2yc8sfvZU521AAwdZafEz7mnzBBsz4wKY5fTtTQBm", errBIP32Prefix},   // pubkey version / prvkey mismatch
		{"xprv9s21ZrQH143K24Mfq5zL5MhWK9hUhhGbd45hLXo2Pq2oqzMMo63oStZzFGTQQD3dC4H2D5GBj7vWvSQaaBv5cxi9gafk7NF3pnBju6dwKvH", errBIP32Version},  // prvkey version / pubkey mismatch
		{"xpub661MyMwAqRbcEYS8w7XLSVeEsBXy79zSzH1J8vCdxAZningWLdN3zgtU6Txnt3siSujt9RCVYsx4qHZGc62TG4McvMGcAUjeuwZdduYEvFn", errBIP32Prefix},   // invalid pubkey prefix 04
		{"xprv9s21ZrQH143K24Mfq5zL5MhWK9hUhhGbd45hLXo2Pq2oqzMMo63oStZzFGpWnsj83BHtEy5Zt8CcDr1UiRXuWCmTQLxEK9vbz5gPstX92JQ", errBIP32Version},  // invalid prvkey prefix 04
		{"xpub661MyMwAqRbcEYS8w7XLSVeEsBXy79zSzH1J8vCdxAZningWLdN3zgtU6N8ZMMXctdiCjxTNq964yKkwrkBJJwpzZS4HS2fxvyYUA4q2Xe4", errBIP32Prefix},   // invalid pubkey prefix 01
		{"xprv9s21ZrQH143K24Mfq5zL5MhWK9hUhhGbd45hLXo2Pq2oqzMMo63oStZzFAzHGBP2UuGCqWLTAPLcMtD9y5gkZ6Eq3Rjuahrv17fEQ3Qen6J", errBIP32Version},  // invalid prvkey prefix 01
		{"xprv9s2SPatNQ9Vc6GTbVMFPFo7jsaZySyzk7L8n2uqKXJen3KUmvQNTuLh3fhZMBoG3G4ZW1N2kZuHEPY53qmbZzCHshoQnNf4GvELZfqTUrcv", errBIP32Version},  // zero depth with non-zero parent fingerprint
		{"xpub661no6RGEX3uJkY4bNnPcw4URcQTrSibUZ4NqJEw5eBkv7ovTwgiT91XX27VbEXGENhYRCf7hyEbWrR3FewATdCEebj6znwMfQkhRYHRLpJ", errBIP32Root},     // zero depth with non-zero parent fingerprint
		{"xprv9s21ZrQH4r4TsiLvyLXqM9P7k1K3EYhA1kkD6xuquB5i39AU8KF42acDyL3qsDbU9NmZn6MsGSUYZEsuoePmjzsB3eFKSUEh3Gu1N3cqVUN", errBIP32Version},  // zero depth with non-zero index
		{"xpub661MyMwAuDcm6CRQ5N4qiHKrJ39Xe1R1NyfouMKTTWcguwVcfrZJaNvhpebzGerh7gucBvzEQWRugZDuDXjNDRmXzSZe4c7mnTK97pTvGS8", errBIP32Root},     // zero depth with non-zero index
		{"DMwo58pR1QLEFihHiXPVykYB6fJmsTeHvyTp7hRThAtCX8CvYzgPcn8XnmdfHGMQzT7ayAmfo4z3gY5KfbrZWZ6St24UVf2Qgo6oujFktLHdHY4", errBIP32Version},  // unknown extended key version
		{"DMwo58pR1QLEFihHiXPVykYB6fJmsTeHvyTp7hRThAtCX8CvYzgPcn8XnmdfHPmHJiEDXkTiJTVV9rHEBUem2mwVbbNfvT2MTcAqj3nesx8uBf9", errBIP32Version},  // unknown extended key version
		{"xprv9s21ZrQH143K24Mfq5zL5MhWK9hUhhGbd45hLXo2Pq2oqzMMo63oStZzF93Y5wvzdUayhgkkFoicQZcP3y52uPPxFnfoLZB21Teqt1VvEHx", errBIP32Version},  // private key 0 not in 1..n-1
		{"xprv9s21ZrQH143K24Mfq5zL5MhWK9hUhhGbd45hLXo2Pq2oqzMMo63oStZzFAzHGBP2UuGCqWLTAPLcMtD5SDKr24z3aiUvKr9bJpdrcLg1y3G", errBIP32Version},  // private key n not in 1..n-1
		{"xpub661MyMwAqRbcEYS8w7XLSVeEsBXy79zSzH1J8vCdxAZningWLdN3zgtU6Q5JXayek4PRsn35jii4veMimro1xefsM58PgBMrvdYre8QyULY", errBIP32Point},    // invalid pubkey 020000…07
		{"xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LnF5kejMRNNU3TGtRBeJgk33yuGBxrMPHL", errBIP32Encoding}, // invalid checksum
	} {
		if _, err := parseBIP32Pub(c.key, bip32VectorVersion); err == nil || c.want != nil && !errors.Is(err, c.want) {
			t.Fatalf("%s…: %v (want %v)", c.key[:24], err, c.want)
		}
	}
}

// TestCKDPubRefusesHardened: a public key cannot derive a hardened child; CKDpub refuses every index >= 2^31 rather
// than computing something else.
func TestCKDPubRefusesHardened(t *testing.T) {
	k, err := parseBIP32Pub(bip32Vectors[0].pubs[1], bip32VectorVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []uint32{bip32H, bip32H | 1, 0xffffffff} {
		if _, _, err := ckdPub(k, i); !errors.Is(err, errBIP32Hardened) {
			t.Fatalf("index %#x: %v (want refused)", i, err)
		}
	}
	// A parent that is not a point on the curve, and one at the deepest serializable depth, are refused too.
	bad := k
	bad.PubKey[1] ^= 0xff
	for onCurve(bad.PubKey[:]) {
		bad.PubKey[2]++
	}
	if _, _, err := ckdPub(bad, 1); !errors.Is(err, errBIP32Point) {
		t.Fatalf("off-curve parent: %v", err)
	}
	deep := k
	deep.Depth = 255
	if _, _, err := ckdPub(deep, 1); !errors.Is(err, errBIP32Depth) {
		t.Fatalf("depth 255 parent: %v", err)
	}
}

// secpGCompressed is the generator G, compressed: the public key of private key 1.
var secpGCompressed = func() (k [33]byte) {
	k[0] = 2
	x, _ := new(big.Int).SetString("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", 16)
	x.FillBytes(k[1:])
	return k
}()

// TestCKDPubSkipsInvalidChild: BIP32's rule for an invalid child (IL >= n, or the child is the point at infinity) is to
// proceed with the next index; ckdPubWith takes the HMAC so a test can force each case at one index, and the result
// is the real derivation at the next index, reported as used.
func TestCKDPubSkipsInvalidChild(t *testing.T) {
	parent, err := parseBIP32Pub(bip32Vectors[0].pubs[2], bip32VectorVersion)
	if err != nil {
		t.Fatal(err)
	}
	nMinus1 := new(big.Int).Sub(secpN, big.NewInt(1))
	ilN, ilMax, ilNMinus1 := make([]byte, 32), bytes.Repeat([]byte{0xff}, 32), make([]byte, 32)
	secpN.FillBytes(ilN)
	nMinus1.FillBytes(ilNMinus1)
	// forced is an HMAC-SHA512 that answers il as IL for the index at, and the real HMAC for every other index.
	forced := func(at uint32, il []byte) func(key, data []byte) [64]byte {
		return func(key, data []byte) (out [64]byte) {
			if binary.BigEndian.Uint32(data[33:]) == at {
				copy(out[:32], il)
				return out
			}
			m := hmac.New(sha512.New, key)
			m.Write(data)
			copy(out[:], m.Sum(nil))
			return out
		}
	}
	for _, c := range []struct {
		name   string
		parent bip32Key
		il     []byte
	}{
		{"IL = n", parent, ilN},
		{"IL = 2^256-1", parent, ilMax},
		{"child at infinity (parent G, IL = n-1)", bip32Key{ChainCode: parent.ChainCode, PubKey: secpGCompressed}, ilNMinus1},
	} {
		want, used, err := ckdPub(c.parent, 8)
		if err != nil || used != 8 {
			t.Fatalf("%s: real index 8: used %d, %v", c.name, used, err)
		}
		got, used, err := ckdPubWith(c.parent, 7, forced(7, c.il))
		if err != nil || used != 8 || got != want {
			t.Fatalf("%s: forced invalid at 7: used %d, %v, child equal to index 8's: %v", c.name, used, err, got == want)
		}
	}
	// The last unhardened index has no unhardened successor: refused, never a hardened child.
	if _, _, err := ckdPubWith(parent, bip32H-1, forced(bip32H-1, ilN)); !errors.Is(err, errBIP32NoIndex) {
		t.Fatalf("invalid at 2^31-1: %v", err)
	}
}

// TestBIP32RootedSerializer: the rooted form keeps only the chain code and key (depth 0, parent fingerprint 0, child
// number 0) in tpub and SLIP-132 Vpub forms with a valid checksum, and parses back to the same key and chain code.
func TestBIP32RootedSerializer(t *testing.T) {
	k, err := parseBIP32Pub(bip32Vectors[0].pubs[4], bip32VectorVersion) // m/0H/1/2H/2: depth 4, child 2
	if err != nil {
		t.Fatal(err)
	}
	if k.Depth != 4 || k.Child != 2 || k.ParentFP == [4]byte{} {
		t.Fatalf("vector key metadata %+v", k)
	}
	for _, c := range []struct {
		version uint32
		prefix  string
	}{{versionTpub, "tpub"}, {versionVpub, "Vpub"}} {
		s := k.rooted(c.version)
		if !strings.HasPrefix(s, c.prefix) {
			t.Fatalf("rooted %s form: %s", c.prefix, s)
		}
		raw, err := base58CheckDecode(s)
		if err != nil || len(raw) != 78 {
			t.Fatalf("rooted %s form: %v", c.prefix, err)
		}
		if binary.BigEndian.Uint32(raw[:4]) != c.version || raw[4] != 0 || !bytes.Equal(raw[5:13], make([]byte, 8)) ||
			!bytes.Equal(raw[13:45], k.ChainCode[:]) || !bytes.Equal(raw[45:], k.PubKey[:]) {
			t.Fatalf("rooted %s form bytes %x", c.prefix, raw)
		}
		back, err := parseBIP32Pub(s, versionTpub, versionVpub)
		if err != nil || back.ChainCode != k.ChainCode || back.PubKey != k.PubKey || back.Depth != 0 || back.Child != 0 || back.ParentFP != [4]byte{} {
			t.Fatalf("rooted %s form parsed back: %+v, %v", c.prefix, back, err)
		}
		if _, err := parseBIP32Pub(s, bip32VectorVersion); !errors.Is(err, errBIP32Version) {
			t.Fatalf("rooted %s form accepted under another version: %v", c.prefix, err)
		}
	}
	// A per-order child of a registered account key: the rooted form of the derived key.
	acct, err := parseEscrowKey(testBIP48Key)
	if err != nil {
		t.Fatal(err)
	}
	child, used, err := deriveOrderKey(acct, 2)
	if err != nil || used != 2 || child.Depth != byte(len(acct.Path)+1) || child.Child != 2 {
		t.Fatalf("per-order child: used %d depth %d child %d, %v", used, child.Depth, child.Child, err)
	}
	if back, err := parseBIP32Pub(child.rooted(versionVpub), versionVpub); err != nil || back.PubKey != child.PubKey || back.ChainCode != child.ChainCode {
		t.Fatalf("per-order rooted Vpub: %v", err)
	}
}

// TestEscrowPerOrderCrossCheck (simulated RPC, not chain evidence): the Core cross-check asks deriveaddresses for the
// address of the descriptor from each account key at <index>/0/0 and of the one from each rooted key at /0/0, as wpkh
// for one key and wsh(sortedmulti(2,…)) for three, and refuses unless Core returns one and the same address for both.
// Malformed input is refused before any RPC. The real comparison is TestEscrowRegtestBIP32.
func TestEscrowPerOrderCrossCheck(t *testing.T) {
	acct, err := parseEscrowKey(testBIP48Key)
	if err != nil {
		t.Fatal(err)
	}
	var keys []perOrderKey
	for i := uint32(2); i < 5; i++ {
		child, used, err := deriveOrderKey(acct, i)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, perOrderKey{Account: acct, Index: used, Rooted: child.rooted(versionVpub)})
	}
	var answers map[string][]string // descriptor body (no checksum) -> addresses
	var asked []string
	rpcErr := 0
	srv := newPayRPCServer(t, "u", "p", false, func(path, method string, params json.RawMessage) (any, int, string) {
		if method != "deriveaddresses" {
			return nil, -32601, "Method not found"
		}
		var p []any
		json.Unmarshal(params, &p)
		desc, _ := p[0].(string)
		body, sum, _ := strings.Cut(desc, "#")
		if want, _ := descChecksum(body); sum != want || len(p) != 1 {
			return nil, -5, "Provided checksum does not match computed checksum"
		}
		asked = append(asked, body)
		if rpcErr != 0 {
			return nil, rpcErr, "fake failure"
		}
		return answers[body], 0, ""
	})
	rpc, err := newRPCClient("test", srv.url(""), "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	e := &escrowBTC{rpc: rpc}
	ctx := context.Background()
	tpubOf := func(k perOrderKey) string {
		r, err := parseBIP32Pub(k.Rooted, versionVpub)
		if err != nil {
			t.Fatal(err)
		}
		return r.serialize(versionTpub)
	}
	single := []string{"wpkh(" + acct.Expression() + "/2/0/0)", "wpkh(" + tpubOf(keys[0]) + "/0/0)"}
	multi := []string{
		"wsh(sortedmulti(2," + acct.Expression() + "/2/0/0," + acct.Expression() + "/3/0/0," + acct.Expression() + "/4/0/0))",
		"wsh(sortedmulti(2," + tpubOf(keys[0]) + "/0/0," + tpubOf(keys[1]) + "/0/0," + tpubOf(keys[2]) + "/0/0))",
	}
	for _, c := range []struct {
		name  string
		keys  []perOrderKey
		descs []string
	}{{"one key (wpkh)", keys[:1], single}, {"three keys (wsh sortedmulti)", keys, multi}} {
		answers, asked = map[string][]string{c.descs[0]: {"bcrt1qsame"}, c.descs[1]: {"bcrt1qsame"}}, nil
		if err := e.checkPerOrderDerivation(ctx, c.keys); err != nil {
			t.Fatalf("%s: agreeing addresses refused: %v", c.name, err)
		}
		if len(asked) != 2 || asked[0] != c.descs[0] || asked[1] != c.descs[1] {
			t.Fatalf("%s: asked %q", c.name, asked)
		}
		answers[c.descs[1]] = []string{"bcrt1qother"}
		if err := e.checkPerOrderDerivation(ctx, c.keys); !errors.Is(err, errPerOrderMismatch) {
			t.Fatalf("%s: differing addresses: %v (want refused)", c.name, err)
		}
		answers[c.descs[0]], answers[c.descs[1]] = nil, nil
		if err := e.checkPerOrderDerivation(ctx, c.keys); !errors.Is(err, errPerOrderMismatch) {
			t.Fatalf("%s: no address from Core: %v (want refused)", c.name, err)
		}
		answers[c.descs[0]], answers[c.descs[1]] = []string{"a", "b"}, []string{"a", "b"}
		if err := e.checkPerOrderDerivation(ctx, c.keys); !errors.Is(err, errPerOrderMismatch) {
			t.Fatalf("%s: two addresses from Core: %v (want refused)", c.name, err)
		}
	}
	rpcErr = -1
	if err := e.checkPerOrderDerivation(ctx, keys[:1]); err == nil {
		t.Fatal("an RPC failure passed the cross-check")
	}
	rpcErr, asked = 0, nil
	child, _, err := deriveOrderKey(acct, 2)
	if err != nil {
		t.Fatal(err)
	}
	notRooted := child.serialize(versionTpub)
	tamper := keys[0]
	tamper.Index = bip32H
	for name, ks := range map[string][]perOrderKey{
		"no keys":            nil,
		"two keys":           keys[:2],
		"four keys":          append(keys[:3:3], keys[0]),
		"hardened index":     {tamper},
		"rooted not rooted":  {{Account: acct, Index: 2, Rooted: notRooted}},
		"rooted not a key":   {{Account: acct, Index: 2, Rooted: "tpub"}},
		"rooted mainnet key": {{Account: acct, Index: 2, Rooted: bip32Vectors[0].pubs[0]}},
	} {
		if err := e.checkPerOrderDerivation(ctx, ks); !errors.Is(err, errPerOrderInput) {
			t.Fatalf("%s: %v (want refused before any RPC)", name, err)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("malformed input reached Core: %q", asked)
	}
}

// FuzzCKDPub: no parent key, chain code or index panics CKDpub or the parser; a derived child is a valid compressed
// point at the requested index or a later unhardened one, one level deeper, and its rooted forms parse back to it.
func FuzzCKDPub(f *testing.F) {
	for _, v := range bip32Vectors {
		for _, s := range v.pubs {
			raw, _ := base58CheckDecode(s)
			f.Add(raw[45:78], raw[13:45], uint32(1), s)
		}
	}
	f.Add(secpGCompressed[:], make([]byte, 32), uint32(bip32H-1), "")
	f.Add(make([]byte, 33), make([]byte, 32), uint32(bip32H), "tpub")
	f.Fuzz(func(t *testing.T, pub, chain []byte, i uint32, s string) {
		if k, err := parseBIP32Pub(s, bip32VectorVersion, versionTpub, versionVpub); err == nil {
			if k.serialize(bip32VectorVersion) == "" || !onCurve(k.PubKey[:]) {
				t.Fatalf("%q parsed to an invalid key", s)
			}
		}
		var k bip32Key
		copy(k.PubKey[:], pub)
		copy(k.ChainCode[:], chain)
		k.Depth = byte(len(s))
		child, used, err := ckdPub(k, i)
		if err != nil {
			return
		}
		if used < i || used >= bip32H || !onCurve(child.PubKey[:]) || child.Child != used || child.Depth != k.Depth+1 {
			t.Fatalf("child %+v at %d from index %d", child, used, i)
		}
		for _, v := range []uint32{versionTpub, versionVpub} {
			back, err := parseBIP32Pub(child.rooted(v), v)
			if err != nil || back.PubKey != child.PubKey || back.ChainCode != child.ChainCode {
				t.Fatalf("rooted %x: %v", v, err)
			}
		}
	})
}
