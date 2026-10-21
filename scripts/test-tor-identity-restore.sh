#!/usr/bin/env bash
# shellcheck disable=SC2031  # only the log-driver check's subshell sources the installer, whose functions set project
# Run the operator guide's onion-identity and .env backup/restore commands verbatim (war-room A-136): the
# fenced blocks after <!-- ops-cmd: env-backup -->, env-restore, tor-identity-backup and tor-identity-restore
# in docs/operator-guide.md, whose restore block runs ./scripts/install.sh restore-onion. It also feeds that restore
# archives it must refuse (war-room A-531): a forged hostname, an extra file, a path outside marketplace/, a symbolic
# or hard link, a duplicate, a compressed archive, oversized files, a mismatched key pair or recorded address, a missing
# address, a running mirror, another onion's key (compared by key, before any Tor started), the administrator onion, and
# a populated volume; none leaves a volume behind. It also checks that the containers carrying key bytes keep no logs. Every container runs with --network none. Requires Docker with Compose v2, the
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
  AGE_RECIPIENT AGE_IDENTITY identity backup address
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
mkdir -p "$deployment/scripts"
cp compose*.yaml Dockerfile "$deployment/"
cp scripts/install.sh "$deployment/scripts/"  # the restore block runs ./scripts/install.sh restore-onion (A-531)
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
# The installer's private temporary directories (the staged archive and key copies) go here; it must be left empty.
stage="$work/tmp"
mkdir "$stage"

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
  if run "$deployment" "$tor_restore" TMPDIR="$stage" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt" \
    address="$hostname" > "$work/refused.log" 2>&1; then fail "$identity restore accepted a non-empty volume"; fi
  grep -qF "Refusing: volume $volume is not empty" "$work/refused.log" ||
    fail "$identity restore did not say why it refused: $(cat "$work/refused.log")"

  # Lose the volume, restore it, and Tor serves the same address with the same key.
  docker volume rm "$volume" >/dev/null
  output=$(run "$deployment" "$tor_restore" TMPDIR="$stage" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt" \
    address="$hostname")
  [[ $(tail -n 1 <<< "$output") == "$hostname" ]] || fail "$identity restore printed '$output', expected $hostname"
  start_tor "$volume"
  [[ $(identity_of "$started") == "$original" ]] || fail "$identity: Tor did not keep the restored identity"
  stop_tor "$started"
  echo "ok: $identity backed up, volume removed, restored, Tor started with $hostname"
done

# A-531: the restore validates before it writes. Every case below targets the mirror's volume, removed first, with an
# archive made from the mirror's backup (or the main one's), and must be refused with no volume left behind.
main_file=$(cd "$deployment" && ls "backups/${project}_tor_identity"-*.tar.age)
mirror_file=$(cd "$deployment" && ls "backups/${project}_tor_mirror_identity"-*.tar.age)
# onion_in BACKUP: the hostname a backup holds, read by the Tor image's tar from this test's own archive.
onion_in() {
  age -d -i "$work/identity.txt" "$deployment/$1" |
    docker run --rm -i --network none --entrypoint tar "$project-tor" -xOf - marketplace/hostname
}
main_onion=$(onion_in "$main_file")
mirror_onion=$(onion_in "$mirror_file")
[[ $main_onion =~ ^[a-z2-7]{56}\.onion$ && $mirror_onion =~ ^[a-z2-7]{56}\.onion$ && $main_onion != "$mirror_onion" ]] ||
  fail "the backups name $main_onion and $mirror_onion"
# The main identity's public key (public data), for a mismatched pair.
main_public=$(age -d -i "$work/identity.txt" "$deployment/$main_file" |
  docker run --rm -i --network none --entrypoint sh "$project-tor" -euc 'tar -xOf - marketplace/hs_ed25519_public_key | base64 -w 0')
# craft NAME SCRIPT: the mirror's backup unpacked by the Tor image's tar in a scratch directory (this test's own
# archive, never operator input), changed by SCRIPT, which writes the new archive to stdout; encrypted as NAME.
craft() {
  age -d -i "$work/identity.txt" "$deployment/$mirror_file" |
    docker run --rm -i --network none --user 0 -e main_public="$main_public" --entrypoint sh "$project-tor" -euc "
      mkdir /tmp/c && cd /tmp/c && tar -xf - && { $2; }" |
    age -r "$recipient" -o "$work/$1.tar.age"
}
mirror_volume="${project}_tor_mirror_identity"
docker volume rm "$mirror_volume" >/dev/null
# refused NAME BACKUP MESSAGE [NAME=VALUE...]: restoring BACKUP into the mirror's volume fails, says MESSAGE, and
# leaves no volume and no staged file.
refused() {
  local name=$1 file=$2 message=$3
  shift 3
  if run "$deployment" "$tor_restore" TMPDIR="$stage" identity=tor_mirror_identity backup="$file" \
    AGE_IDENTITY="$work/identity.txt" address="$mirror_onion" "$@" > "$work/$name.log" 2>&1; then
    fail "the restore accepted $name"
  fi
  grep -qF -- "$message" "$work/$name.log" || fail "$name: expected '$message' in: $(cat "$work/$name.log")"
  if docker volume inspect "$mirror_volume" >/dev/null 2>&1; then fail "$name: the refused restore created $mirror_volume"; fi
  [[ -z $(ls -A "$stage") ]] || fail "$name: the restore left $(ls -A "$stage") in its temporary directory"
  echo "ok: refused $name"
}
craft forged-hostname "printf '%s\n' $main_onion > marketplace/hostname; tar -cf - marketplace"
refused forged-hostname "$work/forged-hostname.tar.age" \
  "the backup's hostname file names $main_onion, but its key is the identity of $mirror_onion"
craft garbage-hostname 'printf "not an address\n" > marketplace/hostname; tar -cf - marketplace'
refused garbage-hostname "$work/garbage-hostname.tar.age" "hostname file names no valid onion address"
craft extra-file 'printf x > marketplace/torrc; tar -cf - marketplace'
refused extra-file "$work/extra-file.tar.age" '- "marketplace/torrc"'
craft outside-file 'printf x > other; tar -cf - marketplace other'
refused outside-file "$work/outside-file.tar.age" '- "other"'
craft client-file 'printf x > marketplace/authorized_clients/a.auth; tar -cf - marketplace'
refused client-file "$work/client-file.tar.age" '- "marketplace/authorized_clients/a.auth"'
craft traversal 'tar -P -cf - --transform "s,^marketplace/hostname\$,../marketplace/hostname," marketplace'
refused traversal "$work/traversal.tar.age" '- "../marketplace/hostname"'
craft absolute 'tar -P -cf - --transform "s,^marketplace/hostname\$,/var/lib/tor/marketplace/hostname," marketplace'
refused absolute "$work/absolute.tar.age" '- "/var/lib/tor/marketplace/hostname"'
craft symlink 'rm marketplace/hostname; ln -s /etc/passwd marketplace/hostname; tar -cf - marketplace'
refused symlink "$work/symlink.tar.age" 'l "marketplace/hostname"'
craft hardlink 'rm marketplace/hostname; ln marketplace/hs_ed25519_secret_key marketplace/hostname; tar -cf - marketplace'
refused hardlink "$work/hardlink.tar.age" 'h "marketplace/'
craft duplicate 'tar -cf /tmp/a marketplace; tar -cf /tmp/b marketplace/hs_ed25519_secret_key; tar -Af /tmp/a /tmp/b; cat /tmp/a'
refused duplicate "$work/duplicate.tar.age" 'The backup holds an entry twice.'
craft admin-layout 'mv marketplace admin; tar -cf - admin'
refused admin-layout "$work/admin-layout.tar.age" 'd "admin/"'
craft no-secret 'rm marketplace/hs_ed25519_secret_key; tar -cf - marketplace'
refused no-secret "$work/no-secret.tar.age" 'The backup holds no marketplace/hs_ed25519_secret_key.'
craft short-key 'head -c 95 marketplace/hs_ed25519_secret_key > k; mv k marketplace/hs_ed25519_secret_key; tar -cf - marketplace'
refused short-key "$work/short-key.tar.age" 'is 95 bytes; a Tor v3 onion secret key is exactly 96 bytes'
# shellcheck disable=SC2016  # expanded in the container, from its environment
craft mismatched-pair 'printf %s "$main_public" | base64 -d > marketplace/hs_ed25519_public_key; tar -cf - marketplace'
refused mismatched-pair "$work/mismatched-pair.tar.age" 'hs_ed25519_public_key does not match hs_ed25519_secret_key'
refused mismatched-address "$deployment/$mirror_file" "the key derives $mirror_onion, not $main_onion" address="$main_onion"
craft no-hostname 'rm marketplace/hostname; tar -cf - marketplace'
refused no-address "$deployment/$mirror_file" 'set address to the onion address you published' address=
craft long-hostname 'head -c 2000 /dev/zero | tr "\0" a > marketplace/hostname; tar -cf - marketplace'
refused long-hostname "$work/long-hostname.tar.age" 'marketplace/hostname is larger than 64 bytes.'
craft long-key 'head -c 5000 /dev/zero > marketplace/hs_ed25519_secret_key; tar -cf - marketplace'
refused long-key "$work/long-key.tar.age" 'marketplace/hs_ed25519_secret_key is larger than 96 bytes.'
craft gzip 'tar -czf - marketplace'
refused gzip "$work/gzip.tar.age" 'The backup is not an uncompressed tar archive.'
printf 'not a tar archive\n' | age -r "$recipient" -o "$work/not-tar.tar.age"
refused not-tar "$work/not-tar.tar.age" 'not an onion identity backup'
refused wrong-age-identity "$deployment/$mirror_file" 'nothing arrived on stdin' AGE_IDENTITY="$work/wrong-identity.txt"
# A running mirror (a container with the Compose labels of this project's tor-mirror) is refused.
running=$(docker run -d --network none --label "com.docker.compose.project=$project" \
  --label com.docker.compose.service=tor-mirror --entrypoint sleep "$project-tor" 300)
containers+=("$running")
refused running-mirror "$deployment/$mirror_file" 'Refusing: tor-mirror is running'
docker rm -f "$running" >/dev/null  # no -v: the Tor image declares no VOLUME
# Public/admin mix-ups, compared by key, not hostname: the main backup restored into tor_identity and, before any Tor
# starts (so no hostname exists yet), into the mirror's volume; the administrator onion's volume as a target; an
# administrator volume prepared with the mirror's key but never started; and one whose key cannot be read as a file.
docker volume rm "${project}_tor_identity" >/dev/null
run "$deployment" "$tor_restore" TMPDIR="$stage" identity=tor_identity backup="$deployment/$main_file" \
  AGE_IDENTITY="$work/identity.txt" address="$main_onion" > /dev/null
refused main-key-as-mirror "$deployment/$main_file" "that key is the identity already in ${project}_tor_identity ($main_onion)" \
  address="$main_onion"
if run "$deployment" "$tor_restore" TMPDIR="$stage" identity=tor_admin_identity backup="$deployment/$mirror_file" \
  AGE_IDENTITY="$work/identity.txt" address="$mirror_onion" > "$work/admin.log" 2>&1; then
  fail 'the restore accepted tor_admin_identity'
fi
grep -q 'The private administrator' "$work/admin.log" || fail "tor_admin_identity: $(cat "$work/admin.log")"
if docker volume inspect "${project}_tor_admin_identity" >/dev/null 2>&1; then fail 'a tor_admin_identity volume was created'; fi
echo 'ok: refused tor_admin_identity'
age -d -i "$work/identity.txt" "$deployment/$mirror_file" |
  docker run --rm -i --log-driver none --network none --user 0 -v "${project}_tor_admin_identity:/var/lib/tor" \
    --entrypoint sh "$project-tor" -euc \
    'mkdir -m 700 /var/lib/tor/admin && tar -xOf - marketplace/hs_ed25519_secret_key > /var/lib/tor/admin/hs_ed25519_secret_key
     chown -R debian-tor:debian-tor /var/lib/tor; chmod 600 /var/lib/tor/admin/hs_ed25519_secret_key'
refused mirror-key-in-admin "$deployment/$mirror_file" "that key is the identity already in ${project}_tor_admin_identity"
docker run --rm --network none --user 0 -v "${project}_tor_admin_identity:/var/lib/tor" --entrypoint sh "$project-tor" -euc \
  'rm /var/lib/tor/admin/hs_ed25519_secret_key; mkdir /var/lib/tor/admin/hs_ed25519_secret_key'
refused unreadable-admin-key "$deployment/$mirror_file" "could not compare the key with the one in ${project}_tor_admin_identity"
docker volume rm "${project}_tor_admin_identity" >/dev/null

# No log driver keeps the key bytes: the archive member and key-comparison containers, rerun here without --rm,
# are configured with --log-driver none and docker logs has nothing from them.
age -d -i "$work/identity.txt" "$deployment/$mirror_file" > "$work/plain.tar"
logged=0
(
  # shellcheck source=scripts/install.sh
  source scripts/install.sh
  # shellcheck disable=SC2329  # called by the installer's functions below
  docker() {
    local argument arguments=()
    if [[ $1 != run ]]; then command docker "$@"; return; fi
    shift
    for argument in "$@"; do [[ $argument == --rm ]] || arguments+=("$argument"); done
    command docker run --cidfile "$work/cid-$((++runs))" "${arguments[@]}"
  }
  runs=0
  restore_stage=$work/log-stage
  mkdir "$restore_stage"
  cp "$work/plain.tar" "$restore_stage/archive"
  onion_archive_member "$project-tor" hs_ed25519_secret_key 96 > "$restore_stage/secret"
  [[ $(wc -c < "$restore_stage/secret") -eq 96 ]] || fail 'the archive member is not the 96-byte key'
  # shellcheck disable=SC2034  # onion_key_in_volume streams onion_files[0]
  onion_files=("$restore_stage/secret")
  status=0
  onion_key_in_volume "$project-tor" "${project}_tor_identity" || status=$?
  [[ $status == 1 ]] || fail "the key comparison returned $status, expected 1 (a different key)"
) || logged=$?
# Every container it created goes to the exit trap first, whatever failed.
for cid in "$work"/cid-*; do
  if [[ -s $cid ]]; then containers+=("$(cat "$cid")"); fi
done
((logged == 0)) || fail 'the log-driver check failed (the message above says why)'
inspected=0
for cid in "$work"/cid-*; do
  inspected=$((inspected + 1))
  container=$(cat "$cid")
  [[ $(docker inspect -f '{{.HostConfig.LogConfig.Type}}' "$container") == none ]] || fail "container $container keeps logs"
  [[ -z $(docker logs "$container" 2>/dev/null) ]] || fail "docker logs shows output of $container"
  docker rm -f "$container" >/dev/null  # no -v: the Tor image declares no VOLUME; named volumes go in the trap
done
((inspected == 2)) || fail 'expected two containers to inspect'
rm -rf "$work"/cid-* "$work/log-stage" "$work/plain.tar"
echo 'ok: the containers that carry key bytes run with --log-driver none, and docker logs holds nothing of them'

# A backup without hostname restores with the recorded address. The restore writes no hostname (Tor writes it from the
# key), only the two key files, owned by debian-tor; Tor then starts with the mirror's identity.
output=$(run "$deployment" "$tor_restore" TMPDIR="$stage" identity=tor_mirror_identity backup="$work/no-hostname.tar.age" \
  AGE_IDENTITY="$work/identity.txt" address="$mirror_onion")
[[ $(tail -n 1 <<< "$output") == "$mirror_onion" ]] || fail "the restore without hostname printed '$output'"
[[ -z $(ls -A "$stage") ]] || fail "the restore left $(ls -A "$stage") in its temporary directory"
docker run --rm --network none --user 0 -v "$mirror_volume:/var/lib/tor:ro" --entrypoint sh "$project-tor" -euc \
  'cd /var/lib/tor && find . | sort && stat -c "%n %U:%G %a" . marketplace marketplace/hs_ed25519_*' > "$work/restored.txt"
for line in . ./marketplace ./marketplace/hs_ed25519_public_key ./marketplace/hs_ed25519_secret_key \
  '. debian-tor:debian-tor 700' 'marketplace debian-tor:debian-tor 700' \
  'marketplace/hs_ed25519_public_key debian-tor:debian-tor 600' 'marketplace/hs_ed25519_secret_key debian-tor:debian-tor 600'; do
  grep -qxF "$line" "$work/restored.txt" || fail "restored files: expected '$line' in: $(cat "$work/restored.txt")"
done
[[ $(wc -l < "$work/restored.txt") -eq 8 ]] || fail "the restore wrote other files: $(cat "$work/restored.txt")"
start_tor "$mirror_volume"
[[ $(docker exec "$started" cat /var/lib/tor/marketplace/hostname) == "$mirror_onion" ]] ||
  fail 'Tor did not serve the mirror identity restored from a backup without hostname'
stop_tor "$started"
echo "ok: a backup without hostname restored with its recorded address, only the keys written; Tor started with $mirror_onion"

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
output=$(run "$deployment" "$tor_restore" TMPDIR="$stage" identity="$identity" backup="$work/root-owned.tar.age" \
  AGE_IDENTITY="$work/identity.txt" address="$hostname")
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
if run "$deployment" "$tor_restore" TMPDIR="$stage" identity="$identity" backup="$file" AGE_IDENTITY="$work/identity.txt" \
  address="$hostname" > "$work/no-image-restore.log" 2>&1; then fail 'the restore ran without the Tor image'; fi
grep -qi 'no such image' "$work/no-image-restore.log" || fail "restore without the image: $(cat "$work/no-image-restore.log")"
echo 'ok: without the Tor image, backup and restore fail and pull nothing'
echo 'PASS: documented .env and onion identity backup and restore'
