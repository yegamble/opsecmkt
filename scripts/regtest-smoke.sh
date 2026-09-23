#!/usr/bin/env bash
# Manual Bitcoin Core regtest smoke test for the BTC payment adapter. Not run in CI.
#
# Requires a running `bitcoind -regtest -server=1` with RPC credentials, for example:
#   bitcoind -regtest -daemon -rpcuser=smoke -rpcpassword=smoke-pass -fallbackfee=0.0002
#   BITCOIN_REGTEST_RPC_URL=http://smoke:smoke-pass@127.0.0.1:18443 ./scripts/regtest-smoke.sh
#
# The test creates/loads the wallet "opsecmkt", mines 101 blocks to it, pays a fresh order address, mines one
# block and checks the adapter reports the output with its confirmations. It refuses any non-regtest node.
set -euo pipefail

if [[ -z "${BITCOIN_REGTEST_RPC_URL:-}" ]]; then
  echo "Set BITCOIN_REGTEST_RPC_URL=http://user:password@127.0.0.1:18443 (bitcoind -regtest)." >&2
  exit 2
fi
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v go >/dev/null || { echo "Go is required" >&2; exit 2; }

rpc() { # rpc <path> <method> <json params>; the URL (with credentials) reaches curl on stdin, not in argv
  local base="${BITCOIN_REGTEST_RPC_URL%/}"
  printf 'url = "%s%s"\n' "$base" "$1" | curl -sS --max-time 10 --noproxy '*' -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"1.0\",\"id\":\"smoke\",\"method\":\"$2\",\"params\":$3}" --config - || true
}

chain="$(rpc "" getblockchaininfo '[]')"
if [[ "$chain" != *'"chain":"regtest"'* ]]; then
  echo "Refusing: the node at BITCOIN_REGTEST_RPC_URL is not on regtest (or is unreachable)." >&2
  exit 1
fi
# Create the wallet if it does not exist yet, otherwise load it; both are harmless when already loaded.
rpc "" createwallet '["opsecmkt"]' >/dev/null
rpc "" loadwallet '["opsecmkt"]' >/dev/null

cd "$(dirname "$0")/.."
BITCOIN_REGTEST_RPC_URL="$BITCOIN_REGTEST_RPC_URL" go test -count=1 -run '^TestRegtestSmoke$' -v ./internal/market/
