-- Bitcoin escrow, Phase 1 slice P1-c1 (WR-escrow A-246, docs/escrow.md): what a settlement needs to be built by the
-- server and signed by one escrow party (ES-1, ES-2, ES-4, ES-5). Nothing writes these while ESCROW_BTC is off, the
-- default; escrow runs on regtest only.
--
-- A settlement is requested in state 'building' (a vendor's claim with the vendor's destination, or a refund to the
-- escrow's pinned refund address when a funded escrow order is cancelled) and built by the escrow watcher: explicit
-- confirmed, unspent inputs, the frozen outputs and an explicit fee rate. The build stores the unsigned PSBT and its
-- unsigned txid and moves the row to 'awaiting_signature' by compare-and-set; a party's pasted signatures move it to
-- 'party_signed'. The pasted PSBT itself is never stored: only the party's signatures, one per input.
ALTER TABLE settlements
 ADD COLUMN destination text CHECK (destination ~ '^[A-Za-z0-9]{14,90}$'),
 ADD COLUMN requested_by text REFERENCES users(id),
 ADD COLUMN fee_rate bigint CHECK (fee_rate > 0),
 ADD COLUMN network_fee bigint CHECK (network_fee > 0),
 ADD COLUMN input_total bigint CHECK (input_total > 0),
 ADD COLUMN psbt text CHECK (psbt <> ''),
 ADD COLUMN txid text CHECK (txid ~ '^[0-9a-f]{64}$'),
 ADD COLUMN signer text CHECK (signer IN ('buyer','vendor')),
 ADD COLUMN signed_at timestamptz,
 ADD COLUMN build_error text NOT NULL DEFAULT '',
 ADD CONSTRAINT settlements_built CHECK (state IN ('building','failed') OR (psbt IS NOT NULL AND txid IS NOT NULL AND fee_rate IS NOT NULL
   AND network_fee IS NOT NULL AND input_total IS NOT NULL)),
 ADD CONSTRAINT settlements_party_signed CHECK (state <> 'party_signed' OR (signer IS NOT NULL AND signed_at IS NOT NULL));

-- A vendor's claim pays a fresh address given for it (ES-5): no two claims pay the same destination.
CREATE UNIQUE INDEX settlements_release_destination ON settlements (destination) WHERE kind = 'release';

-- The frozen outputs of each built settlement, in transaction order: the claimant's output (the vendor on a claim,
-- the buyer on a refund) and, on a claim, the market fee and the return of any overpayment to the buyer's refund
-- address. The network fee is taken from the claimant's output only; an output below the floor is folded into it.
CREATE TABLE settlement_outputs (
 settlement_id bigint NOT NULL REFERENCES settlements(id),
 n integer NOT NULL CHECK (n >= 0),
 role text NOT NULL CHECK (role IN ('claimant','market_fee','buyer_return')),
 address text NOT NULL CHECK (address ~ '^[A-Za-z0-9]{14,90}$'),
 sats bigint NOT NULL CHECK (sats > 0),
 PRIMARY KEY (settlement_id, n),
 UNIQUE (settlement_id, role)
);

-- One escrow party's signature on each input of a settlement (ES-2, ES-4), taken from a pasted PSBT that passed
-- every check. The signature ends in its sighash byte, which must be SIGHASH_ALL (0x01).
CREATE TABLE settlement_signatures (
 settlement_id bigint NOT NULL REFERENCES settlements(id),
 input_n integer NOT NULL CHECK (input_n >= 0),
 party text NOT NULL CHECK (party IN ('buyer','vendor')),
 pubkey text NOT NULL CHECK (pubkey ~ '^0[23][0-9a-f]{64}$'),
 signature text NOT NULL CHECK (signature ~ '^30[0-9a-f]{12,142}01$'),
 PRIMARY KEY (settlement_id, input_n, party)
);

-- A settlement is never rebuilt once a party has signed it (ES-2): its transaction, destination, fee and signer
-- are frozen from then on. Its outputs are written only while it is being built and never changed.
CREATE FUNCTION settlements_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.signer IS NOT NULL AND (NEW.psbt IS DISTINCT FROM OLD.psbt OR NEW.txid IS DISTINCT FROM OLD.txid
      OR NEW.destination IS DISTINCT FROM OLD.destination OR NEW.fee_rate IS DISTINCT FROM OLD.fee_rate
      OR NEW.network_fee IS DISTINCT FROM OLD.network_fee OR NEW.input_total IS DISTINCT FROM OLD.input_total
      OR NEW.signer IS DISTINCT FROM OLD.signer OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.order_id IS DISTINCT FROM OLD.order_id) THEN
    RAISE EXCEPTION 'settlements: settlement % has a party signature; it is never rebuilt', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlements_frozen BEFORE UPDATE ON settlements FOR EACH ROW EXECUTE FUNCTION settlements_frozen();

CREATE FUNCTION settlement_outputs_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP <> 'INSERT' OR NOT EXISTS (SELECT 1 FROM settlements WHERE id = NEW.settlement_id AND state = 'building') THEN
    RAISE EXCEPTION 'settlement_outputs: outputs are written only while their settlement is being built' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlement_outputs_frozen BEFORE INSERT OR UPDATE OR DELETE ON settlement_outputs FOR EACH ROW EXECUTE FUNCTION settlement_outputs_frozen();
