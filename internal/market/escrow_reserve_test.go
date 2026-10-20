package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"slices"

	"strings"
	"testing"
)

var (
	coreWatchPubkeys = []string{
		"020d6954cb5a10e733fa52f54b1401f189a94e63ce18ca0936c97445d995179bc9",
		"039c1397dfd77ba25923cc2061bf33543d964569809a22a65c67f906776cd3f48b",
		"03f1b81ece6804840a9736dd9a484492161bcec92c1416e3c092e89110f4a5e48d",
	}
	coreWatchSPK  = "002031f7bcd5ab2c15ddad87e881fa4ec220f395eff2b8ac2e131434adface70d2a3"
	coreWatchDesc = "wsh(multi(2,[504df747]020d6954cb5a10e733fa52f54b1401f189a94e63ce18ca0936c97445d995179bc9,[ba61cd7f]039c1397dfd77ba25923cc2061bf33543d964569809a22a65c67f906776cd3f48b,[87eaaddf]03f1b81ece6804840a9736dd9a484492161bcec92c1416e3c092e89110f4a5e48d))#csv246nh"
)

// The adapter's reading of getaddressinfo for the orphan sweep (escrowBTC.watchKeys), over HTTP: the three keys from
// pubkeys, or else from desc (multi or sortedmulti, origins stripped), each a compressed key on the curve, and only when
// their 2-of-3 witness script hashes to the reply's scriptPubKey. An address the wallet does not hold (ismine false) and
// one that is not P2WSH (never an escrow) have no keys; any other shape is a bad-reply class, never "no keys".
func TestEscrowWatchKeysReply(t *testing.T) {
	ctx := context.Background()
	unsorted := "wsh(sortedmulti(2," + coreWatchPubkeys[2] + "," + coreWatchPubkeys[0] + "," + coreWatchPubkeys[1] + "))"
	without := func(drop ...string) map[string]any {
		r := map[string]any{"ismine": true, "iswatchonly": false, "solvable": true, "scriptPubKey": coreWatchSPK, "pubkeys": coreWatchPubkeys,
			"desc": coreWatchDesc, "labels": []string{"escrow:o1"}, "script": "multisig", "sigsrequired": 2}
		for _, d := range drop {
			delete(r, d)
		}
		return r
	}
	with := func(r map[string]any, k string, v any) map[string]any { r[k] = v; return r }
	program, _ := hex.DecodeString(coreWatchSPK[4:])
	orphan := segwitAddress("bcrt", 0, program) // the address coreWatchSPK pays
	wpkh, _ := hex.DecodeString(strings.Repeat("ab", 20))
	other := sha256.Sum256([]byte("another escrow address"))
	for _, c := range []struct {
		name  string
		reply map[string]any
		keys  []string
		class string
		addr  string // the address asked about; the orphan's when empty
	}{
		{"Core 31.1", without(), coreWatchPubkeys, "", ""},
		{"desc only", without("pubkeys"), coreWatchPubkeys, "", ""},
		{"sortedmulti desc, keys in another order", with(without("pubkeys"), "desc", unsorted), coreWatchPubkeys, "", ""},
		{"not held", map[string]any{"ismine": false, "labels": []string{}, "scriptPubKey": coreWatchSPK}, nil, "", ""},
		{"labelled, not held", map[string]any{"ismine": false, "labels": []string{"escrow:o2"}}, nil, "", ""},
		{"held, not P2WSH", map[string]any{"ismine": true, "labels": []string{"x"}, "scriptPubKey": "0014" + strings.Repeat("ab", 20)}, nil, "", segwitAddress("bcrt", 0, wpkh)},
		{"held by addr(…) only (QA5W4A-2)", map[string]any{"ismine": true, "solvable": false, "labels": []string{"escrow:o2"}, "scriptPubKey": coreWatchSPK}, nil, "getaddressinfo unsolvable", ""},
		{"no ismine", without("ismine"), nil, "getaddressinfo bad-reply", ""},
		{"no scriptPubKey", without("scriptPubKey"), nil, "getaddressinfo bad-reply", ""},
		{"neither pubkeys nor desc", without("pubkeys", "desc"), nil, "getaddressinfo bad-reply", ""},
		{"two pubkeys", with(without(), "pubkeys", coreWatchPubkeys[:2]), nil, "getaddressinfo bad-reply", ""},
		{"a key repeated", with(without(), "pubkeys", []string{coreWatchPubkeys[0], coreWatchPubkeys[0], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply", ""},
		{"not hex", with(without(), "pubkeys", []string{"zz", coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply", ""},
		{"uncompressed", with(without(), "pubkeys", []string{"04" + strings.Repeat("ab", 64), coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply", ""},
		{"off the curve", with(without(), "pubkeys", []string{"02" + strings.Repeat("00", 32), coreWatchPubkeys[1], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply", ""},
		{"keys of another address", with(without(), "scriptPubKey", "0020"+strings.Repeat("ab", 32)), nil, "getaddressinfo bad-reply", ""},
		{"a reply about another address", without(), nil, "getaddressinfo bad-reply", segwitAddress("bcrt", 0, other[:])},
		{"keys out of script order", with(without(), "pubkeys", []string{coreWatchPubkeys[1], coreWatchPubkeys[0], coreWatchPubkeys[2]}), nil, "getaddressinfo bad-reply", ""},
		{"desc not a 2-of-3", with(without("pubkeys"), "desc", "wsh(multi(1,"+strings.Join(coreWatchPubkeys, ",")+"))"), nil, "getaddressinfo bad-reply", ""},
		{"desc malformed", with(without("pubkeys"), "desc", "wsh(multi(2,"+coreWatchPubkeys[0]+"))"), nil, "getaddressinfo bad-reply", ""},
		{"pubkeys not a list", with(without(), "pubkeys", "02ab"), nil, "getaddressinfo bad-reply", ""},
	} {
		addr := c.addr
		if addr == "" {
			addr = orphan
		}
		srv := newPayRPCServer(t, "u", "p", false, func(path, method string, params json.RawMessage) (any, int, string) {
			if path != fakeWatchWallet || method != "getaddressinfo" || string(params) != `["`+addr+`"]` {
				return nil, -32601, "unexpected call"
			}
			return c.reply, 0, ""
		})
		e, err := newEscrowBTC(escrowConfig{on: true, rpcURL: srv.url(""), arbiterWallet: "opsecmkt-arbiter", watchWallet: "opsecmkt-escrow-watch", confirmations: 3})
		if err != nil {
			t.Fatal(err)
		}
		e.network = "regtest" // as startEscrow reads it from the node (the chain table keys on it, A-439)
		keys, err := e.watchKeys(ctx, addr)
		class := ""
		if err != nil {
			class = errorClassOf(err)
		}
		var got []string
		for _, k := range keys {
			got = append(got, hex.EncodeToString(k))
		}
		if !slices.Equal(got, c.keys) || class != c.class {
			t.Errorf("%s: keys %v, error %v", c.name, got, err)
		}
	}
}
