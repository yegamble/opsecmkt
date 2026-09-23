-- A failed payout whose wallet call ended without a definite answer (timeout, reset connection, unreadable
-- reply) may already have been broadcast; requeueing it needs the administrator's explicit confirmation.
-- A JSON-RPC error from the wallet is a definite rejection (false). Failures recorded before this column
-- existed were never classified, so they are treated as ambiguous. Re-running changes nothing.
ALTER TABLE payouts ADD COLUMN IF NOT EXISTS send_ambiguous boolean;
UPDATE payouts SET send_ambiguous = (state = 'failed') WHERE send_ambiguous IS NULL;
ALTER TABLE payouts ALTER COLUMN send_ambiguous SET DEFAULT false, ALTER COLUMN send_ambiguous SET NOT NULL;
