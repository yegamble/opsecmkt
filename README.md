# OPSEC Market

A Go and PostgreSQL marketplace with server-rendered HTML and CSS. No React, Node.js frontend, JavaScript bundle, or third-party browser assets are required. The Figma reference informs the dark marketplace layout; templates remain ordinary HTML.

## Run locally

Install Docker Engine/Desktop with Compose v2 and OpenSSL, then run:

```sh
./scripts/install.sh
```

Choose `clearnet` and answer `yes` to local HTTP development. Open `http://127.0.0.1:8080/setup`. Read `SETUP_TOKEN` from the generated, permission-restricted `.env` to bootstrap the first administrator. Keep this file private. The installer refuses to overwrite an existing configuration. For later runs use `docker compose up -d --build`; inspect with `docker compose logs app` and stop with `docker compose down`. Do not add `--volumes` unless intentionally destroying persisted data.

The installer generates independent random database, bootstrap, and optional Bitcoin RPC secrets. Leave the external PostgreSQL URL blank to create the private database container. The internal-database overlay waits for the database health check before starting the app. Supplying an external URL omits that service and the health dependency; provision the database and its restricted application account beforehand, and require TLS for remote connections (`sslmode=verify-full` with appropriate trust configuration). Percent-encode special characters in URL credentials. The app applies its schema during startup, so its initial database account needs migration permissions.

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

Nodes default to disabled. The installer accepts an external RPC URL or a local full-node container for each currency. Local nodes require an **operator-supplied reviewed image**, ideally pinned by digest. Bitcoin's image must provide `bitcoind` on PATH and support a writable `/data`; Monero's must provide `monerod` with the same data-directory support. No arbitrary third-party image is silently trusted. Fetch and verify binaries from [Bitcoin Core](https://bitcoincore.org/en/download/) and [Monero](https://www.getmonero.org/downloads/) if building your own images. Verify upstream signatures as well as hashes. Container defaults and architecture must be reviewed against the selected image.

Both nodes store their blockchain in dedicated volumes and publish no ports. They can require substantial disk space and synchronization time. Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payment execution, deposit monitoring, escrow custody, withdrawals and cryptographic multisignature settlement are not implemented.** Do not send real funds to demonstration addresses or treat displayed balances/statuses as chain-confirmed. RPC environment variables and admin connection settings are configuration placeholders until a tested integration is implemented. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

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
