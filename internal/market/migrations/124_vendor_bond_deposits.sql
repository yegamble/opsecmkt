-- A-547 (owner decision 2026-10-18): the vendor bond paid automatically in XMR. With Monero escrow on and ready, new
-- XMR payments on (payment_intake_XMR 'escrow'), the restore gate and the restore sweep clear, the Monero escrow
-- watcher gives each application waiting for a bond a one-time deposit address: an account of a dedicated bond wallet
-- on the escrow wallet-rpc, used for that application alone. Deposits count from the escrow confirmation threshold;
-- once the counted deposits cover the bond captured on the application (migration 121), it is recorded as received
-- (bond_received_how 'xmr'). The bond is a NON-REFUNDABLE fee and the market never sends from the bond wallet: the
-- operator sweeps it offline from its sealed seed. A-544's manual "record bond received" and waiver stay.

-- xmr_bond_wallets: the bond wallet, named bond-NN, created as escrow wallets are (a row in state creating, then
-- create_wallet with the password derived from MONERO_WALLET_SECRET, its seed sealed to ESCROW_XMR_SEED_KEY with the
-- primary address and restore height: sealed). At most one wallet is creating or sealed: the one in use. A file that
-- already had the name and opens with the market's password (a bond wallet this database lost, after a restore) is
-- recorded as an orphan, its seed sealed so the operator can sweep it; one that does not open is an orphan with no
-- seal. Names, addresses and seals never change once set; states move only creating -> sealed | orphan.
CREATE TABLE xmr_bond_wallets (
 id bigserial PRIMARY KEY,
 name text NOT NULL UNIQUE CHECK (name ~ '^bond-[0-9]{2}$'),
 primary_address text UNIQUE CHECK (primary_address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 restore_height bigint CHECK (restore_height >= 0),
 seed_sealed text CHECK (seed_sealed <> ''),
 seed_key_fpr text CHECK (seed_key_fpr ~ '^[0-9A-F]{40}([0-9A-F]{24})?$'),
 sealed_at timestamptz,
 state text NOT NULL DEFAULT 'creating' CHECK (state IN ('creating','sealed','orphan')),
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT xmr_bond_wallets_sealed CHECK (state <> 'sealed' OR (seed_sealed IS NOT NULL AND primary_address IS NOT NULL AND restore_height IS NOT NULL)),
 CONSTRAINT xmr_bond_wallets_seal CHECK (seed_sealed IS NULL OR (seed_key_fpr IS NOT NULL AND sealed_at IS NOT NULL))
);
CREATE UNIQUE INDEX xmr_bond_wallets_in_use ON xmr_bond_wallets ((true)) WHERE state IN ('creating','sealed');

CREATE FUNCTION xmr_bond_wallets_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.name IS DISTINCT FROM OLD.name
      OR (OLD.primary_address IS NOT NULL AND NEW.primary_address IS DISTINCT FROM OLD.primary_address)
      OR (OLD.restore_height IS NOT NULL AND NEW.restore_height IS DISTINCT FROM OLD.restore_height)
      OR (OLD.seed_sealed IS NOT NULL AND NEW.seed_sealed IS DISTINCT FROM OLD.seed_sealed)
      OR (OLD.seed_key_fpr IS NOT NULL AND NEW.seed_key_fpr IS DISTINCT FROM OLD.seed_key_fpr)
      OR (OLD.sealed_at IS NOT NULL AND NEW.sealed_at IS DISTINCT FROM OLD.sealed_at)
      OR (NEW.state IS DISTINCT FROM OLD.state AND NOT (OLD.state = 'creating' AND NEW.state IN ('sealed','orphan'))) THEN
    RAISE EXCEPTION 'xmr_bond_wallets: bond wallet % keeps its name, address, restore height and sealed seed, and moves only from creating to sealed or orphan', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_bond_wallets_kept BEFORE UPDATE ON xmr_bond_wallets FOR EACH ROW EXECUTE FUNCTION xmr_bond_wallets_kept();

-- vendor_bond_accounts: the accounts of the bond wallet, each made by create_account and checked against get_address
-- of its address 0 before its row is written, for one application (application_id) and never reused. An account the
-- wallet holds with no row (a create_account whose answer was lost, or a database restored to an earlier point) is
-- added with no application: it never funds one, and coins there are reported to the administrators once
-- (unclaimed_alerted). An application erased with its account (retention) leaves its account with no application.
CREATE TABLE vendor_bond_accounts (
 id bigserial PRIMARY KEY,
 wallet_id bigint NOT NULL REFERENCES xmr_bond_wallets(id),
 account_index bigint NOT NULL CHECK (account_index BETWEEN 1 AND 4294967295),
 address text NOT NULL UNIQUE CHECK (address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 application_id text UNIQUE REFERENCES vendor_applications(id) ON DELETE SET NULL,
 unclaimed_alerted boolean NOT NULL DEFAULT false,
 created timestamptz NOT NULL DEFAULT now(),
 UNIQUE (wallet_id, account_index)
);

-- vendor_bond_deposits: one row per transaction and bond account (the outputs of one transaction to an account are
-- summed), written by the watcher on every pass; nothing else writes it. confirmations: 0 in the pool, -1 when the
-- wallet no longer lists it or saw a double spend, at most max(100, threshold). locked: it has an unlock time; never
-- counted. credited: counted toward the bond when the application was recorded funded. late: first recorded once the
-- application no longer waited for its bond (withdrawn, rejected, waived, recorded or approved). flagged: the
-- administrators were told of it (locked, or kept uncounted by a decided application). reorg_flagged: credited, it
-- later fell below the threshold; told once, and nothing about the application changes.
CREATE TABLE vendor_bond_deposits (
 id bigserial PRIMARY KEY,
 account_id bigint NOT NULL REFERENCES vendor_bond_accounts(id),
 txid text NOT NULL CHECK (txid ~ '^[0-9a-fA-F]{64}$'),
 amount bigint NOT NULL CHECK (amount > 0),
 confirmations bigint NOT NULL CHECK (confirmations >= -1),
 locked boolean NOT NULL DEFAULT false,
 late boolean NOT NULL DEFAULT false,
 credited boolean NOT NULL DEFAULT false,
 flagged boolean NOT NULL DEFAULT false,
 reorg_flagged boolean NOT NULL DEFAULT false,
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now(),
 UNIQUE (txid, account_id),
 CHECK (NOT (credited AND (late OR locked)))
);
CREATE INDEX vendor_bond_deposits_account ON vendor_bond_deposits(account_id);
