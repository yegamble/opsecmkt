#!/usr/bin/env bash
# UPGRADING.md section 42 rehearsed (A-481; QA5W3B-1 and -2, QA5W4B-2 to -4): a SQL_ASCII and a LATIN1 database written by the market
# before A-481 are moved into UTF8 with scripts/check-utf8.sql, scripts/backup.sh and scripts/restore.sh, as the section
# tells the operator.
#
# Starts a disposable PostgreSQL 17 container (UTF8_MIGRATION_CONTAINER names it; default opsecmkt-utf8-<pid>-<n>). The
# PostgreSQL client tools run inside it through wrappers put first on PATH, so the host needs none, of any version. The
# server built from this checkout migrates a UTF8 seed database; its schema and rows are copied byte for byte
# (pg_dump --encoding=SQL_ASCII) into a SQL_ASCII and a LATIN1 database, and rows are added there as UTF-8 bytes, as the
# market before A-481 stored them (it sent no client_encoding, so the server stored the bytes it was sent).
#
# SQL_ASCII, a review "Great \xff\xfe seller" (the review form did not check UTF-8): check-utf8.sql names the row and
# fails; backup and restore alone succeed, but the new database's own backup does not restore and names the table, so
# the round trip of section 42 stops the switch. A listing is added whose title mixes UTF-8 with a stray byte ("Cr\xc3\xa8me
# \xff br\xc3\xbbl\xc3\xa9e") and whose description is LATIN1 ("\xe9t\xe9"): the check tells the title to be typed (it
# also holds valid multibyte UTF-8) and the other two to be read as LATIN1; corrected as the section says, by the row's
# key, both bad values of that one row in a single round, the check passes, the round trip restores and the server
# starts on the new database.
# The check's reach and readings (QA5W5B-2, -3, -6, -8), in small databases of their own: array elements, domains, a
# composite key, names to quote and a 3-byte lead in SQL_ASCII; cp1252 text read as WIN1252 in a WIN1252 database; text
# to be typed in an EUC_JP one.
# LATIN1, "Crème brûlée — 東京 🔒": restore.sh refuses a backup taken as LATIN1 (it would restore every non-ASCII
# character double-encoded) before changing anything, and one taken as UTF8 with client_encoding UTF8 set for the
# database (QA5W5B-1: its DATABASE entry records LATIN1); the section's way out for an operator with only such a backup
# (a temporary LATIN1 database, backed up again with BACKUP_DUMP_ENCODING=SQL_ASCII; the section's own shell block, run
# as printed in step 3, before step 4 creates the database NEW_DATABASE_URL names: QA5W6-3) restores it exactly, as the option
# does from the old database, and the same option on the new UTF8 database changes nothing. A UTF8 database dumped in
# SJIS is refused with a way out through a UTF8 database.
#
# Needs Docker, Go, Python 3 and age. Nothing outside the container, a temporary directory and one loopback port is
# touched.
set -euo pipefail
umask 077
unset COMPOSE_PROFILES COMPOSE_FILE BACKUP_RECOVERY_SOURCE
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
for dependency in docker go python3 age age-keygen; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
container=${UTF8_MIGRATION_CONTAINER:-opsecmkt-utf8-$$-$RANDOM}
[[ $container =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || { echo 'UTF8_MIGRATION_CONTAINER is not a container name' >&2; exit 2; }
if docker container inspect "$container" >/dev/null 2>&1; then
  echo "Refusing: a container named $container exists; remove it or set UTF8_MIGRATION_CONTAINER." >&2
  exit 1
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-utf8.XXXXXX")
work=$(cd "$work" && pwd -P)
server_pid=''

stop_server() {
  if [[ -n $server_pid ]]; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    server_pid=''
  fi
}
cleanup() {
  local status=$?
  trap - EXIT
  stop_server
  if ((status)); then
    for file in "$work"/*.log; do
      [[ -f $file ]] && { echo "--- $(basename "$file") ---" >&2; cat "$file" >&2; }
    done
  fi
  docker rm -f -v "$container" >/dev/null 2>&1 || true  # -v: postgres keeps its data in an anonymous VOLUME
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

bash scripts/test-postgres-client-wrappers.sh

sql() { local database=$1; shift; docker exec -i "$container" psql -X -q -A -t -v ON_ERROR_STOP=1 -U utf8 -d "$database" "$@"; }
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
hex() { python3 -c 'import sys; print(sys.argv[1].encode("utf-8", "surrogateescape").hex())' "$1"; }

echo 'Building the server'
GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$work/server" ./cmd/server

echo 'Starting PostgreSQL 17'
docker run -d --name "$container" -p 127.0.0.1::5432 -e POSTGRES_USER=utf8 -e POSTGRES_DB=postgres \
  -e POSTGRES_INITDB_ARGS=--encoding=UTF8 -e POSTGRES_PASSWORD=utf8-test-only postgres:17-alpine >/dev/null
# TCP only: the image's first-boot initialisation server listens on the Unix socket alone.
for _ in {1..90}; do
  docker exec "$container" pg_isready -q -h 127.0.0.1 -U utf8 -d postgres && break
  sleep 1
done
docker exec "$container" pg_isready -q -h 127.0.0.1 -U utf8 -d postgres
[[ $(sql postgres -c 'SHOW server_version_num') == 17* ]]
pg_port=$(docker port "$container" 5432/tcp | head -n1 | sed 's/.*://')
url() { printf 'postgres://utf8:utf8-test-only@127.0.0.1:%s/%s?sslmode=disable' "$pg_port" "$1"; }

# The client tools backup.sh and restore.sh run, and section 42's own psql commands (through scripts/postgres-tool.py, which
# passes the URL's credentials and database in PG* variables): the same tool inside the container, against its own
# server. The container's /rehearsal holds scripts/check-utf8.sql and is the working directory, so that the section's
# `-f scripts/check-utf8.sql` reads it as printed.
docker exec "$container" mkdir -p /rehearsal/scripts
docker cp scripts/check-utf8.sql "$container:/rehearsal/scripts/check-utf8.sql"
bash scripts/create-postgres-client-wrappers.sh "$container" "$work/bin"
age-keygen -o "$work/identity" 2> /dev/null
recipient=$(age-keygen -y "$work/identity")

# doc_command NAME: the command of UPGRADING.md section 42 that NAME stands for, as printed, so that the rehearsal runs the
# documented text and cannot drift from it. Each is a client command that takes its database URL from an exported
# variable through scripts/postgres-tool.py, never on its command line (A-483). Exactly one command must match.
doc_command() {
  python3 - "$1" UPGRADING.md <<'PY'
import re, sys
name, path = sys.argv[1:]
text = open(path, encoding='utf-8').read()
start = text.index('## 42. ')
section = re.sub(r'\s+', ' ', re.sub(r'```.*?```', ' ', text[start:text.index('\n## 43. ', start)], flags=re.S))
tool = 'python3 scripts/postgres-tool.py '
wanted = {
    'show_encoding': lambda c: c == tool + "DATABASE_URL psql -c 'SHOW server_encoding'",
    'check_old': lambda c: c == tool + 'OLD_DATABASE_URL psql -X -f scripts/check-utf8.sql',
    'check_new': lambda c: c == tool + 'NEW_DATABASE_URL psql -X -f scripts/check-utf8.sql',
    'type_in': lambda c: c == tool + 'OLD_DATABASE_URL psql',
    'create_utf8': lambda c: c.startswith(tool + 'MAINTENANCE_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_utf8 '),
}[name]
found = [c for c in re.findall(r'`([^`]+)`', section) if wanted(c)]
if len(found) != 1:
    raise SystemExit(f'UPGRADING.md section 42: {len(found)} commands for {name}, expected one')
print(found[0])
PY
}
# doc_lines round_trip: the lines of section 42 step 6's shell block, one per line, without its export line.
doc_lines() {
  python3 - "$1" UPGRADING.md <<'PY'
import re, sys
name, path = sys.argv[1:]
text = open(path, encoding='utf-8').read()
start = text.index('## 42. ')
section = text[start:text.index('\n## 43. ', start)]
blocks = [b for b in re.findall(r'^ *```sh\n(.*?)^ *```', section, re.S | re.M) if 'opsecmkt_utf8_check' in b and 'DROP DATABASE' in b]
if name != 'round_trip' or len(blocks) != 1:
    raise SystemExit(f'UPGRADING.md section 42: {len(blocks)} shell blocks for {name}, expected one')
print('\n'.join(line.strip() for line in blocks[0].splitlines() if line.strip() and not line.strip().startswith('export ')))
PY
}
show_encoding_cmd=$(doc_command show_encoding)
check_old_cmd=$(doc_command check_old)
check_new_cmd=$(doc_command check_new)
type_in_cmd=$(doc_command type_in)
create_utf8_cmd=$(doc_command create_utf8)
step6=()
while IFS= read -r line; do step6+=("$line"); done < <(doc_lines round_trip)
((${#step6[@]} == 4)) || fail "section 42 step 6 has ${#step6[@]} commands, expected 4"
[[ ${step6[0]} == *scripts/backup.sh* && ${step6[1]} == *'CREATE DATABASE opsecmkt_utf8_check'* && ${step6[2]} == *scripts/restore.sh* &&
  ${step6[3]} == *'DROP DATABASE opsecmkt_utf8_check'* ]] || fail "section 42 step 6 is no longer backup, create, restore, drop"

# backup DATABASE FILE: scripts/backup.sh of DATABASE (BACKUP_DUMP_ENCODING passes through from the caller).
backup() {
  PATH="$work/bin:$PATH" BACKUP_DATABASE_URL=$(url "$1") AGE_RECIPIENT=$recipient scripts/backup.sh "$2" > "$work/backup.log" 2>&1 || return "$?"
  # Test-only locator for the CURRENT external state. The state itself is never
  # bundled into the database backup or rolled back with that archive.
  if [[ -f $work/retention-$1/retention-state.json ]]; then
    printf '%s\n' "$work/retention-$1" > "$2.current-retention-location"
  fi
}
# restore_into FILE DATABASE: scripts/restore.sh into the existing, empty DATABASE; its status.
restore_into() {
  PATH="$work/bin:$PATH" RESTORE_DATABASE_URL=$(url "$2") AGE_IDENTITY="$work/identity" scripts/restore.sh "$1" <<< RESTORE > "$work/restore-$2.log" 2>&1 || return "$?"
  if [[ -f $1.current-retention-location ]]; then
    local current
    IFS= read -r current < "$1.current-retention-location"
    cp -R "$current" "$work/retention-$2"
  fi
}
# restore FILE DATABASE: creates DATABASE (UTF8, section 42 step 4's statement) and restores FILE into it; its status.
restore() {
  sql postgres -c "CREATE DATABASE $2 ENCODING 'UTF8' TEMPLATE template0"
  restore_into "$1" "$2"
}
# check DATABASE [VARIABLE]: section 42's check-utf8.sql command, as printed for OLD_DATABASE_URL (step 2, the default) or
# for NEW_DATABASE_URL (step 6), with VARIABLE set to DATABASE's URL; its status (psql exits 3 on a bad value).
check() {
  local command=$check_old_cmd variable=${2:-OLD_DATABASE_URL}
  [[ $variable == NEW_DATABASE_URL ]] && command=$check_new_cmd
  env "$variable=$(url "$1")" PATH="$work/bin:$PATH" bash -euo pipefail -c "$command" > "$work/check-$1.log" 2>&1
}
# step6_command INDEX DATABASE DUMP: command INDEX of section 42 step 6 as printed (only the example's placeholders
# filled in), with NEW_DATABASE_URL naming DATABASE and CHECK_DATABASE_URL the scratch database opsecmkt_utf8_check.
step6_command() {
  local line=${step6[$1]}
  line=${line//'age1...'/$recipient}
  line=${line//'AGE_IDENTITY=...'/AGE_IDENTITY=$work/identity}
  line=${line//backups\/utf8-check.dump.age/$3}
  [[ $line != *...* ]] || fail "placeholder left in: $line"
  env NEW_DATABASE_URL="$(url "$2")" CHECK_DATABASE_URL="$(url opsecmkt_utf8_check)" PATH="$work/bin:$PATH" \
    bash -euo pipefail -c "$line" <<< RESTORE
}
# round_trip DATABASE: section 42 step 6 up to its restore: a backup of DATABASE restored into the scratch database
# opsecmkt_utf8_check, output in round-trip.log. On success the scratch database is left for the caller to read, then
# end_round_trip; on failure it is dropped here.
round_trip() {
  local index
  : > "$work/round-trip.log"
  replay_restored "$1" || return "$?"
  for index in 0 1 2; do
    step6_command "$index" "$1" "$work/$1-check.dump.age" >> "$work/round-trip.log" 2>&1 || {
      sql postgres -c 'DROP DATABASE IF EXISTS opsecmkt_utf8_check' >/dev/null 2>&1 || true
      return 1
    }
  done
}
# end_round_trip DATABASE: step 6's last command, the drop of the scratch database.
end_round_trip() { step6_command 3 "$1" "$work/$1-check.dump.age" >> "$work/round-trip.log" 2>&1; }
# text_hex DATABASE QUERY: the stored bytes of the one text QUERY returns, in hexadecimal.
text_hex() { sql "$1" -c "SELECT encode(convert_to(($2), 'SQL_ASCII'), 'hex')"; }

echo 'Migrating a UTF8 seed database with the current server'
sql postgres -c "CREATE DATABASE seed ENCODING 'UTF8' TEMPLATE template0"
app_port=$(free_port)
(exec env -i PATH="$PATH" DATABASE_URL="$(url seed)" SETUP_TOKEN=utf8-migration-test-token-only-0123456789 COOKIE_SECURE=false \
  ADDR="127.0.0.1:$app_port" "$work/server") > "$work/server-seed.log" 2>&1 &
server_pid=$!
wait_healthy() {
  python3 - "http://127.0.0.1:$app_port" "$server_pid" <<'PY'
import os, sys, time, urllib.request
base, pid = sys.argv[1], int(sys.argv[2])
for _ in range(120):
    try:
        os.kill(pid, 0)
    except OSError:
        raise SystemExit('server exited before becoming healthy')
    try:
        with urllib.request.urlopen(base + '/healthz', timeout=2) as response:
            if response.status == 200 and response.read() == b'ok':
                raise SystemExit(0)
    except OSError:
        pass
    time.sleep(0.5)
raise SystemExit('server did not become healthy')
PY
}
wait_healthy
stop_server

# Enrollment of an ORIGINAL non-UTF8 database starts no server, replay, expiry or
# wallet. Restored UTF8 databases then replay the separately supplied CURRENT
# state through normal isolated startup before becoming new backup sources.
enroll_original() {
  env -i PATH="$PATH" DATABASE_URL="$(url "$1")" RETENTION_ENABLED=on RETENTION_STATE_DIR="$work/retention-$1" \
    "$work/server" -enroll-retention > "$work/enroll-$1.log" 2>&1
}
replay_restored() {
  [[ -f $work/retention-$1/retention-state.json ]] || { echo "Missing current retention state for $1" >&2; return 1; }
  (exec env -i PATH="$PATH" DATABASE_URL="$(url "$1")" SETUP_TOKEN=utf8-migration-test-token-only-0123456789 COOKIE_SECURE=false \
    RETENTION_ENABLED=on RETENTION_STATE_DIR="$work/retention-$1" ADDR="127.0.0.1:$app_port" "$work/server") > "$work/server-replay-$1.log" 2>&1 &
  server_pid=$!
  wait_healthy || { stop_server; return 1; }
  stop_server
  [[ $(sql "$1" -c "SELECT value FROM settings WHERE key='retention_restore_required'") == false ]]
}

title='Crème brûlée — 東京 🔒'
title_hex=$(hex "$title")
bad_hex=477265617420fffe2073656c6c6572  # "Great \xff\xfe seller"
mixed_hex=4372c3a86d6520ff206272c3bb6cc3a965  # "Cr\xc3\xa8me \xff br\xc3\xbbl\xc3\xa9e": UTF-8 with a stray byte
latin1_hex=e974e9  # "\xe9t\xe9": "été" written in LATIN1
latin1_safe='no valid multibyte UTF-8 in it, so reading its bytes as LATIN1 is a safe correction, or as the single-byte encoding they were written in if that was another (a SQL_ASCII database does not record it; WIN1252, for one)'
type_it='it also holds valid multibyte UTF-8, which reading it as LATIN1 would double-encode: type the text it should hold'
# old_database NAME ENCODING BODY_HEX: a pre-A-481 market database: the seed's schema and rows byte for byte, then a
# vendor, a buyer, a listing titled $title, a completed order and its review whose body has the bytes BODY_HEX.
old_database() {
  sql postgres -c "CREATE DATABASE $1 ENCODING '$2' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
  docker exec "$container" sh -c "pg_dump -U utf8 --encoding=SQL_ASCII --format=custom seed | pg_restore -U utf8 --exit-on-error --no-owner -d $1"
  sql "$1" -v title="$title_hex" -v body="$3" <<'SQL'
INSERT INTO users(id,handle,password_hash,role) VALUES ('utf8buyer','utf8_buyer','x','buyer'),('utf8vendor','utf8_vendor','x','vendor');
INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock)
  VALUES ('utf8product','utf8vendor',convert_from(decode(:'title','hex'),'SQL_ASCII'),'Kept as written.','Hardware','Worldwide','digital',1,5);
INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES ('utf8order','utf8buyer','utf8product','XMR',1,'completed');
INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES ('utf8review','utf8order','utf8product','utf8buyer',5,convert_from(decode(:'body','hex'),'SQL_ASCII'));
SQL
  [[ $(sql "$1" -c 'SHOW server_encoding') == "$2" ]] || fail "$1 is not $2"
  # The section's own command for it (before you upgrade), with DATABASE_URL exported: psql prints the encoding in a table.
  [[ $(env DATABASE_URL="$(url "$1")" PATH="$work/bin:$PATH" bash -euo pipefail -c "$show_encoding_cmd" | grep -cw "$2") == 1 ]] ||
    fail "section 42's SHOW server_encoding did not print $2 for $1"
  enroll_original "$1"
}

echo 'SQL_ASCII: a review with bytes that are not UTF-8'
old_database ascii_old SQL_ASCII "$bad_hex"
status=0; check ascii_old || status=$?
[[ $status == 3 ]] || fail "check-utf8.sql exited $status on a SQL_ASCII database holding 0xff, not 3 (section 42 says psql exits 3)"
grep -qF "not valid UTF-8: table public.reviews, column body, row where id = 'utf8review' (ctid (0,1)); $latin1_safe; starting: Great \\377\\376 seller" "$work/check-ascii_old.log" ||
  fail 'check-utf8.sql did not name the review'
grep -q '1 stored text value(s) are not valid UTF-8' "$work/check-ascii_old.log" || fail 'check-utf8.sql did not count the bad value'
# Restored without the check, the bytes reach the new database silently ...
backup ascii_old "$work/ascii.dump.age"
restore "$work/ascii.dump.age" ascii_new || fail 'restore.sh refused the SQL_ASCII backup'
# (convert_to cannot read bytes that are not UTF-8 in a UTF8 database; their length shows they all arrived.)
[[ $(sql ascii_new -c "SELECT octet_length(body) FROM reviews WHERE id='utf8review'") == 15 ]] || fail 'the restored review lost bytes'
# ... and section 42's round trip is what stops the switch: the new database's own backup does not restore.
if round_trip ascii_new; then fail 'the backup of a UTF8 database holding 0xff restored'; fi
grep -q 'COPY failed for table "reviews": ERROR:  invalid byte sequence for encoding "UTF8": 0xff' "$work/round-trip.log" ||
  fail 'the round trip did not name the table'
if check ascii_new NEW_DATABASE_URL; then fail 'check-utf8.sql passed a UTF8 database holding 0xff'; fi
grep -qF "not valid UTF-8: table public.reviews, column body, row where id = 'utf8review' (ctid (0,1)); its bytes cannot be read in a UTF8 database: correct it in the old database" \
  "$work/check-ascii_new.log" || fail 'check-utf8.sql did not name the review in UTF8'
# A listing whose title mixes UTF-8 with a stray byte and whose description is LATIN1: the check tells which reading is
# safe (QA5W4B-3) and names the row by its key (QA5W4B-4).
sql ascii_old -v title="$mixed_hex" -v description="$latin1_hex" <<'SQL'
INSERT INTO products(id,vendor_id,title,description,category,region,kind,xmr,stock)
  VALUES ('utf8mixed','utf8vendor',convert_from(decode(:'title','hex'),'SQL_ASCII'),convert_from(decode(:'description','hex'),'SQL_ASCII'),'Hardware','Worldwide','digital',1,5);
SQL
if check ascii_old; then fail 'check-utf8.sql passed the mixed listing'; fi
grep -q "not valid UTF-8: table public.products, column title, row where id = 'utf8mixed' (ctid ([0-9]*,[0-9]*)); it also holds valid multibyte UTF-8, which reading it as LATIN1 would double-encode: type the text it should hold; starting: Cr\\\\303\\\\250me \\\\377 br" \
  "$work/check-ascii_old.log" || fail 'check-utf8.sql did not tell the mixed title to be typed'
grep -q "not valid UTF-8: table public.products, column description, row where id = 'utf8mixed' (ctid ([0-9]*,[0-9]*)); $latin1_safe; starting: \\\\351t\\\\351" \
  "$work/check-ascii_old.log" || fail 'check-utf8.sql did not tell the LATIN1 description to be read as LATIN1'
grep -q '3 stored text value(s) are not valid UTF-8' "$work/check-ascii_old.log" || fail 'check-utf8.sql did not count the three bad values'
# Corrected on the old database as section 42 says, one statement per line the check printed, with the row's key: the
# review and the description read as LATIN1, the title typed. Both values of the listing are corrected in this one
# round (an UPDATE moves the row, so a second statement by its old ctid would change nothing).
sql ascii_old -c "UPDATE public.reviews SET body = convert_from(convert(convert_to(body, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 'utf8review'"
# The typed title goes through the section's command, `\encoding UTF8` first (the tool lets no PGCLIENTENCODING through).
env OLD_DATABASE_URL="$(url ascii_old)" PATH="$work/bin:$PATH" bash -euo pipefail -c "$type_in_cmd -X -q -v ON_ERROR_STOP=1" <<'SQL'
\encoding UTF8
UPDATE public.products SET title = convert_from(convert_to('Crème brûlée', 'UTF8'), 'SQL_ASCII') WHERE id = 'utf8mixed';
SQL
sql ascii_old -c "UPDATE public.products SET description = convert_from(convert(convert_to(description, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 'utf8mixed'"
check ascii_old || fail 'check-utf8.sql failed after one round of corrections'
grep -q 'Every value in the [0-9]* text column(s) is valid UTF-8.' "$work/check-ascii_old.log" || fail 'check-utf8.sql did not report success'
BACKUP_DUMP_ENCODING=SQL_ASCII backup ascii_old "$work/ascii-fixed.dump.age"
restore "$work/ascii-fixed.dump.age" ascii_fixed || fail 'restore.sh refused the corrected SQL_ASCII backup'
check ascii_fixed NEW_DATABASE_URL || fail 'check-utf8.sql failed on the new database'
round_trip ascii_fixed || fail 'the round trip of the corrected database failed'
[[ $(text_hex ascii_fixed "SELECT title FROM products WHERE id='utf8product'") == "$title_hex" ]] || fail 'the SQL_ASCII title changed'
[[ $(text_hex ascii_fixed "SELECT body FROM reviews WHERE id='utf8review'") == "$(hex 'Great ÿþ seller')" ]] || fail 'the corrected review'
[[ $(text_hex opsecmkt_utf8_check "SELECT body FROM reviews WHERE id='utf8review'") == "$(hex 'Great ÿþ seller')" ]] || fail 'the round trip changed the review'
end_round_trip ascii_fixed || fail 'step 6 could not drop its scratch database'
[[ -z $(sql postgres -c "SELECT 1 FROM pg_database WHERE datname = 'opsecmkt_utf8_check'") ]] || fail 'step 6 left its scratch database'
[[ $(text_hex ascii_fixed "SELECT title FROM products WHERE id='utf8mixed'") == "$(hex 'Crème brûlée')" ]] || fail 'the typed title'
[[ $(text_hex ascii_fixed "SELECT description FROM products WHERE id='utf8mixed'") == "$(hex 'été')" ]] || fail 'the corrected description'
(exec env -i PATH="$PATH" DATABASE_URL="$(url ascii_fixed)" SETUP_TOKEN=utf8-migration-test-token-only-0123456789 COOKIE_SECURE=false \
  RETENTION_ENABLED=on RETENTION_STATE_DIR="$work/retention-ascii_fixed" \
  ADDR="127.0.0.1:$app_port" "$work/server") > "$work/server-ascii.log" 2>&1 &
server_pid=$!
wait_healthy || fail 'the server did not start on the new database'
stop_server

echo 'check-utf8.sql: arrays, domains, a composite key, names to quote, readings in WIN1252 and EUC_JP'
# QA5W5B-2: an element of a text[] or of a domain over varchar[], and a domain over text, are read too, each array
# element named by its subscript. QA5W5B-6: a composite key is written as a condition of all its columns, and a value
# whose valid multibyte UTF-8 starts with a 3-byte lead ("東京 \xff") is to be typed. QA5W5B-8: a table and a column whose
# names need quoting are printed quoted, so the UPDATE written from the line runs as printed.
sql postgres -c "CREATE DATABASE probe_ascii ENCODING 'SQL_ASCII' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
sql probe_ascii <<'SQL'
CREATE DOMAIN memo_text AS text;
CREATE DOMAIN label_list AS varchar(40)[];
CREATE TABLE pairs(a text, b int, note text, PRIMARY KEY (a, b));
CREATE TABLE tagged(id text PRIMARY KEY, tags text[], memo memo_text, labels label_list);
CREATE TABLE "Odd Table"("Odd Col" text);
INSERT INTO pairs VALUES ('x', 1, convert_from(decode('6f6bff', 'hex'), 'SQL_ASCII')), ('x', 2, 'fine');
INSERT INTO tagged VALUES ('t1', ARRAY['fine', convert_from(decode('626164ff', 'hex'), 'SQL_ASCII')],
  convert_from(decode('6d656d6ffe', 'hex'), 'SQL_ASCII'), ARRAY[convert_from(decode('e974e9', 'hex'), 'SQL_ASCII')]::label_list);
INSERT INTO "Odd Table" VALUES (convert_from(decode('e69db1e4baac20ff', 'hex'), 'SQL_ASCII'));
SQL
if check probe_ascii; then fail 'check-utf8.sql passed bad array elements and domain values'; fi
for line in "table public.pairs, column note, row where a = 'x' AND b = '1' (ctid (0,1)); $latin1_safe; starting: ok\\377" \
    "table public.tagged, column tags[2], row where id = 't1' (ctid (0,1)); $latin1_safe; starting: bad\\377" \
    "table public.tagged, column memo, row where id = 't1' (ctid (0,1)); $latin1_safe; starting: memo\\376" \
    "table public.tagged, column labels[1], row where id = 't1' (ctid (0,1)); $latin1_safe; starting: \\351t\\351" \
    "table public.\"Odd Table\", column \"Odd Col\", row (no primary key: use the ctid) (ctid (0,1)); $type_it; starting: \\346\\235\\261\\344\\272\\254 \\377"; do
  grep -qF "not valid UTF-8: $line" "$work/check-probe_ascii.log" || fail "check-utf8.sql did not print: $line"
done
grep -q '5 stored text value(s) are not valid UTF-8' "$work/check-probe_ascii.log" || fail 'check-utf8.sql did not count the five bad values'
# Corrected as section 42 says, each statement written from its line as printed.
sql probe_ascii <<'SQL'
UPDATE public.pairs SET note = convert_from(convert(convert_to(note, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE a = 'x' AND b = '1';
UPDATE public.tagged SET tags[2] = convert_from(convert(convert_to(tags[2], 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 't1';
UPDATE public.tagged SET memo = convert_from(convert(convert_to(memo, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 't1';
UPDATE public.tagged SET labels[1] = convert_from(convert(convert_to(labels[1], 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 't1';
UPDATE public."Odd Table" SET "Odd Col" = convert_from(convert_to('東京 tea', 'UTF8'), 'SQL_ASCII') WHERE ctid = '(0,1)';
SQL
check probe_ascii || fail 'check-utf8.sql failed after the corrections of arrays and domains'
[[ $(text_hex probe_ascii "SELECT tags[2] FROM tagged") == "$(hex 'badÿ')" && $(text_hex probe_ascii "SELECT labels[1] FROM tagged") == "$(hex 'été')" &&
  $(text_hex probe_ascii 'SELECT "Odd Col" FROM "Odd Table"') == "$(hex '東京 tea')" ]] || fail 'the corrected array element, domain or typed value'
# QA5W5B-3: in a WIN1252 database, text a WIN1252 client wrote ("“quote” € 5" in cp1252) is read as WIN1252, the
# database's own encoding; read as LATIN1 its quotes and euro sign would become C1 control characters that pass the check.
sql postgres -c "CREATE DATABASE win1252_old ENCODING 'WIN1252' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
sql win1252_old -c "CREATE TABLE notes(id text PRIMARY KEY, body text)" \
  -c "INSERT INTO notes VALUES ('n1', convert_from(decode('9371756f74659420802035', 'hex'), 'SQL_ASCII'))"
if check win1252_old; then fail 'check-utf8.sql passed cp1252 bytes'; fi
grep -qF "not valid UTF-8: table public.notes, column body, row where id = 'n1' (ctid (0,1)); no valid multibyte UTF-8 in it, so reading its bytes as WIN1252, this database's encoding, is a safe correction; starting: \\223quote\\224 \\200 5" \
  "$work/check-win1252_old.log" || fail 'check-utf8.sql did not tell the cp1252 text to be read as WIN1252'
sql win1252_old -c "UPDATE public.notes SET body = convert_from(convert(convert_to(body, 'SQL_ASCII'), 'WIN1252', 'UTF8'), 'SQL_ASCII') WHERE id = 'n1'"
check win1252_old || fail 'check-utf8.sql failed after the WIN1252 correction'
[[ $(text_hex win1252_old "SELECT body FROM notes") == "$(hex '“quote” € 5')" ]] || fail 'the WIN1252 correction'
# In a database whose encoding is multibyte (EUC_JP), no reading of the bytes is called safe: the text is to be typed.
sql postgres -c "CREATE DATABASE eucjp_old ENCODING 'EUC_JP' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
sql eucjp_old -c "CREATE TABLE notes(id text PRIMARY KEY, body text)" -c "INSERT INTO notes VALUES ('n1', convert_from(decode('a4a2', 'hex'), 'SQL_ASCII'))"
if check eucjp_old; then fail 'check-utf8.sql passed EUC-JP bytes'; fi
grep -qF "not valid UTF-8: table public.notes, column body, row where id = 'n1' (ctid (0,1)); this database's encoding, EUC_JP, is multibyte, so no reading of its bytes is a safe correction: type the text it should hold; starting: \\244\\242" \
  "$work/check-eucjp_old.log" || fail 'check-utf8.sql did not tell the EUC_JP text to be typed'
# The typed text in a LATIN1 database goes through the section's command too: after `\encoding UTF8` the server converts
# the client's UTF-8 to LATIN1, convert_to turns it back and the bytes stored are UTF-8.
sql postgres -c "CREATE DATABASE probe_latin1 ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
sql probe_latin1 -c "CREATE TABLE notes(id text PRIMARY KEY, body text)" -c "INSERT INTO notes VALUES ('n1', convert_from(decode('4372c3a86d6520ff', 'hex'), 'SQL_ASCII'))"
if check probe_latin1; then fail 'check-utf8.sql passed a LATIN1 mixed value'; fi
env OLD_DATABASE_URL="$(url probe_latin1)" PATH="$work/bin:$PATH" bash -euo pipefail -c "$type_in_cmd -X -q -v ON_ERROR_STOP=1" <<'SQL'
\encoding UTF8
UPDATE public.notes SET body = convert_from(convert_to('Crème brûlée', 'UTF8'), 'SQL_ASCII') WHERE id = 'n1';
SQL
check probe_latin1 || fail 'check-utf8.sql failed after the typed correction in LATIN1'
[[ $(text_hex probe_latin1 "SELECT body FROM notes") == "$(hex 'Crème brûlée')" ]] || fail 'the typed text in a LATIN1 database'

echo 'LATIN1: UTF-8 text stored by the old market'
old_database latin1_old LATIN1 "$title_hex"
check latin1_old || fail 'check-utf8.sql failed on a LATIN1 database holding only UTF-8'
# Taken as LATIN1 (the default, as install.sh upgrade's automatic backup takes it), its restore would double-encode
# every non-ASCII character: restore.sh refuses it before changing anything (QA5W4B-2).
backup latin1_old "$work/latin1.dump.age"
if restore "$work/latin1.dump.age" latin1_plain; then fail 'restore.sh restored a LATIN1 dump'; fi
grep -q 'Refusing: this backup was taken from a LATIN1 database and holds its text in LATIN1.' \
  "$work/restore-latin1_plain.log" || fail 'restore.sh did not explain the refusal of the LATIN1 dump'
grep -q 'see UPGRADING.md, section 42.' "$work/restore-latin1_plain.log" || fail 'the refusal does not cite section 42'
[[ $(sql latin1_plain -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'") == 0 ]] ||
  fail 'the refused restore wrote to the database'
# QA5W5B-1: with client_encoding UTF8 set for the database, its default backup is written in UTF8, converted from
# LATIN1, and its script says SET client_encoding = 'UTF8'; its DATABASE entry still records ENCODING = 'LATIN1', and
# restore.sh refuses it by that. The way out the refusal names, a temporary LATIN1 database, converts it back exactly.
sql postgres -c "ALTER DATABASE latin1_old SET client_encoding = 'UTF8'"
backup latin1_old "$work/latin1-set.dump.age"
sql postgres -c "ALTER DATABASE latin1_old RESET client_encoding"
if restore "$work/latin1-set.dump.age" latin1_set; then fail 'restore.sh restored a UTF8 dump of a LATIN1 database'; fi
grep -q 'Refusing: this backup was taken from a LATIN1 database and holds its text in UTF8.' "$work/restore-latin1_set.log" ||
  fail 'restore.sh did not name the LATIN1 database of the UTF8 dump'
grep -q 'restore it into a temporary LATIN1 database' "$work/restore-latin1_set.log" || fail 'the refusal of the UTF8 dump does not name LATIN1'
[[ $(sql latin1_set -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'") == 0 ]] ||
  fail 'the refused restore of the UTF8 dump wrote to the database'
sql postgres -c "CREATE DATABASE latin1_set_tmp ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
age -d -i "$work/identity" "$work/latin1-set.dump.age" | PATH="$work/bin:$PATH" TMP_DATABASE_URL=$(url latin1_set_tmp) \
  python3 scripts/postgres-tool.py TMP_DATABASE_URL pg_restore --dbname='' --exit-on-error --no-owner --no-acl
# This isolated encoding-conversion database inherits the original's identity;
# keep its CURRENT state, never invent a new enrollment for a restored copy.
cp -R "$work/retention-latin1_old" "$work/retention-latin1_set_tmp"
BACKUP_RECOVERY_SOURCE="$work/latin1-set.dump.age" BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_set_tmp "$work/latin1-set-again.dump.age"
restore "$work/latin1-set-again.dump.age" latin1_set_escape || fail 'restore.sh refused the backup of the temporary LATIN1 database (UTF8 dump)'
for column in "title FROM products WHERE id='utf8product'" "body FROM reviews WHERE id='utf8review'"; do
  [[ $(text_hex latin1_set_escape "SELECT $column") == "$title_hex" ]] || fail "LATIN1 dumped as UTF8, through a temporary LATIN1 database: $column"
done
# A UTF8 database whose client_encoding is SJIS is dumped in SJIS, in which no database can be created: the way out
# names the database's own encoding, UTF8.
sql postgres -c "CREATE DATABASE sjis_src ENCODING 'UTF8' TEMPLATE template0"
sql sjis_src -c "CREATE TABLE notes(id text PRIMARY KEY, body text)" -c "INSERT INTO notes VALUES ('n1', 'plain')"
sql postgres -c "ALTER DATABASE sjis_src SET client_encoding = 'SJIS'"
backup sjis_src "$work/sjis.dump.age"
if restore "$work/sjis.dump.age" sjis_new; then fail 'restore.sh restored an SJIS dump'; fi
grep -q 'Refusing: this backup was taken from a UTF8 database and holds its text in SJIS.' "$work/restore-sjis_new.log" ||
  fail 'restore.sh did not name the UTF8 database of the SJIS dump'
grep -q 'restore it into a temporary UTF8 database' "$work/restore-sjis_new.log" || fail 'the refusal of the SJIS dump does not name UTF8'
if grep -q 'temporary SJIS database' "$work/restore-sjis_new.log"; then fail 'the refusal advises an SJIS database'; fi
# Section 42's way out for an operator who has only that backup: restore it into a temporary LATIN1 database (plain
# pg_restore, which converts nothing between two LATIN1 ends), take that up as the old database (check, then back up
# with BACKUP_DUMP_ENCODING=SQL_ASCII), and restore the new backup with restore.sh. QA5W6-3: the section's shell block
# runs as printed (only the example backup's name replaced), with the URLs the section has the operator set, in the
# section's order: in step 3, before step 4 creates opsecmkt_utf8, which NEW_DATABASE_URL names.
placeholder=backups/pre-upgrade-20260101T000000Z.dump.age
escape_block=$(python3 - UPGRADING.md <<'PY'
import re, sys
text = open(sys.argv[1], encoding='utf-8').read()
start = text.index('## 42. ')
section = text[start:text.index('\n## 43. ', start)]
blocks = [b for b in re.findall(r'^ *```sh\n(.*?)^ *```', section, re.S | re.M) if 'opsecmkt_latin1' in b]
if len(blocks) != 1:
    raise SystemExit(f'UPGRADING.md section 42: {len(blocks)} shell blocks name opsecmkt_latin1, expected one')
print('\n'.join(line.strip() for line in blocks[0].splitlines()))
PY
)
[[ $escape_block == *"$placeholder"* ]] || fail "section 42's way out no longer names $placeholder"
escape_block=${escape_block//"$placeholder"/"$work/latin1.dump.age"}
[[ -z $(sql postgres -c "SELECT 1 FROM pg_database WHERE datname IN ('opsecmkt_utf8', 'opsecmkt_latin1')") ]] || fail 'opsecmkt_utf8 or opsecmkt_latin1 exists before step 3'
PATH="$work/bin:$PATH" NEW_DATABASE_URL=$(url opsecmkt_utf8) LATIN1_DATABASE_URL=$(url opsecmkt_latin1) MAINTENANCE_DATABASE_URL=$(url postgres) \
  AGE_IDENTITY="$work/identity" bash -euo pipefail -c "$escape_block" > "$work/escape.log" 2>&1 ||
  fail "section 42's way out, run as printed before step 4, failed"
check opsecmkt_latin1 || fail 'check-utf8.sql failed on the temporary LATIN1 database'
cp -R "$work/retention-latin1_old" "$work/retention-opsecmkt_latin1"
BACKUP_RECOVERY_SOURCE="$work/latin1.dump.age" BACKUP_DUMP_ENCODING=SQL_ASCII backup opsecmkt_latin1 "$work/latin1-again.dump.age"
# The encoding-conversion derivative keeps the old archive's deadline rather
# than acquiring the temporary database's new pg_dump creation time.
python3 - "$work/latin1.dump.age.retention.json" "$work/latin1-again.dump.age.retention.json" <<'PY'
import json,sys
original,derived=[json.load(open(path)) for path in sys.argv[1:]]
assert (original['created_at'],original['expires_at']) == (derived['created_at'],derived['expires_at'])
PY
# Steps 4 and 5: opsecmkt_utf8 created, the backup of the temporary database restored into it with restore.sh.
# (Step 4 as printed, through the maintenance database; restore.sh is then run into the database it made.)
env MAINTENANCE_DATABASE_URL="$(url postgres)" PATH="$work/bin:$PATH" bash -euo pipefail -c "$create_utf8_cmd" > "$work/create-utf8.log" 2>&1 ||
  fail "section 42 step 4's command failed"
[[ $(sql postgres -c "SELECT pg_encoding_to_char(encoding) FROM pg_database WHERE datname = 'opsecmkt_utf8'") == UTF8 ]] || fail 'step 4 did not create opsecmkt_utf8 as UTF8'
restore_into "$work/latin1-again.dump.age" opsecmkt_utf8 || fail 'restore.sh refused the backup of the temporary LATIN1 database'
sql postgres -c 'DROP DATABASE opsecmkt_latin1'
for column in "title FROM products WHERE id='utf8product'" "body FROM reviews WHERE id='utf8review'"; do
  [[ $(text_hex opsecmkt_utf8 "SELECT $column") == "$title_hex" ]] || fail "LATIN1 through a temporary LATIN1 database: $column"
done
# BACKUP_DUMP_ENCODING=SQL_ASCII restores it exactly, and refuses any other value.
if BACKUP_DUMP_ENCODING=LATIN1 backup latin1_old "$work/refused.dump.age"; then fail 'backup.sh took BACKUP_DUMP_ENCODING=LATIN1'; fi
grep -q 'BACKUP_DUMP_ENCODING may only be SQL_ASCII' "$work/backup.log" || fail 'backup.sh did not explain the refusal'
[[ ! -e $work/refused.dump.age ]] || fail 'backup.sh wrote a refused backup'
BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_old "$work/latin1-raw.dump.age"
restore "$work/latin1-raw.dump.age" latin1_new || fail 'restore.sh refused the SQL_ASCII dump of LATIN1'
check latin1_new NEW_DATABASE_URL || fail 'check-utf8.sql failed on the new database'
for column in "title FROM products WHERE id='utf8product'" "body FROM reviews WHERE id='utf8review'"; do
  [[ $(text_hex latin1_new "SELECT $column") == "$title_hex" ]] || fail "LATIN1 with BACKUP_DUMP_ENCODING=SQL_ASCII: $column"
done
# The same option on the new UTF8 database is a no-op on text already right: its round trip keeps every byte.
BACKUP_DUMP_ENCODING=SQL_ASCII round_trip latin1_new || fail 'the round trip of the LATIN1 move failed'
[[ $(text_hex opsecmkt_utf8_check "SELECT title FROM products WHERE id='utf8product'") == "$title_hex" ]] || fail 'BACKUP_DUMP_ENCODING=SQL_ASCII changed UTF8 text'
end_round_trip latin1_new || fail 'step 6 could not drop its scratch database'
echo 'UTF-8 migration rehearsal passed'
