-- Branding has one current version. Replacing/resetting it removes the previous bytes;
-- listing-image ownership, publication and retention rules are deliberately separate.
CREATE TABLE market_branding (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  version text NOT NULL UNIQUE CHECK (version ~ '^[0-9a-f]{32}$'),
  content_type text NOT NULL CHECK (content_type IN ('image/jpeg','image/svg+xml')),
  body bytea NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 2097152),
  width integer NOT NULL CHECK (width BETWEEN 1 AND 1200),
  height integer NOT NULL CHECK (height BETWEEN 1 AND 1200),
  updated_by text REFERENCES users(id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- A separately authorized upload capability carries no reusable administrator
-- credential. Only its hash and a sealed copy for the private handoff are kept.
CREATE TABLE branding_upload_tickets (
  user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
  sealed_token text NOT NULL,
  session_hash text NOT NULL CHECK (session_hash ~ '^[0-9a-f]{64}$'),
  admitted boolean NOT NULL DEFAULT false,
  expires timestamptz NOT NULL,
  created timestamptz NOT NULL DEFAULT now()
);
