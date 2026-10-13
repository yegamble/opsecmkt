#!/usr/bin/env bash
# Run the documentation site as its own onion service (deploy/docs/compose.yaml).
#
#   scripts/docs-onion.sh up        build the site and start it; prints the onion address
#   scripts/docs-onion.sh address   print the onion address
#   scripts/docs-onion.sh status    show the containers
#   scripts/docs-onion.sh down      stop it; keeps the onion keys (the tor_identity volume)
#   AGE_RECIPIENT=age1... scripts/docs-onion.sh backup-keys [FILE]
#                                   write the onion keys, encrypted with age (default backups/docs-onion-<UTC>.tar.age)
#   AGE_IDENTITY=key.txt scripts/docs-onion.sh restore-keys FILE
#                                   put backed-up keys into an empty volume (on a new host, before the first up)
#
# The docs are public and tied to the project. A market on the same host could be linked to them (shared downtime,
# timing, one seizure), so `up` refuses on a host that shows any sign of a market or another onion service:
# a market .env in this checkout, a container from a market Compose file, a Tor container of another project, or
# another project's onion identity volume. Run the docs on a separate host (docs/development.md).
set -euo pipefail
cd "$(dirname "$0")/.."

# DOCS_ONION_PROJECT names another project, for example to rehearse a restore beside the live one.
project=${DOCS_ONION_PROJECT:-opsecmkt-docs}
compose=(docker compose -p "$project" -f deploy/docs/compose.yaml)

die() { printf '%s\n' "$@" >&2; exit 1; }

market_signs() {
  [[ -e .env ]] && echo '- this checkout has a .env (a market was installed from it)'
  docker ps -a --format '{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}|{{.Label "com.docker.compose.project.config_files"}}' |
    while IFS='|' read -r name owner service files; do
      [[ $owner == "$project" ]] && continue
      if [[ $files == *compose.tor.yaml* || $files == *compose.clearnet.yaml* ]]; then
        echo "- container $name comes from a market Compose file"
      elif [[ $service == tor || $service == tor-mirror ]]; then
        echo "- container $name runs another project's Tor service"
      fi
    done
  docker volume ls --format '{{.Name}}|{{.Label "com.docker.compose.project"}}' |
    while IFS='|' read -r name owner; do
      [[ $owner != "$project" && $name == *tor_identity ]] && echo "- volume $name holds another onion service's keys"
    done
  return 0
}

address() { "${compose[@]}" exec -T tor cat /var/lib/tor/docs/hostname; }

volume="${project}_tor_identity"
image=opsecmkt-docs-tor
# keys_container ARGS...: a networkless container of the docs Tor image (user debian-tor) with the key volume at Tor's
# data directory, so an empty volume takes the directory's debian-tor ownership.
keys_container() { docker run --rm -i --network none --entrypoint sh -v "$volume:/var/lib/tor" "$image" "$@"; }

backup_keys() {
  : "${AGE_RECIPIENT:?set AGE_RECIPIENT to the age public key the backup is encrypted to}"
  command -v age >/dev/null || die 'Install age (https://age-encryption.org/) first.'
  docker volume inspect "$volume" >/dev/null 2>&1 || die "No $volume volume: nothing to back up."
  local file=${1:-backups/docs-onion-$(date -u +%Y%m%dT%H%M%SZ).tar.age}
  [[ ! -e $file ]] || die "$file exists; refusing to overwrite it."
  mkdir -p "$(dirname "$file")"
  (umask 077; keys_container -c 'test -s /var/lib/tor/docs/hs_ed25519_secret_key && tar -C /var/lib/tor -cf - docs' |
    age -r "$AGE_RECIPIENT" > "$file.partial") || { rm -f "$file.partial"; die 'Backup failed; nothing was written.'; }
  mv "$file.partial" "$file"
  printf 'Backed up the keys of %s to %s\n' "$(keys_container -c 'cat /var/lib/tor/docs/hostname')" "$file"
}

restore_keys() {
  local file=${1:?usage: restore-keys FILE}
  : "${AGE_IDENTITY:?set AGE_IDENTITY to the age identity file that decrypts the backup}"
  command -v age >/dev/null || die 'Install age (https://age-encryption.org/) first.'
  [[ -z $("${compose[@]}" ps -q tor 2>/dev/null) ]] || die 'Stop the docs onion first (down).'
  docker image inspect "$image" >/dev/null 2>&1 || "${compose[@]}" build tor
  docker volume create "$volume" >/dev/null
  keys_container -c 'test ! -e /var/lib/tor/docs/hs_ed25519_secret_key' ||
    die "$volume already holds onion keys; refusing to replace them. Nothing was changed."
  age -d -i "$AGE_IDENTITY" "$file" | keys_container -c 'tar -C /var/lib/tor -xf - docs && chmod -R go= /var/lib/tor/docs' ||
    die 'Restore failed.'
  printf 'Restored the keys of %s. Run up to serve it.\n' "$(keys_container -c 'cat /var/lib/tor/docs/hostname')"
}

case ${1:-} in
  up)
    command -v docker >/dev/null || die 'Install Docker with Compose v2 first.'
    signs=$(market_signs)
    [[ -z $signs ]] || die 'Refusing to start the docs onion: this host shows signs of a market or another onion service.' \
      "$signs" \
      'The docs are public and tied to the project; on the same host they could be linked to a market.' \
      'Run them on a separate host (docs/development.md#host-the-docs-as-an-onion-service). Nothing was started.'
    "${compose[@]}" up -d --build --wait
    printf 'Docs onion: http://%s\n' "$(address)"
    echo 'Its descriptor can take 1-3 minutes to reach the Tor network. Back up the onion keys (see the docs).'
    ;;
  address) address ;;
  status) "${compose[@]}" ps ;;
  down) "${compose[@]}" down ;;
  backup-keys) shift; backup_keys "$@" ;;
  restore-keys) shift; restore_keys "$@" ;;
  *) die "Usage: $0 up|address|status|down|backup-keys [FILE]|restore-keys FILE" ;;
esac
