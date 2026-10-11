#!/usr/bin/env bash
# Rehearse migration from the frozen 91-migration release on an isolated local PostgreSQL test cluster.
# In CI, --disposable-cluster permits the caller's disposable PostgreSQL container's `opsecmkt` parent database.
set -euo pipefail
umask 077
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

disposable_cluster=false
if [[ ${1:-} == --disposable-cluster ]]; then
  disposable_cluster=true
  shift
fi
intermediate_ref=${1:-329c7b2bf2e8741ff284256958aad55f5454fd53}
current_binary=${2:-}
for dependency in go git python3 psql; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
: "${INTERMEDIATE_TEST_DATABASE_URL:?Set INTERMEDIATE_TEST_DATABASE_URL to a disposable local PostgreSQL test database URL}"
git rev-parse --verify --quiet "${intermediate_ref}^{commit}" >/dev/null || {
  echo 'The intermediate release commit is unavailable in this clone; fetch full history first.' >&2; exit 1;
}
git merge-base --is-ancestor "$intermediate_ref" HEAD || {
  echo 'The intermediate release commit is not an ancestor of this checkout.' >&2; exit 1;
}

# Only use a loopback PostgreSQL host. Local mode additionally requires the caller's parent database to have a
# task/test name; the script creates and removes only its own uniquely named fixture database.
python3 - "$disposable_cluster" <<'PY'
import os, sys
from urllib.parse import unquote, urlsplit
url = urlsplit(os.environ['INTERMEDIATE_TEST_DATABASE_URL'])
if url.scheme not in ('postgres', 'postgresql') or url.hostname not in ('127.0.0.1', 'localhost', '::1'):
    raise SystemExit('INTERMEDIATE_TEST_DATABASE_URL must target a loopback PostgreSQL server')
parent = unquote(url.path.lstrip('/'))
if not parent or any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_' for c in parent):
    raise SystemExit('INTERMEDIATE_TEST_DATABASE_URL must name one simple database')
if sys.argv[1] != 'true' and not (parent.startswith('completion') or parent.startswith('opsecmkt_test')):
    raise SystemExit('Local mode requires a task-owned completion* or opsecmkt_test parent database')
PY

work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-intermediate-upgrade.XXXXXX")
fixture_db="completion_intermediate_${UID}_$$_$RANDOM"
fixture_created=false
server_pid=''
log=''
password='intermediate-upgrade-disposable-password'
setup_token='intermediate-upgrade-bootstrap-token-only-0123456789'
app_port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
base="http://127.0.0.1:$app_port"

connection_url() {
  python3 - "$fixture_db" <<'PY'
import os, sys
from urllib.parse import urlsplit, urlunsplit
base = urlsplit(os.environ['INTERMEDIATE_TEST_DATABASE_URL'])
print(urlunsplit((base.scheme, base.netloc, '/' + sys.argv[1], base.query, base.fragment)))
PY
}

pg() {
  python3 "$root/scripts/postgres-tool.py" INTERMEDIATE_TEST_DATABASE_URL psql -X -q -A -t -v ON_ERROR_STOP=1 "$@"
}
pg_fixture() {
  python3 "$root/scripts/postgres-tool.py" INTERMEDIATE_FIXTURE_DATABASE_URL psql -X -q -A -t -v ON_ERROR_STOP=1 "$@"
}

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
  if [[ $fixture_created == true ]]; then
    pg -c "DROP DATABASE $fixture_db" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

start_server() {
  local name=$1 binary=$2 directory=$3 target_url=${4:-$INTERMEDIATE_FIXTURE_DATABASE_URL}
  log="$work/$name.log"
  local url_file="$work/$name.database-url" token_file="$work/$name.setup-token"
  printf '%s\n' "$target_url" > "$url_file"
  printf '%s\n' "$setup_token" > "$token_file"
  chmod 600 "$url_file" "$token_file"
  (
    cd "$directory"
    exec env -i PATH="$PATH" bash -c '
      IFS= read -r DATABASE_URL < "$1"
      IFS= read -r SETUP_TOKEN < "$2"
      export DATABASE_URL SETUP_TOKEN COOKIE_SECURE=false ADDR="$3"
      exec "$4"
    ' start-server "$url_file" "$token_file" "127.0.0.1:$app_port" "$binary"
  ) > "$log" 2>&1 &
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

old_source="$work/intermediate"
mkdir -p "$old_source"
git archive --format=tar "$intermediate_ref" | tar -x -C "$old_source"
(cd "$old_source" && GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$work/intermediate-server" ./cmd/server)
if [[ -z $current_binary ]]; then
  GOWORK=off CGO_ENABLED=0 go build -trimpath -o "$work/current-server" ./cmd/server
  current_binary="$work/current-server"
elif [[ $current_binary != /* ]]; then
  current_binary="$root/$current_binary"
fi
[[ -x $current_binary ]] || { echo 'Current server binary is not executable' >&2; exit 1; }

intermediate_expected=$(for file in "$old_source"/internal/market/migrations/*.sql; do
  name=$(basename "$file" .sql)
  printf '%d:%s\n' "$((10#${name%%_*}))" "${name#*_}"
done | paste -sd, -)
[[ ${intermediate_expected##*,} == 91:pooled_drain_blockers ]] || {
  echo "Intermediate source does not end at migration 091: $intermediate_expected" >&2; exit 1;
}

INTERMEDIATE_FIXTURE_DATABASE_URL=$(connection_url)
export INTERMEDIATE_FIXTURE_DATABASE_URL
pg -c "CREATE DATABASE $fixture_db WITH ENCODING 'UTF8' TEMPLATE template0"
fixture_created=true
export UPGRADE_INTERMEDIATE_DB="$fixture_db" UPGRADE_BASE="$base" UPGRADE_OLD_SOURCE="$old_source" UPGRADE_ROOT="$root"
echo "Starting intermediate server $intermediate_ref against a new fixture database"
start_server old "$work/intermediate-server" "$old_source"
UPGRADE_INTERMEDIATE_PASSWORD="$password" UPGRADE_SETUP_TOKEN="$setup_token" python3 <<'PY'
import http.cookiejar, os, re, subprocess, sys, urllib.parse, urllib.request
base = os.environ['UPGRADE_BASE']
password = os.environ['UPGRADE_INTERMEDIATE_PASSWORD']
tool = os.path.join(os.environ['UPGRADE_ROOT'], 'scripts', 'postgres-tool.py')

def sql(query):
    return subprocess.run([sys.executable, tool, 'INTERMEDIATE_FIXTURE_DATABASE_URL', 'psql', '-X', '-q', '-A', '-t',
                           '-v', 'ON_ERROR_STOP=1', '-c', query], check=True, capture_output=True,
                          text=True, env=os.environ).stdout.strip()

class Client:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(self, path):
        return self.opener.open(base + path).read().decode()
    def post(self, form_page, path, fields):
        csrf = re.search(r'name="csrf"\s+value="([^"]+)"', self.get(form_page))
        assert csrf, f'{form_page} has no CSRF token'
        try:
            response = self.opener.open(base + path, data=urllib.parse.urlencode({'csrf': csrf[1], **fields}).encode())
        except urllib.error.HTTPError as error:
            raise AssertionError(f'{path} returned {error.code}: {error.read().decode()[:240]}') from error
        assert response.status == 200, f'{path} returned {response.status}'
        return response.url

admin, vendor, buyer = Client(), Client(), Client()
admin.post('/setup', '/setup', {'token': os.environ['UPGRADE_SETUP_TOKEN'], 'handle': 'middle_admin',
                                'password': password, 'site_name': 'Intermediate Upgrade Market'})
# This is a disposable test market. Turn off its installed image CAPTCHA through the old admin action so the
# fixture can create accounts through the real registration forms without attempting to solve image challenges.
admin.post('/admin', '/admin/captcha', {'captcha_required': 'false'})
vendor.post('/register', '/register', {'handle': 'middle_vendor', 'password': password})
buyer.post('/register', '/register', {'handle': 'middle_buyer', 'password': password})
admin.post('/account', '/admin', {'action': 'role', 'handle': 'middle_vendor', 'role': 'vendor'})
vendor.post('/account', '/listings', {'title': 'Intermediate-era item', 'description': 'Terms from the 91-migration release.',
                                      'category': 'Hardware', 'region': 'Worldwide', 'kind': 'physical',
                                      'price_btc': '0.00150000', 'price_xmr': '0.250000000000', 'stock': '5'})
product_id = sql("SELECT id FROM products WHERE title='Intermediate-era item'")
buyer.post('/account', '/orders', {'product_id': product_id, 'currency': 'BTC'})
source = os.path.join(os.environ['UPGRADE_OLD_SOURCE'], 'internal/market/pgp_integration_test.go')
text = open(source, encoding='utf-8').read()
match = re.search(r'const gpgFixtureMessage = `(.+?)`', text, re.S)
assert match, 'intermediate server test tree has no valid encrypted message fixture'
buyer.post('/account', '/messages', {'recipient': 'middle_vendor', 'body': match.group(1)})
print('Intermediate HTTP fixture created users, listing, buyer order and valid encrypted message')
PY
stop_server

old_rows=$(pg_fixture -c "SELECT string_agg(version || ':' || name, ',' ORDER BY version) FROM schema_migrations")
[[ $old_rows == "$intermediate_expected" ]] || { echo 'Intermediate server did not apply exactly its 001–091 migrations' >&2; exit 1; }
old_timestamps=$(pg_fixture -c "SELECT string_agg(version || ':' || name || ':' || applied::text, ',' ORDER BY version) FROM schema_migrations")
old_order=$(pg_fixture -c "SELECT o.id || ':' || o.buyer_id || ':' || o.product_id || ':' || o.currency || ':' || o.amount || ':' || o.state FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='middle_buyer'")
[[ -n $old_order ]] || { echo 'Intermediate buyer order was not created' >&2; exit 1; }
[[ $(pg_fixture -c 'SELECT count(*) FROM notifications') -gt 0 && $(pg_fixture -c 'SELECT count(*) FROM messages') == 1 ]] || {
  echo 'Intermediate HTTP fixture did not create both a message and its notification' >&2; exit 1;
}

echo 'Starting current server on the same intermediate database'
start_server current "$current_binary" "$root"
stop_server
current_expected=$(for file in "$root"/internal/market/migrations/*.sql; do
  name=$(basename "$file" .sql)
  printf '%d:%s\n' "$((10#${name%%_*}))" "${name#*_}"
done | paste -sd, -)
current_rows=$(pg_fixture -c "SELECT string_agg(version || ':' || name, ',' ORDER BY version) FROM schema_migrations")
[[ $current_rows == "$current_expected" ]] || { echo 'Current server did not apply every current migration' >&2; exit 1; }
current_old_timestamps=$(pg_fixture -c "SELECT string_agg(version || ':' || name || ':' || applied::text, ',' ORDER BY version) FROM schema_migrations WHERE version<=91")
[[ $current_old_timestamps == "$old_timestamps" ]] || { echo 'A prior 001–091 applied timestamp changed during upgrade' >&2; exit 1; }
[[ $(pg_fixture -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='middle_buyer' AND o.id || ':' || o.buyer_id || ':' || o.product_id || ':' || o.currency || ':' || o.amount || ':' || o.state = '$old_order'") == 1 ]]
[[ $(pg_fixture -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='middle_buyer' AND o.listing_title IS NULL AND o.listing_description IS NULL AND o.listing_kind IS NULL") == 1 ]]
[[ $(pg_fixture -c 'SELECT count(*) FROM notifications WHERE order_id IS NOT NULL') == 0 ]]
[[ $(pg_fixture -c 'SELECT count(*) FROM messages WHERE recipient_extra_keys IS NOT NULL') == 0 ]]
applied=$(pg_fixture -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")

start_server repeat "$current_binary" "$root"
[[ $(pg_fixture -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]
UPGRADE_INTERMEDIATE_PASSWORD="$password" python3 <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base = os.environ['UPGRADE_BASE']
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
login = client.open(base + '/login').read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', login)
assert csrf, 'current login form has no CSRF token'
form = urllib.parse.urlencode({'csrf': csrf[1], 'handle': 'middle_buyer',
                               'password': os.environ['UPGRADE_INTERMEDIATE_PASSWORD']}).encode()
client.open(base + '/login', data=form).read()
orders = client.open(base + '/orders').read().decode()
assert 'Intermediate-era item' in orders, 'current buyer cannot reload the intermediate order'
print('Current server buyer login and order reload passed')
PY
stop_server
echo 'Intermediate migration upgrade passed: prior timestamps and data retained, new migrations are idempotent, historical snapshots remain unknown, and the buyer can reload the order.'
