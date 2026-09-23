# OPSEC Market

A Go and PostgreSQL marketplace with server-rendered HTML and CSS. No React, Node.js frontend, JavaScript bundle, or third-party browser assets are required. The Figma reference informs the dark marketplace layout; templates remain ordinary HTML. Payments, when configured, run on Bitcoin and Monero **test networks only**; see [implementation status](docs/implementation-status.md).

## Run locally

Install Docker Engine/Desktop with Compose v2 and OpenSSL, then run:

```sh
./scripts/install.sh
```

Choose `clearnet` and answer `yes` to local HTTP development. Open `http://127.0.0.1:8080/setup`. Read `SETUP_TOKEN` from the generated, permission-restricted `.env` to bootstrap the first administrator. Keep this file private. The installer refuses to overwrite an existing configuration. For later runs use `docker compose up -d --build`; inspect with `docker compose logs app` and stop with `docker compose down`. Do not add `--volumes` unless intentionally destroying persisted data.

The installer generates independent random database, bootstrap, audit-signing, and optional Bitcoin RPC secrets. Leave the external PostgreSQL URL blank to create the private database container. The internal-database overlay waits for the database health check before starting the app. Supplying an external URL omits that service and the health dependency; provision the database and its restricted application account beforehand, and require TLS for remote connections (`sslmode=verify-full` with appropriate trust configuration). Percent-encode special characters in URL credentials. The app applies its schema during startup, so its initial database account needs migration permissions.

To run without Docker, install Go 1.26 and PostgreSQL 17+, set `DATABASE_URL`, `SETUP_TOKEN`, `COOKIE_SECURE=false` for local HTTP, and run `go run ./cmd/server` from the repository root. The runtime reads templates and assets from `web/`.

## Clearnet and onion deployment

For clearnet production, keep `COOKIE_SECURE=true` and terminate HTTPS with a reverse proxy on the same host. The container binds only `127.0.0.1:8080`. A minimal host-installed Caddy configuration is:

```caddyfile
market.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Configure DNS, firewall and the certificate issuer for your domain. A proxy in another container needs a private shared network instead of this host-loopback configuration. Never expose the database or node RPC ports.

For onion service deployment choose `tor` in the installer. The base and Tor Compose files publish **no host ports**; Tor connects to `app:8080` on the private Docker network. After initialization, obtain the address with:

```sh
docker compose exec tor cat /var/lib/tor/marketplace/hostname
```

The persistent `tor_identity` volume contains the onion private keys. Preserve it to retain the address; encrypt any offline copy. The setup uses HTTP inside the authenticated onion circuit and sets `COOKIE_SECURE=false` for that endpoint. The application and database share an internal Docker network with no direct Internet egress. Only the Tor gateway has a separate egress network. Selecting external database or RPC endpoints adds `compose.external-egress.yaml`, which explicitly permits direct application egress; that configuration provides onion-only inbound exposure, not Tor-routed outbound traffic. Local full nodes have their own direct egress network to synchronize with peers. Docker network isolation does not protect against host administrators or a compromised container runtime. Use the dedicated Tor Compose file without the clearnet overlay. If changing an existing clearnet deployment, stop it before changing `COMPOSE_FILE` and recreate containers and the backend network so published ports are removed and the network becomes internal. Remove only the unused network, not persistent volumes. Never run both overlays together.

## Optional onion mirror

In a Tor deployment, create a second onion address with its own persistent identity:

```sh
docker compose --profile mirror up -d tor-mirror
docker compose exec tor-mirror cat /var/lib/tor/marketplace/hostname
```

Wait for the mirror's Tor process to initialize before reading its hostname. This optional profile publishes no host ports and reaches the same application/database over the private backend network. The second address is a same-host alternate URL; it does not provide independence from host, application or database failures. Preserve and encrypt the separate `tor_mirror_identity` volume to retain its address. Add `mirror` to `COMPOSE_PROFILES` in `.env` when you want ordinary deployment commands to include it.

Publish both addresses in an operator-signed verification document distributed through a trusted channel. Users should verify that signature with a previously trusted operator key; merely displaying an address or a “canonical” label on a page does not authenticate a mirror. Signing and distribution remain operator tasks.

## Optional cryptocurrency nodes

Nodes default to disabled. The installer accepts an external RPC URL or a local full-node container for each currency. Local nodes require an **operator-supplied reviewed image**, ideally pinned by digest. Bitcoin's image must provide `bitcoind` on PATH and support a writable `/data`; Monero's must provide `monerod` with the same data-directory support, plus `monero-wallet-rpc` for the `monero-wallet` service the installer enables alongside a local Monero node. Local nodes run on test networks only (`BITCOIN_CHAIN`, default `testnet4`, which needs Bitcoin Core 28 or newer; `MONERO_NETWORK`, default `stagenet`). No arbitrary third-party image is silently trusted. Fetch and verify binaries from [Bitcoin Core](https://bitcoincore.org/en/download/) and [Monero](https://www.getmonero.org/downloads/) if building your own images. Verify upstream signatures as well as hashes. Container defaults and architecture must be reviewed against the selected image.

Both nodes store their blockchain in dedicated volumes and publish no ports. They can require substantial disk space and synchronization time. Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payments are implemented for test networks only; mainnet is refused at startup.** With a wallet RPC configured (see [Payments](#payments)), the app issues deposit addresses, watches confirmations and sends single-attempt releases and refunds from the operator's pooled test-network wallet. That is custodial holding, not multisig escrow, and there is no fee accounting. Never send mainnet coins. The admin page's node-mode fields only record a desired configuration; providers are enabled by environment variables and a restart. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

## Migrations

`internal/market/schema.sql` is the frozen baseline. Startup applies it, then every `internal/market/migrations/NNN_name.sql` not yet listed in `schema_migrations`, in numeric order, inside one transaction under an advisory lock, so concurrent app starts are safe and a failed migration changes nothing. A version missing from the table is applied even when higher versions already are. Never edit a merged migration; add a new file. Reserved blocks: 001–009 Foundation, 010–019 authentication, 020–029 PGP, 030–039 orders, 040–049 inventory, 050–059 payments, 060–069 transparency. Take an encrypted backup before upgrading; there are no automatic down-migrations.

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

All keys below are listed in `.env.example` and passed by `compose.yaml`. Blank or absent values keep a feature disabled.

### Authentication

No environment keys. Second-factor secrets are encrypted with a key derived from `SETUP_TOKEN`; rotating `SETUP_TOKEN` makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off and enroll again.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code, and 10 one-time recovery codes are shown once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio alternative, so turning it off may be needed for users who cannot read it. First-run setup never shows it.

### PGP identity

No configuration. Users paste an ASCII-armored public key on `/account`; the fingerprint is shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled.

### Orders

No configuration. Requesting payment on an order requires a test-network wallet provider for its currency (see Payments); without one the step is shown as unavailable. Migration 030 adds `deliveries`, `reviews` and `disputes.outcome`. Digital delivery content is stored unencrypted in the database and shown only to the order's buyer and vendor; vendors should encrypt sensitive content to the buyer's PGP key before delivering it.

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after a payment provider confirms payment; with payments disabled it is never released automatically.

### Payments

**Test networks only. These payments move no real funds.** The application refuses to start if a node or wallet is on mainnet, if it disagrees with `BITCOIN_CHAIN`/`MONERO_NETWORK`, or if a configured wallet cannot be reached. A currency with a blank URL stays disabled, and the site then says "Payments disabled".

- Bitcoin: `BITCOIN_RPC_URL` is a wallet-enabled Bitcoin Core RPC. Credentials go in the URL userinfo (`http://marketplace:PASSWORD@bitcoin:8332`); they are sent as HTTP Basic auth and never logged. `BITCOIN_WALLET` (default `opsecmkt`) must exist; the app loads it if needed. Create it once with `bitcoin-cli -chain=testnet4 createwallet opsecmkt`. `BITCOIN_CHAIN` (default `testnet4`; also `signet`, `regtest`) must match the node. `PAYMENT_CONFIRMATIONS_BTC` defaults to 3; use 1 for regtest.
- Monero: `MONERO_WALLET_RPC_URL` is monero-wallet-rpc (for example `http://monero-wallet:18083`; optional userinfo is sent as HTTP Digest auth). The app uses account 0 and opens a wallet file named `opsecmkt` (empty password) if none is open. Create it first with the `create_wallet` RPC. `MONERO_RPC_URL` (the daemon) is optional; when set, its network is checked too. `MONERO_NETWORK` (default `stagenet`; also `testnet`) must match. `PAYMENT_CONFIRMATIONS_XMR` defaults to 10.
- `PAYMENT_POLL_INTERVAL` (default `30s`, from 1s to 1h) sets how often the single background watcher polls wallets and sends queued payouts. Several app instances can share a database; only one polls at a time.

Operational limits to accept before enabling payments:

- Deposits for all orders sit in **one pooled custodial wallet** per currency. This is not multisig escrow. Whoever controls the node wallet controls the funds.
- There is **no commission or fee accounting**. Bitcoin payouts deduct the network fee from the amount sent (`subtractfeefromamount`). Monero payout fees are paid by the pooled wallet on top of the payout, so keep a small test-coin buffer in it.
- Payouts are **single-attempt**. A failed or interrupted wallet call is shown on the admin page and never retried, because the transaction may already have been broadcast. Check the wallet before paying anything by hand.
- A credited deposit that later conflicts is flagged to moderators and holds the order's unsent payout. The order state is never reverted automatically. Deposits arriving after settlement are flagged, not paid out.
- Recipients must save a payout address on their account page. Payouts wait ("blocked") until they do.

`scripts/regtest-smoke.sh` is a manual check against a local `bitcoind -regtest` (`BITCOIN_REGTEST_RPC_URL=http://user:pass@127.0.0.1:18443`). It is not part of CI.

### Transparency

`AUDIT_SIGNING_KEY` (64 hex characters, an Ed25519 seed generated by the installer with `openssl rand -hex 32`) enables signed audit exports. It is read only from the environment, never stored in the database; blank or malformed values leave the export visibly unavailable. Changing it changes the public key, so treat it like any long-lived signing key: back it up with `.env` and announce rotations.

**Pin the audit public key.** The admin page and `/canary` show the hex Ed25519 public key. Record it once through a channel you trust (for example in the operator-signed canary text) and verify every export against that pinned value, not against the key printed in the `.sig` file:

```sh
go run ./cmd/verify-audit -pub <pinned-hex-key> audit-events-upto-N.jsonl audit-events-upto-N.sig
```

The export for a given `upto` is byte-for-byte reproducible, so two downloads can be compared. **A server compromise can produce validly signed exports**: whoever controls the server or `AUDIT_SIGNING_KEY` can sign an altered history. The signature proves origin from the configured key, not that the log is complete or untampered.

**Warrant canary.** In the admin page, paste the operator's OpenPGP public key, then paste a statement the operator wrote and signed offline (`gpg --clearsign canary.txt`). The application stores it as pasted only if it verifies, re-verifies it on every view of `/canary`, and shows INVALID with the reason if the key changes or the stored text is altered. Keep the operator's private key off the server; the application never writes or signs canary text.

## Backups and recovery

Install [age](https://age-encryption.org/) and generate an identity offline (`age-keygen -o backup-key.txt`). Store the private key separately from backups and use its public recipient for encryption:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/market.dump.age
```

For external databases also set `BACKUP_DATABASE_URL`, install Python 3 and PostgreSQL client tools matching or newer than the server. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage. Database dumps do not include onion keys or blockchain volumes. Maintain offline copies and rehearse recovery.

Restore into an explicitly provisioned **empty** destination, with Python 3 and compatible PostgreSQL client tools:

```sh
RESTORE_DATABASE_URL='postgres://user:password@localhost/recovery?sslmode=verify-full' \
  AGE_IDENTITY=/secure/backup-key.txt ./scripts/restore.sh backups/market.dump.age
```

The restore requires typing `RESTORE`, runs transactionally, and does not drop existing tables. Validate recovered accounts, listings, orders and settings before pointing the application at it. Shut down writes or restore into a separate instance during recovery. Never test a restore against your live database.

## Verification and maintenance

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
bash -n scripts/install.sh scripts/backup.sh scripts/restore.sh
docker compose config --quiet
```

`docker compose config` requires `.env` or equivalent environment variables. Startup logs show database connection/migration errors. Review dependencies and rebuild periodically. The Compose examples use official [Go](https://hub.docker.com/_/golang) and [PostgreSQL](https://hub.docker.com/_/postgres) image families; production operators should pin reviewed image digests and plan PostgreSQL major-version upgrades explicitly. Tor is installed from Debian's package repositories; the onion-service configuration follows the [Tor Project setup guide](https://community.torproject.org/onion-services/setup/).

GitHub Actions repeats the race tests, vet, dependency verification, govulncheck and Compose validation using the latest Go 1.26 patch release. CI supplies PostgreSQL 17 to the integration tests, which use an isolated schema. Locally set `TEST_DATABASE_URL` to a dedicated test database to include those tests. These checks do not substitute for a live Docker/Tor deployment test.

Backup/restore URLs require an explicit host and database. The helper decodes credentials into libpq environment variables so they are not included in process arguments. Standard TLS options are supported; unsupported query options fail closed. Environment variables remain visible to privileged host processes.

The encrypted backup/restore scripts were exercised against an isolated PostgreSQL 16.15 test cluster with age 1.3.2: two rows including Unicode round-tripped, encrypted output had mode 0600, existing backups were refused, a wrong identity failed without creating tables, and restoring into an occupied target rolled back without changing its rows. Both scratch databases and temporary keys/dumps were removed afterward. The Docker PostgreSQL 17 deployment still needs its own live rehearsal.

## Read-only design preview

```sh
go run ./cmd/server -preview
```

Open `http://127.0.0.1:8080`. This preview uses labeled sample data, binds only to loopback, and rejects all form writes. A normal installation starts with an empty real catalog; the administrator can publish listings or grant a registered buyer the vendor role.

See [implementation status](docs/implementation-status.md) for the exact working and unavailable features, and [acceptance checks](docs/acceptance.md) for remaining release gates. Use Go **1.26.8 or newer**; the earlier local 1.26.2 standard library produced known vulnerability findings. The Docker build pins 1.26.8 and CI tracks the current 1.26 patch release.

## CI/CD and browser regression tests

The private [GitHub repository](https://github.com/yegamble/opsecmkt) runs [CI](https://github.com/yegamble/opsecmkt/actions/workflows/ci.yaml) on every branch push and pull request. Tests cover Go/PostgreSQL, container installation with both database modes, deployment configurations, encrypted restore, and Playwright across Chromium, Firefox and WebKit. JavaScript is disabled in browser tests. Node is needed only for development tests, not the application image.

```sh
npm ci
npx playwright install chromium firefox webkit
npm run test:e2e
```

For the real account/order browser journeys, also set `E2E_DATABASE_URL` to a **fresh, disposable** PostgreSQL database. The `db-setup` project initializes it once; every other `tests/e2e/db-*.spec.ts` depends on it and creates its own uniquely named accounts. CI requires it. Never use a production database. Test servers use loopback ports 18080 and 18081 and do not reuse your existing preview.

Version tags produce CI-gated **private draft release artifacts**, not a live deployment. See [CI and release instructions](docs/ci-cd.md) and [operations test instructions](docs/operations-tests.md).
