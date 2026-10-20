-- Ticket bodies are client-encrypted PGP. Only fixed workflow metadata is
-- stored outside the ciphertext; erased identities can be unlinked safely.
CREATE TABLE tickets (
 id text PRIMARY KEY,
 opener_id text REFERENCES users(id) ON DELETE SET NULL,
 assigned_to text REFERENCES users(id) ON DELETE SET NULL,
 status text NOT NULL DEFAULT 'open' CHECK(status IN ('open','closed','ignored')),
 admin_only boolean NOT NULL DEFAULT false,
 created timestamptz NOT NULL DEFAULT now(),
 updated timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tickets_opener_updated ON tickets(opener_id,updated);
CREATE INDEX tickets_status_updated ON tickets(status,admin_only,updated);
CREATE TABLE ticket_participants (
 ticket_id text NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 ever_assigned boolean NOT NULL DEFAULT false,
 ever_messaged boolean NOT NULL DEFAULT false,
 PRIMARY KEY(ticket_id,user_id)
);
CREATE INDEX ticket_participants_user ON ticket_participants(user_id);
CREATE TABLE ticket_messages (
 id text PRIMARY KEY,
 ticket_id text NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
 sender_id text REFERENCES users(id) ON DELETE SET NULL,
 body text NOT NULL CHECK(char_length(body) BETWEEN 1 AND 20000),
 encrypted boolean NOT NULL DEFAULT true,
 created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_messages_thread ON ticket_messages(ticket_id,created,id);
CREATE INDEX ticket_messages_retention_created ON ticket_messages(created,id);
CREATE TABLE ticket_events (
 id bigserial PRIMARY KEY,
 ticket_id text NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
 actor_id text REFERENCES users(id) ON DELETE SET NULL,
 event text NOT NULL CHECK(event IN ('opened','assigned','escalated','closed','ignored','reopened')),
 staff_id text REFERENCES users(id) ON DELETE SET NULL,
 created timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_events_thread ON ticket_events(ticket_id,id);

ALTER TABLE notifications ADD COLUMN ticket_id text REFERENCES tickets(id) ON DELETE SET NULL;
ALTER TABLE notifications ADD COLUMN ticket_message_id text REFERENCES ticket_messages(id) ON DELETE SET NULL;
-- This explicit grant belongs only to intentionally unlinked historical
-- closure metadata. Losing a ticket FK during retention never creates it.
ALTER TABLE notifications ADD COLUMN ticket_history_only boolean NOT NULL DEFAULT false;
ALTER TABLE notifications ADD CONSTRAINT notifications_ticket_history_only CHECK(NOT ticket_history_only OR
 (audience='ticket' AND ticket_id IS NULL AND ticket_message_id IS NULL AND message_id IS NULL AND delivery_order_id IS NULL));
ALTER TABLE notifications DROP CONSTRAINT notifications_audience_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_audience_check CHECK(audience IN ('personal','admin','dispute','ticket'));
ALTER TABLE notifications DROP CONSTRAINT notifications_one_content_source;
ALTER TABLE notifications ADD CONSTRAINT notifications_one_content_source CHECK(num_nonnulls(message_id,delivery_order_id,ticket_message_id)<=1);

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
     AND num_nonnulls(NEW.message_id,NEW.delivery_order_id,NEW.ticket_message_id)=1,
   CASE WHEN COALESCE(config->>'allow_contents'='true',false) AND u.xmpp_include_contents
     AND num_nonnulls(NEW.message_id,NEW.delivery_order_id,NEW.ticket_message_id)=1
     THEN u.pgp_fingerprint ELSE '' END,policy_version
 FROM users u
 WHERE u.id=NEW.user_id AND u.xmpp_enabled AND u.xmpp<>''
   AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL
   AND (NEW.audience='personal' OR NEW.audience='admin' AND u.role='admin'
     OR NEW.audience='dispute' AND u.role IN ('admin','moderator')
     OR NEW.audience='ticket' AND u.role IN ('admin','moderator') AND (NEW.ticket_id IS NOT NULL OR NEW.ticket_history_only));
 RETURN NEW;
END;
$$;
