-- An administrator's confirmed app-host reboot request is committed and audited
-- before the app contacts the narrow host-control socket. There is no outbox
-- worker: uncertain or interrupted dispatch is never retried automatically.
CREATE TABLE host_reboot_requests (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
 user_id text REFERENCES users(id) ON DELETE SET NULL,
 state text NOT NULL CHECK (state IN ('recorded','dispatching','accepted','unknown')),
 requested_at timestamptz NOT NULL,
 accepted_at timestamptz,
 reboot_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK ((accepted_at IS NULL) = (reboot_at IS NULL))
);
CREATE INDEX host_reboot_requests_requested ON host_reboot_requests(requested_at DESC,id DESC);
