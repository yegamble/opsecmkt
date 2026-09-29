-- A-31: the outcome a moderator or administrator recorded for a deposit flagged for review (payments.flagged):
-- 'no_action' (disposition_note holds the required reason), 'refunded' (refunded to the buyer by hand, outside the
-- market's payouts: disposition_txid and disposition_amount in atomic units; no payouts row is ever written for
-- it) or 'escalated' (a moderator passed it to an administrator, who then records one of the other two). '' is
-- undisposed. A disposed flag ('no_action' or 'refunded') leaves the payment-review list and no longer keeps the
-- order's late-deposit notice episode open; a deposit refunded by hand never counts toward a payout. A credited
-- deposit that regresses again after it confirmed (a new A-98 episode) is undisposed again. The order history
-- and the audit log keep every disposition. Existing rows keep the default: undisposed.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS disposition text NOT NULL DEFAULT ''
    CHECK (disposition IN ('', 'no_action', 'refunded', 'escalated'));
ALTER TABLE payments ADD COLUMN IF NOT EXISTS disposition_note text NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS disposition_txid text NOT NULL DEFAULT '';
ALTER TABLE payments ADD COLUMN IF NOT EXISTS disposition_amount bigint NOT NULL DEFAULT 0 CHECK (disposition_amount >= 0);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS disposed_at timestamptz;
