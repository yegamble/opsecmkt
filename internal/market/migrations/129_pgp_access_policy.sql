-- PGP decryption authorizes only the current browser and current public key.
ALTER TABLE sessions ADD COLUMN pgp_access_key text NOT NULL DEFAULT '';
ALTER TABLE pgp_challenges ADD COLUMN session_hash text NOT NULL DEFAULT '';
ALTER TABLE pgp_challenges ADD COLUMN key_digest text NOT NULL DEFAULT '';
-- Pre-upgrade challenges have no session binding and must be reissued.
DELETE FROM pgp_challenges;
ALTER TABLE pgp_challenges DROP CONSTRAINT pgp_challenges_pkey;
ALTER TABLE pgp_challenges ADD PRIMARY KEY(user_id,session_hash);
INSERT INTO settings(key,value) VALUES ('allow_login_without_pgp','true') ON CONFLICT(key) DO NOTHING;
