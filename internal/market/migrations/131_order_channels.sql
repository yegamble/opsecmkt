-- Transaction-specific correspondence and replacement deliveries share the
-- order-private retention lifecycle, never the direct-message clock.
CREATE TABLE order_messages (
 id text PRIMARY KEY,
 order_id text NOT NULL REFERENCES orders(id),
 sender_id text NOT NULL REFERENCES users(id),
 recipient_id text NOT NULL REFERENCES users(id),
 body text NOT NULL CHECK (octet_length(body) <= 20000),
 recipient_match text NOT NULL CHECK (recipient_match IN ('yes','unknown')),
 recipient_extra_keys integer CHECK (recipient_extra_keys >= 0),
 created timestamptz NOT NULL DEFAULT now(),
 CHECK (sender_id <> recipient_id)
);
CREATE INDEX order_messages_order ON order_messages(order_id,created,id);
CREATE INDEX order_messages_sender ON order_messages(sender_id);
CREATE INDEX order_messages_recipient ON order_messages(recipient_id);
CREATE TABLE order_delivery_updates (
 id text PRIMARY KEY,
 order_id text NOT NULL REFERENCES orders(id),
 author_id text NOT NULL REFERENCES users(id),
 content text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 32000),
 created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_delivery_updates_order ON order_delivery_updates(order_id,created,id);
CREATE INDEX order_delivery_updates_author ON order_delivery_updates(author_id);
