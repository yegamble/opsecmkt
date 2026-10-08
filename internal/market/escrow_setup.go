package market

import (
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
//     per-order key and the market's (perOrderXpub);
//   - the first receiving address the wallet must show, the escrow address;
//   - a Sparrow output descriptor of the order wallet, wsh(sortedmulti(2,…)) over the three keys with /<0;1>/*, the
//     party's own with its origin.
// When a per-order key cannot be had, the page says why. It makes no Bitcoin Core RPC and writes nothing.

func init() {
	registerPage("escrow-setup", pageSpec{Protected: true, Off: escrowOff})
	registerLoader("escrow-setup", escrowSetupLoader)
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
	reader, other := roleBuyer, roleVendor
	if d.User.ID == o.VendorID {
		reader, other = roleVendor, roleBuyer
	}
	own := keys[reader]
	steps, err := parseOriginPath(own.Suffix)
	if err != nil || len(steps) != 3 {
		return errors.New("escrow: a per-order escrow key's suffix is malformed")
	}
	index := "/" + strconv.FormatUint(uint64(steps[0]), 10)
	v := &EscrowSetupView{OrderID: o.ID, Short: shortID(o.ID), Own: reader, OwnPath: "[" + own.Fingerprint + own.Origin + index + "]", OwnDerivation: "m" + own.Origin + index}
	var escrowID int64
	if err = a.db.QueryRowContext(ctx, "SELECT id,address FROM escrows WHERE order_id=$1 AND coin='BTC'", o.ID).Scan(&escrowID, &v.Address); err != nil {
		return err
	}
	d.EscrowSetup, d.Title = v, "Order wallet for order "+v.Short
	// Each per-order key (perOrderXpub): checkout stores all three (QA5W4A-5, the market's since QA5W5A-1), so the page
	// is the same whatever key a party registered since and whether or not the escrow signer answers. The other party's
	// and the market's are the cosigners, the reader's own goes into the Sparrow descriptor. Only a row with no stored
	// key (or one that does not give the stored key) leaves one out, saying so.
	order := map[string]bip32Key{}
	for _, party := range []string{other, "market", reader} {
		k, err := a.perOrderXpub(ctx, escrowID, party)
		switch {
		case err == nil:
			order[party] = k
			continue
		case !errors.Is(err, errPerOrderXpub):
			return err
		case party == reader:
			v.NoSparrow = "Your order wallet key for this escrow cannot be read from the market's records, so no Sparrow descriptor can be given here. Build the order wallet from the seed of the key you registered when you ordered, with the path and the cosigner keys above, and tell the market's staff."
		default:
			v.Unavailable = "The " + map[string]string{roleBuyer: "buyer's", roleVendor: "vendor's", "market": "market's"}[party] + " cosigner key of this escrow cannot be read from the market's records, so it cannot be shown here. Contact the market's staff; the escrow, its address and its keys are unchanged."
		}
		if v.Unavailable != "" {
			return nil
		}
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
