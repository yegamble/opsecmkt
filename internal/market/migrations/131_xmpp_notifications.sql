-- Opt-in is new: legacy addresses cleared by migration 098 never enable delivery.
ALTER TABLE users ADD COLUMN xmpp_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN xmpp_version bigint NOT NULL DEFAULT 0;
ALTER TABLE notifications ADD COLUMN audience text NOT NULL DEFAULT 'personal'
 CHECK (audience IN ('personal','admin','dispute'));

-- Old notices have no reliable audience metadata. Keep historical notices to
-- administrators behind the administrator boundary rather than guessing which
-- contain operator-only detail. Explicitly recognizable old payment-review
-- broadcasts also lose moderator access. No old record is queued externally.
UPDATE notifications n SET audience='admin' FROM users u
 WHERE u.id=n.user_id AND (u.role='admin'
   OR n.body LIKE 'Payment review needed for order %'
   OR n.body LIKE 'Payment review escalated for order %'
   OR n.body LIKE 'Monero escrow wallet %'
   OR n.body LIKE 'Vendor bond wallet %'
   OR n.body LIKE 'Vendor bond: %'
   OR n.body LIKE 'The Monero escrow market fee set on the server changed%'
   OR n.body LIKE 'Monero escrow payment provider %'
   OR n.body LIKE 'Bitcoin escrow payment provider %'
   OR n.body ~ '^Order [^:]+: (retention is overdue|Re-request carried out:|Re-request of the failed Monero escrow settlement)'
   OR n.body ~ '^Dispute opened on order [^.]+ needs an unsuspended ');

-- The outbox holds references, not another copy of notification content. Deleting
-- a notice/account under retention also deletes its external-delivery receipt.
CREATE TABLE xmpp_outbox (
 notification_id text PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
 destination text NOT NULL,
 preference_version bigint NOT NULL,
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 next_attempt timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 outcome text NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','sent','cancelled','failed')),
 CHECK ((finished_at IS NULL) = (outcome='pending'))
);
CREATE INDEX xmpp_outbox_due ON xmpp_outbox(next_attempt) WHERE finished_at IS NULL;

-- All frontend notification writers, including wallet watchers and host account
-- recovery, enqueue atomically in the originating transaction. Existing notices
-- are never backfilled. An opt-in made later cannot forward older notices.
CREATE FUNCTION enqueue_xmpp_notification() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE config jsonb;
BEGIN
 -- A damaged optional-delivery setting must not prevent orders/messages from
 -- committing their frontend notices. Admin can replace it in outgoing settings.
 BEGIN
  SELECT value::jsonb INTO config FROM settings WHERE key='xmpp_config';
 EXCEPTION WHEN invalid_text_representation THEN
  RETURN NEW;
 END;
 IF config IS NULL OR config->>'enabled' IS DISTINCT FROM 'true' THEN
  RETURN NEW;
 END IF;
 INSERT INTO xmpp_outbox(notification_id,destination,preference_version)
 SELECT NEW.id,u.xmpp,u.xmpp_version FROM users u
 WHERE u.id=NEW.user_id AND u.xmpp_enabled AND u.xmpp<>''
   AND u.suspended_at IS NULL AND u.closed_at IS NULL AND u.retention_restricted_at IS NULL
   AND (NEW.audience='personal' OR NEW.audience='admin' AND u.role='admin'
     OR NEW.audience='dispute' AND u.role IN ('admin','moderator'));
 RETURN NEW;
END;
$$;
CREATE TRIGGER notifications_xmpp_outbox AFTER INSERT ON notifications
 FOR EACH ROW EXECUTE FUNCTION enqueue_xmpp_notification();
