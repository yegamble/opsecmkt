<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/logo-light.svg">
  <img alt="[OPSECMKT]" src="docs/assets/logo-light.svg" width="420">
</picture>

### A self-hosted marketplace that needs no browser JavaScript

Server-rendered Go and PostgreSQL, PGP identity, Tor-first deployment.

[![CI](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml)

[Quick start](#quick-start) · [Self-host](#self-host) · [How it works](#how-it-works) · [Status](docs/implementation-status.md) · [Operator guide](docs/operator-guide.md) · [Contributing](CONTRIBUTING.md)

</div>

> [!IMPORTANT]
> **Fresh installations use per-order escrow on test networks only.** Escrow is built on the development
> branch, not released, and is off by default. Bitcoin uses 2-of-3 signing; Monero is custodial.
> Public signet practice, claim and refund and one public stagenet digital claim are recorded;
> public testnet4 and other Monero journeys remain unverified. Legacy pooled upgrades must pass the drain
> refusal before this build can start. See the network matrix, [upgrade guide](UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448),
> and [per-order escrow](docs/escrow.md). Tor protects inbound connections; it does not guarantee anonymity.

## Why OPSEC Market

Most marketplace software assumes a JavaScript-heavy browser and a stack of
third-party services. OPSEC Market is built for the opposite case: a visitor on
Tor Browser with scripts off, and an operator who runs everything on one host.

- **Nothing runs in the browser.** Pages are plain HTML and CSS with a strict
  Content Security Policy. There is no frontend build and no third-party asset.
- **Identity is PGP.** Users prove key ownership, can sign in with a PGP
  challenge or TOTP, and message each other in PGP ciphertext the server checks
  but cannot read.
- **The operator owns the instance.** One Docker Compose deployment, onion or
  clearnet, with signed audit exports and an operator-signed canary.

## What you get

| For | Capabilities |
| --- | --- |
| **Buyers** | Catalog search and filters, vendor profiles, order tracking, disputes, and reviews tied to a completed paid order (test networks only) |
| **Vendors** | Physical and digital listings with up to 4 photos each, an avatar, stock, automatic digital delivery, and a desk of orders to fulfil |
| **Moderators** | A dispute desk, payment-review flags, read-only access to disputed orders, and takedown of vendors' photos and avatars |
| **Administrators** | Roles, suspensions, second-factor resets, listing and photo takedowns, the public staff list, CAPTCHA and payment-intake controls, escrow recovery, the canary and signed audit exports |
| **Operators** | One-command install, PostgreSQL inside or outside Compose, optional pruned test-network nodes, onion mirrors, encrypted backups |

### Payment and escrow networks

Payments on this development build use test coins only. Enabling a node alone does not enable escrow.

| Path | Supported networks | Evidence and limits |
| --- | --- | --- |
| **Bitcoin per-order escrow** (`ESCROW_BTC=on`) | Regtest, public signet, testnet4. Off by default; mainnet and testnet3 refused. | 2-of-3 signing. Local regtest and private signet tested. Public signet practice, claim and refund passed on 2026-10-09 ([record](docs/evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)). Sparrow 2.5.5 GUI per-order practice/release/refund passed on isolated regtest at frozen app `f229a1a65172986065bbab4f3aa24a09725695ad`; a separate GUI import of the supplied descriptor produced the same escrow receive address ([record](docs/evidence/sparrow/2026-10-11-f229a1a65172/README.md)). Signing from that imported watch-only wallet, Ledger Flex signing, public Sparrow signing and testnet4 remain unverified. |
| **Monero per-order escrow** (`ESCROW_XMR=on`) | Local fakechain or stagenet. Off by default; testnet and mainnet refused. | Custodial. Local fakechain and offline stagenet fork tested. One funded public stagenet digital claim passed on 2026-10-10 ([record](docs/evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)). Public refunds and physical deadlines remain unverified. |
| **Historical pooled records** | No new pooled payments on this build. | Records remain readable. An older database with any drain blocker, including mainnet pooled history, is refused unchanged. Resolve ordinary blockers using the prior drain-capable build before upgrading. Mainnet history remains a refusal even after settlement: keep the prior release and preserve its records. No real-market drain is claimed. |

Bundled nodes default to Bitcoin **testnet4** and Monero **stagenet**. Bitcoin **signet** is the public network with recorded proof; **regtest** and Monero **fakechain** are isolated development networks. Local nodes require disk space and a full first sync even when pruned. External nodes or cloud test nodes avoid storing chains on your workstation. Use the [Bitcoin operator package](docs/operator-guide.md#bitcoin-escrow-test-networks-only) and [Monero setup](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet) to provision escrow wallets and credentials.

<table>
  <tr>
    <td width="72%"><img alt="Catalog page of the read-only preview, showing sample listings, category filters and the read-only preview banner" src="docs/assets/screenshot-catalog.png"></td>
    <td width="28%"><img alt="The same catalog at phone width, with navigation wrapped and filters collapsed" src="docs/assets/screenshot-mobile.png"></td>
  </tr>
  <tr>
    <td colspan="2" align="center"><sub>Read-only preview with sample data and JavaScript disabled. Desktop and 390 px phone width.</sub></td>
  </tr>
</table>

## Quick start

Try the read-only preview. It needs only **Go 1.26.9 or newer**: no database,
wallet or Node.js.

```sh
go run ./cmd/server -preview
```

Open <http://127.0.0.1:8080>. The preview binds to loopback, shows labelled
sample data and rejects every form write. Stop it with <kbd>Ctrl</kbd>+<kbd>C</kbd>.

<details>
<summary>Port, loopback and toolchain notes</summary>

- To use another port, set `PREVIEW_ADDR`, for example
  `PREVIEW_ADDR=127.0.0.1:18080 go run ./cmd/server -preview`. It must be a
  loopback IP address and port (`127.0.0.1:…` or `[::1]:…`, not `localhost`);
  anything else stops the preview at startup. `ADDR` is ignored in preview mode.
- With the default `GOTOOLCHAIN=auto`, Go downloads the 1.27.2 toolchain pinned
  in `go.mod`. Set `GOTOOLCHAIN=local` to refuse the download.

</details>

## Self-host

You need Docker with Compose v2 (2.17 or later for Tor) and OpenSSL.

**Local marketplace in one command.** HTTP on loopback, internal PostgreSQL, no
wallets:

```sh
./scripts/install.sh --local
```

The installer writes a private `.env`, starts PostgreSQL and the app, and waits
until they are healthy. It then prints the setup link (normally
<http://127.0.0.1:8080/setup>) and a one-time setup token. Paste the token there
with your marketplace name and administrator account. Keep `.env` private: it
holds every secret.

**Onion service or production.** Choose the exposure; the installer asks the
rest, or nothing if you pass every flag (`./scripts/install.sh --help`):

```sh
./scripts/install.sh init --tor        # prints the onion address, setup link and token
./scripts/install.sh init --clearnet   # prints Caddy and nginx snippets for your HTTPS proxy
```

Add `--admin HANDLE` to create the administrator in the terminal instead of with
a token. For each coin the installer defaults to a pruned local test-network
node; `--nodes none` leaves payments off.

**Afterwards.** `status` shows health, setup state and onion addresses.
`add-mirror` adds a second onion address. `add-scanner` adds the ClamAV malware
scanner that photo uploads need (a host with at least 3 GiB of memory), and
`refresh-scanner` refreshes its signatures ([operator
guide](docs/operator-guide.md#photo-uploads-and-the-malware-scanner)). ClamAV
checks uploads for known malware; it does not review picture content. `upgrade`
brings a checkout up to date after `git pull`, with a backup first
([UPGRADING.md](UPGRADING.md)):

```sh
./scripts/install.sh status
AGE_RECIPIENT=age1... ./scripts/install.sh upgrade
```

The [operator guide](docs/operator-guide.md) covers HTTPS and Tor, mirrors,
port and project-name choices, node pruning, backups, upgrades and recovery.
`docker compose logs app` shows startup and migration errors; `docker compose
down` stops everything and keeps the data.

> [!WARNING]
> `docker compose down --volumes` deletes the marketplace's data. Use it only on
> an install you mean to destroy.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/architecture-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/assets/architecture-light.svg">
  <img alt="OPSEC Market architecture: a browser reaches the Go server through Tor or HTTPS. PostgreSQL keeps application and historical payment records. Optional Bitcoin 2-of-3 escrow supports regtest, signet and testnet4; custodial Monero escrow supports fakechain or stagenet. Both are off by default and test-network-only. Legacy pooled upgrades require an empty drain list." src="docs/assets/architecture-light.svg" width="880">
</picture>

A browser with scripts off reaches the Go server through a Tor onion service
(with an optional mirror address) or the operator's HTTPS reverse proxy. The
server renders every page, checks CSRF and origin on every form, and writes an
audit row in the same transaction as each action. PostgreSQL holds all state
and applies migrations on startup. Optional per-order escrow is off by default
and accepts the test networks shown below. Bitcoin uses a dedicated signer and
2-of-3 PSBT signing by the parties; Monero uses a separate escrow wallet RPC and
holds the wallet keys. Historical pooled payment records remain in PostgreSQL;
a refusing migration protects an undrained legacy database.

**What the server keeps.** There is no account, message or order deletion, and
the operator or anyone holding a decrypted backup can read what is stored.
Shipping addresses are never stored: buyers send them to the vendor
PGP-encrypted. Review text, dispute reasons, order
notes and digital delivery content are stored in plaintext. Listing photos and
avatars are kept only as re-encoded JPEGs without the uploaded file, its name or
its metadata; deleting one leaves copies in older backups. The full list is in
[what the server keeps](docs/implementation-status.md#what-the-server-keeps).

**Account recovery.** There is no forgotten-password reset. Recovery codes
replace a lost authenticator, and an administrator's password and second factors
are reset from the host with `-reset-admin-password` and `-reset-admin-factors`
(see the [operator guide](docs/operator-guide.md#authentication)).

## Documentation

| Guide | What it covers |
| --- | --- |
| [Operator guide](docs/operator-guide.md) | Deployment, HTTPS and Tor, mirrors, configuration, backups, recovery, troubleshooting |
| [Test-network payments](docs/testnet-runbook.md) | Wallet setup, funding and payout recovery |
| [Per-order escrow](docs/escrow.md) | The escrow design replacing the pooled wallet, what it will and will not protect, and its status (Phases 1 and 2: Bitcoin, digital and physical orders, regtest, signet or testnet4 (A-443; tested on regtest, a private signet and the recorded public signet smoke); Phase 3: custodial Monero, a local fakechain or stagenet (A-449; one public stagenet digital claim recorded, other public journeys unrun); all off by default) |
| [Upgrading from v0.1.0-alpha.1](UPGRADING.md) | New settings and migration steps |
| [Implementation status](docs/implementation-status.md) | What works, what is missing, and what the server keeps |
| [Acceptance checks](docs/acceptance.md) | The checklist, and the tests that prove each ticked line |
| [Verification](docs/verification.md) | Evidence table of CI runs and local chain runs |
| [Development and testing](docs/development.md) | Running from source, seeding data, every test suite |
| [Operations tests](docs/operations-tests.md) · [CI and releases](docs/ci-cd.md) · [Local chains](docs/local-chain-testing.md) | Rehearsals, pipelines and real regtest and stagenet runs |

## Project layout

| Path | Responsibility |
| --- | --- |
| `cmd/server/` | HTTP server, read-only preview and the host tools (`-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors`, `-set-role`) |
| `cmd/verify-audit/` | Offline verifier for signed audit exports |
| `internal/market/` | Marketplace behaviour, database access and Go tests |
| `internal/market/migrations/` | Ordered SQL migrations; add new files, never rewrite merged ones |
| `web/` | Server-rendered templates and static CSS |
| `deploy/` | Tor image and `torrc`, the malware scanner's `clamd.conf` |
| `scripts/` | Installer, backup and restore, and operations checks |
| `tests/` | Playwright journeys (`tests/e2e/`: preview, database, wallet and, in the `browser-escrow` CI job, Bitcoin and Monero escrow against simulated nodes) and installer tests |
| `docs/` | Status, acceptance, operator runbooks and README assets |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the ground rules, the test gates and
the review loops. Development setup and every test suite are in
[docs/development.md](docs/development.md).
