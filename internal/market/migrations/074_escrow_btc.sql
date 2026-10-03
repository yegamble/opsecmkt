-- Bitcoin escrow, Phase 1 (WR-escrow A-246, docs/escrow.md): the tables Phase 1 needs, created together so the
-- phase adds no corrective migration. Nothing reads or writes them while ESCROW_BTC is off, the default; this
-- release runs escrow on regtest only. Which slice of Phase 1 uses each table is noted with it: P1-a keys and
-- practice signing, P1-b escrow creation and funding, P1-c settlement.

-- P1-a. One Bitcoin escrow key per account (ES-3): an extended public key with its origin. The key is sealed with
-- the keyRoot sealer (label "escrow-xpub"); xpub_hmac is an HMAC of its chain code and public key under a keyRoot
-- derived key, and UNIQUE refuses the same key on two accounts. A key is inactive until its owner signs the
-- practice transaction (ES-9); active_at is the only thing that records. Changing the key writes a new row value
-- and resets active_at; escrows keep the keys they were built with (escrow_pubkeys).
CREATE TABLE user_escrow_keys (
 user_id text PRIMARY KEY REFERENCES users(id),
 xpub_sealed text NOT NULL,
 xpub_hmac text NOT NULL UNIQUE CHECK (xpub_hmac ~ '^[0-9a-f]{64}$'),
 origin_fingerprint text NOT NULL CHECK (origin_fingerprint ~ '^[0-9a-f]{8}$'),
 origin_path text NOT NULL CHECK (origin_path ~ '^(/(0|[1-9][0-9]{0,9})h?){1,10}$'),
 active_at timestamptz,
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now()
);

-- P1-b. One escrow per order. market_index is the arbiter key's child index for this escrow, taken from the
-- escrow_market_index counter (a sequence never hands out a value twice); the address is unique. At checkout the
-- buyer's refund address is pinned (unique: the market refuses an address it already used as an escrow, settlement
-- or fee destination, ES-5) and the market fee is snapshotted: fee_bps (basis points) and fee_destination ('' when
-- the fee is 0). underpaid_noted records when the parties were told the payment window ended short of the price:
-- the refund of an escrow is a settlement, which P1-c builds.
CREATE SEQUENCE escrow_market_index AS bigint MINVALUE 0 START WITH 0;
CREATE TABLE escrows (
 id bigserial PRIMARY KEY,
 order_id text NOT NULL UNIQUE REFERENCES orders(id),
 address text NOT NULL UNIQUE,
 market_index bigint NOT NULL UNIQUE CHECK (market_index >= 0),
 refund_address text NOT NULL UNIQUE CHECK (refund_address <> '' AND refund_address <> address),
 fee_bps integer NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
 fee_destination text NOT NULL CHECK (fee_bps = 0 OR fee_destination <> ''),
 underpaid_noted timestamptz,
 created timestamptz NOT NULL DEFAULT now()
);

-- An escrow order is settled from its escrow, never by a pooled payout (ES-1, invariant 4): the database refuses a
-- payouts row for an order that has an escrow. The application never writes one (settleTerminal); this backs it.
CREATE FUNCTION payouts_not_escrow() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM escrows WHERE order_id = NEW.order_id) THEN
    RAISE EXCEPTION 'payouts: order % is an escrow order; it is settled from its escrow, never by a payout', NEW.order_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payouts_not_escrow BEFORE INSERT OR UPDATE OF order_id ON payouts FOR EACH ROW EXECUTE FUNCTION payouts_not_escrow();

-- P1-b. The three child public keys of each escrow (ES-3): a key is never used by two escrows (PRIMARY KEY). Per
-- party, its own origin: the account key's fingerprint and path and the full derivation suffix below it (e.g.
-- /0/7, not a bare index, A-245), and an optional per-order extended key for a later wallet layout (A-255; NULL in
-- Phase 1). A party key's /0/0 child is its practice key and never an escrow key (ES-9). Every path here is
-- canonical, as the Go parser writes it: no leading zeros and h for hardened, so /0/0 has no other spelling.
CREATE TABLE escrow_pubkeys (
 pubkey text PRIMARY KEY CHECK (pubkey ~ '^0[23][0-9a-f]{64}$'),
 escrow_id bigint NOT NULL REFERENCES escrows(id),
 party text NOT NULL CHECK (party IN ('buyer','vendor','market')),
 user_id text REFERENCES users(id),
 key_hmac text CHECK (key_hmac ~ '^[0-9a-f]{64}$'),
 origin_fingerprint text NOT NULL CHECK (origin_fingerprint ~ '^[0-9a-f]{8}$'),
 origin_path text NOT NULL CHECK (origin_path ~ '^(/(0|[1-9][0-9]{0,9})h?){1,10}$'),
 derivation_suffix text NOT NULL CHECK (derivation_suffix ~ '^(/(0|[1-9][0-9]{0,9})h?){1,10}$'),
 per_order_xpub text,
 UNIQUE (escrow_id, party),
 CHECK ((party = 'market') = (user_id IS NULL) AND (party = 'market') = (key_hmac IS NULL)),
 CHECK (party = 'market' OR derivation_suffix <> '/0/0')
);
CREATE INDEX escrow_pubkeys_key ON escrow_pubkeys (key_hmac) WHERE key_hmac IS NOT NULL;
-- Which escrows use an account's key is read by account and origin, not by key_hmac: an APP_SECRET_KEY rotation
-- changes every HMAC, and must never hide an escrow or change which key it uses (chair's ruling on QA25).
CREATE INDEX escrow_pubkeys_owner ON escrow_pubkeys (user_id, origin_fingerprint, origin_path) WHERE user_id IS NOT NULL;

-- P1-c. Settlements of escrows (ES-1): no new order states; each settlement moves by compare-and-set through
-- building -> awaiting_signature -> party_signed -> signed -> broadcast -> confirmed | external | failed. At most
-- one release or refund per order; a stray refund (coins sent after the order closed) is its own kind. The
-- payments row of each spent output names its settlement, claimed by compare-and-set, so no output is claimed by
-- two settlements.
CREATE TABLE settlements (
 id bigserial PRIMARY KEY,
 order_id text NOT NULL REFERENCES orders(id),
 kind text NOT NULL CHECK (kind IN ('release','refund','stray_refund')),
 state text NOT NULL DEFAULT 'building' CHECK (state IN ('building','awaiting_signature','party_signed','signed','broadcast','confirmed','external','failed')),
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX settlements_one_outcome ON settlements (order_id) WHERE kind IN ('release','refund');
ALTER TABLE payments ADD COLUMN settlement_id bigint REFERENCES settlements(id);
