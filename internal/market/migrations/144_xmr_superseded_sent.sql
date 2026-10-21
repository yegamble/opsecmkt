-- A-566 (PAY66-3). An administrator's re-request (A-401, migration 084) supersedes a failed settlement that keeps its
-- stored transaction, and the new settlement spends exactly the same key images, so at most one of the two
-- transactions can ever be mined. The failed one may still reach the network after all: the daemon's refusal can follow
-- a relay that took effect, or a peer that kept it sends it back. The escrow watcher then records what happened: the
-- superseded settlement's stored transaction pays (it moves back to relayed, takes its deposit claims back and is
-- followed to confirmed and 100 confirmations like any other), and the re-request, which can now never be mined, is
-- closed as superseded without being built or relayed again (escrow_xmr_superseded.go). Migration 084 allowed neither
-- move: a superseded settlement never moved, and a built or relayed one was never superseded.
--
-- The new moves, in one transaction under the order row lock, the re-request first:
--   * a re-request (supersedes set) may move from built or relayed to superseded, but only if, by commit, a settlement
--     it re-requests (directly or through earlier re-requests) is relayed or confirmed (the deferred constraint trigger
--     xmr_settlements_superseded_sent): a stored transaction is closed only for one of the same coins that pays;
--   * a superseded settlement with a stored transaction may move back to relayed only while it has been re-requested
--     and every re-request after it is superseded too, so no other settlement of those coins is still in progress (a
--     stray refund's re-requests are not covered by xmr_settlements_one_outcome, so this is checked here for every kind).
-- A requested re-request already moves to superseded (084); without a transaction it is allowed for every kind now
-- (xmr_settlements_superseded), since a stray refund's re-request may be closed before it was built. Nothing else
-- changes, and no row is written here.
--
-- The order's one stray refund slot (xmr_settlements_one_open_stray, migration 135) no longer counts relayed rows
-- (PAY566-1). A revived first-generation stray refund moves back to relayed while a later deposit's first-generation
-- stray refund may already be requested beside its re-request (A-616 allows that), so the 135 predicate refused the
-- revival on every pass. The slot exists so that two first-generation stray refunds never claim the same deposits: a
-- relayed one's claims are fixed (payments.xmr_settlement_id, stored when it was built), a requested or built one's are
-- what the race is about, and xmr_settlements_one_built still lets one transaction per order be built at a time. The
-- watcher's own request gate (xmrStrayOpenSQL) still counts relayed, so it never requests a second one beside it.
--
-- One fragility: once a superseded settlement is revived, the re-request closed for it names a parent that is no
-- longer superseded, so xmr_settlements_supersedes (084) would refuse any later UPDATE of that closed row. No code
-- updates a superseded row today; a change that does must allow for it.
--
-- The constraint trigger is deferred to COMMIT: a later migration that moves xmr_settlements rows out of built or
-- relayed and then alters or indexes xmr_settlements in the same startup transaction must first run
-- SET CONSTRAINTS xmr_settlements_superseded_sent IMMEDIATE (as migration 073's trigger requires for payouts).
CREATE OR REPLACE FUNCTION xmr_settlements_moves() RETURNS trigger LANGUAGE plpgsql AS $$
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
    OR (OLD.state = 'failed' AND NEW.state = 'superseded' AND OLD.tx_hash IS NOT NULL)
    OR (OLD.state IN ('built','relayed') AND NEW.state = 'superseded' AND OLD.supersedes IS NOT NULL)
    OR (OLD.state = 'superseded' AND NEW.state = 'relayed' AND OLD.tx_hash IS NOT NULL
        AND EXISTS (SELECT 1 FROM xmr_settlements n WHERE n.supersedes = OLD.id)
        AND NOT EXISTS (
          WITH RECURSIVE later AS (
            SELECT id, state FROM xmr_settlements WHERE supersedes = OLD.id
            UNION ALL
            SELECT n.id, n.state FROM xmr_settlements n JOIN later l ON n.supersedes = l.id)
          SELECT 1 FROM later WHERE state <> 'superseded'))) THEN
    RAISE EXCEPTION 'xmr_settlements: settlement % cannot move from % to %: a settlement moves only forward, or from confirmed back to relayed after a reorganisation, or from failed to superseded when it was built and is re-requested, or back to relayed when its stored transaction reached the network after its re-requests, all closed (A-566)', OLD.id, OLD.state, NEW.state
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

ALTER TABLE xmr_settlements DROP CONSTRAINT xmr_settlements_superseded,
  ADD CONSTRAINT xmr_settlements_superseded CHECK (state <> 'superseded' OR kind IN ('release','refund') OR tx_hash IS NOT NULL OR supersedes IS NOT NULL);

CREATE FUNCTION xmr_settlements_superseded_sent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM xmr_settlements WHERE id = NEW.id AND state = 'superseded') AND NOT EXISTS (
      WITH RECURSIVE earlier AS (
        SELECT s.id, s.state, s.supersedes FROM xmr_settlements s WHERE s.id = NEW.supersedes
        UNION ALL
        SELECT s.id, s.state, s.supersedes FROM xmr_settlements s JOIN earlier e ON s.id = e.supersedes)
      SELECT 1 FROM earlier WHERE state IN ('relayed','confirmed')) THEN
    RAISE EXCEPTION 'xmr_settlements: re-request % is closed from % only when a settlement it re-requests is relayed or confirmed (A-566)', NEW.id, OLD.state
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER xmr_settlements_superseded_sent AFTER UPDATE OF state ON xmr_settlements DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW WHEN (OLD.state IN ('built','relayed') AND NEW.state = 'superseded')
  EXECUTE FUNCTION xmr_settlements_superseded_sent();

DROP INDEX xmr_settlements_one_open_stray;
CREATE UNIQUE INDEX xmr_settlements_one_open_stray ON xmr_settlements (order_id)
  WHERE kind = 'stray_refund' AND state IN ('requested','built') AND supersedes IS NULL;
