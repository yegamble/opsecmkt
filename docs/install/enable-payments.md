# Enabling payments

Turn on Bitcoin escrow with the local node the installer started. A new market takes no payments until you do.

| Coin | Escrow | Networks | Setup |
| --- | --- | --- | --- |
| Bitcoin | 2-of-3 multisig per order: buyer, vendor, market | mainnet (installer default), signet, testnet4, regtest | This page |
| Monero | Custodial: market holds the keys | mainnet (installer default), stagenet, local fakechain | [Monero escrow](../operator-guide.md#monero-escrow), or `--monero-escrow yes` at install |

> **Warning:** the installer's local nodes default to **mainnet, which moves real funds**. No mainnet run is recorded;
> mainnet is covered by unit, database and simulated-RPC tests only (see [Mainnet](../operator-guide.md#mainnet)).
> Rehearse on signet and stagenet first. Test coins have no value; mainnet coins do, and a mainnet market needs its own
> database.

## Choose a network

Decide when you install. The installer cannot change a network later, and upgrading never does.

| You want | Install with | Writes to `.env` |
| --- | --- | --- |
| Mainnet (default) | `./scripts/install.sh init --tor --nodes local` | `BITCOIN_CHAIN=main`, `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=`, `BITCOIN_PRUNE_MB=15000`, `PAYMENT_CONFIRMATIONS_BTC=6` |
| Test networks | add `--bitcoin-network signet` (or `testnet4`, `regtest`) and `--monero-network stagenet` | `BITCOIN_CHAIN=signet`, `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet`, `BITCOIN_PRUNE_MB=2000` |

No mainnet run is recorded; mainnet is covered by unit, database and simulated-RPC tests only.

- **One option carries the other.** Name a test network for one coin and the other coin defaults to its test network
  (`signet`, `stagenet`). One of the two options is enough.
- **Conflicts are refused.** `--bitcoin-network main` with `--monero-network stagenet` (or the reverse) stops before
  anything is written: a market is all mainnet or all test networks.
- **Names are strict.** Bitcoin says `main`, Monero says `mainnet`. Bitcoin `mainnet`, `testnet` and `testnet3`, and
  Monero `main` and `testnet`, are refused with the right name.
- **Images are digest-pinned.** The node images carry their tag's `@sha256` digest.
- **A blank network is a test network.** An `.env` with blank `BITCOIN_CHAIN` runs signet, and blank `MONERO_NETWORK`
  runs stagenet.

Before it writes `.env`, a mainnet install prints the real-funds notice, the disk estimates below and the Monero escrow
note. The disk check only warns.

| Node | Mainnet pruned (estimate) | Test networks (estimate) |
| --- | --- | --- |
| Bitcoin | ~30 GB, first sync takes days. 15000 MiB of recent blocks, about two months | signet ~8 GB, testnet4 ~5 GB, hours |
| Monero | 45 GB or more (45 GB in April 2022 per Monero's README, more today), days | stagenet ~20 GB, hours |

A full Bitcoin mainnet node is about 856 GB of blocks + 14 GB of chain state (Bitcoin Core v31.1 chainparams
estimates). More: [Requirements](requirements.md#disk-for-nodes).

> **Note:** with the 15000 MiB window, a wallet backup older than about two months cannot be rescanned for deposits
> (an estimate). Take [wallet backups](../testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)
> often.

## Before you begin

- Installed with a local Bitcoin node (`--nodes local`, or `--nodes custom` with `--bitcoin local`). Your own
  node instead? See [Escrow node set-up](../operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).
- The node has synced. Mainnet: days. Signet and testnet4: hours. Regtest: instant.
- Mainnet only: a database no test market has used, and a new arbiter wallet, watch-only wallet and signer seed. Never
  reuse a test market's.
- Run everything from the repository directory.

Examples use mainnet (`--chain main`). On a test network, change `--chain main` to `--chain signet`, `--chain testnet4`
or `--chain regtest` in steps 1 and 4. The installer printed these same commands with your network filled in.

## Step 1: Create wallets and RPC users

Prints `bitcoin.conf` and `.env` lines **once**. Copy the output. Bitcoin Core names mainnet `main`; `--chain mainnet`
is refused.

```sh
./scripts/escrow-node-setup.sh --chain main --rpc-port 8332 --signer \
  --cli 'docker compose exec -T -u bitcoin bitcoin bitcoin-cli -datadir=/data -rpcport=8332'
```

## Step 2: Add the RPC users to the node

Paste the printed `rpcauth`, `rpcwhitelistdefault` and `rpcwhitelist` lines between the `EOF` markers:

```sh
docker compose exec -T -u bitcoin bitcoin sh -c 'cat >> /data/bitcoin.conf' <<'EOF'
# paste the printed rpcauth, rpcwhitelistdefault and rpcwhitelist lines here
EOF
```

Keep the quotes on `'EOF'`. They stop the shell expanding the `$` in `rpcauth`.

## Step 3: Add the escrow settings to .env

Paste the printed `ESCROW_*` lines:

```sh
cat >> .env <<'EOF'
ESCROW_BTC=on
COMPOSE_PROFILES=internal-db,bitcoin,escrow-signer
# paste the printed ESCROW_* lines here
EOF
```

Use the `COMPOSE_PROFILES` value the installer printed: your current value plus `,escrow-signer`.

On mainnet, also decide these before you start:

| Setting | Mainnet |
| --- | --- |
| `ESCROW_BTC_FEE_ADDRESS` | A **plain address is refused**. With a market fee above 0, give the fee wallet's watch-only receiving descriptor, `wpkh([fingerprint/path]xpub/0/*)`. Never a test wallet's. |
| `ESCROW_BTC_FEE_RATE` | Its default was sized on test chains. Review it against current fees. |
| `PAYMENT_CONFIRMATIONS_BTC` | The installer wrote `6`. |

Details: [Mainnet](../operator-guide.md#mainnet).

## Step 4: Restart and verify the node

Use the `ESCROW_BTC_RPC_URL` from step 1:

```sh
docker compose restart bitcoin
ESCROW_BTC_RPC_URL='THE_PRINTED_URL' ./scripts/escrow-node-setup.sh --chain main --verify --via-compose app --wait 120
```

Expected: `PASS`.

## Step 5: Create the signer seed

```sh
docker compose run --rm --no-deps escrow-signer init
```

Write down the fingerprint. Back up the seed: [Escrow signer](../operator-guide.md#escrow-signer). The seed lives
only in its own volume, not in `.env` or database backups.

## Step 6: Start with escrow on

```sh
docker compose up -d
```

Expected in `docker compose logs app`:

```text
escrow: Bitcoin escrow on
escrow: escrow signer ready (shape (b); its account key matches the pins in this database)
```

`/admin` shows the escrow status. On mainnet, the market's pages say MAINNET.

## Step 7: Back up the escrow wallets

[Back up the arbiter and watch-only wallets](../testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets).
The runbook was written for test networks and has not been run on mainnet.

> **Note:** CI runs these commands on a throwaway regtest install (`escrow-node-rehearsal`). That proves the
> commands, not a mainnet, signet or testnet4 node.

## Monero escrow on mainnet

Off by default. It is **custodial with real funds**: the market holds every escrow wallet's keys. Turning it on at
install needs `--monero-escrow yes` and `--monero-escrow-seed-key FILE`, the OpenPGP public key whose private half you
keep offline. No mainnet run is recorded; mainnet is covered by unit, database and simulated-RPC tests only.
See [Monero escrow](../operator-guide.md#monero-escrow).

## What now?

- [Bitcoin escrow](../operator-guide.md#bitcoin-escrow): keys, signing, limits
- [Per-order escrow](../escrow.md): what escrow protects
- [Production checklist](production-checklist.md)
