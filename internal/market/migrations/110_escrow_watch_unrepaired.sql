-- A-527: the per-order escrows the watch-only wallet repair (ensureSiblingWatch, escrow_watch.go) could not rescan for.
-- pruned: the escrow is older than the oldest block the pruned escrow node keeps (within Bitcoin Core's 2-hour rescan
-- window), so nothing was imported for it. import_failed: Bitcoin Core answered the repair's import of it as failed
-- (its rescan could not read a block); Core holds the descriptor and its label all the same, so a later pass must not
-- read them as a repair, and imports it again. A row goes once the wallet holds the escrow after an import that
-- succeeded (or, for one never imported, holds it with its label: a restored bitcoin_data volume). While an escrow has a
-- row its order does not expire, the orphan sweep leaves escrow_sweep_required set, and /admin names the order.
CREATE TABLE escrow_watch_unrepaired (
  escrow_id bigint PRIMARY KEY REFERENCES escrows(id) ON DELETE CASCADE,
  pruned boolean NOT NULL DEFAULT false,
  import_failed boolean NOT NULL DEFAULT false,
  since timestamptz NOT NULL DEFAULT now(),
  CHECK (pruned OR import_failed)
);
