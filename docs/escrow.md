# Per-order escrow

> [!WARNING]
> **Escrow is on the development branch and is in no release.** Fresh installs use per-order escrow
> only; pooled payment code has been removed. Both currencies are **off by default**. Legacy databases
> must have no pooled drain blockers before migration 107 can commit, and their historical records stay
> readable ([upgrade boundary](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448)).
> Bitcoin escrow accepts **regtest, signet or testnet4 only**; Monero accepts **local fakechain or
> stagenet only**. Public signet and one public stagenet digital claim have recorded evidence;
> testnet4 and physical-device signing remain unverified. Sparrow 2.5.5 GUI per-order practice/release/sibling-refund
> signing passed on isolated regtest at frozen app revision `f229a1a65172986065bbab4f3aa24a09725695ad`; a separate
> GUI import of the supplied descriptor produced the funded escrow receive address in a saved watch-only wallet
> ([A-458 evidence](evidence/sparrow/2026-10-11-f229a1a65172/README.md)). This does not prove signing from that wallet.
> (`ESCROW_BTC`, [operator guide](operator-guide.md#bitcoin-escrow-test-networks-only); `ESCROW_XMR`,
> [operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet)). Those phases, Bitcoin escrow for digital
> orders and for physical orders, and Monero escrow for both, are built and tested on the development
> branch ([Phase 1](#phase-1-bitcoin-escrow-on-regtest), [Phase 2](#phase-2-bitcoin-escrow-for-physical-orders-on-regtest),
> [Phase 3](#phase-3-monero-escrow-on-a-local-fakechain)); Monero escrow is **custodial**: the market can move its
> coins. The [public signet smoke](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md) covers Core practice, digital claim and underpaid refund; mainnet remains refused by escrow, and these paths are not verified for real payments. This
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
- Wallet support depends on the key layout. Bitcoin Core signed the original layout on regtest; Electrum
  (the wallet built into Tails) could not, because it only signs multisig wallets built from every cosigner's
  extended key. Since A-456b the market also serves the per-order layout described below, with a recorded
  BIP39 Electrum signing journey on regtest (A-457). Later A-458 evidence records Sparrow GUI per-order practice,
  release and sibling-refund signing on isolated regtest; a separate watch-only descriptor import/address match is
  recorded, but signing from that imported wallet and physical hardware remain untested
  ([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)).
  `scripts/test-escrow-wallets.sh` repeats this check (real local chain plus a real Electrum, run by hand, not CI:
  Bitcoin Core 31.1 on regtest with P2P disabled and RPC bound to host loopback, Electrum 4.8.2 checked against its release signature and used
  offline). A Bitcoin Core wallet signs the transaction the market serves today. An Electrum wallet restored from a
  BIP39 seed at a key path made for one order signs the per-order layout (shape (b)). Electrum adds a derivation entry
  for every key, the market's included, which the market's signature-paste check refused until A-456b dropped that
  refusal (ruling P4-R13): it protected nothing that is stored, because only the signer's own signatures are ever
  kept, whatever else a pasted copy carries (`TestEscrowPasteForeignFieldsInert`; `TestEscrowWalletFixtures`
  checks that the signed files in `internal/market/testdata/escrow-wallets/` are now accepted; those files come from the
  script's `--update-fixtures` run at `cf6a8a0` with uncommitted changes, logged in
  `artifacts/real-chain/a450-cf6a8a0fdbb4-dirty-fixtures-wallets.log`, and the clean evidence run at `52eb99f`
  (`artifacts/real-chain/a450-52eb99f7aba5-wallets.log`) used different throwaway keys, so its log does not contain
  them; both logs are local and gitignored). Electrum's own seed type, what "Create a new seed" makes and the
  default in Tails, cannot follow a per-order key path at all: Electrum fixes its multisig keys at `m/1'` and
  takes no path for that seed type (`from_seed` in Electrum's `keystore.py`; the script's `electrum-seed-type`
  leg records it). On the development branch a user can register a per-order wallet key and sign its practice
  transaction ([A-455](#per-order-key-layout-registration-and-practice-a-455-shape-b)), and two such users get a
  per-order escrow ([A-456b](#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b)). A-457's manual journey
  (`TestEscrowRegtestElectrumMarketJourney`, `scripts/test-escrow-wallets.sh`) used two BIP39 Electrum 4.8.2 parties:
  both registered and signed practice through the market; one signed a release and the other a sibling-deposit refund;
  the real escrow signer co-signed both, and both were broadcast and confirmed on regtest. A mixed Core/per-order pair
  was refused. At A-457, Electrum GUI menus, Sparrow, physical hardware and public networks were untested; later
  A-458 records Sparrow GUI per-order signing on isolated regtest, while descriptor import, physical hardware and
  public Sparrow signing remain untested ([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)).

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
| 1 | Bitcoin escrow for digital orders on regtest, behind a setting that is off by default | Built and tested on regtest (and, since A-443, a private signet) on the development branch, not released, behind `ESCROW_BTC` (off by default; regtest, signet or testnet4): see [Phase 1](#phase-1-bitcoin-escrow-on-regtest) for what it does, what it protects and the tests behind it. The public signet smoke covers a digital claim and underpaid refund; mainnet remains untested and refused by escrow. The original layout uses Bitcoin Core, while the per-order layout has BIP39 Electrum evidence (A-457) and partial Sparrow GUI evidence (A-458), including a matching address from a separate watch-only descriptor import; signing from that imported wallet, physical hardware and public Sparrow signing remain unverified |
| Wallets | Electrum and Sparrow support (a signing test showed Electrum needs a different key layout) | Built on the development branch: per-order key derivation (A-453), the escrow signer (A-454), registration and practice (A-455), and checkout/settlement with per-order keys (A-456b). A-457 recorded two real BIP39 Electrum parties signing a release and sibling refund on regtest; A-458 later recorded Sparrow GUI per-order practice/release/sibling-refund signing and separate watch-only descriptor import/address match on isolated regtest, but not signing from the imported wallet or physical hardware ([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)) |
| 2 | Physical orders: buyer-signed release, 14-day deadline, no-ship cancellation, fee bumps | Built and tested on regtest (and, since A-443, a private signet); not released; off by default (`ESCROW_BTC`; regtest, signet or testnet4): see [Phase 2](#phase-2-bitcoin-escrow-for-physical-orders-on-regtest) for what it does, what it protects, what it does not do and the tests behind it. Not run on any public test network or on mainnet; Bitcoin Core and the per-order BIP39 Electrum layout are implemented, with the recorded A-457 journey scoped to its release/refund legs |
| 3 | Monero accounts in monthly escrow wallets | Built and tested on a local Monero fakechain (monerod `--regtest`); not released; off by default (`ESCROW_XMR`): see [Phase 3](#phase-3-monero-escrow-on-a-local-fakechain) for what it does, what it protects, what it does not protect and the tests behind it. Custodial: the market can move these coins. Since A-449 it also accepts stagenet ([below](#monero-escrow-on-stagenet-a-449)), checked on an offline stagenet fork and in one public stagenet digital claim ([record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)); public refunds and physical deadlines remain unrun, and testnet/mainnet are refused; no Monero multisig |
| 4 | Public test networks end to end, then escrow-only for new orders, then removal of the pooled wallet | Under way: the Bitcoin node sync guard, the escrow chain pin and the chain table (A-439, [below](#node-sync-guard-chain-pin-and-chain-table-a-439)), the market key protection (A-440, [below](#market-key-protection-a-440)), Monero escrow on stagenet with its network pin and daemon sync guard (A-449, [below](#monero-escrow-on-stagenet-a-449)), broadcast honesty (A-441, [below](#broadcast-honesty-a-441)), the escrow-only switch with its readiness panel (A-444, [below](#escrow-only-cutover-a-444)), which no market has used on a public network, the pooled wallet's drain gate and panel (A-445, [below](#pooled-wallet-drain-a-445)), which no market has drained, and Bitcoin escrow on signet and testnet4 with its private-signet legs (A-443, [below](#bitcoin-escrow-on-signet-and-testnet4-a-443)) are on the development branch; the public signet practice/claim/refund and one public stagenet digital claim have recorded proofs; neither is a deployed-market or real pooled-drain proof |


## Phase 1: Bitcoin escrow on regtest

**Built and tested on regtest; not released; off by default.** Fresh installs offer escrow only; without an enabled escrow there is no payable checkout. With `ESCROW_BTC=on` (refused unless the escrow node reports
chain `regtest`, or since A-443 `signet` or `testnet4`; [operator guide](operator-guide.md#bitcoin-escrow-test-networks-only)) the development branch has:

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
  they are the vendor's; staff are told once (A-461), with an order note naming the deposits it tried, and the
  order's credited deposits are flagged so staff can open the order, also one never disputed and also when it tried
  none of them (QA462C-2; they stay counted; the desk dates the flag from that note and offers no action on it). A
  counted deposit not credited at funding (still confirming or conflicted then) is not flagged, and its flag set by
  another check is not open: the desk offers no action on it, a review is refused (409) and it does not keep a
  late-deposit notice episode open (QA462B, QA462C). One rule decides whether a payment flag is open, for the desk and
  the review alike: a locked transfer, a deposit of a payout below the minimum, an announced regression, a deposit spent
  outside the market, or a deposit that does not count (recorded after the close, or left out of its settlement,
  credited or not, listed as such: QA462D-2), until staff record it; only such an open flag of a deposit neither
  credited, nor below the minimum, nor locked, nor spent outside the market holds the late-deposit notice episode
  (QA462D-1). A refund too few's note names the deposits it leaves out, and a release too few's every deposit it
  flags, so the desk dates those flags (QA462D-4); the desk lists a credited escrow deposit that counts as settled by
  the order's release or refund, never as one that "confirmed again" (QA462D-3). A stray
  refund still being built when the refund fails (both in the same pass) adds those coins to its own: once it is built and spends them, the refund's failure holds nothing
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
once its settlement has a final transaction. Until then an administrator may change it, also while its settlement
is still being built or after it failed (migration 087, A-462): the old settlement is superseded as it stands (the
database refuses a superseded row that gains or loses a build, and any settlement moving back to building), and when
it was a refund whose too-small failure left its deposits out for a stray refund, those deposits count for the order
again, so the vendor's release includes them and no stray refund returns them (a stray refund being built from them
at that moment claims only deposits still left out or sent after the close, so it is built again on the next pass
without them); deposits left out for any other reason, or already spent by a built stray refund or outside the market,
stay as they are. All of this is
subject to the limits under *What it will not protect* above: a server taken over can show a substituted key, and the market key with one party's key can spend.

**What Phase 1 does not do.** It was built and tested on regtest (and, since A-443, a private signet) and has not run
on a public test network or mainnet. It covers
digital orders with automatic delivery only (physical orders are Phase 2, Monero Phase 3). It supports Bitcoin Core
signing only. When a settlement is recorded as spent outside the market, deposits of that escrow that are still
unspent stay there: the market builds nothing more for them, staff are told, and the buyer and the vendor can move
them together from their own wallets. A reorganisation deeper than 100 blocks is not followed.

**How it was tested.** Each tier proves only what it says:

- Go tests with PostgreSQL against a fake escrow node (simulated RPC, not chain evidence) for every state change and
  refusal, for example `TestEscrowCheckoutCreatesEscrow`, `TestEscrowPracticeActivates`, `TestEscrowFundingCompletesOrder`,
  `TestEscrowClaimBuildAndSign`, `TestEscrowPasteRefusals`, `TestEscrowRefundRoutes`, `TestEscrowSettlementRaces`,
  `TestEscrowCosignBroadcastConfirm`, `TestEscrowRawStoredBeforeBroadcast`, `TestEscrowBadSignatureReverts`,
  `TestEscrowBothPartiesSign`, `TestEscrowDisputeDecision`, `TestEscrowDecisionChangeUnbuilt`,
  `TestEscrowMigration087SupersedeUnbuilt`, `TestEscrowDecisionChangeStray`, `TestEscrowReleaseTooFewStaffOpen`,
  `TestEscrowStrayRefundAfterClose`,
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

This browser journey did not test any public test network, mainnet, escrow pages in Firefox or WebKit (it runs in
Chromium only; CI runs preview pages in Firefox and WebKit), Sparrow or hardware wallets. Later A-458 evidence records
Sparrow GUI per-order signing on isolated regtest, but not supplied descriptor import or a physical device
([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)). The A-457 regtest record covers BIP39 Electrum
release/refund signing; it does not cover Electrum GUI menus.

## Phase 2: Bitcoin escrow for physical orders on regtest

**Built and tested on regtest; not released; off by default.** Phase 2 adds to Phase 1 and is switched on and off
by the same setting. Fresh installs offer escrow only. With `ESCROW_BTC=on` (refused unless the
escrow node reports chain `regtest`, or since A-443 `signet` or `testnet4`; [operator guide](operator-guide.md#bitcoin-escrow-test-networks-only)) the
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

- It was built and tested on regtest (and, since A-443, a private signet) and has not run on a public test network or
  on mainnet. It supports Bitcoin Core signing only. Monero is Phase 3.
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

The Phase 2 checks did not test any public test network, mainnet, escrow pages in Firefox or WebKit, Electrum, Sparrow
or hardware wallets. Later A-458 evidence records Sparrow GUI per-order signing on isolated regtest, but not supplied
descriptor import or a physical device ([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)). Also untested:
deadlines of 7 to 30 days in real time (every test moves the clock), and a restore of a real backup into a
running market with Phase 2 orders on a real chain. `scripts/test-backup-restore.sh` (run in CI) restores a real dump
through `scripts/restore.sh` with escrow settlement, fee bump and index floor rows and checks the deadline recheck,
the sweep marker, the floors and the market's key index; the Go tests run the margin block as written.

## Phase 3: Monero escrow on a local fakechain

**Built and tested on a local fakechain; not released; off by default; custodial.** Phase 3 adds Monero to the escrow
built in Phases 1 and 2 and is switched on and off by its own setting, `ESCROW_XMR`. Fresh installs offer escrow only. A *fakechain* is `monerod --regtest`, Monero's regtest and the counterpart of Phase 1's
Bitcoin regtest: a local chain whose coins have no value. Its wallets use the mainnet address format, so on it a refund
or claim address starts with 4 or 8. With `ESCROW_XMR=on` (refused when the market is configured for mainnet, and unless
the escrow daemon reports nettype `fakechain` and a hard fork version of at least 16 and the escrow wallet-rpc is within 10
blocks of that daemon; Phase 3 also required a height below 1,000,000, which A-449 replaced by a network pin when it added
stagenet, [below](#monero-escrow-on-stagenet-a-449); [operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet))
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
  are told, with the deposits it could spend flagged so they can open the order (the note names them, so the desk
  dates the flags: QA462E-3); a deposit recorded after the order
  closed is left to the late-deposit check, which flags it when it announces it to the buyer and staff, so a waiting
  stray refund never silences that notice (QA462C-4). A checked transaction is **stored first** (its hash, metadata, blob, fee and key images) and only then relayed,
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
  (not enough money) waits once and is flagged if it persists, except for a refund whose coins are too few for the
  network fee (below, A-467). These are the codes `monero-wallet-rpc` 0.18.5.1 gave
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
  coins (A-437). Coins at or below the network fee itself never reach that check: asked to take the fee from the
  refund, `monero-wallet-rpc` 0.18.5.1 refuses to build it, with -17 (not enough money; 0.001 and 0.0015 XMR on a young
  fakechain), -16 (tx not possible; 0.0018 XMR) or -46 (destination amount is zero: exactly at the fee); each code was
  recorded from the real wallet at those amounts (`xmr_rpc_shapes_0.18.5.1.json`, QA5W1-5). Those codes also mean other things
  (coins the wallet no longer sees as unlocked or confirmed, a wallet behind the chain), so on them, for a refund or a
  stray refund, the market reads the fee the wallet would take, with calls that build nothing: the priority the wallet
  uses for `transfer` at priority 0 (`get_default_fee_priority`; its -1 means priority 1), the weight it estimates for
  the inputs claimed and two outputs (`estimate_tx_size_and_weight`, ring 16) and the escrow daemon's fee per byte at
  that priority with its quantization mask (`get_fee_estimate`, on monerod's restricted RPC). The fee is wallet2's own
  formula, weight times fee per byte rounded up to the mask; no margin is added, because wallet2's weight estimate is
  already above the built transaction's (on the fakechain, A-467 probe: estimated 1536 for one input, built 1507, so
  0.0018432 XMR against 0.0018084 XMR). When the refund less that fee is below 0.001 XMR, the refund is too few as
  above (ended, or for a stray refund waiting, too-small), whatever the wallet's reason; otherwise the refusal keeps its
  class (-17 waits once and is then an outputs mismatch, flagged; -16 and -46 a refusing wallet). The estimate only
  decides among refusals: a refund the wallet builds is checked as before, and a refund classed too few by a slightly
  high estimate is returned by the stray refund, which the wallet then builds. A fee estimate that fails never classes
  the refund too few: it waits on that settlement alone (`fee-estimate-waiting`), is flagged to staff once if the next
  pass fails the same way (`fee-estimate-unavailable`), and is tried again on each pass, while the wallet's pass goes
  on with its other orders, their payment windows and the wallet's retention (QA5W1-4). A failed estimate and a refusal
  count as one run of passes without a build: when the estimate fails only on some passes, the second pass in a row is
  flagged as whichever class it records (the refusal, or the unavailable estimate) (QA5W2-1). Within that run, which
  ends only when the settlement is built or leaves its waiting state, staff are told once of each distinct flagged
  class it records (the unavailable estimate, an outputs mismatch, a refusing wallet with its code, too few): a class
  already told that recurs, after another class or a wait never flagged such as -37's `outputs-waiting`, is told
  nothing new, while a different diagnosis is (the wallet seeing fewer coins than it listed, or refusing, once the
  estimate answers; a stray refund's coins found too few). What was told is read from the settlement's own order notes,
  so a restart re-announces nothing; a stray refund already flagged too-small that a restart makes read a failing
  estimate is told of the unavailable estimate once, and not of too-small again (QA5W3B-3, QA5W3B-4). A stray refund waiting
  too-small that the wallet keeps refusing is logged once, and its fee is not read again while it claims the same
  outputs; `transfer` is still asked on each pass, so a later deposit that lets it build is seen (QA5W1-6). A release
  too few still waits, and its refusals keep their classes: the
  coins are the vendor's. After a close, deposits are refunded automatically until the wallet is
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
- **It was built and tested on a fakechain.** Phase 3 refused every daemon but a local fakechain; A-449 admits stagenet
  ([below](#monero-escrow-on-stagenet-a-449)), checked on an offline fork and in one public digital claim
  ([record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)). That sample verifies actual funding, fees and claim confirmations; public refunds,
  physical deadlines, fee/ring-member stress and reorganisations remain unverified. Testnet and mainnet are refused.
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
  ([operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet)).
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
  fakechain). Fifteen legs, each a Go test:
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
  15. `TestXMREscrowRegtestRefundBelowFee` (A-467): refunds of 0.001, 0.0015 and 0.0018 XMR decided for the buyer are
      refused by the wallet (-17, -17, -16: the coins are at or below the network fee); each ends too few on the
      market's fee estimate, its deposit is left out, and the one stray refund returns it with a later deposit, relayed
      and confirmed; no note says the outputs mismatched or the wallet refused.

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

The [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) covers one funded digital claim. Other public stagenet
legs remain untested; A-449's offline fork cannot run them. Not tested: testnet and mainnet, Monero multisig, the escrow pages in Firefox or WebKit, deadlines of 7 to 30
days in real time, a wallet with thousands of accounts on a public network (PAYE-E2 measured one on a local fakechain), a restore of a real backup into a
running market with Phase 3 orders on a real chain (`scripts/test-backup-restore.sh` restores a real dump and checks its
rows; the fakechain `Restore` leg simulates the database restore in SQL), and a deployment of any of it.

## Node sync guard, chain pin and chain table (A-439)

Phase 4 groundwork (war-room ruling P4-R3 to P4-R5), on the development branch and not released. Escrow accepts
regtest and, since A-443, signet and testnet4 ([below](#bitcoin-escrow-on-signet-and-testnet4-a-443)); the guard also
protects pooled Bitcoin payments now.

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
  ([operator guide](operator-guide.md#bitcoin-escrow-test-networks-only)); on regtest the initial block download line adds
  the remedy (mine one block, or restart bitcoind with a larger `-maxtipage`), since a regtest node has no peers to
  sync from. Past its payment window, while the node is not synced, an open escrow order's page says the order stays
  open while the market waits for its node, instead of a cancellation time already gone; with the node synced, in the
  usual gap before the next watcher pass, it keeps the usual wording. A lone regtest node is exempt from the peer and
  tip-age checks, and `getnetworkinfo` is called only on a public chain (or, since A-441, in `APP_MODE=tor`).
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

## Market key protection (A-440)

Phase 4 groundwork (war-room ruling P4-R6), on the development branch and not released. `rpcwhitelist` is per user,
not per wallet, so a pooled RPC user on the escrow node that may call `listdescriptors` can print the arbiter wallet's
private key (probed on Core 31.1 regtest), as can one that may call `gethdkeys` (`{"private":true}`), and one that may
call `walletprocesspsbt` can sign with it (QA4W3-1). With `BITCOIN_RPC_URL` set, escrow starts only when the pooled user
is refused each of `listdescriptors` (private=false), `gethdkeys` (private=false) and `walletprocesspsbt` (an empty PSBT,
sign=false) on the arbiter wallet's path with HTTP 403, or that node has no such wallet; any other answer, error or
timeout leaves escrow disabled with a fixed log line naming the method and a line on `/admin`, retried like the other
escrow checks, and while escrow is on the watcher repeats the check daily and disables escrow on a failure (QA4W3-8).
The arbiter wallet's dedication check counts only a spend from it, never coins paid to it (A-471: anyone can pay the
market child key an order's descriptor shows); a passing check records a checkpoint (the block `listsinceblock`
reported, for the wallet's name and birthtime), so later checks read only newer entries and mass payments to that key
cannot keep escrow off by slowing the read (QA4W4A-1). The operator guide whitelists the pooled user to the nine methods the pooled adapter calls
(*The pooled RPC user*; a Go test keeps the line equal to the list in code) or puts escrow on its own node, and states
that escrow and the pooled wallet on one node share one failure domain; the
[runbook](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) backs up and restores the
arbiter and watch-only wallets (operator user, encrypted to the operator's `age` key, height recorded,
`importdescriptors` fallback, fingerprint pin check), never inside the database backup.

How it was tested: `TestEscrowPooledKeyCheck` (simulated RPC and PostgreSQL: 403 and -18 from each method pass; an
answer, error -4, HTTP 401 and a timeout of `listdescriptors`, a `gethdkeys` answer and an allowed `walletprocesspsbt`
(-22) disable, with the log and admin lines; a retry starts escrow once the user is refused; the daily re-check disables
escrow on a widened whitelist, logs once and starts it again once refused), `TestBitcoinPooledRPCMethods` and
`TestEscrowArbiterWalletDedicated` (receives only: on, after one full `listsinceblock`; 5000 more receives: on, read
from the checkpoint in one call; one spend behind 2000 more receives: off; another wallet or an unknown checkpoint block:
read whole once; an answer beyond the 16 MiB reply limit: read as it arrives; an unreadable list: off with the
read-failure line), `TestEscrowArbiterReadOutlivesStartup` (a read longer than startup allows: off with its own line and
admin text, then on at the watcher's retry with no second read; a read that times out: the read-failure line),
`TestEscrowArbiterScanReuse` (QA4W5-4: a failed read is not reused by the retry; a held read is not adopted after the
birthtime changes), `TestEscrowArbiterBadCheckpointBlock` (QA4W5-8: a checkpoint block that is not 64 hex characters
counts as no checkpoint, one full read; a -8 answer falls back to a full read as -5 does) and `TestRPCCallStream` (QA4W5-2: 100,000 entries read; one 300 MiB entry
or skipped key refused, the heap growing by under 64 MiB); on Core
31.1 regtest `TestEscrowRegtestPooled` (the whitelisted pooled user refused
`listdescriptors`, `gethdkeys` and `walletprocesspsbt`, escrow on beside a working pooled provider, an unrestricted user
keeping escrow off), `TestEscrowRegtestArbiterRestore` (backup, wipe, `restorewallet` and the `importdescriptors`
fallback, each co-signing a claim with the fingerprint pin unchanged) and `TestEscrowRegtestArbiterDeposit` (a payment to
an escrow's market child address leaves escrow on after a restart and a retry, and so do three more of 2000 outputs each
across two restarts; a spend from the arbiter wallet to its own change address, behind them, turns it off). Not tested: the watch-only wallet's restore steps in the runbook, and any
public network.

## Monero escrow on stagenet (A-449)

Phase 4 work (war-room ruling P4-R15, owner decision 2026-10-05), on the development branch and not released. Monero escrow
(still off by default, still custodial) accepts stagenet as well as a local fakechain. The [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) passed with real funding and a confirmed vendor release.
The broader network/failure checks below retain their simulated-RPC or offline-fork evidence tiers.

The staff alert for a requested Monero settlement older than 24 hours is a visibility threshold, not a measured settlement
or refund-time bound. The public digital claim's request-to-confirmed sample was 27m44s; public refund waits remain unmeasured, and one sample does not validate the alert threshold.

- **Network check and address tags.** The escrow daemon must report nettype `fakechain` (no network flag set) or
  `stagenet` (the stagenet flag set), and a hard fork version of at least 16; mainnet, testnet and inconsistent flags are
  refused. A daemon still syncing (synchronized false, or a target height above its height) is not refused for a lower
  hard fork, which it reports for the height it has reached: Monero escrow stays disabled and is checked again every
  pass, so a fresh stagenet node delays Monero escrow, not the market's startup. The address forms Monero escrow accepts follow the network it verified, never a configured one, and the two
  changed together: on stagenet the standard address (tag 24, `5…`) and subaddress (36, `7…`), on a fakechain the mainnet
  format it uses (18, `4…`; 42, `8…`); integrated addresses (19, 25) are refused as such, and every other tag as another
  network's. This holds for refund and claim addresses, `ESCROW_XMR_FEE_ADDRESS` (a fee address of the other network stops
  startup) and every address the escrow wallet-rpc returns (a stagenet wallet's reply is refused on a fakechain escrow and
  the reverse). Before the network is verified no address parses; a vendor's claim while the daemon is unreachable uses the
  network pin. The pooled Monero adapter is unchanged (it asks its own wallet).
- **Network pin, replacing the height heuristic.** Phase 3 told a fakechain from a public network by the daemon's and the
  wallet's height (below 1,000,000). That is gone: the database pins the escrow daemon's network (`settings` key
  `escrow_xmr_network`). While the database holds no Monero escrow and no escrow wallet (a wallet's free accounts are
  addresses of one network) the pin follows the daemon. Once it holds either, a daemon on the other network leaves Monero
  escrow disabled, writes nothing, logs one fixed line and shows both networks under *Payment providers* on `/admin`; it
  is retried every pass. A database from before the pin that holds Monero escrow state is pinned `fakechain` when the
  daemon reports fakechain, and refused otherwise. The wallet-rpc must still be within 10 blocks of the daemon, and an
  escrow wallet-rpc whose open wallet is the pooled one is refused at startup (both may now follow one stagenet daemon).
- **Public pages name the verified network (A-472).** The transparency page (`/canary`), the footer, catalog, listing and
  draft notices and the vendor desk tile name the network by the rule `/admin` uses: "network not verified yet" until a
  daemon was verified or the pin was read, "Monero stagenet, a public test network" on stagenet, "a local Monero
  fakechain" on a fakechain. On stagenet the canary says Monero escrow "has been tested only on local test chains, not yet
  on the public stagenet" and lists "Monero escrow verified on the public stagenet" as unavailable; "not verified on any
  public network" stays on a fakechain; while the network is unknown the canary says the market has not yet checked
  which test network it is and that nothing has run on a public network (QA5W1-8), and the buyer's Monero escrow checkout
  offer says "network not verified yet", Monero stagenet or a local Monero fakechain by the same rule (QA5W1-3). On Bitcoin testnet4 the canary
  says the same (tested only on local test chains, not yet on public testnet4); regtest and signet keep "not verified on
  any public network". The same rule names the network in the dispute pages (the moderator's refusal and the
  administrator's preview), `/admin`'s Monero escrow recovery heading and the Bitcoin order panel's headline (testnet4);
  the dispute amounts say "0.5 stagenet XMR" or "0.5 fakechain XMR" once the network is known and plain "XMR" before.
  A Monero escrow order's own panel names the network of its stored escrow address (its tag), not the market's current
  one, so it stays right with Monero escrow off or its network forgotten. Copy only: `TestXMREscrowPagesNameNetwork`,
  `TestXMREscrowDisputeAndAdminNameNetwork`, `TestXMREscrowOrderPanelNamesItsOwnNetwork`,
  `TestEscrowCanaryAndNoticesNameChain` (simulated RPC, not evidence of a public run).
- **Wallet height after a refresh, compared with a fresh daemon reading (A-475).** The daemon's height was read before
  the wallet's refresh, which can take minutes, so a daemon that gained more than 10 blocks meanwhile (catching up after
  downtime, a slow pass; the browser journey's `mine` of 100 blocks did it under load) left the wallet, correctly synced
  to it, more than 10 blocks ahead, and Monero escrow was disabled until a restart. A wallet more than 10 blocks from the
  first reading is now compared with the daemon's height read again (`get_info`), and refused, as before permanently,
  only when it is more than 10 blocks outside the span of the two readings (below the lower less 10, or above the
  higher plus 10): the daemon may move again between the refresh and the second reading, and a height between its own
  two readings is one it had meanwhile. A wallet-rpc on another daemon more than 10 blocks from both readings is
  refused on the same pass; a failed second reading is retried, never a refusal
  (`TestXMREscrowWalletLagRereadsDaemon`, unit; `TestXMREscrowDaemonGainsBlocksDuringRefresh`, database).
- **Daemon sync guard.** The rule of the Bitcoin guard above, for Monero, shared with the pooled Monero check (which before
  read only `target_height`: A-439 found the hole): a daemon is not synced while `get_info` reports `synchronized` false or
  a `target_height` above its height, or, on a public network, it has no peers or its top block (`get_last_block_header`,
  the one RPC the app did not call before; get_info carries no block time) is older than `MONERO_MAX_TIP_AGE` (default 1h,
  20m to 24h). "No peers" is `offline`, or no connections on an unrestricted RPC: monerod 0.18.5.1 reports both
  connection counts as 0 on a restricted RPC (`--restricted-rpc`, as the bundled `monero` service and most remote nodes
  serve) whatever its peers, so there the counts are not read and the tip's age covers a daemon cut off from the network.
  While the escrow daemon is not synced the Monero escrow pass opens no wallet, reads no deposit, funds or expires no order
  and builds, relays or follows no settlement (deadlines still apply); the pooled Monero pass is skipped as while syncing.
  An order held open past its payment window that way (no deposit waiting for confirmations) says on its page that the
  window ended and the order stays open while the market waits for its Monero node, not that it will be cancelled at a
  time already past (as the Bitcoin page does, QA4W1-5; `TestXMREscrowWindowPassedWhileNotSynced`).
  Stateless; one log line per change and a line under *Payment providers*. A fakechain is exempt from the peer and tip-age
  checks and `get_last_block_header` is not called on it.
- **Compose service (INFRA-11).** `compose.nodes.yaml` has a `monero-escrow-wallet` service: the escrow wallet-rpc on
  stagenet, sharing the bundled `monero` daemon, in its own profile and volume with its own login password; the installer's
  `--monero-escrow yes` (or its question beside the Monero questions) generates that password and `MONERO_WALLET_SECRET`,
  asks for the seed key file and mounts it through `compose.escrow-xmr.yaml`
  ([operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet)). Checked with `docker compose config` and the
  installer's tests (a stub Docker); the Compose service has not been started against a public stagenet daemon. The recorded public smoke used separately launched Docker containers, so it does not verify this deployment path.

How it was tested. Simulated RPC and PostgreSQL, not chain evidence: `TestXMRAddressTagsPerNettype`,
`TestXMRAddressStagenetVectors` (real stagenet addresses from a disposable wallet and Monero's own test table),
`TestMoneroNodeNotSynced`, `TestParseMoneroMaxTipAge` (unit); `TestXMREscrowStagenetTags` (a stagenet escrow's wallets,
checkout, claim, notes and pages), `TestXMREscrowFakechainTags`, `TestXMREscrowFeeAddressFollowsNetwork`,
`TestXMREscrowNetworkPinMismatch`, `TestXMREscrowNetworkPinBackfillAndRepin`, `TestXMREscrowNoHeightHeuristic`,
`TestXMREscrowSkipsWhileNotSynced` (each condition, restricted RPC included: no wallet opened, no expiry, no funding, no
build, then the work on the next synced pass at a lower height), `TestXMREscrowFakechainExemptFromPeerAndTipChecks`,
`TestMoneroPooledGuardPeersAndTipAge`, `TestMoneroPooledNoPeersHoldsExpiry`. Real chains, manual: the fourteen Phase 3 legs
again on a fakechain (`scripts/test-escrow-xmr-regtest.sh`), and on an **offline stagenet fork, not the public stagenet**
(`scripts/test-escrow-xmr-stagenet.sh`: stagenet daemons built from the genesis block with the local monerod 0.18.5.1
image, peered only with each other on a Docker network with no route out, local mining at fixed difficulty) the legs such
a chain can run. Built from genesis it is at hard fork version 1 (version 16 starts at height 1,151,720), so Monero escrow
refuses to start on it, as it must, and none of the fifteen legs can run there; the stagenet legs show that refusal names
the hard fork, not the network; that a real stagenet wallet's primary address, subaddress and integrated address are read
under the stagenet tags (the integrated one refused) and refused under the fakechain's, by the parser and by the adapter;
and that the escrow and pooled sync guards read a restricted stagenet daemon with a peer and a fresh tip as synced and an
`--offline` daemon as having no peers. No funded escrow leg can run on that fork (hard fork 1, and `generateblocks`, which
the fakechain legs mine with, works only on a fakechain), so **funded Monero escrow on stagenet is proven only by the
recorded public stagenet digital-claim run (A-447/A-491)** ([evidence](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)); the offline fork proves the stagenet address formats, the old-fork
refusal and the sync guards, and the network pin rests on the simulated-daemon database tests above. Not tested: the
other public stagenet workflows, fee stress, ring-member stress and reorganisations, a daemon that loses its
peers on a public network, and `ESCROW_XMR_MAX_FEE` against stagenet fees. On a restricted RPC a daemon cut off from its
peers looks synced until its tip is older than `MONERO_MAX_TIP_AGE` (QA4W3-5, accepted and documented in the
[operator guide](operator-guide.md#payments): a deposit made in that window is not seen, its order can be cancelled as
unpaid, and the deposit is then returned by a stray refund, or by hand on a pooled order). Since QA4W3-2 Monero escrow waits until a configured
pooled Monero wallet has passed one check, so the open-wallet comparison always has the pooled address
(`TestXMREscrowRefusesPooledWalletOpen`); since QA4W3-3 an unreadable tip is tested as not synced on both paths
(`TestXMREscrowSkipsWhileTipUnreadable`, `TestMoneroPooledGuardPeersAndTipAge`).

## Broadcast honesty (A-441)

Phase 4 groundwork (war-room ruling P4-R7 (a) and (b)), on the development branch and not released. Bitcoin Core 31.1's
`-privatebroadcast` with a proxy that does not answer accepts `sendrawtransaction`, answers the transaction ID and keeps
the transaction in a private queue: it never reaches the node's mempool, a resend is idempotent, and the market recorded
it as broadcast (probed on Core 31.1 regtest by the A-255/A-249 council and again by this slice's real-chain leg).

- **Stall alert, every mode.** A settlement in state broadcast none of whose transactions (its own and its fee bumps')
  the watch-only wallet has seen 6 blocks after the broadcast tells staff once (an order note and a "Payment review
  needed" notification to each moderator and administrator) and shows "queued or stuck: not seen by the network" on
  `/admin` under *Payment providers* and to staff on the order page, until the wallet sees one: then one order note
  clears it. Six blocks: a transaction the node accepted into its mempool is in the wallet at once and one sent
  privately comes back from the network within minutes, so six (about an hour on a public test network, twice the
  default confirmation threshold) is well past either and still within the hour. No settlement state is added
  (migration 074's CHECK is unchanged); the settlement stays broadcast and its stored transaction is sent again on each
  pass as before. Which alert is open is read from the two order notes; the height a transaction went unseen from is in
  memory only, so a restart starts the count again (later, never twice). `getprivatebroadcastinfo` stays off the app's
  whitelist and `abortprivatebroadcast` is never on it.
- **Tor-mode gate.** With `APP_MODE=tor` only, the escrow watcher reads `getnetworkinfo` once per pass and sends a
  settlement only while it shows a proxy for both IPv4 and IPv6 (what `-proxy` sets; `-onion` alone does not) and
  Bitcoin Core 31.1 or later (version 310100); otherwise, or when `getnetworkinfo` fails, no `sendrawtransaction` is
  issued and no settlement changes state, with one log line per change and an `/admin` line saying what to configure
  (`-proxy=<Tor SOCKS address>`, never `-connect` or `-onlynet`). Onion `reachable` is not read: it reflects
  configuration, not liveness. The gate checks configuration; the stall alert covers a proxy that is configured but
  down. Pooled payouts are wallet sends and are never private. Clearnet mode is unchanged. See the
  [operator guide](operator-guide.md#bitcoin-escrow-test-networks-only) (*Tor mode*, *Queued or stuck*).

How it was tested: `TestEscrowBroadcastStallAlert`, `TestEscrowBroadcastStallSeenNeverAlerts` and
`TestEscrowTorBroadcastGate` (simulated RPC and PostgreSQL, not chain evidence: not before 6 blocks however many passes
run at one height, once, cleared on sighting, the resend unchanged; the gate per condition, including `-onion` only with
onion reachable, Core 31.0, a failed or refused `getnetworkinfo`, and clearnet mode); `TestEscrowRegtestPrivateBroadcast`
in `scripts/test-escrow-regtest.sh` against a second Core 31.1 regtest node started with `-privatebroadcast
-proxy=127.0.0.1:9 -listen=0`: the gate on real `getnetworkinfo` answers (that node passes, the main node without a proxy
holds), a co-signed claim queued and never in the mempool or the wallet, the alert at 6 blocks and not before or twice,
and cleared once `generateblock` mined the stored transaction. The later 2026-10-09 public signet smoke records real Tor proxy/private-broadcast delivery ([record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)); outage/stall branches remain local-chain evidence.

## Bitcoin escrow on signet and testnet4 (A-443)

Phase 4 (war-room ruling P4-R5), on the development branch and not released. With the sync guard, the chain pin and
the chain table (A-439), the market key self-check (A-440) and broadcast honesty (A-441) in place, `ESCROW_BTC=on`
accepts a node on `signet` or `testnet4` as well as `regtest`; mainnet (`main`) and testnet3 (`test`) are still refused
at startup (escrow uses test-network key versions and coin type 1), and `BITCOIN_CHAIN`, when set beside it, must name
one of the three. **The public signet smoke passed on 2026-10-09** ([record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)); public testnet4 remains unrun. The operator package of A-446 supports these runs: `scripts/escrow-node-setup.sh` sets up the node and
`scripts/test-escrow-public.sh` runs the smoke (practice signing, one escrow claimed and one refunded, each confirmed)
and writes its record under `docs/evidence/public-testnet/`
([operator guide](operator-guide.md#escrow-node-set-up-and-the-public-network-smoke)). The recorded public signet run passed practice signing, the digital claim and underpaid buyer refund with Core party wallets; `TestEscrowRegtestSmoke` separately covers regtest.

- **Chain table.** The chain decides the address prefix (`bcrt` on regtest, `tb` on signet and testnet4) for every
  address escrow checks or pays. An address given in capitals is lowered by that prefix, so a `TB1…` refund, claim,
  release or fee address is stored and paid as `tb1…` (QA4W1-4), as `BCRT1…` already was on regtest; another chain's
  prefix is left as given and the node refuses it.
- **Pages name the chain.** The order panel's headline and badge, the checkout offer, the escrow key page, the
  transparency page, the footer and catalog notices, the vendor desk tile, the dispute form and `/admin` say signet or
  testnet4 where they said regtest. While escrow is disabled they name the chain the node last reported, or the
  database's pinned chain; a market that has never pinned one (and holds no Bitcoin escrow) whose node has not answered
  yet names none and says "chain not verified yet" (QA4W6-8, `TestEscrowChainNotVerifiedAtFirstStart`).
- **Preconditions on these chains** (the [runbook](testnet-runbook.md#11-bitcoin-escrow-on-signet-or-testnet4)): the
  sync guard's peer and tip-age checks apply (`getnetworkinfo` must be on the escrow user's whitelist), the chain pin
  includes the signet challenge, the pooled user must be refused the arbiter wallet, and in `APP_MODE=tor` broadcasts
  wait for a proxied node. The arbiter wallet check (A-471, QA4W4A-1) applies unchanged: on signet and testnet4 a
  payment to the market's child key costs almost nothing, so anyone can fill the arbiter wallet with dust; the start
  check still reads only the entries newer than its checkpoint (streamed `listsinceblock`, `listsinceblock` on the
  escrow user's whitelist), a read that fails or times out is not a pass, and only a spend from the wallet disables
  escrow. The read's 64 KiB bound applies to each JSON token, never to an entry (QA4W6-1): Core lists every unconfirmed
  fee bump of a payment to that key as an entry whose `walletconflicts` names all the others (1000 bumps: entries of
  about 67 KB, an answer of about bumps² × 67 bytes), and the mined replacement keeps them all, so reading entries whole
  kept escrow off for good after about 1000 bumps. The read now takes each entry key by key
  (`TestEscrowArbiterEntriesStreamByToken`; the `ArbiterDeposit` leg's 1100 bumps on Core 31.1: 82 MB unconfirmed, then
  one 74 KB entry once mined, escrow on throughout; 3000 bumps, measured once: 605 MB, read in 16 s, then one 202 KB
  entry). The square-law volume lasts only while the replacements are unconfirmed: once one is mined Core lists the
  others in no read. While it lasts it costs the node memory (about 1.5 GB per 605 MB answer; the operator guide's
  arbiter wallet paragraph), and since A-479 the escrow watcher paces its retries' reads: after a read that takes 5 s or
  more or times out, pass or failure, the next waits 1 minute, doubling to at most 1 hour, and a slow pass counts as a
  pass meanwhile; a quicker read ends the waits, a quick failure is read again every poll, and a restart reads at once
  (`TestEscrowArbiterReadPacing`, `TestEscrowArbiterQuickFailureNotPaced`,
  `TestEscrowArbiterSlowPassThenLaterCheckFails`), so the market no longer adds such a read every poll. Nothing in that check depends on
  the chain.

How it was tested: DB integration with the simulated Core (`TestEscrowStartupChains`: mainnet and testnet3 refused
before any wallet call, signet and testnet4 started, pinned and named, and named from the pin with the node down;
`TestEscrowConfigChains`; `TestEscrowAddressFormChains`; `TestEscrowUppercaseAddressOnSignet`: capitalised refund,
claim and fee addresses stored in lower case and a claim built, signed, broadcast and confirmed to one;
`TestEscrowPagesNameSignet`). On a real local chain: `scripts/test-escrow-regtest.sh --private-signet` (manual, not
CI) on two Bitcoin Core 31.1 nodes with `-signetchallenge=51` on a Docker network with no route off the host, node A
mining with `generatetoaddress` (maxtries 2000000000, every call's block count checked) and node B the app's node with
one peer. It runs the regtest legs that need no regtest-only RPC (`PracticeSign`, `Funding`, `Settlement`, `Cosign`,
`Gate`, `Bump`, `Physical`, `Deadline`, `Reserve`, `BIP32`, `Signer`, `Pooled`, `ArbiterRestore`, `PerOrderPractice`
and `ArbiterDeposit`, the arbiter wallet's dust leg: 6000 outputs of 1000 sat paid to an order's market child key, a
restart that reads them from the checkpoint and keeps escrow on, 1100 fee bumps of one payment to that key with
restarts before and after the last is mined, and a spend that disables it) and
`TestEscrowRegtestIsolated`: node B leaves the network (`setnetworkactive false`), node A pays and mines an escrow
deposit, the order's payment window is moved past, and a watcher pass cancels nothing (not synced: no peers, while
Core's initial block download stays false); node B comes back and the next pass credits the deposit and completes the
order. Not run there, with the reason the script prints: `Recovery` and `Conflicted` (they fake a reorganisation with
`invalidateblock`, `reconsiderblock` and `prioritisetransaction`) and `PrivateBroadcast` (a `-privatebroadcast` node
and `generateblock`, which cannot find a signet block). That run is private signet, not public: it shows the signet
code paths and the guard's peer check on a real Core, not public peers, fees, relay or reorganisations. Regtest mode,
which CI runs, is unchanged. Last recorded on 2026-10-07 at `afb14d7` (the QA4W6 fix round; transcripts kept locally
under `artifacts/real-chain/`, gitignored): all 18 regtest legs passed (under 2 minutes), and on the private signet,
started cold (node A's coins matured afresh), the 16 legs above passed in 8.5 minutes; in `ArbiterDeposit` there, 1100
fee bumps of one payment made the arbiter wallet's answer 82 MB while unconfirmed and 74 KB once the last was mined,
and escrow stayed on through every restart.

## Escrow-only cutover (A-444)

Phase 4 (war-room ruling P4-R8), on the development branch and not released. The per-coin intake setting gains a third
value, `escrow` (*Escrow only* on `/admin`): escrow checkouts are admitted and pooled payment requests refused, both in
`lockPaymentIntake` (which every checkout calls with the kind it would issue) and in the order state machine's draft to
awaiting-payment step, so no other pooled path gets through. Paused still refuses both. The administrator's form offers
escrow only and paused with no pooled wallet configured, keeps the password and the audit row, and is reversible. The
pooled watcher keeps running while the pooled provider stays configured, so a pooled order opened before the switch is
still credited and paid out. A readiness panel counts and lists the active listings that cannot use escrow
(`escrowEligible`'s rule) and the vendors with active listings but no active Bitcoin escrow key; while a coin is escrow
only such a listing stays visible with "not available for purchase" and the reason, offers no buy form in that coin and
refuses a draft in it. An older build reads `escrow` as paused. Operator steps:
[operator guide](operator-guide.md#cutting-over-to-escrow-only); upgrade notes: UPGRADING.md section 37.

How it was tested: the Go tests and the browser spec listed in the operator guide (PostgreSQL with the fake pooled wallet
and simulated escrow nodes; not chain evidence). Not tested: a cutover on any real chain or public network, and the
removal of the pooled wallet that follows it (P4-R10, A-448).

## Pooled wallet drain (A-445)

Phase 4 (war-room ruling P4-R9, with ARCH-12), on the development branch and not released. Between a coin's cutover to
escrow only and the release that removes the pooled wallet's code (P4-R10, A-448), the pooled wallet drains: the pooled
orders opened before the switch settle and their payouts go out. Migration 091 adds `pooled_drain_blockers()`, a
read-only (STABLE) SQL function listing, one row per item with a reference staff can act on, what still needs the pooled
wallet: open pooled orders (no escrows row, neither a draft nor closed), payouts not sent (every payout is pooled), flagged
pooled deposits whose review is open (the moderation desk's open-flag rule, so the existing disposition closes each one),
pooled deposit addresses of closed orders inside the watch horizon, the restore gate, issued-address warnings not
dismissed (migration 071) and pooled history on mainnet, which always blocks because no release has mainnet escrow. The
horizon is `pooled_watch_horizon()`, which the pooled watcher itself now calls, so the gate and the watcher share one
rule: a deposit inside it is recorded and blocks until its review is recorded; one after it is never read and never
blocks. A deposit recorded inside it but still below the threshold when it ends keeps its address watched and listed
until it confirms (it is then flagged for review), the wallet stops reporting it, or 14 days pass after the later of
the horizon and its first observation while it remains unconfirmed (then flagged for staff review through the existing
disposition; A-486, A-487).
`pooled_pending_deposit()` names the waiting deposit. The removal release's refusing migration is planned to read this
function; this release refuses nothing.

`/admin` shows the list read-only (*Pooled wallet drain*), with each pooled wallet's balance as advice (Bitcoin
`getbalances`, now on the pooled RPC user's method list; Monero `get_balance`) and the note that the market never sweeps:
once the list is empty the operator moves what is left by hand. The list decides, not the balance. A blocked payout (no
payout address) can be held to be paid by hand (a staff note, password step-up, audit; the hold survives an address
save), then recorded as sent by hand with the wallet's transaction ID, password step-up and an audit row; a queued
payout, one being sent or one already sent never is, and the refusal says the market sent or will send it itself. Operator steps: [operator guide](operator-guide.md#draining-the-pooled-wallet);
upgrade notes: UPGRADING.md section 46.

How it was tested: the Go tests, the seeded `scripts/test-upgrade.sh` stage and the browser spec listed in the operator
guide (PostgreSQL with the fake pooled wallet and simulated wallet RPC; the `getbalances` answer's shape was recorded from
Bitcoin Core 31.1 on regtest). Not tested: a drain of any real or public-network pooled wallet, and the removal release.

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

The second piece of per-order wallets (ruling P4-R17), on the development branch. When it was built no order used it;
since A-456b it co-signs per-order escrows ([below](#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b)), while
the Core arbiter wallet co-signs Bitcoin Core escrows. `cmd/escrow-signer` holds
the market's key for per-order escrows in a fresh seed of its own (a mode-0400 file in its own volume, never in `.env`,
the database or `scripts/backup.sh`'s output), listens only on a Unix socket the app's container mounts, and signs one
escrow's PSBT per request through Core's `descriptorprocesspsbt` under its own RPC user (whitelisted to `decodepsbt`,
`descriptorprocesspsbt` and `getdescriptorinfo`) with `ALL` hard-coded. It refuses, with nothing signed, any sighash but
ALL, any input that is not the same `2 <three sorted keys> 3 CHECKMULTISIG` P2WSH escrow with a matching `witness_utxo`,
a script without its key for the requested order index (`account/i/0/k` or `account/i/1/k`, k < 20) or with two of them,
and any result from Core that differs from the request by more than one signature per input under its key. Go does no
elliptic-curve arithmetic with its private key (internal/btc's point multiplication is not constant-time; QA4W2-1): from
the seed it computes only the hardened private steps to the account `m/48h/1h/0h/2h` (HMAC-SHA512 and an addition mod n)
and serializes the account key as a root. At every start Core's `getdescriptorinfo` of `wpkh(<account tprv>/0/*)` gives
the account's public key (checked against Go's chain code); to sign, Core gets the account key and the per-order path
inside the descriptor and derives the key itself. The seed's master key never leaves the process. The app pins the
signer's fingerprint and an HMAC of its account key at first contact; a different key makes shape (b) unavailable and
changes nothing else, and a signer that does not answer is "waiting". `escrow-signer backup` writes the seed encrypted
to the operator's OpenPGP key and `restore` reads it back; the [operator guide](operator-guide.md#escrow-signer) covers
set-up, the pins, backup, break-glass signing and the upgrade order. How it was tested: unit (BIP32 vectors through the
private path; every refusal, the result check and the happy path against a simulated Core; the socket protocol; init,
backup and restore), DB integration (the pins, a mismatch, a signer that is down, replies the app cannot use, against a
simulated signer), and on a real local chain, Core 31.1 on regtest (`TestEscrowRegtestSigner` in
`scripts/test-escrow-regtest.sh`: the account key Core computed agreeing with Core's own derivation from the seed's
master key; a per-order escrow co-signed through the socket, signed by the buyer's Core wallet, broadcast and mined; the
four refusals; backup, restore and an identical signature from the restored seed; a pin mismatch and a stopped signer;
no seed or tprv in the database, the signers' logs or the transcript). Not tested: the Compose service running (checked
with `docker compose config` only), break-glass signing on an offline node, any wallet other than Bitcoin Core, and any
public network.

## Per-order key layout, registration and practice (A-455, shape (b))

The third piece of per-order wallets (rulings P4-R2 and P4-R18, owner decision 2026-10-05), on the development branch
behind `ESCROW_BTC` (off by default, test networks only). When it was built no escrow used a per-order wallet: a checkout
where the buyer's or the vendor's active key was a per-order wallet key was refused. Since A-456a a buyer and a vendor
whose keys are of different kinds are told so, with the vendor's key type ("The vendor's Bitcoin escrow key is for a
per-order wallet and yours is for Bitcoin Core", or the reverse; ruling P4-R18), and a buyer whose own key is the
per-order one is told to register a Bitcoin Core escrow key and linked to the escrow key page. Since A-456b a pair of
per-order wallet keys gets a per-order escrow while the market's escrow signer is available
([below](#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b)); without it the order page says "Escrows with
per-order wallets need the market's escrow signer, which is not available on this market right now", and while Bitcoin
intake is escrow only (A-444) a listing whose vendor's active key is a per-order wallet key is then not available for
purchase in BTC, with that reason, and the readiness panel on `/admin` counts and lists the vendor.

- **Layout.** Each account still has one escrow key, now with a layout (`user_escrow_keys.layout`, migration 088):
  `core`, a Bitcoin Core wallet whose escrows use the account key's child `/0/<i>` (every key registered before), or
  `per_order`, a wallet made for each order from the account key's child `<i>`. The escrow key page asks which wallet
  the key comes from. A per-order wallet key must be the BIP48 multisig account key (`m/48h/1h/<account>h/2h`, a `tpub`
  or `Vpub`, parsed by the same strict parser). Registering the same key with the other layout is a change: the key is
  inactive until the practice transaction of the new layout is signed. A key that is the key of existing escrows
  cannot change layout. The page says plainly that Electrum's own seed type (Electrum's default, also on Tails) cannot
  make per-order wallets: a BIP39 seed in Electrum, or Sparrow, is needed. The registration copy says that a per-order
  key replaces the account's Bitcoin Core key, which (since A-456b, with any account whose key is not a per-order one too) stops Bitcoin escrow buying and, for a vendor under
  escrow-only intake, Bitcoin escrow sales; the pages say that BIP39 Electrum signing has been tested on regtest;
  Electrum GUI steps remain untested, while Sparrow GUI per-order signing and separate watch-only descriptor
  import/address matching have A-458 evidence; signing from the imported wallet and physical hardware remain
  untested ([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)). The
  practice page tells the owner never to send coins to the practice wallet's
  first receiving address. The database refuses an escrow key whose
  suffix has three or more steps (a per-order key, `/<i>/0/0`) starting with 0 or 1, for any party, and a buyer's or
  vendor's at the practice index 2^31-1; shape (a)'s two-step suffixes are not affected.
- **Practice (ES-9) for a per-order wallet key.** The practice escrow is the 2-of-3 of the owner's key at the reserved
  index (`account/2147483647/0/0`) and the `/0/0` children of two cosigner keys served rooted (depth 0, parent
  fingerprint 0, child 0, as `Vpub`): the server's throwaway key's child at that index and an unsignable key's. Go
  derives the cosigner keys; Core must derive the same address from the throwaway key's own path, or no practice is
  served. The page shows the derivation path for the practice wallet, the two cosigner keys, its first receiving
  address and the PSBT; the owner builds the practice wallet from the seed, checks the address, signs and pastes. Only
  the owner's signature is taken from the paste (the practice check never read derivation entries, so the entries
  Electrum writes for every key need no relaxation), and activation needs a signature from the registered key's
  per-order practice key: a signature from its Core practice child (`/0/0`) is refused. Its made-up outpoint is
  checked absent and `testmempoolaccept` must report missing-inputs, as for a Core key.
- **Key type on the listing and the vendor page.** While Bitcoin escrow is on, a listing and a vendor's profile name
  the vendor's active key type: Bitcoin Core, a per-order wallet, or none active.

How it was tested: DB integration with a simulated node that derives rooted keys by real BIP32 public derivation
(`TestEscrowKeyLayoutMigration`, `TestEscrowKeyLayoutRegistration`, `TestEscrowPerOrderPractice`,
`TestEscrowPerOrderPracticeCrossCheck`, `TestEscrowKeyLayoutSwitchNeedsPractice`, `TestEscrowKeyWithEscrowsKeepsLayout`,
`TestEscrowPracticeKeyChangedDuringCheck`, `TestEscrowPerOrderKeyNotUsedForCheckout`, `TestEscrowOnlyPerOrderVendorNotPurchasable`,
`TestEscrowVendorKeyTypeShown`); a browser journey with JavaScript off at 320 px against the simulated node
(`tests/e2e/escrow-per-order-journey.spec.ts`, `npm run test:e2e:escrow`; not chain evidence); and on a real local
chain, Core 31.1 on regtest (`TestEscrowRegtestPerOrderPractice` in `scripts/test-escrow-regtest.sh`: from a Core
wallet's seed standing in for a BIP39 seed and the practice page alone, the operator builds the per-order practice
wallet as a private descriptor, Core derives the address shown and signs the PSBT, and the signature activates the key;
the unsigned PSBT, the same seed's Core wallet holding the shape-(a) range and a SIGHASH_NONE signature are refused).
The manual A-457 journey in `scripts/test-escrow-wallets.sh` confirms both BIP39 Electrum parties' practice signatures
through the market. At A-455, Electrum GUI menus, Sparrow and hardware wallets were untested. Later A-458 evidence
records Sparrow GUI per-order practice and settlements plus a separate watch-only descriptor import/address match
on isolated regtest; signing from the imported wallet, physical devices and public Sparrow signing remain untested
([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)).

## Per-order escrow creation and watching (A-456a, shape (b))

The first half of per-order escrows (rulings P4-R2, P4-R17 and P4-R18), on the development branch behind `ESCROW_BTC`. It
was built behind a switch in the code that was off, because nothing could sign a per-order escrow's release until the
market co-signed it through the escrow signer; A-456b ([below](#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b))
removed the switch. What A-456a built:

- **Shape.** Checkout makes a Bitcoin Core escrow (shape (a)) when both parties' active keys are Bitcoin Core keys and a
  per-order escrow (shape (b)) when both are per-order wallet keys; a mixed pair is refused, naming the vendor's key type,
  whether or not a per-order escrow could be made.
- **Keys.** Each party's key is the first receiving key (`/0/0`) of an order wallet built on the account key's child
  `<i>`: the escrow key's derivation suffix is `/<i>/0/0`. `i` comes from the account's one counter, shared with the
  Bitcoin Core escrows' `/0/<i>` (one more than the highest either used, never below a restore's floor), and is never 0
  or 1 (the receiving and change branches, ancestors of other keys) nor the practice index 2^31-1. The market's key is the
  escrow signer's account key's child `<m>/0/0`, `m` from the market's counter, skipping 0 and 1; the signer's key is read
  from the signer at checkout and must match the pins (no signer, or a different one: refused, try again later). Every
  order wallet key (the account key's child `<i>`, as a tpub) is stored at checkout with its escrow key
  (`per_order_xpub`): the buyer's and the vendor's since QA5W4A-5, so the order wallet can be rebuilt after the party
  registers another key, and the market's (the signer's account key's child `<m>`) since QA5W5A-1, checked to have the
  pinned signer fingerprint and to be the child of the signer key read and checked at that checkout, so that nothing
  but co-signing needs the signer afterwards. Each is checked to give the stored `/0/0` key. This adds nothing the market
  did not already hold: each is derived from a registered (or the signer's pinned) public account key at an index the
  database records, and the watch-only wallet already holds it, rooted, in the escrow's ranged descriptors.
- **Cross-check.** Go derives every key by BIP32 public derivation (A-453) and Bitcoin Core must agree before anything is
  stored: the three keys Core puts in the escrow script, the address from the account keys' paths and from the rooted
  per-order keys, and all 30 addresses of the order wallet that the escrow covers. On any disagreement the checkout gets
  "the market's Bitcoin node did not confirm it … try again later" and nothing is written or imported. While the
  watch-only wallet is rescanning, Core refuses the checkout's import (of either kind of escrow): the checkout reads "the
  market's escrow wallet is rescanning the chain … try again in a few minutes", the order stays a draft and nothing is
  written (QA5W5A-2); the repair below starts such a rescan after the watch-only wallet is restored.
- **Watching.** With the escrow's own address (imported labelled, as in shape (a)), the watch-only wallet imports the
  order wallet's two ranged descriptors over the rooted per-order keys only, receiving `/0/0..19` and change `/1/0..9`,
  and each of the 29 sibling addresses labelled `escrow:<order ID>` (Core lists received coins only for labelled
  addresses, and refuses a label on a ranged descriptor), and the market stores the 29 sibling addresses
  (`escrow_siblings`, migration 089). A deposit to a sibling address is recorded for the order but never counts (left
  out): an order paid only at siblings is cancelled unpaid at the end of its window, and once the order is closed a stray
  refund of those coins to the buyer's refund address is requested (building, signing and co-signing a per-order
  escrow's settlements, the stray refund included, is A-456b). The orphan
  check does not list sibling addresses, and a sibling address is never accepted as a refund, claim or market fee
  address, as an escrow address is not (ES-5).
- **Watch-only wallet repair.** On the first watcher pass after each start the market asks the watch-only wallet whether
  it holds each per-order escrow's address and last sibling address (`/1/9`), each labelled with the escrow's order, and
  imports again what is missing (a watch-only wallet restored from before the escrow), from the escrow's creation time
  and in checkout's order: for a missing address, the escrow's own labelled descriptor, rebuilt from its stored keys (only
  when their 2-of-3 is the escrow's address); for a missing sibling, the order wallet's two ranged descriptors and then
  the 29 sibling addresses, rebuilt from the three order wallet keys stored at checkout (the market's too since
  QA5W5A-1, so the escrow signer is not asked), each checked to give the escrow's stored keys. The siblings are never imported without their ranged
  descriptors first: Core 31.1 keeps an address that an address-only import held first unsolvable for good, whatever is
  imported later (QA5W4A-1). Every escrow being repaired goes into one import, so Core rescans the chain once, from the
  earliest creation time, with up to two minutes for it (QA5W4A-3). The repair waits, and the next watcher pass tries
  again, while the watch-only wallet is already rescanning (an operator's `rescanblockchain`, or an earlier repair's
  import that Core is still running after the app stopped waiting for it: the app asks `getwalletinfo` first and never
  starts a second rescan). Stored keys that do not give the escrow's (or a market row with no stored order wallet key,
  which no escrow created since QA5W5A-1 has) leave the siblings out (logged); the next start tries again. Until
  QA5W5A-1 the repair waited for the escrow signer, whose key it needed for the market's part. Restart the app after
  restoring the watch-only wallet. Bitcoin Core escrows are not repaired this way.
- **Orphan sweep.** After a database restore, a lost per-order escrow's address and siblings are orphans; the sweep
  reserves their keys. An orphan that Core reports unsolvable (held by an address-only import) has no keys to read: it
  counts as read only as a lost per-order escrow's sibling, when another orphan with the same `escrow:<order ID>` label
  gave its three keys, and otherwise the sweep stays incomplete and escrow checkout stays paused (QA5W4A-2), since that
  escrow's keys could otherwise be handed out again.
- **The escrow signer's key is never a party's.** Registration refuses, in either layout ("This key cannot be used
  here"), a key whose origin fingerprint is the escrow signer's pinned fingerprint, or whose HMAC computed as the pin's is
  the pinned one (the signer's key serialized at any depth); a key registered before the signer's pins existed is refused
  at a per-order checkout with a 409 that says so. A key derived from the signer's (a descendant) cannot be detected this
  way: the practice signature, which only the holder of the signer's seed could make for such a key, is what stops it.
- **Database.** Migration 089 refuses any escrow key suffix with a hardened step or a step of 2^31 or more, for every
  party and both shapes (migration 074 allowed ten-digit steps, so `/2147483648/0/0`, the hardened `/0h/0/0`, passed
  migration 088, as did a shape-(a) `/0/2147483648`), and a per-order suffix that is not `/<i>/0/0` with
  `2 <= i < 2^31-1` (so the market never takes the practice index either); it lets the market's per-order key have an
  origin with no path (the signer's rooted key), allows `per_order_xpub` only on a per-order key (the buyer's, the
  vendor's or, since QA5W5A-1, the market's) and only as a tpub, and adds `escrow_siblings`. The restore floor (`scripts/restore.sh`,
  UPGRADING.md section 6) reads an account's highest index in both shapes.

How it was tested: DB integration with a simulated node that derives per-order keys by real BIP32 public derivation and
a simulated signer (`TestEscrowShapeBMixedPairRefused`, `TestEscrowShapeBSwitchOffRefuses`, `TestEscrowShapeBChosen`,
`TestEscrowShapeBSharedCounter`, `TestEscrowShapeBNeverPractice`, `TestEscrowShapeBCrossCheckMismatch`,
`TestEscrowShapeBNeedsSigner`, `TestEscrowShapeBSiblingDeposits`, `TestEscrowShapeBRestartReimport`,
`TestEscrowShapeBNoServedKeyIsAncestor`, `TestEscrowShapeBRestoreFloor`, `TestEscrowMigration089`,
`TestEscrowMigration089BadHistoricalRow`; after QA5W3A: `TestEscrowShapeBCrossCheckSibling`,
`TestEscrowShapeBSignerChainCodeChanged`, `TestEscrowShapeBSiblingNotRefundAddress`,
`TestEscrowShapeBSiblingNotClaimAddress`, `TestEscrowShapeBSiblingNotFeeAddress`,
`TestEscrowSignerKeyRefusedAtRegistration`, `TestEscrowShapeBSignerKeyAtCheckout`,
`TestEscrowShapeBLostAfterWatchRestore`, `TestEscrowShapeBWatchRestoredBeforeEscrow`,
`TestEscrowShapeBRepairEitherEnd`; after QA5W4A: `TestEscrowShapeBRepairOneRescan`, `TestEscrowShapeBRepairWaitsForRescan`,
`TestEscrowShapeBOrderXpubStored`, `TestEscrowShapeBRepairAfterKeyChange`, `TestEscrowShapeBRepairBareAddressCheck`,
`TestEscrowSweepLoneUnsolvableOrphanKeepsGate`; after QA5W5A: `TestEscrowShapeBRepairSignerDown` (the signer down,
answering garbage or with another key: one pass repairs everything), `TestEscrowShapeBRepairNoStoredMarketKey`,
`TestEscrowCheckoutWhileRescanning`; the simulated node reports solvability, and refuses an import while it
rescans, as Core 31.1 does), `scripts/test-upgrade.sh` (bad historical rows stop migration 089 with its message), and on a
real local chain, Core 31.1 on regtest and on a private signet (`scripts/test-escrow-regtest.sh`):
`TestEscrowRegtestPerOrderEscrow` (the escrow created through the checkout form, Core deriving its address
and sibling `/0/1` from the parties' private roots as Go does, and deposits to `/0/1` and `/1/3` seen and recorded left
out; its first run found that Core's `listreceivedbyaddress` lists only labelled addresses, so the siblings are also
imported labelled) and `TestEscrowRegtestPerOrderRepair` (the watch-only wallet repair onto empty wallets standing in for
ones restored from before the escrow: deposits made meanwhile found and every address solvable, with the signer up and,
since QA5W5A-1, with it down; after the vendor registers another key, the siblings still solvable; Core deriving each
stored order wallet key's `/0/0` as the account key's `/<i>/0/0`, the market's included; an address held by `addr(…)` first staying unsolvable after its descriptor is imported).
Not tested in that slice: signing, settling or refunding a per-order escrow (A-456b tests them, below), the orphan sweep of a lost per-order escrow on a
real node (simulated only), a repair rescan longer than one call on a real node, any wallet other than Bitcoin Core, and
any public network.

## Per-order escrow settlement, setup and upload (A-456b, shape (b))

The second half of per-order escrows (rulings P4-R2, P4-R13, P4-R17 and P4-R18), on the development branch behind
`ESCROW_BTC` (off by default, test networks only). A-456a's switch is gone: **a buyer and a vendor whose active escrow
keys are both per-order wallet keys get a per-order escrow at checkout**, while the market's escrow signer is configured
(`ESCROW_SIGNER_SOCKET`) and its latest check found its pinned key. Otherwise the order page says "Escrows with per-order
wallets need the market's escrow signer, which is not available on this market right now", checkout is refused with
nothing written and, under escrow-only Bitcoin intake, a per-order vendor's listings are not available in BTC. A mixed pair
is still refused.

- **The market co-signs through the escrow signer.** A per-order escrow's settlement (a claim, a refund, a stray refund)
  and its fee bumps are co-signed by `cmd/escrow-signer` (A-454), never the Core arbiter wallet, which keeps Bitcoin Core
  escrows. The signer's account key is asked for and checked against the pins first, and must be the one the escrow was
  built with. The request is the stored PSBT exactly as the build checked it (each input's witness UTXO and witness
  script, no derivation, no sighash field, one 2-of-3 script for every input) with the escrow's market index, which is
  what the signer's refusals allow; the signer itself is unchanged and keeps every refusal. Its answer is checked as the
  arbiter's is (exactly one signature per input, the market key's, SIGHASH_ALL), then combined, finalized, re-checked and
  `testmempoolaccept`ed as before. A signer that is not reachable, refuses, or no longer matches leaves the settlement
  signed by its party, with "The market's escrow signer could not co-sign it yet" on the order page and one log line
  (`signer-waiting` or `signer-refused`, the refusal's fixed class logged); nothing is rebuilt and the next pass tries
  again (P4-R17). The market contacts the signer only at checkout (its account key, from which the market's order
  wallet key is derived and stored), to co-sign, and in its health check (its account key, read at startup and on every
  watcher pass and checked against the pins); every per-order key a page, a copy, a paste or a build needs comes from
  what checkout stored, never from the signer (QA5W5A-1), so while the signer is down both parties' order
  pages, setup pages and copies are served and a party's signature is accepted, a sibling's stray refund included (QA5W5A
  found that a sibling's refund made the order page answer 500 then). Should a per-order key not be had (a row with no
  stored order wallet key, or stored data that does not give the address), no page, copy or form fails: the order page
  says "This transaction cannot be signed right now: the market cannot rebuild the keys of the address it spends from
  its records" and offers no copy, the copy and the sign forms answer 503 with that text, nothing stored, a party-signed
  settlement waits with `signer-waiting` and nothing rebuilt, and the log names the fixed class
  `per-order-key-unavailable`. One nobody signed yet records nothing (no party signed, so nothing waits for the signer):
  the watcher logs that class on each pass and still follows its inputs from the stored transaction, so a deposit spent
  outside the market makes it external as usual (QA5W6-1).
- **Sibling deposits are returned (QA5W3A-2).** A-456a recorded a deposit to an order wallet's sibling address and
  requested its stray refund, but the build waited for ever (the input was compared with the escrow address, and Core was
  given only the escrow address's descriptor). A sibling's three keys are now derived in Go from each per-order extended
  key (the parties' account child `<i>` and the signer's `<m>`, as checkout stored them: QA5W4A-5, the market's since
  QA5W5A-1) and must pay the sibling address checkout stored after Core derived it. A stray refund spends the deposits of one address at a time, with that address's own witness
  script: the signer signs one script per request, finding its key at `<m>/<branch>/<k>`. Addresses are tried in the
  order of their first deposit; one whose deposits cannot be built now (too few for their fee, above the fee cap, not as
  recorded on the node, or keys that cannot be derived) never holds up another's, and the next stray refund returns what
  the last left. Each party's copy of a sibling's
  stray refund names its own key at `/<i>/<branch>/<k>`, and the paste check, the co-signature and fee bumps use that
  sibling's keys.
- **The setup page.** `/escrow-setup?id=<order>`, linked from the order page, behind the same gate as
  `/escrow-descriptor` and party-only: the order's buyer and vendor each see their own setup, and staff, strangers, the
  other party's setup and a Bitcoin Core escrow get the unknown page. It shows the party's own key origin with the order's
  per-order index (`[fingerprint/48h/1h/0h/2h/<i>]`) and the derivation path (`m/48h/1h/0h/2h/<i>`); the two cosigner
  keys served rooted as SLIP-132 `Vpub`s (the other party's per-order key and the market's); the first receiving
  address the order wallet must show, the escrow address; and a Sparrow output descriptor of the order wallet
  (`wsh(sortedmulti(2,…/<0;1>/*))` with the party's key origin). The parties' per-order keys are the ones checkout stored
  (QA5W4A-5), each checked to give the key the escrow stores, so the page is the same after either party registers
  another escrow key; the market's is the one checkout stored too (QA5W5A-1), so the page is the same while the escrow
  signer is down. Only a row with no stored order wallet key (none since QA5W5A-1) leaves a key out, and the page says
  it cannot be read from the market's records. The copy says: pay only to the escrow address, never to another address the order
  wallet shows; Electrum can run with `--offline` (the order wallet only signs); delete the order wallet once the order's
  escrow has settled (the seed makes it again); a wallet or signing device cannot check the other party's key. The page
  and the order page say that BIP39 Electrum signed the release and sibling refund on regtest, while GUI menu steps,
  A-458 later recorded Sparrow GUI signing and separate watch-only import/address matching for the supplied descriptor;
  signing from that imported wallet and physical hardware remain untested.
- **The PSBT file upload.** The settlement sign form is a `multipart/form-data` form, no JavaScript, with a file field
  beside the paste (fee bumps keep the paste). A binary PSBT file, as Electrum and Sparrow save one, is taken by its
  magic bytes; a text file (the market's own download, signed) is read as base64. Every text part and a text file must
  be valid UTF-8 without a NUL byte, as every form field must; the body, read before Origin and CSRF are checked as any
  form's is, is bounded to one PSBT of at most 64 KB and the small fields (QA5W5A-5; no legitimate form carries two) and
  so is each field; a paste and a file together are refused; then every ES-2 check applies as to a
  paste. Every other route still refuses a multipart body with a 415 page, unread.
- **Hardware signing copy (A-459).** A buyer or vendor who may sign a settlement or its fee bump can download a separate
  binary `.psbt` file. It adds `non_witness_utxo` from the watch-only wallet's `gettransaction` hex for each input, after
  checking the funding transaction ID and spent output against the stored `witness_utxo`. A missing, malformed or
  mismatched funding transaction refuses the download with 503; the ordinary copy remains available. The only key
  origin in either copy is the reader's own. The separate hardware file forms use the same authorization and signature
  checks as ordinary signing, keep only signatures, and accept at most 8 MiB of PSBT data (128 KiB per funding
  transaction; at most 50 inputs). Ordinary paste and file limits remain 64 KiB. These are simulated-node and form
  checks. A-457's real Core journey checked that the vendor's release download carried the actual funding transaction
  as `non_witness_utxo` matching the funding transaction. A-458's later hardware-format PSBT upload was signed
  with a software key, and no physical hardware wallet has been tested.
- **The ES-2 derivation refusal is relaxed (P4-R13).** A pasted or uploaded PSBT is no longer refused for a derivation
  entry under the market's key or from its fingerprint (Electrum and Sparrow write one for every key; A-450's Electrum
  fixtures are now accepted). Every signature rule stays: a signature under the market's key, another key's signature, a
  sighash other than ALL, a finalized input and another transaction are refused, and only the party's signatures are
  kept.

How it was tested: DB integration with the simulated node and a fake signer that makes `cmd/escrow-signer`'s checks
before it signs (`TestEscrowShapeBCosignedBySigner`, `TestEscrowShapeBSignerWaits`, `TestEscrowShapeBBumpCosignedBySigner`,
`TestEscrowShapeBSiblingStrayRefund`, `TestEscrowShapeBStrayOneScriptEach`, `TestEscrowSettlementKeysOneScript`,
`TestEscrowShapeBSetupPage`, `TestEscrowShapeBSetupKeyChanged`, `TestEscrowSignUpload`,
`TestEscrowPasteMarketDerivationAccepted`, `TestEscrowPasteForeignFieldsInert`, `TestEscrowWalletFixtures`,
`TestEscrowShapeBOpenToUsers`, `TestEscrowShapeBMixedPairRefused`, `TestEscrowOnlyPerOrderVendorNeedsSigner`; after
QA5W5A: `TestEscrowShapeBSiblingStraySignerDown`, `TestEscrowShapeBPerOrderKeyUnavailable`, `TestEscrowShapeBMarketKeyCrossChecked`,
`TestEscrowShapeBSiblingRowMismatch`, `TestEscrowShapeBStrayDustSiblingDoesNotHold`,
`TestEscrowShapeBStraySignerDownEscrowAddressBuilds`, `TestEscrowShapeBSignerAnswerChecked`,
`TestEscrowShapeBMarketFingerprintDiffers`, `TestEscrowShapeBSignerMismatchUnavailable`);
a browser journey with JavaScript off at
320 px against the simulated node and a simulated signer (`tests/e2e/escrow-per-order-settle-journey.spec.ts`, `npm run
test:e2e:escrow`: registration and practice for both parties, checkout, both setup pages, simulated funding, the claim
signed and uploaded as a binary PSBT file, co-signed, confirmed; not chain evidence); and on a real local chain, Core 31.1
on regtest and on a private signet with the real signer built from the tree (`TestEscrowRegtestShapeBRelease`: a
per-order escrow created through checkout and paid, the vendor's claim signed by its order wallet, Core with the private
root at the per-order path, uploaded as a binary PSBT file, co-signed by the signer, broadcast and confirmed, checked with
`gettxout`; `TestEscrowRegtestShapeBSiblingStray`: a deposit to sibling `/0/3` returned by a stray refund with the
sibling's own script, signed at the buyer's `/0/3` key, co-signed at the signer's, confirmed, checked with `gettxout`;
`TestEscrowRegtestShapeBSiblingStraySignerDown`: a sibling `/0/2` deposit's stray refund shown with the sign form on both
order pages, downloaded and signed while the signer's socket is moved aside, signer-waiting with nothing rebuilt, then
co-signed and confirmed once it is back; `TestEscrowRegtestShapeBBump`: the vendor's claim broadcast at 1 sat/vB, bumped
to 5 sat/vB, the bump signed by the vendor's order wallet, co-signed by the signer, replacing the claim in the mempool
and confirmed; no seed or tprv in the signer's log or the database). A-457's `TestEscrowRegtestElectrumMarketJourney`
adds a real BIP39 Electrum release and sibling refund through the market and real signer, each broadcast and confirmed
on regtest, with a mixed pair refused. This A-457 journey did not test Sparrow, a physical hardware wallet, Electrum
  GUI menus or any public network. Later A-458 Sparrow GUI signing evidence covers practice, release and sibling
refund on isolated regtest; a separate descriptor import produced the expected receive address in a watch-only wallet.
Signing from that imported wallet, a physical device, fee bumps and public Sparrow signing remain untested
([record](evidence/sparrow/2026-10-11-f229a1a65172/README.md)).

Escrow chain evidence includes Bitcoin regtest, a private isolated Bitcoin signet (A-443, challenge `51`), a local Monero fakechain, an offline Monero stagenet fork (A-449), the **2026-10-09 public signet smoke** ([record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)), and one **2026-10-10 public stagenet digital claim** ([record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)). The public Bitcoin record covers Core practice signing, a funded digital claim and an underpaid buyer refund, with Tor proxy/private-broadcast observations. Neither public record exercises every physical-order, fee-bump, Electrum, Sparrow or hardware path on a public chain. Public testnet4 and public stagenet refunds remain unrun. Mainnet is never used in development or tests and escrow still refuses it.

### Sparrow and hardware-wallet evidence (A-458)

Sparrow 2.5.5 GUI per-order practice, release and sibling-refund signing now has partial evidence on isolated regtest;
no physical-device result has been recorded. A-459's hardware download and upload are implemented, but their database
tests do not establish device compatibility. Record each actual wallet/device combination
below after running it against an isolated, funded test-network escrow. Retain the transcript and the public
transaction IDs; keep seeds, private keys and wallet passwords out of the evidence.

**Ledger Flex preparation.** Record the installed device firmware version and the Bitcoin Test app version; use the
Bitcoin Test app for a test chain, not the Bitcoin mainnet app. Ledger's [Bitcoin app feature notes](https://github.com/LedgerHQ/app-bitcoin/blob/develop/doc/features.md)
say Bitcoin Test works on all Bitcoin test networks, including Core regtest; its [build configuration](https://github.com/LedgerHQ/app-bitcoin/blob/develop/Makefile)
includes Flex. Update the device firmware if Ledger Wallet does not offer the app; if Bitcoin Test is not listed,
Ledger's [test setup instructions](https://github.com/LedgerHQ/moosig) say to enable Developer Mode in
Ledger Wallet and install it from My Ledger. Sparrow's [connected-device guide](https://www.sparrowwallet.com/docs/connected-wallet.html)
describes USB connection and Ledger support. Use a USB data cable, unlock the Flex, open Bitcoin Test, and
close Ledger Wallet/Live before Sparrow connects so it does not hold the device. Keep the device in view and approve
each connection, wallet-policy registration, address check, and transaction only after reviewing the device prompts.
The signing flow is a normal PSBT file flow: download the hardware `.psbt` from the order, open/sign it in Sparrow,
then upload that signed file on the order page. USB carries the PSBT between Sparrow and the Ledger; the market does
not connect to the device, and HWI's Flex recognition is not proof that this Sparrow policy will work. In particular,
the per-order key path is `m/48h/1h/0h/2h/<i>/<branch>/<index>` (`<i>` is unhardened), with rooted cosigners and a
2-of-3 P2WSH policy. This path/policy combination still needs an actual Flex signing result. Ledger's Bitcoin Test
app displays SegWit addresses with `tb1` even on regtest, where Core displays `bcrt1`; that prefix difference is
expected, but is not evidence that two addresses match. Compare their decoded output scripts against the independently
verified expected address, and reject any signing prompt whose amount, fee, or destination cannot be reconciled.
Never enter or share a seed or PIN for this test.

On 2026-10-11, after restarting Sparrow, the Flex was detected unlocked over USB. With the Bitcoin mainnet app open,
a test-path xpub request returned `Request not currently supported`; the device showed no prompt and produced no
signature. This is not a signing result. Use the Bitcoin Test app before evaluating the test-chain path.

On 2026-10-09, a controlled Sparrow 2.5.5 ARM64 retry verified authenticated Core RPC through the desktop SSH tunnel. After mining 101 blocks on a separate isolated regtest node, Sparrow connected at height 101 and its GUI remained responsive. The prior hung test instance and the retry resources were stopped, with process exits checked. The verified release and private test profile were retained locally. No PSBT was loaded or signed; this connection check does not complete any signing row below.

| Wallet / device model | Wallet version / device firmware | Network and date | Rooted cosigners and descriptor import | Address matches / registration limits / path warning | Ordinary versus hardware PSBT acceptance | Signing-screen amount, fee and destination | Practice, release and sibling-refund result | Evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Sparrow GUI with software signing key | Sparrow 2.5.5 ARM64 | Isolated Core 31.1 regtest, 2026-10-11 | Rooted cosigners worked via native wallet JSON. Separately, File → Import Wallet → Output Descriptor → Import File accepted the checksummed descriptor with origins and saved a watch-only 2-of-3 P2WSH wallet; the party key path was `m/48'/1'/0'/2'/2`, cosigners were rooted at `m`, and no path warning appeared. Receive address `bcrt1qy25ngwmvy3kxwc9q792lq0fkgczysku9rxegxyadcvy57zhsj9xq0fnawc` matched the funded escrow. This import did not sign. | Buyer/vendor practice accepted; outputs independently matched Core | Ordinary release PSBT upload accepted; hardware-format sibling-refund PSBT uploaded and signed with software key | Release: 99,682 sat, 318 sat fee, 3 confirmations; refund: 69,682 sat, 318 sat fee, 3 confirmations; all outputs matched Core | Buyer/vendor practice accepted; per-order index 2 release and index 3 `/0/3` sibling refund confirmed | [A-458 record](evidence/sparrow/2026-10-11-f229a1a65172/README.md); descriptor import was watch-only; no device used |
| Ledger Flex | Firmware and Bitcoin Test app versions pending | USB probe, 2026-10-11; mainnet Bitcoin app | Unrun | Device detected unlocked; test-path xpub returned `Request not currently supported` | No PSBT/signature attempted | No device prompt | No test-chain signing; Bitcoin Test app still required | No device-signing evidence; Bitcoin Test not yet tested |

Import the party-only setup page's order descriptor and compare the first address with the escrow address before
funding. Record whether the wallet accepts the rooted cosigners and the full per-order path, including any warning
or registration limit. Try the ordinary PSBT and the hardware copy separately to establish whether that combination
requires `non_witness_utxo`. Record what the device actually displays when signing, then upload through the matching
form, confirm the release and the sibling refund on the test chain, and retain any refusal verbatim after removing
secrets. A signing device cannot authenticate the other party's key. Claim compatibility only for a recorded row.
The funded signing journey used native wallet JSON; the separate supplied-descriptor import was watch-only and did
not prove key attachment or signing from that wallet. No physical Ledger Flex signed. Fee bumps and public Sparrow
signing also remain untested; the hardware-format PSBT was signed with a software key. Flex evidence remains pending
the Bitcoin Test app device test described above.
