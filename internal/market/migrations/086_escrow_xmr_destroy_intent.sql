-- Monero escrow, Phase 3 (WR-escrow A-248, P3-R9, A-419; docs/escrow.md): typed storage for the market's own decision
-- to destroy an escrow wallet. Nothing writes it while ESCROW_XMR is off, the default; Monero escrow runs on a local
-- fakechain only.
--
-- xmr_escrow_wallets.destroy_started_at: when the watcher, every precondition of destruction met (destroy_after passed,
-- nothing open, followed or left in the wallet), began destroying the wallet. It is committed before the wallet's
-- password is replaced (change_wallet_password), so a wallet that then refuses to open as "Invalid password" is known
-- to be one the market itself was destroying (a crash, or a lost record, between the new password and the record of
-- the destruction), and the watcher may destroy it again (escrow_xmr_retain.go). Any other wallet that refuses its
-- password (restored with a mistyped secret, another install's file, a damaged keys file) keeps its seal until an
-- administrator confirms its destruction. It is set once, only on a retained wallet past destroy_after whose seal is
-- present, at a time no earlier than destroy_after; once set it never changes or clears, and the wallet then moves only
-- to destroyed or missing, or from missing back to retained: a wallet that opens again with the market's password (the
-- password change never took effect) is retained, and its destruction re-runs every precondition. A row is never
-- inserted with it set.
ALTER TABLE xmr_escrow_wallets ADD COLUMN destroy_started_at timestamptz;

CREATE FUNCTION xmr_escrow_wallets_destroy_started() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.destroy_started_at IS NOT NULL THEN
      RAISE EXCEPTION 'xmr_escrow_wallets: a wallet is inserted with no destruction started'
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.destroy_started_at IS NULL THEN
    IF NEW.destroy_started_at IS NOT NULL AND NOT (OLD.state = 'retained' AND NEW.state = 'retained' AND NEW.seed_sealed IS NOT NULL
        AND NEW.destroy_after IS NOT NULL AND NEW.destroy_started_at >= NEW.destroy_after) THEN
      RAISE EXCEPTION 'xmr_escrow_wallets: wallet % starts its destruction only while retained past destroy_after with its sealed seed', OLD.id
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.destroy_started_at IS DISTINCT FROM OLD.destroy_started_at
      OR (NEW.state IS DISTINCT FROM OLD.state AND NEW.state NOT IN ('destroyed','missing') AND NOT (OLD.state = 'missing' AND NEW.state = 'retained')) THEN
    RAISE EXCEPTION 'xmr_escrow_wallets: wallet % keeps the start of its destruction and moves only to destroyed or missing, or from missing back to retained', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER xmr_escrow_wallets_destroy_started BEFORE INSERT OR UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION xmr_escrow_wallets_destroy_started();
