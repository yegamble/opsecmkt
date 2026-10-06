package market

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"opsecmkt/internal/btc"
)

// bip32VectorVersion is the mainnet xpub version of BIP32's test vectors; bip32VectorMaster is test vector 1's master
// public key (the vectors themselves are tested in internal/btc).
const (
	bip32VectorVersion = 0x0488b21e
	bip32VectorMaster  = "xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8"
	bip32H             = btc.Hardened
)

// TestBIP32PerOrderChildRooted: a per-order child of a registered account key is one level below the account at the
// requested index, and its rooted Vpub parses back to the same key and chain code (the rooted serializer itself is
// tested in internal/btc).
func TestBIP32PerOrderChildRooted(t *testing.T) {
	// A per-order child of a registered account key: the rooted form of the derived key.
	acct, err := parseEscrowKey(testBIP48Key)
	if err != nil {
		t.Fatal(err)
	}
	child, used, err := deriveOrderKey(acct, 2)
	if err != nil || used != 2 || child.Depth != byte(len(acct.Path)+1) || child.Child != 2 {
		t.Fatalf("per-order child: used %d depth %d child %d, %v", used, child.Depth, child.Child, err)
	}
	if back, err := parseBIP32Pub(child.Rooted(versionVpub), versionVpub); err != nil || back.PubKey != child.PubKey || back.ChainCode != child.ChainCode {
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
		keys = append(keys, perOrderKey{Account: acct, Index: used, Rooted: child.Rooted(versionVpub)})
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
		return r.Serialize(versionTpub)
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
	notRooted := child.Serialize(versionTpub)
	tamper := keys[0]
	tamper.Index = bip32H
	for name, ks := range map[string][]perOrderKey{
		"no keys":            nil,
		"two keys":           keys[:2],
		"four keys":          append(keys[:3:3], keys[0]),
		"hardened index":     {tamper},
		"rooted not rooted":  {{Account: acct, Index: 2, Rooted: notRooted}},
		"rooted not a key":   {{Account: acct, Index: 2, Rooted: "tpub"}},
		"rooted mainnet key": {{Account: acct, Index: 2, Rooted: bip32VectorMaster}},
	} {
		if err := e.checkPerOrderDerivation(ctx, ks); !errors.Is(err, errPerOrderInput) {
			t.Fatalf("%s: %v (want refused before any RPC)", name, err)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("malformed input reached Core: %q", asked)
	}
}
