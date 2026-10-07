-- A-456b (shape (b) slice 4, second half; rulings P4-R2, P4-R17; QA5W3A-2). A deposit to a per-order escrow's sibling
-- address is returned by a stray refund spending that sibling's own 2-of-3 script: each party's per-order key at
-- /<i>/<branch>/<k> and the escrow signer's at /<m>/<branch>/<k>. Checkout stores the three keys of each of the 29
-- siblings, each derived in Go and checked against Bitcoin Core before anything is stored, so the stray refund never
-- depends on a party's current escrow key (a party may register another key after checkout). See UPGRADING.md section 45.

-- No release created a per-order escrow before this one (A-456a's checkout switch was off), so escrow_siblings holds
-- no row outside a test database. A row would have no keys to give these columns: it stops this migration with a
-- message, before anything changes.
DO $$
DECLARE
  n bigint;
BEGIN
  SELECT count(*) INTO n FROM escrow_siblings;
  IF n > 0 THEN
    RAISE EXCEPTION 'migration 091: escrow_siblings holds % sibling address row(s) without their keys. No release created a per-order escrow before migration 091 (A-456a''s checkout switch was off), so these come from a test or development database: restore the pre-upgrade backup or start from a new database. See UPGRADING.md section 45. Nothing was changed.', n
      USING ERRCODE = 'check_violation';
  END IF;
END $$;

ALTER TABLE escrow_siblings
 ADD COLUMN buyer_pubkey text NOT NULL CHECK (buyer_pubkey ~ '^0[23][0-9a-f]{64}$'),
 ADD COLUMN vendor_pubkey text NOT NULL CHECK (vendor_pubkey ~ '^0[23][0-9a-f]{64}$'),
 ADD COLUMN market_pubkey text NOT NULL CHECK (market_pubkey ~ '^0[23][0-9a-f]{64}$'),
 ADD CONSTRAINT escrow_siblings_distinct_keys CHECK (buyer_pubkey <> vendor_pubkey AND buyer_pubkey <> market_pubkey AND vendor_pubkey <> market_pubkey);
