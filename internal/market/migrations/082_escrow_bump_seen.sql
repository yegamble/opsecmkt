-- Bitcoin escrow, Phase 2 slice P2-d-fix (WR-escrow A-247, A-382; docs/escrow.md): a fee bump whose transaction is
-- already on the network when the market comes to follow it. Nothing writes these while ESCROW_BTC is off, the default;
-- escrow runs on regtest only.
--
-- Every built fee bump is the market's own transaction for its settlement (it pays the settlement's frozen outputs, only
-- the claimant's smaller), whether or not the market stored its final transaction: the buyer and the vendor may sign
-- the copies they downloaded and broadcast it themselves, or a restored database may hold an older row (waiting for a
-- signature, signed by the claimant, or closed by a newer request) whose transaction was sent after the backup. When
-- the watch-only wallet holds that transaction, the market checks it like any final transaction and records it (A-344
-- for settlements): its final transaction is stored, with cosigned 'parties' when its witnesses carry both parties'
-- stored signatures and 'network' when they do not show who signed it, and the bump moves to signed and then broadcast.
-- This replaces migration 080's settlement_bumps_frozen with that one more way to store a final transaction (from
-- awaiting_signature, party_signed or superseded, never with cosigned 'market'); everything else is unchanged.
ALTER TABLE settlement_bumps DROP CONSTRAINT settlement_bumps_cosigned_check,
 ADD CONSTRAINT settlement_bumps_cosigned_check CHECK (cosigned IN ('market','parties','network'));

CREATE OR REPLACE FUNCTION settlement_bumps_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record; seen boolean;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'settlement_bumps: a fee bump is never deleted' USING ERRCODE = 'check_violation';
  END IF;
  SELECT kind, state INTO s FROM settlements WHERE id = NEW.settlement_id;
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'building' OR NEW.psbt IS NOT NULL OR NEW.txid IS NOT NULL OR NEW.network_fee IS NOT NULL OR NEW.signer IS NOT NULL
        OR NEW.raw_tx IS NOT NULL OR s.state IS DISTINCT FROM 'broadcast' THEN
      RAISE EXCEPTION 'settlement_bumps: a fee bump is requested only for a broadcast settlement, and only the market builds it' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  -- A-382: a built bump's transaction found on the network is stored from any state before a final transaction.
  seen := OLD.raw_tx IS NULL AND NEW.raw_tx IS NOT NULL AND OLD.state IN ('awaiting_signature','party_signed','superseded') AND NEW.state = 'signed'
    AND NEW.cosigned IN ('parties','network') AND s.state = 'broadcast';
  IF NEW.settlement_id IS DISTINCT FROM OLD.settlement_id OR NEW.n IS DISTINCT FROM OLD.n OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
      OR NEW.fee_rate IS DISTINCT FROM OLD.fee_rate OR (OLD.state <> 'building' AND (NEW.psbt IS DISTINCT FROM OLD.psbt
      OR NEW.txid IS DISTINCT FROM OLD.txid OR NEW.network_fee IS DISTINCT FROM OLD.network_fee)) THEN
    RAISE EXCEPTION 'settlement_bumps: fee bump % is never rebuilt', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.state = 'building' AND NEW.state = 'awaiting_signature' AND s.state <> 'broadcast' THEN
    RAISE EXCEPTION 'settlement_bumps: a fee bump is built only while its settlement is broadcast' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.signer IS NOT NULL AND NEW.signer <> settlement_claimant(s.kind) THEN
    RAISE EXCEPTION 'settlement_bumps: a fee bump is signed first by its claimant (the vendor of a release, the buyer of a refund)' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.signer IS DISTINCT FROM OLD.signer AND NOT ((OLD.signer IS NULL AND OLD.state = 'awaiting_signature' AND NEW.state = 'party_signed')
      OR (OLD.state = 'party_signed' AND OLD.raw_tx IS NULL AND NEW.state = 'awaiting_signature' AND NEW.signer IS NULL)) THEN
    RAISE EXCEPTION 'settlement_bumps: fee bump % has its signer; it changes only when a signature that did not verify is discarded', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.raw_tx IS NOT NULL AND (NEW.raw_tx IS DISTINCT FROM OLD.raw_tx OR NEW.cosigned IS DISTINCT FROM OLD.cosigned OR NEW.final_at IS DISTINCT FROM OLD.final_at) THEN
    RAISE EXCEPTION 'settlement_bumps: fee bump % has a final transaction; it never changes', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.raw_tx IS NULL AND NEW.raw_tx IS NOT NULL AND NOT (OLD.state = 'party_signed' AND NEW.state = 'signed' AND s.state = 'broadcast') AND NOT seen THEN
    RAISE EXCEPTION 'settlement_bumps: a final transaction is stored only when a party-signed fee bump of a broadcast settlement is signed, or when a built one is found on the network' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.state IS DISTINCT FROM OLD.state AND NOT seen AND NOT (
       (OLD.state = 'building' AND NEW.state IN ('awaiting_signature','failed','superseded'))
    OR (OLD.state = 'awaiting_signature' AND NEW.state IN ('party_signed','superseded'))
    OR (OLD.state = 'party_signed' AND NEW.state IN ('awaiting_signature','signed','superseded'))
    OR (OLD.state = 'signed' AND NEW.state = 'broadcast')
    OR (OLD.state = 'broadcast' AND NEW.state = 'confirmed')
    OR (OLD.state = 'confirmed' AND NEW.state = 'broadcast')) THEN
    RAISE EXCEPTION 'settlement_bumps: fee bump % cannot move from % to %', OLD.id, OLD.state, NEW.state USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
