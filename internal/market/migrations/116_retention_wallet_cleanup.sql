-- Physical files and Core descriptors are outside the application's database.
-- Keep a reviewable obligation after order/participant mappings are erased.
-- Wallet names identify protected host inventory, not anonymous information.
CREATE TABLE retention_wallet_cleanup (
 coin text NOT NULL CHECK(coin IN ('BTC','XMR')),
 wallet_name text NOT NULL CHECK(length(wallet_name) BETWEEN 1 AND 255),
 due_at timestamptz NOT NULL,
 first_registered_at timestamptz NOT NULL,
 last_checked timestamptz NOT NULL,
 review_due timestamptz NOT NULL,
 confirmed_at timestamptz,
 confirmed_by text CHECK(length(confirmed_by) BETWEEN 1 AND 128),
 PRIMARY KEY(coin,wallet_name),
 CHECK((confirmed_at IS NULL)=(confirmed_by IS NULL))
);
CREATE INDEX retention_wallet_cleanup_open ON retention_wallet_cleanup(review_due) WHERE confirmed_at IS NULL;
-- Scope physical destruction proof to the exact wallet without keeping its
-- plaintext inventory name in the permanent address non-reuse guard.
ALTER TABLE retention_escrow_retired_addresses ADD COLUMN wallet_hash text CHECK(wallet_hash ~ '^[0-9a-f]{64}$');

CREATE FUNCTION retention_wallet_cleanup_register(currency text,wallet text,due timestamptz,observed timestamptz) RETURNS void LANGUAGE sql AS $$
 INSERT INTO retention_wallet_cleanup(coin,wallet_name,due_at,first_registered_at,last_checked,review_due)
 VALUES(currency,wallet,due,observed,observed,observed+interval '720 hours')
 ON CONFLICT(coin,wallet_name) DO UPDATE SET
 due_at=LEAST(retention_wallet_cleanup.due_at,EXCLUDED.due_at),
 last_checked=GREATEST(retention_wallet_cleanup.last_checked,EXCLUDED.last_checked),
 review_due=CASE WHEN retention_wallet_cleanup.confirmed_at IS NOT NULL AND observed>retention_wallet_cleanup.confirmed_at THEN observed+interval '720 hours' ELSE retention_wallet_cleanup.review_due END,
 confirmed_at=CASE WHEN observed>retention_wallet_cleanup.confirmed_at THEN NULL ELSE retention_wallet_cleanup.confirmed_at END,
 confirmed_by=CASE WHEN observed>retention_wallet_cleanup.confirmed_at THEN NULL ELSE retention_wallet_cleanup.confirmed_by END
$$;

-- Register promptly at the existing guarded90day destruction, including recovery
-- and operator-confirmed destruction; do not wait for order history expiry.
CREATE FUNCTION retention_destroyed_wallet_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state='destroyed' AND NEW.destroyed_at IS NOT NULL THEN
  IF TG_OP='UPDATE' THEN
   IF OLD.state='destroyed' THEN RETURN NEW; END IF;
  END IF;
  PERFORM retention_wallet_cleanup_register('XMR',NEW.name,COALESCE(NEW.destroy_after,NEW.destroyed_at),NEW.destroyed_at);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_destroyed_wallet_cleanup AFTER INSERT OR UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_destroyed_wallet_cleanup();
SELECT retention_wallet_cleanup_register('XMR',name,COALESCE(destroy_after,destroyed_at),destroyed_at)
 FROM xmr_escrow_wallets WHERE state='destroyed' AND destroyed_at IS NOT NULL;

-- A global allocation highwater survives erasing the last financial row. The
-- row lock serializes allocation with certificate replay without a racing
-- setval that could move PostgreSQL's sequence backwards during checkout.
CREATE TABLE retention_escrow_global_floors (
 kind text PRIMARY KEY CHECK(kind IN ('market','fee')),
 next_index bigint NOT NULL CHECK(next_index>=0),
 allocation_next_index bigint NOT NULL DEFAULT 0 CHECK(allocation_next_index>=next_index)
);
INSERT INTO retention_escrow_global_floors(kind,next_index) VALUES('market',0),('fee',0);
CREATE FUNCTION retention_next_escrow_index(index_kind text) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE floor_index bigint; allocated_index bigint; result bigint;
BEGIN
 SELECT next_index,allocation_next_index INTO STRICT floor_index,allocated_index FROM retention_escrow_global_floors WHERE kind=index_kind FOR UPDATE;
 IF index_kind='market' THEN result:=nextval('escrow_market_index');
 ELSIF index_kind='fee' THEN result:=nextval('escrow_fee_index');
 ELSE RAISE EXCEPTION 'unknown escrow index kind'; END IF;
 result:=GREATEST(result,floor_index,allocated_index);
 UPDATE retention_escrow_global_floors SET allocation_next_index=result+1 WHERE kind=index_kind;
 RETURN result;
END $$;
CREATE FUNCTION retention_escrow_index_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.coin='BTC' AND NEW.market_index<(SELECT next_index FROM retention_escrow_global_floors WHERE kind='market') THEN
   RAISE EXCEPTION 'escrows: retired market indices are never reused' USING ERRCODE='check_violation';
  END IF;
 END IF;
 IF NEW.fee_index IS NOT NULL THEN
  IF TG_OP='UPDATE' THEN
   IF NEW.fee_index IS NOT DISTINCT FROM OLD.fee_index THEN RETURN NEW; END IF;
  END IF;
  IF NEW.fee_index<(SELECT next_index FROM retention_escrow_global_floors WHERE kind='fee') THEN
   RAISE EXCEPTION 'escrows: retired fee indices are never reused' USING ERRCODE='check_violation';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_escrow_index_guard BEFORE INSERT OR UPDATE OF fee_index ON escrows FOR EACH ROW EXECUTE FUNCTION retention_escrow_index_guard();

-- A late hold between the first runtime check and wallet destruction must keep
-- the recovery seed. The password-change start also refuses a duty already in
-- force; preserve the seed if a duty arrives after that external RPC begins.
CREATE FUNCTION retention_xmr_wallet_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.seed_sealed IS NULL AND OLD.seed_sealed IS NOT NULL)
 OR (NEW.destroy_started_at IS NOT NULL AND NEW.destroy_started_at IS DISTINCT FROM OLD.destroy_started_at) THEN
  IF NOT pg_try_advisory_xact_lock(hashtextextended('retention-xmr-wallet:'||NEW.id::text,0)) THEN
   RAISE EXCEPTION 'xmr_escrow_wallets: preservation registration is in progress; retry destruction' USING ERRCODE='lock_not_available';
  END IF;
  IF EXISTS(SELECT 1 FROM xmr_escrow_slots sl JOIN orders o ON o.id=sl.order_id JOIN products p ON p.id=o.product_id JOIN retention_holds h ON h.released_at IS NULL AND ((h.scope='order' AND h.record_id=o.id) OR (h.scope='account' AND h.record_id IN(o.buyer_id,p.vendor_id))) WHERE sl.wallet_id=NEW.id) THEN
   RAISE EXCEPTION 'xmr_escrow_wallets: preservation duty prevents wallet destruction and recovery seed erasure' USING ERRCODE='check_violation';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_xmr_wallet_hold_guard BEFORE UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_xmr_wallet_hold_guard();

CREATE FUNCTION retention_xmr_hold_registration_lock() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE wallet_key bigint;
BEGIN
 IF NEW.scope IN('account','order') THEN
  FOR wallet_key IN SELECT DISTINCT sl.wallet_id FROM xmr_escrow_slots sl JOIN orders o ON o.id=sl.order_id JOIN products p ON p.id=o.product_id WHERE
   (NEW.scope='order' AND NEW.record_id=o.id) OR (NEW.scope='account' AND NEW.record_id IN(o.buyer_id,p.vendor_id)) ORDER BY sl.wallet_id
  LOOP
   IF NOT pg_try_advisory_xact_lock(hashtextextended('retention-xmr-wallet:'||wallet_key::text,0)) THEN
    RAISE EXCEPTION 'retention_holds: exact wallet destruction is in progress; retry preservation registration or release' USING ERRCODE='lock_not_available';
   END IF;
  END LOOP;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_xmr_hold_registration_lock BEFORE INSERT OR UPDATE ON retention_holds FOR EACH ROW EXECUTE FUNCTION retention_xmr_hold_registration_lock();
