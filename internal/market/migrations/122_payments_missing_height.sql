-- A-554 (PAY66-1/PAY66-2): the escrow wallet's height when a Monero escrow deposit credited toward its order's funding
-- was first found at -1 confirmations (no longer listed after a reorganisation, or read as double-spent). The order's
-- release or refund waits for it for 2 x the confirmation threshold blocks from this height, then is built without it.
-- NULL while the deposit is listed; the escrow watcher clears it when the deposit is listed again, so a later
-- disappearance starts a new wait. Rows already at -1 get a height on the watcher's next pass of their wallet.
ALTER TABLE payments ADD COLUMN missing_height bigint CHECK (missing_height >= 0);
