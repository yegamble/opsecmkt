package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"slices"

	"testing"
)

// regtestAddr is a valid regtest P2WPKH address for label.
func regtestAddr(label string) string {
	h := sha256.Sum256([]byte("addr:" + label))
	return segwitAddress("bcrt", 0, h[:20])
}

// netAddr is regtestAddr's address on the escrow node's chain (escrowChains, A-439): the same address on regtest.
func (x *escrowEnv) netAddr(label string) string {
	h := sha256.Sum256([]byte("addr:" + label))
	return segwitAddress(escrowChains[x.A.escrowNetwork()], 0, h[:20])
}

type settleRow struct {
	ID                                              int64
	Kind, State, Signer, TxID, Raw, Cosigned, Error string
	SpentBy                                         string
}

// settleByID reads a settlement row.
func (x *escrowEnv) settleByID(id int64) settleRow {
	x.t.Helper()
	r := settleRow{ID: id}
	if err := x.DB.QueryRow(`SELECT kind,state,COALESCE(signer,''),COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,''),send_error,COALESCE(spent_by,'')
		FROM settlements WHERE id=$1`, id).Scan(&r.Kind, &r.State, &r.Signer, &r.TxID, &r.Raw, &r.Cosigned, &r.Error, &r.SpentBy); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// built is a completed escrow order (refund address refund, funded with sats) whose vendor claimed it to claimTo, and
// the claim built: the order and the settlement.
func (x *escrowEnv) built(refund, claimTo string, sats ...int64) (string, int64) {
	x.t.Helper()
	order := x.completed(refund, sats...)
	if code, body := x.claim(order, x.vendorSess, claimTo); code != 303 {
		x.t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	id, _, state, class := x.settlement(order)
	if state != "awaiting_signature" {
		x.t.Fatalf("claim %s %s", state, class)
	}
	return order, id
}

// partySess is the session and key fingerprint of party.
func (x *escrowEnv) partySess(party string) (string, string) {
	key, sess := x.vendorKey, x.vendorSess
	if party == roleBuyer {
		key, sess = x.buyerKey, x.buyerSess
	}
	k, err := parseEscrowKey(key, keyNetTest)
	if err != nil {
		x.t.Fatal(err)
	}
	return sess, k.Fingerprint
}

// signAs pastes party's good signature (its wallet signing its own copy) and requires it accepted.
func (x *escrowEnv) signAs(order, party string, id int64) {
	x.t.Helper()
	sess, fp := x.partySess(party)
	if code, body := x.sign(order, sess, id, walletSign(x.t, x.partyCopy(order, sess, id), fp, 1)); code != 303 {
		x.t.Fatalf("%s signs: %d %s", party, code, body)
	}
}

var badSigs = map[string]func(pub []byte, txid string) []byte{
	"over the wrong amount": func(pub []byte, txid string) []byte { return fakeSig(pub, "amount-0.0009:"+txid, 1) },
	"r=s=1":                 func([]byte, string) []byte { return []byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01, 0x01} },
}

// signBad pastes party's copy carrying, on every input, a bad signature from the party's own key.
func (x *escrowEnv) signBad(order, party string, id int64, bad func(pub []byte, txid string) []byte) {
	x.t.Helper()
	sess, _ := x.partySess(party)
	cp := x.partyCopy(order, sess, id)
	p, err := decodePSBT(cp)
	if err != nil {
		x.t.Fatal(err)
	}
	pub, _ := hex.DecodeString(x.keyHex(order, party))
	raw := p.Raw
	for i := range p.Inputs {
		raw = addField(x.t, raw, i, append([]byte{0x02}, pub...), bad(pub, p.TxID()))
	}
	if code, body := x.sign(order, sess, id, encodePSBT(raw)); code != 303 {
		x.t.Fatalf("%s's bad signature was not accepted as well-formed: %d %s", party, code, body)
	}
}

// killAtSend makes the next broadcast end the pass as a process killed at that moment would: the pass's context is
// cancelled and the node never receives the transaction. sendNormally undoes it.
func (x *escrowEnv) killAtSend() context.Context {
	ctx, kill := context.WithCancel(context.Background())
	x.core.mu.Lock()
	x.core.beforeSend = func() {
		kill()
		x.core.mu.Lock()
		x.core.sendErr, x.core.beforeSend = -32603, nil
		x.core.mu.Unlock()
	}
	x.core.mu.Unlock()
	return ctx
}

func (x *escrowEnv) sendNormally() {
	x.core.mu.Lock()
	x.core.sendErr, x.core.beforeSend = 0, nil
	x.core.mu.Unlock()
}

// cosignCalls is the co-sign and broadcast RPCs called since the n-th call.
func (x *escrowEnv) cosignCalls(n int) []string {
	return slices.DeleteFunc(x.core.methods()[n:], func(m string) bool {
		return !slices.Contains([]string{"deriveaddresses", "getaddressinfo", "keypoolrefill", "walletprocesspsbt", "combinepsbt", "finalizepsbt", "testmempoolaccept", "sendrawtransaction"}, m)
	})
}

// withGlobalField adds one field to the global map of a raw PSBT.
func withGlobalField(t *testing.T, raw, key, value []byte) []byte {
	t.Helper()
	r := &psbtReader{b: raw, off: 5}
	_, end, err := r.readMap()
	if err != nil {
		t.Fatal(err)
	}
	field := slices.Concat(compactSize(len(key)), key, compactSize(len(value)), value)
	return slices.Concat(raw[:end], field, raw[end:])
}

// TestEscrowFinalTxChecks pins checkFinalTx on a transaction built by hand: the stored one passes; another lock time,
// another output value, a fee above the rate cap, a witness with three items or a signature not ending in 0x01 fail
// with their classes.
func TestEscrowFinalTxChecks(t *testing.T) {
	ws := sortedMultisigScript(fakeChild("a"), fakeChild("b"), fakeChild("c"))
	sum := sha256.Sum256(ws)
	dest := regtestAddr("final-check")
	destScript, _ := escrowAddressScript("regtest", dest)
	in := kv{[]byte{0x01}, append(append([]byte{0xa0, 0x86, 1, 0, 0, 0, 0, 0}, 34, 0, 32), sum[:]...)}
	unsigned := []byte{2, 0, 0, 0, 1}
	unsigned = append(unsigned, make([]byte, 32)...)
	unsigned = append(unsigned, 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff, 1)
	unsigned = append(append(append(unsigned, 0x1e, 0x85, 1, 0, 0, 0, 0, 0), byte(len(destScript))), destScript...) // 99614 sat
	unsigned = append(unsigned, 0, 0, 0, 0)
	stored, err := parsePSBT(buildPSBT(psbtMap(kv{[]byte{0}, unsigned}), psbtMap(in, kv{[]byte{0x05}, ws}), psbtMap()))
	if err != nil {
		t.Fatal(err)
	}
	sig := fakeSig(fakeChild("a"), stored.TxID(), 1)
	final := func(u []byte, items ...[]byte) string {
		w := compactSize(len(items))
		for _, it := range items {
			w = append(append(w, compactSize(len(it))...), it...)
		}
		return hex.EncodeToString(slices.Concat(u[:4], []byte{0, 1}, u[4:len(u)-4], w, u[len(u)-4:]))
	}
	outs := []settleOutput{{"claimant", dest, 99614}}
	good := final(unsigned, nil, sig, sig, ws)
	if c := checkFinalTx("regtest", good, stored, stored.TxID(), outs, 100000, 386); c != "" {
		t.Fatalf("the stored transaction: %s", c)
	}
	other := slices.Clone(unsigned)
	other[len(other)-1] = 1
	sigNone := slices.Clone(sig)
	sigNone[len(sigNone)-1] = 2
	sigBadDER := slices.Clone(sig)
	sigBadDER[4] |= 0x80 // r negative: the right length and the 0x01 at the end, but not strict DER
	if !bytes.HasSuffix(sigBadDER, []byte{1}) || strictDER(sigBadDER[:len(sigBadDER)-1]) {
		t.Fatal("the test's signature is not a non-strict DER signature ending in 0x01")
	}
	for _, c := range []struct {
		name, raw, want string
		outs            []settleOutput
		total, fee      int64
	}{
		{"unreadable", "zz", finalUnreadable, outs, 100000, 386},
		{"no witness", hex.EncodeToString(unsigned), finalUnreadable, outs, 100000, 386},
		{"another lock time", final(other, nil, sig, sig, ws), finalOtherTx, outs, 100000, 386},
		{"another output value", good, finalOutputs, []settleOutput{{"claimant", dest, 99615}}, 100000, 386},
		{"another output address", good, finalOutputs, []settleOutput{{"claimant", regtestAddr("other"), 99614}}, 100000, 386},
		{"an output address that is not a regtest one", good, finalOutputs, []settleOutput{{"claimant", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", 99614}}, 100000, 386},
		{"another fee", good, finalFee, outs, 100001, 386},
		{"a fee above the rate cap", good, finalFee, outs, 99614 + 30000, 30000},
		{"a witness of three items", final(unsigned, nil, sig, ws), finalWitness, outs, 100000, 386},
		{"another witness script", final(unsigned, nil, sig, sig, ws[:104]), finalWitness, outs, 100000, 386},
		{"SIGHASH_NONE", final(unsigned, nil, sig, sigNone, ws), finalWitness, outs, 100000, 386},
		{"a witness of five items", final(unsigned, nil, sig, sig, ws, []byte{1}), finalWitness, outs, 100000, 386},
		{"a witness with a non-empty dummy", final(unsigned, []byte{1}, sig, sig, ws), finalWitness, outs, 100000, 386},
		{"a signature ending in 0x01 that is not strict DER", final(unsigned, nil, sig, sigBadDER, ws), finalWitness, outs, 100000, 386},
	} {
		if got := checkFinalTx("regtest", c.raw, stored, stored.TxID(), c.outs, c.total, c.fee); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The absolute cap binds where the rate cap does not: 12 inputs make about 1,270 vB, so a fee of 110,000 sat is
	// under 100 sat/vB but above 0.001 BTC; 90,000 sat passes both.
	big := []byte{2, 0, 0, 0, 12}
	maps := [][]byte{nil}
	var witnesses [][]byte
	for i := range 12 {
		big = append(append(big, bytes32(byte(i))...), 0, 0, 0, 0, 0, 0xfd, 0xff, 0xff, 0xff)
		maps = append(maps, psbtMap(in, kv{[]byte{0x05}, ws}))
	}
	big = append(append(append(big, 1, 0x1e, 0x85, 1, 0, 0, 0, 0, 0), byte(len(destScript))), destScript...)
	big = append(big, 0, 0, 0, 0)
	maps[0] = psbtMap(kv{[]byte{0}, big})
	bigStored, err := parsePSBT(buildPSBT(append(maps, psbtMap())...))
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		w := []byte{4, 0}
		for _, it := range [][]byte{sig, sig, ws} {
			w = append(append(w, compactSize(len(it))...), it...)
		}
		witnesses = append(witnesses, w)
	}
	bigRaw := hex.EncodeToString(slices.Concat(big[:4], []byte{0, 1}, big[4:len(big)-4], slices.Concat(witnesses...), big[len(big)-4:]))
	if c := checkFinalTx("regtest", bigRaw, bigStored, bigStored.TxID(), outs, 99614+110000, 110000); c != finalFee {
		t.Fatalf("a fee above the absolute cap under the rate cap: %q", c)
	}
	if c := checkFinalTx("regtest", bigRaw, bigStored, bigStored.TxID(), outs, 99614+90000, 90000); c != "" {
		t.Fatalf("a fee under both caps: %q", c)
	}
}

// bytes32 is 32 bytes of b, a made-up outpoint txid.
func bytes32(b byte) []byte { return slices.Repeat([]byte{b}, 32) }

// TestRegtestAddressScript decodes BIP 173 and BIP 350 vectors (with their own prefixes) and regtest addresses, and
// refuses bad checksums, mixed versions and other networks.
func TestRegtestAddressScript(t *testing.T) {
	for _, c := range []struct{ hrp, addr, script string }{
		{"tb", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", "00201863143c14c5166804bd19203356da136c985678cd4d27a1b8c6329604903262"},
		{"tb", "tb1pqqqqp399et2xygdj5xreqhjjvcmzhxw4aywxecjdzew6hylgvsesf3hn0c", "5120000000c4a5cad46221b2a187905e5266362b99d5e91c6ce24d165dab93e86433"},
		{"bc", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "0014751e76e8199196d454941c45d1b3a323f1433bd6"},
	} {
		v, prog, err := decodeSegwitAddress(c.hrp, c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		op := byte(0)
		if v > 0 {
			op = 0x50 + v
		}
		if got := hex.EncodeToString(append([]byte{op, byte(len(prog))}, prog...)); got != c.script {
			t.Errorf("%s: %s", c.addr, got)
		}
	}
	for _, bad := range []string{
		"tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k8", // checksum
		"tb1pw508d6qejxtdg4y5r3zarqfsj6c3",                               // bech32 checksum on version 1
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kemeawh",                     // bech32m checksum on version 0
		"tb1q0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vq24jc47", // padding
	} {
		if _, _, err := decodeSegwitAddress("tb", bad); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
	addr := regtestAddr("x")
	if s, err := escrowAddressScript("regtest", addr); err != nil || len(s) != 22 || s[0] != 0 {
		t.Fatalf("%s: %x %v", addr, s, err)
	}
	for _, other := range []string{"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"} {
		if _, err := escrowAddressScript("regtest", other); err == nil {
			t.Errorf("%s accepted as a regtest address", other)
		}
	}
	if s, err := escrowAddressScript("regtest", base58CheckEncode(append([]byte{0xc4}, make([]byte, 20)...))); err != nil || s[0] != 0xa9 {
		t.Fatalf("P2SH: %x %v", s, err)
	}
	if s, err := escrowAddressScript("regtest", base58CheckEncode(append([]byte{0x6f}, make([]byte, 20)...))); err != nil || s[0] != 0x76 {
		t.Fatalf("P2PKH: %x %v", s, err)
	}
}
