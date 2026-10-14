#!/usr/bin/env bash
# A-536: the bundled database and nodes start with the hardening Compose gives them. This renders compose.yaml,
# compose.nodes.yaml and compose.internal-db.yaml with `docker compose config --format json` (CI's synthetic values, no
# .env: Bitcoin regtest, Monero stagenet), maps each service's user, cap_drop, cap_add, security_opt, read_only and
# tmpfs (with its environment, its image, its command and a test volume at its volume's path) onto `docker run
# --network none`, and starts each service twice: on a new volume, then again on the same one.
#   db                    pg_isready -h 127.0.0.1 (initialised the first time, an existing database the second)
#   bitcoin               bitcoin-cli getblockcount as the bitcoin user; the documented edits of /data/bitcoin.conf
#                         (as bitcoin and as root) and the runbook's wallet backup and restore through /tmp work
#   monero                --offline: both "RPC server initialized OK" lines, still running 5 seconds later
#   monero-escrow-wallet  "Starting wallet RPC server", still running 5 seconds later
# Nothing reaches a network; every container and volume is removed, also on failure. Requires bash 5, Docker with
# Compose v2 and Python 3; pulls the pinned images if they are absent. Mainnet is never started.
set -euo pipefail
((BASH_VERSINFO[0] >= 5)) || { echo "bash 5 or later is required (this is $BASH_VERSION)" >&2; exit 1; }
cd "$(dirname "$0")/.."
for dependency in docker python3; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-service-hardening.XXXXXX")
prefix="opsecmkt-hardening-$$-$RANDOM"
containers=() volumes=()
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  local status=$? container volume
  trap - EXIT
  for container in "${containers[@]}"; do
    docker container inspect "$container" >/dev/null 2>&1 || continue
    ((status == 0)) || { echo "--- logs of $container ---" >&2; docker logs "$container" 2>&1 | tail -n 30 >&2 || true; }
    docker rm -f -v "$container" >/dev/null 2>&1 || true
  done
  for volume in "${volumes[@]}"; do docker volume rm -f "$volume" >/dev/null 2>&1 || true; done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# The compose job's synthetic values, on test networks; only the pinned defaults (no .env, no *_IMAGE) reach the render.
: >"$work/env"
env -i PATH="$PATH" HOME="$HOME" DATABASE_URL=postgres://test:test@db/test \
  SETUP_TOKEN=ci-validation-only-not-a-real-bootstrap-secret \
  BITCOIN_RPC_AUTH='marketplace:0123456789abcdef0123456789abcdef$f25a796e58dd70ff7eb5b96b54de079cc7e3d6f40eb8a0fa55fd05f26a53635e' \
  ESCROW_XMR_WALLET_RPC_PASSWORD=ci-validation-only POSTGRES_PASSWORD=ci-validation-only \
  BITCOIN_CHAIN=regtest MONERO_NETWORK_FLAG=--stagenet \
  docker compose --env-file "$work/env" -f compose.yaml -f compose.nodes.yaml -f compose.internal-db.yaml \
  --profile internal-db --profile bitcoin --profile monero --profile monero-escrow-wallet config --format json \
  >"$work/config.json"

# Writes $work/<service>.options (docker run options), .image, .command and .volume (its volume's path), each
# NUL-terminated field by field. `config` writes a literal $ as $$ (as in BITCOIN_RPC_AUTH); Compose would pass $.
python3 -I - "$work/config.json" "$work" <<'PY'
import json, sys
from pathlib import Path
services, work = json.load(open(sys.argv[1]))['services'], Path(sys.argv[2])
for name in ('db', 'bitcoin', 'monero', 'monero-escrow-wallet'):
    service = services[name]
    for key in ('cap_drop', 'security_opt', 'read_only'):
        if key not in service:
            sys.exit(f'{name}: the rendered service has no {key}')
    if service.get('entrypoint'):
        sys.exit(f'{name}: an entrypoint is set; this test runs the image entrypoint')
    named = [v for v in service.get('volumes') or [] if v.get('type') == 'volume']
    if len(named) != 1:
        sys.exit(f'{name}: expected one named volume, found {named}')
    options = ['--user', service['user']] if service.get('user') else []
    options += [f'--cap-drop={c}' for c in service['cap_drop']]
    options += [f'--cap-add={c}' for c in service.get('cap_add') or []]
    options += [f'--security-opt={o}' for o in service['security_opt']]
    options += ['--read-only'] if service['read_only'] else []
    options += [f'--tmpfs={t}' for t in service.get('tmpfs') or []]
    options += [f"--env={k}={v.replace('$$', '$')}" for k, v in sorted((service.get('environment') or {}).items())
                if v is not None]
    command = [argument.replace('$$', '$') for argument in service.get('command') or []]
    for suffix, fields in (('options', options), ('image', [service['image']]), ('command', command),
                           ('volume', [named[0]['target']])):
        (work / f'{name}.{suffix}').write_text(''.join(f'{field}\0' for field in fields))
PY
fields() { local -n into=$1; into=(); local field; while IFS= read -r -d '' field; do into+=("$field"); done <"$2"; }

running() { [[ $(docker inspect -f '{{.State.Running}}' "$1") == true ]]; }
# run_options <service> <volume>: the service's docker run options into $options, its image into $image.
run_options() {
  local target
  fields options "$work/$1.options"
  fields target "$work/$1.volume"
  options+=(--mount "type=volume,source=$2,target=${target[0]}")
  fields image "$work/$1.image"
}
# start <service> <volume> <container> [extra command arguments...]: one run of the service as Compose renders it.
start() {
  local service=$1 volume=$2 container=$3 options image command
  shift 3
  run_options "$service" "$volume"
  fields command "$work/$service.command"
  containers+=("$container")
  docker run -d --name "$container" --network none "${options[@]}" "${image[0]}" "${command[@]}" "$@" >/dev/null
}
# runs_as <container>: the uid:gid of its main process.
runs_as() { docker exec "$1" stat -c %u:%g /proc/1; }
stop() { docker stop -t 60 "$1" >/dev/null; docker rm -v "$1" >/dev/null; }
# wait_for <container> <seconds> <command...>: until the command succeeds, while the container runs.
wait_for() {
  local container=$1 seconds=$2 i
  shift 2
  for ((i = 0; i < seconds; i++)); do
    "$@" >/dev/null 2>&1 && return 0
    running "$container" || fail "$container exited before it was ready"
    sleep 1
  done
  fail "$container was not ready within $seconds seconds"
}
# logged <container> <text>: the container's log has the text (read whole: grep -q under pipefail can fail on SIGPIPE).
logged() { [[ $(docker logs "$1" 2>&1) == *"$2"* ]]; }

for service in db bitcoin monero monero-escrow-wallet; do
  fields options "$work/$service.options"
  fields image "$work/$service.image"
  security=()
  for option in "${options[@]}"; do [[ $option == --env=* ]] || security+=("$option"); done
  echo "$service: ${image[0]} ${security[*]}"
done

fields command "$work/bitcoin.command"
[[ " ${command[*]} " == *' -chain=regtest '* ]] || fail 'the rendered Bitcoin command is not on regtest'
fields command "$work/monero.command"
for flag in --stagenet --no-igd --no-zmq; do
  [[ " ${command[*]} " == *" $flag "* ]] || fail "the rendered monerod command lacks $flag"
done

for service in db bitcoin monero monero-escrow-wallet; do
  volume="$prefix-$service"
  docker volume create "$volume" >/dev/null
  volumes+=("$volume")
done

for run in 1 2; do
  [[ $run == 1 ]] && when='a new volume' || when='the same volume'

  container="$prefix-db-$run"
  start db "$prefix-db" "$container"
  wait_for "$container" 120 docker exec "$container" pg_isready -q -h 127.0.0.1 -U opsecmkt -d opsecmkt
  if ((run == 1)); then logged "$container" 'PostgreSQL init process complete' || fail 'db did not initialise the new volume'
  else logged "$container" 'Skipping initialization' || fail 'db did not find the existing database'; fi
  [[ $(docker exec "$container" psql -X -q -At -U opsecmkt -d opsecmkt -c 'SELECT current_user') == opsecmkt ]] ||
    fail 'db does not answer a query as opsecmkt'
  echo "OK: db accepts connections on 127.0.0.1, running as $(runs_as "$container") ($when)"
  stop "$container"

  container="$prefix-bitcoin-$run"
  start bitcoin "$prefix-bitcoin" "$container"
  cli=(docker exec -u bitcoin "$container" bitcoin-cli -datadir=/data -chain=regtest -rpcport=8332)
  wait_for "$container" 120 "${cli[@]}" getblockcount
  [[ $("${cli[@]}" getblockcount) =~ ^[0-9]+$ ]] || fail 'getblockcount did not answer a height'
  if ((run == 1)); then
    # The installer's printed escrow step (as bitcoin), docs/operator-guide.md's root `docker compose run ... sh` append
    # and its root `docker compose exec ... sed -i`: each must still edit /data/bitcoin.conf.
    docker exec -i -u bitcoin "$container" sh -c 'cat >> /data/bitcoin.conf' <<<'# A-536 probe: appended as bitcoin'
    run_options bitcoin "$prefix-bitcoin"
    docker run --rm --network none "${options[@]}" --entrypoint sh "${image[0]}" \
      -c 'echo "# A-536 probe: appended as root" >> /data/bitcoin.conf' || fail 'root cannot append to /data/bitcoin.conf'
    docker exec "$container" sed -i '/^# A-536 probe: appended as root$/d' /data/bitcoin.conf ||
      fail 'root cannot edit /data/bitcoin.conf with sed -i'
    [[ $(docker exec "$container" cat /data/bitcoin.conf) == '# A-536 probe: appended as bitcoin' ]] ||
      fail '/data/bitcoin.conf does not hold exactly the line appended as bitcoin'
    # docs/testnet-runbook.md's wallet backup (bitcoind writes it to /tmp, read out as bitcoin) and restore (written to
    # /tmp as bitcoin, then restorewallet).
    "${cli[@]}" -named createwallet wallet_name=a536-probe >/dev/null
    "${cli[@]}" -rpcwallet=a536-probe backupwallet /tmp/a536-probe.bak || fail 'bitcoind cannot back a wallet up to /tmp'
    docker exec -u bitcoin "$container" cat /tmp/a536-probe.bak >"$work/wallet.bak"
    docker exec -u bitcoin "$container" rm /tmp/a536-probe.bak
    [[ -s $work/wallet.bak ]] || fail 'the wallet backup is empty'
    docker exec -i -u bitcoin "$container" sh -c 'cat > /tmp/a536-restore.bak' <"$work/wallet.bak" ||
      fail 'the bitcoin user cannot write /tmp'
    "${cli[@]}" restorewallet a536-restored /tmp/a536-restore.bak >/dev/null || fail 'restorewallet from /tmp failed'
    docker exec -u bitcoin "$container" rm /tmp/a536-restore.bak
  fi
  echo "OK: bitcoind answers getblockcount $("${cli[@]}" getblockcount), running as $(runs_as "$container") ($when)"
  stop "$container"

  container="$prefix-monero-$run"
  start monero "$prefix-monero" "$container" --offline
  wait_for "$container" 120 logged "$container" 'restricted RPC server initialized OK'
  wait_for "$container" 30 logged "$container" 'core RPC server initialized OK'
  sleep 5
  running "$container" || fail 'monerod is no longer running'
  ! docker logs "$container" 2>&1 | grep -E 'Failed to bind|Error starting server|Permission denied' || fail 'monerod logged an error'
  echo "OK: monerod started both RPC servers, running as $(runs_as "$container") ($when)"
  stop "$container"

  container="$prefix-monero-escrow-wallet-$run"
  start monero-escrow-wallet "$prefix-monero-escrow-wallet" "$container"
  wait_for "$container" 60 logged "$container" 'Starting wallet RPC server'
  sleep 5
  running "$container" || fail 'monero-wallet-rpc is no longer running'
  # It writes nothing until a wallet is created: check that its user can write the wallet directory.
  docker exec "$container" touch /home/monero/wallet/.hardening-probe || fail 'the wallet directory is not writable'
  docker exec "$container" rm /home/monero/wallet/.hardening-probe
  echo "OK: monero-wallet-rpc started, running as $(runs_as "$container") ($when)"
  stop "$container"
done
echo 'OK: db, bitcoin, monero and monero-escrow-wallet start hardened on a new volume and again on the same volume'
