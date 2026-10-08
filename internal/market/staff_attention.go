package market

import (
	"context"
	"database/sql"
	"net/http"
)

// StaffAttention holds separate, uncapped counts for work a staff member can currently read or act on.
// The categories intentionally overlap: one order may need a dispute decision and a payment review.
type StaffAttention struct {
	Disputes, PaymentReviews, EscrowAlerts, Payouts int
}

func init() { registerLoader("*", loadStaffAttention) }

// payoutNeedsAttention is shared by the admin payout list and the global attention count, so the badge and list
// cannot disagree about blocked, held, failed, or stuck-sending payouts.
func payoutNeedsAttention(alias string) string {
	return "(" + alias + ".state IN ('blocked','held','failed') OR (" + alias + ".state='sending' AND " + alias + ".updated < now()-interval '5 minutes'))"
}

func loadStaffAttention(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	// The session's role can be stale after a role change or suspension. The count query rechecks it in the same
	// statement snapshot as the counts, so a changed or suspended account receives no counters.
	var v StaffAttention
	query := `SELECT
	 (SELECT count(*) FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products pr ON pr.id=o.product_id
	  WHERE d.status='Open' AND o.state='disputed' AND o.buyer_id<>$1 AND pr.vendor_id<>$1
	    AND (NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id) OR $2='admin')),
	 (SELECT count(DISTINCT pm.order_id) FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products pr ON pr.id=o.product_id
	  WHERE ` + paymentFlagOpen("pm") + ` AND o.buyer_id<>$1 AND pr.vendor_id<>$1
	    AND (pm.disposition<>'escalated' OR $2='admin')),
	 (SELECT count(*) FROM orders o JOIN products pr ON pr.id=o.product_id
	  WHERE ` + escrowStaffAlertCandidates("o.id") + ` AND ` + escrowStaffAlert("o.id") + ` AND o.buyer_id<>$1 AND pr.vendor_id<>$1),
	 (SELECT count(*) FROM payouts p WHERE $2='admin' AND ` + payoutNeedsAttention("p") + `)
	FROM users WHERE id=$1 AND role=$2 AND role IN ('moderator','admin') AND suspended_at IS NULL`
	err := a.db.QueryRowContext(ctx, query, d.User.ID, d.User.Role).Scan(&v.Disputes, &v.PaymentReviews, &v.EscrowAlerts, &v.Payouts)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	d.StaffAttention = &v
	return nil
}
