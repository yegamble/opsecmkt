# Upgrading from v0.1.0-alpha.1

This release adds test-network payments, signed audit exports and several database migrations. Read this
whole page before pulling the new code onto a running v0.1.0-alpha.1 deployment.

## 1. Take a backup and keep the old image

Migrations are **one-way**. There are no down-migrations, and the alpha.1 image cannot run against a
migrated database (for example, migration 001 replaces `orders.status` with `orders.state` and drops the old
unique constraint). Before upgrading:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/pre-upgrade.dump.age
docker image tag opsecmkt-app opsecmkt-app:v0.1.0-alpha.1   # or note the image ID you run today
```

Keep both until you have run the new version for a while. See [Rolling back](#5-rolling-back).

## 2. Add the new keys to your existing `.env`

`scripts/install.sh` refuses to touch an existing `.env`, so it will not add them for you. Add the lines
below with your editor (values in single quotes, as the installer writes them). Blank values keep a feature
off.

| Key | What to put there |
| --- | --- |
| `AUDIT_SIGNING_KEY` | `openssl rand -hex 32` output. Enables signed audit exports; back it up with `.env`. |
| `BITCOIN_CHAIN` | Leave blank to accept whichever test chain the node reports, or `testnet4` / `signet` / `regtest`. Mainnet is always refused. See step 3 before setting it for a local node. |
| `BITCOIN_WALLET` | Wallet name, default `opsecmkt`. |
| `MONERO_NETWORK` | Blank, `stagenet` or `testnet`. |
| `MONERO_WALLET_RPC_URL` | monero-wallet-rpc URL with credentials, for example `http://marketplace:PASSWORD@monero-wallet:18083`. Blank keeps Monero payments off. A daemon URL alone (`MONERO_RPC_URL`, which alpha.1 wrote) does **not** enable them. |
| `MONERO_WALLET_RPC_PASSWORD` | For the local `monero-wallet` service only: `openssl rand -hex 24` output, the same value as the password in `MONERO_WALLET_RPC_URL`. The service refuses to start without it. |
| `PAYMENT_CONFIRMATIONS_BTC` / `PAYMENT_CONFIRMATIONS_XMR` | Defaults 3 and 10. |
| `PAYMENT_POLL_INTERVAL` / `PAYMENT_EXPIRY` | Defaults `30s` and `24h`. |

For a local Monero node, also add `monero-wallet` to `COMPOSE_PROFILES` (for example
`COMPOSE_PROFILES='internal-db,monero,monero-wallet'`).

## 3. `BITCOIN_RPC_URL` now turns payments on

In alpha.1 the installer wrote `BITCOIN_RPC_URL` but nothing read it. Now a non-empty value enables Bitcoin
test-network payments. The app never starts against a mainnet node: startup exits with
`refusing to start: bitcoin chain "main" ...`. A node that is unreachable, still syncing or has no wallet
loaded no longer stops the site; Bitcoin simply shows as unavailable on the admin page until it works.

- **External mainnet node**: blank `BITCOIN_RPC_URL`, or point it at a testnet4/signet/regtest node.
- **Local node from alpha.1**: the old `bitcoin_data` volume holds **mainnet** data (alpha.1 ran `bitcoind`
  without `-chain`). The node service now runs `-chain=${BITCOIN_CHAIN:-testnet4}`. Either:
  - blank `BITCOIN_RPC_URL` and remove `bitcoin` from `COMPOSE_PROFILES` to keep Bitcoin off; or
  - set `BITCOIN_CHAIN='testnet4'` and start from a fresh volume. The mainnet block data is of no use to
    the marketplace and only wastes disk. If you ever kept a wallet on that volume, copy `/data/wallets`
    out first. Then:

    ```sh
    docker compose stop bitcoin && docker compose rm -f bitcoin
    docker volume rm opsecmkt_bitcoin_data
    ```

  Then create the test wallet as described in [docs/testnet-runbook.md](docs/testnet-runbook.md).
- **Local Monero node from alpha.1**: its `monero_data` volume is mainnet data too; the service now runs
  `--${MONERO_NETWORK:-stagenet}`. Remove the old volume the same way (`opsecmkt_monero_data`), add the
  `monero-wallet` profile and the keys above, then follow the runbook.

## 4. Deploy

```sh
git pull   # or check out the release tag
docker compose config --quiet   # fails loudly on a malformed .env
docker compose up -d --build
docker compose logs -f app      # migrations run at startup under an advisory lock
```

Then open the admin page. **Payment providers** shows each currency as Enabled, Unavailable (with the
error; retried every poll), Refused (not a test network) or Disabled. The container stop grace period is now
60 seconds so a redeploy does not interrupt a payout that is being broadcast.

## 5. Rolling back

Rolling back is **not** an image swap. Because migrations are one-way, the old image fails against the
upgraded database. To roll back:

1. Stop the app: `docker compose stop app`.
2. Restore the pre-upgrade backup into a new, empty database with `scripts/restore.sh` (see the README).
   Everything written after the upgrade is lost.
3. Point `DATABASE_URL` at the restored database, check out `v0.1.0-alpha.1` (its compose files and code)
   and run `docker compose up -d --build`, or run the image you tagged in step 1.

## 6. Backups now need the wallets too

Once payments are on, the marketplace is custodial for test coins: the Bitcoin wallet lives in the
`bitcoin_data` volume and the Monero wallet in `monero_wallet`. **Database dumps do not include them.** Back
them up separately (see the runbook). `scripts/restore.sh` now holds every payout that the dump shows as
queued or sending, with the error "Restored from backup: verify in the wallet before releasing"; nothing is
re-sent until an administrator checks the wallet and releases, requeues or marks each one on the admin page.
If the script reports that it could not hold them, do not start the application on that database until you
have run the same update by hand:

```sql
UPDATE payouts SET state='held', updated=now(),
  error='Restored from backup: verify in the wallet before releasing; this payout may already have been sent.'
WHERE state IN ('pending','sending');
```
