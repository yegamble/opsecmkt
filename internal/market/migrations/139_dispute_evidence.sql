-- Private dispute evidence uses the same scanner-normalized image store as photos.
-- The original upload, filename, metadata and scan payload are never retained.
ALTER TABLE images ADD COLUMN order_id text REFERENCES orders(id) ON DELETE CASCADE;
ALTER TABLE images DROP CONSTRAINT images_kind_check;
ALTER TABLE images DROP CONSTRAINT images_check;
ALTER TABLE images ADD CONSTRAINT images_kind_check CHECK (kind IN ('photo','avatar','evidence'));
ALTER TABLE images ADD CONSTRAINT images_subject_check CHECK (
 (kind='photo' AND product_id IS NOT NULL AND order_id IS NULL) OR
 (kind='avatar' AND product_id IS NULL AND order_id IS NULL) OR
 (kind='evidence' AND product_id IS NULL AND order_id IS NOT NULL));
CREATE INDEX images_order_evidence ON images(order_id,created,id) WHERE kind='evidence';
