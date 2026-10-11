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
	d.BitcoinPriceLabel = a.bitcoinPriceLabel()
	d.EscrowOn, d.EscrowAvailable = a.escrowConfigured, a.escrow.Load() != nil
	if a.escrowNetKnown() {
		d.EscrowNet = a.escrowNetwork()
	}
	d.DispatchOn = !physicalEscrowOff(a)
	d.XMREscrowOn, d.XMREscrowAvailable, d.XMRCustody, d.XMREscrowNet = a.escrowXMR != nil, a.xmrEscrowReady(), xmrCustody, a.xmrEscrowNetwork()
	d.XMREscrowNetKnown = a.escrowXMR != nil && a.escrowXMR.formNet() != ""
	return nil
}

// escrowOrdersLoader counts the viewer's escrow orders that may hold coins for the vendor desk's escrow tiles.
func escrowOrdersLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	// The viewer's escrow orders (as buyer or vendor), whatever ESCROW_BTC says now (QA31-2): all but a cancelled one
	// with nothing recorded, and that one too while escrow is not enabled, since its deposits are then not read.
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM escrows es JOIN orders o ON o.id=es.order_id JOIN products p ON p.id=o.product_id
		WHERE es.coin='BTC' AND (o.buyer_id=$1 OR p.vendor_id=$1) AND ($2 OR o.state<>'cancelled'
			OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.confirmations>=0 AND NOT pm.locked AND pm.settlement_id IS NULL))`,
		d.User.ID, a.escrow.Load() == nil).Scan(&d.EscrowOrders); err != nil {
		return err
	}
	// The same for the viewer's Monero escrow orders (A-423): all but a cancelled one with no deposit recorded.
	return a.db.QueryRowContext(ctx, `SELECT count(*) FROM escrows es JOIN orders o ON o.id=es.order_id JOIN products p ON p.id=o.product_id
		WHERE es.coin='XMR' AND (o.buyer_id=$1 OR p.vendor_id=$1) AND (o.state<>'cancelled'
			OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.confirmations>=0 AND NOT pm.locked AND pm.xmr_settlement_id IS NULL))`,
		d.User.ID).Scan(&d.XMREscrowOrders)
}
