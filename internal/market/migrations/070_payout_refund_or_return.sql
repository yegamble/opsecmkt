-- A refund or a return per order, never both (A-268, A-276). A return carries the excess of a release back to the
-- buyer; a refund already returns everything the buyer paid, so a refund beside a return would pay the excess
-- twice. enqueuePayout never queues a return without its release, and migration 069 refuses a release beside a
-- refund, but the database still accepted a refund beside a return: for example when an operator settling a 069
-- clash by hand deleted a release and kept its return. This index refuses it: at most one refund or return per
-- order (a release keeps its return; 064's UNIQUE (order_id, kind) still allows one of each kind).
--
-- A database that already holds a refund and a return for one order (possible only through hand-written SQL) is
-- not changed: this migration stops with an error naming the orders, the whole startup migration rolls back and
-- the server does not start. Nothing is deleted, because either payout may already have been sent; the operator
-- settles each order by hand as UPGRADING.md describes and starts the server again.
DO $$
DECLARE clash text;
BEGIN
  SELECT string_agg(order_id, ', ' ORDER BY order_id) INTO clash
    FROM (SELECT order_id FROM payouts WHERE kind IN ('refund','return') GROUP BY order_id HAVING count(*) > 1) o;
  IF clash IS NOT NULL THEN
    RAISE EXCEPTION 'payouts: order(s) % have both a refund and a return. Nothing was changed. Check in the wallet which of them was sent and settle each order by hand as UPGRADING.md describes (One settlement per order, migrations 069 and 070), then start the server again.', clash;
  END IF;
END $$;
CREATE UNIQUE INDEX payouts_refund_or_return ON payouts (order_id) WHERE kind IN ('refund','return');
