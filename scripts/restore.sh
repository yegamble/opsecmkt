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
echo 'Restore completed. Validate the new database before switching application traffic.'
