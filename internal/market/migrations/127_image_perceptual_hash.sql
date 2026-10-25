-- Hash only already-normalized pixels in the upload/scanner role. Existing
-- pictures keep an empty value and retain exact normalized-byte comparison;
-- migration never decodes old uploads on the application or database host.
ALTER TABLE images ADD COLUMN perceptual_hash text NOT NULL DEFAULT ''
 CHECK(perceptual_hash='' OR perceptual_hash ~ '^[0-9a-f]{16}$');

-- The older enqueue function uses a typed helper so it can operate before this
-- optional column exists without serializing a whole image row to JSON.
CREATE OR REPLACE FUNCTION moderation_image_perceptual_hash(p images)
 RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT p.perceptual_hash $$;

-- Hash normalized bytes inside PostgreSQL and return only bounded metadata.
-- SETOF keeps a missing picture distinguishable from a scalar NULL, and the row
-- lock remains held by the caller's review/persistence transaction.
CREATE FUNCTION moderation_upload_snapshot(target text) RETURNS SETOF text
 LANGUAGE sql VOLATILE AS $$
 SELECT jsonb_build_object('description',p.description,'product_id',p.product_id,
  'image_kind',p.kind,'width',p.width,'height',p.height,
  'bytes',octet_length(p."full"),'image_hash',encode(sha256(p."full"),'hex'),
  'perceptual_hash',p.perceptual_hash,'actor_id',p.owner_user_id,
  'actor_created',u.created)::text
 FROM images p JOIN users u ON u.id=p.owner_user_id WHERE p.id=target
 FOR UPDATE OF p
$$;
