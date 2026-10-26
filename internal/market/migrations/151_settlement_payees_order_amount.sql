-- A-666 (PAY92-1, PAY92-2): the two inputs of a Monero settlement's payees that migration 150 left unchecked.
--
-- 1. orders.amount never changes once the order has an escrow (orders_amount_frozen; the precedents are
--    orders_escrow_currency, 084, and order_quantity_frozen, 140). Migration 150's market_fee and buyer_return rules
--    follow the order's current amount, so a changed amount moved the vendor's share into the market fee or the buyer
--    return. Checkout writes the amount (escrow_xmr_create.go, escrow_create.go) while the order is a draft, before it
--    inserts the escrow, in the same transaction; nothing writes it later. Writing the same value is not refused.
-- 2. A refund or stray refund pays the escrow's refund_address (xmr_settlements_refund_destination; AFTER INSERT, so the
--    table's own CHECK, key and BEFORE trigger refusals are reported first, as 150's output rule does). Every insert
--    site already writes it: escrow_settle.go and escrow_xmr_settle.go select escrows.refund_address, and a re-request
--    (escrow_xmr_rerequest.go) copies the destination of the settlement it supersedes, which 084 requires and which
--    this rule checked when that row was inserted. 084 freezes a settlement's kind and destination and 150 the
--    escrow's refund address, so an insert is the only place to check. The claimant output (150) pays the destination.
--
-- Rows written before this migration are not checked again. pg_restore creates triggers after it loads the data
-- (post-data), so restoring a backup (scripts/restore.sh) loads existing rows unchecked, as before; scripts/restore.sh
-- itself writes neither column. New functions and triggers only: no column, row, index or existing function changes.
CREATE FUNCTION orders_amount_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.amount IS DISTINCT FROM OLD.amount AND EXISTS (SELECT 1 FROM escrows WHERE order_id = OLD.id) THEN
    RAISE EXCEPTION 'orders: order % has an escrow; its amount, which the market fee and any overpayment return follow, never changes', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER orders_amount_frozen BEFORE UPDATE OF amount ON orders FOR EACH ROW EXECUTE FUNCTION orders_amount_frozen();

CREATE FUNCTION xmr_settlements_refund_destination() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind IN ('refund', 'stray_refund')
      AND NEW.destination IS DISTINCT FROM (SELECT refund_address FROM escrows WHERE order_id = NEW.order_id) THEN
    RAISE EXCEPTION 'xmr_settlements: a % of order % pays only the refund address its escrow recorded at checkout', NEW.kind, NEW.order_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER xmr_settlements_refund_destination AFTER INSERT ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_refund_destination();
