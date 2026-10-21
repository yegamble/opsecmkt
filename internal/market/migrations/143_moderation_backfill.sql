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
 -- An unflagged payment: kept until it is flagged again or gone.
 parked boolean NOT NULL DEFAULT false,
 -- The record was deleted; the next worker pass removes the marker and
 -- releases its owner's counts (the deleting transaction takes no global lock).
 gone boolean NOT NULL DEFAULT false,
 PRIMARY KEY (kind,target_id)
);
-- Active markers are taken in first-refusal order; a later refusal never moves one.
CREATE INDEX moderation_backfill_due ON moderation_backfill(first_missed,kind,target_id) WHERE NOT parked AND NOT gone;
CREATE INDEX moderation_backfill_parked ON moderation_backfill(last_missed,kind,target_id) WHERE parked AND NOT gone;
CREATE INDEX moderation_backfill_gone ON moderation_backfill(kind,target_id) WHERE gone;

-- Per-actor state. allocations is the 24-hour share of new lifetime
-- allocations (zeroed when its window expires or capture is disabled). markers
-- counts the actor's waiting markers (bounded per actor) and parked how many of
-- them are parked. While an actor has an active backlog (markers>parked, or a
-- rescan), its new captures queue behind it instead of being admitted, so its
-- share is spent in first-refusal order. Past the marker bound, rescan replaces
-- further markers: the worker re-reads the actor's live records in (creation
-- time, kind, id) order from a stored position (rescan_ts, rescan_kind,
-- rescan_id: the last record read, kept only while the rescan runs and capture
-- is on) and
-- captures every current revision that has no job; rescan_more repeats the
-- re-read after refusals during it. A row is deleted once its window expired
-- with no markers and no rescan, and with the account.
CREATE TABLE moderation_actor_allocations (
 actor_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 window_start timestamptz NOT NULL DEFAULT now(),
 allocations integer NOT NULL DEFAULT 0 CHECK (allocations>=0),
 markers integer NOT NULL DEFAULT 0 CHECK (markers>=0),
 parked integer NOT NULL DEFAULT 0 CHECK (parked>=0),
 rescan boolean NOT NULL DEFAULT false,
 rescan_more boolean NOT NULL DEFAULT false,
 rescan_ts timestamptz,
 rescan_kind text CHECK (rescan_kind IN ('listing','upload','dispute','review','payment_flag')),
 rescan_id text CHECK (length(rescan_id)<=128),
 CHECK ((rescan_ts IS NULL)=(rescan_kind IS NULL) AND (rescan_kind IS NULL)=(rescan_id IS NULL)),
 CHECK (rescan OR rescan_ts IS NULL)
);
CREATE INDEX moderation_actor_allocations_expiry ON moderation_actor_allocations(window_start);
CREATE INDEX moderation_actor_allocations_rescan ON moderation_actor_allocations(window_start,actor_id) WHERE rescan;
-- Index-backed rescan walks: each step reads at most its budget per kind.
CREATE INDEX moderation_rescan_products ON products(vendor_id,created,id);
CREATE INDEX moderation_rescan_images ON images(owner_user_id,created,id);
CREATE INDEX moderation_rescan_reviews ON reviews(buyer_id,created,id);

-- Counts a deleting transaction releases, applied by the next worker pass under
-- its capacity lock: the owner and whether the marker was parked, never the
-- record. Removed by that pass, or with the account.
CREATE TABLE moderation_marker_releases (
 actor_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 parked boolean NOT NULL
);
CREATE INDEX moderation_marker_releases_actor ON moderation_marker_releases(actor_id);
CREATE INDEX moderation_rescan_orders ON orders(buyer_id);

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
-- without a marker leaves no flag; the sweep retries it itself. A later miss of
-- a marked target never moves it; a parked one becomes active again.
CREATE FUNCTION moderation_mark_missed(k text,tid text,r text,aid text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; held integer; wasp boolean;
BEGIN
 IF r IS NOT NULL THEN PERFORM moderation_note_overflow(k,r); END IF;
 IF length(tid) NOT BETWEEN 1 AND 128 THEN RETURN; END IF;
 -- A marker another transaction holds (its source is being deleted) is left
 -- to that transaction, never waited for while the capacity lock is held.
 SELECT parked INTO wasp FROM moderation_backfill WHERE kind=k AND target_id=tid AND NOT gone FOR UPDATE SKIP LOCKED;
 IF NOT FOUND AND EXISTS(SELECT 1 FROM moderation_backfill WHERE kind=k AND target_id=tid) THEN RETURN; END IF;
 IF FOUND THEN
  UPDATE moderation_backfill SET last_missed=now(),parked=false WHERE kind=k AND target_id=tid;
  IF wasp AND aid IS NOT NULL AND aid<>'' THEN
   UPDATE moderation_actor_allocations SET parked=GREATEST(0,parked-1) WHERE actor_id=aid;
  END IF;
  RETURN;
 END IF;
 IF aid IS NULL OR aid='' THEN RETURN; END IF;
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
-- listings and hidden reviews are captured like live ones (withdrawing a record
-- cannot withdraw it from review); unflagged payments are not captured.
CREATE FUNCTION moderation_capture_source(k text,tid text) RETURNS TABLE(digest text,actor text,meta jsonb) LANGUAGE plpgsql STABLE AS $$
DECLARE s jsonb;
BEGIN
 IF k='listing' THEN
  SELECT jsonb_build_object('title',p.title,'description',p.description,'category',p.category,'region',p.region,'kind',p.kind,'price_atomic',p.xmr,'actor_id',p.vendor_id,'actor_created',u.created) INTO s FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=tid;
 ELSIF k='upload' THEN
  SELECT jsonb_build_object('description',p.description,'product_id',p.product_id,'image_kind',p.kind,'width',p.width,'height',p.height,'bytes',octet_length(p."full"),'image_hash',encode(sha256(p."full"),'hex'),'perceptual_hash',p.perceptual_hash,'actor_id',p.owner_user_id,'actor_created',u.created) INTO s FROM images p JOIN users u ON u.id=p.owner_user_id WHERE p.id=tid;
 ELSIF k='dispute' THEN
  SELECT jsonb_build_object('reason',p.reason,'status',p.status,'resolution',p.resolution,'order_id',p.order_id,'order_state',o.state,'actor_id',o.buyer_id,'actor_created',u.created) INTO s FROM disputes p JOIN orders o ON o.id=p.order_id JOIN users u ON u.id=o.buyer_id WHERE p.id=tid;
 ELSIF k='review' THEN
  SELECT jsonb_build_object('body',p.body,'rating',p.rating,'product_id',p.product_id,'order_id',p.order_id,'actor_id',p.buyer_id,'actor_created',u.created) INTO s FROM reviews p JOIN users u ON u.id=p.buyer_id WHERE p.id=tid;
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

-- 'live' (capturable now), 'held' (an unflagged payment: it can be flagged
-- again) or 'gone'.
CREATE FUNCTION moderation_source_state(k text,tid text) RETURNS text LANGUAGE plpgsql STABLE AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM moderation_capture_source(k,tid)) THEN RETURN 'live'; END IF;
 IF k='payment_flag' AND EXISTS(SELECT 1 FROM payments WHERE id::text=tid) THEN RETURN 'held'; END IF;
 RETURN 'gone';
END $$;

-- The exact number of markers (or of parked markers) whose live source
-- belongs to an actor, used to correct the stored counts after markers of
-- deleted sources were dropped without knowing their actor.
CREATE FUNCTION moderation_actor_marker_count(aid text,only_parked boolean) RETURNS integer LANGUAGE sql STABLE AS $$
 SELECT ((SELECT count(*) FROM moderation_backfill b JOIN products p ON p.id=b.target_id WHERE b.kind='listing' AND p.vendor_id=aid AND NOT b.gone AND (b.parked OR NOT only_parked))
  +(SELECT count(*) FROM moderation_backfill b JOIN images p ON p.id=b.target_id WHERE b.kind='upload' AND p.owner_user_id=aid AND NOT b.gone AND (b.parked OR NOT only_parked))
  +(SELECT count(*) FROM moderation_backfill b JOIN reviews p ON p.id=b.target_id WHERE b.kind='review' AND p.buyer_id=aid AND NOT b.gone AND (b.parked OR NOT only_parked))
  +(SELECT count(*) FROM moderation_backfill b JOIN disputes p ON p.id=b.target_id JOIN orders o ON o.id=p.order_id WHERE b.kind='dispute' AND o.buyer_id=aid AND NOT b.gone AND (b.parked OR NOT only_parked))
  +(SELECT count(*) FROM moderation_backfill b JOIN payments p ON p.id::text=b.target_id JOIN orders o ON o.id=p.order_id WHERE b.kind='payment_flag' AND o.buyer_id=aid AND NOT b.gone AND (b.parked OR NOT only_parked)))::integer
$$;

CREATE OR REPLACE FUNCTION moderation_admit_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; a moderation_actor_allocations; wasp boolean;
BEGIN
 IF NEW.capture_version<>2 THEN RETURN NEW; END IF;
 -- A source that vanished has no actor; there is nothing to capture.
 IF NEW.actor_id IS NULL OR NEW.actor_id='' THEN RETURN NULL; END IF;
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 -- A pending or evaluated job already covers this revision.
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=NEW.content_hash
   AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
  DELETE FROM moderation_backfill WHERE (kind,target_id) IN (SELECT kind,target_id FROM moderation_backfill
   WHERE kind=NEW.kind AND target_id=NEW.target_id AND NOT gone FOR UPDATE SKIP LOCKED) RETURNING parked INTO wasp;
  IF FOUND THEN UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-1),parked=GREATEST(0,parked-CASE WHEN wasp THEN 1 ELSE 0 END) WHERE actor_id=NEW.actor_id; END IF;
  RETURN NULL;
 END IF;
 -- Per-actor first in, first out: while the actor has an active backlog, a new
 -- capture queues behind it (the sweep admits the backlog in first-refusal
 -- order) instead of spending the actor's share ahead of it.
 IF current_setting('opsecmkt.moderation_sweep',true) IS DISTINCT FROM 'on' THEN
  SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=NEW.actor_id;
  IF FOUND AND (a.rescan OR a.markers>a.parked) THEN
   PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,NULL,NEW.actor_id); RETURN NULL;
  END IF;
 END IF;
 SELECT count(*) INTO n FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=NEW.actor_id AND completed_at IS NULL LIMIT 3) x;
 IF n>=3 THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_capacity',NEW.actor_id); RETURN NULL; END IF;
 IF b.pending>=b.max_pending THEN PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'pending_capacity',NEW.actor_id); RETURN NULL; END IF;
 IF b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'retained_capacity',NEW.actor_id); RETURN NULL;
 END IF;
 -- Rows locked by another transaction (an account being erased) are left for
 -- a later admission or pass rather than waited for.
 DELETE FROM moderation_actor_allocations WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations
  WHERE window_start<=now()-interval '24 hours' AND markers=0 AND NOT rescan FOR UPDATE SKIP LOCKED);
 SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=NEW.actor_id;
 IF FOUND AND a.window_start>now()-interval '24 hours' AND a.allocations>=GREATEST(8,b.max_records*2/100) THEN
  PERFORM moderation_mark_missed(NEW.kind,NEW.target_id,'actor_share',NEW.actor_id); RETURN NULL;
 END IF;
 INSERT INTO moderation_actor_allocations(actor_id,window_start,allocations) VALUES(NEW.actor_id,now(),1)
 ON CONFLICT(actor_id) DO UPDATE SET
  allocations=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.allocations+1 ELSE 1 END,
  window_start=CASE WHEN moderation_actor_allocations.window_start>now()-interval '24 hours' THEN moderation_actor_allocations.window_start ELSE now() END;
 UPDATE moderation_capacity SET pending=pending+1,allocated_records=allocated_records+1,allocated_bytes=allocated_bytes+524288 WHERE singleton;
 DELETE FROM moderation_backfill WHERE (kind,target_id) IN (SELECT kind,target_id FROM moderation_backfill
  WHERE kind=NEW.kind AND target_id=NEW.target_id AND NOT gone FOR UPDATE SKIP LOCKED) RETURNING parked INTO wasp;
 IF FOUND THEN UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-1),parked=GREATEST(0,parked-CASE WHEN wasp THEN 1 ELSE 0 END) WHERE actor_id=NEW.actor_id; END IF;
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

-- Deleting a marked record only flags its marker and queues its owner's count
-- release. It takes no capacity lock and touches no account row, so it keeps
-- the lock order of admission, completion and the sweep (source rows first,
-- capacity last). The next worker pass, enabled or not, removes the marker and
-- applies the release under its own capacity lock.
CREATE FUNCTION moderation_source_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text; tid text; aid text; wasp boolean;
BEGIN
 IF TG_TABLE_NAME='products' THEN k:='listing'; tid:=OLD.id; aid:=OLD.vendor_id;
 ELSIF TG_TABLE_NAME='images' THEN k:='upload'; tid:=OLD.id; aid:=OLD.owner_user_id;
 ELSIF TG_TABLE_NAME='reviews' THEN k:='review'; tid:=OLD.id; aid:=OLD.buyer_id;
 ELSIF TG_TABLE_NAME='disputes' THEN k:='dispute'; tid:=OLD.id; SELECT buyer_id INTO aid FROM orders WHERE id=OLD.order_id;
 ELSE k:='payment_flag'; tid:=OLD.id::text; SELECT buyer_id INTO aid FROM orders WHERE id=OLD.order_id;
 END IF;
 UPDATE moderation_backfill SET gone=true WHERE kind=k AND target_id=tid AND NOT gone RETURNING parked INTO wasp;
 IF FOUND AND aid IS NOT NULL THEN
  INSERT INTO moderation_marker_releases(actor_id,parked) VALUES(aid,wasp);
 END IF;
 RETURN NULL;
END $$;

-- Start of every worker pass (and of disabling capture), under the capacity
-- lock: remove markers of deleted records, apply queued count releases, and
-- clear rescan positions that name a record no longer present (the rescan
-- restarts; dedup prevents duplicate jobs). Account rows held by another
-- transaction (an erasure) are left for the next pass.
CREATE FUNCTION moderation_backfill_cleanup() RETURNS void LANGUAGE plpgsql AS $$
DECLARE r record; n integer; pk integer;
BEGIN
 PERFORM 1 FROM moderation_capacity WHERE singleton FOR UPDATE;
 DELETE FROM moderation_backfill WHERE gone;
 FOR r IN SELECT a.actor_id FROM moderation_actor_allocations a JOIN users u ON u.id=a.actor_id
  WHERE EXISTS(SELECT 1 FROM moderation_marker_releases x WHERE x.actor_id=a.actor_id)
  FOR UPDATE OF a SKIP LOCKED FOR KEY SHARE OF u SKIP LOCKED LOOP
  WITH d AS (DELETE FROM moderation_marker_releases WHERE actor_id=r.actor_id RETURNING parked)
   SELECT count(*),count(*) FILTER (WHERE parked) INTO n,pk FROM d;
  UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-n),parked=GREATEST(0,parked-pk) WHERE actor_id=r.actor_id;
 END LOOP;
 DELETE FROM moderation_marker_releases x WHERE NOT EXISTS(SELECT 1 FROM moderation_actor_allocations a WHERE a.actor_id=x.actor_id);
 UPDATE moderation_actor_allocations SET rescan_ts=NULL,rescan_kind=NULL,rescan_id=NULL WHERE actor_id IN (
  SELECT a.actor_id FROM moderation_actor_allocations a JOIN users u ON u.id=a.actor_id WHERE a.rescan_id IS NOT NULL AND NOT (
   a.rescan_kind='listing' AND EXISTS(SELECT 1 FROM products WHERE id=a.rescan_id)
   OR a.rescan_kind='upload' AND EXISTS(SELECT 1 FROM images WHERE id=a.rescan_id)
   OR a.rescan_kind='review' AND EXISTS(SELECT 1 FROM reviews WHERE id=a.rescan_id)
   OR a.rescan_kind='dispute' AND EXISTS(SELECT 1 FROM disputes WHERE id=a.rescan_id)
   OR a.rescan_kind='payment_flag' AND EXISTS(SELECT 1 FROM payments WHERE id::text=a.rescan_id))
  FOR UPDATE OF a SKIP LOCKED FOR KEY SHARE OF u SKIP LOCKED);
END $$;
CREATE TRIGGER moderation_product_deleted AFTER DELETE ON products FOR EACH ROW EXECUTE FUNCTION moderation_source_deleted();
CREATE TRIGGER moderation_image_deleted AFTER DELETE ON images FOR EACH ROW EXECUTE FUNCTION moderation_source_deleted();
CREATE TRIGGER moderation_review_deleted AFTER DELETE ON reviews FOR EACH ROW EXECUTE FUNCTION moderation_source_deleted();
CREATE TRIGGER moderation_dispute_deleted AFTER DELETE ON disputes FOR EACH ROW EXECUTE FUNCTION moderation_source_deleted();
CREATE TRIGGER moderation_payment_deleted AFTER DELETE ON payments FOR EACH ROW EXECUTE FUNCTION moderation_source_deleted();

-- One rescan step for an actor: walk its live records after the stored
-- position in (creation time, kind, id) order, at most budget rows, and queue
-- each current revision that has no job. The account cannot move a record in
-- this order: edits, price changes, stock, photos and archive/restore leave its
-- creation time alone. A record already passed and edited later is re-read by
-- the repeat that rescan_more requests. Stops early when the actor or market
-- lacks capacity, keeping the position before that record. Uploads are captured
-- once, like their insert trigger.
CREATE FUNCTION moderation_rescan_actor(aid text,cts timestamptz,ck text,cid text,share bigint,budget integer)
 RETURNS TABLE(done boolean,ts timestamptz,k text,tid text) LANGUAGE plpgsql AS $$
DECLARE t record; c record; seen integer:=0; b moderation_capacity; active integer; a moderation_actor_allocations;
BEGIN
 done:=false; ts:=cts; k:=ck; tid:=cid;
 FOR t IN SELECT x.k,x.tid,x.ts FROM (
   (SELECT 'listing'::text k,p.id tid,p.created ts FROM products p WHERE p.vendor_id=aid
     AND (cts IS NULL OR p.created>=cts AND (p.created>cts OR 'listing'>ck OR 'listing'=ck AND p.id>cid)) ORDER BY p.created,p.id LIMIT budget)
   UNION ALL (SELECT 'upload',i.id,i.created FROM images i WHERE i.owner_user_id=aid
     AND (cts IS NULL OR i.created>=cts AND (i.created>cts OR 'upload'>ck OR 'upload'=ck AND i.id>cid)) ORDER BY i.created,i.id LIMIT budget)
   UNION ALL (SELECT 'review',r.id,r.created FROM reviews r WHERE r.buyer_id=aid
     AND (cts IS NULL OR r.created>=cts AND (r.created>cts OR 'review'>ck OR 'review'=ck AND r.id>cid)) ORDER BY r.created,r.id LIMIT budget)
   UNION ALL (SELECT 'dispute',d.id,d.created FROM orders o JOIN disputes d ON d.order_id=o.id WHERE o.buyer_id=aid
     AND (cts IS NULL OR d.created>=cts AND (d.created>cts OR 'dispute'>ck OR 'dispute'=ck AND d.id>cid)) ORDER BY d.created,d.id LIMIT budget)
   UNION ALL (SELECT 'payment_flag',pm.id::text,pm.created FROM orders o JOIN payments pm ON pm.order_id=o.id WHERE o.buyer_id=aid AND pm.flagged
     AND (cts IS NULL OR pm.created>=cts AND (pm.created>cts OR 'payment_flag'>ck OR 'payment_flag'=ck AND pm.id::text>cid)) ORDER BY pm.created,pm.id::text LIMIT budget)
  ) x ORDER BY x.ts,x.k,x.tid LIMIT budget LOOP
  seen:=seen+1;
  IF NOT (t.k='upload' AND EXISTS(SELECT 1 FROM moderation_jobs WHERE kind='upload' AND target_id=t.tid AND (completed_at IS NULL OR failure_class<>'stale_snapshot'))) THEN
   SELECT * INTO c FROM moderation_capture_source(t.k,t.tid);
   IF FOUND AND NOT EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=t.k AND target_id=t.tid AND content_hash=c.digest AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
    SELECT * INTO b FROM moderation_capacity WHERE singleton;
    SELECT count(*) INTO active FROM (SELECT 1 FROM moderation_jobs WHERE actor_id=aid AND completed_at IS NULL LIMIT 3) x;
    SELECT * INTO a FROM moderation_actor_allocations WHERE actor_id=aid;
    IF active>=3 OR b.pending*2>=b.max_pending OR b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes
     OR a.window_start>now()-interval '24 hours' AND a.allocations>=share THEN RETURN NEXT; RETURN; END IF;
    IF moderation_backfill_capture(t.k,t.tid,c.digest,c.actor,c.meta)='refused' THEN RETURN NEXT; RETURN; END IF;
   END IF;
  END IF;
  ts:=t.ts; k:=t.k; tid:=t.tid;
 END LOOP;
 done:=seen<budget;
 RETURN NEXT;
END $$;

-- One short transaction per worker pass, holding the capacity row lock first,
-- as admission does. Allocation rows (and, before a row is rewritten, its
-- account row, which the foreign-key check would otherwise wait for) are taken
-- with SKIP LOCKED, so an account erasure or a capture switch holding one never
-- deadlocks the pass. Expired
-- share windows are zeroed (and empty rows deleted) even while capture is
-- disabled; nothing is captured then.
CREATE FUNCTION moderation_backfill_sweep(lim integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; m record; c record; r record; st record; admitted integer:=0; budget integer; share bigint;
 refused text[]; seen text[]:='{}'; act text; state text; scanned integer:=0; taken integer:=0;
BEGIN
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE;
 PERFORM moderation_backfill_cleanup();
 SELECT * INTO b FROM moderation_capacity WHERE singleton;
 UPDATE moderation_actor_allocations SET allocations=0 WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations
  WHERE window_start<=now()-interval '24 hours' AND allocations>0 FOR UPDATE SKIP LOCKED);
 DELETE FROM moderation_actor_allocations WHERE actor_id IN (SELECT actor_id FROM moderation_actor_allocations
  WHERE window_start<=now()-interval '24 hours' AND markers=0 AND NOT rescan FOR UPDATE SKIP LOCKED);
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN 0; END IF;
 -- Safety recount for counts no release corrected: one idle row per pass,
 -- rotating through such rows.
 FOR r IN SELECT a.actor_id FROM moderation_actor_allocations a JOIN users u ON u.id=a.actor_id WHERE a.window_start<=now()-interval '24 hours' AND a.markers>0 AND NOT a.rescan
  ORDER BY a.window_start,a.actor_id LIMIT 1 FOR UPDATE OF a SKIP LOCKED FOR KEY SHARE OF u SKIP LOCKED LOOP
  UPDATE moderation_actor_allocations SET markers=moderation_actor_marker_count(r.actor_id,false),parked=moderation_actor_marker_count(r.actor_id,true),window_start=now() WHERE actor_id=r.actor_id;
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
 FOR m IN SELECT kind,target_id FROM moderation_backfill WHERE parked AND NOT gone ORDER BY last_missed,kind,target_id LIMIT 8 FOR UPDATE SKIP LOCKED LOOP
  state:=moderation_source_state(m.kind,m.target_id);
  IF state='gone' THEN DELETE FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id;
  ELSIF state='held' THEN UPDATE moderation_backfill SET last_missed=now() WHERE kind=m.kind AND target_id=m.target_id;
  ELSE
   UPDATE moderation_backfill SET parked=false WHERE kind=m.kind AND target_id=m.target_id;
   UPDATE moderation_actor_allocations SET parked=GREATEST(0,parked-1) WHERE actor_id=moderation_live_actor(m.kind,m.target_id);
  END IF;
 END LOOP;
 -- Active markers in first-refusal order within a window of 4096, skipping
 -- refused actors, and noting which actors still have active markers.
 FOR m IN SELECT kind,target_id FROM moderation_backfill WHERE NOT parked AND NOT gone ORDER BY first_missed,kind,target_id LIMIT 4096 LOOP
  scanned:=scanned+1;
  act:=moderation_live_actor(m.kind,m.target_id);
  IF act IS NOT NULL THEN seen:=seen||act; END IF;
  IF taken>=budget OR COALESCE(act,'')=ANY(refused) THEN CONTINUE; END IF;
  PERFORM 1 FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id AND NOT parked AND NOT gone FOR UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  taken:=taken+1;
  SELECT * INTO c FROM moderation_capture_source(m.kind,m.target_id);
  IF NOT FOUND THEN
   -- Not capturable now, possibly removed during this pass: park or drop it.
   state:=moderation_source_state(m.kind,m.target_id);
   IF state='held' THEN
    UPDATE moderation_backfill SET parked=true,last_missed=now() WHERE kind=m.kind AND target_id=m.target_id;
    UPDATE moderation_actor_allocations SET parked=parked+1 WHERE actor_id=moderation_live_actor(m.kind,m.target_id);
   ELSIF state='gone' THEN DELETE FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id;
   END IF;
   CONTINUE;
  END IF;
  -- Admission deletes the marker once a job covers this revision, or keeps it
  -- in place when capacity still refuses it.
  IF moderation_backfill_capture(m.kind,m.target_id,c.digest,c.actor,c.meta)='queued' THEN admitted:=admitted+1; END IF;
 END LOOP;
 -- With every active marker seen, correct actors whose stored active count
 -- claims markers that are gone.
 IF scanned<4096 THEN
  FOR r IN SELECT a.actor_id FROM moderation_actor_allocations a JOIN users u ON u.id=a.actor_id WHERE a.markers>a.parked AND NOT a.rescan AND a.actor_id<>ALL(seen)
   ORDER BY a.actor_id LIMIT 4 FOR UPDATE OF a SKIP LOCKED FOR KEY SHARE OF u SKIP LOCKED LOOP
   UPDATE moderation_actor_allocations SET markers=moderation_actor_marker_count(r.actor_id,false),parked=moderation_actor_marker_count(r.actor_id,true) WHERE actor_id=r.actor_id;
  END LOOP;
 END IF;
 -- Actor rescans: up to four actors with capacity, 64 records each, so the
 -- capacity lock is held briefly; progress resumes at the stored position.
 FOR r IN SELECT a.actor_id,a.rescan_ts,a.rescan_kind,a.rescan_id,a.rescan_more FROM moderation_actor_allocations a JOIN users u ON u.id=a.actor_id
  WHERE a.rescan AND a.actor_id<>ALL(refused)
  ORDER BY a.window_start,a.actor_id LIMIT 4 FOR UPDATE OF a SKIP LOCKED FOR KEY SHARE OF u SKIP LOCKED LOOP
  SELECT * INTO st FROM moderation_rescan_actor(r.actor_id,r.rescan_ts,r.rescan_kind,r.rescan_id,share,64);
  IF NOT st.done THEN
   UPDATE moderation_actor_allocations SET rescan_ts=st.ts,rescan_kind=st.k,rescan_id=st.tid WHERE actor_id=r.actor_id;
  ELSIF r.rescan_more THEN
   UPDATE moderation_actor_allocations SET rescan_ts=NULL,rescan_kind=NULL,rescan_id=NULL,rescan_more=false WHERE actor_id=r.actor_id;
  ELSE
   UPDATE moderation_actor_allocations SET rescan=false,rescan_ts=NULL,rescan_kind=NULL,rescan_id=NULL,
    markers=moderation_actor_marker_count(r.actor_id,false),parked=moderation_actor_marker_count(r.actor_id,true) WHERE actor_id=r.actor_id;
  END IF;
 END LOOP;
 PERFORM set_config('opsecmkt.moderation_sweep','off',true);
 RETURN admitted;
END $$;
