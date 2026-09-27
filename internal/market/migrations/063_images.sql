-- A-186 (war room 2026-09-27, uploads): listing photos and avatars. A row holds only what the re-encoder
-- produced (uploads.go, images.go): the full JPEG, its thumbnail, the full one's size, an optional description,
-- the owner, the listing for a photo, and the time added. There is no column for the uploaded file, its name,
-- type, original size, a content hash or the scan: none of it is kept. Ids are 128 random bits (32 hex).
-- Replacing, removing or taking one down deletes the row. The CHECKs repeat the application's output bounds.
CREATE TABLE IF NOT EXISTS images (
  id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
  owner_user_id text NOT NULL REFERENCES users(id),
  product_id text REFERENCES products(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('photo','avatar')),
  "full" bytea NOT NULL CHECK (octet_length("full") <= 2097152),
  thumb bytea NOT NULL CHECK (octet_length(thumb) <= 262144),
  width integer NOT NULL CHECK (width BETWEEN 1 AND 1200),
  height integer NOT NULL CHECK (height BETWEEN 1 AND 1200),
  description text NOT NULL DEFAULT '' CHECK (length(description) <= 250),
  created timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind = 'photo') = (product_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS images_one_avatar ON images(owner_user_id) WHERE kind = 'avatar';
CREATE INDEX IF NOT EXISTS images_product ON images(product_id, created);
CREATE INDEX IF NOT EXISTS images_owner ON images(owner_user_id);
-- Owner decision (2026-09-27): staff may be hidden from public view. Storage only; A-187 and A-189 use it.
-- staff_hidden: a moderator's or administrator's "Hide me from the public staff list" (hidden by default);
-- staff_list_public: the administrator setting "Show a public staff list" (off by default).
ALTER TABLE users ADD COLUMN IF NOT EXISTS staff_hidden boolean NOT NULL DEFAULT true;
INSERT INTO settings(key,value) VALUES ('staff_list_public','false') ON CONFLICT DO NOTHING;
