<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/logo-light.svg">
  <img alt="[OPSECMKT]" src="docs/assets/logo-light.svg" width="420">
</picture>

### A self-hosted marketplace that needs no browser JavaScript

Server-rendered Go and PostgreSQL, PGP identity, Tor-first deployment.

[![CI](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml)

[Documentation](docs/README.md) · [Install](#install) · [How it works](#how-it-works) · [Status](docs/implementation-status.md) · [Contributing](CONTRIBUTING.md)

</div>

> **Prerelease (`v0.1.0-alpha.2`).** A new install takes **no payments** until you
> [enable escrow](docs/install/enable-payments.md). `init --tor` and `init --clearnet` run local nodes on **mainnet
> (real funds)** unless you name a test network; `--local` runs no nodes. Mainnet payment and settlement workflows
> remain unverified; deployment and readiness checks do not establish real-funds settlement. Tor protects inbound
> connections; it does not guarantee anonymity.

## Why OPSEC Market

Built for a visitor on Tor Browser with scripts off. Development can use one host;
production separates the app, managed or self-hosted database, payment services and upload gateway.

- **Nothing runs in the browser.** Plain HTML and CSS, a strict Content Security Policy, no third-party assets.
- **Identity is PGP.** Sign in with a PGP challenge or TOTP. Messages are PGP ciphertext the server cannot read.
- **The operator owns the instance.** Native deployment tools for onion or clearnet, with signed audit exports and
  a canary.

Not yet tried in Tor Browser: the [manual pass](docs/verification.md#manual-tor-browser-pass-owner) is pending.

## Install

For a production instance, follow [Deploying a production instance](docs/install/production.md).
It walks through separate application and upload hosts, managed PostgreSQL,
private TLS, Tor or HTTPS ingress, first-run setup, backups and recovery.
Complete [server hardening](docs/install/server-hardening.md) before launch:
approve exact operator/service source addresses, verify both firewall layers,
and install Linux with an independently unlocked LUKS2-encrypted root before
copying production credentials or creating keys. Native production setup verifies
the local host's encrypted storage before writing installation state, then pins
its LUKS identities for later starts. The Go tool can prepare an additional new
data disk; converting an existing root requires a planned migration.
The [storage assessment](docs/install/storage-security-assessment.md) records
the original deployment's encryption gaps and retained-copy exposure; deployment
health does not establish protection from physical access.
The [security review](docs/install/security-review.md) separates verified
perimeter controls from the hardening work still required.
After encrypted-host migration and a real manual-unlock rehearsal, operators can
enable the private administrator's [application-host OS reboot control](docs/operator-guide.md#enabling-the-private-application-host-reboot-control).
It requires a current password and enrolled authenticator, records the request
before dispatch, and leaves disk unlocking with the operator. The deployed
agent passed its current-boot acknowledgement after the real unlock rehearsal.
Owner sign-in, MFA and the full browser button journey remain pending; see the
[verified deployment checkpoint](docs/install/deployment-notes.md#evidence-and-remaining-gates).

The native **`opsecmkt` binary** includes the UI and provides `serve`, `keys`,
`upload-gateway` and `deploy` commands. It can run from any directory. Build it
with `go build -o opsecmkt ./cmd/server`, then run `./opsecmkt` in a terminal for
guided setup, or `./opsecmkt --help` for explicit commands. `./opsecmkt setup`
selects the guide explicitly. It previews actions, confirms writes and explains
which keys were generated or imported and what to back up.
The [binary installation guide](docs/install/binary.md) covers generated or
imported keys, single-host development and separated production services.
Docker deployments still require Docker Compose and reviewed image digests.
The existing shell installer remains available for compatibility.

| Method | Use it to | Command |
| --- | --- | --- |
| [Production with managed PostgreSQL](docs/install/production.md) | Deploy separate production services with Tor or HTTPS | `./opsecmkt deploy init --mode prod ...` |
| [Native guided setup](docs/install/cli-reference.md#guided-interactive-setup) | Configure a deployment, generate/import onion keys, or manage an existing installation | `./opsecmkt` |
| [Read-only preview](docs/install/preview.md) | Look around. Needs only Go 1.26.9+. | `go run ./cmd/server -preview` |
| [Local marketplace](docs/install/local.md) | Try the full app on `127.0.0.1` | `./scripts/install.sh --local` |
| [Onion service](docs/install/tor.md) | Run a market over Tor (recommended) | `./scripts/install.sh init --tor` |
| [Public domain](docs/install/clearnet.md) | Run a market behind HTTPS | `./scripts/install.sh init --clearnet` |

The source-checkout walkthrough below needs Git, Docker with Compose v2
(2.17+ for Tor) and OpenSSL. Use a
[release tag](https://github.com/yegamble/opsecmkt/releases), not `main`:

```sh
git clone https://github.com/yegamble/opsecmkt.git && cd opsecmkt && git checkout v0.1.0-alpha.2
./scripts/install.sh --local
```

Open the printed setup link, paste the token, create the administrator.

> **Warning:** the local nodes of `init --tor` and `init --clearnet` default to **mainnet**. Mainnet payment and
> settlement workflows remain unverified. For a test market add
> `--monero-network stagenet` ([why](docs/install/enable-payments.md#choose-a-network)).

**[Full installation guide →](docs/install/README.md)**

## Features

| For | What they get |
| --- | --- |
| **Buyers** | Search and filters, vendor profiles, order tracking, disputes, reviews tied to paid orders |
| **Vendors** | Physical and digital listings with up to 4 photos, stock, automatic digital delivery, an order desk |
| **Moderators** | Dispute desk, payment-review flags, photo and avatar takedowns |
| **Administrators** | Roles, suspensions, second-factor resets, takedowns, CAPTCHA, payment intake, escrow recovery, canary, signed audit exports, optional private app-host reboot |
| **Operators** | One-command install, internal or external PostgreSQL, optional pruned nodes, onion mirrors, encrypted backups |

Photo uploads need the optional ClamAV scanner. ClamAV checks uploads for known malware; it does not review picture
content.

## Payments

Monero only. Monero conceals sender, recipient and amount details on-chain. That reduces public blockchain exposure; it does not guarantee anonymity. The market still sees order and payment metadata, controls custodial escrow keys, and can be exposed by a compromised host or traffic correlation.

Per-order escrow, off by default. A node alone does not turn it on.

| Coin | Model | Networks | Evidence |
| --- | --- | --- | --- |
| Monero | Custodial: market holds keys | mainnet (installer default), fakechain, stagenet | CI on fakechain; one public stagenet claim [recorded](docs/evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md). Mainnet payment and settlement workflows remain unverified. |

Only Monero (XMR) is supported for listing prices, checkout and settlement. Monero testnet is refused.

**Mainnet default.** The installer's local nodes run mainnet unless you pass `--monero-network stagenet`. Mainnet payment and settlement workflows remain unverified;
deployment and readiness checks do not establish real-funds settlement. Monero escrow on mainnet is custodial with real funds. Upgrading never changes
an existing install's network ([UPGRADING](UPGRADING.md#64-new-installs-default-to-mainnet-a-523)). Full evidence:
[Verification](docs/verification.md) and [Per-order escrow](docs/escrow.md).

## How it works

- A browser reaches the Go server through a Tor onion service or the operator's HTTPS proxy.
- The server renders every page, checks CSRF and origin on every form, and audits each action in the same
  transaction.
- PostgreSQL holds all state and migrates on startup.
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
| [Installation guide](docs/install/README.md) | Methods, requirements, first steps, payments, troubleshooting |
| [Production deployment](docs/install/production.md) | Provider-agnostic managed database, separate hosts, TLS, supervision, verification and recovery |
| [Server hardening](docs/install/server-hardening.md) | Approved source addresses, Tor-only ingress, SSH, Docker firewalls, encrypted storage and safe migration |
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
| `cmd/server/` | HTTP server, preview, host tools (`-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors`, `-set-role`) |
| `cmd/verify-audit/` | Offline verifier for signed audit exports |
| `internal/market/` | Marketplace logic, database access, Go tests |
| `internal/market/migrations/` | Ordered SQL migrations. Add new files; never rewrite merged ones. |
| `web/` | Templates and CSS |
| `deploy/` | Tor image, `torrc`, `clamd.conf` |
| `scripts/` | Installer, backup, restore, operations checks |
| `tests/` | Playwright journeys and installer tests |
| `docs/` | Documentation and assets |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/development.md](docs/development.md).

## License

AGPL-3.0 ([LICENSE](LICENSE)). Report vulnerabilities via [SECURITY.md](SECURITY.md).
