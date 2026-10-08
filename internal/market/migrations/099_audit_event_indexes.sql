-- A-143: account history is ordered by created/id, while the administrator's audit view scans newest-first
-- across accounts. Keep both common orderings index-backed as audit_events grows.
CREATE INDEX IF NOT EXISTS audit_events_user_created_id ON audit_events(user_id, created DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_events_created_id ON audit_events(created DESC, id DESC);
