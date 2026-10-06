# Per-order escrow

> [!WARNING]
> **No release contains escrow, and the pooled wallet stays the default.** Every deposit sits in the
> operator's pooled custodial wallet for its currency ([operator guide](operator-guide.md#payments),
> [implementation status](implementation-status.md#p5-payments)) unless an operator switches on the escrow
> phases built so far, which run **on regtest only** (Bitcoin) or **on a local Monero fakechain only** (a local
> test chain whose coins have no value) and are **off by default** (`ESCROW_BTC`,
> [operator guide](operator-guide.md#bitcoin-escrow-regtest-only); `ESCROW_XMR`,
> [operator guide](operator-guide.md#monero-escrow-local-fakechain-only)). Those phases, Bitcoin escrow for digital
> orders and for physical orders, and Monero escrow for both, are built and tested on the development
> branch ([Phase 1](#phase-1-bitcoin-escrow-on-regtest), [Phase 2](#phase-2-bitcoin-escrow-for-physical-orders-on-regtest),
> [Phase 3](#phase-3-monero-escrow-on-a-local-fakechain)); Monero escrow is **custodial**: the market can move its
> coins. They have not run on any public test network or on mainnet, and they are not verified for real payments. This
> page describes the escrow design the project owner and its design review have settled, what it will and will
> not protect, and how it ships. With each phase, this page, the [README](../README.md), the
> [implementation status](implementation-status.md), the [operator guide](operator-guide.md), the
> [test-network runbook](testnet-runbook.md), the [acceptance checks](acceptance.md) and
> [UPGRADING](../UPGRADING.md) are updated in the same change. Everything below [Phases](#phases) that no phase
> has built yet is a plan.

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
| Monero | One account per order inside a monthly escrow wallet on the server (still custodial, see [Monero](#monero-one-account-per-order)). Each monthly wallet is destroyed 90 days after its last order closes: the market makes it unopenable and wipes its sealed seed, and the operator deletes its files ([Phase 3](#phase-3-monero-escrow-on-a-local-fakechain)). 2-of-3 Monero multisig is evaluated later. |
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
| Vendor never ships | The buyer can cancel once the listing's dispatch window plus 48 hours (counted from payment) has passed, and is refunded (one party's signature, then the market) |
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
  `scripts/test-escrow-wallets.sh` repeats this check (real local chain plus a real Electrum, run by hand, not CI:
  Bitcoin Core 31.1 on regtest with no network, Electrum 4.8.2 checked against its release signature and used
  offline). A Bitcoin Core wallet signs the transaction the market serves today. An Electrum wallet restored from a
  BIP39 seed at a key path made for one order signs a per-order layout that the market does not offer, and the
  market's signature-paste check refuses that signature anyway, because Electrum adds an entry for every key,
  the market's included. The refusal protects nothing that is stored: only the signer's own signatures are ever
  kept, whatever else a pasted copy carries (`TestEscrowPasteForeignFieldsInert`; `TestEscrowWalletFixtures`
  checks the refusal on the signed files in `internal/market/testdata/escrow-wallets/`; those files come from the
  script's `--update-fixtures` run at `cf6a8a0` with uncommitted changes, logged in
  `artifacts/real-chain/a450-cf6a8a0fdbb4-dirty-fixtures-wallets.log`, and the clean evidence run at `52eb99f`
  (`artifacts/real-chain/a450-52eb99f7aba5-wallets.log`) used different throwaway keys, so its log does not contain
  them; both logs are local and gitignored). Electrum's own seed type, what "Create a new seed" makes and the
  default in Tails, cannot follow a per-order key path at all: Electrum fixes its multisig keys at `m/1'` and
  takes no path for that seed type (`from_seed` in Electrum's `keystore.py`; the script's `electrum-seed-type`
  leg records it).

## Monero: one account per order

Each order gets its own account inside a monthly escrow wallet on the server, which runs on its own
wallet service. The buyer pays into that account, and it is settled in one transaction to the recipient (plus
the market-fee output on a fulfilled order), with the network fee taken from the amount. Every payment out
names the order's account, so one order's coins can never pay another's settlement. Monero requires 10
confirmations before received funds can be spent, so a settlement can be sent only after that (about 20
minutes on mainnet). [Phase 3](#phase-3-monero-escrow-on-a-local-fakechain) builds this on a local fakechain only.

A monthly wallet is destroyed 90 days after its last order closes. Monero sent to an order's address within
that time is refunded to the buyer automatically; after it, the order page says it cannot be recovered.

**This is still custodial.** The server holds every escrow wallet's keys, so the operator, or anyone who
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
| Wallet check | Scripted signing test with Electrum and Bitcoin Core on regtest, before any escrow database change | Done on regtest: Core signs; Electrum cannot sign this layout. Repeatable with `scripts/test-escrow-wallets.sh` (real local chain plus a real Electrum, run by hand, not CI); Electrum's own seed type cannot follow a per-order key path |
| 1 | Bitcoin escrow for digital orders on regtest, behind a setting that is off by default | Built and tested on regtest on the development branch, not released, behind `ESCROW_BTC` (off by default, regtest only): see [Phase 1](#phase-1-bitcoin-escrow-on-regtest) for what it does, what it protects and the tests behind it. Not run on any public test network or on mainnet; Electrum, Sparrow and hardware wallets are not supported |
| Wallets | Electrum and Sparrow support (a signing test showed Electrum needs a different key layout) | Not started |
| 2 | Physical orders: buyer-signed release, 14-day deadline, no-ship cancellation, fee bumps | Built and tested on regtest; not released; off by default (`ESCROW_BTC`, regtest only): see [Phase 2](#phase-2-bitcoin-escrow-for-physical-orders-on-regtest) for what it does, what it protects, what it does not do and the tests behind it. Not run on any public test network or on mainnet; only Bitcoin Core signing is supported |
| 3 | Monero accounts in monthly escrow wallets | Built and tested on a local Monero fakechain (monerod `--regtest`); not released; off by default (`ESCROW_XMR`, fakechain only): see [Phase 3](#phase-3-monero-escrow-on-a-local-fakechain) for what it does, what it protects, what it does not protect and the tests behind it. Custodial: the market can move these coins. Not run on stagenet, a public test network or mainnet; no Monero multisig |
| 4 | Public test networks end to end, then escrow-only for new orders, then removal of the pooled wallet | Groundwork only: the node sync guard, the escrow chain pin and the chain table (A-439, [below](#node-sync-guard-chain-pin-and-chain-table-a-439)) are on the development branch; escrow still accepts regtest only and nothing has run on a public test network |


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
  100 confirmations. A deposit that does not count when the payment window ends (Core reports it conflicted, or no
  longer lists it) does not keep the order open, so an order with nothing else is cancelled unpaid and no refund is
  requested; if that deposit confirms later and no refund spends it, it is left out, flagged, noted on the order with
  the buyer and staff told, and returned to the buyer's refund address by a stray refund (A-428). A refund that
  cannot be built because the escrow's coins are too few, on a cancelled order (A-435) or one whose dispute was decided
  for the buyer (A-437), leaves those coins out for a stray refund (left out, flagged, noted on the order with the
  buyer and staff told); they are held, as a too-small stray refund's coins are (below), also when a stray refund of
  other coins was requested and built before the refund failed, and returned together with any deposit left out later,
  such as one that did not count at the cancellation or the decision and confirms since, once together they can pay
  the network fee. A release that fails so (a completed order, or one decided for the vendor) leaves its coins counted:
  they are the vendor's. A stray refund still being built when the refund fails (both
  in the same pass) adds those coins to its own: once it is built and spends them, the refund's failure holds nothing
  more (a coin of the refund's it leaves out, beyond the 50 largest or worth less than its own spending cost, gets one
  more try and is then held); if it fails too-small, the same coins fail a second time, at the grown value, and are then held. A stray
  refund that fails because its coins are too few is noted and told once, and the order page says the coins are held and why; no other is requested
  until a further deposit makes the coins worth more or a lower `ESCROW_BTC_FEE_RATE` makes them cheaper to spend,
  and the buyer's recovery request always gets one more build, whose failure is the answer (A-434). A stray refund
  leaves out of its build a deposit worth no more than its own spending cost at the current fee rate (105 vB times
  the rate) while another remains; that deposit stays for a later stray refund (A-436). A cancelled order's status
  follows its stray refund: held, awaiting signatures, broadcast or confirmed (A-435); on an order decided for the
  buyer whose refund failed so, the next step says the refund could not be built and a stray refund returns the coins (A-437);
- recovery and reconciliation: the restore gate pauses builds, co-signing, broadcasts and escrow checkout, and a
  restored stored transaction is broadcast again only after an administrator confirms it; every checkout first asks
  the watch-only wallet whether it already holds the escrow address it composed (an escrow created after the backup a
  restored database came from) and then moves to the next child keys, up to 20 addresses (A-359; since Phase 2 slice
  P2-e a child key of such an escrow is not reused in another escrow address either, A-366, as the
  [runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) describes); spends the
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
  `TestEscrowConflictedDepositThroughExpiry`, `TestEscrowUncountedDepositBounds`, `TestEscrowStrayRefundTooSmall`,
  `TestEscrowTooSmallRefundReturned`, `TestEscrowStrayStatusFollowsState`, `TestEscrowStrayRefundSkipsDust`,
  `TestEscrowRestoreGatePausesAndAdminRebroadcasts`, `TestEscrowCheckoutSkipsAddressTheWalletWatches`,
  `TestEscrowReorgRetracking`, `TestEscrowExternalSpend`,
  `TestEscrowOutsideSpendFlagged` and, for A-356, `TestEscrowAwaitingSignatureSpentOutside`,
  `TestEscrowAwaitingSignatureOwnTransactionSeen`, `TestEscrowBuildingSpentOutside` and `TestEscrowMigration078WaitingSpent`.
- JSON-RPC adapter tests against an `httptest` server: exact parameters, `"ALL"` pinned, the `rpcwhitelist` methods
  (`TestEscrowRPCPinsParams`, `TestEscrowRPCReconciliation`, `TestEscrowWatchKnowsReply`).
- Real Bitcoin Core 31.1 on regtest, run by hand and in CI by the `escrow-regtest` job, [CI and release delivery](ci-cd.md) (`scripts/test-escrow-regtest.sh`; the app runs inside
  the Go test against the real node, and every party signature is made by that party's own Core wallet): practice
  signing with refused `SIGHASH_NONE` and forged pastes (`TestEscrowRegtestPracticeSign`); escrow creation and funding
  (`TestEscrowRegtestFunding`); building and party signing (`TestEscrowRegtestSettlement`); the claim at a market key
  index past the arbiter wallet's initial range, a refund sent again unchanged after a restart, a both-signed
  settlement, discarded bad signatures, a stray refund and an administrator's dispute refund, each broadcast and
  confirmed (`TestEscrowRegtestCosign`); the parties broadcasting a both-signed settlement themselves, a reorganisation
  and its reversal, a reorganisation with a double-spend, a co-spend with no settlement and an orphan address
  (`TestEscrowRegtestRecovery`); a deposit replaced by a conflicting transaction that was mined (Core's
  `gettransaction` reports it with -1 confirmations and `listreceivedbyaddress` no longer lists it), the order
  cancelled unpaid at the end of its window, then the replacement's block invalidated and the deposit mined after all
  and returned by a stray refund the buyer's wallet signed, broadcast and confirmed (`TestEscrowRegtestConflicted`,
  A-428); and a comma-joined key refused, a forged market-key entry refused, A-356 (the
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

## Phase 2: Bitcoin escrow for physical orders on regtest

**Built and tested on regtest; not released; off by default.** Phase 2 adds to Phase 1 and is switched on and off
by the same setting. The pooled wallet stays the default for every order. With `ESCROW_BTC=on` (refused unless the
escrow node reports chain `regtest`; [operator guide](operator-guide.md#bitcoin-escrow-regtest-only)) the
development branch has, for physical listings:

- **Checkout.** A Bitcoin draft on a physical listing that has a dispatch window (1 to 30 whole days, 3 by default,
  set by the vendor on the listing form while escrow is on) can pay into an escrow once both parties' escrow keys
  are active, as for a digital listing. The vendor must also have a PGP key the buyer can encrypt to, checked before
  any node call (A-83); the offer gives the reason when it cannot be used. The shipping address flow is unchanged:
  the buyer encrypts it to the vendor's PGP key and the server never stores it. Checkout snapshots the release
  deadline length and the listing's dispatch window in the escrow's row; a later change to the setting or the
  listing never reaches it (a database trigger freezes them, migration 079).
- **The release, built at "shipped".** The vendor marks the paid order shipped on the order page with a fresh claim
  address from their own wallet and their password (and authenticator code when enrolled). In one transaction the
  order moves from paid to shipped, the release to the vendor is requested and the release deadline is fixed. The
  watcher builds the release at once, long before the order has an outcome, from the escrow's confirmed deposits,
  with the same fee rules as any settlement. The pooled "Mark as shipped" form and the buyer's pooled "Confirm receipt and complete" are
  refused on an escrow order.
- **The buyer signs it.** The buyer and the vendor each sign the release on the order page, in their own wallet, as
  for any settlement; the page lists the expected outputs and the network fee. **Signing it is the buyer's
  confirmation that the goods arrived, and a buyer who has signed can no longer open a dispute** (409). With both
  signatures the watcher checks the transaction, stores it as completed by the parties, moves the order from
  shipped to completed in the same database transaction and broadcasts it. The market does not sign it, and the
  arbiter wallet is not asked to. If the two signatures do not verify together, both are discarded and both
  parties sign again: the market never falls back to signing alone before the outcome, and the database refuses a
  market-signed final transaction unless the order's state fits the settlement (migration 081).
- **The deadline release.** If the buyer has not signed and no dispute is open when the deadline passes, the order
  completes by the system ("auto-release deadline passed") and the market co-signs the signature already stored for
  the release (the vendor's, or the buyer's if only the buyer signed) and broadcasts it; when both signed, their
  signatures complete it, and when nobody signed, the release waits for the vendor's signature. The deadline is the
  time the order was marked shipped plus the release deadline length: 14 days by default, any whole number of days
  from 7 to 30 set on `/admin` by an administrator with their password, audited, and snapshotted per escrow at
  checkout, so a change affects only later orders. The release at the deadline makes no node call itself and is
  skipped while the restore gate is set.
- **Reminders and the banner.** Every deadline is shown in UTC to the minute: in the shipped, reminder, release and
  dispute notices, on the order page (with the time left, computed when the page loaded) and in a banner on
  `/orders` for any shipped escrow order of the viewer with less than 72 hours left, unless its buyer has signed. 72 hours before the deadline,
  once, the buyer is reminded, unless the buyer has signed or a dispute is open, and the vendor is told the buyer's
  deadline. Notices are inside the market only; nothing is sent by email or any other channel.
- **The no-ship cancel.** A paid physical escrow order has a ship-by time: the time its payment was confirmed plus
  the escrow's dispatch window plus 48 hours. From then on, if the vendor has not marked it shipped, the buyer may
  cancel it and the escrow is refunded to the refund address fixed at checkout, by a refund settlement the buyer or
  the vendor signs and the market co-signs. Before it, the cancel is refused (409) with the date. The vendor's
  "Mark as shipped" and the buyer's cancel lock the order and compare its state, so exactly one takes effect, and the
  other is told which one did and when. A digital escrow order offers its buyer no such cancel.
- **Late and early disputes.** A dispute opened after the deadline is refused (409): the release is due. One opened
  before it stops the clock for good, because a disputed order only moves to resolved; the other party is told the
  deadline no longer applies, and an administrator decides the order. A deadline that passed in restored records
  and is waiting for the restore recheck below does not yet count as passed.
- **A dispute decided.** For the buyer, the release built at shipment is superseded while it has no final
  transaction, its deposits are freed and the refund is requested; if the release is already final (signed by both
  parties, or broadcast or spent on the network) the decision for the buyer is refused (409) and the administrator
  decides for the vendor. For the vendor, the release built at shipment is the settlement (no new claim address).
- **Top-ups.** A deposit that arrives after the release or refund has been built is left out of it, marked, noted
  on the order and announced to the buyer and to staff (not the vendor), and returned to the buyer's refund address
  by a stray refund once the order has closed. A top-up that arrives before the release exists is spent by it.
- **Fee bumps.** A settlement that was broadcast but not mined can be replaced by a transaction (BIP 125) that pays
  the same outputs, in the same order, with only the claimant's output smaller. The claimant (the vendor for a
  release, the buyer for a refund or stray refund) asks on the order page with a higher fee rate and their
  password; the watcher builds it; the claimant signs it first; then the market co-signs it, or the other party
  signs it too and the market does not. The other party's signature alone is refused, and the market never
  completes a bump with it. The highest rate offered keeps the fee within the caps at the smallest size the
  transaction can have: 71 sat/vB for a release of one deposit with two outputs (the form shows the exact maximum
  for each settlement). Two transactions of the market's own for one settlement are not a spend outside the market,
  and only the highest-fee one is sent again (migrations 080 and 082). While the restore gate is set no bump is
  built, co-signed or sent.
- **After a restore from backup.** `scripts/restore.sh` asks for a recheck of release deadlines: on the watcher's next
  pass, gate or not, every deadline of a shipped order with no dispute that had already passed in the restored
  records moves to 72 hours later, the buyer is told, and the moved deadline counts as the reminder. Nothing else
  moves a deadline. The restore also holds restored fee bumps that have a stored transaction, as it holds
  settlements.
- **No escrow key is handed out twice after a restore (A-366, migration 083).** The restore also makes escrow
  checkout wait (503) until the escrow watcher has read, with `getaddressinfo` on the watch-only wallet, the three
  public keys of every address that wallet holds with no escrow row, and reserved them; a checkout skips any child
  key that an escrow or a reservation holds. A restored backup of the watch-only wallet knows no lost escrow
  either, so the restore also moves each account's next child index, and the market's key index, 50 past the
  restored records. A party's own Core wallet signs only keys inside its descriptor range, so `/escrow-key` advises
  `keypoolrefill` once that party's next child index passes 900, and the order page does the same for the party's
  child key in that escrow (A-397).

**What Phase 2 protects, on regtest.** Before the order has its outcome, the market key signs nothing for a
physical order: the release needs the vendor's and the buyer's signatures, the market's stored copy is not signed
before the order is completed, and the database refuses a market-signed final transaction when the order's state does
not fit. A buyer's funds therefore do not reach the vendor unless the buyer signs, or the deadline passes with no
dispute open, or an administrator decides for the vendor. A buyer's dispute stops the deadline for good, and a
decision for the buyer cannot be overtaken by a release that is already final, because that decision is refused. The
deadline length is snapshotted at checkout, the ship-by time when the order is paid and the release deadline when
it is marked shipped; all are frozen in the database, and only the restore recheck moves a deadline, later, never
earlier. A fee bump never changes who is paid. All of this is subject to the limits under *What it will not
protect* above: a server taken over can show a substituted key, and the market key with one party's key can spend.

**What Phase 2 does not do.**

- It runs on regtest only and has not run on a public test network or on mainnet. It supports Bitcoin Core signing
  only. Monero is Phase 3.
- **A buyer who neither signs nor disputes before the deadline loses the choice.** The window is 14 days by default
  (7 to 30 as the administrator sets it), and reminders reach the buyer only inside the market. A buyer who has
  signed cannot dispute, because the signature confirms receipt. Both follow from the owner's design (ES-11).
- It does not check that the goods exist or arrived. An administrator decides disputes from what the parties write.
- The release at the deadline needs a stored signature: if nobody signed, the order completes at the deadline and
  the release waits for the vendor's signature, and nothing is sent until the vendor signs.
- A fee bump changes no output but the claimant's. It cannot be asked for by the other party, and only a settlement
  that has been broadcast and not mined can have one.
- **A high `ESCROW_BTC_FEE_RATE` can leave a settlement stuck (A-388).** The network fee is the rate times an upper
  bound of the transaction's size, while completing the settlement checks the fee at the real size against 100
  sat/vB. Near 100 sat/vB (from about 85 for a one-deposit settlement with three outputs, about 92 with one output) a settlement can be built, signed and then
  refused when the market completes it ("not sent yet: final-fee" in the log). A transaction a party has signed
  cannot be changed, so the buyer and the vendor could then only move the escrow together from their own wallets.
  Keep the rate well below 100; the default is 2. Fee bumps do not have this gap: the form stops at the rate that is
  safe at the real size.
- **Restore limits (A-394).** An escrow created after the backup, whose address the watch-only wallet holds, is
  listed as an orphan and its keys reserved. If `getaddressinfo` cannot read an orphan's keys, escrow checkout stays
  paused (503) until the node answers the sweep or an operator deals with it
  ([runbook](testnet-runbook.md#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused)). A dump taken
  before migration 083 gets the reservation but no index margin. A party who lost 150 or more consecutive escrows,
  or a market that lost 150 or more escrows in all (the market's key), gets 409 instead of a reused key: the margin
  skips 50 and a checkout gives up when it meets 100 held keys (100 without the margin). The market-key case clears
  on a later try; the party case does not.
  **When the watch-only wallet is restored from the same backup, an account that registered its escrow key after the
  backup is unknown to the restored database** (A-398): it has no index floor, and if it registers the same extended
  key again, its next child index starts at `/0/1` and reuses keys its lost escrows used. The market cannot detect
  this; it is inherent to restoring records that never saw the key. A party who registers again after a restore
  should use a fresh wallet account. A restore not made with `scripts/restore.sh` or the manual SQL in
  [UPGRADING.md](../UPGRADING.md#6-backups-now-need-the-wallets-too) sets none of this.

**How it was tested.** Each tier proves only what it says:

- Go tests with PostgreSQL against a fake escrow node (simulated RPC, not chain evidence). Checkout and settings:
  `TestEscrowPhysicalCheckout`, `TestEscrowCheckoutVendorKeyUnusable`, `TestEscrowReleaseDaysSetting`,
  `TestEscrowReleaseDaysConcurrentFirstSaves`, `TestEscrowMigration079Frozen`, `TestListingDispatchDaysField`.
  The release: `TestEscrowShipNeedsClaimAddress`, `TestEscrowPooledCompleteRefusedOnEscrow`, `TestEscrowPhysicalBothSign`,
  `TestEscrowShippedNeverMarketSigns`, `TestEscrowSignDisputeSerialised`, `TestEscrowShippedReleaseBroadcastByParties`,
  `TestEscrowMigration081Guard`. Disputes and top-ups: `TestEscrowDisputeSupersedesRelease`,
  `TestEscrowDisputeVendorReusesRelease`, `TestEscrowTopUpWhileShipped`. The deadline and the cancel:
  `TestEscrowDeadlineRelease`, `TestEscrowDeadlineDisputeRace`, `TestEscrowDeadlineGatePaused`, `TestEscrowReminderOnce`,
  `TestEscrowLateDisputeRefused`, `TestEscrowRestoreExtendsDeadline`, `TestEscrowReleaseAtWriters`,
  `TestEscrowOrdersBannerUnder72h`, `TestEscrowDeadlineBothSigned`, `TestEscrowDeadlineNextStepBothSigned`,
  `TestEscrowNoShipCancel`, `TestEscrowShipCancelRace`. Fee bumps: `TestEscrowBumpClaimantOnly`,
  `TestEscrowBumpConfirmsNotExternal`, `TestEscrowBumpShapeRefusals`, `TestEscrowBumpRateCapAtRealSize`,
  `TestEscrowBumpSeenOnTheNetwork`, `TestEscrowBumpRestoreHold`, `TestEscrowMigration080Bumps`,
  `TestEscrowMigration082Rules`. Restore keys: `TestEscrowMigration083`, `TestEscrowRestoreSkipsReservedKeys`,
  `TestEscrowRestoreIndexFloors`, `TestEscrowCheckoutWaitsForSweep`.
- JSON-RPC adapter test against an `httptest` server: the orphan sweep's reading of `getaddressinfo`
  (`TestEscrowWatchKeysReply`).
- Real Bitcoin Core 31.1 on regtest, run by hand and in CI by the `escrow-regtest` job, [CI and release delivery](ci-cd.md) (`scripts/test-escrow-regtest.sh`; the app runs inside
  the Go test against the real node, reaching it only as the `rpcwhitelist` user, and every party signature is made by
  that party's own Core wallet): `TestEscrowRegtestPhysical` (the release built at shipment, signed by both
  wallets, completed and broadcast with no arbiter signing call, then a release the parties broadcast
  themselves); `TestEscrowRegtestDeadline` (no arbiter signing call before the deadline and one after, the order
  completed, broadcast and confirmed); `TestEscrowRegtestBump` (a replacement at a higher rate mined with only the
  vendor's output smaller, a 100 sat/vB request refused and the form's highest rate broadcast and confirmed);
  `TestEscrowRegtestReserve` (the sweep reading the keys of lost escrows from the real watch-only wallet, and the
  next escrows sharing no key with them, with the database restore simulated in SQL).
- A browser journey in Chromium at 320 px with JavaScript off, every form by keyboard, against the real server and
  PostgreSQL and a **simulated** escrow node whose signed PSBTs come from the simulator, not from a wallet
  (`tests/e2e/escrow-physical-journey.spec.ts`, `npm run test:e2e:escrow`; [development](development.md)): the
  physical order shipped with a claim address and both signatures completing it; the deadline release; a dispute
  cancelling the deadline; the no-ship cancel refused before the window and accepted after; the race between
  the cancel and the shipment, in both orders; a buyer who never signs seeing the notices and the banner; and the administrator's 7
  days applying to a new order only. Not chain evidence. The clock is moved through SQL on the test's own database.

Not tested: any public test network, mainnet, the escrow pages in Firefox or WebKit, Electrum, Sparrow and hardware
wallets, deadlines of 7 to 30 days in real time (every test moves the clock), and a restore of a real backup into a
running market with Phase 2 orders on a real chain. `scripts/test-backup-restore.sh` (run in CI) restores a real dump
through `scripts/restore.sh` with escrow settlement, fee bump and index floor rows and checks the deadline recheck,
the sweep marker, the floors and the market's key index; the Go tests run the margin block as written.

## Phase 3: Monero escrow on a local fakechain

**Built and tested on a local fakechain; not released; off by default; custodial.** Phase 3 adds Monero to the escrow
built in Phases 1 and 2 and is switched on and off by its own setting, `ESCROW_XMR`. The pooled wallet stays the
default for every order. A *fakechain* is `monerod --regtest`, Monero's regtest and the counterpart of Phase 1's
Bitcoin regtest: a local chain whose coins have no value. Its wallets use the mainnet address format, so on it a refund
or claim address starts with 4 or 8. With `ESCROW_XMR=on` (refused when the market is configured for mainnet, and unless
the escrow daemon reports nettype `fakechain`, a hard fork version of at least 16 and a height below 1,000,000, and the
escrow wallet-rpc is within 10 blocks of that daemon; [operator guide](operator-guide.md#monero-escrow-local-fakechain-only))
the development branch has, for digital listings with automatic delivery and for physical listings with a dispatch window:

**The custody sentence.** Every Monero escrow page, notice and document says the same thing: the coins are *held by
this market in an account used only for this order, inside a wallet the market opens each month; the market can move
these coins*. Unlike Bitcoin escrow there is no key of the buyer's or the vendor's and no signature to paste: the
market holds the wallet keys and builds, stores and sends every transaction itself.

**What it does.**

- **Monthly wallets, one account per order.** A watcher of its own, with its own lock and its own `monero-wallet-rpc`
  (one that holds only escrow wallets, never the pooled wallet's URL), creates a wallet named `escrow-YYYYMM-NN` for each
  month. Its password is derived from `MONERO_WALLET_SECRET` and its name, so the database holds no wallet password. Its
  25-word seed is encrypted to an OpenPGP public key whose private half the operator keeps offline (`ESCROW_XMR_SEED_KEY`,
  never the canary key) and stored with the wallet's primary address and restore height before the wallet may take an
  order. Next month's wallet is created from 7 days before its month starts, so that a daily backup holds its seal before it
  takes an order; it becomes active at the start of the month (UTC), and the old one becomes *closing*, with its free
  accounts retired. A first run, or a market that was down past a month's start, creates and activates that month's wallet
  at once, with no lead. The watcher keeps `ESCROW_XMR_SLOTS` (20 by default) free accounts ready on the active wallet.
  Account 0 of a wallet is never an escrow and is never spent from; coins that arrive there alert the administrators, once per episode.
- **The slot claim at checkout.** The buyer of an XMR draft on an eligible listing may pay into a Monero escrow instead
  of the pooled wallet, giving a refund address from their own wallet (a standard address or subaddress; integrated
  addresses, other networks, addresses whose keys are not valid, and any address this market already knows are refused;
  it is checked in Go, with no wallet call, and cannot be changed afterwards). Checkout makes no RPC: in one
  transaction it moves the order out of draft, claims a free account of the active wallet by compare-and-set and pins the
  refund address, the market fee in force (`ESCROW_XMR_FEE_BPS`) and, for a physical order, the release deadline length and
  dispatch window. With no free account, while the restore gate is set, after a restore until the sweep below has run, or
  while Monero escrow is unavailable, it answers 503 and the order stays a draft.
- **Funding at 10 or more confirmations.** The watcher reads each wallet's incoming transfers (`get_transfers` over all
  accounts). A deposit counts from 10 confirmations (or `PAYMENT_CONFIRMATIONS_XMR` when that is higher) with no unlock
  time and no double-spend seen; a locked deposit is flagged for staff and never counted. A digital order completes on
  funding with its content delivered; a physical order becomes paid. An order that received nothing in its payment window is
  cancelled; one that is underpaid (every deposit confirmed, together short of the price, and no top-up for a whole payment
  window) is cancelled too, which requests its refund.
- **Settlements: built, stored and relayed by the watcher, never rebuilt.** Only the watcher builds a settlement, and
  only once the order has its outcome (a release on a completed order or one decided for the vendor, a refund on a
  cancelled order or one decided for the buyer, a stray refund on any closed order). It checks that the open wallet's
  primary address is the one on record and that the order's account has the escrow address, freezes every output of that
  account the settlement does not claim, and asks for one transfer from that account (`account_index` is never 0 and never
  absent) with `do_not_relay`, taking the network fee from the claimant's output alone. It then checks the wallet's
  answer: exactly the claimed outputs' key images, amounts as planned, a fee at most `ESCROW_XMR_MAX_FEE` and the
  claimant's output at least 0.001 XMR; otherwise the transaction is discarded unrelayed, the settlement waits and staff
  are told. A checked transaction is **stored first** (its hash, metadata, blob, fee and key images) and only then relayed,
  with the stored metadata. After a restart or a lost answer the watcher looks the transaction up in the daemon and the
  wallet, and relays the same stored metadata again if neither has it: that spends the same key images, so it cannot pay
  twice. Nothing builds a replacement automatically. A relayed settlement is *confirmed* at the confirmation threshold and
  followed to 100 confirmations; one a reorganisation removes goes back to *relayed* and is relayed again. If another
  transaction spent its key images it is recorded *external* and staff are told, at once. Otherwise a refusal is
  relayed again, as the same stored metadata, on the next pass (`monero-wallet-rpc` gives the same answer when it lost its
  connection to the daemon), and only after 3 refusals in a row is the settlement *failed*; staff are then told, and an
  administrator may re-request it (below). A held settlement whose relay an administrator confirmed after a restore keeps
  that confirmation through those refusals, so it is relayed again while the restore gate is still set. Stored metadata
  the wallet cannot parse (-27, which only a damaged record explains) fails the settlement at once, flagged once, and is
  never relayed again. When building, the wallet's -37 (the coins are not unlocked yet) only waits, never flagged; -17
  (not enough money) waits once and is flagged if it persists. These are the codes `monero-wallet-rpc` 0.18.5.1 gave
  when recorded (`tests/e2e/fixtures/xmr_rpc_shapes_0.18.5.1.json`).
- **Refunds and strays.** A refund goes to the address pinned at checkout, with the network fee taken from the amount.
  A release pays the vendor's claim address the escrow less the market fee (fixed at checkout; a fee below 0.001 XMR is
  added to the vendor's output) and less any amount paid above the price, which goes back to the buyer's refund address
  (also added to the vendor's output when below 0.001 XMR). A settlement spends the deposits that count when it is built; a
  deposit still confirming then, or arriving after the build, is left out and returned to the buyer's refund address by a
  *stray refund* once the order has closed. So is a deposit that did not count when its order was cancelled (the wallet
  read it as a double spend, or no longer listed it) and has confirmed since, when no refund spends it: it is flagged
  and the buyer told. The order page lists a left-out deposit on its own line, not counted as received, and a cancelled
  order's status follows the stray refund that returns it: requested or built, sent, confirmed (A-435). A
  stray refund whose coins are too few for the network fee and an output of at least 0.001 XMR stays requested,
  staff told once (the status of a deposit that did not count then says the market cannot send it), and is built once
  a further deposit makes the coins enough. A refund too few so, on a cancelled order or one decided for the buyer,
  ends instead once no deposit it would spend is still confirming (failed, too-small): its deposits are left out and
  flagged, with one note and the parties and staff told, so the one stray refund returns them together with a later
  deposit, or one that did not count at the cancellation or the decision and has confirmed since, once together they
  pay the fee. Before, the refund waited for good while a later deposit's stray refund waited too or paid without its
  coins (A-437). A release too few still waits: the coins are the vendor's. After a close, deposits are refunded automatically until the wallet is
  destroyed; after that, the order page says Monero sent to its address since then cannot be recovered (ES-8).
- **Confirm receipt and deadlines.** For a digital order the vendor claims the escrow with a fresh Monero address and their
  password once the order has completed on funding. For a physical order the vendor's *Mark as shipped* takes the claim
  address and their password, requests the release to it and fixes the release deadline; nothing is built until the
  order is completed or its dispute is decided for the vendor. The buyer's **Confirm receipt**, with their password,
  completes the order, after which it can no longer be disputed. If the buyer does neither before the deadline (14 days
  by default; the administrator's length for Bitcoin escrow applies, see the limits) and no dispute is open, the order
  completes by the system and the release is built. The buyer is reminded 72 hours before, in the market only, and
  `/orders` shows a banner. The deadlines run in the Monero watcher, so they apply with `ESCROW_BTC` off, and even while the
  escrow wallet-rpc is down. The no-ship cancel of Phase 2 is shared: a buyer may cancel a paid physical order that was not
  shipped once the dispatch window plus 48 hours has passed, and the escrow is refunded.
- **Disputes: decisions are final.** An administrator decides a dispute on a Monero escrow order with their password; a
  moderator's resolve is refused. Unlike a Bitcoin escrow's, a Monero decision cannot be changed once made, because the
  market builds and sends it on its next pass; the resolve form says so before submission. A decision for the buyer
  supersedes a release that has no stored transaction yet and is refused (409) once the release has one.
- **Retention and destruction.** A wallet whose orders have all closed is *retained* and is still read on every pass, so
  a late deposit is refunded. 90 days after its last order closed, and only when no order of it is open, no settlement
  of it is live or still followed to 100 confirmations, its balance and unlocked balance are zero across all accounts
  and nothing of it is in the pool, the watcher replaces its password with 32 random bytes that are never stored, closes
  it, wipes its sealed seed and records it destroyed. Only those preconditions destroy a wallet automatically. The
  watcher records that it began destroying a wallet (migration 086) before it replaces the password, so a destruction
  interrupted after the new password took effect (a crash, or a restore from a backup taken in between) ends destroyed
  on a later pass, once the wallet-rpc refuses that wallet as "Invalid password", provided staff were never flagged that
  it holds something and neither the restore gate nor the sweep marker is set. A retained wallet past its 90 days that
  still holds coins (an unclaimed paid account, coins at account 0, a failed settlement) is not destroyed, and staff are
  flagged once. The market cannot reach the wallet volume: `/admin` lists each destroyed wallet's two files for the
  operator to delete.
- **Restore holds and the orphan sweep.** `scripts/restore.sh` (or the manual SQL in
  [UPGRADING.md](../UPGRADING.md#6-backups-now-need-the-wallets-too)) holds every restored Monero settlement that has a
  stored transaction until an administrator confirms its relay on `/admin`; retires every free account; moves every
  active or sealed wallet to *closing*, so no account of a restored wallet, one the restored records lack included, is
  offered to a new order; asks for a release deadline recheck (the deadline of a shipped order that had passed in the
  restored records moves 72 hours later, and the buyer is told); and sets `xmr_escrow_sweep_required`. Until the
  watcher's sweep has run, checkout answers 503 and no wallet or account is created. The sweep looks for wallets created
  after the backup: in each month from 12 months back through next month, the names after that month's newest row. One
  that opens with the derived password is sealed to the seed key as an *orphan*, kept, never offered an account, and its
  funded accounts are listed to the administrators; one that does not open is recorded as an orphan and never opened. It
  also gives a wallet older than the database a longer subaddress lookahead and rescans it, so deposits to accounts it
  does not know are seen. Under the gate nothing is built or relayed; deposits are still read.
- **If a wallet cannot be opened.** A wallet the escrow wallet-rpc refuses to open (its file gone, or a password that no
  longer opens it) is recorded *missing*, keeps its sealed seed, its free accounts are retired and the administrators
  are told; it is tried on every pass. "Invalid password" alone never wipes a seal: a wallet restored from its seed with
  a mistyped secret, another install's file of the same name and a damaged keys file are refused alike, so only a
  destruction the market itself began (above) ends destroyed without an administrator. A database restored from a
  backup taken after that start keeps it, so such a wallet restored from its seed with a mistyped secret is recorded
  destroyed too once the gate and the sweep marker clear: the operator restores it only with the password derived from
  the unchanged secret and checks in a scratch wallet restored from the seed that it holds nothing before deleting the
  files listed on `/admin`. A wallet another wallet program
  has open is temporarily unavailable: it keeps its state and its accounts, waits, and is logged once until it is closed
  there. A missing wallet past its 90 days is listed on `/admin`, where an administrator confirms its destruction
  (password step-up, audited) once the operator has checked that the file is gone and nothing is owed. A wallet that
  would not open while the sweep ran counts as checked, so after its file is restored the operator sets the sweep marker
  again for its lookahead. A changed `MONERO_WALLET_SECRET` disables Monero escrow with one fixed log line and no wallet
  is created or opened; a changed `ESCROW_XMR_SEED_KEY` stops wallet creation only, until the operator re-pins it.

**What Phase 3 protects, on a local fakechain.** Every transaction out of an order's account names that account, and the
transaction the wallet builds is checked to spend exactly the outputs claimed to exactly the planned destinations, so one
order's coins are not paid out in another's settlement. The database refuses a second outcome per order, a stored
transaction that changes, a transaction stored before the order's state fits the settlement (migration 081's rule, carried
into 084), and any deleted escrow record. A settlement is stored before it is relayed and is relayed again only as the
same stored transaction; it is never rebuilt by the market on its own, and the administrator's re-request is allowed only
when the stored transaction is in neither the daemon's pool nor the chain and none of its coins is spent, builds exactly
the stored coins to the stored plan, and supersedes the old row in the same database transaction, so at most one of the two
transactions can be mined. Seeds are sealed to a key the application only holds the public half of, and no wallet
password is kept anywhere. The refund address is fixed at checkout. An address whose keys are not in ed25519's prime-order
subgroup, which the wallet's own check accepts, is refused offline, because the wallet-rpc used here (0.18.5.1) crashed
when asked to build a transfer to one. After a restore, no restored
wallet takes an order again and no settlement is relayed until an administrator has checked it.

**What Phase 3 does not protect.**

- **It is custodial: the market can move these coins.** The server holds every escrow wallet's keys, opened with a
  password it derives itself. The operator, and anyone who takes over the server or copies the wallet volume and
  `MONERO_WALLET_SECRET`, can spend every escrow's coins. Accounts keep orders apart and remove the pooled balance; they do
  not stop the operator. There is no Monero multisig and none is claimed.
- **It is fakechain only.** It refuses a daemon that is not a local fakechain. It has not run on stagenet, testnet or
  mainnet, so nothing is known about its behaviour on a real network, with real fees, ring members or reorganisations.
- **Backups keep wallet copies until they expire.** A database dump holds each wallet's sealed seed (anyone with the
  OpenPGP private key can open it), and a backup of the escrow wallet volume holds its files. Destroying a wallet reaches
  neither: the dumps and volume copies taken before it stay as they are, and the operator decides how long they are kept.
- **Destroyed wallet files must be deleted by the operator.** The market cannot reach the wallet volume. It only makes a
  wallet unopenable (a password nobody keeps) and wipes its sealed seed from the database; the files stay on the volume
  until the operator deletes them.
- **A wallet created after the last backup can be lost (A-403).** If the database and the escrow wallet volume are both
  lost before the next backup, a wallet created in between is lost with the orders it took. Losing only one of them keeps
  the coins recoverable: the sealed seed is in the database and the files are in the volume (the restore sweep lists the
  funded accounts of a wallet the restored database lacks), although a restored database has lost the orders recorded after
  its backup, as with any restore. The 7-day lead means a wallet takes no order
  before a daily backup holds its seal, but only for a wallet created ahead of its month; a first run, or a wallet created
  after the market was down past a month's start, takes orders at once. This assumes a database backup at least daily
  ([operator guide](operator-guide.md#monero-escrow-local-fakechain-only)).
- **Monero dispute decisions are final.** A wrong decision cannot be undone from the market.
- **A buyer who neither confirms receipt nor disputes before the deadline loses the choice**, as for Bitcoin escrow
  (ES-11), and an administrator decides disputes from what the parties write; it does not check that goods exist or
  arrived. The release deadline length is one setting for both coins: its form on `/admin`, like a physical listing's
  dispatch window, is offered while `ESCROW_BTC` or `ESCROW_XMR` is on, and each escrow keeps the length in force at
  its checkout.
- **The market fee is set only by `ESCROW_XMR_FEE_BPS` and `ESCROW_XMR_FEE_ADDRESS`** (no form on `/admin`); each escrow
  keeps the fee in force at its checkout.
- **One wallet is open at a time, and every wallet that may hold coins is opened on every pass**, until the wallet is
  destroyed. PAYE-E2 measured this on one host, on a local fakechain only ([verification](verification.md#monero-escrow-wallet-scale-paye-e2-war-room-a-429-p3-r12--2026-10-04)):
  a pass over 5 wallets of 10,000 accounts each took 27.6 s against the 10-minute pass timeout, but a catch-up refresh
  over 110 blocks holding 200 deposits took 2.5 minutes, past the 2-minute bound it then had. Refresh now has its own
  8-minute bound, a refresh that times out is finished by the wallet-rpc and stored by the next `open_wallet`, and each
  new wallet looks one address ahead per account instead of 200: with that, the same catch-up took 1.0 s and
  `create_account` 7.5 ms instead of 390 ms (median). A month with far more orders, a slower host or a public network has
  not been measured.
- **A wallet-rpc that crashes building a settlement** (0.18.5.1 does on some destinations) is handled but not cured: a
  settlement whose build drops the connection twice is set aside, staff are told, and an administrator must ask for the
  build again; other settlements are not held up.
- **The orphan sweep is bounded.** It probes wallet names month by month, from 12 months before the current month
  through next month, starting after each month's newest wallet row, and stops each month at its first name with no
  file, so a wallet of an earlier month is not found; an orphan's restore height is unknown (recorded as 0), so
  restoring one from its seed scans the whole chain.

**How it was tested.** Each tier proves only what it says:

- Unit and `httptest` tests, no database. The adapter's exact parameters (account index never 0 or absent) and result
  shapes (`TestXMREscrowRPCPinsParams`, `TestXMREscrowRPCResults`, `TestXMREscrowRPCErrorClasses`,
  `TestXMREscrowAccountZeroRefusedOffline`, `TestXMREscrowNetworkCheck`); every configuration refusal
  (`TestXMREscrowConfigFromEnv`, `TestXMREscrowRefusedOnLiveMarket`, `TestXMREscrowFeeAddressKeysRefuseStartup`,
  `TestXMRWalletSecretRefusals`, `TestXMRSeedKeyRefusals`); the password derivation and the seed seal
  (`TestXMRWalletPasswordDerivation`, `TestXMRSealSeedRoundTrip`); the checks on a built transaction
  (`TestCheckXMRBuild`); and offline address parsing against Monero's own vectors, RFC 8032 keys and small-order points
  (`TestXMRBase58MoneroVectors`, `TestXMRAddressExternalVectors`, `TestXMRAddressRefusals`, `TestXMRAddressKeysRefused`,
  `TestXMRPointValid`, `TestXMRPointPrimeOrder`).
- Go tests with PostgreSQL against a **fake** escrow wallet-rpc and daemon (simulated RPC, not chain evidence), for
  every state change and refusal. Schema: `TestXMREscrowMigration084`, `TestXMREscrowMigration085`, `TestXMREscrowMigration086`,
  `TestEscrowBTCIgnoresXMRRows`, `TestXMREscrowPayoutRefused`, `TestXMREscrowIdxNoCollision`. Wallets, slots and funding:
  `TestXMREscrowFirstRun`, `TestXMREscrowWalletRotation`, `TestXMREscrowSlotClaimRace`, `TestXMREscrowNoRPCAtCheckout`,
  `TestXMREscrowAccountZeroNeverSpent`, `TestXMREscrowFundingThreshold`, `TestXMREscrowTwoAccountsOneTx`,
  `TestXMREscrowSecretMismatch`, `TestXMREscrowSeedKeyRepinRefused`, `TestXMREscrowTopUpAddressChecked`. Settlements:
  `TestXMREscrowStoredBeforeRelay`, `TestXMREscrowNeverRebuilt`, `TestXMREscrowAmbiguousRelay`,
  `TestXMREscrowFreezeExcludesTopUp`, `TestXMREscrowPrimaryAddressMismatch`, `TestXMREscrowKeyImagesExact`,
  `TestXMREscrowFeeCapAndFloor`, `TestXMREscrowReorgRelaysAgain`, `TestXMREscrowExternalSpend`,
  `TestXMREscrowReRequestGuards`, `TestXMREscrowReRequestWaitsForGate`, `TestXMREscrowRelayRefusalRetried`,
  `TestXMREscrowBuildSetAside`, `TestXMREscrowGatePauses`, `TestXMREscrowConflictedDepositThroughExpiry`,
  `TestXMREscrowStraysPerWallet`, `TestXMREscrowLeftOutDepositLine`, `TestXMREscrowStrayTooSmallOnce`. Releases, deadlines and
  disputes: `TestXMREscrowNoBuildBeforeOutcome`, `TestXMREscrowConfirmReceiptStepUp`, `TestXMREscrowShipStepUp`,
  `TestXMREscrowDeadlineWithBTCOff`, `TestXMREscrowDisputeSupersedes`, `TestXMREscrowNoShipCancel`,
  `TestXMREscrowRestoreExtendsDeadline`. Retention and restore: `TestXMREscrowDestroyPreconditions`,
  `TestXMREscrowRestoredPastDestroy`, `TestXMREscrowUnopenableKeepsSeal`, `TestXMREscrowConfirmDestroyed`,
  `TestXMREscrowRestoreRetiresSlots`, `TestXMREscrowRestoreNoUnrecordedSlot`,
  `TestXMREscrowOrphanSwept`, `TestXMREscrowLookaheadForOlderWallet`, `TestXMREscrowHeldSettlementConfirm`.
  `scripts/test-backup-restore.sh` (run in CI) restores a real dump through `scripts/restore.sh` with Monero escrow rows
  and checks the holds, the retired slots, the closed wallets and the sweep marker.
- Real `monerod` and `monero-wallet-rpc` 0.18.5.1 on a local fakechain, run by hand, not in CI
  (`scripts/test-escrow-xmr-regtest.sh`, [runbook](testnet-runbook.md#9-manual-monero-escrow-fakechain-check); the app
  runs inside the Go test against the real daemon and wallet-rpc, and the script refuses any daemon that is not a
  fakechain). Fourteen legs, each a Go test:
  1. `TestXMREscrowRegtestWallets`: the first pass creates the month's wallet, seals its seed (the seal decrypts, and a
     wallet restored from it at the stored restore height has the stored primary address), activates it and keeps its free
     accounts; account 0 holds nothing; the next month's wallet is created sealed ahead of its month and becomes active at
     its start.
  2. `TestXMREscrowRegtestFunding`: a deposit is recorded at 0 confirmations in the pool, counts at 10 and not at 9, and
     completes a digital order.
  3. `TestXMREscrowRegtestTwoAccountsOneTx`: one transaction paying two escrows becomes one payments row per account, each
     its own order's.
  4. `TestXMREscrowRegtestPhysical`: four physical orders funded by one transaction; the watcher makes no `transfer` or
     `relay_tx` before an outcome; then Confirm receipt, the deadline, a dispute for the buyer and a no-ship cancel each
     have their settlement relayed and confirmed to the right address.
  5. `TestXMREscrowRegtestRestartMidSend`: relay's answer is lost and the app and wallet-rpc restart; the same stored
     transaction is relayed, with one `transfer` call in all.
  6. `TestXMREscrowRegtestRelayDropped`: a lost relay answer; the next pass finds the transaction in the pool and records
     it relayed, with no second `relay_tx` or `transfer`.
  7. `TestXMREscrowRegtestTopUp`: a top-up still confirming at the build is left out and returned by a stray refund.
  8. `TestXMREscrowRegtestReorg`: a confirmed release is popped from the chain, goes back to relayed, is relayed again
     and confirms again.
  9. `TestXMREscrowRegtestExternal`: a wallet restored from the sealed seed sweeps the account outside the market while
     the release is built; the daemon refuses the market's transaction and it is recorded external and flagged.
  10. `TestXMREscrowRegtestReRequest`: monerod is stopped around each relay of a release, so the wallet-rpc cannot commit
      it (-4) while it is stored and its coins unspent; the same stored transaction is relayed again on the next passes
      with no second `transfer`, and the third refusal records it failed; the wallet still lists its outputs as
      available; an administrator's re-request spends exactly the stored key images (the daemon's decoded inputs) to the
      stored plan, and it confirms.
  11. `TestXMREscrowRegtestRestore`: a restore (with `scripts/restore.sh`'s own Monero SQL) holds a built release, retires
      the free accounts and closes the wallet; the sweep finds a wallet created after the backup and seals it as an
      orphan; after an administrator's confirmation the held release is relayed once and confirms; the next checkout gets
      an account of a new wallet.
  12. `TestXMREscrowRegtestLookahead`: a deposit to account 300, unknown to the wallet, is missed with the default
      lookahead and seen after the sweep raises it and rescans.
  13. `TestXMREscrowRegtestWrongSecret`: with a changed `MONERO_WALLET_SECRET`, no wallet is created or opened and checkout
      answers 503.
  14. `TestXMREscrowRegtestDestroy`: a deposit on day 89 to a closed order's escrow is refunded; on day 91 the retained
      wallet is destroyed and `open_wallet` with the derived password is refused.

  Every settlement the daemon holds is decoded (RingCT type 6, 16 ring members per input, a fee above zero, the stored one,
  within `ESCROW_XMR_MAX_FEE`). Each leg puts the watcher's clock in a month of its own, so the legs may run again against
  the same nodes; no leg waits in real time for a deadline or a month.
- A browser journey in Chromium at 320 px with JavaScript off against the real server and a **simulated** Monero escrow
  wallet-rpc and daemon is added in this phase (`tests/e2e/escrow-xmr-journey.spec.ts`), its last test covering a wallet
  destroyed after its retention (the order page's ES-8 line) and a missing wallet confirmed destroyed on `/admin`; its
  command and result are not recorded here. A simulation is not chain evidence.
- PAYE-E2 (`scripts/test-escrow-xmr-payee2.sh`, manual) on a real `monerod` and `monero-wallet-rpc` 0.18.5.1 local
  fakechain, one host: one wallet of 10,000 accounts with 200 funded, its calls timed through the app's adapter, a seed
  restore that finds every funded account, and a watcher pass over 5 such wallets within the pass timeout. The numbers
  and their limits are in [verification](verification.md#monero-escrow-wallet-scale-paye-e2-war-room-a-429-p3-r12--2026-10-04).

Not tested: stagenet, testnet and mainnet, Monero multisig, the escrow pages in Firefox or WebKit, deadlines of 7 to 30
days in real time, a wallet with thousands of accounts on a public network (PAYE-E2 measured one on a local fakechain), a restore of a real backup into a
running market with Phase 3 orders on a real chain (`scripts/test-backup-restore.sh` restores a real dump and checks its
rows; the fakechain `Restore` leg simulates the database restore in SQL), and a deployment of any of it.

## Node sync guard, chain pin and chain table (A-439)

Phase 4 groundwork (war-room ruling P4-R3 to P4-R5), on the development branch and not released. Escrow still accepts
regtest only (A-443 opens signet and testnet4); the guard also protects pooled Bitcoin payments now.

- **Sync guard.** Bitcoin Core's `initialblockdownload` turns false once and stays false, so a node that lost its peers
  reports itself synced while deposits sent since are invisible to it; a watcher trusting it would cancel a paid order
  "payment window expired". A node now counts as not synced while it is in initial block download, its headers are
  ahead of its blocks, or, on a public chain (anything but regtest), it has no peers (`getnetworkinfo`) or its tip block
  is older than `BITCOIN_MAX_TIP_AGE` (default 2h, 30m to 24h), or its `getblockchaininfo` has no tip `time` (reported
  as "tip time unreadable": the guard needs Bitcoin Core 31.1, the version the installer pins; earlier versions
  untested). While it is not synced the escrow watcher's Bitcoin
  pass reads no deposit, expires no order, flags nothing and builds, co-signs or broadcasts no settlement (the release
  deadlines, which need no node, still apply), and the pooled Bitcoin pass is skipped as it is during initial block
  download. The rule is stateless: nothing is stored, so a legitimately replaced chain never stalls it (A-147). One
  log line per change and a line under *Payment providers* on `/admin` name the reason
  ([operator guide](operator-guide.md#bitcoin-escrow-regtest-only)); on regtest the initial block download line adds
  the remedy (mine one block, or restart bitcoind with a larger `-maxtipage`), since a regtest node has no peers to
  sync from. Past its payment window, an open escrow order's page says the order stays open while the market waits for
  its node, instead of a cancellation time already gone. A lone regtest node is exempt from the peer and tip-age
  checks, and `getnetworkinfo` is called only on a public chain.
- **Chain pin.** The database pins the escrow node's chain (`settings` key `escrow_btc_chain`: the chain name, plus the
  `signet_challenge` on signet, since a private signet shares public signet's genesis block). A database never changes
  chain once it holds an escrow: a node on another chain leaves escrow disabled, changes nothing and is named on
  `/admin` with the pinned chain; a new chain needs a new database. While no escrow exists the pin follows the node; a
  database from before the pin that holds escrows is pinned only when the node reports regtest. Two regtest nodes
  cannot be told apart.
- **Chain table.** One table maps each escrow chain to its segwit address prefix (regtest `bcrt`; signet and testnet4
  `tb`; mainnet and testnet3 are not escrow chains), and every check of a settlement's output addresses reads it with
  the escrow node's chain. The signet and testnet4 entries are exercised by unit tests only.

How it was tested (simulated RPC and PostgreSQL, not chain evidence): `TestBitcoinNodeNotSynced`,
`TestBitcoinMaxTipAgeSetting`, `TestEscrowAddressTable` (unit); `TestEscrowWatcherSkipsWhileNotSynced` (each condition,
the public-chain ones with the escrow put on signet by hand, including a `getnetworkinfo` refused with HTTP 403 or
error -32601 and a node omitting the tip time: no cancel, no spent-outside flag, no build, no deposit read, nothing written,
the held order page, then the work on the next synced pass), `TestEscrowRegtestExemptFromPeerAndTipChecks`,
`TestEscrowChainPinMismatchDisables`, `TestEscrowChainPinRules`, `TestBitcoinPooledGuardPeersAndTipAge`,
`TestBitcoinPooledGuardRegtestExempt`, `TestBitcoinPooledRegtestIBDNamesRemedy` and
`TestBitcoinPooledNoPeersHoldsExpiry`. The real regtest legs of
`scripts/test-escrow-regtest.sh` were run again against Core 31.1 with `getnetworkinfo` on the whitelist. Not tested: a
real node that lost its peers or stalled on a public network.

## Per-order key derivation (A-453, shape (b) groundwork)

Groundwork for per-order wallets (ruling P4-R2, owner decision 2026-10-05), on the development branch with no product
caller: nothing in the market uses it yet, and escrows still take their child keys from Bitcoin Core. The Go code
(`internal/btc`, shared with the escrow signer since A-454, and `internal/market/escrow_bip32.go`; standard library
`math/big` on secp256k1, no new dependency) derives the unhardened
child `<i>` of a registered account key by BIP32 public derivation, refuses hardened indexes, moves to the next index
when BIP32 calls a child invalid and reports the index it used, and serializes the child rooted (depth 0, parent
fingerprint 0, child number 0) as a `tpub` or a SLIP-132 `Vpub`, so an order wallet holds no link to the account. A
helper asks Core's `deriveaddresses` (already on the escrow user's whitelist) for the address from the account key at
`<i>/0/0` and from the rooted key at `/0/0`, as `wpkh` for one key or the escrow's `wsh(sortedmulti(2,…))` for three,
and refuses any mismatch. How it was tested: unit (BIP32 test vectors 1–3 and 5, a forced invalid child, the rooted
round trip, `FuzzCKDPub`; the helper against a simulated node), and on a real local chain, Core 31.1 on regtest
(`TestEscrowRegtestBIP32` in `scripts/test-escrow-regtest.sh`: indexes 2, 3, 17, 1000 and 2^31-1 for three wallets'
BIP48 accounts agree with Core, and with Core's own derivation from each private root; a wrong index or a changed chain
code is refused). Not tested: any wallet other than Bitcoin Core, and any public network.

## Escrow signer (A-454, shape (b))

The second piece of per-order wallets (ruling P4-R17), on the development branch with no product caller: no order uses
it yet, and every escrow this release creates is still co-signed by the Core arbiter wallet. `cmd/escrow-signer` holds
the market's key for per-order escrows in a fresh seed of its own (a mode-0400 file in its own volume, never in `.env`,
the database or `scripts/backup.sh`'s output), listens only on a Unix socket the app's container mounts, and signs one
escrow's PSBT per request through Core's `descriptorprocesspsbt` under its own RPC user (whitelisted to `decodepsbt` and
`descriptorprocesspsbt`) with `ALL` hard-coded. It refuses, with nothing signed, any sighash but ALL, any input that is
not the same `2 <three sorted keys> 3 CHECKMULTISIG` P2WSH escrow with a matching `witness_utxo`, a script without its
key for the requested order index (`account/i/0/k` or `account/i/1/k`, k < 20) or with two of them, and any result from
Core that differs from the request by more than one signature per input under its key. Its account key is computed in
Go once at startup (Core's `getdescriptorinfo` does not give the public key at the end of a hardened private path);
to sign, Core gets the master key and path inside the descriptor and derives the key itself. The app pins the signer's
fingerprint and an HMAC of its account key at first contact; a different key makes shape (b) unavailable and changes
nothing else, and a signer that does not answer is "waiting". `escrow-signer backup` writes the seed encrypted to the
operator's OpenPGP key and `restore` reads it back; the [operator guide](operator-guide.md#escrow-signer) covers set-up,
the pins, backup, break-glass signing and the upgrade order. How it was tested: unit (BIP32 vectors through the private
path; every refusal, the result check and the happy path against a simulated Core; the socket protocol; init, backup and
restore), DB integration (the pins, a mismatch, a signer that is down, replies the app cannot use, against a simulated
signer), and on a real local chain, Core 31.1 on regtest (`TestEscrowRegtestSigner` in `scripts/test-escrow-regtest.sh`:
a per-order escrow co-signed through the socket, signed by the buyer's Core wallet, broadcast and mined; the four
refusals; backup, restore and an identical signature from the restored seed; a pin mismatch and a stopped signer; no
seed or tprv in the database, the signers' logs or the transcript). Not tested: the Compose service running (checked
with `docker compose config` only), break-glass signing on an offline node, any wallet other than Bitcoin Core, and any
public network.

All escrow testing uses regtest, a local Monero fakechain and public test networks only. Mainnet stays an operator opt-in
and is never used in development or tests.
