# OPSEC Market

**A self-hosted marketplace that works without browser JavaScript.**

Give buyers a searchable catalog, vendors a place to manage physical and digital
listings, and moderators the tools to handle disputes. OPSEC Market combines
server-rendered pages, PGP identity and encrypted messaging, optional two-factor
sign-in, and an operator-controlled Go/PostgreSQL deployment.

- **Buy and sell:** catalog filters, vendor profiles, inventory, order history,
  digital delivery and verified-purchase reviews.
- **Control your instance:** account roles, moderation, signed audit exports and
  operator-signed canaries, with clearnet or onion-service deployment options.
- **Keep the browser simple:** HTML and CSS, no frontend build step, no required
  JavaScript bundle or third-party browser assets.
- **Exercise payment flows:** optional Bitcoin and Monero test-network wallets,
  confirmation tracking, releases and refunds.

**This is a test-network marketplace. Mainnet payments are refused.** Deposits
use pooled custodial wallets; multisig escrow and marketplace fee accounting are
not implemented. See [implementation status](docs/implementation-status.md) for
working features and limits, and [acceptance checks](docs/acceptance.md) for the
verification checklist. Tor deployment does not guarantee anonymity.

[Try the preview](#try-the-preview) · [Run locally](#run-locally) ·
[Seed local data](#seed-local-data) · [Run tests](#run-tests) ·
[Operator guide](docs/operator-guide.md)

## Try the preview

Install **Go 1.26.8 or newer**, then run from the repository root:

```sh
go run ./cmd/server -preview
```

Open <http://127.0.0.1:8080>. The preview includes labeled sample data, binds only
to loopback and rejects form writes. It needs no database, wallet or Node.js.
Stop it with Ctrl-C before starting the writable app on the same port. To use
another port, set `PREVIEW_ADDR` (default `127.0.0.1:8080`), for example
`PREVIEW_ADDR=127.0.0.1:18080 go run ./cmd/server -preview`. It must be a
loopback IP address and port (`127.0.0.1:…` or `[::1]:…`, not `localhost`);
anything else stops the preview at startup. `ADDR` is ignored in preview mode.

## Run locally

### Docker installation

Install Docker Engine/Desktop with Compose v2.17 or later, OpenSSL and curl.
Start a local marketplace with one command:

```sh
./scripts/install.sh --local
```

This creates a private configuration, starts PostgreSQL and the app, and waits
until the app is healthy. Open the printed link (normally
<http://127.0.0.1:8080/setup>), read `SETUP_TOKEN` from the generated private `.env`,
and enter it in the browser wizard. Choose your marketplace name and administrator
credentials, then follow the admin getting-started steps to publish your first
listing. No cryptocurrency wallet is required. Keep `.env` private and retain it
across restarts. The installer refuses to overwrite an existing configuration.

Use `APP_PORT=8081 ./scripts/install.sh --local` if port 8080 is occupied. The
chosen port is saved in `.env`. Any second checkout of this marketplace on the
same host (for example next to a running install, or to rehearse recovery) also
needs its own Compose project name:
`COMPOSE_PROJECT_NAME=opsecmkt-rehearsal APP_PORT=8081 ./scripts/install.sh --local`.
Compose names containers and volumes after the project (`opsecmkt` by default),
not the directory, so two checkouts sharing a name would replace each other's
containers, and `docker compose down --volumes` in one would delete the other's
data. The installer saves the name in `.env` and refuses to install when
containers of that project were created from another directory, or when its
volumes exist with no containers. Never change the name of an existing install:
it would start on new, empty volumes. The shortcut is for local HTTP development only;
for production HTTPS, Tor, external databases or optional test-network nodes, use
the interactive installer instead:

```sh
./scripts/install.sh
```

The interactive clearnet path keeps secure cookies on by default; finish setup
through your HTTPS reverse proxy, which must forward the original `Host` header
(nginx needs `proxy_set_header Host $host;`). For each coin it defaults to a local
test-network node in Docker, **pruned** to save disk: allow about 5 GB (Bitcoin
testnet4) to 8 GB (signet) and up to 20 GB for Monero stagenet (estimates), and
hours for the first sync. Answer `no` to the pruning question for a full node,
or choose `external` or `disabled` instead; see
[Local node pruning](docs/operator-guide.md#local-node-pruning). See the
[operator guide](docs/operator-guide.md) for deployment choices. Both paths keep
the database and optional node RPC ports private.

```sh
docker compose up -d --build  # rebuild/start subsequent runs
docker compose logs app      # inspect startup and migration errors
docker compose down          # stop; preserves database volumes
```

Do not add `--volumes` unless you intend to destroy persisted data.

### Develop with Go and a local database

Requirements: **Go 1.26.8+**, **PostgreSQL 17+**, and OpenSSL. The example below
uses Docker for PostgreSQL only; an existing local PostgreSQL server works too.
This dedicated container uses throwaway local credentials and a loopback port.

```sh
docker run -d --name opsecmkt-dev-db \
  -e POSTGRES_USER=opsecmkt -e POSTGRES_PASSWORD=local-dev-only \
  -e POSTGRES_DB=opsecmkt_dev \
  -p 127.0.0.1:55432:5432 postgres:17-alpine

# Wait until this reports "accepting connections" before continuing:
docker exec opsecmkt-dev-db pg_isready -U opsecmkt -d opsecmkt_dev

export DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_dev?sslmode=disable'
export SETUP_TOKEN="$(openssl rand -hex 32)"
export ADDR='127.0.0.1:8080'
export COOKIE_SECURE=false
# Optional: must be clearnet (default) or tor, otherwise startup stops; it changes no behaviour.
export APP_MODE=clearnet
# Keep local development independent of any configured wallets:
unset BITCOIN_RPC_URL MONERO_RPC_URL MONERO_WALLET_RPC_URL

go run ./cmd/server
```

Open <http://127.0.0.1:8080/setup> and use the generated `SETUP_TOKEN` to bootstrap
your administrator. Save that token privately and reuse it across restarts:
TOTP secrets are encrypted with a key derived from it. The server reads environment
variables directly; it does **not** automatically load `.env`. Run from the repo
root so it can find `web/` templates and assets. Schema and migrations apply on
startup; the database user needs permission to create them.

Stop/start this database with `docker stop opsecmkt-dev-db` and
`docker start opsecmkt-dev-db`. Keep it while you need its data; it is a development
container, not a backup strategy.

## Seed local data

A normal installation starts with an **empty catalog**. There is no standalone
`seed` command. Choose the path that matches what you need:

| Purpose | Data source | Persistence |
| --- | --- | --- |
| Browse the design | `go run ./cmd/server -preview` | Read-only sample data; no PostgreSQL writes |
| Manually test real workflows | Bootstrap accounts and publish listings through the UI | Your development database |
| Repeatable Go integration tests | `internal/market/testenv_test.go` fixtures | Isolated schemas, removed on cleanup |
| Repeatable browser tests | Playwright `db-setup` and per-test fixtures | Disposable database; recreate before rerunning |

### Populate a writable development catalog

1. Complete `/setup` on your local instance to create an administrator.
2. Open `/vendor-dashboard`, choose **+ Publish a new listing**, and add a physical
   listing with stock, a description and reference prices (for example `0.001`
   BTC and `0.5` XMR). Add a digital listing to exercise its editor too.
3. In separate browser profiles, register a buyer and a prospective vendor at
   `/register`. As administrator, use **Assign an account role** on `/admin`
   (enter the account's handle) to give the vendor account its role. Publish
   another listing as that vendor.
4. Sign in as the buyer, browse a listing and create an order draft. Check the
   buyer and vendor views, edit/archive/restore listings, and reload to verify
   persistence. With wallets disabled, requesting payment stays unavailable.

Use invented accounts and content. Paid delivery, completion and refund flows
need a test-network wallet or the fake-wallet integration suite; manually setting
orders to paid in SQL bypasses the behavior you want to test. Follow the
[test-network runbook](docs/testnet-runbook.md) for real wallet testing.

### Seed an automated browser fixture for inspection

After setting up the development PostgreSQL container above, install the browser
test dependencies from the next section and create a separate fresh database:

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_e2e
export E2E_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_e2e?sslmode=disable'
unset BITCOIN_RPC_URL MONERO_RPC_URL MONERO_WALLET_RPC_URL
npx playwright test --project=db-setup
```

This runs the real setup and listing forms, creates `browser_admin` with password
`browser-admin-password-123`, publishes **Browser regression test hardware**, and
turns CAPTCHA off after testing its rejection path. These are public test-only
credentials. To inspect the seeded database after Playwright stops its server:

```sh
DATABASE_URL="$E2E_DATABASE_URL" \
  SETUP_TOKEN='e2e-local-only-setup-token-at-least-32-characters' \
  ADDR=127.0.0.1:18081 COOKIE_SECURE=false APP_MODE=clearnet \
  go run ./cmd/server
```

Open <http://127.0.0.1:18081/login>. The fixture seed is **not idempotent**: setup
can only run once. Stop the inspection server and recreate only this disposable
database before another seed or full browser run:

```sh
docker exec opsecmkt-dev-db dropdb -U opsecmkt opsecmkt_e2e
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_e2e
```

## Run tests

### Go and PostgreSQL

Fast checks run without a database, but database integration tests will **skip**:

```sh
go mod verify
go vet ./...
go test -race -count=1 ./...
```

To include real PostgreSQL coverage, create a dedicated test database in the local
container. Run `createdb` once, then reuse this database across Go test runs:

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_test
export TEST_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_test?sslmode=disable'
go test -race -count=1 ./...
```

Tests create isolated schemas, apply migrations, create their own fixture users,
listings and orders, then remove their schemas. Use a dedicated database even
though schemas are isolated. Use different databases for simultaneous Go test
processes: the watcher uses a database-wide advisory lock. Fake RPC tests cover payment behavior; they do not
prove live node integration. The manual Bitcoin check is
[`scripts/regtest-smoke.sh`](scripts/regtest-smoke.sh).

### Browser regression tests

Install **Node.js 22+** for development tests only:

```sh
npm ci
npx playwright install chromium firefox webkit
npm run test:e2e
```

On Linux, use `npx playwright install --with-deps chromium firefox webkit` if
browser OS dependencies are missing. With `E2E_DATABASE_URL` unset, this runs only
the preview suite across Chromium, Firefox and WebKit, from mobile to desktop.
All browser tests disable JavaScript.

For real account, inventory and order journeys, point `E2E_DATABASE_URL` at a
**fresh disposable database**, as shown in the seed section, then run:

```sh
unset BITCOIN_RPC_URL MONERO_RPC_URL MONERO_WALLET_RPC_URL
npm run test:e2e
npm run test:e2e:report
```

The `db-setup` project seeds once before `database-chromium`; each subsequent test
creates its own accounts and listings. Recreate the E2E database before every full
run, including after a setup-only seed. Playwright starts its own servers on
loopback ports **18080** and **18081**, and refuses to reuse existing servers.
Never set either test database URL to a deployed instance.

### Payment journeys with browser automation

A separate suite exercises both Bitcoin and Monero through the production RPC
adapters, with a local simulated wallet and real PostgreSQL. It covers browser
setup, partial deposits, top-ups, confirmation waiting, pausing new payment
requests, manual and automatic fulfillment, verified reviews, dispute refunds,
paid-order cancellation and password-confirmed administrator payout recovery.
It checks single payouts and never connects to a real chain.

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_wallet_e2e
export E2E_WALLET_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_wallet_e2e?sslmode=disable'
npm run test:e2e:wallet
```

Use a fresh database for every run. This suite owns loopback ports 18082 and
18083 and writes separate `playwright-wallet-report/` and `test-results-wallet/`
artifacts. Real-wallet network validation remains a separate runbook step.

For actual local Bitcoin Core and Monero nodes, mined test coins and real wallet
transactions, follow [isolated real-chain browser testing](docs/local-chain-testing.md)
and run `npm run test:e2e:chain`. This uses Bitcoin regtest and an offline Monero
stagenet fork; it does not claim public-testnet validation.

### Scripts, operations and CI

```sh
python3 -m unittest discover -s tests -p 'test_*.py' -v
for script in scripts/*.sh; do bash -n "$script"; done
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

[CI](.github/workflows/ci.yaml) also validates Compose profiles, container startup,
upgrades, encrypted backup/restore, dependencies and browser regressions with
PostgreSQL. See [operations tests](docs/operations-tests.md) for local rehearsals
and [CI/release instructions](docs/ci-cd.md). Version tags produce CI-gated
draft release artifacts; they do not deploy a live instance.

## Find your way around

| Path | Responsibility |
| --- | --- |
| `cmd/server/` | HTTP server and read-only preview entry point |
| `internal/market/` | Marketplace behavior, database access and Go tests |
| `internal/market/migrations/` | Ordered SQL migrations; add new files, never rewrite merged ones |
| `web/` | Server-rendered templates and static assets |
| `tests/e2e/` | Playwright preview and database-backed journeys |
| `scripts/` | Installer, backup/restore and operations checks |
| `docs/` | Capability status, acceptance criteria and operator runbooks |

## Claude review loops

Claude reads [CLAUDE.md](CLAUDE.md) and the
[completion scrutiny loop](.claude/review-loop.md). The Stop and SubagentStop hooks
request an extra review before finishing and use a recursion guard to keep that
reminder bounded, following the [Claude hook protocol](https://code.claude.com/docs/en/hooks#stop-decision-control). Python 3 is required. Use `/scrutinise <scope>` for a focused
review in Claude Code. Hooks prompt review; they do not certify test results.

For the same external Ralph runner used by Vidra, set the current task and its
acceptance criteria in [.ralph/fix_plan.md](.ralph/fix_plan.md), then run `ralph`
from this directory. Ralph and the Claude CLI must already be installed and
configured. [.ralphrc](.ralphrc) sets the budget and circuit breakers;
[the prompt](.ralph/PROMPT.md) requires a separate final scrutiny iteration before
signalling completion. Keep one writing loop per checkout. No loop is started by
installing these files.

## Deployment and further reading

- [Operator guide](docs/operator-guide.md): HTTPS/Tor, mirrors, configuration,
  migrations, backups and recovery.
- [Test-network payments](docs/testnet-runbook.md): wallet setup and payout recovery.
- [Upgrade guide](UPGRADING.md): compatibility and migration instructions.
- [Implementation status](docs/implementation-status.md): what works and what is missing.
- [Acceptance checks](docs/acceptance.md): evidence required to verify behavior.
