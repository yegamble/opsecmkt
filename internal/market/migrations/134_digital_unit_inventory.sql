-- Existing listings keep their reusable delivery behavior until their vendor
-- explicitly chooses manual fulfillment or distinct inventory.
ALTER TABLE products ADD COLUMN digital_delivery_mode text NOT NULL DEFAULT 'reusable'
 CHECK (digital_delivery_mode IN ('reusable','manual','inventory'));
CREATE TABLE digital_units (
 id text PRIMARY KEY,
 product_id text NOT NULL REFERENCES products(id) ON DELETE CASCADE,
 content text NOT NULL CHECK (char_length(content)<=32000),
 fingerprint text CHECK (fingerprint IS NULL OR length(fingerprint)=64),
 order_id text REFERENCES orders(id) ON DELETE CASCADE,
 reserved_at timestamptz,
 delivered_at timestamptz,
 created timestamptz NOT NULL DEFAULT now(),
 UNIQUE(product_id,fingerprint),
 CHECK ((order_id IS NULL)=(reserved_at IS NULL)),
 CHECK (delivered_at IS NULL OR order_id IS NOT NULL),
 CHECK ((content<>'' AND fingerprint IS NOT NULL) OR (content='' AND fingerprint IS NULL AND order_id IS NOT NULL))
);
CREATE INDEX digital_units_available ON digital_units(product_id,created,id) WHERE order_id IS NULL;
CREATE INDEX digital_units_order ON digital_units(order_id) WHERE order_id IS NOT NULL;
