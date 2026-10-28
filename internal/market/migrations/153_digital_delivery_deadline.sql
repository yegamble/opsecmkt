-- A-709 (owner decision 2026-10-28 "Deadline + dispute window"): a paid digital Monero escrow order has a delivery
-- deadline, escrows.ship_by, 72 hours from payment (escrowTransitionHook); once it has passed with the order still paid,
-- the buyer may cancel it for a refund to the refund address. Orders paid before this release have none, so their
-- buyers could never cancel. This gives each such order, still paid now, a deadline 72 hours from the upgrade.
--
-- Data only: no column, constraint or function changes. Migration 079's trigger allows ship_by to be set from NULL
-- once. Physical orders, orders in any other state (completed ones included) and Bitcoin escrows are not touched.
UPDATE escrows es SET ship_by = now() + interval '72 hours'
  FROM orders o JOIN products p ON p.id = o.product_id
 WHERE o.id = es.order_id AND es.coin = 'XMR' AND o.state = 'paid' AND COALESCE(o.listing_kind, p.kind) = 'digital'
   AND es.ship_by IS NULL;
