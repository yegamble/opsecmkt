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
> **Fresh installations use per-order escrow on test networks only.** Escrow is available but **off until the
> operator sets it up** ([Enable Bitcoin escrow](#enable-bitcoin-escrow)): a default install can register users and
> list goods, but takes no payment. Bitcoin uses 2-of-3 signing; Monero is custodial. Escrow is on the development
> branch, in no release, and runs on test networks only; mainnet and Bitcoin testnet3 are refused. Public signet
> practice, claim and refund and one public stagenet digital claim are recorded; public testnet4 and other Monero
> journeys remain unverified. The pooled wallet is removed: a database holding pooled payment history is refused
> (migrations 107 and 108). See the network matrix, [upgrade guide](UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448),
> and [per-order escrow](docs/escrow.md). Tor protects inbound connections; it does not guarantee anonymity.

## Why OPSEC Market

Most marketplace software assumes a JavaScript-heavy browser and a stack of
third-party services. OPSEC Market is built for the opposite case: a visitor on
Tor Browser with scripts off, and an operator who runs everything on one host.
It has not yet been tried in Tor Browser; the
[manual pass](docs/verification.md#manual-tor-browser-pass-owner) is not yet run.

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
| **Bitcoin per-order escrow** (`ESCROW_BTC=on`) | Regtest, public signet, testnet4. Off by default; mainnet and testnet3 refused. | 2-of-3 signing. Local regtest and private signet tested. Public signet practice, claim and refund passed on 2026-10-09 ([record](docs/evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)). Sparrow 2.5.5 GUI per-order practice, release and sibling stray refund passed on isolated regtest at frozen app `f229a1a65172986065bbab4f3aa24a09725695ad`; a separate GUI import of the supplied descriptor produced the same escrow receive address ([record](docs/evidence/sparrow/2026-10-11-f229a1a65172/README.md)). Signing from that imported watch-only wallet, public Sparrow signing and testnet4 remain unverified. Ledger Flex via Sparrow 2.5.5: attempted, failed before signing, unresolved ([diagnostic](docs/evidence/sparrow/ledger-flex-2026-10-11.md)); only software signing is verified. Real Bitcoin Core regtest escrow legs run in CI (`escrow-regtest`). |
| **Monero per-order escrow** (`ESCROW_XMR=on`) | Local fakechain or stagenet. Off by default; testnet and mainnet refused. | Custodial. Fifteen legs on a real `monerod` fakechain run in CI (`escrow-xmr-regtest`); an offline stagenet fork is tested by hand. One funded public stagenet digital claim passed on 2026-10-10 ([record](docs/evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)). Public refunds and physical deadlines remain unverified. |
| **Pooled wallet (removed)** | None. No pooled payment, payout or drain exists on this build. | A database that holds pooled payment history is refused unchanged: migration 107 while pooled obligations are open (mainnet history always), migration 108 for any `payouts` or `payment_addresses` row. Keep the previous release on such a database; this build neither tests nor drains it (owner directive). No real-market drain is claimed. |

Bundled nodes default to Bitcoin **signet** (the public network with recorded proof; the escrow chain pin makes the first chain sticky, so choose before first use) and Monero **stagenet**; **regtest** and Monero **fakechain** are isolated development networks. Local nodes require disk space and a full first sync even when pruned. External nodes or cloud test nodes avoid storing chains on your workstation. Use the [Bitcoin operator package](docs/operator-guide.md#bitcoin-escrow-test-networks-only) and [Monero setup](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet) to provision escrow wallets and credentials.

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

Install a release, not `main`. Replace `v0.1.0-alpha.2` with the latest prerelease
tag listed on the [Releases page](https://github.com/yegamble/opsecmkt/releases):

```sh
git clone https://github.com/yegamble/opsecmkt.git && cd opsecmkt && git checkout v0.1.0-alpha.2
```

`git pull` on `main` is not a supported upgrade path for a release install; see
[UPGRADING.md](UPGRADING.md).

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
node; `--nodes none` leaves out the nodes. Either way no payment can be taken
until escrow is set up (below).

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

### Enable Bitcoin escrow

A fresh install takes no payment: both escrow adapters are off and the installer edits no
escrow setting. To turn on Bitcoin escrow against the local test-network node the installer
started (Monero escrow has its own steps in the
[operator guide](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet)):

1. Install with a local Bitcoin node (signet by default; regtest or testnet4 by option), for example
   `./scripts/install.sh init --tor --nodes local --bitcoin-network signet`, and let the node sync.
   Never use a mainnet or testnet3 node: escrow refuses them. `--local` installs **no nodes** and the
   installer cannot add them to an existing install later; start with `init` and a node option instead
   (or follow the [operator guide](docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke)
   for a node you run yourself).
2. Run the commands the installer printed at its end, in this directory (they are also in the
   [operator guide](docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke); the examples
   use signet, `--chain` follows your install). First the set-up script, which creates the arbiter and
   watch-only wallets and prints, once, `bitcoin.conf` and `.env` lines for the app's restricted escrow
   RPC user and for the escrow signer's own user. It writes no file. The bundled node listens on RPC port
   8332 whatever the chain, so `bitcoin-cli` needs `-rpcport=8332` as well:

   ```sh
   ./scripts/escrow-node-setup.sh --chain signet --rpc-port 8332 --signer \
     --cli 'docker compose exec -T -u bitcoin bitcoin bitcoin-cli -datadir=/data -rpcport=8332'
   ```

3. Append the printed `rpcauth`, `rpcwhitelistdefault` and `rpcwhitelist` lines to the node's
   `bitcoin.conf`. The quoted `'EOF'` keeps the `$` inside `rpcauth` literal; a shell would expand it in
   an unquoted heredoc or a double-quoted `echo`:

   ```sh
   docker compose exec -T -u bitcoin bitcoin sh -c 'cat >> /data/bitcoin.conf' <<'EOF'
   # paste the printed rpcauth, rpcwhitelistdefault and rpcwhitelist lines here
   EOF
   ```

4. Append the printed `ESCROW_*` lines to `.env`, with `ESCROW_BTC=on` and the signer's profile (the last
   line for a key wins, so write the `COMPOSE_PROFILES` value already in `.env`, here the usual
   `internal-db,bitcoin`, followed by `,escrow-signer`; the installer prints it for your install):

   ```sh
   cat >> .env <<'EOF'
   ESCROW_BTC=on
   COMPOSE_PROFILES=internal-db,bitcoin,escrow-signer
   # paste the printed ESCROW_* lines here
   EOF
   ```

5. Restart the node and check it from the Compose network (the bundled node publishes no port, and the
   printed host `bitcoin` resolves only there; `--via-compose app` sends the checks from the app container).
   It must print `PASS`:

   ```sh
   docker compose restart bitcoin
   ESCROW_BTC_RPC_URL='<the printed URL>' ./scripts/escrow-node-setup.sh --chain signet --verify --via-compose app --wait 120
   ```

6. Make the escrow signer's seed (per-order escrows need the signer; write down the fingerprint it prints and
   back the seed up as the [operator guide](docs/operator-guide.md#escrow-signer) says), then restart
   everything with the new settings:

   ```sh
   docker compose run --rm --no-deps escrow-signer init
   docker compose up -d
   ```

   The app logs `escrow: Bitcoin escrow on (test networks only)` and, once the signer answers, `escrow signer ready`;
   `/admin` shows the escrow status.

CI runs these installer-printed commands on a throwaway regtest install (`escrow-node-rehearsal`); that proves the
command path, not a signet or testnet4 node.

Then back up the arbiter and watch-only wallets as the
[runbook](docs/testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)
says, and read the operator guide's
[Bitcoin escrow](docs/operator-guide.md#bitcoin-escrow-test-networks-only) and
[node set-up](docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke) sections
before inviting anyone: every key, signing step and limit is described there.

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
holds the wallet keys. A database that holds pooled payment records from the
removed wallet is refused by migrations 107 and 108.

**What the server keeps.** There is no account, message or order deletion, and
the operator or anyone holding a decrypted backup can read what is stored.
There is no shipping address field: buyers send the address as a message
encrypted to the vendor's key, and the market keeps that encrypted message
(it cannot read it). It cannot stop an address typed into a plaintext field
such as a dispute reason, an order note or a review. Review text, dispute
reasons, order notes and digital delivery content are stored in plaintext. Listing photos and
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
| [Test-network payments](docs/testnet-runbook.md) | Escrow wallet backup and restore, restore reconciliation, clearing the recovery gate, and chain checks |
| [Per-order escrow](docs/escrow.md) | The escrow design (the pooled wallet it replaced is removed), what it will and will not protect, and its status (Phases 1 and 2: Bitcoin, digital and physical orders, regtest, signet or testnet4 (A-443; tested on regtest, a private signet and the recorded public signet smoke); Phase 3: custodial Monero, a local fakechain or stagenet (A-449; one public stagenet digital claim recorded, other public journeys unrun); all off by default) |
| [Upgrading from v0.1.0-alpha.1](UPGRADING.md) | New settings and migration steps |
| [Implementation status](docs/implementation-status.md) | What works, what is missing, and what the server keeps |
| [Acceptance checks](docs/acceptance.md) | The checklist, and the tests that prove each ticked line |
| [Verification](docs/verification.md) | Evidence table of CI runs and local chain runs |
| [Development and testing](docs/development.md) | Running from source, seeding data, every test suite |
| [Operations tests](docs/operations-tests.md) · [CI and releases](docs/ci-cd.md) · [Local chains](docs/local-chain-testing.md) | Rehearsals, pipelines and the real-chain escrow tests |

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
| `tests/` | Playwright journeys (`tests/e2e/`: preview, database and, in the `browser-escrow` CI job, Bitcoin and Monero escrow against simulated nodes) and installer tests |
| `docs/` | Status, acceptance, operator runbooks and README assets |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the ground rules, the test gates and
the review loops. Development setup and every test suite are in
[docs/development.md](docs/development.md).

## License

AGPL-3.0; see [LICENSE](LICENSE). To report a vulnerability, see [SECURITY.md](SECURITY.md).
