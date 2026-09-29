-- A-155: pgp_expiry_notice records the last key-expiry notice the daily sweep sent the owner, as
-- "<fingerprint>/<unix time encryption to the key stops>/<soon|expired>", so each notice is sent once per expiry;
-- a renewed or replaced key has a new value and is notified again.
-- A-158: pgp_changed_at is when the saved key was last added, replaced, removed or updated without keeping its
-- ownership proof. With pgp_verified_at it tells a buyer that the vendor's key changed or was re-verified after their
-- order was placed. Accounts whose key last changed before this migration have none (NULL).
ALTER TABLE users ADD COLUMN IF NOT EXISTS pgp_expiry_notice text NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS pgp_changed_at timestamptz;
