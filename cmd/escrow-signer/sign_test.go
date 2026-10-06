package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"opsecmkt/internal/btc"
)

// fakeCore is a simulated Bitcoin Core JSON-RPC server (not chain evidence; the real one is the regtest leg,
// internal/market TestEscrowRegtestSigner): decodepsbt answers the decoded form registered for a PSBT string,
// descriptorprocesspsbt answers the PSBT string the test set, and getdescriptorinfo answers wpkh(<tprv>/0/*) with the
// tprv's public key in its place, as Core 31.1 does (the simulation computes it; TestSignerAccountFromCore pins the
// answer for testSeed to real Core's). It records every call and every request body.
type fakeCore struct {
	mu        sync.Mutex
	decoded   map[string]any // PSBT string -> decodepsbt result
	result    string         // descriptorprocesspsbt's PSBT
	refuse    map[string]bool
	descInfo  func(desc string) any // when set, getdescriptorinfo's result instead of the simulation's
	downAt    int                   // > 0: that call (1-based) gets no JSON-RPC answer, as from a node gone away
	forbidden map[string]bool       // methods answered 403, as if not on the signer user's rpcwhitelist
	calls     []fakeCall
	bodies    []string
	srv       *httptest.Server
}

type fakeCall struct {
	method string
	params []json.RawMessage
}

func newFakeCore(t *testing.T) *fakeCore {
	f := &fakeCore{decoded: map[string]any{}, refuse: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opsecmkt-signer" || p != "signer-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{req.Method, req.Params})
		f.bodies = append(f.bodies, string(body))
		if down := len(f.calls) == f.downAt; down || f.forbidden[req.Method] {
			f.mu.Unlock()
			if down {
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}
		refuse := f.refuse[req.Method]
		var result any
		code, msg := 0, ""
		switch req.Method {
		case "decodepsbt":
			var p string
			json.Unmarshal(req.Params[0], &p)
			d, ok := f.decoded[p]
			if !ok {
				code, msg = -22, "TX decode failed"
			}
			result = d
		case "descriptorprocesspsbt":
			result = map[string]any{"psbt": f.result, "complete": false}
		case "getdescriptorinfo":
			var desc string
			json.Unmarshal(req.Params[0], &desc)
			if f.descInfo != nil {
				result = f.descInfo(desc)
			} else if result = simulatedDescriptorInfo(desc); result == nil {
				code, msg = -5, "fake: not a wpkh(tprv/0/*) descriptor"
			}
		default:
			w.WriteHeader(http.StatusForbidden) // not on the signer user's rpcwhitelist
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		if refuse {
			code, msg = -22, "refused by the fake: tprv8ZgxMBicQKsPd fake key text"
		}
		reply := map[string]any{"id": req.ID, "result": result, "error": nil}
		if code != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			reply = map[string]any{"id": req.ID, "result": nil, "error": map[string]any{"code": code, "message": msg}}
		}
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// simulatedDescriptorInfo is getdescriptorinfo of wpkh(<tprv>/0/*) as Core 31.1 answers it: the descriptor with the
// tprv replaced by its tpub (same depth, parent fingerprint, child number and chain code) and its checksum; nil for
// anything else. It stands in for Core, and is the only place the package computes a public key from a private one.
func simulatedDescriptorInfo(desc string) any {
	m := regexp.MustCompile(`^wpkh\((tprv[1-9A-HJ-NP-Za-km-z]+)/0/\*\)$`).FindStringSubmatch(desc)
	if m == nil {
		return nil
	}
	raw, err := btc.Base58CheckDecode(m[1])
	if err != nil || len(raw) != 78 || raw[45] != 0 {
		return nil
	}
	k := btc.ExtKey{Depth: raw[4], Child: binary.BigEndian.Uint32(raw[9:13])}
	copy(k.ParentFP[:], raw[5:9])
	copy(k.ChainCode[:], raw[13:45])
	k.PubKey = btc.Compress(btc.Mul(new(big.Int).SetBytes(raw[46:78]), btc.G))
	cs, _ := btc.DescriptorChecksum(desc) // Core's checksum field is the input descriptor's
	return map[string]any{"descriptor": btc.WithChecksum("wpkh(" + k.Serialize(btc.VersionTpub) + "/0/*)"), "checksum": cs, "isrange": true, "issolvable": true, "hasprivatekeys": true}
}

// noBodyHolds fails the test if any request body sent to the fake Core contains needle (named by label, never printed).
func (f *fakeCore) noBodyHolds(t *testing.T, needle, label string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if needle == "" {
		t.Fatalf("empty %s", label)
	}
	for _, b := range f.bodies {
		if strings.Contains(b, needle) {
			t.Fatalf("a request body sent to Core holds the %s", label)
		}
	}
}

// masterTprv is the seed's BIP32 master key serialized as a tprv: what no RPC body may hold (QA4W2-1).
func masterTprv(t *testing.T, seed []byte) string {
	t.Helper()
	m, err := masterKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	return m.serialize(tprvVersion)
}

func (f *fakeCore) url() string {
	return strings.Replace(f.srv.URL, "http://", "http://opsecmkt-signer:signer-pass@", 1)
}

func (f *fakeCore) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []string
	for _, c := range f.calls {
		m = append(m, c.method)
	}
	return m
}

// testSeed is a fixed 32-byte seed for the unit tests (never used on any chain but in these tests).
var testSeed = bytes.Repeat([]byte{0x5e}, 32)

// partyKeys are two valid compressed keys standing in for the buyer's and the vendor's per-order keys.
func partyKeys(t *testing.T) [2][33]byte {
	t.Helper()
	root, err := btc.ParseExtPub("xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8", xpubVersion)
	if err != nil {
		t.Fatal(err)
	}
	var out [2][33]byte
	for i := range out {
		c, _, err := btc.CKDPub(root, uint32(100+i))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = c.PubKey
	}
	return out
}

// multisig is the witness script 2 <keys sorted> 3 CHECKMULTISIG, as wsh(sortedmulti(2,…)) makes it.
func multisig(keys ...[33]byte) string {
	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, func(a, b [33]byte) int { return bytes.Compare(a[:], b[:]) })
	return rawMultisig(sorted...)
}

// rawMultisig is 2 <keys as given> 3 CHECKMULTISIG (OP_2 + n... is fixed at 3 keys here).
func rawMultisig(keys ...[33]byte) string {
	s := "52"
	for _, k := range keys {
		s += "21" + hex.EncodeToString(k[:])
	}
	return s + "53ae"
}

func p2wsh(scriptHex string) string {
	b, _ := hex.DecodeString(scriptHex)
	h := sha256.Sum256(b)
	return "0020" + hex.EncodeToString(h[:])
}

// decoded is a decodepsbt result shaped as Core 31.1 prints it, one input per witness script.
func decoded(scripts ...string) map[string]any {
	var vin, inputs []any
	for i, s := range scripts {
		vin = append(vin, map[string]any{"txid": strings.Repeat("ab", 31) + hex.EncodeToString([]byte{byte(i)}), "vout": 1, "sequence": 4294967293})
		inputs = append(inputs, map[string]any{
			"witness_utxo":   map[string]any{"amount": json.Number("0.50000000"), "scriptPubKey": map[string]any{"hex": p2wsh(s), "type": "witness_v0_scripthash"}},
			"witness_script": map[string]any{"hex": s, "type": "multisig"},
			"bip32_derivs":   []any{},
		})
	}
	d := map[string]any{
		"tx":           map[string]any{"txid": strings.Repeat("cd", 32), "version": 2, "vin": vin, "vout": []any{map[string]any{"value": json.Number("0.49990000"), "n": 0}}},
		"global_xpubs": []any{}, "psbt_version": 0, "proprietary": []any{}, "unknown": map[string]any{},
		"inputs": inputs, "outputs": []any{map[string]any{}}, "fee": json.Number("0.00010000"),
	}
	return roundTrip(d)
}

// roundTrip deep-copies a decoded value through JSON so tests can change one copy.
func roundTrip(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	dec.Decode(&out)
	return out
}

func input(d map[string]any, i int) map[string]any { return d["inputs"].([]any)[i].(map[string]any) }

// withSig is d with a partial signature (pubkey -> sig hex) added to input i.
func withSig(d map[string]any, i int, pub [33]byte, sig string) map[string]any {
	d = roundTrip(d)
	in := input(d, i)
	ps, _ := in["partial_signatures"].(map[string]any)
	if ps == nil {
		ps = map[string]any{}
	}
	ps[hex.EncodeToString(pub[:])] = sig
	in["partial_signatures"] = ps
	return d
}

const goodSig = "3044022011111111111111111111111111111111111111111111111111111111111111110220222222222222222222222222222222222222222222222222222222222222222201"

type signEnv struct {
	t      *testing.T
	keys   *signerKeys
	core   *fakeCore
	s      *signer
	logs   *bytes.Buffer
	party  [2][33]byte
	before string // the request's PSBT string
}

func newSignEnv(t *testing.T) *signEnv {
	t.Helper()
	keys, err := keysFromSeed(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeCore(t)
	c, err := newCore(f.url())
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.loadAccount(context.Background(), c); err != nil { // as serve does at startup
		t.Fatal(err)
	}
	f.mu.Lock()
	f.calls = nil // the tests count the calls a request makes; the bodies are kept for noSecrets
	f.mu.Unlock()
	logs := &bytes.Buffer{}
	return &signEnv{t: t, keys: keys, core: f, s: newSigner(keys, c, log.New(logs, "", 0)), logs: logs, party: partyKeys(t),
		before: base64.StdEncoding.EncodeToString([]byte("psbt\xff before"))}
}

func (e *signEnv) key(index, chain, i uint32) [33]byte {
	e.t.Helper()
	k, err := e.keys.orderKey(index, chain, i)
	if err != nil {
		e.t.Fatal(err)
	}
	return k
}

// run registers before (and, when after is not nil, Core's signed result) and asks the signer to sign for index.
func (e *signEnv) run(before, after map[string]any, index uint32) (string, error) {
	e.t.Helper()
	e.core.decoded[e.before] = before
	e.core.result = base64.StdEncoding.EncodeToString([]byte("psbt\xff after"))
	if after != nil {
		e.core.decoded[e.core.result] = after
	}
	return e.s.sign(context.Background(), e.before, index)
}

// noSecrets fails the test if the signer's log holds the seed or any tprv, or if any request body sent to Core holds
// the master tprv or the seed (QA4W2-1: Core is given the account tprv only).
func (e *signEnv) noSecrets() {
	e.t.Helper()
	if l := e.logs.String(); strings.Contains(l, "tprv") || strings.Contains(l, hex.EncodeToString(testSeed)) {
		e.t.Fatalf("signer log holds key material: %q", l)
	}
	e.core.noBodyHolds(e.t, masterTprv(e.t, testSeed), "master tprv")
	e.core.noBodyHolds(e.t, hex.EncodeToString(testSeed), "seed (hex)")
}

// TestSignerSignHappyPath (fake Core): a one-escrow PSBT whose script holds the signer's key at /0/k or /1/k (k < 20)
// of the requested index is signed through descriptorprocesspsbt called with the escrow's exact descriptor (the other
// two keys as hex, the signer's as the account tprv and its path, with its checksum), "ALL", bip32derivs false and
// finalize false; Core's result, which adds one signature under the signer's key to every input, is returned as is.
// A party signature already in the request, and a sighash field of ALL, are fine.
func TestSignerSignHappyPath(t *testing.T) {
	for _, c := range []struct {
		name        string
		chain, leaf uint32
		withParty   bool
		sighashALL  bool
		inputs      int
		nonWitness  bool // the input carries a non_witness_utxo that agrees with its prevout and witness_utxo
	}{
		{"/0/0, one input", 0, 0, false, false, 1, false},
		{"/1/19 sibling, two inputs", 1, 19, false, false, 2, false},
		{"/0/3 with a party signature and sighash ALL", 0, 3, true, true, 1, false},
		{"/0/0 with a matching non_witness_utxo", 0, 0, false, false, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSignEnv(t)
			const index = 7
			mine := e.key(index, c.chain, c.leaf)
			script := multisig(e.party[0], mine, e.party[1])
			scripts := make([]string, c.inputs)
			for i := range scripts {
				scripts[i] = script
			}
			before := decoded(scripts...)
			if c.withParty {
				before = withSig(before, 0, e.party[0], goodSig)
			}
			if c.sighashALL {
				input(before, 0)["sighash"] = "ALL"
			}
			if c.nonWitness {
				vin := before["tx"].(map[string]any)["vin"].([]any)[0].(map[string]any)
				input(before, 0)["non_witness_utxo"] = map[string]any{"txid": vin["txid"], "vout": []any{
					map[string]any{"n": 0, "value": json.Number("0.10000000"), "scriptPubKey": map[string]any{"hex": "0014" + strings.Repeat("22", 20)}},
					map[string]any{"n": 1, "value": json.Number("0.50000000"), "scriptPubKey": map[string]any{"hex": p2wsh(script)}}}}
			}
			after := before
			for i := range scripts {
				after = withSig(after, i, mine, goodSig)
			}
			got, err := e.run(before, after, index)
			if err != nil || got != e.core.result {
				t.Fatalf("sign: %q, %v", got, err)
			}
			if m := e.core.methods(); !slices.Equal(m, []string{"decodepsbt", "descriptorprocesspsbt", "decodepsbt"}) {
				t.Fatalf("calls %v", m)
			}
			call := e.core.calls[1]
			var p0, sighash string
			var descs []string
			var derivs, finalize bool
			json.Unmarshal(call.params[0], &p0)
			json.Unmarshal(call.params[1], &descs)
			json.Unmarshal(call.params[2], &sighash)
			json.Unmarshal(call.params[3], &derivs)
			json.Unmarshal(call.params[4], &finalize)
			var parts []string
			for j := 0; j < 3; j++ {
				k := script[4+68*j : 70+68*j] // 52, then 21 and 33 key bytes per key
				if k == hex.EncodeToString(mine[:]) {
					parts = append(parts, e.keys.descriptorKey(index, c.chain, c.leaf))
				} else {
					parts = append(parts, k)
				}
			}
			want := btc.WithChecksum("wsh(sortedmulti(2," + strings.Join(parts, ",") + "))")
			if p0 != e.before || len(call.params) != 5 || len(descs) != 1 || descs[0] != want || sighash != "ALL" || derivs || finalize {
				t.Fatalf("descriptorprocesspsbt params: psbt ok %v, %d params, descriptor ok %v, sighash %q, bip32derivs %v, finalize %v",
					p0 == e.before, len(call.params), len(descs) == 1 && descs[0] == want, sighash, derivs, finalize)
			}
			e.noSecrets()
		})
	}
}

// TestSignerRefusals (fake Core): each refusal is a fixed error class and nothing is signed: descriptorprocesspsbt is
// never called (the request is checked from decodepsbt first), and the log holds no key material.
func TestSignerRefusals(t *testing.T) {
	const index = 7
	e0 := newSignEnv(t)
	mine := e0.key(index, 0, 0)
	p := e0.party
	good := multisig(p[0], mine, p[1])
	uncompressed := "52" + "41" + "04" + strings.Repeat("11", 64) + "21" + hex.EncodeToString(p[0][:]) + "21" + hex.EncodeToString(mine[:]) + "53ae"
	offCurve := [33]byte{2}
	for i := 1; ; i++ {
		offCurve[32] = byte(i)
		if !btc.OnCurve(offCurve[:]) {
			break
		}
	}
	sortedKeys := func(ks ...[33]byte) [][33]byte {
		s := slices.Clone(ks)
		slices.SortFunc(s, func(a, b [33]byte) int { return bytes.Compare(a[:], b[:]) })
		return s
	}
	desc := sortedKeys(p[0], mine, p[1])
	unsorted := rawMultisig(desc[2], desc[0], desc[1])
	cases := []struct {
		name   string
		want   refusal
		before func(e *signEnv) map[string]any
		index  uint32
	}{
		{"sighash NONE", refSighash, func(e *signEnv) map[string]any {
			d := decoded(good)
			input(d, 0)["sighash"] = "NONE"
			return d
		}, index},
		{"sighash SINGLE|ANYONECANPAY on the second input", refSighash, func(e *signEnv) map[string]any {
			d := decoded(good, good)
			input(d, 1)["sighash"] = "SINGLE|ANYONECANPAY"
			return d
		}, index},
		{"1-of-3", refScript, func(e *signEnv) map[string]any { return decoded("51" + good[2:]) }, index},
		{"2-of-2", refScript, func(e *signEnv) map[string]any {
			return decoded(rawMultisig(sortedKeys(p[0], mine)...)[:len(rawMultisig(p[0], mine))-4] + "52ae")
		}, index},
		{"keys not sorted", refScript, func(e *signEnv) map[string]any { return decoded(unsorted) }, index},
		{"a key twice", refScript, func(e *signEnv) map[string]any { return decoded(rawMultisig(sortedKeys(p[0], mine, mine)...)) }, index},
		{"an uncompressed key", refScript, func(e *signEnv) map[string]any { return decoded(uncompressed) }, index},
		{"a key off the curve", refScript, func(e *signEnv) map[string]any { return decoded(multisig(offCurve, mine, p[1])) }, index},
		{"CHECKMULTISIGVERIFY", refScript, func(e *signEnv) map[string]any { return decoded(good[:len(good)-2] + "af") }, index},
		{"a byte after CHECKMULTISIG", refScript, func(e *signEnv) map[string]any { return decoded(good + "00") }, index},
		// QA4W2-4: 2 <3 keys> 4 CHECKMULTISIG (the n byte), and a script of the right length whose second key is
		// pushed with 0x20 instead of 0x21 (the push byte; the key bytes still sit where a 0x21 push would put them).
		{"2 <3 keys> 4 CHECKMULTISIG", refScript, func(e *signEnv) map[string]any { return decoded(good[:len(good)-4] + "54ae") }, index},
		{"a key pushed with 0x20, length preserved", refScript, func(e *signEnv) map[string]any {
			return decoded("52" + "21" + hex.EncodeToString(desc[0][:]) + "20" + hex.EncodeToString(desc[1][:]) + "21" + hex.EncodeToString(desc[2][:]) + "53ae")
		}, index},
		{"no witness script", refScript, func(e *signEnv) map[string]any {
			d := decoded(good)
			delete(input(d, 0), "witness_script")
			return d
		}, index},
		{"no witness_utxo", refWitnessUTXO, func(e *signEnv) map[string]any {
			d := decoded(good)
			delete(input(d, 0), "witness_utxo")
			return d
		}, index},
		{"witness_utxo pays another script", refWitnessUTXO, func(e *signEnv) map[string]any {
			d := decoded(good)
			input(d, 0)["witness_utxo"].(map[string]any)["scriptPubKey"].(map[string]any)["hex"] = p2wsh(multisig(p[0], p[1], e.key(index, 0, 1)))
			return d
		}, index},
		{"witness_utxo is P2WPKH", refWitnessUTXO, func(e *signEnv) map[string]any {
			d := decoded(good)
			input(d, 0)["witness_utxo"].(map[string]any)["scriptPubKey"].(map[string]any)["hex"] = "0014" + strings.Repeat("11", 20)
			return d
		}, index},
		{"non_witness_utxo output differs from witness_utxo", refWitnessUTXO, func(e *signEnv) map[string]any {
			d := decoded(good)
			vin := d["tx"].(map[string]any)["vin"].([]any)[0].(map[string]any)
			input(d, 0)["non_witness_utxo"] = map[string]any{"txid": vin["txid"], "vout": []any{
				map[string]any{"n": 0, "value": json.Number("0.50000000"), "scriptPubKey": map[string]any{"hex": p2wsh(good)}},
				map[string]any{"n": 1, "value": json.Number("0.50000000"), "scriptPubKey": map[string]any{"hex": "0014" + strings.Repeat("22", 20)}}}}
			return d
		}, index},
		// QA4W2-4: the output itself agrees with witness_utxo, but it belongs to another transaction than the prevout.
		{"non_witness_utxo is another transaction", refWitnessUTXO, func(e *signEnv) map[string]any {
			d := decoded(good)
			input(d, 0)["non_witness_utxo"] = map[string]any{"txid": strings.Repeat("ef", 32), "vout": []any{
				map[string]any{"n": 0, "value": json.Number("0.10000000"), "scriptPubKey": map[string]any{"hex": "0014" + strings.Repeat("22", 20)}},
				map[string]any{"n": 1, "value": json.Number("0.50000000"), "scriptPubKey": map[string]any{"hex": p2wsh(good)}}}}
			return d
		}, index},
		{"two escrow scripts in one request", refMixed, func(e *signEnv) map[string]any {
			return decoded(good, multisig(p[0], p[1], e.key(index, 0, 1)))
		}, index},
		{"another index", refKeyNotFound, func(e *signEnv) map[string]any { return decoded(good) }, index + 1},
		{"the signer's key at /0/20", refKeyNotFound, func(e *signEnv) map[string]any { return decoded(multisig(p[0], p[1], e.key(index, 0, 20))) }, index},
		{"the signer's key at /2/0", refKeyNotFound, func(e *signEnv) map[string]any { return decoded(multisig(p[0], p[1], e.key(index, 2, 0))) }, index},
		{"no signer key", refKeyNotFound, func(e *signEnv) map[string]any {
			k, _ := e.keys.orderKey(index, 0, 0)
			k[1] ^= 1
			for !btc.OnCurve(k[:]) {
				k[2]++
			}
			return decoded(multisig(p[0], p[1], k))
		}, index},
		{"two signer keys (/0/0 and /1/3)", refTwoKeys, func(e *signEnv) map[string]any { return decoded(multisig(p[0], mine, e.key(index, 1, 3))) }, index},
		{"three signer keys", refTwoKeys, func(e *signEnv) map[string]any {
			return decoded(multisig(mine, e.key(index, 0, 1), e.key(index, 0, 2)))
		}, index},
		{"no tx (not a version 0 PSBT)", refBadPSBT, func(e *signEnv) map[string]any {
			d := decoded(good)
			delete(d, "tx")
			return d
		}, index},
		{"more inputs than the transaction spends", refBadPSBT, func(e *signEnv) map[string]any {
			d := decoded(good, good)
			tx := d["tx"].(map[string]any)
			tx["vin"] = tx["vin"].([]any)[:1]
			return d
		}, index},
		{"no inputs", refBadPSBT, func(e *signEnv) map[string]any { return decoded() }, index},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSignEnv(t)
			got, err := e.run(c.before(e), nil, c.index)
			var r refusal
			if got != "" || !errors.As(err, &r) || r != c.want {
				t.Fatalf("got %q, %v (want refusal %q)", got, err, c.want)
			}
			if m := e.core.methods(); slices.Contains(m, "descriptorprocesspsbt") {
				t.Fatalf("refused request reached descriptorprocesspsbt: %v", m)
			}
			e.noSecrets()
		})
	}
}

// TestSignerResultChecked (fake Core): Core's result must differ from the request only by one added partial signature
// per input, under the signer's key, with the ALL sighash byte; otherwise nothing is returned (result-mismatch).
func TestSignerResultChecked(t *testing.T) {
	const index = 7
	for _, c := range []struct {
		name  string
		after func(e *signEnv, before map[string]any, mine [33]byte) map[string]any
	}{
		{"no signature added", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any { return b }},
		{"a signature on one of two inputs", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any { return withSig(b, 0, mine, goodSig) }},
		{"a signature under another key", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			return withSig(withSig(b, 0, e.party[0], goodSig), 1, mine, goodSig)
		}},
		{"two signatures on one input", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			return withSig(withSig(withSig(b, 0, mine, goodSig), 0, e.party[1], goodSig), 1, mine, goodSig)
		}},
		{"sighash byte NONE", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			return withSig(withSig(b, 0, mine, goodSig[:len(goodSig)-2]+"02"), 1, mine, goodSig)
		}},
		{"signature not hex", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			return withSig(withSig(b, 0, mine, "zz01"), 1, mine, goodSig)
		}},
		{"an output changed", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			a := withSig(withSig(b, 0, mine, goodSig), 1, mine, goodSig)
			a["tx"].(map[string]any)["vout"].([]any)[0].(map[string]any)["value"] = json.Number("0.40000000")
			return a
		}},
		{"an input finalized", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			a := withSig(withSig(b, 0, mine, goodSig), 1, mine, goodSig)
			input(a, 0)["final_scriptwitness"] = []any{"", goodSig}
			return a
		}},
		{"a derivation entry added", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			a := withSig(withSig(b, 0, mine, goodSig), 1, mine, goodSig)
			input(a, 0)["bip32_derivs"] = []any{map[string]any{"pubkey": hex.EncodeToString(mine[:]), "master_fingerprint": "00000000", "path": "m/48h/1h/0h/2h/7/0/0"}}
			return a
		}},
		{"a global field added", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			a := withSig(withSig(b, 0, mine, goodSig), 1, mine, goodSig)
			a["unknown"] = map[string]any{"ff": "00"}
			return a
		}},
		{"an input dropped", func(e *signEnv, b map[string]any, mine [33]byte) map[string]any {
			a := withSig(b, 0, mine, goodSig)
			a["inputs"] = a["inputs"].([]any)[:1]
			return a
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSignEnv(t)
			mine := e.key(index, 0, 0)
			script := multisig(e.party[0], mine, e.party[1])
			before := withSig(decoded(script, script), 1, e.party[1], goodSig)
			got, err := e.run(before, c.after(e, before, mine), index)
			var r refusal
			if got != "" || !errors.As(err, &r) || r != refResult {
				t.Fatalf("got %q, %v (want %q)", got, err, refResult)
			}
			if m := e.core.methods(); !slices.Equal(m, []string{"decodepsbt", "descriptorprocesspsbt", "decodepsbt"}) {
				t.Fatalf("calls %v", m)
			}
			e.noSecrets()
		})
	}
	// QA4W2-2: Core signed, but its result cannot be checked: decodepsbt refuses it (-22), or the node answers nothing
	// to that second decodepsbt. Core's PSBT is not returned either way.
	for _, c := range []struct {
		name   string
		downAt int // the call (1-based, after set-up) that gets no answer; 0 = none
	}{
		{"result not decodable", 0},
		{"node down after signing", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newSignEnv(t)
			mine := e.key(index, 0, 0)
			script := multisig(e.party[0], mine, e.party[1])
			before := decoded(script)
			var after map[string]any // nil: decodepsbt refuses Core's result with -22
			if c.downAt != 0 {
				after = withSig(before, 0, mine, goodSig) // decodable, had the node answered
				e.core.downAt = c.downAt
			}
			got, err := e.run(before, after, index)
			var r refusal
			if got != "" || !errors.As(err, &r) || r != refResult {
				t.Fatalf("got %q, %v (want %q and nothing returned)", got, err, refResult)
			}
			if m := e.core.methods(); !slices.Equal(m, []string{"decodepsbt", "descriptorprocesspsbt", "decodepsbt"}) {
				t.Fatalf("calls %v", m)
			}
			e.noSecrets()
		})
	}
}

// TestSignerCoreFailures (fake Core): a PSBT Core cannot decode is bad-psbt, a descriptorprocesspsbt error is
// core-refused, and a node that does not answer is core-unavailable; Core's error text (which could quote a key) is
// never logged or returned.
func TestSignerCoreFailures(t *testing.T) {
	const index = 7
	e := newSignEnv(t)
	mine := e.key(index, 0, 0)
	before := decoded(multisig(e.party[0], mine, e.party[1]))
	var r refusal
	if _, err := e.s.sign(context.Background(), e.before, index); !errors.As(err, &r) || r != refBadPSBT {
		t.Fatalf("undecodable PSBT: %v", err)
	}
	if _, err := e.s.sign(context.Background(), "not base64!", index); !errors.As(err, &r) || r != refBadPSBT {
		t.Fatalf("not base64: %v", err)
	}
	e.core.refuse["descriptorprocesspsbt"] = true
	if _, err := e.run(before, nil, index); !errors.As(err, &r) || r != refCoreRefused {
		t.Fatalf("descriptorprocesspsbt refused: %v", err)
	}
	e.core.srv.Close()
	if _, err := e.run(before, nil, index); !errors.As(err, &r) || r != refCoreDown {
		t.Fatalf("node down: %v", err)
	}
	e.noSecrets()
	if strings.Contains(e.logs.String(), "fake key text") {
		t.Fatal("Core's error text reached the log")
	}
}

func TestSignerCoreURL(t *testing.T) {
	for _, u := range []string{"", "http://127.0.0.1:1", "ftp://u:p@h:1", "http://u@h:1", "://"} {
		if _, err := newCore(u); err == nil {
			t.Fatalf("%q accepted", u)
		} else if strings.Contains(err.Error(), "p@") {
			t.Fatalf("error repeats the URL: %v", err)
		}
	}
	if _, err := newCore("http://u:p@127.0.0.1:18443"); err != nil {
		t.Fatal(err)
	}
}
