package market

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// A real Bitcoin Core 31.1 regtest PSBT (scratch p1a/psbt-fixture.sh): walletcreatefundedpsbt spending one coinbase
// output to two P2WPKH outputs, then walletprocesspsbt sign=true "ALL" bip32derivs=true finalize=false. It carries a
// global xpub-free map with the unsigned tx, non_witness_utxo, witness_utxo, one partial signature and three BIP32
// derivations (one input, two outputs).
const (
	testCorePSBTUnsigned = "cHNidP8BAHECAAAAAQrX4wQVUwZMjGNiNRvTEUd0pMyrGi/NeUIeFLBFKwo1AAAAAAD9////AmYfFSEBAAAAFgAUKlAaVqm72Z0AaZ9IFyOPkqJe6wOA0fAIAAAAABYAFJ+fZKEWOb0qNywsbkHfS5y9CIfSAAAAAAABAIMCAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/////wJRAP7///8CAPIFKgEAAAAWABTU7q3CFlsVqhTarhEewpLHU1x8VwAAAAAAAAAAJmokqiGp7eL2HD9x0d79P6mZ36NpU3VcaQaJeZlitIvr2DaXToz5AAAAAAEBHwDyBSoBAAAAFgAU1O6twhZbFaoU2q4RHsKSx1NcfFciBgJfZc+4pZCeHO9fGIyw0+ZjdmLUqdNmQwf5F8Azc5psnRh0UPk1VAAAgAEAAIAAAACAAAAAAAAAAAAAIgICxAPdmFFVw43gyQQXl+OH/ciFdDfQVbYgzjYqLCoPg74YdFD5NVQAAIABAACAAAAAgAEAAAAAAAAAACICAzIpEVIME1qBgu1CPFWJsQZ2VUa7bZycTQ84+c7M2Wi6GHRQ+TVUAACAAQAAgAAAAIAAAAAAAQAAAAA="
	testCorePSBTSigned   = "cHNidP8BAHECAAAAAQrX4wQVUwZMjGNiNRvTEUd0pMyrGi/NeUIeFLBFKwo1AAAAAAD9////AmYfFSEBAAAAFgAUKlAaVqm72Z0AaZ9IFyOPkqJe6wOA0fAIAAAAABYAFJ+fZKEWOb0qNywsbkHfS5y9CIfSAAAAAAABAIMCAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA/////wJRAP7///8CAPIFKgEAAAAWABTU7q3CFlsVqhTarhEewpLHU1x8VwAAAAAAAAAAJmokqiGp7eL2HD9x0d79P6mZ36NpU3VcaQaJeZlitIvr2DaXToz5AAAAAAEBHwDyBSoBAAAAFgAU1O6twhZbFaoU2q4RHsKSx1NcfFciAgJfZc+4pZCeHO9fGIyw0+ZjdmLUqdNmQwf5F8Azc5psnUcwRAIgIR/KeKRsVjY16Vad52oEt6ws/aX3yidpfGK5coyz5D0CIFD781llxVFilaA7uRIW4f63upd800vPm8VNEA383xTZASIGAl9lz7ilkJ4c718YjLDT5mN2YtSp02ZDB/kXwDNzmmydGHRQ+TVUAACAAQAAgAAAAIAAAAAAAAAAAAAiAgLEA92YUVXDjeDJBBeX44f9yIV0N9BVtiDONiosKg+Dvhh0UPk1VAAAgAEAAIAAAACAAQAAAAAAAAAAIgIDMikRUgwTWoGC7UI8VYmxBnZVRrttnJxNDzj5zszZaLoYdFD5NVQAAIABAACAAAAAgAAAAAABAAAAAA=="
	testCorePSBTTxID     = "7ba9ae25073c8a73fa87f97874b593252e1ac79b5d7efb2573fa56c1b8464083"
	testCorePSBTSigner   = "025f65cfb8a5909e1cef5f188cb0d3e6637662d4a9d3664307f917c033739a6c9d"
	testCorePSBTSig      = "30440220211fca78a46c563635e9569de76a04b7ac2cfda5f7ca27697c62b9728cb3e43d022050fbf35965c5516295a03bb91216e1feb7ba977cd34bcf9bc54d100dfcdf14d901"
)

func TestParsePSBTCoreFixture(t *testing.T) {
	p, err := decodePSBT(testCorePSBTSigned)
	if err != nil {
		t.Fatal(err)
	}
	if p.TxID() != testCorePSBTTxID || len(p.Tx.In) != 1 || len(p.Tx.Out) != 2 || len(p.Inputs) != 1 {
		t.Fatalf("txid %s, %d in, %d out, %d input maps", p.TxID(), len(p.Tx.In), len(p.Tx.Out), len(p.Inputs))
	}
	in := p.Inputs[0]
	if len(in.PartialSigs) != 1 || hex.EncodeToString(in.PartialSigs[0].PubKey) != testCorePSBTSigner || hex.EncodeToString(in.PartialSigs[0].Sig) != testCorePSBTSig {
		t.Fatalf("partial sigs %+v", in.PartialSigs)
	}
	if in.PartialSigs[0].SighashByte() != 0x01 || in.HasSighashType || in.Finalized || in.WitnessUTXO == nil || in.WitnessUTXO.Value != 5000000000 {
		t.Fatalf("input %+v", in)
	}
	if len(in.Derivations) != 1 || hex.EncodeToString(in.Derivations[0].PubKey) != testCorePSBTSigner ||
		hex.EncodeToString(in.Derivations[0].Fingerprint[:]) != "7450f935" || formatOriginPath(in.Derivations[0].Path) != "/84h/1h/0h/0/0" {
		t.Fatalf("derivations %+v", in.Derivations)
	}
	u, err := decodePSBT(testCorePSBTUnsigned)
	if err != nil || u.TxID() != testCorePSBTTxID || len(u.Inputs[0].PartialSigs) != 0 {
		t.Fatalf("unsigned: %v", err)
	}
	// Surrounding whitespace and line breaks from a copy are accepted.
	if _, err := decodePSBT("\n  " + testCorePSBTSigned[:40] + "\r\n" + testCorePSBTSigned[40:] + "  \n"); err != nil {
		t.Fatalf("wrapped copy: %v", err)
	}
}

// kv, psbtMap and testTx build PSBTs field by field for the malformed-input table.
type kv struct{ k, v []byte }

func psbtMap(entries ...kv) []byte {
	var b []byte
	for _, e := range entries {
		b = append(append(append(append(b, compactSize(len(e.k))...), e.k...), compactSize(len(e.v))...), e.v...)
	}
	return append(b, 0)
}

func buildPSBT(maps ...[]byte) []byte {
	return append([]byte("psbt\xff"), bytes.Join(maps, nil)...)
}

// testTx serializes a version-2 transaction without witnesses: inputs spending (txid byte, vout) with empty
// scriptSigs and outputs of value with a 22-byte P2WPKH-like script.
func testTx(inputs int, outputs ...int64) []byte {
	b := []byte{2, 0, 0, 0}
	b = append(b, compactSize(inputs)...)
	for i := range inputs {
		b = append(b, bytes.Repeat([]byte{byte(i + 1)}, 32)...)
		b = append(b, 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff)
	}
	b = append(b, compactSize(len(outputs))...)
	for _, v := range outputs {
		b = binary.LittleEndian.AppendUint64(b, uint64(v))
		b = append(b, 22, 0, 20)
		b = append(b, bytes.Repeat([]byte{0xaa}, 20)...)
	}
	return append(b, 0, 0, 0, 0)
}

func sigEntry(pub byte, sighash byte) kv {
	key := append([]byte{0x02, 0x02}, bytes.Repeat([]byte{pub}, 32)...)
	der, _ := hex.DecodeString(testCorePSBTSig[:len(testCorePSBTSig)-2])
	return kv{key, append(der, sighash)}
}

func TestParsePSBTRefusals(t *testing.T) {
	tx := testTx(1, 1000)
	g := psbtMap(kv{[]byte{0}, tx})
	empty := psbtMap()
	derivKey := append([]byte{0x06, 0x02}, bytes.Repeat([]byte{7}, 32)...)
	cases := []struct {
		name string
		raw  []byte
	}{
		{"no magic", append([]byte("psbu\xff"), g...)},
		{"magic only", []byte("psbt\xff")},
		{"missing input map", buildPSBT(g)},
		{"missing output map", buildPSBT(g, empty)},
		{"trailing byte", append(buildPSBT(g, empty, empty), 0)},
		{"no unsigned tx", buildPSBT(psbtMap(kv{[]byte{0x70}, []byte{1}}), empty, empty)},
		{"duplicate unsigned tx", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{[]byte{0}, tx}), empty, empty)},
		{"unsigned tx key too long", buildPSBT(psbtMap(kv{[]byte{0, 1}, tx}), empty, empty)},
		{"duplicate global unknown key", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{[]byte{0x70}, nil}, kv{[]byte{0x70}, nil}), empty, empty)},
		{"version 2", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{[]byte{0xfb}, []byte{2, 0, 0, 0}}), empty, empty)},
		{"version field short", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{[]byte{0xfb}, []byte{0}}), empty, empty)},
		{"empty key", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{nil, nil}), empty, empty)},
		{"duplicate input key", buildPSBT(g, psbtMap(sigEntry(3, 1), sigEntry(3, 1)), empty)},
		{"duplicate output key", buildPSBT(g, empty, psbtMap(kv{[]byte{0x70}, nil}, kv{[]byte{0x70}, nil}))},
		{"partial sig key short", buildPSBT(g, psbtMap(kv{[]byte{0x02, 0x02, 1}, []byte{1}}), empty)},
		{"partial sig empty", buildPSBT(g, psbtMap(kv{sigEntry(3, 1).k, nil}), empty)},
		{"partial sig not DER", buildPSBT(g, psbtMap(kv{sigEntry(3, 1).k, []byte{0x31, 0x00, 0x01}}), empty)},
		{"partial sig key bad prefix", buildPSBT(g, psbtMap(kv{append([]byte{0x02, 0x05}, bytes.Repeat([]byte{1}, 32)...), sigEntry(3, 1).v}), empty)},
		{"sighash field short", buildPSBT(g, psbtMap(kv{[]byte{0x03}, []byte{1}}), empty)},
		{"sighash key too long", buildPSBT(g, psbtMap(kv{[]byte{0x03, 0}, []byte{1, 0, 0, 0}}), empty)},
		{"derivation not multiple of 4", buildPSBT(g, psbtMap(kv{derivKey, []byte{1, 2, 3, 4, 5}}), empty)},
		{"derivation without fingerprint", buildPSBT(g, psbtMap(kv{derivKey, nil}), empty)},
		{"derivation too deep", buildPSBT(g, psbtMap(kv{derivKey, make([]byte, 4+4*256)}), empty)},
		{"witness utxo short", buildPSBT(g, psbtMap(kv{[]byte{0x01}, []byte{1, 2, 3}}), empty)},
		{"witness utxo trailing", buildPSBT(g, psbtMap(kv{[]byte{0x01}, append(make([]byte, 8), 0, 9)}), empty)},
		{"witness utxo negative", buildPSBT(g, psbtMap(kv{[]byte{0x01}, append(bytes.Repeat([]byte{0xff}, 8), 0)}), empty)},
		{"tx with scriptSig", buildPSBT(psbtMap(kv{[]byte{0}, func() []byte { b := testTx(1, 1000); b[41] = 1; return append(append(b[:42:42], 0x51), b[42:]...) }()}), empty, empty)},
		{"tx with witness marker", buildPSBT(psbtMap(kv{[]byte{0}, append([]byte{2, 0, 0, 0, 0, 1}, tx[4:]...)}), empty, empty)},
		{"tx with no inputs", buildPSBT(psbtMap(kv{[]byte{0}, testTx(0, 1000)}), empty)},
		{"tx with no outputs", buildPSBT(psbtMap(kv{[]byte{0}, testTx(1)}), empty)},
		{"tx trailing byte", buildPSBT(psbtMap(kv{[]byte{0}, append(testTx(1, 1000), 0)}), empty, empty)},
		{"tx truncated", buildPSBT(psbtMap(kv{[]byte{0}, tx[:len(tx)-1]}), empty, empty)},
		{"tx output above 21M BTC", buildPSBT(psbtMap(kv{[]byte{0}, testTx(1, 21_000_000*100_000_000+1)}), empty, empty)},
		{"tx input count beyond body", buildPSBT(psbtMap(kv{[]byte{0}, append([]byte{2, 0, 0, 0, 0xfe, 0xff, 0xff, 0xff, 0x0f}, tx[5:]...)}), empty, empty)},
		{"non-canonical compact size", append([]byte("psbt\xff"), 0xfd, 1, 0)},
		{"non-canonical key length, otherwise valid", slices.Concat([]byte("psbt\xff\xfd\x01\x00\x00"), compactSize(len(tx)), tx, []byte{0}, empty, empty)},
		{"compact size beyond body", append([]byte("psbt\xff"), 0xff, 0, 0, 0, 0, 1, 0, 0, 0, 0)},
		{"value length beyond body", append([]byte("psbt\xff"), 1, 0, 0xfe, 0xff, 0xff, 0xff, 0x7f)},
		{"oversized", buildPSBT(psbtMap(kv{[]byte{0}, tx}, kv{[]byte{0x70}, make([]byte, psbtMaxBytes)}), empty, empty)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parsePSBT(c.raw); err == nil {
				t.Fatal("accepted")
			}
			if _, err := decodePSBT(base64.StdEncoding.EncodeToString(c.raw)); err == nil {
				t.Fatal("accepted as base64")
			}
		})
	}
	// The table's base: one input map and one output map, well formed.
	if _, err := parsePSBT(buildPSBT(g, empty, empty)); err != nil {
		t.Fatalf("base: %v", err)
	}
	for _, bad := range []string{"", "cHNidP8", "!!!!", strings.Repeat("A", psbtMaxBase64+4)} {
		if _, err := decodePSBT(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// Every prefix of a real PSBT is refused without panicking.
	raw, _ := base64.StdEncoding.DecodeString(testCorePSBTSigned)
	for i := range raw {
		if _, err := parsePSBT(raw[:i]); err == nil {
			t.Fatalf("prefix %d accepted", i)
		}
	}
}

// Unknown, global-xpub and proprietary fields are ignored (ES-2, D-45: honest wallets add them).
func TestParsePSBTIgnoresUnknownFields(t *testing.T) {
	tx := testTx(2, 1000, 2000)
	xpubKey := append([]byte{0x01}, make([]byte, 78)...)
	p, err := parsePSBT(buildPSBT(
		psbtMap(kv{[]byte{0}, tx}, kv{xpubKey, make([]byte, 8)}, kv{[]byte{0xfc, 3, 'a', 'b', 'c'}, []byte{1}}, kv{[]byte{0x70}, nil}, kv{[]byte{0xfb}, make([]byte, 4)}),
		psbtMap(sigEntry(3, 1), kv{[]byte{0x03}, []byte{1, 0, 0, 0}}, kv{[]byte{0x70}, []byte{9}}, kv{[]byte{0xfc, 0}, nil}),
		psbtMap(sigEntry(4, 2), kv{[]byte{0x08}, []byte{0}}),
		psbtMap(kv{[]byte{0x02}, []byte{1}}),
		psbtMap(),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Inputs) != 2 || len(p.Inputs[0].PartialSigs) != 1 || !p.Inputs[0].HasSighashType || p.Inputs[0].SighashType != 1 || p.Inputs[0].Finalized {
		t.Fatalf("input 0 %+v", p.Inputs[0])
	}
	if p.Inputs[1].PartialSigs[0].SighashByte() != 2 || !p.Inputs[1].Finalized {
		t.Fatalf("input 1 %+v", p.Inputs[1])
	}
}

func TestPSBTWithInputField(t *testing.T) {
	g := psbtMap(kv{[]byte{0}, testTx(2, 1000)})
	raw := buildPSBT(g, psbtMap(), psbtMap(kv{[]byte{0x70}, nil}), psbtMap())
	p, err := parsePSBT(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := sigEntry(9, 1)
	out, err := p.withInputField(1, e.k, e.v)
	if err != nil {
		t.Fatal(err)
	}
	q, err := parsePSBT(out)
	if err != nil || len(q.Inputs[0].PartialSigs) != 0 || len(q.Inputs[1].PartialSigs) != 1 || q.TxID() != p.TxID() {
		t.Fatalf("after insert: %+v %v", q, err)
	}
	if _, err := q.withInputField(1, e.k, e.v); err == nil {
		t.Fatal("duplicate key inserted")
	}
	if _, err := q.withInputField(2, e.k, e.v); err == nil {
		t.Fatal("inserted into a missing input")
	}
}

func FuzzParsePSBT(f *testing.F) {
	for _, s := range []string{testCorePSBTSigned, testCorePSBTUnsigned} {
		raw, _ := base64.StdEncoding.DecodeString(s)
		f.Add(raw)
	}
	f.Add(buildPSBT(psbtMap(kv{[]byte{0}, testTx(1, 1)}), psbtMap(sigEntry(1, 1)), psbtMap()))
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, err := parsePSBT(raw)
		if err != nil {
			return
		}
		_ = p.TxID()
		if len(p.Inputs) != len(p.Tx.In) || len(p.Tx.In) == 0 {
			t.Fatalf("inconsistent: %d maps for %d inputs", len(p.Inputs), len(p.Tx.In))
		}
		again, err := parsePSBT(bytes.Clone(raw))
		if err != nil || again.TxID() != p.TxID() {
			t.Fatalf("reparse: %v", err)
		}
	})
}
