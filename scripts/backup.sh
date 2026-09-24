#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
[[ $# -eq 1 ]] || { echo 'Usage: AGE_RECIPIENT=age1... scripts/backup.sh backups/name.dump.age' >&2; exit 1; }
: "${AGE_RECIPIENT:?Set an age public recipient key; backups are encrypted before writing}"
command -v age >/dev/null || { echo 'Install age first.' >&2; exit 1; }
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
mkdir -p "$(dirname "$output")"
temporary=$(mktemp "${output}.tmp.XXXXXX")
trap 'rm -f "$temporary"' EXIT
if [[ -n ${BACKUP_DATABASE_URL:-} ]]; then
  command -v pg_dump >/dev/null || { echo 'Install PostgreSQL client tools for external backups.' >&2; exit 1; }
  command -v python3 >/dev/null || { echo 'Python 3 is required for external connection URL parsing.' >&2; exit 1; }
  python3 scripts/postgres-tool.py BACKUP_DATABASE_URL pg_dump --format=custom --no-owner --no-acl | age -r "$AGE_RECIPIENT" > "$temporary"
  database=$(url_database "$BACKUP_DATABASE_URL")
  # A database name is not a credential; show it when it is printable as it stands.
  if [[ $database =~ ^[A-Za-z0-9_-]{1,63}$ ]]; then dumped="database $database (BACKUP_DATABASE_URL)"; else dumped='the BACKUP_DATABASE_URL database'; fi
else
  docker compose exec -T db pg_dump -U opsecmkt -d "$database" --format=custom --no-owner --no-acl | age -r "$AGE_RECIPIENT" > "$temporary"
  dumped="internal database $database"
fi
mv "$temporary" "$output"
printf 'Encrypted backup of %s saved to %s\n' "$dumped" "$output"
