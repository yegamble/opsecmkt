# OPSEC Market documentation

Self-hosted marketplace software that needs no browser JavaScript. Go renders every page as HTML and CSS,
PostgreSQL stores the data, users identify with PGP, and one Docker Compose install runs it all, as a Tor onion
service or behind HTTPS.

> **Prerelease:** `v0.1.0-alpha.2`. The installer's local nodes default to **mainnet (real funds)**; test networks are
> an option. No mainnet run is recorded; mainnet is covered by unit, database and simulated-RPC tests only. See
> [Implementation status](implementation-status.md).

## Start here

| I want to | Go to |
| --- | --- |
| See it in one minute | [Read-only preview](install/preview.md) |
| Try it on my machine | [Local marketplace](install/local.md) |
| Run a market | [Onion service](install/tor.md) or [Public domain](install/clearnet.md) |
| Take payments | [Enabling payments](install/enable-payments.md) |
| Go live | [Production checklist](install/production-checklist.md) |
| Upgrade | [Upgrading](../UPGRADING.md) |
| Fix a problem | [Troubleshooting](install/troubleshooting.md) |

## Sections

- **Get started**: install, first steps, payments.
- **Operate**: [operator guide](operator-guide.md), [security and OPSEC](security-operations.md), upgrades, backups, escrow runbooks.
- **Reference**: [installer](install/installer-reference.md), [escrow design](escrow.md), status and evidence.
- **Contribute**: [development](development.md), tests, [contributing](../CONTRIBUTING.md).

## You should know

- A Linux shell, Docker and Docker Compose.
- Tor Browser (onion installs) or an HTTPS reverse proxy (public domain).
- Bitcoin or Monero wallets, to take payments.
