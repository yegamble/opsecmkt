-- MOD72-1: lossless, non-punitive moderation capture.
-- A refused capture leaves a reference-only marker: kind, target, two
-- timestamps and a parked flag, never content, digests or the actor. The worker
-- later captures the target's current revision through the same admission
-- trigger. The marketplace write always commits.
CREATE TABLE moderation_backfill (
 kind text NOT NULL CHECK (kind IN ('listing','upload','dispute','review','payment_flag')),
 target_id text NOT NULL CHECK (length(target_id) BETWEEN 1 AND 128),
 first_missed timestamptz NOT NULL DEFAULT now(),
 last_missed timestamptz NOT NULL DEFAULT now(),
 -- The source is archived, hidden or unflagged: kept until it returns or is gone.
 parked boolean NOT NULL DEFAULT false,
 PRIMARY KEY (kind,target_id)
);
-- Active markers are taken in first-refusal order; a later refusal never moves one.
CREATE INDEX moderation_backfill_due ON moderation_backfill(first_missed,kind,target_id) WHERE NOT parked;
CREATE INDEX moderation_backfill_parked ON moderation_backfill(last_missed,kind,target_id) WHERE parked;

-- Per-actor state, with no target. allocations is the 24-hour share of new
-- lifetime allocations (zeroed when its window expires or capture is disabled).
-- markers counts the actor's waiting markers (bounded per actor). Past that
-- bound, rescan replaces further markers: the worker re-reads the actor's live
-- records, a 1/256 slice (rescan_bucket) at a time, and captures every current
-- revision that has no job; rescan_more repeats the scan after refusals during
-- it. A row is deleted once its window expired with no markers and no rescan,
-- and with the account.
CREATE TABLE moderation_actor_allocations (
 actor_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 window_start timestamptz NOT NULL DEFAULT now(),
 allocations integer NOT NULL DEFAULT 0 CHECK (allocations>=0),
 markers integer NOT NULL DEFAULT 0 CHECK (markers>=0),
 rescan boolean NOT NULL DEFAULT false,
 rescan_more boolean NOT NULL DEFAULT false,
 rescan_bucket smallint NOT NULL DEFAULT 0 CHECK (rescan_bucket BETWEEN 0 AND 256)
);
CREATE INDEX moderation_actor_allocations_expiry ON moderation_actor_allocations(window_start);
CREATE INDEX moderation_actor_allocations_rescan ON moderation_actor_allocations(window_start,actor_id) WHERE rescan;

ALTER TABLE moderation_capacity ADD COLUMN max_backfill bigint NOT NULL DEFAULT 65536 CHECK (max_backfill BETWEEN 1 AND 1000000);
ALTER TABLE moderation_capacity ADD COLUMN backfill bigint NOT NULL DEFAULT 0 CHECK (backfill>=0);
-- Markers per actor; the effective bound is also below max_backfill, so one
-- actor can never fill the marker table.
ALTER TABLE moderation_capacity ADD COLUMN max_actor_markers bigint NOT NULL DEFAULT 16 CHECK (max_actor_markers BETWEEN 0 AND 100000);

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

-- Callers hold the capacity row lock, so the bound checks are atomic. An actor
-- at its marker bound, or a full marker table, sets the actor's rescan flag
-- instead: nothing refused is lost. Inside the sweep, a refusal of a target
-- without a marker leaves no flag; the sweep retries it itself.
CREATE FUNCTION moderation_mark_missed(k text,tid text,r text,aid text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; held integer;
BEGIN
 IF r IS NOT NULL THEN PERFORM moderation_note_overflow(k,r); END IF;
 IF length(tid) NOT BETWEEN 1 AND 128 THEN RETURN; END IF;
 UPDATE moderation_backfill SET last_missed=now(),parked=false WHERE kind=k AND target_id=tid;
 IF FOUND OR aid IS NULL OR aid='' THEN RETURN; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 SELECT markers INTO held FROM moderation_actor_allocations WHERE actor_id=aid;
 IF COALESCE(held,0)<GREATEST(0,LEAST(b.max_actor_markers,b.max_backfill-1)) AND b.backfill<b.max_backfill THEN
  INSERT INTO moderation_backfill(kind,target_id) VALUES(k,tid);
  INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations,markers) VALUES(aid,now(),0,1)
  ON CONFLICT(actor_id) DO UPDATE SET markers=moderation_actor_allocations.markers+1;
  RETURN;
 END IF;
 IF b.backfill>=b.max_backfill THEN PERFORM moderation_note_overflow(k,'backfill_capacity'); END IF;
 IF current_setting('opsecmkt.moderation_sweep',true)='on' THEN RETURN; END IF;
 INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations,rescan) VALUES(aid,now(),0,true)
 ON CONFLICT(actor_id) DO UPDATE SET rescan_more=moderation_actor_allocations.rescan_more OR moderation_actor_allocations.rescan,rescan=true;
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

-- 'live' (capturable now), 'held' (archived, hidden or unflagged: it can
-- return) or 'gone'.
CREATE FUNCTION moderation_source_state(k text,tid text) RETURNS text LANGUAGE plpgsql STABLE AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM moderation_capture_source(k,tid)) THEN RETURN 'live'; END IF;
 IF k='listing' AND EXISTS(SELECT 1 FROM products WHERE id=tid) OR k='review' AND EXISTS(SELECT 1 FROM reviews WHERE id=tid)
  OR k='payment_flag' AND EXISTS(SELECT 1 FROM payments WHERE id::text=tid) THEN RETURN 'held'; END IF;
 RETURN 'gone';
END $$;

-- The exact number of markers whose live source belongs to an actor, used to
-- correct the stored count after markers of deleted sources were dropped.
CREATE FUNCTION moderation_actor_marker_count(aid text) RETURNS integer LANGUAGE sql STABLE AS $$
 SELECT ((SELECT count(*) FROM moderation_backfill b JOIN products p ON p.id=b.target_id WHERE b.kind='listing' AND p.vendor_id=aid)
  +(SELECT count(*) FROM moderation_backfill b JOIN images p ON p.id=b.target_id WHERE b.kind='upload' AND p.owner_user_id=aid)
  +(SELECT count(*) FROM moderation_backfill b JOIN reviews p ON p.id=b.target_id WHERE b.kind='review' AND p.buyer_id=aid)
  +(SELECT count(*) FROM moderation_backfill b JOIN disputes p ON p.id=b.target_id JOIN orders o ON o.id=p.order_id WHERE b.kind='dispute' AND o.buyer_id=aid)
  +(SELECT count(*) FROM moderation_backfill b JOIN payments p ON p.id::text=b.target_id JOIN orders o ON o.id=p.order_id WHERE b.kind='payment_flag' AND o.buyer_id=aid))::integer
$$;

CREATE OR REPLACE FUNCTION moderation_admit_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; a moderation_actor_allocations;
BEGIN
 IF NEW.capture_version<>2 THEN RETURN NEW; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 -- A pending or evaluated job already covers this revision.
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=NEW.content_hash
   AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
  DELETE FROM moderation_backfill WHERE kind=NEW.kind AND target_id=NEW.target_id;
  IF FOUND THEN UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-1) WHERE actor_id=NEW.actor_id; END IF;
  RETURN NULL;
 END IF;
 SELECT count(*) INTO n FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=NEW.actor_id AND completed_at IS NULL LIMIT 3) x;
 IF n>=3 THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_capacity',NEW.actor_id); RETURN NULL; END IF;
 IF b.pending>=b.max_pending THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'pending_capacity',NEW.actor_id); RETURN NULL; END IF;
 IF b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'retained_capacity',NEW.actor_id); RETURN NULL;
 END IF;
 DELETE FROM moderation_actor_allocations WHERE window_start<=now()-interval '24 hours' AND markers=0 AND NOT rescan;
 SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=NEW.actor_id;
 IF FOUND AND a.window_start>now()-interval '24 hours' AND a.allocations>=GREATEST(8,b.max_records*2/100) THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_share',NEW.actor_id); RETURN NULL;
 END IF;
 INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations) VALUES(NEW.actor_id,now(),1)
 ON CONFLICT(actor_id) DO UPDATE SET
  allocations=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.allocations+1 ELSE 1 END,
  window_start=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.window_start ELSE now() END;
 UPDATE moderation_capacity SET pending=pending+1,allocated_records=allocated_records+1,allocated_bytes=allocated_bytes+524288 WHERE singleton;
 DELETE FROM moderation_backfill WHERE kind=NEW.kind AND target_id=NEW.target_id;
 IF FOUND THEN UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-1) WHERE actor_id=NEW.actor_id; END IF;
 NEW.snapshot:='{}'::jsonb; NEW.reserved_results:=1; NEW.results_count:=0;
 NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END;
 RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION moderation_job_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; c record;
BEGIN
 IF NEW.queued_at IS DISTINCT FROM OLD.queued_at THEN NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END; END IF;
 IF OLD.completed_at IS NULL AND NEW.completed_at IS NOT NULL THEN
  UPDATE moderation_capacity SET pending=GREATEST(0,pending-1),completed_v2=completed_v2+CASE WHEN NEW.capture_version=2 THEN 1 ELSE 0 END WHERE singleton;
  IF NEW.capture_version=2 THEN NEW.snapshot:='{}'; NEW.actor_id:=''; NEW.metadata:=NEW.metadata-'actor_created'; END IF;
  -- A stale completion evaluated nothing. If the source itself changed and its
  -- live revision has no pending or evaluated job, leave a marker. A stale
  -- result from changed context alone (the source digest is unchanged) stays a
  -- HOLD for humans and is not captured again.
  IF NEW.capture_version=2 AND NEW.failure_class='stale_snapshot'
   AND EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN
   SELECT * INTO c FROM moderation_capture_source(NEW.kind,NEW.target_id);
   IF FOUND AND c.digest<>NEW.content_hash AND NOT EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=c.digest
     AND id<>NEW.id AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
    PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,NULL,c.actor);
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

-- Restoring an archived listing or unhiding a review makes its current revision
-- capturable again; dedup still suppresses an already evaluated revision.
CREATE OR REPLACE FUNCTION moderation_enqueue() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE s jsonb; k text; tid text; aid text; h text; meta jsonb; ref text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN NEW; END IF;
 IF TG_TABLE_NAME='products' THEN
  IF TG_OP='UPDATE' AND (NEW.title,NEW.description,NEW.category,NEW.region,NEW.kind,NEW.xmr) IS NOT DISTINCT FROM (OLD.title,OLD.description,OLD.category,OLD.region,OLD.kind,OLD.xmr)
   AND NOT (OLD.archived AND NOT NEW.archived) THEN RETURN NEW; END IF;
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
  IF TG_OP='UPDATE' AND (NEW.body,NEW.rating) IS NOT DISTINCT FROM (OLD.body,OLD.rating)
   AND NOT (OLD.hidden_at IS NOT NULL AND NEW.hidden_at IS NULL) THEN RETURN NEW; END IF;
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

-- Queue one current revision through admission. Returns 'queued', 'covered'
-- (a pending or evaluated job exists) or 'refused'.
CREATE FUNCTION moderation_backfill_capture(k text,tid text,d text,aid text,meta jsonb) RETURNS text LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO moderation_jobs(kind,target_id,actor_id,content_hash,snapshot,metadata,actor_ref,capture_version)
 VALUES(k,tid,aid,d,'{}',meta,encode(sha256(convert_to(gen_random_uuid()::text,'UTF8')),'hex'),2)
 ON CONFLICT DO NOTHING;
 IF FOUND THEN RETURN 'queued'; END IF;
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=k AND target_id=tid AND content_hash=d AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN RETURN 'covered'; END IF;
 RETURN 'refused';
END $$;

-- One rescan step for an actor: walk its live records from bucket cur in
-- (bucket, kind, id) order and queue current revisions without a job. Stops when
-- the actor or market lacks capacity (returning that bucket, to be repeated) or
-- after the digest budget at a bucket boundary. Returns 256 at the end. Uploads
-- are captured once, like their insert trigger.
CREATE FUNCTION moderation_rescan_actor(aid text,cur integer,share bigint,budget integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE t record; c record; work integer:=0; bucket integer:=cur; b moderation_capacity; active integer; a moderation_actor_allocations;
BEGIN
 FOR t IN SELECT x.k,x.tid,x.bk FROM (
   SELECT 'listing' k,id tid,hashtext('listing:'||id)&255 bk FROM products WHERE vendor_id=aid AND NOT archived
   UNION ALL SELECT 'upload',id,hashtext('upload:'||id)&255 FROM images WHERE owner_user_id=aid
   UNION ALL SELECT 'review',id,hashtext('review:'||id)&255 FROM reviews WHERE buyer_id=aid AND hidden_at IS NULL
   UNION ALL SELECT 'dispute',d.id,hashtext('dispute:'||d.id)&255 FROM disputes d JOIN orders o ON o.id=d.order_id WHERE o.buyer_id=aid
   UNION ALL SELECT 'payment_flag',p.id::text,hashtext('payment_flag:'||p.id::text)&255 FROM payments p JOIN orders o ON o.id=p.order_id WHERE o.buyer_id=aid AND p.flagged
  ) x WHERE x.bk>=cur ORDER BY x.bk,x.k,x.tid LOOP
  IF t.bk<>bucket THEN
   bucket:=t.bk;
   IF work>=budget THEN RETURN bucket; END IF;
  END IF;
  IF t.k='upload' AND EXISTS(SELECT 1 FROM moderation_jobs WHERE kind='upload' AND target_id=t.tid AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN CONTINUE; END IF;
  work:=work+1;
  SELECT * INTO c FROM moderation_capture_source(t.k,t.tid);
  IF NOT FOUND OR EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=t.k AND target_id=t.tid AND content_hash=c.digest AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN CONTINUE; END IF;
  SELECT * INTO b FROM moderation_capacity WHERE singleton;
  SELECT count(*) INTO active FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=aid AND completed_at IS NULL LIMIT 3) x;
  SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=aid;
  IF active>=3 OR b.pending*2>=b.max_pending OR b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes
   OR a.window_start>now()-interval '24 hours' AND a.allocations>=share THEN RETURN bucket; END IF;
  IF moderation_backfill_capture(t.k,t.tid,c.digest,c.actor,c.meta)='refused' THEN RETURN bucket; END IF;
 END LOOP;
 RETURN 256;
END $$;

-- One short transaction per worker pass, holding the capacity row lock first,
-- as admission does, so markers and actor rows are never held while waiting for
-- it. Expired share windows are zeroed (and empty rows deleted) even while
-- capture is disabled; nothing is captured then.
CREATE FUNCTION moderation_backfill_sweep(lim integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; m record; c record; r record; admitted integer:=0; budget integer; share bigint; refused text[]; st text; nxt integer;
BEGIN
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 UPDATE moderation_actor_allocations SET allocations=0 WHERE window_start<=now()-interval '24 hours' AND allocations>0;
 DELETE FROM moderation_actor_allocations WHERE window_start<=now()-interval '24 hours' AND markers=0 AND NOT rescan;
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN 0; END IF;
 -- Markers of deleted sources were dropped without knowing their actor; correct
 -- one idle count per pass, rotating through such rows.
 FOR r IN SELECT actor_id FROM moderation_actor_allocations WHERE window_start<=now()-interval '24 hours' AND markers>0 AND NOT rescan
  ORDER BY window_start,actor_id LIMIT 1 FOR UPDATE LOOP
  UPDATE moderation_actor_allocations SET markers=moderation_actor_marker_count(r.actor_id),window_start=now() WHERE actor_id=r.actor_id;
 END LOOP;
 IF b.pending*2>=b.max_pending OR b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN RETURN 0; END IF;
 budget:=LEAST(GREATEST(lim,0),GREATEST(1,(b.max_pending+1)/2-b.pending));
 share:=GREATEST(8,b.max_records*2/100);
 PERFORM set_config('opsecmkt.moderation_sweep','on',true);
 -- Actors that admission would refuse now; their markers do not use the budget.
 refused:=ARRAY(SELECT actor_id FROM moderation_jobs WHERE completed_at IS NULL AND actor_id<>'' GROUP BY actor_id HAVING count(*)>=3
  UNION SELECT actor_id FROM moderation_actor_allocations WHERE window_start>now()-interval '24 hours' AND allocations>=share);
 -- Parked markers: a few per pass, oldest check first. A returned source
 -- becomes active again; a gone one loses its marker.
 FOR m IN SELECT kind,target_id FROM moderation_backfill WHERE parked ORDER BY last_missed,kind,target_id LIMIT 8 FOR UPDATE SKIP LOCKED LOOP
  st:=moderation_source_state(m.kind,m.target_id);
  IF st='gone' THEN DELETE FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id;
  ELSIF st='held' THEN UPDATE moderation_backfill SET last_missed=now() WHERE kind=m.kind AND target_id=m.target_id;
  ELSE UPDATE moderation_backfill SET parked=false WHERE kind=m.kind AND target_id=m.target_id;
  END IF;
 END LOOP;
 -- Active markers in first-refusal order, skipping refused actors within a
 -- bounded window of 4096 markers.
 FOR m IN SELECT x.kind,x.target_id FROM (SELECT kind,target_id,first_missed FROM moderation_backfill WHERE NOT parked ORDER BY first_missed,kind,target_id LIMIT 4096) x
  WHERE COALESCE(moderation_live_actor(x.kind,x.target_id),'')<>ALL(refused)
  ORDER BY x.first_missed,x.kind,x.target_id LIMIT budget LOOP
  PERFORM 1 FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id FOR UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  st:=moderation_source_state(m.kind,m.target_id);
  IF st='gone' THEN DELETE FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id; CONTINUE; END IF;
  IF st='held' THEN UPDATE moderation_backfill SET parked=true,last_missed=now() WHERE kind=m.kind AND target_id=m.target_id; CONTINUE; END IF;
  SELECT * INTO c FROM moderation_capture_source(m.kind,m.target_id);
  -- Admission deletes the marker once a job covers this revision, or keeps it
  -- in place when capacity still refuses it.
  IF moderation_backfill_capture(m.kind,m.target_id,c.digest,c.actor,c.meta)='queued' THEN admitted:=admitted+1; END IF;
 END LOOP;
 -- Actor rescans: up to four actors with capacity, 64 digests each, so the
 -- capacity lock is held briefly; progress resumes at the stored position.
 FOR r IN SELECT actor_id,rescan_bucket,rescan_more FROM moderation_actor_allocations WHERE rescan AND actor_id<>ALL(refused)
  ORDER BY window_start,actor_id LIMIT 4 FOR UPDATE LOOP
  nxt:=moderation_rescan_actor(r.actor_id,r.rescan_bucket,share,64);
  IF nxt<256 THEN
   UPDATE moderation_actor_allocations SET rescan_bucket=nxt WHERE actor_id=r.actor_id;
  ELSIF r.rescan_more THEN
   UPDATE moderation_actor_allocations SET rescan_bucket=0,rescan_more=false WHERE actor_id=r.actor_id;
  ELSE
   UPDATE moderation_actor_allocations SET rescan=false,rescan_bucket=0,markers=moderation_actor_marker_count(r.actor_id) WHERE actor_id=r.actor_id;
  END IF;
 END LOOP;
 PERFORM set_config('opsecmkt.moderation_sweep','off',true);
 RETURN admitted;
END $$;
