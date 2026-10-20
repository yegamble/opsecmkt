#!/usr/bin/env bash
# Exercises the built production image; uses only disposable CI containers/volumes.
# With `scanner` (war room A-192) it runs the malware scanner leg instead: the same app and database plus the
# pinned clamd of compose.scanner.yaml, whose only signature is deploy/testdata/clamav-test.ndb (see scanner_leg).
set -euo pipefail
unset COMPOSE_PROFILES COMPOSE_FILE APP_SECRET_KEY CLAMD_ADDRESS SCANNER_MAX_SIGNATURE_AGE_DAYS
mode=${1:?usage: ci-smoke.sh internal|external [scanner]}
leg=${2:-}
[[ "$mode" == internal || "$mode" == external ]] || exit 2
[[ -z "$leg" || "$leg" == scanner ]] || exit 2
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
work=$(mktemp -d)
project="opsecmkt-ci-${mode}${leg:+-$leg}-$$"
network="${project}-backend"
export DATABASE_URL='postgres://opsecmkt:ci-runtime-only@db:5432/opsecmkt?sslmode=disable'
export POSTGRES_PASSWORD='ci-runtime-only'
export SETUP_TOKEN='ci-container-bootstrap-token-only-123456789'
export COOKIE_SECURE=false APP_PORT=18090
# Signed audit export on, with a fixed disposable seed whose public key is pinned below.
export AUDIT_SIGNING_KEY='c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c'
export SMOKE_AUDIT_PUBLIC_KEY='1678b99a3a0e29e1bd91e440816593702f6f0824cfa20e68ae70c02e15f7361c'
# Fresh escrow-only startup with no payment RPC configuration. Legacy pooled URLs are refused separately.
unset BITCOIN_RPC_URL BITCOIN_WALLET MONERO_RPC_URL MONERO_WALLET_RPC_URL MONERO_NETWORK ESCROW_BTC ESCROW_XMR
cat > "$work/compose.yaml" <<YAML
services:
  app:
    image: opsecmkt:ci
networks:
  backend:
    external: true
    name: $network
YAML
: > "$work/env"
compose=(docker compose --env-file "$work/env" -p "$project" -f compose.yaml -f compose.clearnet.yaml -f "$work/compose.yaml")
if [[ "$mode" == internal ]]; then
  compose+=(-f compose.internal-db.yaml --profile internal-db)
fi
if [[ "$leg" == scanner ]]; then
  # clamd exactly as compose.scanner.yaml defines it, except that its signature directory is a read-only bind
  # of the one test database scanner_prepare builds (the image's own signatures are hidden by the mount).
  export CLAMD_ADDRESS=clamd:3310
  mkdir "$work/clamav"
  cat > "$work/scanner.yaml" <<YAML
services:
  clamd:
    volumes:
      - type: bind
        source: $work/clamav
        target: /var/lib/clamav
        read_only: true
YAML
  compose+=(-f compose.scanner.yaml -f "$work/scanner.yaml" --profile scanner)
fi
cleanup() {
  status=$?
  if ((status)); then "${compose[@]}" logs --no-color || true; fi
  "${compose[@]}" down --volumes --remove-orphans || true
  if [[ "$mode" == external ]]; then docker rm -f -v "${project}-db" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# The scanner leg's only signature database. deploy/testdata/clamav-test.ndb (text: one signature matching the
# test marker the Go and browser tests' fake clamd also knows) is packed inside the pinned image into an
# unsigned daily.cud built now. clamd reports that build time in VERSION ("ClamAV x.y.z/1/<now>"), so the
# app's 30-day signature age gate passes on any calendar day with the default SCANNER_MAX_SIGNATURE_AGE_DAYS.
# This proves the upload path against real clamd, not detection quality: no production signature is loaded.
scanner_prepare() {
  local image
  image=$("${compose[@]}" config --format json | python3 -c 'import json, sys; print(json.load(sys.stdin)["services"]["clamd"]["image"])')
  docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null
  # sigtool --build packs every daily.* file of its working directory, needs a COPYING file and reads the
  # builder name from standard input; nothing is signed and nothing leaves the container but daily.cud.
  docker run --rm -i --network none --read-only --tmpfs /build --user clamav --cap-drop ALL \
    --security-opt no-new-privileges:true --entrypoint bash "$image" -c '
      set -e
      cd /build
      cat > daily.ndb
      echo "OPSEC Market CI test signature; not for scanning anything else." > COPYING
      # ClamAV 1.5.4 retries a 00-prefixed MD5 once, reading the builder again with scanf.
      # Two whitespace-separated answers cover both attempts without a producer process.
      sigtool --build=daily.cud --unsigned --cvd-version=1 --datadir=/build <<< "ci ci" >&2
      sigtool --info daily.cud >&2
      cat daily.cud' < deploy/testdata/clamav-test.ndb > "$work/clamav/daily.cud"
  chmod 0755 "$work/clamav"
  chmod 0644 "$work/clamav/daily.cud"
  echo "scanner: $image with deploy/testdata/clamav-test.ndb as its only signature database (an unsigned daily.cud)"
}

# The scanner leg (war room A-192, rulings C1, C2, C4 and C6), driven the way a browser without JavaScript
# drives it: curl with a cookie jar, the page's CSRF token, an Origin header, and multipart bodies with csrf as
# the first part, like the listing's Photos form.
scanner_leg() {
  local base=http://127.0.0.1:18090 jar="$work/cookies" page="$work/page.html" status location csrf listing listing_nonce image reply
  local planted=CI-FIXTURE-TEXT-CHUNK marker=OPSECMKT-CLAMD-TEST-MARKER re
  get() { local path=$1; shift; curl -sS -o "$page" -w '%{http_code}' -b "$jar" -c "$jar" "$@" "$base$path"; }
  post() { local path=$1; shift; curl -sS -o "$page" -w '%{http_code} %{redirect_url}\n' -b "$jar" -c "$jar" -H "Origin: $base" "$@" "$base$path"; }
  page_csrf() { grep -o 'name="csrf" value="[0-9a-f]*"' "$page" | sed -n '1s/.*value="\([0-9a-f]*\)"/\1/p'; }
  rows() {
    local sql='SELECT count(*) FROM images'
    if [[ "$mode" == internal ]]; then "${compose[@]}" exec -T db psql -U opsecmkt -d opsecmkt -tAc "$sql"
    else docker exec "${project}-db" psql -U opsecmkt -d opsecmkt -tAc "$sql"; fi
  }
  # upload FILE: the listing's Photos form as the browser sends it (csrf, id, description, photo, in order).
  upload() {
    curl -sS -o "$page" -w '%{http_code}' -b "$jar" -c "$jar" -H "Origin: $base" \
      --form-string "csrf=$csrf" --form-string "id=$listing" --form-string 'description=' \
      -F "photo=@$1;type=image/png;filename=IMG_0001.png" "$base/listings/photo"
  }
  # wait_ready: the listing's Photos form is offered (the app's scanner poll, every 30 s, found clamd Ready).
  wait_ready() {
    local started=$SECONDS
    until [[ $(get "/listing-edit?id=$listing") == 200 ]] && grep -q 'action="/listings/photo"' "$page" &&
      ! grep -q 'Photo uploads are paused' "$page"; do
      ((SECONDS - started < 180)) || fail "uploads still paused after 180 s: $(grep -o 'Photo uploads are [^<]*' "$page" || true)"
      sleep 2
    done
    echo "scanner: uploads offered after $((SECONDS - started)) s"
  }
  # A PNG with a tEXt chunk (which re-encoding must drop): clean.png carries planted text, infected.png the marker.
  python3 - "$work" "$planted" "$marker" <<'PY'
import struct, sys, zlib
def chunk(kind, body):
    return struct.pack('>I', len(body)) + kind + body + struct.pack('>I', zlib.crc32(kind + body))
def png(text):
    rows = b''.join(b'\0' + bytes(v for x in range(96) for v in ((x * 4) & 255, (y * 5) & 255, 160)) for y in range(72))
    return (b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 96, 72, 8, 2, 0, 0, 0))
            + chunk(b'tEXt', b'Comment\0' + text.encode()) + chunk(b'IDAT', zlib.compress(rows)) + chunk(b'IEND', b''))
work, planted, marker = sys.argv[1:]
open(work + '/clean.png', 'wb').write(png(planted))
open(work + '/infected.png', 'wb').write(png(marker))
PY

  # Bootstrap the administrator (vendors and administrators add listing photos) and publish a listing.
  [[ $(get /setup) == 200 ]] || fail '/setup did not answer 200'
  csrf=$(page_csrf) || fail '/setup has no CSRF token'
  read -r status location < <(post /setup --data-urlencode "csrf=$csrf" --data-urlencode "token=$SETUP_TOKEN" \
    --data-urlencode handle=ci_admin --data-urlencode password=ci-long-disposable-password --data-urlencode 'site_name=CI Scanner Market')
  [[ $status == 303 ]] || fail "setup answered $status"
  [[ $(get /vendor-dashboard) == 200 ]] || fail 'the administrator has no vendor desk after setup'
  csrf=$(page_csrf) || fail '/vendor-dashboard has no CSRF token'
  listing_nonce=$(python3 - "$page" <<'PY_NONCE'
from html.parser import HTMLParser
from pathlib import Path
import sys

class ListingNonce(HTMLParser):
    def __init__(self):
        super().__init__()
        self.in_listings_form = False
        self.forms = 0
        self.nonces = []
    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == 'form':
            self.in_listings_form = attrs.get('action') == '/listings'
            if self.in_listings_form:
                self.forms += 1
        elif self.in_listings_form and tag == 'input' and attrs.get('name') == 'form_nonce':
            self.nonces.append(attrs.get('value', ''))
    def handle_endtag(self, tag):
        if tag == 'form':
            self.in_listings_form = False

parser = ListingNonce()
parser.feed(Path(sys.argv[1]).read_text())
if parser.forms != 1 or len(parser.nonces) != 1 or not parser.nonces[0]:
    raise SystemExit('expected exactly one /listings form with one rendered submission nonce')
print(parser.nonces[0])
PY_NONCE
  ) || fail '/vendor-dashboard /listings form has no submission nonce'
  read -r status location < <(post /listings --data-urlencode "csrf=$csrf" --data-urlencode "form_nonce=$listing_nonce" --data-urlencode 'title=CI scanner listing' \
    --data-urlencode 'description=Listing for the malware scanner leg.' -d category=Hardware -d region=Worldwide -d kind=physical \
    -d stock=1 -d price_btc=0.001 -d price_xmr=0.5)
  re='/listing-edit\?id=([0-9a-f]{64})#photos$'
  [[ $status == 303 && $location =~ $re ]] || fail "publishing the listing answered $status $location"
  listing=${BASH_REMATCH[1]}
  wait_ready
  csrf=$(page_csrf) || fail 'the listing edit page has no CSRF token'

  # Only the test database is loaded: the image's own signatures would find the EICAR test string (assembled
  # here, so this file never contains it), sent over INSTREAM inside the clamd container.
  reply=$(python3 - <<'PY' | "${compose[@]}" exec -T clamd nc 127.0.0.1 3310 | tr -d '\0'
import struct, sys
data = b'X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR' + b'-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*'
sys.stdout.buffer.write(b'zINSTREAM\0' + struct.pack('>I', len(data)) + data + b'\0\0\0\0')
PY
)
  [[ $reply == 'stream: OK' ]] || fail "EICAR answered '$reply': clamd loaded signatures besides the test database"
  echo 'scanner: clamd has only the test signature (the EICAR test string is not found)'

  # The marker in a tEXt chunk passes the app's own caps and is refused by clamd: the original bytes are scanned.
  [[ $(upload "$work/infected.png") == 422 ]] || fail "the marker PNG was not refused as infected: $(cat "$page")"
  grep -qF 'Photo refused: the malware scanner flagged this file. Nothing was stored.' "$page" || fail 'no infected copy'
  grep -qE 'Reference: [0-9a-f]{8}' "$page" || fail 'the infected page has no reference'
  ! grep -q 'Opsecmkt.Test.Marker' "$page" || fail 'the uploader saw the signature name'
  [[ $(rows) == 0 ]] || fail 'the refused upload stored a row'
  echo 'scanner: the marker PNG was refused as infected (422) and stored nothing'

  # A clean PNG is stored, and served publicly only as the re-encoded JPEG (no tEXt, no planted text).
  [[ $(upload "$work/clean.png") == 303 ]] || fail "the clean PNG was not stored: $(cat "$page")"
  [[ $(rows) == 1 ]] || fail 'the clean upload did not store exactly one row'
  [[ $(curl -sS -o "$page" -w '%{http_code}' "$base/product?id=$listing") == 200 ]] || fail 'the product page did not answer 200'
  image=$(grep -o '/img/[0-9a-f]\{32\}-f' "$page" | sed -n 1p)
  [[ -n $image ]] || fail 'the product page shows no photo'
  [[ $(curl -sS -D "$work/headers" -o "$work/served" -w '%{http_code} %{content_type}' "$base$image") == '200 image/jpeg' ]] ||
    fail "$image was not served as a JPEG"
  python3 - "$work/served" "$planted" "$marker" <<'PY' || fail 'the served photo is not a clean re-encoded JPEG'
import sys
data = open(sys.argv[1], 'rb').read()
assert data[:3] == b'\xff\xd8\xff', 'not a JPEG'
for needle in (b'tEXt', b'IHDR', sys.argv[2].encode(), sys.argv[3].encode()):
    assert needle not in data, needle
PY
  echo "scanner: the clean PNG was stored and $image serves a re-encoded JPEG without its tEXt chunk"

  # clamd stopped: uploads are refused with the scanner copy and store nothing; the market keeps serving.
  "${compose[@]}" stop clamd
  [[ $(upload "$work/clean.png") == 503 ]] || fail "an upload with clamd stopped was not refused: $(cat "$page")"
  # Before the app's next 30 s poll the upload reaches clamd and fails there ("did not respond", counted as a
  # scanner failure); after it, the cached state refuses the upload before its body is read ("paused", not
  # counted). Either is the scanner copy, and the admin count below follows whichever was seen.
  if grep -qF 'Photo not saved: the malware scanner did not respond, so nothing was stored. Try again later.' "$page"; then
    scanner_failed=1
  elif grep -qF 'Photo uploads are paused: the malware scanner is not responding.' "$page"; then
    scanner_failed=0
  else
    fail 'no scanner-unavailable copy'
  fi
  [[ $(rows) == 1 ]] || fail 'an upload with clamd stopped stored a row'
  [[ $(curl -sS "$base/healthz") == ok ]] || fail '/healthz failed with clamd stopped'
  [[ $(curl -sS -o /dev/null -w '%{http_code}' "$base/") == 200 ]] || fail 'the catalog failed with clamd stopped'
  # Both prerequisites must succeed; failure of either takes the refusal path.
  # shellcheck disable=SC2015
  [[ $(get "/listing-edit?id=$listing") == 200 ]] && grep -qF 'Photo uploads are paused: the malware scanner is not responding.' "$page" ||
    fail 'the Photos panel does not say uploads are paused'
  echo 'scanner: with clamd stopped an upload is refused (503, nothing stored); /healthz and the catalog answer'
  "${compose[@]}" start clamd
  wait_ready
  [[ $(upload "$work/clean.png") == 303 && $(rows) == 2 ]] || fail 'uploads did not resume after clamd started'
  echo 'scanner: uploads resume after clamd starts'

  # SHUTDOWN from the app container (the only other member of the scanner network) is refused; clamd keeps serving.
  clamd_command() {
    # Expand these variables only inside the child shell.
    # shellcheck disable=SC2016
    "${compose[@]}" exec -T app timeout 10 bash -c 'exec 3<>/dev/tcp/clamd/3310 && printf "z%s\0" "$1" >&3 && tr -d "\0" <&3' _ "$1"
  }
  reply=$(clamd_command SHUTDOWN)
  [[ $reply == 'COMMAND UNAVAILABLE' ]] || fail "SHUTDOWN answered '$reply'"
  sleep 2
  reply=$(clamd_command PING)
  [[ $reply == PONG ]] || fail "PING after SHUTDOWN answered '$reply'"
  [[ $(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$("${compose[@]}" ps -q clamd)") == 'true 0' ]] ||
    fail 'clamd stopped or restarted after SHUTDOWN'
  [[ $(curl -sS "$base/healthz") == ok ]] || fail '/healthz failed after SHUTDOWN'
  [[ $(upload "$work/clean.png") == 303 && $(rows) == 3 ]] || fail 'uploads failed after SHUTDOWN'
  echo 'scanner: SHUTDOWN from the app container answers COMMAND UNAVAILABLE; clamd keeps serving, never restarted'

  # clamd reaches nothing but the scanner network: it resolves app (the control) but not db, connects to app:8080
  # but not to the database's address or outside, and has no default route.
  local db_container db_ip
  if [[ "$mode" == internal ]]; then db_container=$("${compose[@]}" ps -q db); else db_container="${project}-db"; fi
  db_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$db_container")
  [[ -n $db_ip ]] || fail 'no database address on the backend network'
  "${compose[@]}" exec -T clamd getent hosts app >/dev/null || fail 'control: clamd cannot resolve app'
  ! "${compose[@]}" exec -T clamd getent hosts db || fail 'clamd resolves db'
  "${compose[@]}" exec -T clamd nc -z -w 5 app 8080 || fail 'control: clamd cannot connect to app:8080'
  ! "${compose[@]}" exec -T clamd nc -z -w 5 "$db_ip" 5432 2>/dev/null || fail "clamd connects to the database at $db_ip"
  ! "${compose[@]}" exec -T clamd nc -z -w 5 1.1.1.1 443 2>/dev/null || fail 'clamd connects outside the host'
  "${compose[@]}" exec -T clamd cat /proc/net/route > "$work/route"
  ! awk 'NR > 1 && $2 == "00000000"' "$work/route" | grep -q . || fail 'clamd has a default route'
  echo "scanner: clamd resolves app but not db, and cannot connect to the database ($db_ip:5432) or outside (1.1.1.1:443)"

  # /admin/system (A-542) shows the test database's build date (so the age gate passed) and the counts; the log names the
  # signature with the reference only (ruling C4).
  [[ $(get /admin/system) == 200 ]] || fail '/admin/system did not answer 200'
  python3 - "$page" "$scanner_failed" <<'PY' || fail '/admin/system image scanner panel'
import calendar, re, sys, time
html = open(sys.argv[1], encoding='utf-8').read()
raw = html[html.index('id="image-scanner"'):]
raw = raw[:raw.index('</section>')]
panel = ' '.join(re.sub(r'<[^>]+>', ' ', raw).split())
checks = [f'3 stored · 1 refused as infected · {sys.argv[2]} refused because the scanner failed', 'Opsecmkt.Test.Marker.UNOFFICIAL']
missing = [check for check in checks if check not in panel] + ['Ready'] * ('<strong>Ready</strong>' not in raw)
signatures = re.search(r'Signatures: ClamAV [0-9.]+, daily database 1 built (\d{4}-\d{2}-\d{2} \d{2}:\d{2}) UTC, 0 days old \(limit 30\)', panel)
if missing or not signatures:
    sys.exit(f'missing {missing!r} or the test database signature line in: {panel}')
# The date VERSION reported is the test database's build time, minutes ago (not the image's own signatures).
built = calendar.timegm(time.strptime(signatures[1], '%Y-%m-%d %H:%M'))
if not -120 <= time.time() - built <= 3600:
    sys.exit(f'the signatures were not built during this run: {signatures[0]}')
print('scanner: /admin/system shows', signatures[0])
PY
  "${compose[@]}" logs --no-color app > "$work/app.log" 2>&1
  grep -qE 'upload outcome=refused reason=infected sig=Opsecmkt\.Test\.Marker\.UNOFFICIAL ref=[0-9a-f]{8} kind=listing$' "$work/app.log" ||
    fail 'no infected log line'
  echo 'Scanner leg passed: real clamd with a test signature only (not a production signature feed).'
}
# Response hardening on the running production image (war room A-518 (c)): the headers acceptance.md names are on
# a GET page as the image sends them (not only on the handler in a Go test), and no profiling or debugging
# endpoint answers. Statuses and headers only: no refusal body or Content-Type is asserted here.
security_headers_check() {
  local base=http://127.0.0.1:18090 headers="$work/security-headers" csp xcto xfo referrer path
  curl -sS -o /dev/null -D "$headers" "$base/" || fail 'GET / did not answer'
  header() { grep -i "^$1:" "$headers" | sed -n '1s/^[^:]*: *//p' | tr -d '\r' || true; }
  lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
  csp=$(header Content-Security-Policy)
  [[ $csp == *"default-src 'none'"* ]] || fail "GET /: Content-Security-Policy lacks default-src 'none': '$csp'"
  [[ $csp != *unsafe-* && $csp != *script-src* ]] || fail "GET /: Content-Security-Policy allows scripts or unsafe sources: '$csp'"
  xcto=$(header X-Content-Type-Options)
  [[ $(lower "$xcto") == nosniff ]] || fail "GET /: X-Content-Type-Options is '$xcto', not nosniff"
  xfo=$(header X-Frame-Options)
  xfo=$(lower "$xfo")
  [[ $xfo == deny || $xfo == sameorigin || $csp == *"frame-ancestors 'none'"* || $csp == *"frame-ancestors 'self'"* ]] ||
    fail "GET /: no frame protection (X-Frame-Options '$xfo', Content-Security-Policy '$csp')"
  referrer=$(header Referrer-Policy)
  case $(lower "$referrer") in
    no-referrer | same-origin | strict-origin | strict-origin-when-cross-origin) ;;
    *) fail "GET /: Referrer-Policy is '$referrer', not a restrictive policy" ;;
  esac
  for path in /debug/pprof/ /debug/pprof/cmdline /debug/vars; do
    [[ $(curl -sS -o /dev/null -w '%{http_code}' "$base$path") == 404 ]] || fail "GET $path did not answer 404"
  done
  echo 'Production image: CSP, nosniff, frame protection and Referrer-Policy on GET /; /debug/pprof/ and /debug/vars answer 404'
}
command -v go >/dev/null || { echo 'Go is required to build cmd/verify-audit for the audit export check' >&2; exit 1; }
CGO_ENABLED=0 go build -trimpath -o "$work/verify-audit" ./cmd/verify-audit
export SMOKE_WORK="$work" SMOKE_ROOT="$root"
docker network create "$network" >/dev/null
if [[ "$mode" == external ]]; then
  docker run -d --name "${project}-db" --network "$network" --network-alias db \
    -e POSTGRES_USER=opsecmkt -e POSTGRES_DB=opsecmkt -e POSTGRES_PASSWORD -e POSTGRES_INITDB_ARGS=--encoding=UTF8 \
    postgres:17-alpine >/dev/null
  for _attempt in {1..60}; do
    if docker exec "${project}-db" pg_isready -h 127.0.0.1 -U opsecmkt -d opsecmkt; then break; fi
    sleep 1
  done
  docker exec "${project}-db" pg_isready -h 127.0.0.1 -U opsecmkt -d opsecmkt
fi
if [[ "$leg" == scanner ]]; then scanner_prepare; fi
# --wait returns once every started service is healthy: the app by its own `-healthcheck` (compose.yaml), and
# in the scanner leg clamd by clamdcheck.sh (PING).
"${compose[@]}" up -d --no-build --wait --wait-timeout 180
if [[ "$mode" == external ]]; then
  test -z "$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=db)"
fi
if [[ "$leg" == scanner ]]; then
  scanner_leg
  exit 0
fi
python3 - <<'PY'
import http.cookiejar, os, re, time, urllib.parse, urllib.request
base = 'http://127.0.0.1:18090'
for attempt in range(90):
    try:
        assert urllib.request.urlopen(base + '/healthz', timeout=2).read() == b'ok'
        break
    except Exception:
        time.sleep(1)
else:
    raise SystemExit('Production image did not become healthy')
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
html = client.open(base + '/setup').read().decode()
csrf = re.search(r'name="csrf"\s+value="([^"]+)"', html)
assert csrf, 'Setup must contain CSRF token'
data = urllib.parse.urlencode({'csrf': csrf[1], 'token': os.environ['SETUP_TOKEN'], 'handle': 'ci_admin', 'password': 'ci-long-disposable-password', 'site_name': 'CI Runtime Market'}).encode()
response = client.open(base + '/setup', data=data)
assert response.status == 200 and '/setup' not in response.url, 'Bootstrap did not succeed'
assert client.open(base + '/admin').status == 200, 'Admin session missing'
try:
    client.open(base + '/setup')
except urllib.error.HTTPError as error:
    assert error.code == 403, f'Unexpected setup lockdown status {error.code}'
else:
    raise AssertionError('Bootstrap setup was not locked after install')
assert client.open(base + '/healthz').read() == b'ok'

# Every stylesheet the layout links is served from the image with a CSS type and this checkout's bytes.
from pathlib import Path
page = client.open(base + '/').read().decode()
sheets = re.findall(r'<link rel="stylesheet" href="(/static/[^"]+\.css)">', page)
assert '/static/style.css' in sheets and len(sheets) == len(set(sheets)), f'Unexpected stylesheet links: {sheets}'
for sheet in sheets:
    response = client.open(base + sheet)
    body = response.read()
    assert response.status == 200 and response.headers.get_content_type() == 'text/css', f'{sheet}: {response.status} {response.headers.get("Content-Type")}'
    assert body == (Path(os.environ['SMOKE_ROOT']) / 'web' / sheet.lstrip('/')).read_bytes(), f'{sheet} differs from web{sheet}'

# Every administration page (A-542: the overview and its sections), read as one document.
admin_pages = ('/admin', '/admin/users', '/admin/vendors', '/admin/payments', '/admin/security', '/admin/site', '/admin/transparency', '/admin/system')
admin = ''.join(client.open(base + path).read().decode() for path in admin_pages)
# Pooled payments are removed (A-492): no pooled action form, payout table or payout count may be reachable.
assert 'action="/admin/payout"' not in admin and 'action="/account/payout"' not in admin
assert 'id="payouts"' not in admin and '#payouts"' not in admin and ' payouts)' not in admin
assert 'action="/orders/pay"' not in admin
# Signed audit export: available with the pinned key, and the downloaded export verifies offline below.
section = admin[admin.index('id="audit-export-title"'):]
section = section[:section.index('</section>')]
public_key = os.environ['SMOKE_AUDIT_PUBLIC_KEY']
assert 'badge green">Available' in section and public_key in section, 'Signed audit export is not reported available'
assert public_key in client.open(base + '/canary').read().decode(), 'Audit public key missing from /canary'
upto = re.search(r'name="upto" type="number" min="1" max="(\d+)"', section)
assert upto and int(upto[1]) >= 1, 'No audit events to export after bootstrap'
export = client.open(f'{base}/admin/audit-export?upto={upto[1]}')
assert export.headers.get_content_type() == 'application/x-ndjson'
export_bytes = export.read()
assert b'Account created: admin' in export_bytes, 'Bootstrap audit event missing from export'
signature = client.open(f'{base}/admin/audit-export?upto={upto[1]}&sig=1').read()
assert f'public_key: {public_key}\n'.encode() in signature
work = Path(os.environ['SMOKE_WORK'])
(work / 'export.jsonl').write_bytes(export_bytes)
(work / 'export.sig').write_bytes(signature)
(work / 'tampered.jsonl').write_bytes(export_bytes.replace(b'Account created: admin', b'Account created: buyer'))
print('Production image: database, health, bootstrap, admin session, setup lockdown, stylesheets, escrow-only payment surface and audit export passed')
PY
# -status inside the container reports the completed setup, and that this SETUP_TOKEN-only environment is a
# legacy key root, and nothing else (no secret).
status_line=$("${compose[@]}" exec -T app /app/server -status)
[[ $status_line =~ ^installed=true\ schema=[0-9]{3}\ keyroot=setup_token$ ]] || { echo "unexpected -status output: $status_line" >&2; exit 1; }
"$work/verify-audit" -pub "$SMOKE_AUDIT_PUBLIC_KEY" "$work/export.jsonl" "$work/export.sig"
if "$work/verify-audit" -pub "$SMOKE_AUDIT_PUBLIC_KEY" "$work/tampered.jsonl" "$work/export.sig" 2>/dev/null; then
  echo 'verify-audit accepted a tampered export' >&2; exit 1
fi
# The fresh escrow-only server stays healthy without any pooled wallet.
sleep 5
python3 -c "import urllib.request; assert urllib.request.urlopen('http://127.0.0.1:18090/healthz', timeout=5).read() == b'ok'"
app_container=$("${compose[@]}" ps -q app)
[[ $(docker inspect -f '{{.State.Status}} {{.RestartCount}} {{.State.Health.Status}}' "$app_container") == 'running 0 healthy' ]]
security_headers_check
"${compose[@]}" logs --no-color app > "$work/app.log" 2>&1
echo 'Production image: stays healthy without pooled wallets; signed audit export verified with cmd/verify-audit'
