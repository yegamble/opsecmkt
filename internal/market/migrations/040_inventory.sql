-- P4 Inventory: last-edit time and archive time. products.archived and products.delivery_content come from 001.
ALTER TABLE products ADD COLUMN IF NOT EXISTS updated timestamptz NOT NULL DEFAULT now();
ALTER TABLE products ADD COLUMN IF NOT EXISTS archived_at timestamptz;
UPDATE products SET archived_at=now() WHERE archived AND archived_at IS NULL;
