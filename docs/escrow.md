# Per-order escrow

> [!WARNING]
> **No release contains escrow, and the pooled wallet stays the default.** Every deposit sits in the
> operator's pooled custodial wallet for its currency ([operator guide](operator-guide.md#payments),
> [implementation status](implementation-status.md#p5-payments)) unless an operator switches on the first
> escrow phase, which runs **on regtest only** (a local test chain whose coins have no value) and is **off
> by default** (`ESCROW_BTC`, [operator guide](operator-guide.md#bitcoin-escrow-regtest-only)). That phase,
> Bitcoin escrow for digital orders, is built and tested on regtest on the development branch
> ([Phase 1](#phase-1-bitcoin-escrow-on-regtest)); it has not run on any public test network or on mainnet,
> and it is not verified for real payments. This page describes the escrow design the project owner and its
> design review have settled, what it will and will not protect, and how it ships. With each phase, this
> page, the [README](../README.md), the [implementation status](implementation-status.md), the
> [operator guide](operator-guide.md), the [test-network runbook](testnet-runbook.md), the
> [acceptance checks](acceptance.md) and [UPGRADING](../UPGRADING.md) are updated in the same change.
> Everything below [Phases](#phases) that no phase has built yet is a plan.

## Why

With a pooled wallet, whoever controls the server's wallet controls every buyer's deposit: a server
compromise, a seizure, a lost wallet file or an operator who disappears takes all of it at once. Per-order
escrow keeps each order's funds apart. On Bitcoin it also keeps them out of the server's sole control: the
server alone cannot move them.

## Owner decisions (2026-09-29)

| Question | Decision |
| --- | --- |
| Pooled wallet or escrow | Each order has its own escrow, which the buyer pays within the payment window. There is no pooled balance and no treasury. |
| Bitcoin keys | Buyer, vendor and market each hold one key of a 2-of-3 multisig. Buyer and vendor use keys from their own wallets; the market holds only the arbitrator key. |
| Monero | One account per order inside a monthly escrow wallet on the server (still custodial, see [Monero](#monero-one-account-per-order)). Each monthly wallet is deleted 90 days after its last order closes. 2-of-3 Monero multisig is evaluated later. |
| Pooled mode | Kept, with safety fixes only, until escrow ships and is verified; then removed, with an upgrade path for orders still open. |
| Digital (automatic-delivery) orders | Once the escrow is funded, the content is delivered and the order completes. The vendor claims the funds whenever they like by signing; the market co-signs that claim automatically. |
| Physical orders | The buyer signs the release in their own wallet, together with the vendor. If the buyer has not signed and no dispute is open 14 days after the order is marked shipped, the market co-signs the vendor's claim. An administrator can change that default within 7–30 days; each order keeps the deadline it started with. A dispute stops the clock. Funds are never released just because an order is marked shipped. |
| Disputes | An administrator decides, confirming with their password (and authenticator code when enrolled). The market key then co-signs with the party who won. |
| Network fee | Taken from the escrowed amount on every settlement, refunds included. |
| Market fee | Taken from a fulfilled order's escrow and sent to a destination the administrator sets per currency. On a mainnet market that destination must be a watch-only descriptor, so each settlement pays a fresh fee address; a test-network market may use one fixed address. |

## Bitcoin: 2-of-3 multisig per order

**Keys.** Buyer and vendor each register an extended public key, with its key origin (fingerprint and
derivation path), exported from a wallet used only for this market. The server will store it encrypted and
derive a fresh child key from it for each order. The market will use a fresh child of its arbitrator key for
each order too. Each order's escrow address comes from those three child keys:

```text
wsh(sortedmulti(2, <buyer key>, <vendor key>, <market key>))
```

Any two of the three keys can spend; no single key can. The server will accept exactly one key per party,
compose the descriptor itself, and refuse duplicate keys, the market's own key offered as a party key and a
key already used by another account: on regtest, a descriptor that repeats a key, or a key field holding two
keys, let one party spend alone. A pasted private key will be refused, with advice to replace it.

**Proving your wallet can sign.** Before a key can be used for an order, its owner will sign a practice
transaction with it: one that spends coins that do not exist, so it can never be broadcast. This catches a
wrong key or path before any money depends on it.

**Checking the address.** The order page will show the escrow descriptor with its checksum (your own key with
its origin, the other two as plain keys) and offer it as a download, so you can load it into your wallet and
see the same address before paying. The server will import each order's descriptor into a watch-only wallet
before it shows the address.

**Signing without JavaScript.** The server will hold no buyer or vendor private key. For each settlement it
will build the transaction itself as an unsigned PSBT (BIP 174): only that order's escrow as input, the
expected outputs, an explicit fee rate and a fee cap. The party downloads or copies the PSBT text, checks the
outputs listed on the page against their wallet, signs it in their own wallet and pastes the signed PSBT into
a form on the order page. The market will sign only its own stored copy of the transaction it built, only once
the order has reached its outcome, and only once per escrowed coin; a pasted PSBT for any other transaction
will be refused. The server cannot check a pasted signature itself (it does not verify signatures); its validity is
checked only when the market finalizes the transaction, and a signature that does not verify is then discarded and
the settlement waits for a signature again, with both parties told. (Bitcoin Core signs any PSBT it is given, so this check is what keeps the market key from
signing an attacker's transaction.) Only the server broadcasts settlements; after that the page shows the
final transaction, which is public, so you can rebroadcast it from your own wallet if you want.

**Who signs each outcome.** The outputs of each settlement are fixed before anyone signs. Where the market
co-signs, it needs one signature from either party whose key is in the escrow; if buyer and vendor both sign,
the market does not sign at all.

| Outcome | Signers |
| --- | --- |
| Digital order: vendor claims | Vendor, then the market automatically |
| Physical order: release | Buyer and vendor; after the deadline with no dispute, vendor and market |
| Refund (order cancelled) | One party's signature, then the market; the refund goes to the address the buyer gave at checkout |
| Dispute decided for the buyer or the vendor | One party's signature, then the market |
| Buyer and vendor agree | Buyer and vendor; the market is not needed |
| Vendor never ships | The buyer can cancel after the listing's dispatch window and is refunded |
| Coins sent after the order closed | The buyer can recover them from the order page (buyer and market) |

**Fees.** Every settlement will pay its network fee from the claimant's share of the escrow, so the recipient
receives the escrowed amount minus that fee. A fulfilled order's settlement will also have a market-fee
output. Amounts too small to send on their own are added to the claimant's output and noted on the order.

**What this will protect.** The server's watch-only wallet and its one key cannot move an escrow alone. A
seized or copied server holds the market key and the public descriptors, not the funds. This holds for
escrows created while the server was honest, and only when the signers check the address and the
transaction outputs in their own wallet: a server that has been taken over can show a buyer a descriptor
with its own key in place of the vendor's, and then holds two of the three keys for that escrow. Checking the
descriptor in your wallet is what detects that.

**What it will not protect.**

- The market key plus either party's key can spend. An operator who colludes with (or is) a vendor or buyer
  can take that order's funds; the arbitrator is trusted to that extent.
- The market co-signs digital-order claims, deadline releases, refunds and dispute outcomes automatically, so
  its key is usable from the server. Someone who takes over the server and also controls a vendor or buyer
  account can settle that account's escrows to it.
- A party who loses their key and never signs leaves settlement to the other party and the market.
- The other party knows the escrow address, so they can see on the blockchain which coins funded it and
  where it was paid. Pay from coins you do not mind the vendor linking to you.
- Your registered extended public key lets the server derive every address of that wallet account, so it
  can link your market payments to anything else that account received. Use a wallet (or account) made only
  for this market.
- Wallet support is limited at first: on regtest, Bitcoin Core signed these escrow transactions and Electrum
  (the wallet built into Tails) could not, because it only signs multisig wallets built from every cosigner's
  extended key. The first Bitcoin phase supports Bitcoin Core. Electrum and Sparrow support will be designed and
  decided before escrow runs on any public test network; hardware wallets have not been tested.

## Monero: one account per order

Each order will get its own account inside a monthly escrow wallet on the server, which runs on its own
wallet service. The buyer pays into that account, and it is settled in one transaction to the recipient (plus
the market-fee output on a fulfilled order), with the network fee taken from the amount. Every payment out
names the order's account, so one order's coins can never pay another's settlement. Monero requires 10
confirmations before received funds can be spent, so a settlement can be sent only after that (about 20
minutes on mainnet).

A monthly wallet is deleted 90 days after its last order closes. Monero sent to an order's address within
that time is refunded to the buyer automatically; after it, the order page says it cannot be recovered.

**This is still custodial.** The server will hold every escrow wallet's keys, so the operator, or anyone who
takes over the server, can spend them. Accounts keep orders apart and remove the pooled balance; they do not
stop the operator. Monero 2-of-3 multisig is wallet-level and experimental, and is evaluated later.

## Until escrow ships

The pooled wallet keeps working exactly as documented in the [operator guide](operator-guide.md#payments)
and the [test-network runbook](testnet-runbook.md). It receives safety fixes only. After escrow is verified
on regtest and a public test network, pooled mode is removed, with an upgrade path for orders still open at
the upgrade.

## Phases

Each phase is its own change with its own database migrations and tests, and this page claims only what a
shipped phase has proven. The full rules and acceptance criteria are in the project's review ledger
(`.claude/war-room/ledger.md`, "WR-escrow Round D ruling", items A-244 to A-249).

| Phase | Scope | Status |
| --- | --- | --- |
| 0 | Preparation in the current payment code: one settlement per order enforced by the database, a single settlement dispatcher, password confirmation for dispute decisions | Done on the development branch, not yet released; changes nothing for users except that resolving a dispute asks for the resolver's password |
| Wallet check | Scripted signing test with Electrum and Bitcoin Core on regtest, before any escrow database change | Done on regtest: Core signs; Electrum cannot sign this layout |
| 1 | Bitcoin escrow for digital orders on regtest, behind a setting that is off by default | Built and tested on regtest on the development branch, not released, behind `ESCROW_BTC` (off by default, regtest only): see [Phase 1](#phase-1-bitcoin-escrow-on-regtest) for what it does, what it protects and the tests behind it. Not run on any public test network or on mainnet; Electrum, Sparrow and hardware wallets are not supported |
| Wallets | Electrum and Sparrow support (a signing test showed Electrum needs a different key layout) | Not started |
| 2 | Physical orders: buyer-signed release, 14-day deadline, no-ship cancellation | Not started |
| 3 | Monero accounts in monthly escrow wallets | Not started |
| 4 | Public test networks end to end, then escrow-only for new orders, then removal of the pooled wallet | Not started |


## Phase 1: Bitcoin escrow on regtest

**Built and tested on regtest; not released; off by default.** The pooled wallet stays the default for every order
and the only payment path for anything but regtest. With `ESCROW_BTC=on` (refused unless the escrow node reports
chain `regtest`; [operator guide](operator-guide.md#bitcoin-escrow-regtest-only)) the development branch has:

- escrow key registration (one `[fingerprint/path]tpub…` per account, parsed in Go before any RPC; commas, private
  keys and recovery words refused) and activation by signing a practice transaction that can never be broadcast;
- escrow checkout for digital listings with automatic delivery: the refund address pinned and the market fee fixed
  at checkout, the escrow built and checked with Bitcoin Core and imported watch-only before its address is shown,
  each party's descriptor on the order page, and the order completed on funding;
- settlements built by the server as unsigned PSBTs from the escrow's confirmed, unspent coins with an explicit,
  capped fee rate (the vendor's claim to a fresh address; a refund to the pinned address when a funded order is
  cancelled or expires underpaid; an administrator's dispute decision; a stray refund of coins sent after the order
  closed), signed by the buyer or the vendor in their own wallet and pasted back; the market co-signs its own stored
  copy (or, when both parties signed, does not sign), re-checks, stores, broadcasts and follows the transaction for
  100 confirmations;
- recovery and reconciliation: the restore gate pauses builds, co-signing, broadcasts and escrow checkout, and a
  restored stored transaction is broadcast again only after an administrator confirms it; every checkout first asks
  the watch-only wallet whether it already holds the escrow address it composed (an escrow created after the backup a
  restored database came from) and then moves to the next child keys, up to 20 addresses (A-359; only the whole
  address is compared, so a child key of such an escrow can still be reused in another escrow address); spends the
  market did not make are recorded as spent outside the market, with the spending transaction's ID when the
  watch-only wallet names it, and flagged for staff (an order note and a notice to the parties, moderators and
  administrators), as follows. A settlement waiting for a signature, or signed by one party, whose input
  `gettxout` no longer finds (mempool included) is recorded at once, unless the watch-only wallet holds the
  settlement's own transaction, which the parties broadcast: that is checked and, if it passes, recorded as
  broadcast (A-344, A-356). A settlement being built that finds an input spent waits one pass, and is recorded only
  if the input is still spent at a second look on a later pass; spent no longer, it is left to build. A signed or
  broadcast settlement is recorded when its inputs are spent and the wallet does not know its transaction (or
  reports it conflicted), and a confirmed one when a conflicting transaction was mined in its place. An escrow deposit no settlement covers is checked only
  once it is in a block, only for an order whose deposits were read in that pass and whose settlement is not being
  built (the build checks it then), and is never counted or spent afterwards. Watch-only addresses with no escrow
  row are listed for administrators on the escrow watcher's daily check.

**What Phase 1 protects, on regtest.** The market holds one of each escrow's three keys, in an arbiter wallet used
for nothing else, and only public descriptors in its watch-only wallet: no settlement moves without the buyer's or
the vendor's signature. The market signs only the transaction it built and stored, only with `SIGHASH_ALL`, only from
its watcher and only once the order has its outcome; a pasted PSBT for another transaction, carrying an entry under
the market's key, signed with another sighash type, already finalized or signed by another key is refused before
anything is stored, and the pasted PSBT is never kept. The database refuses a second outcome per order and any change
to a settlement's transaction once a party has signed it, and a deposit is claimed by one settlement only
(compare-and-set); only the stored final
transaction is ever broadcast, so a restart mid-send sends the same transaction again. A dispute decision is fixed
once its settlement has a final transaction. All of this is subject to the limits under *What it will not protect*
above: a server taken over can show a substituted key, and the market key with one party's key can spend.

**What Phase 1 does not do.** It runs on regtest only and has not run on a public test network or mainnet. It covers
digital orders with automatic delivery only (physical orders are Phase 2, Monero Phase 3). It supports Bitcoin Core
signing only. When a settlement is recorded as spent outside the market, deposits of that escrow that are still
unspent stay there: the market builds nothing more for them, staff are told, and the buyer and the vendor can move
them together from their own wallets. A reorganisation deeper than 100 blocks is not followed.

**How it was tested.** Each tier proves only what it says:

- Go tests with PostgreSQL against a fake escrow node (simulated RPC, not chain evidence) for every state change and
  refusal, for example `TestEscrowCheckoutCreatesEscrow`, `TestEscrowPracticeActivates`, `TestEscrowFundingCompletesOrder`,
  `TestEscrowClaimBuildAndSign`, `TestEscrowPasteRefusals`, `TestEscrowRefundRoutes`, `TestEscrowSettlementRaces`,
  `TestEscrowCosignBroadcastConfirm`, `TestEscrowRawStoredBeforeBroadcast`, `TestEscrowBadSignatureReverts`,
  `TestEscrowBothPartiesSign`, `TestEscrowDisputeDecision`, `TestEscrowStrayRefundAfterClose`,
  `TestEscrowRestoreGatePausesAndAdminRebroadcasts`, `TestEscrowCheckoutSkipsAddressTheWalletWatches`,
  `TestEscrowReorgRetracking`, `TestEscrowExternalSpend`,
  `TestEscrowOutsideSpendFlagged` and, for A-356, `TestEscrowAwaitingSignatureSpentOutside`,
  `TestEscrowAwaitingSignatureOwnTransactionSeen`, `TestEscrowBuildingSpentOutside` and `TestEscrowMigration078WaitingSpent`.
- JSON-RPC adapter tests against an `httptest` server: exact parameters, `"ALL"` pinned, the `rpcwhitelist` methods
  (`TestEscrowRPCPinsParams`, `TestEscrowRPCReconciliation`, `TestEscrowWatchKnowsReply`).
- Real Bitcoin Core 31.1 on regtest, run by hand, not in CI (`scripts/test-escrow-regtest.sh`; the app runs inside
  the Go test against the real node, and every party signature is made by that party's own Core wallet): practice
  signing with refused `SIGHASH_NONE` and forged pastes (`TestEscrowRegtestPracticeSign`); escrow creation and funding
  (`TestEscrowRegtestFunding`); building and party signing (`TestEscrowRegtestSettlement`); the claim at a market key
  index past the arbiter wallet's initial range, a refund sent again unchanged after a restart, a both-signed
  settlement, discarded bad signatures, a stray refund and an administrator's dispute refund, each broadcast and
  confirmed (`TestEscrowRegtestCosign`); the parties broadcasting a both-signed settlement themselves, a reorganisation
  and its reversal, a reorganisation with a double-spend, a co-spend with no settlement and an orphan address
  (`TestEscrowRegtestRecovery`); and a comma-joined key refused, a forged market-key entry refused, A-356 (the
  parties co-spending an escrow while its claim waits for a signature, and before its claim is built), a
  spent-outpoint build refused, a conflicting outcome refused, the restore gate with a restore's hold (written by the
  test as `scripts/restore.sh` writes it) and one administrator-confirmed rebroadcast, a restored database not
  handing out an escrow address again (A-359; the database restore simulated in SQL, the watch-only wallet the real
  one), two escrows funded by one
  transaction never mixed, and the fee cap
  (`TestEscrowRegtestGate`). The restore gate was also checked once by hand on the real server binary.
- A browser journey in Chromium at 320 px with JavaScript off, every form by keyboard, against the real server and
  PostgreSQL and a **simulated** escrow node whose signed PSBTs come from the simulator, not from a wallet
  (`tests/e2e/escrow-journey.spec.ts`, `npm run test:e2e:escrow`; [development](development.md)): key registration
  and practice signing, checkout, funding, completion on funding, the vendor's claim with a downloaded copy, a refused
  paste, co-signing, broadcast and confirmation, and a refund. Not chain evidence.

Not tested: any public test network, mainnet, the escrow pages in Firefox or WebKit (the escrow journey runs in
Chromium only; CI runs the preview pages in Firefox and WebKit), Electrum, Sparrow and hardware wallets.

All escrow testing uses regtest and public test networks only. Mainnet stays an operator opt-in and is never
used in development or tests.
