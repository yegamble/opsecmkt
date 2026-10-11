ALTER TABLE notifications ADD COLUMN order_id text REFERENCES orders(id) ON DELETE SET NULL;
CREATE INDEX notifications_order_id ON notifications(order_id) WHERE order_id IS NOT NULL;
