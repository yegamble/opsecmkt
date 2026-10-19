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
# read_archive ARGS...: pg_restore ARGS on the archive given on stdin, connecting to no database, in UTC (see the
# backup time below), where the destination's client tools run.
read_archive() {
  if [[ -n ${RESTORE_DATABASE_URL:-} ]]; then
    TZ=UTC python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL pg_restore "$@"
  else
    (cd "$root_dir" && docker compose exec -T -e TZ=UTC db pg_restore "$@")
  fi
}
selected_pg_restore_version() {
  if [[ -n ${RESTORE_DATABASE_URL:-} ]]; then
    python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL pg_restore --version
  else
    # This probe runs before the operator's typed confirmation. Keep it from consuming stdin
    # when the caller provides that confirmation non-interactively (for example, in a recovery
    # rehearsal using a here-string).
    (cd "$root_dir" && docker compose exec -T db pg_restore --version </dev/null)
  fi
}
# QA5W4B-2, QA5W5B-1: the encoding the dump's text was written in, from the SET client_encoding its script starts with,
# and the encoding of the database it was taken from, from its DATABASE entry's CREATE DATABASE (--create prints that
# entry whatever the list selects; the empty list selects nothing else, so only the start of the archive is read).
# pg_dump writes text in the database's encoding, or in a client_encoding set for the database, for the role or by
# PGCLIENTENCODING, unless BACKUP_DUMP_ENCODING=SQL_ASCII makes it write the bytes stored. The market before A-481
# stored UTF-8 bytes whatever the database's encoding: a LATIN1 database backed up without that option (install.sh
# upgrade's automatic pre-upgrade backup, for one) is dumped as LATIN1, or as UTF8 converted from LATIN1 once
# client_encoding is UTF8, and either restored into a UTF8 database double-encodes every non-ASCII character with
# nothing failing. So only a SQL_ASCII dump (the bytes stored) is restored from any database, and a UTF8 dump only from a
# UTF8 or SQL_ASCII one (nothing converted). Every whole-database dump has the DATABASE entry (pg_dump 11 and later write it
# even with --table or --data-only); without it a UTF8 dump of a UTF8 database cannot be told from one converted from
# LATIN1, so only SQL_ASCII is restored then.
# Anything else is refused here, before the confirmation and before anything is created or written. The decryption
# stopping early is expected (|| true); an error of age, pg_restore or the URL parsing is shown above the refusal.
archive_head=$(age -d -i "$AGE_IDENTITY" "$1" | read_archive --file=- --create --use-list=/dev/null) || true
archive_encoding=$(sed -n "s/^SET client_encoding = '\([A-Za-z0-9_]*\)';\$/\1/p" <<< "$archive_head" | head -n 1) || true
source_encoding=$(sed -n "s/^CREATE DATABASE .* WITH TEMPLATE = template0 ENCODING = '\([A-Za-z0-9_]*\)'.*/\1/p" <<< "$archive_head" | head -n 1) || true
if [[ -z $archive_encoding ]]; then
  echo 'Cannot read the database dump in this backup (see the error above: a wrong AGE_IDENTITY, a file that is not a backup made by scripts/backup.sh, client tools older than the dump, or a destination URL that cannot be used). Nothing was changed.' >&2
  exit 1
elif [[ $archive_encoding != SQL_ASCII && -z $source_encoding ]]; then
  printf 'Refusing: this backup holds its text in %s but does not record the encoding of the database it was taken from, as every whole-database dump does (scripts/backup.sh makes only those). Restored into a UTF8 database, its non-ASCII text could come out double-encoded, and nothing here can tell. Nothing was changed. Take a new backup of that database with scripts/backup.sh (with BACKUP_DUMP_ENCODING=SQL_ASCII if the database is not UTF8) and restore that: see UPGRADING.md, section 42.\n' \
    "$archive_encoding" >&2
  exit 1
elif [[ $archive_encoding != SQL_ASCII && ! ($archive_encoding == UTF8 && ($source_encoding == UTF8 || $source_encoding == SQL_ASCII)) ]]; then
  if [[ $source_encoding == UTF8 ]]; then
    effect="restore.sh restores a UTF8 database's backup only from a dump written in UTF8 or SQL_ASCII."
  else
    effect='Restored into a UTF8 database, every non-ASCII character the market stored would be double-encoded.'
  fi
  # PostgreSQL creates no database in these (pg_wchar.h's client-only encodings): the way out below names the database's
  # own encoding, which is always one a database can have.
  case $archive_encoding in
    SJIS | SHIFT_JIS_2004 | BIG5 | GBK | UHC | GB18030 | JOHAB) effect+=" $archive_encoding is an encoding for clients only: no database can be created in it." ;;
  esac
  printf 'Refusing: this backup was taken from a %s database and holds its text in %s. pg_dump writes text in the database'"'"'s encoding, or in a client_encoding set for the database, for the role or by PGCLIENTENCODING, unless BACKUP_DUMP_ENCODING=SQL_ASCII is used. %s Nothing was changed. Back up the old database again with BACKUP_DUMP_ENCODING=SQL_ASCII and restore that backup; if this backup is all you have, restore it into a temporary %s database, back that up with BACKUP_DUMP_ENCODING=SQL_ASCII and restore the new backup: see UPGRADING.md, section 42.\n' \
    "$source_encoding" "$archive_encoding" "$effect" "$source_encoding" >&2
  exit 1
fi
# Custom archives record the pg_dump client that produced them. Refuse an older
# or unrecognized restore client before the operator confirms or any database is
# created/changed. Do not print raw archive headers or connection configuration.
dump_client_version=$(age -d -i "$AGE_IDENTITY" "$1" 2>/dev/null | read_archive --list 2>/dev/null |
  sed -n '/^;[[:space:]]*Dumped by pg_dump version:/ { s/^;[[:space:]]*Dumped by pg_dump version:[[:space:]]*//; p; q; }') || true
restore_client_version=$(selected_pg_restore_version) || {
  echo 'Cannot determine the selected pg_restore version; nothing was changed.' >&2; exit 1; }
if [[ -z $dump_client_version ]] || ! python3 "$script_dir/postgres-policy.py" compatible "$dump_client_version" "$restore_client_version"; then
  if [[ -z $dump_client_version ]]; then
    echo 'Cannot determine which pg_dump version created this archive; nothing was changed.' >&2
  fi
  exit 1
fi
read -r -p "Restore into $destination? Type RESTORE: " confirmation
[[ $confirmation == RESTORE ]] || { echo 'Cancelled'; exit 1; }
if [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  # A new side-by-side database keeps the current one intact; an existing one must be empty (see below).
  exists=$(cd "$root_dir" && docker compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d postgres \
    -c "SELECT count(*) FROM pg_database WHERE datname = '$database'" </dev/null)
  if [[ $exists == 0 ]]; then
    (cd "$root_dir" && docker compose exec -T db psql -X -q -v ON_ERROR_STOP=1 -U opsecmkt -d postgres -c "CREATE DATABASE \"$database\" ENCODING 'UTF8' TEMPLATE template0" </dev/null)
    printf 'Created empty database %s.\n' "$database"
  fi
fi
# No --clean: this cannot silently delete an existing schema. Transaction rolls back on conflicts.
age -d -i "$AGE_IDENTITY" "$1" | client pg_restore --single-transaction --exit-on-error --no-owner --no-acl
# A restored Monero transaction may already have been relayed after the backup.
# Pause outbound sends, hold stored transactions for an administrator's chain check,
# retire free accounts, close old wallets and require the Monero wallet sweep.
# The xmr-escrow-restore marker block is exercised by the Monero recovery tests.
# The same SQL runs in the same way for both destinations.
if ! counts=$(client psql -X -q -A -t -v ON_ERROR_STOP=1 --single-transaction <<'SQL'
SELECT to_regclass('settings') IS NOT NULL AS has_settings \gset
\if :has_settings
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
-- The deletion ledger belongs to the CURRENT external state directory, not the
-- backup set. Startup must replay it and sweep expiry before accepting traffic.
INSERT INTO settings(key,value) VALUES ('retention_restore_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_xmr_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('xmr_escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
\endif
SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('xmr_settlements') AND attname='send_error' AND NOT attisdropped) AS has_xmr \gset
\if :has_xmr
-- xmr-escrow-restore: begin
WITH held AS (
  UPDATE xmr_settlements SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
  WHERE state IN ('built','relayed') RETURNING 1),
retired AS (UPDATE xmr_escrow_slots SET state='retired', updated=now() WHERE state='free' RETURNING 1),
closed AS (UPDATE xmr_escrow_wallets SET state='closing', updated=now() WHERE state IN ('active','sealed') RETURNING 1)
SELECT (SELECT count(*) FROM held) AS xmr_held, (SELECT count(*) FROM retired) AS xmr_retired, (SELECT count(*) FROM closed) AS xmr_closed
-- xmr-escrow-restore: end
\gset
\else
SELECT 0 AS xmr_held, 0 AS xmr_retired, 0 AS xmr_closed \gset
\endif
-- A-547: the vendor bond wallet in use may have given addresses after the backup that these records lack, so it is
-- closed: read on, never issuing again; new bond addresses come from a fresh bond wallet.
SELECT to_regclass('xmr_bond_wallets') IS NOT NULL AS has_bond \gset
\if :has_bond
-- xmr-bond-restore: begin
WITH closed AS (UPDATE xmr_bond_wallets SET state='closed', updated=now() WHERE state='sealed' RETURNING 1)
SELECT count(*) AS bond_closed FROM closed
-- xmr-bond-restore: end
\gset
\else
SELECT 0 AS bond_closed \gset
\endif
SELECT :'has_settings'::boolean AND to_regclass('audit_events') IS NOT NULL AS has_audit \gset
\if :has_audit
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Payout recovery gate set by scripts/restore.sh'
  || CASE WHEN :'xmr_held'::int > 0 THEN '; held ' || :'xmr_held' || ' restored Monero escrow settlement(s) with a stored transaction until an administrator confirms each relay' ELSE '' END
  || '; all outbound payouts paused until an administrator clears the gate');
\endif
SELECT to_regclass('images') IS NOT NULL AS has_images \gset
\if :has_images
SELECT count(*) AS images FROM images \gset
\else
SELECT 0 AS images \gset
\endif
SELECT :'images' || ' ' || :'has_xmr' || ' ' || :'xmr_held' || ' ' || :'xmr_retired' || ' ' || :'xmr_closed' || ' ' || :'bond_closed';
SQL
); then
  echo 'The database was restored, but payout recovery protection FAILED. Do not start the application on it; apply the recovery gate and holds by hand first (UPGRADING.md, section 6).' >&2
  exit 1
fi
read -r images xmr xmr_held xmr_retired xmr_closed bond_closed <<< "$counts"
if [[ ${xmr_held:-0} != 0 ]]; then
  printf 'Held %s restored Monero escrow settlement(s) with a stored transaction; confirm each relay from the admin page only after checking the escrow daemon and the escrow wallet.\n' "$xmr_held"
fi
if [[ ${xmr:-f} == t ]]; then
  echo 'Monero escrow: release deadlines that passed in the restored records move to 72 hours after the Monero escrow watcher next runs, and each buyer is told.'
  printf 'Monero escrow (if used): retired %s free escrow account(s) and moved %s escrow wallet(s) to closing, so no restored wallet takes an order again; Monero escrow checkout waits until the Monero escrow watcher has found the escrow wallets created after the backup and checked every wallet.\n' "$xmr_retired" "$xmr_closed"
fi
if [[ ${bond_closed:-0} != 0 ]]; then
  printf 'Vendor bonds: closed %s vendor bond wallet(s), which may have given addresses after the backup that these records lack: they are still read and still credit the applications recorded on them, but give no new address; applicants without an address get one from a new bond wallet once the Monero escrow watcher has checked the wallets and the gate is cleared.\n' "$bond_closed"
fi
echo 'Application databases now require payout recovery reconciliation. All outbound payouts remain paused until the recovery gate is explicitly cleared (see docs/testnet-runbook.md).'
# A-190 (ruling D4): staff takedowns and owners' removals delete an image's row, so the restored database has every
# photo and avatar the backup had, including those removed since. The backup time is the dump's own creation time
# from its table of contents (pg_restore --list reads only the start of the archive; the decryption stopping early
# is expected, hence || true). The dump holds it as wall-clock fields of the zone pg_dump ran in, which
# scripts/backup.sh sets to UTC, and pg_restore labels them with its own zone, so it runs in UTC too (read_archive).
backup_time=$(age -d -i "$AGE_IDENTITY" "$1" 2>/dev/null | read_archive --list 2>/dev/null | sed -n 's/^;[[:space:]]*Archive created at //p' | head -n 1) || true
printf '%s images restored from a backup taken %s; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again\n' "$images" "${backup_time:-at an unknown time}"
echo 'Restore completed. Validate the new database before switching application traffic.'
echo 'Before starting an application database, set RETENTION_ENABLED=true and mount the CURRENT external RETENTION_STATE_DIR from outside this backup set. Startup must reapply its deletion manifest and preservation register and rerun retention before accepting traffic; the restore script sets retention_restore_required and does not clear it. Never replace current retention state with an older backup copy.'
if [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  printf 'Restored into database %s in the internal-db Compose service. To switch the application to it, change only the database name after the port in DATABASE_URL in .env (no change if it already names %s), keeping your password:\n' "$database" "$database"
  printf "  DATABASE_URL='postgres://opsecmkt:YOUR_POSTGRES_PASSWORD@db:5432/%s?sslmode=disable'\n" "$database"
  echo "then start it: with payments configured, cut it off from users first and reconcile it as docs/testnet-runbook.md describes (Reconcile a restored database before enabling payouts). This revision's scripts/backup.sh dumps the database named in DATABASE_URL, so later backups follow DATABASE_URL (after a rollback to an older revision, see UPGRADING.md)."
else
  echo 'Restored into the database named in RESTORE_DATABASE_URL. To switch the application to it, set DATABASE_URL in .env to that database, and point BACKUP_DATABASE_URL at the same database: external backups follow BACKUP_DATABASE_URL, not DATABASE_URL.'
fi
echo 'Database dumps do not contain wallet-RPC volumes. Restore any lost or older wallet before starting the app as docs/testnet-runbook.md describes ("Back up and restore Monero escrow wallets"), then reconcile its state with the daemon and clear the recovery gate.'
echo 'Run the app on this database with the .env from the same backup set: the SETUP_TOKEN in use when the backup was taken (without it TOTP secrets cannot be read) and the same AUDIT_SIGNING_KEY (else the audit-export public key changes); see docs/operator-guide.md, "Secrets, .env and onion identities".'
