package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

// The setup page of a per-order (shape (b)) escrow (A-456b, rulings P4-R2, P4-R17): GET /escrow-setup?id=<order>, what
// one party builds the order's wallet from in Electrum (with a BIP39 seed) or Sparrow. It is behind the gate of
// /escrow-descriptor (ESCROW_BTC on, signed in, the order's buyer or vendor) and party-only: each party sees its own
// setup, never the other's, and staff, strangers and a Bitcoin Core escrow get the unknown page. It shows:
//   - the party's own key origin with the order's per-order index, [fingerprint/48h/1h/0h/2h/<i>], and that path as a
//     wallet asks for it, m/48h/1h/0h/2h/<i> (from the escrow's own stored origin, so it never changes);
//   - the two cosigner keys served rooted (depth 0, parent fingerprint 0, child 0, as SLIP-132 Vpubs): the other party's
//     per-order key, from its current escrow key, and the market's, from the escrow signer's account key, each derived in
//     Go and shown only when its /0/0 child is the key the escrow stores (which Core derived at checkout);
//   - the first receiving address the wallet must show, the escrow address;
//   - a Sparrow output descriptor of the order wallet, wsh(sortedmulti(2,…)) over the three keys with /<0;1>/*, the
//     party's own with its origin (which needs the party's current escrow key to be the one the escrow was built with).
// The only call it makes is the escrow signer's info (a socket, no Bitcoin Core RPC). Nothing is written.

func init() {
	registerPage("escrow-setup", pageSpec{Protected: true, Off: escrowOff})
	registerLoader("escrow-setup", escrowSetupLoader)
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

// escrowSetupLoader fills /escrow-setup (see the file comment); anyone but the order's two parties, and any order that
// is not a per-order escrow, gets the unknown page.
func escrowSetupLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if a.db == nil || d.User == nil {
		return sql.ErrNoRows
	}
	o, err := scanOrder(a.db.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", r.URL.Query().Get("id"), d.User.ID))
	if err != nil {
		return err
	}
	if !o.btcEscrow() {
		return sql.ErrNoRows
	}
	keys, err := escrowKeys(ctx, a.db, o.ID)
	if err != nil {
		return err
	}
	if !isPerOrderKey(keys["market"]) {
		return sql.ErrNoRows // a Bitcoin Core escrow (shape (a)) has no order wallet
	}
	reader, other, otherID := roleBuyer, roleVendor, o.VendorID
	if d.User.ID == o.VendorID {
		reader, other, otherID = roleVendor, roleBuyer, o.BuyerID
	}
	own := keys[reader]
	steps, err := parseOriginPath(own.Suffix)
	if err != nil || len(steps) != 3 {
		return errors.New("escrow: a per-order escrow key's suffix is malformed")
	}
	index := "/" + strconv.FormatUint(uint64(steps[0]), 10)
	v := &EscrowSetupView{OrderID: o.ID, Short: shortID(o.ID), Own: reader, OwnPath: "[" + own.Fingerprint + own.Origin + index + "]", OwnDerivation: "m" + own.Origin + index}
	if err = a.db.QueryRowContext(ctx, "SELECT address FROM escrows WHERE order_id=$1 AND coin='BTC'", o.ID).Scan(&v.Address); err != nil {
		return err
	}
	d.EscrowSetup, d.Title = v, "Order wallet for order "+v.Short
	// The cosigners: the other party's per-order key and the market's, each the key the escrow stores at /0/0.
	order := map[string]bip32Key{}
	otherAccount, ok, err := a.perOrderAccount(ctx, otherID)
	if err != nil {
		return err
	}
	if order[other], ok = orderWalletKey(otherAccount, keys[other]); !ok {
		v.Unavailable = "The " + other + "'s escrow key is no longer the one this escrow was built with, so its cosigner key cannot be shown here. Contact the market's staff; the escrow, its address and its keys are unchanged."
	}
	signer, err := a.signerAccount(ctx, a.db)
	var refused *signerRefusal
	switch {
	case err == nil:
		if order["market"], ok = orderWalletKey(signer, keys["market"]); !ok && v.Unavailable == "" {
			v.Unavailable = "The market's escrow signer key is not the one this escrow was built with, so the market's cosigner key cannot be shown here. Contact the market's staff."
		}
	case errors.Is(err, errSignerNone), errors.Is(err, errSignerUnreachable), errors.As(err, &refused):
		if v.Unavailable == "" {
			v.Unavailable = "The market's escrow signer is not reachable right now, so the market's cosigner key cannot be shown. Try again later."
		}
	default:
		return err
	}
	if v.Unavailable != "" {
		return nil
	}
	ownAccount, _, err := a.perOrderAccount(ctx, d.User.ID)
	if err != nil {
		return err
	}
	if order[reader], ok = orderWalletKey(ownAccount, own); !ok {
		v.NoSparrow = "Your escrow key is no longer the one this escrow was built with, so no Sparrow descriptor can be given here. Build the order wallet from the seed of the key you registered then, with the path and the cosigner keys above."
	}
	desc := map[string]string{}
	for _, party := range []string{roleBuyer, roleVendor, "market"} {
		k := order[party]
		if party == reader {
			desc[party] = v.OwnPath + k.Serialize(versionTpub) + "/<0;1>/*"
			continue
		}
		fp := k.Fingerprint()
		v.Cosigners = append(v.Cosigners, EscrowCosignerView{Who: map[string]string{roleBuyer: "The buyer's key", roleVendor: "The vendor's key", "market": "The market's key"}[party], Key: k.Rooted(versionVpub)})
		desc[party] = "[" + hex.EncodeToString(fp[:]) + "]" + k.Rooted(versionTpub) + "/<0;1>/*"
	}
	if v.NoSparrow == "" {
		v.Sparrow = withChecksum("wsh(sortedmulti(2," + desc[roleBuyer] + "," + desc[roleVendor] + "," + desc["market"] + "))")
	}
	return nil
}
