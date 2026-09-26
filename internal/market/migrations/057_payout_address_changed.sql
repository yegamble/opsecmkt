-- A-121: when the account's payout address for each currency was last saved or removed. The administrator's
-- "use the account's current address" action on a held or failed payout shows it next to both addresses. NULL:
-- not changed since this column was added (the time was not recorded). Re-running changes nothing.
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_btc_changed timestamptz;
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_xmr_changed timestamptz;
