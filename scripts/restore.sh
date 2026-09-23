#!/usr/bin/env bash
set -euo pipefail
umask 077
script_dir=$(cd "$(dirname "$0")" && pwd)
root_dir=$(cd "$script_dir/.." && pwd)
usage='Usage: AGE_IDENTITY=... RESTORE_DATABASE_URL=postgres://... scripts/restore.sh backup.dump.age
   or: AGE_IDENTITY=... RESTORE_INTERNAL_DATABASE=name scripts/restore.sh backup.dump.age  (internal-db Compose service)'
[[ $# -eq 1 && -f $1 ]] || { echo "$usage" >&2; exit 1; }
: "${AGE_IDENTITY:?Set the path to your age private identity file}"
command -v age >/dev/null || { echo 'Install age first.' >&2; exit 1; }
if [[ -n ${RESTORE_DATABASE_URL:-} && -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  echo 'Set only one of RESTORE_DATABASE_URL and RESTORE_INTERNAL_DATABASE.' >&2; exit 1
elif [[ -n ${RESTORE_DATABASE_URL:-} ]]; then
  # A host-reachable PostgreSQL server: client tools run here, credentials stay out of argv.
  command -v pg_restore >/dev/null || { echo 'Install PostgreSQL client tools first.' >&2; exit 1; }
  command -v python3 >/dev/null || { echo 'Python 3 is required for connection URL parsing.' >&2; exit 1; }
  destination='the explicit RESTORE_DATABASE_URL destination'
elif [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  # The internal-db Compose profile publishes no port: client tools run inside the db container, as backup.sh does.
  database=$RESTORE_INTERNAL_DATABASE
  [[ $database =~ ^[a-z_][a-z0-9_]{0,62}$ ]] || { echo 'RESTORE_INTERNAL_DATABASE must be a lower-case PostgreSQL identifier (letters, digits, underscore).' >&2; exit 1; }
  # System databases would receive the application schema (template1 would copy it into every new database).
  [[ $database != postgres && $database != template0 && $database != template1 ]] || { echo "RESTORE_INTERNAL_DATABASE cannot be the system database $database." >&2; exit 1; }
  command -v docker >/dev/null || { echo 'Docker with Compose v2 is required for the internal database.' >&2; exit 1; }
  # </dev/null: `exec -T` forwards stdin, which would otherwise swallow the typed confirmation.
  (cd "$root_dir" && docker compose exec -T db pg_isready -q -U opsecmkt -d postgres </dev/null) || {
    echo 'The internal-db Compose service is not running; start only the database with: docker compose up -d --wait db' >&2; exit 1; }
  destination="database \"$database\" in the internal-db Compose service"
else
  echo 'Set an explicit destination: RESTORE_DATABASE_URL (ideally a new empty database) or RESTORE_INTERNAL_DATABASE.' >&2
  echo "$usage" >&2
  exit 1
fi
# client TOOL ARGS...: runs a PostgreSQL client against the chosen destination.
client() {
  local tool=$1
  shift
  if [[ -n ${RESTORE_DATABASE_URL:-} ]]; then
    # Empty --dbname selects the parsed libpq environment without exposing credentials in argv.
    python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL "$tool" --dbname='' "$@"
  else
    (cd "$root_dir" && docker compose exec -T db "$tool" --username=opsecmkt --dbname="$database" "$@")
  fi
}
read -r -p "Restore into $destination? Type RESTORE: " confirmation
[[ $confirmation == RESTORE ]] || { echo 'Cancelled'; exit 1; }
if [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  # A new side-by-side database keeps the current one intact; an existing one must be empty (see below).
  exists=$(cd "$root_dir" && docker compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d postgres \
    -c "SELECT count(*) FROM pg_database WHERE datname = '$database'" </dev/null)
  if [[ $exists == 0 ]]; then
    (cd "$root_dir" && docker compose exec -T db psql -X -q -v ON_ERROR_STOP=1 -U opsecmkt -d postgres -c "CREATE DATABASE \"$database\"" </dev/null)
    printf 'Created empty database %s.\n' "$database"
  fi
fi
# No --clean: this cannot silently delete an existing schema. Transaction rolls back on conflicts.
age -d -i "$AGE_IDENTITY" "$1" | client pg_restore --single-transaction --exit-on-error --no-owner --no-acl
# The dump can predate a payout's creation as well as its broadcast. Pause ALL outbound payouts until
# reconciliation, and replace automatic holds with manual recovery holds. Older app dumps have settings
# even if payments migrations have not run; table-only test dumps can lack either table. The application
# matches the "Restored from backup:" error prefix (restoredHoldPrefix in internal/market/payments_admin.go)
# and asks for a "wallet shows no broadcast" confirmation before an administrator releases such a hold.
# The same SQL runs in the same way for both destinations.
if ! held=$(client psql -X -q -A -t -v ON_ERROR_STOP=1 --single-transaction <<'SQL'
SELECT to_regclass('settings') IS NOT NULL AS has_settings \gset
\if :has_settings
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
\endif
SELECT to_regclass('payouts') IS NOT NULL AS has_payouts \gset
\if :has_payouts
WITH held AS (
  UPDATE payouts SET state='held', updated=now(),
    error='Restored from backup: verify in the wallet before releasing; this payout may already have been sent.'
  WHERE state IN ('pending','sending','blocked','held') RETURNING 1)
SELECT count(*) FROM held;
\else
SELECT 0;
\endif
SQL
); then
  echo 'The database was restored, but payout recovery protection FAILED. Do not start the application on it; apply the recovery gate and holds by hand first (see UPGRADING.md).' >&2
  exit 1
fi
printf 'Held %s restored payout(s); release each from the admin page only after checking the wallet.\n' "$held"
echo 'Application databases now require payout recovery reconciliation. All outbound payouts remain paused until the recovery gate is explicitly cleared (see docs/testnet-runbook.md).'
echo 'Restore completed. Validate the new database before switching application traffic.'
echo 'Database dumps do not contain the custodial wallets (bitcoin_data / monero_wallet volumes); restore those from their own backups.'
