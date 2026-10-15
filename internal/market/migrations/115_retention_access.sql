-- A stale HTTP session cannot create new obligations after an account has been
-- restricted pending closure. Existing funded obligations can still settle.
CREATE FUNCTION retention_new_obligation_allowed(account_key text) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE allowed boolean;
BEGIN
 SELECT closed_at IS NULL AND retention_restricted_at IS NULL INTO allowed FROM users WHERE id=account_key FOR SHARE NOWAIT;
 RETURN COALESCE(allowed,false);
END
$$;
CREATE FUNCTION retention_obligation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE buyer_key text; vendor_key text;
BEGIN
 IF TG_TABLE_NAME='products' THEN
  vendor_key:=NEW.vendor_id;
 ELSIF TG_TABLE_NAME='orders' THEN
  buyer_key:=NEW.buyer_id;
  SELECT vendor_id INTO vendor_key FROM products WHERE id=NEW.product_id;
 ELSE
  SELECT o.buyer_id,p.vendor_id INTO buyer_key,vendor_key FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=NEW.order_id;
 END IF;
 IF (buyer_key IS NOT NULL AND NOT retention_new_obligation_allowed(buyer_key))
    OR (vendor_key IS NOT NULL AND NOT retention_new_obligation_allowed(vendor_key)) THEN
   RAISE EXCEPTION 'account closure prevents new financial obligations' USING ERRCODE='check_violation';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_product_obligation BEFORE INSERT ON products FOR EACH ROW EXECUTE FUNCTION retention_obligation_guard();
CREATE TRIGGER retention_order_obligation BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION retention_obligation_guard();
CREATE TRIGGER retention_escrow_obligation BEFORE INSERT ON escrows FOR EACH ROW EXECUTE FUNCTION retention_obligation_guard();
