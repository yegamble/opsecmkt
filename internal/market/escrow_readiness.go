package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// Escrow-only readiness (A-444, P4-R8). While a coin's intake is escrow only (payment_intake_<COIN>='escrow'), a
// listing is bought in that coin only through escrow. listingEscrowBlock says why a listing cannot be: the listing
// itself (escrowEligible's rule: a digital listing without automatic delivery content, a physical one without a
// dispatch window, a service listing), the coin's escrow being off on this market, or, for Bitcoin, its vendor having
// no active escrow key, or only a per-order wallet key (A-455: no escrow is made with one until A-456). Such a listing stays visible with a plain "not available for purchase" reason and no buy form
// in that coin (purchaseGateLoader), drafts in that coin are refused (orderDraftAction), and the administrator's
// readiness panel on /admin counts and lists the active listings that cannot use escrow and the vendors with active
// listings but no active Bitcoin escrow key (escrowReadinessLoader).

func init() {
	registerLoader("product", purchaseGateLoader)
	registerLoader("checkout", purchaseGateLoader)
	registerLoader("admin", escrowReadinessLoader)
}

// readinessListLimit caps each list of the readiness panel; its counts are never capped.
const readinessListLimit = 100

// escrowOn reports whether coin's escrow is enabled on this market (ESCROW_BTC, ESCROW_XMR), ready right now or not:
// a node that is down refuses a checkout for a while (503), it does not make a listing unavailable.
func (a *App) escrowOn(coin string) bool {
	switch coin {
	case "BTC":
		return a.escrowConfigured
	case "XMR":
		return a.escrowXMR != nil
	}
	return false
}

// goSpaces is every rune strings.TrimSpace removes (unicode.IsSpace), so escrowEligibleSQL's btrim agrees with
// escrowEligible on whether a listing's delivery content is blank.
var goSpaces = func() string {
	var b strings.Builder
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}()

// escrowEligibleSQL is escrowEligible's listing rule for products p ($1 = goSpaces), for listings that are not
// archived (a taken-down listing is archived).
var escrowEligibleSQL = `(p.kind='digital' AND btrim(p.delivery_content,$1)<>'' AND char_length(p.delivery_content)<=` + strconv.Itoa(maxDeliveryChars) + `
	OR p.kind='physical' AND p.dispatch_days IS NOT NULL)`

// escrowIneligibleReason is why a listing of kind cannot use escrow, and what its vendor can do.
func escrowIneligibleReason(kind string) (reason, fix string) {
	switch kind {
	case "digital":
		return "This digital listing has no automatic delivery content, which escrow needs.", "Edit listing to add delivery content, sent automatically once an order is paid."
	case "physical":
		return "This physical listing has no dispatch window, which escrow needs.", "Edit listing to set a dispatch window."
	}
	return "Escrow does not take service listings.", "Offer it as a digital listing with automatic delivery or a physical listing with a dispatch window."
}

// listingEscrowBlock says why listing productID cannot be bought through coin's escrow ("" when it can), and what its
// vendor can do about it. q is a *sql.Tx or *sql.DB.
func (a *App) listingEscrowBlock(ctx context.Context, q rowQuerier, productID, coin string) (reason, fix string, err error) {
	if !a.escrowOn(coin) {
		return "Escrow for " + coin + " is not enabled on this market.", "Only the market's operator can enable it.", nil
	}
	var kind string
	var vendorKey, perOrder bool
	if err = q.QueryRowContext(ctx, `SELECT p.kind,EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=p.vendor_id AND k.active_at IS NOT NULL),
		EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=p.vendor_id AND k.active_at IS NOT NULL AND k.layout<>'core')
		FROM products p WHERE p.id=$1`, productID).Scan(&kind, &vendorKey, &perOrder); err != nil {
		return "", "", err
	}
	ok, _, _, err := escrowEligible(ctx, q, &Order{ProductID: productID, Kind: kind, Currency: coin})
	switch {
	case err != nil:
		return "", "", err
	case !ok:
		reason, fix = escrowIneligibleReason(kind)
	case coin == "BTC" && !vendorKey:
		reason, fix = "The vendor has no active Bitcoin escrow key.", "Register your Bitcoin escrow key and sign its practice transaction on your escrow key page."
	case coin == "BTC" && perOrder && !a.perOrderAvailable(): // A-456b: checkout refuses it the same way (errEscrowSigner)
		reason, fix = "The vendor's Bitcoin escrow key is for a per-order wallet. "+errPerOrderUnavailable,
			"Register a Bitcoin Core escrow key and sign its practice transaction on your escrow key page, or ask the market's operator about the escrow signer."
	}
	return reason, fix, nil
}

// purchaseBlock is the refusal of a new order of productID in coin while coin's intake is escrow only and the listing
// cannot use escrow; nil otherwise.
func (a *App) purchaseBlock(ctx context.Context, q rowQuerier, productID, coin string) error {
	var intake string
	if err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey(coin)).Scan(&intake); err != nil && err != sql.ErrNoRows {
		return err
	}
	if intake != intakeEscrow {
		return nil
	}
	reason, _, err := a.listingEscrowBlock(ctx, q, productID, coin)
	if err != nil || reason == "" {
		return err
	}
	return fail(409, "Not available for purchase in "+coin+": this market takes new "+coin+" payments through escrow only. "+reason+" Nothing was changed.")
}

// purchaseGateLoader: the product and checkout pages of a listing, per coin whose intake is escrow only, say why the
// listing cannot be bought in it; with every coin blocked it is not available for purchase at all.
func purchaseGateLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Product == nil || d.Product.Archived {
		return nil
	}
	for _, coin := range []string{"BTC", "XMR"} {
		if d.Settings[paymentIntakeKey(coin)] != intakeEscrow {
			continue
		}
		reason, fix, err := a.listingEscrowBlock(ctx, a.db, d.Product.ID, coin)
		if err != nil {
			return err
		}
		if reason == "" {
			continue
		}
		if d.PurchaseBlocked == nil {
			d.PurchaseBlocked = map[string]bool{}
		}
		d.PurchaseBlocked[coin] = true
		d.PurchaseBlocks = append(d.PurchaseBlocks, PurchaseBlock{Currency: coin, Reason: reason, Fix: fix})
	}
	d.NoPurchase = len(d.PurchaseBlocks) == 2
	return nil
}

// escrowReadinessLoader fills /admin's readiness panel for an escrow-only cutover.
func escrowReadinessLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	v := &EscrowReadinessView{Limit: readinessListLimit}
	for _, coin := range []string{"BTC", "XMR"} {
		c := ReadinessCoin{Currency: coin, Intake: d.Settings[paymentIntakeKey(coin)], EscrowOn: a.escrowOn(coin), Pooled: a.pooledConfigured(coin)}
		switch {
		case !c.EscrowOn || a.escrowReady(coin):
		case coin == "XMR":
			c.NotReady, _ = a.escrowXMR.notReady()
		default:
			c.NotReady = "its startup checks have not passed; the server log names which" // QA4W5-9: not always a node or wallet
		}
		v.Coins = append(v.Coins, c)
	}
	rows, err := a.db.QueryContext(ctx, `SELECT p.id,p.title,u.handle,p.kind,count(*) OVER() FROM products p JOIN users u ON u.id=p.vendor_id
		WHERE NOT p.archived AND NOT `+escrowEligibleSQL+` ORDER BY u.handle,p.title,p.id LIMIT $2`, goSpaces, readinessListLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l ReadinessListing
		if err = rows.Scan(&l.ID, &l.Title, &l.Vendor, &l.Kind, &v.ListingCount); err != nil {
			return err
		}
		l.Reason, _ = escrowIneligibleReason(l.Kind)
		v.Listings = append(v.Listings, l)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	rows, err = a.db.QueryContext(ctx, `SELECT u.handle,count(*),count(*) OVER() FROM products p JOIN users u ON u.id=p.vendor_id
		WHERE NOT p.archived AND NOT EXISTS(SELECT 1 FROM user_escrow_keys k WHERE k.user_id=p.vendor_id AND k.active_at IS NOT NULL AND k.layout='core')
		GROUP BY u.id,u.handle ORDER BY u.handle LIMIT $1`, readinessListLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r ReadinessVendor
		if err = rows.Scan(&r.Handle, &r.Listings, &v.VendorCount); err != nil {
			return err
		}
		v.Vendors = append(v.Vendors, r)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	d.EscrowReadiness = v
	return nil
}
