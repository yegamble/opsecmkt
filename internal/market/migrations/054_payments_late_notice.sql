-- A-7: late_notice marks a deposit first recorded while its order was funded and not yet terminal (paid, shipped,
-- delivered, disputed); the watcher announces it once to the order history, the buyer and the vendor, then clears it.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS late_notice boolean NOT NULL DEFAULT false;
