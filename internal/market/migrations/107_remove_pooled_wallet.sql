-- A-448: pooled payments are removed for new releases. An older installation may cross this migration only after
-- every pooled drain blocker, including mainnet history, has been resolved. Keep all historical payment records.
DO $$
DECLARE
  blocker_kinds text;
BEGIN
  SELECT string_agg(DISTINCT kind, ', ' ORDER BY kind)
    INTO blocker_kinds
    FROM pooled_drain_blockers();
  IF blocker_kinds IS NOT NULL THEN
    RAISE EXCEPTION 'migration 107 pooled payment removal refused; resolve pooled drain blocker kinds: %. Nothing was changed. Restore the pre-upgrade database, drain it with the previous release, then retry.', blocker_kinds;
  END IF;
END
$$;

-- Preserve an operator pause. The old enabled value meant pooled intake; escrow is the supported replacement.
UPDATE settings
   SET value = 'escrow'
 WHERE key IN ('payment_intake_BTC', 'payment_intake_XMR')
   AND value = 'true';
