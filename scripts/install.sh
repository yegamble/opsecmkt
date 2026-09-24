#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
local_mode=false
case ${1:-} in
  --local) [[ $# -eq 1 ]] || { echo 'Usage: scripts/install.sh [--local]' >&2; exit 2; }; local_mode=true ;;
  '') ;;
  *) echo 'Usage: scripts/install.sh [--local]' >&2; exit 2 ;;
esac
command -v docker >/dev/null || { echo 'Install Docker with Compose v2 first.' >&2; exit 1; }
docker compose version >/dev/null || { echo 'Docker Compose v2 is required.' >&2; exit 1; }
command -v openssl >/dev/null || { echo 'OpenSSL is required to generate secrets.' >&2; exit 1; }
[[ ! -e .env ]] || { echo '.env already exists; edit it and use docker compose up -d --build (see UPGRADING.md for new keys). No secrets overwritten.' >&2; exit 1; }
# Compose names containers, volumes and networks after the project, not the checkout. A second checkout with
# the same name would recreate another installation's containers with new secrets, and `docker compose down -v`
# there would delete its volumes. Refuse unless everything under this name was created from this directory.
project=${COMPOSE_PROJECT_NAME:-opsecmkt}
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || { echo 'COMPOSE_PROJECT_NAME must use lower-case letters, digits, hyphens and underscores, starting with a letter or digit.' >&2; exit 1; }
second_copy="To install a second copy on this host, give it its own project name, for example: COMPOSE_PROJECT_NAME=${project}-rehearsal ./scripts/install.sh${1:+ $1}"
# One line per container; the prefix keeps a container without the label from vanishing as an empty line.
owners=$(docker ps -a --filter "label=com.docker.compose.project=$project" --format 'dir={{.Label "com.docker.compose.project.working_dir"}}') || {
  echo 'Could not list Docker containers (is the Docker daemon running?); nothing was written or started.' >&2; exit 1; }
if [[ -n $owners ]]; then
  here=$(pwd -P)
  foreign=''
  while IFS= read -r owner; do
    owner=${owner#dir=}
    # The label holds the path Compose was started from, which may go through a symlink.
    if [[ -z $owner ]]; then
      foreign="$foreign  (no working directory label)"$'\n'
    elif [[ $owner != "$PWD" && ! ( -d $owner && $(cd "$owner" 2>/dev/null && pwd -P) == "$here" ) ]]; then
      foreign="$foreign  $owner"$'\n'
    fi
  done <<< "$owners"
  if [[ -n $foreign ]]; then
    printf "Refusing to install: Compose project '%s' already has containers created from another directory:\n" "$project" >&2
    printf '%s' "$foreign" | sort -u >&2
    printf '%s\n' 'Installing here would recreate them with new secrets, and docker compose down -v here would delete their volumes. Nothing was written or started.' \
      "To manage that installation, work in its directory. $second_copy" >&2
    exit 1
  fi
else
  volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || {
    echo 'Could not list Docker volumes (is the Docker daemon running?); nothing was written or started.' >&2; exit 1; }
  if [[ -n $volumes ]]; then
    printf "Refusing to install: Compose project '%s' has volumes but no containers, so the checkout that created them is unknown:\n" "$project" >&2
    printf '%s\n' "$volumes" | sed 's/^/  /' >&2
    printf '%s\n' "They may hold another installation's data: installing here would start on them with new secrets, and docker compose down -v here would delete them. Nothing was written or started." \
      "To use that data, start it from the checkout (and .env) that created it. $second_copy" >&2
    exit 1
  fi
fi
# secret N prints N random bytes as hex and refuses to continue if generation failed.
secret() {
  local value
  value=$(openssl rand -hex "$1") || value=''
  [[ $value =~ ^[0-9a-f]+$ && ${#value} -eq $(($1 * 2)) ]] || { echo 'Could not generate a random secret with openssl; nothing was started.' >&2; exit 1; }
  printf '%s' "$value"
}
if [[ $local_mode == true ]]; then
  mode=clearnet
  database=''
  # The quick start always creates its own local database and disables external wallet connections.
  unset DATABASE_URL POSTGRES_PASSWORD SETUP_TOKEN AUDIT_SIGNING_KEY APP_MODE COOKIE_SECURE
  unset COMPOSE_FILE COMPOSE_PROFILES BITCOIN_RPC_URL MONERO_RPC_URL MONERO_WALLET_RPC_URL
else
  read -r -p 'Exposure (clearnet/tor) [clearnet]: ' mode
fi
mode=${mode:-clearnet}
[[ $mode == clearnet || $mode == tor ]] || { echo 'Invalid mode'; exit 1; }
if [[ $local_mode == false ]]; then
  read -r -s -p 'External PostgreSQL URL (blank creates internal database): ' database
  printf '\n'
fi
if [[ $mode == clearnet ]]; then
  command -v curl >/dev/null || { echo 'curl is required to verify application readiness.' >&2; exit 1; }
  [[ ${APP_PORT:-8080} =~ ^[0-9]{1,5}$ ]] && ((10#${APP_PORT:-8080} > 0 && 10#${APP_PORT:-8080} <= 65535)) || { echo 'APP_PORT must be between 1 and 65535.' >&2; exit 1; }
fi
profiles=''
compose_files="compose.yaml:compose.${mode}.yaml:compose.nodes.yaml"
external_egress=false
password=$(secret 24) || exit 1
if [[ -z $database ]]; then
  database="postgres://opsecmkt:${password}@db:5432/opsecmkt?sslmode=disable"
  profiles=internal-db
  compose_files="${compose_files}:compose.internal-db.yaml"
else
  [[ $database == postgres://* || $database == postgresql://* ]] || { echo 'Use a postgres:// or postgresql:// database URL'; exit 1; }
  external_egress=true
fi
secure=true
if [[ $mode == tor || $local_mode == true ]]; then
  secure=false
else
  read -r -p 'Local HTTP development only? (yes/no) [no]: ' local_http
  [[ ${local_http:-no} != yes ]] || secure=false
fi
# Write literal dotenv values; refuse quotes/newlines rather than evaluating shell input.
put() {
  [[ $2 != *"'"* && $2 != *$'\n'* && $2 != *$'\r'* ]] || { echo 'Values cannot contain single quotes or newlines.' >&2; exit 1; }
  printf "%s='%s'\n" "$1" "$2" >> "$config"
}
config=$(mktemp .env.install.XXXXXX)
trap 'rm -f "$config"' EXIT
put DATABASE_URL "$database"
put POSTGRES_PASSWORD "$password"
setup_token=$(secret 32) || exit 1
put SETUP_TOKEN "$setup_token"
put APP_MODE "$mode"
put COOKIE_SECURE "$secure"
put APP_PORT "${APP_PORT:-8080}"
# Every later docker compose command in this directory reads the project name from .env. Never change it on an
# existing install: Compose would start it on new, empty volumes.
put COMPOSE_PROJECT_NAME "$project"
if [[ $local_mode == true ]]; then
  put BITCOIN_RPC_URL ''
  put MONERO_RPC_URL ''
  put MONERO_WALLET_RPC_URL ''
fi
# Ed25519 seed for signed audit exports.
audit_key=$(secret 32) || exit 1
put AUDIT_SIGNING_KEY "$audit_key"
# Payments are test-network only (mainnet is always refused). BITCOIN_CHAIN / MONERO_NETWORK stay blank
# unless a local node is chosen: the app then accepts whichever test network an external node reports.
bitcoin_chain=''
monero_network=''
# Local nodes are pruned unless the operator answers no. Keep this default equal to compose.nodes.yaml's.
bitcoin_prune_mb=2000
monero_prune_flags='--prune-blockchain --sync-pruned-blocks'
for coin in BITCOIN MONERO; do
  if [[ $local_mode == true ]]; then
    choice=disabled
  else
    # Sizes are upstream estimates (docs/operator-guide.md#local-node-pruning) and grow with each chain.
    case $coin in
      BITCOIN) printf '%s\n' "BITCOIN: 'local' (the default) runs a pruned Bitcoin Core node in Docker. It keeps about 5-8 GB on disk (${bitcoin_prune_mb} MiB of recent blocks plus the chain state), but its first sync still downloads the whole test chain (about 24 GB signet, 31 GB testnet4) and can take hours. 'external' uses a node you run elsewhere; 'disabled' leaves Bitcoin payments off." ;;
      MONERO) printf '%s\n' "MONERO: 'local' (the default) runs a pruned monerod and monero-wallet-rpc in Docker. Pruning keeps about a third of the chain; stagenet/testnet sizes are not published upstream, so keep 20 GB free (an estimate). The first sync can take hours. 'external' uses a node you run elsewhere; 'disabled' leaves Monero payments off." ;;
    esac
    read -r -p "$coin node (local/external/disabled) [local]: " choice
  fi
  case ${choice:-local} in
    disabled) ;;
    external)
      read -r -s -p "$coin RPC URL: " rpc; printf '\n'
      [[ $rpc == http://* || $rpc == https://* ]] || { echo 'Use an http:// or https:// RPC URL'; exit 1; }
      put "${coin}_RPC_URL" "$rpc"
      if [[ $coin == MONERO ]]; then
        read -r -s -p 'MONERO wallet RPC URL (monero-wallet-rpc; blank for none): ' wallet_rpc; printf '\n'
        if [[ -n $wallet_rpc ]]; then
          [[ $wallet_rpc == http://* || $wallet_rpc == https://* ]] || { echo 'Use an http:// or https:// RPC URL'; exit 1; }
          put MONERO_WALLET_RPC_URL "$wallet_rpc"
          external_egress=true
        else
          echo 'WARNING: no Monero wallet RPC URL was given, so Monero payments stay disabled (a daemon alone cannot process payments) and no egress is opened for it. Set MONERO_WALLET_RPC_URL in .env later to enable them.' >&2
        fi
      else
        external_egress=true
      fi
      ;;
    local)
      case $coin in
        BITCOIN) default_image='bitcoin/bitcoin:latest' ;;
        MONERO) default_image='ghcr.io/sethforprivacy/simple-monerod:latest' ;;
      esac
      read -r -p "$coin Docker image [$default_image; blank uses this default]: " node_image
      node_image=${node_image:-$default_image}
      image_name=$node_image
      if [[ $node_image == *@* ]]; then
        image_name=${node_image%@*}
        image_digest=${node_image##*@}
        [[ $image_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
          echo 'Invalid image digest; use a complete @sha256: followed by 64 lowercase hexadecimal characters.' >&2
          exit 1
        }
      fi
      [[ $image_name =~ ^[[:alnum:]][[:alnum:]./:_-]*$ && $image_name != */ && $image_name != *: ]] || {
        echo 'Invalid Docker image reference; include a repository name and optional tag or complete SHA-256 digest.' >&2
        exit 1
      }
      put "${coin}_IMAGE" "$node_image"
      # A local node never runs without a generated RPC password (secret exits otherwise).
      if [[ $coin == BITCOIN ]]; then
        rpc_password=$(secret 24) || exit 1
        put BITCOIN_RPC_PASSWORD "$rpc_password"
        put BITCOIN_RPC_URL "http://marketplace:${rpc_password}@bitcoin:8332"
        read -r -p 'BITCOIN network (testnet4/signet/live) [testnet4]: ' bitcoin_network
        case ${bitcoin_network:-testnet4} in
          testnet4|signet) bitcoin_chain=${bitcoin_network:-testnet4} ;;
          live) echo 'Live Bitcoin payments are disabled: this application refuses mainnet wallets and addresses.' >&2; exit 1 ;;
          *) echo 'Choose testnet4, signet, or live.' >&2; exit 1 ;;
        esac
        read -r -p 'Prune the BITCOIN node to save disk? (yes/no) [yes]: ' prune
        case ${prune:-yes} in
          yes) ;;
          no)
            bitcoin_prune_mb=0
            echo 'Full node: BITCOIN keeps every block, about 28 GB on signet or 33 GB on testnet4 today (upstream estimates, growing), with the same first sync. BITCOIN_PRUNE_MB=0 in .env; see docs/operator-guide.md#local-node-pruning to change it later.'
            ;;
          *) echo 'Answer yes or no.' >&2; exit 1 ;;
        esac
        put BITCOIN_PRUNE_MB "$bitcoin_prune_mb"
        profiles="${profiles:+$profiles,}bitcoin"
      else
        read -r -p 'MONERO network (stagenet/testnet/live) [stagenet]: ' monero_network_choice
        case ${monero_network_choice:-stagenet} in
          stagenet|testnet) monero_network=${monero_network_choice:-stagenet} ;;
          live) echo 'Live Monero payments are disabled: this application refuses mainnet wallets and addresses.' >&2; exit 1 ;;
          *) echo 'Choose stagenet, testnet, or live.' >&2; exit 1 ;;
        esac
        read -r -p 'Prune the MONERO node to save disk? (yes/no) [yes]: ' prune
        case ${prune:-yes} in
          yes) ;;
          no)
            monero_prune_flags=''
            echo 'Full node: MONERO keeps the whole chain, about three times the pruned size (upstream ratio), with a longer first sync. MONERO_PRUNE_FLAGS is blank in .env; see docs/operator-guide.md#local-node-pruning to change it later.'
            ;;
          *) echo 'Answer yes or no.' >&2; exit 1 ;;
        esac
        put MONERO_PRUNE_FLAGS "$monero_prune_flags"
        wallet_password=$(secret 24) || exit 1
        put MONERO_RPC_URL 'http://monero:18081'
        put MONERO_WALLET_RPC_PASSWORD "$wallet_password"
        put MONERO_WALLET_RPC_URL "http://marketplace:${wallet_password}@monero-wallet:18083"
        put MONERO_WALLET_IMAGE "${MONERO_WALLET_IMAGE:-ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest}"
        profiles="${profiles:+$profiles,}monero,monero-wallet"
      fi
      ;;
    *) echo 'Invalid node choice'; exit 1 ;;
  esac
done
put BITCOIN_CHAIN "$bitcoin_chain"
put MONERO_NETWORK "$monero_network"
if [[ ,$profiles, == *,bitcoin,* ]] && ! grep -q "^BITCOIN_RPC_PASSWORD='[0-9a-f]\{48\}'$" "$config"; then
  echo 'Refusing to enable the local Bitcoin node without a generated RPC password.' >&2; exit 1
fi
if [[ $mode == tor && $external_egress == true ]]; then
  compose_files="${compose_files}:compose.external-egress.yaml"
  echo 'External endpoints selected: app direct network egress is enabled; only inbound exposure is Tor-only.'
fi
put COMPOSE_FILE "$compose_files"
put COMPOSE_PROFILES "$profiles"
mv "$config" .env
# Validate before starting. The internal database overlay waits for database health.
docker compose config --quiet
docker compose up -d --build
if [[ $mode == clearnet ]]; then
  endpoint="http://127.0.0.1:${APP_PORT:-8080}"
  ready=false
  for attempt in {1..60}; do
    if [[ $(curl --noproxy '*' --silent --fail --max-time 2 "$endpoint/healthz" 2>/dev/null) == ok ]]; then
      ready=true
      break
    fi
    sleep 1
  done
  [[ $ready == true ]] || { echo 'Application did not become healthy. Inspect docker compose logs app; configuration and data were preserved.' >&2; exit 1; }
fi
printf 'Read SETUP_TOKEN from the protected .env to authorize the browser setup wizard. Keep this file private.\n'
if grep -q -E "^(BITCOIN_RPC_URL|MONERO_WALLET_RPC_URL)='[^']+'$" .env; then
  printf 'Payment wallets must exist before test-network payments work; until then the admin page shows the provider as unavailable. Follow docs/testnet-runbook.md.\n'
fi
if [[ $mode == tor ]]; then
  printf 'Onion address (after Tor initializes): docker compose exec tor cat /var/lib/tor/marketplace/hostname\n'
else
  if [[ $secure == false ]]; then
    printf 'Ready. Open %s/setup to name your marketplace and create its administrator.\n' "$endpoint"
  else
    printf 'Application healthy at %s. Configure your HTTPS reverse proxy, then open https://YOUR-DOMAIN/setup. Secure cookies require HTTPS.\n' "$endpoint"
  fi
fi
