-- Listings created before taxonomy support retain their category and shipping region.
-- A blank subcategory explicitly means that the vendor has not classified the listing further.
ALTER TABLE products ADD COLUMN subcategory text NOT NULL DEFAULT '';
ALTER TABLE products ADD COLUMN shipping_regions text[] NOT NULL DEFAULT '{}';
ALTER TABLE products ADD CONSTRAINT products_subcategory_check CHECK (
 subcategory='' OR
 (category='Hardware' AND subcategory IN ('USB / Storage','RF / RFID','Faraday / Shielding','Network','Surveillance')) OR
 (category='Digital' AND subcategory IN ('Research / Reports','Guides / Courses','Software / Tools','Credentials / Access')) OR
 (category='Services' AND subcategory IN ('Hosting / VPS','Consulting','Custom Dev'))
);
ALTER TABLE products ADD CONSTRAINT products_shipping_regions_check CHECK (
 cardinality(shipping_regions)<=9 AND array_position(shipping_regions,NULL) IS NULL AND
 shipping_regions <@ ARRAY['Worldwide','North America','United States','Europe','Asia','Middle East','Africa','Oceania','South America']::text[]
);
CREATE INDEX products_category_subcategory ON products(category,subcategory) WHERE NOT archived;
