-- Vendor collateral has its own wallets and ledger; it is never an order payment.
INSERT INTO settings(key,value) VALUES ('vendor_bond','{"amount":0,"currency":"USD"}') ON CONFLICT DO NOTHING;

CREATE TABLE fiat_rates (
    fiat text NOT NULL CHECK (fiat IN ('USD','GBP','EUR')),
    coin text NOT NULL CHECK (coin IN ('BTC','XMR')),
    rate text NOT NULL,
    as_of timestamptz NOT NULL,
    sources text[] NOT NULL,
    checked_at timestamptz NOT NULL,
    available boolean NOT NULL,
    pending_rate text NOT NULL DEFAULT '',
    pending_at timestamptz,
    pending_as_of timestamptz,
    PRIMARY KEY (fiat,coin)
);

CREATE TABLE vendor_bond_quotes (
    id text PRIMARY KEY,
    vendor_id text NOT NULL REFERENCES users(id),
    reference_currency text NOT NULL CHECK (reference_currency IN ('USD','GBP','EUR','BTC','XMR')),
    amount bigint NOT NULL CHECK (amount>0),
    coin text NOT NULL CHECK (coin IN ('BTC','XMR')),
    crypto_amount bigint NOT NULL CHECK (crypto_amount>0),
    address text NOT NULL CHECK (address<>''),
    provider_reference text NOT NULL CHECK (provider_reference<>''),
    rate text NOT NULL DEFAULT '',
    rate_sources text[] NOT NULL DEFAULT '{}',
    rate_as_of timestamptz,
    quoted_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now()+interval '30 minutes',
    committed_at timestamptz,
    confirmed bigint NOT NULL DEFAULT 0 CHECK (confirmed>=0),
    pending bigint NOT NULL DEFAULT 0 CHECK (pending>=0),
    checked_at timestamptz,
    observation_ok boolean NOT NULL DEFAULT false,
    UNIQUE (coin,address),
    UNIQUE (coin,provider_reference)
);
CREATE INDEX vendor_bond_quotes_vendor ON vendor_bond_quotes(vendor_id,reference_currency);
CREATE INDEX vendor_bond_quotes_poll ON vendor_bond_quotes(checked_at);
-- One renewable quote per vendor, coin and reference currency until fully observed on time.
CREATE UNIQUE INDEX vendor_bond_quotes_uncommitted ON vendor_bond_quotes(vendor_id,coin,reference_currency) WHERE committed_at IS NULL;
