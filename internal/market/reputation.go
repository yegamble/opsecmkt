package market

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// A-653 (owner ruling 2026-10-25, "Go rule"): a vendor's reputation figures are counted on read from the market's own
// order, order event, dispute, review and listing rows over retained history, and a fixed table of Go predicates
// proposes one closed label. Nothing is stored and no model is involved: this code never reads the moderation_* tables
// (capture is optional, budget-limited and keyed by a random actor reference) or package moderation
// (TestReputationIndependentOfModerationCapture). The figures and label are shown to administrators only, on an account
// page (/admin/users?handle=), and an administrator may confirm a HIGH_REPUTATION label when letting the vendor publish
// without review (vendorAutoApproveAction recomputes it inside that transaction).

// The closed labels. INSUFFICIENT_HISTORY is the default for a vendor with few completed orders. reputationRank orders
// them; worsening any adverse figure, or an unreviewed completion ending adversely instead, never raises the rank
// (TestReputationMonotonic).
const (
	repHigh         = "HIGH_REPUTATION"
	repStandard     = "STANDARD"
	repInsufficient = "INSUFFICIENT_HISTORY"
	repAtRisk       = "AT_RISK"
)

// The reason codes: one per row of the rule table, and STRONG_RECORD when no row holds the vendor below HIGH_REPUTATION.
const (
	repFewCompleted  = "FEW_COMPLETED_ORDERS"
	repFewBuyers     = "FEW_DISTINCT_BUYERS"
	repConcentration = "BUYER_CONCENTRATION"
	repNondelivery   = "NONDELIVERY_REFUND_RATE"
	repDisputesLost  = "DISPUTES_LOST"
	repLowRatings    = "LOW_RATINGS"
	repRemovals      = "LISTING_REMOVALS"
	repStrongRecord  = "STRONG_RECORD"
)

// The thresholds. A rate is an adverse count against the completed orders plus that count, so another kind of adverse
// outcome never dilutes it; an adverse rate gives AT_RISK only from repRiskMin events.
const (
	repMinCompleted        = 5  // fewer completed orders: INSUFFICIENT_HISTORY
	repHighCompleted       = 25 // HIGH_REPUTATION needs at least this many completed orders
	repHighBuyers          = 10 // and this many distinct buyers of them
	repHighSpreadPct       = 50 // distinct buyers at least this share of completed, non-delivery and lost orders for HIGH_REPUTATION
	repRiskSpreadPct       = 25 // below this share, from repMinCompleted such orders with one completed: AT_RISK
	repRiskMin             = 3
	repHighNondeliveryPct  = 5
	repRiskNondeliveryPct  = 20
	repHighLostPct         = 2
	repRiskLostPct         = 10
	repHighLowRatingPct    = 10
	repRiskLowRatingPct    = 30
	repRiskRemovals        = 3 // current takedowns and rejections; any one keeps the vendor below HIGH_REPUTATION
	repLowRating           = 2 // a counted review of this many stars or fewer is a low rating
	reputationDigestPrefix = "A-653 reputation figures v2"
)

// reputationRank orders the labels, best first: HIGH_REPUTATION 3, STANDARD 2, INSUFFICIENT_HISTORY 1, AT_RISK 0.
func reputationRank(label string) int {
	switch label {
	case repHigh:
		return 3
	case repStandard:
		return 2
	case repInsufficient:
		return 1
	}
	return 0
}

// ReputationFigures are a vendor's counts over funded orders (orders that reached paid). Completed counts only the
// three ordinary completions; a dispute counts only once decided or conceded, never merely opened. Settlement mechanics
// (stray refunds, buyer returns, failed, external or superseded settlements) and payment-window expiries are not read.
type ReputationFigures struct {
	Funded, FundedBuyers                                         int
	CompletedConfirmed, CompletedDeadline, CompletedDigital      int // buyer confirmed receipt; release deadline; digital order completed by the system (at its dispute window's end, A-709; on funding before)
	CompletedBuyers                                              int // distinct buyers of those completed orders
	NoShipCancels, VendorCancels                                 int // paid orders cancelled by the buyer (not shipped or delivered in time) or by the vendor
	DecidedRefund, DecidedRelease, BuyerConceded, VendorConceded int
	Reviews, LowReviews                                          int // counted reviews (reviewCountedSQL) and those of repLowRating stars or fewer
	Takedowns, Rejections                                        int // the vendor's listings taken down or rejected now
	// HeldRefunds: completed orders, or orders decided for the vendor, that an administrator refunded to the buyer because
	// the vendor's ban held the release too long (A-652f). Its own class: never a no-ship cancel or a vendor cancellation,
	// and not an adverse rate; it counts in the buyer spread, so a completion refunded so never lifts the label.
	HeldRefunds int
}

func (f ReputationFigures) Completed() int {
	return f.CompletedConfirmed + f.CompletedDeadline + f.CompletedDigital
}
func (f ReputationFigures) Nondelivery() int  { return f.NoShipCancels + f.VendorCancels }
func (f ReputationFigures) DisputesLost() int { return f.DecidedRefund + f.VendorConceded }
func (f ReputationFigures) Removals() int     { return f.Takedowns + f.Rejections }

// ReputationRule is one row of the fixed rule table: its figure, what HIGH_REPUTATION needs, what gives a lower label,
// and the label the row holds the vendor to now ("" when it holds nothing).
type ReputationRule struct {
	Code, Figure, High, Lower, Effect string
}

// ReputationView is what an administrator's account page shows of a vendor: figures, the rule table, the proposed label
// with its reason codes, and the digest the confirmation form carries.
type ReputationView struct {
	Figures ReputationFigures
	Rules   []ReputationRule
	Label   string
	Reasons []string
	Digest  string
}

// repAtLeast reports whether n is at least pct percent of of, and not zero.
func repAtLeast(n, of, pct int) bool { return n > 0 && n*100 >= of*pct }

func repPct(n, of int) string {
	if of == 0 {
		return "0%"
	}
	return strconv.Itoa(n*100/of) + "%"
}

// reputationRules evaluates the rule table for f. Every predicate is monotonic: a larger adverse count, fewer distinct
// buyers or an unreviewed completion counted instead as an adverse outcome never lifts the label.
func reputationRules(f ReputationFigures) []ReputationRule {
	c, b := f.Completed(), f.CompletedBuyers
	rate := func(code, what string, n, highPct, riskPct int) ReputationRule {
		r := ReputationRule{Code: code, Figure: fmt.Sprintf("%s against %d completed: %s of %d", what, c, repPct(n, c+n), c+n),
			High: fmt.Sprintf("below %d%%", highPct), Lower: fmt.Sprintf("%s at %d or more and %d%% or more", repAtRisk, repRiskMin, riskPct)}
		switch {
		case n >= repRiskMin && repAtLeast(n, c+n, riskPct):
			r.Effect = repAtRisk
		case repAtLeast(n, c+n, highPct):
			r.Effect = repStandard
		}
		return r
	}
	volume := ReputationRule{Code: repFewCompleted, Figure: fmt.Sprintf("%d completed (buyer confirmed %d, release deadline %d, digital dispute window ended %d) of %d funded; %d held releases refunded to the buyer after a ban",
		c, f.CompletedConfirmed, f.CompletedDeadline, f.CompletedDigital, f.Funded, f.HeldRefunds),
		High: fmt.Sprintf("%d or more", repHighCompleted), Lower: fmt.Sprintf("%s below %d", repInsufficient, repMinCompleted)}
	switch {
	case c < repMinCompleted:
		volume.Effect = repInsufficient
	case c < repHighCompleted:
		volume.Effect = repStandard
	}
	buyers := ReputationRule{Code: repFewBuyers, Figure: fmt.Sprintf("%d distinct buyers of completed orders (%d of funded orders)", b, f.FundedBuyers),
		High: fmt.Sprintf("%d or more", repHighBuyers)}
	if b < repHighBuyers {
		buyers.Effect = repStandard
	}
	// QA89-1: the spread is measured against the orders the rates count (completed, non-delivery refunds and disputes
	// lost) and the held-release refunds (A-652f), so a repeat buyer's completion ending adversely or refunded after a ban
	// instead keeps its share and never lifts the label.
	adverse := f.Nondelivery() + f.DisputesLost() + f.HeldRefunds
	s := c + adverse
	spread := ReputationRule{Code: repConcentration, Figure: fmt.Sprintf("%d distinct buyers for %d completed and %d non-delivery refunds, disputes lost or held releases refunded: %s of %d",
		b, c, adverse, repPct(b, s), s),
		High: fmt.Sprintf("%d%% or more", repHighSpreadPct), Lower: fmt.Sprintf("%s below %d%% from %d such orders", repAtRisk, repRiskSpreadPct, repMinCompleted)}
	switch {
	case c > 0 && s >= repMinCompleted && b*100 < s*repRiskSpreadPct:
		spread.Effect = repAtRisk
	case c > 0 && b*100 < s*repHighSpreadPct:
		spread.Effect = repStandard
	}
	nondelivery := rate(repNondelivery, fmt.Sprintf("%d non-delivery refunds (buyer no-ship cancels %d, vendor cancellations of paid orders %d)",
		f.Nondelivery(), f.NoShipCancels, f.VendorCancels), f.Nondelivery(), repHighNondeliveryPct, repRiskNondeliveryPct)
	lost := rate(repDisputesLost, fmt.Sprintf("%d disputes lost (decided refund %d, vendor conceded %d; won: decided release %d, buyer conceded %d)",
		f.DisputesLost(), f.DecidedRefund, f.VendorConceded, f.DecidedRelease, f.BuyerConceded), f.DisputesLost(), repHighLostPct, repRiskLostPct)
	ratings := ReputationRule{Code: repLowRatings, Figure: fmt.Sprintf("%d of %d counted reviews at %d stars or fewer: %s", f.LowReviews, f.Reviews, repLowRating, repPct(f.LowReviews, f.Reviews)),
		High: fmt.Sprintf("below %d%%", repHighLowRatingPct), Lower: fmt.Sprintf("%s at %d or more and %d%% or more", repAtRisk, repRiskMin, repRiskLowRatingPct)}
	switch {
	case f.LowReviews >= repRiskMin && repAtLeast(f.LowReviews, f.Reviews, repRiskLowRatingPct):
		ratings.Effect = repAtRisk
	case repAtLeast(f.LowReviews, f.Reviews, repHighLowRatingPct):
		ratings.Effect = repStandard
	}
	removals := ReputationRule{Code: repRemovals, Figure: fmt.Sprintf("%d listings now (taken down %d, rejected %d)", f.Removals(), f.Takedowns, f.Rejections),
		High: "none", Lower: fmt.Sprintf("%s at %d or more", repAtRisk, repRiskRemovals)}
	switch {
	case f.Removals() >= repRiskRemovals:
		removals.Effect = repAtRisk
	case f.Removals() > 0:
		removals.Effect = repStandard
	}
	return []ReputationRule{volume, buyers, spread, nondelivery, lost, ratings, removals}
}

// reputationLabel proposes the label for f and its reason codes: AT_RISK when any row gives it, else
// INSUFFICIENT_HISTORY when there are too few completed orders, else STANDARD when any row keeps the vendor below
// HIGH_REPUTATION, else HIGH_REPUTATION (STRONG_RECORD).
func reputationLabel(rules []ReputationRule) (string, []string) {
	for _, label := range []string{repAtRisk, repInsufficient, repStandard} {
		var reasons []string
		for _, r := range rules {
			if r.Effect == label {
				reasons = append(reasons, r.Code)
			}
		}
		if reasons != nil {
			return label, reasons
		}
	}
	return repHigh, []string{repStrongRecord}
}

// reputationDigest names vendorID's figures: the confirmation form carries it, so a change since the page was shown
// is refused.
func reputationDigest(vendorID string, f ReputationFigures) string {
	n := []int{f.Funded, f.FundedBuyers, f.CompletedConfirmed, f.CompletedDeadline, f.CompletedDigital, f.CompletedBuyers, f.NoShipCancels,
		f.VendorCancels, f.DecidedRefund, f.DecidedRelease, f.BuyerConceded, f.VendorConceded, f.Reviews, f.LowReviews, f.Takedowns, f.Rejections, f.HeldRefunds}
	parts := make([]string, len(n))
	for i, x := range n {
		parts[i] = strconv.Itoa(x)
	}
	h := sha256.Sum256([]byte(reputationDigestPrefix + "\x00" + vendorID + "\x00" + strings.Join(parts, ",")))
	return hex.EncodeToString(h[:16])
}

// assessReputation is the pure rule: the table, label and digest for vendorID's figures f.
func assessReputation(vendorID string, f ReputationFigures) *ReputationView {
	v := &ReputationView{Figures: f, Rules: reputationRules(f), Digest: reputationDigest(vendorID, f)}
	v.Label, v.Reasons = reputationLabel(v.Rules)
	return v
}

// A completion counts only by its ordinary route: the buyer's confirmation from shipped or delivered, the system's at
// a physical order's release deadline (from shipped) or a digital order's at the end of its dispute window (from
// delivered; A-709, on funding before it). The event that ended an order is its latest move into its current state;
// same-state notes (settlement, decision and dispute-window notes) are not moves. $2–$5 are the decided and conceded dispute statuses. A held-release refund (A-652f) is the one move out of
// completed or resolved, an administrator's (staff) to cancelled.
var reputationSQL = `WITH f AS (SELECT o.id,o.buyer_id,o.state,
		(SELECT e.from_state||':'||CASE WHEN e.actor_id IS NULL THEN 'system' WHEN e.actor_id=o.buyer_id THEN 'buyer' WHEN e.actor_id=p.vendor_id THEN 'vendor' ELSE 'staff' END
			FROM order_events e WHERE e.order_id=o.id AND e.to_state=o.state AND e.from_state<>o.state ORDER BY e.id DESC LIMIT 1) AS ended
	FROM orders o JOIN products p ON p.id=o.product_id
	WHERE p.vendor_id=$1 AND EXISTS(SELECT 1 FROM order_events e WHERE e.order_id=o.id AND e.to_state='paid' AND e.from_state<>'paid')),
c AS (SELECT buyer_id,ended FROM f WHERE state='completed' AND ended IN ('shipped:buyer','delivered:buyer','shipped:system','delivered:system')),
d AS (SELECT d.status FROM disputes d JOIN f ON f.id=d.order_id),
r AS (SELECT r.rating FROM reviews r JOIN orders o ON o.id=r.order_id JOIN products p ON p.id=r.product_id WHERE p.vendor_id=$1 AND o.state='completed' AND ` + reviewCountedSQL("r") + `)
SELECT (SELECT count(*) FROM f),(SELECT count(DISTINCT buyer_id) FROM f),
	(SELECT count(*) FROM c WHERE ended IN ('shipped:buyer','delivered:buyer')),(SELECT count(*) FROM c WHERE ended='shipped:system'),
	(SELECT count(*) FROM c WHERE ended='delivered:system'),(SELECT count(DISTINCT buyer_id) FROM c),
	(SELECT count(*) FROM f WHERE state='cancelled' AND ended='paid:buyer'),(SELECT count(*) FROM f WHERE state='cancelled' AND ended='paid:vendor'),
	(SELECT count(*) FROM d WHERE status=$2),(SELECT count(*) FROM d WHERE status=$3),(SELECT count(*) FROM d WHERE status=$4),(SELECT count(*) FROM d WHERE status=$5),
	(SELECT count(*) FROM r),(SELECT count(*) FROM r WHERE rating<=` + strconv.Itoa(repLowRating) + `),
	(SELECT count(*) FROM products p WHERE p.vendor_id=$1 AND p.admin_archive_reason IS NOT NULL),
	(SELECT count(*) FROM products p WHERE p.vendor_id=$1 AND p.review_state='rejected'),
	(SELECT count(*) FROM f WHERE state='cancelled' AND ended IN ('completed:staff','resolved:staff'))`

// vendorReputationFigures counts vendorID's figures in one statement (one snapshot), on q: the database for the
// account page, the confirming transaction for vendorAutoApproveAction. It takes no lock.
func vendorReputationFigures(ctx context.Context, q rowQuerier, vendorID string) (ReputationFigures, error) {
	var f ReputationFigures
	err := q.QueryRowContext(ctx, reputationSQL, vendorID,
		disputeOutcomes["refund"], disputeOutcomes["release"], concededByBuyer, concededByVendor).Scan(
		&f.Funded, &f.FundedBuyers, &f.CompletedConfirmed, &f.CompletedDeadline, &f.CompletedDigital, &f.CompletedBuyers,
		&f.NoShipCancels, &f.VendorCancels, &f.DecidedRefund, &f.DecidedRelease, &f.BuyerConceded, &f.VendorConceded,
		&f.Reviews, &f.LowReviews, &f.Takedowns, &f.Rejections, &f.HeldRefunds)
	return f, err
}

// vendorReputation is vendorID's figures assessed by the rule.
func vendorReputation(ctx context.Context, q rowQuerier, vendorID string) (*ReputationView, error) {
	f, err := vendorReputationFigures(ctx, q, vendorID)
	if err != nil {
		return nil, err
	}
	return assessReputation(vendorID, f), nil
}
