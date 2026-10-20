-- A-613 (PAY560-2). A Monero escrow settlement that failed before any transaction was built (tx_hash NULL) can never
-- be re-requested (xmr_settlements_moves, migration 084, lets a failed settlement become superseded only when it has a
-- stored transaction), so the escrow staff alert it raised (A-560) never cleared: the staff badge, the moderation
-- desk and retention's escrow_alert blocker kept the order for good. A refund too few (send_error too-small) is not
-- such an alert at all (its deposits are left out, flagged for payment review and returned by a stray refund, as a
-- Bitcoin refund too few is); any other unbuilt failure (nothing-to-settle) stays an alert until an administrator who
-- is not a party to the order records here that what it owed was settled by hand with the operator (the order page's
-- form, POST /admin/escrow-xmr-settled-by-hand): a note of 10-500 characters, the time and the administrator, at most
-- once per settlement. The record is for staff only and is never changed; it is deleted only with the rest of the
-- order's escrow records by a certified retention retirement (migration 114's permit).
CREATE TABLE xmr_settlement_hand_settled (
 settlement_id bigint PRIMARY KEY REFERENCES xmr_settlements(id),
 note text NOT NULL CHECK (char_length(note) BETWEEN 10 AND 500),
 settled_by text NOT NULL REFERENCES users(id),
 settled_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION xmr_settlement_hand_settled_kept() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE order_key text;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NOT EXISTS (SELECT 1 FROM xmr_settlements WHERE id = NEW.settlement_id AND state = 'failed' AND tx_hash IS NULL
        AND NOT (kind = 'refund' AND send_error = 'too-small')) THEN
      RAISE EXCEPTION 'xmr_settlement_hand_settled: only a failed Monero settlement without a stored transaction, other than a refund too few, is marked settled by hand'
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
