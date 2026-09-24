#!/usr/bin/env bash
# Check every documented onion-mirror command against .env files that hold COMPOSE_FILE and COMPOSE_PROFILES
# exactly as scripts/install.sh writes them for Tor (war-room A-140). `docker compose --profile mirror ...`
# replaces COMPOSE_PROFILES for that command, so on an internal-db install it fails with
# 'service "app" depends on undefined service "db"'; the Compose matrix passes explicit --profile flags and
# cannot see that. Usage: scripts/test-mirror-commands.sh [internal|external] (default: both).
#
# Each command is resolved with `docker compose <its global options> config --services`: Compose loads the
# project as that command would, and every service the command names (or tor-mirror, for a bare `up`) must be
# active. `up --dry-run` is not used: on Compose 5.5.1 it sometimes failed spuriously ('app is missing
# dependency db') or left lines out of its plan (docs/verification.md). Requires Docker with Compose v2 and OpenSSL. Runs in a temporary copy of the
# deployment files under a unique project name, so the checkout's own .env is never read; nothing is built,
# pulled, created or started.
set -euo pipefail
cd "$(dirname "$0")/.."
case ${1:-all} in
  internal|external|all) only=${1:-all} ;;
  *) echo 'Usage: scripts/test-mirror-commands.sh [internal|external]' >&2; exit 2 ;;
esac
for dependency in docker openssl; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
# Only the temporary .env written below may configure Compose.
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES DATABASE_URL SETUP_TOKEN POSTGRES_PASSWORD \
  APP_MODE COOKIE_SECURE APP_PORT
work=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-mirror-commands.XXXXXX")
trap 'rm -rf "$work"' EXIT
cp compose*.yaml Dockerfile "$work/"
cp -R deploy "$work/"
project="opsecmkt-mirror-check-$(openssl rand -hex 6)"
fail() { echo "FAIL: $*" >&2; exit 1; }

# The fenced block after the marker in the operator guide.
guide_commands=$(awk '
  /<!-- ops-cmd: onion-mirror -->/ { marked = 1; next }
  marked && /^```/ { if (inside) exit; inside = 1; next }
  inside { print }
' docs/operator-guide.md)
[[ -n $guide_commands ]] || fail 'docs/operator-guide.md has no <!-- ops-cmd: onion-mirror --> block'
# Mirror commands given inline elsewhere: document|text. Each text must still appear there verbatim; the
# backticks are the documents' Markdown, not command substitution.
# shellcheck disable=SC2016
inline_commands=(
  'docs/operator-guide.md|docker compose up -d --force-recreate tor tor-mirror'
  'UPGRADING.md|docker compose up -d --force-recreate tor tor-mirror'
  'docs/testnet-runbook.md|`docker compose stop tor-mirror`'
  # Recovery step 9 starts the mirror that step 1 stopped, then checks it runs.
  'docs/testnet-runbook.md|run `docker compose up -d`'
  'docs/testnet-runbook.md|`docker compose ps tor-mirror`'
)
# database|COMPOSE_FILE|COMPOSE_PROFILES as scripts/install.sh writes them for Tor, with and without local nodes.
# tests/test_installer.py runs the installer and fails if its values drift from these lines.
installs=(
  'internal|compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml|internal-db,bitcoin,monero,monero-wallet'
  'internal|compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml|internal-db'
  'external|compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.external-egress.yaml|bitcoin,monero,monero-wallet'
  'external|compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.external-egress.yaml|'
)

# resolve COMMAND: print why a documented `docker compose ...` command would not reach the mirror in the
# temporary deployment, and return 1; return 0 if it would. Read-only `config` pipelines run as written.
resolve() {
  local command=$1 word sub='' services i=2
  local -a words globals=() targets=()
  if [[ $command == 'docker compose config '* ]]; then
    services=$(cd "$work" && bash -o pipefail -c "$command" 2>&1) || { echo "it failed: $services"; return 1; }
    return 0
  fi
  [[ $command != *'|'* ]] || { echo 'only config commands may use a pipeline here'; return 1; }
  read -ra words <<< "${command%%#*}"
  [[ ${words[0]-} == docker && ${words[1]-} == compose ]] || { echo 'not a docker compose command'; return 1; }
  while ((i < ${#words[@]})); do
    case ${words[i]} in
      --profile|-f|--file|-p|--project-name|--env-file) globals+=("${words[i]}" "${words[i + 1]-}"); i=$((i + 2)) ;;
      -*) globals+=("${words[i]}"); i=$((i + 1)) ;;
      *) sub=${words[i]}; i=$((i + 1)); break ;;
    esac
  done
  for word in "${words[@]:i}"; do
    [[ $word == -* ]] && continue
    targets+=("$word")
    [[ $sub != exec ]] || break  # the rest is the command run inside the container
  done
  services=$(cd "$work" && docker compose ${globals[@]+"${globals[@]}"} config --services 2>&1) || {
    echo "$services"; return 1; }
  for word in ${targets[@]+"${targets[@]}"}; do
    grep -qx -- "$word" <<< "$services" || { echo "$word is not an active service"; return 1; }
  done
  if [[ $sub == up && ${#targets[@]} -eq 0 ]] && ! grep -qx tor-mirror <<< "$services"; then
    echo 'a bare up would leave tor-mirror out'; return 1
  fi
}
# write_env PROFILES: the installer's .env keys for this install ($url, $files) with COMPOSE_PROFILES=PROFILES.
write_env() {
  cat > "$work/.env" <<ENV
DATABASE_URL='$url'
POSTGRES_PASSWORD='mirror-check'
SETUP_TOKEN='$(openssl rand -hex 32)'
APP_MODE='tor'
COOKIE_SECURE='false'
APP_PORT='8080'
COMPOSE_PROJECT_NAME='$project'
COMPOSE_FILE='$files'
COMPOSE_PROFILES='$1'
ENV
}

checked=0
for install in "${installs[@]}"; do
  IFS='|' read -r database files profiles <<< "$install"
  [[ $only == all || $only == "$database" ]] || continue
  case $database in
    internal) url='postgres://opsecmkt:mirror-check@db:5432/opsecmkt?sslmode=disable' ;;
    external) url='postgres://opsecmkt:mirror-check@database.invalid:5432/opsecmkt' ;;
  esac
  label="Tor, $database database, COMPOSE_PROFILES='$profiles'"
  write_env "$profiles"
  (cd "$work" && docker compose config --quiet) || fail "$label: the installer's configuration does not validate"
  # Control: without mirror in .env, a bare up leaves the mirror out, and on an internal-db install the old guide
  # command fails; this check tells both apart from the documented commands.
  why=$(resolve 'docker compose up -d') && fail "$label: tor-mirror is active before mirror is added"
  if [[ $database == internal ]]; then
    why=$(resolve 'docker compose --profile mirror up -d tor-mirror') &&
      fail "$label: the replaced command 'docker compose --profile mirror up -d tor-mirror' resolved"
    grep -Eq 'depends on undefined service "?db"?' <<< "$why" || fail "$label: unexpected control failure: $why"
  fi
  # The guide's first step: add mirror to COMPOSE_PROFILES, keeping the installer's profiles.
  write_env "${profiles:+$profiles,}mirror"
  if [[ $database == internal ]]; then
    (cd "$work" && docker compose config --services) | grep -qx db || fail "$label + mirror: the database was dropped"
  fi
  while IFS= read -r command; do
    [[ -n $command ]] || continue
    why=$(resolve "$command") || fail "$label + mirror: '$command': $why"
    echo "ok: $command"
  done <<< "$guide_commands"
  for inline in "${inline_commands[@]}"; do
    document=${inline%%|*}
    text=${inline#*|}
    grep -qF -- "$text" "$document" || fail "$document no longer contains: $text"
    command=${text#run }
    command=${command#\`}
    command=${command%\`}
    why=$(resolve "$command") || fail "$label + mirror: '$command' ($document): $why"
    echo "ok: $command ($document)"
  done
  echo "PASS: $label, then the documented mirror commands with mirror added"
  checked=$((checked + 1))
done
((checked > 0)) || fail 'no installer configuration was checked'
