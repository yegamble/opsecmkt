# Per-order escrow (planned)

> [!WARNING]
> **Not implemented yet.** No release contains escrow. Today every deposit sits in the operator's
> pooled custodial wallet for its currency ([operator guide](operator-guide.md#payments),
> [implementation status](implementation-status.md#p5-payments)). This page describes the escrow design
> the project owner has chosen, what it will and will not protect, and how it will ship. Parts marked
> *being designed* are still under review. When a phase ships, this page, the [README](../README.md), the
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
| Monero | One wallet per order on the server now (still custodial, see [Monero](#monero-one-wallet-per-order)); 2-of-3 Monero multisig is evaluated later. |
| Pooled mode | Kept, with safety fixes only, until escrow ships and is verified; then removed, with an upgrade path for orders still open. |
| Digital (automatic-delivery) orders | Once the escrow is funded, the content is delivered and the order completes. The vendor claims the funds whenever they like by signing; the market co-signs that claim automatically. |
| Network fee | Taken from the escrowed amount on every settlement, refunds included. |
| Market fee | Taken from a fulfilled order's escrow and sent to an address the administrator sets per currency. |
| Physical orders | **Open question.** Release when the vendor marks the order shipped (market co-signs), or when the buyer confirms receipt (buyer signs), with a timeout and dispute as the backstop. |

## Bitcoin: 2-of-3 multisig per order

**Keys.** Buyer and vendor each register an extended public key, with its key origin (fingerprint and
derivation path), exported from their own wallet. The market has one arbitrator key. Each order will get a
fresh address from those three keys:

```text
wsh(sortedmulti(2, <buyer key>, <vendor key>, <market key>))
```

Any two of the three keys can spend; no single key can. The server will refuse duplicate keys, the market's
own key offered as a party key and a key already used by another account, because a descriptor that repeats a
key lets one party spend alone (shown on regtest).

**Checking the address.** The order page will show the full descriptor with its checksum, so the buyer can
load it into their own wallet and see the same address before paying. The server will import each order's
descriptor into a watch-only wallet before it shows the address.

**Signing without JavaScript.** The server will hold no buyer or vendor private key. For each settlement it
will build the transaction itself as an unsigned PSBT (BIP 174): only that order's escrow as input, the
expected outputs, an explicit fee rate and a fee cap. The party downloads or copies the PSBT text, signs it
in their own wallet and pastes the signed PSBT into a form on the order page. The market will sign only a
PSBT whose transaction is the one it built and stored, adding its signature to its own copy; a pasted PSBT
that changes anything will be refused. (Bitcoin Core signs any PSBT it is given, so this check is what keeps
the market key from signing an attacker's transaction.)

**Who signs each outcome** (*being designed* where noted):

| Outcome | Signers |
| --- | --- |
| Digital order: vendor claims | Vendor, then the market automatically |
| Physical order: release | Open question (see [Owner decisions](#owner-decisions-2026-09-29)) |
| Buyer and vendor agree on a release or refund | Buyer and vendor; the market is not needed |
| Refund (order cancelled) | Buyer and market |
| Dispute decided for the buyer | Buyer and market |
| Dispute decided for the vendor | Vendor and market |
| Timeouts: vendor never ships, buyer never confirms, a party never signs | *Being designed* |

In a dispute an administrator reviews the order and decides; the market key then co-signs with the party
who won. (Today moderators can also resolve disputes; whether they keep that power with escrow is part of the
design.) Buyer and vendor keep talking on the order page, as now.

**Fees.** Every settlement will pay its network fee from the escrow, so the recipient receives the escrowed
amount minus that fee. A fulfilled order's settlement will also have a market-fee output to the
administrator's address. The fee rate, minimum amounts and what happens to amounts too small to send are *being designed*.

**What this will protect.** The server's watch-only wallet and its one key cannot move an escrow alone. A
seized or copied server holds the market key and the public descriptors, not the funds.

**What it will not protect.**

- The market key plus either party's key can spend. An operator who colludes with (or is) a vendor or buyer
  can take that order's funds; the arbitrator is trusted to that extent.
- Because the market co-signs digital-order claims automatically, the market key used for them is on the
  server. Someone who takes over the server and also controls a vendor account can claim that vendor's
  escrows.
- A party who loses their key and never signs leaves settlement to the other party and the market.
- The other party knows the escrow address, so they can see on the blockchain which coins funded it and
  where it was paid. Pay from coins you do not mind the vendor linking to you.
- Your registered extended public key lets the server derive every address of that wallet account, so it
  can link your market payments to anything else that account received. Use a wallet (or account) made only
  for this market.
- Wallet compatibility is not yet proven: Bitcoin Core was tested on regtest; Sparrow, Electrum and hardware
  wallets have not been.

## Monero: one wallet per order

Each order will get its own Monero wallet on the server. The buyer pays into it, and it is settled in one
transaction to the recipient (plus the market-fee output on a fulfilled order), with the network fee taken
from the amount. Monero requires 10 confirmations before received funds can be spent, so a settlement can
be sent only after that (about 20 minutes on mainnet).

**This is still custodial.** The server will hold every order wallet's keys, so the operator, or anyone who takes
over the server, can spend them. Per-order wallets keep orders apart and remove the pooled balance; they do not
stop the operator. Monero 2-of-3 multisig is wallet-level and experimental, and is evaluated later.

## Until escrow ships

The pooled wallet keeps working exactly as documented in the [operator guide](operator-guide.md#payments)
and the [test-network runbook](testnet-runbook.md). It receives safety fixes only. After escrow is verified
on regtest and a public test network, pooled mode is removed, with an upgrade path for orders still open at
the upgrade.

## Phases

The phases, and what each must prove before the next starts, are being set by the project's design review
(`.claude/war-room/ledger.md`, A-237). This list is updated as each phase is ruled and shipped.

| Phase | Scope | Status |
| --- | --- | --- |
| Design | Data model, order states, key registration, PSBT validation, timeouts, fees, Monero wallet handling, upgrade path | In review |

All escrow testing uses regtest and public test networks only. Mainnet stays an operator opt-in and is never
used in development or tests.
