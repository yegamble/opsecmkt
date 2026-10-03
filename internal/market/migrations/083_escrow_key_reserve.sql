-- Bitcoin escrow, Phase 2 slice P2-e (WR-escrow A-366, rule P2-R10; docs/testnet-runbook.md): no escrow child key is
-- handed out twice after a restore from backup. Nothing writes these while ESCROW_BTC is off, the default; escrow runs
-- on regtest only.
--
-- A restored database knows nothing of the escrows created after its backup, so each party's next child index and the
-- market's index are back where the backup left them, and a key a lost escrow used could be composed again into another
-- escrow address, linking the two escrows on chain.
--
-- escrow_reserved_keys: the exact layer. The escrow watcher's orphan sweep reads, with getaddressinfo on the watch-only
-- wallet, the three public keys of every address that wallet holds with no escrows row, and reserves each here with the
-- address it was read from. Escrow checkout never uses a child key found here or in escrow_pubkeys. scripts/restore.sh
-- (and UPGRADING.md section 6) set the setting escrow_sweep_required, and checkout waits until a full sweep has cleared
-- it.
CREATE TABLE escrow_reserved_keys (
 pubkey text PRIMARY KEY CHECK (pubkey ~ '^0[23][0-9a-f]{64}$'),
 address text NOT NULL CHECK (address ~ '^[A-Za-z0-9]{14,90}$'),
 created timestamptz NOT NULL DEFAULT now()
);

-- escrow_index_floors: the margin layer, for a watch-only wallet restored from the same backup (which knows no lost
-- escrow either). scripts/restore.sh writes, for every account with an escrow key, the next child index the market
-- would hand it (one more than its highest on record, or its floor if higher) plus 50; checkout's next child index for
-- the account is never below it.
CREATE TABLE escrow_index_floors (
 user_id text PRIMARY KEY REFERENCES users(id),
 next_index bigint NOT NULL CHECK (next_index >= 1)
);
