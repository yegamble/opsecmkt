-- MOD72-6: listing hold mode with per-vendor auto-approve (listing_review.go). An administrator's
-- listing_review_mode is 'publish' (the default: a new or edited listing is public at once, as before) or 'hold'
-- (a vendor's new listing, or its edit of an approved one, waits as 'pending' until a moderator or administrator
-- approves it). Existing listings stay approved, so upgrading changes nothing until an administrator chooses hold.
-- Staff may instead reject a pending listing ('rejected', with review_rejection_reason shown to its vendor): it stays
-- hidden, is not archived, and the vendor's next change to what buyers see sends it back to 'pending'. Only staff
-- approval writes 'approved'; switching back to publish approves nothing, and the moderation layer (capture
-- triggers, worker, model) never reads or writes these columns.
ALTER TABLE products ADD COLUMN review_state text NOT NULL DEFAULT 'approved'
 CHECK (review_state IN ('approved','pending','rejected'));
-- The reason exists only on a rejected listing; retention's listing_private purge clears it with the listing's other
-- private text, so a purged rejected listing may have none.
ALTER TABLE products ADD COLUMN review_rejection_reason text
 CHECK (review_rejection_reason IS NULL OR (review_state='rejected' AND char_length(review_rejection_reason) BETWEEN 1 AND 500));
CREATE INDEX products_review_queue ON products(updated, id) WHERE review_state='pending' AND NOT archived;

-- An administrator may let a vendor's listings publish without review; who set it is in the audit trail.
ALTER TABLE users ADD COLUMN listing_auto_approve boolean NOT NULL DEFAULT false;

INSERT INTO settings(key,value) VALUES('listing_review_mode','publish') ON CONFLICT(key) DO NOTHING;
