-- A-546 (owner decision 2026-10-18): a ban is an administrator's permanent closure of an account, distinct from a
-- reversible suspension. banned_at is when it was banned, banned_by the administrator (unlinked if that account is
-- erased) and ban_reason the administrator's reason (10–500 characters, shown to administrators only). A banned
-- account is always also suspended, so every rule that refuses or skips a suspended account (sign-in, sessions, new
-- orders, messages, dispute resolvers, staff notices, public images) applies to it too. Lifting a ban clears all
-- three; the audit trail keeps the history.
ALTER TABLE users ADD COLUMN banned_at timestamptz;
ALTER TABLE users ADD COLUMN banned_by text REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE users ADD COLUMN ban_reason text CHECK (char_length(btrim(ban_reason)) BETWEEN 10 AND 500);
ALTER TABLE users ADD CONSTRAINT users_ban_reason CHECK ((banned_at IS NULL) = (ban_reason IS NULL));
ALTER TABLE users ADD CONSTRAINT users_ban_suspends CHECK (banned_at IS NULL OR suspended_at IS NOT NULL);
