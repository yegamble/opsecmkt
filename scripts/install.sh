#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
command -v docker >/dev/null || { echo 'Install Docker with Compose v2 first.' >&2; exit 1; }
docker compose version >/dev/null || { echo 'Docker Compose v2 is required.' >&2; exit 1; }
command -v openssl >/dev/null || { echo 'OpenSSL is required to generate secrets.' >&2; exit 1; }
[[ ! -e .env ]] || { echo '.env already exists; edit it and use docker compose up -d --build. No secrets overwritten.' >&2; exit 1; }
read -r -p 'Exposure (clearnet/tor) [clearnet]: ' mode
mode=${mode:-clearnet}
[[ $mode == clearnet || $mode == tor ]] || { echo 'Invalid mode'; exit 1; }
read -r -s -p 'External PostgreSQL URL (blank creates internal database): ' database
printf '\n'
profiles=''
compose_files="compose.yaml:compose.${mode}.yaml:compose.nodes.yaml"
external_egress=false
password=$(openssl rand -hex 24)
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
put SETUP_TOKEN "$(openssl rand -hex 32)"
put APP_MODE "$mode"
put COOKIE_SECURE "$secure"
# Ed25519 seed for signed audit exports; payment networks default to test networks (mainnet is refused).
put AUDIT_SIGNING_KEY "$(openssl rand -hex 32)"
put BITCOIN_CHAIN testnet4
put MONERO_NETWORK stagenet
for coin in BITCOIN MONERO; do
  read -r -p "$coin node (disabled/external/local) [disabled]: " choice
  case ${choice:-disabled} in
    disabled) ;;
    external)
      read -r -s -p "$coin RPC URL: " rpc; printf '\n'
      [[ $rpc == http://* || $rpc == https://* ]] || { echo 'Use an http:// or https:// RPC URL'; exit 1; }
      external_egress=true
      put "${coin}_RPC_URL" "$rpc"
      if [[ $coin == MONERO ]]; then
        read -r -s -p 'MONERO wallet RPC URL (monero-wallet-rpc; blank for none): ' wallet_rpc; printf '\n'
        if [[ -n $wallet_rpc ]]; then
          [[ $wallet_rpc == http://* || $wallet_rpc == https://* ]] || { echo 'Use an http:// or https:// RPC URL'; exit 1; }
          put MONERO_WALLET_RPC_URL "$wallet_rpc"
        fi
      fi
      ;;
    local)
      read -r -p "$coin reviewed Docker image (prefer @sha256 digest): " node_image
      [[ -n $node_image ]] || { echo 'A reviewed image is required'; exit 1; }
      put "${coin}_IMAGE" "$node_image"
      if [[ $coin == BITCOIN ]]; then
        rpc_password=$(openssl rand -hex 24)
        put BITCOIN_RPC_PASSWORD "$rpc_password"
        put BITCOIN_RPC_URL "http://marketplace:${rpc_password}@bitcoin:8332"
        profiles="${profiles:+$profiles,}bitcoin"
      else
        put MONERO_RPC_URL 'http://monero:18081'
        put MONERO_WALLET_RPC_URL 'http://monero-wallet:18083'
        profiles="${profiles:+$profiles,}monero,monero-wallet"
      fi
      ;;
    *) echo 'Invalid node choice'; exit 1 ;;
  esac
done
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
if [[ $mode == tor ]]; then
  printf 'Onion address (after Tor initializes): docker compose exec tor cat /var/lib/tor/marketplace/hostname\n'
else
  printf 'Local endpoint: http://127.0.0.1:8080 (production requires a TLS reverse proxy).\n'
fi
