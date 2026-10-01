#!/usr/bin/env bash
# Manual Bitcoin escrow check on a real Bitcoin Core regtest node (WR-escrow Phase 1, A-246). Not run in CI.
# REGTEST ONLY: it refuses any node that does not report chain "regtest", before any wallet call.
#
# Slice P1-a leg: a buyer's Core wallet holding a BIP48 account registers its tpub with origin on a real App,
# fetches the practice PSBT, signs it with walletprocesspsbt and pastes it back; the key becomes active. Before
# that it checks the app's rpcwhitelist user cannot export or back up keys (listdescriptors, dumpwallet,
# backupwallet refused) but can refill the arbiter keypool, and that unsigned, other-wallet, SIGHASH_NONE,
# SIGHASH_SINGLE|ANYONECANPAY, corrupted and forged-final pastes are refused.
#
# Slice P1-b leg: a buyer and a vendor register BIP48 keys from their own Core wallets and activate them by practice
# signing; the buyer creates an escrow through the real checkout form. Core checks the descriptor each party's order
# page shows (checksum, derived address = the address shown, each party's child key at its origin), that the
# watch-only wallet imported only the bare-key descriptor of that address, and that its witness script is the
# 2-of-3 of the stored buyer, vendor and market keys (the market's an arbiter-wallet child). A regtest wallet funds
# the address, 3 blocks are mined and one escrow watcher pass completes the order (content delivered, no payouts
# row).
#
# Slice P1-c1 leg: a funded, completed escrow (paid above its price, with a market fee) is claimed by its vendor; one
# watcher pass builds the settlement (gettxout, createpsbt, utxoupdatepsbt; Core's decodepsbt agrees on its txid and
# fee); SIGHASH_NONE and the other party's signature, both made by real Core wallets, are refused; the vendor signs
# its own copy in its own Core wallet and pastes it (party_signed). Then an underpaid escrow is cancelled by its
# buyer and the buyer signs the refund in their wallet. For both, Core confirms that the stored PSBT plus the market's
# signature (the arbiter wallet's walletprocesspsbt, called by the test only) plus the stored party signatures would
# finalize with the stored txid and outputs and pass testmempoolaccept; nothing is broadcast in that leg.
#
# Slice P1-c2 leg: the App's escrow watcher co-signs, checks, stores, broadcasts and follows each settlement to 3
# confirmations, every party signature made by that party's own Core wallet: a vendor claim at a market child index
# past the arbiter wallet's initial key range (so the watcher must keypoolrefill under rpcwhitelist), with each
# confirmed output checked against the plan (gettxout); a refund whose co-sign step runs alone, the App restarted
# before any broadcast, then the stored transaction broadcast unchanged; a settlement both parties sign (the market
# does not: Core finalizes the stored PSBT with the two party signatures alone to the broadcast transaction); a
# vendor signature over a changed input amount and an r=s=1 signature, each discarded (A-339) before the good one
# completes it; a stray refund of coins sent to a cancelled order's escrow; and an administrator's dispute decision
# for the buyer.
#
# Slice P1-d1 leg (recovery and reconciliation): both parties sign a claim and broadcast the transaction their two
# signatures finalize to before the watcher's pass, once left in the mempool and once mined; the watcher records it as
# broadcast (not external) and confirms it (A-344). invalidateblock takes that confirmed settlement's block out of
# the chain: the watcher returns it to broadcast and sends the stored transaction again; reconsiderblock brings the
# block back and it is confirmed again. A confirmed claim's block is invalidated and the parties replace its
# transaction with one paying elsewhere, which is mined: the watcher records it external with the replacing txid. The
# parties co-spend a completed order's escrow from their own wallets with no settlement: the watcher flags the deposit
# with the spending txid. A labelled descriptor imported into the watch-only wallet with no escrow row is listed as an
# orphan on the daily check.
#
# Slice P1-d2 leg (the Phase 1 gate's remaining adversarial legs): a key field holding two keys (a comma) is refused
# before any RPC, while Core reads the descriptor built from them as a 2-of-4; a vendor's Core-signed copy with a
# signature added under the market's key is refused and changes nothing; A-356: the parties co-spend an escrow from
# their own wallets while its claim waits for a signature, and the watcher records the claim external with the
# co-spend's txid; the parties co-spend an escrow before its claim, the build refuses the spent input (nothing built)
# and after one pass the claim is external with that txid; once an administrator's refund decision is broadcast,
# changing it is refused and the database refuses a second outcome; a party-signed claim is not co-signed while the
# restore gate is set, and a stored transaction held as scripts/restore.sh holds it is broadcast only after an
# administrator's confirmation, once; one transaction funds two escrows and each claim spends only its own output; ten
# deposits at 100 sat/vB exceed the fee cap, so nothing is built until the rate is lowered. Every leg logs its txids.
#
# Ruled Phase 1 legs and where they run: release (Settlement, Cosign: claim), refund (Settlement, Cosign), dispute
# (Cosign), comma injection refused (Gate), NONE paste refused (PracticeSign, Settlement), forged market slot refused
# (Gate), conflicting outcome refused (Gate), spent-outpoint build refused (Gate), restart mid-send rebroadcasts the
# same tx (Cosign: refund), reorg + double-spend flagged (Recovery), high index via keypoolrefill (Cosign: claim at
# index 2501), restore gate + reconciliation (Gate; Recovery: co-spend and orphan), two escrows never mixed (Gate), fee
# cap (Gate); also A-339, both-signed and stray refund (Cosign), A-344 and a plain reorg (Recovery), A-356 (Gate).
#
# Needs a regtest node with two RPC users: an operator user with full rights (sets up the wallets, standing in
# for the buyer's own node) and the app's user restricted by rpcwhitelist exactly as docs/operator-guide.md
# ("Bitcoin escrow (regtest only)") describes, plus a disposable PostgreSQL database. For example:
#
#   salt=$(openssl rand -hex 16); hash=$(printf %s app-pass | openssl dgst -sha256 -hmac "$salt" | cut -d' ' -f2)
#   docker run -d --name escrow-regtest -p 127.0.0.1:18443:18443 bitcoin/bitcoin:31.1 bitcoind -regtest -fallbackfee=0.0002 \
#     -server=1 -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 -rpcuser=operator -rpcpassword=operator-pass \
#     "-rpcauth=opsecmkt-escrow:$salt\$$hash" -rpcwhitelistdefault=0 \
#     -rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions
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
go test -count=1 -timeout 15m -run '^TestEscrowRegtest(PracticeSign|Funding|Settlement|Cosign|Recovery|Gate)$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
# Each leg is reported from its own go test result line, so one failing leg never makes another read as failed.
failed=0
for leg in TestEscrowRegtestPracticeSign TestEscrowRegtestFunding TestEscrowRegtestSettlement TestEscrowRegtestCosign TestEscrowRegtestRecovery TestEscrowRegtestGate; do
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
echo "PASS: practice signing, escrow creation and funding, settlement building and party signing, co-signing, broadcast and confirmation, recovery and reconciliation, and the Phase 1 gate's remaining adversarial legs, on regtest; transcript in $log"
