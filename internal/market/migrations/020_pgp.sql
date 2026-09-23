-- P2 PGP identity: key fingerprints, ownership proofs, PGP second factor, message inspection results.
-- Keys saved before this migration keep an empty fingerprint and are unverified; pages derive the
-- fingerprint by parsing the saved key, and verification stores it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS pgp_fingerprint text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS pgp_verified_at timestamptz;
ALTER TABLE users ADD COLUMN IF NOT EXISTS pgp_2fa boolean NOT NULL DEFAULT false;

-- One open ownership challenge per user. sign: challenge is the exact text to sign.
-- decrypt: challenge is the armored message; only the sha256 of its plaintext nonce is stored.
CREATE TABLE IF NOT EXISTS pgp_challenges (
 user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK (kind IN ('sign','decrypt')),
 fingerprint text NOT NULL,
 challenge text NOT NULL,
 nonce_hash text NOT NULL DEFAULT '',
 expires timestamptz NOT NULL,
 created timestamptz NOT NULL DEFAULT now()
);

-- PGP sign-in code for a pending login: sha256 of the one-time code and the armored message carrying it.
ALTER TABLE pending_logins ADD COLUMN IF NOT EXISTS pgp_nonce text;
ALTER TABLE pending_logins ADD COLUMN IF NOT EXISTS pgp_challenge text;

-- encrypted NULL = stored before packet inspection existed; the messages page inspects those bodies when rendering.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS encrypted boolean;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS recipient_match text NOT NULL DEFAULT 'unknown';
ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_recipient_match_check;
ALTER TABLE messages ADD CONSTRAINT messages_recipient_match_check CHECK (recipient_match IN ('yes','no','unknown'));
