package market

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeEscrowCore is a scripted Bitcoin Core for escrow DB tests (simulated RPC, not chain evidence). It keeps
// Core's semantics where the escrow code depends on them: getdescriptorinfo returns a public form, deriveaddresses
// needs a checksum, createpsbt and utxoupdatepsbt build real PSBT bytes, descriptorprocesspsbt signs with the
// descriptor's private key only, and finalizepsbt completes only with two valid signatures, or when the input is
// already finalized (Core trusts final fields). Its "signatures" are keyed hashes shaped as DER: fakeSig.
type fakeEscrowCore struct {
	t  *testing.T
	mu sync.Mutex

	chain         string
	arbiterFP     string
	arbiterDesc   string
	arbiterCode   int  // JSON-RPC error for getrawchangeaddress (0 = none)
	prevoutExists bool // gettxout finds the practice outpoint
	allowed       bool // testmempoolaccept
	reject        string
	calls         []fakeCall
}

type fakeCall struct {
	Path, Method string
	Params       json.RawMessage
}

const fakeArbiterFP = "a1b2c3d4"

func newFakeEscrowCore(t *testing.T) (*fakeEscrowCore, *payRPCServer) {
	c := &fakeEscrowCore{t: t, chain: "regtest", arbiterFP: fakeArbiterFP, reject: "missing-inputs"}
	c.arbiterDesc = "wpkh([" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:] + "/1/*)#fakechk0"
	return c, newPayRPCServer(t, "escrowapp", "escrow-rpc-pass", false, c.handle)
}

func (c *fakeEscrowCore) methods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var m []string
	for _, call := range c.calls {
		m = append(m, call.Method)
	}
	return m
}

var fakeTprv = regexp.MustCompile(`tprv[1-9A-HJ-NP-Za-km-z]+`)

func fakePubFromTprv(tprv string) []byte {
	h := sha256.Sum256([]byte("pub:" + tprv))
	return append([]byte{3}, h[:]...)
}

func fakeChild(expr string) []byte {
	h := sha256.Sum256([]byte("child:" + expr))
	return append([]byte{2}, h[:]...)
}

// fakeSig is the fake Core's signature of txid by pub with sighash byte sh: valid DER shape, sighash byte last.
func fakeSig(pub []byte, txid string, sh byte) []byte {
	r := sha256.Sum256(append([]byte("r"+txid), pub...))
	s := sha256.Sum256(append([]byte("s"+txid+string(rune(sh))), pub...))
	r[0], s[0] = r[0]&0x7f|1, s[0]&0x7f|1
	sig := append([]byte{0x30, 0x44, 0x02, 0x20}, r[:]...)
	sig = append(append(sig, 0x02, 0x20), s[:]...)
	return append(sig, sh)
}

func (c *fakeEscrowCore) handle(path, method string, params json.RawMessage) (any, int, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fakeCall{path, method, params})
	var p []json.RawMessage
	json.Unmarshal(params, &p)
	str := func(i int) string {
		var s string
		if i < len(p) {
			json.Unmarshal(p[i], &s)
		}
		return s
	}
	wallet := path == "/wallet/opsecmkt-arbiter"
	switch method {
	case "getblockchaininfo":
		return map[string]any{"chain": c.chain, "blocks": 200}, 0, ""
	case "getrawchangeaddress":
		if !wallet {
			return nil, -18, "Requested wallet does not exist or is not loaded"
		}
		if c.arbiterCode != 0 {
			return nil, c.arbiterCode, "Error: This wallet has no available keys"
		}
		return "bcrt1qarbiterchange000000000000000000000", 0, ""
	case "getaddressinfo":
		return map[string]any{"ismine": true, "hdmasterfingerprint": c.arbiterFP, "parent_desc": c.arbiterDesc}, 0, ""
	case "getdescriptorinfo":
		desc := str(0)
		public := fakeTprv.ReplaceAllStringFunc(desc, func(k string) string { return "fakepub" + hex.EncodeToString(fakePubFromTprv(k)) })
		return map[string]any{"descriptor": public + "#fakechk1", "checksum": "fakechk2", "hasprivatekeys": public != desc}, 0, ""
	case "deriveaddresses":
		desc := str(0)
		if !strings.Contains(desc, "#") {
			return nil, -5, "Missing checksum"
		}
		h := sha256.Sum256([]byte(desc))
		return []string{"bcrt1q" + hex.EncodeToString(h[:])[:40]}, 0, ""
	case "gettxout":
		if c.prevoutExists {
			return map[string]any{"confirmations": 1, "value": json.Number("0.00100000")}, 0, ""
		}
		return nil, 0, ""
	case "createpsbt":
		return c.createPSBT(p), 0, ""
	case "utxoupdatepsbt":
		var descs []string
		json.Unmarshal(p[1], &descs)
		return c.utxoUpdate(str(0), descs[0]), 0, ""
	case "descriptorprocesspsbt":
		var descs []string
		json.Unmarshal(p[1], &descs)
		pk := c.mustParse(str(0))
		tprv := fakeTprv.FindString(descs[0])
		if tprv == "" || str(2) != "ALL" {
			return nil, -8, "unexpected fake call"
		}
		pub := fakePubFromTprv(tprv)
		out, err := pk.withInputField(0, append([]byte{0x02}, pub...), fakeSig(pub, pk.TxID(), 1))
		if err != nil {
			c.t.Errorf("fake descriptorprocesspsbt: %v", err)
		}
		return map[string]any{"psbt": encodePSBT(out), "complete": false}, 0, ""
	case "finalizepsbt":
		pk := c.mustParse(str(0))
		return map[string]any{"hex": "02000000" + pk.TxID(), "complete": fakeComplete(pk)}, 0, ""
	case "testmempoolaccept":
		return []map[string]any{{"txid": "t", "allowed": c.allowed, "reject-reason": c.reject}}, 0, ""
	case "walletprocesspsbt":
		return map[string]any{"psbt": str(0), "complete": false}, 0, ""
	case "combinepsbt":
		return "cHNidP8combined", 0, ""
	case "keypoolrefill":
		return nil, 0, ""
	case "importdescriptors":
		return []map[string]any{{"success": true}}, 0, ""
	case "sendrawtransaction":
		return "txid-broadcast", 0, ""
	}
	return nil, -32601, "Method not found"
}

func (c *fakeEscrowCore) mustParse(b64 string) *psbtPacket {
	pk, err := decodePSBT(b64)
	if err != nil {
		c.t.Fatalf("fake core got an unreadable PSBT: %v", err)
	}
	return pk
}

// fakeComplete is finalizepsbt's verdict: a finalized input, or two valid signatures from the witness script's
// keys and no invalid one.
func fakeComplete(pk *psbtPacket) bool {
	in := pk.Inputs[0]
	if in.Finalized {
		return true
	}
	if len(in.WitnessScript) != 105 {
		return false
	}
	keys := [][]byte{in.WitnessScript[2:35], in.WitnessScript[36:69], in.WitnessScript[70:103]}
	valid := 0
	for _, s := range in.PartialSigs {
		if !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, s.PubKey) }) {
			continue
		}
		if !bytes.Equal(s.Sig, fakeSig(s.PubKey, pk.TxID(), s.SighashByte())) {
			return false
		}
		valid++
	}
	return valid >= 2
}

func (c *fakeEscrowCore) createPSBT(p []json.RawMessage) string {
	var ins []struct {
		TxID string `json:"txid"`
		Vout uint32 `json:"vout"`
	}
	var outs []map[string]json.Number
	json.Unmarshal(p[0], &ins)
	json.Unmarshal(p[1], &outs)
	tx := []byte{2, 0, 0, 0}
	tx = append(tx, compactSize(len(ins))...)
	for _, in := range ins {
		id, _ := hex.DecodeString(in.TxID)
		slices.Reverse(id)
		tx = append(tx, id...)
		tx = binary.LittleEndian.AppendUint32(tx, in.Vout)
		tx = append(tx, 0, 0xfd, 0xff, 0xff, 0xff)
	}
	tx = append(tx, compactSize(len(outs))...)
	for _, o := range outs {
		for addr, amt := range o {
			sats, _ := parseAmount(string(amt), 8)
			h := sha256.Sum256([]byte(addr))
			tx = binary.LittleEndian.AppendUint64(tx, uint64(sats))
			tx = append(append(tx, 34, 0, 32), h[:]...)
		}
	}
	tx = append(tx, 0, 0, 0, 0)
	maps := [][]byte{psbtMap(kv{[]byte{0}, tx})}
	for range ins {
		maps = append(maps, psbtMap())
	}
	for range outs {
		maps = append(maps, psbtMap())
	}
	return base64.StdEncoding.EncodeToString(buildPSBT(maps...))
}

var fakeMulti = regexp.MustCompile(`^wsh\(sortedmulti\(2,([^,]+),([^,]+),([^,]+)\)\)#`)

// utxoUpdate adds, as Core does when the input has a witness UTXO, the witness script and the derivations of keys
// with an origin: "[fp/path]tpub…/suffix" (the fake child fakeChild) and "fakepub<hex>" (its own fingerprint).
func (c *fakeEscrowCore) utxoUpdate(b64, desc string) string {
	pk := c.mustParse(b64)
	if pk.Inputs[0].WitnessUTXO == nil {
		return b64
	}
	m := fakeMulti.FindStringSubmatch(desc)
	if m == nil {
		c.t.Fatalf("fake utxoupdatepsbt: descriptor %q", desc)
	}
	type entry struct{ pub, deriv []byte }
	var entries []entry
	for _, el := range m[1:] {
		switch {
		case strings.HasPrefix(el, "["):
			end := strings.IndexByte(el, ']')
			fp, _ := hex.DecodeString(el[1:9])
			rest := el[end+1:]
			path, err := parseOriginPath(el[9:end] + rest[strings.IndexByte(rest, '/'):])
			if err != nil {
				c.t.Fatalf("fake utxoupdatepsbt: path in %q: %v", el, err)
			}
			d := slices.Clone(fp)
			for _, step := range path {
				d = binary.LittleEndian.AppendUint32(d, step)
			}
			entries = append(entries, entry{fakeChild(el[end+1:]), d})
		case strings.HasPrefix(el, "fakepub"):
			pub, _ := hex.DecodeString(el[len("fakepub"):])
			entries = append(entries, entry{pub, pub[1:5]})
		default:
			pub, _ := hex.DecodeString(el)
			entries = append(entries, entry{pub, nil})
		}
	}
	keys := [][]byte{entries[0].pub, entries[1].pub, entries[2].pub}
	slices.SortFunc(keys, bytes.Compare)
	ws := []byte{0x52}
	for _, k := range keys {
		ws = append(append(ws, 33), k...)
	}
	ws = append(ws, 0x53, 0xae)
	raw, err := pk.withInputField(0, []byte{0x05}, ws)
	for _, e := range entries {
		if err != nil || e.deriv == nil {
			continue
		}
		var next *psbtPacket
		if next, err = parsePSBT(raw); err == nil {
			raw, err = next.withInputField(0, append([]byte{0x06}, e.pub...), e.deriv)
		}
	}
	if err != nil {
		c.t.Fatalf("fake utxoupdatepsbt: %v", err)
	}
	return encodePSBT(raw)
}

// fakeOwnerSign is the owner's wallet in the fake: it adds a signature (sighash sh) from the practice PSBT's
// derivation at fingerprint fp, the owner's practice child key.
func fakeOwnerSign(t *testing.T, practice, fp string, sh byte) string {
	t.Helper()
	pk, err := decodePSBT(practice)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range pk.Inputs[0].Derivations {
		if hex.EncodeToString(d.Fingerprint[:]) == fp {
			out, err := pk.withInputField(0, append([]byte{0x02}, d.PubKey...), fakeSig(d.PubKey, pk.TxID(), sh))
			if err != nil {
				t.Fatal(err)
			}
			return encodePSBT(out)
		}
	}
	t.Fatalf("no derivation at fingerprint %s", fp)
	return ""
}
