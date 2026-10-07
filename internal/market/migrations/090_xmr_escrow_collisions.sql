-- A-480. A Monero escrow deposit the database refused as another order's escrow account (payments_xmr_escrow_idx,
-- migration 084) is never written to the ledger; the escrow watcher notes it on the order it read it for and tells the
-- staff (xmrFlagCollision). Each such deposit is one record here, per order and transaction: the watcher meets the same
-- refusal on every later pass, and the record (not the note text) keeps the note and the staff notice to once; a new
-- transaction opens a new record. A record is open until a moderator or administrator who is not a party records what
-- was done about it (the order page's form, POST /moderator/xmr-collision): a note of 10-500 characters, the time and
-- the staff member. While one is open, the order has an escrow staff alert (escrowStaffAlert): staff may read the order
-- and the moderation desk lists it.
CREATE TABLE xmr_escrow_collisions (
 id bigserial PRIMARY KEY,
 order_id text NOT NULL REFERENCES orders(id),
 wallet_name text NOT NULL CHECK (wallet_name <> ''),
 account bigint NOT NULL CHECK (account BETWEEN 1 AND 4294967295),
 txid text NOT NULL CHECK (txid <> ''),
 -- txid holds only the first 20 characters of the transaction ID: a record backfilled below from a note written before
 -- this migration (the note named no more). The watcher completes it when it meets the same deposit again.
 txid_truncated boolean NOT NULL DEFAULT false,
 noted timestamptz NOT NULL DEFAULT now(),
 disposition_note text NOT NULL DEFAULT '',
 disposed_at timestamptz,
 disposed_by text REFERENCES users(id),
 UNIQUE (order_id, txid),
 CONSTRAINT xmr_escrow_collisions_truncated CHECK (NOT txid_truncated OR char_length(txid) = 20),
 CONSTRAINT xmr_escrow_collisions_disposition CHECK (
   (disposed_at IS NULL AND disposed_by IS NULL AND disposition_note = '')
   OR (disposed_at IS NOT NULL AND disposed_by IS NOT NULL AND char_length(disposition_note) BETWEEN 10 AND 500))
);
CREATE INDEX xmr_escrow_collisions_open ON xmr_escrow_collisions(order_id) WHERE disposed_at IS NULL;

-- Notes written before this migration: each system note of the watcher's fixed form becomes one open record, noted at
-- the note's time, holding the order, the wallet and account it names and the 20 hexadecimal digits of the transaction
-- it names (truncate(txid, 20) wrote them followed by an ellipsis), marked txid_truncated. None had a disposition, so
-- each stays open until staff record one. A note by a user that quotes the form is not a watcher note (actor_id set).
-- Records are numbered in the order their notes were written (the order page lists them by id).
INSERT INTO xmr_escrow_collisions(order_id, wallet_name, account, txid, txid_truncated, noted)
SELECT order_id, wallet_name, account, txid, true, noted FROM (
  SELECT DISTINCT ON (e.order_id, m[1]) e.order_id, m[2] AS wallet_name, m[3]::bigint AS account, m[1] AS txid, e.created AS noted, e.id
  FROM order_events e
  CROSS JOIN LATERAL regexp_match(e.note, '^\S+ \S+ Monero escrow deposit ([0-9a-f]{20})… to wallet (\S+) account ([1-9][0-9]{0,9}) was not recorded: the database refused it as another order''s escrow account\. Moderator review required\.$') m
  WHERE e.actor_id IS NULL AND m IS NOT NULL AND m[3]::bigint <= 4294967295
  ORDER BY e.order_id, m[1], e.created, e.id
) backfill
ORDER BY backfill.noted, backfill.id;
