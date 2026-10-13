#!/usr/bin/env bash
# A-526: the installer's onion identity import (init --onion-key-dir, add-mirror --mirror-onion-key-dir) against the
# real Tor image, offline. It sources scripts/install.sh for the functions init and add-mirror call, and, with
# networking disabled in every container: generates two identities with Tor, each laid out as mkp224o writes one
# (hostname, hs_ed25519_public_key, hs_ed25519_secret_key; the hostname forged, since the installer must ignore it),
# imports them into scratch volumes (one with its public key, one without), starts Tor on each volume as
# compose.tor.yaml runs it and compares the address and a digest of the secret key. A 95-byte copy and a mismatched
# pair are refused before any volume is written, Tor itself refuses a wrong header in the dry run, and a volume Tor has
# used is never overwritten. Requires Docker and the Tor image (TOR_IMAGE, default opsecmkt-tor:ci: docker build -t
# opsecmkt-tor:ci -f deploy/Dockerfile.tor deploy). The exit trap removes every container and volume it created.
# Prints onion hostnames (public), never key material.
set -euo pipefail
cd "$(dirname "$0")/.."
image=${TOR_IMAGE:-opsecmkt-tor:ci}
command -v docker >/dev/null || { echo 'Missing dependency: docker' >&2; exit 1; }
docker image inspect "$image" >/dev/null 2>&1 || {
  echo "Missing Tor image $image: docker build -t $image -f deploy/Dockerfile.tor deploy" >&2; exit 1; }
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-tor-import.XXXXXX")
project="opsecmkt-tor-import-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
containers=()
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  local status=$? container volume
  trap - EXIT
  for container in ${containers[@]+"${containers[@]}"}; do
    docker container inspect "$container" >/dev/null 2>&1 || continue
    ((status == 0)) || { echo "--- logs of $container ---" >&2; docker logs "$container" 2>&1 | tail -n 20 >&2 || true; }
    docker rm -f "$container" >/dev/null 2>&1 || true  # no -v: the Tor image declares no VOLUME; the named volumes go below
  done
  for volume in $(docker volume ls -q --filter "name=^${project}_"); do docker volume rm -f "$volume" >/dev/null || true; done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP  # interrupted runs clean up through the EXIT trap too

# generate DIR: a new identity from Tor, offline, in DIR as mkp224o lays one out (the archive is this script's own).
generate() {
  mkdir -p "$1"
  docker run --rm --network none --cap-drop ALL --security-opt no-new-privileges:true --tmpfs /gen --entrypoint sh \
    "$image" -euc '
      umask 077
      mkdir /gen/data /gen/data/hs
      : > /gen/torrc
      tor -f /gen/torrc --DisableNetwork 1 --SocksPort 0 --DataDirectory /gen/data --HiddenServiceDir /gen/data/hs \
        --HiddenServicePort "80 127.0.0.1:8080" > /gen/log 2>&1 &
      i=0
      while [ ! -s /gen/data/hs/hostname ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
      kill "$!" 2>/dev/null || true
      wait "$!" 2>/dev/null || true
      tar -C /gen/data/hs -cf - hostname hs_ed25519_public_key hs_ed25519_secret_key' | tar -C "$1" -xf -
  [[ -s $1/hostname && -s $1/hs_ed25519_secret_key && -s $1/hs_ed25519_public_key ]] || fail "Tor generated no identity in $1"
}
# digest FILE: the SHA-256 of FILE, from the Tor image's sha256sum (the key is never printed).
digest() { docker run --rm -i --network none --entrypoint sha256sum "$image" < "$1" | cut -d ' ' -f 1; }
# import_identity DIR NAME: what init does with --onion-key-dir DIR (onion_key_check, onion_dry_run, then onion_volume_create and
# onion_import), into the project's volume NAME; prints the address the dry run derived. In a subshell that sources the
# installer, whose own exit trap removes its staged key copy.
import_identity() {
  (
    # shellcheck source=scripts/install.sh
    source scripts/install.sh
    onion_key_check "$1"
    address=$(onion_dry_run "$image") || exit 1
    onion_volume_create "$project" "$2"
    onion_import "$image" "${project}_$2"
    printf '%s\n' "$address"
  )
}
# start_tor VOLUME: run Tor on VOLUME as compose.tor.yaml does (no capabilities, no new privileges), offline, until it
# has written the hostname; prints the hostname and the secret key's digest.
start_tor() {
  local container i
  container=$(docker run -d --network none --add-host app:127.0.0.1 --cap-drop ALL \
    --security-opt no-new-privileges:true -v "$1:/var/lib/tor" "$image")
  containers+=("$container")
  for ((i = 0; i < 100; i++)); do
    docker exec "$container" test -s /var/lib/tor/marketplace/hostname 2>/dev/null && break
    [[ $(docker inspect -f '{{.State.Running}}' "$container") == true ]] || fail "Tor exited on $1: $(docker logs "$container" 2>&1)"
    sleep 0.2
  done
  docker exec "$container" sh -c 'cat /var/lib/tor/marketplace/hostname; sha256sum < /var/lib/tor/marketplace/hs_ed25519_secret_key | cut -d " " -f 1'
  docker rm -f "$container" >/dev/null  # no -v: the Tor image declares no VOLUME; Tor keeps its keys in a named volume
}
# owners VOLUME: owner, group and mode of the identity directory and its key files.
owners() {
  docker run --rm --network none --user 0 --entrypoint sh -v "$1:/var/lib/tor:ro" "$image" -c \
    'cd /var/lib/tor && stat -c "%n %U:%G %a" . marketplace marketplace/hs_ed25519_*'
}
volume_exists() { docker volume inspect "${project}_$1" >/dev/null 2>&1; }

generate "$work/main"
generate "$work/mirror"
main=$(cat "$work/main/hostname")
mirror=$(cat "$work/mirror/hostname")
[[ $main != "$mirror" ]] || fail 'Tor generated the same identity twice'
# The installer ignores hostname: forge both.
printf '%s\n' "$mirror" > "$work/main/hostname"
printf '%s\n' "$main" > "$work/mirror/hostname"

# The main identity, with its public key, into tor_identity.
printed=$(import_identity "$work/main" tor_identity)
[[ $printed == "$main" ]] || fail "the dry run derived $printed, expected $main"
[[ $(docker volume inspect -f '{{index .Labels "com.docker.compose.project"}} {{index .Labels "com.docker.compose.volume"}}' \
  "${project}_tor_identity") == "$project tor_identity" ]] || fail 'the volume lacks the Compose labels'
expected=$(owners "${project}_tor_identity")
for line in '. debian-tor:debian-tor 700' 'marketplace debian-tor:debian-tor 700' \
  'marketplace/hs_ed25519_public_key debian-tor:debian-tor 600' 'marketplace/hs_ed25519_secret_key debian-tor:debian-tor 600'; do
  grep -qxF "$line" <<< "$expected" || fail "imported files: expected '$line' in: $expected"
done
[[ $(start_tor "${project}_tor_identity") == "$main"$'\n'"$(digest "$work/main/hs_ed25519_secret_key")" ]] ||
  fail 'Tor did not serve the imported main identity'
echo "ok: identity with its public key imported (hostname forged and ignored); Tor started with $main and the same key"

# The mirror identity, secret key only, into tor_mirror_identity.
rm "$work/mirror/hs_ed25519_public_key"
printed=$(import_identity "$work/mirror" tor_mirror_identity)
[[ $printed == "$mirror" ]] || fail "the dry run derived $printed, expected $mirror"
[[ $(start_tor "${project}_tor_mirror_identity") == "$mirror"$'\n'"$(digest "$work/mirror/hs_ed25519_secret_key")" ]] ||
  fail 'Tor did not serve the imported mirror identity'
echo "ok: secret key alone imported; Tor started with $mirror and the same key"

# A truncated key (Tor would silently create a new identity from it) is refused before any volume exists.
mkdir "$work/short"
head -c 95 "$work/main/hs_ed25519_secret_key" > "$work/short/hs_ed25519_secret_key"
if import_identity "$work/short" short > "$work/short.log" 2>&1; then fail 'a 95-byte secret key was imported'; fi
grep -q 'is 95 bytes; a Tor v3 onion secret key is exactly 96 bytes' "$work/short.log" || fail "95-byte key: $(cat "$work/short.log")"
! volume_exists short || fail 'a volume was created for a 95-byte key'
echo 'ok: a 95-byte secret key is refused before any volume is written'

# A secret key with another identity's public key: Tor refuses the pair in the dry run.
mkdir "$work/mixed"
cp "$work/main/hs_ed25519_secret_key" "$work/mixed/"
generate "$work/other"
cp "$work/other/hs_ed25519_public_key" "$work/mixed/"
if import_identity "$work/mixed" mixed > "$work/mixed.log" 2>&1; then fail 'a mismatched key pair was imported'; fi
grep -q 'hs_ed25519_public_key does not match hs_ed25519_secret_key' "$work/mixed.log" || fail "mismatched pair: $(cat "$work/mixed.log")"
! volume_exists mixed || fail 'a volume was created for a mismatched pair'
echo 'ok: a mismatched key pair is refused by Tor in the dry run before any volume is written'

# Past the installer's own header check, Tor in the dry run refuses a wrong header too (and writes no address).
mkdir "$work/header"
{ printf '== ed25519v1-secret: type1 ==\0\0\0'; tail -c 64 "$work/main/hs_ed25519_secret_key"; } > "$work/header/key"
# shellcheck source=scripts/install.sh disable=SC2034  # onion_files is what onion_dry_run streams
if (source scripts/install.sh; onion_files=("$work/header/key"); onion_dry_run "$image") > "$work/header.log" 2>&1; then
  fail "the dry run accepted a wrong header: $(cat "$work/header.log")"
fi
grep -q 'Tor did not accept the key' "$work/header.log" || fail "wrong header: $(cat "$work/header.log")"
echo 'ok: the dry run refuses a key Tor does not load'

# A volume Tor has used is never overwritten.
before=$(start_tor "${project}_tor_identity")
# shellcheck source=scripts/install.sh
if (source scripts/install.sh; onion_key_check "$work/mirror"; onion_import "$image" "${project}_tor_identity") \
  > "$work/used.log" 2>&1; then fail 'an import overwrote a used volume'; fi
grep -q "Refusing: volume ${project}_tor_identity is not empty" "$work/used.log" || fail "used volume: $(cat "$work/used.log")"
[[ $(start_tor "${project}_tor_identity") == "$before" ]] || fail 'the refused import changed the volume'
echo 'ok: an import into a volume Tor has used is refused and changes nothing'
echo 'PASS: onion identity import'
