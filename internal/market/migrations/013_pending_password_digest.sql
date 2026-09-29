-- A-157: a pending sign-in records the SHA-256 of the password hash its password was checked against. /challenge
-- starts the session only while the account still has that hash, so a sign-in whose password was checked just
-- before a password change cannot finish after it. Rows written before this migration (NULL) are refused: their
-- owner signs in again.
ALTER TABLE pending_logins ADD COLUMN IF NOT EXISTS password_digest text;
