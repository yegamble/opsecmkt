-- A-217: bound the staff shared-transaction lookup to the exact recorded ID,
-- including pre-existing uppercase IDs, without imposing uniqueness on sendmany.
CREATE INDEX payouts_txid_lookup ON payouts (lower(txid), id) WHERE txid<>'';
