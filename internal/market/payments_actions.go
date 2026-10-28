package market

import (
	"context"
	"net/http"
)

func init() {
	registerLoader("*", paymentsGlobalLoader)
	registerLoader("vendor-dashboard", escrowOrdersLoader)
	registerPreview("admin", func(d *PageData) {
		d.Providers = nil
	})
}

func paymentsGlobalLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	d.PaymentsEnabled = false
	d.PaymentNetworks = ""
	d.LiveNetwork = a.live
	d.DispatchOn = !physicalEscrowOff(a)
	d.XMREscrowOn, d.XMREscrowAvailable, d.XMRCustody, d.XMREscrowNet = a.escrowXMR != nil, a.xmrEscrowReady(), xmrCustody, a.xmrEscrowNetwork()
	d.XMREscrowNetKnown = a.escrowXMR != nil && a.escrowXMR.formNet() != ""
	return nil
}

// xmrMayHoldSQL is SQL, over an orders alias o, that is true for a Monero escrow order that may still hold coins
// (A-423): all but a cancelled one with no unclaimed deposit recorded, and, since A-710, all but one whose outcome (its
// release or refund) is confirmed and followed to escrowReorgDepth. The vendor desk's tile counts these orders on the
// vendor's listings and its "Payouts not yet confirmed" filter lists them.
const xmrMayHoldSQL = `EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id AND es.coin='XMR') AND (o.state<>'cancelled'
	OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.confirmations>=0 AND NOT pm.locked AND pm.xmr_settlement_id IS NULL))
	AND NOT EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.order_id=o.id AND s.kind IN ('release','refund') AND s.state='confirmed' AND s.reorg_safe_at IS NOT NULL)`

// escrowOrdersLoader counts the vendor's Monero escrow orders that may hold coins (xmrMayHoldSQL) for the vendor desk's
// escrow tile: orders on the viewer's own listings only, not the viewer's purchases (A-710).
func escrowOrdersLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	return a.db.QueryRowContext(ctx, `SELECT count(*) FROM orders o JOIN products p ON p.id=o.product_id WHERE p.vendor_id=$1 AND `+xmrMayHoldSQL,
		d.User.ID).Scan(&d.XMREscrowOrders)
}
