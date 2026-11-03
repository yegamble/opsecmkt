#!/usr/bin/env bash
# Upgrade regression: databases created by a released tag (UPGRADE_FROM_REF: v0.1.0-alpha.2 by default, or
# v0.1.0-alpha.1) and an intermediate migrated release upgrade under the current server. CI runs both tags.
#
# Starts one disposable PostgreSQL 17 container, builds and runs the old server from its git tag. From alpha.1
# (no numbered migrations) it populates through alpha.1's own HTTP forms (admin, vendor, buyers, a listing, XMR
# order drafts with the old status text, an armored message), stops it, then boots the server built from this
# checkout against the same database and checks the migrated data and behaviour, then that it refuses to start on
# a database recording a migration it does not include, then upgrades the 91-migration intermediate server from
# its frozen commit and performs a disposable Tor-mode deployment upgrade and side-by-side restore. From a later
# tag (numbered migrations) it populates through that tag's forms (admin, a vendor approved from its PGP-keyed
# application, a PGP-keyed buyer, a listing, an unfunded XMR draft, an encrypted message), then checks that the
# current server keeps that data and the recorded migrations, a restart and the same refusal; the intermediate and
# Tor legs are alpha.1's. Needs Docker, Go, Git (with the tag fetched), Python 3 and age. Only temporary deployment
# files, project-owned containers/volumes and loopback ports are used.
set -euo pipefail
umask 077
unset COMPOSE_PROFILES COMPOSE_FILE
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
source "$root/scripts/a144_source_archive.sh"
from_ref=${UPGRADE_FROM_REF:-v0.1.0-alpha.2}
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
# alpha.1 has the single schema.sql and its own forms; a later tag records numbered migrations and needs PGP keys.
if git cat-file -e "${from_ref}^{commit}:internal/market/migrations" 2>/dev/null; then
  from_layout=migrated
  git merge-base --is-ancestor "$from_ref" HEAD || {
    echo "$from_ref is not an ancestor of this checkout; refusing to test an unrelated migration history" >&2
    exit 1
  }
else
  from_layout=alpha1
fi
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
    # Expand these variables only inside the child shell.
    # shellcheck disable=SC2016
    exec env -i PATH="$PATH" bash -c '
      IFS= read -r DATABASE_URL < "$1"
      IFS= read -r SETUP_TOKEN < "$2"
      export DATABASE_URL SETUP_TOKEN COOKIE_SECURE=false ADDR="$3"
      exec "$4"
    ' start-server "$database_url_file" "$setup_token_file" "127.0.0.1:$app_port" "$binary"
  ) > "$log" 2>&1 &
  server_pid=$!
}

# check_startup NAME BINARY DIRECTORY: runs BINARY serve --check-startup (A-694) in launch_server's clean
# environment, in the foreground, with its output in $work/NAME.log; returns its exit status.
check_startup() {
  local name=$1 binary=$2 directory=$3
  local database_url_file="$work/$name.database-url" setup_token_file="$work/$name.setup-token"
  printf '%s\n' "$database_url" > "$database_url_file"
  printf '%s\n' "$setup_token" > "$setup_token_file"
  chmod 600 "$database_url_file" "$setup_token_file"
  (
    cd "$directory"
    # shellcheck disable=SC2016
    exec env -i PATH="$PATH" bash -c '
      IFS= read -r DATABASE_URL < "$1"
      IFS= read -r SETUP_TOKEN < "$2"
      export DATABASE_URL SETUP_TOKEN COOKIE_SECURE=false
      exec "$3" serve --check-startup
    ' check-startup "$database_url_file" "$setup_token_file" "$binary"
  ) > "$work/$name.log" 2>&1
}

# status_report NAME BINARY DIRECTORY: runs BINARY -status in launch_server's clean environment, with stdout in
# $work/NAME.out and stderr in $work/NAME.err; returns its exit status.
status_report() {
  local name=$1 binary=$2 directory=$3
  local database_url_file="$work/$name.database-url" setup_token_file="$work/$name.setup-token"
  printf '%s\n' "$database_url" > "$database_url_file"
  printf '%s\n' "$setup_token" > "$setup_token_file"
  chmod 600 "$database_url_file" "$setup_token_file"
  (
    cd "$directory"
    # shellcheck disable=SC2016
    exec env -i PATH="$PATH" bash -c '
      IFS= read -r DATABASE_URL < "$1"
      IFS= read -r SETUP_TOKEN < "$2"
      export DATABASE_URL SETUP_TOKEN
      exec "$3" -status
    ' status-report "$database_url_file" "$setup_token_file" "$binary"
  ) > "$work/$name.out" 2> "$work/$name.err"
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
if [[ $from_layout == alpha1 ]]; then
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
else
# A tag with numbered migrations (v0.1.0-alpha.2 and later), through its own forms. Two steps are explicit fixture
# preparation, as in the browser tests (tests/e2e/db-helpers.ts): the committed keys' private halves were discarded,
# so the administrator's second factor (A-706: staff actions need TOTP or PGP sign-in) is the pgp_2fa stand-in, and
# the buyer's decryption proof (orders and messages need one) is the session's pgp_access_key. No wallet runs: XMR
# intake is paused through the administrator's form, so the draft is unfunded and no payment is requested.
python3 <<'PY'
import hashlib, html, http.cookiejar, os, re, subprocess, urllib.error, urllib.parse, urllib.request
base = os.environ['UPGRADE_BASE']
password = 'upgrade-long-disposable-password'

def sql(query):
    return subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t', '-v', 'ON_ERROR_STOP=1',
                           '-U', 'upgrade', '-d', 'opsecmkt', '-c', query], check=True, capture_output=True, text=True).stdout.strip()

def fixture(name):
    with open('tests/e2e/fixtures/pgp/' + name, encoding='utf-8') as file:
        return file.read()

class Client:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
    def get(self, path):
        return self.opener.open(base + path).read().decode()
    # Submits the one post form for action on page (the one containing marker, if given) with its rendered hidden
    # fields (CSRF token, submission nonce, ids) and fields; returns the final URL after redirects.
    def submit(self, page, action, fields, marker=''):
        forms = [body for body in re.findall(r'<form\b(?=[^>]*\bmethod="post")(?=[^>]*\baction="' + re.escape(action) + r'")[^>]*>(.*?)</form>',
                                             self.get(page), re.S) if marker in body]
        assert len(forms) == 1, f'{page} has {len(forms)} matching {action} forms'
        hidden = {}
        for tag in re.findall(r'<input\b(?=[^>]*\btype="hidden")[^>]*>', forms[0]):
            name, value = re.search(r'\bname="([^"]+)"', tag), re.search(r'\bvalue="([^"]*)"', tag)
            hidden[name[1]] = html.unescape(value[1]) if value else ''
        try:
            response = self.opener.open(base + action, data=urllib.parse.urlencode({**hidden, **fields}).encode())
        except urllib.error.HTTPError as error:
            raise AssertionError(f'{action} answered {error.code}: {error.read().decode()[:2000]}')
        assert response.status == 200, f'{action} returned {response.status}'
        return response.url
    # The fixture stand-in for a completed PGP decryption in this browser (seedPGPAccess in tests/e2e/db-helpers.ts).
    def seed_pgp_access(self, handle):
        token_hash = hashlib.sha256(next(c.value for c in self.jar if c.name == 'session').encode()).hexdigest()
        key = bytes.fromhex(sql(f"SELECT encode(convert_to(pgp,'UTF8'),'hex') FROM users WHERE handle='{handle}'"))
        bound = sql(f"UPDATE sessions SET pgp_access_key='{hashlib.sha256(key).hexdigest()}' WHERE token_hash='{token_hash}' "
                    f"AND user_id=(SELECT id FROM users WHERE handle='{handle}') RETURNING token_hash")
        assert bound == token_hash, f'{handle} session proof fixture was not bound'

admin, vendor, buyer = Client(), Client(), Client()
assert '/admin' in admin.submit('/setup', '/setup', {'token': os.environ['UPGRADE_SETUP_TOKEN'], 'handle': 'alpha2_admin', 'password': password,
                                                     'site_name': 'Alpha Two Upgrade Market'}), 'first-admin setup did not reach /admin'
assert sql("UPDATE users SET pgp_2fa=true WHERE handle='alpha2_admin' AND role='admin' RETURNING 1") == '1'
admin.submit('/admin/payments', '/admin/payment-intake', {'enabled': 'false', 'password': password}, 'name="currency" value="XMR"')
assert sql("SELECT value FROM settings WHERE key='payment_intake_XMR'") == 'false', 'XMR intake was not paused'
# Registration asks a written question by default; the operator turns the human check off so this script can register.
admin.submit('/admin/security', '/admin/captcha', {'captcha_required': 'false', 'password': password})
assert sql("SELECT value FROM settings WHERE key='captcha_required'") == 'false', 'the human check was not turned off'
# A vendor registers with its public key, which opens a vendor application; the administrator approves it.
vendor.submit('/register', '/register', {'handle': 'alpha2_vendor', 'password': password, 'account_type': 'vendor', 'pgp': fixture('recipient.pub.asc')})
assert sql("SELECT a.state FROM vendor_applications a JOIN users u ON u.id=a.user_id WHERE u.handle='alpha2_vendor'") == 'awaiting_approval'
admin.submit('/admin/vendors', '/admin/vendor-applications', {'action': 'approve', 'note': '', 'password': password}, 'Decision for alpha2_vendor')
assert sql("SELECT role FROM users WHERE handle='alpha2_vendor'") == 'vendor', 'the vendor application was not approved'
assert '/listing-edit' in vendor.submit('/vendor-dashboard', '/listings', {
    'title': 'Alpha two soldering kit', 'description': 'Listed before the upgrade.', 'category': 'Hardware', 'region': 'Worldwide',
    'kind': 'physical', 'price_xmr': '0.250000000000', 'stock': '5'}), 'the listing was not created'
product_id = sql("SELECT id FROM products WHERE title='Alpha two soldering kit' AND review_state='approved'")
buyer.submit('/register', '/register', {'handle': 'alpha2_buyer', 'password': password, 'pgp': fixture('other.pub.asc')})
buyer.seed_pgp_access('alpha2_buyer')
first = buyer.submit('/checkout?id=' + product_id, '/orders', {'currency': 'XMR', 'quantity': '1'})
assert buyer.submit('/checkout?id=' + product_id, '/orders', {'currency': 'XMR', 'quantity': '1'}) == first, 'the XMR draft must be reused'
# A real OpenPGP message to the vendor's key (tests/e2e/fixtures/pgp/message-to-recipient.asc).
assert urllib.parse.urlsplit(buyer.submit('/messages?to=alpha2_vendor', '/messages', {'body': fixture('message-to-recipient.asc')})).path == '/messages'
print('alpha.2-layout data created over HTTP')
PY
stop_server

# The release as it left the database: its migrations recorded, the data above and its audit trail.
from_expected=$(git ls-tree --name-only "$from_ref" internal/market/migrations/ | while IFS= read -r file; do
  [[ $file == *.sql ]] || continue
  name=$(basename "$file" .sql); printf '%d:%s\n' "$((10#${name%%_*}))" "${name#*_}"
done | paste -sd, -)
[[ $(sql -c "SELECT string_agg(version || ':' || name, ',' ORDER BY version) FROM schema_migrations") == "$from_expected" ]] ||
  { echo "$from_ref did not record exactly its own migration files" >&2; exit 1; }
from_max=$(sql -c 'SELECT max(version) FROM schema_migrations')
[[ $(sql -c "SELECT string_agg(handle || ':' || role, ',' ORDER BY handle) FROM users") == 'alpha2_admin:admin,alpha2_buyer:buyer,alpha2_vendor:vendor' ]]
[[ $(sql -c "SELECT string_agg(state || ':' || currency || ':' || quantity, ',') FROM orders") == 'draft:XMR:1' ]]
[[ $(sql -c "SELECT count(*) FROM messages WHERE encrypted") == 1 && $(sql -c 'SELECT count(*) FROM products') == 1 ]]
# Each snapshot is compared again after the upgrade (concat_ws keeps a NULL from hiding the row). A starting server
# may notify staff (for example that no staff contact is set), so only the release's notifications are compared.
notified=$(sql -c 'SELECT max(created) FROM notifications')
snapshots=(
  "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations WHERE version <= $from_max"
  "SELECT string_agg(concat_ws(':', id, handle, role, md5(password_hash), md5(pgp), pgp_fingerprint, pgp_2fa, vendor_approved, created), ',' ORDER BY id) FROM users"
  "SELECT string_agg(concat_ws(':', id, user_id, state, approval, bond_piconero, decided_at), ',' ORDER BY id) FROM vendor_applications"
  "SELECT string_agg(concat_ws(':', id, vendor_id, md5(title || description), category, region, kind, xmr, stock, dispatch_days, review_state, archived), ',' ORDER BY id) FROM products"
  "SELECT string_agg(concat_ws(':', id, buyer_id, product_id, currency, amount, quantity, state, created), ',' ORDER BY id) FROM orders"
  "SELECT string_agg(concat_ws(':', id, sender_id, recipient_id, md5(body), encrypted, recipient_match, created), ',' ORDER BY id) FROM messages"
  "SELECT string_agg(concat_ws(':', id, user_id, md5(body), message_id, is_read), ',' ORDER BY id) FROM notifications WHERE created <= '$notified'"
  "SELECT string_agg(key || '=' || value, ',' ORDER BY key) FROM settings WHERE key IN ('site_name','payment_intake_XMR','listing_review_mode')"
)
before_snapshots=()
for query in "${snapshots[@]}"; do
  snapshot=$(sql -c "$query")
  [[ -n $snapshot ]] || { echo "Nothing to compare after the upgrade: $query" >&2; exit 1; }
  before_snapshots+=("$snapshot")
done
# Signed audit export stays byte-stable: no existing audit_events row is rewritten (newer rows may follow).
audit_max=$(sql -c 'SELECT max(id) FROM audit_events')
audit_query="SELECT md5(string_agg(concat_ws(':', id, user_id, action, created), ',' ORDER BY id)) FROM audit_events WHERE id <= $audit_max"
before_audit=$(sql -c "$audit_query")
fi

echo 'Upgrading with the current server'
start_server new "$work/new-server" "$root"

# Every migration file is recorded exactly once, by version and name.
expected=$(for file in internal/market/migrations/*.sql; do
  name=$(basename "$file" .sql); printf '%d:%s\n' "$((10#${name%%_*}))" "${name#*_}"
done | paste -sd, -)
recorded=$(sql -c "SELECT string_agg(version || ':' || name, ',' ORDER BY version) FROM schema_migrations")
[[ $recorded == "$expected" ]] || { echo "schema_migrations has $recorded; migration files are $expected" >&2; exit 1; }
if [[ $from_layout == alpha1 ]]; then
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

# A-742: alpha.1 has no second factor, so its administrator is counted, and stderr alone carries the count with no
# handle or secret. Migration 155 moved the seeded CAPTCHA to the new default. UPGRADING.md's summary names both.
status_report status-after-alpha1 "$work/new-server" "$root"
[[ $(cat "$work/status-after-alpha1.out") =~ ^installed=true\ schema=[0-9]+\ keyroot=setup_token$ ]] ||
  { echo "-status stdout is not its one line: $(cat "$work/status-after-alpha1.out")" >&2; exit 1; }
grep -q '^warning: 1 active staff account(s) have no second factor' "$work/status-after-alpha1.err" ||
  { echo '-status did not warn about the upgraded administrator' >&2; exit 1; }
if grep -qF -e alpha_admin -e upgrade-test-only -e "$setup_token" "$work/status-after-alpha1.err"; then
  echo '-status printed a handle, the database password or the setup token' >&2
  exit 1
fi
[[ $(sql -c "SELECT value FROM settings WHERE key='captcha_required'") == under_attack ]] ||
  { echo 'migration 155 did not move the seeded CAPTCHA to under_attack' >&2; exit 1; }

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
import hashlib, html, http.cookiejar, os, pathlib, re, secrets, subprocess, urllib.error, urllib.parse, urllib.request
base, order = os.environ['UPGRADE_BASE'], os.environ['UPGRADE_XMR_ORDER']
password = 'upgrade-long-disposable-password'
jar = http.cookiejar.CookieJar()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
def csrf(path, session=client):
    match = re.search(r'name="csrf"\s+value="([^"]+)"', session.open(base + path).read().decode())
    assert match, f'{path} has no CSRF token'
    return html.unescape(match[1])
def sign_in(session, handle):
    return session.open(base + '/login', data=urllib.parse.urlencode({
        'csrf': csrf('/login', session), 'handle': handle, 'password': password}).encode())
response = sign_in(client, 'alpha_buyer')
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
public_key = pathlib.Path('tests/e2e/fixtures/pgp/recipient.pub.asc').read_text()
def save_encryption_key(session, handle):
    account = session.open(base + '/account').read().decode()
    profiles = re.findall(r'<form\b(?=[^>]*\baction="/account")[^>]*>(.*?)</form>', account, re.S)
    assert len(profiles) == 1, f'{handle} must have one matching profile form'
    tokens = {}
    for name in ('csrf', 'form_nonce'):
        token = re.search(r'name="' + name + r'"\s+value="([^"]+)"', profiles[0])
        assert token, f'{handle} profile form must supply {name}'
        tokens[name] = html.unescape(token[1])
    saved = session.open(base + '/account', data=urllib.parse.urlencode({
        **tokens, 'pgp': public_key, 'password': password}).encode())
    assert saved.status == 200 and urllib.parse.urlsplit(saved.url).path == '/account', f'{handle} encryption key was not saved'
vendor = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
signed_vendor = sign_in(vendor, 'alpha_vendor')
assert signed_vendor.status == 200 and '/login' not in signed_vendor.url, 'alpha.1 vendor credentials rejected'
save_encryption_key(vendor, 'alpha_vendor')
save_encryption_key(client, 'alpha_buyer')
# Saving a public key alone still does not authenticate this browser.
saved_without_proof = client.open(base + '/checkout?' + urllib.parse.urlencode({'id': product})).read().decode()
assert not re.findall(r'<form\b(?=[^>]*\baction="/orders")[^>]*>', saved_without_proof), 'a saved key without session proof must not enable purchase'
# Explicit fixture preparation for the remaining draft-reuse migration check.
# Authentication regressions above retain the real keyless refusal; focused PGP
# tests exercise challenge decryption instead of this test-only database setup.
def database(statement):
    return subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t',
                           '-v', 'ON_ERROR_STOP=1', '-U', 'upgrade', '-d', 'opsecmkt', '-c', statement],
                          check=True, capture_output=True, text=True).stdout.strip()
session_token = next(cookie.value for cookie in jar if cookie.name == 'session')
session_hash = hashlib.sha256(session_token.encode()).hexdigest()
saved_key_hex = database("SELECT encode(convert_to(pgp,'UTF8'),'hex') FROM users WHERE handle='alpha_buyer'")
key_digest = hashlib.sha256(bytes.fromhex(saved_key_hex)).hexdigest()
proof_row = database("UPDATE sessions SET pgp_access_key='" + key_digest + "' WHERE token_hash='" + session_hash +
                     "' AND user_id=(SELECT id FROM users WHERE handle='alpha_buyer') RETURNING token_hash")
assert proof_row == session_hash, 'explicit buyer session proof fixture was not bound'
checkout = client.open(base + '/checkout?' + urllib.parse.urlencode({'id': product})).read().decode()
orders_forms = re.findall(r'<form\b(?=[^>]*\baction="/orders")[^>]*>(.*?)</form>', checkout, re.S)
assert len(orders_forms) == 1, 'checkout must render one matching /orders form'
csrf_match = re.search(r'name="csrf"\s+value="([^"]+)"', orders_forms[0])
nonce_match = re.search(r'name="form_nonce"\s+value="([^"]+)"', orders_forms[0])
assert csrf_match and nonce_match, 'rendered /orders form must supply its CSRF token and nonce'
again = client.open(base + '/orders', data=urllib.parse.urlencode({'csrf': html.unescape(csrf_match[1]), 'form_nonce': html.unescape(nonce_match[1]),
                                                                     'product_id': product, 'currency': 'XMR'}).encode())
assert again.status == 200 and urllib.parse.parse_qs(urllib.parse.urlsplit(again.url).query).get('id') == [order], f'reorder did not return the migrated draft ({again.url})'
print('Upgraded server: alpha.1 browse-only sign-in, required PGP onboarding, encrypted-key setup and migrated draft reuse passed')
PY
[[ $(sql -c "SELECT count(*) FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha_buyer' AND o.currency='XMR'") == 1 ]]
[[ $(sql -c 'SELECT count(*) FROM orders') == 2 ]]
else
# The release's migrations keep their recorded times (none is applied again) and its data is unchanged.
for i in "${!snapshots[@]}"; do
  [[ $(sql -c "${snapshots[i]}") == "${before_snapshots[i]}" ]] || { echo "The upgrade changed the result of: ${snapshots[i]}" >&2; exit 1; }
done
[[ $(sql -c "$audit_query") == "$before_audit" ]] || { echo 'The upgrade rewrote an existing audit_events row' >&2; exit 1; }
# -status prints its one line; the administrator has a second factor, so no staff account is counted without one.
status_report status-after-from "$work/new-server" "$root"
[[ $(cat "$work/status-after-from.out") =~ ^installed=true\ schema=[0-9]+\ keyroot=setup_token$ ]] ||
  { echo "-status stdout is not its one line: $(cat "$work/status-after-from.out")" >&2; exit 1; }
if grep -q 'have no second factor' "$work/status-after-from.err" ||
  grep -qF -e alpha2_admin -e upgrade-test-only -e "$setup_token" "$work/status-after-from.err"; then
  echo '-status counted the administrator without a factor, or printed a handle, the database password or the setup token' >&2
  exit 1
fi
# Behaviour through the upgraded server: the release's passwords sign in, the draft is shown and reused (the buyer's
# new session gets the same decryption-proof stand-in), and the vendor receives the buyer's message.
python3 <<'PY'
import hashlib, html, http.cookiejar, os, re, subprocess, urllib.error, urllib.parse, urllib.request
base = os.environ['UPGRADE_BASE']
password = 'upgrade-long-disposable-password'

def sql(query):
    return subprocess.run(['docker', 'exec', '-i', os.environ['UPGRADE_DB'], 'psql', '-X', '-q', '-A', '-t', '-v', 'ON_ERROR_STOP=1',
                           '-U', 'upgrade', '-d', 'opsecmkt', '-c', query], check=True, capture_output=True, text=True).stdout.strip()

def signed_in(handle):
    jar = http.cookiejar.CookieJar()
    client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    csrf = re.search(r'name="csrf"\s+value="([^"]+)"', client.open(base + '/login').read().decode())
    assert csrf, '/login has no CSRF token'
    response = client.open(base + '/login', data=urllib.parse.urlencode({'csrf': html.unescape(csrf[1]), 'handle': handle, 'password': password}).encode())
    assert response.status == 200 and '/login' not in response.url, f'{handle} credentials rejected after the upgrade ({response.url})'
    return client, jar

order, product = sql("SELECT o.id || ' ' || o.product_id FROM orders o JOIN users u ON u.id=o.buyer_id WHERE u.handle='alpha2_buyer'").split()
buyer, jar = signed_in('alpha2_buyer')
page = buyer.open(base + '/order?id=' + order).read().decode()
assert 'Draft — unfunded' in page and 'Alpha two soldering kit' in page, 'the draft is not shown after the upgrade'
token_hash = hashlib.sha256(next(c.value for c in jar if c.name == 'session').encode()).hexdigest()
key = bytes.fromhex(sql("SELECT encode(convert_to(pgp,'UTF8'),'hex') FROM users WHERE handle='alpha2_buyer'"))
assert sql(f"UPDATE sessions SET pgp_access_key='{hashlib.sha256(key).hexdigest()}' WHERE token_hash='{token_hash}' RETURNING 1") == '1'
checkout = buyer.open(base + '/checkout?' + urllib.parse.urlencode({'id': product})).read().decode()
forms = re.findall(r'<form\b(?=[^>]*\baction="/orders")[^>]*>(.*?)</form>', checkout, re.S)
assert len(forms) == 1, 'checkout must render one /orders form'
fields = {name: html.unescape(re.search(r'name="' + name + r'"\s+value="([^"]+)"', forms[0])[1]) for name in ('csrf', 'form_nonce', 'product_id')}
again = buyer.open(base + '/orders', data=urllib.parse.urlencode({**fields, 'currency': 'XMR', 'quantity': '1'}).encode())
assert urllib.parse.parse_qs(urllib.parse.urlsplit(again.url).query).get('id') == [order], f'reorder did not return the draft ({again.url})'
vendor, _ = signed_in('alpha2_vendor')
assert 'alpha2_buyer' in vendor.open(base + '/messages').read().decode(), "the vendor's inbox lost the buyer's message"
print('Upgraded server: sign-in, the unfunded draft, its reuse and the encrypted message passed')
PY
[[ $(sql -c 'SELECT count(*) FROM orders') == 1 ]]
fi
stop_server

# A second start applies nothing new and keeps serving.
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
start_server restart "$work/new-server" "$root"
stop_server
[[ $(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]

# The 090 backfill and intermediate rehearsals do not depend on the starting tag, and the Tor rehearsal seeds and
# restores alpha.1 (scripts/test-tor-upgrade-rollback.sh), so only the alpha.1 leg runs them.
if [[ $from_layout == alpha1 ]]; then
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
fi

# Rollback rehearsal: to this server, a database that a newer release has migrated is one that records a
# migration it does not include. It must exit non-zero before serving, name that migration, and change nothing.
sql -c "INSERT INTO schema_migrations(version,name) VALUES (999,'from_a_newer_release')"
applied=$(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations")
# The startup check (A-694), which production deploy commands print because the app keeps no container logs, gives
# the same refusal without serving: exit 1, the migration and the rollback section named, no URL or token printed.
checked=0
check_startup check-refused "$work/new-server" "$root" || checked=$?
((checked == 1)) || { echo "serve --check-startup exited $checked on a database with a migration it does not include" >&2; exit 1; }
grep -q 'startup check: refused: database migration failed: the database was upgraded by a newer release: it records migration(s) 999_from_a_newer_release, which this server does not include' "$work/check-refused.log"
grep -q 'UPGRADING.md, section "Rolling back"' "$work/check-refused.log"
if grep -qF -e upgrade-test-only -e "$setup_token" "$work/check-refused.log"; then
  echo 'serve --check-startup printed the database password or the setup token' >&2
  exit 1
fi
[[ $(sql -c "SELECT string_agg(version || '@' || applied, ',' ORDER BY version) FROM schema_migrations") == "$applied" ]]
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
check_startup check-current "$work/new-server" "$root"
grep -q 'startup check: passed; nothing was changed' "$work/check-current.log"
start_server roll-forward "$work/new-server" "$root"
stop_server

if [[ $from_layout == alpha1 ]]; then
# A-41: use the shared intermediate-release leg against this disposable PG17 container. The URL is passed only
# in the helper's environment; it creates a uniquely named database and never modifies opsecmkt.
INTERMEDIATE_TEST_DATABASE_URL="$database_url" scripts/test-intermediate-upgrade.sh --disposable-cluster \
  "$intermediate_ref" "$work/new-server"

# Exercise the supported deployment upgrade and side-by-side restore flow in one disposable Tor-mode Compose
# directory. Its overlay binds the app only to loopback and puts Tor on an internal network; it makes no claim
# that a Tor circuit or public onion service was established.
bash scripts/test-tor-upgrade-rollback.sh "$from_ref"
echo "Upgrade from $from_ref and intermediate $intermediate_ref passed: health, migration history, Monero drafts and encrypted messages preserved, browse-only password sign-in retained, PGP purchase authentication required, draft uniqueness verified, Monero collision notes backfilled, a database from a newer release refused, and Tor rollback rehearsed."
else
echo "Upgrade from $from_ref passed: health, its recorded migrations, users, vendor application, listing, Monero draft, encrypted message, settings and audit trail preserved, password sign-in and draft reuse retained, a restart applied nothing, and a database from a newer release refused."
fi
