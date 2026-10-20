-- Existing orders remain single-unit orders. Stock uses bigint so returning a
-- reserved bulk order cannot overflow the former integer column after restocks.
ALTER TABLE orders ADD COLUMN quantity bigint NOT NULL DEFAULT 1 CHECK (quantity > 0);
ALTER TABLE products ALTER COLUMN stock TYPE bigint;

CREATE FUNCTION order_quantity_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.state <> 'draft' AND NEW.quantity <> OLD.quantity THEN
  RAISE EXCEPTION 'quantity is immutable after checkout';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER order_quantity_frozen BEFORE UPDATE OF quantity ON orders
 FOR EACH ROW EXECUTE FUNCTION order_quantity_frozen();
