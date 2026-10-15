-- Retention never bypasses the escrow preservation triggers. Keep bounded,
-- reviewable operational exceptions when financial closure or retirement is due.
ALTER TABLE orders ADD COLUMN financial_checked_at timestamptz;

CREATE TABLE retention_escrow_exceptions (
 order_id text PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,
 reason text NOT NULL CHECK (reason IN (
  'recovery_gate','pending_recovery','dispute_open','payment_review','escrow_alert',
  'settlement_pending','deposit_unreconciled','wallet_unavailable','wallet_unverified',
  'watch_incomplete','monero_wallet_not_destroyed','bitcoin_wallet_retirement_required',
  'monero_financial_retirement_required')),
 first_seen timestamptz NOT NULL,
 last_checked timestamptz NOT NULL,
 review_due timestamptz NOT NULL,
 resolved_at timestamptz,
 CHECK (last_checked >= first_seen),
 CHECK (review_due >= first_seen)
);
CREATE INDEX retention_escrow_exceptions_open ON retention_escrow_exceptions(review_due) WHERE resolved_at IS NULL;
CREATE INDEX orders_financial_checked ON orders(financial_checked_at, id) WHERE state IN ('completed','cancelled','resolved');

-- Closure must become invalid immediately when new financial work arrives,
-- including outside the next bounded retention batch. These triggers preserve
-- escrow state and only invalidate retention eligibility.
CREATE FUNCTION retention_reopen_order(order_key text) RETURNS void LANGUAGE sql AS $$
 UPDATE orders SET financial_closed_at=NULL,financial_checked_at=NULL,private_expired_at=NULL
 WHERE id=order_key
$$;

CREATE FUNCTION retention_payment_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.confirmations>=0 AND (NEW.locked OR NEW.spent_outside
    OR NEW.currency='BTC' AND NOT EXISTS(SELECT 1 FROM settlements s WHERE s.id=NEW.settlement_id AND s.state='confirmed' AND s.reorg_safe_at IS NOT NULL)
    OR NEW.currency='XMR' AND NOT EXISTS(SELECT 1 FROM xmr_settlements s WHERE s.id=NEW.xmr_settlement_id AND s.state='confirmed' AND s.reorg_safe_at IS NOT NULL)) THEN
  PERFORM retention_reopen_order(NEW.order_id);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_payment_reopened AFTER INSERT OR UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION retention_payment_reopened();

CREATE FUNCTION retention_settlement_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state NOT IN ('confirmed','failed','external','superseded') OR NEW.state='confirmed' AND NEW.reorg_safe_at IS NULL THEN
  PERFORM retention_reopen_order(NEW.order_id);
 ELSIF NEW.state='confirmed' AND (TG_OP='INSERT' OR NEW.reorg_safe_at IS DISTINCT FROM OLD.reorg_safe_at) THEN
  UPDATE orders SET financial_checked_at=NULL WHERE id=NEW.order_id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_settlement_reopened AFTER INSERT OR UPDATE ON settlements FOR EACH ROW EXECUTE FUNCTION retention_settlement_reopened();
CREATE TRIGGER retention_xmr_settlement_reopened AFTER INSERT OR UPDATE ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION retention_settlement_reopened();

CREATE FUNCTION retention_bump_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE order_key text;
BEGIN
 IF NEW.state NOT IN ('confirmed','failed','superseded') THEN
  SELECT order_id INTO order_key FROM settlements WHERE id=NEW.settlement_id;
  PERFORM retention_reopen_order(order_key);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_bump_reopened AFTER INSERT OR UPDATE ON settlement_bumps FOR EACH ROW EXECUTE FUNCTION retention_bump_reopened();

CREATE FUNCTION retention_dispute_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.status='Open' THEN PERFORM retention_reopen_order(NEW.order_id); END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_dispute_reopened AFTER INSERT OR UPDATE ON disputes FOR EACH ROW EXECUTE FUNCTION retention_dispute_reopened();

CREATE FUNCTION retention_recovery_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- A request alone does not prove new financial work and cannot restart an
 -- expiry clock. Pause/recheck it; observed funds and live settlements use the
 -- separate fact triggers to invalidate closure immediately.
 IF NEW.recover_requested IS NOT NULL THEN UPDATE orders SET financial_checked_at=NULL WHERE id=NEW.order_id; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_recovery_reopened AFTER UPDATE OF recover_requested ON escrows FOR EACH ROW EXECUTE FUNCTION retention_recovery_reopened();

CREATE FUNCTION retention_collision_reopened() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.disposed_at IS NULL THEN PERFORM retention_reopen_order(NEW.order_id); END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_collision_reopened AFTER INSERT OR UPDATE ON xmr_escrow_collisions FOR EACH ROW EXECUTE FUNCTION retention_collision_reopened();

CREATE FUNCTION retention_wallet_destroyed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state='destroyed' AND OLD.state<>'destroyed' THEN
  UPDATE orders SET financial_checked_at=NULL WHERE id IN(SELECT order_id FROM xmr_escrow_slots WHERE wallet_id=NEW.id);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_wallet_destroyed AFTER UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_wallet_destroyed();
