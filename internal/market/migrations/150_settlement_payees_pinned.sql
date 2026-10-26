-- A-652a (PAY87-1, PAY89-6, PAY91-1; owner decision 2026-10-25 "refund + hold, no new payee"): a Monero settlement's
-- recorded plan pays only the addresses its order recorded, and only the amounts planXMRSettlement (escrow_xmr_settle.go)
-- gives them. Three database rules, enforced from this migration on (rows written before it are not checked again):
--
-- 1. Each new xmr_settlement_outputs row (xmr_settlement_outputs_payee; AFTER INSERT, so the table's own CHECK and key
--    refusals are reported first, as before):
--    - only a release has a market_fee or buyer_return output; a refund or stray refund pays its claimant alone;
--    - the claimant pays the settlement's destination (the vendor's claim address of a release, the pinned refund
--      address of a refund or stray refund; a re-request keeps the destination it supersedes, 084);
--    - a re-request (supersedes set) builds the stored plan of the settlement it supersedes (xmrStoredPlan): its
--      market_fee or buyer_return output repeats that settlement's output of the same role, address and amount;
--    - otherwise a buyer_return pays the escrow's refund_address, and a market_fee pays exactly
--      marketFeeSats(orders.amount, escrows.fee_bps) (escrow_settle.go: amount/10000*fee_bps + amount%10000*fee_bps/10000
--      piconero) to the escrow's fee_destination.
--    One path pays the market fee elsewhere: when the wallet refused the recorded fee address, the settlement's
--    send_error is the wait class destination-refused-market_fee, and the escrow watcher, once the operator has
--    configured another ESCROW_XMR_FEE_ADDRESS that the wallet accepts, pays the fee there (buildXMRSettlement). That
--    address is read from the process environment and recorded nowhere before its output row, so the database cannot
--    name it: while a settlement that is not a re-request carries that class, its market_fee may pay any address. The
--    amount rule still holds then, and only a release has a market_fee at all.
-- 2. When a settlement is built (requested to built: tx_hash first stored; xmr_settlements_plan_fits, AFTER UPDATE) and
--    it has outputs:
--    - it has a claimant output, and its outputs and network fee add up to its input total (the claimant's row is after
--      the network fee, which the wallet takes from that output alone);
--    - a re-request has its superseded settlement's input total and as many outputs;
--    - a release that is not a re-request has a market_fee output exactly when the escrow has a fee destination and the
--      fee reaches the output floor, and a buyer_return output exactly when its input total exceeds orders.amount by at
--      least the floor, of exactly that excess. The floor is minPayoutXMR (escrow_amounts.go), 1000000000 piconero; a
--      fee or excess below it is folded into the claimant's output.
--    The claimant's output is therefore the input total less the network fee and the outputs above. The database does
--    not bound the network fee (ESCROW_XMR_MAX_FEE is configuration, applied by checkXMRBuild), does not tie the input
--    total to the deposits the same transaction claims after it, and cannot read the stored transaction: checkXMRBuild
--    compares the wallet's transaction with the plan before it is stored. A build stored without any output row is not
--    refused (the watcher always writes the plan first; tests store bare builds).
-- 3. escrows.refund_address, escrows.fee_destination and escrows.fee_bps never change once the escrow exists. No
--    application path, script or earlier migration updates them (checkout inserts them; retention deletes the row whole).
--
-- orders.amount is not frozen: the application writes it only while the order is a draft, before its escrow exists, and
-- the fee and excess above follow its current value. New functions and triggers only: no column, row, index, kind,
-- output role or existing function changes.
CREATE FUNCTION xmr_settlement_outputs_payee() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
BEGIN
  SELECT x.kind, x.destination, x.send_error, x.supersedes, e.refund_address, e.fee_destination,
      o.amount / 10000 * e.fee_bps + o.amount % 10000 * e.fee_bps / 10000 AS market_fee INTO s
    FROM xmr_settlements x JOIN escrows e ON e.order_id = x.order_id JOIN orders o ON o.id = x.order_id WHERE x.id = NEW.settlement_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: settlement % has no escrow', NEW.settlement_id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.role IN ('market_fee', 'buyer_return') AND s.kind <> 'release' THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: a % has no % output; only a release pays a market fee or returns an overpayment', s.kind, NEW.role
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.role = 'claimant' AND NEW.address IS DISTINCT FROM s.destination THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: the claimant output of settlement % pays only its recorded address, the settlement''s destination', NEW.settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.role = 'claimant' THEN
    RETURN NULL;
  END IF;
  IF s.supersedes IS NOT NULL THEN
    IF NOT EXISTS (SELECT 1 FROM xmr_settlement_outputs p WHERE p.settlement_id = s.supersedes AND p.role = NEW.role AND p.address = NEW.address AND p.amount = NEW.amount) THEN
      RAISE EXCEPTION 'xmr_settlement_outputs: the % output of re-request % repeats the stored plan of settlement %, address and amount', NEW.role, NEW.settlement_id, s.supersedes
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
  END IF;
  IF NEW.role = 'market_fee' AND NEW.amount IS DISTINCT FROM s.market_fee THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: the market fee of settlement % is % piconero, the fee recorded at checkout, not %', NEW.settlement_id, s.market_fee, NEW.amount
      USING ERRCODE = 'check_violation';
  END IF;
  IF (NEW.role = 'buyer_return' AND NEW.address IS DISTINCT FROM s.refund_address)
      OR (NEW.role = 'market_fee' AND NEW.address IS DISTINCT FROM s.fee_destination AND s.send_error IS DISTINCT FROM 'destination-refused-market_fee') THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: the % output of settlement % pays only its recorded address (the buyer return the escrow''s refund address, the market fee the escrow''s fee destination)', NEW.role, NEW.settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER xmr_settlement_outputs_payee AFTER INSERT ON xmr_settlement_outputs FOR EACH ROW EXECUTE FUNCTION xmr_settlement_outputs_payee();

CREATE FUNCTION xmr_settlements_plan_fits() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  min_output CONSTANT bigint := 1000000000; -- minPayoutXMR, escrow_amounts.go
  p record;
  q record;
BEGIN
  SELECT count(*) AS n, sum(amount) AS total, bool_or(role = 'claimant') AS claimant, bool_or(role = 'market_fee') AS market_fee,
      max(amount) FILTER (WHERE role = 'buyer_return') AS buyer_return
    INTO p FROM xmr_settlement_outputs WHERE settlement_id = NEW.id;
  IF p.n = 0 THEN
    RETURN NULL;
  END IF;
  IF NOT p.claimant OR p.total + NEW.fee <> NEW.input_total THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % is built with a claimant output, and its outputs (% piconero) and network fee (%) add up to its input total (%)', NEW.id, p.total, NEW.fee, NEW.input_total
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.supersedes IS NOT NULL THEN
    SELECT x.input_total, (SELECT count(*) FROM xmr_settlement_outputs o WHERE o.settlement_id = x.id) AS n INTO q FROM xmr_settlements x WHERE x.id = NEW.supersedes;
    IF q.input_total IS DISTINCT FROM NEW.input_total OR q.n <> p.n THEN
      RAISE EXCEPTION 'xmr_settlements: re-request % spends the input total of settlement % to its stored plan', NEW.id, NEW.supersedes
        USING ERRCODE = 'check_violation';
    END IF;
  ELSIF NEW.kind = 'release' THEN
    SELECT o.amount AS price, e.fee_destination, o.amount / 10000 * e.fee_bps + o.amount % 10000 * e.fee_bps / 10000 AS fee INTO q
      FROM orders o JOIN escrows e ON e.order_id = o.id WHERE o.id = NEW.order_id;
    IF p.market_fee IS DISTINCT FROM (q.fee_destination <> '' AND q.fee >= min_output) THEN
      RAISE EXCEPTION 'xmr_settlements: release % has a market fee output exactly when its escrow has a fee destination and its fee of % piconero reaches the output floor', NEW.id, q.fee
        USING ERRCODE = 'check_violation';
    END IF;
    IF (p.buyer_return IS NOT NULL) IS DISTINCT FROM (NEW.input_total - q.price >= min_output)
        OR (p.buyer_return IS NOT NULL AND p.buyer_return <> NEW.input_total - q.price) THEN
      RAISE EXCEPTION 'xmr_settlements: release % returns its overpayment (% piconero, its input total less the order amount) exactly when it reaches the output floor', NEW.id, NEW.input_total - q.price
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER xmr_settlements_plan_fits AFTER UPDATE OF tx_hash ON xmr_settlements FOR EACH ROW WHEN (OLD.tx_hash IS NULL AND NEW.tx_hash IS NOT NULL)
  EXECUTE FUNCTION xmr_settlements_plan_fits();

CREATE FUNCTION escrows_payees_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.refund_address IS DISTINCT FROM OLD.refund_address OR NEW.fee_destination IS DISTINCT FROM OLD.fee_destination
      OR NEW.fee_bps IS DISTINCT FROM OLD.fee_bps THEN
    RAISE EXCEPTION 'escrows: escrow % keeps the refund address, fee destination and fee rate recorded at checkout', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER escrows_payees_frozen BEFORE UPDATE ON escrows FOR EACH ROW EXECUTE FUNCTION escrows_payees_frozen();
