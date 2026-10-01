-- Bitcoin escrow, Phase 1 slice P1-d2 (WR-escrow A-246, A-356; docs/escrow.md): a settlement still being built, or
-- waiting for a party's signature, whose escrow deposit another transaction spent. Nothing writes these while
-- ESCROW_BTC is off, the default; escrow runs on regtest only.
--
-- A settlement being built whose input stays spent after the build has waited on it is external without ever having
-- been built: it has no PSBT, transaction ID, fee or amount, and never gets them (migration 077 makes external final).
ALTER TABLE settlements DROP CONSTRAINT settlements_built;
ALTER TABLE settlements ADD CONSTRAINT settlements_built CHECK (state IN ('building','failed')
  OR (state = 'external' AND psbt IS NULL AND txid IS NULL AND fee_rate IS NULL AND network_fee IS NULL AND input_total IS NULL AND signer IS NULL)
  OR (psbt IS NOT NULL AND txid IS NOT NULL AND fee_rate IS NOT NULL AND network_fee IS NOT NULL AND input_total IS NOT NULL));

-- A settlement waiting for a signature whose own transaction the market finds already on the network (the buyer and
-- the vendor signed the copies they downloaded and broadcast it themselves) is recorded as broadcast, as a party-signed
-- one is (A-344): its final transaction may now be stored when it moves from awaiting_signature to signed, as well as
-- from party_signed. Everything else in migration 076's settlements_frozen is unchanged.
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
  IF OLD.raw_tx IS NULL AND NEW.raw_tx IS NOT NULL AND NOT (OLD.state IN ('party_signed','awaiting_signature') AND NEW.state = 'signed') THEN
    RAISE EXCEPTION 'settlements: a final transaction is stored only when a party-signed settlement, or one waiting for a signature whose transaction is on the network, is signed' USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.state = 'superseded' AND NEW.state <> 'superseded' THEN
    RAISE EXCEPTION 'settlements: settlement % was superseded by a changed dispute decision', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
