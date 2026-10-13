# Development and testing

How to run OPSEC Market from source, fill a development database, and run every
test suite locally. For deploying a marketplace, use the
[operator guide](operator-guide.md). For what each check proves, see
[verification](verification.md) and [acceptance checks](acceptance.md).

## Run from source with a local database

Requirements: **Go 1.26.9+** (with the default `GOTOOLCHAIN=auto`, Go downloads the
1.27.2 toolchain pinned in `go.mod`; set `GOTOOLCHAIN=local` to refuse the download),
**PostgreSQL 17+**, and OpenSSL. The example below
uses Docker for PostgreSQL only; an existing local PostgreSQL server works too, if its databases are UTF8
(`SHOW server_encoding`; startup and the tests refuse any other encoding, so create them with
`createdb -E UTF8 -T template0 NAME`).
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
# Optional: must be clearnet (default) or tor, otherwise startup stops; tor also holds Bitcoin escrow broadcasts
# unless the escrow node sends through a proxy (A-441, docs/operator-guide.md).
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
[test-network runbook](testnet-runbook.md) for real wallet testing.

### Seed an automated browser fixture for inspection

After setting up the development PostgreSQL container above, install the browser
test dependencies (see [Browser regression tests](#browser-regression-tests)) and create a separate fresh database:

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

## Documentation site

The docs in `docs/` build into a static site with [MkDocs](https://www.mkdocs.org/) and the Material theme
(`mkdocs.yml`). The site loads no third-party fonts or scripts. Pages work without JavaScript; search needs it.

```sh
python3 -m venv .venv-docs
.venv-docs/bin/pip install -r requirements-docs.txt
.venv-docs/bin/mkdocs serve      # http://127.0.0.1:8000, reloads on save
.venv-docs/bin/mkdocs build      # writes site/; fails on any broken link or anchor
```

`scripts/mkdocs_hooks.py` publishes `UPGRADING.md`, `SECURITY.md` and `CONTRIBUTING.md` from the repository root
and points other links outside `docs/` at GitHub. Add a new page to `nav` in `mkdocs.yml`. CI's `docs` job runs
the same strict build.

`requirements-docs.txt` is hash-locked. To change a version, edit `requirements-docs.in` and run the `uv pip compile`
command at its top. MkDocs stays below 2.0, which drops the plugin and theme system Material needs. The planned
successor is [Zensical](https://zensical.org/), which reads `mkdocs.yml`; migrate once it runs hooks.

### Host the docs as an onion service

The site runs as its own onion service: an nginx container with no internet access, behind the project's pinned Tor
image, in a separate Compose project (`opsecmkt-docs`, `deploy/docs/compose.yaml`). The image runs the same strict
build as CI, keeps no access log and sends a restrictive Content Security Policy.

> **Warning:** run the docs on a **separate host** from any market. The docs are public and tied to the project;
> on a shared host the two onion addresses could be linked by shared downtime and timing, and one seizure takes
> both. `scripts/docs-onion.sh up` refuses on a host with a market `.env` in the checkout, a container from a market
> Compose file, another project's Tor container or another onion identity volume.

On the docs host:

```sh
git clone https://github.com/yegamble/opsecmkt.git && cd opsecmkt
./scripts/docs-onion.sh up        # builds, starts, prints the onion address
./scripts/docs-onion.sh address   # print it again
./scripts/docs-onion.sh down      # stop; keeps the address
```

To publish changes, pull and run `up` again.

The address exists only as the keys in the `opsecmkt-docs_tor_identity` volume. Back them up with
[age](https://age-encryption.org/) before publishing the address, and store the backup away from the age identity:

```sh
AGE_RECIPIENT=age1... ./scripts/docs-onion.sh backup-keys          # writes backups/docs-onion-<UTC>.tar.age
AGE_IDENTITY=key.txt ./scripts/docs-onion.sh restore-keys FILE     # new host, before the first up
```

`backup-keys` never overwrites a file, and `restore-keys` refuses a volume that already holds keys. Publish the
address in a signed statement before linking to it.

**Cloudflare.** Cloudflare cannot host an onion address you own. Its
[Onion Routing](https://developers.cloudflare.com/network/onion-routing) only serves a site already proxied on a
clearnet domain, and sends Tor Browser users to a Cloudflare-owned `.onion` that you cannot publish or keep, while
Cloudflare sees every request. [Cloudflare Tunnel](https://developers.cloudflare.com/tunnel/) exposes services on
clearnet domains only. Use Cloudflare, if at all, for an optional clearnet copy of these public docs, never for a
market.

## Run tests

### Go and PostgreSQL

Fast checks run without a database, but database integration tests will **skip**:

```sh
go mod verify
"$(go env GOROOT)/bin/gofmt" -l cmd internal   # lists unformatted files; must print nothing
go vet ./...
go test -race -count=1 ./...
```

To include real PostgreSQL coverage, create a dedicated test database in the local
container. Run `createdb` once, then reuse this database across Go test runs:

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_test
export TEST_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_test?sslmode=disable'
go test -race -count=1 -timeout 30m -run '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
go test -race -count=1 -timeout 30m -skip '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
```

The two commands are CI's two Go jobs (see [CI and release delivery](ci-cd.md)): with the same pattern, one as `-run`
and one as `-skip`, they run every test exactly once between them. Run them one after the other against the same
database, or at the same time against two different databases (below). They replace the single
`go test -race -count=1 -timeout 30m ./...`: `-timeout` is per package, and `internal/market` in one race process no
longer finishes within 30 minutes on a loaded 4-CPU host (QA4W2-E1). Measured on 2026-10-06 on the shared 4-CPU
development host with other suites running (load average 6 to 11), one after the other against one database: the
`-run` half took 1021 s wall (`internal/market` 941 s) and the `-skip` half 958 s (`internal/market` 953 s,
`cmd/server` 113 s).

Tests create isolated schemas, apply migrations, create their own fixture users,
listings and orders, then remove their schemas. When the test role has the CREATEDB privilege,
`internal/market` migrates one template database per test process (`opsecmkt_tmpl_<pid>_<random>`) and
gives each database test a copy of it (`opsecmkt_copy_<random>`), which is several times faster; without
CREATEDB it logs one line and migrates a schema per test in `TEST_DATABASE_URL`'s database instead.
`TestSettlementClashGuide` and
`TestReturnWithoutReleaseGuide` run UPGRADING.md's SQL blocks through `psql`, so
install the PostgreSQL client on the host; without `psql` they skip (on CI they fail). Use a dedicated database even
though schemas are isolated. Use different databases for simultaneous Go test
processes: the watcher uses a database-wide advisory lock. The `cmd/server` tests
create a database of their own beside it (`opsecmkt_cmd_server_<random>`, dropped
when they finish; without the CREATEDB privilege they use `TEST_DATABASE_URL`'s
database and log that). A run that panics, times out or is killed leaves these
databases behind: drop leftover `opsecmkt_cmd_server_*`, `opsecmkt_tmpl_*` and
`opsecmkt_copy_*` databases that have no connections (no later run uses them). Each half of the
`internal/market` race run takes longer than `go test`'s default 10-minute timeout on a busy host, so both commands
set `-timeout 30m`. Fake RPC tests cover payment behavior; they do not
prove live node integration. The real-chain Bitcoin escrow check is
[`scripts/test-escrow-regtest.sh`](../scripts/test-escrow-regtest.sh) (CI's `escrow-regtest` job).

### Browser regression tests

Install **Node.js 22+** for development tests only:

```sh
npm ci
npx playwright install chromium firefox webkit
npm run test:e2e
```

On Linux, use `npx playwright install --with-deps chromium firefox webkit` if
browser OS dependencies are missing. With `E2E_DATABASE_URL` unset, this runs only
the preview suite: Chromium from 320 px to desktop, plus Firefox and WebKit at 390 px.
All browser tests disable JavaScript.

For real account, inventory and order journeys, point `E2E_DATABASE_URL` at a
**fresh disposable database**, as shown in the seed section, then run:

```sh
unset BITCOIN_RPC_URL MONERO_RPC_URL MONERO_WALLET_RPC_URL
npm run test:e2e
npm run test:e2e:report
```

The `db-setup` project seeds once before `database-chromium` and `database-firefox`
(the upload cases of `db-listing-photos.spec.ts` in Firefox; alone, on a fresh
database: `npx playwright test --project=database-firefox`); each subsequent test
creates its own accounts and listings. Recreate the E2E database before every full
run, including after a setup-only seed. Playwright starts its own servers on
loopback ports **18080** and **18081**, and refuses to reuse existing servers.
Never set either test database URL to a deployed instance.

### Bitcoin escrow paste journey (simulated escrow node)

`tests/e2e/escrow-journey.spec.ts` drives Bitcoin escrow (a simulated regtest node, `ESCROW_BTC=on`;
see [escrow](escrow.md)) through the real server and PostgreSQL in Chromium at 320 px with
JavaScript disabled, every form by keyboard alone: two escrow keys registered and activated
by practice signing, an escrow checkout funded and completed on funding (automatic delivery),
the vendor's claim (copy downloaded, a SIGHASH_NONE paste refused with its error, the signed
copy pasted, co-signed by the market, broadcast and confirmed), and a refund of an underpaid
order the buyer cancels, with the order page checked and photographed at each state. The
escrow node is **simulated**: `tests/e2e/fixtures/escrow_rpc.py`, served by
`escrow-simulators.py --escrow-port`, answers the escrow adapter's PSBT methods with real PSBT and
transaction bytes but its own fake signatures, and plays each party's wallet. It knows
regtest only and refuses mainnet and other-network addresses and keys. This is not chain
evidence; real Bitcoin Core legs are `scripts/test-escrow-regtest.sh`, and
`scripts/test-escrow-wallets.sh` checks which real wallets (Bitcoin Core, Electrum 4.8.2) sign the
escrow transactions, by hand on a regtest node with P2P disabled and RPC bound to host loopback. With a disposable
`TEST_DATABASE_URL`, its A-457 leg also runs both BIP39 Electrum parties through registration, practice, checkout,
funding, release and sibling refund with the real escrow signer. It is not run in CI.

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_escrow_e2e
export E2E_ESCROW_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_escrow_e2e?sslmode=disable'
npm run test:e2e:escrow
```

`tests/e2e/escrow-per-order-journey.spec.ts` (A-455) runs next on the same database: a vendor registers a
per-order wallet key and signs its practice transaction (the simulated node derives rooted keys by real BIP32 public
derivation), a buyer sees the vendor's key type on the listing and the vendor page, and the order page names the
vendor's per-order wallet key beside the buyer's Bitcoin Core key (a mixed pair, refused since A-456a).
`tests/e2e/escrow-per-order-settle-journey.spec.ts` (A-456b) follows: both parties register per-order wallet keys and
sign their practice, the buyer pays into a per-order escrow, each party reads its own order wallet setup page, the escrow
is funded, and the vendor's claim is signed and uploaded as a binary PSBT file, co-signed through a simulated escrow
signer (`escrow-simulators.py --signer-socket`, a Unix socket named by the config for its run) and confirmed.
`tests/e2e/escrow-physical-journey.spec.ts` runs after it on the same database and follows a
physical escrow order (WR-escrow Phase 2): shipped with a claim address, signed by the vendor and
the buyer, released at its deadline, a dispute that cancels the deadline, the buyer's no-ship
cancel and its race with the shipment, a buyer who never signs (the shipped and 72-hour notices,
the `/orders` banner) and the administrator's release deadline length. The test moves the release
and ship-by clocks through SQL (with `psql` on `PATH`, using the connection in
`E2E_ESCROW_DATABASE_URL`); the simulated node is the same, so this is not chain evidence.

Use a fresh database for every run. It owns loopback ports 18090–18092, writes
`playwright-escrow-report/` and `test-results-escrow/` (with one screenshot per order-page
state), and is not part of `npm run test:e2e`. CI runs it, with the Monero suite below, in the
`browser-escrow` job ("Escrow browser journeys (simulated nodes, real application)", A-474; see
[CI and release delivery](ci-cd.md)). `python3 -m unittest tests.test_escrow_rpc_fixture`
checks the simulator alone.

### Monero escrow journey (simulated escrow wallet-rpc)

`tests/e2e/escrow-xmr-journey.spec.ts` drives Monero escrow (local fakechain only,
`ESCROW_XMR=on`; WR-escrow Phase 3) through the real server and PostgreSQL in Chromium at 320 px
with JavaScript disabled, every form by keyboard alone: checkout with a refund address and the
custody sentence, funding that waits at 9 confirmations and completes a digital order at 10, the
vendor's claim with a password step-up (built, sent and confirmed, with its outputs and network
fee), an underpaid order cancelled and refunded, a physical order shipped with a claim address and
released by Confirm receipt, a release at the deadline, a dispute decided for the buyer (the resolve
form states that the decision is final), the administrator's re-request of a failed settlement and
retry of a set-aside one, the restore-paused and sweep states (set through SQL), and the order page
with Monero escrow off, read on a second server that runs with `ESCROW_XMR` off on the same
database. The retained and destroyed wallet states come with escrow Phase 3's retention slice.

The escrow wallet-rpc and its daemon are **simulated**: `tests/e2e/fixtures/xmr_escrow_rpc.py`,
served by `escrow-simulators.py --xmr-wallet-port --xmr-daemon-port`. Its keys and addresses are real
Monero keys and addresses (prime-order ed25519 points, derived as Monero derives them), and its
replies have monero-wallet-rpc 0.18.5.1's shapes and error codes: `xmr_record_shapes.py` recorded
them from the real nodes into `xmr_rpc_shapes_0.18.5.1.json`, and
`python3 -m unittest tests.test_xmr_escrow_rpc_fixture` runs the same recorder against the
simulator and requires the same file. `TestXMREscrowFixtureAdapter` drives the app's own adapter
against it. No chain exists and no coins move, so this is not chain evidence; real fakechain legs are
`scripts/test-escrow-xmr-regtest.sh`, and `scripts/test-escrow-xmr-payee2.sh` measures how one
monthly escrow wallet scales (PAYE-E2).

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_escrow_xmr_e2e
export E2E_ESCROW_XMR_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_escrow_xmr_e2e?sslmode=disable'
npm run test:e2e:escrow-xmr
```

Use a fresh database for every run; the test moves clocks and restore settings through SQL with
`psql` on `PATH`. It owns loopback ports 18093–18097, writes `playwright-escrow-xmr-report/` and
`test-results-escrow-xmr/` (one screenshot per order-page state), and is not part of
`npm run test:e2e`; the `browser-escrow` CI job runs it after the Bitcoin escrow suite, each on its
own database.

For actual local Bitcoin Core escrow transactions, use
`scripts/test-escrow-regtest.sh` with a disposable PostgreSQL database
([real-chain escrow tests](local-chain-testing.md) lists every real-chain script
and what CI runs). Neither local procedure proves public-network behavior; public
records identify their own frozen sources in [verification](verification.md).

### Monero escrow on a local fakechain (real monerod and monero-wallet-rpc)

Monero escrow (WR-escrow Phase 3, `ESCROW_XMR=on`, custodial, off by default; see [escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain))
has fifteen legs that run the real server code (a Go test per leg, with its own App and Monero escrow watcher) against a real
`monerod --regtest` and three real `monero-wallet-rpc` 0.18.5.1 instances: `scripts/test-escrow-xmr-regtest.sh`. It runs in
CI (the `escrow-xmr-regtest` job, A-498), refuses any daemon that is not a fakechain with hard fork 16 or later before any wallet call, and needs a
disposable PostgreSQL database in `TEST_DATABASE_URL` (the legs create their own databases from it, as the other database
tests do). The header of the script has the exact `docker` commands; the
[runbook](testnet-runbook.md#9-manual-monero-escrow-fakechain-check) repeats them. The loopback ports it uses by default:

| Port | What |
| --- | --- |
| 28381 | the fakechain `monerod` (`ESCROW_XMR_REGTEST_DAEMON_URL`) |
| 28382 | the escrow wallet-rpc the App uses (`ESCROW_XMR_REGTEST_WALLET_URL`) |
| 28383 | a funder wallet-rpc standing in for the buyer's wallet (`ESCROW_XMR_REGTEST_FUNDER_URL`) |
| 28384 | a wallet-rpc used only to restore a sealed seed (`ESCROW_XMR_REGTEST_RESTORE_URL`) |

The wallet-rpcs listen without RPC login on `127.0.0.1` only, and the transcript is written to `ESCROW_XMR_REGTEST_LOG`
(default `artifacts/real-chain/test-escrow-xmr-regtest-<UTC time>.log`, gitignored). The Go tests of the other tiers need
none of this: without these variables the `Regtest` legs skip, and `go test ./internal/market/` runs the unit and
fake-wallet-rpc tests (the latter with `TEST_DATABASE_URL`). A simulated-RPC browser journey for Monero escrow is added in
this phase; its command is recorded when it merges, and it is not chain evidence.

### Scripts, operations and CI

```sh
python3 -m unittest discover -s tests -p 'test_*.py' -v
for script in scripts/*.sh; do bash -n "$script"; done
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

[CI](../.github/workflows/ci.yaml) also validates Compose profiles, container startup,
upgrades, encrypted backup/restore, dependencies and browser regressions with
PostgreSQL (the `browser` job: preview, database and wallet journeys; the `browser-escrow` job:
the Bitcoin and Monero escrow journeys). See [operations tests](operations-tests.md) for local rehearsals
and [CI/release instructions](ci-cd.md). Version tags produce CI-gated
draft release artifacts; they do not deploy a live instance.
