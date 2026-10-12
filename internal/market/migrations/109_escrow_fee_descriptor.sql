-- A-460 (ES-5, owner decision 2026-09-29): the Bitcoin escrow market fee may be paid to a watch-only descriptor, and on
-- a mainnet market must be, so each settlement pays a fresh fee address. Checkout snapshots the descriptor's account
-- key, sealed (fee_descriptor), and no address. The fee index is taken from escrow_fee_index once per escrow, at its
-- first release build, and kept for every rebuild; the address derived from it is then stored in fee_destination, where
-- the checks that refuse an address the market already used find it. scripts/restore.sh moves the sequence past the
-- restored records, as it moves escrow_market_index.
CREATE SEQUENCE escrow_fee_index AS bigint MINVALUE 0 START WITH 0;

DO $$
DECLARE
  c text;
BEGIN
  SELECT conname INTO c FROM pg_constraint
    WHERE conrelid = 'escrows'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%fee_bps = 0) OR (fee_destination <> %';
  IF c IS NULL THEN
    RAISE EXCEPTION 'migration 109: the escrows fee destination check of migration 074 was not found';
  END IF;
  EXECUTE format('ALTER TABLE escrows DROP CONSTRAINT %I', c);
END
$$;

ALTER TABLE escrows
  ADD COLUMN fee_descriptor text CHECK (fee_descriptor <> ''),
  ADD COLUMN fee_index bigint UNIQUE CHECK (fee_index BETWEEN 0 AND 2147483647),
  ADD CONSTRAINT escrows_fee_destination_or_descriptor CHECK (fee_bps = 0 OR fee_destination <> '' OR fee_descriptor IS NOT NULL),
  ADD CONSTRAINT escrows_fee_descriptor_btc CHECK (fee_descriptor IS NULL OR coin = 'BTC'),
  ADD CONSTRAINT escrows_fee_index_descriptor CHECK (fee_index IS NULL OR fee_descriptor IS NOT NULL);
