-- Review fix 2026-09-23: Monero transfers with a non-zero unlock_time are recorded and reported, never credited.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS locked boolean NOT NULL DEFAULT false;
