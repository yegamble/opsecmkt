#!/usr/bin/env bash
# Integration test. Requires a disposable PostgreSQL server and a CREATEDB role.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
: "${TEST_DATABASE_URL:?Set an explicit PostgreSQL test URL; its role must have CREATEDB}"
for dependency in python3 psql pg_dump pg_restore age age-keygen go; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-ops.XXXXXX")
export OPS_TEST_ROOT="$PWD"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(8))')
source_db="opsecmkt_ops_${suffix}_source"
target_db="opsecmkt_ops_${suffix}_target"
# The real application schema: migrated by the server itself, then backed up and restored.
app_source_db="opsecmkt_ops_${suffix}_app_source"
app_target_db="opsecmkt_ops_${suffix}_app_target"
server_pid=''

# Reuse the production URL parser, substituting psql only after validation.
# The database URL and password never appear in process arguments.
pg() {
  local database=$1
  shift
  python3 - "$database" "$@" <<'PY'
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
path = Path(os.environ['OPS_TEST_ROOT']) / 'scripts' / 'postgres-tool.py'
spec = importlib.util.spec_from_file_location('postgres_tool', path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
database, arguments = sys.argv[1], sys.argv[2:]
def execute(_executable, _argv, env):
    if database:
        if not database.startswith('opsecmkt_ops_') or not all(c.isalnum() or c == '_' for c in database):
            raise SystemExit('Refusing a non-test database override')
        env['PGDATABASE'] = database
    raise SystemExit(subprocess.call(['psql', '-X', '-v', 'ON_ERROR_STOP=1', *arguments], env=env))
module.os.execvpe = execute
sys.argv = [str(path), 'TEST_DATABASE_URL', 'pg_dump']
try:
    module.main()
except (KeyError, ValueError):
    raise SystemExit('Invalid TEST_DATABASE_URL configuration')
PY
}
stop_app() {
  if [[ -n $server_pid ]]; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    server_pid=''
  fi
}
cleanup() {
  local status=$?
  trap - EXIT
  stop_app
  if ((status)) && [[ -f $work/app.log ]]; then echo '--- application log ---' >&2; cat "$work/app.log" >&2; fi
  for database in "$source_db" "$target_db" "$app_source_db" "$app_target_db"; do
    pg '' -q -c "DROP DATABASE IF EXISTS $database" >/dev/null 2>&1 || true
  done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
pg '' -q -c "CREATE DATABASE $source_db"
pg '' -q -c "CREATE DATABASE $target_db"
pg '' -q -c "CREATE DATABASE $app_source_db"
pg '' -q -c "CREATE DATABASE $app_target_db"
pg "$source_db" -q -c "CREATE TABLE a_first (id integer PRIMARY KEY, note text NOT NULL); INSERT INTO a_first VALUES (1, 'Crème brûlée — 東京 🔒'); CREATE TABLE z_conflict (id integer PRIMARY KEY); INSERT INTO z_conflict VALUES (42);"
# The payouts shape restore.sh relies on (state, error, updated) in a dump older than send_ambiguous; one row
# per state.
pg "$source_db" -q -c "CREATE TABLE payouts (id integer PRIMARY KEY, state text NOT NULL, error text NOT NULL DEFAULT '', updated timestamptz NOT NULL DEFAULT now()); INSERT INTO payouts(id,state) VALUES (1,'pending'),(2,'sending'),(3,'sent'),(5,'blocked'),(6,'held'); INSERT INTO payouts(id,state,error) VALUES (4,'failed','Insufficient funds');"

# Only the generated scratch database names replace the supplied URL path.
connection_url() {
  python3 - "$1" <<'PY'
import os
import sys
from urllib.parse import urlsplit, urlunsplit
url = urlsplit(os.environ['TEST_DATABASE_URL'])
print(urlunsplit((url.scheme, url.netloc, '/' + sys.argv[1], url.query, url.fragment)))
PY
}
export BACKUP_DATABASE_URL
BACKUP_DATABASE_URL=$(connection_url "$source_db")
export RESTORE_DATABASE_URL
RESTORE_DATABASE_URL=$(connection_url "$target_db")
age-keygen -o "$work/identity" 2> "$work/keygen.log"
age-keygen -o "$work/wrong-identity" 2>> "$work/keygen.log"
export AGE_RECIPIENT AGE_IDENTITY
AGE_RECIPIENT=$(age-keygen -y "$work/identity")
AGE_IDENTITY="$work/identity"
backup="$work/backup.dump.age"
scripts/backup.sh "$backup" > "$work/backup.log"
# The success line names the database dumped, never the URL or its password.
grep -q "of database $source_db (BACKUP_DATABASE_URL) saved to" "$work/backup.log"
if grep -q -e '://' "$work/backup.log"; then echo 'backup.sh printed the connection URL' >&2; exit 1; fi
[[ -s $backup ]] || { echo 'Backup is empty' >&2; exit 1; }
python3 - "$backup" <<'PY_CHECK'
from pathlib import Path
import stat
import sys
backup = Path(sys.argv[1])
assert stat.S_IMODE(backup.stat().st_mode) == 0o600, 'Backup permissions must be 0600'
with backup.open('rb') as stream:
    assert stream.read(22) == b'age-encryption.org/v1\n', 'Backup must be age-encrypted'
PY_CHECK

# Existing backups must be preserved byte for byte.
cp "$backup" "$work/original.dump.age"
if scripts/backup.sh "$backup" > "$work/overwrite.log" 2>&1; then
  echo 'Backup unexpectedly overwrote an existing file' >&2; exit 1
fi
cmp "$backup" "$work/original.dump.age"

# Decryption with an unrelated identity must fail without creating schema.
if AGE_IDENTITY="$work/wrong-identity" scripts/restore.sh "$backup" <<< 'RESTORE' > "$work/wrong-key.log" 2>&1; then
  echo 'Restore unexpectedly accepted the wrong identity' >&2; exit 1
fi
[[ $(pg "$target_db" -Atq -c "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'") == 0 ]]

scripts/restore.sh "$backup" <<< 'RESTORE' > "$work/restore.log"
[[ $(pg "$target_db" -Atq -c 'SELECT note FROM a_first WHERE id = 1') == 'Crème brûlée — 東京 🔒' ]]
[[ $(pg "$target_db" -Atq -c 'SELECT id FROM z_conflict') == 42 ]]
# Blocked and automatically held payouts must also require reconciliation after restoration. A failed payout
# may have been requeued and sent after the backup: it keeps its state and error behind the restore marker
# (this dump has no send_ambiguous column; migration 053 marks such failures ambiguous).
[[ $(pg "$target_db" -Atq -c "SELECT string_agg(id || ':' || state || ':' || (error LIKE 'Restored from backup:%'), ',' ORDER BY id) FROM payouts") == '1:held:true,2:held:true,3:sent:false,4:failed:true,5:held:true,6:held:true' ]]
[[ $(pg "$target_db" -Atq -c 'SELECT error FROM payouts WHERE id = 4') == 'Restored from backup: verify in the wallet before requeueing; this payout may have been requeued and sent after the backup. Last error: Insufficient funds' ]]
[[ $(pg "$source_db" -Atq -c "SELECT count(*) FROM payouts WHERE state = 'held'") == 1 ]]
grep -q 'Held 4 restored payout(s)' "$work/restore.log"
grep -q 'Marked 1 restored failed payout(s) as possibly sent' "$work/restore.log"
grep -q 'do not contain the custodial wallets' "$work/restore.log"
grep -q 'point BACKUP_DATABASE_URL at the same database' "$work/restore.log"

# Restore creates a_first before colliding with z_conflict. A failure must roll
# the entire transaction back, including that earlier successful CREATE TABLE.
pg "$target_db" -q -c 'DROP TABLE a_first; UPDATE z_conflict SET id = 99;'
if scripts/restore.sh "$backup" <<< 'RESTORE' > "$work/conflict.log" 2>&1; then
  echo 'Restore unexpectedly accepted an existing conflicting table' >&2; exit 1
fi
[[ $(pg "$target_db" -Atq -c "SELECT to_regclass('public.a_first') IS NULL") == t ]]
[[ $(pg "$target_db" -Atq -c 'SELECT id FROM z_conflict') == 99 ]]

# An old application backup with settings but no payments schema still needs the persistent gate:
# subsequent migrations must not turn payments on without reconciliation.
pg "$source_db" -q -c "DROP TABLE a_first, z_conflict, payouts; CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL); INSERT INTO settings VALUES ('payments_recovery_required','false');"
pg "$target_db" -q -c 'DROP TABLE z_conflict, payouts;'
legacy_backup="$work/legacy.dump.age"
scripts/backup.sh "$legacy_backup"
scripts/restore.sh "$legacy_backup" <<< 'RESTORE' > "$work/legacy-restore.log"
[[ $(pg "$target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$target_db" -Atq -c "SELECT to_regclass('payouts') IS NULL") == t ]]
grep -q 'Held 0 restored payout(s)' "$work/legacy-restore.log"
grep -q 'Marked 0 restored failed payout(s)' "$work/legacy-restore.log"

# The same round trip on the real application schema. The server migrates an empty database itself; a
# release payout is queued (pending), another already sent and a third failed with a definite wallet rejection;
# the dump is restored into an empty database. The restore must hold the queued payout and mark the failed one
# possibly sent, and the server must then start on the restored database as-is.
# start_app DATABASE: runs the server from this checkout with a clean environment and waits for /healthz.
CGO_ENABLED=0 go build -trimpath -o "$work/server" ./cmd/server
app_port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
start_app() {
  (exec env -i PATH="$PATH" DATABASE_URL="$(connection_url "$1")" SETUP_TOKEN='ops-restore-test-bootstrap-token-0123456789' \
    COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" "$work/server") >> "$work/app.log" 2>&1 &
  server_pid=$!
  python3 - "http://127.0.0.1:$app_port/healthz" "$server_pid" <<'PY'
import os, sys, time, urllib.request
url, pid = sys.argv[1], int(sys.argv[2])
for _ in range(120):
    try:
        os.kill(pid, 0)
    except OSError:
        raise SystemExit('The application exited before becoming healthy')
    try:
        with urllib.request.urlopen(url, timeout=2) as response:
            if response.status == 200 and response.read() == b'ok':
                raise SystemExit(0)
    except OSError:
        pass
    time.sleep(0.5)
raise SystemExit('The application did not become healthy')
PY
}
start_app "$app_source_db"
stop_app
migrations=$(pg "$app_source_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
[[ $(pg "$app_source_db" -Atq -c 'SELECT count(*) FROM schema_migrations') == $(find internal/market/migrations -name '*.sql' | wc -l | tr -d ' ') ]]
pg "$app_source_db" -q -c "
INSERT INTO users(id,handle,password_hash,role) VALUES ('ops-vendor','ops_vendor','not-a-login','vendor'),('ops-buyer','ops_buyer','not-a-login','buyer');
INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES ('ops-product','ops-vendor','Restore drill kit','','Hardware','Worldwide','physical',150000,250000000000,3);
INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES
 ('ops-order-queued','ops-buyer','ops-product','BTC',150000,'completed'),('ops-order-sent','ops-buyer','ops-product','XMR',250000000000,'resolved'),
 ('ops-order-failed','ops-buyer','ops-product','BTC',150000,'completed');
INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES ('ops-order-queued','release','ops-vendor','BTC',150000,'tb1qopsrestorequeuedpayout','pending');
INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid) VALUES ('ops-order-sent','refund','ops-buyer','XMR',250000000000,'ops-restore-sent-address','sent',repeat('ab',32));
INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error,send_ambiguous) VALUES ('ops-order-failed','release','ops-vendor','BTC',150000,'tb1qopsrestorefailedpayout','failed','Insufficient funds',false);"
# Two funded orders with no payout row in the backup, both settled and paid out after it (the release sent to the
# vendor once the buyer completed the delivered order, the refund sent to the buyer once a moderator resolved the
# open dispute); the restored database knows of neither send. The disputed order also holds a locked transfer,
# which never counts toward a payout.
pg "$app_source_db" -q -c "
INSERT INTO users(id,handle,password_hash,role) VALUES ('ops-admin','ops_admin','not-a-login','admin');
UPDATE users SET payout_btc='tb1qopsrestorelatevendor' WHERE id='ops-vendor';
INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES
 ('ops-order-late','ops-buyer','ops-product','BTC',150000,'delivered'),('ops-order-disputed','ops-buyer','ops-product','XMR',250000000000,'disputed');
INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES
 ('ops-order-late','BTC','tb1qopsrestorelatedeposit','bitcoin'),('ops-order-disputed','XMR','ops-restore-disputed-deposit','monero');
INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,locked) VALUES
 ('ops-order-late','BTC',repeat('11',32),0,'tb1qopsrestorelatedeposit',100000,6,true,false),
 ('ops-order-late','BTC',repeat('12',32),1,'tb1qopsrestorelatedeposit',50000,6,true,false),
 ('ops-order-disputed','XMR',repeat('21',32),0,'ops-restore-disputed-deposit',250000000000,12,false,false),
 ('ops-order-disputed','XMR',repeat('22',32),0,'ops-restore-disputed-deposit',1000000,12,false,true);
INSERT INTO disputes(id,order_id,reason) VALUES ('ops-dispute','ops-order-disputed','The parcel never arrived.');
INSERT INTO audit_events(user_id,action) VALUES ('ops-admin','Restore drill: an audit row written before the backup');"
app_backup="$work/app.dump.age"
BACKUP_DATABASE_URL=$(connection_url "$app_source_db") scripts/backup.sh "$app_backup"
RESTORE_DATABASE_URL=$(connection_url "$app_target_db") scripts/restore.sh "$app_backup" <<< 'RESTORE' > "$work/app-restore.log"
grep -q 'Held 1 restored payout(s)' "$work/app-restore.log"
grep -q 'Marked 1 restored failed payout(s) as possibly sent' "$work/app-restore.log"
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT count(*) FROM settings WHERE key='payments_recovery_required'") == 0 ]]
payouts="SELECT string_agg(order_id || ':' || state || ':' || (error LIKE 'Restored from backup:%') || ':' || send_ambiguous || ':' || txid, ',' ORDER BY order_id) FROM payouts"
held="ops-order-failed:failed:true:true:,ops-order-queued:held:true:false:,ops-order-sent:sent:false:false:$(printf 'ab%.0s' {1..32})"
[[ $(pg "$app_target_db" -Atq -c "$payouts") == "$held" ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT string_agg(state || ':' || send_ambiguous, ',' ORDER BY order_id) FROM payouts WHERE order_id IN ('ops-order-failed','ops-order-queued')") == failed:false,pending:false ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
# The server starts on the restored database, applies nothing again and leaves the held payout alone.
start_app "$app_target_db"
stop_app
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
[[ $(pg "$app_target_db" -Atq -c "$payouts") == "$held" ]]

# Payouts sent after the backup (docs/testnet-runbook.md, "Reconcile a restored database before enabling payouts").
# The runbook's SQL runs verbatim: runbook_sql NAME prints the psql here-document (or the sql block) that follows
# the marker <!-- runbook-sql: NAME --> in the runbook.
runbook_sql() {
  python3 - "$1" docs/testnet-runbook.md <<'PY'
import re
import sys
name, path = sys.argv[1], sys.argv[2]
text = open(path, encoding='utf-8').read()
marker = '<!-- runbook-sql: %s -->' % name
if text.count(marker) != 1:
    raise SystemExit('%s must contain %s exactly once' % (path, marker))
fence = re.match(r'\s*```(\w*)\n(.*?)\n```', text.split(marker, 1)[1], re.S)
if not fence:
    raise SystemExit('%s: no code block after %s' % (path, marker))
here = re.search(r"<<'SQL'\n(.*?)\nSQL$", fence.group(2), re.S | re.M)
if here:
    print(here.group(1))
elif fence.group(1) == 'sql':
    print(fence.group(2))
else:
    raise SystemExit('%s: the block after %s has no SQL' % (path, marker))
PY
}
runbook_sql find-order > "$work/find-order.sql"
runbook_sql record-payout > "$work/record-payout.sql"
runbook_sql clear-gate > "$work/clear-gate.sql"
late_txid=$(printf 'cd%.0s' {1..32})
refund_txid=$(printf 'ef%.0s' {1..32})
# record ORDER KIND STATE AMOUNT ADDRESS TXID HANDLE yes|no: the runbook's record-payout SQL with its variables.
record() {
  pg "$app_target_db" -Atq -v order_id="$1" -v kind="$2" -v state="$3" -v amount="$4" -v address="$5" \
    -v txid="$6" -v handle="$7" -v record="$8" -f "$work/record-payout.sql"
}
# complete_again: what the buyer completing ops-order-late would do, rolled back: the transition's compare-and-set
# (orders_state.go) and enqueuePayout's insert (payments_hooks.go). "1:1" means a second release would be queued.
complete_again() {
  pg "$app_target_db" -Atq -c 'BEGIN' -c "WITH moved AS (UPDATE orders SET state='completed',updated=now() WHERE id='ops-order-late' AND state='delivered' RETURNING id),
    queued AS (INSERT INTO payouts(order_id,kind,user_id,currency,amount,address) SELECT id,'release','ops-vendor','BTC',150000,'tb1qopsrestorelatevendor' FROM moved
      ON CONFLICT (order_id) DO NOTHING RETURNING 1)
    SELECT (SELECT count(*) FROM moved) || ':' || (SELECT count(*) FROM queued)" -c 'ROLLBACK'
}
payout_count="SELECT count(*) FROM payouts"
audit_rows="SELECT md5(string_agg(id || ':' || coalesce(user_id,'') || ':' || action || ':' || created, ',' ORDER BY id)) FROM audit_events"
audit_before=$(pg "$app_target_db" -Atq -c "$audit_rows")
audit_max=$(pg "$app_target_db" -Atq -c 'SELECT max(id) FROM audit_events')
# Unreconciled, the restored order has no payout row and completing it would queue a second release.
[[ $(pg "$app_target_db" -Atq -c "SELECT count(*) FROM payouts WHERE order_id IN ('ops-order-late','ops-order-disputed')") == 0 ]]
[[ $(complete_again) == 1:1 ]]
# Finding the order from the wallet's send: amount and currency, then the recipient's address.
[[ $(pg "$app_target_db" -Atq -v currency=BTC -v amount=150000 -f "$work/find-order.sql") == 'ops-order-late|delivered|150000|tb1qopsrestorelatevendor|' ]]
# The check (record=no) prints one row and writes nothing.
check_row='ops-order-late|delivered|completed|release|BTC|ops_vendor|150000|150000|0|none|t'
[[ $(record ops-order-late release completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin no) == "$check_row"$'\nCheck passed; nothing was written. Run it again with -v record=yes to record this payout.' ]]
[[ $(pg "$app_target_db" -Atq -c "$payout_count") == 3 ]]
# Refusals write nothing: record=yes only records what the check accepts.
refused() {
  local output payouts_before
  payouts_before=$(pg "$app_target_db" -Atq -c "$payout_count")
  output=$(record "$@")
  [[ $(tail -n 1 <<< "$output") == "Refused; nothing was written: $refusal" ]] || { printf 'Unexpected output for %s:\n%s\n' "$*" "$output" >&2; return 1; }
  [[ $(pg "$app_target_db" -Atq -c "$payout_count") == "$payouts_before" ]]
}
refusal='no administrator with that handle' refused ops-order-late release completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_vendor yes
refusal='kind does not match the final state' refused ops-order-late refund completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes
refusal='the order cannot have reached that final state' refused ops-order-late release resolved 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes
refusal='counted deposits do not add up to the amount' refused ops-order-late release completed 140000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes
refusal='transaction ID must be 64 lower-case hexadecimal characters' refused ops-order-late release completed 150000 tb1qopsrestorelatevendor abc ops_admin yes
refusal='address is blank' refused ops-order-late release completed 150000 '' "$late_txid" ops_admin yes
refusal='no such order' refused ops-order-missing release completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes
refusal='the order already has a payout; resolve it on the admin page' refused ops-order-sent refund resolved 250000000000 ops-restore-sent-address "$late_txid" ops_admin yes
# Recording: one sent payout row, the order completed, its deposits credited, one order event and one audit row.
[[ $(record ops-order-late release completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes) == "$check_row"$'\nRecorded the payout as sent; committed.' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT kind || ':' || user_id || ':' || currency || ':' || amount || ':' || address || ':' || state || ':' || txid || ':' || send_ambiguous || ':' || error FROM payouts WHERE order_id='ops-order-late'") == "release:ops-vendor:BTC:150000:tb1qopsrestorelatevendor:sent:$late_txid:false:" ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT state FROM orders WHERE id='ops-order-late'") == completed ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(credited::text, ',' ORDER BY idx) FROM payments WHERE order_id='ops-order-late'") == true,true ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT from_state || ':' || to_state || ':' || actor_id || ':' || note FROM order_events WHERE order_id='ops-order-late'") == "delivered:completed:ops-admin:Reconciled after a restore from backup: the TESTNET release of 0.0015 BTC to the vendor was sent after the backup was taken, in transaction $late_txid. Recorded by an administrator from the wallet; nothing was sent now." ]]
late_payout=$(pg "$app_target_db" -Atq -c "SELECT id FROM payouts WHERE order_id='ops-order-late'")
[[ $(pg "$app_target_db" -Atq -c "SELECT user_id || ':' || action FROM audit_events WHERE id > $audit_max") == "ops-admin:Recorded payout $late_payout (0.0015 BTC, release) sent after the backup with transaction $late_txid for order ops-orde; restored from backup, order delivered -> completed" ]]
# The same payout cannot be recorded twice, nor the same transaction for another order.
refusal='the order already has a payout; resolve it on the admin page' refused ops-order-late release completed 150000 tb1qopsrestorelatevendor "$late_txid" ops_admin yes
refusal='that transaction ID is already recorded on another payout' refused ops-order-disputed refund resolved 250000000000 ops-restore-refund-address "$late_txid" ops_admin yes
# A dispute resolved after the backup: the refund is recorded, the dispute closed with its outcome, the locked
# transfer left uncredited.
[[ $(tail -n 1 <<< "$(record ops-order-disputed refund resolved 250000000000 ops-restore-refund-address "$refund_txid" ops_admin yes)") == 'Recorded the payout as sent; committed.' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT o.state || ':' || d.outcome || ':' || d.status || ':' || p.kind || ':' || p.user_id || ':' || p.state || ':' || p.txid FROM orders o JOIN disputes d ON d.order_id=o.id JOIN payouts p ON p.order_id=o.id WHERE o.id='ops-order-disputed'") == "resolved:refund:Resolved — refund to buyer:refund:ops-buyer:sent:$refund_txid" ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(txid || ':' || credited, ',' ORDER BY txid) FROM payments WHERE order_id='ops-order-disputed'") == "$(printf '21%.0s' {1..32}):true,$(printf '22%.0s' {1..32}):false" ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT count(*) FROM audit_events WHERE id > $audit_max") == 2 ]]
# Existing audit rows are never rewritten (signed export stays byte-stable).
[[ $(pg "$app_target_db" -Atq -c "SELECT md5(string_agg(id || ':' || coalesce(user_id,'') || ':' || action || ':' || created, ',' ORDER BY id)) FROM audit_events WHERE id <= $audit_max") == "$audit_before" ]]
# Completing the reconciled order again changes nothing and queues nothing.
[[ $(complete_again) == 0:0 ]]
# Clear the gate with the runbook's SQL, restart the server and check that no second payout exists.
[[ $(pg "$app_target_db" -At -f "$work/clear-gate.sql") == $'BEGIN\nUPDATE 1\nCOMMIT' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == false ]]
start_app "$app_target_db"
stop_app
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(order_id || ':' || state, ',' ORDER BY order_id) FROM payouts WHERE order_id IN ('ops-order-late','ops-order-disputed')") == ops-order-disputed:sent,ops-order-late:sent ]]
[[ $(pg "$app_target_db" -Atq -c "$payout_count") == 5 ]]
[[ $(complete_again) == 0:0 ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT count(*) FROM payouts WHERE order_id IN ('ops-order-late','ops-order-disputed')") == 0 ]]
echo 'Encrypted backup/restore regressions passed (Unicode, overwrite refusal, wrong key, transaction rollback, payout hold, failed payouts marked possibly sent, application schema restore and restart, runbook reconciliation of payouts sent after the backup).'
