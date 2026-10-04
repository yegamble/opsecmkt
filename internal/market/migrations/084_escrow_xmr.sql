-- Monero escrow, Phase 3 slice X-a (WR-escrow A-248, P3-R1; docs/escrow.md): the tables Phase 3 needs, created together
-- so the phase adds no corrective migration, and the escrows and payments columns that let Monero escrows share the
-- Bitcoin escrow's pay-twice guards. Nothing reads or writes the new tables while ESCROW_XMR is off, the default; this
-- release runs Monero escrow on a local fakechain only. Every existing escrows row is a Bitcoin escrow and reads back
-- unchanged (coin 'BTC', its market index kept), and nothing here changes what Bitcoin escrow does.
--
-- ES-7, owner decision 2026-09-29: one escrow wallet per month on a dedicated monero-wallet-rpc, one account per order.
-- The escrow watcher creates each wallet and its accounts ahead of time (slots); checkout claims a free slot by
-- compare-and-set and makes no RPC; the watcher alone builds, stores and relays each settlement from the order's account.
-- Monero addresses here are 95 base58 characters (a standard address or a subaddress); an integrated address (106) is
-- never stored.

-- xmr_escrow_wallets: one per month, named escrow-YYYYMM-NN (NN counts that month's wallets). The watcher inserts the
-- row as creating before create_wallet, then stores the primary address, the restore height and the wallet's seed
-- sealed to the operator's pinned OpenPGP key (seed_key_fpr) together (sealed), before any slot of it can be claimed. A
-- sealed wallet becomes active at the start of its month and closing when it ends (it takes no new order and its free
-- slots are retired), retained once its last order closed (last_closed_at), and destroyed (its password replaced by one
-- never stored, seed_sealed wiped) no earlier than destroy_after, 90 days after its last order closed (ES-7, P3-R9). A
-- wallet the watcher cannot open is missing (P3-R9) until it opens again; one found on the wallet-rpc with no row (after
-- a restore from backup, P3-R10) is an orphan, re-sealed, and then retained until it is destroyed: inserted as one, or
-- a creating row whose name create_wallet finds already taken (creating -> orphan). States
-- move only forward: creating -> sealed -> active -> closing -> retained -> destroyed, with sealed -> closing (a wallet
-- closed before its month began, after a restore), any state but destroyed -> missing, missing back to any of sealed,
-- active, closing or retained (it opened again), orphan -> retained, and missing or orphan -> destroyed. A wallet that may hold an order's coins always has its sealed seed: the name, month, primary address,
-- restore height, seal key and seal time never change once set, the seal is removed only by destruction, last_closed_at
-- and destroy_after only move later once set, and a destroyed wallet never changes again. id stays below 2^30 so that a
-- Monero escrow's payments idx fits a bigint (see payments).
CREATE TABLE xmr_escrow_wallets (
 id bigserial PRIMARY KEY CHECK (id < 1073741824),
 name text NOT NULL UNIQUE CHECK (name ~ '^escrow-[0-9]{6}-[0-9]{2}$'),
 month date NOT NULL CHECK (month = date_trunc('month', month)),
 primary_address text UNIQUE CHECK (primary_address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 restore_height bigint CHECK (restore_height >= 0),
 seed_sealed text CHECK (seed_sealed <> ''),
 seed_key_fpr text CHECK (seed_key_fpr ~ '^[0-9A-F]{40}([0-9A-F]{24})?$'),
 sealed_at timestamptz,
 state text NOT NULL DEFAULT 'creating' CHECK (state IN ('creating','sealed','active','closing','retained','destroyed','missing','orphan')),
 last_closed_at timestamptz,
 destroy_after timestamptz,
 destroyed_at timestamptz,
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT xmr_escrow_wallets_name_month CHECK (substr(name, 8, 6) = to_char(month, 'YYYYMM')),
 CONSTRAINT xmr_escrow_wallets_sealed CHECK (state NOT IN ('sealed','active','closing','retained')
   OR (seed_sealed IS NOT NULL AND primary_address IS NOT NULL AND restore_height IS NOT NULL)),
 CONSTRAINT xmr_escrow_wallets_seal CHECK (seed_sealed IS NULL OR (seed_key_fpr IS NOT NULL AND sealed_at IS NOT NULL)),
 CONSTRAINT xmr_escrow_wallets_destroyed CHECK ((state = 'destroyed') = (destroyed_at IS NOT NULL)
   AND (state <> 'destroyed' OR (seed_sealed IS NULL AND destroy_after IS NOT NULL AND destroyed_at >= destroy_after))),
 -- 90 days, in hours so that the check does not depend on the session's time zone.
 CONSTRAINT xmr_escrow_wallets_retention CHECK (destroy_after IS NULL OR (last_closed_at IS NOT NULL AND destroy_after >= last_closed_at + interval '2160 hours'))
);

CREATE FUNCTION xmr_escrow_wallets_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.state = 'destroyed' OR NEW.name IS DISTINCT FROM OLD.name OR NEW.month IS DISTINCT FROM OLD.month
      OR (OLD.primary_address IS NOT NULL AND NEW.primary_address IS DISTINCT FROM OLD.primary_address)
      OR (OLD.restore_height IS NOT NULL AND NEW.restore_height IS DISTINCT FROM OLD.restore_height)
      OR (OLD.seed_key_fpr IS NOT NULL AND NEW.seed_key_fpr IS DISTINCT FROM OLD.seed_key_fpr)
      OR (OLD.sealed_at IS NOT NULL AND NEW.sealed_at IS DISTINCT FROM OLD.sealed_at)
      OR (OLD.seed_sealed IS NOT NULL AND NEW.seed_sealed IS DISTINCT FROM OLD.seed_sealed AND NOT (NEW.seed_sealed IS NULL AND NEW.state = 'destroyed')) THEN
    RAISE EXCEPTION 'xmr_escrow_wallets: wallet % keeps its name, address, restore height and sealed seed; the seal is removed only when the wallet is destroyed, and a destroyed wallet never changes', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  IF (OLD.last_closed_at IS NOT NULL AND NOT COALESCE(NEW.last_closed_at >= OLD.last_closed_at, false))
      OR (OLD.destroy_after IS NOT NULL AND NOT COALESCE(NEW.destroy_after >= OLD.destroy_after, false)) THEN
    RAISE EXCEPTION 'xmr_escrow_wallets: wallet % keeps its last close and destruction times; they only move later', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
       (OLD.state = 'creating' AND NEW.state IN ('sealed','missing','orphan'))
    OR (OLD.state = 'sealed' AND NEW.state IN ('active','closing','missing'))
    OR (OLD.state = 'active' AND NEW.state IN ('closing','missing'))
    OR (OLD.state = 'closing' AND NEW.state IN ('retained','missing'))
    OR (OLD.state = 'retained' AND NEW.state IN ('destroyed','missing'))
    OR (OLD.state = 'missing' AND NEW.state IN ('sealed','active','closing','retained','destroyed'))
    OR (OLD.state = 'orphan' AND NEW.state IN ('retained','missing','destroyed'))) THEN
    RAISE EXCEPTION 'xmr_escrow_wallets: wallet % cannot move from % to %: a wallet moves only forward', OLD.id, OLD.state, NEW.state USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_escrow_wallets_kept BEFORE UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION xmr_escrow_wallets_kept();

-- xmr_escrow_slots: one account of an escrow wallet and its address (the account's address 0, a subaddress). Account 0
-- is never a slot and never spent (account_index >= 1). The watcher creates slots free (create_account) ahead of
-- checkout; checkout claims a free slot of an active wallet by compare-and-set, naming the order, and inserts the escrow
-- in the same transaction; a free slot is retired when its wallet closes or after a restore. A claimed slot is its
-- order's for good, so an account is never used by two orders, and a retired one is never claimed; a slot's wallet,
-- account and address never change.
CREATE TABLE xmr_escrow_slots (
 id bigserial PRIMARY KEY,
 wallet_id bigint NOT NULL REFERENCES xmr_escrow_wallets(id),
 account_index bigint NOT NULL CHECK (account_index BETWEEN 1 AND 4294967295),
 address text NOT NULL UNIQUE CHECK (address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 state text NOT NULL DEFAULT 'free' CHECK (state IN ('free','claimed','retired')),
 order_id text UNIQUE REFERENCES orders(id),
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now(),
 UNIQUE (wallet_id, account_index),
 UNIQUE (id, order_id, address), -- the target of escrows_xmr_slot
 CONSTRAINT xmr_escrow_slots_claimed CHECK ((state = 'claimed') = (order_id IS NOT NULL))
);

CREATE FUNCTION xmr_escrow_slots_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state = 'claimed' THEN
      RAISE EXCEPTION 'xmr_escrow_slots: a slot is created free or retired; only checkout claims it' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.wallet_id IS DISTINCT FROM OLD.wallet_id OR NEW.account_index IS DISTINCT FROM OLD.account_index OR NEW.address IS DISTINCT FROM OLD.address
      OR (OLD.state <> 'free' AND (NEW.state IS DISTINCT FROM OLD.state OR NEW.order_id IS DISTINCT FROM OLD.order_id)) THEN
    RAISE EXCEPTION 'xmr_escrow_slots: slot % keeps its wallet, account and address, and a claimed or retired slot never changes', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.state = 'free' AND NEW.state = 'claimed' AND NOT EXISTS (SELECT 1 FROM xmr_escrow_wallets WHERE id = NEW.wallet_id AND state = 'active') THEN
    RAISE EXCEPTION 'xmr_escrow_slots: slot % is claimed only from an active wallet', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_escrow_slots_kept BEFORE INSERT OR UPDATE ON xmr_escrow_slots FOR EACH ROW EXECUTE FUNCTION xmr_escrow_slots_kept();

-- escrows: one table for both coins, because every pay-twice guard keys on it (payouts_not_escrow, the send claim,
-- Order.Escrow, the escrow-only order edges), so a Monero escrow that a Bitcoin query misses fails closed, where a guard
-- missing on a sibling table would pay twice; every Bitcoin escrow query that could read a Monero row filters on coin.
-- coin is the order's currency (escrows_coin) and never changes, nor does the currency of an order with an escrow. A
-- Bitcoin escrow has the arbiter key's child index (market_index) and no slot; a Monero escrow has no market index and
-- names its slot (xmr_slot_id): escrows.address is that slot's address, and the slot is claimed by the escrow's order
-- (escrows_xmr_slot), so checkout claims the slot before it inserts the escrow. A Monero escrow's refund and fee
-- addresses are Monero addresses.
ALTER TABLE escrows
 ADD COLUMN coin text NOT NULL DEFAULT 'BTC' CHECK (coin IN ('BTC','XMR')),
 ADD COLUMN xmr_slot_id bigint UNIQUE,
 ALTER COLUMN market_index DROP NOT NULL,
 ADD CONSTRAINT escrows_market_index_btc CHECK ((coin = 'BTC') = (market_index IS NOT NULL)),
 ADD CONSTRAINT escrows_xmr_slot_xmr CHECK ((coin = 'XMR') = (xmr_slot_id IS NOT NULL)),
 ADD CONSTRAINT escrows_xmr_slot FOREIGN KEY (xmr_slot_id, order_id, address) REFERENCES xmr_escrow_slots (id, order_id, address),
 ADD CONSTRAINT escrows_xmr_addresses CHECK (coin <> 'XMR' OR (refund_address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'
   AND (fee_destination = '' OR fee_destination ~ '^[1-9A-HJ-NP-Za-km-z]{95}$')));

CREATE FUNCTION escrows_coin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND (NEW.coin IS DISTINCT FROM OLD.coin OR NEW.order_id IS DISTINCT FROM OLD.order_id) THEN
    RAISE EXCEPTION 'escrows: escrow % keeps its coin and its order', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'INSERT' AND NEW.coin IS DISTINCT FROM (SELECT currency FROM orders WHERE id = NEW.order_id) THEN
    RAISE EXCEPTION 'escrows: the coin of an escrow is its order''s currency (order %)', NEW.order_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER escrows_coin BEFORE INSERT OR UPDATE OF coin, order_id ON escrows FOR EACH ROW EXECUTE FUNCTION escrows_coin();

CREATE FUNCTION orders_escrow_currency() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.currency IS DISTINCT FROM OLD.currency AND EXISTS (SELECT 1 FROM escrows WHERE order_id = OLD.id) THEN
    RAISE EXCEPTION 'orders: order % has an escrow; its currency, the escrow''s coin, never changes', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER orders_escrow_currency BEFORE UPDATE OF currency ON orders FOR EACH ROW EXECUTE FUNCTION orders_escrow_currency();

-- xmr_settlements: the settlements of Monero escrows (ES-1). Bitcoin's settlements table cannot hold them (a PSBT and
-- party signatures, no stored Monero transaction), and settlements and xmr_settlements each refuse the other coin's
-- escrow. The watcher alone builds each one from the order's account: requested (a vendor's claim, a refund to the
-- pinned refund address, a stray refund) -> built (the transaction made with do_not_relay and stored, with its hash,
-- metadata, blob, fee, input total and the key images it spends, before relay_tx) -> relayed -> confirmed (followed to
-- 100 confirmations, reorg_safe_at), or external (another transaction spent its key images) or failed; an unbuilt
-- release or refund that a changed dispute decision replaces is superseded. The stored transaction is written only by
-- the move requested -> built, never changes and is never rebuilt automatically: a relay is only ever repeated with the
-- stored metadata, which spends the same key images, so it cannot pay twice. States move only forward, apart from
-- confirmed -> relayed when a reorganisation drops it.
--
-- The administrator's re-request (owner decision O-1, A-401, P3-R6): a built settlement that failed (the daemon refused
-- its stored transaction for good, and no other transaction spent its key images) becomes superseded, keeping its stored
-- transaction, and in the same transaction a new row names it in supersedes, with the same order, kind and destination.
-- When the new row stores its transaction, its key images must be exactly the superseded row's, so at most one of the
-- two transactions can ever be mined. That its outputs pay the stored plan (the same destinations and amounts, only the
-- fee and the claimant's output changed by the fee) is checked in Go by the escrow watcher (X-d). A row is superseded at
-- most once; a relayed, confirmed or external one never is.
--
-- In a custodial wallet the market signs alone, so a transaction is stored only when the order's state fits the
-- settlement's kind (escrow_market_may_sign, migration 081: a release on a completed order or one decided for the
-- vendor, a refund on a cancelled order or one decided for the buyer, a stray refund on a closed order). At most one
-- live release or refund per order, one built transaction not yet relayed per order (two unrelayed transactions of
-- one account could spend the same outputs), one stray refund in progress per order, and a release destination is
-- never paid twice.
CREATE TABLE xmr_settlements (
 id bigserial PRIMARY KEY,
 order_id text NOT NULL REFERENCES orders(id),
 kind text NOT NULL CHECK (kind IN ('release','refund','stray_refund')),
 state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested','built','relayed','confirmed','external','failed','superseded')),
 destination text NOT NULL CHECK (destination ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 requested_by text REFERENCES users(id),
 tx_hash text UNIQUE CHECK (tx_hash ~ '^[0-9a-f]{64}$'),
 tx_metadata text CHECK (tx_metadata ~ '^([0-9a-f]{2})+$'),
 tx_blob text CHECK (tx_blob ~ '^([0-9a-f]{2})+$'),
 fee bigint CHECK (fee > 0),
 input_total bigint CHECK (input_total > 0),
 key_images text[] CHECK (cardinality(key_images) > 0 AND array_to_string(key_images, ',') ~ '^[0-9a-f]{64}(,[0-9a-f]{64})*$'),
 relayed_at timestamptz,
 confirmed_at timestamptz,
 reorg_safe_at timestamptz,
 supersedes bigint UNIQUE REFERENCES xmr_settlements(id),
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT xmr_settlements_tx CHECK ((tx_hash IS NULL) = (tx_metadata IS NULL) AND (tx_hash IS NULL) = (tx_blob IS NULL)
   AND (tx_hash IS NULL) = (fee IS NULL) AND (tx_hash IS NULL) = (input_total IS NULL) AND (tx_hash IS NULL) = (key_images IS NULL)),
 CONSTRAINT xmr_settlements_built CHECK (state NOT IN ('built','relayed','confirmed') OR tx_hash IS NOT NULL),
 CONSTRAINT xmr_settlements_unbuilt CHECK (state <> 'requested' OR tx_hash IS NULL),
 -- Superseded without a transaction: a release or refund replaced by a changed decision; with one: a failed settlement
 -- of any kind re-requested (xmr_settlements_moves allows superseded only from requested, or from failed with a tx).
 CONSTRAINT xmr_settlements_superseded CHECK (state <> 'superseded' OR kind IN ('release','refund') OR tx_hash IS NOT NULL),
 CONSTRAINT xmr_settlements_relayed CHECK (state NOT IN ('relayed','confirmed') OR relayed_at IS NOT NULL),
 CONSTRAINT xmr_settlements_confirmed CHECK (state <> 'confirmed' OR confirmed_at IS NOT NULL),
 CONSTRAINT xmr_settlements_reorg_safe CHECK (reorg_safe_at IS NULL OR state = 'confirmed')
);
CREATE UNIQUE INDEX xmr_settlements_one_outcome ON xmr_settlements (order_id) WHERE kind IN ('release','refund') AND state <> 'superseded';
CREATE UNIQUE INDEX xmr_settlements_one_built ON xmr_settlements (order_id) WHERE state = 'built';
CREATE UNIQUE INDEX xmr_settlements_one_open_stray ON xmr_settlements (order_id) WHERE kind = 'stray_refund' AND state IN ('requested','built','relayed');
-- A re-request pays the destination of the release it supersedes (xmr_settlements_supersedes), so only the first is counted.
CREATE UNIQUE INDEX xmr_settlements_release_destination ON xmr_settlements (destination) WHERE kind = 'release' AND supersedes IS NULL;

CREATE FUNCTION xmr_settlements_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.order_id IS DISTINCT FROM OLD.order_id OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.destination IS DISTINCT FROM OLD.destination
      OR NEW.requested_by IS DISTINCT FROM OLD.requested_by OR NEW.supersedes IS DISTINCT FROM OLD.supersedes THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % keeps its order, kind and destination', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.tx_hash IS NULL AND NEW.tx_hash IS NOT NULL AND NOT (OLD.state = 'requested' AND NEW.state = 'built') THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % stores its transaction only when it is built from requested', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.tx_hash IS NOT NULL AND (NEW.tx_hash IS DISTINCT FROM OLD.tx_hash OR NEW.tx_metadata IS DISTINCT FROM OLD.tx_metadata
      OR NEW.tx_blob IS DISTINCT FROM OLD.tx_blob OR NEW.fee IS DISTINCT FROM OLD.fee OR NEW.input_total IS DISTINCT FROM OLD.input_total
      OR NEW.key_images IS DISTINCT FROM OLD.key_images) THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % has its stored transaction; it is never rebuilt or changed', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlements_frozen BEFORE UPDATE ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_frozen();

CREATE FUNCTION xmr_settlements_moves() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'requested' THEN
      RAISE EXCEPTION 'xmr_settlements: a settlement is inserted as requested; only the escrow watcher builds it' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
       (OLD.state = 'requested' AND NEW.state IN ('built','external','failed','superseded'))
    OR (OLD.state = 'built' AND NEW.state IN ('relayed','external','failed'))
    OR (OLD.state = 'relayed' AND NEW.state IN ('confirmed','external','failed'))
    OR (OLD.state = 'confirmed' AND NEW.state IN ('relayed','external'))
    OR (OLD.state = 'failed' AND NEW.state = 'superseded' AND OLD.tx_hash IS NOT NULL)) THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % cannot move from % to %: a settlement moves only forward, or from confirmed back to relayed after a reorganisation, or from failed to superseded when it was built and is re-requested', OLD.id, OLD.state, NEW.state
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlements_moves BEFORE INSERT OR UPDATE OF state ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_moves();

CREATE FUNCTION xmr_settlements_market_fits() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.tx_hash IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.tx_hash IS NULL) AND NOT escrow_market_may_sign(NEW.kind, NEW.order_id) THEN
    RAISE EXCEPTION 'xmr_settlements: the market stores the transaction of settlement % only once its order''s state fits its kind (a release on a completed order or one decided for the vendor, a refund on a cancelled order or one decided for the buyer)', NEW.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlements_market_fits BEFORE INSERT OR UPDATE ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_market_fits();

CREATE FUNCTION xmr_settlements_supersedes() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s record;
BEGIN
  IF NEW.supersedes IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT order_id, kind, destination, state, tx_hash, key_images INTO s FROM xmr_settlements WHERE id = NEW.supersedes;
  IF s.order_id IS DISTINCT FROM NEW.order_id OR s.kind IS DISTINCT FROM NEW.kind OR s.destination IS DISTINCT FROM NEW.destination
      OR s.state IS DISTINCT FROM 'superseded' OR s.tx_hash IS NULL THEN
    RAISE EXCEPTION 'xmr_settlements: a re-request of settlement % keeps its order, kind and destination, and follows it once it failed after it was built and is superseded', NEW.supersedes
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.tx_hash IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.tx_hash IS NULL) AND NOT (NEW.key_images @> s.key_images AND NEW.key_images <@ s.key_images) THEN
    RAISE EXCEPTION 'xmr_settlements: the re-request of settlement % spends exactly its key images', NEW.supersedes USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlements_supersedes BEFORE INSERT OR UPDATE ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_supersedes();

CREATE FUNCTION xmr_settlements_xmr_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM escrows WHERE order_id = NEW.order_id AND coin = 'XMR') THEN
    RAISE EXCEPTION 'xmr_settlements: order % has no Monero escrow', NEW.order_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlements_xmr_only BEFORE INSERT OR UPDATE OF order_id ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION xmr_settlements_xmr_only();

CREATE FUNCTION settlements_btc_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM escrows WHERE order_id = NEW.order_id AND coin <> 'BTC') THEN
    RAISE EXCEPTION 'settlements: order % has a Monero escrow; its settlements are xmr_settlements', NEW.order_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlements_btc_only BEFORE INSERT OR UPDATE OF order_id ON settlements FOR EACH ROW EXECUTE FUNCTION settlements_btc_only();

-- xmr_settlement_outputs: the plan of each settlement, in destination order: the claimant (the vendor of a release, the
-- buyer of a refund or stray refund) and, on a release, the market fee and the return of any overpayment to the buyer's
-- refund address, in piconero. Written while the settlement is requested, before its transaction is built; never changed.
CREATE TABLE xmr_settlement_outputs (
 settlement_id bigint NOT NULL REFERENCES xmr_settlements(id),
 n integer NOT NULL CHECK (n >= 0),
 role text NOT NULL CHECK (role IN ('claimant','market_fee','buyer_return')),
 address text NOT NULL CHECK (address ~ '^[1-9A-HJ-NP-Za-km-z]{95}$'),
 amount bigint NOT NULL CHECK (amount > 0),
 PRIMARY KEY (settlement_id, n),
 UNIQUE (settlement_id, role)
);

CREATE FUNCTION xmr_settlement_outputs_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP <> 'INSERT' OR NOT EXISTS (SELECT 1 FROM xmr_settlements WHERE id = NEW.settlement_id AND state = 'requested') THEN
    RAISE EXCEPTION 'xmr_settlement_outputs: outputs are written only while their settlement is requested' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_settlement_outputs_frozen BEFORE INSERT OR UPDATE OR DELETE ON xmr_settlement_outputs FOR EACH ROW EXECUTE FUNCTION xmr_settlement_outputs_frozen();

-- No escrow record is ever deleted (the application deletes none): a deleted escrows row would turn its order into a
-- pooled one, which payouts_not_escrow and the send claim no longer guard, and a deleted wallet, slot or settlement row
-- would lose the seal, the account or the stored transaction of coins the market holds. Outputs are refused above.
CREATE FUNCTION escrow_records_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%: escrow records are never deleted', TG_TABLE_NAME USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER escrows_not_deleted BEFORE DELETE ON escrows FOR EACH ROW EXECUTE FUNCTION escrow_records_kept();
CREATE TRIGGER xmr_escrow_wallets_not_deleted BEFORE DELETE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION escrow_records_kept();
CREATE TRIGGER xmr_escrow_slots_not_deleted BEFORE DELETE ON xmr_escrow_slots FOR EACH ROW EXECUTE FUNCTION escrow_records_kept();
CREATE TRIGGER xmr_settlements_not_deleted BEFORE DELETE ON xmr_settlements FOR EACH ROW EXECUTE FUNCTION escrow_records_kept();

-- payments stays the ledger for both coins (ES-1). A Monero escrow deposit is one row per transaction and account, with
-- idx = 2^62 + wallet_id * 2^32 + account_index. A pooled Monero row's idx is its subaddress minor index (below 2^32),
-- so the two never share (currency, txid, idx), the ledger's unique key, even when one transaction pays both; and the
-- minor index cannot tell two escrow accounts apart, since every account's escrow address is its address 0.
-- wallet_id < 2^30 and account_index < 2^32 keep idx within a bigint. Every Monero row of a Monero escrow order, and
-- every row in that range, must name the account of the slot its order claimed (payments_xmr_escrow_idx): a row keyed
-- on a minor index, or naming another order's account, is refused, never merged into another row, and the escrow
-- watcher flags the refusal to staff. A payments row's Monero settlement is xmr_settlement_id (settlement_id is a
-- Bitcoin settlement's).
ALTER TABLE payments
 ADD COLUMN xmr_settlement_id bigint REFERENCES xmr_settlements(id),
 ADD CONSTRAINT payments_xmr_settlement CHECK (xmr_settlement_id IS NULL OR (currency = 'XMR' AND idx >= 4611686018427387904));

CREATE FUNCTION payments_xmr_escrow_idx() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.currency = 'XMR' AND (NEW.idx >= 4611686018427387904 OR EXISTS (SELECT 1 FROM escrows WHERE order_id = NEW.order_id AND coin = 'XMR'))
      AND NOT EXISTS (SELECT 1 FROM xmr_escrow_slots s
        WHERE s.order_id = NEW.order_id AND NEW.idx = 4611686018427387904 + s.wallet_id * 4294967296 + s.account_index) THEN
    RAISE EXCEPTION 'payments: Monero output % of transaction % is not keyed on order %''s escrow account; it is never recorded under another key or for another order', NEW.idx, NEW.txid, NEW.order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payments_xmr_escrow_idx';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payments_xmr_escrow_idx BEFORE INSERT OR UPDATE OF currency, txid, idx, order_id ON payments FOR EACH ROW EXECUTE FUNCTION payments_xmr_escrow_idx();
