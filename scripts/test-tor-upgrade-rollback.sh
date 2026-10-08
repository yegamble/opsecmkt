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
alpha1_app_image_ref="$project-alpha1-app"
current_app_image_ref="$project-current-app"
db_password=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
setup_token=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
base=''
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
    for image in "$alpha1_app_image_ref" "$current_app_image_ref"; do
      if docker image inspect "$image" >/dev/null 2>&1; then docker image rm "$image" >/dev/null || status=1; fi
    done
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

# Hash every persistent table row and sequence state in public, in stable table/row order. This includes audit
# history and migration records while ignoring connection-local state. No application writes run during either
# measurement. The alpha.1 rollback server only points at the restored database, then is stopped before the
# second measurement. Equality proves the side-by-side restore did not alter upgraded data or audit history.
database_hash() {
  local database=$1
  compose exec -T db psql -X -q -A -t -F '' -v ON_ERROR_STOP=1 -U opsecmkt -d "$database" <<'SQL' |
SELECT format(
  'SELECT %L UNION ALL SELECT %L || chr(9) || row_to_json(t)::text FROM %I.%I AS t ORDER BY 1;',
  'T:' || schemaname || '.' || tablename,
  'R:' || schemaname || '.' || tablename,
  schemaname, tablename
)
FROM pg_catalog.pg_tables
WHERE schemaname = 'public'
ORDER BY schemaname, tablename
\gexec
SELECT format(
  'SELECT %L || chr(9) || COALESCE(last_value::text, ''NULL'') || chr(9) || is_called::text FROM %I.%I;',
  'S:' || schemaname || '.' || sequencename,
  schemaname, sequencename
)
FROM pg_catalog.pg_sequences
WHERE schemaname = 'public'
ORDER BY schemaname, sequencename
\gexec
SQL
  python3 -c 'import hashlib,sys; h=hashlib.sha256(); [h.update(row) for row in sys.stdin.buffer]; print(h.hexdigest())'
}

app_image_id() {
  local container
  container=$(compose ps -q app)
  [[ -n $container ]] || { echo 'The app service has no running container' >&2; return 1; }
  docker inspect --format '{{.Image}}' "$container"
}

tor_hostname() {
  compose exec -T tor cat /var/lib/tor/marketplace/hostname | tr -d '\r\n'
}

tor_identity_hash() {
  compose exec -T tor sha256sum /var/lib/tor/marketplace/hs_ed25519_secret_key | awk '{print $1}'
}

refresh_base() {
  local container bindings app_networks app_network_info app_ip network_id
  container=$(compose ps -q app)
  [[ -n $container ]] || { echo 'The app service has no running container' >&2; return 1; }
  bindings=$(docker inspect --format '{{json .HostConfig.PortBindings}}' "$container")
  [[ $bindings == null || $bindings == '{}' ]] || {
    echo 'The offline rehearsal unexpectedly published an app port on the host' >&2; return 1; }
  # Linux Docker hosts can route to a bridge container IP. Use that only in this CI rehearsal;
  # publishing the app port would weaken the internal-network isolation being tested.
  app_networks=$(docker inspect --format '{{json .NetworkSettings.Networks}}' "$container")
  app_network_info=$(compose config --format json | python3 -c '
import ipaddress, json, sys
attached = json.loads(sys.argv[1])
config = json.load(sys.stdin)
service_networks = config["services"]["app"].get("networks", {})
assert set(service_networks) == {"backend"}, "app must be attached only to the backend network"
backend = config["networks"]["backend"]
assert backend.get("internal") is True, "backend must remain internal"
expected_name = backend.get("name") or "{}_backend".format(config["name"])
assert list(attached) == [expected_name], "app runtime network must be the Compose backend network"
network = attached[expected_name]
address = ipaddress.ip_address(network["IPAddress"])
assert address.version == 4 and address.is_private and not address.is_loopback and not address.is_unspecified and not address.is_multicast
print("{}\t{}".format(address, network["NetworkID"]))
' "$app_networks")
  app_ip=${app_network_info%%$'\t'*}
  network_id=${app_network_info#*$'\t'}
  [[ -n $app_ip && -n $network_id && $network_id != "$app_network_info" ]] || {
    echo 'Could not determine the app backend network address' >&2; return 1; }
  [[ $(docker network inspect --format '{{.Internal}}' "$network_id") == true ]] || {
    echo 'The app runtime backend network is not internal' >&2; return 1; }
  base="http://$app_ip:8080"
}

wait_http() {
  echo "Waiting for app health at its internal backend address $base/healthz"
  if ! python3 - "$base" <<'PY'
import sys, time, urllib.request
url = sys.argv[1] + '/healthz'
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
last = 'no response'
for _ in range(180):
    try:
        with opener.open(url, timeout=2) as response:
            if response.status == 200 and response.read() == b'ok':
                raise SystemExit(0)
            last = f'HTTP {response.status}'
    except Exception as error:
        last = f'{type(error).__name__}: {error}'
    time.sleep(1)
raise SystemExit(f'app did not become healthy on the internal backend address ({last})')
PY
  then
    # These service logs help distinguish a host-published-port problem from an app or database startup failure.
    # Do not dump Compose config or the deployment environment, which contain credentials.
    compose logs --tail 80 app db tor >&2 || true
    return 1
  fi
}

mkdir -p "$deploy"
git -C "$root" archive --format=tar "$from_ref" > "$old_archive"
git -C "$root" archive --format=tar HEAD > "$current_archive"
tar -xf "$old_archive" -C "$deploy"
cat > "$deploy/compose.a144-offline.yaml" <<'YAML'
services:
  app:
    image: ${A144_APP_IMAGE:?A144_APP_IMAGE must name this test project's app image}
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
A144_APP_IMAGE=$alpha1_app_image_ref
APP_MODE=tor
COOKIE_SECURE=false
DATABASE_URL=postgres://opsecmkt:$db_password@db:5432/opsecmkt?sslmode=disable
POSTGRES_PASSWORD=$db_password
SETUP_TOKEN=$setup_token
EOF
chmod 600 "$deploy/.env"

echo "Starting alpha.1 Compose project $project with real Tor and offline test egress"
compose up -d --build --wait --wait-timeout 300 app db tor
refresh_base
wait_http
# The service hostname is generated locally. Tor egress is blocked and no circuit is asserted.
for _ in {1..60}; do
  if compose exec -T tor test -s /var/lib/tor/marketplace/hostname >/dev/null 2>&1; then break; fi
  sleep 1
done
compose exec -T tor test -s /var/lib/tor/marketplace/hostname
alpha1_tor_hostname=$(tor_hostname)
alpha1_tor_identity_hash=$(tor_identity_hash)
[[ $alpha1_tor_hostname == *.onion && $alpha1_tor_identity_hash =~ ^[[:xdigit:]]{64}$ ]]
alpha1_app_image_id=$(app_image_id)

echo 'Seeding alpha.1 users, listing and order through the old HTTP forms'
A144_BASE="$base" A144_SETUP_TOKEN="$setup_token" A144_SEED="$seed" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base, seed = os.environ['A144_BASE'], os.environ['A144_SEED']
password = 'a144 disposable upgrade password'

class Client:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                                  urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
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
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                                  urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
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
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                     urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
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
if ! (cd "$deploy" && AGE_RECIPIENT="$recipient" A144_APP_IMAGE="$current_app_image_ref" \
  scripts/install.sh upgrade) >"$work/upgrade.log" 2>&1; then
  cat "$work/upgrade.log" >&2
  exit 1
fi
refresh_base
wait_http
upgraded_app_image_id=$(app_image_id)
[[ $upgraded_app_image_id != "$alpha1_app_image_id" ]] || { echo 'The current app image did not change from alpha.1' >&2; exit 1; }
[[ $(tor_hostname) == "$alpha1_tor_hostname" ]] || { echo 'The Tor hostname changed during upgrade' >&2; exit 1; }
[[ $(tor_identity_hash) == "$alpha1_tor_identity_hash" ]] || { echo 'The Tor identity key changed during upgrade' >&2; exit 1; }
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM schema_migrations") -gt 0 ]]
[[ $(db_sql opsecmkt -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='${seed}_buyer' AND o.state='draft'") == 1 ]]
# Migration 010 enables CAPTCHA. Disable it as an operator would so the browser fixture can authenticate using
# the actual login form without inventing a CAPTCHA-solving step.
db_sql opsecmkt -c "UPDATE settings SET value='false' WHERE key='captcha_required'"
[[ $(db_sql opsecmkt -c "SELECT value FROM settings WHERE key='captcha_required'") == false ]]
A144_BASE="$base" A144_HANDLE="${seed}_buyer" A144_PASSWORD='a144 disposable upgrade password' A144_TITLE="${seed} alpha.1 item" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base = os.environ['A144_BASE']
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                     urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
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
echo 'Stopping app and Tor before restoring or changing DATABASE_URL'
compose stop app tor
upgraded_snapshot=$(database_hash opsecmkt)
[[ $upgraded_snapshot =~ ^[[:xdigit:]]{64}$ ]]

echo 'Restoring the encrypted alpha.1 backup into a new database beside the upgraded database'
if ! (cd "$deploy" && printf 'RESTORE\n' | AGE_IDENTITY="$work/identity" RESTORE_INTERNAL_DATABASE=a144_rollback \
  scripts/restore.sh "$backup") >"$work/restore.log" 2>&1; then
  cat "$work/restore.log" >&2
  exit 1
fi
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM information_schema.tables WHERE table_name='schema_migrations'") == 0 ]]
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
[[ $(database_hash opsecmkt) == "$upgraded_snapshot" ]]

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
compose up -d --wait --wait-timeout 300 app tor
refresh_base
wait_http
rollback_app_image_id=$(app_image_id)
[[ $rollback_app_image_id == "$alpha1_app_image_id" ]] || { echo 'Rollback did not restore the original alpha.1 app image' >&2; exit 1; }
[[ $rollback_app_image_id != "$upgraded_app_image_id" ]] || { echo 'Rollback is still serving the current app image' >&2; exit 1; }
[[ $(tor_hostname) == "$alpha1_tor_hostname" ]] || { echo 'The Tor hostname changed during rollback' >&2; exit 1; }
[[ $(tor_identity_hash) == "$alpha1_tor_identity_hash" ]] || { echo 'The Tor identity key changed during rollback' >&2; exit 1; }
A144_BASE="$base" A144_HANDLE="${seed}_buyer" A144_PASSWORD='a144 disposable upgrade password' A144_TITLE="${seed} alpha.1 item" python3 - <<'PY'
import http.cookiejar, os, re, urllib.parse, urllib.request
base = os.environ['A144_BASE']
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                     urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
login = opener.open(base + '/login', timeout=10).read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', login)
assert csrf, 'restored alpha.1 login form has no CSRF token'
data = urllib.parse.urlencode({'csrf': csrf.group(1), 'handle': os.environ['A144_HANDLE'],
                               'password': os.environ['A144_PASSWORD']}).encode()
opener.open(base + '/login', data=data, timeout=10).read()
orders = opener.open(base + '/orders', timeout=10).read().decode()
assert os.environ['A144_TITLE'] in orders, 'alpha.1 did not serve its restored buyer order'
PY
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id JOIN products p ON p.id=o.product_id WHERE u.handle='${seed}_buyer' AND p.title='${seed} alpha.1 item' AND o.currency='BTC' AND o.status='Draft — payment unavailable'") == 1 ]]
[[ $(db_sql a144_rollback -c "SELECT count(*) FROM products WHERE title='${seed} alpha.1 item'") == 1 ]]
compose stop app tor
[[ $(database_hash opsecmkt) == "$upgraded_snapshot" ]]
echo 'A-144 offline Tor deployment rehearsal passed; no Tor circuit was tested.'
