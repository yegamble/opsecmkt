-- Bitcoin escrow, Phase 2 slice P2-b (WR-escrow A-247, P2-R2, A-372; docs/escrow.md): the database backstop for "the
-- market never signs before the outcome". Nothing writes these while ESCROW_BTC is off, the default; escrow runs on
-- regtest only.
--
-- A physical escrow order's release is built when the vendor marks it shipped, long before the order has an outcome:
-- the buyer and the vendor may complete it with their two signatures (cosigned 'parties'), and only then is the order
-- completed. A final transaction the market co-signed (cosigned 'market') is stored only when the order's state fits
-- the settlement's kind: a release on a completed order or one whose dispute was decided for the vendor, a refund on a
-- cancelled order or one decided for the buyer, a stray refund on any closed order. The same holds for a fee bump the
-- market co-signs (migration 080), by its settlement's kind and order. Rows stored before this migration are not
-- checked again.
CREATE FUNCTION escrow_market_may_sign(kind text, order_id text) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT COALESCE((SELECT CASE kind
      WHEN 'release' THEN o.state = 'completed' OR (o.state = 'resolved' AND COALESCE(d.outcome, '') = 'release')
      WHEN 'refund' THEN o.state = 'cancelled' OR (o.state = 'resolved' AND COALESCE(d.outcome, '') = 'refund')
      WHEN 'stray_refund' THEN o.state IN ('completed', 'cancelled', 'resolved')
      ELSE false END
    FROM orders o LEFT JOIN disputes d ON d.order_id = o.id WHERE o.id = escrow_market_may_sign.order_id), false)
$$;

CREATE FUNCTION settlements_market_fits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.raw_tx IS NOT NULL AND NEW.cosigned = 'market' AND (TG_OP = 'INSERT' OR OLD.raw_tx IS NULL)
      AND NOT escrow_market_may_sign(NEW.kind, NEW.order_id) THEN
    RAISE EXCEPTION 'settlements: the market co-signs settlement % only once its order''s state fits its kind (a release on a completed order or one decided for the vendor, a refund on a cancelled order or one decided for the buyer)', NEW.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlements_market_fits BEFORE INSERT OR UPDATE ON settlements FOR EACH ROW EXECUTE FUNCTION settlements_market_fits();

CREATE FUNCTION settlement_bumps_market_fits() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
BEGIN
  IF NEW.raw_tx IS NOT NULL AND NEW.cosigned = 'market' AND (TG_OP = 'INSERT' OR OLD.raw_tx IS NULL) THEN
    SELECT kind, order_id INTO s FROM settlements WHERE id = NEW.settlement_id;
    IF NOT escrow_market_may_sign(s.kind, s.order_id) THEN
      RAISE EXCEPTION 'settlement_bumps: the market co-signs fee bump % only once its order''s state fits its settlement''s kind', NEW.id
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlement_bumps_market_fits BEFORE INSERT OR UPDATE ON settlement_bumps FOR EACH ROW EXECUTE FUNCTION settlement_bumps_market_fits();
