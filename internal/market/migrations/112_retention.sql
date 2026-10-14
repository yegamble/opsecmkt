-- Explicit enrollment: this migration records no invented authentication time
-- and deletes nothing. Runtime retention remains opt-in until its external state
-- directory and ordinary backup expiry have been configured.
ALTER TABLE users ADD COLUMN last_authenticated_at timestamptz;
ALTER TABLE users ADD COLUMN inactivity_notified_at timestamptz;
ALTER TABLE users ADD COLUMN deletion_requested_at timestamptz;
ALTER TABLE users ADD COLUMN closed_at timestamptz;
ALTER TABLE users ADD COLUMN retention_checked_at timestamptz;
ALTER TABLE users ADD COLUMN retention_restricted_at timestamptz;
ALTER TABLE users ADD COLUMN profile_scrubbed_at timestamptz;
ALTER TABLE products ADD COLUMN private_expired_at timestamptz;
ALTER TABLE orders ADD COLUMN financial_closed_at timestamptz;
ALTER TABLE orders ADD COLUMN private_expired_at timestamptz;
ALTER TABLE orders ADD COLUMN financial_retirement_checked_at timestamptz;
ALTER TABLE disputes ADD COLUMN resolution_updated_at timestamptz;
CREATE FUNCTION retention_dispute_resolution_clock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.resolution IS DISTINCT FROM OLD.resolution
    AND COALESCE(current_setting('opsecmkt.retention_purge',true),'')<>'on' THEN
   NEW.resolution_updated_at:=now();
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_dispute_resolution_clock BEFORE UPDATE OF resolution ON disputes
 FOR EACH ROW EXECUTE FUNCTION retention_dispute_resolution_clock();
ALTER TABLE provider_episode_deliveries ADD COLUMN created timestamptz NOT NULL DEFAULT now();
CREATE INDEX users_retention_activity ON users(last_authenticated_at) WHERE closed_at IS NULL;
CREATE INDEX messages_retention_created ON messages(created,id);
CREATE INDEX notifications_retention_created ON notifications(created,id);
CREATE INDEX provider_episode_deliveries_retention_created ON provider_episode_deliveries(created);
CREATE INDEX orders_retention_closed ON orders(financial_closed_at) WHERE financial_closed_at IS NOT NULL;

-- A mirror of the external authoritative register. Review deadlines are alerts,
-- never implicit release dates. Record IDs deliberately have no FK: deleting an
-- object must not silently remove its preservation or restore history.
CREATE TABLE retention_holds (
 id text PRIMARY KEY,
 scope text NOT NULL CHECK(scope IN ('account','message','order','audit')),
 record_id text NOT NULL,
 authority text NOT NULL, reason text NOT NULL, owner text NOT NULL,
 readers text NOT NULL, release_condition text NOT NULL,
 began_at timestamptz NOT NULL, review_at timestamptz NOT NULL,
 released_at timestamptz,
 CHECK(review_at >= began_at),
 CHECK(length(authority) BETWEEN 1 AND 1000),
 CHECK(length(reason) BETWEEN 1 AND 2000),
 CHECK(length(owner) BETWEEN 1 AND 200),
 CHECK(length(readers) BETWEEN 1 AND 1000),
 CHECK(length(release_condition) BETWEEN 1 AND 2000)
);
CREATE INDEX retention_holds_active ON retention_holds(scope,record_id) WHERE released_at IS NULL;
CREATE TABLE retention_operations (
 id text PRIMARY KEY, created timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE retention_account_exceptions (
 user_id text PRIMARY KEY,
 reason text NOT NULL CHECK(reason IN('open_escrow','administrator_continuity')),
 owner text NOT NULL DEFAULT 'instance operator',
 first_seen timestamptz NOT NULL, last_checked timestamptz NOT NULL,
 review_due timestamptz NOT NULL, resolved_at timestamptz
);
CREATE TABLE retention_runs (
 id bigserial PRIMARY KEY, started timestamptz NOT NULL,
 completed timestamptz NOT NULL DEFAULT now(), counts jsonb NOT NULL
);

-- Obsolete saved pooled-wallet defaults are not read by the application. This
-- maintenance-only erasure removes legacy identifying values without restoring
-- any pooled payment feature or writing financial records.
CREATE FUNCTION retention_erase_legacy_profile(account_key text) RETURNS void LANGUAGE sql AS $$
 UPDATE users SET payout_btc='',payout_xmr='',payout_btc_changed=NULL,payout_xmr_changed=NULL
 WHERE id=account_key
$$;

-- The maintenance transaction enables deletion only after journaling the purge
-- intent outside PostgreSQL. Ordinary application writes remain append-only.
CREATE FUNCTION audit_retention_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' AND current_setting('opsecmkt.retention_purge',true)='on' THEN
   RETURN OLD;
 END IF;
 RAISE EXCEPTION 'audit events are append-only outside retention maintenance'
   USING ERRCODE='check_violation';
END $$;
CREATE TRIGGER audit_retention_guard BEFORE UPDATE OR DELETE ON audit_events
 FOR EACH ROW EXECUTE FUNCTION audit_retention_guard();
