-- A-683 (STAFF94-1): the moderation worker survives a database outage and the private panel can tell when it stopped.
--
-- pass_interval_seconds is the controller's --interval, written with every pass status (10 seconds to 1 hour, as the
-- flag allows). The panel and the /admin overview warn once the last pass is older than three intervals plus five
-- minutes; a status written before this migration has none, and the one-minute default applies.
-- db_outage_from and db_outage_until record the last database outage a worker recovered from: the start of its first
-- failed pass (the worker's clock) and the end of the pass that reported it (the database's). The pass that reports
-- it also writes failure_class 'db_unavailable'; later passes keep these two columns.
--
-- New nullable columns only: no existing row, column or function changes.
ALTER TABLE moderation_worker_status
  ADD COLUMN pass_interval_seconds integer CHECK (pass_interval_seconds BETWEEN 10 AND 3600),
  ADD COLUMN db_outage_from timestamptz,
  ADD COLUMN db_outage_until timestamptz;
