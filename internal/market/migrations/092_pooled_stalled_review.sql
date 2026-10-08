-- A-487: the drain panel names the fixed staff-review exit for a pooled deposit that remains unconfirmed.
-- The blocker predicates and seven kinds are unchanged from migration 091; only the watched-address detail changes.
CREATE OR REPLACE FUNCTION pooled_drain_blockers() RETURNS TABLE(kind text, ref text, order_id text, currency text, detail text)
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
              || ' awaits confirmation or staff review 14 days after the later of the horizon and first observation', '')
    FROM payment_addresses pa JOIN orders o ON o.id = pa.order_id
   WHERE o.state IN ('cancelled','resolved','completed')
     AND pooled_watch_horizon(o.state, o.updated, pa.created)
  UNION ALL
  SELECT 'watched_address', pa.address, pa.order_id, pa.currency,
         'until recorded deposit ' || pooled_pending_deposit(pa.currency, pa.address)
         || ' confirms, leaves the wallet, or reaches staff review 14 days after the later of the horizon and first observation; its watch horizon ended '
         || to_char(GREATEST(o.updated + interval '24 hours', pa.created + interval '30 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI "UTC"')
    FROM payment_addresses pa JOIN orders o ON o.id = pa.order_id
   WHERE o.state IN ('cancelled','resolved','completed')
     AND NOT pooled_watch_horizon(o.state, o.updated, pa.created)
     AND EXISTS (SELECT 1 FROM payments pm WHERE pm.currency = pa.currency AND pm.address = pa.address AND NOT pm.credited AND NOT pm.flagged AND pm.confirmations >= 0)
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
