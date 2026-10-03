-- Bitcoin escrow, Phase 2 slice P2-d (WR-escrow A-247, P2-R7, A-375; docs/escrow.md): same-recipient fee bumps of a
-- broadcast settlement. Nothing writes these while ESCROW_BTC is off, the default; escrow runs on regtest only.
--
-- A fee bump is a new transaction that replaces a broadcast, unconfirmed settlement's (BIP125): the same inputs and
-- output scripts in the same order, only the claimant's output smaller, so the network fee is higher. It never changes
-- the settlement's own row, whose transaction stays frozen (migrations 075 to 078). The claimant (the vendor of a
-- release, the buyer of a refund or stray refund) asks for it with a fee rate ('building'); the escrow watcher builds it
-- from the settlement's stored transaction ('awaiting_signature'); the claimant signs it first ('party_signed'), then
-- the market co-signs it, or the other party signs it too and the market does not; the final transaction is stored
-- ('signed') before it is first sent ('broadcast') and, when it is mined, 'confirmed' (the settlement is confirmed then,
-- and this row records which transaction it was). A newer request closes an open bump ('superseded'), as does the
-- settlement confirming or becoming external; a bump that cannot be built is 'failed'.
CREATE TABLE settlement_bumps (
 id bigserial PRIMARY KEY,
 settlement_id bigint NOT NULL REFERENCES settlements(id),
 n integer NOT NULL CHECK (n >= 1),
 requested_by text NOT NULL REFERENCES users(id),
 fee_rate bigint NOT NULL CHECK (fee_rate > 0),
 network_fee bigint CHECK (network_fee > 0),
 psbt text CHECK (psbt <> ''),
 txid text UNIQUE CHECK (txid ~ '^[0-9a-f]{64}$'),
 state text NOT NULL DEFAULT 'building' CHECK (state IN ('building','awaiting_signature','party_signed','signed','broadcast','confirmed','failed','superseded')),
 signer text CHECK (signer IN ('buyer','vendor')),
 signed_at timestamptz,
 raw_tx text CHECK (raw_tx ~ '^([0-9a-f]{2})+$'),
 cosigned text CHECK (cosigned IN ('market','parties')),
 build_error text NOT NULL DEFAULT '',
 send_error text NOT NULL DEFAULT '',
 created timestamptz NOT NULL DEFAULT now(),
 final_at timestamptz,
 broadcast_at timestamptz,
 confirmed_at timestamptz,
 updated timestamptz NOT NULL DEFAULT now(),
 UNIQUE (settlement_id, n),
 CONSTRAINT settlement_bumps_built CHECK (state IN ('building','failed','superseded') OR (psbt IS NOT NULL AND txid IS NOT NULL AND network_fee IS NOT NULL)),
 CONSTRAINT settlement_bumps_party_signed CHECK (state <> 'party_signed' OR (signer IS NOT NULL AND signed_at IS NOT NULL)),
 CONSTRAINT settlement_bumps_final CHECK (state NOT IN ('signed','broadcast','confirmed') OR raw_tx IS NOT NULL),
 CONSTRAINT settlement_bumps_final_only_signed CHECK ((raw_tx IS NULL) = (cosigned IS NULL) AND (raw_tx IS NULL) = (final_at IS NULL)),
 CONSTRAINT settlement_bumps_closed_unsent CHECK (state NOT IN ('failed','superseded') OR raw_tx IS NULL),
 CONSTRAINT settlement_bumps_broadcast CHECK (state NOT IN ('broadcast','confirmed') OR broadcast_at IS NOT NULL),
 CONSTRAINT settlement_bumps_confirmed CHECK (state <> 'confirmed' OR confirmed_at IS NOT NULL)
);

-- At most one open fee bump per settlement, and at most one confirmed.
CREATE UNIQUE INDEX settlement_bumps_one_open ON settlement_bumps (settlement_id) WHERE state IN ('building','awaiting_signature','party_signed');
CREATE UNIQUE INDEX settlement_bumps_one_confirmed ON settlement_bumps (settlement_id) WHERE state = 'confirmed';

-- One party's signature on each input of a fee bump, from a pasted PSBT that passed ES-2's checks (the pasted PSBT is
-- never stored). The claimant's comes first; the other party's only once the claimant has signed.
CREATE TABLE settlement_bump_signatures (
 bump_id bigint NOT NULL REFERENCES settlement_bumps(id),
 input_n integer NOT NULL CHECK (input_n >= 0),
 party text NOT NULL CHECK (party IN ('buyer','vendor')),
 pubkey text NOT NULL CHECK (pubkey ~ '^0[23][0-9a-f]{64}$'),
 signature text NOT NULL CHECK (signature ~ '^30[0-9a-f]{12,142}01$'),
 PRIMARY KEY (bump_id, input_n, party)
);

-- The claimant of a settlement kind: the vendor of a release, the buyer of a refund or stray refund.
CREATE FUNCTION settlement_claimant(kind text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE kind WHEN 'release' THEN 'vendor' ELSE 'buyer' END
$$;

-- A fee bump is requested only for a broadcast settlement, unbuilt; it is built only while that settlement is still
-- broadcast and never rebuilt; its signer is always the claimant, and changes only when the claimant's signature did
-- not verify (it is then cleared, before any final transaction); the final transaction is stored only when a
-- party-signed bump of a broadcast settlement is signed and never changes; a bump with a final transaction moves only
-- forward (signed -> broadcast -> confirmed), except confirmed -> broadcast after a reorganisation; failed and
-- superseded are final. A bump is never deleted.
CREATE FUNCTION settlement_bumps_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
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
  IF OLD.raw_tx IS NULL AND NEW.raw_tx IS NOT NULL AND NOT (OLD.state = 'party_signed' AND NEW.state = 'signed' AND s.state = 'broadcast') THEN
    RAISE EXCEPTION 'settlement_bumps: a final transaction is stored only when a party-signed fee bump of a broadcast settlement is signed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
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
CREATE TRIGGER settlement_bumps_frozen BEFORE INSERT OR UPDATE OR DELETE ON settlement_bumps FOR EACH ROW EXECUTE FUNCTION settlement_bumps_frozen();

-- A signature is written only while its fee bump waits for it: the claimant's while the bump awaits a signature, the
-- other party's only once the claimant has signed. It is removed only when a signature did not verify, before any
-- final transaction exists, and never changed.
CREATE FUNCTION settlement_bump_signatures_kept() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b record;
BEGIN
  SELECT sb.state, sb.raw_tx, sb.signer, settlement_claimant(s.kind) AS claimant INTO b FROM settlement_bumps sb JOIN settlements s ON s.id = sb.settlement_id
    WHERE sb.id = CASE WHEN TG_OP = 'INSERT' THEN NEW.bump_id ELSE OLD.bump_id END;
  IF TG_OP = 'UPDATE'
      OR (TG_OP = 'INSERT' AND NOT ((NEW.party = b.claimant AND b.state = 'awaiting_signature')
        OR (NEW.party <> b.claimant AND b.state = 'party_signed' AND b.signer = b.claimant)))
      OR (TG_OP = 'DELETE' AND (b.state <> 'party_signed' OR b.raw_tx IS NOT NULL)) THEN
    RAISE EXCEPTION 'settlement_bump_signatures: the claimant signs a fee bump first; a signature is kept as pasted until it is discarded unverified, before the bump is final' USING ERRCODE = 'check_violation';
  END IF;
  RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END $$;
CREATE TRIGGER settlement_bump_signatures_kept BEFORE INSERT OR UPDATE OR DELETE ON settlement_bump_signatures FOR EACH ROW EXECUTE FUNCTION settlement_bump_signatures_kept();
