-- A-130 first slice: a successful authenticated form's redirect is committed with its changes.
-- The opaque session and request fingerprints contain no form text, credentials or message body.
CREATE TABLE IF NOT EXISTS form_submissions (
 nonce text PRIMARY KEY CHECK (nonce ~ '^[0-9a-f]{32}$'),
 user_id text NOT NULL,
 session_hash text NOT NULL,
 action_path text NOT NULL,
 request_hmac text NOT NULL,
 redirect text NOT NULL,
 notice text NOT NULL DEFAULT '',
 expires timestamptz NOT NULL DEFAULT now() + interval '24 hours'
);
CREATE INDEX IF NOT EXISTS form_submissions_expires ON form_submissions(expires);
