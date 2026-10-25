# OPSEC Market documentation

Self-hosted marketplace software that needs no browser JavaScript. Go renders every page as HTML and CSS,
PostgreSQL stores the data and users identify with PGP. Production uses separate application and upload hosts,
a remote database, and Tor or HTTPS ingress.

> **Prerelease:** `v0.1.0-alpha.2`. The installer's local nodes default to **mainnet (real funds)**; test networks are
> an option. Mainnet payment and settlement workflows remain unverified; deployment and readiness checks do not
> establish real-funds settlement. See
> [Implementation status](implementation-status.md).

## Start here

| I want to | Go to |
| --- | --- |
| See it in one minute | [Read-only preview](install/preview.md) |
| Try it on my machine | [Local marketplace](install/local.md) |
| Deploy a production market | [Production deployment with managed PostgreSQL](install/production.md) |
| Restrict server access and protect stored data | [Server hardening](install/server-hardening.md) |
| Try a single-host deployment | [Onion service](install/tor.md) or [Public domain](install/clearnet.md) |
| Take payments | [Enabling payments](install/enable-payments.md) |
| Check launch readiness | [Production checklist](install/production-checklist.md) |
| Upgrade | [Upgrading](../UPGRADING.md) |
| Fix a problem | [Troubleshooting](install/troubleshooting.md) |

## Sections

- **Get started**: install, first steps, payments.
- **Operate**: [operator guide](operator-guide.md), [security and OPSEC](security-operations.md), [retention operations](retention.md), [data retention and privacy policy](data-retention-privacy-policy.md), upgrades, backups, escrow runbooks.
- **Reference**: [installer](install/installer-reference.md), [escrow design](escrow.md), status and evidence.
- **Contribute**: [development](development.md), tests, [contributing](../CONTRIBUTING.md).

## You should know

- A Linux shell, Docker and Docker Compose.
- Tor Browser (onion installs) or an HTTPS reverse proxy (public domain).
- A dedicated Monero escrow wallet-rpc, to take payments.

For the existing deployment, the [storage security assessment](install/storage-security-assessment.md)
and [production security review](install/security-review.md) record measured gaps.
Use the hardening guide as the target policy,
then record verified outcomes before marking the production checklist complete.
