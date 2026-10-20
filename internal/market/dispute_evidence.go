package market

import (
	"context"
	"database/sql"
	"net/http"
)

const evidencePerParty = 4

// lockEvidenceOrder serializes the final authorization with dispute decisions and
// retention. Staff status grants no right to submit evidence for somebody else.
func lockEvidenceOrder(ctx context.Context, tx *sql.Tx, a *App, actor, id string) error {
	var state string
	var expired bool
	err := tx.QueryRowContext(ctx, `SELECT o.state,o.private_expired_at IS NOT NULL OR ($3::boolean AND COALESCE(o.financial_closed_at+interval '30 days'<=now(),false))
 FROM orders o JOIN products p ON p.id=o.product_id
 WHERE o.id=$1 AND (o.buyer_id=$2 OR p.vendor_id=$2) AND EXISTS(SELECT 1 FROM disputes d WHERE d.order_id=o.id)
 FOR UPDATE OF o`, id, actor, a.retention != nil).Scan(&state, &expired)
	if err == sql.ErrNoRows {
		return errNoOrder
	}
	if err != nil {
		return err
	}
	if expired {
		return fail(http.StatusConflict, "Evidence not saved: this order's private retention window has expired. Nothing was stored.")
	}
	if state != stateDisputed {
		return fail(http.StatusConflict, "Evidence not saved: only an open dispute accepts new evidence. Nothing was stored.")
	}
	return nil
}

type DisputeEvidenceImage struct {
	StoredImage
	Party, Created string
}
type DisputeEvidenceView struct {
	UploadsView
	Images    []DisputeEvidenceImage
	CanUpload bool
}

func init() { registerLoader("order", loadDisputeEvidence) }

func loadDisputeEvidence(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Order == nil || d.User == nil {
		return nil
	}
	o, u := d.Order, d.User
	party := u.ID == o.BuyerID || u.ID == o.VendorID
	if !party {
		reviewer, err := staffReviewer(ctx, a, o, u)
		if err != nil {
			return err
		}
		if !reviewer {
			return nil
		}
	}
	var disputed, expired, purged bool
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM disputes WHERE order_id=$1),private_expired_at IS NOT NULL OR ($2::boolean AND COALESCE(financial_closed_at+interval '30 days'<=now(),false)),private_expired_at IS NOT NULL FROM orders WHERE id=$1`, o.ID, a.retention != nil).Scan(&disputed, &expired, &purged); err != nil {
		return err
	}
	if !disputed {
		return nil
	}
	v := &DisputeEvidenceView{CanUpload: party && o.State == stateDisputed && !expired}
	if expired && !purged {
		v.Blocked = "This order no longer accepts new evidence: its private retention window has ended."
	}
	if purged {
		v.Blocked = "Private dispute evidence has expired under the order retention policy."
		d.DisputeImages = v
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,width,height,octet_length("full"),description,CASE WHEN owner_user_id=$2 THEN 'buyer' ELSE 'vendor' END,to_char(created AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI')||' UTC' FROM images WHERE order_id=$1 AND kind='evidence' ORDER BY created,id`, o.ID, o.BuyerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, desc, who, created string
		var w, h int
		var size int64
		if err := rows.Scan(&id, &w, &h, &size, &desc, &who, &created); err != nil {
			return err
		}
		img := storedImage(len(v.Images)+1, id, w, h, size, desc)
		img.Thumb = evidenceImageURL(id, false)
		img.Full = evidenceImageURL(id, true)
		v.Images = append(v.Images, DisputeEvidenceImage{StoredImage: img, Party: who, Created: created})
		if party && ((who == "buyer" && u.ID == o.BuyerID) || (who == "vendor" && u.ID == o.VendorID)) {
			count++
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count >= evidencePerParty {
		v.CanUpload = false
		v.Blocked = "You have added all 4 images allowed for your side of this dispute."
	}
	if v.CanUpload {
		state, err := a.uploadScannerState(ctx)
		if err != nil {
			return err
		}
		uploadState(&v.UploadsView, state)
		v.Authorize, v.NoPath = a.privateUploadPath(ctx, u)
		if v.Authorize && d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, u.ID); err != nil {
				return err
			}
		}
	}
	d.DisputeImages = v
	return nil
}

func evidenceImageURL(id string, full bool) string { return "/desk/evidence/" + id + imageSuffix(full) }

// previewDisputeImages exercises the complete image-only form with scripts off.
// Preview POSTs remain refused and the sample carries no real evidence.
func previewDisputeImages(d *PageData) {
	if d.Order == nil {
		return
	}
	d.Order.ID, d.Order.State, d.Order.Status = previewDisputedID, stateDisputed, stateLabel(stateDisputed)
	d.Disputes = []Dispute{{ID: "sample-image-dispute", OrderID: previewDisputedID, Reason: "Sample dispute; no evidence is stored.", Status: "Open", Created: "Sample", Opener: roleBuyer}}
	d.DisputeImages = &DisputeEvidenceView{CanUpload: true}
}
