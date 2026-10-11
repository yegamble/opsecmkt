package market

import (
	"context"
	"database/sql"
	"net/http"
)

// StaffAttention holds separate, uncapped counts for work a staff member can currently read or act on.
// The categories intentionally overlap: one order may need a dispute decision and a payment review.
type StaffAttention struct {
	Disputes, PaymentReviews, EscrowAlerts int
}

func init() { registerLoader("*", loadStaffAttention) }

func loadStaffAttention(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || (d.User.Role != "moderator" && d.User.Role != "admin") {
		return nil
	}
	// The session's role can be stale after a role change or suspension. The count query rechecks it in the same
	// statement snapshot as the counts, so a changed or suspended account receives no counters.
	// This navigation query is small at execution time but structurally complex. PostgreSQL's JIT compilation
	// otherwise costs hundreds of milliseconds on each request, even with empty queues. Keep it off for this
	// read transaction only; no connection-wide or operator setting is changed.
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL jit=off"); err != nil {
		return err
	}
	var v StaffAttention
	query := `SELECT
	 (SELECT count(*) FROM disputes d JOIN orders o ON o.id=d.order_id JOIN products pr ON pr.id=o.product_id
	  WHERE d.status='Open' AND o.state='disputed' AND o.buyer_id<>$1 AND pr.vendor_id<>$1
	    AND (NOT EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id) OR $2='admin')),
	 (SELECT count(DISTINCT pm.order_id) FROM payments pm JOIN orders o ON o.id=pm.order_id JOIN products pr ON pr.id=o.product_id
	  WHERE ` + paymentFlagOpen("pm") + ` AND o.buyer_id<>$1 AND pr.vendor_id<>$1
	    AND (pm.disposition<>'escalated' OR $2='admin')),
	 (SELECT count(*) FROM orders o JOIN products pr ON pr.id=o.product_id
	  WHERE ` + escrowStaffAlertCandidates("o.id") + ` AND ` + escrowStaffAlert("o.id") + ` AND o.buyer_id<>$1 AND pr.vendor_id<>$1)
	FROM users WHERE id=$1 AND role=$2 AND role IN ('moderator','admin') AND suspended_at IS NULL`
	err = tx.QueryRowContext(ctx, query, d.User.ID, d.User.Role).Scan(&v.Disputes, &v.PaymentReviews, &v.EscrowAlerts)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	d.StaffAttention = &v
	return nil
}
