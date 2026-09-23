-- P1 Authentication: TOTP second factor, one-time recovery codes, image CAPTCHA.
-- totp_secret/totp_pending hold values sealed with a.seal("totp", ...); empty = none.
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_pending text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_enabled boolean NOT NULL DEFAULT false;
-- Highest accepted 30-second step; codes for this step or earlier are rejected (replay guard).
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step bigint NOT NULL DEFAULT 0;
-- Freshly generated recovery codes, sealed (label "recovery"), shown once on the next /totp view then cleared.
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_reveal text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS recovery_reveal_until timestamptz;

CREATE TABLE IF NOT EXISTS recovery_codes (
 code_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 used_at timestamptz, created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS recovery_codes_user ON recovery_codes(user_id);

CREATE TABLE IF NOT EXISTS captchas (
 id text PRIMARY KEY, answer_hash text NOT NULL, session_hash text NOT NULL,
 expires timestamptz NOT NULL, used boolean NOT NULL DEFAULT false, created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS captchas_expires ON captchas(expires);
CREATE INDEX IF NOT EXISTS captchas_session ON captchas(session_hash,created);

INSERT INTO settings(key,value) VALUES ('captcha_required','true') ON CONFLICT DO NOTHING;
