#!/usr/bin/env bash
# Upgrade regression: a database created and used by v0.1.0-alpha.1 must upgrade under the current server.
#
# Starts a disposable PostgreSQL 17 container, builds and runs the alpha.1 server from its git tag once,
# populates it through alpha.1's own HTTP forms (admin, vendor, buyers, a listing, BTC/XMR order drafts with
# the old status text, an armored message), stops it, then boots the server built from this checkout against
# the same database and checks the migrated data and behaviour, then that it refuses to start on a database
# recording a migration it does not include (rollback rehearsal). Needs Docker, Go, Git (with the tag fetched)
# and Python 3. Nothing outside the container, a temporary directory and two loopback ports is touched.
set -euo pipefail
umask 077
unset COMPOSE_PROFILES COMPOSE_FILE
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
from_ref=${UPGRADE_FROM_REF:-v0.1.0-alpha.1}
for dependency in docker go git python3; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
git rev-parse --verify --quiet "${from_ref}^{commit}" >/dev/null || {
  echo "$from_ref is not in this clone; fetch it first (git fetch origin tag $from_ref, or checkout with fetch-depth: 0)" >&2
  exit 1
}
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-upgrade.XXXXXX")
db="opsecmkt-upgrade-$$-$RANDOM"
server_pid=''
log=''

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
  docker rm -f -v "$db" >/dev/null 2>&1 || true  # -v: postgres keeps its data in an anonymous VOLUME
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

sql() { docker exec -i "$db" psql -X -q -A -t -v ON_ERROR_STOP=1 -U upgrade -d opsecmkt "$@"; }
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

# start_server NAME BINARY DIRECTORY: runs a server with a clean environment (nothing inherited from the
# caller's shell, so a developer's payment or signing settings cannot leak in) and waits for /healthz.
start_server() {
  local name=$1 binary=$2 directory=$3
  log="$work/$name.log"
  (cd "$directory" && exec env -i PATH="$PATH" DATABASE_URL="$database_url" SETUP_TOKEN="$setup_token" \
    COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" "$binary") > "$log" 2>&1 &
  server_pid=$!
  python3 - "$base" "$server_pid" <<'PY'
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

echo "Building $from_ref and the current server"
mkdir -p "$work/old"
git archive --format=tar "$from_ref" | tar -x -C "$work/old"
(cd "$work/old" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$work/old-server" ./cmd/server)
GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$work/new-server" ./cmd/server

echo 'Starting PostgreSQL 17'
docker run -d --name "$db" -p 127.0.0.1::5432 -e POSTGRES_USER=upgrade -e POSTGRES_DB=opsecmkt \
  -e POSTGRES_INITDB_ARGS=--encoding=UTF8 -e POSTGRES_PASSWORD=upgrade-test-only postgres:17-alpine >/dev/null
# TCP only: the image's first-boot initialisation server listens on the Unix socket alone.
for _ in {1..90}; do
  docker exec "$db" pg_isready -q -h 127.0.0.1 -U upgrade -d opsecmkt && break
  sleep 1
done
docker exec "$db" pg_isready -q -h 127.0.0.1 -U upgrade -d opsecmkt
[[ $(sql -c 'SHOW server_version_num') == 17* ]]
pg_port=$(docker port "$db" 5432/tcp | head -n1 | sed 's/.*://')
database_url="postgres://upgrade:upgrade-test-only@127.0.0.1:$pg_port/opsecmkt?sslmode=disable"
setup_token='upgrade-test-bootstrap-token-only-0123456789'
app_port=$(free_port)
base="http://127.0.0.1:$app_port"
export UPGRADE_BASE="$base" UPGRADE_SETUP_TOKEN="$setup_token" UPGRADE_DB="$db"

echo "Populating the database through the $from_ref server"
start_server old "$work/old-server" "$work/old"
python3 <<'PY'
import http.cookiejar, os, re, subprocess, urllib.parse, urllib.request
base = os.environ['UPGRADE_BASE']
password = 'upgrade-long-disposable-password'

def sql(query):
    return subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t', '-v', 'ON_ERROR_STOP=1',
                           '-U', 'upgrade', '-d', 'opsecmkt', '-c', query], check=True, capture_output=True, text=True).stdout.strip()

class Client:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(self, path):
        return self.opener.open(base + path).read().decode()
    def post(self, form_page, path, fields):
        csrf = re.search(r'name="csrf"\s+value="([^"]+)"', self.get(form_page))
        assert csrf, f'{form_page} has no CSRF token'
        response = self.opener.open(base + path, data=urllib.parse.urlencode({'csrf': csrf[1], **fields}).encode())
        assert response.status == 200, f'{path} returned {response.status}'
        return response.url

admin = Client()
admin.post('/setup', '/setup', {'token': os.environ['UPGRADE_SETUP_TOKEN'], 'handle': 'alpha_admin', 'password': password, 'site_name': 'Alpha Upgrade Market'})
vendor, buyer, buyer2 = Client(), Client(), Client()
for client, handle in ((vendor, 'alpha_vendor'), (buyer, 'alpha_buyer'), (buyer2, 'alpha_buyer_two')):
    client.post('/register', '/register', {'handle': handle, 'password': password})
vendor_id = sql("SELECT id FROM users WHERE handle='alpha_vendor'")
admin.post('/account', '/admin', {'action': 'role', 'user_id': vendor_id, 'role': 'vendor'})
vendor.post('/account', '/listings', {'title': 'Alpha era soldering kit', 'description': 'Listed before the upgrade.', 'category': 'Hardware',
                                      'region': 'Worldwide', 'kind': 'physical', 'price_btc': '0.00150000', 'price_xmr': '0.250000000000', 'stock': '5'})
product_id = sql("SELECT id FROM products WHERE title='Alpha era soldering kit'")
first = buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'BTC'})
assert buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'BTC'}) == first, 'alpha.1 must reuse the BTC draft'
buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'XMR'})
buyer2.post('/account', '/orders', {'product_id': product_id, 'currency': 'BTC'})
armored = '-----BEGIN PGP MESSAGE-----\n\nhQEMA1pre-upgrade-message-body-kept-verbatim-0123456789abcdef\n=abcd\n-----END PGP MESSAGE-----'
buyer.post('/account', '/messages', {'recipient': 'alpha_vendor', 'body': armored})
print('alpha.1 data created over HTTP')
PY
stop_server

# The alpha.1 schema as it left it: the old status text and the three-column unique constraint.
[[ $(sql -c "SELECT count(*) FROM information_schema.tables WHERE table_name='schema_migrations'") == 0 ]]
[[ $(sql -c "SELECT string_agg(DISTINCT status, ',') FROM orders") == 'Draft — payment unavailable' ]]
[[ $(sql -c "SELECT count(*) FROM pg_constraint WHERE conname='orders_buyer_id_product_id_currency_key'") == 1 ]]
[[ $(sql -c "SELECT string_agg(handle || ':' || role, ',' ORDER BY handle) FROM users") == 'alpha_admin:admin,alpha_buyer:buyer,alpha_buyer_two:buyer,alpha_vendor:vendor' ]]
[[ $(sql -c 'SELECT count(*) FROM messages') == 1 && $(sql -c 'SELECT count(*) FROM notifications') == 1 ]]
before_orders=$(sql -c "SELECT string_agg(id || ':' || buyer_id || ':' || currency || ':' || amount, ',' ORDER BY id) FROM orders")
before_message=$(sql -c 'SELECT md5(body) FROM messages')
[[ $(sql -c 'SELECT count(*) FROM orders') == 3 ]]

echo 'Upgrading with the current server'
start_server new "$work/new-server" "$root"

# Every migration file is recorded exactly once, by version and name.
expected=$(for file in internal/market/migrations/*.sql; do
  name=$(basename "$file" .sql); printf '%d:%s\n' "$((10#${name%%_*}))" "${name#*_}"
done | paste -sd, -)
recorded=$(sql -c "SELECT string_agg(version || ':' || name, ',' ORDER BY version) FROM schema_migrations")
[[ $recorded == "$expected" ]] || { echo "schema_migrations has $recorded; migration files are $expected" >&2; exit 1; }
# Orders keep their ids, parties and amounts, move to state 'draft' and lose the old free-text status.
[[ $(sql -c "SELECT string_agg(id || ':' || buyer_id || ':' || currency || ':' || amount, ',' ORDER BY id) FROM orders") == "$before_orders" ]]
[[ $(sql -c "SELECT string_agg(DISTINCT state, ',') FROM orders") == draft ]]
[[ $(sql -c "SELECT count(*) FROM information_schema.columns WHERE table_name='orders' AND column_name='status'") == 0 ]]
[[ $(sql -c "SELECT count(*) FROM pg_constraint WHERE conname='orders_buyer_id_product_id_currency_key'") == 0 ]]
[[ $(sql -c "SELECT indexdef FROM pg_indexes WHERE indexname='orders_one_draft'") == *'UNIQUE INDEX orders_one_draft ON public.orders USING btree (buyer_id, product_id, currency) WHERE (state = '"'draft'"'::text)'* ]]
# Messages, users and the site configuration survive; new columns take their defaults.
[[ $(sql -c 'SELECT md5(body) FROM messages') == "$before_message" ]]
[[ $(sql -c "SELECT recipient_match || ':' || (encrypted IS NULL) FROM messages") == 'unknown:true' ]]
[[ $(sql -c 'SELECT count(*) FROM users') == 4 ]]
[[ $(sql -c "SELECT value FROM settings WHERE key='site_name'") == 'Alpha Upgrade Market' ]]

# One draft per (buyer, product, currency): a second draft is refused by the database...
if sql -c "INSERT INTO orders(id,buyer_id,product_id,currency,amount) SELECT 'duplicate-draft',buyer_id,product_id,currency,amount FROM orders WHERE currency='XMR'" > "$work/duplicate.out" 2>&1; then
  echo 'A second draft for the same buyer, product and currency was accepted' >&2; exit 1
fi
grep -q 'orders_one_draft' "$work/duplicate.out"
# ...but only while the first is still a draft (the rule is per draft, not per order).
[[ $(sql <<'SQL'
BEGIN;
UPDATE orders SET state='cancelled' WHERE currency='XMR';
INSERT INTO orders(id,buyer_id,product_id,currency,amount) SELECT 'replacement-draft',buyer_id,product_id,currency,amount FROM orders WHERE currency='XMR';
SELECT count(*) FROM orders WHERE currency='XMR';
ROLLBACK;
SQL
) == 2 ]]

# One settlement per order (migration 069): a release keeps its return, but a release and a refund for the same
# order are refused by the database.
[[ $(sql -c "SELECT indexdef FROM pg_indexes WHERE indexname='payouts_one_settlement'") == *"UNIQUE INDEX payouts_one_settlement ON public.payouts USING btree (order_id) WHERE (kind = ANY (ARRAY['release'::text, 'refund'::text]))"* ]]
[[ $(sql <<'SQL'
BEGIN;
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'release',buyer_id,currency,amount FROM orders WHERE currency='XMR';
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'return',buyer_id,currency,amount FROM orders WHERE currency='XMR';
SELECT count(*) FROM payouts;
ROLLBACK;
SQL
) == 2 ]]
if sql > "$work/settlement.out" 2>&1 <<'SQL'
BEGIN;
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'release',buyer_id,currency,amount FROM orders WHERE currency='XMR';
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'refund',buyer_id,currency,amount FROM orders WHERE currency='XMR';
COMMIT;
SQL
then
  echo 'A release and a refund for the same order were both accepted' >&2; exit 1
fi
grep -q 'payouts_one_settlement' "$work/settlement.out"
[[ $(sql -c 'SELECT count(*) FROM payouts') == 0 ]]
# A refund or a return per order (migration 070): a refund beside a return would pay the excess twice.
[[ $(sql -c "SELECT indexdef FROM pg_indexes WHERE indexname='payouts_refund_or_return'") == *"UNIQUE INDEX payouts_refund_or_return ON public.payouts USING btree (order_id) WHERE (kind = ANY (ARRAY['refund'::text, 'return'::text]))"* ]]
if sql > "$work/refund-return.out" 2>&1 <<'SQL'
BEGIN;
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'refund',buyer_id,currency,amount FROM orders WHERE currency='XMR';
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'return',buyer_id,currency,amount FROM orders WHERE currency='XMR';
COMMIT;
SQL
then
  echo 'A refund and a return for the same order were both accepted' >&2; exit 1
fi
grep -q 'payouts_refund_or_return' "$work/refund-return.out"
[[ $(sql -c 'SELECT count(*) FROM payouts') == 0 ]]
# A return needs its release (migration 073): a deferred constraint trigger refuses, at commit, a return with no
# release, while a release and its return inserted in one transaction are accepted in either order.
[[ $(sql -c "SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname='payouts_return_needs_release'") == 'CREATE CONSTRAINT TRIGGER payouts_return_needs_release AFTER INSERT OR DELETE OR UPDATE OF kind, order_id ON public.payouts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payouts_return_needs_release()' ]]
[[ $(sql <<'SQL'
BEGIN;
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'return',buyer_id,currency,amount FROM orders WHERE currency='XMR';
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'release',buyer_id,currency,amount FROM orders WHERE currency='XMR';
SET CONSTRAINTS payouts_return_needs_release IMMEDIATE;
SELECT count(*) FROM payouts;
ROLLBACK;
SQL
) == 2 ]]
if sql > "$work/return-release.out" 2>&1 <<'SQL'
BEGIN;
INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'return',buyer_id,currency,amount FROM orders WHERE currency='XMR';
COMMIT;
SQL
then
  echo 'A return without a release was accepted' >&2; exit 1
fi
grep -q 'has a return but no release' "$work/return-release.out"
[[ $(sql -c 'SELECT count(*) FROM payouts') == 0 ]]

# Bitcoin escrow, migration 087 (A-462): a never-built release or refund may be superseded as it stands (the check
# settlements_built and the trigger settlements_supersede_keeps), no settlement moves back to building
# (settlements_never_rebuilding), and a deposit names the refund of its own order whose too-small failure left it out
# only while it is left out (payments_too_few_refund).
for trigger in settlements_supersede_keeps settlements_never_rebuilding payments_too_few_refund; do
  [[ $(sql -c "SELECT count(*) FROM pg_trigger WHERE tgname='$trigger' AND NOT tgisinternal") == 1 ]]
done
[[ $(sql -c "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='settlements_built'") == *"'superseded'"* ]]
[[ $(sql -c "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='payments_too_few_left_out'") == 'CHECK (((escrow_too_few_by IS NULL) OR escrow_left_out))' ]]

# The escrow key layout (migration 088): every key defaults to the Bitcoin Core layout, and a per-order derivation
# suffix starting with /0 or /1 is refused by validated constraints while shape (a)'s /0/<i> is not affected.
[[ $(sql -c "SELECT column_default || ':' || is_nullable FROM information_schema.columns WHERE table_name='user_escrow_keys' AND column_name='layout'") == "'core'::text:NO" ]]
[[ $(sql -c "SELECT string_agg(conname || '=' || convalidated, ',' ORDER BY conname) FROM pg_constraint WHERE conname IN ('escrow_pubkeys_per_order_index','escrow_pubkeys_per_order_practice')") == 'escrow_pubkeys_per_order_index=true,escrow_pubkeys_per_order_practice=true' ]]
[[ $(sql -c "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='escrow_pubkeys_per_order_index'") == *"'^/[01]h?(/[^/]+){2,}\$'"* ]]
# Per-order escrows (migration 089, A-456a, QA5W3A-11): every suffix step is unhardened and below 2^31 and a per-order
# suffix is exactly /<i>/0/0 with 2 <= i < 2^31-1, the market's per-order key may have an origin with no path, and the
# sibling addresses have their table; both checks validated.
[[ $(sql -c "SELECT string_agg(conname || '=' || convalidated, ',' ORDER BY conname) FROM pg_constraint WHERE conname IN ('escrow_pubkeys_suffix_steps','escrow_pubkeys_origin_path','escrow_pubkeys_origin_path_check')") == 'escrow_pubkeys_origin_path=true,escrow_pubkeys_suffix_steps=true' ]]
[[ $(sql -c "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='escrow_pubkeys_suffix_steps'") == *'2147483646'*'2147483647'* ]]
[[ $(sql -c "SELECT to_regclass('escrow_siblings') IS NOT NULL") == t ]]

# Behaviour through the upgraded server: an alpha.1 password still signs in, the migrated draft is shown as
# a draft, and ordering the same product and currency again returns that draft instead of a second one.
# (Migration 010 turns the CAPTCHA on; switch it off as an operator would, so this script can sign in.)
sql -c "UPDATE settings SET value='false' WHERE key='captcha_required'"
export UPGRADE_BTC_ORDER
UPGRADE_BTC_ORDER=$(sql -c "SELECT o.id FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='BTC'")
python3 <<'PY'
import http.cookiejar, os, re, subprocess, urllib.parse, urllib.request
base, order = os.environ['UPGRADE_BASE'], os.environ['UPGRADE_BTC_ORDER']
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
def csrf(path):
    match = re.search(r'name="csrf"\s+value="([^"]+)"', client.open(base + path).read().decode())
    assert match, f'{path} has no CSRF token'
    return match[1]
response = client.open(base + '/login', data=urllib.parse.urlencode({'csrf': csrf('/login'), 'handle': 'alpha_buyer', 'password': 'upgrade-long-disposable-password'}).encode())
assert response.status == 200 and '/login' not in response.url, f'alpha.1 credentials rejected ({response.url})'
page = client.open(base + '/order?id=' + order).read().decode()
assert 'Draft — unfunded' in page and 'Alpha era soldering kit' in page, 'migrated order is not shown as a draft'
product = subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t', '-U', 'upgrade', '-d', 'opsecmkt', '-c',
                          'SELECT product_id FROM orders WHERE id=' + "'" + order + "'"], check=True, capture_output=True, text=True).stdout.strip()
again = client.open(base + '/orders', data=urllib.parse.urlencode({'csrf': csrf('/account'), 'product_id': product, 'currency': 'BTC'}).encode())
assert again.status == 200 and urllib.parse.parse_qs(urllib.parse.urlsplit(again.url).query).get('id') == [order], f'reorder did not return the migrated draft ({again.url})'
print('Upgraded server: alpha.1 sign-in, migrated draft page and draft reuse passed')
PY
[[ $(sql -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='BTC'") == 1 ]]
[[ $(sql -c 'SELECT count(*) FROM orders') == 3 ]]
stop_server

# A second start applies nothing new and keeps serving.
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
start_server restart "$work/new-server" "$root"
stop_server
[[ $(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]

# Migration 090 (A-480) on a database holding a refused-deposit note written before it: the database is put back to its
# pre-090 state (090 only adds xmr_escrow_collisions), a watcher note of the old fixed form is written on the XMR draft,
# and the server applies 090 again on start. The note becomes one open record with the transaction's first 20 hex
# digits (txid_truncated), the wallet, the account and the note's time; a user's note quoting the form does not.
xmr_order=$(sql -c "SELECT o.id FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='XMR'")
refused_note="TESTNET stagenet Monero escrow deposit 0123456789abcdef0123… to wallet escrow-202609-01 account 4 was not recorded: the database refused it as another order's escrow account. Moderator review required."
sql -c "DROP TABLE xmr_escrow_collisions; DELETE FROM schema_migrations WHERE version=90;
  INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created) VALUES
    ('$xmr_order','draft','draft',NULL,\$n\$$refused_note\$n\$,'2026-09-01 10:00+00'),
    ('$xmr_order','draft','draft',(SELECT buyer_id FROM orders WHERE id='$xmr_order'),\$n\$$refused_note\$n\$,'2026-09-02 10:00+00')"
start_server backfill "$work/new-server" "$root"
stop_server
[[ $(sql -c "SELECT string_agg(order_id || ' ' || wallet_name || ' ' || account || ' ' || txid || ' ' || txid_truncated || ' ' || (disposed_at IS NULL) || ' ' || to_char(noted AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI'), ',') FROM xmr_escrow_collisions") == "$xmr_order escrow-202609-01 4 0123456789abcdef0123 true true 2026-09-01 10:00" ]]
[[ $(sql -c "SELECT count(*) FROM schema_migrations WHERE version=90") == 1 ]]

# Migration 089 (A-456a, QA5W3A-11) over bad historical rows: the database is put back as 088 left it (089's table and
# checks undone, its row in schema_migrations removed) holding two escrow key rows 089 refuses: a shape-(a) buyer key
# /0/2147483648 (a step of 2^31 names a hardened key) and a market per-order key at the practice index /2147483647/0/0.
# The current server must exit non-zero before serving, with 089's message counting them, naming a suffix and
# UPGRADING.md section 44, recording and changing nothing; with the rows gone it starts and applies 089.
sql <<'SQL'
BEGIN;
DELETE FROM schema_migrations WHERE version=89;
DROP TABLE escrow_siblings;
ALTER TABLE escrow_pubkeys DROP CONSTRAINT escrow_pubkeys_suffix_steps, DROP CONSTRAINT escrow_pubkeys_origin_path;
ALTER TABLE escrow_pubkeys ADD CONSTRAINT escrow_pubkeys_origin_path_check CHECK (origin_path ~ '^(/(0|[1-9][0-9]{0,9})h?){1,10}$');
INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination)
  SELECT id,'bcrt1qupgradebad089',nextval('escrow_market_index'),'bcrt1qupgradebadrefund089',0,'' FROM orders WHERE currency='BTC' ORDER BY id LIMIT 1;
INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix)
  SELECT '02'||repeat('b8',32),e.id,'buyer',o.buyer_id,repeat('a',64),'a1b2c3d4','/48h/1h/0h/2h','/0/2147483648'
    FROM escrows e JOIN orders o ON o.id=e.order_id WHERE e.address='bcrt1qupgradebad089'
  UNION ALL SELECT '02'||repeat('b9',32),id,'market',NULL,NULL,'a1b2c3d4','/48h/1h/0h/2h','/2147483647/0/0' FROM escrows WHERE address='bcrt1qupgradebad089';
COMMIT;
SQL
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
(cd "$root" && exec env -i PATH="$PATH" DATABASE_URL="$database_url" SETUP_TOKEN="$setup_token" \
  COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" "$work/new-server") > "$work/refused-089.log" 2>&1 &
server_pid=$!
for _ in {1..60}; do
  kill -0 "$server_pid" 2>/dev/null || break
  sleep 0.5
done
if kill -0 "$server_pid" 2>/dev/null; then
  echo 'The current server kept running over an escrow key row migration 089 refuses' >&2
  exit 1
fi
refused=0
wait "$server_pid" || refused=$?
server_pid=''
((refused != 0)) || { echo 'The current server exited 0 over an escrow key row migration 089 refuses' >&2; exit 1; }
grep -q 'migration 089: 2 escrow key row(s) in escrow_pubkeys have a derivation suffix (such as /0/2147483648) with a hardened step or a step of 2^31 or more' "$work/refused-089.log"
grep -q 'See UPGRADING.md section 44' "$work/refused-089.log"
[[ $(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]
[[ $(sql -c "SELECT count(*) FROM pg_constraint WHERE conname='escrow_pubkeys_suffix_steps'") == 0 && $(sql -c "SELECT to_regclass('escrow_siblings') IS NULL") == t ]]
sql <<'SQL'
BEGIN;
DELETE FROM escrow_pubkeys WHERE derivation_suffix IN ('/0/2147483648','/2147483647/0/0');
ALTER TABLE escrows DISABLE TRIGGER escrows_not_deleted;
DELETE FROM escrows WHERE address='bcrt1qupgradebad089';
ALTER TABLE escrows ENABLE TRIGGER escrows_not_deleted;
COMMIT;
SQL
start_server after-089 "$work/new-server" "$root"
stop_server
[[ $(sql -c "SELECT count(*) FROM schema_migrations WHERE version=89 AND name='escrow_per_order'") == 1 ]]
[[ $(sql -c "SELECT string_agg(conname || '=' || convalidated, ',' ORDER BY conname) FROM pg_constraint WHERE conname IN ('escrow_pubkeys_suffix_steps','escrow_pubkeys_origin_path','escrow_pubkeys_origin_path_check')") == 'escrow_pubkeys_origin_path=true,escrow_pubkeys_suffix_steps=true' ]]

# Rollback rehearsal: to this server, a database that a newer release has migrated is one that records a
# migration it does not include. It must exit non-zero before serving, name that migration, and change nothing.
sql -c "INSERT INTO schema_migrations(version,name) VALUES (999,'from_a_newer_release')"
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
(cd "$root" && exec env -i PATH="$PATH" DATABASE_URL="$database_url" SETUP_TOKEN="$setup_token" \
  COOKIE_SECURE=false ADDR="127.0.0.1:$app_port" "$work/new-server") > "$work/refused.log" 2>&1 &
server_pid=$!
for _ in {1..60}; do
  kill -0 "$server_pid" 2>/dev/null || break
  sleep 0.5
done
if kill -0 "$server_pid" 2>/dev/null; then
  echo 'The current server kept running on a database with a migration it does not include' >&2
  exit 1
fi
refused=0
wait "$server_pid" || refused=$?
server_pid=''
((refused != 0)) || { echo 'The current server exited 0 on a database with a migration it does not include' >&2; exit 1; }
grep -q 'records migration(s) 999_from_a_newer_release, which this server does not include' "$work/refused.log"
grep -q 'UPGRADING.md, section "Rolling back"' "$work/refused.log"
[[ $(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]
# Rolling forward (the newer release's row gone again) starts normally.
sql -c 'DELETE FROM schema_migrations WHERE version=999'
start_server roll-forward "$work/new-server" "$root"
stop_server
echo "Upgrade from $from_ref passed: health, every migration recorded once, orders migrated to draft, one draft per buyer/product/currency, data and sign-in preserved; an escrow key row migration 089 refuses stops startup with its message; a database from a newer release is refused at startup."
