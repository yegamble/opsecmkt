-- One settlement per order (A-218). Migration 064 replaced UNIQUE (order_id) with UNIQUE (order_id, kind) so a
-- release can carry a 'return' of the excess, which also let a release and a refund for the same order both be
-- inserted. enqueuePayout never queues a second settlement, but the database no longer refused one. This index
-- refuses it again: at most one release or refund per order; a release keeps its return (064's key still allows
-- one of each kind).
--
-- A database that already holds a release and a refund for one order (possible only through hand-written SQL)
-- is not changed: this migration stops with an error naming the orders, the whole startup migration rolls back
-- and the server does not start. Nothing is deleted, because either payout may already have been sent; the
-- operator settles each order by hand as UPGRADING.md describes and starts the server again.
DO $$
DECLARE clash text;
BEGIN
  SELECT string_agg(order_id, ', ' ORDER BY order_id) INTO clash
    FROM (SELECT order_id FROM payouts WHERE kind IN ('release','refund') GROUP BY order_id HAVING count(*) > 1) o;
  IF clash IS NOT NULL THEN
    RAISE EXCEPTION 'payouts: order(s) % have both a release and a refund. Nothing was changed. Check in the wallet which of them was sent and settle each order by hand as UPGRADING.md describes (One settlement per order, migration 069), then start the server again.', clash;
  END IF;
END $$;
CREATE UNIQUE INDEX payouts_one_settlement ON payouts (order_id) WHERE kind IN ('release','refund');
