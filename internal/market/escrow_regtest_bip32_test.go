package market

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestEscrowRegtestBIP32 is A-453's real-chain leg (scripts/test-escrow-regtest.sh; not run in CI): the Go per-order
// key derivation (escrow_bip32.go, shape (b), no product caller yet) checked against Bitcoin Core on regtest. Three
// Core wallets each hold a BIP48 account (m/48h/1h/0h/2h) of their own seed; for several per-order indexes (2, 3, 17,
// 1000 and 2^31-1) Go derives each account's child, serializes it rooted (depth 0, fingerprint 0, child 0) as a Vpub,
// and the cross-check helper, run as the App's rpcwhitelist user (deriveaddresses), finds that Core derives the same
// address from the account key at <index>/0/0 and from the rooted key at /0/0, for each key alone (wpkh) and for the
// three together (wsh(sortedmulti(2,…))). As an independent third route, the operator derives the address from each
// wallet's private root key along the full path m/48h/1h/0h/2h/<index>/0/0, and it is the same. Finally the helper,
// on real Core, refuses a rooted key paired with the wrong index and one with a single chain-code byte changed.
func TestEscrowRegtestBIP32(t *testing.T) {
	appURL, opURL := os.Getenv("ESCROW_REGTEST_APP_RPC_URL"), os.Getenv("ESCROW_REGTEST_OPERATOR_RPC_URL")
	if appURL == "" || opURL == "" {
		t.Skip("set ESCROW_REGTEST_APP_RPC_URL and ESCROW_REGTEST_OPERATOR_RPC_URL (scripts/test-escrow-regtest.sh) to run against bitcoind -regtest")
	}
	ctx := context.Background()
	op, err := newRPCClient("regtest operator", opURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newRPCClient("regtest app", appURL, "1.0", false)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*rpcClient{"operator": op, "app": app} {
		var info struct {
			Chain string `json:"chain"`
		}
		if err := c.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.Chain != "regtest" {
			t.Fatalf("refusing: the %s node URL is not on regtest (or is unreachable)", name)
		}
	}
	call := func(wallet, method string, out any, params ...any) {
		t.Helper()
		path := ""
		if wallet != "" {
			path = "/wallet/" + wallet
		}
		if params == nil {
			params = []any{}
		}
		if err := op.call(ctx, path, method, params, out); err != nil {
			t.Fatalf("operator %s: %v", method, err)
		}
	}
	type descList struct {
		Descriptors []struct {
			Desc     string `json:"desc"`
			Internal bool   `json:"internal"`
		} `json:"descriptors"`
	}
	// account creates a Core wallet and returns its root tprv (kept in memory, never logged) and its BIP48 account key
	// with origin as listdescriptors normalizes it, parsed strictly as a registration would be.
	s := randomToken()[:8]
	account := func(name string) (string, escrowKey) {
		t.Helper()
		call("", "createwallet", nil, name)
		var priv descList
		call(name, "listdescriptors", &priv, true)
		var root string
		for _, d := range priv.Descriptors {
			if strings.HasPrefix(d.Desc, "wpkh(tprv") && !d.Internal {
				root = strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/", 2)[0]
			}
		}
		if root == "" {
			t.Fatalf("%s: no wpkh root key", name)
		}
		var info struct {
			Checksum string `json:"checksum"`
		}
		d := "wpkh(" + root + "/48h/1h/0h/2h/0/*)"
		call("", "getdescriptorinfo", &info, d)
		var imported []struct {
			Success bool `json:"success"`
		}
		call(name, "importdescriptors", &imported, []map[string]any{{"desc": d + "#" + info.Checksum, "active": false, "range": []int{0, 2}, "timestamp": "now"}})
		if len(imported) != 1 || !imported[0].Success {
			t.Fatalf("%s: importdescriptors: %+v", name, imported)
		}
		var pub descList
		call(name, "listdescriptors", &pub)
		for _, d := range pub.Descriptors {
			if strings.Contains(d.Desc, "/48h/1h/0h/2h]") {
				expr := strings.SplitN(strings.TrimPrefix(d.Desc, "wpkh("), "/0/*", 2)[0]
				k, err := parseEscrowKey(expr)
				if err != nil || !k.BIP48 {
					t.Fatalf("%s: account key %q: %v", name, expr, err)
				}
				return root, k
			}
		}
		t.Fatalf("%s: listdescriptors has no BIP48 account", name)
		return "", escrowKey{}
	}
	type party struct {
		name, root string
		key        escrowKey
	}
	var parties []party
	for _, who := range []string{"buyer", "vendor", "market"} {
		name := "a453-" + who + "-" + s
		root, key := account(name)
		parties = append(parties, party{name, root, key})
		t.Logf("%s wallet %s: account [%s%s]%s…", who, name, key.Fingerprint, key.PathText, key.XPub[:24])
	}

	esc, err := newEscrowBTC(escrowConfig{rpcURL: appURL})
	if err != nil {
		t.Fatal(err)
	}
	privateAddress := func(root, path string) string {
		t.Helper()
		var info struct {
			Checksum string `json:"checksum"`
		}
		d := "wpkh(" + root + path + ")"
		call("", "getdescriptorinfo", &info, d)
		var addrs []string
		call("", "deriveaddresses", &addrs, d+"#"+info.Checksum)
		if len(addrs) != 1 {
			t.Fatalf("deriveaddresses from the private root: %q", addrs)
		}
		return addrs[0]
	}
	for _, i := range []uint32{2, 3, 17, 1000, bip32H - 1} {
		var all []perOrderKey
		for _, p := range parties {
			child, used, err := deriveOrderKey(p.key, i)
			if err != nil || used != i {
				t.Fatalf("%s index %d: Go derivation used %d: %v", p.name, i, used, err)
			}
			k := perOrderKey{Account: p.key, Index: used, Rooted: child.Rooted(versionVpub)}
			if err := esc.checkPerOrderDerivation(ctx, []perOrderKey{k}); err != nil {
				t.Fatalf("%s index %d: Core cross-check (wpkh): %v", p.name, i, err)
			}
			rooted, err := esc.deriveAddresses(ctx, withChecksum("wpkh("+child.Rooted(versionTpub)+"/0/0)"), -1)
			if err != nil || len(rooted) != 1 {
				t.Fatalf("%s index %d: deriveaddresses of the rooted key: %q %v", p.name, i, rooted, err)
			}
			full := privateAddress(p.root, "/48h/1h/0h/2h/"+strconv.FormatUint(uint64(i), 10)+"/0/0")
			if full != rooted[0] {
				t.Fatalf("%s index %d: private root path gives %s, Go rooted key %s", p.name, i, full, rooted[0])
			}
			t.Logf("index %d %-6s: Go child %s… rooted %s…; wpkh /0/0 = %s from the account path, the rooted key and the private root", i,
				strings.Split(p.name, "-")[1], child.Serialize(versionTpub)[:20], k.Rooted[:20], full)
			all = append(all, k)
		}
		if err := esc.checkPerOrderDerivation(ctx, all); err != nil {
			t.Fatalf("index %d: Core cross-check (wsh sortedmulti 2-of-3): %v", i, err)
		}
		addr, err := esc.deriveAddresses(ctx, withChecksum("wsh(sortedmulti(2,"+all[0].Account.Expression()+"/"+strconv.FormatUint(uint64(i), 10)+"/0/0,"+
			all[1].Account.Expression()+"/"+strconv.FormatUint(uint64(i), 10)+"/0/0,"+all[2].Account.Expression()+"/"+strconv.FormatUint(uint64(i), 10)+"/0/0))"), -1)
		if err != nil || len(addr) != 1 {
			t.Fatalf("index %d: %q %v", i, addr, err)
		}
		t.Logf("index %d: 2-of-3 wsh(sortedmulti) /0/0 = %s from the account paths and from the rooted keys", i, addr[0])
	}

	// Refusals on real Core: a rooted key paired with another index, and one with a chain-code byte changed.
	p := parties[0]
	child, _, err := deriveOrderKey(p.key, 5)
	if err != nil {
		t.Fatal(err)
	}
	wrong := perOrderKey{Account: p.key, Index: 6, Rooted: child.Rooted(versionVpub)}
	if err := esc.checkPerOrderDerivation(ctx, []perOrderKey{wrong}); !errors.Is(err, errPerOrderMismatch) {
		t.Fatalf("rooted index 5 checked as index 6: %v (want refused)", err)
	}
	t.Log("refused: rooted child 5 checked as index 6 -> per-order-derivation-mismatch")
	flipped := child
	flipped.ChainCode[7] ^= 1
	bad := perOrderKey{Account: p.key, Index: 5, Rooted: flipped.Rooted(versionVpub)}
	if err := esc.checkPerOrderDerivation(ctx, []perOrderKey{bad}); !errors.Is(err, errPerOrderMismatch) {
		t.Fatalf("chain-code byte flipped: %v (want refused)", err)
	}
	t.Log("refused: rooted child 5 with one chain-code byte flipped -> per-order-derivation-mismatch")
	three := []perOrderKey{bad, {Account: parties[1].key, Index: 5}, {Account: parties[2].key, Index: 5}}
	for j := 1; j < 3; j++ {
		c, _, err := deriveOrderKey(three[j].Account, 5)
		if err != nil {
			t.Fatal(err)
		}
		three[j].Rooted = c.Rooted(versionVpub)
	}
	if err := esc.checkPerOrderDerivation(ctx, three); !errors.Is(err, errPerOrderMismatch) {
		t.Fatalf("2-of-3 with one bad rooted key: %v (want refused)", err)
	}
	t.Log("refused: 2-of-3 at index 5 with the buyer's rooted key's chain code changed -> per-order-derivation-mismatch")
}
