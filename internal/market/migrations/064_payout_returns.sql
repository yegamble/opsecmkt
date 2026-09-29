-- Overpayment returns: a release pays the vendor the order price and a second payout, kind 'return', sends the
-- excess back to the buyer (payments_hooks.go enqueuePayout). An order therefore has at most one payout of each
-- kind (release + return, or a single refund) instead of exactly one. The constraints are found by definition,
-- not by name, so a database whose default constraint names differ is migrated the same way.
DO $$
DECLARE c record;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='payouts'::regclass AND contype='u'
      AND conkey=ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid='payouts'::regclass AND attname='order_id')]::smallint[] LOOP
    EXECUTE format('ALTER TABLE payouts DROP CONSTRAINT %I', c.conname);
  END LOOP;
  FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='payouts'::regclass AND contype='c'
      AND pg_get_constraintdef(oid) LIKE '%kind%' LOOP
    EXECUTE format('ALTER TABLE payouts DROP CONSTRAINT %I', c.conname);
  END LOOP;
END $$;
ALTER TABLE payouts ADD CONSTRAINT payouts_kind_check CHECK (kind IN ('release','refund','return'));
ALTER TABLE payouts ADD CONSTRAINT payouts_order_kind_key UNIQUE (order_id, kind);
