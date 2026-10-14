-- Exact technical non-reuse state has no account, order, destination or address
-- mapping. Preserve allocation safety after the associated personal rows expire.
CREATE TABLE retention_escrow_retired_addresses (
 coin text NOT NULL CHECK(coin IN ('BTC','XMR')),
 address_hash text NOT NULL CHECK(address_hash ~ '^[0-9a-f]{64}$'),
 label_checked boolean NOT NULL DEFAULT false,
 PRIMARY KEY(coin,address_hash)
);
CREATE TABLE retention_escrow_retired_keys (
 pubkey_hash text PRIMARY KEY CHECK(pubkey_hash ~ '^[0-9a-f]{64}$')
);
CREATE TABLE retention_escrow_key_floors (
 key_hmac text PRIMARY KEY CHECK(key_hmac ~ '^[0-9a-f]{64}$'),
 next_index bigint NOT NULL CHECK(next_index>=1)
);
CREATE TABLE retention_xmr_wallet_names (
 month date PRIMARY KEY CHECK(month=date_trunc('month',month)),
 next_n integer NOT NULL CHECK(next_n>=1)
);
CREATE TABLE retention_xmr_retired_wallets (
 name_hash text PRIMARY KEY CHECK(name_hash ~ '^[0-9a-f]{64}$')
);
INSERT INTO retention_xmr_wallet_names(month,next_n)
 SELECT month,max(substr(name,15,2)::int)+1 FROM xmr_escrow_wallets GROUP BY month;

-- Restore reconciliation can discover a higher numbered wallet on the RPC
-- volume. Advancing the allocation counter is atomic with that orphan record.
CREATE FUNCTION retention_xmr_wallet_name_seen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO retention_xmr_wallet_names(month,next_n) VALUES(NEW.month,substr(NEW.name,15,2)::int+1)
 ON CONFLICT(month) DO UPDATE SET next_n=EXCLUDED.next_n WHERE retention_xmr_wallet_names.next_n<EXCLUDED.next_n;
 RETURN NEW;
END $$;
CREATE TRIGGER retention_xmr_wallet_name_seen AFTER INSERT ON xmr_escrow_wallets FOR EACH ROW EXECUTE FUNCTION retention_xmr_wallet_name_seen();

-- A permit is useful only within the transaction that applied a previously
-- journaled retirement certificate. It cannot authorize another order or a
-- later transaction. The runtime checks holds and financial safety before
-- journaling; replay uses that committed decision rather than stale backup state.
CREATE TABLE retention_escrow_permits (
 order_id text PRIMARY KEY,
 transaction_id bigint NOT NULL,
 wallet_id bigint,
 closed_at timestamptz NOT NULL,
 certified_at timestamptz NOT NULL,
 CHECK(certified_at>=closed_at+interval '4320 hours')
);
CREATE FUNCTION retention_escrow_delete_allowed(order_key text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM retention_escrow_permits WHERE order_id=order_key AND transaction_id=txid_current())
$$;
CREATE FUNCTION retention_wallet_delete_allowed(wallet_key bigint) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM xmr_escrow_wallets w WHERE w.id=wallet_key AND w.state='destroyed' AND w.seed_sealed IS NULL)
 AND EXISTS(SELECT 1 FROM retention_escrow_permits WHERE wallet_id=wallet_key AND transaction_id=txid_current())
 AND NOT EXISTS(SELECT 1 FROM xmr_escrow_slots s WHERE s.wallet_id=wallet_key AND s.order_id IS NOT NULL)
$$;

CREATE OR REPLACE FUNCTION escrow_records_kept() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE order_key text;
BEGIN
 CASE TG_TABLE_NAME
  WHEN 'escrows' THEN order_key:=OLD.order_id;
  WHEN 'xmr_settlements' THEN order_key:=OLD.order_id;
  WHEN 'xmr_settlement_rerequests' THEN SELECT order_id INTO order_key FROM xmr_settlements WHERE id=OLD.settlement_id;
  WHEN 'xmr_escrow_slots' THEN
   order_key:=OLD.order_id;
   IF order_key IS NULL AND retention_wallet_delete_allowed(OLD.wallet_id) THEN RETURN OLD; END IF;
  WHEN 'xmr_escrow_wallets' THEN
   IF retention_wallet_delete_allowed(OLD.id) THEN RETURN OLD; END IF;
 END CASE;
 IF order_key IS NOT NULL AND retention_escrow_delete_allowed(order_key) THEN RETURN OLD; END IF;
 RAISE EXCEPTION '%: escrow records are never deleted outside a certified exact-order retirement',TG_TABLE_NAME USING ERRCODE='check_violation';
END $$;

-- Keep every existing insert/update/signature guard unchanged. Add only an
-- exact-order DELETE exception at the start of these known trigger functions.
-- Fail migration if a future implementation no longer has the expected body.
DO $$
DECLARE name text; table_name text; key_column text; original text; rewritten text;
BEGIN
 FOR name,table_name,key_column IN VALUES
  ('settlement_outputs_frozen','settlements','settlement_id'),
  ('settlement_signatures_kept','settlements','settlement_id'),
  ('settlement_bumps_frozen','settlements','settlement_id'),
  ('settlement_bump_signatures_kept','settlement_bumps','bump_id'),
  ('xmr_settlement_outputs_frozen','xmr_settlements','settlement_id')
 LOOP
  original:=pg_get_functiondef((name||'()')::regprocedure);
  IF strpos(original,E'BEGIN\n')=0 THEN RAISE EXCEPTION 'retirement migration: unexpected body for %',name; END IF;
  IF name='settlement_bump_signatures_kept' THEN
   rewritten:=replace(original,E'BEGIN\n',E'BEGIN\n IF TG_OP=''DELETE'' THEN\n  IF retention_escrow_delete_allowed((SELECT retirement_parent.order_id FROM settlement_bumps retirement_bump JOIN settlements retirement_parent ON retirement_parent.id=retirement_bump.settlement_id WHERE retirement_bump.id=OLD.bump_id)) THEN RETURN OLD; END IF;\n END IF;\n');
  ELSE
   rewritten:=replace(original,E'BEGIN\n',format(E'BEGIN\n IF TG_OP=''DELETE'' THEN\n  IF retention_escrow_delete_allowed((SELECT order_id FROM %I WHERE id=OLD.%I)) THEN RETURN OLD; END IF;\n END IF;\n',table_name,key_column));
  END IF;
  EXECUTE rewritten;
 END LOOP;
END $$;
