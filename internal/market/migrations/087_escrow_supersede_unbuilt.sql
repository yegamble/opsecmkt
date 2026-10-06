-- Bitcoin escrow (WR-escrow A-462; docs/escrow.md): a changed dispute decision supersedes the settlement of the decision
-- it replaces while that settlement has no final transaction, including one that was never built: a release or refund
-- still building (waiting for a deposit to confirm, or not yet tried) or one that failed (too few coins, nothing left to
-- spend). Before this migration settlements_built (migration 078) refused a superseded row with no PSBT, so such a
-- change failed and the decision stayed frozen. Nothing writes these while ESCROW_BTC is off, the default; escrow runs
-- on regtest only.
--
-- A superseded settlement either keeps the whole build it had (PSBT, transaction ID, fee rate, network fee, value) or
-- was never built: no PSBT, transaction ID, network fee or signer, and never any later (a failed one keeps the value it
-- could spend and the fee rate it failed at, which migration 075 already allows on a failed row). Every other case of
-- migration 078's settlements_built is unchanged.
ALTER TABLE settlements DROP CONSTRAINT settlements_built;
ALTER TABLE settlements ADD CONSTRAINT settlements_built CHECK (state IN ('building','failed')
  OR (state = 'external' AND psbt IS NULL AND txid IS NULL AND fee_rate IS NULL AND network_fee IS NULL AND input_total IS NULL AND signer IS NULL)
  OR (state = 'superseded' AND psbt IS NULL AND txid IS NULL AND network_fee IS NULL AND signer IS NULL)
  OR (psbt IS NOT NULL AND txid IS NOT NULL AND fee_rate IS NOT NULL AND network_fee IS NOT NULL AND input_total IS NOT NULL));

-- Superseding changes nothing but the state (and when it changed, and why it waited): the row's build, destination and
-- signer are kept as they were, so a settlement waiting for a signature never loses its PSBT on the way to superseded
-- (the check above would otherwise accept a built row stripped to no PSBT) and one being built never gains one. From
-- superseded nothing changes them again (settlements_frozen, migration 078) and the state never changes back.
CREATE FUNCTION settlements_supersede_keeps() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.psbt IS DISTINCT FROM OLD.psbt OR NEW.txid IS DISTINCT FROM OLD.txid OR NEW.fee_rate IS DISTINCT FROM OLD.fee_rate
      OR NEW.network_fee IS DISTINCT FROM OLD.network_fee OR NEW.input_total IS DISTINCT FROM OLD.input_total
      OR NEW.destination IS DISTINCT FROM OLD.destination OR NEW.signer IS DISTINCT FROM OLD.signer THEN
    RAISE EXCEPTION 'settlements: settlement % is superseded as it is: its build, destination and signer do not change', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlements_supersede_keeps BEFORE UPDATE ON settlements FOR EACH ROW
  WHEN (NEW.state = 'superseded' AND OLD.state <> 'superseded') EXECUTE FUNCTION settlements_supersede_keeps();

-- The refund settlement whose too-small failure left a deposit out for a stray refund (leaveOutTooFew, A-435, A-437):
-- when a changed decision supersedes that refund, the deposits it left out count for the order again (the release
-- includes them), and no deposit left out for any other reason does. Set only together with escrow_left_out. Deposits
-- left out by such a failure before this migration have none (see UPGRADING.md).
ALTER TABLE payments
 ADD COLUMN escrow_too_few_by bigint REFERENCES settlements(id),
 ADD CONSTRAINT payments_too_few_left_out CHECK (escrow_too_few_by IS NULL OR escrow_left_out);
