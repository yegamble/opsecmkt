-- A-84: find a historical pooled deposit by the 20-character transaction-ID prefix in a displayed
-- notification, without scanning a vendor's entire payment history. Full txids use payments' unique index.
CREATE INDEX IF NOT EXISTS payments_txid20_privacy ON payments ((left(txid,20)));
