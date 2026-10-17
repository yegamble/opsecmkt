# OPSEC Market documentation

Self-hosted marketplace software that needs no browser JavaScript. Go renders HTML and CSS, PostgreSQL stores
marketplace data, users identify with PGP, and payments use Monero. The standalone Go CLI supports guided setup,
explicit flags, key tools and application/worker roles. Development may use one host; production separates services.

> **Prerelease software.** The native CLI changes are unreleased; build the current checkout to use them.
> Native setup defaults to stagenet with escrow disabled. The legacy shell installer's bundled nodes default to
> **mainnet (real funds)**. Mainnet payment and settlement workflows remain unverified; deployment and readiness checks do not
> establish real-funds settlement. See [Implementation status](implementation-status.md).

## Start here

| I want to | Go to |
| --- | --- |
| Set up using guided prompts or flags | [Standalone binary](install/binary.md) and [complete CLI reference](install/cli-reference.md) |
| See it in one minute | [Read-only preview](install/preview.md) |
| Try a single-host development deployment | [Native development setup](install/binary.md#initializing-a-development-deployment) |
| Prepare production services | [Service separation, sizing and worker reuse](install/service-topology.md) |
| Configure Tor and onion keys | [Public and private administrator onions](install/tor.md) |
| Use the legacy shell installer | [Local marketplace](install/local.md) or [public domain](install/clearnet.md) |
| Deploy a production market | [Production deployment with managed PostgreSQL](install/production.md) |
| Restrict server access and protect stored data | [Server hardening](install/server-hardening.md) |
| Take payments | [Enabling payments](install/enable-payments.md) |
| Check launch readiness | [Production checklist](install/production-checklist.md) |
| Upgrade | [Upgrading](../UPGRADING.md) |
| Fix a problem | [Troubleshooting](install/troubleshooting.md) |

## Sections

- **Get started**: [native setup](install/binary.md), [installation methods](install/README.md), first steps, payments.
- **Operate**: [operator guide](operator-guide.md), [security and OPSEC](security-operations.md), [retention operations](retention.md), [data retention and privacy policy](data-retention-privacy-policy.md), upgrades, backups, escrow runbooks.
- **Reference**: [CLI](install/cli-reference.md), [legacy installer](install/installer-reference.md), [escrow design](escrow.md), status and evidence.
- **Contribute**: [development](development.md), tests, [contributing](../CONTRIBUTING.md).

## You should know

- A Linux shell; Docker and Compose 2.17+ for managed deployments, or prepared PostgreSQL for direct serving.
- Tor Browser (onion installs) or an HTTPS reverse proxy (public domain).
- A dedicated Monero escrow wallet-rpc, to take payments.
- Prepared remote services and TLS identities for production; the CLI does not provision those hosts.

The [deployment checkpoint](install/deployment-notes.md#evidence-and-remaining-gates),
[storage assessment](install/storage-security-assessment.md) and
[production security review](install/security-review.md) separate measured results,
historical findings and open checks. Follow the hardening guide, then record
actual outcomes before marking the production checklist complete.
