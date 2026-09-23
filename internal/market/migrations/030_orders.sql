-- P3 Orders: digital delivery records, verified reviews keyed to completed orders, dispute outcomes.
-- A deliveries row is written only in the same transaction as the paid -> delivered transition.
CREATE TABLE IF NOT EXISTS deliveries (
 order_id text PRIMARY KEY REFERENCES orders(id), content text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 32000),
 created timestamptz NOT NULL DEFAULT now()
);

-- One review per order; the application only inserts for orders in state 'completed'.
CREATE TABLE IF NOT EXISTS reviews (
 id text PRIMARY KEY, order_id text UNIQUE NOT NULL REFERENCES orders(id), product_id text NOT NULL REFERENCES products(id),
 buyer_id text NOT NULL REFERENCES users(id), rating integer NOT NULL CHECK (rating BETWEEN 1 AND 5),
 body text NOT NULL DEFAULT '' CHECK (char_length(body) <= 2000), created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS reviews_product ON reviews(product_id, created);

ALTER TABLE disputes ADD COLUMN IF NOT EXISTS outcome text NOT NULL DEFAULT '';
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_outcome_check;
ALTER TABLE disputes ADD CONSTRAINT disputes_outcome_check CHECK (outcome IN ('','release','refund'));
