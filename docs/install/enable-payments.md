# Enabling payments

Turn on Bitcoin escrow with the local node the installer started. A new market takes no payments until you do.

| Coin | Escrow | Networks | Setup |
| --- | --- | --- | --- |
| Bitcoin | 2-of-3 multisig per order: buyer, vendor, market | signet (default), testnet4, regtest | This page |
| Monero | Custodial: market holds the keys | stagenet, local fakechain | [Monero escrow](../operator-guide.md#monero-escrow), or `--monero-escrow yes` at install |

> **Warning:** this page is for **test networks**. Test coins have no value. Mainnet moves real funds, needs a new
> database and has no recorded run: see [Mainnet](../operator-guide.md#mainnet).

## Before you begin

- Installed with a local Bitcoin node (`--nodes local`, or `--nodes custom` with `--bitcoin local`). Your own
  node instead? See [Escrow node set-up](../operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).
- The node has synced. Regtest: instant. Signet and testnet4: hours.
- Run everything from the repository directory.

Examples use signet. On testnet4 or regtest, change `--chain signet`. The installer printed these same commands
with your network filled in.

## Step 1: Create wallets and RPC users

Prints `bitcoin.conf` and `.env` lines **once**. Copy the output.

```sh
./scripts/escrow-node-setup.sh --chain signet --rpc-port 8332 --signer \
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

## Step 4: Restart and verify the node

Use the `ESCROW_BTC_RPC_URL` from step 1:

```sh
docker compose restart bitcoin
ESCROW_BTC_RPC_URL='THE_PRINTED_URL' ./scripts/escrow-node-setup.sh --chain signet --verify --via-compose app --wait 120
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

`/admin` shows the escrow status.

## Step 7: Back up the escrow wallets

[Back up the arbiter and watch-only wallets](../testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets).

> **Note:** CI runs these commands on a throwaway regtest install (`escrow-node-rehearsal`). That proves the
> commands, not a signet or testnet4 node.

## What now?

- [Bitcoin escrow](../operator-guide.md#bitcoin-escrow): keys, signing, limits
- [Per-order escrow](../escrow.md): what escrow protects
- [Production checklist](production-checklist.md)
