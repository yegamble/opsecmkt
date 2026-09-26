-- P1 Authentication: a pending sign-in (correct password, second factor outstanding) records its first failed
-- second-factor check, so that failure writes one audit row and one owner notification, however often it repeats (A-152).
ALTER TABLE pending_logins ADD COLUMN IF NOT EXISTS factor_failed boolean NOT NULL DEFAULT false;
