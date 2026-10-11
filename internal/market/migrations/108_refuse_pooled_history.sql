-- A-492: no pooled market exists (owner directive 2026-10-11), so this build keeps no pooled payment history. A database
-- holding any payouts or payment_addresses row is refused before anything changes, and both tables then refuse every
-- new row. The tables, their constraints and the drain functions migration 107 reads stay: migrations are append-only.
-- See UPGRADING.md section 59.
DO $$
DECLARE
  payout_rows bigint;
  address_rows bigint;
BEGIN
  SELECT count(*) INTO payout_rows FROM payouts;
  SELECT count(*) INTO address_rows FROM payment_addresses;
  IF payout_rows > 0 OR address_rows > 0 THEN
    RAISE EXCEPTION 'migration 108 refused: this database holds pooled payment history (% payouts row(s), % payment_addresses row(s)), which this build does not support. Nothing was changed. Keep the previous release on this database; see UPGRADING.md section 59.', payout_rows, address_rows;
  END IF;
END
$$;

CREATE OR REPLACE FUNCTION refuse_pooled_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'pooled payments are removed: % takes no new rows (migration 108, UPGRADING.md section 59)', TG_TABLE_NAME;
END
$$;

CREATE OR REPLACE TRIGGER payouts_refuse_insert BEFORE INSERT ON payouts
  FOR EACH ROW EXECUTE FUNCTION refuse_pooled_history();
CREATE OR REPLACE TRIGGER payment_addresses_refuse_insert BEFORE INSERT ON payment_addresses
  FOR EACH ROW EXECUTE FUNCTION refuse_pooled_history();
