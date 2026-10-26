-- A-652c (PAY87-3, owner decision 2026-10-25 "refund + hold"): a release to a banned vendor is held until the ban is
-- lifted; refunds are paid as usual, and no other address is ever paid. The escrow watcher holds such a release before
-- it asks the wallet anything (escrow_xmr_settle.go, xmrReleaseHeld, send_error vendor-closed); this is the database
-- backstop. escrow_market_may_sign (migration 081), which xmr_settlements_market_fits (084) and the Bitcoin settlement
-- and fee bump triggers (081) read before a transaction is stored, now also requires, for a release, that the vendor
-- of the order's listing is not banned (users.banned_at, migration 123). It is read by the storing statement itself,
-- so a ban committed before that statement refuses the store even in a transaction begun before it; a release stored
-- before the ban is not checked again, and is relayed and followed as before. Lifting the ban (banned_at NULL) allows
-- it again with no other step. Refunds and stray refunds are unchanged. Only the function's body changes: the same
-- signature, language and volatility, no index, kind, output role, column or row.
CREATE OR REPLACE FUNCTION escrow_market_may_sign(kind text, order_id text) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT COALESCE((SELECT CASE kind
      WHEN 'release' THEN (o.state = 'completed' OR (o.state = 'resolved' AND COALESCE(d.outcome, '') = 'release'))
        AND NOT EXISTS (SELECT 1 FROM products p JOIN users v ON v.id = p.vendor_id WHERE p.id = o.product_id AND v.banned_at IS NOT NULL)
      WHEN 'refund' THEN o.state = 'cancelled' OR (o.state = 'resolved' AND COALESCE(d.outcome, '') = 'refund')
      WHEN 'stray_refund' THEN o.state IN ('completed', 'cancelled', 'resolved')
      ELSE false END
    FROM orders o LEFT JOIN disputes d ON d.order_id = o.id WHERE o.id = escrow_market_may_sign.order_id), false)
$$;
