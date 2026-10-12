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
	"strconv"

	"opsecmkt/internal/btc"
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
// A per-order wallet key (layout per_order, shape (b): A-455, rulings P4-R2 and P4-R18) practises as its escrows will
// work: the owner's wallet is built for one order from the account key's child at the reserved per-order index
// escrowPracticeIndex (never used by an escrow: migration 088 refuses it for a party key), and the two other keys are
// the /0/0 children of cosigner keys served rooted (depth 0, parent fingerprint 0, child 0; A-453's serializer): the
// throwaway key's child at that index (the server signs with the throwaway key along that path) and an unsignable
// key's. Go derives the cosigner keys; Core derives the same address from the throwaway key's own path, or nothing is
// served (the P4-R2 cross-check). The owner's wallet signs its key at account/escrowPracticeIndex/0/0.
//
// Everything is derived from keyRoot, the account and the key's HMAC, so the same PSBT is rebuilt on every request
// and nothing is stored but user_escrow_keys.active_at. Verifying a pasted copy: the parsed PSBT must be this
// transaction, carry exactly one signature, from the owner's practice key, with sighash byte 0x01, and nothing
// finalized; then Core signs the server's own copy with the throwaway key, the owner's signature alone is added, and
// finalizepsbt must complete (Core verifies both signatures while finalizing).

const (
	escrowPracticeSuffix = "/0/0"    // the reserved practice child of every party key (layout core)
	escrowPracticeIndex  = 1<<31 - 1 // the reserved per-order index of practice (layout per_order)
	practicePrevSats     = 100_000
	practiceOutSats      = 99_000
	practiceMaxFeeRate   = 100 // sat/vB, testmempoolaccept's explicit maxfeerate
)

// Fixed messages for a practice PSBT that is refused. None of them quotes the paste.
var (
	errPracticeOtherTx   = &keyRefusal{msg: "This is not the practice transaction shown on this page. Copy the practice PSBT below again, sign it and paste the result."}
	errPracticeFinal     = &keyRefusal{msg: "The pasted PSBT is already finalized. Sign the practice PSBT without finalizing it (in Bitcoin Core: walletprocesspsbt with finalize false, or the PSBT dialog's Sign button) and paste the result."}
	errPracticeUnsigned  = &keyRefusal{msg: "The pasted PSBT has no signature from your registered key. Sign it in the wallet that holds the key you registered, then paste the signed PSBT."}
	errPracticeOtherKey  = &keyRefusal{msg: "The pasted PSBT carries a signature from a key other than your registered key. Sign it only in the wallet that holds the key you registered."}
	errPracticeSighash   = &keyRefusal{msg: "The signature uses a sighash type other than ALL. Sign with the default sighash type (ALL)."}
	errPracticeBadSig    = &keyRefusal{msg: "Your signature did not verify. Sign the practice PSBT shown below again, unchanged, and paste the result."}
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

	// A per-order wallet key's practice (layout per_order) also serves what the owner's practice wallet is built from.
	PerOrder  bool
	OwnPath   string    // the owner's key origin for the practice wallet: [fingerprint/account path/2147483647]
	OwnDeriv  string    // its derivation path as a wallet asks for it: m/account path/2147483647
	Cosigners [2]string // the throwaway and unsignable cosigner keys, rooted, as SLIP-132 Vpubs
	Address   string    // the practice escrow address: for a per-order wallet key, its practice wallet's first receiving address
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
func throwawayTprv(seed []byte, version uint32) string {
	k, chainCode := throwawayKey(seed)
	raw := binary.BigEndian.AppendUint32(nil, version)
	raw = append(raw, make([]byte, 9)...) // depth 0, parent fingerprint 0, child 0
	raw = append(raw, chainCode...)
	raw = append(append(raw, 0), k...)
	return base58CheckEncode(raw)
}

// throwawayKey is the throwaway key's private scalar and chain code.
func throwawayKey(seed []byte) (k, chainCode []byte) {
	for i := 0; ; i++ {
		k := practiceHash(seed, "throwaway-key", i)
		if n := new(big.Int).SetBytes(k); n.Sign() == 0 || n.Cmp(secpN) >= 0 {
			continue
		}
		return k, practiceHash(seed, "throwaway-chaincode", i)
	}
}

// throwawayPub is the throwaway key's public side (depth 0): its key k·G with its chain code. Computed with math/big,
// which is not constant time; the key guards nothing (it signs only a transaction that can never be broadcast).
func throwawayPub(seed []byte) bip32Key {
	k, chainCode := throwawayKey(seed)
	pub := bip32Key{PubKey: btc.Compress(btc.Mul(new(big.Int).SetBytes(k), btc.G))}
	copy(pub.ChainCode[:], chainCode)
	return pub
}

// unsignableXPub is an extended public key whose key is unsignableKey: nobody knows the private key of it or of any
// of its children (a child's private key is the parent's plus a known tweak).
func unsignableXPub(seed []byte) bip32Key {
	k := bip32Key{}
	copy(k.PubKey[:], unsignableKey(seed))
	copy(k.ChainCode[:], practiceHash(seed, "unsignable-chaincode", 0))
	return k
}

// practiceCosigner is cosigner root's child at the practice index, served rooted (Rooted: depth 0, parent fingerprint
// 0, child 0) to the owner's practice wallet, and that child's /0/0 child, the key the wallet's first address uses.
func practiceCosigner(root bip32Key) (child, first bip32Key, err error) {
	child, used, err := ckdPub(root, escrowPracticeIndex)
	if err != nil || used != escrowPracticeIndex {
		return child, first, errPracticeKeyShape // BIP32 calls that child invalid (probability about 2^-127)
	}
	if first, _, err = ckdPub(child, 0); err == nil {
		first, _, err = ckdPub(first, 0)
	}
	return child, first, err
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

// buildPractice builds the practice PSBT for key, of a per-order wallet key when perOrder (see the file comment). Its
// only RPCs are getdescriptorinfo, deriveaddresses, gettxout, createpsbt and utxoupdatepsbt; none touches a wallet.
func (e *escrowBTC) buildPractice(ctx context.Context, key escrowKey, seed []byte, perOrder bool) (*practiceTx, error) {
	pt := &practiceTx{prevTxID: hex.EncodeToString(practiceHash(seed, "prevout", 0)), PerOrder: perOrder}
	ownerPath := slices.Concat(key.Path, []uint32{0, 0})
	var public string
	var server []byte // the throwaway key in the script, as Go derives it (per_order)
	var err error
	if perOrder {
		ownerPath = slices.Concat(key.Path, []uint32{escrowPracticeIndex, 0, 0})
		if public, server, err = e.perOrderPracticeDescriptors(ctx, key, seed, pt); err != nil {
			return nil, err
		}
	} else {
		pt.Blank = unsignableKey(seed)
		pt.signDesc = "wsh(sortedmulti(2," + key.Expression() + escrowPracticeSuffix + "," + throwawayTprv(seed, e.keyNet().prv) + "," + hex.EncodeToString(pt.Blank) + "))"
		if public, err = e.descriptorInfo(ctx, pt.signDesc); err != nil {
			return nil, err
		}
		addrs, err := e.deriveAddresses(ctx, public, -1)
		if err != nil {
			return nil, err
		}
		if len(addrs) != 1 {
			return nil, &escrowRPCError{Method: "deriveaddresses", Class: "bad-reply"}
		}
		pt.Address = addrs[0]
	}
	if found, _, _, err := e.txOut(ctx, pt.prevTxID, 0); err != nil {
		return nil, err
	} else if found {
		log.Print("escrow: practice transaction refused: its made-up outpoint exists on the escrow node")
		return nil, errPracticeUnsafeOut
	}
	// The practice transaction pays the same escrow script back, less a fee: the owner's wallet shows one input
	// and one output of the practice escrow.
	unsigned, err := e.createPSBT(ctx, []escrowOutpoint{{TxID: pt.prevTxID, Vout: 0}}, []escrowPayment{{Address: pt.Address, Sats: practiceOutSats}})
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
	if pt.OwnerKey, pt.Server, err = practiceKeys(final.Inputs[0], key.Fingerprint, ownerPath, pt.Blank); err != nil {
		return nil, err
	}
	if perOrder && !bytes.Equal(pt.Server, server) {
		return nil, errPracticeKeyShape
	}
	return pt, nil
}

// perOrderPracticeDescriptors composes a per-order wallet key's practice escrow (see the file comment) and fills in
// pt's practice-wallet material, its unsignable key and the private descriptor the throwaway key signs with. It
// returns the public descriptor (with Core's checksum) and the throwaway key in the script. Core must derive one and
// the same address from the rooted cosigner keys Go derived and from the throwaway key's own path (the signing
// descriptor's public form); otherwise nothing is served (errPerOrderMismatch).
func (e *escrowBTC) perOrderPracticeDescriptors(ctx context.Context, key escrowKey, seed []byte, pt *practiceTx) (string, []byte, error) {
	throwaway, server, err := practiceCosigner(throwawayPub(seed))
	if err != nil {
		return "", nil, err
	}
	blank, blankFirst, err := practiceCosigner(unsignableXPub(seed))
	if err != nil {
		return "", nil, err
	}
	pt.Blank = blankFirst.PubKey[:]
	index := "/" + strconv.FormatUint(escrowPracticeIndex, 10)
	owner := key.Expression() + index + "/0/0"
	n := e.keyNet()
	pt.signDesc = "wsh(sortedmulti(2," + owner + "," + throwawayTprv(seed, n.prv) + index + "/0/0," + blank.Rooted(n.pub) + "/0/0))"
	public, err := e.descriptorInfo(ctx, "wsh(sortedmulti(2,"+owner+","+throwaway.Rooted(n.pub)+"/0/0,"+blank.Rooted(n.pub)+"/0/0))")
	if err != nil {
		return "", nil, err
	}
	signPublic, err := e.descriptorInfo(ctx, pt.signDesc)
	if err != nil {
		return "", nil, err
	}
	fromRooted, err := e.deriveAddresses(ctx, public, -1)
	if err != nil {
		return "", nil, err
	}
	fromPath, err := e.deriveAddresses(ctx, signPublic, -1)
	if err != nil {
		return "", nil, err
	}
	if len(fromRooted) != 1 || !slices.Equal(fromRooted, fromPath) {
		return "", nil, errPerOrderMismatch
	}
	pt.OwnPath, pt.OwnDeriv = "["+key.Fingerprint+key.PathText+index+"]", "m"+key.PathText+index
	pt.Cosigners = [2]string{throwaway.Rooted(n.msig), blank.Rooted(n.msig)}
	pt.Address = fromRooted[0]
	return public, server.PubKey[:], nil
}

// practiceKeys reads the practice input's witness script, 2 <k1> <k2> <k3> 3 CHECKMULTISIG with three distinct
// sorted compressed keys, and names them: the owner's (the derivation at fingerprint fp and ownerPath: the key's origin
// followed by escrowPracticeSuffix, or by the practice index and /0/0 for a per-order wallet key), the unsignable one
// and the throwaway key (the third).
func practiceKeys(in psbtInput, fp string, ownerPath []uint32, blank []byte) (owner, server []byte, err error) {
	ws := in.WitnessScript
	if len(ws) != 105 || ws[0] != 0x52 || ws[1] != 33 || ws[35] != 33 || ws[69] != 33 || ws[103] != 0x53 || ws[104] != 0xae {
		return nil, nil, errPracticeKeyShape
	}
	keys := [][]byte{ws[2:35], ws[36:69], ws[70:103]}
	if !slices.IsSortedFunc(keys, bytes.Compare) || bytes.Equal(keys[0], keys[1]) || bytes.Equal(keys[1], keys[2]) {
		return nil, nil, errPracticeKeyShape
	}
	for _, d := range in.Derivations {
		if hex.EncodeToString(d.Fingerprint[:]) == fp && slices.Equal(d.Path, ownerPath) {
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
