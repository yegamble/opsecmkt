-- A-616 (PAY72-1/-3, STAFF72-3, DA72-N1). Migration 084 allowed one Monero stray refund in progress (requested, built or
-- relayed) per order, re-requests included. An administrator's re-request (A-401) of a failed stray refund carries out
-- by superseding it with a new stray refund in the same transaction, so while a first-generation stray refund of a later
-- deposit was in progress, the re-request's INSERT broke this index and failed on every pass (while staff were told it
-- waits for the market's next pass); and a re-request that waits (deposit-missing, A-614) held back the stray refund of
-- every later deposit for as long as it waited. A superseding stray refund no longer holds the slot: the index covers
-- first-generation stray refunds only (supersedes IS NULL), so at most one first-generation and any re-requested ones
-- are in progress together. Their coins stay disjoint without it: a first-generation stray refund claims only deposits
-- no settlement claims (xmr_settlement_id IS NULL, by compare-and-set when it is built), a re-request spends exactly the
-- key images of the settlement it supersedes (xmr_settlements_supersedes), and one built transaction per order at a time
-- (xmr_settlements_one_built) is relayed before another is built.
DROP INDEX xmr_settlements_one_open_stray;
CREATE UNIQUE INDEX xmr_settlements_one_open_stray ON xmr_settlements (order_id)
  WHERE kind = 'stray_refund' AND state IN ('requested','built','relayed') AND supersedes IS NULL;
