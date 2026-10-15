# Installing OPSEC Market

Pick a method, follow its page, then do the [first steps](first-steps.md).

> **Prerelease:** `v0.1.0-alpha.2`. A new install takes **no payments** until you set up escrow. Its local nodes
> default to **mainnet (real funds)**; test networks are an option. No mainnet run is recorded; mainnet is covered by
> unit, database and simulated-RPC tests only. See [Implementation status](../implementation-status.md).

## Choose a method

| Method | Use it to | Address | Time |
| --- | --- | --- | --- |
| [Read-only preview](preview.md) | Look around with sample data | `127.0.0.1:8080` | 1 min |
| [Local marketplace](local.md) | Try the full app on your machine | `127.0.0.1:8080` | 5 min |
| [Onion service](tor.md) | Run a market over Tor (recommended) | `.onion` | 10 min + node sync |
| [Public domain](clearnet.md) | Run a market behind your HTTPS proxy | `https://YOUR-DOMAIN` | 15 min + node sync |

New to OPSEC Market? Start with **local**. Running a real market? Use **onion service**.

## Mainnet or test networks

`init --tor` and `init --clearnet` with local nodes (the default) write mainnet. No mainnet run is recorded; mainnet is
covered by unit, database and simulated-RPC tests only.

| You want | Pass | Writes |
| --- | --- | --- |
| Mainnet (default) | nothing | `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=` |
| Test networks | `--monero-network stagenet` | `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet` |
| No nodes | `--nodes none`, or `--local` | Nothing for payments |

Details and the other options: [Enabling payments](enable-payments.md#choose-a-network). Upgrading never changes an
existing install's network ([UPGRADING](../../UPGRADING.md#64-new-installs-default-to-mainnet-a-523)).

> **Note:** nodes cannot be added after install. To take payments with a bundled node, choose them when you
> install with Tor or public domain.

## The path

1. [Requirements](requirements.md)
2. Install with your method's page
3. [First steps](first-steps.md): administrator, health check, backups
4. [Enabling payments](enable-payments.md) (optional)
5. [Production checklist](production-checklist.md) before inviting users

## Reference

- [Installer reference](installer-reference.md)
- [External PostgreSQL](external-database.md)
- [Troubleshooting](troubleshooting.md)
- [Upgrading](../../UPGRADING.md)
