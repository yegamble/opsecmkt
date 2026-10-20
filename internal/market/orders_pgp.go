package market

import (
	"context"
	"net/http"
)

const noBuyerOrderKeyReason = "Add a usable PGP public key on your account before creating an order or requesting payment. Your key must support encryption and must not be expired or revoked."

func init() {
	registerLoader("product", orderPGPPageLoader)
	registerLoader("checkout", orderPGPPageLoader)
	registerLoader("order", orderPGPPageLoader)
}

// Display the same requirements enforced under the account locks by lockNewIntake. Key ownership proof and
// second-factor sign-in are separate choices; a current encryption-capable public key is sufficient for orders.
func orderPGPPageLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Product != nil {
		vendorUsable, err := userCanReceiveEncrypted(ctx, a.db, d.Product.VendorID)
		if err != nil {
			return err
		}
		d.VendorKeyUnusable = !vendorUsable
		if d.User != nil && d.User.ID != d.Product.VendorID {
			buyerUsable, err := userCanReceiveEncrypted(ctx, a.db, d.User.ID)
			if err != nil {
				return err
			}
			d.BuyerKeyUnusable = !buyerUsable
		}
		d.NoPurchase = d.NoPurchase || d.VendorKeyUnusable || d.BuyerKeyUnusable
	}
	if d.Order == nil || d.Order.State != stateDraft || d.OrderViewer != roleBuyer {
		return nil
	}
	buyerUsable, err := userCanReceiveEncrypted(ctx, a.db, d.Order.BuyerID)
	if err != nil {
		return err
	}
	vendorUsable, err := userCanReceiveEncrypted(ctx, a.db, d.Order.VendorID)
	if err != nil {
		return err
	}
	d.BuyerKeyUnusable, d.VendorKeyUnusable = !buyerUsable, !vendorUsable
	reason := ""
	if !buyerUsable {
		reason = noBuyerOrderKeyReason
	} else if !vendorUsable {
		reason = noVendorKeyReason
	}
	if reason != "" {
		if d.EscrowCheckout != nil {
			d.EscrowCheckout.Reason = reason
		}
		if d.XMREscrowCheckout != nil {
			d.XMREscrowCheckout.Reason = reason
		}
	}
	return nil
}
