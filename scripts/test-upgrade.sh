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
  docker rm -f "$db" >/dev/null 2>&1 || true
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
  -e POSTGRES_PASSWORD=upgrade-test-only postgres:17-alpine >/dev/null
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
echo "Upgrade from $from_ref passed: health, every migration recorded once, orders migrated to draft, one draft per buyer/product/currency, data and sign-in preserved; a database from a newer release is refused at startup."
