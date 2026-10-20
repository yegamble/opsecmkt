package market

import (
	"context"
	"errors"
	"strings"

	"opsecmkt/internal/btc"
)

// The Bitcoin escrow market fee paid to a watch-only descriptor (A-460; ES-5, owner decision 2026-09-29). The fee
// destination (ESCROW_BTC_FEE_ADDRESS, or the administrator's setting) may be a plain address or the fee wallet's
// receiving descriptor, wpkh([fingerprint/path]xpub/0/*) with or without its #checksum, or its key expression alone. On a
// mainnet market it must be the descriptor, so no two settlements pay one fee address; a test-network market may use a
// fixed address. The descriptor is parsed strictly in Go (parseEscrowKey, the market's key class) and stored sealed
// (escrowFeeDescLabel): its key would link every fee address. Checkout snapshots it (escrows.fee_descriptor). The fee
// index is taken from the escrow_fee_index sequence once per escrow, at its first release build, and reused on every
// rebuild, never at checkout; the address /0/<index> is derived in Go, Bitcoin Core must derive the same one, and it is
// stored in fee_destination (migration 109). scripts/restore.sh moves the sequence past the restored records.

const (
	escrowFeeDescPrefix = "desc:" // a fee destination that is a sealed descriptor key, not an address
	escrowFeeDescLabel  = "escrow-fee-descriptor"
	escrowFeeDescTries  = 5 // fee indexes tried when a derived address was already used on this market
)

var (
	errFeeDescriptor     = errors.New("the fee descriptor must be the fee wallet's receiving descriptor, wpkh([fingerprint/path]key/0/*) with or without its #checksum, or its key expression [fingerprint/path]key, with a public key of this market's network")
	errFeePlainOnMainnet = errors.New("on a mainnet market the market fee must go to a watch-only descriptor, so each settlement pays a fresh fee address: give the fee wallet's receiving descriptor, wpkh([fingerprint/path]xpub/0/*)")
	errFeeDescUnusable   = errors.New("escrow: the fee descriptor gives no usable fee address")
)

// isFeeDescriptorInput reports whether s is offered as a descriptor rather than an address.
func isFeeDescriptorInput(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "wpkh(") || strings.HasPrefix(s, "[")
}

// isSealedFeeDescriptor reports a stored fee destination that is a sealed descriptor key.
func isSealedFeeDescriptor(dest string) bool { return strings.HasPrefix(dest, escrowFeeDescPrefix) }

// parseFeeDescriptor reads a fee descriptor: wpkh(<key>/0/*) with an optional #checksum, which must be right, or the key
// expression alone (with or without /0/*). The key is parsed as an escrow key of class n.
func parseFeeDescriptor(s string, n keyNet) (escrowKey, error) {
	s = strings.TrimSpace(s)
	if desc, sum, ok := strings.Cut(s, "#"); ok {
		if withChecksum(desc) != s || sum == "" {
			return escrowKey{}, errFeeDescriptor
		}
		s = desc
	}
	if inner, ok := strings.CutPrefix(s, "wpkh("); ok {
		inner, ok = strings.CutSuffix(inner, "/0/*)")
		if !ok {
			return escrowKey{}, errFeeDescriptor
		}
		s = inner
	}
	k, err := parseEscrowKey(s, n)
	if err != nil {
		return escrowKey{}, errFeeDescriptor
	}
	return k, nil
}

// feeDescriptorAddress is the P2WPKH address of k's receiving child /0/<index> on chain, derived in Go.
func feeDescriptorAddress(k escrowKey, chain string, index uint32) (string, error) {
	hrp := escrowChains[chain]
	if hrp == "" {
		return "", errFeeDescUnusable
	}
	acct := btc.ExtKey{Depth: byte(len(k.Path)), ChainCode: k.ChainCode, PubKey: k.PubKey}
	branch, used, err := btc.CKDPub(acct, 0)
	if err != nil || used != 0 {
		return "", errFeeDescUnusable
	}
	child, used, err := btc.CKDPub(branch, index)
	if err != nil || used != index {
		return "", errFeeDescUnusable
	}
	h := btc.Hash160(child.PubKey[:])
	return segwitAddress(hrp, 0, h[:]), nil
}

// feeDestinationForm checks a fee destination (the startup setting or the administrator's) for a market of class main
// on chain: a descriptor is parsed and sealed (escrowFeeDescPrefix); a plain address is refused on a mainnet market and
// otherwise lowered by the chain (escrowAddressForm) and returned for the node to check. The key may not be the arbiter's.
func (a *App) feeDestinationForm(chain, in, arbiterFP string) (dest string, plain bool, err error) {
	if !isFeeDescriptorInput(in) {
		if a.live {
			return "", false, errFeePlainOnMainnet
		}
		addr, ok := escrowAddressForm(chain, in)
		if !ok {
			return "", false, errEscrowFeeAddress
		}
		return addr, true, nil
	}
	k, err := parseFeeDescriptor(in, a.keyNet())
	if err != nil {
		return "", false, err
	}
	if arbiterFP != "" && k.Fingerprint == arbiterFP {
		return "", false, errFeeDescriptor
	}
	sealed, err := a.seal(escrowFeeDescLabel, k.Expression())
	if err != nil {
		return "", false, err
	}
	return escrowFeeDescPrefix + sealed, false, nil
}

// openFeeDescriptor opens a sealed fee descriptor key.
func (a *App) openFeeDescriptor(dest string) (escrowKey, error) {
	plain, err := a.open(escrowFeeDescLabel, strings.TrimPrefix(dest, escrowFeeDescPrefix))
	if err != nil {
		return escrowKey{}, err
	}
	return parseEscrowKey(plain, a.keyNet())
}

// highestFeeIndex is the highest fee index any escrow holds (-1 when none), for the admin page.
func highestFeeIndex(ctx context.Context, q rowQuerier) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(max(fee_index),-1),(SELECT allocation_next_index-1 FROM retention_escrow_global_floors WHERE kind='fee')) FROM escrows`).Scan(&n)
	return n, err
}
