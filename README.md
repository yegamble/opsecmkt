<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/logo-light.svg">
  <img alt="[OPSECMKT]" src="docs/assets/logo-light.svg" width="420">
</picture>

### A self-hosted marketplace that needs no browser JavaScript

Server-rendered Go and PostgreSQL, PGP identity, Monero payments, and a standalone deployment CLI.

[![CI](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml)

[Documentation](docs/README.md) · [Install](#install) · [CLI reference](docs/install/cli-reference.md) · [Production](#production-deployment) · [Status](docs/implementation-status.md) · [Contributing](CONTRIBUTING.md)

</div>

> **Prerelease software.** New installations take **no payments** until escrow is configured and enabled.
> The native CLI defaults to **stagenet**; the legacy shell installer's bundled nodes default to **mainnet
> (real funds)**. No mainnet run is recorded; mainnet is covered by unit, database and simulated-RPC tests only.
> Tor protects inbound connections; it does not guarantee anonymity. See [Payments](#payments) before enabling escrow.

## Why OPSEC Market

Built for a visitor on Tor Browser with scripts off. Development can use one host;
production separates the app, database, payment services and upload gateway.

- **Nothing runs in the browser.** Plain HTML and CSS, a strict Content Security Policy, no third-party assets.
- **Identity is PGP.** Sign in with a PGP challenge or TOTP. Messages are PGP ciphertext the server cannot read.
- **Guided or automated deployment.** One Go binary provides setup, application and worker roles, key tools,
  and Docker Compose lifecycle commands. Keep dependency hosts and keys through application rebuilds.
- **Administrator-only operations status.** Service-isolation notices and signed dependency-version reports,
  alongside signed audit exports and a canary.

Not yet tried in Tor Browser: the [manual pass](docs/verification.md#manual-tor-browser-pass-owner) is pending.

## Install

The **`opsecmkt` binary** embeds the UI and database migrations. Copy it to the server and run it from any
directory; a source checkout and Go compiler are unnecessary on the target server. PostgreSQL, Monero, Tor and
ClamAV remain separate services.

**The native CLI changes are unreleased; build this checkout to use them.** Go 1.27.2 is recommended; the module
minimum is 1.26.9, with automatic toolchain selection using the pinned version in `go.mod`.
See [development requirements](docs/development.md#run-from-source-with-a-local-database).

```sh
go build -trimpath -o opsecmkt ./cmd/server
./opsecmkt
```

For tagged releases, the release workflow builds Linux amd64 and arm64 binaries with `SHA256SUMS` after CI and
attaches them to a draft release. When those assets are published, verify the checksum before running the matching
binary. Older release assets do not include this CLI. See the [binary installation guide](docs/install/binary.md).

With no arguments in a terminal, guided setup opens automatically. It walks through development or production
settings, existing services, and key generation or import. `?` explains a question; `quit` or Ctrl-C cancels.
Before saving, review the equivalent command and generated/imported key paths. Saving configuration and starting
services are separate confirmations, both defaulting to **no**. The guide prints what to back up and how to
complete administrator setup. Secrets are supplied through private files rather than pasted into prompts.

Development defaults to loopback HTTP, stagenet and disabled escrow. Managed deployments need **Docker Engine,
Compose 2.17+ and reviewed images pinned by digest**. Production also needs prepared dependency hosts and TLS
identities; the CLI does not provision remote servers or install their services.

For a members-only site with registration closed, add
`--site-access members --registrations closed` to `deploy init` or `serve`.
The defaults are `public` and `open`. Administrators can later change them
independently without restarting under **Admin → Site access and registration**.
See [site access and registration](docs/operator-guide.md#site-access-and-registration).

| Task | Guide | Command |
| --- | --- | --- |
| Configure a deployment, manage it, or prepare onion keys | [Guided setup](docs/install/cli-reference.md#guided-interactive-setup) | `./opsecmkt setup` |
| Look around without a database | [Read-only preview](docs/install/preview.md) | `./opsecmkt serve --preview` |
| Run the app against an existing PostgreSQL service | [Direct serving](docs/install/binary.md#running-directly-with-flags) | `./opsecmkt serve --help` |
| Automate deployment with explicit flags | [Complete CLI reference](docs/install/cli-reference.md) | `./opsecmkt deploy init --help` |
| Generate or import application, TLS, recovery or onion keys | [Key commands](docs/install/cli-reference.md#generating-and-importing-keys) | `./opsecmkt keys help` |

Automatic prompting requires terminal input and output. Use explicit `serve` under a service manager and flagged
commands for automation. Headless no-argument startup retains environment-configured serving.

The shell installer remains available for [local](docs/install/local.md), [legacy Tor](docs/install/tor.md#legacy-single-host-checkout-walkthrough)
and [clearnet](docs/install/clearnet.md) checkouts. Some backup and recovery procedures still use scripts; see the
[Bash-to-Go migration matrix](docs/install/cli-reference.md#moving-from-bash-to-go).

## Production deployment

Use separate operating-system hosts for the following roles. Containers on one Docker engine share its kernel
and daemon authority; they are suitable for single-host development but do not establish production host separation.

| Role | Runs |
| --- | --- |
| Application | Marketplace and private upload backend; client-authorized administrator ingress is required for production Tor |
| Database | PostgreSQL and its persistent database/WAL storage |
| Payments | `monerod`, dedicated `monero-wallet-rpc` and authenticated RPC front end |
| Upload/scanner | Public gateway, ClamAV, and public Tor or HTTPS ingress |
| Version worker | `opsecmkt versions-worker`, with its own report-signing key and version inventory |

The production profile rejects known local or matching configured service hosts and requires verified TLS
and authenticated Monero RPC. Uploads use a separate gateway over mutual TLS. Hostnames cannot prove physical separation;
verify the actual placement and firewall rules. Raw uploads are scanned and normalized on the gateway host
before the app accepts bounded JPEGs for photos/avatars, or bounded JPEG/restricted SVG for logos.
The gateway handles browser sessions and remains a trusted component.

For Tor, use distinct public and administrator onions. Public ingress cannot access administrator routes;
the private administrator onion requires Tor v3 client authorization. Generate service identities or import the
full address and matching native public/secret key files. Optional vanity prefixes accept up to six lowercase
Base32 characters; a long search can be cancelled. See [Tor setup and identity backups](docs/install/tor.md).
Complete production Tor administrator setup on that private onion using the printed token-file location.
Private administrator photo/avatar uploads are currently unavailable with isolated uploads.
Logo uploads use a separate confirmed handoff; see [market branding](docs/operator-guide.md#changing-the-market-name-and-logo).

Only administrators see service-isolation warnings and dependency status for PostgreSQL, Monero daemon/wallet RPC,
Tor, ClamAV and Go. The separate version worker checks upstream releases and signs reports; the app verifies a pinned
public key and warns about newer releases, unknown versions and stale reports. This does not install updates or
certify that an installed version is secure. Installed versions come from operator inventory or limited observations
by the app. Configure the worker and its inventory using the
[worker deployment guide](docs/install/service-topology.md#version-worker-host).

Retain the private installation directory, stable keys, endpoints and retention state. `deploy up` resumes the
same installation; `deploy redeploy` replaces only the app image while preserving workers and their configuration.
Review migrations and verify backups before upgrading. Follow the
[host sizing, network and redeployment guide](docs/install/service-topology.md) for the complete procedure.
Optional [RAM secret loading and manual incident commands](docs/install/ram-secrets-and-emergency.md)
have separate recovery requirements; Tor identity volumes and other persistent copies need their own protection.

## Features

| For | What they get |
| --- | --- |
| **Buyers** | Search and filters, vendor profiles, order tracking, disputes, reviews tied to paid orders |
| **Vendors** | Physical and digital listings with up to 4 photos, stock, automatic digital delivery, an order desk |
| **Moderators** | Dispute desk, payment-review flags, photo and avatar takedowns |
| **Administrators** | Market name and logo, site access and registration, roles, suspensions, second-factor resets, takedowns, CAPTCHA, payment intake, escrow recovery, canary, signed audit exports, OPSEC and dependency notices |
| **Operators** | Guided or flagged setup, generated/imported keys, separated services, retained workers through app rebuilds, encrypted backups |

Photo uploads require ClamAV. Development can use integrated scanning; production requires the separate
upload/scanner gateway. ClamAV checks uploads for known malware; it does not review picture content.

## Payments

Monero only. Monero conceals sender, recipient and amount details on-chain. That reduces public blockchain exposure; it does not guarantee anonymity. The market still sees order and payment metadata, controls custodial escrow keys, and can be exposed by a compromised host or traffic correlation.

Per-order escrow, off by default. A node alone does not turn it on.

| Coin | Model | Networks | Evidence |
| --- | --- | --- | --- |
| Monero | Custodial: market holds keys | stagenet (native default), fakechain (CI/test harness), mainnet | CI on fakechain; one public stagenet claim [recorded](docs/evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md). Mainnet unrun. |

Only Monero (XMR) is supported for listing prices, checkout and settlement. Monero testnet is refused.

**Network selection.** Native setup defaults to stagenet; choose mainnet explicitly for real funds. The legacy
`scripts/install.sh init --tor` and `init --clearnet` local nodes default to mainnet; use
`--monero-network stagenet` for a test market, or `--local` for no nodes. No mainnet run is recorded; mainnet is
covered by unit, database and simulated-RPC tests only. Monero escrow on mainnet is custodial with real funds.
Upgrading never changes an existing install's network ([UPGRADING](UPGRADING.md#64-new-installs-default-to-mainnet-a-523)). Full evidence:
[Verification](docs/verification.md) and [Per-order escrow](docs/escrow.md).

## How it works

- In production, a browser reaches the public gateway through Tor or HTTPS; the gateway proxies ordinary requests to the Go app.
- On production deployments, raw uploads stay on the separate gateway/scanner host; the app receives normalized images over mutual TLS.
- The server renders every page, checks CSRF and origin on every form, and audits each action in the same
  transaction.
- PostgreSQL holds marketplace data; the application applies migrations on startup.
- Escrow, when on, uses a dedicated Monero escrow wallet RPC. The market controls the keys.

**What the server keeps.** Opt-in [retention maintenance](docs/retention.md) expires accounts, messages, orders and audit records, with scoped preservation and financial safeguards. It is off by default; operators must enable it and schedule expiry of recovery copies. The operator or anyone with a
decrypted backup can read retained plaintext and metadata and obtain ciphertext; properly recipient-encrypted content
requires a recipient's private key to decrypt. Shipping addresses are sent PGP-encrypted to the vendor, but an address typed into a plaintext field is not
protected. Reviews, dispute reasons and order notes are plaintext. Manual digital delivery accepts plaintext or pasted
ciphertext; automatic delivery content is server-readable. Photos are re-encoded JPEGs without metadata. Full list:
[what the server keeps](docs/implementation-status.md#what-the-server-keeps).
The [data retention and private information policy](docs/data-retention-privacy-policy.md) defines expiry, preservation
exceptions and recipient-encryption requirements. Database retention and backup/export expiry tools are implemented; recipient-encryption enforcement for remaining private fields is pending.

**Account recovery.** No password-reset email. Recovery codes replace a lost authenticator. Administrators are
reset from the host ([Authentication](docs/operator-guide.md#authentication)).

## Documentation

Start at the [documentation home](docs/README.md). To browse it as a site, see
[Documentation site](docs/development.md#documentation-site).

| Guide | Covers |
| --- | --- |
| [Standalone binary](docs/install/binary.md) | Guided setup, direct serving, imported/generated keys and deployment commands |
| [CLI reference](docs/install/cli-reference.md) | Every command and flag, defaults, examples and remaining Bash procedures |
| [Service topology and sizing](docs/install/service-topology.md) | Host roles, starting resource budgets, network boundaries and worker reuse |
| [Tor and onion identities](docs/install/tor.md) | Separate public/admin services, client authorization and native key import/generation |
| [RAM secrets and incident response](docs/install/ram-secrets-and-emergency.md) | Encrypted offline bundles, tmpfs unlock, targeted manual shutdown and recovery limits |
| [Installation guide](docs/install/README.md) | Methods, requirements, first steps, payments, troubleshooting |
| [Operator guide](docs/operator-guide.md) | HTTPS and Tor, mirrors, configuration, backups, recovery |
| [Security and OPSEC](docs/security-operations.md) | Private admin ingress, offline export keys, deployment packages and compromise limits |
| [Retention operations](docs/retention.md) | Enable expiry, manage holds, expire copies and restore current deletion state |
| [Data retention and privacy policy](docs/data-retention-privacy-policy.md) | Retention deadlines, preservation and operator access; recipient-encryption requirements |
| [Upgrading](UPGRADING.md) | Upgrading from v0.1.0-alpha.1 |
| [Per-order escrow](docs/escrow.md) | Escrow design, guarantees and limits |
| [Test-network payments](docs/testnet-runbook.md) | Escrow wallet backups, restores, the recovery gate |
| [Implementation status](docs/implementation-status.md) | What works, what is missing, what is stored |
| [Development](docs/development.md) | Running from source and every test suite |

## Project layout

| Path | Contents |
| --- | --- |
| `cmd/server/` | Standalone CLI, application/worker entry points, guided setup, preview and host tools |
| `cmd/verify-audit/` | Offline verifier for signed audit exports |
| `internal/interactive/` | Guided setup and existing-installation prompts |
| `internal/operator/` | Private deployment configuration, Compose lifecycle and app-only redeployment |
| `internal/keymaterial/`, `internal/onionidentity/` | Application, TLS, OpenPGP and native Tor identity tools |
| `internal/dependencies/` | Signed dependency-release reports and upstream version polling |
| `internal/secretfile/`, `internal/ramsecrets/`, `internal/emergency/` | Private secret loading, encrypted bundles, tmpfs and explicit incident operations |
| `internal/market/` | Marketplace logic, database access, Go tests |
| `internal/market/migrations/` | Ordered SQL migrations. Add new files; never rewrite merged ones. |
| `web/` | Templates and CSS embedded in the binary |
| `deploy/` | Tor image, `torrc`, `clamd.conf` |
| `scripts/` | Legacy installer, backup, restore and remaining operations checks |
| `tests/` | Playwright journeys and installer tests |
| `docs/` | Documentation and assets |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/development.md](docs/development.md).

## License

AGPL-3.0 ([LICENSE](LICENSE)). Report vulnerabilities via [SECURITY.md](SECURITY.md).
