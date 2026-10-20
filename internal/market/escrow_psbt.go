package market

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
)

// A strict BIP 174 (PSBT version 0) reader for pasted escrow PSBTs (WR-escrow ES-2, ES-9). It reads the unsigned
// transaction and, per input, the partial signatures (public key -> signature ending in its sighash byte), the
// sighash type field, the BIP32 derivations, the witness UTXO and whether the input is already finalized. Unknown,
// global and proprietary fields are ignored (an honest wallet adds them; D-45), but every map is checked for
// duplicate keys and every length against what remains, so no allocation exceeds the input. Signatures are not
// verified here: Bitcoin Core does that (finalizepsbt).

const (
	psbtMaxBytes  = 64 << 10 // a form post is at most 64 KB (app.go), so a larger PSBT could never be pasted
	psbtMaxBase64 = (psbtMaxBytes + 2) / 3 * 4
	maxSatoshis   = 21_000_000 * 100_000_000
)

// psbtError is a PSBT that could not be read. Its text is fixed and safe to show; it never quotes the input.
type psbtError struct{ msg string }

func (e *psbtError) Error() string { return "The PSBT could not be read: " + e.msg + "." }

func psbtErr(msg string) error { return &psbtError{msg} }

type psbtTxIn struct {
	TxID     [32]byte // internal byte order (as serialized)
	Vout     uint32
	Sequence uint32
}

type psbtTxOut struct {
	Value  int64
	Script []byte
}

type psbtTx struct {
	Version  int32
	In       []psbtTxIn
	Out      []psbtTxOut
	LockTime uint32
}

type psbtSig struct {
	PubKey []byte // 33 (or 65) bytes
	Sig    []byte // DER signature followed by its sighash byte
}

// SighashByte is the sighash type the signature commits to (its last byte).
func (s psbtSig) SighashByte() byte { return s.Sig[len(s.Sig)-1] }

type psbtDerivation struct {
	PubKey      []byte
	Fingerprint [4]byte
	Path        []uint32
}

type psbtInput struct {
	NonWitnessUTXO []byte // funding transaction, present on a hardware copy only
	PartialSigs    []psbtSig
	SighashType    uint32
	HasSighashType bool
	Derivations    []psbtDerivation
	WitnessUTXO    *psbtTxOut
	WitnessScript  []byte
	Finalized      bool // a final scriptSig or scriptWitness is present
}

type psbtPacket struct {
	Raw        []byte
	UnsignedTx []byte
	Tx         psbtTx
	Inputs     []psbtInput
	inputEnds  []int // offset in Raw of each input map's terminating 0x00
}

// TxID is the unsigned transaction's id, as Bitcoin Core prints it (byte-reversed double SHA-256).
func (p *psbtPacket) TxID() string {
	h := doubleSHA256(p.UnsignedTx)
	slices.Reverse(h[:])
	return hex.EncodeToString(h[:])
}

// decodePSBT decodes a pasted base64 PSBT (surrounding whitespace and line breaks allowed) and parses it.
func decodePSBT(s string) (*psbtPacket, error) {
	return decodePSBTLimit(s, psbtMaxBytes)
}

func decodeHardwarePSBT(s string) (*psbtPacket, error) {
	return decodePSBTLimit(s, hardwareMaxBytes)
}

func decodePSBTLimit(s string, limit int) (*psbtPacket, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return nil, psbtErr("nothing was pasted")
	}
	if len(s) > (limit+2)/3*4 {
		return nil, psbtErr("it is too large")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, psbtErr("it is not base64 text")
	}
	return parsePSBTLimit(raw, limit)
}

func encodePSBT(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// psbtReader reads compact sizes and byte strings from b, never past its end.
type psbtReader struct {
	b   []byte
	off int
}

func (r *psbtReader) left() int { return len(r.b) - r.off }

func (r *psbtReader) compactSize() (int, error) {
	if r.left() < 1 {
		return 0, psbtErr("it ends too early")
	}
	first := r.b[r.off]
	r.off++
	var n uint64
	var size int
	switch first {
	case 0xfd:
		size = 2
	case 0xfe:
		size = 4
	case 0xff:
		size = 8
	default:
		return int(first), nil
	}
	if r.left() < size {
		return 0, psbtErr("it ends too early")
	}
	for i := size - 1; i >= 0; i-- {
		n = n<<8 | uint64(r.b[r.off+i])
	}
	r.off += size
	if size == 2 && n < 0xfd || size == 4 && n <= 0xffff || size == 8 && n <= 0xffffffff {
		return 0, psbtErr("a length is not minimally encoded")
	}
	if n > uint64(r.left()) {
		return 0, psbtErr("a length runs past its end")
	}
	return int(n), nil
}

func (r *psbtReader) bytes(n int) ([]byte, error) {
	if n < 0 || n > r.left() {
		return nil, psbtErr("it ends too early")
	}
	out := r.b[r.off : r.off+n : r.off+n]
	r.off += n
	return out, nil
}

// readMap reads one key-value map up to its 0x00 terminator, refusing an empty key or a duplicate key. It returns
// the entries and the offset of the terminator.
func (r *psbtReader) readMap() (entries [][2][]byte, end int, err error) {
	seen := map[string]bool{}
	for {
		kl, err := r.compactSize()
		if err != nil {
			return nil, 0, err
		}
		if kl == 0 {
			return entries, r.off - 1, nil
		}
		key, err := r.bytes(kl)
		if err != nil {
			return nil, 0, err
		}
		if seen[string(key)] {
			return nil, 0, psbtErr("a field appears twice")
		}
		seen[string(key)] = true
		vl, err := r.compactSize()
		if err != nil {
			return nil, 0, err
		}
		val, err := r.bytes(vl)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, [2][]byte{key, val})
	}
}

// parsePSBT parses raw BIP 174 version 0 bytes (see the file comment).
func parsePSBT(raw []byte) (*psbtPacket, error) {
	return parsePSBTLimit(raw, psbtMaxBytes)
}

func parsePSBTLimit(raw []byte, limit int) (*psbtPacket, error) {
	if len(raw) > limit {
		return nil, psbtErr("it is too large")
	}
	if !bytes.HasPrefix(raw, []byte("psbt\xff")) {
		return nil, psbtErr("it does not start like a PSBT")
	}
	r := &psbtReader{b: raw, off: 5}
	global, _, err := r.readMap()
	if err != nil {
		return nil, err
	}
	p := &psbtPacket{Raw: raw}
	for _, e := range global {
		key, val := e[0], e[1]
		switch key[0] {
		case 0x00:
			if len(key) != 1 {
				return nil, psbtErr("its unsigned transaction field is malformed")
			}
			p.UnsignedTx = val
		case 0xfb:
			if len(key) != 1 || len(val) != 4 {
				return nil, psbtErr("its version field is malformed")
			}
			if binary.LittleEndian.Uint32(val) != 0 {
				return nil, psbtErr("only PSBT version 0 is supported")
			}
		}
	}
	if p.UnsignedTx == nil {
		return nil, psbtErr("it has no unsigned transaction")
	}
	if p.Tx, err = parseUnsignedTx(p.UnsignedTx); err != nil {
		return nil, err
	}
	for range p.Tx.In {
		entries, end, err := r.readMap()
		if err != nil {
			return nil, err
		}
		in, err := parsePSBTInput(entries)
		if err != nil {
			return nil, err
		}
		p.Inputs = append(p.Inputs, in)
		p.inputEnds = append(p.inputEnds, end)
	}
	for range p.Tx.Out {
		if _, _, err := r.readMap(); err != nil {
			return nil, err
		}
	}
	if r.left() != 0 {
		return nil, psbtErr("it has data after its last field")
	}
	return p, nil
}

func parsePSBTInput(entries [][2][]byte) (psbtInput, error) {
	var in psbtInput
	for _, e := range entries {
		key, val := e[0], e[1]
		switch key[0] {
		case 0x00: // non-witness UTXO; validated against the outpoint when a hardware copy is made
			if len(key) != 1 {
				return in, psbtErr("a non-witness UTXO field is malformed")
			}
			in.NonWitnessUTXO = val
		case 0x01: // witness UTXO
			if len(key) != 1 {
				return in, psbtErr("a witness UTXO field is malformed")
			}
			r := &psbtReader{b: val}
			out, err := readTxOut(r)
			if err != nil || r.left() != 0 {
				return in, psbtErr("a witness UTXO field is malformed")
			}
			in.WitnessUTXO = &out
		case 0x02: // partial signature
			pub := key[1:]
			if !validPubKey(pub) {
				return in, psbtErr("a signature's public key is malformed")
			}
			if len(val) < 2 || !strictDER(val[:len(val)-1]) {
				return in, psbtErr("a signature is malformed")
			}
			in.PartialSigs = append(in.PartialSigs, psbtSig{PubKey: pub, Sig: val})
		case 0x03: // sighash type
			if len(key) != 1 || len(val) != 4 {
				return in, psbtErr("a sighash type field is malformed")
			}
			in.SighashType, in.HasSighashType = binary.LittleEndian.Uint32(val), true
		case 0x05: // witness script
			if len(key) != 1 {
				return in, psbtErr("a witness script field is malformed")
			}
			in.WitnessScript = val
		case 0x06: // BIP32 derivation
			pub := key[1:]
			if !validPubKey(pub) || len(val) < 4 || len(val)%4 != 0 || len(val) > 4+4*255 {
				return in, psbtErr("a key derivation field is malformed")
			}
			d := psbtDerivation{PubKey: pub, Path: make([]uint32, 0, (len(val)-4)/4)}
			copy(d.Fingerprint[:], val[:4])
			for i := 4; i < len(val); i += 4 {
				d.Path = append(d.Path, binary.LittleEndian.Uint32(val[i:]))
			}
			in.Derivations = append(in.Derivations, d)
		case 0x07, 0x08: // final scriptSig, final scriptWitness
			if len(key) != 1 {
				return in, psbtErr("a final script field is malformed")
			}
			in.Finalized = true
		}
	}
	return in, nil
}

// validPubKey accepts a 33-byte compressed or 65-byte uncompressed SEC public key by its length and prefix.
func validPubKey(b []byte) bool {
	return len(b) == 33 && (b[0] == 2 || b[0] == 3) || len(b) == 65 && b[0] == 4
}

// strictDER is BIP 66's shape check of a DER signature (without its sighash byte).
func strictDER(sig []byte) bool {
	if len(sig) < 8 || len(sig) > 72 || sig[0] != 0x30 || int(sig[1]) != len(sig)-2 {
		return false
	}
	lenR := int(sig[3])
	if sig[2] != 0x02 || lenR == 0 || 5+lenR >= len(sig) {
		return false
	}
	lenS := int(sig[5+lenR])
	if lenR+lenS+6 != len(sig) || sig[4+lenR] != 0x02 || lenS == 0 {
		return false
	}
	r, s := sig[4:4+lenR], sig[6+lenR:]
	return r[0]&0x80 == 0 && !(lenR > 1 && r[0] == 0 && r[1]&0x80 == 0) &&
		s[0]&0x80 == 0 && !(lenS > 1 && s[0] == 0 && s[1]&0x80 == 0)
}

// parseUnsignedTx reads the PSBT's transaction: version, at least one input with an empty scriptSig, at least one
// output, lock time, in the serialization without witnesses, and nothing after it.
func parseUnsignedTx(b []byte) (psbtTx, error) {
	r := &psbtReader{b: b}
	var tx psbtTx
	head, err := r.bytes(4)
	if err != nil {
		return tx, psbtErr("its transaction is malformed")
	}
	tx.Version = int32(binary.LittleEndian.Uint32(head))
	nIn, err := r.compactSize()
	if err != nil || nIn == 0 || nIn > r.left()/41 {
		return tx, psbtErr("its transaction is malformed or has no inputs")
	}
	tx.In = make([]psbtTxIn, 0, nIn)
	for range nIn {
		b, err := r.bytes(36)
		if err != nil {
			return tx, psbtErr("its transaction is malformed")
		}
		var in psbtTxIn
		copy(in.TxID[:], b[:32])
		in.Vout = binary.LittleEndian.Uint32(b[32:])
		if n, err := r.compactSize(); err != nil || n != 0 {
			return tx, psbtErr("its transaction is malformed or already signed")
		}
		seq, err := r.bytes(4)
		if err != nil {
			return tx, psbtErr("its transaction is malformed")
		}
		in.Sequence = binary.LittleEndian.Uint32(seq)
		tx.In = append(tx.In, in)
	}
	nOut, err := r.compactSize()
	if err != nil || nOut == 0 || nOut > r.left()/9 {
		return tx, psbtErr("its transaction is malformed or has no outputs")
	}
	tx.Out = make([]psbtTxOut, 0, nOut)
	for range nOut {
		out, err := readTxOut(r)
		if err != nil {
			return tx, err
		}
		tx.Out = append(tx.Out, out)
	}
	lock, err := r.bytes(4)
	if err != nil || r.left() != 0 {
		return tx, psbtErr("its transaction is malformed")
	}
	tx.LockTime = binary.LittleEndian.Uint32(lock)
	return tx, nil
}

func readTxOut(r *psbtReader) (psbtTxOut, error) {
	v, err := r.bytes(8)
	if err != nil {
		return psbtTxOut{}, psbtErr("an output is malformed")
	}
	value := int64(binary.LittleEndian.Uint64(v))
	if value < 0 || value > maxSatoshis {
		return psbtTxOut{}, psbtErr("an output amount is out of range")
	}
	n, err := r.compactSize()
	if err != nil {
		return psbtTxOut{}, psbtErr("an output is malformed")
	}
	script, err := r.bytes(n)
	if err != nil {
		return psbtTxOut{}, psbtErr("an output is malformed")
	}
	return psbtTxOut{Value: value, Script: script}, nil
}

// withInputField returns a copy of the PSBT with one field added to input i's map. It refuses a key the map
// already has, so a field is never replaced.
func (p *psbtPacket) withInputField(i int, key, value []byte) ([]byte, error) {
	return p.withInputFieldLimit(i, key, value, psbtMaxBytes)
}

func (p *psbtPacket) withInputFieldLimit(i int, key, value []byte, limit int) ([]byte, error) {
	if i < 0 || i >= len(p.inputEnds) || len(key) == 0 {
		return nil, errors.New("psbt: no such input")
	}
	start := 0
	if i > 0 {
		start = p.inputEnds[i-1] + 1
	} else {
		// The first input map starts after the global map: re-read it to find the offset.
		r := &psbtReader{b: p.Raw, off: 5}
		if _, end, err := r.readMap(); err != nil {
			return nil, err
		} else {
			start = end + 1
		}
	}
	r := &psbtReader{b: p.Raw[:p.inputEnds[i]+1], off: start}
	entries, _, err := r.readMap()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if bytes.Equal(e[0], key) {
			return nil, errors.New("psbt: the field is already present")
		}
	}
	end := p.inputEnds[i]
	var field []byte
	field = append(field, compactSize(len(key))...)
	field = append(field, key...)
	field = append(field, compactSize(len(value))...)
	field = append(field, value...)
	out := make([]byte, 0, len(p.Raw)+len(field))
	out = append(out, p.Raw[:end]...)
	out = append(out, field...)
	out = append(out, p.Raw[end:]...)
	if len(out) > limit {
		return nil, errors.New("psbt: too large")
	}
	return out, nil
}

func compactSize(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return binary.LittleEndian.AppendUint16([]byte{0xfd}, uint16(n))
	case n <= 0xffffffff:
		return binary.LittleEndian.AppendUint32([]byte{0xfe}, uint32(n))
	}
	return binary.LittleEndian.AppendUint64([]byte{0xff}, uint64(n))
}
