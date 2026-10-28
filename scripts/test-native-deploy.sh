#!/usr/bin/env bash
# The native deployment controller (`opsecmkt deploy`, internal/operator) against a real Docker Engine (A-697). The Go
# tests drive it with a fake docker; this runs the commands an operator runs, in development mode with the internal
# database and public Tor, under a unique project name:
#   - compose.json rendered for development and production (production storage stubbed by a Go test) is accepted
#     by `docker compose config --quiet`;
#   - a one-off `compose run --rm --no-deps -T` with logging driver none still prints its output;
#   - deploy init, plan, install; a saved site access setting; deploy redeploy to a second reference of the same image
#     keeps that setting, the db and tor containers, the onion address and every volume; deploy doctor --start-check
#     prints the one-off run's output and leaves no run container behind;
#   - with migration 999_from_a_newer_release recorded, deploy redeploy fails, leaves operator.json with the requested
#     image and changes no volume, schema row or setting; the refusal shows in deploy logs and doctor --start-check;
#   - deploy down removes the containers and retains the volumes.
# Production mode (LUKS storage checks, the remote database, the app's logging driver none, the start check a failed
# production start runs) is not run here.
# Requires Linux, Bash 4+, Docker with Compose v2, Go, Python 3 and curl, plus the images APP_IMAGE (default
# opsecmkt:ci) and TOR_IMAGE (default opsecmkt-tor:ci); pulls the pinned registry and PostgreSQL images. Native deploy
# accepts only IMAGE@sha256:DIGEST references, so both images are pushed to a loopback-only registry container.
# The exit trap removes this run's containers, volumes and networks (selected by its project labels), the registry,
# the tags it created and the work directory; it never touches another project.
set -euo pipefail
unset DOCKER_HOST DOCKER_CONTEXT COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
app_source=${APP_IMAGE:-opsecmkt:ci}
tor_source=${TOR_IMAGE:-opsecmkt-tor:ci}
# Distribution registry 3.1.2, pinned by its index digest; it serves only this run's images on 127.0.0.1.
registry_image='registry:3.1.2@sha256:d4032d4cc323c1731c707931ecada07a53076eac38e3b61ae57533f170f2c6c3'
# Not under a tmpfs /tmp: the controller refuses retention state on tmpfs inside the installation.
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/opsecmkt-native.XXXXXX")
project="opsecmkt-native-$$"
probe="${project}-lognone"
registry="${project}-registry"
dir="$work/install"
bin="$work/opsecmkt"
refs=()
newer='it records migration(s) 999_from_a_newer_release, which this server does not include. Nothing was changed.'

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "== [${SECONDS}s] $*"; }
compose() { docker compose --project-name "$project" --project-directory "$dir" --env-file /dev/null -f "$dir/compose.json" "$@"; }
deploy() { "$bin" deploy "$1" --dir "$dir" "${@:2}"; }
sql() { compose exec -T db psql -X -q -A -t -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt -c "$1"; }
by_label() { docker "$1" ls -q --filter "label=com.docker.compose.project=$2"; }
project_containers() { docker ps -aq --filter "label=com.docker.compose.project=$project" "$@"; }
container() { compose ps -aq "$1"; }
inspect() { docker inspect --format "$1" "$2"; }
setting() { python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$dir/operator.json" "$1"; }
volumes() {
  local volume names
  mapfile -t names < <(by_label volume "$project" | LC_ALL=C sort)
  for volume in ${names[@]+"${names[@]}"}; do inspect '{{.Name}} {{.CreatedAt}}' "$volume"; done
}
# Schema row count, newest migration and the two saved access settings, on one line.
state() { sql "SELECT (SELECT count(*)||' '||max(version) FROM schema_migrations)||' '||string_agg(key||'='||value, ' ' ORDER BY key) FROM settings WHERE key IN ('registrations','site_access')"; }
onion() {
  for _ in $(seq 60); do compose exec -T tor cat /var/lib/tor/marketplace/hostname 2>/dev/null && return 0; sleep 1; done
  fail "tor wrote no onion hostname"
}
no_run_containers() {
  local names
  [[ -z "$(project_containers --filter label=com.docker.compose.oneoff=True)" ]] || fail "$1 left a one-off container"
  names=$(docker ps -a --filter "label=com.docker.compose.project=$project" --format '{{.Names}}')
  if grep -q -- '-run-' <<<"$names"; then fail "$1 left a *-run-* container: $names"; fi
}
wait_healthy() {
  local id
  for _ in $(seq 60); do
    id=$(container app)
    [[ -n "$id" && "$(inspect '{{.State.Health.Status}}' "$id")" == healthy ]] && return 0
    sleep 2
  done
  fail "the app container did not become healthy"
}

cleanup() {
  local status=$? kind name ref ids
  trap - EXIT
  set +e
  if ((status)) && [[ -f "$dir/compose.json" ]]; then
    compose ps --all
    "$bin" deploy logs --dir "$dir" # redacted by the controller
  fi
  [[ -f "$dir/compose.json" ]] && compose down --volumes --remove-orphans --timeout 10 >/dev/null 2>&1
  [[ -f "$work/probe.yaml" ]] && docker compose --project-name "$probe" --env-file /dev/null -f "$work/probe.yaml" down --remove-orphans >/dev/null 2>&1
  # Anything Compose did not remove, selected only by this run's project labels.
  for name in "$project" "$probe"; do
    mapfile -t ids < <(docker ps -aq --filter "label=com.docker.compose.project=$name")
    ((${#ids[@]})) && docker rm -f -v "${ids[@]}" >/dev/null 2>&1
    for kind in volume network; do
      mapfile -t ids < <(by_label "$kind" "$name")
      ((${#ids[@]})) && docker "$kind" rm "${ids[@]}" >/dev/null 2>&1
    done
  done
  docker rm -f -v "$registry" >/dev/null 2>&1
  for ref in ${refs[@]+"${refs[@]}"}; do docker image rm "$ref" >/dev/null 2>&1; done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

step "Preconditions"
docker compose version
docker image inspect "$app_source" >/dev/null 2>&1 || fail "build $app_source first: docker build -t $app_source ."
docker image inspect "$tor_source" >/dev/null 2>&1 || fail "build $tor_source first: docker build -t $tor_source -f deploy/Dockerfile.tor deploy"
go build -o "$bin" ./cmd/server
"$bin" version

step "compose.json for development and production (stub storage) through docker compose config --quiet"
mkdir "$work/render"
OPSECMKT_RENDER_DIR="$work/render" go test -count=1 -run '^TestRenderInstallationsForDockerCompose$' ./internal/operator
for expected in 'dev-full:admin-tor-prepare app clamd db monero monero-wallet tor tor-admin tor-identity-prepare' \
  'prod:app' 'prod-tor:admin-tor-prepare app tor-admin'; do
  name=${expected%%:*}
  rendered=(docker compose --project-name "opsecmkt-render-$name" --project-directory "$work/render/$name" --env-file /dev/null -f "$work/render/$name/compose.json")
  "${rendered[@]}" config --quiet || fail "docker compose refused the rendered $name compose.json"
  services=$("${rendered[@]}" config --services | LC_ALL=C sort | tr '\n' ' ')
  [[ "$services" == "${expected#*:} " ]] || fail "$name renders services '$services', expected '${expected#*:}'"
  echo "$name: accepted; services: $services"
done
rm -rf "$work/render"

# The production app's logging driver is none, so its container keeps no logs; the start check reads a one-off run's
# attached output instead. Show that such output reaches the caller, with the controller's run flags and this image.
step "compose run --rm --no-deps -T output with logging driver none"
cat > "$work/probe.yaml" <<YAML
services:
  app:
    image: $app_source
    network_mode: none
    logging:
      driver: none
YAML
output=$(docker compose --project-name "$probe" --env-file /dev/null -f "$work/probe.yaml" run --rm --no-deps -T app version)
grep -q '^opsecmkt ' <<<"$output" || fail "no output from a one-off run with logging driver none: $output"
echo "attached output: $output"
[[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$probe")" ]] || fail "the probe run left a container"

step "Digest references from a loopback registry"
docker run -d --name "$registry" -p 127.0.0.1::5000 --cap-drop ALL --security-opt no-new-privileges:true "$registry_image" >/dev/null
address=$(docker port "$registry" 5000/tcp)
address=${address%%$'\n'*}
[[ "$address" == 127.0.0.1:* ]] || fail "registry port: $address"
for _ in $(seq 30); do curl -fsS "http://$address/v2/" >/dev/null 2>&1 && break; sleep 1; done
curl -fsS "http://$address/v2/" >/dev/null || fail "the registry did not start"
published=
publish() { # SOURCE NAME: push SOURCE as NAME; sets published to NAME@sha256:DIGEST, the digest the registry serves
  local tag="$address/$2:native" headers digest
  docker tag "$1" "$tag"
  refs+=("$tag")
  docker push -q "$tag" >/dev/null || fail "pushing $tag failed"
  headers=$(curl -fsSI -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "http://$address/v2/$2/manifests/native" | tr -d '\r')
  digest=$(sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *\(sha256:[0-9a-f]\{64\}\)$/\1/p' <<<"$headers")
  [[ -n "$digest" ]] || fail "the registry names no digest for $tag"
  published="$address/$2@$digest"
  refs+=("$published")
  docker image inspect "$published" >/dev/null 2>&1 || docker pull -q "$published" >/dev/null
}
publish "$app_source" opsecmkt-app
first=$published
publish "$app_source" opsecmkt-app-next
second=$published
publish "$tor_source" opsecmkt-tor
tor=$published
for ref in "$first" "$second"; do
  [[ "$(inspect '{{.Id}}' "$ref")" == "$(inspect '{{.Id}}' "$app_source")" ]] || fail "$ref is not $app_source"
done
echo "app: $first"
echo "app, second reference: $second"
echo "tor: $tor"

step "deploy init, plan and install (development, internal database, public Tor)"
deploy init --mode dev --project "$project" --exposure tor --app-image "$first" --tor-image "$tor"
deploy plan
deploy install
compose config --quiet
compose ps --all
wait_healthy
app_id=$(container app)
db_id=$(container db)
tor_id=$(container tor)
[[ -n "$db_id" && -n "$tor_id" ]] || fail "install did not create db and tor"
[[ "$(inspect '{{.State.Running}}' "$tor_id")" == true ]] || fail "tor is not running"
[[ "$(inspect '{{.Config.Image}}' "$app_id")" == "$first" ]] || fail "the app runs $(inspect '{{.Config.Image}}' "$app_id")"
address_onion=$(setting onion_address)
[[ "$(onion)" == "$address_onion" ]] || fail "tor serves another onion address than deploy init generated"
installed_volumes=$(volumes)
[[ "$(wc -l <<<"$installed_volumes")" -eq 2 ]] || fail "expected the postgres_data and tor_identity volumes: $installed_volumes"
echo "$installed_volumes"
[[ "$(state)" == *" registrations=open site_access=public" ]] || fail "first start did not seed the initial access settings: $(state)"

step "Save an administrator access setting"
# The statement the administrator's access form commits (internal/market/site_access.go, saveSiteAccess).
sql "INSERT INTO settings(key,value) VALUES ('site_access','members'),('registrations','closed') ON CONFLICT(key) DO UPDATE SET value=excluded.value"
saved=$(state)
echo "schema rows, newest migration, settings: $saved"

step "deploy redeploy to a second reference of the same image"
deploy redeploy --app-image "$second"
wait_healthy
next_id=$(container app)
[[ "$next_id" != "$app_id" ]] || fail "redeploy did not recreate the app container"
[[ "$(inspect '{{.Config.Image}}' "$next_id")" == "$second" ]] || fail "the app runs $(inspect '{{.Config.Image}}' "$next_id")"
[[ "$(setting app_image)" == "$second" ]] || fail "operator.json app_image is $(setting app_image)"
[[ "$(container db)" == "$db_id" ]] || fail "redeploy replaced the db container"
[[ "$(container tor)" == "$tor_id" ]] || fail "redeploy replaced the tor container"
[[ "$(state)" == "$saved" ]] || fail "redeploy changed the saved settings: $(state)"
[[ "$(volumes)" == "$installed_volumes" ]] || fail "redeploy changed the volumes: $(volumes)"
[[ "$(onion)" == "$address_onion" ]] || fail "the onion address changed"
echo "app container replaced; db, tor, onion address, volumes and saved settings retained"

step "deploy doctor --start-check on the running installation"
password=$(cat "$dir/secrets/postgres-password")
output=$(deploy doctor --start-check 2>&1) || { printf '%s\n' "$output"; fail "doctor --start-check failed on a healthy installation"; }
printf '%s\n' "$output"
grep -qF 'startup check: database: connected (UTF8)' <<<"$output" || fail "the start check printed no output"
grep -qF 'startup check: passed;' <<<"$output" || fail "the start check did not pass"
if grep -qF -- "$password" <<<"$output"; then fail "the start check output contains the database password"; fi
no_run_containers "doctor --start-check"
[[ "$(container app)" == "$next_id" ]] || fail "the start check replaced the app container"

step "deploy redeploy onto a database a newer release migrated"
sql "INSERT INTO schema_migrations(version,name) VALUES (999,'from_a_newer_release')"
newer_state=$(state)
began=$SECONDS
status=0
output=$(timeout 900 "$bin" deploy redeploy --dir "$dir" --app-image "$first" 2>&1) || status=$?
elapsed=$((SECONDS - began))
printf '%s\n' "$output"
((status != 124)) || fail "redeploy did not finish within 900 s"
((status != 0)) || fail "redeploy onto a newer database reported success"
echo "redeploy failed (exit $status) after ${elapsed}s"
grep -qF 'Docker command failed; configuration and volumes were retained' <<<"$output" || fail "redeploy did not report the failed start"
[[ "$(setting app_image)" == "$first" ]] || fail "operator.json app_image is $(setting app_image), not the requested $first"
failed_id=$(container app)
[[ "$(inspect '{{.Config.Image}}' "$failed_id")" == "$first" ]] || fail "the refused app container runs $(inspect '{{.Config.Image}}' "$failed_id")"
echo "app container: $(inspect '{{.State.Status}}, {{.RestartCount}} restarts, health {{.State.Health.Status}}' "$failed_id")"
[[ "$(container db)" == "$db_id" && "$(container tor)" == "$tor_id" ]] || fail "the failed redeploy replaced db or tor"
[[ "$(volumes)" == "$installed_volumes" ]] || fail "the failed redeploy changed the volumes: $(volumes)"
[[ "$(state)" == "$newer_state" ]] || fail "the refused start changed schema rows or settings: $(state)"
# Development keeps container logs, so a failed development start does not run the check; deploy logs shows why.
logs=$(deploy logs)
grep -qF "$newer" <<<"$logs" || { printf '%s\n' "$logs"; fail "deploy logs does not show the refusal"; }
echo "deploy logs: $(grep -m1 -F "$newer" <<<"$logs")"
status=0
output=$(deploy doctor --start-check 2>&1) || status=$?
printf '%s\n' "$output"
((status != 0)) || fail "doctor --start-check passed on a newer database"
grep -qF "startup check: refused: database migration failed: the database was upgraded by a newer release: $newer" <<<"$output" ||
  fail "doctor --start-check does not print the refusal"
grep -qF 'the refusal is printed above' <<<"$output" || fail "doctor --start-check does not point at the refusal"
no_run_containers "the refused start check"

step "deploy down"
deploy down
[[ -z "$(project_containers)" ]] || fail "deploy down left containers"
[[ "$(volumes)" == "$installed_volumes" ]] || fail "deploy down did not retain the volumes"
echo "containers removed; volumes retained"
step "Native deployment passed (development mode; production storage, logging and start path not covered)"
