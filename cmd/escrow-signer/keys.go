package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"

	"opsecmkt/internal/btc"
)

// The signer's keys (A-454, ruling P4-R17). The seed is a fresh one of its own, never the Core arbiter wallet's. Its
// BIP32 master key is the root; the signer's account is m/48h/1h/0h/2h; the market key of the escrow with per-order
// index i is account/i/0/0, and its per-order wallet's siblings account/i/0/k and account/i/1/k (k < 20) may be signed
// for too (P4-R2: a top-up to a sibling address is refunded, not stranded).
//
// Go does no elliptic-curve arithmetic with a private key (QA4W2-1): internal/btc's point multiplication is math/big
// code and not constant-time. From the seed Go computes the master key (HMAC-SHA512) and the four hardened CKDpriv
// steps to the account (HMAC-SHA512 and an addition mod n), and serializes the account's private key as a root (depth
// 0, no parent): the account tprv. Bitcoin Core does the rest. At startup getdescriptorinfo of wpkh(<account
// tprv>/0/*) answers the descriptor with the account's tpub in its place (checked on Core 31.1 regtest), which is
// parsed and checked against Go's chain code; every per-order public key is derived from that tpub in Go (CKDpub, on
// public data only). To sign, the account tprv and the per-order path, tprv…/i/c/k, go into the descriptor of one
// descriptorprocesspsbt call and Core derives the key (the same key as master/48h/1h/0h/2h/i/c/k: a root's metadata
// takes no part in derivation). The master key is never serialized and never leaves the process. A mismatch between
// Core's account key and Go's derivation cannot sign anything: the signer only names a key derived from that account
// key, and Core adds no signature for any other, which the result check refuses.

const (
	tprvVersion = 0x04358394 // BIP32 test-network private key version
	seedLen     = 32         // init's default; a 64-byte seed is accepted too
)

// accountPath is m/48h/1h/0h/2h: BIP48, coin type 1 (test networks), account 0, script type 2 (native SegWit
// multisig), as the escrow's party accounts are (escrow_keys.go isBIP48Path).
var accountPath = []uint32{48, 1, 0, 2}

var (
	errSeedSize    = errors.New("the seed must be exactly 32 or 64 bytes")
	errSeedMaster  = errors.New("this seed gives an invalid BIP32 master key (probability about 2^-127); make a new one with init")
	errAccountCore = errors.New("the escrow node's getdescriptorinfo did not answer this seed's account key")
)

// extPriv is a BIP32 extended private key: the key and the chain code (no metadata: the signer serializes only the
// account key, as a root whose depth, parent fingerprint and child number are 0).
type extPriv struct {
	key   *big.Int
	chain [32]byte
}

func hmac512(key, data []byte) []byte {
	m := hmac.New(sha512.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// masterKey is BIP32's master key generation: HMAC-SHA512 keyed "Bitcoin seed" of the seed; IL is the key (refused
// when 0 or >= n), IR the chain code.
func masterKey(seed []byte) (extPriv, error) {
	sum := hmac512([]byte("Bitcoin seed"), seed)
	k := new(big.Int).SetBytes(sum[:32])
	if k.Sign() == 0 || k.Cmp(btc.N) >= 0 {
		return extPriv{}, errSeedMaster
	}
	m := extPriv{key: k}
	copy(m.chain[:], sum[32:])
	return m, nil
}

// hardenedChild is BIP32's CKDpriv for the hardened index 2^31+i: I = HMAC-SHA512(c, 0x00 || ser256(k) || ser32(i)),
// child key IL + k mod n. An invalid child (IL >= n or a zero key), which BIP32 skips to the next index, is refused
// instead: the account path is fixed (probability about 2^-127 per step).
func hardenedChild(p extPriv, i uint32) (extPriv, error) {
	data := make([]byte, 37)
	p.key.FillBytes(data[1:33])
	binary.BigEndian.PutUint32(data[33:], i|btc.Hardened)
	sum := hmac512(p.chain[:], data)
	il := new(big.Int).SetBytes(sum[:32])
	if il.Cmp(btc.N) >= 0 {
		return extPriv{}, errSeedMaster
	}
	k := il.Add(il, p.key)
	k.Mod(k, btc.N)
	if k.Sign() == 0 {
		return extPriv{}, errSeedMaster
	}
	c := extPriv{key: k}
	copy(c.chain[:], sum[32:])
	return c, nil
}

func (p extPriv) hardenedChild(i uint32) (extPriv, error) { return hardenedChild(p, i) }

// serialize is the key's BIP32 serialization as a root (depth 0, parent fingerprint 0, child number 0) under version.
// Only the account key is serialized, and only into the descriptors handed to Core.
func (p extPriv) serialize(version uint32) string {
	raw := binary.BigEndian.AppendUint32(make([]byte, 0, 78), version)
	raw = append(raw, make([]byte, 9)...) // depth, parent fingerprint, child number
	raw = append(raw, p.chain[:]...)
	raw = append(raw, 0)
	raw = append(raw, p.key.FillBytes(make([]byte, 32))...)
	return btc.Base58CheckEncode(raw)
}

// signerKeys is what the signer keeps in memory for its lifetime: the account tprv (only ever placed in a descriptor
// sent to Core over its RPC), its chain code, and the account's public key as Core gave it (loadAccount).
type signerKeys struct {
	accountTprv string     // m/48h/1h/0h/2h, rooted
	chain       [32]byte   // the account's chain code, to check Core's answer
	account     btc.ExtKey // m/48h/1h/0h/2h, rooted; zero until loadAccount
}

// keysFromSeed derives the signer's account tprv from a 32- or 64-byte seed; loadAccount then asks Core for its public
// key. Errors name no key material.
func keysFromSeed(seed []byte) (*signerKeys, error) {
	if len(seed) != 32 && len(seed) != 64 {
		return nil, errSeedSize
	}
	acct, err := masterKey(seed)
	if err != nil {
		return nil, err
	}
	for _, step := range accountPath {
		if acct, err = hardenedChild(acct, step); err != nil {
			return nil, err
		}
	}
	return &signerKeys{accountTprv: acct.serialize(tprvVersion), chain: acct.chain}, nil
}

// coreTpubDescriptor is getdescriptorinfo's descriptor for wpkh(<account tprv>/0/*): the same with the tpub, and its
// checksum.
var coreTpubDescriptor = regexp.MustCompile(`^wpkh\((tpub[1-9A-HJ-NP-Za-km-z]+)/0/\*\)#[0-9a-z]{8}$`)

// loadAccount has Core compute the account's public key: getdescriptorinfo of wpkh(<account tprv>/0/*) answers the
// descriptor with the account tpub in the tprv's place. The tpub must parse as a rooted tpub (a valid point) with Go's
// chain code. The error names no key material.
func (k *signerKeys) loadAccount(ctx context.Context, c rpcCaller) error {
	var res struct {
		Descriptor string `json:"descriptor"`
	}
	if err := c.call(ctx, "getdescriptorinfo", []any{"wpkh(" + k.accountTprv + "/0/*)"}, &res); err != nil {
		return fmt.Errorf("getdescriptorinfo: %w (the node must be running and the signer's RPC user whitelisted to decodepsbt, descriptorprocesspsbt and getdescriptorinfo)", err)
	}
	m := coreTpubDescriptor.FindStringSubmatch(res.Descriptor)
	if m == nil {
		return errAccountCore
	}
	pub, err := btc.ParseExtPub(m[1], btc.VersionTpub)
	if err != nil || pub.Depth != 0 || pub.ChainCode != k.chain {
		return errAccountCore
	}
	k.account = pub
	return nil
}

// accountTpub is the account key rooted, as a tpub: what info answers and the app pins (by HMAC).
func (k *signerKeys) accountTpub() string { return k.account.Rooted(btc.VersionTpub) }

// fingerprint is the account key's own fingerprint (HASH160 of its public key, 4 bytes, hex): the origin a descriptor
// names for keys derived from the rooted account key.
func (k *signerKeys) fingerprint() string {
	fp := k.account.Fingerprint()
	return hex.EncodeToString(fp[:])
}

var errOrderKey = errors.New("no such per-order key")

// orderKey is the public key account/index/chain/i, derived in Go from the account key (internal/btc CKDPub). A step
// BIP32 would skip (an invalid child, about 2^-127) is refused: Core derives the path as written, so the two would
// disagree.
func (k *signerKeys) orderKey(index, chain, i uint32) ([33]byte, error) {
	key := k.account
	for _, step := range []uint32{index, chain, i} {
		next, used, err := btc.CKDPub(key, step)
		if err != nil || used != step {
			return [33]byte{}, errOrderKey
		}
		key = next
	}
	return key.PubKey, nil
}

// siblingKeys is every key account/index/chain/k for chain 0 and 1 and k < n, each with its chain and k (orderKey's
// keys, deriving the index and chain levels once).
func (k *signerKeys) siblingKeys(index, n uint32) map[[33]byte][2]uint32 {
	out := map[[33]byte][2]uint32{}
	order, used, err := btc.CKDPub(k.account, index)
	if err != nil || used != index {
		return out
	}
	for chain := uint32(0); chain < 2; chain++ {
		ck, used, err := btc.CKDPub(order, chain)
		if err != nil || used != chain {
			continue
		}
		for i := uint32(0); i < n; i++ {
			if leaf, used, err := btc.CKDPub(ck, i); err == nil && used == i {
				out[leaf.PubKey] = [2]uint32{chain, i}
			}
		}
	}
	return out
}

// descriptorKey is the key expression of account/index/chain/i that Core derives from: the account tprv and the
// per-order path. It goes into exactly one place, the descriptor of a descriptorprocesspsbt call.
func (k *signerKeys) descriptorKey(index, chain, i uint32) string {
	return k.accountTprv + "/" + itoa(index) + "/" + itoa(chain) + "/" + itoa(i)
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
