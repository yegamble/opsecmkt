-- MOD72-1: lossless, non-punitive moderation capture.
-- A refused capture leaves a reference-only marker: kind, target and two
-- timestamps, never content, digests or the actor. The worker later captures the
-- target's current revision through the same admission trigger. The marketplace
-- write always commits.
CREATE TABLE moderation_backfill (
 kind text NOT NULL CHECK (kind IN ('listing','upload','dispute','review','payment_flag')),
 target_id text NOT NULL CHECK (length(target_id) BETWEEN 1 AND 128),
 first_missed timestamptz NOT NULL DEFAULT now(),
 last_missed timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (kind,target_id)
);
CREATE INDEX moderation_backfill_due ON moderation_backfill(last_missed,kind,target_id);

-- A per-actor 24-hour share of new lifetime allocations. Rows carry no target
-- and are deleted once their window expires (or with the account).
CREATE TABLE moderation_actor_allocations (
 actor_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 window_start timestamptz NOT NULL DEFAULT now(),
 allocations integer NOT NULL DEFAULT 0 CHECK (allocations>=0)
);
CREATE INDEX moderation_actor_allocations_expiry ON moderation_actor_allocations(window_start);

ALTER TABLE moderation_capacity ADD COLUMN max_backfill bigint NOT NULL DEFAULT 65536 CHECK (max_backfill BETWEEN 1 AND 1000000);
ALTER TABLE moderation_capacity ADD COLUMN backfill bigint NOT NULL DEFAULT 0 CHECK (backfill>=0);

INSERT INTO moderation_overflow(kind,reason) SELECT k,r FROM unnest(ARRAY['listing','upload','dispute','review','payment_flag','all']) k
 CROSS JOIN unnest(ARRAY['actor_share','backfill_capacity']) r;

-- A revision that completed stale was never evaluated; it must not suppress a
-- later capture of the same content. Pending jobs stay unique per revision.
ALTER TABLE moderation_jobs DROP CONSTRAINT moderation_jobs_kind_target_id_content_hash_key;
CREATE UNIQUE INDEX moderation_jobs_revision ON moderation_jobs(kind,target_id,content_hash)
 WHERE completed_at IS NULL OR failure_class<>'stale_snapshot';
CREATE INDEX moderation_jobs_target ON moderation_jobs(kind,target_id);

-- Backfill re-attempts are the same missed target; they are not counted again.
CREATE OR REPLACE FUNCTION moderation_note_overflow(k text,r text) RETURNS void LANGUAGE sql AS $$
 UPDATE moderation_overflow SET occurrences=LEAST(2147483647,occurrences+1),last_seen=now()
 WHERE kind=k AND reason=r AND current_setting('opsecmkt.moderation_sweep',true) IS DISTINCT FROM 'on'
$$;

CREATE FUNCTION moderation_backfill_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE moderation_capacity SET backfill=GREATEST(0,backfill+CASE WHEN TG_OP='INSERT' THEN 1 ELSE -1 END) WHERE singleton;
 RETURN NULL;
END $$;
CREATE TRIGGER moderation_backfill_counted AFTER INSERT OR DELETE ON moderation_backfill FOR EACH ROW EXECUTE FUNCTION moderation_backfill_count();

-- Callers hold the capacity row lock, so the bound check is atomic.
CREATE FUNCTION moderation_mark_missed(k text,tid text,r text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity;
BEGIN
 IF r IS NOT NULL THEN PERFORM moderation_note_overflow(k,r); END IF;
 IF length(tid) NOT BETWEEN 1 AND 128 THEN RETURN; END IF;
 UPDATE moderation_backfill SET last_missed=now() WHERE kind=k AND target_id=tid;
 IF FOUND THEN RETURN; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 IF b.backfill>=b.max_backfill THEN PERFORM moderation_note_overflow(k,'backfill_capacity'); RETURN; END IF;
 INSERT INTO moderation_backfill(kind,target_id) VALUES(k,tid) ON CONFLICT(kind,target_id) DO UPDATE SET last_missed=now();
END $$;

-- The current, capturable revision of a target: the same canonical source and
-- digest as moderation_enqueue, without taking source row locks. Archived
-- listings, hidden reviews and unflagged payments are not captured.
CREATE FUNCTION moderation_capture_source(k text,tid text) RETURNS TABLE(digest text,actor text,meta jsonb) LANGUAGE plpgsql STABLE AS $$
DECLARE s jsonb;
BEGIN
 IF k='listing' THEN
  SELECT jsonb_build_object('title',p.title,'description',p.description,'category',p.category,'region',p.region,'kind',p.kind,'price_atomic',p.xmr,'actor_id',p.vendor_id,'actor_created',u.created) INTO s FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=tid AND NOT p.archived;
 ELSIF k='upload' THEN
  SELECT jsonb_build_object('description',p.description,'product_id',p.product_id,'image_kind',p.kind,'width',p.width,'height',p.height,'bytes',octet_length(p."full"),'image_hash',encode(sha256(p."full"),'hex'),'perceptual_hash',p.perceptual_hash,'actor_id',p.owner_user_id,'actor_created',u.created) INTO s FROM images p JOIN users u ON u.id=p.owner_user_id WHERE p.id=tid;
 ELSIF k='dispute' THEN
  SELECT jsonb_build_object('reason',p.reason,'status',p.status,'resolution',p.resolution,'order_id',p.order_id,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created) INTO s FROM disputes p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id=tid;
 ELSIF k='review' THEN
  SELECT jsonb_build_object('body',p.body,'rating',p.rating,'product_id',p.product_id,'order_id',p.order_id,'actor_id',p.buyer_id,'actor_created',u.created) INTO s FROM reviews p JOIN users u ON u.id=p.buyer_id WHERE p.id=tid AND p.hidden_at IS NULL;
 ELSIF k='payment_flag' THEN
  SELECT jsonb_build_object('order_id',p.order_id,'flagged',p.flagged,'disposition',p.disposition,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created) INTO s FROM payments p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id::text=tid AND p.flagged;
 END IF;
 IF s IS NULL OR s->>'actor_id' IS NULL THEN RETURN; END IF;
 digest:=encode(sha256(convert_to(s::text,'UTF8')),'hex');
 actor:=s->>'actor_id';
 meta:=jsonb_strip_nulls(jsonb_build_object('actor_created',s->'actor_created','order_id',s->'order_id','order_state',s->'order_state',
  'product_id',s->'product_id','image_kind',s->'image_kind','image_hash',s->'image_hash','perceptual_hash',s->'perceptual_hash',
  'width',s->'width','height',s->'height','bytes',s->'bytes'));
 RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION moderation_admit_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; a moderation_actor_allocations;
BEGIN
 IF NEW.capture_version<>2 THEN RETURN NEW; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 -- A pending or evaluated job already covers this revision.
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=NEW.content_hash
   AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
  DELETE FROM moderation_backfill WHERE kind=NEW.kind AND target_id=NEW.target_id;
  RETURN NULL;
 END IF;
 SELECT count(*) INTO n FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=NEW.actor_id AND completed_at IS NULL LIMIT 3) x;
 IF n>=3 THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_capacity'); RETURN NULL; END IF;
 IF b.pending>=b.max_pending THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'pending_capacity'); RETURN NULL; END IF;
 IF b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'retained_capacity'); RETURN NULL;
 END IF;
 SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=NEW.actor_id;
 IF FOUND AND a.window_start>now()-interval '24 hours' AND a.allocations>=GREATEST(8,b.max_records*2/100) THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_share'); RETURN NULL;
 END IF;
 INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations) VALUES(NEW.actor_id,now(),1)
 ON CONFLICT(actor_id) DO UPDATE SET
  allocations=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.allocations+1 ELSE 1 END,
  window_start=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.window_start ELSE now() END;
 UPDATE moderation_capacity SET pending=pending+1,allocated_records=allocated_records+1,allocated_bytes=allocated_bytes+524288 WHERE singleton;
 DELETE FROM moderation_backfill WHERE kind=NEW.kind AND target_id=NEW.target_id;
 NEW.snapshot:='{}'::jsonb; NEW.reserved_results:=1; NEW.results_count:=0;
 NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION moderation_job_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; d text;
BEGIN
 IF NEW.queued_at IS DISTINCT FROM OLD.queued_at THEN NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END; END IF;
 IF OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL THEN
  UPDATE moderation_capacity SET pending=GREATEST(0,pending-1),completed_v2=completed_v2+CASE WHEN NEW.capture_version=2 THEN 1 ELSE 0 END WHERE singleton;
  IF NEW.capture_version=2 THEN NEW.snapshot:='{}'; NEW.actor_id:=''; NEW.metadata:=NEW.metadata-'actor_created'; END IF;
  -- A stale completion evaluated nothing. If the live revision has no pending
  -- or evaluated job, leave a marker so the worker captures it.
  IF NEW.capture_version=2 AND NEW.failure_class='stale_snapshot'
   AND EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN
   SELECT c.digest INTO d FROM moderation_capture_source(NEW.kind,NEW.target_id) c;
   IF d IS NOT NULL AND NOT EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=d
     AND id<>NEW.id AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
    PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,NULL);
   END IF;
  END IF;
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

-- One short transaction per worker pass. The capacity row is locked first, as
-- admission does, so markers are never held while waiting for it. Expired actor
-- windows are removed even while capture is disabled; nothing is captured then.
CREATE FUNCTION moderation_backfill_sweep(lim integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; m record; c record; admitted integer:=0; budget integer;
BEGIN
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 DELETE FROM moderation_actor_allocations WHERE window_start<=now()-interval '24 hours';
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN 0; END IF;
 IF b.backfill=0 OR b.pending*2>=b.max_pending OR b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN RETURN 0; END IF;
 budget:=LEAST(GREATEST(lim,0),GREATEST(1,(b.max_pending+1)/2-b.pending));
 PERFORM set_config('opsecmkt.moderation_sweep','on',true);
 FOR m IN SELECT kind,target_id FROM moderation_backfill ORDER BY last_missed,kind,target_id LIMIT budget FOR UPDATE SKIP LOCKED LOOP
  SELECT * INTO c FROM moderation_capture_source(m.kind,m.target_id);
  IF NOT FOUND THEN
   DELETE FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id;
   CONTINUE;
  END IF;
  -- Admission deletes the marker once a job covers this revision, or keeps it
  -- (moving it behind other markers) when capacity still refuses it.
  INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot,metadata,actor_ref,capture_version)
  VALUES(m.kind,m.target_id,c.actor,c.digest,'{}',c.meta,encode(sha256(convert_to(gen_random_uuid()::text,'UTF8')),'hex'),2)
  ON CONFLICT DO NOTHING;
  IF FOUND THEN admitted:=admitted+1; END IF;
 END LOOP;
 PERFORM set_config('opsecmkt.moderation_sweep','off',true);
 RETURN admitted;
END $$;
