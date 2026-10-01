-- Bitcoin escrow, Phase 1 slice P1-c2 (WR-escrow A-246, docs/escrow.md): the market's co-signature, the stored final
-- transaction and its broadcast, dispute decisions and the stray refund (ES-1, ES-2, ES-4, ES-8). Nothing writes these
-- while ESCROW_BTC is off, the default; escrow runs on regtest only.
--
-- A party-signed settlement is completed by the escrow watcher: the market's signature on its own stored copy (or
-- both parties' signatures, when both signed; the market then does not sign), finalized and checked, and stored as
-- the raw transaction (state 'signed', cosigned names who completed it) before it is broadcast for the first time.
-- Only that stored transaction is ever broadcast again. It then moves to 'broadcast' and, with enough confirmations,
-- 'confirmed'; one whose escrow outputs another transaction spent is 'external' (spent_by, when known).
ALTER TABLE settlements
 ADD COLUMN raw_tx text CHECK (raw_tx ~ '^([0-9a-f]{2})+$'),
 ADD COLUMN cosigned text CHECK (cosigned IN ('market','parties')),
 ADD COLUMN final_at timestamptz,
 ADD COLUMN broadcast_at timestamptz,
 ADD COLUMN confirmed_at timestamptz,
 ADD COLUMN spent_by text CHECK (spent_by ~ '^[0-9a-f]{64}$'),
 ADD COLUMN send_error text NOT NULL DEFAULT '',
 ADD CONSTRAINT settlements_final CHECK (state NOT IN ('signed','broadcast','confirmed') OR (raw_tx IS NOT NULL AND cosigned IS NOT NULL AND final_at IS NOT NULL)),
 ADD CONSTRAINT settlements_final_only_signed CHECK ((raw_tx IS NULL) = (cosigned IS NULL) AND (raw_tx IS NULL) = (final_at IS NULL)),
 ADD CONSTRAINT settlements_broadcast CHECK (state NOT IN ('broadcast','confirmed') OR broadcast_at IS NOT NULL),
 ADD CONSTRAINT settlements_confirmed CHECK (state <> 'confirmed' OR confirmed_at IS NOT NULL);

-- An administrator may change an escrow dispute's decision until its settlement has a final transaction (ES-4): the
-- settlement of the decision it replaces is then 'superseded' (its outputs are free again), and the one outcome per
-- order counts only settlements that are not superseded.
ALTER TABLE settlements DROP CONSTRAINT settlements_state_check;
ALTER TABLE settlements ADD CONSTRAINT settlements_state_check CHECK (state IN ('building','awaiting_signature','party_signed','signed','broadcast','confirmed','external','failed','superseded'));
ALTER TABLE settlements ADD CONSTRAINT settlements_superseded CHECK (state <> 'superseded' OR (kind IN ('release','refund') AND raw_tx IS NULL));
DROP INDEX settlements_one_outcome;
CREATE UNIQUE INDEX settlements_one_outcome ON settlements (order_id) WHERE kind IN ('release','refund') AND state <> 'superseded';

-- A stray refund returns coins first recorded after the order closed (and outputs a bounded build left out) to the
-- buyer's pinned refund address (ES-8). One may be in progress per order at a time; a later deposit gets another.
CREATE UNIQUE INDEX settlements_one_open_stray ON settlements (order_id) WHERE kind = 'stray_refund' AND state IN ('building','awaiting_signature','party_signed','signed','broadcast');

-- A-342: a claim or refund waits a bounded time for an output recorded before completion to confirm (or for an
-- escrow with more outputs than one settlement spends); then it is built without the outputs it left out, which are
-- marked here and returned by a stray refund.
ALTER TABLE payments ADD COLUMN escrow_left_out boolean NOT NULL DEFAULT false;

-- The buyer's "Recover coins at this escrow" (ES-8): the escrow watcher reads the escrow on its next pass, whatever
-- its age, and requests a stray refund of what it finds.
ALTER TABLE escrows ADD COLUMN recover_requested timestamptz;

-- A settlement is never rebuilt once a party has signed it or it has a final transaction (ES-2). Before the final
-- transaction exists, a party signature that did not verify is discarded (A-339): the signer then changes (to the
-- other party, if it signed too, or to none) and the settlement may return to awaiting a signature. The final
-- transaction, once stored, never changes, and the settlement never goes back to an earlier state.
CREATE OR REPLACE FUNCTION settlements_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.signer IS NOT NULL OR OLD.raw_tx IS NOT NULL OR OLD.state NOT IN ('building','awaiting_signature')) AND (NEW.psbt IS DISTINCT FROM OLD.psbt
      OR NEW.txid IS DISTINCT FROM OLD.txid OR NEW.destination IS DISTINCT FROM OLD.destination OR NEW.fee_rate IS DISTINCT FROM OLD.fee_rate
      OR NEW.network_fee IS DISTINCT FROM OLD.network_fee OR NEW.input_total IS DISTINCT FROM OLD.input_total
      OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.order_id IS DISTINCT FROM OLD.order_id) THEN
    RAISE EXCEPTION 'settlements: settlement % has a party signature; it is never rebuilt', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.signer IS NOT NULL AND NEW.signer IS DISTINCT FROM OLD.signer
      AND NOT (OLD.state = 'party_signed' AND OLD.raw_tx IS NULL AND NEW.state IN ('party_signed','awaiting_signature')) THEN
    RAISE EXCEPTION 'settlements: settlement % has a party signature; its signer changes only when a signature that did not verify is discarded', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.raw_tx IS NOT NULL AND (NEW.raw_tx IS DISTINCT FROM OLD.raw_tx OR NEW.cosigned IS DISTINCT FROM OLD.cosigned
      OR NEW.final_at IS DISTINCT FROM OLD.final_at OR NEW.signer IS DISTINCT FROM OLD.signer
      OR NEW.state IN ('building','awaiting_signature','party_signed','failed','superseded')) THEN
    RAISE EXCEPTION 'settlements: settlement % has a final transaction; it is never rebuilt or changed', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.raw_tx IS NULL AND NEW.raw_tx IS NOT NULL AND NOT (OLD.state = 'party_signed' AND NEW.state = 'signed') THEN
    RAISE EXCEPTION 'settlements: a final transaction is stored only when a party-signed settlement is signed' USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.state = 'superseded' AND NEW.state <> 'superseded' THEN
    RAISE EXCEPTION 'settlements: settlement % was superseded by a changed dispute decision', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

-- A party's signatures are written only while the settlement waits for them, and removed only when they did not
-- verify, before any final transaction exists (A-339); they are never changed.
CREATE FUNCTION settlement_signatures_kept() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
BEGIN
  SELECT state, raw_tx INTO s FROM settlements WHERE id = CASE WHEN TG_OP = 'INSERT' THEN NEW.settlement_id ELSE OLD.settlement_id END;
  IF TG_OP = 'UPDATE' OR (TG_OP = 'INSERT' AND s.state NOT IN ('awaiting_signature','party_signed'))
      OR (TG_OP = 'DELETE' AND (s.state <> 'party_signed' OR s.raw_tx IS NOT NULL)) THEN
    RAISE EXCEPTION 'settlement_signatures: a signature is kept as pasted until it is discarded unverified, before the settlement is final' USING ERRCODE = 'check_violation';
  END IF;
  RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END $$;
CREATE TRIGGER settlement_signatures_kept BEFORE INSERT OR UPDATE OR DELETE ON settlement_signatures FOR EACH ROW EXECUTE FUNCTION settlement_signatures_kept();
