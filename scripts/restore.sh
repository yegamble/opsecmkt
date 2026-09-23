#!/usr/bin/env bash
set -euo pipefail
umask 077
script_dir=$(cd "$(dirname "$0")" && pwd)
[[ $# -eq 1 && -f $1 ]] || { echo 'Usage: RESTORE_DATABASE_URL=... AGE_IDENTITY=... scripts/restore.sh backup.dump.age' >&2; exit 1; }
: "${RESTORE_DATABASE_URL:?Set an explicit destination PostgreSQL URL, ideally a new empty database}"
: "${AGE_IDENTITY:?Set the path to your age private identity file}"
command -v pg_restore >/dev/null || { echo 'Install PostgreSQL client tools first.' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'Python 3 is required for connection URL parsing.' >&2; exit 1; }
command -v age >/dev/null || { echo 'Install age first.' >&2; exit 1; }
read -r -p 'Restore into the explicit RESTORE_DATABASE_URL destination? Type RESTORE: ' confirmation
[[ $confirmation == RESTORE ]] || { echo 'Cancelled'; exit 1; }
# Empty --dbname selects the parsed libpq environment without exposing credentials in argv.
# No --clean: this cannot silently delete an existing schema. Transaction rolls back on conflicts.
age -d -i "$AGE_IDENTITY" "$1" | python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL pg_restore --dbname='' --single-transaction --exit-on-error --no-owner --no-acl
# The dump can predate a payout's creation as well as its broadcast. Pause ALL outbound payouts until
# reconciliation, and replace automatic holds with manual recovery holds. Older app dumps have settings
# even if payments migrations have not run; table-only test dumps can lack either table.
if ! held=$(python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL psql -X -q -A -t -v ON_ERROR_STOP=1 --single-transaction <<'SQL'
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
