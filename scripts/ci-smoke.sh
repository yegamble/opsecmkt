#!/usr/bin/env bash
# Exercises the built production image; uses only disposable CI containers/volumes.
set -euo pipefail
unset COMPOSE_PROFILES COMPOSE_FILE
mode=${1:?usage: ci-smoke.sh internal|external}
[[ "$mode" == internal || "$mode" == external ]] || exit 2
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
work=$(mktemp -d)
project="opsecmkt-ci-${mode}-$$"
network="${project}-backend"
export DATABASE_URL='postgres://opsecmkt:ci-runtime-only@db:5432/opsecmkt?sslmode=disable'
export POSTGRES_PASSWORD='ci-runtime-only'
export SETUP_TOKEN='ci-container-bootstrap-token-only-123456789'
export COOKIE_SECURE=false APP_PORT=18090
# Signed audit export on, with a fixed disposable seed whose public key is pinned below.
export AUDIT_SIGNING_KEY='c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c15c'
export SMOKE_AUDIT_PUBLIC_KEY='1678b99a3a0e29e1bd91e440816593702f6f0824cfa20e68ae70c02e15f7361c'
# A configured but unreachable test-chain node (nothing listens on port 9 inside the app container): the
# site must still start and serve, with Bitcoin payments shown as unavailable and retried every poll.
export BITCOIN_RPC_URL='http://ci:ci-unreachable-node@127.0.0.1:9' BITCOIN_CHAIN=testnet4 PAYMENT_POLL_INTERVAL=2s
unset MONERO_RPC_URL MONERO_WALLET_RPC_URL MONERO_NETWORK
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
cleanup() {
  status=$?
  if ((status)); then "${compose[@]}" logs --no-color || true; fi
  "${compose[@]}" down --volumes --remove-orphans || true
  if [[ "$mode" == external ]]; then docker rm -f "${project}-db" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
command -v go >/dev/null || { echo 'Go is required to build cmd/verify-audit for the audit export check' >&2; exit 1; }
CGO_ENABLED=0 go build -trimpath -o "$work/verify-audit" ./cmd/verify-audit
export SMOKE_WORK="$work" SMOKE_ROOT="$root"
docker network create "$network" >/dev/null
if [[ "$mode" == external ]]; then
  docker run -d --name "${project}-db" --network "$network" --network-alias db \
    -e POSTGRES_USER=opsecmkt -e POSTGRES_DB=opsecmkt -e POSTGRES_PASSWORD \
    postgres:17-alpine >/dev/null
  for attempt in {1..60}; do
    if docker exec "${project}-db" pg_isready -U opsecmkt -d opsecmkt; then break; fi
    sleep 1
  done
  docker exec "${project}-db" pg_isready -U opsecmkt -d opsecmkt
fi
"${compose[@]}" up -d --no-build
if [[ "$mode" == external ]]; then
  test -z "$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=db)"
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

admin = client.open(base + '/admin').read().decode()
# Degraded payments: the configured node is unreachable, so BTC is unavailable (not fatal); XMR is unset.
def provider_row(currency):
    row = re.search(r'<tr>\s*<td>' + currency + r'</td>(.*?)</tr>', admin, re.S)
    assert row, f'No {currency} provider row on /admin'
    return row[1]
btc = provider_row('BTC')
assert 'Unavailable — retried every poll' in btc and 'bitcoin node check failed' in btc, f'BTC should be unavailable: {btc}'
assert '<td>Disabled</td>' in provider_row('XMR')
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
print('Production image: database, health, bootstrap, admin session, setup lockdown, stylesheets, unavailable BTC and audit export passed')
PY
"$work/verify-audit" -pub "$SMOKE_AUDIT_PUBLIC_KEY" "$work/export.jsonl" "$work/export.sig"
if "$work/verify-audit" -pub "$SMOKE_AUDIT_PUBLIC_KEY" "$work/tampered.jsonl" "$work/export.sig" 2>/dev/null; then
  echo 'verify-audit accepted a tampered export' >&2; exit 1
fi
# Still up after several failed payment polls, never restarted, and the degraded start was logged.
sleep 5
python3 -c "import urllib.request; assert urllib.request.urlopen('http://127.0.0.1:18090/healthz', timeout=5).read() == b'ok'"
app_container=$("${compose[@]}" ps -q app)
[[ $(docker inspect -f '{{.State.Status}} {{.RestartCount}}' "$app_container") == 'running 0' ]]
"${compose[@]}" logs --no-color app > "$work/app.log" 2>&1
grep -q 'payments: BTC unavailable at startup' "$work/app.log"
echo 'Production image: stays healthy with an unreachable payment node; signed audit export verified with cmd/verify-audit'
