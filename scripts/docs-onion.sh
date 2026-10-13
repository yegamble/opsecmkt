#!/usr/bin/env bash
# Run the documentation site as its own onion service (deploy/docs/compose.yaml).
#
#   scripts/docs-onion.sh up        build the site and start it; prints the onion address
#   scripts/docs-onion.sh address   print the onion address
#   scripts/docs-onion.sh status    show the containers
#   scripts/docs-onion.sh down      stop it; keeps the onion keys (the tor_identity volume)
#
# The docs are public and tied to the project. A market on the same host could be linked to them (shared downtime,
# timing, one seizure), so `up` refuses on a host that shows any sign of a market or another onion service:
# a market .env in this checkout, a container from a market Compose file, a Tor container of another project, or
# another project's onion identity volume. Run the docs on a separate host (docs/development.md).
set -euo pipefail
cd "$(dirname "$0")/.."

project=opsecmkt-docs
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
  *) die "Usage: $0 up|address|status|down" ;;
esac
