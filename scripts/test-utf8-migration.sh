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
# LATIN1, "Crème brûlée — 東京 🔒": restore.sh refuses a backup taken as LATIN1 (it would restore every non-ASCII
# character double-encoded) before changing anything; the section's way out for an operator with only that backup (a
# temporary LATIN1 database, backed up again with BACKUP_DUMP_ENCODING=SQL_ASCII) restores it exactly, as the option
# does from the old database, and the same option on the new UTF8 database changes nothing.
#
# Needs Docker, Go, Python 3 and age. Nothing outside the container, a temporary directory and one loopback port is
# touched.
set -euo pipefail
umask 077
unset COMPOSE_PROFILES COMPOSE_FILE
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

# The client tools backup.sh and restore.sh run (through scripts/postgres-tool.py, which passes the URL's credentials
# and database in PG* variables): the same tool inside the container, against its own server.
mkdir "$work/bin"
for tool in pg_dump pg_restore psql; do
  printf '#!/usr/bin/env bash\nexec docker exec -i -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE -e TZ %q %q -h 127.0.0.1 -p 5432 "$@"\n' \
    "$container" "$tool" > "$work/bin/$tool"
  chmod 700 "$work/bin/$tool"
done
age-keygen -o "$work/identity" 2> /dev/null
recipient=$(age-keygen -y "$work/identity")

# backup DATABASE FILE: scripts/backup.sh of DATABASE (BACKUP_DUMP_ENCODING passes through from the caller).
backup() {
  PATH="$work/bin:$PATH" BACKUP_DATABASE_URL=$(url "$1") AGE_RECIPIENT=$recipient scripts/backup.sh "$2" > "$work/backup.log" 2>&1
}
# restore FILE DATABASE: creates DATABASE (UTF8, section 42 step 4) and runs scripts/restore.sh into it; its status.
restore() {
  sql postgres -c "CREATE DATABASE $2 ENCODING 'UTF8' TEMPLATE template0"
  PATH="$work/bin:$PATH" RESTORE_DATABASE_URL=$(url "$2") AGE_IDENTITY="$work/identity" scripts/restore.sh "$1" <<< RESTORE > "$work/restore-$2.log" 2>&1
}
# check DATABASE: scripts/check-utf8.sql run by psql against DATABASE, as section 42 has the operator run it; its status.
check() {
  PATH="$work/bin:$PATH" CHECK_DATABASE_URL=$(url "$1") python3 scripts/postgres-tool.py CHECK_DATABASE_URL psql -X \
    < scripts/check-utf8.sql > "$work/check-$1.log" 2>&1
}
# round_trip DATABASE: section 42's check before the switch, a backup of DATABASE restored into a scratch database.
round_trip() {
  backup "$1" "$work/$1-check.dump.age" && restore "$work/$1-check.dump.age" "$1_check"
}
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

title='Crème brûlée — 東京 🔒'
title_hex=$(hex "$title")
bad_hex=477265617420fffe2073656c6c6572  # "Great \xff\xfe seller"
mixed_hex=4372c3a86d6520ff206272c3bb6cc3a965  # "Cr\xc3\xa8me \xff br\xc3\xbbl\xc3\xa9e": UTF-8 with a stray byte
latin1_hex=e974e9  # "\xe9t\xe9": "été" written in LATIN1
latin1_safe='no valid multibyte UTF-8 in it, so reading its bytes as LATIN1 is a safe correction'
# old_database NAME ENCODING BODY_HEX: a pre-A-481 market database: the seed's schema and rows byte for byte, then a
# vendor, a buyer, a listing titled $title, a completed order and its review whose body has the bytes BODY_HEX.
old_database() {
  sql postgres -c "CREATE DATABASE $1 ENCODING '$2' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
  docker exec "$container" sh -c "pg_dump -U utf8 --encoding=SQL_ASCII --format=custom seed | pg_restore -U utf8 --exit-on-error --no-owner -d $1"
  sql "$1" -v title="$title_hex" -v body="$3" <<'SQL'
INSERT INTO users(id,handle,password_hash,role) VALUES ('utf8buyer','utf8_buyer','x','buyer'),('utf8vendor','utf8_vendor','x','vendor');
INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
  VALUES ('utf8product','utf8vendor',convert_from(decode(:'title','hex'),'SQL_ASCII'),'Kept as written.','Hardware','Worldwide','digital',100000,1,5);
INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES ('utf8order','utf8buyer','utf8product','BTC',100000,'completed');
INSERT INTO reviews(id,order_id,product_id,buyer_id,rating,body) VALUES ('utf8review','utf8order','utf8product','utf8buyer',5,convert_from(decode(:'body','hex'),'SQL_ASCII'));
SQL
  [[ $(sql "$1" -c 'SHOW server_encoding') == "$2" ]] || fail "$1 is not $2"
}

echo 'SQL_ASCII: a review with bytes that are not UTF-8'
old_database ascii_old SQL_ASCII "$bad_hex"
if check ascii_old; then fail 'check-utf8.sql passed a SQL_ASCII database holding 0xff'; fi
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
grep -q 'COPY failed for table "reviews": ERROR:  invalid byte sequence for encoding "UTF8": 0xff' "$work/restore-ascii_new_check.log" ||
  fail 'the round trip did not name the table'
if check ascii_new; then fail 'check-utf8.sql passed a UTF8 database holding 0xff'; fi
grep -qF "not valid UTF-8: table public.reviews, column body, row where id = 'utf8review' (ctid (0,1)); its bytes cannot be read in a UTF8 database: correct it in the old database" \
  "$work/check-ascii_new.log" || fail 'check-utf8.sql did not name the review in UTF8'
# A listing whose title mixes UTF-8 with a stray byte and whose description is LATIN1: the check tells which reading is
# safe (QA5W4B-3) and names the row by its key (QA5W4B-4).
sql ascii_old -v title="$mixed_hex" -v description="$latin1_hex" <<'SQL'
INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
  VALUES ('utf8mixed','utf8vendor',convert_from(decode(:'title','hex'),'SQL_ASCII'),convert_from(decode(:'description','hex'),'SQL_ASCII'),'Hardware','Worldwide','digital',100000,1,5);
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
sql ascii_old -c "UPDATE public.products SET title = convert_from(convert_to('Crème brûlée', 'UTF8'), 'SQL_ASCII') WHERE id = 'utf8mixed'"
sql ascii_old -c "UPDATE public.products SET description = convert_from(convert(convert_to(description, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = 'utf8mixed'"
check ascii_old || fail 'check-utf8.sql failed after one round of corrections'
grep -q 'Every value in the [0-9]* text column(s) is valid UTF-8.' "$work/check-ascii_old.log" || fail 'check-utf8.sql did not report success'
BACKUP_DUMP_ENCODING=SQL_ASCII backup ascii_old "$work/ascii-fixed.dump.age"
restore "$work/ascii-fixed.dump.age" ascii_fixed || fail 'restore.sh refused the corrected SQL_ASCII backup'
check ascii_fixed || fail 'check-utf8.sql failed on the new database'
round_trip ascii_fixed || fail 'the round trip of the corrected database failed'
[[ $(text_hex ascii_fixed "SELECT title FROM products WHERE id='utf8product'") == "$title_hex" ]] || fail 'the SQL_ASCII title changed'
[[ $(text_hex ascii_fixed "SELECT body FROM reviews WHERE id='utf8review'") == "$(hex 'Great ÿþ seller')" ]] || fail 'the corrected review'
[[ $(text_hex ascii_fixed_check "SELECT body FROM reviews WHERE id='utf8review'") == "$(hex 'Great ÿþ seller')" ]] || fail 'the round trip changed the review'
[[ $(text_hex ascii_fixed "SELECT title FROM products WHERE id='utf8mixed'") == "$(hex 'Crème brûlée')" ]] || fail 'the typed title'
[[ $(text_hex ascii_fixed "SELECT description FROM products WHERE id='utf8mixed'") == "$(hex 'été')" ]] || fail 'the corrected description'
(exec env -i PATH="$PATH" DATABASE_URL="$(url ascii_fixed)" SETUP_TOKEN=utf8-migration-test-token-only-0123456789 COOKIE_SECURE=false \
  ADDR="127.0.0.1:$app_port" "$work/server") > "$work/server-ascii.log" 2>&1 &
server_pid=$!
wait_healthy || fail 'the server did not start on the new database'
stop_server

echo 'LATIN1: UTF-8 text stored by the old market'
old_database latin1_old LATIN1 "$title_hex"
check latin1_old || fail 'check-utf8.sql failed on a LATIN1 database holding only UTF-8'
# Taken as LATIN1 (the default, as install.sh upgrade's automatic backup takes it), its restore would double-encode
# every non-ASCII character: restore.sh refuses it before changing anything (QA5W4B-2).
backup latin1_old "$work/latin1.dump.age"
if restore "$work/latin1.dump.age" latin1_plain; then fail 'restore.sh restored a LATIN1 dump'; fi
grep -q 'Refusing: this backup holds its text in LATIN1, as pg_dump writes a LATIN1 database backed up without BACKUP_DUMP_ENCODING=SQL_ASCII.' \
  "$work/restore-latin1_plain.log" || fail 'restore.sh did not explain the refusal of the LATIN1 dump'
grep -q 'see UPGRADING.md, section 42.' "$work/restore-latin1_plain.log" || fail 'the refusal does not cite section 42'
[[ $(sql latin1_plain -c "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'") == 0 ]] ||
  fail 'the refused restore wrote to the database'
# Section 42's way out for an operator who has only that backup: restore it into a temporary LATIN1 database (plain
# pg_restore, which converts nothing between two LATIN1 ends), take that up as the old database (check, then back up
# with BACKUP_DUMP_ENCODING=SQL_ASCII), and restore the new backup with restore.sh.
sql postgres -c "CREATE DATABASE latin1_tmp ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
age -d -i "$work/identity" "$work/latin1.dump.age" | PATH="$work/bin:$PATH" TMP_DATABASE_URL=$(url latin1_tmp) \
  python3 scripts/postgres-tool.py TMP_DATABASE_URL pg_restore --dbname='' --exit-on-error --no-owner --no-acl
check latin1_tmp || fail 'check-utf8.sql failed on the temporary LATIN1 database'
BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_tmp "$work/latin1-again.dump.age"
restore "$work/latin1-again.dump.age" latin1_escape || fail 'restore.sh refused the backup of the temporary LATIN1 database'
for column in "title FROM products WHERE id='utf8product'" "body FROM reviews WHERE id='utf8review'"; do
  [[ $(text_hex latin1_escape "SELECT $column") == "$title_hex" ]] || fail "LATIN1 through a temporary LATIN1 database: $column"
done
# BACKUP_DUMP_ENCODING=SQL_ASCII restores it exactly, and refuses any other value.
if BACKUP_DUMP_ENCODING=LATIN1 backup latin1_old "$work/refused.dump.age"; then fail 'backup.sh took BACKUP_DUMP_ENCODING=LATIN1'; fi
grep -q 'BACKUP_DUMP_ENCODING may only be SQL_ASCII' "$work/backup.log" || fail 'backup.sh did not explain the refusal'
[[ ! -e $work/refused.dump.age ]] || fail 'backup.sh wrote a refused backup'
BACKUP_DUMP_ENCODING=SQL_ASCII backup latin1_old "$work/latin1-raw.dump.age"
restore "$work/latin1-raw.dump.age" latin1_new || fail 'restore.sh refused the SQL_ASCII dump of LATIN1'
check latin1_new || fail 'check-utf8.sql failed on the new database'
for column in "title FROM products WHERE id='utf8product'" "body FROM reviews WHERE id='utf8review'"; do
  [[ $(text_hex latin1_new "SELECT $column") == "$title_hex" ]] || fail "LATIN1 with BACKUP_DUMP_ENCODING=SQL_ASCII: $column"
done
# The same option on the new UTF8 database is a no-op on text already right: its round trip keeps every byte.
BACKUP_DUMP_ENCODING=SQL_ASCII round_trip latin1_new || fail 'the round trip of the LATIN1 move failed'
[[ $(text_hex latin1_new_check "SELECT title FROM products WHERE id='utf8product'") == "$title_hex" ]] || fail 'BACKUP_DUMP_ENCODING=SQL_ASCII changed UTF8 text'
echo 'UTF-8 migration rehearsal passed'
