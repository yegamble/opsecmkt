#!/usr/bin/env bash
# Upgrade regression: databases created by v0.1.0-alpha.1 and an intermediate migrated release upgrade under the
# current server.
#
# Starts one disposable PostgreSQL 17 container, builds and runs the alpha.1 server from its git tag and the
# 91-migration intermediate server from its frozen commit,
# populates alpha.1 through its own HTTP forms (admin, vendor, buyers, a listing, XMR order drafts with
# the old status text, an armored message), stops it, then boots the server built from this checkout against
# the same database and checks the migrated data and behaviour, then that it refuses to start on a database
# recording a migration it does not include, then performs a disposable Tor-mode deployment upgrade and
# side-by-side restore. Needs Docker, Go, Git (with the tag fetched), Python 3 and age. Only temporary deployment
# files, project-owned containers/volumes and loopback ports are used.
set -euo pipefail
umask 077
unset COMPOSE_PROFILES COMPOSE_FILE
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
source "$root/scripts/a144_source_archive.sh"
from_ref=${UPGRADE_FROM_REF:-v0.1.0-alpha.1}
intermediate_ref=${UPGRADE_INTERMEDIATE_REF:-329c7b2bf2e8741ff284256958aad55f5454fd53}
for dependency in docker go git python3 age age-keygen; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
git rev-parse --verify --quiet "${from_ref}^{commit}" >/dev/null || {
  echo "$from_ref is not in this clone; fetch it first (git fetch origin tag $from_ref, or checkout with fetch-depth: 0)" >&2
  exit 1
}
git rev-parse --verify --quiet "${intermediate_ref}^{commit}" >/dev/null || {
  echo "$intermediate_ref is not in this clone; fetch full history before running the intermediate migration upgrade" >&2
  exit 1
}
git merge-base --is-ancestor "$intermediate_ref" HEAD || {
  echo "$intermediate_ref is not an ancestor of this checkout; refusing to test an unrelated migration history" >&2
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

# launch_server NAME BINARY DIRECTORY [DATABASE_URL]: runs a server with a clean environment (nothing inherited
# from the caller's shell, so a developer's payment or signing settings cannot leak in). Secrets are read from
# private temporary files inside $work and never appear in the process argument list.
launch_server() {
  local name=$1 binary=$2 directory=$3 target_database_url=${4:-$database_url}
  log="$work/$name.log"
  local database_url_file="$work/$name.database-url" setup_token_file="$work/$name.setup-token"
  printf '%s\n' "$target_database_url" > "$database_url_file"
  printf '%s\n' "$setup_token" > "$setup_token_file"
  chmod 600 "$database_url_file" "$setup_token_file"
  (
    cd "$directory"
    exec env -i PATH="$PATH" bash -c '
      IFS= read -r DATABASE_URL < "$1"
      IFS= read -r SETUP_TOKEN < "$2"
      export DATABASE_URL SETUP_TOKEN COOKIE_SECURE=false ADDR="$3"
      exec "$4"
    ' start-server "$database_url_file" "$setup_token_file" "127.0.0.1:$app_port" "$binary"
  ) > "$log" 2>&1 &
  server_pid=$!
}

# start_server NAME BINARY DIRECTORY [DATABASE_URL]: launch and wait for /healthz.
start_server() {
  local name=$1 binary=$2 directory=$3 target_database_url=${4:-$database_url}
  launch_server "$name" "$binary" "$directory" "$target_database_url"
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
git archive --format=tar "$from_ref" | extract_a144_source - "$work/old"
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
# The immutable alpha.1 form requires both old price fields; only XMR drafts are created.
vendor.post('/account', '/listings', {'title': 'Alpha era soldering kit', 'description': 'Listed before the upgrade.', 'category': 'Hardware',
                                      'region': 'Worldwide', 'kind': 'physical', 'price_btc': '0.00150000', 'price_xmr': '0.250000000000', 'stock': '5'})
product_id = sql("SELECT id FROM products WHERE title='Alpha era soldering kit'")
first = buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'XMR'})
assert buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'XMR'}) == first, 'alpha.1 must reuse the XMR draft'
buyer2.post('/account', '/orders', {'product_id': product_id, 'currency': 'XMR'})
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
[[ $(sql -c 'SELECT count(*) FROM orders') == 2 ]]

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
if sql -c "INSERT INTO orders(id,buyer_id,product_id,currency,amount) SELECT 'duplicate-draft',buyer_id,product_id,currency,amount FROM orders WHERE currency='XMR' AND buyer_id=(SELECT id FROM users WHERE handle='alpha_buyer')" > "$work/duplicate.out" 2>&1; then
  echo 'A second draft for the same buyer, product and currency was accepted' >&2; exit 1
fi
grep -q 'orders_one_draft' "$work/duplicate.out"
# ...but only while the first is still a draft (the rule is per draft, not per order).
[[ $(sql <<'SQL'
BEGIN;
UPDATE orders SET state='cancelled' WHERE currency='XMR' AND buyer_id=(SELECT id FROM users WHERE handle='alpha_buyer');
INSERT INTO orders(id,buyer_id,product_id,currency,amount) SELECT 'replacement-draft',buyer_id,product_id,currency,amount FROM orders WHERE currency='XMR' AND buyer_id=(SELECT id FROM users WHERE handle='alpha_buyer');
SELECT count(*) FROM orders WHERE buyer_id=(SELECT id FROM users WHERE handle='alpha_buyer');
ROLLBACK;
SQL
) == 2 ]]

# Migration 108 (A-492): no pooled market exists, so both pooled tables refuse every new row.
for pooled in "INSERT INTO payouts(order_id,kind,user_id,currency,amount) SELECT id,'release',buyer_id,currency,amount FROM orders WHERE currency='XMR'" \
  "INSERT INTO payment_addresses(order_id,currency,address,provider) SELECT id,currency,'upgrade-refused-108','xmr-stagenet' FROM orders WHERE currency='XMR'"; do
  if sql -c "$pooled" > "$work/pooled-108.out" 2>&1; then echo 'A pooled row was accepted after migration 108' >&2; exit 1; fi
  grep -q 'pooled payments are removed: .* takes no new rows' "$work/pooled-108.out"
done
[[ $(sql -c 'SELECT (SELECT count(*) FROM payouts) + (SELECT count(*) FROM payment_addresses)') == 0 ]]
# Behaviour through the upgraded server: an alpha.1 password still permits browse-only
# sign-in and the migrated draft remains readable. The old keyless session must complete
# PGP onboarding before creating or reusing a draft; upgrade never grants that proof.
# (Migration 010 turns the CAPTCHA on; switch it off as an operator would, so this script can sign in.)
[[ $(sql -c "SELECT value FROM settings WHERE key='payment_intake_XMR'") == escrow ]]
sql -c "UPDATE settings SET value='false' WHERE key='captcha_required'"
sql -c "UPDATE settings SET value='false' WHERE key='payment_intake_XMR'"
export UPGRADE_XMR_ORDER
UPGRADE_XMR_ORDER=$(sql -c "SELECT o.id FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='XMR'")
python3 <<'PY'
import http.cookiejar, os, re, secrets, subprocess, urllib.error, urllib.parse, urllib.request
base, order = os.environ['UPGRADE_BASE'], os.environ['UPGRADE_XMR_ORDER']
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
checkout = client.open(base + '/checkout?' + urllib.parse.urlencode({'id': product})).read().decode()
orders_forms = re.findall(r'<form\b(?=[^>]*\baction="/orders")[^>]*>(.*?)</form>', checkout, re.S)
assert not orders_forms, 'a legacy keyless session must not receive a purchase form'
assert 'href="/account"' in checkout and 'href="/pgp"' in checkout, 'checkout must link PGP setup and verification'
account = client.open(base + '/account').read().decode()
assert 'name="pgp"' in account, 'a migrated buyer must be able to add its public key'
assert 'PGP' in client.open(base + '/pgp').read().decode(), 'a migrated buyer must be able to reach PGP verification'
# Supply only the legacy seller's missing public key as explicit fixture preparation.
# The buyer remains keyless and receives no authentication proof, so the next refusal
# isolates the buyer guard instead of stopping earlier at the seller prerequisite.
with open('tests/e2e/fixtures/pgp/recipient.pub.asc', encoding='utf-8') as fixture:
    vendor_public = fixture.read().strip().replace("'", "''")
subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t', '-v', 'ON_ERROR_STOP=1',
                '-U', 'upgrade', '-d', 'opsecmkt', '-c',
                "UPDATE users SET pgp='" + vendor_public + "' WHERE handle='alpha_vendor'"], check=True, capture_output=True, text=True)
# A forged submission cannot reuse the old draft through the newly protected mutation.
try:
    client.open(base + '/orders', data=urllib.parse.urlencode({'csrf': csrf('/account'), 'form_nonce': secrets.token_hex(16),
                                                              'product_id': product, 'currency': 'XMR'}).encode())
    raise AssertionError('a legacy keyless session was allowed to purchase')
except urllib.error.HTTPError as error:
    refusal = error.read().decode()
    assert error.code == 403 and 'This browser has not completed PGP authentication.' in refusal, (
        f'keyless purchase must require PGP authentication, got {error.code}')
print('Upgraded server: alpha.1 browse-only sign-in, preserved draft and required PGP onboarding passed')
PY
[[ $(sql -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='XMR'") == 1 ]]
[[ $(sql -c 'SELECT count(*) FROM orders') == 2 ]]
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

# Rollback rehearsal: to this server, a database that a newer release has migrated is one that records a
# migration it does not include. It must exit non-zero before serving, name that migration, and change nothing.
sql -c "INSERT INTO schema_migrations(version,name) VALUES (999,'from_a_newer_release')"
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
launch_server refused "$work/new-server" "$root"
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

# A-41: use the shared intermediate-release leg against this disposable PG17 container. The URL is passed only
# in the helper's environment; it creates a uniquely named database and never modifies opsecmkt.
INTERMEDIATE_TEST_DATABASE_URL="$database_url" scripts/test-intermediate-upgrade.sh --disposable-cluster \
  "$intermediate_ref" "$work/new-server"

# Exercise the supported deployment upgrade and side-by-side restore flow in one disposable Tor-mode Compose
# directory. Its overlay binds the app only to loopback and puts Tor on an internal network; it makes no claim
# that a Tor circuit or public onion service was established.
bash scripts/test-tor-upgrade-rollback.sh "$from_ref"
echo "Upgrade from $from_ref and intermediate $intermediate_ref passed: health, migration history, Monero drafts and encrypted messages preserved, browse-only password sign-in retained, PGP purchase authentication required, draft uniqueness verified, Monero collision notes backfilled, a database from a newer release refused, and Tor rollback rehearsed."
