-- P1 Authentication: administrator account suspension (A-27). A suspended account keeps its data, role,
-- listings and orders, but cannot sign in and none of its sessions authenticate. NULL = not suspended.
ALTER TABLE users ADD COLUMN IF NOT EXISTS suspended_at timestamptz;
CREATE INDEX IF NOT EXISTS users_suspended ON users(suspended_at) WHERE suspended_at IS NOT NULL;
