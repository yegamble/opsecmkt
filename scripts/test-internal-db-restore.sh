#!/usr/bin/env bash
# Integration test: encrypted backup and restore through the internal-db Compose service, which publishes no
# database port. Requires Docker with Compose v2, age, age-keygen and Python 3. Uses a uniquely named Compose
# project in a temporary copy of the deployment files, so the checkout's own .env and volumes are never used.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
for dependency in docker age age-keygen python3; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
# Only the temporary .env written below may configure Compose or the scripts.
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES DATABASE_URL SETUP_TOKEN POSTGRES_PASSWORD \
  BACKUP_DATABASE_URL RESTORE_DATABASE_URL RESTORE_INTERNAL_DATABASE AGE_RECIPIENT AGE_IDENTITY
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-internal-restore.XXXXXX")
root="$work/deploy"
mkdir -p "$root/scripts"
cp compose.yaml compose.internal-db.yaml "$root/"
cp scripts/backup.sh scripts/restore.sh scripts/postgres-tool.py "$root/scripts/"
suffix=$(python3 -c 'import secrets; print(secrets.token_hex(6))')
project="opsecmkt-restore-test-$suffix"
password=$(python3 -c 'import secrets; print(secrets.token_hex(16))')
cat > "$root/.env" <<ENV
COMPOSE_PROJECT_NAME='$project'
COMPOSE_FILE='compose.yaml:compose.internal-db.yaml'
COMPOSE_PROFILES='internal-db'
POSTGRES_PASSWORD='$password'
DATABASE_URL='postgres://opsecmkt:$password@db:5432/opsecmkt?sslmode=disable'
SETUP_TOKEN='$(python3 -c 'import secrets; print(secrets.token_hex(32))')'
ENV

compose() { (cd "$root" && docker compose "$@"); }
# sql DATABASE QUERY: one unaligned result from inside the db container.
sql() { compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d "$1" -c "$2"; }
cleanup() {
  local status=$?
  trap - EXIT
  if ((status)); then
    for log in "$work"/*.log; do [[ -f $log ]] && { echo "--- $log ---" >&2; cat "$log" >&2; }; done
    compose logs --no-color db >&2 || true
  fi
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
start_db() {
  compose up -d --wait db > "$work/compose-up.log" 2>&1
  # The deployment under test: no database port reaches the host.
  [[ -z $(docker inspect -f '{{range $port, $bindings := .HostConfig.PortBindings}}{{$port}}{{end}}' "$(compose ps -q db)") ]]
}
restore() { (cd "$root" && scripts/restore.sh "$@"); }
backup() { (cd "$root" && scripts/backup.sh "$@"); }
payouts="SELECT string_agg(id || ':' || state || ':' || (error LIKE 'Restored from backup:%'), ',' ORDER BY id) FROM payouts"
gate="SELECT value FROM settings WHERE key='payments_recovery_required'"
restored_payouts='1:held:true,2:held:true,3:sent:false,4:failed:false,5:held:true,6:held:true'
source_payouts='1:pending:false,2:sending:false,3:sent:false,4:failed:false,5:blocked:false,6:held:false'

start_db
sql opsecmkt "CREATE TABLE notes (id integer PRIMARY KEY, note text NOT NULL); INSERT INTO notes VALUES (1, 'Crème brûlée — 東京 🔒');
CREATE TABLE settings (key text PRIMARY KEY, value text NOT NULL); INSERT INTO settings VALUES ('payments_recovery_required','false');
CREATE TABLE payouts (id integer PRIMARY KEY, state text NOT NULL, error text NOT NULL DEFAULT '', updated timestamptz NOT NULL DEFAULT now());
INSERT INTO payouts(id,state) VALUES (1,'pending'),(2,'sending'),(3,'sent'),(4,'failed'),(5,'blocked'),(6,'held');" >/dev/null

age-keygen -o "$work/identity" 2> "$work/keygen.log"
age-keygen -o "$work/wrong-identity" 2>> "$work/keygen.log"
export AGE_IDENTITY="$work/identity"
AGE_RECIPIENT=$(age-keygen -y "$work/identity")
# backup.sh without BACKUP_DATABASE_URL dumps through `docker compose exec -T db`.
AGE_RECIPIENT=$AGE_RECIPIENT backup "$work/backup.dump.age" > "$work/backup.log"
python3 - "$work/backup.dump.age" <<'PY'
from pathlib import Path
import stat, sys
backup = Path(sys.argv[1])
assert stat.S_IMODE(backup.stat().st_mode) == 0o600, 'Backup permissions must be 0600'
with backup.open('rb') as stream:
    assert stream.read(22) == b'age-encryption.org/v1\n', 'Backup must be age-encrypted'
PY
databases="SELECT string_agg(datname, ',' ORDER BY datname) FROM pg_database WHERE datname LIKE 'opsecmkt%'"
[[ $(sql postgres "$databases") == opsecmkt ]]

# Ambiguous, missing, malformed and unconfirmed destinations are refused before anything is created.
if RESTORE_INTERNAL_DATABASE=opsecmkt_restored RESTORE_DATABASE_URL='postgres://x@127.0.0.1/x' restore "$work/backup.dump.age" <<< RESTORE > "$work/both.log" 2>&1; then
  echo 'Restore accepted two destinations' >&2; exit 1
fi
grep -q 'Set only one of' "$work/both.log"
if restore "$work/backup.dump.age" <<< RESTORE > "$work/none.log" 2>&1; then
  echo 'Restore accepted no destination' >&2; exit 1
fi
grep -q 'Set an explicit destination' "$work/none.log"
if RESTORE_INTERNAL_DATABASE='x"; DROP DATABASE opsecmkt; --' restore "$work/backup.dump.age" <<< RESTORE > "$work/name.log" 2>&1; then
  echo 'Restore accepted an unsafe database name' >&2; exit 1
fi
grep -q 'lower-case PostgreSQL identifier' "$work/name.log"
for system in postgres template0 template1; do
  if RESTORE_INTERNAL_DATABASE=$system restore "$work/backup.dump.age" <<< RESTORE > "$work/system.log" 2>&1; then
    echo "Restore accepted the system database $system" >&2; exit 1
  fi
  grep -q "cannot be the system database $system" "$work/system.log"
done
if RESTORE_INTERNAL_DATABASE=opsecmkt_restored restore "$work/backup.dump.age" <<< restore > "$work/cancel.log" 2>&1; then
  echo 'Restore ran without the typed confirmation' >&2; exit 1
fi
grep -q 'Cancelled' "$work/cancel.log"
[[ $(sql postgres "$databases") == opsecmkt ]]

# A wrong identity restores nothing.
if AGE_IDENTITY="$work/wrong-identity" RESTORE_INTERNAL_DATABASE=opsecmkt_wrong_key restore "$work/backup.dump.age" <<< RESTORE > "$work/wrong-key.log" 2>&1; then
  echo 'Restore accepted the wrong identity' >&2; exit 1
fi
[[ $(sql opsecmkt_wrong_key "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'") == 0 ]]

# Side-by-side rollback target: a new database next to the live one, created by the script.
RESTORE_INTERNAL_DATABASE=opsecmkt_restored restore "$work/backup.dump.age" <<< RESTORE > "$work/restore.log"
grep -q 'Created empty database opsecmkt_restored' "$work/restore.log"
grep -q 'Held 4 restored payout(s)' "$work/restore.log"
grep -q 'do not contain the custodial wallets' "$work/restore.log"
[[ $(sql opsecmkt_restored 'SELECT note FROM notes WHERE id = 1') == 'Crème brûlée — 東京 🔒' ]]
[[ $(sql opsecmkt_restored "$payouts") == "$restored_payouts" ]]
[[ $(sql opsecmkt_restored "$gate") == true ]]
# The live database is untouched.
[[ $(sql opsecmkt "$payouts") == "$source_payouts" ]]
[[ $(sql opsecmkt "$gate") == false ]]

# Restoring over the populated live database conflicts and rolls back completely; the gate is not applied.
sql opsecmkt 'DROP TABLE notes' >/dev/null
if RESTORE_INTERNAL_DATABASE=opsecmkt restore "$work/backup.dump.age" <<< RESTORE > "$work/conflict.log" 2>&1; then
  echo 'Restore unexpectedly succeeded over a populated database' >&2; exit 1
fi
[[ $(sql opsecmkt "SELECT to_regclass('public.notes') IS NULL") == t ]]
[[ $(sql opsecmkt "$payouts") == "$source_payouts" ]]
[[ $(sql opsecmkt "$gate") == false ]]

# Fresh-volume rollback target: recreate the db service with an empty volume and restore into opsecmkt itself.
compose down --volumes > "$work/compose-down.log" 2>&1
start_db
[[ $(sql opsecmkt "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'") == 0 ]]
RESTORE_INTERNAL_DATABASE=opsecmkt restore "$work/backup.dump.age" <<< RESTORE > "$work/fresh-restore.log"
if grep -q 'Created empty database' "$work/fresh-restore.log"; then echo 'Existing database was recreated' >&2; exit 1; fi
grep -q 'Held 4 restored payout(s)' "$work/fresh-restore.log"
[[ $(sql opsecmkt 'SELECT note FROM notes WHERE id = 1') == 'Crème brûlée — 東京 🔒' ]]
[[ $(sql opsecmkt "$payouts") == "$restored_payouts" ]]
[[ $(sql opsecmkt "$gate") == true ]]

# With the database service stopped the script says how to start it, rather than failing obscurely.
compose stop db > "$work/compose-stop.log" 2>&1
if RESTORE_INTERNAL_DATABASE=opsecmkt_other restore "$work/backup.dump.age" <<< RESTORE > "$work/stopped.log" 2>&1; then
  echo 'Restore succeeded without a running database service' >&2; exit 1
fi
grep -q 'docker compose up -d --wait db' "$work/stopped.log"
echo 'Internal-db Compose backup/restore regressions passed (no published port, typed confirmation, wrong key, side-by-side and fresh-volume restores, conflict rollback, payout recovery gate and holds).'
