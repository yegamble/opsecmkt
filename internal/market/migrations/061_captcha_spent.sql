-- A-176: CAPTCHA challenges are stateless (expiry ‖ nonce ‖ MAC bound to the session), so issuing one writes
-- nothing. Single use is enforced by recording each id when an answer is posted; the background sweep deletes
-- rows once they are past expiry. The per-view `captchas` rows are no longer written or read, so the table is
-- dropped: a challenge shown before the upgrade is refused after it, and that user reloads for a new image.
CREATE TABLE IF NOT EXISTS captcha_spent (
  id text PRIMARY KEY,
  expires timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS captcha_spent_expires ON captcha_spent(expires);
DROP TABLE IF EXISTS captchas;
