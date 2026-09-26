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
> **Test networks only.** Bitcoin and Monero mainnet are refused. Deposits sit in
> the operator's pooled custodial wallets: this is not escrow, and there is no
> multisig or fee accounting. Tor protects inbound connections to the site; it
> does not guarantee anonymity. See [implementation status](docs/implementation-status.md).

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
| **Buyers** | Catalog search and filters, vendor profiles, order tracking, disputes, and reviews tied to a completed test-network order |
| **Vendors** | Physical and digital listings, stock, automatic digital delivery, and a desk of orders to fulfil |
| **Moderators** | A dispute desk, payment-review flags, and read-only access to disputed orders |
| **Administrators** | Roles, suspensions, second-factor resets, listing takedowns, CAPTCHA and payment-intake controls, payout recovery, the canary and signed audit exports |
| **Operators** | One-command install, PostgreSQL inside or outside Compose, optional pruned test-network nodes, onion mirrors, encrypted backups |

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

Try the read-only preview. It needs only **Go 1.26.8 or newer**: no database,
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
- With the default `GOTOOLCHAIN=auto`, Go downloads the 1.27.1 toolchain pinned
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
`add-mirror` adds a second onion address. `upgrade` brings a checkout up to date
after `git pull`, with a backup first ([UPGRADING.md](UPGRADING.md)):

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
  <img alt="Architecture: a browser without JavaScript reaches the Go server through a Tor onion service or an HTTPS reverse proxy; the server stores data in PostgreSQL and optionally talks to test-network Bitcoin and Monero wallets." src="docs/assets/architecture-light.svg" width="880">
</picture>

A browser with scripts off reaches the Go server through a Tor onion service
(with an optional mirror address) or the operator's HTTPS reverse proxy. The
server renders every page, checks CSRF and origin on every form, and writes an
audit row in the same transaction as each action. PostgreSQL holds all state
and applies migrations on startup. Optional Bitcoin Core and monero-wallet-rpc
wallets run on test networks only; a single watcher confirms deposits and sends
payouts.

**What the server keeps.** There is no account, message or order deletion, and
the operator or anyone holding a decrypted backup can read what is stored.
Shipping addresses are never stored: buyers send them to the vendor
PGP-encrypted. Review text, dispute reasons, order
notes and digital delivery content are stored in plaintext. The full list is in
[what the server keeps](docs/implementation-status.md#what-the-server-keeps).

**Account recovery.** There is no forgotten-password reset. Recovery codes
replace a lost authenticator, and an administrator's password is reset from the
host with `-reset-admin-password` (see the [operator guide](docs/operator-guide.md)).

## Documentation

| Guide | What it covers |
| --- | --- |
| [Operator guide](docs/operator-guide.md) | Deployment, HTTPS and Tor, mirrors, configuration, backups, recovery, troubleshooting |
| [Test-network payments](docs/testnet-runbook.md) | Wallet setup, funding and payout recovery |
| [Upgrading from v0.1.0-alpha.1](UPGRADING.md) | New settings and migration steps |
| [Implementation status](docs/implementation-status.md) | What works, what is missing, and what the server keeps |
| [Acceptance checks](docs/acceptance.md) | The checklist, and the tests that prove each ticked line |
| [Verification](docs/verification.md) | Evidence table of CI runs and local chain runs |
| [Development and testing](docs/development.md) | Running from source, seeding data, every test suite |
| [Operations tests](docs/operations-tests.md) · [CI and releases](docs/ci-cd.md) · [Local chains](docs/local-chain-testing.md) | Rehearsals, pipelines and real regtest and stagenet runs |

## Project layout

| Path | Responsibility |
| --- | --- |
| `cmd/server/` | HTTP server, read-only preview and the `-reset-admin-password` tool |
| `cmd/verify-audit/` | Offline verifier for signed audit exports |
| `internal/market/` | Marketplace behaviour, database access and Go tests |
| `internal/market/migrations/` | Ordered SQL migrations; add new files, never rewrite merged ones |
| `web/` | Server-rendered templates and static CSS |
| `deploy/` | Tor image and `torrc` |
| `scripts/` | Installer, backup and restore, and operations checks |
| `tests/` | Playwright journeys (`tests/e2e/`) and installer tests |
| `docs/` | Status, acceptance, operator runbooks and README assets |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the ground rules, the test gates and
the review loops. Development setup and every test suite are in
[docs/development.md](docs/development.md).
