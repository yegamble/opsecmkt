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
# reconciliation, and replace automatic holds with manual recovery holds. A payout that failed before the
# backup may have been requeued and sent after it, so failed payouts are marked possibly sent (send_ambiguous,
# which dumps taken before migration 053 lack; that migration marks their failed payouts ambiguous itself) and
# their error is prefixed with the same marker. Older app dumps have settings even if payments migrations have
# not run; table-only test dumps can lack either table. The application matches the "Restored from backup:"
# error prefix (restoredHoldPrefix in internal/market/payments_admin.go) and asks for a "wallet shows no
# broadcast" confirmation before an administrator releases or requeues such a payout. A payout held for an
# account suspension keeps that text after the restore prefix, so its payout address check is not lost
# (heldForSuspension in payments_admin.go). One new audit row (no user) records the gate and the counts; existing
# audit rows are never changed. Bitcoin escrow settlements (ESCROW_BTC, regtest only) are never sent as payouts, but
# a restored one with a stored final transaction (signed or broadcast) may also have been sent, or replaced, after the
# backup: each is held with the same marker in settlements.send_error (restoredSettlementHold in
# internal/market/escrow_recovery.go) until an administrator confirms its rebroadcast on the admin page, and the audit
# row counts them when there are any. A restored fee bump of a settlement (settlement_bumps, migration 080) with a stored
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
# block moves each account's next escrow child index (escrow_index_floors) and the market's escrow key index 50 past the
# restored records (a database from before migration 083, development builds only, gets no margin). The block between
# its markers is plain SQL: internal/market/escrow_reserve_test.go runs it as written here.
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
\endif
SELECT to_regclass('escrow_index_floors') IS NOT NULL AS has_floors \gset
\if :has_floors
-- escrow-key-margin: begin
INSERT INTO escrow_index_floors(user_id,next_index)
SELECT k.user_id, GREATEST(COALESCE(max(split_part(p.derivation_suffix,'/',3)::bigint),0)+1, COALESCE(max(f.next_index),1))+50
FROM user_escrow_keys k
LEFT JOIN escrow_pubkeys p ON p.user_id=k.user_id AND p.derivation_suffix ~ '^/0/[1-9][0-9]{0,9}$'
LEFT JOIN escrow_index_floors f ON f.user_id=k.user_id
GROUP BY k.user_id
ON CONFLICT (user_id) DO UPDATE SET next_index=EXCLUDED.next_index;
DO $$ BEGIN PERFORM setval('escrow_market_index', nextval('escrow_market_index')+49); END $$;
-- escrow-key-margin: end
\endif
SELECT to_regclass('payouts') IS NOT NULL AS has_payouts \gset
\if :has_payouts
SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('payouts') AND attname='send_ambiguous' AND NOT attisdropped) AS has_send_ambiguous \gset
\if :has_send_ambiguous
UPDATE payouts SET send_ambiguous=true WHERE state='failed';
\endif
WITH held AS (
  UPDATE payouts SET state='held', updated=now(),
    error='Restored from backup: verify in the wallet before releasing; this payout may already have been sent.'
      || coalesce(' ' || substring(error from '(?:Suspended account:|Larger than the order price:).*'), '')
  WHERE state IN ('pending','sending','blocked','held') RETURNING 1),
failed AS (
  UPDATE payouts SET updated=now(),
    error='Restored from backup: verify in the wallet before requeueing; this payout may have been requeued and sent after the backup. Last error: ' || error
  WHERE state='failed' RETURNING 1)
SELECT (SELECT count(*) FROM held) AS held, (SELECT count(*) FROM failed) AS failed \gset
\else
SELECT 0 AS held, 0 AS failed \gset
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
SELECT :'has_settings'::boolean AND to_regclass('audit_events') IS NOT NULL AS has_audit \gset
\if :has_audit
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Payout recovery gate set by scripts/restore.sh: held ' || :'held'
  || ' restored payout(s) and marked ' || :'failed' || ' restored failed payout(s) as possibly sent'
  || CASE WHEN :'escrow_held'::int > 0 THEN '; held ' || :'escrow_held' || ' restored escrow settlement(s) with a stored transaction until an administrator confirms each rebroadcast' ELSE '' END
  || CASE WHEN :'bump_held'::int > 0 THEN '; held ' || :'bump_held' || ' restored escrow fee bump(s) with a stored transaction until an administrator confirms its settlement''s rebroadcast' ELSE '' END
  || '; all outbound payouts paused until an administrator clears the gate');
\endif
SELECT to_regclass('images') IS NOT NULL AS has_images \gset
\if :has_images
SELECT count(*) AS images FROM images \gset
\else
SELECT 0 AS images \gset
\endif
SELECT :'held' || ' ' || :'failed' || ' ' || :'images' || ' ' || :'escrow_held' || ' ' || :'bump_held' || ' ' || :'has_floors';
SQL
); then
  echo 'The database was restored, but payout recovery protection FAILED. Do not start the application on it; apply the recovery gate and holds by hand first (UPGRADING.md, section 6).' >&2
  exit 1
fi
read -r held failed images escrow_held bump_held floors <<< "$counts"
printf 'Held %s restored payout(s); release each from the admin page only after checking the wallet.\n' "$held"
if [[ ${escrow_held:-0} != 0 ]]; then
  printf 'Held %s restored Bitcoin escrow settlement(s) with a stored transaction; confirm each rebroadcast from the admin page only after checking the chain and the escrow watch-only wallet.\n' "$escrow_held"
fi
if [[ ${bump_held:-0} != 0 ]]; then
  printf "Held %s restored Bitcoin escrow fee bump(s) with a stored transaction; confirming its settlement's rebroadcast on the admin page releases each.\n" "$bump_held"
fi
printf 'Marked %s restored failed payout(s) as possibly sent after the backup; requeue each from the admin page only after checking the wallet.\n' "$failed"
echo 'Bitcoin escrow (if used): release deadlines that passed in the restored records move to 72 hours after the escrow watcher'"'"'s next pass, and each buyer is told.'
if [[ ${floors:-f} == t ]]; then
  echo 'Bitcoin escrow (if used): escrow checkout waits until the escrow watcher has reserved the keys of the escrows its watch-only wallet holds and the restored records lack, and every next escrow key index moves 50 past the restored records.'
else
  echo 'Bitcoin escrow (if used): escrow checkout waits until the escrow watcher has reserved the keys of the escrows its watch-only wallet holds and the restored records lack; this database predates migration 083, so escrow key indices get no margin (docs/testnet-runbook.md).'
fi
echo 'Application databases now require payout recovery reconciliation. All outbound payouts remain paused until the recovery gate is explicitly cleared (see docs/testnet-runbook.md).'
# A-190 (ruling D4): staff takedowns and owners' removals delete an image's row, so the restored database has every
# photo and avatar the backup had, including those removed since. The backup time is the dump's own creation time
# from its table of contents (pg_restore --list reads only the start of the archive; the decryption stopping early
# is expected, hence || true). The dump holds it as wall-clock fields of the zone pg_dump ran in, which
# scripts/backup.sh sets to UTC, and pg_restore labels them with its own zone, so it runs in UTC too.
list_archive() {
  if [[ -n ${RESTORE_DATABASE_URL:-} ]]; then
    TZ=UTC python3 "$script_dir/postgres-tool.py" RESTORE_DATABASE_URL pg_restore --list
  else
    (cd "$root_dir" && docker compose exec -T -e TZ=UTC db pg_restore --list)
  fi
}
backup_time=$(age -d -i "$AGE_IDENTITY" "$1" 2>/dev/null | list_archive 2>/dev/null | sed -n 's/^;[[:space:]]*Archive created at //p' | head -n 1) || true
printf '%s images restored from a backup taken %s; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again\n' "$images" "${backup_time:-at an unknown time}"
echo 'Restore completed. Validate the new database before switching application traffic.'
if [[ -n ${RESTORE_INTERNAL_DATABASE:-} ]]; then
  printf 'Restored into database %s in the internal-db Compose service. To switch the application to it, change only the database name after the port in DATABASE_URL in .env (no change if it already names %s), keeping your password:\n' "$database" "$database"
  printf "  DATABASE_URL='postgres://opsecmkt:YOUR_POSTGRES_PASSWORD@db:5432/%s?sslmode=disable'\n" "$database"
  echo "then start it: with payments configured, cut it off from users first and reconcile it as docs/testnet-runbook.md describes (Reconcile a restored database before enabling payouts). This revision's scripts/backup.sh dumps the database named in DATABASE_URL, so later backups follow DATABASE_URL (after a rollback to an older revision, see UPGRADING.md)."
else
  echo 'Restored into the database named in RESTORE_DATABASE_URL. To switch the application to it, set DATABASE_URL in .env to that database, and point BACKUP_DATABASE_URL at the same database: external backups follow BACKUP_DATABASE_URL, not DATABASE_URL.'
fi
echo 'Database dumps do not contain the custodial wallets (bitcoin_data / monero_wallet volumes). A wallet restored from an older backup or seed hands out addresses this database has already issued, and payment requests then fail: restore the wallets and advance their address index before users return, as docs/testnet-runbook.md describes ("Restore the wallets").'
echo 'Run the app on this database with the .env from the same backup set: the SETUP_TOKEN in use when the backup was taken (without it TOTP secrets cannot be read) and the same AUDIT_SIGNING_KEY (else the audit-export public key changes); see docs/operator-guide.md, "Secrets, .env and onion identities".'
