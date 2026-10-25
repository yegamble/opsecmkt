#!/usr/bin/env bash
# Integration test. Requires a disposable PostgreSQL server and a CREATEDB role.
set -euo pipefail
umask 077
unset BACKUP_RECOVERY_SOURCE
cd "$(dirname "$0")/.."
: "${TEST_DATABASE_URL:?Set an explicit PostgreSQL test URL; its role must have CREATEDB}"
for dependency in python3 psql pg_dump pg_restore age age-keygen go; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-ops.XXXXXX")
work=$(cd "$work" && pwd -P)
export OPS_TEST_ROOT="$PWD"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(8))')
source_db="opsecmkt_ops_${suffix}_source"
target_db="opsecmkt_ops_${suffix}_target"
# The real application schema: migrated by the server itself, then backed up and restored.
app_source_db="opsecmkt_ops_${suffix}_app_source"
app_target_db="opsecmkt_ops_${suffix}_app_target"
# A copy made without scripts/restore.sh (plain pg_dump/pg_restore, as a snapshot or point-in-time recovery would).
app_manual_db="opsecmkt_ops_${suffix}_app_manual"
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
  for database in "$source_db" "$target_db" "$app_source_db" "$app_target_db" "$app_manual_db"; do
    pg '' -q -c "DROP DATABASE IF EXISTS $database" >/dev/null 2>&1 || true
  done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
pg '' -q -c "CREATE DATABASE $source_db ENCODING 'UTF8' TEMPLATE template0"
pg '' -q -c "CREATE DATABASE $target_db ENCODING 'UTF8' TEMPLATE template0"
pg '' -q -c "CREATE DATABASE $app_source_db ENCODING 'UTF8' TEMPLATE template0"
pg '' -q -c "CREATE DATABASE $app_target_db ENCODING 'UTF8' TEMPLATE template0"
pg '' -q -c "CREATE DATABASE $app_manual_db ENCODING 'UTF8' TEMPLATE template0"
pg "$source_db" -q -c "CREATE TABLE a_first (id integer PRIMARY KEY, note text NOT NULL); INSERT INTO a_first VALUES (1, 'Crème brûlée — 東京 🔒'); CREATE TABLE z_conflict (id integer PRIMARY KEY); INSERT INTO z_conflict VALUES (42);"

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
grep -Eq 'client: pg_dump \(PostgreSQL\) [0-9]' "$work/backup.log"
if grep -q -e '://' "$work/backup.log"; then echo 'backup.sh printed the connection URL' >&2; exit 1; fi
[[ -s $backup ]] || { echo 'Backup is empty' >&2; exit 1; }
python3 - "$backup" <<'PY_CHECK'
from pathlib import Path
import stat
import sys
import importlib.util
import os
from datetime import datetime, timedelta, timezone
backup = Path(sys.argv[1])
assert stat.S_IMODE(backup.stat().st_mode) == 0o600, 'Backup permissions must be 0600'
with backup.open('rb') as stream:
    assert stream.read(22) == b'age-encryption.org/v1\n', 'Backup must be age-encrypted'
spec = importlib.util.spec_from_file_location('backup_retention', Path(os.environ['OPS_TEST_ROOT']) / 'scripts/backup-retention.py')
retention = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)
metadata, _, _ = retention.metadata_for(backup, datetime.now(timezone.utc))
assert retention.timestamp(metadata['expires_at']) - retention.timestamp(metadata['created_at']) == timedelta(days=30)
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
# A-492: pooled payouts are removed (migration 108 refuses a database holding any), so restore.sh reports none.
if grep -q 'restored payout\|restored failed payout' "$work/restore.log"; then echo 'restore.sh still reports pooled payouts' >&2; exit 1; fi
grep -q 'Database dumps do not contain wallet-RPC volumes.' "$work/restore.log"
# A-142: the wallet reminder names the runbook section, which must exist under that heading.
grep -Fq 'docs/testnet-runbook.md describes ("Back up and restore Monero escrow wallets")' "$work/restore.log"
grep -q '^## Back up and restore Monero escrow wallets$' docs/testnet-runbook.md
grep -q 'point BACKUP_DATABASE_URL at the same database' "$work/restore.log"
# A-190 (ruling D4): the line about images removed after the backup, with the dump's creation time; a dump
# without an images table restores none.
images_line() { printf '^%s images restored from a backup taken [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} [A-Z]+; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again$' "$1"; }
grep -Eq "$(images_line 0)" "$work/restore.log"

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
pg "$source_db" -q -c "DROP TABLE a_first, z_conflict; CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL); INSERT INTO settings VALUES ('payments_recovery_required','false');"
pg "$target_db" -q -c 'DROP TABLE z_conflict;'
legacy_backup="$work/legacy.dump.age"
scripts/backup.sh "$legacy_backup"
scripts/restore.sh "$legacy_backup" <<< 'RESTORE' > "$work/legacy-restore.log"
[[ $(pg "$target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$target_db" -Atq -c "SELECT value FROM settings WHERE key='retention_restore_required'") == true ]]
if BACKUP_DATABASE_URL=$(connection_url "$target_db") scripts/backup.sh "$work/unreplayed.dump.age" > "$work/unreplayed-backup.log" 2>&1; then
  echo 'Backup created a fresh archive before retention replay' >&2; exit 1
fi
grep -q 'before current external retention state has been replayed' "$work/unreplayed-backup.log"
if compgen -G "$work/unreplayed.dump.age*" >/dev/null; then echo 'An unreplayed backup left a file behind' >&2; exit 1; fi
[[ $(pg "$target_db" -Atq -c "SELECT to_regclass('payouts') IS NULL") == t ]]
# A database without the Monero escrow tables (before migrations 084 and 085) restores, and the script says nothing of it.
if grep -Eq '^(Held [0-9]+ restored Monero escrow|Monero escrow \(if used\):)' "$work/legacy-restore.log"; then echo 'restore.sh reported Monero escrow state for a database without it' >&2; exit 1; fi

# The same round trip on the real application schema. The server migrates an empty database itself (with migration
# 108 no pooled payouts or payment addresses can be written); the dump is restored into an empty database. The restore
# must set the gate and hold the escrow settlements, and the server must then start on the restored database as-is.
# start_app DATABASE: runs the server from this checkout with a clean environment and waits for /healthz.
CGO_ENABLED=0 go build -trimpath -o "$work/server" ./cmd/server
app_port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
source_retention_state="$work/source-retention-state"
restored_retention_state="$work/current-restored-retention-state"
start_app() {
  local current_retention_state=$source_retention_state
  [[ $1 == "$app_source_db" ]] || current_retention_state=$restored_retention_state
  (exec env -i PATH="$PATH" DATABASE_URL="$(connection_url "$1")" SETUP_TOKEN='ops-restore-test-bootstrap-token-0123456789' \
    RETENTION_ENABLED=on RETENTION_STATE_DIR="$current_retention_state" COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" "$work/server") >> "$work/app.log" 2>&1 &
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
# Startup writes its migration audit in the same transaction as the schema.
# Preserve that baseline along with the later operator canary in the archive.
audit_all="SELECT string_agg(coalesce(user_id,'-') || ':' || action, '|' ORDER BY id) FROM audit_events"
startup_audit=$(pg "$app_source_db" -Atq -c "$audit_all")
[[ $(pg "$app_source_db" -Atq -c 'SELECT count(*) FROM audit_events WHERE user_id IS NULL') == 1 ]]
[[ $(pg "$app_source_db" -Atq -c 'SELECT count(*) FROM audit_events') == 1 ]]
migrations=$(pg "$app_source_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
[[ $(pg "$app_source_db" -Atq -c 'SELECT count(*) FROM schema_migrations') == $(find internal/market/migrations -name '*.sql' | wc -l | tr -d ' ') ]]
pg "$app_source_db" -q -c "
INSERT INTO users(id,handle,password_hash,role) VALUES ('ops-vendor','ops_vendor','not-a-login','vendor'),('ops-buyer','ops_buyer','not-a-login','buyer'),
 ('ops-admin','ops_admin','not-a-login','admin');
INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock) VALUES ('ops-product','ops-vendor','Restore drill kit','','Hardware','Worldwide','physical',250000000000,3);
INSERT INTO audit_events(user_id,action) VALUES ('ops-admin','Restore drill: an audit row written before the backup');
INSERT INTO images(id,owner_user_id,product_id,kind,\"full\",thumb,width,height) VALUES
 (repeat('a',32),'ops-vendor','ops-product','photo','\\xffd8ff','\\xffd8ff',1,1),(repeat('b',32),'ops-vendor',NULL,'avatar','\\xffd8ff','\\xffd8ff',1,1);"
# Migration 108 refuses a pooled row in the application schema the server migrated.
for pooled in "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES ('none','release','ops-vendor','XMR',250000000000,'x','sent')" \
  "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES ('none','XMR','8opsrestorepooled','monero')"; do
  if pg "$app_source_db" -q -c "$pooled" > "$work/pooled.out" 2>&1; then echo 'the application schema accepted a pooled row' >&2; exit 1; fi
  grep -q 'pooled payments are removed' "$work/pooled.out"
done
# Monero escrow (migrations 084 and 085; ESCROW_XMR, local fakechain only): an active monthly escrow wallet with a
# free account and one claimed by a completed order whose release is built (its transaction stored, not relayed yet).
# The restore holds the built release, retires the free account, moves the wallet to closing and asks for the sweep;
# it also closes the vendor bond wallet in use (A-547), which then never issues a bond address again.
pg "$app_source_db" -q -c "
INSERT INTO xmr_escrow_wallets(name,month,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state) VALUES
 ('escrow-202610-01','2026-10-01','4'||repeat('A',94),100,'-----BEGIN PGP MESSAGE-----','0123456789ABCDEF0123456789ABCDEF01234567',now(),'sealed');
UPDATE xmr_escrow_wallets SET state='active' WHERE name='escrow-202610-01';
INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,n,'8'||repeat(chr(65+n),94) FROM xmr_escrow_wallets, generate_series(1,2) n;
INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES ('ops-order-xmr-escrow','ops-buyer','ops-product','XMR',250000000000,'completed');
UPDATE xmr_escrow_slots SET state='claimed',order_id='ops-order-xmr-escrow' WHERE account_index=1;
INSERT INTO escrows(order_id,address,coin,xmr_slot_id,refund_address,fee_bps,fee_destination)
 SELECT order_id,address,'XMR',id,'8'||repeat('D',94),0,'' FROM xmr_escrow_slots WHERE account_index=1;
INSERT INTO xmr_settlements(order_id,kind,destination) VALUES ('ops-order-xmr-escrow','release','8'||repeat('E',94));
UPDATE xmr_settlements SET state='built',tx_hash=repeat('f1',32),tx_metadata='0a0b',tx_blob='0c0d',fee=1874400000,input_total=250000000000,key_images=ARRAY[repeat('a2',32)];
INSERT INTO xmr_bond_wallets(name,state) VALUES ('bond-01','creating');
UPDATE xmr_bond_wallets SET state='sealed',primary_address='4'||repeat('B',94),restore_height=100,seed_sealed='-----BEGIN PGP MESSAGE-----',
 seed_key_fpr='0123456789ABCDEF0123456789ABCDEF01234567',sealed_at=now() WHERE name='bond-01';"
xmr_state="SELECT (SELECT string_agg(state || ':' || (send_error LIKE 'Restored from backup:%'), ',') FROM xmr_settlements) || ' ' ||
 (SELECT string_agg(account_index || ':' || state, ',' ORDER BY account_index) FROM xmr_escrow_slots) || ' ' || (SELECT string_agg(name || ':' || state, ',') FROM xmr_escrow_wallets) || ' ' ||
 (SELECT string_agg(name || ':' || state, ',') FROM xmr_bond_wallets)"
xmr_restored='built:true 1:claimed,2:retired escrow-202610-01:closing bond-01:closed'
app_backup="$work/app.dump.age"
# The backup is taken in one time zone and restored in another (PR #27 SHOULD-3): the restore line must still show
# when it was taken, in UTC. A custom-format dump keeps the wall-clock time of the zone pg_dump ran in, and
# pg_restore --list labels it with its own, so both scripts run those tools in UTC.
backup_started=$(date -u +%s)
TZ=Asia/Tokyo BACKUP_DATABASE_URL=$(connection_url "$app_source_db") scripts/backup.sh "$app_backup"
# The deletion ledger is current external state copied separately, never restored
# from the rolled-back database backup. A missing current ledger must fail closed.
cp -R "$source_retention_state" "$restored_retention_state"
backup_finished=$(date -u +%s)
TZ=America/New_York RESTORE_DATABASE_URL=$(connection_url "$app_target_db") scripts/restore.sh "$app_backup" <<< 'RESTORE' > "$work/app-restore.log"
grep -q 'Held 1 restored Monero escrow settlement(s) with a stored transaction' "$work/app-restore.log"
grep -q 'Monero escrow (if used): retired 1 free escrow account(s) and moved 1 escrow wallet(s) to closing' "$work/app-restore.log"
grep -Eq "$(images_line 2)" "$work/app-restore.log"
python3 - "$work/app-restore.log" "$backup_started" "$backup_finished" <<'PY'
import calendar, re, sys, time
log, started, finished = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
match = re.search(r'from a backup taken (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) (\S+);', open(log, encoding='utf-8').read())
assert match, 'no restored-images line'
taken = calendar.timegm(time.strptime(match.group(1), '%Y-%m-%d %H:%M:%S'))
assert match.group(2) == 'UTC' and started - 1 <= taken <= finished + 1, \
    'backup time %s %s, taken between %s and %s UTC' % (match.group(1), match.group(2),
    time.strftime('%H:%M:%S', time.gmtime(started)), time.strftime('%H:%M:%S', time.gmtime(finished)))
PY
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT count(*) FROM settings WHERE key='payments_recovery_required'") == 0 ]]
# The escrow release deadline recheck is asked for (escrow_deadline.go moves passed deadlines on its next pass).
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='escrow_xmr_release_recheck'") == true ]]
grep -q 'release deadlines that passed in the restored records move to 72 hours' "$work/app-restore.log"
# P3-R10: the restored Monero escrow is held, its free account retired, its wallet closing, and the sweep asked for.
[[ $(pg "$app_target_db" -Atq -c "$xmr_state") == "$xmr_restored" ]]
[[ $(pg "$app_source_db" -Atq -c "$xmr_state") == 'built:false 1:claimed,2:free escrow-202610-01:active bond-01:sealed' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='xmr_escrow_sweep_required'") == true ]]
[[ $(pg "$app_source_db" -Atq -c "SELECT count(*) FROM settings WHERE key='xmr_escrow_sweep_required'") == 0 ]]
pooled_rows="SELECT (SELECT count(*) FROM payouts) || ':' || (SELECT count(*) FROM payment_addresses)"
[[ $(pg "$app_target_db" -Atq -c "$pooled_rows") == 0:0 ]]
[[ $(pg "$app_target_db" -Atq -c "$xmr_state") == "$xmr_restored" ]]
# restore.sh adds one audit row with no user and the counts; the row from before the backup is unchanged.
source_audit=$(pg "$app_source_db" -Atq -c "$audit_all")
[[ $source_audit == "$startup_audit|ops-admin:Restore drill: an audit row written before the backup" ]]
restore_audit="-:Payout recovery gate set by scripts/restore.sh; held 1 restored Monero escrow settlement(s) with a stored transaction until an administrator confirms each relay; all outbound payouts paused until an administrator clears the gate"
[[ $(pg "$app_target_db" -Atq -c "$audit_all") == "$source_audit|$restore_audit" ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
# A missing current ledger and an attempt to switch retention off both refuse
# startup before any user/payment traffic; neither clears the restore gate.
for retention_mode in missing disabled; do
  if [[ $retention_mode == missing ]]; then enabled=on; else enabled=off; fi
  env -i PATH="$PATH" DATABASE_URL="$(connection_url "$app_target_db")" SETUP_TOKEN='ops-restore-test-bootstrap-token-0123456789' \
    RETENTION_ENABLED="$enabled" RETENTION_STATE_DIR="$work/missing-retention-state" COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" \
    python3 - "$work/server" "$work/refused-retention.log" <<'PY'
from pathlib import Path
import subprocess, sys
try:
    result = subprocess.run([sys.argv[1]], capture_output=True, text=True, timeout=10)
except subprocess.TimeoutExpired:
    raise SystemExit('Restored application started without current retention state')
Path(sys.argv[2]).write_text(result.stdout + result.stderr)
assert result.returncode != 0, 'Restored application ignored the retention restore gate'
assert 'retention' in result.stdout + result.stderr, 'Startup failed for a different reason'
PY
  [[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='retention_restore_required'") == true ]]
done
# The server starts on the restored database, applies nothing again and leaves the held settlements alone.
start_app "$app_target_db"
stop_app
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='retention_restore_required'") == false ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
# Actual application replay clears the backup-source gate while retaining payout
# reconciliation. A fresh recovery backup is safe only after that replay.
BACKUP_DATABASE_URL=$(connection_url "$app_target_db") scripts/backup.sh "$work/replayed.dump.age" > "$work/replayed-backup.log"
[[ -s $work/replayed.dump.age && -s $work/replayed.dump.age.retention.json ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
[[ $(pg "$app_target_db" -Atq -c "$pooled_rows") == 0:0 ]]
[[ $(pg "$app_target_db" -Atq -c "$xmr_state") == "$xmr_restored" ]]

# runbook_sql NAME [FILE] prints the psql here-document (or the sql block) that follows the marker
# <!-- runbook-sql: NAME --> in FILE (the runbook by default).
runbook_sql() {
  python3 - "$1" "${2:-docs/testnet-runbook.md}" <<'PY'
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
# A-505: the restore gate is set and cleared with the server's host commands. runbook_command NAME FILE prints the code
# block that follows the marker <!-- runbook-command: NAME --> in FILE, which must appear exactly once.
runbook_command() {
  python3 - "$1" "$2" <<'PY'
import re
import sys
name, path = sys.argv[1], sys.argv[2]
text = open(path, encoding='utf-8').read()
marker = '<!-- runbook-command: %s -->' % name
if text.count(marker) != 1:
    raise SystemExit('%s must contain %s exactly once' % (path, marker))
fence = re.match(r'\s*```\w*\n(.*?)\n```', text.split(marker, 1)[1], re.S)
if not fence:
    raise SystemExit('%s: no code block after %s' % (path, marker))
print(fence.group(1))
PY
}
[[ $(runbook_command clear-gate docs/testnet-runbook.md) == 'docker compose run --rm app -clear-restore-gate YOUR_ADMIN_HANDLE' ]]
[[ $(runbook_command restore-protection UPGRADING.md) == 'docker compose run --rm app -set-restore-gate' ]]
# No hand-typed SQL sets or clears the gate any more.
if grep -q -e 'runbook-sql: clear-gate' -e 'runbook-sql: restore-protection' docs/testnet-runbook.md UPGRADING.md; then
  echo 'the hand-typed restore gate SQL is still documented' >&2; exit 1
fi

# A-497: the clear-gate command belongs to the current-build reading path. A Markdown heading whose text says "prior build"
# or "never run" (headings inside code fences are ignored) marks the text after it as prior-build. The marker NAME in
# FILE must exist and no such heading may precede it: that puts it outside every prior-build range and means a reader
# going from the top never crosses a "never run" warning on the way to it.
marker_outside_prior_build() {
  awk -v marker="<!-- runbook-command: $1 -->" '
    /^```/ { fence = !fence }
    !fence && /^#+ / && tolower($0) ~ /prior[ -]build|never run/ { warned = 1 }
    index($0, marker) { found = 1; if (warned) bad = 1 }
    END { exit !(found && !bad) }
  ' "$2"
}
marker_outside_prior_build clear-gate docs/testnet-runbook.md
# The check itself (`! cmd` would not stop under set -e): a warning heading before the marker, at any level, is
# refused; one after it and a "#" comment inside a code fence are not; a missing marker is refused.
printf '## Prior build only (never run with this build)\n\ntext\n\n<!-- runbook-command: clear-gate -->\n' > "$work/prior-before.md"
printf '# Title\n\n### Prior-build detail\n\nx\n\n## Next\n\n<!-- runbook-command: clear-gate -->\n' > "$work/prior-nested.md"
# The Markdown code fence is literal fixture content.
# shellcheck disable=SC2016
printf '# Title\n\n## Current\n\n```sh\n# prior build comment, not a heading\n```\n\n<!-- runbook-command: clear-gate -->\n\n## Old (never run)\n' > "$work/prior-after.md"
if marker_outside_prior_build clear-gate "$work/prior-before.md"; then echo 'prior-build check accepted a marker below a warning heading' >&2; exit 1; fi
if marker_outside_prior_build clear-gate "$work/prior-nested.md"; then echo 'prior-build check accepted a marker below a nested warning heading' >&2; exit 1; fi
marker_outside_prior_build clear-gate "$work/prior-after.md"
if marker_outside_prior_build no-such-block "$work/prior-after.md"; then echo 'prior-build check accepted a missing marker' >&2; exit 1; fi

# A restore not done by restore.sh (plain pg_dump/pg_restore here; a volume snapshot or point-in-time recovery is
# the same) brings a built Monero settlement back unheld with no gate. The protection of UPGRADING.md section 6, the
# server's -set-restore-gate (A-505), must leave the copy exactly as restore.sh leaves it, with its own audit row.
(export MANUAL_SOURCE_URL MANUAL_TARGET_URL
 MANUAL_SOURCE_URL=$(connection_url "$app_source_db")
 MANUAL_TARGET_URL=$(connection_url "$app_manual_db")
 python3 scripts/postgres-tool.py MANUAL_SOURCE_URL pg_dump --format=custom --no-owner --no-acl > "$work/manual.dump"
 python3 scripts/postgres-tool.py MANUAL_TARGET_URL pg_restore --dbname='' --single-transaction --exit-on-error --no-owner --no-acl < "$work/manual.dump")
[[ $(pg "$app_manual_db" -Atq -c "SELECT count(*) FROM settings WHERE key='payments_recovery_required'") == 0 ]]
set_gate_out=$(env -i PATH="$PATH" DATABASE_URL="$(connection_url "$app_manual_db")" "$work/server" -set-restore-gate </dev/null)
[[ $set_gate_out == 'Restore gate set: '*'held 1 restored Monero escrow settlement(s), retired 1 free escrow account(s), moved 1 escrow wallet(s) to closing and closed 1 vendor bond wallet(s); audited. '* ]]
[[ $(pg "$app_manual_db" -Atq -c "SELECT value FROM settings WHERE key='escrow_xmr_release_recheck'") == true ]]
[[ $(pg "$app_manual_db" -Atq -c "SELECT value FROM settings WHERE key='xmr_escrow_sweep_required'") == true ]]
[[ $(pg "$app_manual_db" -Atq -c "$xmr_state") == "$xmr_restored" ]]
[[ $(pg "$app_manual_db" -Atq -c "SELECT value FROM settings WHERE key='payments_recovery_required'") == true ]]
[[ $(pg "$app_manual_db" -Atq -c "SELECT value FROM settings WHERE key='retention_restore_required'") == true ]]
[[ $(pg "$app_manual_db" -Atq -c "$audit_all") == "$source_audit|-:Payout recovery gate set by host command (-set-restore-gate); held 1 restored Monero escrow settlement(s) with a stored transaction until an administrator confirms each relay; all outbound payouts paused until an administrator clears the gate" ]]

audit_rows="SELECT md5(string_agg(id || ':' || coalesce(user_id,'') || ':' || action || ':' || created, ',' ORDER BY id)) FROM audit_events"
audit_before=$(pg "$app_target_db" -Atq -c "$audit_rows")
audit_max=$(pg "$app_target_db" -Atq -c 'SELECT max(id) FROM audit_events')
# Clear the gate with the server's -clear-restore-gate (A-505) and restart the server. clear_gate HANDLE ANSWERS runs
# it in a pseudo-terminal with ANSWERS typed ahead, and prints its exit status, then everything the terminal showed.
clear_gate() {
  GATE_DATABASE_URL=$(connection_url "$app_target_db") python3 - "$work/server" "$1" "$2" <<'PY'
import os
import pty
import sys
server, handle, answers = sys.argv[1:4]
pid, fd = pty.fork()
if pid == 0:
    os.execve(server, [server, '-clear-restore-gate', handle], {'PATH': os.environ['PATH'], 'DATABASE_URL': os.environ['GATE_DATABASE_URL']})
os.write(fd, answers.encode())
out = b''
while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out += chunk
_, status = os.waitpid(pid, 0)
print(os.waitstatus_to_exitcode(status))
sys.stdout.write(out.decode('utf-8', 'replace').replace('\r\n', '\n'))
PY
}
gate_value="SELECT value FROM settings WHERE key='payments_recovery_required'"
audit_count='SELECT count(*) FROM audit_events'
audit_total=$(pg "$app_target_db" -Atq -c "$audit_count")
all_yes=$'yes\nyes\nyes\nyes\nyes\n'
# Refused, changing nothing: stdin not a terminal; an unknown or non-administrator handle; a step not confirmed.
if gate_out=$(env -i PATH="$PATH" DATABASE_URL="$(connection_url "$app_target_db")" "$work/server" -clear-restore-gate ops_admin 2>&1 <<< "$all_yes"); then
  echo 'the clear-gate command accepted stdin that is not a terminal' >&2; exit 1
fi
[[ $gate_out == *'stdin is not one'* ]]
for handle in ops_nobody ops_vendor; do
  gate_out=$(clear_gate "$handle" "$all_yes")
  [[ ${gate_out%%$'\n'*} == 1 && $gate_out == *"no administrator has the handle \"$handle\""* && $gate_out != *'Type yes'* ]]
done
gate_out=$(clear_gate ops_admin $'yes\nyes\nno\n')
[[ ${gate_out%%$'\n'*} == 1 && $gate_out == *'reconciliation step 3 not confirmed; finish it and run this again; nothing was changed'* ]]
[[ $(pg "$app_target_db" -Atq -c "$gate_value") == true ]]
[[ $(pg "$app_target_db" -Atq -c "$audit_count") == "$audit_total" ]]
gate_out=$(clear_gate ops_admin "$all_yes")
[[ ${gate_out%%$'\n'*} == 0 && $gate_out == *'Restore gate cleared by administrator ops_admin; audited.'* ]]
[[ $(pg "$app_target_db" -Atq -c "$gate_value") == false ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT user_id || ':' || action FROM audit_events WHERE id > (SELECT max(id) - 1 FROM audit_events)") == 'ops-admin:Payout recovery gate cleared by ops_admin by host command after reconciling the restored database with the wallets' ]]
[[ $(pg "$app_target_db" -Atq -c "$audit_count") == $((audit_total + 1)) ]]
# Clearing again is refused: the gate is not set.
gate_out=$(clear_gate ops_admin "$all_yes")
[[ ${gate_out%%$'\n'*} == 1 && $gate_out == *'the restore gate is not set; nothing was changed'* ]]
[[ $(pg "$app_target_db" -Atq -c "$audit_count") == $((audit_total + 1)) ]]
# Every audit row from before the clear is unchanged.
[[ $(pg "$app_target_db" -Atq -c "SELECT md5(string_agg(id || ':' || coalesce(user_id,'') || ':' || action || ':' || created, ',' ORDER BY id)) FROM audit_events WHERE id <= $audit_max") == "$audit_before" ]]
start_app "$app_target_db"
stop_app
# The restart applies nothing again; the gate stays cleared and the escrow holds outlive it.
[[ $(pg "$app_target_db" -Atq -c "SELECT string_agg(version || ':' || name || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$migrations" ]]
[[ $(pg "$app_target_db" -Atq -c "$gate_value") == false ]]
[[ $(pg "$app_target_db" -Atq -c "$pooled_rows") == 0:0 ]]
[[ $(pg "$app_target_db" -Atq -c "$xmr_state") == "$xmr_restored" ]]

# A-561: the physical wallet cleanup register's review and confirmation SQL (docs/retention.md) runs verbatim. Each
# statement changes the exact open row and adds its audit row by the named administrator, or changes nothing and adds
# no row (unknown or non-administrator handle, inexact wallet name, an entry already confirmed).
runbook_sql wallet-cleanup-review docs/retention.md > "$work/wallet-cleanup-review.sql"
runbook_sql wallet-cleanup-confirm docs/retention.md > "$work/wallet-cleanup-confirm.sql"
pg "$app_target_db" -q -c "SELECT retention_wallet_cleanup_register('XMR', w, now() - interval '1 day', now() - interval '40 days') FROM unnest(ARRAY['escrow-202601-07', 'escrow-202601-08']) AS w"
cleanup_row="SELECT due_at || '|' || review_due || '|' || coalesce(confirmed_at::text, '-') || '|' || coalesce(confirmed_by, '-') FROM retention_wallet_cleanup WHERE coin='XMR' AND wallet_name="
cleanup_other=$(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-08'")
cleanup_before=$(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-07'")
audit_total=$(pg "$app_target_db" -Atq -c "$audit_count")
last_audit="SELECT coalesce(user_id, '-') || ':' || action FROM audit_events ORDER BY id DESC LIMIT 1"
wallet_cleanup() { pg "$app_target_db" -At -v handle="$2" -v wallet="$3" -v operator='host operator' -f "$work/wallet-cleanup-$1.sql"; }
unchanged_cleanup() {
  [[ $(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-07'") == "$1" ]]
  [[ $(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-08'") == "$cleanup_other" ]]
  [[ $(pg "$app_target_db" -Atq -c "$audit_count") == "$2" ]]
}
for step in review confirm; do
  for refused in 'ops_nobody escrow-202601-07' 'ops_vendor escrow-202601-07' 'ops_admin escrow-202601-0' 'ops_admin ESCROW-202601-07'; do
    read -r handle wallet <<< "$refused"
    [[ $(wallet_cleanup "$step" "$handle" "$wallet") == 'INSERT 0 0' ]]
    unchanged_cleanup "$cleanup_before" "$audit_total"
  done
done
[[ $(wallet_cleanup review ops_admin escrow-202601-07) == 'INSERT 0 1' ]]
# The review moves only the next review 30 days ahead; the original cleanup deadline is kept.
[[ $(pg "$app_target_db" -Atq -c "SELECT review_due > now() + interval '29 days', confirmed_at IS NULL FROM retention_wallet_cleanup WHERE wallet_name='escrow-202601-07'") == 't|t' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT due_at::text FROM retention_wallet_cleanup WHERE wallet_name='escrow-202601-07'") == "${cleanup_before%%|*}" ]]
[[ $(pg "$app_target_db" -Atq -c "$last_audit") == 'ops-admin:Physical wallet cleanup of Monero wallet escrow-202601-07 reviewed by ops_admin; next review in 30 days' ]]
[[ $(pg "$app_target_db" -Atq -c "$audit_count") == $((audit_total + 1)) ]]
cleanup_reviewed=$(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-07'")
[[ $(wallet_cleanup confirm ops_admin escrow-202601-07) == 'INSERT 0 1' ]]
[[ $(pg "$app_target_db" -Atq -c "SELECT confirmed_at IS NOT NULL AND confirmed_by = 'host operator' FROM retention_wallet_cleanup WHERE wallet_name='escrow-202601-07'") == t ]]
[[ $(pg "$app_target_db" -Atq -c "$last_audit") == 'ops-admin:Physical wallet cleanup of Monero wallet escrow-202601-07 confirmed by ops_admin (attested by host operator)' ]]
[[ $(pg "$app_target_db" -Atq -c "$audit_count") == $((audit_total + 2)) ]]
[[ $(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-07'") != "$cleanup_reviewed" ]]
# A confirmed entry is closed: neither statement changes it again or adds an audit row.
cleanup_confirmed=$(pg "$app_target_db" -Atq -c "${cleanup_row}'escrow-202601-07'")
for step in review confirm; do
  [[ $(wallet_cleanup "$step" ops_admin escrow-202601-07) == 'INSERT 0 0' ]]
  unchanged_cleanup "$cleanup_confirmed" $((audit_total + 2))
done
echo 'Encrypted backup/restore regressions passed (Unicode, overwrite refusal, wrong key, transaction rollback, Monero escrow held, its free accounts retired, its wallets closed and its sweep asked for, restore audit row, pooled rows refused by migration 108, application schema restore and restart, manual UPGRADING.md protection after a plain pg_dump/pg_restore copy, audited gate clear refusing unknown handles, audited wallet cleanup review and confirmation).'
