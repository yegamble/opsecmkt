-- SEC72AA-1 (A-647): moderation admission never waits on the global capacity row.
--
-- Lock rule: foreground transactions never wait on moderation_capacity; worker and staff transactions take it first.
--
-- A marketplace write (a listing edit, an upload, a dispute, a review, a payment flag) runs admission while it holds
-- its own source rows. Admission used to wait on moderation_capacity, so a transaction that admitted one change and
-- then locked another row deadlocked with a write of that row waiting on the capacity row. Admission now takes the
-- capacity row with SKIP LOCKED. When another transaction holds it, the write leaves only a reference in
-- moderation_deferred (kind, target id, when noted: no account, digest or content) and commits; no overflow note,
-- marker, allocation row or counter is touched. The next worker pass, under the capacity lock, admits each deferred
-- record's current revision through normal admission (per-account order, caps, markers and rescans apply), and drops
-- a record that is gone or no longer capturable (an unflagged payment is captured again when it is flagged again).
-- Deleting a record removes its reference at once, without the capacity lock. The worker's result and cached
-- completion transactions, the private-panel and offline reviews and the capture switch take the capacity row before
-- any job, proposal or marketplace row (the private-panel review locks only its own administrator's account row
-- first), so they wait for it holding nothing a marketplace transaction needs.
CREATE TABLE moderation_deferred (
 kind text NOT NULL CHECK (kind IN ('listing','upload','dispute','review','payment_flag')),
 target_id text NOT NULL CHECK (length(target_id) BETWEEN 1 AND 128),
 noted timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (kind,target_id)
);
CREATE INDEX moderation_deferred_due ON moderation_deferred(noted,kind,target_id);

CREATE OR REPLACE FUNCTION moderation_admit_job() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b moderation_capacity; n integer; a moderation_actor_allocations; wasp boolean;
BEGIN
 IF NEW.capture_version<>2 THEN RETURN NEW; END IF;
 -- A source that vanished has no actor; there is nothing to capture.
 IF NEW.actor_id IS NULL OR NEW.actor_id='' THEN RETURN NULL; END IF;
 -- Never wait on the capacity row: while another transaction holds it, leave a
 -- reference for the next worker pass. A caller that already holds it (the
 -- worker pass) gets its own row here.
 SELECT * INTO b FROM moderation_capacity WHERE singleton FOR UPDATE SKIP LOCKED;
 IF NOT FOUND THEN
  IF length(NEW.target_id) BETWEEN 1 AND 128 THEN
   INSERT INTO moderation_deferred(kind,target_id) VALUES(NEW.kind,NEW.target_id) ON CONFLICT DO NOTHING;
  END IF;
  RETURN NULL;
 END IF;
 -- A pending or evaluated job already covers this revision.
 IF EXISTS(SELECT 1 FROM moderation_jobs WHERE kind=NEW.kind AND target_id=NEW.target_id AND content_hash=NEW.content_hash
   AND (completed_at IS NULL OR failure_class<>'stale_snapshot')) THEN
  DELETE FROM moderation_backfill WHERE (kind,target_id) IN (SELECT kind,target_id FROM moderation_backfill
   WHERE kind=NEW.kind AND target_id=NEW.target_id FOR UPDATE SKIP LOCKED) RETURNING parked INTO wasp;
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
  WHERE kind=NEW.kind AND target_id=NEW.target_id FOR UPDATE SKIP LOCKED) RETURNING parked INTO wasp;
 IF FOUND THEN UPDATE moderation_actor_allocations SET markers=GREATEST(0,markers-1),parked=GREATEST(0,parked-CASE WHEN wasp THEN 1 ELSE 0 END) WHERE actor_id=NEW.actor_id; END IF;
 NEW.snapshot:='{}'::jsonb; NEW.reserved_results:=1; NEW.results_count:=0;
 NEW.priority_at:=NEW.queued_at-CASE WHEN NEW.kind IN ('dispute','payment_flag') THEN interval '5 minutes' ELSE interval '0' END;
 RETURN NEW;
END $$;

-- Under the capacity lock, admit up to lim deferred records, oldest first, each
-- at its current revision, through normal admission (outside the sweep's
-- setting, so a refusal leaves a marker or a rescan flag as a write would). A
-- record that is gone or not capturable now (an unflagged payment) is dropped. A
-- reference another transaction holds (its record is being deleted), or whose
-- account row is being deleted, is skipped, never waited for while the capacity
-- lock is held. Nothing is admitted while capture is disabled. Returns the
-- number of jobs queued.
CREATE FUNCTION moderation_deferred_drain(lim integer) RETURNS integer LANGUAGE plpgsql AS $$
DECLARE m record; c record; live boolean; n integer:=0; prev text;
BEGIN
 PERFORM 1 FROM moderation_capacity WHERE singleton FOR UPDATE;
 IF NOT EXISTS(SELECT 1 FROM settings WHERE key='moderation_enabled' AND value='true') THEN RETURN 0; END IF;
 prev:=current_setting('opsecmkt.moderation_sweep',true);
 PERFORM set_config('opsecmkt.moderation_sweep','off',true);
 FOR m IN SELECT kind,target_id FROM moderation_deferred ORDER BY noted,kind,target_id LIMIT GREATEST(lim,0) FOR UPDATE SKIP LOCKED LOOP
  SELECT * INTO c FROM moderation_capture_source(m.kind,m.target_id);
  live:=FOUND;
  IF live THEN
   PERFORM 1 FROM users WHERE id=c.actor FOR KEY SHARE SKIP LOCKED;
   CONTINUE WHEN NOT FOUND;
  END IF;
  DELETE FROM moderation_deferred WHERE kind=m.kind AND target_id=m.target_id;
  IF live AND moderation_backfill_capture(m.kind,m.target_id,c.digest,c.actor,c.meta)='queued' THEN n:=n+1; END IF;
 END LOOP;
 PERFORM set_config('opsecmkt.moderation_sweep',COALESCE(prev,''),true);
 RETURN n;
END $$;

-- 146's source-deletion trigger, also removing the record's deferred reference.
-- Like the marker, it takes no capacity lock and writes no account record.
CREATE OR REPLACE FUNCTION moderation_source_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text; tid text; aid text; wasp boolean;
BEGIN
 IF TG_TABLE_NAME='products' THEN k:='listing'; tid:=OLD.id; aid:=OLD.vendor_id;
 ELSIF TG_TABLE_NAME='images' THEN k:='upload'; tid:=OLD.id; aid:=OLD.owner_user_id;
 ELSIF TG_TABLE_NAME='reviews' THEN k:='review'; tid:=OLD.id; aid:=OLD.buyer_id;
 ELSIF TG_TABLE_NAME='disputes' THEN k:='dispute'; tid:=OLD.id; SELECT buyer_id INTO aid FROM orders WHERE id=OLD.order_id;
 ELSE k:='payment_flag'; tid:=OLD.id::text; SELECT buyer_id INTO aid FROM orders WHERE id=OLD.order_id;
 END IF;
 DELETE FROM moderation_deferred WHERE kind=k AND target_id=tid;
 PERFORM set_config('opsecmkt.moderation_source_delete','on',true);
 DELETE FROM moderation_backfill WHERE kind=k AND target_id=tid RETURNING parked INTO wasp;
 PERFORM set_config('opsecmkt.moderation_source_delete','off',true);
 -- parked is NOT NULL, so wasp is set exactly when a marker was deleted.
 IF wasp IS NOT NULL THEN
  INSERT INTO moderation_marker_releases(actor_id,parked) VALUES(aid,wasp);
 END IF;
 RETURN NULL;
END $$;

-- 146's sweep, draining deferred references before its pending early return.
CREATE OR REPLACE FUNCTION moderation_backfill_sweep(lim integer) RETURNS integer LANGUAGE plpgsql AS $$
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
 -- Records whose capture found the capacity row busy, admitted (or marked) at
 -- their current revision even when the queue is past half full.
 admitted:=moderation_deferred_drain(lim);
 SELECT * INTO b FROM moderation_capacity WHERE singleton;
 IF b.pending*2>=b.max_pending OR b.allocated_records>=b.max_records OR b.allocated_bytes+524288>b.max_bytes THEN RETURN admitted; END IF;
 budget:=LEAST(GREATEST(lim,0),GREATEST(1,(b.max_pending+1)/2-b.pending));
 share:=GREATEST(8,b.max_records*2/100);
 PERFORM set_config('opsecmkt.moderation_sweep','on',true);
 -- Actors that admission would refuse now; their markers do not use the budget.
 refused:=ARRAY(SELECT actor_id FROM moderation_jobs WHERE completed_at IS NULL AND actor_id<>'' GROUP BY actor_id HAVING count(*)>=3
  UNION SELECT actor_id FROM moderation_actor_allocations WHERE window_start>now()-interval '24 hours' AND allocations>=share);
 -- Parked markers: a few per pass, oldest check first. A returned source
 -- becomes active again; a gone one loses its marker.
 FOR m IN SELECT kind,target_id FROM moderation_backfill WHERE parked ORDER BY last_missed,kind,target_id LIMIT 8 FOR UPDATE SKIP LOCKED LOOP
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
 FOR m IN SELECT kind,target_id FROM moderation_backfill WHERE NOT parked ORDER BY first_missed,kind,target_id LIMIT 4096 LOOP
  scanned:=scanned+1;
  act:=moderation_live_actor(m.kind,m.target_id);
  IF act IS NOT NULL THEN seen:=seen||act; END IF;
  IF taken>=budget OR COALESCE(act,'')=ANY(refused) THEN CONTINUE; END IF;
  PERFORM 1 FROM moderation_backfill WHERE kind=m.kind AND target_id=m.target_id AND NOT parked FOR UPDATE SKIP LOCKED;
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
