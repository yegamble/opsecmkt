package market

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
)

// The final transaction of an escrow settlement, read and checked in Go before anything leaves the market (WR-escrow
// ES-2, slice P1-c2): Bitcoin Core finalizes it, and the escrow watcher then re-checks what Core returned against what
// the market built and stored. The transaction must be the stored one (its serialization without witnesses, and so its
// txid), pay exactly the frozen outputs (each value, and each script decoded here from its regtest address), have the
// stored network fee within the caps, and spend each input with the escrow's 2-of-3 witness whose two signatures
// both end in SIGHASH_ALL. A transaction that fails any check is never stored or broadcast.

// Fixed classes of a final transaction that failed a re-check (settlements.send_error, logs).
const (
	finalUnreadable = "final-unreadable"
	finalOtherTx    = "final-other-tx"
	finalOutputs    = "final-outputs"
	finalFee        = "final-fee"
	finalWitness    = "final-witness"
)

// signedTx is a transaction in the network serialization with witnesses (BIP 144).
type signedTx struct {
	Unsigned []byte // the serialization without witnesses: what the txid hashes
	In       []psbtTxIn
	Out      []psbtTxOut
	Witness  [][][]byte // per input, its witness stack
	VSize    int64
}

// TxID is the transaction's id, as Bitcoin Core prints it.
func (t *signedTx) TxID() string {
	h := doubleSHA256(t.Unsigned)
	slices.Reverse(h[:])
	return hex.EncodeToString(h[:])
}

// parseSignedTx reads a segwit transaction: version, the marker and flag, inputs with empty scriptSigs, outputs, one
// witness stack per input and the lock time, with nothing after it.
func parseSignedTx(raw []byte) (*signedTx, error) {
	bad := errors.New("escrow: the final transaction is malformed")
	if len(raw) < 10 || raw[4] != 0 || raw[5] != 1 {
		return nil, bad
	}
	r := &psbtReader{b: raw, off: 6}
	t := &signedTx{}
	nIn, err := r.compactSize()
	if err != nil || nIn == 0 {
		return nil, bad
	}
	for range nIn {
		b, err := r.bytes(36)
		if err != nil {
			return nil, bad
		}
		var in psbtTxIn
		copy(in.TxID[:], b[:32])
		in.Vout = binary.LittleEndian.Uint32(b[32:])
		if n, err := r.compactSize(); err != nil || n != 0 {
			return nil, bad
		}
		seq, err := r.bytes(4)
		if err != nil {
			return nil, bad
		}
		in.Sequence = binary.LittleEndian.Uint32(seq)
		t.In = append(t.In, in)
	}
	nOut, err := r.compactSize()
	if err != nil || nOut == 0 {
		return nil, bad
	}
	for range nOut {
		out, err := readTxOut(r)
		if err != nil {
			return nil, bad
		}
		t.Out = append(t.Out, out)
	}
	witnessStart := r.off
	for range nIn {
		items, err := r.compactSize()
		if err != nil {
			return nil, bad
		}
		var stack [][]byte
		for range items {
			n, err := r.compactSize()
			if err != nil {
				return nil, bad
			}
			item, err := r.bytes(n)
			if err != nil {
				return nil, bad
			}
			stack = append(stack, item)
		}
		t.Witness = append(t.Witness, stack)
	}
	if r.left() != 4 {
		return nil, bad
	}
	t.Unsigned = slices.Concat(raw[:4], raw[6:witnessStart], raw[len(raw)-4:])
	weight := int64(len(t.Unsigned))*3 + int64(len(raw))
	t.VSize = (weight + 3) / 4
	return t, nil
}

// escrowChains is the one chain table of Bitcoin escrow (A-439, P4-R5): each chain escrow can run on, keyed by the
// node's getblockchaininfo chain, and the human-readable part of its segwit addresses. Mainnet ("main") and testnet3
// ("test") are not escrow chains (escrow hard-codes test-network key versions and coin type 1): startEscrow refuses a
// node on any chain not in the table (errEscrowChain; A-443 opened signet and testnet4).
var escrowChains = map[string]string{"regtest": "bcrt", "signet": "tb", "testnet4": "tb"}

// escrowAddressScript is the scriptPubKey an address of chain pays: a bech32 (witness version 0: P2WPKH or P2WSH) or
// bech32m (versions 1 to 16) address with the chain's human-readable part (escrowChains), or a base58check P2PKH
// (version 0x6f) or P2SH (0xc4) address, the test-network versions all three chains share. Anything else, and any
// chain not in the table, is refused.
func escrowAddressScript(chain, addr string) ([]byte, error) {
	hrp, ok := escrowChains[chain]
	if !ok {
		return nil, errors.New("escrow: not an escrow chain")
	}
	if strings.HasPrefix(addr, hrp+"1") {
		version, program, err := decodeSegwitAddress(hrp, addr)
		if err != nil {
			return nil, err
		}
		op := byte(0)
		if version > 0 {
			op = 0x50 + version
		}
		return append([]byte{op, byte(len(program))}, program...), nil
	}
	payload, err := base58CheckDecode(addr)
	if err != nil || len(payload) != 21 {
		return nil, errors.New("escrow: not an address of the escrow chain")
	}
	switch payload[0] {
	case 0x6f:
		return slices.Concat([]byte{0x76, 0xa9, 20}, payload[1:], []byte{0x88, 0xac}), nil
	case 0xc4:
		return slices.Concat([]byte{0xa9, 20}, payload[1:], []byte{0x87}), nil
	}
	return nil, errors.New("escrow: not an address of the escrow chain")
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if top>>i&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, 2*len(hrp)+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// decodeSegwitAddress decodes a lower-case segwit address with the human-readable part hrp (BIP 173, BIP 350): the
// witness version and program, checked for the checksum constant of its version and the program's length.
func decodeSegwitAddress(hrp, addr string) (byte, []byte, error) {
	bad := errors.New("escrow: not a valid segwit address")
	sep := strings.LastIndexByte(addr, '1')
	if len(addr) > 90 || sep < 1 || addr[:sep] != hrp || len(addr)-sep-1 < 7 {
		return 0, nil, bad
	}
	data := make([]byte, 0, len(addr)-sep-1)
	for i := sep + 1; i < len(addr); i++ {
		v := strings.IndexByte(bech32Charset, addr[i])
		if v < 0 {
			return 0, nil, bad
		}
		data = append(data, byte(v))
	}
	version := data[0]
	want := uint32(1) // bech32
	if version > 0 {
		want = 0x2bc830a3 // bech32m
	}
	if version > 16 || bech32Polymod(append(bech32HRPExpand(hrp), data...)) != want {
		return 0, nil, bad
	}
	// The 5-bit groups after the version, without the 6 checksum groups, regrouped into bytes; the leftover bits
	// must be zero padding of fewer than 5 bits.
	var program []byte
	acc, bits := uint32(0), uint(0)
	for _, v := range data[1 : len(data)-6] {
		acc, bits = acc<<5|uint32(v), bits+5
		if bits >= 8 {
			bits -= 8
			program = append(program, byte(acc>>bits))
		}
	}
	if bits >= 5 || acc&(1<<bits-1) != 0 || len(program) < 2 || len(program) > 40 || version == 0 && len(program) != 20 && len(program) != 32 {
		return 0, nil, bad
	}
	return version, program, nil
}

// checkFinalTx re-checks a finalized settlement transaction (rawHex, from finalizepsbt) against the stored build: the
// stored unsigned transaction and txid, the frozen outputs (each address read as an address of chain, the escrow
// node's: escrowAddressScript), the stored network fee within escrowMaxFee and escrowMaxFeeRate, and on every input the
// 2-of-3 witness [empty, signature, signature, witness script] with the input's stored witness script and two DER
// signatures ending in SIGHASH_ALL. It returns a fixed class, or "".
func checkFinalTx(chain, rawHex string, stored *psbtPacket, txid string, outs []settleOutput, inputTotal, networkFee int64) string {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return finalUnreadable
	}
	t, err := parseSignedTx(raw)
	if err != nil {
		return finalUnreadable
	}
	if !bytes.Equal(t.Unsigned, stored.UnsignedTx) || t.TxID() != txid || len(t.In) != len(stored.Inputs) {
		return finalOtherTx
	}
	if len(t.Out) != len(outs) {
		return finalOutputs
	}
	var paid int64
	for i, o := range outs {
		script, err := escrowAddressScript(chain, o.Address)
		if err != nil || t.Out[i].Value != o.Sats || !bytes.Equal(t.Out[i].Script, script) {
			return finalOutputs
		}
		paid += o.Sats
	}
	if fee := inputTotal - paid; fee != networkFee || fee <= 0 || fee > escrowMaxFee || fee > escrowMaxFeeRate*t.VSize {
		return finalFee
	}
	for i, w := range t.Witness {
		ws := stored.Inputs[i].WitnessScript
		if len(w) != 4 || len(w[0]) != 0 || len(ws) == 0 || !bytes.Equal(w[3], ws) {
			return finalWitness
		}
		for _, sig := range w[1:3] {
			if len(sig) < 9 || sig[len(sig)-1] != 0x01 || !strictDER(sig[:len(sig)-1]) {
				return finalWitness
			}
		}
	}
	return ""
}
