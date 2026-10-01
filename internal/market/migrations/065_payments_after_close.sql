-- A-211: after_close marks a deposit first recorded by the watcher while its order was already cancelled,
-- completed or resolved. It is never counted toward the order's settlement (payoutBasis) or refunded
-- automatically: the watcher flags it for staff once it confirms, and the order page shows it as not paid out.
-- Rows recorded before this migration keep the default and their earlier treatment.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS after_close boolean NOT NULL DEFAULT false;
