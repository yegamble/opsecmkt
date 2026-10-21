package market

import "strconv"

// A-520 (PRIV60-6): a listing's exact stock is shown only to its vendor and to staff (moderators and administrators).
// Everyone else sees a band. An exact public count goes down by one with each sale, so anyone watching it could count
// a vendor's sales and time them against an order they know about; a band changes only when the count crosses an
// edge. The edges: 1–5 is the "few left" range where a buyer ordering a handful still learns whether that many can be
// bought, and where single sales are most telling; 6–20 covers ordinary multi-unit orders; above 20 says only that
// stock is plentiful. "Out of stock" stays exact, since the catalog's in-stock filter and the purchase rule
// (purchasable.go) already reveal it.

// stockBand names the range n falls in, without its exact value.
func stockBand(n int) string {
	switch {
	case n < 1:
		return "0"
	case n <= 5:
		return "1–5"
	case n <= 20:
		return "6–20"
	default:
		return "21+"
	}
}

// StockLabel is a listing's stock as this page's viewer may see it: the exact count for the listing's vendor and for
// staff, its band (stockBand) for everyone else, signed out or not.
func (d PageData) StockLabel(vendorID string, stock int) string {
	if d.SeesExactStock(vendorID) {
		return strconv.Itoa(stock)
	}
	return stockBand(stock)
}

// SeesExactStock reports whether this page's viewer may see the exact stock of a listing of vendorID.
func (d PageData) SeesExactStock(vendorID string) bool {
	u := d.User
	return u != nil && (u.ID == vendorID || u.Role == "moderator" || u.Role == "admin")
}
