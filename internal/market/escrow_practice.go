package market

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"math/big"
	"slices"
)

// Practice signing (WR-escrow ES-9). A registered escrow key stays inactive until its owner signs a practice PSBT
// with it in their own wallet. The practice transaction spends a made-up output of a 2-of-3 escrow built like a
// real one (wsh(sortedmulti(2, …))) from:
//   - the owner's key at escrowPracticeSuffix, a child reserved for practice and never used by an escrow (migration
//     074 refuses it for a party key);
//   - a throwaway key the server can sign with, and
//   - a public key nobody can sign with (a point with no known private key).
//
// Never the market's key. Its only input is a fabricated outpoint (a txid from a keyed hash) that gettxout confirms
// does not exist, so the transaction can never be broadcast; testmempoolaccept confirms "missing-inputs".
//
// Everything is derived from keyRoot, the account and the key's HMAC, so the same PSBT is rebuilt on every request
// and nothing is stored but user_escrow_keys.active_at. Verifying a pasted copy: the parsed PSBT must be this
// transaction, carry exactly one signature, from the owner's practice key, with sighash byte 0x01, and nothing
// finalized; then Core signs the server's own copy with the throwaway key, the owner's signature alone is added, and
// finalizepsbt must complete (Core verifies both signatures while finalizing).

const (
	escrowPracticeSuffix = "/0/0" // the reserved practice child of every party key
	practicePrevSats     = 100_000
	practiceOutSats      = 99_000
	practiceMaxFeeRate   = 100 // sat/vB, testmempoolaccept's explicit maxfeerate
)

// Fixed messages for a practice PSBT that is refused. None of them quotes the paste.
var (
	errPracticeOtherTx   = &keyRefusal{"This is not the practice transaction shown on this page. Copy the practice PSBT below again, sign it and paste the result."}
	errPracticeFinal     = &keyRefusal{"The pasted PSBT is already finalized. Sign the practice PSBT without finalizing it (in Bitcoin Core: walletprocesspsbt with finalize false, or the PSBT dialog's Sign button) and paste the result."}
	errPracticeUnsigned  = &keyRefusal{"The pasted PSBT has no signature from your registered key. Sign it in the wallet that holds the key you registered, then paste the signed PSBT."}
	errPracticeOtherKey  = &keyRefusal{"The pasted PSBT carries a signature from a key other than your registered key. Sign it only in the wallet that holds the key you registered."}
	errPracticeSighash   = &keyRefusal{"The signature uses a sighash type other than ALL. Sign with the default sighash type (ALL)."}
	errPracticeBadSig    = &keyRefusal{"Your signature did not verify. Sign the practice PSBT shown below again, unchanged, and paste the result."}
	errPracticeNode      = &httpError{Code: 503, Msg: "The practice transaction cannot be checked right now: the market's Bitcoin node did not answer. Try again later."}
	errPracticeKeyShape  = errors.New("practice: unexpected key derivations")
	errPracticeUnsafeOut = errors.New("practice: the fabricated outpoint exists or the transaction is acceptable to the mempool")
)

// practiceTx is the practice PSBT for one registered key.
type practiceTx struct {
	PSBT     string // base64, as served
	TxID     string
	OwnerKey []byte // the owner's practice child public key (the only signature accepted from the owner)
	Server   []byte // the throwaway key's public key
	Blank    []byte // the unsignable key
	signDesc string // private descriptor holding only the throwaway key, for descriptorprocesspsbt
	prevTxID string
}

// practiceSeed is the per-key secret the practice material derives from: never stored.
func (a *App) practiceSeed(userID, keyHMAC string) []byte {
	k := sha256.Sum256([]byte("escrow-practice:" + a.keyRoot))
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte(userID + "\x00" + keyHMAC))
	return m.Sum(nil)
}

func practiceHash(seed []byte, label string, i int) []byte {
	m := hmac.New(sha256.New, seed)
	m.Write([]byte(label))
	m.Write([]byte{byte(i)})
	return m.Sum(nil)
}

// throwawayTprv is a test-network extended private key (depth 0) from seed: the server's practice co-signer.
func throwawayTprv(seed []byte) string {
	for i := 0; ; i++ {
		k := practiceHash(seed, "throwaway-key", i)
		if n := new(big.Int).SetBytes(k); n.Sign() == 0 || n.Cmp(secpN) >= 0 {
			continue
		}
		raw := binary.BigEndian.AppendUint32(nil, 0x04358394)
		raw = append(raw, make([]byte, 9)...) // depth 0, parent fingerprint 0, child 0
		raw = append(raw, practiceHash(seed, "throwaway-chaincode", i)...)
		raw = append(append(raw, 0), k...)
		return base58CheckEncode(raw)
	}
}

// unsignableKey is a compressed public key whose x coordinate is a hash: a point on the curve for which nobody
// knows a private key.
func unsignableKey(seed []byte) []byte {
	for i := 0; ; i++ {
		pub := append([]byte{2}, practiceHash(seed, "unsignable", i)...)
		if onCurve(pub) {
			return pub
		}
	}
}

// buildPractice builds the practice PSBT for key (see the file comment). Its only RPCs are getdescriptorinfo,
// deriveaddresses, gettxout, createpsbt and utxoupdatepsbt; none touches a wallet.
func (e *escrowBTC) buildPractice(ctx context.Context, key escrowKey, seed []byte) (*practiceTx, error) {
	blank := unsignableKey(seed)
	sign := "wsh(sortedmulti(2," + key.Expression() + escrowPracticeSuffix + "," + throwawayTprv(seed) + "," + hex.EncodeToString(blank) + "))"
	public, err := e.descriptorInfo(ctx, sign)
	if err != nil {
		return nil, err
	}
	addrs, err := e.deriveAddresses(ctx, public, -1)
	if err != nil {
		return nil, err
	}
	if len(addrs) != 1 {
		return nil, &escrowRPCError{Method: "deriveaddresses", Class: "bad-reply"}
	}
	prev := practiceHash(seed, "prevout", 0)
	pt := &practiceTx{Blank: blank, signDesc: sign, prevTxID: hex.EncodeToString(prev)}
	if found, _, _, err := e.txOut(ctx, pt.prevTxID, 0); err != nil {
		return nil, err
	} else if found {
		log.Print("escrow: practice transaction refused: its made-up outpoint exists on the escrow node")
		return nil, errPracticeUnsafeOut
	}
	// The practice transaction pays the same escrow script back, less a fee: the owner's wallet shows one input
	// and one output of the practice escrow.
	unsigned, err := e.createPSBT(ctx, []escrowOutpoint{{TxID: pt.prevTxID, Vout: 0}}, []escrowPayment{{Address: addrs[0], Sats: practiceOutSats}})
	if err != nil {
		return nil, err
	}
	p, err := decodePSBT(unsigned)
	if err != nil || len(p.Tx.In) != 1 || len(p.Tx.Out) != 1 || p.Tx.Out[0].Value != practiceOutSats {
		return nil, &escrowRPCError{Method: "createpsbt", Class: "bad-reply"}
	}
	// createpsbt cannot know the made-up output being spent: add its witness UTXO (value, the escrow script) so the
	// owner's wallet can sign and utxoupdatepsbt fills in the witness script and key derivations.
	spk := p.Tx.Out[0].Script
	utxo := binary.LittleEndian.AppendUint64(nil, practicePrevSats)
	utxo = append(append(utxo, compactSize(len(spk))...), spk...)
	withUTXO, err := p.withInputField(0, []byte{0x01}, utxo)
	if err != nil {
		return nil, err
	}
	served, err := e.utxoUpdatePSBT(ctx, encodePSBT(withUTXO), []string{public})
	if err != nil {
		return nil, err
	}
	final, err := decodePSBT(served)
	if err != nil || final.TxID() != p.TxID() {
		return nil, &escrowRPCError{Method: "utxoupdatepsbt", Class: "bad-reply"}
	}
	pt.PSBT, pt.TxID = served, final.TxID()
	if pt.OwnerKey, pt.Server, err = practiceKeys(final.Inputs[0], key, blank); err != nil {
		return nil, err
	}
	return pt, nil
}

// practiceKeys reads the practice input's witness script, 2 <k1> <k2> <k3> 3 CHECKMULTISIG with three distinct
// sorted compressed keys, and names them: the owner's (the derivation at key's origin + escrowPracticeSuffix), the
// unsignable one and the throwaway key (the third).
func practiceKeys(in psbtInput, key escrowKey, blank []byte) (owner, server []byte, err error) {
	ws := in.WitnessScript
	if len(ws) != 105 || ws[0] != 0x52 || ws[1] != 33 || ws[35] != 33 || ws[69] != 33 || ws[103] != 0x53 || ws[104] != 0xae {
		return nil, nil, errPracticeKeyShape
	}
	keys := [][]byte{ws[2:35], ws[36:69], ws[70:103]}
	if !slices.IsSortedFunc(keys, bytes.Compare) || bytes.Equal(keys[0], keys[1]) || bytes.Equal(keys[1], keys[2]) {
		return nil, nil, errPracticeKeyShape
	}
	want := slices.Concat(key.Path, []uint32{0, 0})
	for _, d := range in.Derivations {
		if hex.EncodeToString(d.Fingerprint[:]) == key.Fingerprint && slices.Equal(d.Path, want) {
			if owner != nil {
				return nil, nil, errPracticeKeyShape
			}
			owner = d.PubKey
		}
	}
	for _, k := range keys {
		switch {
		case bytes.Equal(k, owner), bytes.Equal(k, blank):
		case server == nil:
			server = k
		default:
			return nil, nil, errPracticeKeyShape
		}
	}
	if owner == nil || server == nil || !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, owner) }) || !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, blank) }) {
		return nil, nil, errPracticeKeyShape
	}
	return owner, server, nil
}

// checkPracticePaste applies the offline checks to a pasted practice PSBT and returns the owner's signature.
func checkPracticePaste(pt *practiceTx, pasted string) (psbtSig, error) {
	p, err := decodePSBT(pasted)
	if err != nil {
		return psbtSig{}, err
	}
	if p.TxID() != pt.TxID || len(p.Inputs) != 1 {
		return psbtSig{}, errPracticeOtherTx
	}
	in := p.Inputs[0]
	if in.Finalized {
		return psbtSig{}, errPracticeFinal
	}
	if in.HasSighashType && in.SighashType != 1 {
		return psbtSig{}, errPracticeSighash
	}
	var mine *psbtSig
	for i, s := range in.PartialSigs {
		if !bytes.Equal(s.PubKey, pt.OwnerKey) {
			return psbtSig{}, errPracticeOtherKey
		}
		mine = &in.PartialSigs[i]
	}
	if mine == nil {
		return psbtSig{}, errPracticeUnsigned
	}
	if mine.SighashByte() != 0x01 {
		return psbtSig{}, errPracticeSighash
	}
	return *mine, nil
}

// verifyPractice checks a pasted practice PSBT against pt, built in the same request (buildPractice has just
// checked with gettxout that its outpoint does not exist); nil means the owner's key signed the practice
// transaction.
func (e *escrowBTC) verifyPractice(ctx context.Context, pt *practiceTx, pasted string) error {
	sig, err := checkPracticePaste(pt, pasted)
	if err != nil {
		return err
	}
	// The server's own copy, signed with the throwaway key only: one signature, so not complete on its own.
	signed, err := e.throwawaySign(ctx, pt.PSBT, pt.signDesc)
	if err != nil {
		return err
	}
	sp, err := decodePSBT(signed.PSBT)
	if err != nil || signed.Complete || sp.TxID() != pt.TxID || len(sp.Inputs[0].PartialSigs) != 1 || !bytes.Equal(sp.Inputs[0].PartialSigs[0].PubKey, pt.Server) {
		return &escrowRPCError{Method: "descriptorprocesspsbt", Class: "bad-reply"}
	}
	// Only the owner's signature is taken from the paste; nothing else of it reaches Core.
	check, err := sp.withInputField(0, append([]byte{0x02}, sig.PubKey...), sig.Sig)
	if err != nil {
		return err
	}
	rawHex, complete, err := e.finalizePSBT(ctx, encodePSBT(check))
	if err != nil {
		return err
	}
	if !complete {
		return errPracticeBadSig
	}
	allowed, reject, err := e.testMempoolAccept(ctx, rawHex, practiceMaxFeeRate)
	if err != nil {
		return err
	}
	if allowed || reject != "missing-inputs" {
		log.Print("escrow: practice check refused: testmempoolaccept did not report missing-inputs for the practice transaction")
		return errPracticeUnsafeOut
	}
	return nil
}
