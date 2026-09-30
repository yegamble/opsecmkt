#!/usr/bin/env bash
# Run the operator guide's onion-identity and .env backup/restore commands verbatim (war-room A-136): the
# fenced blocks after <!-- ops-cmd: env-backup -->, env-restore, tor-identity-backup and tor-identity-restore
# in docs/operator-guide.md. Every container runs with --network none. Requires Docker with Compose v2, the
# Tor image (TOR_IMAGE, default opsecmkt-tor:ci: docker build -t opsecmkt-tor:ci -f deploy/Dockerfile.tor deploy),
# age, age-keygen and OpenSSL. Works in a temporary copy of the Compose files under a unique project name, so
# the checkout's .env and volumes are never used; the exit trap removes every container, volume and image tag
# it created. Prints onion hostnames (public) but never key material.
set -euo pipefail
cd "$(dirname "$0")/.."
image=${TOR_IMAGE:-opsecmkt-tor:ci}
for dependency in docker age age-keygen openssl; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
docker image inspect "$image" >/dev/null 2>&1 || {
  echo "Missing Tor image $image: docker build -t $image -f deploy/Dockerfile.tor deploy" >&2; exit 1; }
# Only the variables each step sets below may reach the documented commands.
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES DATABASE_URL SETUP_TOKEN POSTGRES_PASSWORD \
  AGE_RECIPIENT AGE_IDENTITY identity backup
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-tor-restore.XXXXXX")
project="opsecmkt-tor-restore-$(openssl rand -hex 6)"
containers=()
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  local status=$? container volume
  trap - EXIT
  for container in ${containers[@]+"${containers[@]}"}; do
    docker container inspect "$container" >/dev/null 2>&1 || continue  # already removed by stop_tor
    ((status == 0)) || { echo "--- logs of $container ---" >&2; docker logs "$container" 2>&1 | tail -n 20 >&2 || true; }
    docker rm -f "$container" >/dev/null 2>&1 || true  # no -v: the Tor image declares no VOLUME; the named volumes go below
  done
  for volume in $(docker volume ls -q --filter "name=^${project}_"); do docker volume rm -f "$volume" >/dev/null || true; done
  docker image rm "$project-tor" >/dev/null 2>&1 || true  # only this tag; $image stays
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP  # interrupted runs clean up through the EXIT trap too

# block NAME: the fenced block after <!-- ops-cmd: NAME --> in the operator guide.
block() {
  local text
  text=$(awk -v marker="<!-- ops-cmd: $1 -->" '
    $0 == marker { marked = 1; next }
    marked && /^```/ { if (inside) exit; inside = 1; next }
    inside { print }
  ' docs/operator-guide.md)
  [[ -n $text ]] || fail "docs/operator-guide.md has no <!-- ops-cmd: $1 --> block"
  printf '%s\n' "$text"
}
env_backup=$(block env-backup)
env_restore=$(block env-restore)
tor_backup=$(block tor-identity-backup)
tor_restore=$(block tor-identity-restore)
# run DIRECTORY BLOCK [NAME=VALUE...]: run a documented block verbatim with bash in DIRECTORY (a deployment
# checkout), with only the named shell variables set, as an operator would set them first.
run() {
  local directory=$1 text=$2
  shift 2
  (cd "$directory" && env "$@" bash -c "$text")
}

# The deployment: a Tor install as scripts/install.sh writes it, with the mirror, under a unique project name.
deployment="$work/deployment"
mkdir -p "$deployment"
cp compose*.yaml Dockerfile "$deployment/"
cp -R deploy "$deployment/"
cat > "$deployment/.env" <<ENV
DATABASE_URL='postgres://opsecmkt:tor-restore-check@db:5432/opsecmkt?sslmode=disable'
POSTGRES_PASSWORD='tor-restore-check'
SETUP_TOKEN='$(openssl rand -hex 32)'
AUDIT_SIGNING_KEY='$(openssl rand -hex 32)'
APP_MODE='tor'
COOKIE_SECURE='false'
APP_PORT='8080'
COMPOSE_PROJECT_NAME='$project'
COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml'
COMPOSE_PROFILES='internal-db,mirror'
ENV
chmod 600 "$deployment/.env"
[[ $(cd "$deployment" && docker compose config | sed -n 's/^name: //p') == "$project" ]] ||
  fail 'docker compose config does not name the test project'
# The image Compose would build for the tor service, from the prebuilt Tor image: nothing is built or pulled.
docker tag "$image" "$project-tor"
# A test-only age identity inside $work (removed by the trap).
age-keygen -o "$work/identity.txt" 2>/dev/null
recipient=$(age-keygen -y "$work/identity.txt")
age-keygen -o "$work/wrong-identity.txt" 2>/dev/null

# start_tor VOLUME: run Tor on VOLUME as compose.tor.yaml does (no capabilities, no new privileges), offline.
# Waits until it bootstraps (0% is all it reaches offline) and is still running after the onion keys loaded.
started=''
start_tor() {
  local container i logs
  container=$(docker run -d --network none --add-host app:127.0.0.1 --cap-drop ALL \
    --security-opt no-new-privileges:true -v "$1:/var/lib/tor" "$project-tor")
  containers+=("$container")
  # Logs are read into a variable: grep -q in a pipe can end docker logs early, which pipefail reports.
  for ((i = 0; i < 60; i++)); do
    logs=$(docker logs "$container" 2>&1)
    grep -q 'Bootstrapped 0%' <<< "$logs" && break
    [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || break
    sleep 0.5
  done
  sleep 3
  logs=$(docker logs "$container" 2>&1)
  [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || fail "Tor exited on $1: $logs"
  grep -q 'Bootstrapped 0%' <<< "$logs" || fail "Tor did not start on $1: $logs"
  if grep -q '\[err\]' <<< "$logs"; then fail "Tor logged an error on $1: $logs"; fi
  started=$container
}
# identity_of CONTAINER: the onion hostname and a digest of the secret key file (the key is never printed).
identity_of() {
  docker exec "$1" sh -c 'cat /var/lib/tor/marketplace/hostname; sha256sum < /var/lib/tor/marketplace/hs_ed25519_secret_key'
}
stop_tor() { docker rm -f "$1" >/dev/null; }  # no -v: the Tor image declares no VOLUME; Tor keeps its keys in a named volume
# list_archive [-v]: list the tar archive on stdin with the Tor image's GNU tar (host tar formats differ).
list_archive() { docker run --rm -i --network none --entrypoint tar "$project-tor" "-t${1:+v}f" -; }

# .env: back it up, then restore it into a second checkout that has none.
output=$(run "$deployment" "$env_backup" AGE_RECIPIENT="$recipient")
env_file=$(cd "$deployment" && ls backups/env-*.age)
[[ $(wc -l <<< "$env_file") -eq 1 ]] || fail ".env backup did not write exactly one file: $env_file"
[[ $output == *"Wrote $env_file"* ]] || fail ".env backup did not name its file: $output"
[[ $(stat -c %a "$deployment/$env_file" 2>/dev/null || stat -f %Lp "$deployment/$env_file") == 600 ]] ||
  fail ".env backup is not mode 600"
! grep -q SETUP_TOKEN "$deployment/$env_file" || fail '.env backup is not encrypted'
second="$work/second"
mkdir -p "$second"
if run "$second" "$env_restore" AGE_IDENTITY="$work/wrong-identity.txt" backup="$deployment/$env_file" \
  > "$work/env-wrong.log" 2>&1; then fail '.env restore accepted the wrong identity'; fi
[[ ! -e $second/.env ]] || fail '.env restore with the wrong identity left a .env'
run "$second" "$env_restore" AGE_IDENTITY="$work/identity.txt" backup="$deployment/$env_file"
cmp -s "$deployment/.env" "$second/.env" || fail 'the restored .env differs'
[[ $(stat -c %a "$second/.env" 2>/dev/null || stat -f %Lp "$second/.env") == 600 ]] || fail 'restored .env is not mode 600'
printf 'CHANGED=1\n' >> "$second/.env"
cp "$second/.env" "$work/changed.env"
if run "$second" "$env_restore" AGE_IDENTITY="$work/identity.txt" backup="$deployment/$env_file" \
  > "$work/env-exists.log" 2>&1; then fail '.env restore overwrote an existing .env'; fi
grep -q 'Refusing' "$work/env-exists.log" || fail ".env restore did not say why it refused: $(cat "$work/env-exists.log")"
cmp -s "$work/changed.env" "$second/.env" || fail '.env restore changed an existing .env'
echo 'ok: .env backed up encrypted (0600) and restored into a checkout without one; wrong identity and existing .env refused'

# Onion identities: the main onion and the mirror, each backed up, deleted and restored.
for identity in tor_identity tor_mirror_identity; do
  volume="${project}_$identity"
  start_tor "$volume"
  original=$(identity_of "$started")
  hostname=${original%%$'\n'*}
  [[ $hostname == *.onion ]] || fail "no onion hostname for $identity"
  stop_tor "$started"

  output=$(run "$deployment" "$tor_backup" identity="$identity" AGE_RECIPIENT="$recipient")
  file=$(cd "$deployment" && ls "backups/$volume"-*.tar.age)
  [[ $(wc -l <<< "$file") -eq 1 ]] || fail "$identity backup did not write exactly one file: $file"
  [[ $output == *"$hostname"* && $output == *"Wrote $file"* ]] || fail "$identity backup output: $output"
  [[ $(stat -c %a "$deployment/$file" 2>/dev/null || stat -f %Lp "$deployment/$file") == 600 ]] ||
    fail "$identity backup is not mode 600"
  members=$(age -d -i "$work/identity.txt" "$deployment/$file" | list_archive)
  grep -qx 'marketplace/hs_ed25519_secret_key' <<< "$members" || fail "$identity backup lacks the secret key: $members"

  # Restoring over the live identity is refused and changes nothing.
  if run "$deployment" "$tor_restore" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt" \
    > "$work/refused.log" 2>&1; then fail "$identity restore accepted a non-empty volume"; fi
  grep -q 'Refusing' "$work/refused.log" || fail "$identity restore did not say why it refused: $(cat "$work/refused.log")"

  # Lose the volume, restore it, and Tor serves the same address with the same key.
  docker volume rm "$volume" >/dev/null
  output=$(run "$deployment" "$tor_restore" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt")
  [[ $(tail -n 1 <<< "$output") == "$hostname" ]] || fail "$identity restore printed '$output', expected $hostname"
  start_tor "$volume"
  [[ $(identity_of "$started") == "$original" ]] || fail "$identity: Tor did not keep the restored identity"
  stop_tor "$started"
  echo "ok: $identity backed up, volume removed, restored, Tor started with $hostname"
done

# A root-owned archive (for example one taken by hand as root) restored into a volume created root-owned
# beforehand: the restore's chown and chmod must still leave Tor able to use it.
identity=tor_identity
volume="${project}_$identity"
file=$(cd "$deployment" && ls "backups/$volume"-*.tar.age)
start_tor "$volume"
original=$(identity_of "$started")
hostname=${original%%$'\n'*}
stop_tor "$started"
age -d -i "$work/identity.txt" "$deployment/$file" |
  docker run --rm -i --network none --user 0 --entrypoint sh "$project-tor" -euc '
    mkdir /tmp/copy && tar -C /tmp/copy -xf - && chown -R root:root /tmp/copy
    tar -C /tmp/copy -cf - marketplace' |
  age -r "$recipient" -o "$work/root-owned.tar.age"
owners=$(age -d -i "$work/identity.txt" "$work/root-owned.tar.age" | list_archive -v | awk '{ print $2 }' | sort -u)
[[ $owners == root/root ]] || fail "the root-owned archive has owners $owners"
docker volume rm "$volume" >/dev/null
docker volume create "$volume" >/dev/null
docker run --rm --network none --user 0 -v "$volume:/mnt" --entrypoint sh "$project-tor" -euc \
  'chown root:root /mnt; chmod 755 /mnt'
output=$(run "$deployment" "$tor_restore" identity="$identity" backup="$work/root-owned.tar.age" \
  AGE_IDENTITY="$work/identity.txt")
[[ $(tail -n 1 <<< "$output") == "$hostname" ]] || fail "root-owned restore printed '$output', expected $hostname"
start_tor "$volume"
[[ $(identity_of "$started") == "$original" ]] || fail 'Tor did not keep the identity restored from a root-owned archive'
stop_tor "$started"
echo "ok: root-owned archive restored into a root-owned volume, Tor started with $hostname"

# Without the project's Tor image, both commands fail instead of pulling an image of that name from a registry.
docker image rm "$project-tor" >/dev/null
before=$(cd "$deployment" && ls backups)
if run "$deployment" "$tor_backup" identity="$identity" AGE_RECIPIENT="$recipient" > "$work/no-image.log" 2>&1; then
  fail 'the backup ran without the Tor image'
fi
grep -qi 'no such image' "$work/no-image.log" || fail "backup without the image: $(cat "$work/no-image.log")"
[[ $(cd "$deployment" && ls backups) == "$before" ]] || fail 'the failed backup left a file'
docker volume rm "$volume" >/dev/null
if run "$deployment" "$tor_restore" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt" \
  > "$work/no-image-restore.log" 2>&1; then fail 'the restore ran without the Tor image'; fi
grep -qi 'no such image' "$work/no-image-restore.log" || fail "restore without the image: $(cat "$work/no-image-restore.log")"
echo 'ok: without the Tor image, backup and restore fail and pull nothing'
echo 'PASS: documented .env and onion identity backup and restore'
