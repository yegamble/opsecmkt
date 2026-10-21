-- Prospective v2 capture stores references and digests only. Historical
-- snapshots and immutable decisions are preserved as explicit legacy records.
ALTER TABLE moderation_jobs ADD COLUMN capture_version smallint NOT NULL DEFAULT 0 CHECK(capture_version IN (0,2));
ALTER TABLE moderation_jobs ADD COLUMN metadata jsonb NOT NULL DEFAULT '{}' CHECK(octet_length(metadata::text)<=8192);
ALTER TABLE moderation_jobs ADD COLUMN actor_ref text NOT NULL DEFAULT '';
ALTER TABLE moderation_jobs ADD COLUMN reserved_results bigint NOT NULL DEFAULT 1;
ALTER TABLE moderation_jobs ADD COLUMN results_count bigint NOT NULL DEFAULT 0;
ALTER TABLE moderation_jobs ADD COLUMN priority_at timestamptz;
UPDATE moderation_jobs SET priority_at=queued_at-CASE WHEN kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END;
UPDATE moderation_jobs j SET results_count=(SELECT count(*) FROM moderation_suggestions s WHERE s.job_id=j.id);
UPDATE moderation_jobs SET reserved_results=GREATEST(1,results_count);
ALTER TABLE moderation_jobs ALTER COLUMN capture_version SET DEFAULT 2;
ALTER TABLE moderation_jobs ADD CONSTRAINT moderation_jobs_v2_minimal CHECK(capture_version=0 OR
 (snapshot='{}'::jsonb AND actor_ref~'^[0-9a-f]{64}$' AND length(target_id)<=128 AND length(actor_id)<=128));
CREATE INDEX moderation_actor_active ON moderation_jobs(actor_id) WHERE completed_at IS NULL;
CREATE INDEX moderation_jobs_fair_age ON moderation_jobs(priority_at,id) WHERE completed_at IS NULL;
CREATE INDEX moderation_jobs_recent_capture ON moderation_jobs(queued_at DESC,id DESC);

ALTER TABLE moderation_suggestions ADD COLUMN storage_version smallint NOT NULL DEFAULT 0;
ALTER TABLE moderation_suggestions ALTER COLUMN storage_version SET DEFAULT 2;
ALTER TABLE moderation_suggestions ADD CONSTRAINT moderation_suggestions_v2_minimal CHECK(storage_version=0 OR
 (storage_version=2 AND actor_id='' AND octet_length(input::text)<=8192 AND octet_length(decision::text)<=16384
 AND input->>'metadata_version'='2' AND NOT input ? 'fields' AND NOT input ? 'actor'
 AND input-ARRAY['metadata_version','item_id','kind','source_digest','context','field_digests']='{}'::jsonb
 AND length(target_id)<=128 AND length(prompt_version)<=80 AND length(model_id)<=128 AND length(weights_hash)<=64));
ALTER TABLE moderation_escalations ADD COLUMN storage_version smallint NOT NULL DEFAULT 0;
ALTER TABLE moderation_escalations ALTER COLUMN storage_version SET DEFAULT 2;
ALTER TABLE moderation_escalations ADD CONSTRAINT moderation_escalations_v2_bound CHECK(storage_version=0 OR
 (storage_version=2 AND octet_length(encrypted_payload)<=262144 AND length(actor_key)<=128));

CREATE TABLE moderation_capacity (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 max_pending bigint NOT NULL DEFAULT 256 CHECK(max_pending BETWEEN 1 AND 10000),
 max_records bigint NOT NULL DEFAULT 4096 CHECK(max_records BETWEEN 1 AND 1000000),
 max_bytes bigint NOT NULL DEFAULT 268435456 CHECK(max_bytes BETWEEN 524288 AND 1099511627776),
 pending bigint NOT NULL DEFAULT 0 CHECK(pending>=0),
 allocated_records bigint NOT NULL DEFAULT 0 CHECK(allocated_records>=0),
 allocated_bytes bigint NOT NULL DEFAULT 0 CHECK(allocated_bytes>=0),
 legacy_jobs bigint NOT NULL DEFAULT 0, legacy_bytes bigint NOT NULL DEFAULT 0,
 completed_v2 bigint NOT NULL DEFAULT 0,
 unreviewed bigint NOT NULL DEFAULT 0, admin_pending bigint NOT NULL DEFAULT 0, injections bigint NOT NULL DEFAULT 0
);
-- One migration-time inventory, never repeated in admission or worker polling.
INSERT INTO moderation_capacity(singleton,pending,allocated_records,allocated_bytes,legacy_jobs,legacy_bytes,unreviewed,admin_pending,injections)
SELECT true,(SELECT count(*) FROM moderation_jobs WHERE completed_at IS NULL),
 COALESCE(sum(reserved_results),0),GREATEST(COALESCE(sum(reserved_results),0)*524288,
 (SELECT COALESCE(sum(octet_length(snapshot::text)),0) FROM moderation_jobs)+
 (SELECT COALESCE(sum(octet_length(input::text)+octet_length(decision::text)),0) FROM moderation_suggestions)+
 (SELECT COALESCE(sum(octet_length(encrypted_payload)),0) FROM moderation_escalations)),
 count(*),(SELECT COALESCE(sum(octet_length(snapshot::text)),0) FROM moderation_jobs)+
 (SELECT COALESCE(sum(octet_length(input::text)+octet_length(decision::text)),0) FROM moderation_suggestions),
 (SELECT count(*) FROM moderation_suggestions s WHERE disposition<>'AUTO_APPLY' AND NOT EXISTS(SELECT 1 FROM moderation_reviews r WHERE r.suggestion_id=s.id)),
 (SELECT count(*) FROM moderation_suggestions s WHERE disposition='ESCALATE_ADMIN' AND NOT EXISTS(SELECT 1 FROM moderation_reviews r WHERE r.suggestion_id=s.id)),
 (SELECT count(*) FROM moderation_injection_events) FROM moderation_jobs;

CREATE TABLE moderation_overflow (
 kind text NOT NULL, reason text NOT NULL, occurrences bigint NOT NULL DEFAULT 0 CHECK(occurrences BETWEEN 0 AND 2147483647),
 last_seen timestamptz, default_action text NOT NULL DEFAULT 'HOLD' CHECK(default_action='HOLD'), PRIMARY KEY(kind,reason)
);
INSERT INTO moderation_overflow(kind,reason) SELECT k,r FROM unnest(ARRAY['listing','upload','dispute','review','payment_flag','all']) k
 CROSS JOIN unnest(ARRAY['actor_capacity','pending_capacity','retained_capacity']) r;
CREATE FUNCTION moderation_note_overflow(k text,r text) RETURNS void LANGUAGE sql AS $$
 UPDATE moderation_overflow SET occurrences=LEAST(2147483647,occurrences+1),last_seen=now() WHERE kind=k AND reason=r
$$;

CREATE FUNCTION moderation_admit_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer;
BEGIN
 IF NEW.capture_version<>2 THEN RETURN NEW; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=NEW.content_hash) THEN RETURN NULL; END IF;
 SELECT count(*) INTO n FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=NEW.actor_id AND completed_at IS NULL LIMIT 3) a;
 IF n>=3 THEN PERFORM moderation_note_overflow(NEW.kind,'actor_capacity'); RETURN NULL; END IF;
 IF b.pending>=b.max_pending THEN PERFORM moderation_note_overflow(NEW.kind,'pending_capacity'); RETURN NULL; END IF;
 IF b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN
  PERFORM moderation_note_overflow(NEW.kind,'retained_capacity'); RETURN NULL;
 END IF;
 UPDATE moderation_capacity SET pending=pending+1,allocated_records=allocated_records+1,allocated_bytes=allocated_bytes+524288 WHERE singleton;
 NEW.snapshot:='{}'::jsonb; NEW.reserved_results:=1; NEW.results_count:=0;
 NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END;
 RETURN NEW;
END $$;
CREATE TRIGGER moderation_job_admission BEFORE INSERT ON moderation_jobs FOR EACH ROW EXECUTE FUNCTION moderation_admit_job();

CREATE FUNCTION moderation_job_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer;
BEGIN
 IF NEW.queued_at IS DISTINCT FROM OLD.queued_at THEN NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END; END IF;
 IF OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL THEN
  UPDATE moderation_capacity SET pending=GREATEST(0,pending-1),completed_v2=completed_v2+CASE WHEN NEW.capture_version=2 THEN 1 ELSE 0 END WHERE singleton;
  IF NEW.capture_version=2 THEN NEW.snapshot:='{}'; NEW.actor_id:=''; NEW.metadata:=NEW.metadata-'actor_created'; END IF;
 ELSIF OLD.completed_at IS NOT NULL AND NEW.completed_at IS NULL THEN
  IF NEW.capture_version=2 THEN NEW.completed_at:=OLD.completed_at; RETURN NEW; END IF;
  SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
  SELECT count(*) INTO n FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=NEW.actor_id AND completed_at IS NULL LIMIT 3) a;
  IF n>=3 THEN PERFORM moderation_note_overflow(NEW.kind,'actor_capacity'); RETURN NULL; END IF;
  IF b.pending>=b.max_pending THEN PERFORM moderation_note_overflow(NEW.kind,'pending_capacity'); RETURN NULL; END IF;
  UPDATE moderation_capacity SET pending=pending+1 WHERE singleton;
 END IF;
 RETURN NEW;
END $$;

-- Ownership is resolved from live source rows only. Completed captures retain
-- no raw actor link; deleted source rows deliberately stop contributing.
CREATE FUNCTION moderation_live_actor(k text,tid text) RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE aid text;
BEGIN
 IF k='listing' THEN SELECT vendor_id INTO aid FROM products WHERE id=tid;
 ELSIF k='upload' THEN SELECT owner_user_id INTO aid FROM images WHERE id=tid;
 ELSIF k='review' THEN SELECT buyer_id INTO aid FROM reviews WHERE id=tid;
 ELSIF k='dispute' THEN SELECT o.buyer_id INTO aid FROM disputes d JOIN orders o ON o.id=d.order_id WHERE d.id=tid;
 ELSIF k='payment_flag' THEN SELECT o.buyer_id INTO aid FROM payments p JOIN orders o ON o.id=p.order_id WHERE p.id::text=tid;
 END IF;
 RETURN aid;
END $$;
CREATE TRIGGER moderation_job_completion BEFORE UPDATE ON moderation_jobs FOR EACH ROW EXECUTE FUNCTION moderation_job_lifecycle();

CREATE FUNCTION moderation_result_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='moderation_suggestions' THEN
  UPDATE moderation_jobs SET results_count=results_count+1 WHERE id=NEW.job_id AND results_count<reserved_results;
  IF NOT FOUND THEN RAISE EXCEPTION 'moderation result has no allocation'; END IF;
  UPDATE moderation_capacity SET unreviewed=unreviewed+CASE WHEN NEW.disposition<>'AUTO_APPLY' THEN 1 ELSE 0 END,
   admin_pending=admin_pending+CASE WHEN NEW.disposition='ESCALATE_ADMIN' THEN 1 ELSE 0 END WHERE singleton;
 ELSIF TG_TABLE_NAME='moderation_reviews' THEN
  UPDATE moderation_capacity SET unreviewed=GREATEST(0,unreviewed-CASE WHEN s.disposition<>'AUTO_APPLY' THEN 1 ELSE 0 END),
   admin_pending=GREATEST(0,admin_pending-CASE WHEN s.disposition='ESCALATE_ADMIN' THEN 1 ELSE 0 END)
   FROM moderation_suggestions s WHERE singleton AND s.id=NEW.suggestion_id;
 ELSE UPDATE moderation_capacity SET injections=injections+1 WHERE singleton;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER moderation_count_suggestion AFTER INSERT ON moderation_suggestions FOR EACH ROW EXECUTE FUNCTION moderation_result_lifecycle();
CREATE TRIGGER moderation_count_review AFTER INSERT ON moderation_reviews FOR EACH ROW EXECUTE FUNCTION moderation_result_lifecycle();
CREATE TRIGGER moderation_count_injection AFTER INSERT ON moderation_injection_events FOR EACH ROW EXECUTE FUNCTION moderation_result_lifecycle();

CREATE FUNCTION moderation_claim(w text,lease_ms bigint,policy text) RETURNS SETOF moderation_jobs LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; j moderation_jobs;
BEGIN
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 SELECT * INTO j FROM moderation_jobs q WHERE completed_at IS NULL AND (lease_until IS NULL OR lease_until<now())
 AND (reserved_results>results_count OR EXISTS(SELECT 1 FROM moderation_suggestions s WHERE s.job_id=q.id AND s.policy_hash=policy)
 OR (b.allocated_records<b.max_records AND b.allocated_bytes+524288<=b.max_bytes))
 ORDER BY priority_at,id FOR UPDATE SKIP LOCKED LIMIT 1;
 IF NOT FOUND THEN
  IF (b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes)
   AND EXISTS(SELECT 1 FROM moderation_jobs q WHERE completed_at IS NULL AND (lease_until IS NULL OR lease_until<now())
    AND reserved_results<=results_count AND NOT EXISTS(SELECT 1 FROM moderation_suggestions s WHERE s.job_id=q.id AND s.policy_hash=policy))
   THEN PERFORM moderation_note_overflow('all','retained_capacity'); END IF;
  RETURN;
 END IF;
 IF j.reserved_results<=j.results_count AND NOT EXISTS(SELECT 1 FROM moderation_suggestions WHERE job_id=j.id AND policy_hash=policy) THEN
  UPDATE moderation_capacity SET allocated_records=allocated_records+1,allocated_bytes=allocated_bytes+524288 WHERE singleton;
  UPDATE moderation_jobs SET reserved_results=reserved_results+1 WHERE id=j.id;
 END IF;
 RETURN QUERY UPDATE moderation_jobs SET lease_until=now()+lease_ms*interval '1 millisecond',claimed_by=w,attempts=attempts+1 WHERE id=j.id RETURNING *;
END $$;

-- A bounded, in-memory hydration of the current source. Hashes cover the exact
-- canonical source used by 125/127; clipped fields always force human review.
CREATE FUNCTION moderation_live_source(k text,tid text) RETURNS TABLE(digest text,source jsonb) LANGUAGE plpgsql AS $$
DECLARE s jsonb;
BEGIN
 IF k='listing' THEN
  SELECT jsonb_build_object('title',p.title,'description',p.description,'category',p.category,'region',p.region,'kind',p.kind,'price_atomic',p.xmr,'actor_id',p.vendor_id,'actor_created',u.created) INTO s FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=tid;
 ELSIF k='upload' THEN SELECT moderation_upload_snapshot(tid)::jsonb INTO s;
 ELSIF k='dispute' THEN
  SELECT jsonb_build_object('reason',p.reason,'status',p.status,'resolution',p.resolution,'order_id',p.order_id,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created) INTO s FROM disputes p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id=tid;
 ELSIF k='review' THEN
  SELECT jsonb_build_object('body',p.body,'rating',p.rating,'product_id',p.product_id,'order_id',p.order_id,'actor_id',p.buyer_id,'actor_created',u.created) INTO s FROM reviews p JOIN users u ON u.id=p.buyer_id WHERE p.id=tid;
 ELSIF k='payment_flag' THEN
  SELECT jsonb_build_object('order_id',p.order_id,'flagged',p.flagged,'disposition',p.disposition,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created) INTO s FROM payments p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id::text=tid;
 END IF;
 IF s IS NULL THEN RETURN; END IF;
 digest:=encode(sha256(convert_to(s::text,'UTF8')),'hex');
 SELECT jsonb_object_agg(key,CASE WHEN jsonb_typeof(value)='string' AND char_length(value #>> '{}')>4097 THEN to_jsonb(left(value #>> '{}',4097)) ELSE value END) INTO source FROM jsonb_each(s);
 RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION moderation_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s jsonb; k text; tid text; aid text; h text; meta jsonb; ref text;
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
 ref:=encode(sha256(convert_to(gen_random_uuid()::text,'UTF8')),'hex');
 meta:=jsonb_strip_nulls(jsonb_build_object('actor_created',s->'actor_created','order_id',s->'order_id','order_state',s->'order_state',
  'product_id',s->'product_id','image_kind',s->'image_kind','image_hash',s->'image_hash','perceptual_hash',s->'perceptual_hash',
  'width',s->'width','height',s->'height','bytes',s->'bytes'));
 INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot,metadata,actor_ref,capture_version)
 VALUES(k,tid,aid,h,'{}',meta,ref,2) ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
