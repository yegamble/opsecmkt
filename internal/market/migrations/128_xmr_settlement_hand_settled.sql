-- A-613 (PAY560-2, PAY613-1/-3). Two kinds of failed Monero escrow settlement can never leave state failed, so the
-- escrow staff alert they raised (A-560) never cleared: the staff badge, the moderation desk and retention's
-- escrow_alert blocker kept the order for good. An administrator who is not a party to the order closes such an alert
-- here (the order page's form, POST /admin/escrow-xmr-settled-by-hand) with a note of 10-500 characters, the time and
-- the administrator, at most once per settlement; the kind says which case it was, and is fixed by the settlement:
--   * nothing-owed: one that failed before any transaction was built (tx_hash NULL: nothing-to-settle, no deposit
--     counted when it was to be built), which is never re-requested (xmr_settlements_moves, migration 084). Nothing was
--     sent and nothing is owed for it now; the record only dismisses the alert. It changes nothing the market does: a
--     deposit of the escrow listed again is handled automatically as any deposit is (on an order whose coins go back to
--     the buyer, a stray refund returns it) and is flagged for review, so staff never pay it by hand. A refund too few
--     (send_error too-small) raises no alert at all and is not recorded here. Nor is one that still claims a deposit
--     (payments.xmr_settlement_id: a re-request that failed nothing-to-settle after taking over the deposits of the
--     settlement it supersedes, PAY613-4, A-614): its coins are in the escrow wallet and may be owed, so it keeps its
--     alert until staff settle with the operator.
--   * settled-by-hand: one with a stored transaction whose last re-request was refused for good (on-chain: that
--     transaction is in the chain; key-image-spent: a coin it spends is spent), so it is never superseded; staff have
--     checked the wallet and the chain and settled anything still owed with the operator. A refusal in-pool is not
--     final (the transaction may still be mined or dropped) and is re-requested again instead. A settlement recorded
--     here is never re-requested again.
-- The record is for staff only and is never changed; it is deleted only with the rest of the order's escrow records by
-- a certified retention retirement (migration 114's permit).
CREATE TABLE xmr_settlement_hand_settled (
 settlement_id bigint PRIMARY KEY REFERENCES xmr_settlements(id),
 kind text NOT NULL CHECK (kind IN ('nothing-owed','settled-by-hand')),
 note text NOT NULL CHECK (char_length(note) BETWEEN 10 AND 500),
 settled_by text NOT NULL REFERENCES users(id),
 settled_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION xmr_settlement_hand_settled_kept() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE order_key text;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NOT EXISTS (SELECT 1 FROM xmr_settlements s WHERE s.id = NEW.settlement_id AND s.state = 'failed' AND (
        NEW.kind = 'nothing-owed' AND s.tx_hash IS NULL AND NOT (s.kind = 'refund' AND s.send_error = 'too-small')
          AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.xmr_settlement_id = s.id)
        OR NEW.kind = 'settled-by-hand' AND s.tx_hash IS NOT NULL AND (SELECT r.state || ' ' || r.reason FROM xmr_settlement_rerequests r
          WHERE r.settlement_id = s.id ORDER BY r.id DESC LIMIT 1) IN ('refused on-chain', 'refused key-image-spent'))) THEN
      RAISE EXCEPTION 'xmr_settlement_hand_settled: only a failed Monero settlement never built (other than a refund too few, as nothing-owed), or one whose last re-request was refused on-chain or key-image-spent (as settled-by-hand), is recorded'
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' THEN
    SELECT order_id INTO order_key FROM xmr_settlements WHERE id = OLD.settlement_id;
    IF order_key IS NOT NULL AND retention_escrow_delete_allowed(order_key) THEN
      RETURN OLD;
    END IF;
  END IF;
  RAISE EXCEPTION 'xmr_settlement_hand_settled: a record of settlement % is never changed, and deleted only by a certified exact-order retirement', OLD.settlement_id
    USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER xmr_settlement_hand_settled_kept BEFORE INSERT OR UPDATE OR DELETE ON xmr_settlement_hand_settled FOR EACH ROW EXECUTE FUNCTION xmr_settlement_hand_settled_kept();

CREATE FUNCTION xmr_settlement_rerequests_not_hand_settled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM xmr_settlement_hand_settled WHERE settlement_id = NEW.settlement_id) THEN
    RAISE EXCEPTION 'xmr_settlement_rerequests: settlement % was marked settled by hand and is never re-requested', NEW.settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlement_rerequests_not_hand_settled BEFORE INSERT ON xmr_settlement_rerequests FOR EACH ROW EXECUTE FUNCTION xmr_settlement_rerequests_not_hand_settled();

-- A recorded settlement never takes a deposit claim (the market claims deposits only for a requested settlement it
-- builds, or a re-request's new one), so a record keeps meaning what it meant when it was written and the escrow
-- staff alert can read the record alone (xmrFailedClosedSQL).
CREATE FUNCTION payments_claim_not_hand_settled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM xmr_settlement_hand_settled WHERE settlement_id = NEW.xmr_settlement_id) THEN
    RAISE EXCEPTION 'payments: settlement % was recorded in xmr_settlement_hand_settled and claims no deposit', NEW.xmr_settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payments_claim_not_hand_settled BEFORE INSERT OR UPDATE OF xmr_settlement_id ON payments FOR EACH ROW
  WHEN (NEW.xmr_settlement_id IS NOT NULL) EXECUTE FUNCTION payments_claim_not_hand_settled();
