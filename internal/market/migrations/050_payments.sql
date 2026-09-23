-- P5 Payments: payout addresses, payout queue, ledger flags and per-currency watcher status.
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_btc text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_xmr text NOT NULL DEFAULT '';

-- credited: counted toward the paid transition or a payout. flagged: already reported to moderators
-- (credited deposit conflicted/missing, or a deposit that arrived after settlement).
ALTER TABLE payments ADD COLUMN IF NOT EXISTS credited boolean NOT NULL DEFAULT false;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS flagged boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS payments_address ON payments(currency, address);

-- One payout per order (release to the vendor or refund to the buyer). Single attempt:
-- pending -> sending (committed before the wallet call) -> sent | failed. blocked = recipient has no
-- payout address; held = a credited deposit was conflicted. sending/failed are never retried automatically.
CREATE TABLE IF NOT EXISTS payouts (
 id bigserial PRIMARY KEY,
 order_id text NOT NULL UNIQUE REFERENCES orders(id),
 kind text NOT NULL CHECK (kind IN ('release','refund')),
 user_id text NOT NULL REFERENCES users(id),
 currency text NOT NULL CHECK (currency IN ('BTC','XMR')),
 amount bigint NOT NULL CHECK (amount > 0),
 address text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('blocked','held','pending','sending','sent','failed')),
 txid text NOT NULL DEFAULT '',
 error text NOT NULL DEFAULT '',
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payouts_state ON payouts(state, currency, id);
CREATE INDEX IF NOT EXISTS payouts_user ON payouts(user_id, currency, state);

CREATE TABLE IF NOT EXISTS payment_status (
 currency text PRIMARY KEY CHECK (currency IN ('BTC','XMR')),
 network text NOT NULL,
 last_poll timestamptz,
 last_error text NOT NULL DEFAULT ''
);
