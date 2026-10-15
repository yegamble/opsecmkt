# Enabling Monero payments

This market accepts Monero (XMR) only. Payments use a custodial account for each order: the market holds the wallet keys and can move its funds. A node alone does not enable escrow.

Monero conceals sender, recipient and amount details on-chain. The market still sees order and payment metadata; Tor and Monero do not guarantee anonymity or protect a compromised host.

## Choose a network

Choose at installation. The installer keeps the chosen network on upgrade.

| Network | Installer option | Configuration |
| --- | --- | --- |
| Mainnet (default) | `--nodes local` | `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=` |
| Public test network | `--nodes local --monero-network stagenet` | `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet` |
| No bundled node | `--nodes none` or `--local` | Configure an external daemon separately if needed |

Mainnet moves real funds. No mainnet run is recorded; unit, database and simulated-RPC checks are the available mainnet evidence. Rehearse on stagenet first. Local fakechain tests run separately from installer deployments. Monero testnet is unsupported.

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

The recorded [public stagenet claim](../evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) proves one digital claim. Public refunds, physical-order deadlines and mainnet remain unverified. See [verification](../verification.md) for evidence limits.
