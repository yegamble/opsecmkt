-- Owner directive 2026-10-15: Monero is the only supported currency.
-- Existing migrations and financial history remain immutable.
-- Keep the frozen legacy column for compatibility with immutable migrations,
-- without inventing a Bitcoin quote for a Monero-only listing.
ALTER TABLE products DROP CONSTRAINT products_btc_check;
ALTER TABLE products ALTER COLUMN btc SET DEFAULT 0;
ALTER TABLE products ADD CONSTRAINT products_btc_check CHECK(btc>=0);

ALTER TABLE orders ADD CONSTRAINT orders_monero_only CHECK(currency='XMR');
ALTER TABLE escrows ADD CONSTRAINT escrows_monero_only CHECK(coin='XMR');
ALTER TABLE payments ADD CONSTRAINT payments_monero_only CHECK(currency='XMR');
UPDATE settings SET value='false' WHERE key='payment_intake_BTC';

CREATE FUNCTION monero_only_intake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.key='payment_intake_BTC' AND NEW.value<>'false' THEN
  RAISE EXCEPTION 'Bitcoin is removed: only Monero payment intake may be enabled' USING ERRCODE='check_violation';
 END IF;
 RETURN NEW;
END
$$;
CREATE TRIGGER monero_only_intake BEFORE INSERT OR UPDATE ON settings
 FOR EACH ROW EXECUTE FUNCTION monero_only_intake();
