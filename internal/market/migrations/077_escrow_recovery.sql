-- Bitcoin escrow, Phase 1 slice P1-d1 (WR-escrow A-246, docs/escrow.md): recovery and reconciliation (ES-6). Nothing
-- writes these while ESCROW_BTC is off, the default; escrow runs on regtest only.
--
-- A settlement with a final transaction moves only forward, with one exception for a reorganisation: signed ->
-- broadcast or external; broadcast -> confirmed or external; confirmed -> broadcast (its transaction dropped out of the
-- chain and is sent again) or external (a transaction that conflicts with it was mined instead). An external
-- settlement never changes again. Migration 076's settlements_frozen already refuses a final transaction going back to
-- a state before it existed; this trigger also refuses every other step back (confirmed -> signed, broadcast ->
-- signed) and any move out of external.
CREATE FUNCTION settlements_moves() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.state IS DISTINCT FROM OLD.state AND OLD.state IN ('signed','broadcast','confirmed','external') AND NOT (
       (OLD.state = 'signed' AND NEW.state IN ('broadcast','external'))
    OR (OLD.state = 'broadcast' AND NEW.state IN ('confirmed','external'))
    OR (OLD.state = 'confirmed' AND NEW.state IN ('broadcast','external'))) THEN
    RAISE EXCEPTION 'settlements: settlement % cannot move from % to %: a settlement with a final transaction moves only forward, or from confirmed back to broadcast after a reorganisation', OLD.id, OLD.state, NEW.state
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER settlements_moves BEFORE UPDATE OF state ON settlements FOR EACH ROW EXECUTE FUNCTION settlements_moves();

-- A confirmed settlement is followed until its transaction has 100 confirmations (reorg_safe_at records when it had),
-- so a reorganisation that takes it out of the chain returns it to broadcast. A settlement whose transaction the market
-- found already on the network when it came to complete it (A-344: the parties broadcast the transaction they both
-- signed, or a restored database held an older party-signed row) is recorded with cosigned 'network' when the stored
-- signatures do not show who signed it.
ALTER TABLE settlements
 ADD COLUMN reorg_safe_at timestamptz,
 ADD CONSTRAINT settlements_reorg_safe CHECK (reorg_safe_at IS NULL OR state = 'confirmed'),
 DROP CONSTRAINT settlements_cosigned_check,
 ADD CONSTRAINT settlements_cosigned_check CHECK (cosigned IN ('market','parties','network'));

-- An escrow output with no settlement of the market covering it that another transaction spent (the parties co-spent
-- it directly, say) is flagged for staff, with the spending transaction when the watch-only wallet names it.
ALTER TABLE payments
 ADD COLUMN spent_outside boolean NOT NULL DEFAULT false,
 ADD COLUMN spent_by text CHECK (spent_by ~ '^[0-9a-f]{64}$'),
 ADD CONSTRAINT payments_spent_by_outside CHECK (spent_by IS NULL OR spent_outside);

-- An address the escrow watch-only wallet holds with no escrow row: a descriptor imported for an escrow whose checkout
-- rolled back, or one created after the backup a restored database was made from. The escrow watcher lists them here
-- for administrators on its daily check (address, the wallet label's order ID, what the wallet says it received); a
-- row is removed once the address has an escrow row again or the wallet no longer holds it.
CREATE TABLE escrow_orphans (
 address text PRIMARY KEY CHECK (address ~ '^[A-Za-z0-9]{14,90}$'),
 order_id text NOT NULL DEFAULT '' CHECK (order_id ~ '^[0-9A-Za-z_-]{0,100}$'),
 received bigint NOT NULL DEFAULT 0 CHECK (received >= 0),
 first_seen timestamptz NOT NULL DEFAULT now(),
 last_seen timestamptz NOT NULL DEFAULT now()
);
