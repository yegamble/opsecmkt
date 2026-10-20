-- Moderation records proposals only. No trigger changes a marketplace target.
INSERT INTO settings(key,value) VALUES ('moderation_enabled','false') ON CONFLICT DO NOTHING;

CREATE TABLE moderation_jobs (
 id bigserial PRIMARY KEY,
 kind text NOT NULL CHECK (kind IN ('listing','upload','dispute','review','payment_flag')),
 target_id text NOT NULL, actor_id text NOT NULL,
 content_hash text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
 snapshot jsonb NOT NULL CHECK (octet_length(snapshot::text) <= 65536),
 queued_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz, claimed_by text NOT NULL DEFAULT '', attempts integer NOT NULL DEFAULT 0,
 completed_at timestamptz, failure_class text NOT NULL DEFAULT '',
 UNIQUE(kind,target_id,content_hash)
);
CREATE INDEX moderation_jobs_pending ON moderation_jobs(queued_at,id) WHERE completed_at IS NULL;
CREATE INDEX moderation_jobs_actor_window ON moderation_jobs(actor_id,queued_at);
CREATE INDEX moderation_jobs_kind_recent ON moderation_jobs(kind,queued_at DESC);

CREATE TABLE moderation_suggestions (
 id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{64}$'), job_id bigint NOT NULL REFERENCES moderation_jobs(id),
 kind text NOT NULL, target_id text NOT NULL, actor_id text NOT NULL,
 content_hash text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
 policy_hash text NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
 input jsonb NOT NULL CHECK (octet_length(input::text) <= 131072),
 decision jsonb NOT NULL CHECK (octet_length(decision::text) <= 131072),
 disposition text NOT NULL CHECK (disposition IN ('AUTO_APPLY','QUEUE_HUMAN','ESCALATE_ADMIN','HOLD')),
 risk_score integer NOT NULL CHECK (risk_score BETWEEN 0 AND 100),
 confidence double precision NOT NULL CHECK (confidence BETWEEN 0 AND 1),
 reason_codes jsonb NOT NULL CHECK (jsonb_typeof(reason_codes)='array'),
 input_hash text NOT NULL, output_hash text NOT NULL, text_hash text NOT NULL DEFAULT '',
 prompt_version text NOT NULL, model_id text NOT NULL, weights_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 default_action text NOT NULL DEFAULT 'HOLD' CHECK (default_action='HOLD'), failure_class text NOT NULL DEFAULT '',
 UNIQUE(job_id,policy_hash)
);
CREATE INDEX moderation_suggestions_queue ON moderation_suggestions(created_at,id);
CREATE INDEX moderation_suggestions_kind_recent ON moderation_suggestions(kind,created_at DESC);

CREATE TABLE moderation_reviews (
 suggestion_id text PRIMARY KEY REFERENCES moderation_suggestions(id),
 reviewer_user_id text REFERENCES users(id) ON DELETE SET NULL,
 action text NOT NULL CHECK (action IN ('accept','dismiss','hold')),
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE moderation_risk_scores (
 suggestion_id text PRIMARY KEY REFERENCES moderation_suggestions(id),
 score integer NOT NULL CHECK(score BETWEEN 0 AND 100), disposition text NOT NULL,
 reasons jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE moderation_injection_events (
 suggestion_id text PRIMARY KEY REFERENCES moderation_suggestions(id),
 evidence jsonb NOT NULL, input_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE moderation_escalations (
 id bigserial PRIMARY KEY, actor_key text NOT NULL, pattern_hash text NOT NULL,
 latest_suggestion_id text NOT NULL REFERENCES moderation_suggestions(id),
 occurrences bigint NOT NULL DEFAULT 1,
 encrypted_payload bytea, signature bytea, payload_hash text NOT NULL DEFAULT '',
 first_created timestamptz NOT NULL DEFAULT now(), last_created timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL, default_action text NOT NULL DEFAULT 'HOLD' CHECK(default_action='HOLD'),
 UNIQUE(actor_key,pattern_hash)
);
CREATE TABLE moderation_worker_status (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 present boolean NOT NULL DEFAULT false,
 model_state text NOT NULL DEFAULT 'absent' CHECK(model_state IN ('absent','not_observed','ready','failed','disabled')),
 prompt_version text NOT NULL DEFAULT '', model_id text NOT NULL DEFAULT '', weights_hash text NOT NULL DEFAULT '',
 last_pass timestamptz, failure_class text NOT NULL DEFAULT ''
);
CREATE INDEX moderation_escalations_latest ON moderation_escalations(latest_suggestion_id);
INSERT INTO moderation_worker_status(singleton) VALUES (true);

CREATE FUNCTION moderation_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'moderation audit records are append-only';
END $$;
CREATE TRIGGER moderation_suggestions_immutable BEFORE UPDATE OR DELETE ON moderation_suggestions FOR EACH ROW EXECUTE FUNCTION moderation_immutable();
CREATE TRIGGER moderation_risk_immutable BEFORE UPDATE OR DELETE ON moderation_risk_scores FOR EACH ROW EXECUTE FUNCTION moderation_immutable();
CREATE TRIGGER moderation_injection_immutable BEFORE UPDATE OR DELETE ON moderation_injection_events FOR EACH ROW EXECUTE FUNCTION moderation_immutable();

-- Snapshots deliberately exclude keys, addresses, delivery content, image bytes,
-- user handles and encrypted messages. Source labels never confer instruction trust.
-- Replaced by migration 127 once the gateway hash column exists. This typed
-- helper never serializes the image row (which contains bounded but large bytes).
CREATE FUNCTION moderation_image_perceptual_hash(p images) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT ''::text $$;
CREATE FUNCTION moderation_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s jsonb; k text; tid text; aid text; h text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='products' THEN
  IF TG_OP='UPDATE' AND (NEW.title,NEW.description,NEW.category,NEW.region,NEW.kind,NEW.xmr) IS NOT DISTINCT FROM (OLD.title,OLD.description,OLD.category,OLD.region,OLD.kind,OLD.xmr) THEN RETURN NEW; END IF;
  k:='listing'; tid:=NEW.id; aid:=NEW.vendor_id;
  s:=jsonb_build_object('title',NEW.title,'description',NEW.description,'category',NEW.category,'region',NEW.region,'kind',NEW.kind,'price_atomic',NEW.xmr);
 ELSIF TG_TABLE_NAME='images' THEN
  k:='upload'; tid:=NEW.id; aid:=NEW.owner_user_id;
  s:=jsonb_build_object('description',NEW.description,'product_id',NEW.product_id,'image_kind',NEW.kind,'width',NEW.width,'height',NEW.height,'bytes',octet_length(NEW."full"),'image_hash',encode(sha256(NEW."full"),'hex'),'perceptual_hash',moderation_image_perceptual_hash(NEW));
 ELSIF TG_TABLE_NAME='disputes' THEN
  IF TG_OP='UPDATE' AND (NEW.reason,NEW.status,NEW.resolution) IS NOT DISTINCT FROM (OLD.reason,OLD.status,OLD.resolution) THEN RETURN NEW; END IF;
  k:='dispute'; tid:=NEW.id; SELECT buyer_id INTO aid FROM orders WHERE id=NEW.order_id;
  s:=jsonb_build_object('reason',NEW.reason,'status',NEW.status,'resolution',NEW.resolution,'order_id',NEW.order_id,'order_state',(SELECT state FROM orders WHERE id=NEW.order_id));
 ELSIF TG_TABLE_NAME='reviews' THEN
  IF TG_OP='UPDATE' AND (NEW.body,NEW.rating) IS NOT DISTINCT FROM (OLD.body,OLD.rating) THEN RETURN NEW; END IF;
  k:='review'; tid:=NEW.id; aid:=NEW.buyer_id;
  s:=jsonb_build_object('body',NEW.body,'rating',NEW.rating,'product_id',NEW.product_id,'order_id',NEW.order_id);
 ELSIF TG_TABLE_NAME='payments' THEN
  IF NOT NEW.flagged THEN RETURN NEW; END IF;
  IF TG_OP='UPDATE' AND OLD.flagged THEN RETURN NEW; END IF;
  k:='payment_flag'; tid:=NEW.id::text; SELECT buyer_id INTO aid FROM orders WHERE id=NEW.order_id;
  s:=jsonb_build_object('order_id',NEW.order_id,'flagged',true,'disposition',NEW.disposition,'order_state',(SELECT state FROM orders WHERE id=NEW.order_id));
 END IF;
 IF aid IS NULL THEN RETURN NEW; END IF;
 s:=s || jsonb_build_object('actor_id',aid,'actor_created',(SELECT created FROM users WHERE id=aid));
 h:=encode(sha256(convert_to(s::text,'UTF8')),'hex');
 -- Keep the full-content digest, but a bounded excerpt in the job. 4097 code
 -- points exceeds the core's 4096-field cap, so truncation queues human review.
 SELECT jsonb_object_agg(key,CASE WHEN jsonb_typeof(value)='string' AND char_length(value #>> '{}')>4097
   THEN to_jsonb(left(value #>> '{}',4097)) ELSE value END) INTO s FROM jsonb_each(s);
 INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot) VALUES(k,tid,aid,h,s) ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
CREATE TRIGGER moderation_product AFTER INSERT OR UPDATE ON products FOR EACH ROW EXECUTE FUNCTION moderation_enqueue();
CREATE TRIGGER moderation_image AFTER INSERT ON images FOR EACH ROW EXECUTE FUNCTION moderation_enqueue();
CREATE TRIGGER moderation_dispute AFTER INSERT OR UPDATE ON disputes FOR EACH ROW EXECUTE FUNCTION moderation_enqueue();
CREATE TRIGGER moderation_review AFTER INSERT OR UPDATE ON reviews FOR EACH ROW EXECUTE FUNCTION moderation_enqueue();
CREATE TRIGGER moderation_payment AFTER INSERT OR UPDATE ON payments FOR EACH ROW EXECUTE FUNCTION moderation_enqueue();
