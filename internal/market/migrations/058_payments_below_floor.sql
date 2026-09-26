-- A-99: below_floor marks deposits counted for a payout that was not queued because their sum is below the
-- currency's minimum automatic payout (payments_hooks.go payoutFloor). Such deposits are flagged for staff review
-- once (payments.flagged; the marker stops the watcher repeating the flag on later passes) and are listed as open
-- on the payment-review desk. A later payout that includes them (more deposits lift the sum over the floor)
-- clears the marker.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS below_floor boolean NOT NULL DEFAULT false;
