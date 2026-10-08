#!/usr/bin/env bash
# A-144 deployment rehearsal. A real Tor container runs on internal-only egress; this tests deployment and
# rollback mechanics, not circuit construction, descriptor publication, or onion reachability.
set -euo pipefail
umask 077
shopt -s nullglob
[[ $# -eq 1 ]] || { echo 'Usage: scripts/test-tor-upgrade-rollback.sh v0.1.0-alpha.1' >&2; exit 2; }
from_ref=$1
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
for dependency in docker git python3 age age-keygen; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
git rev-parse --verify --quiet "${from_ref}^{commit}" >/dev/null || { echo "Unknown release ref: $from_ref" >&2; exit 1; }

work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-a144.XXXXXX")
deploy="$work/deployment"
runtime="$work/runtime"
project="a144-$(python3 -c 'import secrets; print(secrets.token_hex(5))')"
db_password=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
setup_token=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
app_port=$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)
base="http://127.0.0.1:$app_port"
seed="a144_$(python3 -c 'import secrets; print(secrets.token_hex(4))')"
old_archive="$work/old.tar"
current_archive="$work/current.tar"
cleanup_log="$work/cleanup.log"

compose() {
  (cd "$deploy" && env -i "PATH=$PATH" "HOME=${HOME:-/tmp}" "DOCKER_CONFIG=${DOCKER_CONFIG:-}" \
    "DOCKER_HOST=${DOCKER_HOST:-}" "DOCKER_CONTEXT=${DOCKER_CONTEXT:-}" docker compose --project-name "$project" \
    -f compose.yaml -f compose.tor.yaml -f compose.internal-db.yaml -f compose.a144-offline.yaml "$@")
}

restore_runtime() {
  [[ -d $runtime ]] || return 0
  if [[ -f $runtime/.env && ! -e $deploy/.env ]]; then mv "$runtime/.env" "$deploy/.env"; fi
  if [[ -d $runtime/backups && ! -e $deploy/backups ]]; then mv "$runtime/backups" "$deploy/backups"; fi
  if [[ -f $runtime/compose.a144-offline.yaml && ! -e $deploy/compose.a144-offline.yaml ]]; then
    mv "$runtime/compose.a144-offline.yaml" "$deploy/compose.a144-offline.yaml"
  fi
  for file in "$runtime"/.env.pre-upgrade-*; do
    [[ ! -f $file || -e $deploy/$(basename "$file") ]] || mv "$file" "$deploy/$(basename "$file")"
  done
}

cleanup() {
  local status=$? containers='' volumes='' networks=''
  trap - EXIT INT TERM
  if [[ -d $deploy ]]; then
    restore_runtime || status=1
    compose down --rmi local --volumes --remove-orphans >"$cleanup_log" 2>&1 || status=1
    containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    networks=$(docker network ls -q --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    if [[ -n $containers || -n $volumes || -n $networks ]]; then
      status=1
      printf 'A-144 cleanup left project resources: containers=%s volumes=%s networks=%s\n' \
        "${containers:-none}" "${volumes:-none}" "${networks:-none}" >&2
      cat "$cleanup_log" >&2 || true
    fi
  fi
  if [[ -z $containers$volumes$networks ]]; then rm -rf "$work"; else echo "Retained rehearsal files at $work" >&2; fi
  exit "$status"
}
trap cleanup EXIT INT TERM

replace_source() {
  local archive=$1 file entry
  rm -rf "$runtime"
  mkdir -p "$runtime"
  [[ ! -f $deploy/.env ]] || mv "$deploy/.env" "$runtime/.env"
  [[ ! -d $deploy/backups ]] || mv "$deploy/backups" "$runtime/backups"
  [[ ! -f $deploy/compose.a144-offline.yaml ]] || mv "$deploy/compose.a144-offline.yaml" "$runtime/compose.a144-offline.yaml"
  for file in "$deploy"/.env.pre-upgrade-*; do [[ ! -f $file ]] || mv "$file" "$runtime/$(basename "$file")"; done
  for entry in "$deploy"/* "$deploy"/.[!.]* "$deploy"/..?*; do
    [[ -e $entry || -L $entry ]] || continue
    rm -rf "$entry"
  done
  tar -xf "$archive" -C "$deploy"
  restore_runtime
}

db_sql() {
  local database=$1
  shift
  compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d "$database" "$@" </dev/null
}

wait_http() {
  python3 - "$base" <<'PY'
import sys, time, urllib.request
url = sys.argv[1] + '/healthz'
for _ in range(180):
    try:
        with urllib.request.urlopen(url, timeout=2) as response:
            if response.status == 200 and response.read() == b'ok':
                raise SystemExit(0)
    except OSError:
        pass
    time.sleep(1)
raise SystemExit('app did not become healthy on the loopback-only test port')
PY
}

mkdir -p "$deploy"
git -C "$root" archive --format=tar "$from_ref" > "$old_archive"
git -C "$root" archive --format=tar HEAD > "$current_archive"
tar -xf "$old_archive" -C "$deploy"
cat > "$deploy/compose.a144-offline.yaml" <<'YAML'
services:
  app:
    ports:
      - "127.0.0.1:${A144_APP_PORT}:8080"
  tor:
    command: ["--DisableNetwork", "1"]
networks:
  tor_egress:
    internal: true
YAML
cat > "$deploy/.env" <<EOF
COMPOSE_PROJECT_NAME=$project
COMPOSE_FILE=compose.yaml:compose.tor.yaml:compose.internal-db.yaml:compose.a144-offline.yaml
COMPOSE_PROFILES=internal-db
APP_MODE=tor
APP_PORT=$app_port
A144_APP_PORT=$app_port
COOKIE_SECURE=false
DATABASE_URL=postgres://opsecmkt:$db_password@db:5432/opsecmkt?sslmode=disable
POSTGRES_PASSWORD=$db_password
SETUP_TOKEN=$setup_token
EOF
chmod 600 "$deploy/.env"

echo "Starting alpha.1 Compose project $project with real Tor and offline test egress"
compose up -d --build --wait --wait-timeout 300 app db tor
wait_http
# The service hostname is generated locally. Tor egress is blocked and no circuit is asserted.
for _ in {1..60}; do
  if compose exec -T tor test -s /var/lib/tor/marketplace/hostname >/dev/null 2>&1; then break; fi
  sleep 1
done
compose exec -T tor test -s /var/lib/tor/marketplace/hostname

echo 'Seeding alpha.1 users, listing and order through the old HTTP forms'
A144_BASE="$base" A144_SETUP_TOKEN="$setup_token" A144_SEED="$seed" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base, seed = os.environ['A144_BASE'], os.environ['A144_SEED']
password = 'a144 disposable upgrade password'

class Client:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(self, path):
        with self.opener.open(base + path, timeout=10) as response:
            return response.read().decode()
    def post(self, form_page, path, fields):
        page = self.get(form_page)
        token = re.search(r'name="csrf"\s+value="([^"]+)"', page)
        assert token, f'{form_page} did not render its CSRF token'
        data = urllib.parse.urlencode({'csrf': token.group(1), **fields}).encode()
        with self.opener.open(base + path, data=data, timeout=10) as response:
            assert response.status in (200, 303), f'{path} returned {response.status}'
            return response.read().decode(errors='replace')

admin, vendor, buyer = Client(), Client(), Client()
admin.post('/setup', '/setup', {'token': os.environ['A144_SETUP_TOKEN'], 'handle': seed + '_admin',
                                'password': password, 'site_name': 'Disposable Tor Upgrade Rehearsal'})
vendor.post('/register', '/register', {'handle': seed + '_vendor', 'password': password})
buyer.post('/register', '/register', {'handle': seed + '_buyer', 'password': password})
PY
A144_VENDOR_ID=$(db_sql opsecmkt -c "SELECT id FROM users WHERE handle='${seed}_vendor'")
A144_BASE="$base" A144_SEED="$seed" A144_VENDOR_ID="$A144_VENDOR_ID" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base, seed = os.environ['A144_BASE'], os.environ['A144_SEED']
password = 'a144 disposable upgrade password'
class Client:
    def __init__(self): self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    def get(self, path): return self.opener.open(base + path, timeout=10).read().decode()
    def post(self, form_page, path, fields):
        token = re.search(r'name="csrf"\s+value="([^"]+)"', self.get(form_page))
        assert token, f'{form_page} did not render its CSRF token'
        data = urllib.parse.urlencode({'csrf': token.group(1), **fields}).encode()
        return self.opener.open(base + path, data=data, timeout=10).read().decode(errors='replace')
admin, vendor = Client(), Client()
admin.post('/login', '/login', {'handle': seed + '_admin', 'password': password})
admin.post('/account', '/admin', {'action': 'role', 'user_id': os.environ['A144_VENDOR_ID'], 'role': 'vendor'})
vendor.post('/login', '/login', {'handle': seed + '_vendor', 'password': password})
vendor.post('/account', '/listings', {'title': seed + ' alpha.1 item',
    'description': 'Created by the legacy deployment.', 'category': 'Hardware', 'region': 'Worldwide',
    'kind': 'physical', 'price_btc': '0.00150000', 'price_xmr': '0.250000000000', 'stock': '5'})
PY
A144_PRODUCT_ID=$(db_sql opsecmkt -c "SELECT id FROM products WHERE title='${seed} alpha.1 item'")
A144_BASE="$base" A144_SEED="$seed" A144_PRODUCT_ID="$A144_PRODUCT_ID" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base, seed = os.environ['A144_BASE'], os.environ['A144_SEED']
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
login = opener.open(base + '/login', timeout=10).read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', login)
assert csrf, 'legacy login form did not render CSRF'
data = urllib.parse.urlencode({'csrf': csrf.group(1), 'handle': seed + '_buyer',
                               'password': 'a144 disposable upgrade password'}).encode()
opener.open(base + '/login', data=data, timeout=10).read()
page = opener.open(base + '/account', timeout=10).read().decode()
token = re.search(r'name="csrf"\s+value="([^"]+)"', page)
assert token, 'legacy account page did not render CSRF'
data = urllib.parse.urlencode({'csrf': token.group(1), 'product_id': os.environ['A144_PRODUCT_ID'], 'currency': 'BTC'}).encode()
opener.open(base + '/orders', data=data, timeout=10).read()
print('alpha.1 HTTP fixtures created')
PY
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='${seed}_buyer'") == 1 ]]
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]

echo 'Swapping alpha.1 source for current source in place and running install.sh upgrade'
replace_source "$current_archive"
age-keygen -o "$work/identity" >"$work/age-keygen.log" 2>&1
chmod 600 "$work/identity"
recipient=$(age-keygen -y "$work/identity")
if ! (cd "$deploy" && AGE_RECIPIENT="$recipient" scripts/install.sh upgrade) >"$work/upgrade.log" 2>&1; then
  cat "$work/upgrade.log" >&2
  exit 1
fi
wait_http
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM schema_migrations") -gt 0 ]]
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='${seed}_buyer' AND o.state='draft'") == 1 ]]
A144_BASE="$base" A144_HANDLE="${seed}_buyer" A144_PASSWORD='a144 disposable upgrade password' A144_TITLE="${seed} alpha.1 item" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base = os.environ['A144_BASE']
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
login = opener.open(base + '/login', timeout=10).read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', login)
assert csrf, 'upgraded login form has no CSRF token'
data = urllib.parse.urlencode({'csrf': csrf.group(1), 'handle': os.environ['A144_HANDLE'],
                               'password': os.environ['A144_PASSWORD']}).encode()
opener.open(base + '/login', data=data, timeout=10).read()
orders = opener.open(base + '/orders', timeout=10).read().decode()
assert os.environ['A144_TITLE'] in orders, 'upgraded alpha.1 buyer cannot see the seeded order'
PY

backup_files=("$deploy"/backups/pre-upgrade-*.dump.age)
[[ ${#backup_files[@]} == 1 ]] || { echo "Expected one encrypted pre-upgrade backup, found ${#backup_files[@]}" >&2; exit 1; }
backup=${backup_files[0]}
[[ -n $backup && -s $backup ]]
upgraded_snapshot=$(db_sql opsecmkt -c "SELECT (SELECT count(*) FROM schema_migrations)||':'||(SELECT count(*) FROM orders)||':'||(SELECT count(*) FROM products)")

echo 'Restoring the encrypted alpha.1 backup into a new database beside the upgraded database'
if ! (cd "$deploy" && printf 'RESTORE\n' | AGE_IDENTITY="$work/identity" RESTORE_INTERNAL_DATABASE=a144_rollback \
  scripts/restore.sh "$backup") >"$work/restore.log" 2>&1; then
  cat "$work/restore.log" >&2
  exit 1
fi
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM information_schema.tables WHERE table_name='schema_migrations'") == 0 ]]
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
[[ $(db_sql opsecmkt -c "SELECT (SELECT count(*) FROM schema_migrations)||':'||(SELECT count(*) FROM orders)||':'||(SELECT count(*) FROM products)") == "$upgraded_snapshot" ]]

saved_env_files=("$deploy"/.env.pre-upgrade-*)
[[ ${#saved_env_files[@]} == 1 ]] || { echo 'Expected one saved pre-upgrade .env' >&2; exit 1; }
saved_env=${saved_env_files[0]}
python3 - "$saved_env" "$deploy/.env" <<'PY'
import os, sys
from urllib.parse import urlsplit, urlunsplit
source, target = sys.argv[1:]
lines = open(source, encoding='utf-8').read().splitlines()
matches = 0
for index, line in enumerate(lines):
    if line.startswith('DATABASE_URL='):
        matches += 1
        key, value = line.split('=', 1)
        parsed = urlsplit(value)
        if parsed.hostname != 'db' or parsed.path != '/opsecmkt':
            raise SystemExit('saved internal database URL had an unexpected host or database')
        lines[index] = key + '=' + urlunsplit((parsed.scheme, parsed.netloc, '/a144_rollback', parsed.query, parsed.fragment))
if matches != 1:
    raise SystemExit('saved alpha.1 environment did not contain exactly one database URL')
with open(target, 'w', encoding='utf-8') as stream:
    stream.write('\n'.join(lines) + '\n')
os.chmod(target, 0o600)
PY

echo 'Swapping back to alpha.1 in the same Compose directory and serving the restored database'
replace_source "$old_archive"
compose up -d --build --wait --wait-timeout 300 app
wait_http
A144_BASE="$base" A144_HANDLE="${seed}_buyer" A144_PASSWORD='a144 disposable upgrade password' A144_TITLE="${seed} alpha.1 item" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base = os.environ['A144_BASE']
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
login = opener.open(base + '/login', timeout=10).read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', login)
assert csrf, 'restored alpha.1 login form has no CSRF token'
data = urllib.parse.urlencode({'csrf': csrf.group(1), 'handle': os.environ['A144_HANDLE'],
                               'password': os.environ['A144_PASSWORD']}).encode()
opener.open(base + '/login', data=data, timeout=10).read()
orders = opener.open(base + '/orders', timeout=10).read().decode()
assert os.environ['A144_TITLE'] in orders, 'alpha.1 did not serve its restored buyer order'
PY
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
[[ $(db_sql opsecmkt -c "SELECT (SELECT count(*) FROM schema_migrations)||':'||(SELECT count(*) FROM orders)||':'||(SELECT count(*) FROM products)") == "$upgraded_snapshot" ]]
echo 'A-144 offline Tor deployment rehearsal passed; no Tor circuit was tested.'
