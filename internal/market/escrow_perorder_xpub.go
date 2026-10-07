package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
)

// STUB until phase5/qa5w4a-fix (QA5W4A-5, chair ruling) is merged: that round stores each per-order key's extended
// public key (the account key's child <i>) at checkout in escrow_pubkeys.per_order_xpub and provides perOrderXpub over
// it. This stub has the same contract, derived on demand as A-456a did: a party's from its current escrow key, the
// market's from the escrow signer's pinned account key, each only when its /0/0 child is the key the escrow stores. At
// the merge this file is replaced by that helper; its callers (the setup page, a sibling's keys) stay.

// errPerOrderXpub is a per-order key whose extended key cannot be had: (in this stub) the party registered another
// escrow key since checkout, or the escrow signer did not answer with its pinned key.
var errPerOrderXpub = errors.New("escrow: the per-order extended key of this escrow key cannot be derived")

// perOrderXpub is the per-order extended public key (the order wallet's account key, the account key's child <i>) of
// party's key in the escrow escrowID: party "buyer", "vendor" or "market". A non-escrow failure (the database, the
// signer's transport) is returned as is.
func (a *App) perOrderXpub(ctx context.Context, escrowID int64, party string) (bip32Key, error) {
	var row escrowKeyRow
	var pub, user string
	if err := a.db.QueryRowContext(ctx, "SELECT pubkey,COALESCE(user_id,''),origin_fingerprint,origin_path,derivation_suffix FROM escrow_pubkeys WHERE escrow_id=$1 AND party=$2",
		escrowID, party).Scan(&pub, &user, &row.Fingerprint, &row.Origin, &row.Suffix); err != nil {
		return bip32Key{}, err
	}
	var err error
	if row.PubKey, err = hex.DecodeString(pub); err != nil {
		return bip32Key{}, err
	}
	var account escrowKey
	if party == "market" {
		if account, err = a.signerAccount(ctx, a.db); err != nil {
			return bip32Key{}, errors.Join(errPerOrderXpub, err)
		}
	} else {
		var ok bool
		if account, ok, err = a.perOrderAccount(ctx, user); err != nil {
			return bip32Key{}, err
		} else if !ok {
			return bip32Key{}, errPerOrderXpub
		}
	}
	k, ok := orderWalletKey(account, row)
	if !ok {
		return bip32Key{}, errPerOrderXpub
	}
	return k, nil
}

// perOrderAccount is a party's current escrow key as an extended key, or ok false when it has none or it cannot be read.
func (a *App) perOrderAccount(ctx context.Context, userID string) (escrowKey, bool, error) {
	var sealed, fp, path string
	err := a.db.QueryRowContext(ctx, "SELECT xpub_sealed,origin_fingerprint,origin_path FROM user_escrow_keys WHERE user_id=$1", userID).Scan(&sealed, &fp, &path)
	if err == sql.ErrNoRows {
		return escrowKey{}, false, nil
	}
	if err != nil {
		return escrowKey{}, false, err
	}
	xpub, err := a.open("escrow-xpub", sealed)
	if err != nil {
		return escrowKey{}, false, nil
	}
	k, err := parseEscrowKey("[" + fp + path + "]" + xpub)
	return k, err == nil, nil
}

// orderWalletKey is the per-order key (the order wallet's account key) of an escrow key row, derived from account at the
// row's per-order index, or ok false unless its /0/0 child is the row's key: the account is not the one the escrow was
// built with.
func orderWalletKey(account escrowKey, row escrowKeyRow) (bip32Key, bool) {
	steps, err := parseOriginPath(row.Suffix)
	if err != nil || len(steps) != 3 {
		return bip32Key{}, false
	}
	order, leaf, ok := perOrderKeys(account, steps[0])
	return order, ok && bytes.Equal(leaf.PubKey[:], row.PubKey)
}
