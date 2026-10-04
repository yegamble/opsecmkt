#!/usr/bin/env bash
# PAYE-E2 (WR-escrow Phase 3, P3-R12): how one monthly Monero escrow wallet scales, measured on a real monerod and
# monero-wallet-rpc 0.18.5.1 local fakechain. Manual, not run in CI; it takes about an hour at the default size.
# FAKECHAIN ONLY: it refuses any daemon that does not report nettype "fakechain" with hard fork version 16 or later,
# before any wallet call, and the Go leg checks the same again.
#
# internal/market/escrow_xmr_payee2_test.go (TestXMREscrowPAYEE2), through the app's own adapter and its timeouts: one
# escrow wallet gets 10,000 accounts (create_account), 200 of them chosen at random are funded two per block while it is
# closed; then create_account (total and per call), open_wallet, refresh over the funding blocks and over 1,000 empty
# blocks, get_transfers all_accounts (time and payload size), get_accounts, incoming_transfers of one account, the
# wallet file's size and the wallet-rpc's resident memory, and a seed restore (restore_deterministic_wallet,
# set_subaddress_lookahead past the highest account, rescan_blockchain) that must find every funded account. It passes
# when a full watcher pass over 5 such wallets (copies of the measured file) fits the Monero watcher's pass timeout and
# every call its own timeout; the report compares the pass with the poll interval too.
#
# Needs the nodes scripts/test-escrow-xmr-regtest.sh describes (a fakechain monerod and three monero-wallet-rpc: the
# escrow one, a funder and one for the restore) and the escrow wallet-rpc's container name, to copy its wallet file and
# read its memory; no database. For example (loopback ports 29081-29084):
#
#   docker network create xmr-payee2
#   docker run -d --name xmr-payee2-monerod --network xmr-payee2 -p 127.0.0.1:29081:18081 \
#     ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 --regtest --offline --fixed-difficulty 1 \
#     --rpc-bind-ip 0.0.0.0 --rpc-bind-port 18081 --confirm-external-bind --no-igd
#   docker run -d --name xmr-payee2-wallet-escrow --network xmr-payee2 -p 127.0.0.1:29082:18083 \
#     -e RPC_EXPOSE_UNAUTHENTICATED=1 ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
#     --daemon-address xmr-payee2-monerod:18081 --trusted-daemon --wallet-dir /tmp/w --disable-rpc-login \
#     --rpc-bind-port 18083 --allow-mismatched-daemon-version
#   (the same again as xmr-payee2-wallet-funder on 29083 and xmr-payee2-wallet-restore on 29084)
#   ESCROW_XMR_REGTEST_DAEMON_URL=http://127.0.0.1:29081 ESCROW_XMR_REGTEST_WALLET_URL=http://127.0.0.1:29082 \
#   ESCROW_XMR_REGTEST_FUNDER_URL=http://127.0.0.1:29083 ESCROW_XMR_REGTEST_RESTORE_URL=http://127.0.0.1:29084 \
#   ESCROW_XMR_PAYEE2_WALLET_CONTAINER=xmr-payee2-wallet-escrow ./scripts/test-escrow-xmr-payee2.sh
#   docker rm -f xmr-payee2-monerod xmr-payee2-wallet-escrow xmr-payee2-wallet-funder xmr-payee2-wallet-restore
#   docker network rm xmr-payee2
#
# ESCROW_XMR_PAYEE2_ACCOUNTS, _FUNDED and _WALLETS change the size (defaults 10000, 200, 5; a smaller run is no
# PAYE-E2 evidence). The report is artifacts/real-chain/paye-e2-<commit>.txt (gitignored), also printed; the go test
# transcript is ESCROW_XMR_PAYEE2_LOG (default artifacts/real-chain/test-escrow-xmr-payee2-<UTC time>.log). No seed,
# password or URL is printed.
set -euo pipefail

for v in ESCROW_XMR_REGTEST_DAEMON_URL ESCROW_XMR_REGTEST_WALLET_URL ESCROW_XMR_REGTEST_FUNDER_URL ESCROW_XMR_REGTEST_RESTORE_URL ESCROW_XMR_PAYEE2_WALLET_CONTAINER; do
  if [[ -z "${!v:-}" ]]; then
    echo "Set $v (see the comment at the top of this script)." >&2
    exit 2
  fi
done
for tool in curl go docker git; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done

daemon_field() { # daemon_field <method> <field>: a string or number field of the daemon's JSON-RPC answer, or nothing
  local base="${ESCROW_XMR_REGTEST_DAEMON_URL%/}"
  printf 'url = "%s/json_rpc"\n' "$base" | curl -sS --max-time 10 --noproxy '*' -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"$1\"}" --config - 2>/dev/null |
    tr -d ' \n' | sed -n "s/.*\"$2\":\"\{0,1\}\([a-z0-9]*\)\"\{0,1\}[,}].*/\1/p" || true
}
nettype="$(daemon_field get_info nettype)"
version="$(daemon_field hard_fork_info version)"
if [[ "$nettype" != fakechain ]]; then
  echo "Refusing: the daemon at ESCROW_XMR_REGTEST_DAEMON_URL is not on a fakechain (or is unreachable)." >&2
  exit 1
fi
if ! [[ "$version" =~ ^[0-9]+$ ]] || (( version < 16 )); then
  echo "Refusing: the daemon's hard fork version is below 16 (start monerod --regtest from release 0.18 or later)." >&2
  exit 1
fi

cd "$(dirname "$0")/.."
sha="$(git rev-parse --short=12 HEAD)"
if [[ -n "$(git status --porcelain -- internal)" ]]; then
  sha="$sha-dirty"
fi
log="${ESCROW_XMR_PAYEE2_LOG:-artifacts/real-chain/test-escrow-xmr-payee2-$(date -u +%Y%m%dT%H%M%SZ).log}"
report="artifacts/real-chain/paye-e2-$sha.txt"
mkdir -p "$(dirname "$log")"
echo "PAYE-E2 on a local Monero fakechain (hard fork $version); report: $report; transcript: $log"
set +e
ESCROW_XMR_PAYEE2=1 ESCROW_XMR_PAYEE2_SHA="$sha" go test -count=1 -timeout 180m -run '^TestXMREscrowPAYEE2$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
if [[ -f "$report" ]]; then
  echo "--- $report"
  cat "$report"
fi
if grep -q -- '--- PASS: TestXMREscrowPAYEE2 ' "$log" && [[ $status -eq 0 ]] && grep -q '^VERDICT: PASS' "$report"; then
  echo "PASS: PAYE-E2; report in $report"
  exit 0
fi
if grep -q -- '--- SKIP: TestXMREscrowPAYEE2 ' "$log"; then
  echo "SKIPPED: TestXMREscrowPAYEE2 (a skipped leg is not a pass); see $log" >&2
else
  echo "FAILED: PAYE-E2 (go test exited $status); see $report and $log" >&2
fi
exit 1
