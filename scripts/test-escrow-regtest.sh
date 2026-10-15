#!/usr/bin/env bash
# Bitcoin escrow check on a real Bitcoin Core regtest node (WR-escrow Phase 1, A-246). Run by hand, and in CI by the escrow-regtest
# job (A-438, .github/workflows/ci.yaml, docs/ci-cd.md), which starts a node like the one below with generated passwords.
# Two modes, never a public network:
#   - regtest (the default; what CI runs): it refuses any node that does not report chain "regtest", before any wallet
#     call. Everything below the private-signet paragraph describes this mode.
#   - --private-signet (A-443; by hand, not in CI): see "Private-signet mode" at the end of this comment.
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
# A-454 leg (shape (b) escrow signer, TestEscrowRegtestSigner; no order uses it yet): cmd/escrow-signer is built from
# this tree, given a fresh seed in a temporary directory and started against the node as its own RPC user (whitelisted
# to decodepsbt, descriptorprocesspsbt and getdescriptorinfo; the leg checks getblockchaininfo is refused); its account
# key, computed by Core from the account tprv (getdescriptorinfo), agrees with Core's derivation from the seed's master
# key along m/48h/1h/0h/2h; the App pins its fingerprint and key HMAC at startup. A hand-built shape-(b) escrow (two
# Core wallets' BIP48 per-order keys, rooted, and the signer's account/5/0/0) is funded, co-signed through the socket,
# signed by the buyer with Core, combined, finalized, accepted by testmempoolaccept, broadcast and mined. Refused with
# nothing signed: an input the buyer signed SIGHASH_NONE, another index, a script holding two of the signer's keys, and
# two escrows in one request. The seed is backed up to a throwaway PGP key and restored; a signer from the restored seed
# gives the identical signature (break-glass rehearsal). Restarted against a signer with another seed the App reports
# the pin mismatch (shape (a) stays on, the pins unchanged); with the signer stopped, waiting. No database column,
# signer log or transcript line holds the seed or a tprv.
#
# TestEscrowRegtestArbiterRestore: the arbiter wallet is backed up
# as the operator user (backupwallet, height recorded; the private descriptors exported for the fallback, never
# printed); a claim is built and signed by the vendor's Core wallet; the arbiter wallet is unloaded (the harness cannot
# delete files inside the node, so it restores under a new name, as onto a fresh node) and a watcher pass co-signs
# nothing; restorewallet brings it back and, after a restart with the fingerprint pin unchanged, the watcher co-signs,
# broadcasts and confirms the claim; a blank wallet given the exported descriptors with importdescriptors (timestamp
# "now") co-signs a second claim the same way. Encryption to the operator's key (docs/testnet-runbook.md) happens off
# the node and is not exercised here.
#
# A-471 leg (a payment to the arbiter wallet cannot disable escrow, TestEscrowRegtestArbiterDeposit): a regtest wallet pays
# the wpkh address of an escrow's market child key (shown in the buyer's descriptor; the arbiter wallet owns it), so the
# arbiter wallet has a transaction (txcount 1, one "receive"); a restart and a watcher retry's check both start escrow.
# QA4W4A-1: three transactions of 2000 outputs each pay the same address; a restart reads only what is newer than the
# checkpoint the last passing check left (now the tip) and escrow stays on, as after another restart. QA4W6-1: the payer
# pays it again replaceably and bumps the fee 1100 times (Core lists every unconfirmed replacement, each entry naming all
# the others in walletconflicts, about 82 MB from the checkpoint); a restart keeps escrow on, and once the last
# replacement is mined (one entry of about 74 KB) so do two more restarts. Control, behind
# them: the operator spends from the arbiter wallet to its own change address (listtransactions lists no send for it)
# and a restart leaves escrow disabled (not dedicated).
#
# A-441 leg (broadcast honesty, TestEscrowRegtestPrivateBroadcast): on a second regtest node started with -privatebroadcast
# -proxy=127.0.0.1:9 -listen=0 (a proxy nobody answers; same users and whitelists), the Tor-mode gate reads real
# getnetworkinfo answers (it lets the private node send, proxy set for IPv4 and IPv6 on Core 31.1, and holds the main
# node, no proxy). In APP_MODE=tor a funded escrow's claim, signed by the vendor's wallet, is co-signed and sent:
# sendrawtransaction answers its txid but it never reaches the mempool or the watch-only wallet (getprivatebroadcastinfo
# lists it), so the settlement stays broadcast; five blocks later nothing is reported, at six one staff notice and one
# order note say it is queued or stuck and /admin lists it, and later passes do not repeat it. generateblock then mines the
# stored transaction: one note clears the alert, the /admin line goes and it confirms.
#
# A-455 leg (a per-order wallet key's practice signature, TestEscrowRegtestPerOrderPractice; no escrow uses such a key
# yet): a Core wallet's seed stands in for a BIP39 seed; its BIP48 account key is registered on a real App as a per-order
# wallet key. From the practice page alone (the path, the two rooted cosigner Vpubs, the first address and the PSBT) the
# operator builds the per-order practice wallet from the seed as a private descriptor (root/48h/1h/0h/2h/2147483647/0/0
# with both cosigners at /0/0): Core derives the address the page shows and signs the PSBT with descriptorprocesspsbt,
# and that signature activates the key (Core's finalizepsbt verifies it beside the server's throwaway signature;
# testmempoolaccept reports missing-inputs). Refused first: the unsigned PSBT, the vendor's Core wallet holding the
# account's shape-(a) range (it has no signature to give), and the per-order wallet's SIGHASH_NONE signature.
#
# A-446 leg (the public-network smoke's body, TestEscrowRegtestSmoke; scripts/test-escrow-public.sh runs the same body on
# public signet or testnet4 as TestEscrowPublicSmoke): practice signing by a buyer's and a vendor's Core wallets, then two
# escrows funded together, one at the price (completed on funding, claimed by the vendor, co-signed, broadcast and
# confirmed, each output checked with gettxout) and one short of it (cancelled by the buyer and refunded to the pinned
# refund address, confirmed). Every txid is logged as the public script's manifest reads it.
#
# A-456a leg (a shape-(b) escrow created and watched, TestEscrowRegtestPerOrderEscrow): two Core wallets' seeds stand in
# for the parties' BIP39 seeds and a simulated signer serves a rooted account key (no signing). The real checkout form builds the escrow at per-order index
# 2 (the app's Go derivation must agree with Core's); the operator's Core derives the same address from the parties'
# private roots at 2/0/0 and the signer key at <m>/0/0, and Go's own derivation gives it too. The watch-only wallet holds
# the order wallet's siblings /0/1 and /1/9; the operator pays /0/1 and /1/3, 3 blocks are mined, and one watcher pass
# records both deposits for the order, left out and not credited, the order still awaiting payment.
#
# QA5W4A leg (the watch-only wallet repair of a per-order escrow, TestEscrowRegtestPerOrderRepair): an
# escrow like the A-456a leg's, funded at its address and at /0/1 and /1/3, then the app restarted
# onto empty watch-only wallets standing in for ones restored from before the escrow. Signer up: one pass imports
# everything again, every address held, labelled and solvable, the deposits found. Signer down: the same, in one pass
# (QA5W5A-1: checkout stores the market's order wallet key with the parties', so the repair never asks the signer). The
# vendor's key re-registered: the siblings still solvable (the order wallet keys stored at checkout). And Core's own
# behaviour: an address an addr(…) import held first stays unsolvable after its bare or ranged descriptor is imported,
# and the orphan sweep's read reports it unsolvable.
#
# A-456b legs (per-order escrows settled through the real escrow signer; cmd/escrow-signer is built from this tree with a
# fresh seed and the signer's RPC user, and two Core wallets' seeds stand in for the parties' BIP39 seeds, a party's
# order wallet played by Core's descriptorprocesspsbt with its private root at its per-order path). TestEscrowRegtestShapeBRelease:
# a per-order escrow created through the checkout form and paid its price completes; the vendor claims it, the watcher
# builds the claim, the vendor's order wallet signs its copy and the vendor uploads it as a binary PSBT file through the
# multipart sign form; the next pass co-signs it through the signer at the escrow's market index, checks it and
# broadcasts it; after the confirmations it is confirmed and gettxout shows the vendor's output.
# TestEscrowRegtestShapeBSiblingStray (QA5W3A-2): after the order completed, the funder pays the order wallet's sibling
# /0/3; the watcher records it left out and builds a stray refund with the sibling's own witness script (Core derives the
# sibling's address from the stored keys); the buyer's order wallet signs at its /0/3 key, the signer co-signs at its
# sibling key, and the refund is broadcast, confirmed and checked with gettxout. TestEscrowRegtestShapeBSiblingStraySignerDown
# (QA5W5A-1): a sibling /0/2 deposit's stray refund is built, then the signer's socket is moved aside: both parties' order
# pages are served with the sign form, the buyer's copy downloads, the buyer's order wallet signs it and the paste is
# accepted, and a pass records signer-waiting with nothing rebuilt; with the socket back it is co-signed, broadcast,
# confirmed and checked with gettxout. TestEscrowRegtestShapeBBump: the vendor's claim broadcast at 1 sat/vB is bumped to
# 5 sat/vB; the vendor's order wallet signs the bump copy, the signer co-signs it, it replaces the claim in the mempool
# and confirms. Neither the signer's log nor the database holds the seed or a tprv.
#
# A-527 leg (the watch-only wallet repair on a pruned node, TestEscrowRegtestPrunedRepair; on the third node, started with
# -prune=550 -fastprune, block times set with setmocktime): a per-order escrow created through the checkout form (its
# creation set 10 days back) is paid; a day later 400 blocks and 6 hours after those 300 more are mined and
# pruneblockchain prunes past the deposit's block; a newer escrow, created after the block at pruneheight, is paid.
# Core's own behaviour is recorded first, as the operator into a scratch watch-only wallet. Observed on Core 31.1:
# importdescriptors of the old escrow's descriptor from its creation time answers success false, error code -1
# ("Rescan failed for descriptor with timestamp … This error could be caused by pruning or data corruption …"), yet the
# wallet keeps the descriptor (getaddressinfo ismine true, its escrow:<order ID> label) and lists nothing it received;
# in one call with a request whose timestamp is after the block at pruneheight (plus Core's 2-hour window), only the old
# request fails. Then the restore gate and escrow_sweep_required are set and the App restarts onto an empty watch-only
# wallet: one pass imports only the newer escrow (its order completes), lists the old one as cannot be rescanned on
# this pruned node (not imported, its order still awaiting payment), and keeps the gate and the marker set; /admin
# names the order; a second pass imports nothing.
#
# Ruled Phase 1 legs and where they run: release (Settlement, Cosign: claim), refund (Settlement, Cosign), dispute
# (Cosign), comma injection refused (Gate), NONE paste refused (PracticeSign, Settlement), forged market slot refused
# (Gate), conflicting outcome refused (Gate), spent-outpoint build refused (Gate), restart mid-send rebroadcasts the
# same tx (Cosign: refund), reorg + double-spend flagged (Recovery), high index via keypoolrefill (Cosign: claim at
# index 2501), restore gate + reconciliation (Gate; Recovery: co-spend and orphan), two escrows never mixed (Gate), fee
# cap (Gate); also A-339, both-signed and stray refund (Cosign), A-344 and a plain reorg (Recovery), A-356 and A-359
# (Gate); A-428, a deposit conflicted at an unpaid cancellation and mined afterwards (Conflicted).
#
# Needs a regtest node with three RPC users: an operator user with full rights (sets up the wallets, standing in
# for the buyer's own node), the app's user restricted by rpcwhitelist exactly as docs/operator-guide.md
# ("Bitcoin escrow") describes, and the escrow signer's user restricted to decodepsbt, descriptorprocesspsbt and getdescriptorinfo ("Escrow
# signer"); a second node like it (the same three users) started with -privatebroadcast -proxy=127.0.0.1:9 -listen=0
# for the A-441 leg (its own chain: the two nodes are not connected); a third like it started with -prune=550
# -fastprune for the A-527 leg (its own chain too; a leg that fails part-way can leave its tip region pruned, so start
# it afresh after one); and a disposable PostgreSQL database. For example:
#
#   salt=$(openssl rand -hex 16); hash=$(printf %s app-pass | openssl dgst -sha256 -hmac "$salt" | cut -d' ' -f2)
#   ssalt=$(openssl rand -hex 16); shash=$(printf %s signer-pass | openssl dgst -sha256 -hmac "$ssalt" | cut -d' ' -f2)
#   docker run -d --name escrow-regtest -p 127.0.0.1:18443:18443 bitcoin/bitcoin:31.1 bitcoind -regtest -fallbackfee=0.0002 \
#     -server=1 -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 -rpcuser=operator -rpcpassword=operator-pass \
#     "-rpcauth=opsecmkt-escrow:$salt\$$hash" \
#     "-rpcauth=opsecmkt-signer:$ssalt\$$shash" -rpcwhitelistdefault=0 \
#     -rpcwhitelist=opsecmkt-signer:decodepsbt,descriptorprocesspsbt,getdescriptorinfo \
#     -rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getnetworkinfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions,listsinceblock,getblockhash,getblockheader,setlabel
#   # the same command again as escrow-regtest-private, published on 127.0.0.1:18453, ending in:
#   #   -privatebroadcast -proxy=127.0.0.1:9 -listen=0
#   # and again as escrow-regtest-pruned, published on 127.0.0.1:18463, ending in:
#   #   -prune=550 -fastprune
#   ESCROW_REGTEST_OPERATOR_RPC_URL=http://operator:operator-pass@127.0.0.1:18443 \
#   ESCROW_REGTEST_APP_RPC_URL=http://opsecmkt-escrow:app-pass@127.0.0.1:18443 \
#   ESCROW_REGTEST_SIGNER_RPC_URL=http://opsecmkt-signer:signer-pass@127.0.0.1:18443 \
#   ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL=http://operator:operator-pass@127.0.0.1:18453 \
#   ESCROW_REGTEST_PRIVATE_APP_RPC_URL=http://opsecmkt-escrow:app-pass@127.0.0.1:18453 \
#   ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL=http://operator:operator-pass@127.0.0.1:18463 \
#   ESCROW_REGTEST_PRUNED_APP_RPC_URL=http://opsecmkt-escrow:app-pass@127.0.0.1:18463 \
#   TEST_DATABASE_URL=postgres://…/a_disposable_database ./scripts/test-escrow-regtest.sh
#
# The transcript is written to ESCROW_REGTEST_LOG (default artifacts/real-chain/test-escrow-regtest-<UTC time>.log,
# gitignored). URLs reach curl on stdin and are never printed. A transcript holding a serialized tprv fails the run
# (A-454: no leg prints private key material).
#
# Private-signet mode (A-443): ./scripts/test-escrow-regtest.sh --private-signet, with TEST_DATABASE_URL and Docker
# (Linux: the host reaches the containers by their address on the network). It is private signet, not public. The
# script starts two Bitcoin Core nodes itself (ESCROW_SIGNET_IMAGE, default bitcoin/bitcoin:31.1) with
# -chain=signet -signetchallenge=51 (a block needs no signature, so Core's own generatetoaddress mines it; no fetched
# code) on a Docker network created with --internal (no route off the host, no published port), with the same four RPC
# users and whitelists as above and generated passwords: node A (the miner; no outbound connection, listening for node
# B) and node B (the app's node; connecting to node A only, not listening), so node B has a peer, as the sync guard
# requires on a public chain; neither uses DNS seeds or fixed seeds. Node A's
# miner wallet is mined to maturity by TestEscrowPrivateSignetPrepare (generatetoaddress with maxtries 2000000000, each
# call's block count checked: Core's default returns [] silently on signet), and its datadir is cached
# (ESCROW_SIGNET_CACHE, default under artifacts/real-chain/private-signet-cache/) so a later run skips those minutes;
# each run mines a fresh block on it. Every leg that needs no regtest-only RPC then runs against node B, a leg's
# 101-block maturity replaced by a payment from node A's miner wallet (matureFunds), plus TestEscrowRegtestIsolated (node
# B leaves the network: an expired order is not cancelled; back on the network: its deposit is credited). Not run, with
# the reason printed: TestEscrowRegtestRecovery and TestEscrowRegtestConflicted (invalidateblock, reconsiderblock,
# prioritisetransaction) and TestEscrowRegtestPrivateBroadcast (a -privatebroadcast node and generateblock, which cannot
# find a signet block). Every transcript line carries "[private signet, not public]" and the transcript starts with a
# header (node version, commit, the nodes' command line with credentials redacted, start and finish times); default
# ESCROW_REGTEST_LOG artifacts/real-chain/test-escrow-private-signet-<UTC time>.log. Containers, their volumes and the
# network are removed on exit (ESCROW_SIGNET_PREFIX names them, default escrow-private-signet; an existing one is
# refused). Runtime on a loaded 4-core host: about 25 minutes the first time (9 of them maturing node A's coins) and about
# 20 with the cached datadir, so it is not in CI.
set -euo pipefail

private_signet() {
  local label="private signet, not public"
  [[ -n "${TEST_DATABASE_URL:-}" ]] || { echo "Set TEST_DATABASE_URL (see the comment at the top of this script)." >&2; exit 2; }
  for c in docker curl go openssl git; do
    command -v "$c" >/dev/null || { echo "$c is required" >&2; exit 2; }
  done
  cd "$(dirname "$0")/.."
  local image="${ESCROW_SIGNET_IMAGE:-bitcoin/bitcoin:31.1}" prefix="${ESCROW_SIGNET_PREFIX:-escrow-private-signet}"
  local net="$prefix-net" a="$prefix-a" b="$prefix-b"
  local cache="${ESCROW_SIGNET_CACHE:-artifacts/real-chain/private-signet-cache/$(printf %s "$image" | tr -c 'A-Za-z0-9.' '-')-challenge51.tar}"
  local log="${ESCROW_REGTEST_LOG:-artifacts/real-chain/test-escrow-private-signet-$(date -u +%Y%m%dT%H%M%SZ).log}"
  local started body
  started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  for n in "$a" "$b"; do
    if docker container inspect "$n" >/dev/null 2>&1; then
      echo "Refusing: a container named $n exists (remove it, or set ESCROW_SIGNET_PREFIX)." >&2
      exit 2
    fi
  done
  if docker network inspect "$net" >/dev/null 2>&1; then
    echo "Refusing: a Docker network named $net exists (remove it, or set ESCROW_SIGNET_PREFIX)." >&2
    exit 2
  fi
  mkdir -p "$(dirname "$log")" "$(dirname "$cache")"
  body=$(mktemp)
  # shellcheck disable=SC2064 # expand the names now
  trap "docker rm -f -v '$a' '$b' >/dev/null 2>&1 || true; docker network rm '$net' >/dev/null 2>&1 || true; rm -f '$body' '$cache.part'" EXIT
  say() { printf '[%s] %s\n' "$label" "$*" | tee -a "$body"; }

  # The users' whitelists are the ones this script's header documents (TestEscrowWhitelistDocumented checks the escrow line).
  local escrow_user=opsecmkt-escrow whitelist
  whitelist=$(sed -n "s/^#  *-rpcwhitelist=$escrow_user:\([a-z,]*\)\$/\1/p" scripts/test-escrow-regtest.sh)
  [[ -n $whitelist ]] || { echo "the header's rpcwhitelist lines were not found" >&2; exit 2; }
  local operator_pass app_pass signer_pass
  operator_pass=$(openssl rand -hex 24) app_pass=$(openssl rand -hex 24) signer_pass=$(openssl rand -hex 24)
  rpcauth() { # rpcauth <user> <password>: user:salt$HMAC-SHA256(salt, password), the form bitcoind reads
    local salt hash
    salt=$(openssl rand -hex 16)
    hash=$(printf %s "$2" | openssl dgst -sha256 -hmac "$salt" | sed 's/^.* //')
    printf '%s:%s$%s' "$1" "$salt" "$hash"
  }
  local common=(bitcoind -chain=signet -signetchallenge=51 -dnsseed=0 -fixedseeds=0 -fallbackfee=0.0002 -server=1
    -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0 -rpcport=38332 -port=38333 -rpcwhitelistdefault=0
    "-rpcwhitelist=$escrow_user:$whitelist"
    -rpcwhitelist=opsecmkt-signer:decodepsbt,descriptorprocesspsbt,getdescriptorinfo)
  local users=("-rpcauth=$(rpcauth operator "$operator_pass")" "-rpcauth=$(rpcauth opsecmkt-escrow "$app_pass")"
    "-rpcauth=$(rpcauth opsecmkt-signer "$signer_pass")")
  rpc() { # rpc <ip> <method> [params]: the operator's JSON-RPC answer; the URL reaches curl on stdin, never printed
    printf 'url = "http://operator:%s@%s:38332/"\n' "$operator_pass" "$1" |
      curl -sS --max-time 30 --noproxy '*' -H 'Content-Type: application/json' --config - \
        --data "{\"jsonrpc\":\"1.0\",\"id\":\"signet\",\"method\":\"$2\",\"params\":${3:-[]}}" 2>/dev/null || true
  }
  ip_of() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }
  field() { sed -n "s/.*\"$1\":\([^,}]*\).*/\1/p"; }
  wait_signet() { # wait_signet <ip> <name>: until the node answers on signet with signet_challenge 51
    local info
    for _ in $(seq 120); do
      info=$(rpc "$1" getblockchaininfo)
      if grep -q '"chain":"signet"' <<<"$info" && grep -q '"signet_challenge":"51"' <<<"$info"; then
        return 0
      fi
      sleep 1
    done
    echo "Refusing: $2 did not answer on the private signet (chain signet, signet_challenge 51) within 120 seconds." >&2
    exit 1
  }

  docker network create --internal "$net" >/dev/null
  docker create --name "$a" --network "$net" "$image" "${common[@]}" -connect=0 -listen=1 "${users[@]}" >/dev/null
  local cached=no
  if [[ -s "$cache" ]]; then
    docker cp - "$a:/home/bitcoin" <"$cache"
    cached=yes
  fi
  docker start "$a" >/dev/null
  local ip_a ip_b
  ip_a=$(ip_of "$a")
  wait_signet "$ip_a" "node A"
  say "node A ($a) up on the private signet; datadir from cache: $cached"
  local miner_url="http://operator:$operator_pass@$ip_a:38332"
  set +e
  ESCROW_REGTEST_CHAIN=signet ESCROW_REGTEST_MINER_RPC_URL="$miner_url" go test -count=1 -timeout 30m \
    -run '^TestEscrowPrivateSignetPrepare$' -v ./internal/market/ 2>&1 | sed -u "s/^/[$label] /" | tee -a "$body"
  local prep=${PIPESTATUS[0]}
  set -e
  if [[ $prep -ne 0 ]] || ! grep -q -- "--- PASS: TestEscrowPrivateSignetPrepare " "$body"; then
    echo "FAILED: node A's preparation (TestEscrowPrivateSignetPrepare); see $log" >&2
    cp "$body" "$log"
    exit 1
  fi
  if [[ $cached == no ]]; then
    docker stop -t 120 "$a" >/dev/null
    docker cp "$a:/home/bitcoin/.bitcoin" - >"$cache.part"
    mv "$cache.part" "$cache"
    docker start "$a" >/dev/null
    wait_signet "$ip_a" "node A"
    say "node A's datadir cached at $cache (a later run starts from it)"
  fi
  docker run -d --name "$b" --network "$net" "$image" "${common[@]}" "-connect=$a:38333" -listen=0 "${users[@]}" >/dev/null
  ip_b=$(ip_of "$b")
  wait_signet "$ip_b" "node B"
  local height_a height_b peers ibd
  for i in $(seq 180); do
    height_a=$(rpc "$ip_a" getblockcount | field result)
    height_b=$(rpc "$ip_b" getblockcount | field result)
    peers=$(rpc "$ip_b" getconnectioncount | field result)
    ibd=$(rpc "$ip_b" getblockchaininfo | field initialblockdownload)
    if [[ -n $height_a && $height_a == "$height_b" && ${peers:-0} -gt 0 && $ibd == false ]]; then
      break
    fi
    if [[ $i -eq 180 ]]; then
      echo "FAILED: node B did not sync from node A within 180 seconds (heights $height_a/$height_b, peers $peers, IBD $ibd)" >&2
      exit 1
    fi
    sleep 1
  done
  say "node B ($b) synced from node A: height $height_b, $peers peer(s), initialblockdownload false"

  local legs=(PracticeSign Funding Settlement Cosign Gate Bump Physical Deadline Reserve BIP32 Signer ArbiterRestore ArbiterDeposit PerOrderPractice PerOrderEscrow PerOrderRepair ShapeBRelease ShapeBSiblingStray ShapeBSiblingStraySignerDown ShapeBBump Smoke Isolated)
  local pattern
  pattern=$(IFS='|'; echo "${legs[*]}")
  set +e
  ESCROW_REGTEST_CHAIN=signet ESCROW_REGTEST_MINER_RPC_URL="$miner_url" \
    ESCROW_REGTEST_OPERATOR_RPC_URL="http://operator:$operator_pass@$ip_b:38332" \
    ESCROW_REGTEST_APP_RPC_URL="http://opsecmkt-escrow:$app_pass@$ip_b:38332" \
    ESCROW_REGTEST_SIGNER_RPC_URL="http://opsecmkt-signer:$signer_pass@$ip_b:38332" \
    go test -count=1 -timeout 120m -run "^TestEscrowRegtest($pattern)\$" -v ./internal/market/ 2>&1 | sed -u "s/^/[$label] /" | tee -a "$body"
  local status=${PIPESTATUS[0]}
  set -e
  local failed=0 leg
  for leg in "${legs[@]}"; do
    leg=TestEscrowRegtest$leg
    if grep -q -- "--- PASS: $leg " "$body"; then
      say "PASS: $leg"
    elif grep -q -- "--- FAIL: $leg " "$body"; then
      say "FAILED: $leg"
      failed=1
    elif grep -q -- "--- SKIP: $leg " "$body"; then
      say "SKIPPED: $leg (a skipped leg is not a pass)"
      failed=1
    else
      say "NOT RUN: $leg (go test reported no result for it)"
      failed=1
    fi
  done
  say "NOT RUN on the private signet: TestEscrowRegtestRecovery (it fakes a reorganisation with invalidateblock and reconsiderblock, test-only RPCs this harness treats as regtest-only: on two connected nodes the invalidated block stays on node A, so what runs would be a split network, not the single-node reorganisation the leg is written for)"
  say "NOT RUN on the private signet: TestEscrowRegtestConflicted (the same reorganisation, with invalidateblock, and prioritisetransaction to bring the conflicted deposit back)"
  say "NOT RUN on the private signet: TestEscrowRegtestPrunedRepair (it needs a node started with -prune=550 -fastprune whose block times setmocktime sets and pruneblockchain prunes, a regtest-only setup)"
  say "NOT RUN on the private signet: TestEscrowRegtestPrivateBroadcast (it needs a node started with -privatebroadcast behind a dead proxy and mines the queued transaction with generateblock, which takes no maxtries: Core's fixed 1000000 tries do not find a signet block)"
  if grep -qE 'tprv[1-9A-HJ-NP-Za-km-z]{100,}' "$body"; then
    say "FAILED: the transcript holds a serialized tprv (private key material)"
    failed=1
  fi
  local version commit
  version=$(rpc "$ip_a" getnetworkinfo | sed -n 's/.*"subversion":"\([^"]*\)".*/\1/p')
  commit=$(git rev-parse HEAD)
  if [[ -n $(git status --porcelain 2>/dev/null) ]]; then
    commit="$commit (worktree has uncommitted changes)"
  fi
  {
    echo "# Bitcoin escrow real-chain legs on a private signet: $label"
    echo "# $label: signet_challenge 51 on Docker network $net (--internal: no route off this host); no public network was contacted"
    echo "# node version: $version (image $image)"
    echo "# commit: $commit"
    echo "# command line (both nodes; credentials redacted): ${common[*]} -rpcauth=operator:<redacted> -rpcauth=opsecmkt-escrow:<redacted> -rpcauth=opsecmkt-signer:<redacted>"
    echo "#   node A: -connect=0 -listen=1 (the miner); node B: -connect=$a:38333 -listen=0 (the app's node)"
    echo "# node A datadir from cache: $cached ($cache)"
    echo "# started: $started  finished: $(date -u +%Y-%m-%dT%H:%M:%SZ)  go test exit: $status"
    echo
    cat "$body"
  } >"$log"
  if [[ $status -ne 0 || $failed -ne 0 ]]; then
    echo "FAILED ($label): go test exited $status; see $log" >&2
    exit 1
  fi
  echo "PASS ($label): ${#legs[@]} legs on a private signet (two nodes, signet_challenge 51), 3 regtest-only legs not run; transcript in $log"
  exit 0
}

case "${1:-}" in
  --private-signet) private_signet ;;
  "") ;;
  *) echo "Usage: $0 [--private-signet]" >&2; exit 2 ;;
esac

for v in ESCROW_REGTEST_OPERATOR_RPC_URL ESCROW_REGTEST_APP_RPC_URL ESCROW_REGTEST_SIGNER_RPC_URL ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL ESCROW_REGTEST_PRIVATE_APP_RPC_URL ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL ESCROW_REGTEST_PRUNED_APP_RPC_URL TEST_DATABASE_URL; do
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
for v in ESCROW_REGTEST_OPERATOR_RPC_URL ESCROW_REGTEST_APP_RPC_URL ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL ESCROW_REGTEST_PRIVATE_APP_RPC_URL ESCROW_REGTEST_PRUNED_OPERATOR_RPC_URL ESCROW_REGTEST_PRUNED_APP_RPC_URL; do
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
go test -count=1 -timeout 15m -run '^TestEscrowRegtest(PracticeSign|Funding|Settlement|Cosign|Recovery|Gate|Bump|Physical|Deadline|Reserve|Conflicted|BIP32|Signer|ArbiterRestore|ArbiterDeposit|PrivateBroadcast|PerOrderPractice|PerOrderEscrow|PerOrderRepair|ShapeBRelease|ShapeBSiblingStray|ShapeBSiblingStraySignerDown|ShapeBBump|Smoke|PrunedRepair)$' -v ./internal/market/ 2>&1 | tee "$log"
status=${PIPESTATUS[0]}
set -e
# Each leg is reported from its own go test result line, so one failing leg never makes another read as failed.
failed=0
for leg in TestEscrowRegtestPracticeSign TestEscrowRegtestFunding TestEscrowRegtestSettlement TestEscrowRegtestCosign TestEscrowRegtestRecovery TestEscrowRegtestGate TestEscrowRegtestBump TestEscrowRegtestPhysical TestEscrowRegtestDeadline TestEscrowRegtestReserve TestEscrowRegtestConflicted TestEscrowRegtestBIP32 TestEscrowRegtestSigner TestEscrowRegtestArbiterRestore TestEscrowRegtestArbiterDeposit TestEscrowRegtestPrivateBroadcast TestEscrowRegtestPerOrderPractice TestEscrowRegtestPerOrderEscrow TestEscrowRegtestPerOrderRepair TestEscrowRegtestShapeBRelease TestEscrowRegtestShapeBSiblingStray TestEscrowRegtestShapeBSiblingStraySignerDown TestEscrowRegtestShapeBBump TestEscrowRegtestSmoke TestEscrowRegtestPrunedRepair; do
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
if grep -qE 'tprv[1-9A-HJ-NP-Za-km-z]{100,}' "$log"; then
  echo "FAILED: the transcript holds a serialized tprv (private key material); see $log" >&2
  failed=1
fi
if [[ $status -ne 0 || $failed -ne 0 ]]; then
  echo "FAILED: go test exited $status; see $log" >&2
  exit 1
fi
echo "PASS: practice signing, escrow creation and funding, settlement building and party signing, co-signing, broadcast and confirmation, recovery and reconciliation, the Phase 1 gate's remaining adversarial legs, a same-recipient fee bump, a physical order's release signed by the buyer and the vendor with no arbiter signature, one released at its deadline with the vendor's signature co-signed by the market, no escrow key handed out again after a restore, a deposit conflicted at an unpaid cancellation and mined afterwards returned by a stray refund, the Go per-order key derivation agreeing with Core, the escrow signer co-signing a shape-(b) escrow and refusing what it must, the arbiter wallet backed up, wiped, restored (restorewallet, then the importdescriptors fallback) and co-signing with its fingerprint pin unchanged, a payment to the arbiter wallet (and 6000 dust outputs, and 1100 fee bumps of one payment) leaving escrow on while a spend from it disables escrow, and a settlement queued by -privatebroadcast behind a dead proxy reported queued or stuck after 6 blocks and cleared once mined, a per-order wallet key's practice signed by Core from the seed and the practice page, a per-order escrow created with Go and Core agreeing on its address and its sibling deposits recorded left out, its watch-only wallet repaired after a restore (with the signer up, down, and after a key change), a per-order escrow's release uploaded as a PSBT file and co-signed by the real escrow signer, a sibling deposit returned by a stray refund spending the sibling's own script and co-signed by the signer (and, with the signer down meanwhile, still shown, downloaded and signed, waiting only for the co-signature), a per-order claim fee-bumped and co-signed by the signer, the public-network smoke's legs (practice signing, one escrow claimed and one refunded, each confirmed), and the watch-only wallet repair on a pruned node leaving an escrow older than the prune height listed with the restore gate kept, on regtest; transcript in $log"
