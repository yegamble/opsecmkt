-- Monero escrow, Phase 3 slice X-d (WR-escrow A-248, P3-R6, A-401, A-415; docs/escrow.md): typed storage for what the
-- Monero escrow watcher records about a settlement besides its state. Nothing writes these while ESCROW_XMR is off,
-- the default; Monero escrow runs on a local fakechain only.
--
-- xmr_settlements.send_error: why a settlement is not built or sent yet, why it failed, or that a restore holds it, as
-- a fixed class, never text from a node (Bitcoin escrow's settlements.send_error, migration 076; a restore's hold
-- writes restoredSettlementHold here, as it does there). The order page states it, and a class staff must see is
-- flagged once, when it is first recorded. It is not part of the stored transaction, so migration 084's freeze
-- (xmr_settlements_frozen) lets it change on a built, relayed or confirmed settlement.
ALTER TABLE xmr_settlements ADD COLUMN send_error text NOT NULL DEFAULT '';

-- xmr_settlement_rerequests: an administrator's re-request of a failed Monero settlement (owner decision O-1, A-401).
-- The administrator records it from the order page with a password step-up (and an audit row); the escrow watcher
-- decides it, because the checks need the daemon: refused, with a fixed reason, while the failed settlement's stored
-- transaction is in the daemon's pool (in-pool) or chain (on-chain) or one of its key images is spent
-- (key-image-spent); otherwise done, in the transaction that supersedes the failed settlement with a new one
-- (new_settlement_id, which names it in supersedes, migration 084). A request is recorded only for a failed settlement
-- with a stored transaction, at most one is open per settlement, and its state moves only forward: requested ->
-- refused or done, never changed again. A refused settlement may be re-requested again later, in a new row. Rows are
-- never deleted (escrow_records_kept, migration 084).
CREATE TABLE xmr_settlement_rerequests (
 id bigserial PRIMARY KEY,
 settlement_id bigint NOT NULL REFERENCES xmr_settlements(id),
 requested_by text NOT NULL REFERENCES users(id),
 requested_at timestamptz NOT NULL DEFAULT now(),
 state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested','refused','done')),
 reason text NOT NULL DEFAULT '' CHECK (reason IN ('','in-pool','on-chain','key-image-spent')),
 decided_at timestamptz,
 new_settlement_id bigint UNIQUE REFERENCES xmr_settlements(id),
 CONSTRAINT xmr_settlement_rerequests_decided CHECK ((state = 'requested') = (decided_at IS NULL)),
 CONSTRAINT xmr_settlement_rerequests_reason CHECK ((state = 'refused') = (reason <> '')),
 CONSTRAINT xmr_settlement_rerequests_done CHECK ((state = 'done') = (new_settlement_id IS NOT NULL))
);
CREATE UNIQUE INDEX xmr_settlement_rerequests_one_open ON xmr_settlement_rerequests (settlement_id) WHERE state = 'requested';

CREATE FUNCTION xmr_settlement_rerequests_moves() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'requested' OR NOT EXISTS (SELECT 1 FROM xmr_settlements WHERE id = NEW.settlement_id AND state = 'failed' AND tx_hash IS NOT NULL) THEN
      RAISE EXCEPTION 'xmr_settlement_rerequests: a re-request is recorded as requested, for a failed settlement with a stored transaction'
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.state <> 'requested' OR NEW.state = 'requested' OR NEW.settlement_id IS DISTINCT FROM OLD.settlement_id
      OR NEW.requested_by IS DISTINCT FROM OLD.requested_by OR NEW.requested_at IS DISTINCT FROM OLD.requested_at THEN
    RAISE EXCEPTION 'xmr_settlement_rerequests: re-request % moves only from requested to refused or done, once, and keeps its settlement and requester', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.state = 'done' AND NOT EXISTS (SELECT 1 FROM xmr_settlements WHERE id = NEW.new_settlement_id AND supersedes = NEW.settlement_id) THEN
    RAISE EXCEPTION 'xmr_settlement_rerequests: re-request % is done only by the settlement that supersedes settlement %', OLD.id, OLD.settlement_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlement_rerequests_moves BEFORE INSERT OR UPDATE ON xmr_settlement_rerequests FOR EACH ROW EXECUTE FUNCTION xmr_settlement_rerequests_moves();
CREATE TRIGGER xmr_settlement_rerequests_not_deleted BEFORE DELETE ON xmr_settlement_rerequests FOR EACH ROW EXECUTE FUNCTION escrow_records_kept();
