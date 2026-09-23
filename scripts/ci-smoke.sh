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
print('Production image: database, health, bootstrap, admin session and setup lockdown passed')
PY
