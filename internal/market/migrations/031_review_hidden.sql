-- P3 Orders: an administrator can hide a review (A-125). A hidden review is kept, never deleted: it is left out of
-- the public product and vendor pages and their rating, and the buyer and vendor still see it on the order.
-- NULL = public.
ALTER TABLE reviews ADD COLUMN IF NOT EXISTS hidden_at timestamptz;
