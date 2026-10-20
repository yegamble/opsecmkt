package market

import (
	"context"
	"database/sql"
	"net/http"
)

// The escrow on the order page (WR-escrow Phase 1, slice P1-b; ES-3, ES-10): the escrow address, the escrow
// descriptor with its checksum (the reader's own key with its origin, the other two bare) and a download of it, the
// honest-claim copy and the tracing notice, the funding figures and each deposit, and the order's settlement
// (escrowSettlementView, P1-c1). On a draft that can use escrow the buyer is offered escrow checkout beside the pooled
// payment request. Nothing here makes an RPC.

func init() {
	registerRaw("/escrow-descriptor", escrowDescriptorDownload)
}

// escrowOrder fills the escrow panel of an escrow order, or the escrow checkout offer on a draft, for loadOrderDetail
// (which has already resolved the viewer). An escrow order always gets its panel: with ESCROW_BTC switched off, or
// escrow down, it shows the escrow address with the unavailable copy, never the pooled payment panel, which would say
// no funds are held (A-315). Any other order with ESCROW_BTC off gets nothing, so its page is as before. A Monero
// escrow order (migration 084) gets its Monero escrow panel instead (xmrEscrowOrder), and an XMR draft the Monero
// escrow checkout offer (xmrEscrowCheckoutOffer).
func (a *App) escrowOrder(ctx context.Context, d *PageData) error {
	if d.Order.Currency != "XMR" {
		return nil
	}
	if !d.Order.Escrow {
		return a.xmrEscrowCheckoutOffer(ctx, d)
	}
	return a.xmrEscrowOrder(ctx, d)
}

// escrowDescriptorFor is the escrow descriptor of order as reader (buyer, vendor or "") sees it: the bare keys of
// wsh(sortedmulti(2, buyer, vendor, market)), the reader's own with its origin, and the checksum.
func escrowDescriptorFor(ctx context.Context, db *sql.DB, orderID, reader string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT p.party,p.pubkey,p.origin_fingerprint,p.origin_path,p.derivation_suffix FROM escrow_pubkeys p JOIN escrows es ON es.id=p.escrow_id
		WHERE es.order_id=$1 AND es.coin='BTC' ORDER BY CASE p.party WHEN 'buyer' THEN 0 WHEN 'vendor' THEN 1 ELSE 2 END`, orderID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var party, pub, fp, origin, suffix string
		if err = rows.Scan(&party, &pub, &fp, &origin, &suffix); err != nil {
			return "", err
		}
		if party == reader {
			pub = "[" + fp + origin + suffix + "]" + pub
		}
		keys = append(keys, pub)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if len(keys) != 3 {
		return "", sql.ErrNoRows
	}
	return withChecksum("wsh(sortedmulti(2," + keys[0] + "," + keys[1] + "," + keys[2] + "))"), nil
}

// escrowDescriptorDownload serves GET /escrow-descriptor?id=<order>: the escrow descriptor as the order page shows it
// to its reader, as a text file. With ESCROW_BTC off, for anyone but the order's buyer and vendor, and for an order
// without an escrow it answers as an unknown page does.
func escrowDescriptorDownload(a *App, w http.ResponseWriter, r *http.Request, u *User) {
	if !a.escrowConfigured || a.db == nil || u == nil {
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
	ctx := r.Context()
	o, err := scanOrder(a.db.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2)", r.URL.Query().Get("id"), u.ID))
	if err == sql.ErrNoRows || err == nil && !o.btcEscrow() {
		a.refuse(w, r, u, 404, pageNotFound)
		return
	}
	reader := roleVendor
	if u.ID == o.BuyerID {
		reader = roleBuyer
	}
	var desc string
	if err == nil {
		desc, err = escrowDescriptorFor(ctx, a.db, o.ID, reader)
	}
	if err != nil {
		writeEscrowDownloadFailure(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="escrow-`+shortID(o.ID)+`.txt"`)
	w.Write([]byte(desc + "\n"))
}
