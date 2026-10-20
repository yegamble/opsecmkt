package market

import (
	"context"
	"math"
	"net/url"
	"strconv"
)

// Units supplies single-unit preview and historical in-memory fixtures with the
// database migration's default. Persisted quantities are constrained positive.
func (o Order) Units() int64 {
	if o.Quantity == 0 {
		return 1
	}
	return o.Quantity
}

func orderQuantity(f url.Values) (int64, error) {
	if !f.Has("quantity") {
		return 1, nil
	}
	raw := f.Get("quantity")
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fail(400, "Quantity must be a positive whole number no greater than the available stock.")
		}
	}
	q, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || q < 1 {
		return 0, fail(400, "Quantity must be a positive whole number no greater than the available stock.")
	}
	return q, nil
}

// checkedOrderTotal uses integer atomic currency units. Never let SQL or a
// browser's floating-point calculation overflow or determine the amount owed.
func checkedOrderTotal(unitPrice, quantity int64) (int64, error) {
	if unitPrice < 1 || quantity < 1 || unitPrice > math.MaxInt64/quantity {
		return 0, fail(400, "The total for this quantity is too large. Reduce the quantity or ask the vendor to review the price. Nothing was changed.")
	}
	return unitPrice * quantity, nil
}

// Checkout pages are advisory; stockHook independently checks the actual stock
// under the listing lock when payment is requested.
func quantityCheckoutReason(ctx context.Context, q rowQuerier, o *Order) (string, error) {
	var stock int64
	var archived bool
	if err := q.QueryRowContext(ctx, "SELECT stock,archived FROM products WHERE id=$1", o.ProductID).Scan(&stock, &archived); err != nil {
		return "", err
	}
	if archived {
		return "This listing is no longer available. You can cancel this unfunded draft.", nil
	}
	if stock < o.Units() {
		return "Only " + strconv.FormatInt(stock, 10) + " units are available. Review the listing to save a smaller quantity, or cancel this unfunded draft.", nil
	}
	return "", nil
}
