-- Exact-wallet replay authority for a durable committed90day seed-erasure
-- certificate. It never grants payment/order deletion or generic guard bypass.
CREATE TABLE retention_xmr_destroy_permits (
 wallet_name text PRIMARY KEY,
 transaction_id bigint NOT NULL,
 last_closed_at timestamptz NOT NULL,
 destroy_after timestamptz NOT NULL,
 destroyed_at timestamptz NOT NULL,
 started_at timestamptz,
 CHECK(destroy_after>=last_closed_at+interval '2160 hours'),
 CHECK(destroyed_at>=destroy_after)
);
CREATE FUNCTION retention_xmr_destroy_replay_allowed(before_row xmr_escrow_wallets,after_row xmr_escrow_wallets) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT after_row.state='destroyed' AND after_row.seed_sealed IS NULL
 AND after_row.id=before_row.id AND after_row.name=before_row.name AND after_row.month=before_row.month
 AND after_row.primary_address IS NOT DISTINCT FROM before_row.primary_address
 AND after_row.restore_height IS NOT DISTINCT FROM before_row.restore_height
 AND after_row.seed_key_fpr IS NOT DISTINCT FROM before_row.seed_key_fpr
 AND after_row.sealed_at IS NOT DISTINCT FROM before_row.sealed_at
 AND after_row.created=before_row.created
 AND EXISTS(SELECT 1 FROM retention_xmr_destroy_permits p WHERE p.wallet_name=before_row.name AND p.transaction_id=txid_current()
 AND p.last_closed_at=after_row.last_closed_at AND p.destroy_after=after_row.destroy_after AND p.destroyed_at=after_row.destroyed_at AND p.started_at IS NOT DISTINCT FROM after_row.destroy_started_at)
$$;
DO $$
DECLARE name text; original text;
BEGIN
 FOREACH name IN ARRAY ARRAY['xmr_escrow_wallets_kept','xmr_escrow_wallets_destroy_started','retention_xmr_wallet_hold_guard'] LOOP
  original:=pg_get_functiondef((name||'()')::regprocedure);
  IF strpos(original,E'BEGIN\n')=0 THEN RAISE EXCEPTION 'unexpected destruction trigger body for %',name; END IF;
  EXECUTE replace(original,E'BEGIN\n',E'BEGIN\n IF TG_OP=''UPDATE'' THEN\n  IF retention_xmr_destroy_replay_allowed(OLD,NEW) THEN RETURN NEW; END IF;\n END IF;\n');
 END LOOP;
END $$;

CREATE FUNCTION retention_destroyed_wallet_nonreuse() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.state='destroyed' THEN
  INSERT INTO retention_xmr_retired_wallets(name_hash) VALUES(encode(sha256(convert_to(NEW.name,'UTF8')),'hex')) ON CONFLICT DO NOTHING;
  INSERT INTO retention_xmr_wallet_names(month,next_n) VALUES(NEW.month,substr(NEW.name,15,2)::int+1) ON CONFLICT(month) DO UPDATE SET next_n=GREATEST(retention_xmr_wallet_names.next_n,EXCLUDED.next_n);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_destroyed_wallet_nonreuse AFTER INSERT OR UPDATE ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_destroyed_wallet_nonreuse();
INSERT INTO retention_xmr_retired_wallets(name_hash) SELECT encode(sha256(convert_to(name,'UTF8')),'hex') FROM xmr_escrow_wallets WHERE state='destroyed' ON CONFLICT DO NOTHING;
CREATE FUNCTION retention_xmr_retired_name_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM retention_xmr_retired_wallets WHERE name_hash=encode(sha256(convert_to(NEW.name,'UTF8')),'hex')) AND NOT(NEW.state='destroyed' AND NEW.seed_sealed IS NULL) THEN
  RAISE EXCEPTION 'xmr_escrow_wallets: destroyed wallet names are never reused or resealed' USING ERRCODE='check_violation';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_xmr_retired_name_guard BEFORE INSERT ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_xmr_retired_name_guard();
