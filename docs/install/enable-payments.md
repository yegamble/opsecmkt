# Enabling Monero payments

This market accepts Monero (XMR) only. Payments use a custodial account for each order: the market holds the wallet keys and can move its funds. A node alone does not enable escrow.

Monero conceals sender, recipient and amount details on-chain. The market still sees order and payment metadata; Tor and Monero do not guarantee anonymity or protect a compromised host.

## Choose a network

Choose at installation. Native deployment defaults to **stagenet** when escrow is enabled; the legacy shell installer defaults bundled nodes to **mainnet**. A market that has pinned its escrow network cannot switch networks. Upgrades keep the existing choice.

For native production, pass `--monero-network stagenet` for rehearsal or `--monero-network mainnet` for real funds. The table below describes the **legacy shell installer**:

| Network | Installer option | Configuration |
| --- | --- | --- |
| Mainnet (default) | `--nodes local` | `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=` |
| Public test network | `--nodes local --monero-network stagenet` | `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet` |
| No bundled node | `--nodes none` or `--local` | Configure an external daemon separately if needed |

Mainnet moves real funds. Mainnet payment and settlement workflows remain unverified; deployment and readiness checks do not establish real-funds settlement. Rehearse on stagenet first. Local fakechain tests run separately from installer deployments. Monero testnet is unsupported.

## Native production with separate payment services

Configure payments during the single fresh initialization in the [production deployment procedure](production.md#4-initialize-and-start-the-application). Prepare these services and files **before** that step:

1. Run a Monero daemon and a dedicated escrow wallet RPC on a payment host separate from the application, database and upload gateway. Use the same explicit network on both, and start the wallet RPC with `--daemon-address` naming the same daemon the application reaches through its daemon URL. Keep wallet files in persistent protected storage.
2. Expose the wallet and restricted daemon RPC through authenticated HTTPS on the private network. Allow only the application host. Use separate credentials or the supported mutual-TLS client identity; verify certificates against the endpoint names.
3. Save both authenticated HTTPS origins in separate regular mode-`0600` files. Use `https://USER:URL_ENCODED_PASSWORD@PAYMENT_HOST:PORT`, without `/json_rpc`; the application appends each RPC path. Copy the public issuing CA to the application inputs directory.
4. Generate the [seed-recovery OpenPGP pair offline](binary.md#importing-and-generating-keys), keep the private half off every runtime host and provide only its armored public half. Prepare separate encrypted wallet-file, database and private-installation backups.
5. For Tor deployments, configure and check [payment-host Tor egress](../operator-guide.md#declaring-separate-payment-host-tor-egress). The application cannot inspect the remote daemon's network route.

Append this complete escrow flag block to the **same** fresh `deploy init --mode prod` command in the production guide, before running it. Replace paths with the prepared input files:

```text
--escrow-xmr
--monero-network stagenet
--monero-wallet-rpc-url-file /srv/opsecmkt-inputs/wallet.url
--monero-daemon-url-file /srv/opsecmkt-inputs/daemon.url
--monero-rpc-ca /srv/opsecmkt-inputs/monero-ca.pem
--monero-seed-key /srv/opsecmkt-inputs/escrow-public.asc
```

For a deliberate real-funds deployment, replace `stagenet` with `mainnet` in that block and on both remote services. For production Tor with checked remote Tor egress, also add `--set ESCROW_XMR_TOR_RELAY=remote-tor`. It records an operator declaration; it does not verify Tor delivery. Without an explicit relay choice, native production Tor holds settlement relay.

When RPC authentication uses a client certificate instead of URL credentials, also import `--monero-rpc-cert CLIENT.pem --monero-rpc-key CLIENT.key`. Keep that private key mode `0600`. Native initialization generates a stable `MONERO_WALLET_SECRET` when escrow is enabled unless you import one using `--monero-wallet-secret-file FILE`; preserve it through every rebuild and recovery.

Run `deploy plan`, `deploy install` and `deploy status` as shown in the production guide. A healthy application does not establish payment readiness. Keep intake paused until the daemon and wallet finish synchronization and the administrator readiness panel accepts the endpoints, network, wallet secret and public seed key. Then set **New escrow checkouts** to **Escrow** in Admin. Initial mainnet synchronization may continue long after infrastructure and application health checks succeed.

Retain encrypted copies of the private installation, database, payment wallet files, active ingress identities and current external retention ledger. Keep the seed-decryption private key separately offline. A managed database snapshot does not contain wallet files or the application's wallet secret. Rehearse restoration, chain reconciliation and the recovery gates on stagenet before relying on recovery for real funds; follow the [wallet recovery runbook](../testnet-runbook.md#back-up-and-restore-monero-escrow-wallets).

A managed-database point-in-time recovery or snapshot restore skips the payment restore gate that `scripts/restore.sh` sets. Keep the application stopped with `./opsecmkt deploy down --dir DIR` (or the service manager of a directly supervised `opsecmkt serve`), not Compose commands, and apply the protection in [UPGRADING.md section 6](../../UPGRADING.md#6-backups-now-need-the-wallets-too) before its first start on that database. Then follow the [reconciliation procedure](../testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts).

An already initialized native installation without escrow needs a deliberately reviewed configuration migration. The native controller currently has no command that imports all remote escrow material into an existing installation. Do not rerun `init`, delete its directory, replace its application keys or connect a new independent installation to a live custodial wallet RPC as a shortcut.

## Legacy shell installer

The remaining examples use `scripts/install.sh` and its single-host bundled services. They do not establish the separate host boundaries required by the native production profile.

## Before you begin

- Meet the [installation requirements](requirements.md) and run from the repository directory.
- Prepare an armored OpenPGP public key in a file outside the checkout. Keep its private half offline: escrow wallet seeds are encrypted to this key.
- Choose a fresh database for each network, and keep `MONERO_WALLET_SECRET` and wallet backups encrypted.

## Install with escrow enabled

For a test market:

```sh
./scripts/install.sh init --tor --nodes local --monero-network stagenet \
  --monero-escrow yes --monero-escrow-seed-key /ABSOLUTE/PATH/OPERATOR_PUBLIC_KEY.asc
```

Omit `--monero-network stagenet` only when deliberately choosing mainnet with real funds. The installer writes the dedicated wallet-rpc configuration, seed-key mount and secret. It starts the `monero` and `monero-escrow-wallet` profiles; the wallet-rpc has its own volume and login.

For an existing local install, follow [Monero escrow configuration](../operator-guide.md#monero-escrow). Do not reuse the obsolete pooled-wallet settings.

## Check readiness and Tor relay

Run `./scripts/install.sh check` and open **Admin → Escrow status and payment intake**. The daemon must sync and the wallet-rpc, network pin, wallet secret and seed key must pass before checkout is available. Set **XMR intake** to **Escrow** when ready; pausing intake prevents new checkouts.

In Tor mode, add the [local nodes over Tor overlay](../operator-guide.md#local-nodes-over-tor) before relaying payments. It is explicit, not automatic. Until the overlay is configured, Monero settlements are built and stored but relay is held. The escape setting `ESCROW_XMR_TOR_RELAY=clearnet-accepted` deliberately accepts direct network exposure. Monero delivery through Tor has not been verified here.

## Back up and verify

Back up `.env`, the database, the `monero_escrow_wallet` volume and the offline seed-decryption key separately. Rehearse [Monero wallet recovery](../testnet-runbook.md) on test networks. An encrypted database backup does not include wallet files.

The recorded [public stagenet claim](../evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) proves one digital claim. Public refunds, physical-order deadlines and mainnet payment and settlement workflows remain unverified. See [verification](../verification.md) for evidence limits.
