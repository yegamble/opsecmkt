#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
command -v docker >/dev/null || { echo 'Install Docker with Compose v2 first.' >&2; exit 1; }
docker compose version >/dev/null || { echo 'Docker Compose v2 is required.' >&2; exit 1; }
command -v openssl >/dev/null || { echo 'OpenSSL is required to generate secrets.' >&2; exit 1; }
[[ ! -e .env ]] || { echo '.env already exists; edit it and use docker compose up -d --build (see UPGRADING.md for new keys). No secrets overwritten.' >&2; exit 1; }
# secret N prints N random bytes as hex and refuses to continue if generation failed.
secret() {
  local value
  value=$(openssl rand -hex "$1") || value=''
  [[ $value =~ ^[0-9a-f]+$ && ${#value} -eq $(($1 * 2)) ]] || { echo 'Could not generate a random secret with openssl; nothing was started.' >&2; exit 1; }
  printf '%s' "$value"
}
read -r -p 'Exposure (clearnet/tor) [clearnet]: ' mode
mode=${mode:-clearnet}
[[ $mode == clearnet || $mode == tor ]] || { echo 'Invalid mode'; exit 1; }
read -r -s -p 'External PostgreSQL URL (blank creates internal database): ' database
printf '\n'
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
if [[ $mode == tor ]]; then
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
# Ed25519 seed for signed audit exports.
audit_key=$(secret 32) || exit 1
put AUDIT_SIGNING_KEY "$audit_key"
# Payments are test-network only (mainnet is always refused). BITCOIN_CHAIN / MONERO_NETWORK stay blank
# unless a local node is chosen: the app then accepts whichever test network an external node reports.
bitcoin_chain=''
monero_network=''
for coin in BITCOIN MONERO; do
  read -r -p "$coin node (disabled/external/local) [disabled]: " choice
  case ${choice:-disabled} in
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
      read -r -p "$coin reviewed Docker image (prefer @sha256 digest): " node_image
      [[ -n $node_image ]] || { echo 'A reviewed image is required'; exit 1; }
      put "${coin}_IMAGE" "$node_image"
      # A local node never runs without a generated RPC password (secret exits otherwise).
      if [[ $coin == BITCOIN ]]; then
        rpc_password=$(secret 24) || exit 1
        put BITCOIN_RPC_PASSWORD "$rpc_password"
        put BITCOIN_RPC_URL "http://marketplace:${rpc_password}@bitcoin:8332"
        bitcoin_chain=testnet4
        profiles="${profiles:+$profiles,}bitcoin"
      else
        wallet_password=$(secret 24) || exit 1
        put MONERO_RPC_URL 'http://monero:18081'
        put MONERO_WALLET_RPC_PASSWORD "$wallet_password"
        put MONERO_WALLET_RPC_URL "http://marketplace:${wallet_password}@monero-wallet:18083"
        monero_network=stagenet
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
printf 'Started. Read SETUP_TOKEN from the protected .env and open /setup to create the first admin.\n'
if grep -q -e '^BITCOIN_RPC_URL=' -e '^MONERO_WALLET_RPC_URL=' .env; then
  printf 'Payment wallets must exist before test-network payments work; until then the admin page shows the provider as unavailable. Follow docs/testnet-runbook.md.\n'
fi
if [[ $mode == tor ]]; then
  printf 'Onion address (after Tor initializes): docker compose exec tor cat /var/lib/tor/marketplace/hostname\n'
else
  printf 'Local endpoint: http://127.0.0.1:8080 (production requires a TLS reverse proxy).\n'
fi
