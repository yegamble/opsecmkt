-- A-301: every staff desk load with an open dispute reads when each holder of dispute evidence left staff, from the
-- role-change audit rows ("Role changed from OLD to NEW by …", A-294). audit_events grows with every sign-in and had
-- no index but its primary key, so that read scanned the whole table. This partial index holds only the role-change
-- rows; disputeEvidenceQuery repeats its predicate so the planner can use it. Additive only: no row changes, and the
-- signed audit export is unaffected. Building it reads audit_events once, at startup.
CREATE INDEX IF NOT EXISTS audit_events_role_change ON audit_events(user_id, created) WHERE action LIKE 'Role changed from %';
