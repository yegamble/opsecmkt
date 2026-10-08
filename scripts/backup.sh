#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
[[ $# -eq 1 ]] || { echo 'Usage: AGE_RECIPIENT=age1... scripts/backup.sh backups/name.dump.age' >&2; exit 1; }
: "${AGE_RECIPIENT:?Set an age public recipient key; backups are encrypted before writing}"
command -v age >/dev/null || { echo 'Install age first.' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'Python 3 is required for safe backup-space checks.' >&2; exit 1; }
output=$1
[[ ! -e $output ]] || { echo 'Refusing to overwrite backup.' >&2; exit 1; }
# url_database URL: the database (path) of a PostgreSQL URL, cut as Go's net/url does. Never prints the URL.
url_database() {
  local url=${1%%#*} pattern='^postgres(ql)?://[^/]*/([^/]+)$'
  url=${url%%\?*}
  if [[ $url =~ $pattern ]]; then printf '%s' "${BASH_REMATCH[2]}"; fi
}
# env_database: the database in the last DATABASE_URL assignment in .env, which Compose passes to the app; a
# side-by-side restore or rollback switches it. Reads the installer's single quotes, double quotes, `export`
# and unquoted values with a trailing comment; anything else yields nothing rather than a guess.
env_database() {
  [[ -f .env ]] || return 0
  local line value='' pattern='^[[:space:]]*(export[[:space:]]+)?DATABASE_URL[[:space:]]*=[[:space:]]*(.*)$'
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    if [[ $line =~ $pattern ]]; then value=${BASH_REMATCH[2]}; fi
  done < .env
  case $value in
    \'*) value=${value#\'}; value=${value%%\'*} ;;
    \"*) value=${value#\"}; value=${value%%\"*} ;;
    *) value=${value%%[[:space:]]*} ;;
  esac
  url_database "$value"
}
identifier='^[a-z_][a-z0-9_]{0,62}$'
if [[ -n ${BACKUP_DATABASE_URL:-} && -n ${BACKUP_INTERNAL_DATABASE:-} ]]; then
  echo 'Set only one of BACKUP_DATABASE_URL and BACKUP_INTERNAL_DATABASE.' >&2; exit 1
elif [[ -z ${BACKUP_DATABASE_URL:-} ]]; then
  # The internal-db service publishes no port: dump, inside it, the database the application is configured to use.
  configured=$(env_database)
  [[ $configured =~ $identifier ]] || configured=''
  database=${BACKUP_INTERNAL_DATABASE:-$configured}
  if [[ -n ${BACKUP_INTERNAL_DATABASE:-} ]]; then
    [[ $database =~ $identifier ]] || { echo 'BACKUP_INTERNAL_DATABASE must be a lower-case PostgreSQL identifier (letters, digits, underscore).' >&2; exit 1; }
    [[ -z $configured || $configured == "$database" ]] || {
      printf 'Refusing to back up: BACKUP_INTERNAL_DATABASE (%s) differs from the database in DATABASE_URL in .env (%s), which the application uses. Unset BACKUP_INTERNAL_DATABASE or correct DATABASE_URL.\n' "$database" "$configured" >&2
      exit 1
    }
  fi
  [[ -n $database ]] || {
    echo 'Could not read a lower-case database name from DATABASE_URL in .env; set BACKUP_INTERNAL_DATABASE to the database the application uses.' >&2
    exit 1
  }
fi
# BACKUP_DUMP_ENCODING=SQL_ASCII is only for moving a database that is not UTF8 into a UTF8 one (UPGRADING.md, section
# 42): pg_dump then writes each text as the bytes stored, converting nothing. The market before A-481 stored UTF-8 bytes
# whatever the database's encoding, so a LATIN1 database dumped as LATIN1 comes out double-encoded in UTF8.
dump_encoding=()
case ${BACKUP_DUMP_ENCODING:-} in
  '') ;;
  SQL_ASCII) dump_encoding=(--encoding=SQL_ASCII) ;;
  *) echo 'BACKUP_DUMP_ENCODING may only be SQL_ASCII, and only to move a database that is not UTF8 (UPGRADING.md, section 42).' >&2; exit 1 ;;
esac
# Create only the destination directory before checking it. The guard runs before
# any temporary archive or pg_dump output is created.
output_dir=$(dirname "$output")
mkdir -p "$output_dir"
if [[ -n ${BACKUP_DATABASE_URL:-} ]]; then
  command -v pg_dump >/dev/null || { echo 'Install PostgreSQL client tools for external backups.' >&2; exit 1; }
  command -v psql >/dev/null || { echo 'Install PostgreSQL client tools for external backups.' >&2; exit 1; }
  database_bytes=$(python3 scripts/postgres-tool.py BACKUP_DATABASE_URL psql -X -q -A -t -v ON_ERROR_STOP=1 \
    -c 'SELECT pg_database_size(current_database())') || {
      echo 'Could not determine the source database size; refusing to create a backup.' >&2; exit 1; }
else
  database_bytes=$(docker compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d "$database" \
    -c 'SELECT pg_database_size(current_database())') || {
      echo 'Could not determine the source database size; refusing to create a backup.' >&2; exit 1; }
fi
database_bytes=${database_bytes//[$'\t\r\n ']/}
available_kib=$(df -PkP "$output_dir" | awk 'NR == 2 && $4 ~ /^[0-9]+$/ { print $4; found = 1 } END { if (!found) exit 1 }') || {
  echo 'Could not determine free space on the backup destination filesystem.' >&2; exit 1; }
[[ $available_kib =~ ^[0-9]+$ ]] || {
  echo 'Could not determine free space on the backup destination filesystem.' >&2; exit 1; }
if ! python3 scripts/postgres-policy.py space "$database_bytes" "$available_kib"; then
  echo 'Backup refused before writing an archive.' >&2
  exit 1
fi
if [[ -n ${BACKUP_DATABASE_URL:-} ]]; then
  dump_client_version=$(python3 scripts/postgres-tool.py BACKUP_DATABASE_URL pg_dump --version) || {
    echo 'Could not determine the selected pg_dump version.' >&2; exit 1; }
else
  dump_client_version=$(docker compose exec -T db pg_dump --version) || {
    echo 'Could not determine the selected pg_dump version.' >&2; exit 1; }
fi
if ! python3 scripts/postgres-policy.py compatible "$dump_client_version" "$dump_client_version" >/dev/null; then
  echo 'Cannot safely interpret the selected pg_dump version; refusing to create a backup.' >&2
  exit 1
fi
temporary=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$temporary"' EXIT
if [[ -n ${BACKUP_DATABASE_URL:-} ]]; then
  # TZ=UTC here and below: a custom-format dump records its creation time as wall-clock fields of pg_dump's zone,
  # and restore.sh reads them back as UTC.
  TZ=UTC python3 scripts/postgres-tool.py BACKUP_DATABASE_URL pg_dump --format=custom --no-owner --no-acl ${dump_encoding[@]+"${dump_encoding[@]}"} | age -r "$AGE_RECIPIENT" > "$temporary"
  database=$(url_database "$BACKUP_DATABASE_URL")
  # A database name is not a credential; show it when it is printable as it stands.
  if [[ $database =~ ^[A-Za-z0-9_-]{1,63}$ ]]; then dumped="database $database (BACKUP_DATABASE_URL)"; else dumped='the BACKUP_DATABASE_URL database'; fi
else
  docker compose exec -T -e TZ=UTC db pg_dump -U opsecmkt -d "$database" --format=custom --no-owner --no-acl ${dump_encoding[@]+"${dump_encoding[@]}"} | age -r "$AGE_RECIPIENT" > "$temporary"
  dumped="internal database $database"
fi
mv "$temporary" "$output"
printf 'Encrypted backup of %s saved to %s (client: %s)\n' "$dumped" "$output" "$dump_client_version"
