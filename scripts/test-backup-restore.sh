#!/usr/bin/env bash
# Integration test. Requires a disposable PostgreSQL server and a CREATEDB role.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
: "${TEST_DATABASE_URL:?Set an explicit PostgreSQL test URL; its role must have CREATEDB}"
for dependency in python3 psql pg_dump pg_restore age age-keygen; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-ops.XXXXXX")
export OPS_TEST_ROOT="$PWD"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(8))')
source_db="opsecmkt_ops_${suffix}_source"
target_db="opsecmkt_ops_${suffix}_target"

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
cleanup() {
  local status=$?
  trap - EXIT
  pg '' -q -c "DROP DATABASE IF EXISTS $source_db" >/dev/null 2>&1 || true
  pg '' -q -c "DROP DATABASE IF EXISTS $target_db" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
pg '' -q -c "CREATE DATABASE $source_db"
pg '' -q -c "CREATE DATABASE $target_db"
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
# Queued and in-flight payouts are held after a restore so they are never sent twice; others are untouched.
[[ $(pg "$target_db" -Atq -c "SELECT string_agg(id || ':' || state || ':' || (error LIKE 'Restored from backup:%'), ',' ORDER BY id) FROM payouts") == '1:held:true,2:held:true,3:sent:false,4:failed:false,5:blocked:false,6:held:false' ]]
[[ $(pg "$source_db" -Atq -c "SELECT count(*) FROM payouts WHERE state = 'held'") == 1 ]]
grep -q 'Held 2 restored payout(s)' "$work/restore.log"
grep -q 'do not contain the custodial wallets' "$work/restore.log"

# Restore creates a_first before colliding with z_conflict. A failure must roll
# the entire transaction back, including that earlier successful CREATE TABLE.
pg "$target_db" -q -c 'DROP TABLE a_first; UPDATE z_conflict SET id = 99;'
if scripts/restore.sh "$backup" <<< 'RESTORE' > "$work/conflict.log" 2>&1; then
  echo 'Restore unexpectedly accepted an existing conflicting table' >&2; exit 1
fi
[[ $(pg "$target_db" -Atq -c "SELECT to_regclass('public.a_first') IS NULL") == t ]]
[[ $(pg "$target_db" -Atq -c 'SELECT id FROM z_conflict') == 99 ]]
echo 'Encrypted backup/restore regressions passed (Unicode, overwrite refusal, wrong key, transaction rollback, payout hold).'
