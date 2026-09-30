# Per-order escrow (planned)

> [!WARNING]
> **Not implemented yet.** No release contains escrow. Today every deposit sits in the operator's
> pooled custodial wallet for its currency ([operator guide](operator-guide.md#payments),
> [implementation status](implementation-status.md#p5-payments)). This page describes the escrow design
> the project owner and its design review have settled, what it will and will not protect, and how it will
> ship. When a phase ships, this page, the [README](../README.md), the
> [implementation status](implementation-status.md), the [operator guide](operator-guide.md), the
> [test-network runbook](testnet-runbook.md), the [acceptance checks](acceptance.md) and
> [UPGRADING](../UPGRADING.md) are updated in the same change. Until a phase is listed as shipped under
> [Phases](#phases), treat everything here as a plan.

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
will be refused. (Bitcoin Core signs any PSBT it is given, so this check is what keeps the market key from
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
| 1 | Bitcoin escrow for digital orders on regtest, behind a setting that is off by default | In progress on the development branch, not released, behind `ESCROW_BTC` (off by default, regtest only; [operator guide](operator-guide.md#bitcoin-escrow-regtest-only)): escrow key registration and practice signing; escrow checkout for digital listings with automatic delivery (refund address pinned, market fee fixed at checkout), the escrow built and checked with Bitcoin Core and imported watch-only before its address is shown, the descriptor on the order page, the order completed on funding, and a deposit after an escrow order closed flagged for payment review (it stays in the escrow). Checked against Bitcoin Core 31.1 on regtest. The vendor's claim, refunds and dispute decisions (settlements) are not built, so escrowed coins cannot yet be moved through the market |
| Wallets | Electrum and Sparrow support (a signing test showed Electrum needs a different key layout) | Not started |
| 2 | Physical orders: buyer-signed release, 14-day deadline, no-ship cancellation | Not started |
| 3 | Monero accounts in monthly escrow wallets | Not started |
| 4 | Public test networks end to end, then escrow-only for new orders, then removal of the pooled wallet | Not started |

All escrow testing uses regtest and public test networks only. Mainnet stays an operator opt-in and is never
used in development or tests.
