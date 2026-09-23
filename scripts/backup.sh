#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
[[ $# -eq 1 ]] || { echo 'Usage: AGE_RECIPIENT=age1... scripts/backup.sh backups/name.dump.age' >&2; exit 1; }
: "${AGE_RECIPIENT:?Set an age public recipient key; backups are encrypted before writing}"
command -v age >/dev/null || { echo 'Install age first.' >&2; exit 1; }
output=$1
[[ ! -e $output ]] || { echo 'Refusing to overwrite backup.' >&2; exit 1; }
mkdir -p "$(dirname "$output")"
temporary=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$temporary"' EXIT
if [[ -n ${BACKUP_DATABASE_URL:-} ]]; then
  command -v pg_dump >/dev/null || { echo 'Install PostgreSQL client tools for external backups.' >&2; exit 1; }
  command -v python3 >/dev/null || { echo 'Python 3 is required for external connection URL parsing.' >&2; exit 1; }
  python3 scripts/postgres-tool.py BACKUP_DATABASE_URL pg_dump --format=custom --no-owner --no-acl | age -r "$AGE_RECIPIENT" > "$temporary"
else
  docker compose exec -T db pg_dump -U opsecmkt -d opsecmkt --format=custom --no-owner --no-acl | age -r "$AGE_RECIPIENT" > "$temporary"
fi
mv "$temporary" "$output"
printf 'Encrypted database backup saved to %s\n' "$output"
