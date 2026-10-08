-- A-445 (war-room ruling P4-R9): the pooled wallet's drain gate. Three read-only functions; no table, column or row
-- changes. See UPGRADING.md section 46.

-- pooled_watch_horizon is the pooled watcher's time rule for a deposit address (payments_watcher.go, pollCurrency): the
-- address of an order that is not a draft stays watched for 24 hours after the order last changed state (its
-- terminal time, for an order that closed), and the address of a cancelled, resolved or completed order for 30 days
-- after it was issued. The watcher also reads the address of every order still awaiting payment or funded and not
-- closed, and of every order with a payout pending or held; those are open orders and unsent payouts, which
-- pooled_drain_blockers lists in their own right. The watcher calls this function, so the drain gate and the watcher
-- share one horizon: a deposit sent to an address past it is never read, never recorded and never blocks the drain.
CREATE FUNCTION pooled_watch_horizon(order_state text, order_updated timestamptz, issued timestamptz) RETURNS boolean
  LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT (order_state <> 'draft' AND order_updated > now() - interval '24 hours')
      OR (order_state IN ('cancelled','resolved','completed') AND issued > now() - interval '30 days')
$$;

-- pooled_pending_deposit names (txid:index) the first deposit recorded at the pooled address addr of currency cur that
-- is still pending: neither credited (counted for its order's payment or settlement) nor flagged for review, and not
-- ended by the watcher's missing or conflict handling (a row the wallet no longer reports, or reports conflicted, has
-- negative confirmations). NULL when there is none. The watcher keeps reading such an address after the horizon
-- (A-486, ruling R1), so the deposit is flagged once it confirms (flagLateDeposits) and then closed through the review
-- disposition, or ends with negative confirmations once the wallet drops, replaces or reorganises it out; the drain
-- gate lists the address meanwhile. Both call this function, so they share one definition.
CREATE FUNCTION pooled_pending_deposit(cur text, addr text) RETURNS text
  LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT pm.txid || ':' || pm.idx FROM payments pm
   WHERE pm.currency = cur AND pm.address = addr AND NOT pm.credited AND NOT pm.flagged AND pm.confirmations >= 0
   ORDER BY pm.id LIMIT 1
$$;

-- pooled_drain_blockers lists what still needs the pooled wallet, one row per blocker: its kind, a reference staff can
-- act on (ref), the order it concerns (NULL when none), its currency (NULL when none) and a detail. The list being
-- empty, not the wallet's balance, is what says the pooled wallet is drained. Read-only (STABLE): it writes nothing.
-- A pooled order is one without an escrows row (Order.Escrow; every payment_addresses row and every payouts row is
-- pooled: only the pooled payment request writes the one, and migration 074 refuses the other on an escrow order).
--   open_order          an order that is neither a draft (no deposit address yet) nor cancelled, resolved or completed;
--                       ref is the order id, detail its state. It is closed by its parties, a dispute or the watcher.
--   unsent_payout       a payout in any state but sent (blocked, held, pending, sending, failed); ref is the payout id,
--                       detail its kind and state. Sent by the watcher, or resolved on /admin: release, requeue, mark
--                       sent with the wallet's transaction ID, or, for a blocked one, record it sent by hand.
--   flagged_payment     a deposit of a pooled order whose payment-review flag is open: paymentFlagOpenRule
--                       (payment_review.go) as it stood when this migration was written (flagged, disposition '' or
--                       'escalated', and still recordable), so every one can be closed through the existing
--                       disposition on the moderation desk; ref is the payments id, detail txid:index.
--   watched_address     a deposit address of a cancelled, resolved or completed pooled order the watcher still reads:
--                       inside the watch horizon (pooled_watch_horizon), or past it while a recorded deposit there is
--                       pending (pooled_pending_deposit); ref is the address, detail until when (the time the horizon
--                       ends, UTC, and the pending deposit when there is one).
--   recovery_required   the restore gate (settings payments_recovery_required = 'true', scripts/restore.sh); ref is
--                       the settings key. Cleared by the operator after reconciling the restore.
--   address_collision   a currency whose wallet handed out already-issued addresses and whose /admin warning is not
--                       dismissed (payment_status.address_collisions, migration 071); ref is the currency, detail the count.
--   mainnet_history     a currency with deposit addresses issued on mainnet (payment_addresses.provider's network part
--                       main or mainnet, as checkNetworkHistory reads it); ref is the currency, detail the count. No
--                       release has a mainnet escrow, so such history always blocks; the release that adds mainnet
--                       escrow replaces this function.
CREATE FUNCTION pooled_drain_blockers() RETURNS TABLE(kind text, ref text, order_id text, currency text, detail text)
  LANGUAGE sql STABLE AS $$
  SELECT 'open_order', o.id, o.id, o.currency, o.state
    FROM orders o
   WHERE o.state NOT IN ('draft','cancelled','resolved','completed')
     AND NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id = o.id)
  UNION ALL
  SELECT 'unsent_payout', p.id::text, p.order_id, p.currency, p.kind || ' ' || p.state
    FROM payouts p
   WHERE p.state <> 'sent'
  UNION ALL
  SELECT 'flagged_payment', pm.id::text, pm.order_id, pm.currency, pm.txid || ':' || pm.idx
    FROM payments pm
   WHERE NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id = pm.order_id)
     AND pm.flagged AND pm.disposition IN ('','escalated') AND (pm.locked OR pm.below_floor OR pm.regress_notice OR pm.spent_outside
	OR pm.escrow_left_out OR NOT pm.credited AND (pm.after_close OR NOT EXISTS (SELECT 1 FROM escrows es WHERE es.order_id=pm.order_id)))
  UNION ALL
  SELECT 'watched_address', pa.address, pa.order_id, pa.currency,
         'until ' || to_char(GREATEST(o.updated + interval '24 hours', pa.created + interval '30 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI "UTC"')
         || COALESCE(', and after that while recorded deposit ' || pooled_pending_deposit(pa.currency, pa.address)
              || ' awaits confirmation', '')
    FROM payment_addresses pa JOIN orders o ON o.id = pa.order_id
   WHERE o.state IN ('cancelled','resolved','completed')
     AND pooled_watch_horizon(o.state, o.updated, pa.created)
  UNION ALL
  SELECT 'watched_address', pa.address, pa.order_id, pa.currency,
         'until recorded deposit ' || pooled_pending_deposit(pa.currency, pa.address)
         || ' confirms (it is then flagged for review) or leaves the wallet; its watch horizon ended '
         || to_char(GREATEST(o.updated + interval '24 hours', pa.created + interval '30 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI "UTC"')
    FROM payment_addresses pa JOIN orders o ON o.id = pa.order_id
   WHERE o.state IN ('cancelled','resolved','completed')
     AND NOT pooled_watch_horizon(o.state, o.updated, pa.created)
     AND pooled_pending_deposit(pa.currency, pa.address) IS NOT NULL
  UNION ALL
  SELECT 'recovery_required', s.key, NULL, NULL, ''
    FROM settings s
   WHERE s.key = 'payments_recovery_required' AND s.value = 'true'
  UNION ALL
  SELECT 'address_collision', ps.currency, NULL, ps.currency, ps.address_collisions::text
    FROM payment_status ps
   WHERE ps.address_collisions > 0
  UNION ALL
  SELECT 'mainnet_history', pa.currency, NULL, pa.currency, count(*)::text
    FROM payment_addresses pa
   WHERE split_part(pa.provider, '-', 2) IN ('main','mainnet')
   GROUP BY pa.currency
$$;
