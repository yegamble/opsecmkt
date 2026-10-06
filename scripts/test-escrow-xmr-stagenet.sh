#!/usr/bin/env bash
# Manual Monero escrow check on an OFFLINE STAGENET FORK, NOT THE PUBLIC STAGENET (war-room A-449, P4-R15). Not run in
# CI. It refuses any daemon that does not report nettype "stagenet" at a height below 10,000 (the public stagenet is
# past 1,900,000), before any wallet call, and each Go leg checks the same again.
#
# A stagenet chain built from its genesis block is at hard fork version 1 (version 2 starts at height 32,000 and 16 at
# 1,151,720), so Monero escrow's network check refuses it, as it must, and the fakechain legs of
# scripts/test-escrow-xmr-regtest.sh cannot run on it. The legs here (internal/market/escrow_xmr_stagenet_test.go) are the
# ones such a chain can run:
#   OldForkRefused: an App with ESCROW_XMR=on against the stagenet daemon is refused for its hard fork, not its network.
#   Addresses: a real stagenet wallet's primary address, a create_account subaddress and an integrated address parse
#     under the stagenet address tags (24, 36; the integrated one refused as such) and are refused under the
#     fakechain's; the adapter verified for stagenet reads the real wallet's replies, one verified for the fakechain
#     refuses them.
#   SyncGuard: the escrow adapter's sync read (get_info, get_last_block_header): a restricted-RPC daemon of a peered
#     pair hides its connection counts (0) and is synced on offline false and a fresh tip; its unrestricted peer shows
#     the connection; on the --offline daemon it is not synced (no peers).
#   PooledGuard: the pooled Monero check against the real wallet-rpc is synced with the peered daemon and not synced (no
#     peers) with the --offline one.
#
# Needs, from the local monerod and monero-wallet-rpc 0.18.5.1 images only (no download, no public peer): two stagenet
# daemons peered only with each other on a Docker network with no route out, one of them serving restricted RPC, one
# --offline stagenet daemon, and a stagenet wallet-rpc on the unrestricted daemon, plus a disposable PostgreSQL
# database. The host reaches the containers on the internal network by their addresses. For example:
#
#   docker network create --internal xmr-stagenet-fork
#   for n in a b; do o=b; [ $n = b ] && o=a; r=''; [ $n = b ] && r='--restricted-rpc'
#     docker run -d --name "xmr-sn-$n" --network xmr-stagenet-fork --network-alias "sn$n" \
#       ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 --stagenet --fixed-difficulty 1 --no-igd --disable-dns-checkpoints \
#       --add-exclusive-node "sn$o:38080" --p2p-bind-ip 0.0.0.0 --p2p-bind-port 38080 --rpc-bind-ip 0.0.0.0 \
#       --rpc-bind-port 38081 --confirm-external-bind --check-updates disabled $r
#   done
#   docker run -d --name xmr-sn-offline --network xmr-stagenet-fork ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 \
#     --stagenet --offline --fixed-difficulty 1 --no-igd --rpc-bind-ip 0.0.0.0 --rpc-bind-port 38081 --confirm-external-bind
#   docker run -d --name xmr-sn-wallet --network xmr-stagenet-fork -e RPC_EXPOSE_UNAUTHENTICATED=1 \
#     ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 --stagenet --daemon-address sna:38081 --trusted-daemon \
#     --wallet-dir /tmp/w --disable-rpc-login --rpc-bind-port 38083 --allow-mismatched-daemon-version
#   ip() { docker inspect "$1" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'; }
#   ESCROW_XMR_STAGENET_MINER_URL=http://$(ip xmr-sn-a):38081 ESCROW_XMR_STAGENET_DAEMON_URL=http://$(ip xmr-sn-b):38081 \
#   ESCROW_XMR_STAGENET_OFFLINE_URL=http://$(ip xmr-sn-offline):38081 ESCROW_XMR_STAGENET_WALLET_URL=http://$(ip xmr-sn-wallet):38083 \
#   TEST_DATABASE_URL=postgres://…/a_disposable_database ./scripts/test-escrow-xmr-stagenet.sh
#   docker rm -f xmr-sn-a xmr-sn-b xmr-sn-offline xmr-sn-wallet; docker network rm xmr-stagenet-fork
#
# The script mines 20 blocks on the miner daemon (a local mining thread, fixed difficulty 1) so the tip is fresh. The
# transcript is written to ESCROW_XMR_STAGENET_LOG (default artifacts/real-chain/test-escrow-xmr-stagenet-<UTC time>.log,
# gitignored); no seed, password or URL is printed.
set -euo pipefail

for v in ESCROW_XMR_STAGENET_MINER_URL ESCROW_XMR_STAGENET_DAEMON_URL ESCROW_XMR_STAGENET_OFFLINE_URL ESCROW_XMR_STAGENET_WALLET_URL TEST_DATABASE_URL; do
  if [[ -z "${!v:-}" ]]; then
    echo "Set $v (see the comment at the top of this script)." >&2
    exit 2
  fi
done
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v go >/dev/null || { echo "Go is required" >&2; exit 2; }

# rpc <base URL> <path> <JSON body>: the daemon's answer, or nothing.
rpc() {
  printf 'url = "%s%s"\n' "${1%/}" "$2" | curl -sS --max-time 20 --noproxy '*' -H 'Content-Type: application/json' \
    --data "$3" --config - 2>/dev/null | tr -d ' \n' || true
}
field() { sed -n "s/.*\"$1\":\"\{0,1\}\([a-z0-9]*\)\"\{0,1\}[,}].*/\1/p"; }
info() { rpc "$1" /json_rpc '{"jsonrpc":"2.0","id":"0","method":"get_info"}'; }
for v in ESCROW_XMR_STAGENET_MINER_URL ESCROW_XMR_STAGENET_DAEMON_URL ESCROW_XMR_STAGENET_OFFLINE_URL; do
  answer="$(info "${!v}")"
  nettype="$(field nettype <<<"$answer")"
  height="$(field height <<<"$answer")"
  if [[ "$nettype" != stagenet ]] || ! [[ "$height" =~ ^[0-9]+$ ]] || (( height >= 10000 )); then
    echo "Refusing: the daemon at $v is not an offline stagenet fork (stagenet below height 10,000), or is unreachable." >&2
    exit 1
  fi
done

# A stagenet address whose keys were thrown away: the mined coins are unspendable, on a chain that is thrown away too.
miner_address=51v8eaPQVU3QGCHyM5xcH9geZCJvso4drBmjc2bbKWo7MsyWYeQNGAb12Uv22dwmcGgroVK4W7Fc2CDPKupxGpnEFG7acys
start="$(field height <<<"$(info "$ESCROW_XMR_STAGENET_MINER_URL")")"
rpc "$ESCROW_XMR_STAGENET_MINER_URL" /start_mining "{\"miner_address\":\"$miner_address\",\"threads_count\":1,\"do_background_mining\":false,\"ignore_battery\":true}" >/dev/null
deadline=$((SECONDS + 180))
until h="$(field height <<<"$(info "$ESCROW_XMR_STAGENET_DAEMON_URL")")"; [[ "$h" =~ ^[0-9]+$ ]] && (( h >= start + 20 )); do
  if (( SECONDS > deadline )); then
    rpc "$ESCROW_XMR_STAGENET_MINER_URL" /stop_mining '{}' >/dev/null
    echo "Mining 20 blocks did not reach the restricted daemon within 3 minutes." >&2
    exit 1
  fi
  sleep 1
done
rpc "$ESCROW_XMR_STAGENET_MINER_URL" /stop_mining '{}' >/dev/null

cd "$(dirname "$0")/.."
log="${ESCROW_XMR_STAGENET_LOG:-artifacts/real-chain/test-escrow-xmr-stagenet-$(date -u +%Y%m%dT%H%M%SZ).log}"
mkdir -p "$(dirname "$log")"
echo "Monero escrow check on an offline stagenet fork, not public (height $h, hard fork 1); transcript: $log"
set +e
go test -count=1 -timeout 15m -run '^TestXMREscrowStagenet(OldForkRefused|Addresses|SyncGuard|PooledGuard)$' -v ./internal/market/ 2>&1 | tee -a "$log"
status=${PIPESTATUS[0]}
set -e
failed=0
for leg in TestXMREscrowStagenetOldForkRefused TestXMREscrowStagenetAddresses TestXMREscrowStagenetSyncGuard TestXMREscrowStagenetPooledGuard; do
  if grep -q -- "--- PASS: $leg " "$log"; then
    echo "PASS: $leg"
  elif grep -q -- "--- FAIL: $leg " "$log"; then
    echo "FAILED: $leg; see $log" >&2
    failed=1
  elif grep -q -- "--- SKIP: $leg " "$log"; then
    echo "SKIPPED: $leg (a skipped leg is not a pass); see $log" >&2
    failed=1
  else
    echo "NOT RUN: $leg (go test reported no result for it); see $log" >&2
    failed=1
  fi
done
if [[ $status -ne 0 || $failed -ne 0 ]]; then
  echo "FAILED: go test exited $status; see $log" >&2
  exit 1
fi
echo "PASS: on an offline stagenet fork (not public): Monero escrow refused for the chain's hard fork, not its network; real stagenet addresses accepted under the stagenet tags and refused under the fakechain's; the escrow and pooled Monero sync guards synced with a peered daemon and not synced (no peers) with an --offline one; transcript in $log"
