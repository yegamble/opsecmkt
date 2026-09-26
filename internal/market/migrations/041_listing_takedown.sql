-- P4 Inventory: an administrator's takedown of a listing (A-163). NULL = not taken down. Otherwise the listing is
-- archived and this is the administrator's reason (10–500 characters), stored unencrypted and shown to the vendor
-- on the listing's edit page and vendor desk card, never on public pages. The vendor cannot restore it; only an
-- administrator's restore clears it. Archiving by the vendor or by a role change leaves it NULL.
ALTER TABLE products ADD COLUMN IF NOT EXISTS admin_archive_reason text;
ALTER TABLE products DROP CONSTRAINT IF EXISTS products_takedown_archived;
ALTER TABLE products ADD CONSTRAINT products_takedown_archived CHECK (admin_archive_reason IS NULL OR archived);
