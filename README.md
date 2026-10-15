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
> (real funds)** unless you name a test network; `--local` runs no nodes. No mainnet run is recorded; mainnet is covered
> by unit, database and simulated-RPC tests only. Tor protects inbound connections; it does not guarantee anonymity.

## Why OPSEC Market

Built for a visitor on Tor Browser with scripts off, and an operator who runs everything on one host.

- **Nothing runs in the browser.** Plain HTML and CSS, a strict Content Security Policy, no third-party assets.
- **Identity is PGP.** Sign in with a PGP challenge or TOTP. Messages are PGP ciphertext the server cannot read.
- **The operator owns the instance.** One Docker Compose install, onion or clearnet, with signed audit exports and
  a canary.

Not yet tried in Tor Browser: the [manual pass](docs/verification.md#manual-tor-browser-pass-owner) is pending.

<table>
  <tr>
    <td width="72%"><img alt="Catalog page of the read-only preview, showing sample listings, category filters and the read-only preview banner" src="docs/assets/screenshot-catalog.png"></td>
    <td width="28%"><img alt="The same catalog at phone width, with navigation wrapped and filters collapsed" src="docs/assets/screenshot-mobile.png"></td>
  </tr>
  <tr>
    <td colspan="2" align="center"><sub>Read-only preview with sample data and JavaScript disabled. Desktop and 390 px phone width.</sub></td>
  </tr>
</table>

## Install

| Method | Use it to | Command |
| --- | --- | --- |
| [Read-only preview](docs/install/preview.md) | Look around. Needs only Go 1.26.9+. | `go run ./cmd/server -preview` |
| [Local marketplace](docs/install/local.md) | Try the full app on `127.0.0.1` | `./scripts/install.sh --local` |
| [Onion service](docs/install/tor.md) | Run a market over Tor (recommended) | `./scripts/install.sh init --tor` |
| [Public domain](docs/install/clearnet.md) | Run a market behind HTTPS | `./scripts/install.sh init --clearnet` |

Needs Git, Docker with Compose v2 (2.17+ for Tor) and OpenSSL. Use a
[release tag](https://github.com/yegamble/opsecmkt/releases), not `main`:

```sh
git clone https://github.com/yegamble/opsecmkt.git && cd opsecmkt && git checkout v0.1.0-alpha.2
./scripts/install.sh --local
```

Open the printed setup link, paste the token, create the administrator.

> **Warning:** the local nodes of `init --tor` and `init --clearnet` default to **mainnet**. Real funds; no mainnet run
> is recorded; mainnet is covered by unit, database and simulated-RPC tests only. For a test market add
> `--bitcoin-network signet --monero-network stagenet` ([why](docs/install/enable-payments.md#choose-a-network)).

**[Full installation guide →](docs/install/README.md)**

## Features

| For | What they get |
| --- | --- |
| **Buyers** | Search and filters, vendor profiles, order tracking, disputes, reviews tied to paid orders |
| **Vendors** | Physical and digital listings with up to 4 photos, stock, automatic digital delivery, an order desk |
| **Moderators** | Dispute desk, payment-review flags, photo and avatar takedowns |
| **Administrators** | Roles, suspensions, second-factor resets, takedowns, CAPTCHA, payment intake, escrow recovery, canary, signed audit exports |
| **Operators** | One-command install, internal or external PostgreSQL, optional pruned nodes, onion mirrors, encrypted backups |

Photo uploads need the optional ClamAV scanner. ClamAV checks uploads for known malware; it does not review picture
content.

## Payments

Per-order escrow, off by default. A node alone does not turn it on.

| Coin | Model | Networks | Evidence |
| --- | --- | --- | --- |
| Bitcoin | 2-of-3 multisig: buyer, vendor, market | mainnet (installer default), regtest, signet, testnet4 | CI on regtest; public signet run [recorded](docs/evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md). testnet4, mainnet and hardware-wallet signing unverified. |
| Monero | Custodial: market holds keys | mainnet (installer default), fakechain, stagenet | CI on fakechain; one public stagenet claim [recorded](docs/evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md). Mainnet unrun. |

Bitcoin testnet3 and Monero testnet are refused. A market is all mainnet or all test networks.

**Mainnet default.** The installer's local nodes run mainnet unless you pass `--bitcoin-network
signet|testnet4|regtest` or `--monero-network stagenet`. No mainnet run is recorded; mainnet is covered by unit,
database and simulated-RPC tests only. Monero escrow on mainnet is custodial with real funds. Upgrading never changes
an existing install's network ([UPGRADING](UPGRADING.md#64-new-installs-default-to-mainnet-a-523)). Full evidence:
[Verification](docs/verification.md) and [Per-order escrow](docs/escrow.md).

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/architecture-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/architecture-light.svg">
  <img alt="OPSEC Market architecture: a browser reaches the Go server through Tor or HTTPS. PostgreSQL keeps application and historical payment records. Optional Bitcoin 2-of-3 escrow is shown on regtest, signet and testnet4; custodial Monero escrow on fakechain or stagenet. Both are off by default. The installer's local nodes default to mainnet (real funds); no mainnet run is recorded." src="docs/assets/architecture-light.svg" width="880">
</picture>

- A browser reaches the Go server through a Tor onion service or the operator's HTTPS proxy.
- The server renders every page, checks CSRF and origin on every form, and audits each action in the same
  transaction.
- PostgreSQL holds all state and migrates on startup.
- Escrow, when on, uses a dedicated Bitcoin signer with 2-of-3 PSBT signing, or a Monero escrow wallet RPC.

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
