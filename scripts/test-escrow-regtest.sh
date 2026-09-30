#!/usr/bin/env bash
# Manual Bitcoin escrow check on a real Bitcoin Core regtest node (WR-escrow Phase 1, A-246). Not run in CI.
# REGTEST ONLY: it refuses any node that does not report chain "regtest", before any wallet call.
#
# Slice P1-a leg: a buyer's Core wallet holding a BIP48 account registers its tpub with origin on a real App,
# fetches the practice PSBT, signs it with walletprocesspsbt and pastes it back; the key becomes active. Before
# that it checks the app's rpcwhitelist user cannot export or back up keys (listdescriptors, dumpwallet,
# backupwallet refused) but can refill the arbiter keypool, and that unsigned, other-wallet, SIGHASH_NONE,
# SIGHASH_SINGLE|ANYONECANPAY, corrupted and forged-final pastes are refused. Later slices add their legs here.
#
# Needs a regtest node with two RPC users: an operator user with full rights (sets up the wallets, standing in
# for the buyer's own node) and the app's user restricted by rpcwhitelist exactly as docs/operator-guide.md
# ("Bitcoin escrow (regtest only)") describes, plus a disposable PostgreSQL database. For example:
#
#   salt=$(openssl rand -hex 16); hash=$(printf %s app-pass | openssl dgst -sha256 -hmac "$salt" | cut -d' ' -f2)
#   docker run -d --name escrow-regtest -p 127.0.0.1:18443:18443 bitcoin/bitcoin:31.1 bitcoind -regtest \
#     -server=1 -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 -rpcuser=operator -rpcpassword=operator-pass \
#     "-rpcauth=opsecmkt-escrow:$salt\$$hash" -rpcwhitelistdefault=0 \
#     -rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction
#   ESCROW_REGTEST_OPERATOR_RPC_URL=http://operator:operator-pass@127.0.0.1:18443 \
#   ESCROW_REGTEST_APP_RPC_URL=http://opsecmkt-escrow:app-pass@127.0.0.1:18443 \
#   TEST_DATABASE_URL=postgres://…/a_disposable_database ./scripts/test-escrow-regtest.sh
#
# The transcript is written to ESCROW_REGTEST_LOG (default artifacts/real-chain/test-escrow-regtest-<UTC time>.log,
# gitignored). URLs reach curl on stdin and are never printed.
set -euo pipefail

for v in ESCROW_REGTEST_OPERATOR_RPC_URL ESCROW_REGTEST_APP_RPC_URL TEST_DATABASE_URL; do
  if [[ -z "${!v:-}" ]]; then
    echo "Set $v (see the comment at the top of this script)." >&2
    exit 2
  fi
done
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v go >/dev/null || { echo "Go is required" >&2; exit 2; }

chain_of() { # chain_of <url>: the chain getblockchaininfo reports, or nothing
  local base="${1%/}"
  printf 'url = "%s"\n' "$base" | curl -sS --max-time 10 --noproxy '*' -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"1.0","id":"escrow","method":"getblockchaininfo","params":[]}' --config - 2>/dev/null |
    sed -n 's/.*"chain":"\([a-z0-9]*\)".*/\1/p' || true
}
for v in ESCROW_REGTEST_OPERATOR_RPC_URL ESCROW_REGTEST_APP_RPC_URL; do
  if [[ "$(chain_of "${!v}")" != regtest ]]; then
    echo "Refusing: the node at $v is not on regtest (or is unreachable)." >&2
    exit 1
  fi
done

cd "$(dirname "$0")/.."
log="${ESCROW_REGTEST_LOG:-artifacts/real-chain/test-escrow-regtest-$(date -u +%Y%m%dT%H%M%SZ).log}"
mkdir -p "$(dirname "$log")"
echo "Bitcoin escrow regtest check; transcript: $log"
set +e
go test -count=1 -timeout 10m -run '^TestEscrowRegtestPracticeSign$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
if [[ $status -ne 0 ]] || ! grep -q -- '--- PASS: TestEscrowRegtestPracticeSign' "$log"; then
  echo "FAILED (or skipped): see $log" >&2
  exit 1
fi
echo "PASS: practice signing on regtest; transcript in $log"
