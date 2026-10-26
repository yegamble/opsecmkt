-- A-652a (PAY87-1, PAY89-6; owner decision 2026-10-25 "refund + hold, no new payee"): a Monero settlement pays only
-- the addresses its order already recorded. Two database rules, enforced from this migration on (rows written before it
-- are not re-checked):
--
-- 1. Each xmr_settlement_outputs row pays its role's recorded address. The claimant is the settlement's destination
--    (the vendor's claim address of a release, the pinned refund address of a refund or stray refund; xmr_settlements
--    already keeps the destination, 084, and a re-request keeps the one it supersedes). The buyer_return of a release is
--    the escrow's refund_address. The market_fee is the escrow's fee_destination, except in the one path that replaces
--    it: when the wallet refused that address, the settlement records the wait class destination-refused-market_fee in
--    send_error, and the escrow watcher, once the operator has configured another ESCROW_XMR_FEE_ADDRESS that the wallet
--    accepts, pays the market fee there instead (escrow_xmr_settle.go, buildXMRSettlement). That replacement is read from
--    the process environment and is persisted nowhere before its output row, so the database cannot name it: the rule
--    allows a different market_fee address only while the settlement carries that class, or when the settlement is a
--    re-request whose superseded settlement's market_fee output already paid that address (the stored plan it reuses).
--    The trigger runs AFTER INSERT, so the table's own CHECK and key refusals are reported first, as before.
-- 2. escrows.refund_address and escrows.fee_destination never change once the escrow exists. No application path,
--    script or earlier migration updates either (checkout inserts both; retention deletes the row whole), so nothing
--    legitimate is refused.
--
-- New functions and triggers only: no column, row, index, kind, output role or existing function changes.
CREATE FUNCTION xmr_settlement_outputs_payee() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
BEGIN
  SELECT x.destination, x.send_error, x.supersedes, e.refund_address, e.fee_destination INTO s
    FROM xmr_settlements x JOIN escrows e ON e.order_id = x.order_id WHERE x.id = NEW.settlement_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: settlement % has no escrow', NEW.settlement_id USING ERRCODE = 'check_violation';
  END IF;
  IF (NEW.role = 'claimant' AND NEW.address IS DISTINCT FROM s.destination)
      OR (NEW.role = 'buyer_return' AND NEW.address IS DISTINCT FROM s.refund_address)
      OR (NEW.role = 'market_fee' AND NEW.address IS DISTINCT FROM s.fee_destination
        AND s.send_error IS DISTINCT FROM 'destination-refused-market_fee'
        AND NOT EXISTS (SELECT 1 FROM xmr_settlement_outputs o WHERE o.settlement_id = s.supersedes AND o.role = 'market_fee' AND o.address = NEW.address)) THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: the % output of settlement % pays only its recorded address (the claimant the settlement''s destination, the buyer return the escrow''s refund address, the market fee the escrow''s fee destination or its recorded replacement)', NEW.role, NEW.settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER xmr_settlement_outputs_payee AFTER INSERT ON xmr_settlement_outputs FOR EACH ROW EXECUTE FUNCTION xmr_settlement_outputs_payee();

CREATE FUNCTION escrows_payees_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.refund_address IS DISTINCT FROM OLD.refund_address OR NEW.fee_destination IS DISTINCT FROM OLD.fee_destination THEN
    RAISE EXCEPTION 'escrows: escrow % keeps the refund address and fee destination recorded at checkout', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER escrows_payees_frozen BEFORE UPDATE ON escrows FOR EACH ROW EXECUTE FUNCTION escrows_payees_frozen();
