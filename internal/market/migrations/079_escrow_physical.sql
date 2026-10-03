-- Bitcoin escrow, Phase 2 slice P2-a (WR-escrow A-247, A-378; docs/escrow.md): what a physical escrow order needs to
-- measure its two clocks. Nothing writes the escrow columns while ESCROW_BTC is off, the default; escrow runs on
-- regtest only.
--
-- The vendor's dispatch window is a listing field (1 to 30 days; NULL = none set, so a physical listing without one
-- takes no escrow checkout, and a digital listing never has one). Each escrow snapshots the window and the
-- administrator's release deadline length (setting escrow_release_days, 7 to 30 days) at checkout; a later change to
-- the listing or the setting never reaches an existing escrow. The columns for the two deadlines themselves
-- (ship_by when the order is paid, release_at when it is marked shipped) and the reminder claim are written by later
-- slices; they are nullable because an escrow of a digital listing has none of these values.
ALTER TABLE products ADD COLUMN dispatch_days smallint CHECK (dispatch_days BETWEEN 1 AND 30);

ALTER TABLE escrows
 ADD COLUMN release_days smallint CHECK (release_days BETWEEN 7 AND 30),
 ADD COLUMN dispatch_days smallint CHECK (dispatch_days BETWEEN 1 AND 30),
 ADD COLUMN ship_by timestamptz,
 ADD COLUMN release_at timestamptz,
 ADD COLUMN reminded_at timestamptz,
 ADD COLUMN release_recheck boolean NOT NULL DEFAULT false;

-- Once set, the snapshots and the deadlines are frozen: a party relies on the dates it was shown. The one exception
-- is for the recheck a restore from backup makes (a later slice): release_at may move later, never earlier and never
-- to NULL, in the same statement that sets release_recheck from false to true.
CREATE FUNCTION escrows_deadlines_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.release_days IS NOT NULL AND NEW.release_days IS DISTINCT FROM OLD.release_days THEN
    RAISE EXCEPTION 'escrows: escrow % has its release deadline length; it is never changed', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.dispatch_days IS NOT NULL AND NEW.dispatch_days IS DISTINCT FROM OLD.dispatch_days THEN
    RAISE EXCEPTION 'escrows: escrow % has its dispatch window; it is never changed', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.ship_by IS NOT NULL AND NEW.ship_by IS DISTINCT FROM OLD.ship_by THEN
    RAISE EXCEPTION 'escrows: escrow % has its ship-by time; it is never changed', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.release_at IS NOT NULL AND NEW.release_at IS DISTINCT FROM OLD.release_at
      AND NOT (COALESCE(NEW.release_at > OLD.release_at, false) AND NOT OLD.release_recheck AND NEW.release_recheck) THEN
    RAISE EXCEPTION 'escrows: escrow % has its release time; it is moved only later, by a restore recheck', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER escrows_deadlines_frozen BEFORE UPDATE ON escrows FOR EACH ROW EXECUTE FUNCTION escrows_deadlines_frozen();
