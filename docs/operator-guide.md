# OPSEC Market operator guide

Deployment, feature configuration, migrations and recovery reference. Start with
[the README](../README.md) for product context and local development. Commands
below run from the repository root.

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

Nodes default to disabled. For a local node, the installer defaults to [`bitcoin/bitcoin:latest`](https://hub.docker.com/r/bitcoin/bitcoin) for Bitcoin Core and Seth for Privacy's [`simple-monerod`](https://github.com/sethforprivacy/simple-monerod-docker) plus [`simple-monero-wallet-rpc`](https://github.com/sethforprivacy/simple-monero-wallet-rpc-docker) images for Monero. Press Enter to use these defaults or supply an image override. The Bitcoin image is community-published and explicitly unofficial; it verifies upstream Bitcoin Core binaries. The Monero images are community-maintained, not published by the Monero project. Review publisher and image provenance before use. `latest` follows new releases; set an immutable `@sha256:<64 lowercase hex characters>` digest in `.env` for a reproducible deployment. Bitcoin and Monero use the same daemon image on test and live networks; the network is selected with daemon flags, not by changing the image. The installer offers Bitcoin `testnet4` or `signet`, and Monero `stagenet` or `testnet`. **Live is not supported by the payment adapters:** selecting it exits before writing `.env`, because the application refuses mainnet wallets and addresses. Full live-payment support needs a separate adapter and payout-safety change.

Both nodes store their blockchain in dedicated volumes and publish no ports. They can require substantial disk space and synchronization time. Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payments are implemented for test networks only; mainnet is refused at startup.** With a wallet RPC configured (see [Payments](#payments)), the app issues deposit addresses, watches confirmations and sends single-attempt releases and refunds from the operator's pooled test-network wallet. That is custodial holding, not multisig escrow, and there is no fee accounting. Do not send mainnet coins. Providers are enabled by environment variables and a restart. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

## Migrations

`internal/market/schema.sql` is the frozen baseline. Startup applies it, then every `internal/market/migrations/NNN_name.sql` not yet listed in `schema_migrations`, in numeric order, inside one transaction under an advisory lock, so concurrent app starts are safe and a failed migration changes nothing. A version missing from the table is applied even when higher versions already are. A version in the table that the running server does not include means a newer release migrated the database: the server then exits with an error naming it, before changing or serving anything (see [Rolling back](../UPGRADING.md#5-rolling-back)). Never edit a merged migration; add a new file. Reserved blocks: 001–009 Foundation, 010–019 authentication, 020–029 PGP, 030–039 orders, 040–049 inventory, 050–059 payments, 060–069 transparency. Take an encrypted backup before upgrading; there are no automatic down-migrations.

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

All keys below are listed in `.env.example` and passed by `compose.yaml`. Blank or absent values keep a feature disabled.

### Authentication

No environment keys. Second-factor secrets are encrypted with a key derived from `SETUP_TOKEN`; rotating `SETUP_TOKEN` makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off and enroll again (an administrator resets a non-administrator with no recovery code left under *Admin → Reset a user's second factors*; the administrator's own account needs the operator reset in [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). Startup refuses a placeholder or obviously non-random `SETUP_TOKEN`; use `openssl rand -hex 32`.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code, and 10 one-time recovery codes are shown once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio alternative, so turning it off may be needed for users who cannot read it. First-run setup never shows it.
- **Password change**: users change their own password on *Account → Change password* with the current password (plus an authenticator code or an unused recovery code when TOTP is on). Every other session and pending sign-in of that account ends. There is no email or administrator password reset: a user who forgets their password cannot be recovered through the application.
- **Second-factor reset**: an administrator can turn off TOTP (deleting its secret and recovery codes) and PGP sign-in for a non-administrator account under *Admin → Reset a user's second factors*, confirmed with the administrator's own password (and authenticator code when enrolled). The account's PGP key and verification stay; its sessions and pending sign-ins end, both accounts get an audit row and the user is notified. Anyone who knows that account's password can then sign in, so confirm the request really comes from its owner (for example with a message signed by their verified PGP key) before resetting.
- **Administrator lockout** is not recoverable in the application: the reset refuses the administrator's own account and other administrators. Keep the administrator's recovery codes offline. If they are lost, an operator with database access runs, in `psql` against the application database (replace `ADMIN_HANDLE`):

  ```sql
  BEGIN;
  UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL,pgp_2fa=false WHERE handle='ADMIN_HANDLE' AND role='admin';
  DELETE FROM recovery_codes WHERE user_id=(SELECT id FROM users WHERE handle='ADMIN_HANDLE' AND role='admin');
  DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE handle='ADMIN_HANDLE' AND role='admin');
  DELETE FROM pending_logins WHERE user_id=(SELECT id FROM users WHERE handle='ADMIN_HANDLE' AND role='admin');
  INSERT INTO audit_events(user_id,action) SELECT id,'Second factors reset by the operator in the database' FROM users WHERE handle='ADMIN_HANDLE' AND role='admin';
  COMMIT;
  ```

  Check that the `UPDATE` affected one row, then sign in with the password and enroll TOTP again. A forgotten administrator password is likewise an operator task (a new bcrypt hash written with SQL); the application has no administrator password reset.

### PGP identity

No configuration. Users paste an ASCII-armored public key on `/account`; the fingerprint is shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled or an administrator resets the account's second factors (see Authentication). Vendor pages and order pages show the other party's armored public key, fingerprint and whether ownership is verified, and "Message vendor"/"Contact vendor" links open `/messages?to=<handle>` with the recipient filled in; without a saved key, the page says messages cannot be sent until one is added.

### Orders

No configuration. Requesting payment on an order requires a test-network wallet provider for its currency (see Payments); without one the step is shown as unavailable. Migration 030 adds `deliveries`, `reviews` and `disputes.outcome`. Digital delivery content is stored unencrypted in the database and shown only to the order's buyer and vendor, and read-only to moderators and administrators once the order is disputed; vendors should encrypt sensitive content to the buyer's PGP key before delivering it.

**Shipping addresses are never stored, by design.** There is no address field. For a paid physical order the order page asks the buyer to encrypt their address to the vendor's PGP key in their own PGP application and send it through `/messages`, which accepts only OpenPGP-encrypted messages; the server keeps only that ciphertext. A vendor without a saved key cannot receive addresses until they add one.

Moderators and administrators open the order page of a disputed (or resolved) order they are not party to, read-only, from the moderation desk; buyer and vendor actions stay unavailable to them. Changing a vendor's role to buyer or moderator archives their active listings in the same audited transaction and blocks new drafts and payment requests on them; existing orders continue.

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after a payment provider confirms payment; with payments disabled it is never released automatically.

### Payments

**Test networks only. These payments move no real funds.** The application refuses to start if a node or wallet is on mainnet or disagrees with `BITCOIN_CHAIN`/`MONERO_NETWORK` (blank accepts whichever test network the node reports). A node that is unreachable, still syncing or has no wallet loaded does **not** stop the site: that currency shows as unavailable on the admin page, payment requests for it are refused, and every watcher pass checks it again (reloading the Bitcoin wallet or reopening the Monero wallet after a node restart). A node that reports mainnet after startup disables its currency until the app is restarted. A currency with a blank URL stays disabled, and the site then says "Payments disabled". Step-by-step setup, wallet creation and payout recovery: [docs/testnet-runbook.md](testnet-runbook.md). Upgrading from v0.1.0-alpha.1: [UPGRADING.md](../UPGRADING.md).

- Bitcoin: `BITCOIN_RPC_URL` is a wallet-enabled Bitcoin Core RPC. Credentials go in the URL userinfo (`http://marketplace:PASSWORD@bitcoin:8332`); they are sent as HTTP Basic auth and never logged. `BITCOIN_WALLET` (default `opsecmkt`) must exist; the app loads it if needed. Create it once as shown in the [runbook](testnet-runbook.md#2-create-the-bitcoin-wallet) (inside the compose container `bitcoin-cli` needs `-rpcport=8332 -rpcuser=marketplace`). `BITCOIN_CHAIN` (blank, `testnet4`, `signet` or `regtest`) must match the node when set; the local node service runs `testnet4` when it is blank. `PAYMENT_CONFIRMATIONS_BTC` defaults to 3; use 1 for regtest.
- Monero: `MONERO_WALLET_RPC_URL` is monero-wallet-rpc with its `--rpc-login` credentials as userinfo (for example `http://marketplace:PASSWORD@monero-wallet:18083`), sent as HTTP Digest auth. The local `monero-wallet` service requires `MONERO_WALLET_RPC_PASSWORD` (the installer generates it) and no longer runs with `--disable-rpc-login`. The app uses account 0 and opens a wallet file named `opsecmkt` (empty password) if none is open. Create it first with the `create_wallet` RPC ([runbook](testnet-runbook.md#3-create-the-monero-wallet)). `MONERO_RPC_URL` (the daemon) is optional; when set, its network and sync state are checked too. `MONERO_NETWORK` (blank, `stagenet` or `testnet`) must match when set. `PAYMENT_CONFIRMATIONS_XMR` defaults to 10.
- `PAYMENT_POLL_INTERVAL` (default `30s`, from 1s to 1h) sets how often the single background watcher polls wallets and sends queued payouts. Several app instances can share a database; only one polls at a time.
- `PAYMENT_EXPIRY` (Go duration, default `24h`, from 10m to 720h) is the payment window. An order still awaiting payment that long after its address was issued, with no deposit seen, is cancelled by the watcher and its reserved stock is returned. An order with a deposit still confirming stays open. A buyer can hold at most 3 orders awaiting payment at once. Requesting payment records the listing's current price.

Administrators can pause or enable **new payment addresses** independently for
Bitcoin and Monero under **Admin → New payment intake** after the wallet services
are configured. Each change requires the administrator's password (and TOTP when
enrolled), is audited, and takes effect without restarting. Pausing intake never
stops monitoring existing addresses, partial-payment top-ups, fulfillment,
refunds or payouts. It does not turn off the wallet daemon or bypass network
validation. Migration 052 supplies the persisted per-currency policy.

On an open order, **Remaining to send** subtracts both confirmed and unconfirmed
valid deposits from the required amount. Send only the remainder to the same
address; once it reaches zero, wait for confirmations instead of paying twice.
Locked and conflicted transfers do not count. The order is paid only when the
confirmed total reaches the required amount.

Operational limits to accept before enabling payments:

- Deposits for all orders sit in **one pooled custodial wallet** per currency. This is not multisig escrow. Whoever controls the node wallet controls the funds.
- There is **no commission or fee accounting**. Bitcoin payouts deduct the network fee from the amount sent (`subtractfeefromamount`). Monero payout fees are paid by the pooled wallet on top of the payout, so keep a small test-coin buffer in it.
- Payouts are **single-attempt**. A failed or interrupted wallet call is shown on the admin page and never retried automatically, because the transaction may already have been broadcast. After checking the wallet, an administrator can release a held payout, requeue a failed or stuck one, or record one as sent with its transaction ID (password-confirmed and audited). A failure the wallet rejected with a pre-broadcast error (nothing broadcast) requeues with the password alone; requeueing one whose outcome is unknown (timeout, dropped connection, or a Monero error such as -38 that can follow a submission; see the [runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)) or a stuck send, or releasing a payout held by a restore from backup, also needs an explicit, audited confirmation that the wallet shows no broadcast transaction. A wallet send may take up to 30 seconds (ordinary wallet reads 10 seconds). A shutdown or redeploy lets an in-flight send finish and its outcome be recorded (at most 30 seconds plus 10 to record; even added to the 10-second HTTP drain this stays within the app's 60-second stop grace period).
- The watcher never cancels an order for non-payment unless it read that order's deposit address from the wallet in the same pass, and it skips a currency entirely while its node is still syncing (Bitcoin `initialblockdownload`; Monero daemon `target_height` above `height`, when `MONERO_RPC_URL` is set).
- A credited deposit that later conflicts is flagged to moderators and holds the order's unsent payout. A credited deposit that drops below the confirmation threshold (a reorg) also holds the payout, which resumes automatically once it confirms again. The order state is never reverted automatically.
- Funded orders that are not yet completed, cancelled or resolved stay watched for as long as they are open. A deposit first seen after an order was funded is noted once in the order history and to the buyer and vendor; it is part of the order's single release or refund if it has the confirmation threshold when the order settles; otherwise it is not paid out automatically (see late deposits below).
- Late deposits: closed orders stay watched while their address is less than **30 days** old and a deposit is still confirming or unhandled. Funds that confirm after a cancellation that refunded nothing are refunded to the buyer (blocked until the buyer saves an address). Extra funds on completed or resolved orders, or after a refund was already queued, are flagged to moderators, not paid out. Deposits to older addresses are not seen by the watcher; handle them by hand from the wallet.
- Monero transfers with a non-zero `unlock_time` (locked transfers) are recorded and shown, but never counted toward payment or paid out. The order history notes "locked transfer ignored" once and moderators and the buyer are notified.
- Recipients must save a payout address on their account page. Payouts wait ("blocked") until they do. Saving or removing a payout address needs the current password, plus an authenticator code when TOTP is enrolled.

`scripts/regtest-smoke.sh` is a manual check against a local `bitcoind -regtest` (`BITCOIN_REGTEST_RPC_URL=http://user:pass@127.0.0.1:18443`); see the [runbook](testnet-runbook.md#7-manual-regtest-check-bitcoin). It is not part of CI.

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

For external databases also set `BACKUP_DATABASE_URL`, install Python 3 and PostgreSQL client tools matching or newer than the server. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage. Database dumps do not include onion keys or blockchain volumes, and in particular **not the custodial payment wallets** (the `bitcoin_data` and `monero_wallet` volumes, or your external wallets): back those up on their own (see the [runbook](testnet-runbook.md#6-back-up-the-wallets)). Maintain offline copies and rehearse recovery.

Without `BACKUP_DATABASE_URL`, the script dumps the internal database through `docker compose exec -T db`, so it needs no published database port or host PostgreSQL tools.

Restore into an explicitly named **empty** destination. Stop the application first. For the internal database (the default deployment, which publishes no database port), keep the `db` service running and name a database inside it; client tools run in the container:

```sh
docker compose stop app
AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE=opsecmkt_restored \
  ./scripts/restore.sh backups/market.dump.age
```

A database that does not exist yet is created next to the current one, which stays untouched. Point the app at it by changing the database name in `DATABASE_URL` in `.env` (for example `postgres://opsecmkt:PASSWORD@db:5432/opsecmkt_restored?sslmode=disable`), then `docker compose up -d`. If the `postgres_data` volume itself was lost, `docker compose up -d --wait db` initializes an empty `opsecmkt` database and `RESTORE_INTERNAL_DATABASE=opsecmkt` restores into it with no `.env` change. For an external or otherwise host-reachable server, create an empty database there and restore with Python 3 and compatible PostgreSQL client tools:

```sh
RESTORE_DATABASE_URL='postgres://user:password@localhost/recovery?sslmode=verify-full' \
  AGE_IDENTITY=/secure/backup-key.txt ./scripts/restore.sh backups/market.dump.age
```

Set exactly one of the two destinations. Either way the restore requires typing `RESTORE`, runs in one transaction, and does not drop existing tables: restoring into a database that already has the application's tables fails and changes nothing. The payout protection below is applied by the same SQL in both modes. Restore with the `SETUP_TOKEN` that was in use when the backup was taken, or TOTP secrets in it cannot be read (see [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). To roll back an upgrade, follow [UPGRADING.md](../UPGRADING.md#5-rolling-back).

The script sets a persistent recovery gate that pauses all outbound payouts,
including payouts created after restoration, and converts pending, sending, blocked and held payouts into
manual recovery holds (error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the wallet shows no broadcast transaction for it. A database backup may predate an already-sent payout's creation, so reviewing only
existing payout rows is insufficient. Follow the [complete reconciliation and explicit unlock procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
before allowing user writes or restarting payment sends. Validate recovered accounts, listings, orders and
settings before switching traffic. Never test a restore against your live database.

## Verification and maintenance

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
bash -n scripts/install.sh scripts/backup.sh scripts/restore.sh
docker compose config --quiet
```

`docker compose config` requires `.env` or equivalent environment variables. Startup logs show database connection/migration errors. Review dependencies and rebuild periodically. The Compose examples use official [Go](https://hub.docker.com/_/golang) and [PostgreSQL](https://hub.docker.com/_/postgres) image families; production operators should pin reviewed image digests and plan PostgreSQL major-version upgrades explicitly. Tor is installed from Debian's package repositories; the onion-service configuration follows the [Tor Project setup guide](https://community.torproject.org/onion-services/setup/).

GitHub Actions repeats the race tests, vet, dependency verification, govulncheck and Compose validation using the Go toolchain pinned in `go.mod` (`toolchain` directive), the same version the Docker image builds with. CI supplies PostgreSQL 17 to the integration tests, which use an isolated schema. Locally set `TEST_DATABASE_URL` to a dedicated test database to include those tests. These checks do not substitute for a live Docker/Tor deployment test.

Backup/restore URLs require an explicit host and database. The helper decodes credentials into libpq environment variables so they are not included in process arguments. Standard TLS options are supported; unsupported query options fail closed. Environment variables remain visible to privileged host processes.

The encrypted backup/restore scripts were exercised against an isolated PostgreSQL 16.15 test cluster with age 1.3.2: two rows including Unicode round-tripped, encrypted output had mode 0600, existing backups were refused, a wrong identity failed without creating tables, and restoring into an occupied target rolled back without changing its rows. Both scratch databases and temporary keys/dumps were removed afterward. CI now also runs `scripts/test-internal-db-restore.sh`, which backs up and restores through the PostgreSQL 17 internal-db Compose service with no published port (synthetic tables, payout gate and holds); see [operations-tests.md](operations-tests.md). A full rehearsal on your own deployment, including the application and wallets, is still yours to run.
