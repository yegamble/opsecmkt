package market

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Escrow descriptors and child keys (WR-escrow ES-3, Phase 1 slice P1-b), checked in Go with the standard library
// only: the BIP 380 descriptor checksum, the party child path, and the witness script Bitcoin Core fills in for an
// escrow (escrow_create.go). Core derives every child key; nothing here does elliptic-curve arithmetic.

const (
	descInputCharset    = "0123456789()[],'/*abcdefgh@:$%{}IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
	descChecksumCharset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

var descGenerator = [5]uint64{0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d, 0x3706b1677a, 0x644d626ffd}

func descPolymod(c uint64, v int) uint64 {
	top := c >> 35
	c = (c&0x7ffffffff)<<5 ^ uint64(v)
	for i, g := range descGenerator {
		if top>>i&1 != 0 {
			c ^= g
		}
	}
	return c
}

// descChecksum is the 8-character BIP 380 checksum of desc (Bitcoin Core's "#…" suffix); false for a character
// outside the descriptor alphabet. Bitcoin Core's getdescriptorinfo must agree with it for every escrow built
// (buildEscrow), so a wrong checksum is never shown.
func descChecksum(desc string) (string, bool) {
	c := uint64(1)
	cls, clsCount := 0, 0
	for i := 0; i < len(desc); i++ {
		pos := strings.IndexByte(descInputCharset, desc[i])
		if pos < 0 {
			return "", false
		}
		c = descPolymod(c, pos&31)
		cls = cls*3 + pos>>5
		if clsCount++; clsCount == 3 {
			c = descPolymod(c, cls)
			cls, clsCount = 0, 0
		}
	}
	if clsCount > 0 {
		c = descPolymod(c, cls)
	}
	for range 8 {
		c = descPolymod(c, 0)
	}
	c ^= 1
	out := make([]byte, 8)
	for i := range out {
		out[i] = descChecksumCharset[c>>(5*(7-i))&31]
	}
	return string(out), true
}

// withChecksum is desc followed by "#" and its checksum; desc must be in the descriptor alphabet (every descriptor
// this package composes is: hex keys, key expressions parseEscrowKey accepted, addresses checked by escrowAddressOK).
func withChecksum(desc string) string {
	sum, _ := descChecksum(desc)
	return desc + "#" + sum
}

// escrowChildSuffix is a party key's derivation suffix for its index-th escrow: /0/<index>, index from 1 (/0/0 is
// the practice child, ES-9).
func escrowChildSuffix(index int64) string { return "/0/" + strconv.FormatInt(index, 10) }

var errEscrowOrigin = errors.New("escrow: party key origin or derivation suffix out of rule")

// checkEscrowOrigin enforces the chair's ruling (b) on a party key of an escrow before it is used or stored:
// escrow_pubkeys.origin_path is exactly the registered account origin (key.PathText, canonical) and
// derivation_suffix is the child path below it, /0/<i> with 1 <= i < 2^31 (unhardened: a public key derives it). So
// no split of origin and suffix can name the account's /0/0 practice child (ES-9), whatever the two strings are.
func checkEscrowOrigin(key escrowKey, origin, suffix string) error {
	if origin != key.PathText {
		return errEscrowOrigin
	}
	steps, err := parseOriginPath(suffix)
	if err != nil || len(steps) != 2 || steps[0] != 0 || steps[1]&(1<<31) != 0 || formatOriginPath(steps) != suffix {
		return errEscrowOrigin
	}
	if slices.Equal(slices.Concat(key.Path, steps), slices.Concat(key.Path, []uint32{0, 0})) { // the practice child
		return errEscrowOrigin
	}
	return nil
}

// escrowExpectKey is one key an escrow's witness script must hold: whose it is and its full derivation (the
// origin's fingerprint, and the origin path followed by the derivation suffix).
type escrowExpectKey struct {
	Party       string // buyer, vendor, market
	Fingerprint string
	Path        []uint32
}

// The witness-script refusals, one per guard, each a fixed class for the log (never a key).
var (
	errEscrowScriptShape  = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-not-2-of-3"}
	errEscrowScriptKeys   = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-keys-not-distinct"}
	errEscrowScriptPays   = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-not-paid"}
	errEscrowScriptDerivs = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-derivations"}
	errEscrowArbiterCount = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-arbiter-keys"}
	errEscrowPartyKey     = &escrowRPCError{Method: "utxoupdatepsbt", Class: "script-party-key"}
)

// checkEscrowWitness checks the escrow input Bitcoin Core filled in (utxoupdatepsbt with the escrow's public
// descriptor) before any address is shown (ES-3): the witness script is 2 <k1> <k2> <k3> 3 CHECKMULTISIG over three
// distinct compressed keys on the curve, in sortedmulti order; spk pays exactly that script (P2WSH); there is one
// derivation per key and no other; exactly one derivation has the pinned arbiter fingerprint; and every expected
// key sits at its expected fingerprint and path. It returns each party's key.
func checkEscrowWitness(in psbtInput, spk []byte, arbiterFP string, want []escrowExpectKey) (map[string][]byte, error) {
	ws := in.WitnessScript
	if len(ws) != 105 || ws[0] != 0x52 || ws[1] != 33 || ws[35] != 33 || ws[69] != 33 || ws[103] != 0x53 || ws[104] != 0xae {
		return nil, errEscrowScriptShape
	}
	keys := [][]byte{ws[2:35], ws[36:69], ws[70:103]}
	for _, k := range keys {
		if !onCurve(k) {
			return nil, errEscrowScriptShape
		}
	}
	if bytes.Compare(keys[0], keys[1]) >= 0 || bytes.Compare(keys[1], keys[2]) >= 0 { // sorted and distinct
		return nil, errEscrowScriptKeys
	}
	sum := sha256.Sum256(ws)
	if !bytes.Equal(spk, append([]byte{0, 32}, sum[:]...)) {
		return nil, errEscrowScriptPays
	}
	if len(in.Derivations) != len(keys) || len(want) != len(keys) {
		return nil, errEscrowScriptDerivs
	}
	arbiter := 0
	got := map[string][]byte{}
	named := map[string]bool{}
	for _, d := range in.Derivations {
		if !slices.ContainsFunc(keys, func(k []byte) bool { return bytes.Equal(k, d.PubKey) }) || named[string(d.PubKey)] {
			return nil, errEscrowScriptDerivs
		}
		named[string(d.PubKey)] = true
		fp := hex.EncodeToString(d.Fingerprint[:])
		if fp == arbiterFP {
			arbiter++
		}
		for _, w := range want {
			if fp == w.Fingerprint && slices.Equal(d.Path, w.Path) {
				if got[w.Party] != nil {
					return nil, errEscrowPartyKey
				}
				got[w.Party] = d.PubKey
			}
		}
	}
	if arbiter != 1 {
		return nil, errEscrowArbiterCount
	}
	for _, w := range want {
		if got[w.Party] == nil || (w.Party == "market") != (w.Fingerprint == arbiterFP) {
			return nil, errEscrowPartyKey
		}
	}
	return got, nil
}
