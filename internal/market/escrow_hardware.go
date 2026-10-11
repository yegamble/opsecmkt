package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
)

// A settlement spends at most 50 deposits. A 128 KiB funding transaction per
// deposit takes at most 6.25 MiB; 8 MiB leaves space for the ordinary PSBT maps
// and signatures. These limits apply only to the hardware download/upload.
const (
	hardwareMaxPrevTxBytes = 128 << 10
	hardwareMaxBytes       = 8 << 20
	hardwareMaxBase64      = (hardwareMaxBytes + 2) / 3 * 4
)

var errHardwareFunding = errors.New("escrow: a funding transaction is unavailable or does not match the settlement input")

// fundingTxOutput checks the complete serialized transaction's txid and returns
// the output an input spends. Both legacy and segwit funding transactions are
// accepted; the latter hashes the serialization without its witness.
func fundingTxOutput(h, txid string, vout uint32) (psbtTxOut, error) {
	if len(h) == 0 || len(h) > hardwareMaxPrevTxBytes*2 || len(h)%2 != 0 {
		return psbtTxOut{}, errHardwareFunding
	}
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) < 10 {
		return psbtTxOut{}, errHardwareFunding
	}
	r := &psbtReader{b: raw, off: 4}
	segwit := r.left() >= 2 && raw[4] == 0
	if segwit {
		if raw[5] != 1 {
			return psbtTxOut{}, errHardwareFunding
		}
		r.off = 6
	}
	start := r.off
	nIn, err := r.compactSize()
	if err != nil || nIn == 0 || nIn > r.left()/41 {
		return psbtTxOut{}, errHardwareFunding
	}
	for range nIn {
		if _, err = r.bytes(36); err != nil {
			return psbtTxOut{}, errHardwareFunding
		}
		n, e := r.compactSize()
		if e != nil {
			return psbtTxOut{}, errHardwareFunding
		}
		if _, err = r.bytes(n + 4); err != nil {
			return psbtTxOut{}, errHardwareFunding
		}
	}
	nOut, err := r.compactSize()
	if err != nil || nOut == 0 || nOut > r.left()/9 || uint64(vout) >= uint64(nOut) {
		return psbtTxOut{}, errHardwareFunding
	}
	var selected psbtTxOut
	for i := range nOut {
		out, e := readTxOut(r)
		if e != nil {
			return psbtTxOut{}, errHardwareFunding
		}
		if uint32(i) == vout {
			selected = out
		}
	}
	witnessStart := r.off
	if segwit {
		for range nIn {
			items, e := r.compactSize()
			if e != nil || items > r.left() {
				return psbtTxOut{}, errHardwareFunding
			}
			for range items {
				n, e := r.compactSize()
				if e != nil {
					return psbtTxOut{}, errHardwareFunding
				}
				if _, err = r.bytes(n); err != nil {
					return psbtTxOut{}, errHardwareFunding
				}
			}
		}
	}
	if r.left() != 4 {
		return psbtTxOut{}, errHardwareFunding
	}
	unsigned := raw
	if segwit {
		unsigned = slices.Concat(raw[:4], raw[start:witnessStart], raw[len(raw)-4:])
	}
	hash := doubleSHA256(unsigned)
	slices.Reverse(hash[:])
	if hex.EncodeToString(hash[:]) != txid {
		return psbtTxOut{}, errHardwareFunding
	}
	return selected, nil
}

// hardwarePSBTCopy starts with the reader's ordinary copy, retaining only that
// reader's origin. It adds each verified funding transaction as non_witness_utxo.
func hardwarePSBTCopy(ctx context.Context, stored string, key escrowKeyRow, fetch func(context.Context, string) (string, error)) (string, error) {
	copyText, err := settlementCopy(stored, key)
	if err != nil {
		return "", err
	}
	p, err := decodePSBT(copyText)
	if err != nil {
		return "", err
	}
	if len(p.Inputs) > escrowMaxInputs {
		return "", errHardwareFunding
	}
	raw := p.Raw
	seen := make(map[string]string)
	for i, in := range p.Tx.In {
		id := in.TxID
		slices.Reverse(id[:])
		txid := hex.EncodeToString(id[:])
		h := seen[txid]
		if h == "" {
			h, err = fetch(ctx, txid)
			if err != nil || h == "" || len(h) > hardwareMaxPrevTxBytes*2 {
				return "", errHardwareFunding
			}
			seen[txid] = h
		}
		out, err := fundingTxOutput(h, txid, in.Vout)
		if err != nil || p.Inputs[i].WitnessUTXO == nil || out.Value != p.Inputs[i].WitnessUTXO.Value || !bytes.Equal(out.Script, p.Inputs[i].WitnessUTXO.Script) {
			return "", errHardwareFunding
		}
		funding, _ := hex.DecodeString(h)
		if existing := p.Inputs[i].NonWitnessUTXO; len(existing) != 0 {
			if !bytes.Equal(existing, funding) {
				return "", errHardwareFunding
			}
			continue // Core already supplied the verified funding transaction.
		}
		next, err := parsePSBTLimit(raw, hardwareMaxBytes)
		if err != nil {
			return "", err
		}
		raw, err = next.withInputFieldLimit(i, []byte{0}, funding, hardwareMaxBytes)
		if err != nil {
			return "", err
		}
	}
	return encodePSBT(raw), nil
}
