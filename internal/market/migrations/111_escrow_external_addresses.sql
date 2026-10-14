-- External address freshness is private to its recipient account. A global
-- rejection reveals another account's wallet history and lets an unrelated user
-- reserve its addresses. Escrow addresses and per-order spending constraints stay
-- global; only external refund/claim destination uniqueness changes here.
CREATE TABLE escrow_external_addresses (
 user_id text NOT NULL REFERENCES users(id),
 coin text NOT NULL CHECK (coin IN ('BTC','XMR')),
 address text NOT NULL CHECK (address <> ''),
 PRIMARY KEY (user_id, coin, address)
);

INSERT INTO escrow_external_addresses(user_id,coin,address)
 SELECT o.buyer_id,e.coin,e.refund_address FROM escrows e JOIN orders o ON o.id=e.order_id
 ON CONFLICT DO NOTHING;
INSERT INTO escrow_external_addresses(user_id,coin,address)
 SELECT CASE WHEN s.kind='release' THEN p.vendor_id ELSE o.buyer_id END,'BTC',s.destination
 FROM settlements s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id
 WHERE s.destination IS NOT NULL ON CONFLICT DO NOTHING;
INSERT INTO escrow_external_addresses(user_id,coin,address)
 SELECT CASE WHEN s.kind='release' THEN p.vendor_id ELSE o.buyer_id END,'XMR',s.destination
 FROM xmr_settlements s JOIN orders o ON o.id=s.order_id JOIN products p ON p.id=o.product_id
 ON CONFLICT DO NOTHING;

ALTER TABLE escrows DROP CONSTRAINT escrows_refund_address_key;
DROP INDEX settlements_release_destination;
DROP INDEX xmr_settlements_release_destination;

-- AFTER triggers run after the original order/outcome constraints. Reservations
-- are transactional and atomic across checkout and claims, including an account
-- acting as buyer on one order and vendor on another. A failed checkout/claim
-- rolls its reservation back; the recipient, never a staff actor, owns it.
CREATE FUNCTION escrow_refund_address_reserve() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO escrow_external_addresses(user_id,coin,address)
    SELECT buyer_id,NEW.coin,NEW.refund_address FROM orders WHERE id=NEW.order_id;
  RETURN NEW;
END $$;
CREATE TRIGGER escrow_refund_address_reserve AFTER INSERT ON escrows
 FOR EACH ROW EXECUTE FUNCTION escrow_refund_address_reserve();

CREATE FUNCTION escrow_claim_address_reserve() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE claim_coin text;
BEGIN
  IF NEW.kind <> 'release' OR NEW.destination IS NULL THEN RETURN NEW; END IF;
  IF TG_OP='UPDATE' AND OLD.kind=NEW.kind AND OLD.destination IS NOT DISTINCT FROM NEW.destination THEN
    RETURN NEW;
  END IF;
  claim_coin := 'BTC';
  IF TG_TABLE_NAME='xmr_settlements' THEN
    -- A re-request spends the original order's inputs to its unchanged address.
    -- Its original release already reserved that address for the same vendor.
    IF NEW.supersedes IS NOT NULL THEN RETURN NEW; END IF;
    claim_coin := 'XMR';
  END IF;
  INSERT INTO escrow_external_addresses(user_id,coin,address)
    SELECT p.vendor_id,claim_coin,NEW.destination FROM orders o JOIN products p ON p.id=o.product_id
    WHERE o.id=NEW.order_id;
  RETURN NEW;
END $$;
CREATE TRIGGER escrow_claim_address_reserve AFTER INSERT OR UPDATE OF destination,kind ON settlements
 FOR EACH ROW EXECUTE FUNCTION escrow_claim_address_reserve();
CREATE TRIGGER xmr_escrow_claim_address_reserve AFTER INSERT OR UPDATE OF destination,kind ON xmr_settlements
 FOR EACH ROW EXECUTE FUNCTION escrow_claim_address_reserve();
