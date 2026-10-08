-- A-58: the listing terms seen when payment is requested stay with the order.
-- NULL means no trustworthy purchase-time snapshot was recorded (including earlier
-- orders and drafts that never requested payment). New drafts remain NULL until
-- Draft -> Awaiting payment commits.
ALTER TABLE orders ADD COLUMN listing_title text;
ALTER TABLE orders ADD COLUMN listing_description text;
ALTER TABLE orders ADD COLUMN listing_kind text;
ALTER TABLE orders ADD CONSTRAINT orders_listing_snapshot_complete CHECK (
 (listing_title IS NULL AND listing_description IS NULL AND listing_kind IS NULL)
 OR (listing_title IS NOT NULL AND listing_description IS NOT NULL AND listing_kind IS NOT NULL AND listing_kind IN ('physical','digital','service'))
);
