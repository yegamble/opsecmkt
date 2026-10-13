#!/usr/bin/env bash
# A-524: the bundled monerod starts with compose.nodes.yaml's RPC port layout. monerod runs a core RPC server at the
# network's default port unless --rpc-bind-port pins it, and a restricted one on --rpc-restricted-bind-port; when both
# ask for one port it exits with "Failed to bind IPv4" (on mainnet both were 18081 before A-524). This renders the
# monero service's image and command with `docker compose config` (stagenet, no .env), forces the restricted port to
# 38081, stagenet's core default, which reproduces that mainnet collision class without mainnet, adds --offline and
# --no-igd, and runs it with --network none. It passes when both RPC servers report "initialized OK" and monerod is
# still running a few seconds later. Nothing reaches a network. Requires Docker with Compose v2 and Python 3; pulls
# the pinned image if it is absent. Mainnet itself is never started (development and CI never use mainnet).
set -euo pipefail
cd "$(dirname "$0")/.."
for dependency in docker python3; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-monerod-ports.XXXXXX")
container="opsecmkt-monerod-ports-$$-$RANDOM"
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  local status=$?
  trap - EXIT
  if docker container inspect "$container" >/dev/null 2>&1; then
    ((status == 0)) || { echo "--- logs of $container ---" >&2; docker logs "$container" 2>&1 | tail -n 30 >&2 || true; }
    docker rm -f -v "$container" >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# The compose job's synthetic values; only the pinned defaults (no .env, no *_IMAGE or prune override) reach the render.
: >"$work/env"
env -i PATH="$PATH" HOME="$HOME" DATABASE_URL=postgres://test:test@db/test \
  SETUP_TOKEN=ci-validation-only-not-a-real-bootstrap-secret \
  BITCOIN_RPC_AUTH='marketplace:0123456789abcdef0123456789abcdef$f25a796e58dd70ff7eb5b96b54de079cc7e3d6f40eb8a0fa55fd05f26a53635e' \
  MONERO_NETWORK_FLAG=--stagenet \
  docker compose --env-file "$work/env" -f compose.yaml -f compose.nodes.yaml --profile monero config --format json \
  >"$work/config.json"
# One NUL-terminated field per line of output: the image, then each command argument.
python3 -I - "$work/config.json" >"$work/command" <<'PY'
import json, sys
monero = json.load(open(sys.argv[1]))['services']['monero']
sys.stdout.write('\0'.join([monero['image'], *monero['command']]) + '\0')
PY
fields=()
while IFS= read -r -d '' field; do fields+=("$field"); done <"$work/command"
image=${fields[0]}
rendered=("${fields[@]:1}")
echo "image: $image"
echo "compose command: ${rendered[*]}"

arguments=()
restricted=0 core=''
for argument in "${rendered[@]}"; do
  case $argument in
    --rpc-restricted-bind-port=*)
      [[ $argument == --rpc-restricted-bind-port=18081 ]] || fail "compose's restricted port is not 18081: $argument"
      restricted=$((restricted + 1))
      argument=--rpc-restricted-bind-port=38081 ;;
    --rpc-bind-port=*) core=${argument#*=} ;;
  esac
  arguments+=("$argument")
done
((restricted == 1)) || fail "expected one --rpc-restricted-bind-port in compose's command, found $restricted"
[[ -n $core ]] || fail 'compose does not pin the core RPC port (--rpc-bind-port), so it takes the network default'
[[ $core != 38081 ]] || fail 'the core RPC port is the forced restricted port 38081'
[[ " ${arguments[*]} " == *' --stagenet '* ]] || fail 'the rendered command is not on stagenet'
# The image's entrypoint already passes --non-interactive.
arguments+=(--offline --no-igd)
echo "probe command: ${arguments[*]}"

docker run -d --name "$container" --network none "$image" "${arguments[@]}" >/dev/null
for _ in $(seq 60); do
  logs=$(docker logs "$container" 2>&1)
  if grep -q 'core RPC server initialized OK' <<<"$logs" && grep -q 'restricted RPC server initialized OK' <<<"$logs"; then
    break
  fi
  [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || fail 'monerod exited before both RPC servers started'
  sleep 1
done
grep -F 'core RPC server initialized OK' <<<"$logs" || fail 'no "core RPC server initialized OK" within 60 seconds'
grep -F 'restricted RPC server initialized OK' <<<"$logs" || fail 'no "restricted RPC server initialized OK" within 60 seconds'
grep -F "core RPC server initialized OK on port: $core" <<<"$logs" >/dev/null || fail "the core RPC server is not on port $core"
grep -F "Binding on 127.0.0.1 (IPv4):$core" <<<"$logs" >/dev/null || fail "the core RPC server is not bound to 127.0.0.1:$core"
grep -F 'restricted RPC server initialized OK on port: 38081' <<<"$logs" >/dev/null || fail 'the restricted RPC server is not on 38081'
# A bind failure ends monerod right after initialisation; give it time to exit before checking that it did not.
sleep 5
[[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || fail 'monerod is no longer running'
if grep -E 'Failed to bind|Error starting server' <<<"$(docker logs "$container" 2>&1)"; then
  fail 'monerod logged a bind error'
fi
echo "OK: monerod $image runs with the core RPC on 127.0.0.1:$core and the restricted RPC on 38081 (stagenet, offline)"
