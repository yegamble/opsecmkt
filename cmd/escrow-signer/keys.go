package main

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"strconv"

	"opsecmkt/internal/btc"
)

// The signer's keys (A-454, ruling P4-R17). The seed is a fresh one of its own, never the Core arbiter wallet's. Its
// BIP32 master key is the root; the signer's account is m/48h/1h/0h/2h; the market key of the escrow with per-order
// index i is account/i/0/0, and its per-order wallet's siblings account/i/0/k and account/i/1/k (k < 20) may be signed
// for too (P4-R2: a top-up to a sibling address is refunded, not stranded).
//
// Go does as little private-key arithmetic as possible. To sign, the signer hands Bitcoin Core the master tprv with the
// full path, tprv…/48h/1h/0h/2h/i/c/k, inside the descriptor of one descriptorprocesspsbt call, and Core derives the
// key. Go derives the account's public key once at startup, because Core cannot: getdescriptorinfo of a private
// descriptor answers the master tpub with the hardened steps still to walk (wpkh(tpub…/48h/1h/0h/2h/*), probed on Core
// 31.1 regtest), which no public derivation can follow, and only a wallet's listdescriptors would show the account key
// (the key would then sit in a Core wallet file). That costs the master key from the seed (HMAC-SHA512), four hardened
// CKDpriv steps (HMAC-SHA512 and an addition mod n) and one scalar multiplication for the account public key, done with
// internal/btc's Mul, which is not constant-time: it runs once per process start on a key no request can choose, and
// the signer answers no timing-observable request with it, so no attacker can repeat or time it. A mismatch between
// Go's account key and Core's derivation cannot sign anything: the signer only names a key Go derived from that account
// key, and Core adds no signature for it, which the result check refuses.

const (
	tprvVersion = 0x04358394 // BIP32 test-network private key version
	seedLen     = 32         // init's default; a 64-byte seed is accepted too
)

// accountPath is m/48h/1h/0h/2h: BIP48, coin type 1 (test networks), account 0, script type 2 (native SegWit
// multisig), as the escrow's party accounts are (escrow_keys.go isBIP48Path).
var accountPath = []uint32{48, 1, 0, 2}

var (
	errSeedSize   = errors.New("the seed must be exactly 32 or 64 bytes")
	errSeedMaster = errors.New("this seed gives an invalid BIP32 master key (probability about 2^-127); make a new one with init")
)

// extPriv is a BIP32 extended private key: the key and the chain code (no metadata: the signer serializes only the
// master key, whose depth, parent fingerprint and child number are 0).
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

// public is the key's extended public key, rooted (depth 0, parent fingerprint 0, child 0).
func (p extPriv) public() btc.ExtKey {
	return btc.ExtKey{ChainCode: p.chain, PubKey: btc.Compress(btc.Mul(p.key, btc.G))}
}

// serialize is the key's BIP32 serialization as a root (depth 0, parent fingerprint 0, child 0) under version. Only
// the master key is serialized, and only into the descriptor handed to Core.
func (p extPriv) serialize(version uint32) string {
	raw := binary.BigEndian.AppendUint32(make([]byte, 0, 78), version)
	raw = append(raw, make([]byte, 9)...) // depth, parent fingerprint, child number
	raw = append(raw, p.chain[:]...)
	raw = append(raw, 0)
	raw = append(raw, p.key.FillBytes(make([]byte, 32))...)
	return btc.Base58CheckEncode(raw)
}

// signerKeys is what the signer keeps in memory for its lifetime: the master tprv (only ever placed in a descriptor
// sent to Core over its RPC) and the account's public key.
type signerKeys struct {
	masterTprv string
	account    btc.ExtKey // m/48h/1h/0h/2h, rooted
}

// keysFromSeed derives the signer's keys from a 32- or 64-byte seed. Errors name no key material.
func keysFromSeed(seed []byte) (*signerKeys, error) {
	if len(seed) != 32 && len(seed) != 64 {
		return nil, errSeedSize
	}
	m, err := masterKey(seed)
	if err != nil {
		return nil, err
	}
	acct := m
	for _, step := range accountPath {
		if acct, err = hardenedChild(acct, step); err != nil {
			return nil, err
		}
	}
	return &signerKeys{masterTprv: m.serialize(tprvVersion), account: acct.public()}, nil
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

// descriptorKey is the key expression of account/index/chain/i that Core derives from: the master tprv and the full
// path. It goes into exactly one place, the descriptor of a descriptorprocesspsbt call.
func (k *signerKeys) descriptorKey(index, chain, i uint32) string {
	return k.masterTprv + "/48h/1h/0h/2h/" + itoa(index) + "/" + itoa(chain) + "/" + itoa(i)
}

func itoa(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
