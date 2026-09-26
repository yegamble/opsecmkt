-- A-98: a credited deposit that falls back below the confirmation threshold (conflicted, missing, or at a lower
-- depth after a reorg) is announced once per episode. regress_notice marks the current episode as announced; the
-- watcher clears it when the deposit reaches the threshold again, so a later regression is announced again.
-- payments.flagged stays set once a deposit was reported (staff read access and the payment-review list).
ALTER TABLE payments ADD COLUMN IF NOT EXISTS regress_notice boolean NOT NULL DEFAULT false;
-- Credited deposits already reported as conflicted before this column existed are in an announced episode.
-- Re-running changes nothing.
UPDATE payments SET regress_notice = true WHERE credited AND flagged AND confirmations < 0 AND NOT regress_notice;
-- The watcher's hold reason no longer promises a moderator review (there is no such action); the watcher
-- recognises its own holds by this exact text (payments_hooks.go heldReason), so rewrite held rows.
UPDATE payouts
SET error = 'A credited deposit is conflicted or below the confirmation threshold; the payment watcher holds this payout and releases it by itself once the deposit confirms again.'
WHERE state = 'held'
    AND error = 'A credited deposit is conflicted or below the confirmation threshold; payout held until it confirms again or a moderator reviews it.';
-- tip_height: the highest node tip the watcher has seen for the currency. A pass whose node reports a lower tip
-- (restarted or restored behind it, still catching up) is skipped as syncing, so no deposit is recorded as
-- missing or re-confirming meanwhile. NULL until a provider reports a height.
ALTER TABLE payment_status ADD COLUMN IF NOT EXISTS tip_height bigint;
