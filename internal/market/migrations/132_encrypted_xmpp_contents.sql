-- Both permissions are opt-in. Enabling them cannot append content to alerts
-- already queued before the recipient and administrator agreed to this scope.
ALTER TABLE users ADD COLUMN xmpp_include_contents boolean NOT NULL DEFAULT false;
ALTER TABLE messages ADD COLUMN order_id text REFERENCES orders(id) ON DELETE SET NULL;
ALTER TABLE messages ADD COLUMN dispute_id text REFERENCES disputes(id) ON DELETE SET NULL;
ALTER TABLE messages ADD CONSTRAINT messages_one_context CHECK(order_id IS NULL OR dispute_id IS NULL);
ALTER TABLE notifications ADD COLUMN message_id text REFERENCES messages(id) ON DELETE SET NULL;
ALTER TABLE notifications ADD COLUMN delivery_order_id text REFERENCES orders(id) ON DELETE SET NULL;
ALTER TABLE notifications ADD CONSTRAINT notifications_one_content_source CHECK(message_id IS NULL OR delivery_order_id IS NULL);
ALTER TABLE xmpp_outbox ADD COLUMN include_contents boolean NOT NULL DEFAULT false;
ALTER TABLE xmpp_outbox ADD COLUMN recipient_fingerprint text NOT NULL DEFAULT '';
ALTER TABLE xmpp_outbox ADD COLUMN content_policy_version bigint NOT NULL DEFAULT 0 CHECK(content_policy_version>=0);

-- New per-order deliveries hold ciphertext for the buyer. Shared listing
-- templates and existing historical deliveries are not silently rewritten.
ALTER TABLE deliveries ADD COLUMN encrypted boolean NOT NULL DEFAULT false;
ALTER TABLE deliveries ADD COLUMN recipient_fingerprint text NOT NULL DEFAULT '';
ALTER TABLE deliveries DROP CONSTRAINT deliveries_content_check;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_content_check CHECK(char_length(content) BETWEEN 1 AND 262144);

CREATE OR REPLACE FUNCTION enqueue_xmpp_notification() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE config jsonb; policy_version bigint;
BEGIN
 BEGIN
  SELECT value::jsonb INTO config FROM settings WHERE key='xmpp_config';
  policy_version := COALESCE((config->>'content_policy_version')::bigint,0);
 EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
  RETURN NEW;
 END;
 IF config IS NULL OR config->>'enabled' IS DISTINCT FROM 'true' OR policy_version<0 THEN
  RETURN NEW;
 END IF;
 INSERT INTO xmpp_outbox(notification_id,destination,preference_version,include_contents,recipient_fingerprint,content_policy_version)
 SELECT NEW.id,u.xmpp,u.xmpp_version,
   COALESCE(config->>'allow_contents'='true',false) AND u.xmpp_include_contents
     AND (NEW.message_id IS NOT NULL OR NEW.delivery_order_id IS NOT NULL),
   CASE WHEN COALESCE(config->>'allow_contents'='true',false) AND u.xmpp_include_contents
     AND (NEW.message_id IS NOT NULL OR NEW.delivery_order_id IS NOT NULL)
     THEN u.pgp_fingerprint ELSE '' END,policy_version
 FROM users u
 WHERE u.id=NEW.user_id AND u.xmpp_enabled AND u.xmpp<>''
   AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL
   AND (NEW.audience='personal' OR NEW.audience='admin' AND u.role='admin'
     OR NEW.audience='dispute' AND u.role IN ('admin','moderator'));
 RETURN NEW;
END;
$$;
