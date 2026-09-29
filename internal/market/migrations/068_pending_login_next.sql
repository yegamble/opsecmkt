-- A-126: next is the page a pending sign-in lands on once /challenge completes it: a return target already checked
-- by signInNext when /login stored it (and checked again when it is used), or '' for the marketplace home.
ALTER TABLE pending_logins ADD COLUMN IF NOT EXISTS next text NOT NULL DEFAULT '';
