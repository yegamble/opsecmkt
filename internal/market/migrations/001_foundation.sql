-- Foundation: order state machine, second-factor seam, payment ledger tables, inventory columns.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'draft';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS updated timestamptz NOT NULL DEFAULT now();
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_state_check;
ALTER TABLE orders ADD CONSTRAINT orders_state_check CHECK (state IN ('draft','awaiting_payment','paid','shipped','delivered','completed','disputed','resolved','cancelled'));
-- Only unfunded drafts could exist before this migration; the column default already backfilled them.
ALTER TABLE orders DROP COLUMN IF EXISTS status;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_buyer_id_product_id_currency_key;
CREATE UNIQUE INDEX IF NOT EXISTS orders_one_draft ON orders(buyer_id,product_id,currency) WHERE state='draft';
CREATE INDEX IF NOT EXISTS orders_state_updated ON orders(state,updated);

CREATE TABLE IF NOT EXISTS order_events (
 id bigserial PRIMARY KEY, order_id text NOT NULL REFERENCES orders(id), from_state text NOT NULL, to_state text NOT NULL,
 actor_id text REFERENCES users(id), note text NOT NULL DEFAULT '', created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS order_events_order ON order_events(order_id,id);

CREATE TABLE IF NOT EXISTS pending_logins (
 token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires timestamptz NOT NULL, created timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS payment_addresses (
 order_id text PRIMARY KEY REFERENCES orders(id), currency text NOT NULL CHECK(currency IN ('BTC','XMR')),
 address text UNIQUE NOT NULL, provider text NOT NULL, subaddr_index bigint, created timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS payments (
 id bigserial PRIMARY KEY, order_id text NOT NULL REFERENCES orders(id), currency text NOT NULL CHECK(currency IN ('BTC','XMR')),
 txid text NOT NULL, idx bigint NOT NULL, address text NOT NULL, amount bigint NOT NULL CHECK(amount>=0), confirmations bigint NOT NULL DEFAULT 0,
 created timestamptz NOT NULL DEFAULT now(), updated timestamptz NOT NULL DEFAULT now(), UNIQUE(currency, txid, idx)
);
CREATE INDEX IF NOT EXISTS payments_order ON payments(order_id);

ALTER TABLE products ADD COLUMN IF NOT EXISTS archived boolean NOT NULL DEFAULT false;
ALTER TABLE products ADD COLUMN IF NOT EXISTS delivery_content text NOT NULL DEFAULT '';
-- The listing form offers a service fulfillment type; allow it at the schema level.
ALTER TABLE products DROP CONSTRAINT IF EXISTS products_kind_check;
ALTER TABLE products ADD CONSTRAINT products_kind_check CHECK (kind IN ('physical','digital','service'));
