#!/usr/bin/env bash
# Exercise the resolved optional inference sandbox with a synthetic static
# process, without downloading a runtime/model or starting the marketplace.
# This proves the selected container boundary, not the safety of model bytes.
set -euo pipefail
cd "$(dirname "$0")/.."
for dependency in docker python3; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
moderation_go=${MODERATION_TEST_GO:-go}
"$moderation_go" version >/dev/null
docker_local=(env -u DOCKER_HOST -u DOCKER_CONTEXT -u DOCKER_TLS -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH
  docker --host=unix:///var/run/docker.sock)
"${docker_local[@]}" info >/dev/null
"${docker_local[@]}" compose version >/dev/null
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-moderation-isolation.XXXXXX")
image="opsecmkt-moderation-isolation:probe-$$-$RANDOM"
probe_container="opsecmkt-moderation-isolation-$$-$RANDOM"
cleanup() {
  local result=$?
  trap - EXIT
  "${docker_local[@]}" rm -f -v "$probe_container" >/dev/null 2>&1 || true
  if "${docker_local[@]}" image inspect "$image" >/dev/null 2>&1; then
    "${docker_local[@]}" run --rm --network none --read-only --user 0:0 --cap-drop ALL --cap-add CHOWN \
      --security-opt no-new-privileges:true --pids-limit 8 --memory 32m \
      --mount "type=bind,source=$work/socket,target=/run/moderation" \
      "$image" cleanup-test-socket >/dev/null 2>&1 || true
  fi
  "${docker_local[@]}" image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP
mkdir "$work/socket"
printf '%s\n' 'synthetic-public-weights-fixture' >"$work/model.gguf"
chmod 644 "$work/model.gguf"
printf '%s\n' 'synthetic-host-secret-not-production' >"$work/not-mounted-secret"
chmod 600 "$work/not-mounted-secret"
GOTOOLCHAIN=local CGO_ENABLED=0 "$moderation_go" build -trimpath -o "$work/probe" ./deploy/moderation/probe
chmod 0555 "$work/probe"
cat >"$work/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY probe /probe
ENTRYPOINT ["/probe"]
DOCKERFILE
BUILDX_CONFIG="$work/buildx" "${docker_local[@]}" build --network none --pull=false -q -t "$image" "$work" >/dev/null
"${docker_local[@]}" run --rm --network none --read-only --user 0:0 --cap-drop ALL --cap-add CHOWN \
  --security-opt no-new-privileges:true --pids-limit 8 --memory 32m \
  --mount "type=bind,source=$work/socket,target=/run/moderation" \
  "$image" prepare-test-socket
weights_sha=$(sha256sum "$work/model.gguf")
weights_sha=${weights_sha%% *}
MODERATION_RUNTIME_IMAGE="localhost/moderation-test@sha256:$(printf '%064d' 0)" \
MODERATION_WEIGHTS_SHA256="$weights_sha" MODERATION_WEIGHTS_FILE="$work/model.gguf" \
MODERATION_SOCKET_DIR="$work/socket" \
  "${docker_local[@]}" compose --env-file /dev/null -f deploy/moderation/compose.yaml \
  --profile moderation-inference config --format json >"$work/config.json"
# The test helper prepared ownership inside Docker's namespace. Do not run the
# production host preflight in this outer namespace: nested/user-mapped test
# environments can report that same UID as unmapped. The process probe below
# checks the identity and socket access inside the actual sandbox.
# Map the checked configuration to docker run so this test cannot weaken a
# sandbox control independently of the deployment recipe.
python3 -I - "$work/config.json" "$work/options" <<'PY'
import json, sys
service = json.load(open(sys.argv[1]))['services']['moderation-inference']
assert service['network_mode'] == 'none' and not service.get('ports')
assert service['read_only'] and service['cap_drop'] == ['ALL']
assert service['security_opt'] == ['no-new-privileges:true']
assert service['user'] == '65532:65532'
options = ['--network', service['network_mode'], '--user', service['user'], '--read-only']
for value in service['cap_drop']:
    options += ['--cap-drop', value]
for value in service['security_opt']:
    options += ['--security-opt', value]
for key, flag in [('pids_limit', '--pids-limit'), ('cpus', '--cpus'), ('mem_limit', '--memory'), ('memswap_limit', '--memory-swap')]:
    options += [flag, str(service[key])]
for name, limits in service['ulimits'].items():
    if isinstance(limits, int):
        soft = hard = limits
    else:
        soft, hard = limits.get('soft', 0), limits.get('hard', 0)
    options += ['--ulimit', f'{name}={soft}:{hard}']
for value in service['tmpfs']:
    options += ['--tmpfs', value]
for name, value in service['environment'].items():
    options += ['--env', f'{name}={value}']
for volume in service['volumes']:
    assert volume['type'] == 'bind'
    mount = f"type=bind,source={volume['source']},target={volume['target']}"
    if volume.get('read_only'):
        mount += ',readonly'
    options += ['--mount', mount]
with open(sys.argv[2], 'w') as destination:
    destination.write(''.join(value+'\0' for value in options))
PY
options=()
while IFS= read -r -d '' option; do options+=("$option"); done <"$work/options"
# A host credential is deliberately present in the CLI environment. It must
# never appear in the launched inference process environment.
MODERATION_TEST_SECRET=synthetic-not-production \
  "${docker_local[@]}" run --rm --name "$probe_container" "${options[@]}" "$image" "$work/not-mounted-secret"
