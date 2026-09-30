-- A return needs its release (A-268). A return carries the excess of a release back to the buyer, so it is valid only
-- beside the release of the same order. enqueuePayout queues a return only together with its release, migration 069
-- refuses a release beside a refund and 070 a refund beside a return, but the database still accepted a return with
-- no release: for example when an operator settling a 069 clash by hand deleted a release and kept its return. This
-- constraint trigger refuses it. It is deferred to COMMIT, so one transaction may insert the release and its return
-- in either order, and it checks only the orders a row entered or left (INSERT, DELETE, UPDATE of kind or order_id),
-- through 064's unique index on (order_id, kind). Before checking it takes the transaction advisory lock 268073 and
-- keeps it until the transaction ends, so two transactions that each change one side of the same order (one adds a
-- return while the other deletes its release) check one after the other, and the second, reading after the first
-- committed, sees its row. It is one lock for every order: payouts are written rarely (the application writes them
-- only when it queues a settlement, under the order's row lock), and a transaction holds that one lock however many
-- rows it changes, where a lock per order would run out of lock-table memory on a large hand-written delete. This
-- makes the check race-free under READ COMMITTED, PostgreSQL's default, which the application and psql use
-- (SERIALIZABLE detects such a race by itself); a REPEATABLE READ transaction reads with its own earlier snapshot.
--
-- A later migration that writes payouts rows (INSERT, DELETE, or UPDATE of kind or order_id) and then alters or
-- indexes payouts in the same startup transaction fails with "pending trigger events": it must run
-- SET CONSTRAINTS payouts_return_needs_release IMMEDIATE after writing the rows.
--
-- A database that already holds a return without a release (possible only through hand-written SQL) is not changed:
-- this migration stops with an error naming the orders, the whole startup migration rolls back and the server does
-- not start. Nothing is deleted, because the return or the missing release may already have been sent; the operator
-- settles each order by hand as UPGRADING.md describes and starts the server again.
DO $$
DECLARE orphan text;
BEGIN
  SELECT string_agg(order_id, ', ' ORDER BY order_id) INTO orphan
    FROM (SELECT order_id FROM payouts GROUP BY order_id
          HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0) o;
  IF orphan IS NOT NULL THEN
    RAISE EXCEPTION 'payouts: order(s) % have a return but no release. Nothing was changed. Check in the wallet whether each return was sent and settle each order by hand as UPGRADING.md describes (A return needs its release, migration 073), then start the server again.', orphan;
  END IF;
END $$;
CREATE FUNCTION payouts_return_needs_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  affected text[] := '{}';
  o text;
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') THEN
    affected := affected || OLD.order_id;
  END IF;
  IF TG_OP = 'INSERT' OR (TG_OP = 'UPDATE' AND NEW.order_id IS DISTINCT FROM OLD.order_id) THEN
    affected := affected || NEW.order_id;
  END IF;
  PERFORM pg_advisory_xact_lock(268073);
  FOREACH o IN ARRAY affected LOOP
    IF EXISTS (SELECT 1 FROM payouts WHERE order_id = o AND kind = 'return')
       AND NOT EXISTS (SELECT 1 FROM payouts WHERE order_id = o AND kind = 'release') THEN
      RAISE EXCEPTION USING ERRCODE = 'check_violation', CONSTRAINT = 'payouts_return_needs_release', TABLE = 'payouts',
        MESSAGE = format('payouts: order %s has a return but no release. A return sends the excess of a release back to the buyer and is valid only beside the release of the same order, so this transaction was rolled back and nothing was changed. Settle the order as UPGRADING.md describes (A return needs its release, migration 073).', o);
    END IF;
  END LOOP;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER payouts_return_needs_release AFTER INSERT OR UPDATE OF kind, order_id OR DELETE ON payouts
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payouts_return_needs_release();
