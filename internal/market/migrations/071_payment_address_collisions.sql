-- A-288: a wallet restored from an older backup or seed hands out addresses the database already issued to other
-- orders. Each such payment request is refused (payment_addresses.address is unique); these columns count the
-- refusals per currency since an administrator last dismissed the /admin warning, and record the latest one.
-- Additive only: existing rows start at 0 / NULL (no warning).
ALTER TABLE payment_status ADD COLUMN IF NOT EXISTS address_collisions integer NOT NULL DEFAULT 0;
ALTER TABLE payment_status ADD COLUMN IF NOT EXISTS address_collision_last timestamptz;
