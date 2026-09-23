CREATE TABLE IF NOT EXISTS users (
 id text PRIMARY KEY, handle text UNIQUE NOT NULL, password_hash text NOT NULL,
 role text NOT NULL CHECK (role IN ('buyer','vendor','moderator','admin')),
 pgp text NOT NULL DEFAULT '', xmpp text NOT NULL DEFAULT '', created timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS settings (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires timestamptz NOT NULL, created timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS products (
 id text PRIMARY KEY, vendor_id text NOT NULL REFERENCES users(id), title text NOT NULL, description text NOT NULL,
 category text NOT NULL, region text NOT NULL, kind text NOT NULL CHECK(kind IN ('physical','digital')),
 btc bigint NOT NULL CHECK(btc>0), xmr bigint NOT NULL CHECK(xmr>0), stock integer NOT NULL CHECK(stock>=0),
 created timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS orders (
 id text PRIMARY KEY, buyer_id text NOT NULL REFERENCES users(id), product_id text NOT NULL REFERENCES products(id),
 currency text NOT NULL CHECK(currency IN ('BTC','XMR')), amount bigint NOT NULL CHECK(amount>0),
 status text NOT NULL DEFAULT 'Draft — payment unavailable', created timestamptz NOT NULL DEFAULT now(),
 UNIQUE(buyer_id, product_id, currency)
);
CREATE TABLE IF NOT EXISTS messages (id text PRIMARY KEY, sender_id text NOT NULL REFERENCES users(id), recipient_id text NOT NULL REFERENCES users(id), body text NOT NULL, created timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS notifications (id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id), body text NOT NULL, is_read boolean NOT NULL DEFAULT false, created timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS disputes (id text PRIMARY KEY, order_id text UNIQUE NOT NULL REFERENCES orders(id), reason text NOT NULL, status text NOT NULL DEFAULT 'Open', resolution text NOT NULL DEFAULT '', created timestamptz NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS audit_events (id bigserial PRIMARY KEY, user_id text REFERENCES users(id), action text NOT NULL, created timestamptz NOT NULL DEFAULT now());
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires);
CREATE INDEX IF NOT EXISTS messages_recipient ON messages(recipient_id,created);
CREATE INDEX IF NOT EXISTS notifications_user ON notifications(user_id,created);
INSERT INTO settings(key,value) VALUES ('site_name','OPSMKT'),('bitcoin_mode','disabled'),('monero_mode','disabled') ON CONFLICT DO NOTHING;
