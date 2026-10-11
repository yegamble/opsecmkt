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
# The dump can predate a send as well as its broadcast. Pause ALL outbound sends until reconciliation (the
# payments_recovery_required gate). Older app dumps have settings even if payments migrations have not run;
# table-only test dumps can lack any table. One new audit row (no user) records the gate and the counts; existing
# audit rows are never changed. Pooled payouts are removed: migration 108 refuses a database that holds any
# (UPGRADING.md section 59), so nothing here touches the payouts table. A restored Bitcoin escrow settlement
# (ESCROW_BTC, test networks only) with a stored final transaction (signed or broadcast) may have been sent, or
# replaced, after the backup: each is held with the "Restored from backup:" marker in settlements.send_error
# (restoredSettlementHold in internal/market/escrow_recovery.go) until an administrator confirms its rebroadcast on
# the admin page, and the audit row counts them when there are any. A restored fee bump of a settlement (settlement_bumps, migration 080) with a stored
# final transaction is held the same way, in settlement_bumps.send_error, until that confirmation of its settlement.
# A physical escrow order's release deadline may have passed in the restored records: the escrow_release_recheck setting
# asks the escrow watcher's next pass to move every passed deadline of a shipped order to 72 hours later and tell its
# buyer (recheckReleaseDeadlines in internal/market/escrow_deadline.go, the only code that moves a deadline);
# escrow_xmr_release_recheck asks the same of the Monero escrow watcher for Monero escrow orders
# (xmrRecheckReleaseDeadlines in internal/market/escrow_xmr_deadline.go), on its first pass after the restore gate is cleared.
# The restored records also lack every escrow created after the backup, so their child keys could be handed out again
# inside other escrow addresses (A-366): escrow_sweep_required makes escrow checkout wait until the escrow watcher's
# orphan sweep has reserved the keys of every address its watch-only wallet holds with no escrow row (reconcileOrphans
# in internal/market/escrow_recovery.go), and, for a watch-only wallet restored from the same backup, the escrow-key-margin
# block moves each account's next escrow child index (escrow_index_floors; one counter for the /0/<i> keys and the
# per-order /<i>/0/0 ones, A-456a) and the market's escrow key index 50 past the
# restored records (a database from before migration 083, development builds only, gets no margin). The block between
# its markers is plain SQL: internal/market/escrow_reserve_test.go runs it as written here.
# Monero escrow (ESCROW_XMR, local fakechain only; migrations 084 and 085) is held the same way: every restored Monero
# settlement with a stored transaction (built or relayed) gets the same marker in xmr_settlements.send_error, so the
# Monero escrow watcher relays it again only after an administrator confirms it on the admin page; every free slot is
# retired and every active or sealed escrow wallet moves to closing, so no account of a restored wallet (one the
# restored records lack included) is ever offered to a new order; and xmr_escrow_sweep_required makes Monero escrow
# checkout wait until the watcher's sweep has found the escrow wallets created after the backup and checked every wallet
# (escrow_xmr_restore.go). The block between its xmr-escrow-restore markers is plain SQL: internal/market/
# escrow_xmr_retain_test.go runs it as written here. A database from before migration 085 (development builds only)
# gets none of it.
# The same SQL runs in the same way for both destinations.
if ! counts=$(client psql -X -q -A -t -v ON_ERROR_STOP=1 --single-transaction <<'SQL'
SELECT to_regclass('settings') IS NOT NULL AS has_settings \gset
\if :has_settings
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_xmr_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('xmr_escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
\endif
SELECT to_regclass('escrow_index_floors') IS NOT NULL AS has_floors \gset
\if :has_floors
-- escrow-key-margin: begin
INSERT INTO escrow_index_floors(user_id,next_index)
SELECT k.user_id, GREATEST(COALESCE(max(CASE WHEN p.derivation_suffix LIKE '/0/%' THEN split_part(p.derivation_suffix,'/',3) ELSE split_part(p.derivation_suffix,'/',2) END::bigint),0)+1, COALESCE(max(f.next_index),1))+50
FROM user_escrow_keys k
LEFT JOIN escrow_pubkeys p ON p.user_id=k.user_id AND (p.derivation_suffix ~ '^/0/[1-9][0-9]{0,9}$' OR p.derivation_suffix ~ '^/[1-9][0-9]{0,9}/0/0$')
LEFT JOIN escrow_index_floors f ON f.user_id=k.user_id
GROUP BY k.user_id
ON CONFLICT (user_id) DO UPDATE SET next_index=EXCLUDED.next_index;
DO $$ BEGIN PERFORM setval('escrow_market_index', nextval('escrow_market_index')+49); END $$;
-- escrow-key-margin: end
\endif
SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('settlements') AND attname='send_error' AND NOT attisdropped) AS has_settlements \gset
\if :has_settlements
WITH escrow_held AS (
  UPDATE settlements SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
  WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast') RETURNING 1)
SELECT count(*) AS escrow_held FROM escrow_held \gset
\else
SELECT 0 AS escrow_held \gset
\endif
SELECT to_regclass('settlement_bumps') IS NOT NULL AS has_bumps \gset
\if :has_bumps
WITH bump_held AS (
  UPDATE settlement_bumps SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
  WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast') RETURNING 1)
SELECT count(*) AS bump_held FROM bump_held \gset
\else
SELECT 0 AS bump_held \gset
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
SELECT :'has_settings'::boolean AND to_regclass('audit_events') IS NOT NULL AS has_audit \gset
\if :has_audit
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Payout recovery gate set by scripts/restore.sh'
  || CASE WHEN :'escrow_held'::int > 0 THEN '; held ' || :'escrow_held' || ' restored escrow settlement(s) with a stored transaction until an administrator confirms each rebroadcast' ELSE '' END
  || CASE WHEN :'bump_held'::int > 0 THEN '; held ' || :'bump_held' || ' restored escrow fee bump(s) with a stored transaction until an administrator confirms its settlement''s rebroadcast' ELSE '' END
  || CASE WHEN :'xmr_held'::int > 0 THEN '; held ' || :'xmr_held' || ' restored Monero escrow settlement(s) with a stored transaction until an administrator confirms each relay' ELSE '' END
  || '; all outbound payouts paused until an administrator clears the gate');
\endif
SELECT to_regclass('images') IS NOT NULL AS has_images \gset
\if :has_images
SELECT count(*) AS images FROM images \gset
\else
SELECT 0 AS images \gset
\endif
SELECT :'images' || ' ' || :'escrow_held' || ' ' || :'bump_held' || ' ' || :'has_floors'
  || ' ' || :'has_xmr' || ' ' || :'xmr_held' || ' ' || :'xmr_retired' || ' ' || :'xmr_closed';
SQL
); then
  echo 'The database was restored, but payout recovery protection FAILED. Do not start the application on it; apply the recovery gate and holds by hand first (UPGRADING.md, section 6).' >&2
  exit 1
fi
read -r images escrow_held bump_held floors xmr xmr_held xmr_retired xmr_closed <<< "$counts"
if [[ ${escrow_held:-0} != 0 ]]; then
  printf 'Held %s restored Bitcoin escrow settlement(s) with a stored transaction; confirm each rebroadcast from the admin page only after checking the chain and the escrow watch-only wallet.\n' "$escrow_held"
fi
if [[ ${bump_held:-0} != 0 ]]; then
  printf "Held %s restored Bitcoin escrow fee bump(s) with a stored transaction; confirming its settlement's rebroadcast on the admin page releases each.\n" "$bump_held"
fi
if [[ ${xmr_held:-0} != 0 ]]; then
  printf 'Held %s restored Monero escrow settlement(s) with a stored transaction; confirm each relay from the admin page only after checking the escrow daemon and the escrow wallet.\n' "$xmr_held"
fi
echo 'Bitcoin escrow (if used): release deadlines that passed in the restored records move to 72 hours after the escrow watcher'"'"'s next pass, and each buyer is told.'
if [[ ${floors:-f} == t ]]; then
  echo 'Bitcoin escrow (if used): escrow checkout waits until the escrow watcher has reserved the keys of the escrows its watch-only wallet holds and the restored records lack, and every next escrow key index moves 50 past the restored records.'
else
  echo 'Bitcoin escrow (if used): escrow checkout waits until the escrow watcher has reserved the keys of the escrows its watch-only wallet holds and the restored records lack; this database predates migration 083, so escrow key indices get no margin (docs/testnet-runbook.md).'
fi
if [[ ${xmr:-f} == t ]]; then
  printf 'Monero escrow (if used): retired %s free escrow account(s) and moved %s escrow wallet(s) to closing, so no restored wallet takes an order again; Monero escrow checkout waits until the Monero escrow watcher has found the escrow wallets created after the backup and checked every wallet.\n' "$xmr_retired" "$xmr_closed"
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
if [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  printf 'Restored into database %s in the internal-db Compose service. To switch the application to it, change only the database name after the port in DATABASE_URL in .env (no change if it already names %s), keeping your password:\n' "$database" "$database"
  printf "  DATABASE_URL='postgres://opsecmkt:YOUR_POSTGRES_PASSWORD@db:5432/%s?sslmode=disable'\n" "$database"
  echo "then start it: with payments configured, cut it off from users first and reconcile it as docs/testnet-runbook.md describes (Reconcile a restored database before enabling payouts). This revision's scripts/backup.sh dumps the database named in DATABASE_URL, so later backups follow DATABASE_URL (after a rollback to an older revision, see UPGRADING.md)."
else
  echo 'Restored into the database named in RESTORE_DATABASE_URL. To switch the application to it, set DATABASE_URL in .env to that database, and point BACKUP_DATABASE_URL at the same database: external backups follow BACKUP_DATABASE_URL, not DATABASE_URL.'
fi
echo 'Database dumps do not contain the escrow wallets (the escrow node'"'"'s arbiter and watch-only wallets, the escrow wallet-rpc volume). Restore any wallet that is lost or older than this database before the app starts, as docs/testnet-runbook.md describes ("Back up and restore the escrow arbiter and watch-only wallets"), then reconcile and clear the gate.'
echo 'Run the app on this database with the .env from the same backup set: the SETUP_TOKEN in use when the backup was taken (without it TOTP secrets cannot be read) and the same AUDIT_SIGNING_KEY (else the audit-export public key changes); see docs/operator-guide.md, "Secrets, .env and onion identities".'
