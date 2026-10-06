package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"reflect"
	"strings"

	"opsecmkt/internal/btc"
)

// Signing (A-454, ruling P4-R17). A request is one escrow's PSBT and its per-order index. The signer decodes it with
// Core's decodepsbt and refuses, before anything is signed, unless every input is the same 2-of-3 P2WSH escrow:
// witness script exactly 2 <three sorted distinct compressed keys> 3 CHECKMULTISIG, its witness_utxo paying
// P2WSH(sha256(script)), no sighash type but ALL, and exactly one of the script's keys the signer's own at
// account/index/0/k or account/index/1/k with k < 20. Core then signs through descriptorprocesspsbt with "ALL"
// hard-coded, the escrow's own descriptor (the two other keys as hex, the signer's as the master tprv and path),
// bip32derivs false and finalize false. Core signs every input whose PSBT witness script holds a key the descriptor
// gives it (whatever the descriptor's script; probed on Core 31.1 regtest), which is why every input is checked here
// first. Last, Core's result is decoded and compared with the request: it must differ only by one added partial
// signature per input, under the signer's key, ending in the ALL sighash byte; anything else is refused and nothing is
// returned. The signer never returns or logs key material; each refusal is one fixed class.

// refusal is a fixed error class, the only thing a refused request answers (and logs).
type refusal string

func (r refusal) Error() string { return string(r) }

const (
	refSighash     refusal = "sighash"           // an input's sighash type is set and is not ALL
	refScript      refusal = "not-escrow-script" // an input's witness script is not exactly a sorted 2-of-3 CHECKMULTISIG of compressed keys
	refWitnessUTXO refusal = "witness-utxo"      // an input's witness_utxo is missing or does not pay P2WSH(sha256(script)), or non_witness_utxo disagrees
	refMixed       refusal = "mixed-scripts"     // the inputs spend more than one escrow script
	refKeyNotFound refusal = "key-not-found"     // the script holds none of the signer's keys for the requested index
	refTwoKeys     refusal = "two-signer-keys"   // the script holds more than one of them
	refBadPSBT     refusal = "bad-psbt"          // not base64, Core cannot decode it, not a version 0 PSBT, or no inputs
	refResult      refusal = "result-mismatch"   // Core's result is not the request plus one signature per input under the signer's key
	refCoreRefused refusal = "core-refused"      // descriptorprocesspsbt answered an error
	refCoreDown    refusal = "core-unavailable"  // the escrow node did not answer (or the signer's user may not call the method)
)

// siblings is how many sibling keys of each chain (/0/k and /1/k, k < siblings) the signer signs for (P4-R2).
const siblings = 20

// rpcCaller is the one Core method the signer uses (core.call).
type rpcCaller interface {
	call(ctx context.Context, method string, params []any, out any) error
}

type signer struct {
	keys *signerKeys
	core rpcCaller
	log  *log.Logger
}

func newSigner(keys *signerKeys, c rpcCaller, l *log.Logger) *signer {
	return &signer{keys: keys, core: c, log: l}
}

// decodedPSBT is the part of decodepsbt the checks read.
type decodedPSBT struct {
	Tx *struct {
		Vin []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	} `json:"tx"`
	Inputs []struct {
		Sighash       *string `json:"sighash"`
		WitnessScript *struct {
			Hex string `json:"hex"`
		} `json:"witness_script"`
		WitnessUTXO *struct {
			Amount       json.Number `json:"amount"`
			ScriptPubKey struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"witness_utxo"`
		NonWitnessUTXO *struct {
			Txid string `json:"txid"`
			Vout []struct {
				N            uint32      `json:"n"`
				Value        json.Number `json:"value"`
				ScriptPubKey struct {
					Hex string `json:"hex"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"non_witness_utxo"`
	} `json:"inputs"`
}

// sign checks the request, has Core sign it and checks Core's result (see the comment at the top of this file).
func (s *signer) sign(ctx context.Context, psbt string, index uint32) (string, error) {
	out, err := s.signChecked(ctx, psbt, index)
	if err != nil {
		var r refusal
		if !errors.As(err, &r) {
			r = refCoreDown
		}
		s.log.Printf("sign index %d: refused (%s); nothing signed", index, r)
		return "", r
	}
	s.log.Printf("sign index %d: signed", index)
	return out, nil
}

func (s *signer) signChecked(ctx context.Context, psbt string, index uint32) (string, error) {
	if _, err := base64.StdEncoding.DecodeString(psbt); err != nil || psbt == "" {
		return "", refBadPSBT
	}
	before, raw, err := s.decode(ctx, psbt)
	if err != nil {
		return "", err
	}
	script, err := checkInputs(before)
	if err != nil {
		return "", err
	}
	keys := scriptKeys(script)
	mine, chain, leaf, err := s.findKey(keys, index)
	if err != nil {
		return "", err
	}
	parts := make([]string, 3)
	for i, k := range keys {
		parts[i] = hex.EncodeToString(k[:])
		if k == mine {
			parts[i] = s.keys.descriptorKey(index, chain, leaf)
		}
	}
	desc := btc.WithChecksum("wsh(sortedmulti(2," + strings.Join(parts, ",") + "))")
	var res struct {
		PSBT string `json:"psbt"`
	}
	switch err := s.core.call(ctx, "descriptorprocesspsbt", []any{psbt, []string{desc}, "ALL", false, false}, &res); {
	case errors.Is(err, errCoreRefused):
		return "", refCoreRefused
	case err != nil:
		return "", refCoreDown
	}
	_, after, err := s.decode(ctx, res.PSBT)
	if err != nil {
		return "", refResult
	}
	if !onlyOneSignatureAdded(raw, after, hex.EncodeToString(mine[:])) {
		return "", refResult
	}
	return res.PSBT, nil
}

// decode is decodepsbt as the checks read it and as the generic tree the result comparison reads.
func (s *signer) decode(ctx context.Context, psbt string) (decodedPSBT, map[string]any, error) {
	var tree map[string]any
	switch err := s.core.call(ctx, "decodepsbt", []any{psbt}, &tree); {
	case errors.Is(err, errCoreRefused):
		return decodedPSBT{}, nil, refBadPSBT
	case err != nil:
		return decodedPSBT{}, nil, refCoreDown
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return decodedPSBT{}, nil, refBadPSBT
	}
	var d decodedPSBT
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&d) != nil {
		return decodedPSBT{}, nil, refBadPSBT
	}
	return d, tree, nil
}

// checkInputs applies the per-input refusals in a fixed order (sighash, script, witness_utxo, then one script for
// all inputs) and returns the escrow's witness script.
func checkInputs(d decodedPSBT) ([]byte, error) {
	if d.Tx == nil || len(d.Inputs) == 0 || len(d.Inputs) != len(d.Tx.Vin) {
		return nil, refBadPSBT
	}
	var first []byte
	for i, in := range d.Inputs {
		if in.Sighash != nil && *in.Sighash != "ALL" {
			return nil, refSighash
		}
		if in.WitnessScript == nil {
			return nil, refScript
		}
		script, err := hex.DecodeString(in.WitnessScript.Hex)
		if err != nil || !isEscrowScript(script) {
			return nil, refScript
		}
		sum := sha256.Sum256(script)
		spk := "0020" + hex.EncodeToString(sum[:])
		if in.WitnessUTXO == nil || in.WitnessUTXO.ScriptPubKey.Hex != spk {
			return nil, refWitnessUTXO
		}
		if nw := in.NonWitnessUTXO; nw != nil {
			// Core signs the amount of non_witness_utxo when the input has one: it must be the same output.
			vin := d.Tx.Vin[i]
			ok := nw.Txid == vin.Txid
			found := false
			for _, o := range nw.Vout {
				if o.N == vin.Vout {
					found = true
					ok = ok && o.ScriptPubKey.Hex == spk && o.Value == in.WitnessUTXO.Amount
				}
			}
			if !ok || !found {
				return nil, refWitnessUTXO
			}
		}
		if first == nil {
			first = script
		} else if !bytes.Equal(script, first) {
			return nil, refMixed
		}
	}
	return first, nil
}

// isEscrowScript reports whether script is exactly OP_2 <33> <33> <33> OP_3 OP_CHECKMULTISIG with three distinct
// compressed keys on the curve in ascending byte order (wsh(sortedmulti(2,…)) of three keys).
func isEscrowScript(script []byte) bool {
	if len(script) != 1+3*34+2 || script[0] != 0x52 || script[103] != 0x53 || script[104] != 0xae {
		return false
	}
	keys := make([][]byte, 3)
	for i := range keys {
		if script[1+34*i] != 0x21 {
			return false
		}
		keys[i] = script[2+34*i : 35+34*i]
		if !btc.OnCurve(keys[i]) {
			return false
		}
	}
	return bytes.Compare(keys[0], keys[1]) < 0 && bytes.Compare(keys[1], keys[2]) < 0
}

// scriptKeys is the three keys of a script isEscrowScript accepted, in script order.
func scriptKeys(script []byte) (keys [3][33]byte) {
	for i := range keys {
		copy(keys[i][:], script[2+34*i:35+34*i])
	}
	return keys
}

// findKey finds the signer's key for the requested index among the script's keys: exactly one of them must be
// account/index/0/k or account/index/1/k with k < siblings.
func (s *signer) findKey(keys [3][33]byte, index uint32) (mine [33]byte, chain, leaf uint32, err error) {
	found := 0
	ours := s.keys.siblingKeys(index, siblings)
	for _, sk := range keys {
		if at, ok := ours[sk]; ok {
			found++
			mine, chain, leaf = sk, at[0], at[1]
		}
	}
	switch {
	case found == 0:
		return mine, 0, 0, refKeyNotFound
	case found > 1:
		return mine, 0, 0, refTwoKeys
	}
	return mine, chain, leaf, nil
}

// onlyOneSignatureAdded compares decodepsbt's trees of the request and of Core's result: everything but each input's
// partial_signatures must be equal, and each input's partial signatures must be the request's plus exactly one, under
// mine (hex), a hex string ending in the SIGHASH_ALL byte 01.
func onlyOneSignatureAdded(before, after map[string]any, mine string) bool {
	if len(before) != len(after) {
		return false
	}
	for k, v := range before {
		if k == "inputs" {
			continue
		}
		if w, ok := after[k]; !ok || !reflect.DeepEqual(v, w) {
			return false
		}
	}
	bi, ok1 := before["inputs"].([]any)
	ai, ok2 := after["inputs"].([]any)
	if !ok1 || !ok2 || len(bi) != len(ai) || len(bi) == 0 {
		return false
	}
	for i := range bi {
		b, ok1 := bi[i].(map[string]any)
		a, ok2 := ai[i].(map[string]any)
		if !ok1 || !ok2 {
			return false
		}
		bs, _ := b["partial_signatures"].(map[string]any)
		as, _ := a["partial_signatures"].(map[string]any)
		if _, already := bs[mine]; already || len(as) != len(bs)+1 {
			return false
		}
		for k, v := range bs {
			if as[k] != v {
				return false
			}
		}
		sig, _ := as[mine].(string)
		if raw, err := hex.DecodeString(sig); err != nil || len(raw) < 9 || raw[len(raw)-1] != 0x01 {
			return false
		}
		for k, v := range b {
			if k == "partial_signatures" {
				continue
			}
			if w, ok := a[k]; !ok || !reflect.DeepEqual(v, w) {
				return false
			}
		}
		for k := range a {
			if _, ok := b[k]; !ok && k != "partial_signatures" {
				return false
			}
		}
	}
	return true
}
