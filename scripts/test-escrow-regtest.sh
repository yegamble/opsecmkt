#!/usr/bin/env bash
# Bitcoin escrow check on a real Bitcoin Core regtest node (WR-escrow Phase 1, A-246). Run by hand, and in CI by the escrow-regtest
# job (A-438, .github/workflows/ci.yaml, docs/ci-cd.md), which starts a node like the one below with generated passwords.
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
# administrator's confirmation, once; a restored database never re-issues an escrow address (A-359: an escrow created
# after the backup point is lost by a restore simulated in SQL, checkout answers 503 while the restore gate is set, and
# the same buyer and vendor then get a fresh address because the watch-only wallet still holds the lost one); one
# transaction funds two escrows and each claim spends only its own output; ten deposits at 100 sat/vB exceed the fee
# cap, so nothing is built until the rate is lowered. Every leg logs its txids.
#
# Slice P2-d leg (Phase 2 fee bumps, P2-R7, A-375): a vendor's claim with a market fee and a buyer return is broadcast at
# 1 sat/vB and left unmined; the buyer may not ask for a fee bump of it; the vendor asks for one at 5 sat/vB, Core's
# decodepsbt agrees on its txid, inputs, addresses and fee; the buyer's Core-signed copy pasted before the vendor's
# signature is refused; the vendor signs it in their own wallet and the watcher co-signs it, testmempoolaccept accepts it
# as a replacement and it replaces the original in the mempool (another pass does not bring the original back). After 3
# blocks the settlement and the bump are confirmed (not external), the original is conflicted in the watch-only wallet
# by the bump, and each confirmed output (gettxout) is the plan's with only the vendor's output smaller. P2-d-fix, in the
# same leg: a 100 sat/vB bump request is refused with the fee-cap message and a bump at the form's highest rate is
# broadcast and confirmed (A-383); the vendor signs a bump on the market, the buyer signs its own copy, and they
# combinepsbt, finalizepsbt and sendrawtransaction it themselves; once mined the settlement is confirmed with the bump
# recorded as seen on the network and nothing flagged (A-382).
#
# Slice P2-b leg (Phase 2 physical release, P2-R1, P2-R2): the App reaches Core through a counting proxy. A funded
# physical escrow order is marked shipped by its vendor with a fresh claim address, and its release is built while the
# order is shipped (Core's decodepsbt agrees on its txid); the vendor signs it in their own wallet and a watcher pass
# leaves it waiting for the buyer; the buyer signs it in their own wallet and the next pass finalizes the two signatures
# alone (cosigned parties), completes the order and broadcasts it. The arbiter wallet's walletprocesspsbt (and
# keypoolrefill) call count over the whole leg is 0. After 3 blocks it is confirmed and each output (gettxout) is the
# plan's. A second order's release is signed by the vendor through the market and by the buyer offline in their own
# wallet, and the parties combine, finalize and broadcast it (A-385): the watcher records it as broadcast, completes the
# order and confirms it.
#
# Slice P2-c leg (Phase 2 release deadline, P2-R3): the App reaches Core through a counting proxy. A funded physical
# escrow order gets its ship-by time; the vendor marks it shipped and signs the release in their own wallet, and a
# watcher pass leaves it waiting for the buyer (arbiter walletprocesspsbt 0). The release deadline is moved into the
# past through SQL, and one watcher pass completes the order ("auto-release deadline passed"), co-signs the vendor's
# signature with the arbiter wallet (walletprocesspsbt 1) and broadcasts it; after 3 blocks it is confirmed and each
# output (gettxout) is the plan's.
#
# Slice P2-e leg (Phase 2 key reuse after a restore, P2-R10, A-366): escrows for two buyers and one vendor are created,
# the backup point is taken and three more are created and lost by a database-only restore simulated in SQL (the
# margin left out, so the exact layer alone carries it); checkout answers 503 until the orphan sweep has run, the sweep
# reserves exactly the lost escrows' nine keys read with getaddressinfo from the real watch-only wallet, and the next
# escrow of each buyer shares no key with them. Then the watch-only wallet is backed up at a second backup point, two
# more escrows are lost, and the wallet is restored from that backup (restorewallet) so it forgets them too; after
# scripts/restore.sh's own margin SQL and the sweep, the next escrow's indices are at or above the floors (market at
# least 50 past the backup point) and it shares no key with any lost escrow.
#
# A-428 leg (a deposit that did not count when its order was cancelled, TestEscrowRegtestConflicted): a regtest wallet
# pays an escrow and the watcher records the deposit unconfirmed; the payer replaces it with a transaction spending the
# same inputs to an address of its own, which is mined, so Core reports the deposit conflicted (the leg logs
# gettransaction's confirmations on the watch-only wallet and whether listreceivedbyaddress still lists it) and the
# ledger row goes negative. The payment window ends (the clock moved in SQL): the order is cancelled unpaid with no
# refund. invalidateblock takes the replacement's block out of the chain, the original deposit is sent again
# (prioritisetransaction lets it replace the replacement in the mempool) and mined to 3 confirmations: the deposit is
# left out and flagged once (an order note, the buyer and staff told), and a stray refund to the refund address pinned
# at checkout is requested, signed by the buyer's own wallet, co-signed, broadcast and confirmed (gettxout).
#
# A-453 leg (shape (b) per-order key derivation, TestEscrowRegtestBIP32; no product caller yet): three Core wallets each
# hold a BIP48 account of their own seed; for per-order indexes 2, 3, 17, 1000 and 2^31-1 the Go derivation
# (escrow_bip32.go) derives each account's child and serializes it rooted (depth 0, fingerprint 0, child 0); as the
# app's whitelisted user, Core's deriveaddresses gives the same address from the account key at <index>/0/0 and from
# the rooted key at /0/0, for each key (wpkh) and the three together (wsh(sortedmulti(2,…))), and the operator's
# derivation from each private root along m/48h/1h/0h/2h/<index>/0/0 agrees; a rooted key checked at another index, or
# with one chain-code byte changed, is refused.
#
# Ruled Phase 1 legs and where they run: release (Settlement, Cosign: claim), refund (Settlement, Cosign), dispute
# (Cosign), comma injection refused (Gate), NONE paste refused (PracticeSign, Settlement), forged market slot refused
# (Gate), conflicting outcome refused (Gate), spent-outpoint build refused (Gate), restart mid-send rebroadcasts the
# same tx (Cosign: refund), reorg + double-spend flagged (Recovery), high index via keypoolrefill (Cosign: claim at
# index 2501), restore gate + reconciliation (Gate; Recovery: co-spend and orphan), two escrows never mixed (Gate), fee
# cap (Gate); also A-339, both-signed and stray refund (Cosign), A-344 and a plain reorg (Recovery), A-356 and A-359
# (Gate); A-428, a deposit conflicted at an unpaid cancellation and mined afterwards (Conflicted).
#
# Needs a regtest node with two RPC users: an operator user with full rights (sets up the wallets, standing in
# for the buyer's own node) and the app's user restricted by rpcwhitelist exactly as docs/operator-guide.md
# ("Bitcoin escrow (regtest only)") describes, plus a disposable PostgreSQL database. For example:
#
#   salt=$(openssl rand -hex 16); hash=$(printf %s app-pass | openssl dgst -sha256 -hmac "$salt" | cut -d' ' -f2)
#   docker run -d --name escrow-regtest -p 127.0.0.1:18443:18443 bitcoin/bitcoin:31.1 bitcoind -regtest -fallbackfee=0.0002 \
#     -server=1 -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 -rpcuser=operator -rpcpassword=operator-pass \
#     "-rpcauth=opsecmkt-escrow:$salt\$$hash" -rpcwhitelistdefault=0 \
#     -rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getnetworkinfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions
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
go test -count=1 -timeout 15m -run '^TestEscrowRegtest(PracticeSign|Funding|Settlement|Cosign|Recovery|Gate|Bump|Physical|Deadline|Reserve|Conflicted|BIP32)$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
# Each leg is reported from its own go test result line, so one failing leg never makes another read as failed.
failed=0
for leg in TestEscrowRegtestPracticeSign TestEscrowRegtestFunding TestEscrowRegtestSettlement TestEscrowRegtestCosign TestEscrowRegtestRecovery TestEscrowRegtestGate TestEscrowRegtestBump TestEscrowRegtestPhysical TestEscrowRegtestDeadline TestEscrowRegtestReserve TestEscrowRegtestConflicted TestEscrowRegtestBIP32; do
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
echo "PASS: practice signing, escrow creation and funding, settlement building and party signing, co-signing, broadcast and confirmation, recovery and reconciliation, the Phase 1 gate's remaining adversarial legs, a same-recipient fee bump, a physical order's release signed by the buyer and the vendor with no arbiter signature, one released at its deadline with the vendor's signature co-signed by the market, no escrow key handed out again after a restore, a deposit conflicted at an unpaid cancellation and mined afterwards returned by a stray refund, and the Go per-order key derivation agreeing with Core, on regtest; transcript in $log"
