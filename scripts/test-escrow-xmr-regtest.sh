#!/usr/bin/env bash
# Monero escrow check on a real monerod and monero-wallet-rpc 0.18.5.1 local fakechain (WR-escrow Phase 3,
# A-248). Run in CI by the escrow-xmr-regtest job (A-498), which starts the nodes as below. FAKECHAIN ONLY: it refuses any daemon that does not report nettype "fakechain" with hard fork
# version 16 or later, before any wallet call, and each Go leg checks the same again.
#
# Slice X-c legs (internal/market/escrow_xmr_regtest_test.go), each against a real App and its Monero escrow watcher:
#   Wallets: the first pass creates the month's escrow wallet on the escrow wallet-rpc, seals its seed to a test
#     OpenPGP key (the seal decrypts with the test's private key, and restore_deterministic_wallet from it at the stored
#     restore height gives the stored primary address), activates it and keeps 5 free slots, each a real account whose
#     address 0 is the slot's; account 0 holds nothing. On the watcher's clock the next month's wallet is then created
#     sealed 6 days ahead and becomes active at its month's start, the first closed with its free slots retired (and
#     retained, having no order).
#   Funding: a buyer takes a Monero escrow through the real checkout form; the funder pays its price; in the pool it is
#     recorded at 0 confirmations; at 9 confirmations (as the daemon counts them) nothing counts; at 10 the digital
#     order completes on funding (content delivered, no payout). Each leg waits until the escrow wallet has refreshed to
#     the daemon's height before a pass, and mines in batches of 20 within a two-minute RPC bound, retrying a failed
#     generateblocks up to 5 times from the height the daemon reached.
#   TwoAccountsOneTx: one transaction pays two escrows; at 10 confirmations there are two payments rows of it, one per
#     escrow account, each its own order's, and both orders complete.
#
# Slice X-e leg (internal/market/escrow_xmr_regtest_physical_test.go), the App's escrow wallet-rpc behind a proxy that
# counts each RPC method:
#   Physical: one transaction funds four physical orders (paid after 10 blocks); three are marked shipped to real
#     subaddresses: the watcher reads the wallet (get_transfers) but makes no transfer or relay_tx, and the gate refuses
#     each release. Then, only after each outcome: Confirm receipt and the release deadline each complete an order whose
#     release is built, relayed and confirmed to the claim address; a dispute decided for the buyer and the buyer's
#     no-ship cancel each have their refund relayed and confirmed to the refund address (4 transfers, 4 relays in all).
# Slice X-d legs (internal/market/escrow_xmr_regtest_settle_test.go): the App's escrow wallet-rpc is reached through a
# counting proxy that can lose a call's answer after the wallet acted on it; claim and refund addresses are the
# funder's subaddresses; every settlement the daemon has is decoded (Shape: RingCT type 6, 16 ring members per input,
# a fee above 0, the stored one and within ESCROW_XMR_MAX_FEE).
#   RestartMidSend: relay_tx's answer is lost, the pool is flushed, and the App and the escrow wallet-rpc container
#     (ESCROW_XMR_REGTEST_WALLET_CONTAINER, optional) restart: the next pass relays the same stored transaction (same
#     hash); the proxy saw transfer once; it confirms.
#   RelayDropped: relay_tx's answer is lost; the next pass finds the transaction in the pool and records it relayed with
#     no second relay_tx or transfer.
#   TopUp: a top-up still confirming when the release is built is not spent by it; once confirmed it is left out and a
#     stray refund returns it to the buyer's refund address.
#   Reorg: a confirmed release is taken out of the chain (pop_blocks of every block since it was relayed, flush_txpool,
#     new blocks without it): back to relayed, the same transaction relayed again, confirmed again.
#   External: while the release is built but not relayed, a wallet restored from the sealed seed sweeps the escrow
#     account outside the market; the daemon refuses the market's transaction, and it is recorded external and flagged.
#   ReRequest (A-426, A-401): the monerod container (ESCROW_XMR_REGTEST_DAEMON_CONTAINER) is stopped right before each
#     relay of a release's stored transaction and started again once the escrow wallet-rpc answered, so relay_tx is
#     refused (-4) while the transaction is stored and its key images unspent; the same stored transaction is relayed
#     again on the next passes, with no second transfer, and the third refusal records the release failed;
#     incoming_transfers available still lists its outputs; the funding deposit is then taken out of the chain
#     (pop_blocks, flush_txpool) and an administrator's re-request (password step-up) is carried out: the new settlement
#     waits (deposit-missing), past twice the threshold in blocks too, with no transfer (A-614); once the same deposit
#     transaction is mined again (send_raw_transaction) it builds a transaction spending exactly the stored key images
#     (the stored row's and the daemon's decoded inputs) to the stored plan, which is relayed and confirms.
# Slice X-f legs (internal/market/escrow_xmr_regtest_restore_test.go), a restore simulated with scripts/restore.sh's own
# Monero SQL:
#   Restore: the restore holds a built release whose relay the daemon lost, retires the free slots and closes the wallet;
#     under the gate nothing is built or relayed; the sweep finds a wallet created after the "backup" (it opens with the
#     derived password), seals it as an orphan (the seal restores to its primary address) and lists its funded account;
#     after an administrator's confirmation the held release is relayed once (same transaction, no transfer) and
#     confirms; once the gate is cleared the next checkout gets a new wallet's account, no address reused.
#   Lookahead: a deposit to account 300, which the wallet does not know, is missed by its refresh with the default
#     lookahead (the negative leg); the sweep raises the lookahead past it and rescans, and it funds the order.
#   WrongSecret: with MONERO_WALLET_SECRET changed from the pinned one, Monero escrow is disabled, no wallet is created
#     or opened, checkout answers 503.
#   Destroy: a deposit on day 89 to a closed order's escrow is returned by a stray refund; on day 91 the retained wallet
#     is destroyed (password replaced, seal wiped) and open_wallet with the derived password is refused.
# A-467 leg (internal/market/escrow_xmr_regtest_below_fee_test.go):
#   RefundBelowFee: orders of 0.001, 0.0015 and 0.0018 XMR decided for the buyer; the wallet refuses each refund (-17,
#     -17, -16: coins at or below the network fee); the market's fee estimate (get_default_fee_priority,
#     estimate_tx_size_and_weight, get_fee_estimate) classes each too few: failed too-small, its deposit left out with
#     one note, the stray refund waiting too-small, no outputs-mismatch or refusing-wallet note; a later 0.005 XMR deposit
#     lets that one stray refund return both deposits (relayed, decoded, confirmed).
# A-547 leg (internal/market/vendor_bond_regtest_test.go):
#   Bond: a buyer applies to sell under a bond with automatic approval; the first pass creates the vendor bond wallet on
#     the escrow wallet-rpc (its sealed seed restores to its primary address) and gives the application a real account's
#     address 0; the funder pays the bond: one block short of the threshold nothing is recorded, at the threshold the
#     bond is recorded and the account approved as a vendor; pop_blocks takes the deposit below the threshold again:
#     flagged to the administrators once, nothing revoked; mined again, nothing changes. The bond wallet lists no
#     outgoing, pending or failed transfer.
#
# Needs a fakechain monerod and three monero-wallet-rpc instances attached to it (the App's escrow wallet-rpc, a funder
# standing in for the buyer's wallet, and one used only to restore a sealed seed), plus a disposable PostgreSQL database.
# The wallet-rpcs listen without RPC login on 127.0.0.1 only. The ReRequest leg stops and starts the monerod container
# named by ESCROW_XMR_REGTEST_DAEMON_CONTAINER (required), so it needs the docker command, and monerod must run with
# --keep-fakechain: without it, monerod deletes the fakechain when it starts (the leg checks the height it comes back
# at). For example:
#
#   docker network create xmr-escrow-regtest
#   docker run -d --name xmr-escrow-monerod --network xmr-escrow-regtest -p 127.0.0.1:28381:18081 \
#     ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 --regtest --keep-fakechain --offline --fixed-difficulty 1 \
#     --rpc-bind-ip 0.0.0.0 --rpc-bind-port 18081 --confirm-external-bind --no-igd
#   for w in escrow:28382 funder:28383 restore:28384; do
#     docker run -d --name "xmr-escrow-wallet-${w%%:*}" --network xmr-escrow-regtest -p "127.0.0.1:${w##*:}:18083" \
#       -e RPC_EXPOSE_UNAUTHENTICATED=1 ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
#       --daemon-address xmr-escrow-monerod:18081 --trusted-daemon --wallet-dir /tmp/w --disable-rpc-login \
#       --rpc-bind-port 18083 --allow-mismatched-daemon-version
#   done
#   ESCROW_XMR_REGTEST_DAEMON_URL=http://127.0.0.1:28381 ESCROW_XMR_REGTEST_WALLET_URL=http://127.0.0.1:28382 \
#   ESCROW_XMR_REGTEST_FUNDER_URL=http://127.0.0.1:28383 ESCROW_XMR_REGTEST_RESTORE_URL=http://127.0.0.1:28384 \
#   ESCROW_XMR_REGTEST_WALLET_CONTAINER=xmr-escrow-wallet-escrow ESCROW_XMR_REGTEST_DAEMON_CONTAINER=xmr-escrow-monerod \
#   TEST_DATABASE_URL=postgres://…/a_disposable_database ./scripts/test-escrow-xmr-regtest.sh
#   docker rm -f xmr-escrow-monerod xmr-escrow-wallet-escrow xmr-escrow-wallet-funder xmr-escrow-wallet-restore
#   docker network rm xmr-escrow-regtest
#
# (The image's entrypoint adds --non-interactive and the RPC bind address itself; RPC_EXPOSE_UNAUTHENTICATED=1 lets the
# wallet-rpc answer on the published loopback port.) The legs may run again against the same nodes: each puts the
# watcher's clock in a month of its own. The transcript is written to ESCROW_XMR_REGTEST_LOG (default
# artifacts/real-chain/test-escrow-xmr-regtest-<UTC time>.log, gitignored); no seed, password or URL is printed.
set -euo pipefail

for v in ESCROW_XMR_REGTEST_DAEMON_URL ESCROW_XMR_REGTEST_WALLET_URL ESCROW_XMR_REGTEST_FUNDER_URL ESCROW_XMR_REGTEST_RESTORE_URL ESCROW_XMR_REGTEST_DAEMON_CONTAINER TEST_DATABASE_URL; do
  if [[ -z "${!v:-}" ]]; then
    echo "Set $v (see the comment at the top of this script)." >&2
    exit 2
  fi
done
command -v curl >/dev/null || { echo "curl is required" >&2; exit 2; }
command -v go >/dev/null || { echo "Go is required" >&2; exit 2; }

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
log="${ESCROW_XMR_REGTEST_LOG:-artifacts/real-chain/test-escrow-xmr-regtest-$(date -u +%Y%m%dT%H%M%SZ).log}"
mkdir -p "$(dirname "$log")"
echo "Monero escrow fakechain check (hard fork $version); transcript: $log"
set +e
go test -count=1 -timeout 45m -run '^TestXMREscrowRegtest(Wallets|Funding|TwoAccountsOneTx|Physical|RestartMidSend|RelayDropped|TopUp|Reorg|FundingReorg|External|ReRequest|Restore|Lookahead|WrongSecret|Destroy|RefundBelowFee|Bond)$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
# Each leg is reported from its own go test result line, so one failing leg never makes another read as failed.
failed=0
for leg in TestXMREscrowRegtestWallets TestXMREscrowRegtestFunding TestXMREscrowRegtestTwoAccountsOneTx TestXMREscrowRegtestPhysical \
  TestXMREscrowRegtestRestartMidSend TestXMREscrowRegtestRelayDropped TestXMREscrowRegtestTopUp TestXMREscrowRegtestReorg TestXMREscrowRegtestFundingReorg TestXMREscrowRegtestExternal TestXMREscrowRegtestReRequest \
  TestXMREscrowRegtestRestore TestXMREscrowRegtestLookahead TestXMREscrowRegtestWrongSecret TestXMREscrowRegtestDestroy TestXMREscrowRegtestRefundBelowFee TestXMREscrowRegtestBond; do
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
echo "PASS: Monero escrow wallets (creation, sealed seed restored to its primary address, slots, account 0 empty, monthly rotation), funding at 10 confirmations and not at 9, one transaction paying two escrow accounts, physical orders (no spending call before the outcome; after Confirm receipt, the deadline, a dispute for the buyer and a no-ship cancel, each settlement relayed and confirmed), and settlements (stored before relay, never rebuilt across a lost relay answer and restarts, a top-up left out and stray-refunded, relayed again after a reorganisation, a release held while a reorganisation took a credited funding deposit below the threshold and then spending both funding deposits, and built without a funding deposit flushed from the pool only after 20 blocks and never stray-refunded, an outside spend recorded external, a relay refused three times while monerod was stopped then failed, and its administrator's re-request spending the same key images to the same plan and confirmed; ring 16, rct type 6, fee within the cap), a restore (held release relayed once after confirmation, an orphan wallet found and sealed, free slots retired, a fresh wallet after it), the lookahead of a wallet missing an account, a changed secret refused, and a wallet destroyed after its retention with a day-89 deposit refunded first, and refunds of 0.001, 0.0015 and 0.0018 XMR the wallet refused as below the network fee ended too few and returned by one stray refund each with a later deposit, and a vendor bond recorded at the threshold and not one short, then flagged and not revoked when pop_blocks took its deposit below it, on a local fakechain; transcript in $log"
