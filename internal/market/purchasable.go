package market

import (
	"context"
	"net/http"
)

// A-575: one rule says whether a listing takes a new order now. orderDraftAction and lockNewIntake refuse through
// listingRefusal and sellerState.refusal with their established messages, and purchaseUnavailable composes the same
// pieces with the shared escrow (listingEscrowBlock), digital-unit (availableDigitalBatch) and settlement-floor checks
// for display: the catalog, a vendor's public page and the vendor desk mark each listing it refuses "Not available for
// purchase" instead of its stock count, the desk with the reason. The listings stay listed, so the catalog's count and
// pages are unchanged and agree with the cards (TestCatalogMarksUnbuyableListings).
//
// Conditions of the buyer (their own listing, their key and session proof, the quantity asked) are not listing
// conditions, and neither is intake paused by the administrator: a market-wide pause the payment-status banner
// announces and escrow checkout refuses.

// intakeRefusal names the condition a new order is refused on; intakeOpen is none.
type intakeRefusal int

const (
	intakeOpen intakeRefusal = iota
	refusedNoStock
	refusedArchived
	refusedService
	refusedNotVendor
	refusedHeld // suspended, or banned (a ban suspends)
	refusedDeactivated
	refusedVacation
	refusedVendorKey
)

// listingRefusal is the listing side of the rule, in orderDraftAction's order.
func listingRefusal(stock int64, archived bool, kind string) intakeRefusal {
	switch {
	case stock < 1:
		return refusedNoStock
	case archived:
		return refusedArchived
	case kind == "service":
		return refusedService
	}
	return intakeOpen
}

// sellerState is what the seller side of the rule reads from the vendor's account.
type sellerState struct {
	CanSell, Suspended, Banned, Deactivated, Vacation, KeyUsable bool
}

// refusal is the seller side of the rule, in lockNewIntake's order.
func (s sellerState) refusal() intakeRefusal {
	switch {
	case !s.CanSell:
		return refusedNotVendor
	case s.Suspended:
		return refusedHeld
	case s.Deactivated:
		return refusedDeactivated
	case s.Vacation:
		return refusedVacation
	case !s.KeyUsable:
		return refusedVendorKey
	}
	return intakeOpen
}

// reason is the sentence for s's refusal ("" when it takes orders).
func (s sellerState) reason() string {
	switch s.refusal() {
	case refusedNotVendor:
		return sellerNotVendor
	case refusedHeld:
		return sellerHeld(s.Banned)
	case refusedDeactivated:
		return sellerDeactivated
	case refusedVacation:
		return sellerVacation
	case refusedVendorKey:
		return sellerPGPRequired
	}
	return ""
}

// sellerAccountSQL reads sellerState's account columns (all but KeyUsable) of the user $1.
const sellerAccountSQL = "SELECT role,vendor_approved,suspended_at IS NOT NULL,banned_at IS NOT NULL,vendor_deactivated,vendor_vacation FROM users WHERE id=$1"

// scanSellerAccount fills s's account fields from a sellerAccountSQL row.
func (s *sellerState) scanSellerAccount(row interface{ Scan(...any) error }) error {
	var role string
	var approved bool
	if err := row.Scan(&role, &approved, &s.Suspended, &s.Banned, &s.Deactivated, &s.Vacation); err != nil {
		return err
	}
	s.CanSell = (&User{Role: role, VendorApproved: approved}).CanSell()
	return nil
}

// loadSellerState reads vendorID's sellerState without locks, for display.
func loadSellerState(ctx context.Context, q rowQuerier, vendorID string) (sellerState, error) {
	var s sellerState
	if err := s.scanSellerAccount(q.QueryRowContext(ctx, sellerAccountSQL, vendorID)); err != nil {
		return s, err
	}
	var err error
	s.KeyUsable, err = userCanReceiveEncrypted(ctx, q, vendorID)
	return s, err
}

const (
	unavailableService       = "This legacy service listing accepts no new orders."
	unavailableServiceFix    = "Offer it as a supported physical or digital listing instead."
	unavailableNoUnits       = "This digital listing has no distinct digital units left."
	unavailableNoUnitsFix    = "Add digital inventory units."
	unavailableArchived      = "This listing is archived and accepts no new orders."
	unavailableOutOfStock    = "Out of stock."
	unavailableOutOfStockFix = "Add stock."
)

// purchaseUnavailable says why no buyer can order listing p now ("" when one can) and what its vendor can do (fix,
// possibly ""), given its seller's state and the market's XMR intake setting. p needs Stock, Archived, Kind, ID,
// DigitalDeliveryMode and XMROrderUnavailable as load fills them.
func (a *App) purchaseUnavailable(ctx context.Context, q rowQuerier, p *Product, seller sellerState, intake string) (reason, fix string, err error) {
	switch listingRefusal(int64(p.Stock), p.Archived, p.Kind) {
	case refusedNoStock:
		return unavailableOutOfStock, unavailableOutOfStockFix, nil
	case refusedArchived:
		return unavailableArchived, "", nil
	case refusedService:
		return unavailableService, unavailableServiceFix, nil
	}
	if r := seller.reason(); r != "" {
		return r, "", nil
	}
	if intake == intakeEscrow {
		if reason, fix, err = a.listingEscrowBlock(ctx, q, p.ID, "XMR"); err != nil || reason != "" {
			return reason, fix, err
		}
	}
	if p.Kind == "digital" && p.DigitalDeliveryMode == "inventory" {
		if _, err = availableDigitalBatch(ctx, q, p.ID, 1); err != nil {
			if _, ok := err.(*httpError); ok {
				return unavailableNoUnits, unavailableNoUnitsFix, nil
			}
			return "", "", err
		}
	}
	if p.XMROrderUnavailable != "" {
		return p.XMROrderUnavailable, "", nil
	}
	return "", "", nil
}

// purchasableLoader marks the listings of the catalog, a vendor's page and the vendor desk that no buyer can order
// now (purchaseUnavailable). Each seller is read once per page.
func purchasableLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	sellers := map[string]sellerState{}
	for i := range d.Products {
		p := &d.Products[i]
		s, ok := sellers[p.VendorID]
		if !ok {
			var err error
			if s, err = loadSellerState(ctx, a.db, p.VendorID); err != nil {
				return err
			}
			sellers[p.VendorID] = s
		}
		var err error
		if p.Unavailable, p.UnavailableFix, err = a.purchaseUnavailable(ctx, a.db, p, s, d.Settings[paymentIntakeKey("XMR")]); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	for _, page := range []string{"catalog", "vendor", "vendor-dashboard"} {
		registerLoader(page, purchasableLoader)
	}
}
