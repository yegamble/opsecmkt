-- A-181: the catalog lists unarchived listings newest first, 25 a page (load.go); without an index every
-- catalog request scanned and sorted the whole products table. A trigram index for the ILIKE search
-- (pg_trgm) is deferred: creating the extension fails on locked-down external databases.
CREATE INDEX IF NOT EXISTS products_archived_created ON products(archived, created);
