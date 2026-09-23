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
# The payouts shape restore.sh relies on (state, error, updated); one row per state.
pg "$source_db" -q -c "CREATE TABLE payouts (id integer PRIMARY KEY, state text NOT NULL, error text NOT NULL DEFAULT '', updated timestamptz NOT NULL DEFAULT now()); INSERT INTO payouts(id,state) VALUES (1,'pending'),(2,'sending'),(3,'sent'),(4,'failed'),(5,'blocked'),(6,'held');"

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
scripts/backup.sh "$backup"
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
# Blocked and automatically held payouts must also require reconciliation after restoration.
[[ $(pg "$target_db" -Atq -c "SELECT string_agg(id || ':' || state || ':' || (error LIKE 'Restored from backup:%'), ',' ORDER BY id) FROM payouts") == '1:held:true,2:held:true,3:sent:false,4:failed:false,5:held:true,6:held:true' ]]
[[ $(pg "$source_db" -Atq -c "SELECT count(*) FROM payouts WHERE state = 'held'") == 1 ]]
grep -q 'Held 4 restored payout(s)' "$work/restore.log"
grep -q 'do not contain the custodial wallets' "$work/restore.log"

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

# The same round trip on the real application schema. The server migrates an empty database itself; a
# release payout is queued (pending) and another already sent; the dump is restored into an empty database.
# The restore must hold the queued payout, and the server must then start on the restored database as-is.
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
 ('ops-order-queued','ops-buyer','ops-product','BTC',150000,'completed'),('ops-order-sent','ops-buyer','ops-product','XMR',250000000000,'resolved');
INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES ('ops-order-queued','release','ops-vendor','BTC',150000,'tb1qopsrestorequeuedpayout','pending');
INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid) VALUES ('ops-order-sent','refund','ops-buyer','XMR',250000000000,'ops-restore-sent-address','sent',repeat('ab',32));"
app_backup="$work/app.dump.age"
BACKUP_DATABASE_URL=$(connection_url "$app_source_db") scripts/backup.sh "$app_backup"
RESTORE_DATABASE_URL=$(connection_url "$app_target_db") scripts/restore.sh "$app_backup" <<< 'RESTORE' > "$work/app-restore.log"
grep -q 'Held 1 restored payout(s)' "$work/app-restore.log"
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT count(*) FROM settings WHERE key='payments_recovery_required'") == 0 ]]
payouts="SELECT string_agg(order_id || ':' || state || ':' || (error LIKE 'Restored from backup:%') || ':' || txid, ',' ORDER BY order_id) FROM payouts"
held="ops-order-queued:held:true:,ops-order-sent:sent:false:$(printf 'ab%.0s' {1..32})"
[[ $(pg "$app_target_db" -Atq -c "$payouts") == "$held" ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT state FROM payouts WHERE order_id='ops-order-queued'") == pending ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
# The server starts on the restored database, applies nothing again and leaves the held payout alone.
start_app "$app_target_db"
stop_app
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
[[ $(pg "$app_target_db" -Atq -c "$payouts") == "$held" ]]
echo 'Encrypted backup/restore regressions passed (Unicode, overwrite refusal, wrong key, transaction rollback, payout hold, application schema restore and restart).'
