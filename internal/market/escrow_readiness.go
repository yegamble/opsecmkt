package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode"
)

// Monero checkout requires eligible fulfillment and configured escrow. These
// gates drive the product/checkout explanation and the administrator's readiness
// panel; direct draft and checkout actions independently enforce the same rules.

func init() {
	registerLoader("product", purchaseGateLoader)
	registerLoader("checkout", purchaseGateLoader)
	registerLoader("admin", escrowReadinessLoader)
}

// readinessListLimit caps each list of the readiness panel; its counts are never capped.
const readinessListLimit = 100

// escrowOn reports whether Monero escrow is enabled, ready right now or not:
// a node that is down refuses a checkout for a while (503), it does not make a listing unavailable.
func (a *App) escrowOn(coin string) bool {
	switch coin {
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
	if err = q.QueryRowContext(ctx, "SELECT kind FROM products WHERE id=$1", productID).Scan(&kind); err != nil {
		return "", "", err
	}
	ok, _, _, err := escrowEligible(ctx, q, &Order{ProductID: productID, Kind: kind, Currency: coin})
	switch {
	case err != nil:
		return "", "", err
	case !ok:
		reason, fix = escrowIneligibleReason(kind)
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
	if d.Product == nil {
		return nil
	}
	var suspended, banned bool
	if err := a.db.QueryRowContext(ctx, "SELECT suspended_at IS NOT NULL,banned_at IS NOT NULL FROM users WHERE id=$1", d.Product.VendorID).Scan(&suspended, &banned); err != nil {
		return err
	}
	d.SellerClosed = banned
	if d.Product.Archived {
		return nil
	}
	if suspended {
		d.VendorSuspensionReason = "Unavailable. " + sellerHeld(banned)
		d.NoPurchase = true
		return nil
	}
	if d.Product.VendorDeactivated || d.Product.VendorVacation {
		reason := sellerVacation
		if d.Product.VendorDeactivated {
			reason = sellerDeactivated
		}
		d.VendorSuspensionReason = "Unavailable. " + reason
		d.NoPurchase = true
		return nil
	}
	if ok, err := userCanReceiveEncrypted(ctx, a.db, d.Product.VendorID); err != nil {
		return err
	} else if !ok {
		d.VendorSuspensionReason = "Unavailable. " + sellerPGPRequired
		d.NoPurchase = true
		return nil
	}
	if d.Product.Kind == "service" {
		d.NoPurchase = true
		return nil
	}
	for _, coin := range []string{"XMR"} {
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
	d.NoPurchase = len(d.PurchaseBlocks) == 1
	return nil
}

// escrowReadinessLoader fills /admin's readiness panel for an escrow-only cutover.
func escrowReadinessLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	v := &EscrowReadinessView{Limit: readinessListLimit}
	for _, coin := range []string{"XMR"} {
		c := ReadinessCoin{Currency: coin, Intake: d.Settings[paymentIntakeKey(coin)], EscrowOn: a.escrowOn(coin)}
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

	d.EscrowReadiness = v
	return nil
}
