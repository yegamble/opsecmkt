# OPSEC Market operator guide

Deployment, feature configuration, migrations and recovery reference. Start with
[the README](../README.md) for product context and local development. Commands
below run from the repository root.

For threat boundaries, offline keys, private administrator ingress and reviewed
deployment packages, read [Security and OPSEC operations](security-operations.md).

**Current build:** Monero (XMR) per-order custodial escrow is the only payment path. It is off by default. Native `opsecmkt deploy init` defaults to stagenet; the legacy `scripts/install.sh` chooses mainnet for local nodes unless stagenet is selected. Mainnet has real funds and no recorded real-funds transaction. Monero testnet is unsupported. Start with [Enabling Monero payments](install/enable-payments.md).

## Native production storage and restarts

New native production deployments require a verified LUKS2-encrypted Linux root
before initialization creates keys. The controller checks local Docker/containerd,
installation, private inputs and retention storage, then records expected mount
and filesystem identities with their LUKS UUIDs. `install`, `up`, `redeploy` and
`doctor` recheck those bindings. Run the
CLI on the service host against its local Docker Engine. Follow the
[encryption-first setup](install/server-hardening.md#prepare-encryption-before-running-setup)
before transferring credentials. The legacy shell installer below does not
implement this native storage gate.

Production starts require Linux root, systemd, no active swap and a persistent
root-owned binary. They install Docker/containerd pre-start drop-ins and reload
systemd without restarting either daemon. Keep the operator installation on
persistent encrypted storage; the new starts reject the former tmpfs/RAM-unlocked
production layout. Development RAM workflows remain available. Native gateway,
payment and backup units need separately reviewed storage dependencies.

Existing unencrypted hosts need a planned migration to fresh encrypted roots;
installing the new executable does not encrypt them. After restoring the retained
state on verified replacement storage, run `deploy storage bind --dir DIR`,
install reviewed startup checks and complete the recovery tests. Preserve keys,
onion identities, wallet state and the current external retention ledger. See the
[migration procedure](install/server-hardening.md#migrate-an-existing-unencrypted-installation).

### Restarting services while disks remain unlocked

A service-only restart does not need another disk passphrase while its encrypted
storage remains mounted. For a controlled restart of the native application-host
installation:

1. Record the current payment-intake and recovery gates. Pause intake for planned
   maintenance when required, and allow in-flight application work to stop
   gracefully. Keep a verified backup and the current retention ledger.
2. Check the saved installation and storage:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy plan --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy doctor --dir /srv/opsecmkt/market-prod
   ```

3. Stop its configured services, retaining keys and persistent volumes:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-prod
   ```

4. Resume the same installation:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy up --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy status --dir /srv/opsecmkt/market-prod
   ```

   `up` rechecks storage before writing runtime files or starting services. It
   does not provision the gateway, managed database, payment host or worker.
5. Verify application health through the intended gateway, private administrator
   access, required RPC/database connections and current payment readiness. Keep
   intake paused until the required checks pass; a restart must not automatically
   reopen intake or clear a recovery gate.

If a start fails, retain the directory and inspect `plan`, `status` and bounded
diagnostics. Correct the identified storage or service error and retry `up`.
An unexpected storage UUID requires investigation; `storage bind` is a reviewed
migration step, not a response to every startup failure.

### Recovering after a full host reboot

This is the target runbook for an encrypted host whose operating-system unlock
and service dependencies have passed rehearsal. Preboot SSH or console unlocking
must be configured and tested through the operating system/provider; the Go
controller does not install that transport or supply its passphrase.

1. For planned maintenance, record the host, expected LUKS identities, current
   health, intake/recovery gates and rollback. Confirm off-host unlock material
   and authenticated recovery access before approving the reboot. Quiesce the
   affected application/wallet role consistently and allow a graceful shutdown.
2. Reboot through the host's supported service manager or provider procedure.
   With the encrypted root locked, application services remain unavailable until
   an authorized operator unlocks it.
3. Authenticate the recorded preboot SSH identity or recovery console, then
   unlock the expected root through the OS procedure. If additional state
   volumes require manual unlocking, use their recorded device and UUID with
   `deploy storage unlock`; do not run `prepare` on an existing volume.

   Prefer the interactive SSH or console unlock prompt. For a separately tested
   scripted unlock, send the protected key file's exact bytes: do not add an
   `echo` newline, carriage return or other delimiter. Debian's
   [`cryptroot-unlock` implementation](https://sources.debian.org/src/cryptsetup/2%3A2.7.5-2/debian/initramfs/cryptroot-unlock/#L186)
   copies noninteractive standard input verbatim. Keep unlock material out of
   command arguments and logs. Failed attempts can exhaust the current boot's
   retry budget and leave root locked. Stop and inspect the failure; an approved
   restart may be needed to repeat the planned rehearsal. Exhausted retries are
   not an automatic data-erasure action. Do not claim unlock success until the
   running OS and its expected storage have been verified.

   A scripted wrapper must keep unrelated child processes from consuming the
   unlock stream. In Python, give those children `stdin=subprocess.DEVNULL`;
   pass the protected bytes only to the designated unlock connection. If a
   wrapper fails before that submission, inspect the existing locked boot and
   use a separately reviewed recovery without automatically rebooting again.
4. Verify the expected mounts and saved bindings **without needing Docker**:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy storage check-bound --dir /srv/opsecmkt/market-prod
   ```

   Expect `expected LUKS2 storage available`. Missing, locked or changed volumes
   must refuse startup without creating fallback state.
5. Let the reviewed supervisors resume services. If Docker/containerd previously
   failed while waiting for manual state-volume unlock, clear those failed unit
   states and start them **after** step 4 succeeds:

   ```sh
   sudo systemctl reset-failed containerd.service docker.service
   sudo systemctl start containerd.service docker.service
   sudo /usr/local/bin/opsecmkt deploy doctor --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy up --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy status --dir /srv/opsecmkt/market-prod
   ```

   The installed pre-start checks run again. Confirm gateway health, unchanged
   onion identities, authorized admin access, database TLS, retention state and
   payment readiness before restoring the approved intake policy. Check backup
   scheduling and scanner readiness separately.

   If private app-host reboot control is configured, root must acknowledge this
   new boot after those checks succeed. Run `deploy host-reboot ready --dir DIR
   --confirm-unlock-rehearsed` with the retained installation path. An earlier
   boot's acknowledgement cannot enable another web reboot automatically.

A failed unlock or check leaves services stopped and produces an error for the
operator. Ordinary restart/recovery does not format disks, erase keyslots or
invoke emergency self-destruction. `down` retains persistent data. Use the
[backup and recovery procedure](#backups-and-recovery) when storage is damaged;
do not regenerate keys or erase a volume to make a boot pass.

### Enabling the private application-host reboot control

This optional control reboots the **application host's operating system**. The
storefront and private administrator connection disconnect. Disk unlocking still
requires the operator's separate trusted console or preboot SSH procedure. Plan
an attended maintenance window and keep payment intake at its approved setting.

You need Linux root on the encrypted application host, a persistent installed
binary and the retained Tor production installation with its client-authorized
private administrator onion.
The administrator must enroll TOTP. Keep verified backups, current retention
state and off-host unlock material available. The existing unencrypted production
hosts do not meet these prerequisites.

1. Configure the narrow host service on the application host:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy host-reboot configure \
     --dir /srv/opsecmkt/market-prod
   ```

   Expect a configured agent with a new **disabled** unlock-readiness policy.
   Its root-owned policy and receipt ledger stay on verified encrypted storage.
   It accepts only the saved application's numeric UID through a dedicated
   Unix socket. The app receives no Docker/systemd socket, root credential or
   unlock key.
2. Attach that socket to the retained application installation:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy up \
     --dir /srv/opsecmkt/market-prod
   ```

   Open **Admin > System & audit > Application host** (`/admin/system`) through the client-authorized private
   administrator onion. Expect an unavailable reboot button until the host
   policy and the administrator's authenticator are ready.
3. Complete the [full host reboot and manual-unlock rehearsal](#recovering-after-a-full-host-reboot)
   through the separate operator procedure. Verify the expected mounts, storage
   bindings, unchanged onion identities, services and recovery/payment gates
   after unlock. A synthetic test or a service-only restart does not establish
   this readiness.
4. Record that completed rehearsal on the application host:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy host-reboot ready \
     --dir /srv/opsecmkt/market-prod --confirm-unlock-rehearsed
   ```

   This stores the operator's acknowledgement for the **current OS boot**;
   the flag cannot prove that a reboot, unlock or recovery test happened. Each
   later OS reboot or replacement host needs a new acknowledgement after manual
   unlock and verification. The agent checks storage, boot identity and policy
   again before accepting a request and before executing it.
5. Reload **Admin > System & audit > Application host**. Expect the enabled form only when the
   local agent reports readiness and the administrator has TOTP enrolled.
   Review and retain the displayed request ID before submitting.

To request a planned reboot, select the downtime/manual-unlock acknowledgement,
enter the current password and a fresh authenticator code, then select **Reboot
application host OS**. The application commits the request and its audit record
before contacting the host agent. The agent saves an acceptance receipt before
replying, provides at least 30 seconds after that response, and enforces a
10-minute cooldown. The action has one fixed application-host target. Gateway,
payment and managed database maintenance use their own procedures.

An acceptance receipt means a reboot was scheduled. It does not prove shutdown,
unlock or service recovery. If a response is lost, retain the **same request ID**
and inspect the admin status or ask the operator to check the root-owned agent
ledger. Do not submit a fresh request as a retry. Form replay never sends another
reboot; uncertain dispatch and agent restarts do not replay it automatically.
After shutdown, use the [full host recovery procedure](#recovering-after-a-full-host-reboot)
to unlock storage and verify services before restoring the approved intake policy.
Re-acknowledge that new boot from the host before expecting the reboot button to
be enabled again. Preserve the existing agent receipt history during recovery.

The root agent delegates human authorization to the application UID. A compromised
application can misuse this limited reboot capability; it gains no general
command execution or disk-unlock authority through the socket. Treat enabling
the feature as an explicit operator decision.

## Installer

`scripts/install.sh` installs a checkout and maintains it afterwards. It fetches nothing itself (Docker builds and pulls the images). After `init` it changes `.env` only by rewriting the lines it names through a temporary file moved into place (mode `0600`; every other line stays byte for byte). It prints one secret, the setup token, and only at the end of `init` and from `setup-token`.

| Command | What it does |
| --- | --- |
| `init` (default) | Refuses an existing `.env` or project volumes, asks for exposure/database/Monero settings, writes configuration and starts services. `--local` selects loopback HTTP, internal PostgreSQL and no node. |
| `init --admin HANDLE [--site-name NAME]` | The same, but creates the first administrator in the terminal ([First administrator from the host](#authentication)): it asks for the password twice without showing it, refuses one that does not match, is not 12-72 bytes or is not valid UTF-8 before it writes or starts anything, and pipes it to `-bootstrap-admin` on standard input, never in a command line, the environment or `.env`. Then it removes `SETUP_TOKEN` from `.env`, recreates the app and prints no token. |
| `check` | Validates `.env` and the Compose configuration and starts nothing: the app's startup refusals (placeholder or weak secrets, `COOKIE_SECURE`, a placeholder `DATABASE_URL`, `APP_MODE` against `COMPOSE_FILE`), [`compose.nodes-tor.yaml`](#local-nodes-over-tor) only with `APP_MODE=tor` and `compose.tor.yaml` and after `compose.nodes.yaml`, `ESCROW_XMR_TOR_RELAY` blank or `clearnet-accepted` with Monero escrow on (and a warning when `APP_MODE=tor` and `ESCROW_XMR=on` would hold Monero relays: neither that value nor `compose.nodes-tor.yaml`, A-515), a warning when `APP_MODE=tor` runs the local Monero node (`monero` profile) without `compose.nodes-tor.yaml` (A-530), `POSTGRES_PASSWORD` against the password in `DATABASE_URL`, `COMPOSE_PROFILES` against the database and node settings, node images on `latest`, the mirror outside Tor, the [malware scanner](#photo-uploads-and-the-malware-scanner) (its profile, `CLAMD_ADDRESS` and `compose.scanner.yaml` only together, a readable `deploy/clamd.conf`, `CLAMAV_IMAGE` pinned to a release with signatures and, from `docker compose config`, `clamd` alone with the app on the internal `scanner` network with no port or volume), the project's containers created from another checkout (or this checkout's containers under another project name), and `docker compose config`. When the directory those containers were created from no longer exists, the checkout was probably moved: `check` says so, and `docker compose up -d --force-recreate` run in the moved checkout recreates them with its directory, keeping the volumes (a plain `up -d` leaves them as they are). Do not change `COMPOSE_PROJECT_NAME` for this. Messages name keys, never values. |
| `status` | Mode, each service's health, whether setup is pending or complete, the schema version, every onion address and the malware scanner's state with the version and date of its signatures. |
| `setup-token [--new]` | While setup is pending: shows the token and setup link, or with `--new` issues a new token and recreates the app. After setup it exits 1: the token grants nothing then. It never shows a token that is also the key root (an install without its own `APP_SECRET_KEY`); `--new` issues a separate one and keeps the keys. |
| `add-mirror` | See [Optional onion mirror](#optional-onion-mirror). |
| `add-scanner`, `refresh-scanner` | Add the ClamAV malware scanner that photo uploads need, and refresh its signatures: see [Photo uploads and the malware scanner](#photo-uploads-and-the-malware-scanner). |
| `upgrade [--no-backup]` | After `git pull`: see [UPGRADING.md](../UPGRADING.md). |

Flags answer installer questions: `--tor`, `--clearnet`, or `--local`; `--external-db URL`; `--nodes local|none|custom`; and the Monero network, image, pruning and escrow options. See the [installer reference](install/installer-reference.md).

The setup token opens `/setup` once. After setup, `status`, `add-mirror` and `upgrade` remove it from `.env` when `APP_SECRET_KEY` is set and the running app reports `keyroot=app_secret_key` (the next `docker compose up -d` drops it from the running app); it grants nothing by then. An app started before `.env` had `APP_SECRET_KEY` still derives its keys from the token, so the token stays until the app is recreated.

Commands that change `.env` (`init`, `setup-token --new`, `add-mirror`, `add-scanner`, `upgrade`, and `status` when it removes the token) hold the lock directory `.env.lock` while they run, so two of them cannot overwrite each other's changes; a second one refuses with a message (`status` only leaves the token). If a killed run left the lock behind, remove it with `rmdir .env.lock`. The installer switches off shell tracing, so `SHELLOPTS=xtrace` cannot print its secrets.

### Recover after a failed first install

`init` writes `.env` before pulling or starting the services. If a pinned image cannot be pulled or a service does not become healthy, the installer leaves `.env` and the data in place; a second `init` correctly refuses to overwrite that configuration. Keep this `.env`: it contains the secrets and Compose project settings for this installation. Check the startup error with `docker compose ps` and `docker compose logs`, correct the affected pinned image or service/profile settings in `.env` if needed, then run:

```sh
./scripts/install.sh check
docker compose up -d --build --wait
```

Run the Compose command from this checkout, with the same `.env`; it retries startup without generating new secrets or selecting a new project. Do not remove `.env` to get past the `init` refusal, and do not remove volumes: they may already contain this installation's database or wallet data. If the failure was a temporary registry or network problem, fix or wait for that cause and repeat the check and `up` command.

## Clearnet and onion deployment

For clearnet production, keep `COOKIE_SECURE=true` and terminate HTTPS with a reverse proxy on the same host. The app is published only on host loopback, at `127.0.0.1:${APP_PORT}` (`APP_PORT` in `.env`, default `8080`; inside its container the app always listens on 8080). A minimal host-installed Caddy configuration for the default port is below. If you chose another `APP_PORT`, use that port instead of 8080:

```caddyfile
market.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

**The proxy must forward the original `Host` header.** The app refuses a form POST whose `Origin` (which browsers send with every form) names a different host from the `Host` header it receives; it does not read `X-Forwarded-Host`. Caddy's `reverse_proxy` forwards `Host` by default. nginx's `proxy_pass` does not: it sends the upstream address (`127.0.0.1:8080`) instead, and every form, including `/setup`, then fails with 403 "Origin does not match Host; if this market runs behind a reverse proxy, the proxy must forward the Host header." A minimal nginx server block, with the certificate paths for your domain:

```nginx
server {
    listen 443 ssl;
    server_name market.example.com;
    ssl_certificate     /etc/letsencrypt/live/market.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/market.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
}
```

`$host` drops the port. If visitors reach the proxy on a non-default port (for example `https://market.example.com:8443`), use `proxy_set_header Host $http_host;` so the forwarded `Host` keeps the port their browser puts in `Origin`. Other proxies need their equivalent setting (for example `ProxyPreserveHost On` in Apache). The onion services (including the mirror) need nothing: Tor's `HiddenServicePort` forwards the connection unchanged, so the app receives the `.onion` `Host` the browser sent.

`APP_MODE` is `clearnet` or `tor`; any other value stops startup. In Tor mode, Monero settlement relay is held until explicit node Tor configuration or explicit acceptance of direct network exposure. This does not encrypt stored order metadata or make custody offline.

Configure DNS, firewall and the certificate issuer for your domain. A proxy in another container needs a private shared network instead of this host-loopback configuration. Never expose the database or node RPC ports.

**External PostgreSQL and the database encoding.** The database must be UTF8. Startup checks `SHOW server_encoding` before it runs the first migration and, for any other encoding (`SQL_ASCII`, `LATIN1`), exits with `the database encoding is SQL_ASCII, not UTF8 ...` and changes nothing. The length limits (for example the 250-character image description), handle rules, case folding and sorting all assume characters; on `SQL_ASCII` a limit counts bytes, so 250 "é" is refused there and accepted on UTF8. The internal PostgreSQL of the Compose files is initialised with `--encoding=UTF8`. On an external server create the database like this (from `psql` as a role with `CREATEDB`; `TEMPLATE template0` is what lets you choose an encoding the server's default differs from):

```sql
CREATE DATABASE opsecmkt ENCODING 'UTF8' TEMPLATE template0;
```

Check an existing database with `export DATABASE_URL` and `python3 scripts/postgres-tool.py DATABASE_URL psql -c 'SHOW server_encoding'` (it must print `UTF8`; the tool keeps the URL's password off the command line, where other users of the host could read it), or `SELECT datname, pg_encoding_to_char(encoding) FROM pg_database;` for all of them. The encoding of a database cannot be changed in place: to move a non-UTF8 one, follow [UPGRADING.md section 42](../UPGRADING.md#42-database-encoding-must-be-utf8-a-481), which checks the old database for text that is not valid UTF-8 (`scripts/check-utf8.sql`), copies the text byte for byte and proves the new database's own backup restores before the switch. Every connection the app opens also sends `client_encoding=UTF8`, overriding the database and role defaults and any `client_encoding` in `DATABASE_URL`.

**External PostgreSQL and time zones.** Every database connection the app opens (the web app, its migrations, `-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors`, `-set-role`, `-set-restore-gate` and `-clear-restore-gate`) sets `TimeZone=UTC` itself, overriding the server, database and role defaults and any `TimeZone` in `DATABASE_URL`. An external or host PostgreSQL left on local time therefore needs no change: pages show every time in UTC, labelled " UTC", and do not reveal the server's UTC offset. Other clients, such as `psql` or the backup scripts, still use the server's own zone.

For onion service deployment run `./scripts/install.sh init --tor` (or choose `tor` in the installer). The base and Tor Compose files publish **no host ports**; Tor connects to `app:8080` on the private Docker network. The installer prints the onion address, and `./scripts/install.sh status` shows it later. By hand:

```sh
docker compose exec tor cat /var/lib/tor/marketplace/hostname
```

The persistent `tor_identity` volume contains the onion private keys. Preserve it to retain the address; back it up encrypted with the [documented commands](#secrets-env-and-onion-identities). The setup uses HTTP inside the authenticated onion circuit and sets `COOKIE_SECURE=false` for that endpoint. The application and database share an internal Docker network with no direct Internet egress. Only the Tor gateway has a separate egress network. Selecting external database or RPC endpoints adds `compose.external-egress.yaml`, which explicitly permits direct application egress; that configuration provides onion-only inbound exposure, not Tor-routed outbound traffic. Local nodes have their own direct egress network to synchronize with peers, unless [`compose.nodes-tor.yaml`](#local-nodes-over-tor) sends them through a Tor client of their own. Docker network isolation does not protect against host administrators or a compromised container runtime. Use the dedicated Tor Compose file without the clearnet overlay. If changing an existing clearnet deployment, stop it before changing `COMPOSE_FILE` and recreate containers and the backend network so published ports are removed and the network becomes internal. Remove only the unused network, not persistent volumes. Never run both overlays together.

**Tor restarts with the app; this needs Docker Compose 2.17 or later** (check with `docker compose version`). Tor looks up the app's container address once, when it starts ([`deploy/torrc`](../deploy/torrc): `HiddenServicePort 80 app:8080`), and a recreated app container can get a different address, for example when an upgrade adds node services. Tor then keeps forwarding to the old address and the onion stops answering. `compose.tor.yaml` therefore gives `tor` and `tor-mirror` a `restart: true` dependency on the app: whenever Compose recreates the app (`docker compose up -d` after a rebuild or a `.env` change) or restarts it (`docker compose restart app`), it restarts Tor too. The mirror is included only when the command sees the `mirror` profile, so add `mirror` to `COMPOSE_PROFILES` in `.env` if you run it (see [Optional onion mirror](#optional-onion-mirror)). Compose does not restart Tor for `docker compose stop app` followed by `up -d`, and the setting does not repair a Tor that already forwards to an old address. On a Tor install deployed before this change, recreate Tor once:

```sh
docker compose up -d --force-recreate tor              # without the onion mirror
docker compose up -d --force-recreate tor tor-mirror   # with the onion mirror
```

Name `tor-mirror` only if you run the mirror: naming it starts it, which creates a second onion address. **If the onion does not answer after any change, recreate Tor** with the same command. Recreating keeps the `tor_identity` and `tor_mirror_identity` volumes, so the addresses stay the same.

## Optional onion mirror

In a Tor deployment, create a second onion address with its own persistent identity. The mirror is the **same instance** reached at another address: one app, one database and one set of escrow wallets and nodes, so orders, accounts and balances are the same rows seen twice, not copies. It is not an independent instance: nothing replicates data or balances to a second server, and two separate installations are two unrelated markets with their own databases and wallets, never safe to point at the same wallets or to treat as one market:

```sh
./scripts/install.sh add-mirror
```

It adds `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there, starts the mirror, waits up to 90 seconds for its address and prints both addresses and the command that backs up the mirror's keys. Add the mirror's address to `OFFICIAL_ADDRESSES` in `.env` so PGP sign-in codes name it ([PGP identity](#pgp-identity)); the installer does not change that setting. It refuses on a clearnet install, and running it again changes nothing. To give the mirror an existing (for example vanity) address instead of a new one, add `--mirror-onion-key-dir DIR --mirror-onion-address ADDR.onion` the first time; the key must differ from the main onion's ([Use your own onion address](install/tor.md#use-your-own-vanity-onion-address)). To do the same by hand, first add `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there: the installer's default Tor install has `COMPOSE_PROFILES='internal-db,monero'`, which becomes `COMPOSE_PROFILES='internal-db,monero,mirror'`; `COMPOSE_PROFILES='internal-db'` becomes `COMPOSE_PROFILES='internal-db,mirror'`, and an empty value becomes `COMPOSE_PROFILES='mirror'`. Then:

<!-- ops-cmd: onion-mirror -->
```sh
docker compose config --services | grep -x tor-mirror   # prints tor-mirror; nothing means .env lacks the profile
docker compose up -d
docker compose exec tor-mirror cat /var/lib/tor/marketplace/hostname
```

Wait for the mirror's Tor process to initialize before reading its hostname. Do not start the mirror with a `--profile mirror` option instead: an option replaces `COMPOSE_PROFILES` for that command rather than adding to it, so with the internal database the command fails (`service "app" depends on undefined service "db"`), and a mirror started that way is left out of later `docker compose up -d` runs. With the profile in `.env`, every `docker compose` command includes the mirror and Compose restarts its Tor whenever it recreates the app (see [Clearnet and onion deployment](#clearnet-and-onion-deployment)). If you already run a mirror that is not in `COMPOSE_PROFILES`, add it now. This optional profile publishes no host ports and reaches the same application/database over the private backend network. **A mirror is a second onion address for the same instance, not an independent instance:** both addresses serve one application process, one PostgreSQL database and one set of escrow nodes and wallets, so an order, an account or a settlement made through one is the same on the other, and nothing here protects against the loss, seizure or compromise of that host or database. The second address is a same-host alternate URL; it does not provide independence from host, application or database failures. Preserve the separate `tor_mirror_identity` volume to retain its address, and back it up like the main one ([Secrets, .env and onion identities](#secrets-env-and-onion-identities)).

**Mirror versus independent instance.** An independent instance has its own database, its own accounts, orders and
escrow keys and its own node wallets; two of them share nothing and replicate nothing. A mirror shares everything, so it
adds reachability, not redundancy: there is no copy to fail over to, and nothing is replicated between two onion
addresses because there is only one database. If you want a second market, run a second installation with its own
`.env`, database and wallets, and do not give it the first one's escrow wallets: two markets that can spend the same
escrows would not know of each other's settlements.

Publish both addresses in an operator-signed verification document distributed through a trusted channel. Users should verify that signature with a previously trusted operator key; merely displaying an address or a “canonical” label on a page does not authenticate a mirror. Signing and distribution remain operator tasks.

## Photo uploads and the malware scanner

Disputed orders also offer **Dispute image evidence** on the order page. Actual parties can upload at most four JPEG/PNG images per side while the dispute is open. They use the same ClamAV checks, size limits, isolated production gateway and JPEG normalization as listing photos. PDF, Office documents and SVG are refused. Evidence is unencrypted and visible only to the parties and independent case staff; visible identifying information is not removed by metadata stripping. Evidence follows private-order retention and applicable preservation holds.

On the private administrator connection, evidence uses the same password-confirmed, one-use gateway handoff as staff photos. Approved moderators can also authorize their own listing photos without losing their moderation role. A party whose account closure is waiting for unresolved duties can add evidence to an existing open dispute; that exception does not restore listing or avatar uploads. See [case privacy and roles](marketplace-design-review.md).


Listing photos and vendor and staff avatars need ClamAV's malware scanner, `clamd`, an optional service in [`compose.scanner.yaml`](../compose.scanner.yaml). The app stores an upload only after `clamd` has scanned the file it received and found nothing, and refuses uploads whenever it cannot ask. There is no setting that accepts uploads without it: with `CLAMD_ADDRESS` blank, uploads are off. Vendors (and administrators, on any listing) add up to 4 photos on each listing's edit page, under **Photos**, and vendors, moderators and administrators set an avatar on `/account`; each owner can remove their own there (war room A-188). Catalog, product and vendor pages show a published listing's photos and a vendor's avatar to anyone in public mode, or to signed-in viewers in members-only mode; staff avatars follow the staff list rules below (war room A-189). ClamAV checks uploads for known malware; it does not review picture content: staff take down pictures that break the market's rules (below). What an image row keeps and who can see it is in [What the server keeps](implementation-status.md#what-the-server-keeps), which `/canary#records` tells users.

What uploaders see follows the scanner's state on `/admin/system#image-scanner`: *Ready* shows the upload forms; *Unavailable* or *Signatures stale* keeps them on the page but disabled, with "Photo uploads are paused: …" and the reason (never the signature date); *Disabled* (no `CLAMD_ADDRESS`) replaces them with "Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up." Listing details still save in every state, and stored photos and avatars can still be removed.

**With isolated uploads** (`UPLOAD_MODE=isolated`, the [separate upload gateway](install/service-topology.md)), the app never contacts `clamd`: the gateway polls its own scanner every 30 seconds and reports the result to the app over the private mutual-TLS upload backend, with the same client certificate it uses for uploads (war room A-552). The report holds only the scanner's state, its engine and signature versions and age, the gateway's last check and last successful scan, and its 24-hour refusal counts; no file, file name, signature name, account, host, address or URL. The app keeps the latest report in the database, so every app process shows the same panel. `/admin/system#image-scanner` shows it with the time it arrived. When no report has arrived for 3 minutes, the panel says **Scanner status unknown — gateway not reporting** and the admin overview flags the System & audit section. While the report is missing, stale or not *Ready*, the avatar and listing-photo forms are disabled before anyone picks a file, with "Uploads paused: the virus scanner is unavailable." The gateway itself still refuses uploads while its scanner is unavailable or stale. When the panel shows unknown, check that the gateway process is running and can reach the app's upload backend address; its log says `upload gateway: scanner report not delivered (...)` once when reports start failing and `upload gateway: scanner report delivered` once they resume. Refresh the gateway's signatures on the upload host; the `install.sh` scanner commands act only on the Compose scanner of the host they run on. The dependency table's ClamAV row shows the engine version from the report.

**Staff uploads from the private administrator connection** (war room A-551). With isolated uploads and a [private administrator onion](install/tor.md#private-administrator-onion) (`ADMIN_SOCKET`), that connection reaches the app directly, which never accepts original files, and the public gateway refuses administrator sessions. There, the avatar panel (administrators and moderators) and an administrator's listing Photos panel therefore ask for the account password (and authenticator code when enrolled) and offer **Authorize avatar upload** or **Authorize photo upload**. The next page sends only a one-use authorization and the file (plus a photo's description) to the gateway origin configured in `BRANDING_UPLOAD_ORIGIN`, as the logo upload does; it carries no password, code, session or CSRF token. The authorization is bound to the account, the session that confirmed it, the kind of upload and, for a photo, the listing; it lasts 10 minutes, is spent when the gateway presents it, and a newer authorization replaces an unused one. Unused authorizations are held in the app's memory, so restarting the app revokes them. The gateway scans and normalizes the picture and the app stores it as for any other upload; the gateway's result page says whether it was saved, and the account or listing on the private connection shows it after a reload. As with the logo, the gateway may learn the private admin onion's hostname from the browser's Origin header. Without `BRANDING_UPLOAD_ORIGIN` those panels say uploads are unavailable from the private connection instead of showing a form that would fail. Uploads on the public address and single-host installations are unchanged.

Moderators and administrators also choose on `/account` whether to **hide themselves from the public staff list**; everyone is hidden until they untick it, and each change is recorded in their activity log. A hidden staff member's avatar can be shown only to the buyer and vendor of orders whose dispute they decided (and to moderators and administrators). The public staff list itself is off until an administrator turns it on (war room A-189). Changing a moderator's role on `/admin/users` deletes their avatar, which was uploaded under these rules, tells them so and resets their choice to hidden; a vendor you make a moderator, or a former moderator you promote again, starts hidden (PR #28 review).

**Taking a photo down.** Moderators remove a vendor's listing photo or avatar that breaks the market's rules, and administrators any listing photo or avatar (staff avatars and administrators' own listings included), from the moderation desk's **Recent photos** (`/moderator#recent-photos`: the 50 newest images available under the current site-access policy; a moderator sees vendors' only), or from the *Remove photo N* and *Remove this avatar* forms on the product and vendor pages, with a reason of 10–500 characters. The image is deleted from the database at once (there is no hidden copy to restore; the vendor uploads another), the owner is notified with the reason, the vendor sees "Photo removed by staff: <reason>" on the listing's edit page, and both accounts' activity logs record it. Moderators can do nothing else to listings. Copies remain in backups made before the takedown: restoring one brings the image back (see [Backups and recovery](#backups-and-recovery)). Until PostgreSQL reuses the space, the deleted image's bytes also remain in PostgreSQL's data files and write-ahead log, readable by anyone with access to the database server's disk (the `postgres_data` volume for the internal database); the same holds for an owner's removal or an avatar replaced by a new one.

### Turning it on

```sh
./scripts/install.sh add-scanner
```

or `./scripts/install.sh init --uploads yes` on a new install. The host needs **at least 3 GiB of memory** as Docker reports it (`docker info`); the installer refuses a smaller one, changing nothing, and warns below 4 GiB. `clamd` keeps ClamAV's signature databases in memory, about 1 GiB (see the [resource limits](#resource-limits-and-log-rotation)).

`add-scanner` changes three things in `.env` together (in a copy moved into place, so never only some of them): it adds `scanner` to `COMPOSE_PROFILES` and `compose.scanner.yaml` to `COMPOSE_FILE`, and sets `CLAMD_ADDRESS='clamd:3310'`; it also writes `CLAMAV_IMAGE` when `.env` has none. If `check` refuses the result, it puts the previous `.env` back. It then starts `clamd`, waits for it to load its signatures and answer (usually under a minute; it gives up after 10 minutes and names `docker compose logs clamd`), recreates the app so that it joins the scanner network and reads `CLAMD_ADDRESS` (on Tor, Tor restarts with it), and prints the signatures `clamd` reports, for example:

```text
Malware scanner ready. Signatures: ClamAV 1.5.4/28130/Mon Sep 21 06:24:29 2026
```

That is the ClamAV release, the version of its daily signature database and when that database was built. Running `add-scanner` again changes nothing in `.env`. `./scripts/install.sh status` prints the same line (`Malware scanner: answering; signatures ...`), or says the scanner is off or not answering. `check` refuses the profile without `CLAMD_ADDRESS`, `CLAMD_ADDRESS` without the profile, any other address (a remote scanner is not supported) and the profile without `compose.scanner.yaml`.

### How it runs

- **Its own signatures, no downloads.** `clamd` reads the signatures built into its image (`/var/lib/clamav`, read-only). There is no signature volume and no `freshclam`, and the container has no route out of the host in either mode.
- **Isolated.** It is on its own `scanner` network, `internal: true` in clearnet and Tor installs alike, which only the app and `clamd` join. It publishes no port. Nothing depends on it: the market, `/healthz` and the app's healthcheck work while it is down or loading, and only uploads wait. The installer's commands (`init`, `upgrade`, `add-mirror`, `setup-token --new`) wait for every other service to become healthy but only start `clamd`, so a scanner that never becomes healthy does not stop them part-way; `init` then says so and names `docker compose logs clamd`, and `status` shows it.
- **Hardened.** A read-only container running as the image's `clamav` user with every capability dropped and `no-new-privileges`, within 3 GiB, 2 CPUs and 256 processes. Uploads are streamed into a 64 MiB in-memory `/tmp` while they are scanned. The image's own entrypoint is not used: it creates `/run/clamav` and changes the ownership of `/var/lib/clamav`, which a read-only container refuses, and it would start `freshclam`; Compose runs `clamd --foreground --config-file=/etc/clamav/clamd.conf` directly instead.
- **Configured by [`deploy/clamd.conf`](../deploy/clamd.conf),** mounted read-only; each option is commented there. `SHUTDOWN`, `RELOAD` and `STATS` answer `COMMAND UNAVAILABLE`, so nothing on the scanner network can stop it (`restart: unless-stopped` covers a crash). A stream over 9 MiB is refused (the upload limit is 4 MiB), and `AlertExceedsMax` turns any scanning limit a file reaches into a detection, which the app refuses, instead of an `OK` for a file scanned only in part. Image fuzzy hashing and broken-media alerts are off until they are measured on real phone photos (war room A-197). `clamd` logs to Docker's rotated logs; it does not log scans that find nothing. It reads `deploy/clamd.conf` as its own user, so the file must be readable by everyone (`chmod 644 deploy/clamd.conf` if your checkout was made with a restrictive `umask`); `check` says so.

`scripts/test-scanner.sh` runs this service definition with no network at all and checks each point above against the real image ([operations tests](operations-tests.md#malware-scanner-container)).

### Keeping the signatures fresh

ClamAV publishes new signatures every day, but `clamd` has only those its image was built with. ClamAV rebuilds each release tag with newer signatures ("the non-base version will only ever be updated to have newer signature databases", [ClamAV's Docker documentation](https://docs.clamav.net/manual/Installing/Docker.html)). For example, `clamav/clamav:1.5.4` was released on 7 August 2026, and its build of 21 September 2026 carries the daily database of that morning. How often the tags are rebuilt is not documented. To fetch the newest build:

```sh
./scripts/install.sh refresh-scanner
```

It has Docker pull `CLAMAV_IMAGE` again even though the tag is already present, recreates `clamd` when the pull brought a newer build (uploads pause while it loads, usually under a minute), and prints the signatures before and after. `./scripts/install.sh upgrade` runs it whenever the scanner is on (it never turns the scanner on); a failed pull there is a warning, and the upgrade itself still completes.

The installer keeps `CLAMAV_IMAGE_DEFAULT` in `.env` as bookkeeping for upgrades. If `CLAMAV_IMAGE` still equals the recorded default, a later upgrade advances it to that installer's default; a custom tag or digest is preserved. For older installs without the marker, the installer records one only when the image equals the default it currently knows, and does not guess whether another pin was previously managed. This marker is not a cryptographic attestation; `.env` remains operator-controlled configuration.

**Refresh at least monthly.** The upload code will refuse uploads while the signatures are older than a limit, 30 days by default (war room ruling C2), so an install that is neither upgraded nor refreshed stops taking photos. On a clearnet host, a weekly cron entry does it:

```text
17 4 * * 1 cd /srv/opsecmkt && ./scripts/install.sh refresh-scanner >> refresh-scanner.log 2>&1
```

**On Tor** the pull, like every image Docker pulls or builds for `upgrade`, goes directly from the host to Docker Hub, not through Tor. It tells Docker Hub, and anyone watching the host's connection, that this host fetches ClamAV images, nothing about the market. The `clamd` container itself never connects anywhere. If the host must not contact Docker Hub, fetch the image on another machine and carry it over (use `--platform linux/arm64` there if the market's host is ARM):

```sh
docker pull clamav/clamav:1.5.4-debian
docker save -o clamav.tar clamav/clamav:1.5.4-debian
```

and, on the market's host, in its checkout:

```sh
docker load -i clamav.tar
docker compose up -d clamd
./scripts/install.sh status
```

`upgrade` on such a host warns that it could not refresh the scanner; load a new image instead. `freshclam` (downloading signatures from inside a container, directly or over Tor) is not supported; war room A-195 tests it over Tor first, and turning it on would replace the image's signatures for good.

### The image

The default, `clamav/clamav:1.5.4-debian`, is ClamAV 1.5.4 with its signatures on a Debian base. It is published for amd64, arm64 and ppc64le hosts, while the Alpine-based `clamav/clamav:1.5.4` is published for amd64 only. `CLAMAV_IMAGE` in `.env` overrides it. `check` accepts only a `MAJOR.MINOR.PATCH` tag, optionally with `-debian` (upstream rebuilds those only with newer signatures), or an `@sha256` digest. It refuses `latest`, `stable`, `unstable` and feature-release tags such as `1.5`, with or without `-debian`, which move to other releases, and any `_base` tag, which has no signatures (with no volume, `clamd` would have none and exit). Append `@sha256:<digest>` to pin one build, but a digest never receives newer signatures: `refresh-scanner` then changes nothing, and you replace the digest yourself. To try another release before switching to it, run `CLAMAV_IMAGE=clamav/clamav:<tag> ./scripts/test-scanner.sh`, then set it in `.env` and run `./scripts/install.sh refresh-scanner`. The default is written in `compose.scanner.yaml`, `.env.example` and `scripts/install.sh`; change all three together (`tests/test_compose_scanner.py` fails otherwise).

### Troubleshooting and turning it off

- **`status` says the scanner is not answering.** `docker compose ps clamd` shows whether it is starting (the healthcheck allows it 10 minutes), restarting or stopped, and `docker compose logs clamd` why. `docker inspect --format '{{.State.OOMKilled}} {{.RestartCount}}' "$(docker compose ps -q clamd)"` shows whether it ran out of memory; raise `mem_limit` for `clamd` in your own Compose file as for the [other limits](#resource-limits-and-log-rotation).
- **`Can't open/parse the config file /etc/clamav/clamd.conf`** in its log: make `deploy/clamd.conf` readable by everyone (`chmod 644 deploy/clamd.conf`) and run `docker compose up -d clamd`.
- **Turning it off:** remove `scanner` from `COMPOSE_PROFILES` and `compose.scanner.yaml` from `COMPOSE_FILE`, set `CLAMD_ADDRESS=''`, then run `./scripts/install.sh check` and `docker compose up -d --remove-orphans`, which removes the `clamd` container and recreates the app without the scanner network. Uploads are then off.

## Denial-of-service defences

An onion service cannot see visitors' addresses, so it cannot block an attacker by address. The defences are Tor's own (proof of work and introduction limits, set in [`deploy/torrc`](../deploy/torrc)), per-container ceilings so that one overloaded service cannot starve the host, and an outside probe that tells you what visitors see. None of them stops a large attack; they keep a small one from taking the market down and tell you which setting to change.

### Onion-service defences

`tor` and `tor-mirror` share `deploy/torrc`, so each onion address gets the same settings and its own queue. Each per-service option follows the `HiddenServiceDir` line it applies to; Tor refuses one that comes before it.

| Setting | Value | What it does and what it costs |
| --- | --- | --- |
| `HiddenServicePoWDefensesEnabled` | `1` | Publishes a proof-of-work puzzle in the onion descriptor. While the service is not overloaded the suggested effort is zero and nobody pays anything. Under load, Tor serves the clients that solved a harder puzzle first. Tor Browser 13.0 and later solve it automatically inside Tor, without JavaScript and with nothing shown on the page; at a high suggested effort that can take about a minute on a desktop and longer on an old phone. Clients that cannot solve puzzles still connect, at effort zero, so they get no priority: Arti-based clients (Arti's proof-of-work support is still experimental) and C tor older than 0.4.8 or built without the `pow` module. When the service is quiet they notice nothing; under attack they wait behind every client that solved a puzzle and may not get in. Do not combine this with Onionbalance, which does not pass the puzzle on. |
| `HiddenServicePoWQueueRate` / `HiddenServicePoWQueueBurst` | `60` / `600` | How many queued visitor connections (new rendezvous circuits) Tor lets through to the app per second, and at once; the rest wait in a bounded queue ordered by puzzle effort, and Tor discards the lowest-effort half when it fills. Tor's defaults (250 / 2500) suit a far larger service. 60 / 600 assumes the app sheds excess work quickly with `503` and `Retry-After` (its admission control); **without that admission control, use 30 / 300**. With the mirror running, the app can receive the rate of both addresses. |
| `HiddenServiceEnableIntroDoSDefense` (`RatePerSec`, `BurstPerSec`) | `1` (`25`, `200`) | Asks each introduction point (3 by default) to pass on at most 25 introductions a second, in bursts of 200: together about 75 a second and 600 at once. This is deliberately looser than the proof-of-work queue: introduction points cannot see puzzle effort, so a tighter limit would drop visitors who solved a puzzle before Tor could put them first. It protects the Tor network and this Tor process more than reachability: a flood can still use up every introduction point's allowance. If you raise `HiddenServicePoWQueueRate` above about 75, raise `HiddenServiceEnableIntroDoSRatePerSec` so that 3 × the rate stays above it. Tor refuses a burst below its rate, for both limits. |
| `HiddenServiceMaxStreams` / `HiddenServiceMaxStreamsCloseCircuit` | `32` / `1` | At most 32 simultaneous connections on one visitor circuit; a circuit that opens more is closed, so one circuit cannot hold hundreds of slow connections to the app. A browser opens only a few connections per site at a time. |
| `HeartbeatPeriod` | `30 minutes` | How often Tor logs its heartbeat ([Watching the onion](#watching-the-onion)); 30 minutes is the shortest Tor allows. |
| `MaxMemInQueues` | `128 MB` | When queued cell data passes this, Tor closes the circuits holding the most. Unset, Tor sizes it from the host's memory, far above the Tor container's 256m [memory limit](#resource-limits-and-log-rotation), so a flood would get the whole Tor process killed and restarted instead. Raise it together with that limit. |

To change a value, edit `deploy/torrc`, rebuild the Tor image, check that Tor accepts the file (with the app running) and recreate Tor. Recreating keeps the onion identity volumes, so the addresses stay the same:

```sh
docker compose build tor
docker compose run --rm --no-deps tor --verify-config    # ends with "Configuration was valid"
docker compose up -d tor              # without the onion mirror
docker compose up -d tor tor-mirror   # with the onion mirror
```

With [`compose.nodes-tor.yaml`](#local-nodes-over-tor) in `COMPOSE_FILE`, the nodes' `tor-socks` is a separate image built from the same `Dockerfile.tor`: rebuild and restart it too, with `docker compose build tor tor-socks` instead of the first line and `tor tor-socks` (or `tor tor-mirror tor-socks`) in the `up -d` line. Its own `deploy/torrc-socks` is not read by the `--verify-config` line above.

An edited `deploy/torrc` is a local change to the checkout: if an upgrade changes the same file, `git pull` refuses until you set your edit aside (`git stash`, `git pull`, `git stash pop`).

### Watching the onion

**Heartbeat.** Every 30 minutes each Tor service logs two lines worth watching; the counts are totals since that Tor started:

```sh
docker compose logs --since 2h tor | grep Heartbeat
docker compose logs --since 2h tor-mirror | grep Heartbeat    # with the onion mirror
```

```text
Heartbeat: Tor's uptime is 2 days 3:00 hours, with 41 circuits open. I've sent 1.20 GB and received 180.33 MB. ...
Heartbeat: Our onion service received 5210 v3 INTRODUCE2 cells and attempted to launch 5188 rendezvous circuits.
```

The difference between two consecutive onion-service lines, divided by 1 800 seconds, is the rate of introductions (visitors asking to connect) and of rendezvous circuits (connections Tor let through). Introductions growing far faster than rendezvous circuits mean the proof-of-work queue is discarding requests: more visitors, or an attacker, than `HiddenServicePoWQueueRate` lets through. A sudden jump in open circuits is often the first sign of a flood. Tor does not log the suggested puzzle effort at the default `notice` level. At the `info` level it logs when the suggested effort changes significantly (`Suggested effort changed significantly, updating descriptors`, without the value) and the effort of every queued request (`Enqueued rendezvous request with effort: N. Queued requests: M`); the suggested value itself (`Recalculated suggested effort: N`) is logged only at the `debug` level, which is far noisier. To see these lines for a while, add `Log [rend]info stdout` (or `Log [rend]debug stdout`) after `Log notice stdout` in `deploy/torrc` and rebuild as above; both are noisy under attack, so remove the line afterwards.

**Onion probe.** [`scripts/onion-probe.sh`](../scripts/onion-probe.sh) fetches `/canary` through a Tor SOCKS proxy as a new visitor would: each probe gets its own circuit, so it goes through an introduction point, the proof-of-work queue and a rendezvous every time. Run it from a separate machine, not the market's server or another server at the same provider: a probe on the server does not see what visitors see, and one at the same provider fails together with it. The machine needs `bash`, `curl` and Tor 0.4.8 or later with the `pow` module (`tor --list-modules` prints `pow: yes`), for example tor from the [Tor Project's repository](https://support.torproject.org/little-t-tor/getting-started/installing/) or a running Tor Browser 13.0 or later. A Tor without puzzle support waits behind puzzle-solving visitors and reports failures they would not see. Copy the script there and run:

```sh
./onion-probe.sh --count 5 --interval 30 ADDRESS.onion                          # system tor, 127.0.0.1:9050
./onion-probe.sh --socks 127.0.0.1:9150 --count 5 --interval 30 ADDRESS.onion   # Tor Browser
```

It prints one line per probe (`ok http=200 seconds=3.84`, or `FAIL` with curl's reason) and a summary, and exits `0` when every probe got HTTP 200 and `1` otherwise, so cron or a monitoring agent can alert on it. `--timeout` (default 120 seconds) bounds each probe and `--path` probes another page. Probe the mirror address too. A cron entry that keeps a log:

```text
*/15 * * * * $HOME/onion-probe.sh --count 3 --interval 20 ADDRESS.onion >> $HOME/onion-probe.log 2>&1
```

**Reading a failing probe.** Look at the app and the database at the same time:

```sh
docker stats --no-stream    # CPU and memory of each container, against its limit
docker compose exec db psql -U opsecmkt -d opsecmkt -c "SELECT state, count(*) FROM pg_stat_activity WHERE datname = 'opsecmkt' GROUP BY state"
```

The second command needs the internal database; run the same query in `psql` against an external one. The app holds at most 12 database connections for pages, forms and background work, plus one for `/healthz`.

- **The app is busy:** its CPU is at its limit or most of the 12 connections are `active`, and it answers with `503`. This is real overload, and proof of work is doing its job if visitors with Tor Browser still get in, slowly. If it persists, lower `HiddenServicePoWQueueRate` towards what the app handles (30 / 300 is the conservative setting).
- **The app is idle:** the probe fails or takes tens of seconds while the app's CPU is low and its connections are `idle`. This is the **OnionFlation** signature (Lee, Kim and Kang, "Onions Got Puzzled", USENIX Security 2025): short bursts of requests make Tor raise the suggested puzzle effort for everyone although the app has spare capacity, and real visitors' puzzles become too hard to finish in time. **Raise `HiddenServicePoWQueueRate` (with the burst at about ten times the rate); do not lower it.** A faster queue drains the bursts before they look like congestion; a slower one makes the queue look congested sooner and pushes the effort higher still. Raise it in steps (for example 60, 120, then 250, Tor's default), keep the introduction limit above it (see the table) and return to 60 once the attack stops.

### Tor image

[`deploy/Dockerfile.tor`](../deploy/Dockerfile.tor) installs tor from the Tor Project's own Debian repository as its [installation instructions](https://support.torproject.org/little-t-tor/getting-started/installing/) describe: the archive signing key, refused unless its fingerprint is `A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89`, is trusted for that repository only, and apt verifies the package with it. Debian's own archive also carries tor (currently an older build): an apt preference lets apt choose tor only from `deb.torproject.org`, and the build fails unless the installed tor is a version `deb.torproject.org` publishes, so a `TOR_VERSION` naming a Debian archive build (such as `0.4.9.11-0+deb12u1`) is refused. The Debian base image is pinned by digest and tor by exact package version (`TOR_VERSION`, now `0.4.9.13-1~d12.bookworm+1`), and the build fails unless `tor --list-modules` reports `pow: yes`. Building needs access to `deb.torproject.org` (HTTPS) and `deb.debian.org`; the built image runs offline until Tor itself connects.

The Tor Project's repository lists only its newest release. When it has moved past the pinned version, the build installs the repository's current release instead, but only if it is the same or newer than the pin, and prints `tor ... is no longer published; installing the newer ...`; it never installs an older tor, and the fingerprint, origin and `pow: yes` checks still apply. An existing install keeps using the image it built. To build one specific release instead, print the version the Tor Project currently publishes

```sh
curl -s https://deb.torproject.org/torproject.org/dists/bookworm/main/binary-amd64/Packages | awk '$1 == "Package:" { tor = $2 == "tor" } tor && $1 == "Version:" { print $2 }'
```

put it in `.env` (the value below is only an example), then run the `docker compose build tor` sequence under [Onion-service defences](#onion-service-defences) (`docker compose build tor tor-socks` and `up -d tor tor-socks` with [`compose.nodes-tor.yaml`](#local-nodes-over-tor)):

```sh
TOR_VERSION='0.4.9.14-1~d12.bookworm+1'
```

Delete that line again once you upgrade to a release whose `Dockerfile.tor` pins the same or a later version. Maintainers bump `TOR_VERSION` in `Dockerfile.tor`, and the base image to a new dated tag and digest (`docker buildx imagetools inspect debian:bookworm-slim`), and let the CI Tor image job confirm the version, `pow: yes` and the configuration.

### Resource limits and log rotation

The database and bundled Monero services have read-only root filesystems and `no-new-privileges`.
Their data volumes and temporary directories remain writable. PostgreSQL and both Monero services start as
their service users with all capabilities dropped. The application, Tor and scanner also drop all capabilities. Alternative node images may require different
temporary paths; review their entrypoints before overriding the defaults. Wallet-RPC's shared ring database
stays in its wallet volume. Its temporary file log under `/tmp` rotates at 1 MiB, keeping two files (the current
one and one rotated); Docker still captures its console output.

monerod also writes its own log file, which Docker's rotation does not reach: `bitmonero.log` in the `monero_data`
volume (`stagenet/bitmonero.log` on stagenet). Its defaults keep 50 files of 100 MB. The bundled `monero` service
(`compose.nodes.yaml`, `compose.nodes-tor.yaml` and the native `deploy` development render) starts it with
`--max-log-file-size=10485760 --max-log-files=3`: at most three files of about 10 MiB, the current one and two rotated
(`bitmonero.log-<UTC time>`), deleted oldest first whenever the current file rotates (A-538). Its console output still
goes to Docker's rotated log as well; the file stays for when that log has rotated past a fault or `LOG_DRIVER` keeps no
copy. Both node logs run at the default log level 0. Even so they can name peer IP addresses, transaction IDs and
(the wallet's) addresses and key images: keep copies private, and do not raise `--log-level` except briefly while
diagnosing. The caps limit size, not age. On a quiet node a file can hold lines older than the 14 days that the
[data retention policy](data-retention-privacy-policy.md) sets for operational logs; to enforce the age as well,
delete rotated files older than that, for example from a daily job:

```sh
docker compose exec -T monero find /home/monero/.bitmonero -maxdepth 2 -name 'bitmonero.log-*' -mtime +14 -delete
```

A node you run yourself on another host needs the same flags, or that host's own rotation, for its log files.

The application build/runtime base images and internal PostgreSQL image are pinned to registry digests.
Refresh those pins after reviewing upstream updates, then run the build, container and database checks.
The node release tags remain configurable through `*_IMAGE` in `.env`; use reviewed digests for reproducible
node deployments. ClamAV's release tag allows `refresh-scanner` to obtain newer signatures for that release;
using an immutable scanner digest requires explicitly updating it to refresh its bundled signatures.

Every service has a memory, CPU and process ceiling, so a flood, a leak or a runaway sync in one container slows down or restarts that container instead of starving the host, and the database with it:

| Service | `mem_limit` | CPUs | `pids_limit` | Notes |
| --- | --- | --- | --- | --- |
| `app` | 512m | 2 | 512 | `GOMEMLIMIT=400MiB` makes Go's garbage collector work harder before the memory limit is reached; Go sizes its scheduler to the 2-CPU limit. |
| `db` | 1g | 2 | 256 | Internal database only. Every PostgreSQL connection is a process; 256 leaves room above the default 100 connections. |
| `tor`, `tor-mirror` | 256m | 1 | 64 | `MaxMemInQueues 128 MB` in `deploy/torrc` keeps Tor's queues inside this ([Onion-service defences](#onion-service-defences)). |
| `tor-admin` | 256m | 1 | 64 | Optional client-authorized admin onion; `MaxMemInQueues 128 MB` in `deploy/torrc-admin`. |
| `tor-socks` | 256m | 1 | 64 | The nodes' Tor client, only with [`compose.nodes-tor.yaml`](#local-nodes-over-tor); `MaxMemInQueues 128 MB` in `deploy/torrc-socks`. |
| `monero` | 2g | 2 | 512 | monerod starts threads per core for sync and verification. |
| `monero-escrow-wallet` | 1g | 1 | 128 | Dedicated Monero escrow wallet-rpc, only with its profile. |
| `clamd` | 3g | 2 | 256 | The [malware scanner](#photo-uploads-and-the-malware-scanner), only when it is on. It holds ClamAV's signatures in memory: 1035 MiB at its peak with ClamAV 1.5.4 and the signatures of 21 September 2026 (`scripts/test-scanner.sh` prints it), and `ConcurrentDatabaseReload no` keeps a reload from loading a second copy. The figures are provisional until war room A-196 measures scans at the upload limits. |

CPUs are set as `cpu_quota` over `cpu_period` (`cpu_period: 100000` and `cpu_quota: 200000` is 2 CPUs), not with `cpus`: Docker refuses to create a container whose `cpus` exceeds the host's CPU count, so `cpus: 2` would stop the app from starting on a 1-CPU server, while a quota simply never binds there. `pids_limit` counts threads as well as processes. The limits are ceilings, not reservations: a host smaller than their sum still starts everything, but the ceilings then protect it less. When a container reaches its memory limit the kernel kills its process and Docker restarts it (`restart: unless-stopped`); `docker inspect --format '{{.State.OOMKilled}} {{.RestartCount}}' "$(docker compose ps -q app)"` shows whether that happened. A CPU limit only slows a container down; at its process limit new threads fail to start.

Each container logs to Docker's `json-file` driver, rotated at 10 MB and kept to three files (`x-logging` in each Compose file), so `docker compose logs` holds at most about 30 MB per container and a flood of error lines cannot fill the disk. Rotation deletes the oldest lines; copy logs elsewhere first if you need them for longer.

For these containers this setting replaces any default logging driver set in Docker's `daemon.json`. To choose another driver, set `LOG_DRIVER` in `.env` (absent or blank means `json-file`): `LOG_DRIVER='local'` selects Docker's compressed `local` driver, which takes the same rotation options. Other drivers, such as `journald` or `syslog`, refuse `max-size` and `max-file`, so Compose would fail to create the containers; for those, leave `LOG_DRIVER` unset and replace the whole logging block of each service in a file of your own added to `COMPOSE_FILE` (as below for resource limits), with the rotation that driver's destination provides:

```yaml
services:
  app:
    logging: !override
      driver: journald
  # ... the same for db, tor, tor-mirror and each node service you run
```

`!override` needs Docker Compose 2.24.4 or later; `./scripts/install.sh check` runs `docker compose config`, which shows whether the result is what you meant.

To change a limit, leave the tracked Compose files alone: put the services you change in a file of your own, for example `compose.local.yaml` beside them, and add it at the end of `COMPOSE_FILE` in `.env` (for example `COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.local.yaml'`). Then run `./scripts/install.sh check` and `docker compose up -d`. Change CPUs with `cpu_quota`; adding `cpus` next to it makes Docker refuse the container. For example, to set operator-selected application and Monero limits:

```yaml
services:
  app:
    mem_limit: 1g
    environment:
      GOMEMLIMIT: 800MiB   # about 80% of mem_limit
  monero:
    mem_limit: 4g
    cpu_quota: 400000      # 4 CPUs
```

## Optional cryptocurrency nodes

The installer provisions Monero only. `--nodes local` starts a pruned daemon; it does not enable escrow unless `--monero-escrow yes` is also selected. New local installs default to `MONERO_NETWORK=mainnet` with real funds. Use `--monero-network stagenet` for testing. No mainnet run is recorded.

### Node image versions

The bundled Monero daemon and dedicated wallet-rpc use pinned community-maintained 0.18.5.1 images. Review image provenance and immutable digests before changing `MONERO_IMAGE` or `MONERO_WALLET_IMAGE`. Run configuration tests and `scripts/test-escrow-xmr-regtest.sh` on a local fakechain after a version change.

### Local node pruning

`MONERO_PRUNE_FLAGS='--prune-blockchain --sync-pruned-blocks'` is the default. A quoted blank disables those flags for a full node. Pruning retains the data needed for wallet refresh and restore. Chain sizes grow; the historical 45 GB pruned mainnet example is a lower planning reference, and 20 GB for stagenet is an unverified estimate. Keep free-space monitoring active.

### Local nodes over Tor

A new Tor install with the local Monero node (`./scripts/install.sh --tor` with `--nodes local`, or a local node chosen at the questions) lists `compose.nodes-tor.yaml` after `compose.nodes.yaml` (and the database overlay) in `COMPOSE_FILE` by default (A-530). `--nodes-tor no` opts out: the installer then says the node's peer-to-peer traffic is not routed through Tor, and every later `check` (which `upgrade` runs) warns while `APP_MODE=tor`, the `monero` profile and no `compose.nodes-tor.yaml` remain. `--nodes-tor yes` is refused without Tor and a local node. `upgrade` never adds or removes the overlay: an install from before A-530, or one that opted out, keeps its recorded `COMPOSE_FILE` ([upgrade notes](../UPGRADING.md#a-tor-installs-local-monero-node-defaults-to-tor-a-530)). To add it to such an install, append `compose.nodes-tor.yaml` after `compose.nodes.yaml` in `COMPOSE_FILE`, run `./scripts/install.sh check`, then `docker compose up -d --build`; to remove it, delete it from `COMPOSE_FILE` and run the same two commands. The native `opsecmkt deploy` controller has no counterpart: its development-only local daemon reaches peers directly, and `deploy plan` says so. The overlay sends the bundled Monero daemon's peer traffic through a separate Tor SOCKS client on the internal node network. It does not cover an external daemon or operator image pulls and package downloads.

The `tor-socks` listener at `10.199.42.2:9050` accepts the internal node subnet and refuses its host gateway and other sources. SOCKS isolation reduces circuit sharing; it does not defeat timing correlation. The node loses its direct egress route; the app and wallet-rpc cannot reach the SOCKS listener. If that subnet conflicts with another project, edit `compose.nodes-tor.yaml` and `deploy/torrc-socks` together. Rebuild `tor-socks` whenever rebuilding the Tor image.

Monero delivery through Tor remains unverified. With `APP_MODE=tor`, Monero settlement relay is held unless this overlay is configured or `ESCROW_XMR_TOR_RELAY=clearnet-accepted` explicitly accepts direct network exposure. See [Monero escrow](#monero-escrow).

## Migrations

`internal/market/schema.sql` is the frozen baseline. Startup applies it, then every `internal/market/migrations/NNN_name.sql` not yet listed in `schema_migrations`, in numeric order, inside one transaction under an advisory lock, so concurrent app starts are safe and a failed migration changes nothing. A version missing from the table is applied even when higher versions already are. A version in the table that the running server does not include means a newer release migrated the database: the server then exits with an error naming it, before changing or serving anything (see [Rolling back](../UPGRADING.md#5-rolling-back)). It does the same when a recorded version has a different name from the server's migration of that number: a different build, such as a development branch, used the number, so the server's own migration was never applied. Never edit a merged migration; add a new file. Reserved blocks: 001–009 Foundation, 010–019 authentication, 020–029 PGP, 030–039 orders, 040–049 inventory, 050–059 payments, 060–069 transparency; 070–106 contain later payments, privacy, escrow, operations and receipt migrations. Migration 106 records whether a dynamic receipt originally required password confirmation; existing receipts default to false and retain their fixed-route checks. Migration 099 adds indexes for account-filtered and global audit-event ordering; migration 100 adds short-lived form-submission receipts; migration 101 persists provider-alert episode state and delivery deduplication receipts. Take an encrypted backup before upgrading; there are no automatic down-migrations.

Startup runs in separately bounded phases, each set by an optional Go duration in `.env` (blank uses the default):

- `DATABASE_CONNECT_TIMEOUT` (default `15s`, from 1s to 10m): reaching PostgreSQL.
- `MIGRATION_LOCK_TIMEOUT` (default `10m`, from 1s to 24h): waiting for the migration lock while another app instance migrates, or completes first-run setup on, the same database.
- `MIGRATION_TIMEOUT` (default `10m`, from 1s to 24h): applying the baseline and pending migrations. An upgrade whose migrations rewrite large tables can need longer; raise it before deploying.
- Payment provider startup (connecting to the configured wallets and nodes) keeps a fixed 15-second bound; a provider that answers with an error is shown as unavailable and retried, as described under [Payments](#payments).

When a phase runs out of time the server exits with an error naming it and the setting to raise, for example `database migration timed out after 10m0s and was rolled back; nothing was changed. Raise MIGRATION_TIMEOUT ...`. A timed-out lock wait or migration is rolled back and its database session ended before the process exits, so the next start begins from the unchanged database with the lock free. An invalid value stops startup with a message naming the variable and its accepted range.

The application's pool opens with fixed per-connection limits (startup parameters, set by the app, not in PostgreSQL's configuration): `statement_timeout` 8 s, `lock_timeout` 3 s and `idle_in_transaction_session_timeout` 15 s, so one slow query or held row lock cannot tie up the pool for longer, and `jit` off, because PostgreSQL's JIT compiler spent 3–6 s compiling the staff escrow-alert query on every moderator and administrator page load (MOD72-8). A payment request, and whatever may queue behind one (a checkout, listing edit, archive or takedown, a cancellation returning stock, a payment-intake change, and an administrator's suspension, second-factor reset or role change), waits up to 11 s for a row lock instead (its statement limit is raised to 11 s with it), because the payment request holds its rows while the escrow node is asked about the escrow address (up to 10 s). Each account has at most one payment request in progress at a time; another is refused at once with 429 "Your previous request is still being processed" and `Retry-After: 5`. Startup opens this pool and checks that the database accepts it **before** migrating, so a database or pooler that refuses these parameters stops startup with nothing changed (see [Connection poolers](#connection-poolers)). Migrations then run on their own database connection without the limits; the migration transaction also lifts any that are set on the database role. `-status`, `-bootstrap-admin`, `-reset-admin-password`, `-set-role`, `scripts/backup.sh`, `scripts/restore.sh` and `psql` sessions do not have them either. A page or form stopped by one answers 503 ("The market is busy") and logs cause `57014` (statement) or `55P03` (lock); see [Troubleshooting](#troubleshooting).

### Connection poolers

An external database reached through a connection pooler such as PgBouncer, or a hosted provider's pooler, may refuse startup parameters it does not know. Startup then exits with `database refused the application's session settings (SQLSTATE 08P01): ...` (the code depends on the pooler) before any migration runs. Either:

- connect `DATABASE_URL` to PostgreSQL directly, or to the provider's direct (session) endpoint; or
- let the pooler drop the parameters and set them on the application's role instead. In PgBouncer, add them to `ignore_startup_parameters` (a comma-separated list; keep any entries already there):

  ```ini
  ignore_startup_parameters = statement_timeout,lock_timeout,idle_in_transaction_session_timeout,jit
  ```

  PgBouncer then discards them, so the limits apply only once they are set for the role the app connects as (`opsecmkt` below; use the user in your `DATABASE_URL`). They take effect for new sessions:

  ```sql
  ALTER ROLE opsecmkt SET statement_timeout = '8s';
  ALTER ROLE opsecmkt SET lock_timeout = '3s';
  ALTER ROLE opsecmkt SET idle_in_transaction_session_timeout = '15s';
  ALTER ROLE opsecmkt SET jit = off;
  ```

  The migration transaction lifts these itself, and `pg_dump` and the restore script set their own. `-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors`, `-set-role`, `-set-restore-gate` and `-clear-restore-gate` then run under them too, which their short transactions do not notice. Without the role settings the app runs with no statement or lock limit, as before A-177, and with the server's JIT setting.

A serving app also holds a session advisory lock on one database connection of its own for as long as it runs (A-505), so that the restore gate host commands can refuse while it runs; the escrow watchers take session advisory locks too. That session is named `opsecmkt-serving` and turns off the role's or database's statement, lock and idle-session limits for itself. A pooler in transaction mode does not keep a session's locks, and shows two symptoms: startup refuses with `the database connection does not keep one session (a connection pooler in transaction mode?)` (the app reads its session's process ID before and after taking the lock and finds them different), or, if the pooler happens to keep the session, the restore gate commands may still not see the app once the pooler moves it. Use session pooling or a direct connection.

The pool also sets `TimeZone` to UTC as a startup parameter; PgBouncer passes `TimeZone` through by itself.

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

Environment settings are listed in `.env.example`. Unless a feature states
otherwise, blank or absent values keep it disabled.

### Administration sections

`/admin` (the **Admin** link) is an overview: the staff attention counts, the
*Get your marketplace ready* checklist and a link to each section, flagged when
something in it needs you (for example open vendor applications, a Monero escrow
alert or a paused image scanner). Every
administration panel is on exactly one section page, and every admin page starts
with the same section navigation (plain links that wrap on a narrow screen; the
current one is marked for screen readers). Where this guide says
**Admin → panel**, open the section that holds it:

| Section | Address | Panels |
|---|---|---|
| Overview | `/admin` | Staff attention, *Get your marketplace ready*, section links |
| Users | `/admin/users` | *User directory* (each account's page at `/admin/users?handle=`), *Assign an account role*, *Reset a user's second factors*, *Suspend or restore an account*, *Ban or unban an account* (and, on a banned account's page, *Orders of this banned account*), *Staff roster* |
| Vendors | `/admin/vendors` | *Vendor applications*: vendor policy, open applications awaiting a bond or decision, verified-vendor mark |
| Payments | `/admin/payments` | *Escrow status and payment intake*: Monero escrow alerts, *Monero escrow watcher health*, release deadline, *Held releases of banned vendors* (the held-release refund period, A-652f), *Monero escrow recovery and retention*, *New escrow checkouts*, *Escrow readiness* |
| Security & access | `/admin/security` | *Site access and registration*, *Sign-in protection* (CAPTCHA), *Handles with paused sign-in* |
| Site & branding | `/admin/site` | *Market name and logo*, *Staff contact*, *Outgoing XMPP* |
| Transparency | `/admin/transparency` | *Operator PGP key*, *Warrant canary*, *Signed audit export*, *Public staff list* |
| System & audit | `/admin/system` | Schema version and server start, *Service isolation and updates*, *Application host*, *Image scanner*, *Retention and preservation*, *Recent server errors*, *Audit trail* |
| Optional moderation | `/admin/moderation` | Private-ingress-only proposal queue, bounded worker status, confirmed review annotations and encrypted escalation downloads |

Every form keeps its address (`POST /admin`, `/admin/suspend`, `/admin/access`
and so on); a successful change returns to the section holding its form, at the
panel when the change has one, with its one-shot notice. A form refused with an
unknown handle shows the Users section again. Each section page refuses buyers,
vendors and moderators exactly as `/admin` does, and is reachable only through
the private administrator ingress when one is configured. Bookmarks to an old
`/admin#panel` fragment open the overview; follow its section link.

### Moderation proposals

The optional [moderation controller](moderation-agent.md) and local model are
off by default. The Admin overview on private administrator ingress links to `/admin/moderation`, available only
on the private administrator connection; it shows job/review/escalation counts,
injection-suspicion counts, prompt/weights pins, last pass and bounded failure
classes. These are worker observations, not proof of model or host integrity.
Model endpoints, raw model JSON and transport errors are omitted. The model never
reads listing or upload text: those proposals carry `MODEL_NOT_USED` and wait in
the human queue unless a deterministic check escalates them; only reviews,
disputes and payment flags reach the model.

While capture is on, the Admin overview (`/admin`) shows a **Moderation capture
needs attention** alert when the lifetime pool has at most two daily actor shares
left (162 allocations at `--max-records 4096`) or when refused targets still wait
after a `retained_capacity` or `backfill_capacity` overflow recorded since the
oldest of them was refused. An unflagged payment's parked marker is not counted,
and an older overflow does not make later per-account throttle markers (an account
editing faster than its share) an alert; accounts awaiting a rescan are counted
but do not trigger it on their own. On the
private administrator connection it reads, for example, "2 of 512 lifetime
allocations used; 1 target(s) waiting for capture" and names where to raise
the limits: **Moderation budget** on the private moderation panel, or
`--max-records` and `--max-reserved-bytes` on the moderation worker (each
allocation reserves 524288 bytes; at the defaults the byte budget binds first, so
raise both). Waiting targets include records noted while the capacity row was
busy (`moderation_deferred`), each target once. With `ADMIN_SOCKET` set the
public listener refuses `/admin`; without it there is no private moderation
panel, and the alert is one sentence without numbers telling you to restart the
moderation worker with higher `--max-records` and `--max-reserved-bytes`, or, on
a Tor deployment, to set up the private administrator connection. It names no account, target or time. It clears on the next load once the
pool has room again and the worker has captured the waiting targets.

**Moderation budget** on `/admin/moderation` raises the lifetime limits without
host access or a restart: enter the new maximum lifetime allocations and maximum
reserved bytes in whole MiB (the form starts at the current values) and confirm
with your password and, when enrolled, authenticator code. It only raises; a
lower or unchanged submission, a value out of bounds or a wrong password is
refused with "Nothing was changed". Each raise is audited ("Moderation budget
raised from … to … by administrator …"). The worker's next pass captures the
waiting targets. A worker restarted with a lower `--max-records` or
`--max-reserved-bytes` keeps the stored value and logs "stored moderation budget
is higher; flag ignored". Lowering the budget is a host-only procedure; see
[Capacity and overload](moderation-agent.md#capacity-and-overload).

**Accept**, **Dismiss** and **Hold** record review annotations only. They do not
hide a listing, change an account/order, publish a reply or move funds. Confirmed
forms retain password/TOTP, CSRF and nonce checks. Accepting changed or expired
content is refused. Review deadlines default to `HOLD`, preserving marketplace
state. Binding decisions still use the existing independently authorized staff
and payment workflows.

The [escalation runbook](moderation-escalation-runbook.md) explains optional
encrypted packets and separately signed one-use review tokens. A token can be
submitted without an application login at `/admin/moderation/offline`, through
the same protected private ingress. Keep its action-signing private key offline
and distinct from the controller's online packet-signing seed.

### Service status and health

**Admin → Service isolation and updates** (`/admin/system#dependency-status`)
lists each service with its installed version, the latest upstream release, a
status and a **Health** column (war room A-553). Health is only what this app
observes itself, never a service's address, host or credentials:

| Row | Installed version | Health |
|---|---|---|
| OPSEC Market (this app) | This build's version and commit: release binaries carry both; `go build` in a git checkout shows `development` with the checkout's commit; the Docker image build excludes `.git`, so it shows `development` with no commit | Serving this page |
| PostgreSQL | Observed on the app's database connection | Reachable when this page's queries answered |
| Monero daemon | Observed by this app: the release from monerod's `get_info`; a restricted RPC hides it, so the row shows the RPC interface version from `get_version` and the release from operator inventory, if any | Reachable or the failing class (for example `get_info timeout`), synced at a height or not synced (height against target, or the watcher's sync guard reason), the last successful check and the last error class with their times |
| Monero wallet RPC | Observed by this app: monero-wallet-rpc's `get_version` reports only its RPC interface version (for example `1.31`), never its release, so the release comes from operator inventory | Reachable or the failing class, the last successful check and the last error class |
| Tor | Operator inventory only | Not observed: the app has no connection to Tor |
| ClamAV | The engine from the app's clamd state, or from the upload gateway's report with isolated uploads (A-552) | The scanner state and when it was checked or reported |
| Go application runtime | Observed in this process | Running |

The Monero rows come from the Monero escrow watcher, not from the page: the
watcher records what its calls to the daemon and the wallet-rpc showed after
every pass, and, once the daemon is synced, asks the wallet-rpc (and a daemon
whose `get_info` hid its release) for `get_version` at most once an hour per app
process, on the RPC connections Monero escrow already uses (no other outbound
connection). With Monero escrow off they read *Monero escrow is off on this
app*. Before the first pass they read *No Monero escrow watcher pass recorded
yet*.

**Latest upstream** still needs the separate versions worker. Without its
signed report every row reads *Not compared: versions worker not configured*
and the panel names the step: run `opsecmkt versions-worker` on its own host
and give the app the report and its pinned key (`DEPENDENCY_REPORT_FILE` and
`DEPENDENCY_REPORT_PUBLIC_KEY`, or `--dependency-report-dir` and
`--dependency-report-public-key` with `deploy init`), as
[Version-worker host](install/service-topology.md#version-worker-host)
describes. The market's own row is not tracked by the worker; compare it with
the release notes and `UPGRADING.md` before upgrading.

**Admin → Monero escrow watcher health** (`/admin/payments#xmr-escrow-health`,
while `ESCROW_XMR=on`) shows the latest pass and whether it completed without
errors, otherwise **Last pass failed** with its error class (for example
`monero escrow wallet escrow-202610-01: open_wallet unreachable`, or the
not-ready or not-synced state), and the time and age of the last good pass. When
`ESCROW_XMR_SEED_KEY` is not the seed key pinned in the database it shows
**Monero escrow wallet creation stopped** (existing wallets are still read; see
[Monero escrow](#monero-escrow) to restore or re-pin the key). With no pass
recorded for 15 minutes it says the watcher may have stopped. The watcher
writes all of this to one database row (`settings` key `escrow_xmr_health`)
after each pass, so every app process shows the same state and a restart or
redeploy keeps it. The overview flags the Payments section while the last pass
failed, the record is stale or wallet creation is stopped, and the 10-minute
provider outage notice points here instead of to the server logs.

### User directory

*Admin → Users* starts with the **User directory** (A-545): search for any part of
a handle, in upper or lower case (`%` and `_` are matched as typed, not as
wildcards), and filter by role (buyer, vendor, moderator, administrator), status
(active, suspended, banned, closed or closing) and vendor state (applicant waiting
for a bond, applicant waiting for approval, approved vendor, verified vendor). The
newest accounts come first, 50 to a page, and every account is reachable through
the page links, which keep the search. It replaces the old *Newest accounts* list;
*Assign an account role* below still takes a handle typed exactly.

Select a handle to open the account's page, `/admin/users?handle=HANDLE` (the
handle exactly, case-sensitive). It shows only what the market already stores for
other reasons: the role and status; the account's creation date; when it was
suspended and by which administrator (from the audit trail), or banned, by whom
and the ban reason (administrators only); retention state (closure requested,
restricted or closed); whether TOTP and PGP sign-in are on, and the saved PGP key's
fingerprint and verification date, never a secret; the latest vendor application
with its bond; the verified-vendor mark and who set it; active and archived listing
counts; order counts by state as buyer and as vendor; and disputes opened by and
against the account, still open, released to the vendor or refunded to the buyer.
It shows no sign-in times, network addresses or page visits, and the market does not
collect them for it.

For a vendor the page also shows **Reputation figures** (A-653): counts of its funded
orders by how they ended, disputes decided or conceded either way, distinct buyers,
counted reviews and low ratings, and listings now taken down or rejected, all over
retained history, worked out when the page loads and never stored, with the fixed rule
table and the label it proposes: HIGH_REPUTATION, STANDARD, INSUFFICIENT_HISTORY (too
few completed orders) or AT_RISK, with the reason codes. No model is involved, and only
administrators see it. Read the label as a pointer to the figures, not a verdict: a
small or concentrated record (few distinct buyers, which is how bought reviews and
sockpuppet orders show) is never HIGH_REPUTATION. When the label is HIGH_REPUTATION
the panel offers *Confirm HIGH_REPUTATION and publish without review* (see *Vendors who
publish without review*). If the database is too busy to count the figures in time, the
panel reads *Figures unavailable* and offers no confirmation; the rest of the page and
its account actions still work, and reloading tries the count again.

The page carries the account's actions, each the same form as elsewhere with the
same password confirmation, audit rows and notices, returning to the account's
page: suspend or restore, ban or lift the ban (with a reason), reset second factors,
change the role (buyer, vendor approved at once, or moderator), open a vendor
application on the account's behalf, record the bond, waive it, approve or reject an
open application, and set or remove the verified-vendor mark. When you open an
application you choose where it starts, whatever the vendor policy's approval mode
(owner decision 2026-10-19): *waiting for the bond* asks for the policy's current
bond (offered only while the policy sets one; set it under *Vendors* first), and
*waiting for approval* asks for no bond. It is never approved on this path, even
under automatic approval: after the bond is recorded it waits for your approval like
a manually approved application, so you can always hold an account at "not approved
yet". It is not offered for a suspended account (restore it first; the form is
refused with 409 and nothing is stored) or a banned, closed or closing one.
**Message user** opens `/messages?to=HANDLE` (staff may message any account except
a banned one; it is not shown for a banned or closed account). Your own account and other
administrators' pages show the details only: administrators are managed on the host
([Administrators](#administrators)).

### Site access and registration

An administrator controls browsing and account creation separately under
**Admin → Security & access → Site access and registration** (`/admin/security`):

| Site access | New registrations | Result |
| --- | --- | --- |
| Public | Open | Visitors can browse and create accounts. This is the default. |
| Public | Closed | Visitors can browse; only existing accounts can sign in. |
| Members only | Open | Visitors must sign in or create an account to view site content. |
| Members only | Closed | Site content is available only to existing signed-in accounts. |

To change either setting, select both choices, enter your current password and
authenticator code when enabled, then select **Save access settings**. Changes
are audited and take effect without a restart. Closing registration does not
delete accounts or end existing sessions. First-administrator setup remains
available until initialization is complete.

Members-only mode protects listings, search, vendor pages, listing photos, avatars and transparency
pages. Sign-in, allowed registration, setup and their required resources remain
reachable. The market name and logo remain public so visitors can identify the
site before signing in. An account provides access; these controls do not add an approval
process for new members. Previously downloaded or publicly cached content cannot
be recalled by changing the setting.

For new native installations, add `--site-access members --registrations closed`
to `deploy init`, or use the same flags with `serve`. Direct environments can set
`INITIAL_SITE_ACCESS=members` and `INITIAL_REGISTRATIONS=closed`. The defaults are
`public` and `open`. On first startup these values initialize database settings
only when absent. Later administrator changes survive restarts and redeployments;
editing an initial value does not override them. See the
[CLI reference](install/cli-reference.md#additional-runtime-settings).

After changing access, check a signed-out session and an existing account.
Members-only content should lead to sign-in, while closed `/register` shows a
notice without a form. Existing accounts should still be able to sign in.

### Vendor applications

Accounts become vendors through an application decided under the policy in **Admin → Vendors → Vendor applications** (`/admin/vendors`).
Choose a bond in XMR (0 for none) and manual or automatic approval, write the bond payment instructions applicants
see, and confirm with your password (and authenticator code when enabled). A change applies to new applications only.

- **The bond is a non-refundable fee.** Say so in your instructions and public terms. With Monero escrow on and ready
  and new XMR payments on, each applicant waiting for a bond gets a one-time address of the market's
  [bond wallet](#vendor-bond-wallet), and the bond is recorded as received once deposits covering it have the escrow
  confirmations; the queue shows each address, the XMR received and any flagged deposit. Otherwise (escrow off or not
  ready, intake paused, or a restore being checked) no address is shown and the applicants follow your instructions:
  check your own wallet (on a test network while testing), then choose *Record bond received*. *Record bond received*
  and *Waive the bond* stay available in both cases.
- With manual approval, approve or reject each application with an optional note the applicant sees. With automatic
  approval, the bond received on the Monero network, recording it or waiving it approves the application (an account
  that is suspended, closed or closing waits for you instead); with no bond and automatic approval every vendor sign-up
  is a vendor at once, so use that only where anyone may sell.
- Approval gives the vendor role (audited like *Assign an account role*); rejected or withdrawn applicants stay buyers.
  *Assign an account role* and `-set-role` still change roles directly: making an account a vendor records an
  approved application ("Role assigned by an administrator") and closes any open one. A removed vendor keeps its
  approved application, sees that its vendor role was removed and may apply again.
- *Verified vendors* marks a vendor with a badge on its page and listings. It is your judgment as staff, not an
  identity check; the vendor is notified and the change is audited.

Vendors that existed before this release keep selling: the upgrade records an approved application for each, without
a verified mark.

### Listing review

**Admin → Vendors → Listing review** (`/admin/vendors#listing-review`) chooses whether new and edited listings are
public at once (**publish at once**, the default and the value after upgrading) or wait for staff (**hold for
review**). Confirm with your password (and authenticator code when enabled); the change is audited. In hold mode a
vendor's new listing, its change to an approved listing's title, description, category, subcategory, region, additional shipping regions, fulfillment type, price, dispatch window or vendor-or-automatic digital delivery, and a new photo on it, hide the listing from buyers until a moderator or administrator
approves it. Stock changes, automatic delivery content and your own edits do not hold a listing; your own listings
are never held. Orders placed before an edit continue.

- Review listings in **Listings awaiting review** on the moderation desk (`/moderator#listing-review`), the longest
  waiting first. Open each to see it with its photos. **Approve** publishes it; **Reject** needs a reason of 10–500
  characters that the vendor reads (never an address, real name or tracking number). A rejection is not a takedown:
  the listing stays hidden but is not archived, the vendor sees "Rejected — <reason>" and edits it, and that edit (or
  a new photo) puts it back in the queue; no administrator is needed. To remove a listing for good, take it down from
  its edit page instead (administrators). Both need the reviewer's password and are audited on both accounts; the
  vendor is notified. A form opened before the vendor changed the listing is refused: reload the desk.
  Moderators review; only administrators change the mode or a vendor's flag.
- **Vendors who publish without review**: enter a vendor's handle (or use the account's page in the user directory) to
  let its new and edited listings publish at once, or to hold them again. It applies to the vendor's next save;
  approved listings stay visible and listings already waiting still need staff. Banning the account turns it off,
  and lifting the ban does not turn it back on. On the account's page you may instead confirm a HIGH_REPUTATION
  label (A-653) with the same password step: the market computes the label again when you submit and refuses, with
  nothing changed, if it is no longer HIGH_REPUTATION or the figures changed since the page loaded (reload and read
  them again). Your audit row records the label name; the vendor is told only that it may publish without review.
- Switching back to publish at once approves nothing: listings already waiting stay hidden until staff approve them.
  The audit row says how many still wait.
- Automated moderation (the optional moderation worker and model) never approves, holds or rejects a listing, and a
  capture that overflowed leaves a held listing held.

### XMPP notifications

XMPP is disabled until an administrator configures **Admin → Site & branding → Outgoing XMPP**
(`/admin/site#xmpp-outgoing`). It also stays off for each recipient until that account opts in.
The sender is an existing XMPP account operated by the market, much like an outgoing mail account;
the application does not create accounts on an XMPP server.
The server must support SASL PLAIN authentication over verified TLS and resource binding.

1. Enter its bare sender address (`market@example.test`), server hostname, port and password.
   Choose STARTTLS (normally port 5222) or direct TLS (normally port 5223), using the server's actual configuration.
2. Enable outgoing delivery and confirm with your administrator password and, when enrolled, authenticator code.
   The XMPP password is encrypted in the database using the application's existing secret root. It is never
   filled back into the form; leave it blank to retain it. Preserve `APP_SECRET_KEY` (or the legacy root) on restart
   and restore, as for TOTP secrets. An application host compromise can still read or use the configured credential.
3. On **Your account → XMPP notifications** (`/account#xmpp-notifications`), enter a private bare recipient
   address and opt in, confirming with your account password and authenticator code when enrolled.
   Staff also opt in here; configuring the outgoing sender does not subscribe administrators or moderators.
4. Use disposable recipients and a test order/message to check delivery before relying on it. Check the same
   event in **Notifications**. A frontend notice remains available if XMPP fails.

The server requires a valid TLS certificate and authenticates only after TLS succeeds. Configure outbound network
access from the application host to the chosen XMPP server. This connection is direct: Tor mode does not route
XMPP through Tor automatically. The XMPP service can see the application host's connection, sender, recipients,
message bodies and timing, and recipients' services can retain copies. TLS protects transport to the server;
notification text and routing metadata remain visible to those services. Choose a separate sender account and
recipients appropriate for this disclosure. The private destination is not published on vendor or public profile pages.

By default, alerts carry only the same notification text and recipient authorization as the frontend: payment
confirmation, shipping, new-message metadata and dispute changes. Encrypted message armor is omitted unless
both of the following are enabled:

- An administrator selects **Allow PGP-encrypted message contents in XMPP** under *Outgoing XMPP*.
- The recipient selects **Include PGP-encrypted message contents** under their account's *XMPP notifications*.
  Their XMPP notifications must be enabled and their account must have a usable PGP public encryption key.
  Enabling PGP sign-in or proving key ownership is not required for this preference.

These choices are off by default and require the same password/authenticator confirmation as the other XMPP
settings. When both allow it, a new received-message alert can append the already encrypted PGP armor; the
market does not decrypt or forward a plaintext message body. The same consent can include a new stored
buyer-encrypted digital delivery in that buyer's order alert; vendors and staff do not receive the buyer's
delivery contents through their own alerts. The original ciphertext must match the recipient's current usable
key with a visible recipient key ID. Hidden-recipient messages, mismatches, changed keys or unavailable content
keep the summary alone, even when the market accepted the original message after sender confirmation.
Its PGP protection is retained, while the sender, recipient, notification text, timing and armored ciphertext,
including visible encryption key IDs, remain visible to the XMPP services. A recipient uses their PGP client and private key to decrypt the armor.
The market checks encrypted packet structure and visible recipient key IDs; without the private key it cannot
prove ciphertext integrity or successful decryption. Configure the XMPP service to accept message bodies up to
256 KiB plus the XML stanza overhead, so a long encrypted digital delivery fits. A server refusal uses the same
bounded retries and leaves the market notice and delivery available.
Existing messages and deliveries are not resent when either choice is enabled. Changing the global forwarding permission invalidates queued content from its earlier
revision while leaving the notification summary eligible; changing the recipient choice also invalidates earlier queued deliveries.

Order-linked alerts include the order reference so the recipient can find the corresponding market record;
opening it still requires the normal order permission. Direct messages keep their message references.
[Support tickets](#encrypted-support-tickets) use a ticket ID and their own assignment, status and escalation
workflow; their notifications obey the same current recipient and content-consent checks.
Administrators
also receive vendor applications and bond-review states, security events and unusual activity notices.
Moderators receive dispute alerts and their own account/order activity. Administrator-only notices never go to
moderators, buyers or vendors. A role change is checked again before staff notices are displayed or delivered;
being told of a dispute does not grant a moderator permission to resolve Monero escrow.

Only new notifications are queued, in the transaction that creates the frontend notice; enabling XMPP does not
resend existing notices. Opting out or changing the destination invalidates pending deliveries for the old
preference. Delivery retries use a stable message identifier, with at most 12 attempts, exponential delays from 30 seconds
to one hour, and no send after the notice is 24 hours old. Delivery is at least once: a lost connection after
server acceptance can produce a duplicate. Acceptance by the outgoing server does not prove that the destination
received or read the alert. Disabling outgoing delivery cancels pending sends when the worker next processes them; check frontend notifications during an outage.

### Encrypted support tickets

Use **Support tickets** (`/tickets`) for routine support. Your account lists its own tickets; staff see the
queue their current role permits. Open a ticket at `/ticket?id=…` from that list or its notification. A ticket
uses an ID rather than a plaintext subject, and records Open, Closed or Ignored status, the current assignee
and procedural history.

1. Save a usable PGP public encryption key under **Your account → Public profile**. Keep its private key in your
   own PGP client. Complete PGP challenge decryption for this session before opening or replying to a ticket.
   Opening a ticket also needs an active staff recipient with a usable key, available through
   the public staff list or configured staff contact; a hidden staff roster is not published for ticket creation.
2. Choose that staff member in the ticket form, obtain the displayed public keys, and encrypt your initial body
   locally to both your own key and the selected staff member's key. Paste the armored encrypted message into
   the form. Do not put a plaintext support narrative in a status or reference field.
3. Read replies by decrypting their armor in your PGP client. Before each reply, check the current assignee and
   recipient keys again, especially after a reassignment or escalation. Encrypt the next body to the opener
   and the current assignee. The application does not hold private keys or decrypt the body.
4. The opener can close their own Open ticket. Current authorized staff can manage normal tickets, including
   assigning another staff member or escalating to an administrator while the ticket is Open. Staff can close,
   ignore or reopen a ticket; ordinary users cannot ignore or reopen one. Closed and Ignored threads do not
   accept replies until permitted staff reopen them.

Escalated or administrator-only tickets remain available to their opener and permitted administrators;
moderators cannot access them through the staff queue. A moderator who opened one keeps their own personal
participant view, which grants no administrator staff-management powers. An administrator seeing a ticket
in the queue still needs a matching private key to decrypt it.

Assignment identifies the next reply's recipient; it does not grant retroactive decryption. A new assignee cannot decrypt old bodies
unless those bodies were already encrypted to their key. A former recipient may retain copies they already
obtained. Ticket IDs, status/history, sender/recipient relationships and timing remain visible to the market;
PGP protects message contents, not that metadata.

Ticket notifications use the same current participant and role restrictions in the frontend and XMPP.
After reassignment, active staff who previously handled or sent a message in the ticket can receive a generic
closure or ignore notice. After escalation, a former moderator handler may still receive that minimal metadata;
it gives no access to the protected thread. Optional XMPP contents require both the administrator's permission
and the recipient's separate opt-in, plus a current usable key and visible recipient-key match. Assignment or
escalation must not send protected content to a recipient who no longer qualifies. Keep XMPP provider retention
and visible key IDs in mind when choosing content forwarding.

When [retention maintenance](retention.md) is enabled, save needed ticket ciphertext locally: bodies expire
after 90 days, or earlier if the opener or message sender is closed or erased. A related account preservation
hold delays cleanup, including holds tied to previous participants or staff in the history. Closed and Ignored
ticket metadata expires 90 days after its last update only once its bodies are gone and holds are released;
Open metadata remains for staff workflow. Deletion uses the restore journal. Provider/client copies survive
independently of market cleanup.

### Changing the market name and logo

An administrator manages the site's identity under **Admin → Site & branding → Market name and logo** (`/admin/site`).
The saved name labels browser tabs and the home link, and supplies the text
wordmark when no custom logo is set. A name change does not rewrite lettering
inside an uploaded image.

1. Enter the **Market name** and select **Save market name**.
2. To replace the logo, enter your password and second factor when enabled, then
   select **Authorize logo upload**.
3. On the next page, choose a JPEG, PNG or self-contained SVG and select the
   upload button. Authorization expires after 10 minutes and accepts one upload.
4. Reopen the market and check the header, footer and sign-in page. Reload the
   admin page to confirm the saved logo and audit event.
5. To remove the custom image, confirm **Use text wordmark**. The saved name is
   retained.

The logo always fits a fixed box: 224 × 56 CSS pixels in the desktop header,
192 × 48 in the mobile header and footer. Its proportions are preserved, so tall
or wide artwork leaves space inside the box instead of stretching the layout.
Long text names are visually shortened to fit; the full name remains available
to assistive technology and in browser titles.

JPEG and PNG inputs are limited to 4 MiB and pass the existing scanner and
re-encoding limits. They are stored as JPEG, with transparency flattened onto
white; use SVG when the artwork needs a transparent background.
SVG inputs are limited to 256 KiB and normalized to a small
set of static shapes and text. Scripts, CSS styles, links, embedded images,
external resources, animations and document declarations are refused. Export
complex artwork as simple outlined paths or as a PNG. Raster metadata is removed;
SVG presentation, visible text and supported accessibility labels may remain.
ClamAV checks uploads for known malware; it does not review picture content.

For a separate private administrator onion with isolated uploads, configure
`BRANDING_UPLOAD_ORIGIN` on the application with the public upload-gateway origin,
then restart the application. Native `serve` and `deploy init` accept
`--branding-upload-origin`. Use a verified HTTPS origin or an HTTP v3 onion
origin, without credentials, a path, query or fragment. Keep the private admin
onion's client authorization in place. The same origin carries staff avatar and
administrator listing-photo uploads from the private connection ([photo uploads](#photo-uploads-and-the-malware-scanner)).
A blank value disables these upload flows on that topology; it does not prevent
existing services from starting. With isolated uploads the logo form follows the
gateway's scanner report: while the report is missing, stale or not *Ready* it
reads "Logo uploads are paused" and authorizing a logo upload is refused.

The confirmation stays on the private admin connection. The next form sends only
a short-lived logo authorization token and the file to the configured gateway;
it sends no admin password, second-factor code or admin session field. The gateway
may learn the private admin onion's hostname through the browser's Origin header.
The token authorizes only this logo upload and grants no general administrator
access. The gateway processes originals; the app accepts only normalized logo data.
Update both processes to a supporting release before using the feature.

The name and logo are public even when browsing requires sign-in. Replacing or
removing a logo cannot recall downloaded copies. Database recovery copies can
retain earlier logos until their normal expiry; removal is not forensic erasure.

### Authentication

Second-factor secrets are encrypted with a key derived from `APP_SECRET_KEY` (on an install without it, from `SETUP_TOKEN`: see [Secrets](#secrets-env-and-onion-identities)); changing that key makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off (the sign-in's *I lost my authenticator* checkbox does both at once) and enroll again (an administrator resets a non-administrator with no recovery code left under *Admin → Reset a user's second factors*; an administrator with no recovery code left needs the host command in *Administrator lockout* below). Startup refuses a placeholder or obviously non-random `APP_SECRET_KEY` or `SETUP_TOKEN`; use `openssl rand -hex 32`.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code and the account password, and 10 one-time recovery codes are shown once. Turning TOTP or PGP sign-in on or off, replacing recovery codes and adding, changing or removing the PGP key all need the account password (and a current code when TOTP is on), and each notifies the account; so do sign-ins with a recovery code and the first failed second step after a correct password. A user who reports such a notification they did not cause should change their password at once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **Lost authenticator**: a user (including an administrator) with an unused recovery code recovers without staff. On the sign-in's *Use a recovery code instead* form, they tick *I lost my authenticator — turn two-step sign-in off*: the code signs them in and TOTP is turned off in the same step (the other recovery codes are deleted; PGP sign-in, if on, stays on), then they set up a new authenticator on `/totp`. Without the checkbox, a recovery-code sign-in lands on `/totp` showing how many codes remain, where TOTP can still be turned off with the password and another recovery code. Only a user with no recovery code left needs the *Second-factor reset* below (or, for an administrator, the operator procedure).
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio or text alternative, so turning it off may be needed for users who cannot read it; the form's "Can’t read the image?" help shows the staff contact (below). First-run setup never shows it. Showing a challenge writes nothing to the database; the id of each answered challenge is kept in `captcha_spent` until it expires (at most 10 minutes) so it cannot be reused. A challenge shown before an upgrade to this version is refused after it; the user reloads the page.
- **Release step: decide the CAPTCHA.** Decide the CAPTCHA: keep it on with an account-free staff contact, or turn it off. Do this before you tell anyone the address, and again whenever you change the staff contact. The default image CAPTCHA has no audio or text alternative, so it does not meet WCAG 2.2 success criterion 3.3.8 (Accessible Authentication, Minimum) for a person who cannot read the image, and it stands in front of the only way to register or sign in. The market does not offer a second path. Two choices are honest. (1) Keep it on and set *Admin → Staff contact* to an address that works without an account, such as an onion or XMPP address: `/login` and `/register` then tell the reader to write to it and ask for the CAPTCHA to be turned off, which an administrator does under *Admin → Sign-in protection*. A bare handle does not count, because a handle can be messaged only from an account. (2) Turn it off, accepting that registration and sign-in then have no image check (the per-handle sign-in pause still applies). With the CAPTCHA on and no such contact, *Admin* shows a warning that names the problem, and `/login` and `/register` say there is no way in without the image instead of pointing at a contact that does not exist. `./scripts/install.sh check` cannot give the same warning: the CAPTCHA and staff-contact settings live only in the database, `check` starts nothing and runs before the first start, so it has nothing to read. The release notes repeat the limit in one line.
- **Sign-in pauses**: after 10 failed passwords for one handle within ten minutes, sign-in for that handle is refused (HTTP 429) until that window ends, at most 10 minutes, **even with the right password**; the page says sign-in for this handle is paused, that someone else may have caused it and that the password may be fine. Correct sign-ins, registrations and setup attempts do not count. The limit is per handle because there is no per-client signal over Tor, so **anyone who knows a handle can trigger it, including for your administrator account**, and keep it paused by repeating 10 wrong guesses every ten minutes: each guess needs a solved CAPTCHA while CAPTCHA is on, and costs nothing when it is off. While any handle is paused, the app logs `sign-in limiter refusing handles=N` (a count, never a handle) at most once a minute, when a sign-in fails or is refused; watch for it with `docker compose logs app | grep 'sign-in limiter'`. Break-glass: the limits are held in memory, so restarting the app (`docker compose restart app`) clears every pause; sign in immediately afterwards, before an attacker can spend the budget again. Other users' pauses are cleared too, and a restart does not stop the attacker from repeating it.
  With a [private administrator onion](install/tor.md#private-administrator-onion) (`ADMIN_SOCKET`), sign-in through it has budgets of its own (A-570): failed passwords and rescue codes on the public storefront never pause sign-in, authenticator or PGP code checks, or password confirmations on the private onion, even when the storefront's limiter table is full, and failures there never spend the public budgets, so a public attacker who knows your administrator handle cannot lock you out of the private onion; sign in there instead of restarting. The storefront refuses an administrator's sign-in with the same "Invalid handle or password" (401) as a wrong password, whether the password is right or not, also on the paused sign-in's authenticator-code rescue, and a right password spends the same sign-in (and code) budget as a wrong one, so guessing there never confirms an administrator's password (A-611; before, a right one was answered "Page not found"). The private budgets are the same sizes (10 failed passwords per handle and 10 codes per account each ten minutes) and are enforced, so a client holding the onion's authorization key is still limited, to those budgets. Host, forwarding headers and cookies cannot select them; only a request arriving on the private socket does. The private onion also has its own small admission limits (8 pages, 4 public and 4 signed-in form posts, 2 password checks and 2 CAPTCHA images at a time, among others), so a public flood that fills the storefront's limits does not refuse it, though both still share the database pool and CPU. Upload limits are shared too, so a flood of public uploads can still make an administrator's upload answer busy.
  The administrator panel lists currently paused handles for this application instance only, with pauses on the private administrator onion listed separately; the list is in memory and can include handles with no account. During a pause, any account with TOTP enabled, administrators included, can still sign in by entering its current password and six-digit authenticator code together on `/login` (A-571; before this release only a sole administrator could). CAPTCHA and the bounded password-check slot still apply. The code is checked first: a wrong or already-used code, an unknown handle or an account without TOTP is refused with "Invalid handle or password" before any password check and never spends the account's sign-in code budget, so an attacker who knows only the handle cannot spend the codes `/challenge` needs. Wrong codes spend only the rescue path's own budget of ten per account per ten minutes; when it is spent, the rescue is closed for that account until its window ends, a right code included, while `/challenge` and normal sign-in after the pause still work. A right code spends one of the account's ten code attempts per ten minutes, the budget `/challenge` uses, whether the password is right or wrong; after ten, rescue and `/challenge` wait for that window to expire. Spending them therefore needs a current code: three of the million possible codes are accepted at any time, and the rescue path checks at most ten codes per account per ten minutes. The residual risk: anyone who knows the handle can spend the password budget and the rescue budget (ten wrong passwords and ten wrong codes), and for as long as they keep repeating that every ten minutes, the account cannot sign in with its password at all. On a market with a [private administrator onion](install/tor.md#private-administrator-onion), administrators sign in there instead, since its budgets are separate; otherwise restart the app as described above. Use the existing host recovery commands if an attack keeps exhausting it. The code's replay guard and session commit together. Every refused rescue, wrong codes included, is counted without handles in `paused sign-in rescue refused attempts=N` (refusals since the previous line) at most once a minute; watch with `docker compose logs app | grep 'sign-in rescue'`. A steady count with no paused account of yours signing in is someone guessing codes. Accounts without TOTP use the existing sign-in and operator recovery paths during a pause. Outside a pause, the same *Authenticator code (if you use one)* field lets any account whose only second factor is TOTP sign in in one step (A-549): the code is checked like one entered on `/challenge`, against the same per-account budget and replay guard, and a wrong, expired or reused code moves the sign-in on to `/challenge`, which says the code was not accepted. An account with PGP sign-in on still completes `/challenge`; recovery codes are entered only there. During a pause, an account without TOTP that fills in the field gets "Invalid handle or password" rather than the pause explanation, as does a wrong code, so the field's help says to leave it blank to see why. New registrations, setup and CLI bootstrap refuse case-insensitive handle collisions; a non-unique index lets legacy case variants coexist. Sign-in resolves an exact handle first, then a case-insensitive match only when unique. Staff lookup and host role/reset commands still require the exact handle.

- **Password-confirmation pauses**: every action confirmed with the account password (resolving disputes, payment reviews, admin settings, key, TOTP and password changes) shares one per-account limit of 10 failed confirmations per ten minutes: a **wrong** password, or a wrong authenticator or recovery code after the right one. After that the account's confirmations are refused (HTTP 429, "Too many failed password confirmations. Try again in ten minutes.") until that window ends, even with the right password. Correct confirmations do not count, so staff can resolve any number of disputes in a row. Only someone signed in to the account can spend it; restarting the app clears it, as for sign-in pauses.
- **Message and recipient-lookup budgets** (A-516): each account may send 20 messages in ten minutes, counted apart from its budget of 100 changes (an attempt counts once its armor is well formed, including one to an unknown handle or one the sender may not write to); the 21st is refused with HTTP 429 "Message not sent: an account can send at most 20 messages in 10 minutes" and nothing is stored. Loading a recipient's key on *Messages* (`/messages?to=`) is limited to 60 different handles per account in ten minutes; loading one again in that window costs nothing. Below it, an unknown handle and one the account may not write to show the same words and no key (A-543). Past it the page loads no key and says nothing about whether the handle exists, only that the account can look up at most 60 different handles in 10 minutes. Both budgets bound spam and handle enumeration from one account; they do not stop someone with many accounts. They are held in memory per app process, so restarting the app clears them, and they are fixed in the code.
- **Password change**: users change their own password on *Account → Change password* with the current password (plus an authenticator code or an unused recovery code when TOTP is on). Every other session and pending sign-in of that account ends. There is no email or administrator password reset: a user who forgets their password cannot be recovered through the application.
- **Second-factor reset**: an administrator can turn off TOTP (deleting its secret and recovery codes) and PGP sign-in for a non-administrator account under *Admin → Reset a user's second factors* by entering its handle (exact, case-sensitive; any account, not only those in the list of up to 100 shown there), confirmed with the administrator's own password (and authenticator code when enrolled). Without a replacement public key, the account's PGP key and verification stay; its sessions and pending sign-ins end, both accounts get an audit row and the user is notified. Browse-only login may then accept the password, but strict login still requires PGP decryption. Confirm the request really comes from its owner before resetting. For a lost private key, explicitly provide the verified owner's replacement public key as described under [PGP access](#pgp-access-for-purchases-and-messages).
- **Unreadable PGP key**: an account with PGP sign-in on whose saved key can no longer be read (for example one saved before this market refused keys like it) is never signed in with the password alone. When browse-only login is allowed, TOTP can still satisfy optional sign-in verification; strict login always requires a usable PGP key and decryption. Without an available permitted factor, the sign-in is refused after the correct password, with a message that names the problem, the second-factor reset and the staff contact, and the owner is notified once. Such a user needs the *Second-factor reset* above (an administrator, *Administrator lockout* below); confirm who is asking first, since the key that would prove it is the one that cannot be read. For lost or unreadable key recovery, supply the owner's replacement public key in the privileged reset, then have them sign in and decrypt with it. A reset without replacement does not authorize password-only removal or replacement of the existing binding.
- **Account suspension**: to contain a compromised or abusive account, an administrator enters its handle under *Admin → Suspend or restore an account*, chooses *Suspend* and confirms with their own password (and authenticator code when enrolled). Its sessions and pending sign-ins end at once and it cannot sign in until restored: after a correct password it is told "This account is suspended." followed by the staff contact (below) Its role, listings and orders are unchanged (change the role to buyer as well if a vendor's listings should be archived), but orders waiting on this account stop until it is restored; the other party can still complete, cancel where allowed, or open a dispute. A suspended moderator is not notified of payment reviews and is not offered as a dispute contact. (Moderators never decide a Monero escrow dispute; on a historical order without escrow, a suspended moderator does not count as its resolver.) Suspension and restore touch only the account, its sessions and its notices. The panel lists suspended accounts; *Restore* with the same form allows sign-in again. Both changes are audited on both accounts and the user is notified. Administrator accounts, including your own, cannot be suspended there: to contain one, remove its administrator role from the host first ([Administrators](#administrators)).
- **Account ban** (A-546): a ban is **permanent closure**, for an account that must not use the market again; use suspension for a hold you may lift. Under *Admin → Users → Ban or unban an account* (`/admin/users#ban`) enter the handle, choose *Ban*, give a reason of 10–500 characters on one line and confirm with your own password (and authenticator code when enrolled). In one step the account is signed out everywhere and cannot sign in (order partners see that the account is closed, not suspended) (after a correct password it is told only "This account is closed and cannot sign in." and the staff contact, not your reason), its active listings are archived, an open vendor application is withdrawn, a verified-vendor mark is removed, *publish without review* is turned off (in hold mode its listings wait for staff again), and nobody can start a new order or message with it. While it stays banned, its reviews are not shown or counted in vendor ratings (A-649, see [Orders](#orders); unbanning counts them again). Its handle stays reserved for good: nobody can register it again, in any letter case, even after retention has closed and erased the account (the lower-case handle is the one thing kept). The ban itself leaves open orders going: the buyer or vendor on the other side can still complete, concede or dispute them; decide disputes as an administrator who is not a party. Banning a vendor gives the buyer of each of its shipped Monero escrow orders with no dispute at least 72 hours from the ban to open one (A-652d): a release deadline sooner than that moves to 72 hours after the ban (a later one stays, and lifting the ban never shortens it), and each buyer gets a notice for their order naming the deadline, saying the vendor's account is closed and that otherwise the order completes and its payment is held. The ban's audit rows count these orders. If one of those orders is being changed at that moment (an escrow pass, say), the ban waits for it, for up to about 11 seconds; only if an order stays busy that long, or is marked shipped at that very instant, is the ban refused with *Service unavailable* (503), nothing changed: submit it again. An order the escrow pass completes at its already-passed deadline at the same moment is not extended; its release is held like any other. A vendor banned before this version keeps its shipped orders' deadlines: do not lift and reimpose the ban to change that, because lifting it releases held payments at the next pass. Nothing is paid by this; decide any dispute a buyer opens as usual (a decision for the buyer refunds the buyer's refund address). Releases to a banned vendor are held until the ban is lifted; refunds are paid as usual; no other address is ever paid (owner decision 2026-10-25, A-652c). A release the vendor asked for at shipment or claimed stays *Not built yet* once the order completes or is decided for the vendor: you get one *Payment review needed* notice for the order saying the vendor's account is closed and the release is held, and the order page says the same to the buyer. Nothing for it is asked of the escrow wallet, and the database refuses to store its transaction. Do not pay it, or anything else, by hand. A release the market had already built or sent before the ban is sent and followed as usual. Lifting the ban is all it takes: the next escrow pass builds the release to the vendor's address and sends it. If the ban stays, an administrator who is not a party can instead refund such an order to its buyer refund address once the release has been held for the period under *Admin → Payments → Held releases of banned vendors* (`ban_hold_refund_days`, 30 to 3650 days, default 90, counted from the ban or from when the order completed or its dispute was decided, whichever is later; a new ban starts it again; A-652f). Open the order page (the banned account's page links every such order; the moderation desk lists one under *Escrow alerts* only once its release request has waited more than 24 hours or failed, never a completed digital order the vendor never claimed or one decided or conceded for the vendor before shipping), read *Refund the held release to the buyer*, give a reason of 20–5000 characters (it goes into your audit trail and the vendor's) and confirm with your password (and code). This applies even when the buyer confirmed receipt, and to a completed digital order the vendor never claimed. The order is cancelled, the held release is cancelled and the market sends one refund, of everything counted less the network fee, to the refund address the buyer gave at checkout on its next pass; both parties are told and nobody else is paid. It is refused, with nothing changed, before the period, once the ban is lifted, while the restore gate is set or a restore's sweep is pending, if you are a party, if anything changed since you loaded the page, and for a release that was built or may already be on the network (or a re-request of one), which is never replaced: settle those with the operator. The same applies to a dispute decided (or conceded) for the vendor before the order was shipped, which has no release request; its period counts from the later of the ban and the decision. While you can refund such an order, its order page is readable to administrators (not moderators) even without an escrow alert; it shows no delivery content and no refund address, and the order history notes are visible as for any staff read. To give buyers their money back for a banned vendor's orders that are unpaid or paid but not shipped, open the account's page (*User directory*, or the banned list) and use *Orders of this banned account* (`#ban-orders`, A-652b): it lists the account's open Monero escrow orders and those whose escrow may still hold coins, previews how many orders and how much it refunds, and with your password (and code) cancels them and asks the market to send each one holding coins back to the refund address its buyer gave at checkout, on its next pass; nobody else is paid and this cannot be undone. Shipped, delivered and disputed orders are not refunded there. If an order changed after you loaded the page, nothing is changed: reload and review again. It refunds at most 100 orders per submission, refuses while the restore gate is set or a restore's sweep of the escrow wallets has not completed (`xmr_escrow_sweep_required`; it completes by itself), and leaves out orders where you are the buyer (another administrator refunds those). The overview flags *Users* while a banned account has open escrow orders. A release that still needs the vendor's claim address (a completed digital order, or a dispute decided for the vendor before they gave one) waits for that claim, which a banned vendor cannot make, until the ban is lifted and they claim it. The reason is stored with the ban, shown to administrators in the panel's list and written to both accounts' audit rows, so it appears in the signed audit export: write a factual reason, not personal details you would not otherwise keep. *Unban* with the same form needs a reason too; it puts back the suspension the account had before the ban, if any (it then stays suspended until you restore it), and otherwise lets it sign in again, but does not bring back its listings (the vendor publishes them again) or the verified mark (verify again under *Vendors* if you decide to), and *publish without review* stays off (turn it on again under *Vendors* if you decide to). Banning a moderator also makes them a buyer, in the same step and audited on both accounts; lifting the ban does not return the staff role (assign it again if you decide to). A vendor keeps the vendor role. *Suspend or restore* refuses a banned account, and while it is banned neither *Assign an account role* nor `-set-role` gives it any role but buyer. Its vendor page says the account is closed, with no *Message vendor*. Administrator accounts, including your own, cannot be banned, so neither can the last administrator; to ban a former administrator, change their role on the host first ([Administrators](#administrators)).
- **Staff contact**: set *Admin → Staff contact* to how people reach the staff — a handle, onion address or XMPP address, at most 200 characters on one line — confirmed with your own password (and authenticator code when enrolled); the old and new values are audited. Anyone can read it, signed in or not: it is shown on the Transparency page (`/canary`), to a suspended account after its correct password, on `/challenge` for someone who lost their second factors, beside the CAPTCHA, in the expired- and unreadable-PGP-key sign-in refusals, to a vendor whose listing an administrator took down, with a photo the malware scanner refused, on the two-factor page after a sign-in with the last recovery code, and under notifications that tell people to contact the market staff (such as the sign-in and security notices). It is always plain text; if it is exactly the handle of a moderator or administrator who is not suspended and whose PGP key can receive encrypted messages, signed-in users also get a link to message that handle. While it is empty those places say "Ask the operator of this market." — so set it soon after installation. With the CAPTCHA on, make it an address that works without an account (see *Release step: decide the CAPTCHA* above); *Admin* warns when it is empty or a bare handle. Save it empty to remove it.
- **Administrator lockout** (lost second factors) is not recoverable in the application: the web reset refuses the administrator's own account and other administrators. Keep the administrator's recovery codes offline: with one left, the administrator recovers alone at sign-in (*Lost authenticator* above). Without one, and also after a key rotation left the stored TOTP secret unreadable, an operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-factors ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL` (the internal or an external database), starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands, it turns off TOTP (its secret, any unconfirmed enrollment and the recovery codes, including any not yet shown) and PGP sign-in (the PGP key and its verification stay), ends every session and pending sign-in of that account, writes the audit row "Second factors reset by host command: … turned off; ended N session(s) and any pending sign-ins", notifies the account and prints a one-line summary. The administrator then signs in with the password alone and enrolls TOTP (or PGP sign-in) again. It refuses, changing nothing: an account that is not an administrator (reset those on the web under *Admin → Reset a user's second factors*, above), an unknown handle, an administrator with no second factor enrolled, and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password` below. The password is not changed: if it is lost too, also run `-reset-admin-password`.
- **First administrator from the host** (instead of entering `SETUP_TOKEN` in the browser on `/setup`; `./scripts/install.sh init --admin ADMIN_HANDLE` does this during installation): an operator on the host, in the deployment directory (with its `.env`), after the app has started once so the database schema exists, runs (replace `ADMIN_HANDLE`: 3–32 letters, digits or underscores, exact and case-sensitive; `-site-name` is optional):

  ```sh
  docker compose run --rm app -bootstrap-admin ADMIN_HANDLE -site-name "My Market"
  ```

  It connects with `DATABASE_URL`, starts no web server and needs no setup token. It prompts twice for the administrator's password without showing it (12–72 bytes, as on `/setup`; valid UTF-8 without a NUL byte, as the sign-in form sends, so a terminal set to `LATIN1` is refused) and then, in one transaction under the same lock as `/setup` (so it cannot race the browser form or another run), creates that one administrator, records the marketplace name (default `OPSMKT`, at most 80 characters; change it later on `/admin/site`), marks the market installed and writes the same audit row as `/setup` ("Account created: admin"). It prints only the handle; sign in on `/login` with that handle and password. From then on `/setup` refuses every token. It refuses, changing nothing, when the market is already set up (by `/setup` or an earlier run), a handle, password or name that breaks the rules above, a database the server has never started against, and a database upgraded by a newer release. Arguments go after the service name, as for `-reset-admin-password` below. Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input (`docker compose run --rm -T app -bootstrap-admin ADMIN_HANDLE`); it is never accepted as an argument. Anyone who can run this command with the deployment's `.env` can create the first administrator, just as they could read `SETUP_TOKEN` from it; there is no web equivalent.
- **Forgotten administrator password**: the application has no web password reset. An operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-password ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL`, starts no web server, prompts twice for the new password without showing it (12–72 bytes, the registration rule; valid UTF-8 without a NUL byte) and, in one transaction, stores its hash, ends every session and pending sign-in of that account and writes the audit row "Password reset by the operator on the host". It refuses an account that is not an administrator, an unknown handle, and a database upgraded by a newer release (as startup does); nothing changes then. Arguments go after the service name because the image's entrypoint is the server itself (do not repeat `/app/server`). Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input; it is never accepted as an argument. The site can keep running. Second factors are unchanged: if they are lost too, also run `-reset-admin-factors` (above).

### Administrators

The web page never adds, removes or changes an administrator: *Admin → Assign an account role* and the role form on an account's page in the *User directory* offer only buyer, vendor and moderator and refuse administrators' handles, and suspension, the ban and the second-factor reset refuse administrators too. Administrators are managed from the host with `-set-role`, by an operator in the deployment directory (with its `.env`). Replace `HANDLE` (exact and case-sensitive) and `ROLE` (`buyer`, `vendor`, `moderator` or `admin`):

```sh
docker compose run --rm app -set-role HANDLE ROLE
```

It connects with `DATABASE_URL`, starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands (so two runs happen one after the other), it changes the role, writes the audit row "Role changed from OLD to NEW by host command" on the account and prints a one-line summary. It does what *Assign an account role* does for the new role: assigning Buyer removes selling approval and archives active listings; promotion to moderator or administrator preserves genuine selling approval and listings (audited as "Archived N active listing(s): role changed to NEW by host command"), and an administrator or moderator who stops being staff loses their staff avatar and goes back to hidden on the public staff list. Removing an administrator (any role other than `admin`) also ends every session and pending sign-in of that account. It refuses, changing nothing: an unknown handle, a role other than the four above, the role the account already has, removing the last active administrator, changing a closed account, making a suspended or closing account an administrator (restore it on the web first), and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password`.

- **Add an administrator**: the person registers an ordinary account on `/register` and, preferably, sets up TOTP and saves the recovery codes. Confirm out of band that the handle is theirs, then run `docker compose run --rm app -set-role HANDLE admin`. Their open session becomes an administrator's at once. A vendor made an administrator or moderator keeps its independent selling approval and active listings.
- **Remove an administrator**: `docker compose run --rm app -set-role HANDLE buyer` (or `moderator`, or `vendor`). The account is signed out everywhere and signs in again with the new role. The last administrator cannot be removed: add another first.
- **Contain a compromised administrator** (someone else knows its password, or its owner acts against the market):
  1. Demote it at once: `docker compose run --rm app -set-role HANDLE buyer`. This ends its sessions and pending sign-ins, and a password-confirmed change of its own that is not yet saved when the demotion commits (a suspension, factor reset, review or listing takedown, payment intake or staff list switch) is refused with 403, changing nothing. Whoever has the password can still sign in as a buyer until the next step.
  2. As another administrator, suspend it on the web: *Admin → Suspend or restore an account*, choose *Suspend* and confirm with your own password. A suspended account cannot sign in (see *Account suspension* above).
  3. Review what it did. On `/admin/system`, enter the exact case-sensitive handle in *Filter audit by account*; the page shows at most its 100 newest matching events, or says when none are recorded. Invalid handles are refused rather than trimmed or truncated. The filter is account-scoped, so it shows events recorded on that account; also inspect rows on accounts it changed, or download the *Signed audit export* (when `AUDIT_SIGNING_KEY` is set, see [Transparency](#transparency)) and search it. Role-change rows on the target account identify the acting administrator by handle; older rows retain their original text. Check the escrow decisions and settings it made, marketplace settings, sign-in protection, operator key and canary, and undo what it changed. An administrator web account cannot change the market fee or the address it is paid to (they are host configuration: *Market fee* under [Monero escrow](#monero-escrow)), so there is no fee change to undo on the web; a market-fee change notice in the audit trail or the administrators' notifications that no operator made points to the host, not to this account.
  4. To reinstate the account later: restore it on the web, run `-set-role HANDLE admin`, then give it a new password with `-reset-admin-password` (which works on administrators only) and, if someone else may know its second factors, reset them with `-reset-admin-factors HANDLE` (see [Authentication](#authentication)).

### PGP identity

**Official addresses in sign-in codes** (A-572). A reverse-proxy phishing site can relay the market page by page, including the encrypted PGP sign-in code, but cannot change what is inside it. After decryption the code names the account's handle, the market name (*Admin → Site & branding*), the UTC time it was requested, until when it is valid (the pending sign-in's expiry, at most 10 minutes after the password) and the market's official addresses, with an instruction to enter the code only when the browser's address bar shows one of them (a code requested on the [private administrator onion](install/tor.md#private-administrator-onion) instead says it was requested on that private connection, without naming it); the code itself is on the last line and is all the market accepts. The addresses come only from `OFFICIAL_ADDRESSES`, a comma-separated list of at most 8 HTTPS or HTTP v3 onion origins read at startup (`.env` for Compose, `--set OFFICIAL_ADDRESSES=…` for native `init`, `--official-addresses` or the environment for native `serve`), never from the request's `Host` or forwarding headers. List the public onion, the [mirror](#optional-onion-mirror) and the clearnet origin you publish, for example `OFFICIAL_ADDRESSES=http://<56 characters>.onion,https://market.example`, and recreate the app (`docker compose up -d`). **Never list the private administrator onion**: every user receives this text; a native configuration that records the administrator onion's address refuses it, but a Compose install, a native install whose administrator onion came from an older identity volume, an edited env file and `serve --official-addresses` are not checked, so check the list yourself. Unset, the code says "no official address configured" and asks users to check the address another way; an invalid value stops startup with a message that names the variable but not the value. Publish the same addresses where users can verify them independently, such as your signed canary.

Users paste an ASCII-armored public key on `/account`; it is stored without armor headers or surrounding text (only public key, subkey, user ID and signature packets are accepted: a key with a GnuPG photo ID is refused, with the `gpg --edit-key` … `deluid` steps to remove it), and the fingerprint and user IDs are shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in; renewing the same key (same fingerprint, for example `gpg --quick-set-expire`) keeps them unless it drops a revocation or rolls back the encryption subkey. `/account` and `/pgp` show when the key and its encryption subkey expire, and each process checks all saved keys daily and notifies owners once 30 days before and once after encryption to their key stops. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled or an administrator resets the account's second factors (see Authentication). Vendor pages and order pages show the other party's armored public key, fingerprint and whether ownership is verified, and "Message vendor" links on vendor and listing pages open `/messages?to=<handle>` with the recipient filled in; transaction-specific correspondence stays on its order page. Without a saved key, the page says messages cannot be sent until one is added. Users cannot search for or write to arbitrary private accounts. Ordinary accounts contact public vendors, listed or configured staff, and staff available through their own dispute. A vendor can reply to an existing customer through a visible order or a prior buyer/vendor exchange. A historical buyer-to-buyer message does not authorize another message. Unavailable and unknown recipients receive the same response and expose no key. Staff retain their support compose field. Staff support uses encrypted messages; disputes remain tied to an order, rather than becoming private buyer lookup.

### PGP access for purchases and messages

Every purchase and message send requires a usable PGP public key and successful decryption of a challenge in the current browser session. Saving a key, signing an ownership challenge, or using TOTP alone does not establish this access. Open **Account → PGP**, request a decrypt challenge, decrypt it locally, and return its code. The server accepts public keys only and stores a challenge hash; keep the private key in your own PGP application. A new session or any change to the saved public key requires another decryption proof. Enroll an authenticator under account security as additional protection against account takeover.

Administrators control **Allow login without PGP** under authentication settings. It defaults to enabled so existing keyless accounts can sign in, browse, and add their public key. This never permits purchases or messages without the session proof. When disabled, new sign-ins must complete PGP decryption, including accounts with TOTP. The administrator must first prove PGP decryption in the current session before disabling the option. Publish an external recovery contact before enforcing this policy: keyless accounts cannot send support messages.

Vendor signup, application approval, and assignment of the vendor role require a usable public key. Vendors cannot remove their key or replace it with one that cannot receive encryption. Keys can expire after they are saved; an expired key must be renewed before new purchases or messages can proceed. Existing vendor accounts are preserved during upgrade and must add a usable key before trading if they have none.

An existing PGP identity cannot be replaced or removed with a password alone: the current session must have decrypted with the saved key, or the account must confirm with an already-enrolled authenticator. This protects expired and unreadable saved keys too. Enrolling or activating a first authenticator on an account that already has a PGP key, and viewing pending authenticator or recovery material, requires the saved-key decryption proof, so a password thief cannot enroll or copy an authenticator to replace that key. Adding a first PGP key clears any inactive authenticator enrollment that could have been exposed before the key was bound. First key enrollment and a safe same-fingerprint renewal remain available; purchases and messages still require a fresh decrypt proof after the saved key changes.

For a lost private key, verify the owner through a trusted recovery channel before rebinding their identity. The existing non-administrator factor-reset form accepts an optional replacement **public** key; the administrator must confirm that they verified the owner's identity through the trusted recovery channel, then confirm with their own password and enrolled authenticator. The replacement must be a currently usable public key. Recovery ends the target's sessions and pending sign-ins, invalidates old proofs and challenges, records the reset, and leaves the new key unverified until its owner proves it.

For the last administrator, supply the replacement public key through the existing host reset command:

```sh
opsecmkt -reset-admin-factors ADMIN_HANDLE -reset-admin-pgp-key-file /secure/replacement-public.asc
```

With Compose, bind-mount that public file read-only into the one-off app container and use its container path. The file is limited to 16 KiB; private keys are refused. Sign in with the account password and decrypt with the replacement key, then enroll MFA again. The command preserves the site's strict PGP login setting. Omitting the file preserves reset-only behavior and the existing PGP key; a factor reset alone cannot satisfy strict PGP login when the private key is lost.

### Vendor availability and activity privacy

Vendors manage availability from their account. **Vacation** keeps the profile and products public, displays **On vacation**, and refuses new purchases. **Deactivation** hides the profile and products from public catalog, search, direct product pages, and image routes. It preserves existing orders and disputes and does not archive or erase products; reactivation restores normal visibility.

A vendor may stop buyer messages while away. This hides message buttons and blocks direct sends, including direct sends through an old conversation. The separate order correspondence form remains available to the actual buyer and vendor for existing order work. When messages remain enabled, existing customers may still contact a deactivated vendor through their established order or exchange. Staff support and dispute workflows remain available.

Last-seen sharing is off by default. When enabled, the public profile shows coarse activity at hour precision. Activity is recorded only while sharing is enabled. Turning it off clears the stored timestamp and removes it from public data, rather than merely hiding a label.

### Orders

Migration 105 copies a listing’s title, full description and fulfillment kind to the order in the same transaction that first reserves stock and requests payment (Monero escrow checkout). A draft keeps showing the live listing until then. Later edits, archive and takedown leave the purchased copy unchanged; the order and dispute pages show the saved terms while their product link opens the **current** listing. An order without a copy has no trustworthy recorded terms and shows “Not recorded” rather than using today’s title or description as purchase history. The saved description is visible only through an order page its buyer, vendor or eligible reviewing staff may open. It remains in the database and encrypted backups with that order; editing or archiving the listing does not erase it. Delivery content is still governed by its existing delivery record and is not copied into this snapshot.

Both the buyer and vendor must have usable PGP public encryption keys before creating a draft or checking out, for physical and digital orders. They can add them under **Your account → Public profile**. The buyer must also complete PGP challenge decryption in the current session before purchasing. Requesting payment also requires Monero escrow to be on and ready on the configured network (use test networks for development; see Payments). Existing orders remain readable and cancellable when a key becomes unusable; opening or replying to a dispute still requires session PGP decryption.

Migration 132 makes new automatic and manual per-order digital delivery records PGP-encrypted to the buyer before storage. The buyer decrypts the displayed armor in their own PGP client. The vendor and eligible dispute staff can see the stored armor, but that alone does not let them decrypt it. The shared listing template for automatic delivery remains server-readable, and manual input is available to the running server while it encrypts it. Encryption failure refuses manual delivery or leaves automatic delivery pending without a plaintext fallback. Older plaintext delivery records are retained as they were and are never eligible for XMPP content forwarding.

**There is no shipping address field, by design.** For a paid physical order the order page asks the buyer to encrypt their address to the vendor's PGP key in their own PGP application and send it through the order page’s **Order correspondence** form, which accepts only OpenPGP-encrypted messages; the server keeps that encrypted message (and who sent it to whom and when) but cannot read it. A vendor without a key that can receive encrypted messages (none saved, or one that has expired or can only sign) cannot receive addresses; every physical or digital draft and checkout now requires usable buyer and vendor encryption keys. If the vendor's key was changed or its ownership proved again after the order was placed, the buyer's order page says so and asks them to confirm the key with the vendor before sending the address. The market cannot stop an address being typed into a plaintext field: the vendor's note to the buyer, cancellation reasons, dispute reasons, dispute replies and review text are stored unencrypted in the database and every backup. Each of those fields warns that it is unencrypted, says who can read it and asks users never to include an address, real name or tracking number but to send them encrypted from the order page (the dispute forms point to the "Contact dispute staff" keys). `/canary#records` lists what is kept.

Every disputable order is a Monero escrow order, and only an administrator decides a dispute on one (the decision moves coins from the escrow); moderators can read it on the moderation desk but not resolve it. Opening a dispute therefore notifies every administrator who is not the order's buyer or vendor and is not suspended. An administrator who is a party cannot resolve it, and a suspended account cannot sign in, so keep at least one administrator who does not trade: if every unsuspended administrator is a party (for example the only administrator is the vendor), the dispute is still accepted even when an independent moderator exists, the order history records that no independent resolver was available, every administrator is notified that it needs an administrator who is not a party, added on the host with the role command (`-set-role HANDLE admin`, [Administrators](#administrators)), independent moderators receive review notices but cannot resolve Monero escrow, and the moderation desk marks it "No eligible resolver" for administrators until such an administrator exists. A concession that closes the dispute is reported to the same administrators. Historical orders without escrow (read-only) keep the earlier rule, under which an independent moderator also counts as a resolver. On an escrow order the parties' order page says an administrator decides: its "Contact dispute staff" panel advises sending evidence to an independent administrator first and labels moderators as able to pass evidence on to an administrator; a historical order keeps the moderator wording.

The party who did not open a dispute can put one reply on record from the order page while it is open (plain text in the order history, shown to both parties and staff); the resolve form says whether it did. New image evidence is uploaded and read on the order page; vendors append replacement digital delivery there too. Private records use the order's original retention deadline and never extend it. For each open dispute the moderation desk also lists which moderators and administrators each party messaged since it opened, with the count and the latest time, from message metadata only: ask those holders what the encrypted evidence shows before deciding, since nobody else can read it.

Resolving a Monero dispute selects a release to the vendor or refund to the buyer, which the watcher builds and sends from the order account. Only an independent administrator decides it, with password confirmation and an unchanged amount. The decision is final because custody allows a send without party signatures.

Independent moderators and administrators read authorized dispute and payment-review orders from the moderation desk. They also read orders with an open Monero account-collision or settlement-wait alert; the order page explains the access reason. Other orders remain private. Staff record a refused-deposit disposition with password confirmation; ordinary buyer/vendor actions remain unavailable to staff.

Reviews are published as soon as the buyer writes them. To take one down (for example text that names or locates a vendor), sign in as an administrator, open the listing or the vendor's page, open *Hide this review* under it and confirm with your password (and authenticator code when enrolled). The review is kept but leaves both public pages and their rating; the buyer and vendor still see it on the order, marked "Not shown", and administrators see it labelled with *Restore this review*. Both actions are audited without the review text and the reviewer is notified. Migration 031 adds `reviews.hidden_at`.

Reviews by a banned account (A-649) need no action: while the ban lasts none of them, earlier or later, is shown on the listing and vendor pages or counted in a rating, and lifting the ban brings them back (nothing is written to the reviews; a review you hid stays hidden). Administrators see each one labelled "Not counted: reviewer's account is banned", with its own count above the list, and can still hide it so it stays down after an unban. The vendor (and a moderator who can read the order) sees it on the order with the same "Not shown" wording as a hidden review, so the order does not say which; the public pages say only that reviews by banned accounts are not shown or counted, never how many. The ban and unban audit rows say how many of the account's reviews stopped counting or count again. A closed account's reviews are deleted by retention instead.

### Inventory

Distinct digital inventory accepts plain text and links, refusing armored PGP MESSAGE entries at import and checkout; the combined batch is encrypted once to the eventual buyer. Use manual per-order delivery for ciphertext already encrypted to that buyer.

Only approved vendors (see [Vendor applications](#vendor-applications)) can list. Vendors manage listings from the vendor desk (Edit / Archive / Restore). The vendor desk's readiness panel lists the current encryption-key and escrow-key setup needed for new orders; a vendor can also open it from the setup link on `/account`. Check the panel after changing a key or the market's escrow configuration. Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after an escrow is funded; with escrow off it is never released automatically. Administrators can edit other vendors' listings but cannot view or change their automatic delivery content: the editor shows only whether it is set, and only the vendor can change or remove it. A save that changes another vendor's listing notifies that vendor, naming the fields changed, and is recorded in the vendor's audit trail as well as yours (A-516).

To remove another vendor's listing (for example counterfeit or prohibited goods) while the vendor keeps selling everything else, open its edit page as an administrator, open *Take down listing*, give the vendor a reason of 10–500 characters and confirm with your password (and authenticator code when enrolled). The listing is archived; the vendor is notified with the reason and sees it on the listing and the vendor desk, can still edit the listing, but cannot restore it. The listing leaves the marketplace, but the buyers of its existing orders, and moderators and administrators reviewing those orders, can still open its page (marked Archived, without the reason) from the order. The reason is stored unencrypted and kept in both accounts' activity logs; it is never shown publicly, but do not put an address or real name in it. While the listing is taken down, paid orders on it are not delivered automatically: they wait for the vendor to deliver by hand or cancel and refund (cancelling a funded Monero escrow order requests its refund, which the market builds and sends). *Restore listing* on the same page, confirmed the same way, undoes it. To take away all of a vendor's listings, change their role instead (that archive carries no reason and the vendor can restore those listings if made a vendor again). Migration 041 adds `products.admin_archive_reason`.

### Payments

Monero (XMR) is the only payment currency. Per-order custodial escrow is off by default; the market holds wallet keys and can move the coins. A node alone does not enable it. Configure the dedicated wallet-rpc, offline seed recipient and wallet secret using [Enabling Monero payments](install/enable-payments.md).

`MONERO_NETWORK` selects `mainnet` or `stagenet` (blank defaults to stagenet); `MONERO_NETWORK_FLAG` sets the matching bundled daemon and wallet-rpc flag. The installer writes mainnet by default and never changes an existing network on upgrade. Local fakechain tests are separate. Monero testnet is unsupported.

`PAYMENT_CONFIRMATIONS_XMR` defaults to 10. `PAYMENT_POLL_INTERVAL` defaults to 30 seconds. `MONERO_MAX_TIP_AGE` defaults to one hour and is bounded from 20 minutes to 24 hours. The daemon's sync state, peers and tip age gate watcher work; a local fakechain is exempt from public-network peer and tip checks. Unsynced nodes do not credit deposits, expire payment windows or relay settlements. Physical release deadlines still apply.

Monero hides transaction details on-chain. The operator still sees account, order, address, amount, timing and transaction metadata and controls escrow keys. Use fresh external refund and claim addresses, encrypt private messages to their recipients, and keep server and wallet access restricted. Tor and Monero do not guarantee anonymity or protect a compromised host.

#### Mainnet

Mainnet moves real funds and has no recorded run in this project. Mainnet evidence is limited to unit, database and simulated-RPC tests. Rehearse on stagenet first, use a fresh database and wallets for each network, and back up the wallet secret, encrypted seeds and wallet files. Existing network pins cannot be changed while escrow state remains.

#### Overpayments and the payment QR code

Use the exact address and amount shown on the order. A QR code is a convenience; compare its address with the rendered text in your own wallet. Monero escrow accounts are used by one order. Settlements, refunds and stray returns follow the rules below.

#### Monero escrow

**Off by default, custodial, on a fakechain or stagenet (or mainnet on a mainnet market, the installer's default: [Mainnet](#mainnet)).** [Per-order escrow](escrow.md) Phase 3 lets the
buyer of an XMR draft on a digital listing with automatic delivery, or on a physical listing with a dispatch window, pay into
an account of a monthly escrow wallet the market holds. **The market holds the keys and can
move these coins**: there is no 2-of-3, no key of the buyer's or
vendor's and nothing for them to sign. It runs against a local Monero fakechain (`monerod --regtest`) or, since A-449, Monero
stagenet (`monerod --stagenet`); coins on both have no value. On a market with `MONERO_NETWORK=mainnet` it runs on
mainnet instead (real funds, no recorded run; [Mainnet](#mainnet)); it refuses testnet, a mainnet daemon on any other
market, and anything else. The [public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) covers one funded claim; broader public journeys
remain unverified. Offline-fork evidence is separate ([per-order escrow](escrow.md#monero-escrow-on-stagenet-a-449)).
With Monero escrow off, Monero checkout is unavailable. What it does, protects and does not protect, and the tests behind it, are in
[per-order escrow, Phase 3](escrow.md#phase-3-monero-escrow-on-a-local-fakechain) and
[Monero escrow on stagenet](escrow.md#monero-escrow-on-stagenet-a-449).

You need four things before switching it on: a fakechain or stagenet daemon, a monero-wallet-rpc of its own on that network,
an OpenPGP key whose private half stays offline, and a secret. A regtest chain makes a block only when something mines one
(the checks use the daemon's `generateblocks`); the market never does, so on a fakechain nothing confirms a deposit until you
mine. On stagenet the public network mines blocks about every 2 minutes; the daemon must be synced and have peers (below).

**Settings.** All are read at startup, from `.env` through Compose. With `ESCROW_XMR` blank or `off` the escrow configuration
is not used, no seed-key file is opened and no escrow node is contacted. Historical Monero escrow recovery state remains visible
under `/admin/payments` when present; turning escrow off does not delete wallets, orders or settlements.

| Setting | Default | What it takes |
| --- | --- | --- |
| `ESCROW_XMR` | blank (off) | `on` or `off`; any other value stops startup. On a mainnet market (`MONERO_NETWORK=mainnet`) it runs on mainnet only; without `MONERO_NETWORK=mainnet` a mainnet daemon is refused ([Mainnet](#mainnet)). |
| `ESCROW_XMR_WALLET_RPC_URL` | none; required when on | `http(s)://[user:password@]host:port` of the dedicated Monero escrow wallet-rpc (credentials are answered with HTTP Digest). Keep it dedicated to escrow wallets and attach it to the daemon configured below. A non-blank `MONERO_WALLET_RPC_URL` (an older release's setting) is refused before startup. |
| `ESCROW_XMR_DAEMON_URL` | none; required when on | The same form, for the fakechain, stagenet or (opted in) mainnet `monerod` used by this escrow wallet-rpc. The bundled `monero` service may be used when configured for stagenet (or, with a blank `MONERO_NETWORK_FLAG=`, mainnet) and can serve restricted RPC; set this key to `http://monero:18081` rather than the retired `MONERO_RPC_URL`. |
| `ESCROW_XMR_FEE_BPS` | `0` | The market fee on a fulfilled order, a whole number of basis points from 0 to 10000 (250 = 2.5%). Each escrow keeps the fee in force at its checkout. Host-only: there is no form or settings row for it (see *Market fee* below). |
| `ESCROW_XMR_FEE_ADDRESS` | none | Required above 0 basis points, ignored at 0: a standard address or subaddress of the escrow daemon's network (95 characters; stagenet ones start with 5 or 7, fakechain and mainnet ones with 4 or 8; integrated addresses are refused; a mainnet market needs a mainnet address) whose keys are valid, checked in Go at startup: one of the other network stops startup once the daemon is reached. A fee below 0.001 XMR is added to the vendor's output instead. |
| `ESCROW_XMR_SLOTS` | `20` | The free accounts the watcher keeps ready on the active wallet, a whole number from 5 to 200. Checkout takes one; none free is a 503 until the watcher makes more. |
| `ESCROW_XMR_MAX_FEE` | `10000000000` (0.01 XMR) | The highest network fee a settlement may pay, in piconero, a whole number from 1 to `100000000000` (0.1 XMR). A transaction the wallet builds above it is discarded unrelayed and its settlement waits. A young fakechain charged about 0.0018 XMR for a one-input, two-output transfer. |
| `MONERO_WALLET_SECRET` | none; required when on | At least 32 characters, random (not a placeholder, at least 8 distinct characters, no repeating pattern), and different from `SETUP_TOKEN` and `APP_SECRET_KEY`: `openssl rand -hex 32`. Each wallet's password derives from it. |
| `ESCROW_XMR_SEED_KEY` | none; required when on | The path, inside the app container, of a file holding one armored OpenPGP public key (at most 64 KiB). A private key block, several keys, a revoked key or one with no encryption key usable now (signing-only, expired) stops startup. |
| `PAYMENT_CONFIRMATIONS_XMR` | `10` | A whole number from 1 to 1000. A Monero escrow deposit counts from the larger of it and 10. |
| `ESCROW_XMR_TOR_RELAY` | blank | Blank or `clearnet-accepted`; any other value stops startup. With `APP_MODE=tor`, `clearnet-accepted` lets Monero escrow settlements be relayed without the node Tor overlay (*Tor mode*, below). Ignored in clearnet mode. |

**Market fee (host-only).** The fee rate and the address it is paid to are set only on the app host, by
`ESCROW_XMR_FEE_BPS` and `ESCROW_XMR_FEE_ADDRESS`: no `/admin` page, form or settings row changes them, so an
administrator web account, even one someone else has taken over, cannot change or redirect the fee. *Admin → Payments*
(*Market fee*) shows the rate and the full address read-only, labelled "Set on the server; administrators cannot change it
here", says that no market fee is charged at 0 basis points or while Monero escrow is off, and names the command that
changes it. On a native installation, edit the two keys in the `settings` of the private `operator.json`, check with
`./opsecmkt deploy plan --dir DIR`, then restart only the app with
`./opsecmkt deploy redeploy --dir DIR --app-image IMAGE@sha256:DIGEST`, giving the `app_image` already recorded in
`operator.json`; `redeploy` keeps keys, endpoints, workers and volumes ([Binary installation](install/binary.md#rebuilding-or-replacing-only-the-application)).
A Compose installation edits `.env` and runs `docker compose up -d app`. The values pass the startup checks above; then
each start with Monero escrow on compares them with the last values it recorded (the settings row `escrow_xmr_fee_seen`, a
record that is never read as the fee). On a change it records the new values, writes one audit row naming the old and new
rate and address ("Monero escrow market fee changed on the server (seen at startup): ...") and notifies every
administrator who is not suspended once; an unchanged restart writes nothing. The first start of this release, or of a
database that has no record, announces the configured fee once the same way. A notice whose new rate or address no
operator configured means someone changed the host's configuration: treat it as a compromise of the app host, not of an
administrator account. A database restore cannot change the fee in force, which is host configuration; it can roll the
record back, so the next start after a restore may announce the host's current, unchanged fee once more (as does an
edited record). While `ESCROW_XMR` is off the fee is not read and nothing is compared. A refused fee
destination on a settlement is repaired with the host's current `ESCROW_XMR_FEE_ADDRESS` after a restart (below).

`PAYMENT_EXPIRY` (the payment window), `PAYMENT_POLL_INTERVAL` and `MONERO_MAX_TIP_AGE` apply to Monero escrow. The
refusal messages name the setting and never quote its value.

**At startup.** A configuration error, an unusable seed key file, a network the market is not configured for, or an escrow node that answers but is
not what Monero escrow needs stops startup with a message that names the setting (for a node, that the daemon at ESCROW_XMR_DAEMON_URL failed the network check, or the same for the wallet-rpc).
The daemon must report nettype `fakechain` (with no network flag) or `stagenet` (with the stagenet flag), or mainnet on a
market with `MONERO_NETWORK=mainnet`, and a hard fork version of at least 16 (a daemon still syncing reports the hard fork of
the height it has reached, so a lower version is not refused while `get_info` says it is syncing: Monero escrow stays
disabled with `… could not be checked at startup (hard_fork_info daemon-syncing)` and is checked again every poll, so a
fresh stagenet node delays Monero escrow, not the market), and the escrow wallet-rpc must, after a refresh,
be within 10 blocks of that daemon (so a wallet-rpc attached to another chain is refused). The daemon's height is read
again after the refresh before a wallet more than 10 blocks from it is refused, and a wallet within 10 blocks of the span
between the two readings passes (A-475), so a daemon that gains blocks while the wallet refreshes (catching up after
downtime, say) does not disable Monero escrow. Before A-449 the daemon also had to
be below height 1,000,000 (no fakechain gets there; every public network is past it): that heuristic is gone, and the network
pin below tells networks apart instead. The escrow wallet-rpc must be attached to the configured escrow daemon; its refreshed
height is checked against the daemon's height, and the daemon must report local fakechain, stagenet or (opted in) mainnet. A wallet-rpc with no wallet open is not a failure. Addresses (refund, claim,
the fee address, and every address the wallet-rpc returns) are accepted only in the forms of the network the daemon was
verified on: stagenet 5… and 7…, a fakechain 4… and 8… (it uses mainnet's address format), mainnet 4… and 8…. A node that cannot be
reached leaves Monero escrow not ready with `escrow: Monero escrow disabled: the escrow daemon or wallet-rpc could not be
checked at startup (<class>)` while the market runs; the escrow watcher retries every `PAYMENT_POLL_INTERVAL` and logs
`escrow: Monero escrow on (stagenet)` (or `(fakechain)`) when it passes. A line is logged when the state changes. A node that
answers as another network after startup disables Monero escrow until the app is restarted. While Monero escrow is
configured but not ready, for any of these reasons, `/admin/payments` also shows an alert under *Escrow status and payment intake* ("Monero escrow
not ready: …") with the current failed check and its remedy. The escrow-only readiness panel reads `XMR escrow: on (not ready: …)`,
and *Escrow status and payment intake* names no Monero escrow network until a daemon was verified on one; the alert goes once Monero
escrow is ready (QA4W4B-1). Two more lines mean the pins below have
refused: `escrow: Monero escrow disabled: MONERO_WALLET_SECRET differs from the secret this database was first started with
…` (nothing is created or opened) and `escrow: Monero escrow wallet creation stopped: ESCROW_XMR_SEED_KEY is not the seed key
pinned in this database …` (existing wallets are still read and their orders funded; no wallet is created).

**The network pin (A-449).** The database records the network its Monero escrows are on in `settings` (key
`escrow_xmr_network`: `fakechain`, `stagenet` or `mainnet`). While the database holds no Monero escrow and no escrow wallet, the pin
follows the daemon and may change. Once it holds either (a wallet counts: its free accounts are addresses of one network),
a daemon on the other network leaves Monero escrow disabled and changes nothing, with one line, `escrow: Monero escrow
disabled: the escrow daemon is not on the network this database's Monero escrows are on (escrow_xmr_network) …`, and a line
under *Escrow status and payment intake* on `/admin/payments` naming both networks; the check is retried every `PAYMENT_POLL_INTERVAL`, so pointing
`ESCROW_XMR_DAEMON_URL` (and the escrow wallet-rpc) back at the pinned network brings it back. A new network needs a new
database: there is no supported way to move a database's Monero escrows to another network. A database from before A-449
that holds Monero escrow state and no pin is pinned `fakechain` at the first start against a fakechain daemon (the only
network Monero escrow ran on before); against a stagenet daemon it is refused as above, and nothing is written.

**When the daemon is not synced (A-449).** Each watcher pass first reads the escrow daemon (`get_info`, and on mainnet or stagenet
`get_last_block_header`). While it reports `synchronized` false or a `target_height` above its height, or, on either public network, has
no peers (`offline`, or no connections on an unrestricted RPC; a restricted RPC hides the counts) or a top block older than
`MONERO_MAX_TIP_AGE` (default 1h; [Payments](#payments)), the pass opens no wallet, creates or destroys none, reads no
deposit, funds or expires no order and builds, relays or follows no settlement; release deadlines still apply. One line is
logged when this starts (`escrow: Monero escrow daemon not synced (<reason>); Monero escrow watcher passes skipped until it
is`) and one when it ends (`escrow: Monero escrow daemon synced; Monero escrow watcher passes resumed`), and `/admin/payments` shows
the reason under *Escrow status and payment intake*. Nothing is stored for this. A fakechain is exempt from the peer and tip-age checks.
If the daemon cannot be read at all, or its top block cannot (`get_last_block_header` fails), the pass applies only the
deadlines and logs `monero escrow watcher: monero escrow: daemon check failed: <class>`. On a restricted RPC (the bundled
`monero` service) a daemon cut off from its peers looks synced until its top block is older than `MONERO_MAX_TIP_AGE`
(default 1 hour, at least 20 minutes): a deposit made in that window is not seen, an order whose payment window ends in it
can be cancelled as unpaid, and the deposit then goes back to the buyer's refund address by a stray refund once the daemon
sees it and it confirms. A lower `MONERO_MAX_TIP_AGE` narrows the window ([Payments](#payments)).

**Tor mode: relay waits for an explicit routing choice.** The escrow wallet RPC
relays a settlement through its daemon. A daemon with direct peer access can
expose the payment host's IP address. With `APP_MODE=tor` and `ESCROW_XMR=on`,
the watcher builds, checks and stores settlements but holds relay until one of
these configurations is selected:

| Configuration | Meaning |
| --- | --- |
| Blank `ESCROW_XMR_TOR_RELAY`, with `compose.nodes-tor.yaml` in `COMPOSE_FILE` | Compatibility path for the bundled local daemon and its Tor client. The overlay does not cover an external daemon. [Monero over Tor remains unverified](#local-nodes-over-tor). |
| `ESCROW_XMR_TOR_RELAY=remote-tor` | Production declaration that the operator configured and checked Tor egress on the separate payment host. Requires the complete production contract, Tor exposure, enabled escrow and separate authenticated HTTPS Monero endpoints. |
| `ESCROW_XMR_TOR_RELAY=clearnet-accepted` | Explicitly accepts direct relay exposure. This setting does not configure Tor. |

A native production deployment with a blank setting keeps relay held because
its application-host configuration contains no local node overlay. The
administrator panel and order pages show the hold. Restart after changing the
saved routing choice; the next eligible pass relays each stored transaction
unchanged, without building a replacement.

### Declaring separate payment-host Tor egress

You need administrative access to the payment host and its network controls.
The application validates endpoint declarations but cannot inspect remote Tor,
firewall behavior or physical placement. `remote-tor` is an operator declaration,
not proof of anonymous delivery.

1. Run the daemon and dedicated wallet RPC on the separate payment host. Keep
   raw RPC listeners private. Expose authenticated HTTPS only to the app's
   private route; mutual TLS at a dedicated front proxy is supported.
2. Give the daemon a Tor SOCKS proxy with `--proxy=PROXY_IP:9050` and block its
   direct internet route. In Docker, attach the daemon only to internal
   networks and let a separate Tor container alone join an egress network.
   Keep the wallet connected to that same daemon. Preserve the Tor client's
   guard state across restarts.
3. Verify Tor bootstrap, daemon peer connections and synchronization. From the
   daemon's network boundary, confirm direct external connections fail. During
   a controlled maintenance window, stop the proxy and confirm peer traffic
   cannot fall back to direct access. Restore the proxy and recheck sync.
4. Initialize with `--set ESCROW_XMR_TOR_RELAY=remote-tor`. For an existing native
   installation, update that setting in its private `operator.json`, inspect
   `deploy plan --dir DIR`, then run `deploy up --dir DIR`. The plan and
   administrator panel retain the routing limitation notice.
5. Confirm the relay hold cleared and the payment readiness panel is healthy.
   Recheck network enforcement after host, Docker, proxy or firewall changes.
   Keep intake paused until readiness and recovery checks are complete.

Monero documents the distinction between the general `--proxy` route and the
optional `--tx-proxy` anonymity-network route in its
[proxy reference](https://docs.getmonero.org/interacting/monerod-reference/#tori2p-and-proxies).
Keep the proxy's failure behavior enforced by network policy. A successful RPC
response or `remote-tor` declaration alone does not establish that behavior.

**Stagenet.** Point `ESCROW_XMR_DAEMON_URL` at a synced stagenet `monerod` (the bundled `monero` service with
`MONERO_NETWORK=stagenet`, `http://monero:18081`, is one; its restricted RPC answers every call escrow makes) and run the
escrow wallet-rpc with `--stagenet` against the same daemon (the `monero-escrow-wallet` Compose service below does, and the
installer sets it up). The bundled `monero` daemon may serve the escrow wallet-rpc; the wallet-rpc itself must be dedicated to escrow. Give buyers' refund and vendors' claim addresses from stagenet wallets (5… or 7…), and an `ESCROW_XMR_FEE_ADDRESS`
on stagenet. Stagenet coins come from the public faucet or your own mining; they have no value. Keep `ESCROW_XMR_MAX_FEE`
in mind: the default 0.01 XMR cap was sized for a fakechain and is not measured on stagenet.

**The dedicated escrow wallet-rpc.** Run a dedicated `monero-wallet-rpc` for escrow wallets, attached to the escrow
daemon, with its own authenticated RPC boundary and volume. A non-blank `MONERO_WALLET_RPC_URL` is refused.

*Background refresh is off (A-641).* Right after each `open_wallet`, the market sends `auto_refresh` with
`{"enable": false}` to the escrow wallet-rpc. It then refreshes every wallet it opens itself. monero-wallet-rpc 0.18.5.1
has no command-line flag for this. The setting applies to the whole wallet-rpc and lasts until it restarts, so the
market sends it after every open. With the background refresh on, an unreachable daemon can block the wallet-rpc for
about 40 seconds, and a settlement relay can then time out instead of being refused. If you use the escrow wallet-rpc
by hand, do not turn `auto_refresh` back on. If the call fails, the market logs `background refresh not turned off`
once while the failure lasts, and carries on.

Production can authenticate at a private mutual-TLS front proxy using
`MONERO_RPC_CERT` and `MONERO_RPC_KEY`. With this arrangement, a wallet listener
using `--disable-rpc-login` must stay on an unpublished, isolated network behind
that proxy. Permit the app's private route to reach only the authenticated HTTPS
listener. Do not expose the raw wallet port. Keep the proxy's read timeout above
the application's eight-minute wallet-refresh deadline. The stable
`MONERO_WALLET_SECRET` belongs to the app's protected configuration; the app uses
it to derive wallet passwords. Back it up with the database and wallet files.

*On stagenet, with the bundled nodes (A-449).* `compose.nodes.yaml` has a `monero-escrow-wallet` service for it, in its
own profile (`monero-escrow-wallet`) and volume (`monero_escrow_wallet`), with its own login (`escrow`,
`ESCROW_XMR_WALLET_RPC_PASSWORD`), run with `--stagenet` against the bundled `monero` daemon, which must then run stagenet
(`MONERO_NETWORK=stagenet`, selected explicitly). Its memory limit is 1 GiB (PAYE-E2 measured 574 MB at peak over five
10,000-account wallets). With a blank `ESCROW_XMR_WALLET_RPC_PASSWORD` Compose refuses every command with the profile active
(`env file …/ESCROW_XMR_WALLET_RPC_PASSWORD-is-blank not found`, A-466; with the limits named under
[Payments](#payments): a project directory more than about 31 levels deep, a host without `/dev/null`, a password of only
spaces); were the service started anyway, its login would
be `:`, which monero-wallet-rpc refuses at startup (a login of `escrow:` would start it with an empty password; probed on
0.18.5.1). The installer sets it
all up: `./scripts/install.sh --monero local --monero-network stagenet --monero-escrow yes --monero-escrow-seed-key FILE`,
or answer yes where it asks about Monero. It generates `ESCROW_XMR_WALLET_RPC_PASSWORD` and `MONERO_WALLET_SECRET` (never a
default), writes `ESCROW_XMR=on`, `ESCROW_XMR_WALLET_RPC_URL=http://escrow:PASSWORD@monero-escrow-wallet:18083`,
`ESCROW_XMR_DAEMON_URL=http://monero:18081` and `ESCROW_XMR_SEED_KEY=/etc/opsecmkt/escrow-xmr-seed-key.asc`, adds the
profile, copies the public key you give (an armored `-----BEGIN PGP PUBLIC KEY BLOCK-----`; a private key block is refused)
to `escrow-xmr-seed-key.asc` in the checkout (gitignored) and adds `compose.escrow-xmr.yaml`, which mounts it read-only into
the app. `./scripts/install.sh check` refuses the profile without its password and the overlay without the key file. Back
up `.env` (it holds `MONERO_WALLET_SECRET`) and the `monero_escrow_wallet` volume yourself: `scripts/backup.sh` copies the
database, not wallet volumes. To turn it on for an existing install, add the same keys, profile, file and overlay by hand.

*On a local fakechain.* The Compose files have no service for a fakechain daemon or its wallet-rpc: the example below is a
plain `docker run` on the Compose backend network (`<COMPOSE_PROJECT_NAME>_backend`, `opsecmkt_backend` by default), run
(with other names and a throwaway password) against a fakechain `monerod`, and not checked as a Compose service. Use a long
random password without a single quote; put the same credentials in `ESCROW_XMR_WALLET_RPC_URL`
(`http://marketplace:PASSWORD@opsecmkt-escrow-wallet:18083`).

```sh
docker run -d --name opsecmkt-escrow-wallet --network opsecmkt_backend --restart unless-stopped \
  -v opsecmkt_escrow_wallets:/home/monero/wallet \
  ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
  --daemon-address FAKECHAIN_MONEROD_HOST:18081 --trusted-daemon --allow-mismatched-daemon-version \
  --wallet-dir=/home/monero/wallet --shared-ringdb-dir=/home/monero/wallet/.shared-ringdb \
  --rpc-bind-port=18083 --rpc-login 'marketplace:PASSWORD'
```

`--shared-ringdb-dir` keeps the wallet-rpc's ring database inside the wallet volume, as the `monero-escrow-wallet` service
does, so that it is backed up with the wallets and the [stale-ring break-glass](#stale-ring-break-glass-a-618) finds it.

Use the pinned wallet-rpc release the escrow checks used (0.18.5.1).
The fakechain daemon is started as the header of `scripts/test-escrow-xmr-regtest.sh` starts one (`--regtest
--keep-fakechain --offline --fixed-difficulty 1`; without `--keep-fakechain`, monerod deletes the fakechain each time it
starts); it is addressed through `ESCROW_XMR_DAEMON_URL`; the obsolete `MONERO_RPC_URL` must remain blank. No wallet needs creating: the escrow
watcher creates every wallet, named `escrow-YYYYMM-NN`, on its first pass. The escrow wallet-rpc must be dedicated to escrow and attached to this daemon; readiness checks verify its daemon network and refreshed height, not a pooled-wallet URL or address.

**The seed key.** Every wallet's 25-word seed is encrypted to this key when the wallet is created, so that the database never
holds a seed in the clear. Generate it **offline**, on a machine that never runs the market, and keep its private half offline.
It must never be the canary's operator key: that key can be changed from `/admin/transparency`, and taking over an administrator account
must not be able to redirect future seeds. The app reads only a file, so only the public half goes to the server.

```sh
gpg --quick-generate-key 'Monero escrow seeds' default default never   # asks for a passphrase; creates a signing key and an encryption subkey
gpg --list-keys --with-colons 'Monero escrow seeds' | sed -n 's/^fpr:::::::::\([0-9A-F]*\):$/\1/p' | head -n 1
gpg --armor --export FINGERPRINT > escrow-xmr-seed-key.pub.asc
```

Choose a key without an expiry: once its encryption key has expired the app refuses to start. Extending an expiry keeps the
fingerprint and needs no re-pin. Copy only `escrow-xmr-seed-key.pub.asc` to the server and mount it read-only into the app
container at the path `ESCROW_XMR_SEED_KEY` names, for example in a Compose file you add to `COMPOSE_FILE` (a
`compose.override.yaml` is not read once `COMPOSE_FILE` is set):

```yaml
services:
  app:
    volumes:
      - ./escrow-xmr-seed-key.pub.asc:/run/escrow-xmr-seed-key.pub.asc:ro
```

and `ESCROW_XMR_SEED_KEY=/run/escrow-xmr-seed-key.pub.asc` in `.env`. The app seals each seed once, when it creates or finds
the wallet, and never re-seals it to a later key.

**The pins.** On its first start with Monero escrow on, the app records two values in `settings` and checks them on every
start and every watcher pass. `escrow_xmr_secret_verifier` is an HMAC of a fixed label under `MONERO_WALLET_SECRET` (never a
password): with another secret Monero escrow stays disabled rather than create wallets nobody could open with the old one,
and **no tool re-keys the existing wallets**, so keep the secret with `.env`
([Secrets](#secrets-env-and-onion-identities)) and restore the original if it is lost or changed.
`escrow_xmr_seed_key_fingerprint` is the key's fingerprint: with another key the app still reads existing wallets but creates
none, until you re-pin it. Replace the key only when the old one must go. Wallets already sealed to the old key stay sealed
to it, so keep the old private key until every wallet sealed to it has been destroyed. List them, then pin the new key's
fingerprint (uppercase hex, no spaces; the SQL changes nothing else and the audit row names no key):

<!-- runbook-sql: xmr-seed-key-repin -->
```sh
db_sql -v new_fingerprint=NEW_FINGERPRINT_IN_UPPERCASE_HEX <<'SQL'
SELECT name, state, seed_key_fpr FROM xmr_escrow_wallets WHERE seed_sealed IS NOT NULL ORDER BY id;
BEGIN;
UPDATE settings SET value=:'new_fingerprint' WHERE key='escrow_xmr_seed_key_fingerprint';
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Monero escrow seed key re-pinned by hand (docs/operator-guide.md)');
COMMIT;
SQL
```

(`db_sql` is the helper of the [runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
step: `docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt "$@"` with your database name.) Set
`ESCROW_XMR_SEED_KEY` to the new key's file first and restart the app; the next watcher pass then creates wallets again.
On a database that has never pinned a key (Monero escrow never started on it) the `UPDATE` reports `UPDATE 0`: the first start pins
the key it finds.

**The offline seed check.** Do this once after the first wallet is created, and again after a change of key, to prove that a
seed can be recovered. It uses only the database, the private key and a scratch wallet-rpc that is not the escrow one.

1. Copy one wallet's sealed seed, its primary address and its restore height out of the database:

   ```sh
   db_sql -X -At -c "SELECT seed_sealed FROM xmr_escrow_wallets WHERE name = 'escrow-YYYYMM-NN'" > seed.asc
   db_sql -X -At -F ' ' -c "SELECT restore_height, primary_address FROM xmr_escrow_wallets WHERE name = 'escrow-YYYYMM-NN'"
   ```

2. On the offline machine with the private key: `gpg --decrypt seed.asc` prints the 25 words. Do not paste them anywhere
   else; delete `seed.asc` and the terminal history afterwards.
3. Restore them in a scratch wallet (a separate wallet-rpc on the same fakechain, never the escrow wallet-rpc) from that
   restore height, and compare the `address` it answers with the primary address from step 1; they must be equal (add
   `--digest -u USER:PASSWORD` to `curl` if the scratch wallet-rpc has a login):

   ```sh
   curl -s -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":"0","method":"restore_deterministic_wallet","params":{"filename":"seed-check","password":"","seed":"THE 25 WORDS","restore_height":RESTORE_HEIGHT,"language":"English","autosave_current":false}}' http://SCRATCH_WALLET_RPC/json_rpc
   ```

   Then delete the scratch wallet's files. A wallet restored this way has your own password, not the derived one; to put it
   back under the escrow wallet-rpc after a loss, use the same call on the escrow wallet-rpc with `"filename"` the
   wallet's name and `"password"` the derived password, which is the hex HMAC-SHA256 of the name:

   ```sh
   printf 'opsecmkt xmr escrow wallet\0%s' escrow-YYYYMM-NN | openssl dgst -sha256 -hmac "$MONERO_WALLET_SECRET" -r | cut -d' ' -f1
   ```

   A wallet restored from its seed into the same wallet-rpc keeps that wallet-rpc's ring database (`.shared-ringdb`),
   which is not part of the wallet files: a restore from seed does **not** clear a stale ring
   ([stale-ring break-glass](#stale-ring-break-glass-a-618)).

**Backups.** Two things are backed up, and a wallet is lost only if both are lost:

- **The database** holds each wallet's sealed seed, its primary address and every slot, order and settlement. Take a database
  backup (`scripts/backup.sh`, [Backups and recovery](#backups-and-recovery)) **at least once a day**; nothing in this
  repository schedules it. That interval is what the 7-day lead assumes (A-403): the watcher creates next month's wallet at least
  7 days before its month, so that a daily backup holds the wallet's seal before it takes an order. The lead does not apply to
  a first run, or after the market was down past a month's start: that month's wallet is created and takes orders at once,
  and if the database and the wallet volume are both lost before the next backup, the wallet and the orders in it are lost.
- **The escrow wallet volume** holds the wallet files. Back it up at least as often, with the wallet-rpc stopped so the files
  are consistent (the watcher stores each wallet at the end of its work), encrypted like any secret. With the example above:

  ```sh
  umask 077
  mkdir -p backups
  docker stop opsecmkt-escrow-wallet
  docker run --rm --log-driver none --network none -v opsecmkt_escrow_wallets:/wallets:ro --entrypoint tar \
    ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 -C /wallets -cf - . | age -r "$AGE_RECIPIENT" -o "backups/escrow-wallets-$(date -u +%Y%m%dT%H%M%SZ).tar.age"
  docker start opsecmkt-escrow-wallet
  ```

With `--shared-ringdb-dir` inside the wallet directory (the `monero-escrow-wallet` service and the example above), the copy
also holds the wallet-rpc's ring database, `.shared-ringdb`, and any directory the
[stale-ring break-glass](#stale-ring-break-glass-a-618) moved aside. Keep both: the stored rings are what make a coin
respent after a reorganisation use the ring it used before. Check that the file is not empty before you rely on it. While it is stopped, deposits are not read and nothing is built or sent; the watcher carries on when it is back. To restore the
volume from such a copy into an empty one (set `AGE_IDENTITY` to your age identity file and `backup` to the copy), with the
wallet-rpc stopped or not yet started:

```sh
docker volume create opsecmkt_escrow_wallets
age -d -i "$AGE_IDENTITY" "$backup" | docker run --rm -i --network none --user 0 -v opsecmkt_escrow_wallets:/wallets --entrypoint tar \
  ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 -C /wallets -xf -
```

Backups keep wallet copies until they expire. A database dump holds sealed seeds and a volume copy holds wallet files, also of
wallets the market has since destroyed; destruction reaches neither. Decide how long copies live, and delete them with the
rest of a destroyed wallet's records.

**Restore.** Follow [Backups and recovery](#backups-and-recovery) and the
[reconciliation procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts), with the app stopped and
the `.env` (the same `MONERO_WALLET_SECRET`, `ESCROW_XMR_SEED_KEY`) from the backup set. `scripts/restore.sh` then, for
Monero escrow: holds every restored settlement that has a stored transaction (built or relayed) until you confirm it on
`/admin/payments` (below); retires every free account; moves every active or sealed wallet to closing, so that no account of a
restored wallet, one the restored records lack included, is offered to a new order; asks for the release deadline recheck
(`escrow_xmr_release_recheck`: on the watcher's first pass after the gate is cleared, every passed deadline of a shipped
order moves 72 hours later and its buyer is told); and sets `xmr_escrow_sweep_required`, which makes Monero escrow checkout
answer 503 ("Monero escrow is paused until this market has checked its escrow wallets after restoring its records from a
backup …"), and creates no wallet or account, until the watcher's **sweep** has run. The sweep needs the escrow wallet-rpc and
the seed key; it runs on every pass, gate or not, and only a pass that did all of it clears the marker (`escrow: Monero
escrow's restore sweep is complete; Monero escrow checkout is open again once the restore gate is cleared`). It
probes for wallets created after the backup and seals each it can open to the seed key as an *orphan* (its funded accounts are
listed to the administrators, who check them for coins of orders the database lost), records any it cannot open as an orphan
too, reads every wallet that may hold coins, and gives a wallet older than the database a longer subaddress lookahead and a
rescan so that deposits to accounts it does not know are seen. Then [clear the gate](testnet-runbook.md#clear-the-gate) as the runbook describes.

**What staff see while a restore pauses Monero escrow** (A-559). While the restore gate or the sweep is set and
`ESCROW_XMR` is on (with it off a restore pauses nothing here and none of this shows), the staff
summary on `/admin` and `/moderator` carries an alert, *Paused after a restore from backup*, naming what is paused and what
still runs: under the gate, new Monero escrow checkouts, settlement building and relay (except a held settlement an
administrator confirms, relayed once), re-requests, release deadlines and their reminders, wallet, account and vendor
bond address creation, recording a vendor bond as paid and wallet destruction, while deposits are still watched and
relayed settlements followed to their confirmations; under the sweep alone, checkout, wallet, account and bond address
creation, bond recording and wallet destruction, while settlements, relays and release deadlines run as usual. *Admin → Payments* flags it on *Monero escrow recovery and
retention* (`Restore gate: set|not set. Restore sweep: pending|not pending.`) with the count of Monero escrow checkouts
refused while paused since the server started, and *Escrow readiness* reads `XMR escrow: on; intake: paused (restore).`
even though the intake setting says escrow. A refused checkout answers 503 with `Retry-After: 5` and the reason on the
refusal page; it is logged as `http paused kind=action name=/orders/escrow-xmr status=503 cause=restore-gate` (or
`restore-sweep`), not as an `http 5xx` line, and is not listed under *Recent server errors*. When the sweep completes while
the gate is still set, every administrator gets one notification ("Monero escrow's restore sweep is complete … The restore
gate is still set …"); later passes send no other. The alert and flag clear once the gate is cleared and the sweep has run.

If you restored **only the escrow wallet volume** from an older backup and the database is current, no restore script ran,
so set the marker yourself before the app starts; the sweep then probes, reads and raises the lookahead as above. Set it
again **whenever you later restore a missing wallet's file** (from a backup or from its seed): a wallet that would not open
while the sweep ran counted as checked and got no lookahead, so the sweep must run again once its file is back:

<!-- runbook-sql: xmr-sweep-marker -->
```sh
db_sql <<'SQL'
INSERT INTO settings(key,value) VALUES ('xmr_escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
SQL
```

A wallet created after that backup is not in the volume: it is marked missing (below) until you restore it from its sealed seed
(the offline seed check above, with `"filename"` the wallet's name and the derived password), and then the sweep marker must be
set again: the sweep is the only thing that raises a restored wallet's subaddress lookahead, and the wallet-rpc's default reaches 50
accounts, so without the sweep a deposit to a later account is not seen (the `Lookahead` leg of the fakechain script shows
this for account 300). A restore not done by `scripts/restore.sh` or the SQL of [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too)
holds nothing.

**Held settlements.** Under *Admin → Escrow status and payment intake → Monero escrow recovery and retention (local fakechain, Monero stagenet or network not verified yet)*, each restored
settlement with a stored transaction is listed with its order, kind, state and transaction hash. The restored records may
predate its relay, or a transaction that spent its coins, so check it first in the escrow daemon and the escrow wallet, for
example with the daemon's own RPC (a daemon started with a login needs `--digest -u USER:PASSWORD`):

```sh
db_sql -X -At -c "SELECT tx_hash, array_to_string(key_images, ' ') FROM xmr_settlements WHERE id = SETTLEMENT_ID"
curl -s http://ESCROW_DAEMON/get_transactions -d '{"txs_hashes":["TX_HASH"],"decode_as_json":false}'   # "missed_tx" names the hash when the daemon has no such transaction
curl -s http://ESCROW_DAEMON/is_key_image_spent -d '{"key_images":["KEY_IMAGE"]}'                      # spent_status: 0 unspent, 1 spent in a block, 2 spent in the pool
```

then tick that it was not already relayed and that nothing else spent the coins and confirm with your password (and
authenticator code when enrolled). The market then relays the same stored transaction once on its next pass, under the gate
too, and never builds it again; the confirmation is audited and noted on the order. Until then the settlement is followed but
never relayed, whether or not the gate has been cleared. The form exists only while `ESCROW_XMR` is on.

**A failed settlement: the administrator's re-request (A-401).** A settlement is *failed* when the escrow daemon refused its
stored transaction for good and no other transaction spent its coins; staff are told and the order page says so. Nothing
rebuilds it automatically. An administrator who is **not** a party to the order may ask on the order page, with their password
(and authenticator code when enrolled), for it to be built again; the request is audited. On its next pass the watcher
checks that the stored transaction is in neither the daemon's pool nor its chain and that none of its coins is spent. If one
is, the request is refused with the reason on the order page and in a notice to you, and may be asked again later. If none is,
the failed settlement becomes *superseded* and a new one is built from exactly the same coins to exactly the same destinations
(only the network fee, and so the claimant's output, may differ) and sent; the database refuses any other coins, so at most one
of the two transactions can ever be mined. The old transaction may still be the one that is: a refusal does not prove it never
reached the network (A-560, below), and a peer that kept it can send it back. If the daemon has it (pool or chain) before the
new settlement is built or relayed, or when the new one's coins turn out spent, the market records its own earlier transaction
as the outcome (A-566): the new settlement is closed (*superseded*, never built or relayed again), the old one is *relayed*
again with its deposits and followed to confirmed, and one order note, a notice to each party and one to you say what it paid
(by recipient) and that nothing is paid twice. Nothing is flagged and you have nothing to do; it is never recorded as spent
outside the market. A spend by any other transaction still is. If a deposit those coins came from is no longer listed by the escrow wallet (a
reorganisation or a double spend took it out of the chain), the new settlement stays requested (*deposit-missing*) and builds
nothing until the wallet lists that deposit again, however long that takes; it then builds and sends as above (A-614).
The deposit usually comes back at a new global output index, and the escrow wallet reuses the ring it stored when it built
these coins before, so that build is then refused *ring-stale*: staff are alerted and you follow the
[stale-ring break-glass](#stale-ring-break-glass-a-618) below, after which it builds and sends (A-640). Unlike
a first release or refund (A-554, below) it has no bounded wait, because it can be built from no other coins. Staff are
notified each time it starts waiting (once per episode); past 24 hours it is an escrow alert as any settlement still requested is, and its
monthly wallet is kept from destruction while it waits. Settling a waiting re-request by hand is **break-glass**: wait by
default, and never pay from other funds while the market can still send it (it builds and sends it once the deposit is
listed again, after the stale-ring break-glass if the wallet refuses that build). It waits only after a deep reorganisation or a double spend, or a wallet restore or rescan that lost the
deposit. A re-requested stray refund never holds back the stray refund of a later deposit to the same order (A-616,
migration 135): that one is requested, built from deposits no settlement claims and sent beside it, and the order page's
status says two stray refunds are in progress. While a re-request is still requested, the *Payment review* reason of each
deposit it claims keeps its usual text and adds which settlement re-requests which, and for a deposit no longer listed
that it waits for it, that the market builds and sends it once the deposit is listed again, and that if the escrow wallet
then refuses the build because its stored ring predates the reorganisation, staff are alerted and the operator follows the
stale-ring break-glass, ending "Do not pay anything by hand for it, or it is paid twice"; the desk's 24-hour line for a re-request waiting so says the same in short (A-617).

<a id="stale-ring-break-glass-a-618"></a>
**A settlement refused for a stale ring: break-glass (A-618).** monero-wallet-rpc 0.18.5.1 stores the ring (the decoys and
the real output, by global output index) of every transaction it builds in its ring database, also one built and never
sent (the market builds with `do_not_relay`), and reuses that ring whenever the same coins are spent again. A
reorganisation deeper than a deposit's confirmations that mines the deposit again gives its output another global index,
which the stored ring lacks. Every later build of those coins (a re-request after a refused relay, or any settlement built
once before) then fails, unless the wallet's random decoy picker happens to draw the new index, which on mainnet may take
days or never happen (not measured). monero-wallet-rpc has no call to clear a ring. Nothing is built or sent, so nothing can be paid
twice; the market tries again on every pass. The order page shows *wallet-refusing ring-stale*, then *wallet-refused
ring-stale* (flagged to staff), as "The escrow wallet keeps refusing to build this transaction (ring-stale) …"; this market's versions
before A-618 recorded the same refusal as `rpc:-4`. **Never pay it by hand**: once the ring is cleared the market sends
the settlement itself, and a payment by hand would pay twice.

1. *Diagnose, before any restart.* The wallet-rpc's log is on its tmpfs and a restart loses it, so copy it first and keep
   it private (it names wallet addresses and key images), then look for the stale ring's warning:

   ```sh
   umask 077
   docker compose exec -T monero-escrow-wallet sh -c 'cat /tmp/monero-wallet-rpc.log*' > "escrow-wallet-rpc-$(date -u +%Y%m%dT%H%M%SZ).log"
   grep -n -e 'Known ring does not include the spent output' -e 'real_out_found' escrow-wallet-rpc-*.log
   ```

   Then check that none of the coins is spent. For a re-request they are the key images of the settlement it supersedes;
   each must answer `spent_status` 0 (as under *Held settlements* above):

   ```sh
   db_sql -X -At -c "SELECT array_to_string(o.key_images, ' ') FROM xmr_settlements s JOIN xmr_settlements o ON o.id = s.supersedes WHERE s.id = SETTLEMENT_ID"
   curl -s http://ESCROW_DAEMON/is_key_image_spent -d '{"key_images":["KEY_IMAGE"]}'
   ```

   A spent coin is not a stale ring: stop and follow the failed-settlement paragraphs below instead.
2. *Remedy A (the rehearsed one): move the ring database aside.* Preconditions: the coins are unspent (step 1), and no other settlement is in flight (below). The cost: the ring
   database is shared by every wallet on that wallet-rpc (each month's escrow wallet and the vendor bond wallets), so the
   stored rings of all their coins are dropped too, and a coin of any of them that is later rebuilt after a reorganisation
   picks a new ring instead of reusing its old one. That is a privacy cost: a coin spent again with a different ring
   than a transaction that already reached any node lets whoever saw both compare the two rings and find the real coin.
   For the stuck coins themselves both remedies give a new ring (the old one no longer contains them); remedy A also
   drops it for every other coin. So use remedy A only when no other Monero settlement may be built again, that is when
   this answers 0, and otherwise use remedy B, which keeps every other coin's ring:

   ```sh
   db_sql -X -At -c "SELECT count(*) FROM xmr_settlements WHERE id <> SETTLEMENT_ID AND (state IN ('built','relayed') OR (state = 'failed' AND tx_hash IS NOT NULL) OR (state = 'requested' AND supersedes IS NOT NULL))"
   ```

   When it is not 0 you may also wait: the stuck settlement pays nothing twice while it waits, and built and relayed
   settlements confirm within the confirmation threshold (a failed one with a transaction, or a waiting re-request, does
   not clear by waiting). On a busy market it may never reach 0, so remedy B (rehearsed too, below) is then the path.
   Stop the wallet-rpc, then move its `.shared-ringdb` to a dated
   directory with the pinned image as uid 1000 and no network; never delete it, and never touch the wallet files (with
   another `COMPOSE_PROJECT_NAME`, the volume is `<COMPOSE_PROJECT_NAME>_monero_escrow_wallet`; use the
   `MONERO_WALLET_IMAGE` your `.env` pins):

   ```sh
   docker compose stop monero-escrow-wallet
   docker run --rm --network none --user 1000:1000 -v opsecmkt_monero_escrow_wallet:/home/monero/wallet --entrypoint mv \
     ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
     /home/monero/wallet/.shared-ringdb "/home/monero/wallet/.shared-ringdb.stale-$(date -u +%Y%m%dT%H%M%SZ)"
   docker compose start monero-escrow-wallet
   ```

   `mv` refusing with "No such file or directory" means the ring database is elsewhere: the wallet-rpc does not run with
   `--shared-ringdb-dir=/home/monero/wallet/.shared-ringdb`, as `compose.nodes.yaml` runs it. A native deploy
   (`opsecmkt deploy --escrow-xmr`, [binary.md](install/binary.md)) names the service `monero-wallet` and the volume
   `<project>_monero_wallet` (same path inside it): run the same steps with those names, stopping and starting that one
   service with `docker compose --project-name <project> --project-directory <deploy dir> --file <deploy dir>/compose.json`.
   On a separate payment host
   ([Enabling payments](install/enable-payments.md)) stop its wallet-rpc service and, as the user it runs as, move
   `WALLET_DIR/.shared-ringdb` aside the same way. While it is stopped nothing is built or sent. On its next pass the
   market opens the wallet and builds the settlement from the same coins to the same plan with a fresh ring, and sends
   it; the order page then shows it relayed. The fakechain leg `TestXMREscrowRegtestStaleRing` rehearses exactly these
   steps.
3. *Remedy B (targeted): unset the rings of these coins only.* Use the official `monero-wallet-cli` 0.18.5.1 (the
   release the wallet-rpc runs; the images ship no `monero-wallet-cli`). getmonero.org's current `hashes.txt` lists only
   the latest release, so check the archive against the release's signed `hashes.txt` kept in the monero-site
   repository, verified with binaryFate's key from the monero repository (fingerprint
   `81AC591F E9C4B65C 5806AFC3 F0AF4D46 2A0BDF92`); its line for the Linux x86-64 archive is the SHA-256 that
   `scripts/test-escrow-xmr-regtest.sh` pins:

   ```sh
   umask 022
   curl -sSfLO https://downloads.getmonero.org/cli/monero-linux-x64-v0.18.5.1.tar.bz2
   curl -sSfL -o hashes-v0.18.5.1.txt https://raw.githubusercontent.com/monero-project/monero-site/76f3846db3442be40676fd2e5432dd809d44ea3f/downloads/hashes.txt
   curl -sSfL -o binaryfate.asc https://raw.githubusercontent.com/monero-project/monero/v0.18.5.1/utils/gpg_keys/binaryfate.asc
   gpg --import binaryfate.asc && gpg --verify hashes-v0.18.5.1.txt   # Good signature from binaryFate, primary key 81AC 591F E9C4 B65C 5806 AFC3 F0AF 4D46 2A0B DF92
   grep -F ' monero-linux-x64-v0.18.5.1.tar.bz2' hashes-v0.18.5.1.txt   # 22a7dda7b0cb699fdd6b7674c3b4a4465b337cc98a54983523b759e1e7cc9958
   echo '22a7dda7b0cb699fdd6b7674c3b4a4465b337cc98a54983523b759e1e7cc9958  monero-linux-x64-v0.18.5.1.tar.bz2' | sha256sum -c
   tar -xjf monero-linux-x64-v0.18.5.1.tar.bz2 monero-x86_64-linux-gnu-v0.18.5.1/monero-wallet-cli
   cli=$PWD/monero-x86_64-linux-gnu-v0.18.5.1/monero-wallet-cli
   ```

   Find the order's wallet and the stuck key images (step 1's query: the key images of the settlement the re-request
   supersedes), then write the wallet's derived password to a file on tmpfs,
   never on a command line (it prompts for `MONERO_WALLET_SECRET` without echo; on a separate payment host, make the
   file on the app host and copy only the file). The file is mode 0640 inside a mode-0700 directory, so that the CLI,
   which runs as uid 1000 with your group added, can read it and no other user can reach it:

   ```sh
   db_sql -X -At -F ' ' -c "SELECT w.name, s.account_index FROM escrows e JOIN xmr_escrow_slots s ON s.address = e.address JOIN xmr_escrow_wallets w ON w.id = s.wallet_id WHERE e.order_id = 'ORDER_ID'"
   umask 027
   pwdir=$(mktemp -d /dev/shm/escrow-wallet-pass.XXXXXX)
   python3 -I - escrow-YYYYMM-NN > "$pwdir/pass" <<'PY'
   import getpass, hashlib, hmac, sys
   secret = getpass.getpass('MONERO_WALLET_SECRET: ').encode()
   sys.stdout.write(hmac.new(secret, b'opsecmkt xmr escrow wallet\0' + sys.argv[1].encode(), hashlib.sha256).hexdigest())
   PY
   ```

   Then open the order's wallet offline with the **same** `--shared-ringdb-dir` the wallet-rpc uses (no network flag on
   mainnet, as the wallet-rpc runs; add `--stagenet` on stagenet; never `--regtest`, which the CLI refuses). Run the CLI
   as the user the wallet-rpc runs as (uid 1000 on Compose and the native deploy), never as root: the CLI rewrites the
   wallet files on exit and opens the ring database's lock file, and a root-owned file there breaks the wallet-rpc, which
   then runs with no ring database at all. The official CLI is a glibc build and does not run in the wallet-rpc image
   (Alpine); on Compose run it in the Debian image the market's Dockerfile pins, with no network, the wallet volume at
   `/home/monero/wallet`, and the CLI and the password file mounted read-only (on an ARM host add
   `--platform linux/amd64`, which needs amd64 emulation). Each step below is one session with its commands piped in:

   ```sh
   docker compose stop monero-escrow-wallet
   wallet=escrow-YYYYMM-NN
   kis='KEY_IMAGE …'   # step 1's key images, separated by spaces
   wallet_cli() {
     docker run --rm -i --network none --user 1000:1000 --group-add "$(id -g)" \
       -v opsecmkt_monero_escrow_wallet:/home/monero/wallet -v "$cli:/usr/local/bin/monero-wallet-cli:ro" \
       -v "$pwdir/pass:/run/escrow-wallet-pass:ro" --entrypoint /usr/local/bin/monero-wallet-cli \
       debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587 \
       --offline --wallet-file "/home/monero/wallet/$wallet" --password-file /run/escrow-wallet-pass \
       --shared-ringdb-dir /home/monero/wallet/.shared-ringdb --log-file /dev/null
   }
   printf 'N\n' | wallet_cli
   { printf 'set ask-password 0\n'; cat "$pwdir/pass"; printf '\nexit\n'; } | wallet_cli
   { printf 'print_ring %s\n' $kis; echo exit; } | wallet_cli
   printf 'unset_ring %s\nexit\n' "$kis" | wallet_cli
   { printf 'print_ring %s\n' $kis; echo exit; } | wallet_cli
   { printf 'set ask-password 2\n'; cat "$pwdir/pass"; printf '\nexit\n'; } | wallet_cli
   printf 'set\n' | wallet_cli | grep -a 'ask-password ='
   shred -u "$pwdir/pass" && rmdir "$pwdir"
   docker compose start monero-escrow-wallet
   ```

   (With another `COMPOSE_PROJECT_NAME` or a native deploy, use the service and volume names of remedy A. On a separate
   payment host run the same `monero-wallet-cli` arguments directly, as the wallet-rpc's user, with `WALLET_DIR` in
   place of `/home/monero/wallet`.) What each session shows, and why:

   - Each says `Opened wallet: …`, that it is in offline mode, and the prompt `[wallet … (no daemon)]:`.
   - `N`: the first time the CLI opens a wallet it asks "Do you want to do it now? (Y/Yes/N/No)" about background
     mining, and that question reads its answer from stdin and loses the rest of piped input; answered once, the wallet
     file keeps the answer ("Background mining not enabled"). On a later open the `N` is only an unknown command.
   - `set ask-password 0`: the CLI asks for the wallet password, and gets it from the file on its stdin, never on a
     command line. This step is required: with the default `ask-password = 2 (decrypt)` the CLI keeps the spend key
     encrypted in memory and derives its ring database key from it, which is not the key the wallet-rpc uses, so it sees
     no stored ring at all and `unset_ring` silently removes nothing. The cost: while the CLI runs, offline, the spend key
     is unencrypted in its memory, as it always is in the running wallet-rpc's.
   - The first `print_ring` must print each stored ring, `KEY_IMAGE absolute` and 16 global output indices. If it
     still answers "Key image either not spent, or spent with ring size 1", stop: run the `ask-password 2` and `set`
     sessions, start the wallet-rpc, and use remedy A or wait. The same answer also comes from a wrong
     `--shared-ringdb-dir`, where the CLI opens an empty ring database and `unset_ring` "succeeds" without changing
     anything.
   - The second `print_ring` must answer "Error: Key image either not spent, or spent with ring size 1" for each.
   - `set ask-password 2` restores the default, and `set` must then show `ask-password = 2 (decrypt)`, so the keys file
     ends with the setting it began with.

   On its next pass the market builds the settlement from the same coins to the same plan with a fresh ring for the
   stuck coins and sends it once. Remedy B keeps every other coin's ring: the fakechain leg
   `TestXMREscrowRegtestStaleRingRemedyB` rehearses exactly these steps, with another order's release in flight (so
   remedy A's query answers 1) whose ring `print_ring` shows unchanged after the `unset_ring`.

A restore from seed does **not** clear a stale ring: the ring database is not part of the wallet files, and a wallet
restored into the same wallet-rpc reuses it. Restoring the wallet volume from a copy restores its `.shared-ringdb` too.

**Failed and set-aside settlements are escrow alerts (A-560).** Until it is superseded, a failed settlement is listed under
*Escrow alerts* on the moderation desk, counted in the staff badge and named in the order page's staff help, and a
payment-review row of a deposit it spends says that it failed. The entry says which case it is. One with a stored
transaction (the daemon refused it, or its stored metadata cannot be read) can be re-requested as above (*Re-request
settlement N* on the order page); the alert clears when the re-request supersedes it. Such a failure does not prove that
nothing reached the network (the wallet-rpc's refusal can follow a lost connection after the daemon took the
transaction, and a relayed settlement can fail on unreadable metadata), and the order page says so too: it says the
transaction was built and stored and has not been seen mined, and that it may still be mined (A-566). Do not pay
such a settlement by hand while its transaction may still be mined or re-requested: settle by hand only after a re-request
was refused for good, as below. The entry offers the re-request only when
the market would record one: while one waits for the watcher's next pass (or for a restore to finish) it says so; after
a refusal it names the reason (the transaction in the pool or the chain, or a coin already spent) and asks you to check
the escrow wallet and the chain instead; with `ESCROW_XMR` off it says the re-request is available once it is back
on. A re-request refused *in-pool* is not final (the transaction may still be mined or dropped): re-request it again
later. One refused *on-chain* (the transaction is in the chain) or *key-image-spent* (a coin it spends is spent) is
refused every time, so the settlement is never superseded (A-613): check the escrow wallet and the chain for what was
actually sent, settle anything still owed with the operator, and then an administrator who is not a party marks it
settled by hand on the order page (*Mark settlement N settled by hand*), with the record described below. After that it
is never re-requested again. Marking is refused (409) before any re-request (re-request it first), while one waits and
after an *in-pool* refusal. One that failed before any
transaction was built cannot be re-requested, and the market never builds that settlement again; a failed release or
refund also stands in for the order's one release or refund, which is all the database allows (A-613):

- A refund whose coins were too few (*too-small*) is not an escrow alert, as a Bitcoin refund too few is not: its
  deposits are left out and flagged under *Payment review* (staff read the order while a flag is open), and a stray
  refund returns them, together with any later deposit, once together they can pay the network fee. A stray refund
  that keeps waiting is an alert once it has been requested for 24 hours.
- Any other (*nothing-to-settle*) stays an alert. Nothing was sent because no deposit of the escrow counted when the
  market came to build it (none was left to spend, or the deposit it was for was missing, for example read as
  double-spent). Nothing is owed for it now, and **you must not pay anything by hand for it**: if a deposit of that
  escrow is listed again, the market handles it automatically as it does any deposit (on an order whose coins go back
  to the buyer, a stray refund returns it), and the deposit is flagged under *Payment review* or raises its own escrow
  alert. A payment by hand would pay twice. Read why under the settlement on the order page; then an administrator who
  is not a party dismisses it there (*Dismiss settlement N (nothing owed now)*, under the settlement). Dismissing it
  changes nothing the market does with later deposits; it only closes this alert.
- The exception: a failed settlement with no transaction built that still **claims deposits** (a re-request that failed
  *nothing-to-settle* after taking over the deposits of the settlement it superseded, for example when a claimed
  deposit was read as double-spent). The market no longer leaves one: since A-614 such a re-request waits for the
  deposit (*deposit-missing*, above) instead of failing. A database whose market ran an earlier build may still hold
  one. Its alert and the deposit's *Payment review* row say so: the coins are in the escrow wallet and may still be
  owed, and the market does not settle them on its own, not even when that deposit is listed again (a failed
  settlement is never built again). It cannot be dismissed (the order page offers no form and the action refuses it);
  settle with the operator, outside the market. Nothing in the market clears its alert, so the order stays on the desk
  and under retention's `escrow_alert` reason; UPGRADING.md lists such settlements.

Either record (dismissed as nothing owed, or marked settled by hand) takes a note of 10–500 characters (why nothing is
owed now; or what the wallet and the chain showed and what was done), confirmed with the password (and the
authenticator code when enrolled), with an audit row. It clears the alert: the desk entry, the badge count, the staff
help and retention's `escrow_alert` reason. The note is for staff only (shown under the settlement to staff who can
read the order); the parties get no order note or notice. It can be recorded once per settlement (a second record is
refused); it works with `ESCROW_XMR` off, as recording a refused deposit's disposition does (the order page then offers
only these actions, and its staff help says so), and the market sends nothing for the settlement either way.
Moderators cannot record it.

A set-aside settlement (below) is an alert until an administrator asks for it to be built again.

**A set-aside settlement.** A settlement whose build makes the escrow wallet-rpc stop responding twice (monero-wallet-rpc
0.18.5.1 crashes on some destinations) is set aside and flagged, and the watcher skips it so that others are not held up. Find
out why the wallet-rpc stops (its container logs), then an administrator who is not a party asks on the order page, with a
password, for it to be built again. If the wallet-rpc stops twice more, it is set aside again.

**Destruction, and deleting the files.** A wallet whose orders have all closed is retained; 90 days after its last order
closed, and only when no order of it is open, no settlement of it is live or still followed to 100 confirmations, its balance
and unlocked balance are zero across all accounts and nothing of it is in the pool, the watcher records that it began
destroying the wallet, replaces its password with 32 random bytes that are never kept, closes it, wipes its sealed seed from
the database and records it destroyed (log: `escrow: Monero escrow wallet … destroyed`). Automatic destruction happens only
when those preconditions hold. If the watcher stops after the new password took effect (a crash, or a restore from a backup
taken in between), the wallet-rpc refuses that wallet with "Invalid password" and it is recorded destroyed on a later pass,
unless staff were flagged that it holds something or the restore gate or the sweep marker is set. A database restored from
a backup taken after the market began destroying a wallet keeps that start: restore such a wallet from its seed only with
the derived password from the unchanged `MONERO_WALLET_SECRET` (the `openssl` line above), because with a mistyped one it is
refused alike and recorded destroyed once the gate and the sweep marker clear, and check in a scratch wallet restored from
the seed (step 3 above) that it holds nothing before you delete the files `/admin/payments` then lists. A retained wallet past its 90
days that still holds coins (an unclaimed paid account, coins at account 0, a failed settlement) is not destroyed, and staff
are flagged once. Any other wallet that will not open keeps its sealed seed and is recorded missing: its file gone, or "Invalid
password" when the market did not begin destroying it (for example a wallet you restored from its seed with a mistyped
`MONERO_WALLET_SECRET`, another install's file of the same name, or a damaged keys file). A missing wallet past its 90 days is
listed on `/admin/payments`, where an administrator confirms its destruction (password step-up, audited) once you have checked that its
file is gone and nothing is owed on it; that is refused while the restore gate or the sweep marker is set. **Do not keep an
escrow wallet open in another wallet program** (for example `monero-wallet-cli` on the same files): a wallet "opened by another
wallet program" keeps its state and its accounts, but the market cannot read its deposits or send its settlements until you
close it there (logged once). **The market cannot reach the wallet volume, so the files stay until you delete them.**
*Admin → Escrow status and payment intake → Monero escrow recovery and retention (local fakechain, Monero stagenet or network not verified yet)* lists *Destroyed Monero escrow wallets* with
their two files, `NAME` and `NAME.keys`. Delete each pair from the escrow wallet volume:

```sh
docker exec opsecmkt-escrow-wallet sh -c 'rm -v -- /home/monero/wallet/escrow-YYYYMM-NN /home/monero/wallet/escrow-YYYYMM-NN.keys'
```

and delete the copies in your backups when they expire. A deposit sent to a destroyed wallet's address cannot be recovered by
the market (the order page says so); a destroyed wallet is never opened again.

**What staff see.** Each of these shows staff what needs attention.

- *A refused address at checkout, claim or ship* tells the user at once, with the reason and nothing changed: an integrated
  address, another network's, keys that are not valid, or an address this market already knows (an escrow, slot or wallet
  primary address, the fee address, the buyer's refund address for that order, or an external address already used by
  the recipient account). Another account's external address history is not exposed by these refusals.
- *The wallet refuses a settlement's address when it builds* (`destination-refused`): the order is flagged for moderator review
  with a note naming whose address it was, the deposits are flagged and the settlement waits. A refused vendor claim or buyer
  refund address is never tried again for that settlement and this build has no tool to change it, so contact the user; the
  coins stay in the escrow account. A refused fee address is replaced by `ESCROW_XMR_FEE_ADDRESS` once you correct it and restart
  and the wallet accepts the new one; the order notes that the fee went to the address configured since.
- *A wallet goes missing* (its file is gone from the volume, or it no longer opens with the derived password): every
  administrator is notified ("Monero escrow wallet … cannot be opened on the escrow wallet-rpc …"), its free accounts are
  retired and it takes no order, and its sealed seed is kept. The market tries it again on every pass; once it opens it is
  closing again, or retained when the market had begun destroying it, to be destroyed once every precondition holds again.
  Restore its files from a backup of the volume, or restore it from its sealed seed as above, then set the sweep
  marker again (*Restore*, above) so that the sweep raises its lookahead. Past its 90 days it is listed on `/admin/payments` for an
  administrator to confirm its destruction, as under *Destruction* above. A file the market did not create that is
  found under a wallet's name, and does not open with the derived password, is recorded as an orphan, never opened, and
  announced the same way.
- *Other flags*, each noted on the order or sent to the administrators once: a retained wallet past its 90 days that still holds
  coins (it is not destroyed), a deposit with an unlock time (never counted), a
  deposit the database refused as another order's account (recorded once per deposit; staff record what was done about
  it on the order page, which clears its escrow alert, A-480), coins at an account no order claimed or at account 0 (which the
  market never spends), a settlement whose build failed a check (the fee above `ESCROW_XMR_MAX_FEE`, a claimant output below
  0.001 XMR, a transaction that does not match its plan, an address mismatch) and a settlement spent outside the market.
  A deposit credited toward an order's funding that a reorganisation takes below the confirmation threshold again, or
  that the wallet no longer lists, is flagged once on the payment-review desk and holds the order's release or refund
  (A-554): it waits while the deposit confirms again, and for one that is missing twice the threshold in blocks, then
  is built without it, with an order note naming it. Should the wallet list that deposit again after a release, it
  stays in the escrow account (no stray refund returns it): decide by hand what happens to it and record the review on
  the desk. After a refund it is the buyer's: it is left out, noted as credited and listed again, and the stray refund
  returns it to the buyer's refund address (PAY66-6). A release or refund whose credited deposits are all gone for good
  stays requested (*deposit-missing*) and keeps its monthly wallet, with its sealed seed, from destruction past the 90
  days; there is no web action for it. Resolving it by hand is **break-glass**: wait by default, and never pay from
  other funds while the market can still send it; the wait needs a deep reorganisation or a double spend, or a wallet
  restore or rescan that lost the deposit. If the deposit is listed again the market builds and sends it; if the escrow
  wallet then refuses that build because a ring it stored for those coins predates the reorganisation, staff are alerted
  and you follow the [stale-ring break-glass](#stale-ring-break-glass-a-618) (A-618). You see it on the order page's wait line, as an
  open "Credited deposit conflicted or missing" flag on the payment-review desk, in the desk's alert for a settlement
  requested for more than 24 hours, and, past the 90 days, in the notice that a retained wallet is kept (PAY66-7). The market
  notices such a regression only once the order's release or refund is being built, not while the order is paid or
  shipped; and after you record *no action* on the flag, a later regression of the same deposit sends a new note and
  notice but does not reopen the flag on the desk. A re-request (A-401) one of whose claimed deposits is no longer
  listed waits the same way (*deposit-missing*), with no bounded wait, since it spends only the coins of the
  transaction it replaces: staff get one order note and notice each time it starts waiting (A-614), and it builds
  once the wallet lists that deposit again, after the [stale-ring break-glass](#stale-ring-break-glass-a-618) if the
  wallet refuses that build *ring-stale* (A-640).
  A refund whose claimant output would be below 0.001 XMR (on a cancelled order or one decided for the buyer, with no
  deposit it would spend still confirming) does not wait: it ends (failed, too-small) and its deposits are left out for
  the stray refund, which returns them with any later deposit once together they pay the fee; the parties and staff
  are told once (A-437). A refund already waiting so when you upgrade ends on the next pass. So does a refund whose
  coins are at or below the network fee itself, which the wallet refuses to build (-17 "not enough money", -16 "tx not
  possible" or -46): on those codes the market reads the fee the wallet would take (the wallet-rpc's
  `get_default_fee_priority` and `estimate_tx_size_and_weight`, the escrow daemon's `get_fee_estimate`; none builds or
  sends anything) and, when the refund less that fee is below 0.001 XMR, treats it as too few, never as an outputs
  mismatch or a refusing wallet (A-467). A refund that could pay the fee keeps the wallet's classing, and is flagged as
  before when the refusal persists. A refund already flagged "outputs mismatch" or "keeps refusing" for this reason when
  you upgrade ends too few on the next pass (its earlier order note stays). If the fee estimate fails (for example a
  daemon RPC that does not serve `get_fee_estimate`), that refund waits, staff are told once if it fails again on the
  next pass, and the market tries again on each pass; the wallet's other orders carry on (QA5W1-4). An estimate that
  fails only on alternate passes, with the wallet refusing on the others, is flagged on the second pass all the same,
  and staff are told once of each distinct problem in that run (the unavailable estimate, then, say, coins the wallet
  no longer sees, or a refusal), however often it flips between them, a restart included (QA5W2-1, QA5W3B).

**Switching it off with orders open.** Setting `ESCROW_XMR` back to `off` moves no coins and closes no order, but it stops the
Monero escrow watcher: deposits are not read, nothing is built or sent, and each order page says so. Release deadlines are
applied again once it is on. The coins stay in their accounts.

**Compose.** `compose.yaml` passes these variables from `.env` to the app container, blank in `.env.example` so that Monero
escrow stays off; it adds no escrow wallet-rpc, no fakechain daemon and no mount for the seed key. Passing the variables was
checked with `docker compose config` only.

**What was checked.** [Per-order escrow, Phase 3](escrow.md#phase-3-monero-escrow-on-a-local-fakechain) lists each tier: unit and `httptest` tests, Go
tests with PostgreSQL against a fake wallet-rpc, nineteen legs against a real fakechain `monerod` and `monero-wallet-rpc`
0.18.5.1 (`scripts/test-escrow-xmr-regtest.sh`, run by the `escrow-xmr-regtest` CI job;
[runbook](testnet-runbook.md#9-manual-monero-escrow-fakechain-check)), and a simulated-RPC browser journey added in this
phase. Nothing has run on stagenet, testnet or mainnet, with Monero multisig, or as a deployment.

#### Vendor bond wallet

A-547. With a vendor bond set ([Vendor applications](#vendor-applications)), the Monero escrow watcher creates one
long-lived **bond wallet** on the escrow wallet-rpc when the first applicant needs an address: `bond-01` (a later
`bond-NN` when a file of that name already exists, or after a restore closed the one in use), with the password derived from `MONERO_WALLET_SECRET` as an
escrow wallet's (the HMAC of the name, as in the offline seed check above) and its seed sealed to `ESCROW_XMR_SEED_KEY`
(`xmr_bond_wallets.seed_sealed`, with `primary_address` and `restore_height`). Each application gets its own account
of it, never reused; deposits count from the escrow confirmation threshold. It lives in the escrow wallet volume, so
the volume backups above cover it, and the database dump holds its sealed seed.

**The market never sends from the bond wallet.** There is no refund, forward or transfer of any kind, and the market fee
address is never involved: bonds accumulate there until you move them. To collect them, sweep it **offline from its
sealed seed**, never through the escrow wallet-rpc:

1. List the bond wallets that may hold bonds (in use `sealed`, `closed` by a restore, or an `orphan` whose seed was
   sealed), then copy one wallet's sealed seed out of the database; repeat the steps below for each:

   ```sh
   db_sql -X -At -F ' ' -c "SELECT name, state, restore_height, primary_address FROM xmr_bond_wallets WHERE seed_sealed IS NOT NULL ORDER BY id"
   db_sql -X -At -c "SELECT seed_sealed FROM xmr_bond_wallets WHERE name = 'bond-NN'" > bond-seed.asc
   ```

2. On the offline machine with the private seed key, `gpg --decrypt bond-seed.asc` prints the 25 words; delete
   `bond-seed.asc` and the terminal history afterwards.
3. Restore the words from that restore height in a wallet of your own (a wallet you run yourself, not the escrow
   wallet-rpc), check that its primary address is the one from step 1, let it sync, and send the bonds where you
   choose. The market notices nothing: deposits already counted stay counted, and the bond addresses keep working.

While new XMR payments are paused, or under the restore gate or the sweep, no bond wallet or account is created and no
address shown; an applicant who already has an address is told that payments to it still count. Deposits are always
read; only the restore gate and the sweep hold recording: no bond is recorded and nothing is approved until both clear.

**Restore.** A restored database may lack bond addresses the bond wallet gave out after the backup, and a restored wallet
file would give those account numbers again. So `scripts/restore.sh` (and the manual SQL of
[UPGRADING.md section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too)) moves the bond wallet in use to `closed`:
it is still read every pass, and payments to the addresses its records hold still credit their applications (an
applicant keeps the address it was shown), but it never gives an address again; new addresses come from a fresh
`bond-NN`. The sweep that `xmr_escrow_sweep_required` starts also probes `bond-NN` names after the newest recorded one (a
bond wallet created after the backup is recorded as an orphan, its seed sealed and its funded accounts listed to the
administrators, and read from then on like a closed wallet) and raises each bond wallet's lookahead past its highest
recorded account. An account the records lack (given out after the backup, or a lost `create_account` answer) is
recorded for no application and reported, as is a later deposit to it or to any orphan account; coins there fund
nothing, so record such a bond by hand. A bond wallet that does not open is reported and never replaced (it may hold
bonds), and while the one in use does not open no address is promised: restore it from its sealed seed with its name
and derived password, as for an escrow wallet, and set the sweep marker again.

#### Escrow checkouts and readiness

Admin → **New escrow checkouts** sets XMR intake to **Escrow** or **Paused**. Pausing prevents new checkout; order history stays readable. Enabling intake does not configure escrow: `ESCROW_XMR=on`, the daemon, dedicated wallet-rpc, seed key and wallet secret must pass readiness checks.

The readiness panel lists active listings that cannot use escrow. Digital listings need automatic delivery content; physical listings need a dispatch window and a vendor encryption key. No escrow signing-key registration is required. Unavailable listings remain visible with a reason; blocked checkouts are refused server-side.

While escrow is configured but not ready, Admin shows the failed check and its remedy. Checkout waits for readiness; it never falls back to another currency or pooled wallet.

### Transparency

`AUDIT_SIGNING_KEY` (64 hex characters, an Ed25519 seed generated by the installer with `openssl rand -hex 32`) enables signed audit exports. It is read only from the environment, never stored in the database; blank or malformed values leave the export visibly unavailable. Changing it changes the public key, so treat it like any long-lived signing key: back it up with `.env` and announce rotations.

**Exports are private operator evidence.** An export names every account's handle and its activity: sign-ins, account changes, order actions, key fingerprints, short order IDs and transaction IDs. Store it like a database backup and never publish it. Visitors do not receive exports; `/canary` publishes only the public key, so that you can prove an export you hold came from this server. `/canary#records` tells users what the server keeps and whether retention maintenance is enabled.

**Pin the audit public key.** The admin page and `/canary` show the hex Ed25519 public key. Record it once through a channel you trust (for example in the operator-signed canary text) and verify every export you hold against that pinned value, not against the key printed in the `.sig` file:

```sh
go run ./cmd/verify-audit -pub <pinned-hex-key> audit-events-upto-N.jsonl audit-events-upto-N.sig
```

The server prepares and sends one export at a time, retaining its slot until the download completes or fails. Another export receives 503 with `Retry-After: 5`; wait for the active download to finish before retrying. This bounds concurrent export buffers. The unencrypted export for a given range is byte-for-byte reproducible while its retained rows and account handles are unchanged. Retention expiry and account erasure can change a later export; verify the original issued pair independently. Web exports accept an exclusive `from` and inclusive `upto`; `from=0` preserves the original whole-history export bytes and signature. When the audit log has not changed between requests, adjacent ranges can be concatenated in ID order to reproduce the bounded whole export, but verify each range against its own signature before concatenating. A download cut short leaves a partial file that fails verification with "export sha256 does not match the signature file", the same failure as an altered file: download the same `upto` again (each attempt counts against the 10 downloads per 10 minutes). The export route has its own time limits instead of the server's 12 s request bound and 20 s write timeout: up to one minute to read the events from the database, then 20 seconds plus one second per 32 KB to send the file (a 50 MB export gets about 27 minutes; onion throughput measured 0.19–0.45 MB/s). Every other route keeps the server's limits. **A server compromise can produce validly signed exports**: whoever controls the server or `AUDIT_SIGNING_KEY` can sign an altered history. The signature proves origin from the configured key, not that the log is complete or untampered.


**Host-only ranged export.** For an export from event ID 800 exclusive through 900 inclusive, run the command on the server host from the release checkout. The directory must already exist and should be restricted to the operator (for example `install -d -m 700 /secure/audit`). The JSONL, signature and retention sidecar files are created with mode 0600 and refuse to overwrite existing files. Keep all three together; the sidecar preserves the earliest included event's original 180-day expiry. Empty ranges create no files. Standard output reports the saved paths and reminds you to keep both files private; keep both files private. This command makes a read-only database query and reads the signing key, but starts no HTTP server or wallet:

```sh
set -a
. ./.env
set +a
go run ./cmd/server -export-audit 900 -export-audit-from 800 -export-audit-dir /secure/audit
```

`-export-audit-from` is an exclusive lower bound and `-export-audit N` is the inclusive upper bound; omit `-export-audit-from` to export from the beginning. Keep `DATABASE_URL` and `AUDIT_SIGNING_KEY` in the environment and never put their values in shell arguments or logs. The `-export-audit` command validates its arguments before connecting and writes the range as `800 < event ID <= 900`. Verify the pair with the pinned public key as above.

**Warrant canary.** In the admin page, paste the operator's OpenPGP public key, then paste a statement the operator wrote and signed offline (`gpg --clearsign canary.txt`). The application stores it as pasted only if it verifies, re-verifies it on every view of `/canary`, and shows INVALID with the reason if the key changes or the stored text is altered. Keep the operator's private key off the server; the application never writes or signs canary text.

A genuine canary also shows INVALID once it is no longer current, and the reason says which case applies and what to do:

- *the configured key expired on …*: the operator key's expiry date has passed (the admin page shows the key as **Expired**, not Configured). Extend it offline (`gpg --quick-set-expire <fingerprint> 1y`) and save the updated public key; a canary it signed before then verifies again. Or save a new key and publish a canary signed with it.
- *the signature expired on …*: the canary was signed with its own expiry (`gpg --default-sig-expire` or `--ask-sig-expire`). Sign a new statement.
- *it is dated …, after this server's clock*: the signing computer's clock is ahead of the server's. There is no tolerance for this, so a canary can never claim a time that has not yet come on the server. Check both clocks, then sign again, or submit the same text again after the stated time.
- *the key … is revoked*: configure a new key and sign a new statement with it.

"Signature does not verify with the configured key" is kept for a signature that does not check out against the key at all: the text was altered, or a different key signed it.

#### Encrypted audit exports

`AUDIT_EXPORT_RECIPIENT` is optional: set it to the path, inside the application
container, of one armored OpenPGP public key with a currently usable encryption
subkey. Export that public key from your offline operator environment; transfer
only the public file. Keep its private key apart from the server and from the
exports. The key file must be at most 64 KiB; private, revoked, expired,
signing-only and malformed keys refuse an export without falling back to
plaintext. The host command checks the recipient before connecting to the
database.

For Compose, create a separate override, for example `compose.audit-export.yaml`:

```yaml
services:
  app:
    volumes:
      - type: bind
        source: /srv/opsecmkt-public-keys/audit-export-recipient.asc
        target: /run/audit-export-recipient.asc
        read_only: true
        bind:
          create_host_path: false
```

Make the public file readable by the app's UID 65532. Set
`AUDIT_EXPORT_RECIPIENT='/run/audit-export-recipient.asc'` in `.env` and append
`:compose.audit-export.yaml` to its existing `COMPOSE_FILE` value, preserving all
other overlays and profiles. Recreate the application, then check the export
availability on `/admin/transparency`. A missing file or unusable key makes exports
unavailable; it does not stop the rest of the market.

Both the HTTP download and `-export-audit` produce `.jsonl.asc` and `.sig.asc`
files. The server signs the original JSONL bytes, then encrypts each output in
memory before writing it. No plaintext host export file is created in this mode.
Ciphertext differs on repeated downloads because encryption uses fresh
randomness; decrypted JSONL and signature bytes retain their original format.

Decrypt the pair on the offline operator workstation, in a private directory:

```sh
umask 077
gpg --output audit-events-upto-900.jsonl --decrypt audit-events-upto-900.jsonl.asc
gpg --output audit-events-upto-900.sig --decrypt audit-events-upto-900.sig.asc
go run ./cmd/verify-audit -pub YOUR_PINNED_ED25519_PUBLIC_KEY \
  audit-events-upto-900.jsonl audit-events-upto-900.sig
```

For the host-only command, `AUDIT_EXPORT_RECIPIENT` must name a file readable on
that host, rather than the container's mount path. Preserve the original signing
public-key pin when verifying. This setting protects exported files only:
database audit rows and the online signing key remain accessible to a host
administrator. Public-recipient replacement affects future exports; keep old
private keys until their retained exports no longer need recovery.

## Backups and recovery

The [data retention and private information policy](data-retention-privacy-policy.md) requires a 30-day maximum for
ordinary recovery copies, including off-host backups, snapshots, WAL archives and upgrade copies. A registered
preservation exception or dedicated escrow recovery material follows the policy's separate safeguards.
[Enable retention maintenance](retention.md) for database expiry and restore replay, and schedule the backup/export expiry helper daily at every copy location.
The backup script writes an original-deadline sidecar; restore requires the current external retention state before the server can reopen.

Install [age](https://age-encryption.org/) and Python 3, then generate an identity offline (`age-keygen -o backup-key.txt`). Store the private key separately from backups and use its public recipient for encryption:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/market.dump.age
```

For external databases also set `BACKUP_DATABASE_URL` and install PostgreSQL client tools matching or newer than the server. `./scripts/install.sh upgrade` backs up through this script too, so on an external database give it both: `BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade` (the value of `DATABASE_URL` in `.env` serves when this host can reach that server; quote it in single quotes); without `BACKUP_DATABASE_URL` it refuses before changing anything. Before it creates a temporary archive or streams dump data, the script checks free space on the output filesystem against 125% of the source database size plus a fixed reserve. The default reserve is 256 MiB; set `BACKUP_SPACE_HEADROOM_BYTES` to a non-negative byte count to change it, or explicitly set it to `0`. The estimate is deliberately conservative, but concurrent filesystem writers can still consume space after the check. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Its success line reports the `pg_dump` client version actually selected for that backup. Restore reads the producing version from the archive and refuses a selected `pg_restore` older than that version before confirmation or database writes. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage ([commands below](#secrets-env-and-onion-identities)). Database dumps do not include onion keys or blockchain volumes, and in particular **not the custodial payment wallets** (the `monero_escrow_wallet` volume, or your external wallets): back those up on their own (the escrow wallets: see the [runbook](testnet-runbook.md#back-up-and-restore-monero-escrow-wallets)). Maintain offline copies and rehearse recovery.

Coordinate payment-wallet backups separately. The [wallet backup procedure](testnet-runbook.md#back-up-and-restore-monero-escrow-wallets)
requires stopping both the application and its dedicated wallet RPC before
copying wallet files. An independent timer that copies active files or stops
only the wallet can interrupt settlement work and does not establish a
consistent recovery point. An initial backup made while the wallet directory
is empty protects deployment keys and configuration; it does not protect wallets
created later. Keep the large, rebuildable blockchain outside recovery archives.
Verify encrypted copies off the host, and retain the matching application
secret, database, wallet files and current external retention state.

Run database backups on a schedule that matches your recovery-point needs. This database-only cron example runs daily at 02:15 in the cron daemon's local time and creates a dated archive whose filename uses UTC (replace the recipient and checkout path):

```cron
15 2 * * * cd /srv/opsecmkt && AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh "backups/market-$(date -u +\%Y\%m\%dT\%H\%M\%SZ).dump.age" >> backups/backup.log 2>&1
```

For an external database, keep `BACKUP_DATABASE_URL` in a root-owned mode-0600 environment file or wrapper and have the cron job invoke that; do not put its credential in the crontab.
Copy the completed `.dump.age` archive and its `.dump.age.retention.json` sidecar together, unchanged, to each approved encrypted-storage location.
Periodically verify an off-host copy by restoring it to a disposable database. Run an inventory dry run before scheduling expiry:

```sh
python3 scripts/backup-retention.py expire /srv/backups /srv/offsite-copies \
  --holds /srv/opsecmkt-retention/backup-preservations.json
```

After verifying the inventory and current preservation register, schedule the same command daily with `--delete`.
The helper uses original creation timestamps and digests, rather than filesystem modification age.
See [copy expiry and legacy enrollment](retention.md#expiring-backups-and-signed-exports) for held copies, signed audit pairs and provider storage.

Rehearse recovery on another host, or in a second checkout with its **own Compose project name**. Compose names containers, volumes and networks after the project (`opsecmkt` unless `.env` sets `COMPOSE_PROJECT_NAME`), not the directory: a second checkout under the same name recreates the live containers with its own secrets, and its `docker compose down --volumes` deletes the live volumes. Install the rehearsal copy with, for example, `COMPOSE_PROJECT_NAME=opsecmkt-rehearsal APP_PORT=8081 ./scripts/install.sh` (add `--local` for a quick local copy; a Tor rehearsal with a local node on the same host as a live install that has [`compose.nodes-tor.yaml`](#local-nodes-over-tor) needs `--nodes-tor no`, for the subnet reason below); the installer saves the name in that checkout's `.env`, so its later `docker compose`, `backup.sh` and `restore.sh` commands stay on the rehearsal project, and it refuses to install when containers of the chosen project were created from another directory, or when any of that project's volumes exist. If you copy a `.env` into the rehearsal checkout instead, change `COMPOSE_PROJECT_NAME` (add the line if it is missing) and `APP_PORT` in the copy before running any `docker compose` command there. On the same host, also drop `compose.nodes-tor.yaml` from the copy's `COMPOSE_FILE` (or the `monero` profile from its `COMPOSE_PROFILES`): its pinned node network `10.199.42.0/28` already exists for the live project, and Docker refuses a second one as overlapping ([Local nodes over Tor](#local-nodes-over-tor); a different subnet means editing `compose.nodes-tor.yaml` and `deploy/torrc-socks` together). Never change `COMPOSE_PROJECT_NAME` on an existing install: it would start on new, empty volumes (`<project>_postgres_data` and the others).

Without `BACKUP_DATABASE_URL`, the script dumps the internal database through `docker compose exec -T db`, so it needs no published database port or host PostgreSQL tools. It dumps the database named in `DATABASE_URL` in `.env` (the name after the port), so after a side-by-side restore or rollback that switches `DATABASE_URL`, backups follow the database the application uses. The success line names the database dumped (never the URL or password). If the script cannot read a plain lower-case name there (for example the name comes from Compose variable interpolation), set `BACKUP_INTERNAL_DATABASE` to it; a `BACKUP_INTERNAL_DATABASE` that differs from the name in `DATABASE_URL` is refused, and so is setting it together with `BACKUP_DATABASE_URL`.

**External databases:** `BACKUP_DATABASE_URL` is used exactly as given; the script does not look at `DATABASE_URL`. When you switch `DATABASE_URL` to a restored database, point `BACKUP_DATABASE_URL` (in your backup job or cron entry) at that database too, or every later backup silently dumps the old one. The success line names the database it dumped; check it. `BACKUP_DUMP_ENCODING=SQL_ASCII` (the dump holds each text as the bytes stored, unconverted) is only for moving a database that is not UTF8 into a UTF8 one ([UPGRADING.md section 42](../UPGRADING.md#42-database-encoding-must-be-utf8-a-481)); the script refuses any other value. Leave it unset in backup jobs.

Restore into an explicitly named **empty** destination. Stop the application first. For the internal database (the default deployment, which publishes no database port), keep the `db` service running and name a database inside it; client tools run in the container:

```sh
docker compose stop app
AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE=opsecmkt_restored \
  ./scripts/restore.sh backups/market.dump.age
```

To verify an archive without touching the live database, restore it to a uniquely named disposable database and drop only that database when finished. The generated name is checked before use, and the cleanup uses `--if-exists` in case restore failed before creating it:

```sh
set -euo pipefail
verify_db="opsecmkt_verify_$(date -u +%Y%m%dt%H%M%Sz)_$$"
[[ $verify_db =~ ^opsecmkt_verify_[0-9]{8}t[0-9]{6}z_[0-9]+$ ]] || { echo "invalid generated database name" >&2; exit 1; }
exists=$(docker compose exec -T db psql -X -At -v ON_ERROR_STOP=1 -U opsecmkt -d postgres \
  -c "SELECT count(*) FROM pg_database WHERE datname='$verify_db'")
test "$exists" = 0 || { echo "refusing to reuse database $verify_db" >&2; exit 1; }
cleanup() { docker compose exec -T db dropdb -U opsecmkt --if-exists "$verify_db"; }
trap cleanup EXIT
AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE="$verify_db" \
  ./scripts/restore.sh backups/market.dump.age
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d "$verify_db" \
  -c 'SELECT count(*) AS migration_count FROM schema_migrations'
```

The trap removes only the generated `opsecmkt_verify_...` database; this check does not point the application at it. For an external PostgreSQL server, use a fresh explicitly named database there and its corresponding `dropdb` command after verification.

A database that does not exist yet is created next to the current one, which stays untouched. Point the app at it by changing the database name in `DATABASE_URL` in `.env` (for example `postgres://opsecmkt:PASSWORD@db:5432/opsecmkt_restored?sslmode=disable`); the script prints this exact line for the database it restored into. With payments configured, cut the site off from users before starting the app on it, as the [reconciliation procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) describes; otherwise run `docker compose up -d`. Later `scripts/backup.sh` runs then dump `opsecmkt_restored`, not the abandoned `opsecmkt`. If the `postgres_data` volume itself was lost, `docker compose up -d --wait db` initializes an empty `opsecmkt` database and `RESTORE_INTERNAL_DATABASE=opsecmkt` restores into it with no `.env` change. For an external or otherwise host-reachable server, create an empty database there and restore with Python 3 and compatible PostgreSQL client tools:

```sh
RESTORE_DATABASE_URL='postgres://user:password@localhost/recovery?sslmode=verify-full' \
  AGE_IDENTITY=/secure/backup-key.txt ./scripts/restore.sh backups/market.dump.age
```

After switching `DATABASE_URL` to that database, set `BACKUP_DATABASE_URL` to it as well (see above).

Set exactly one of the two destinations. Before anything else the script reads the encoding the backup's text was written in and the encoding of the database it was taken from (both recorded in the dump), and restores only a dump written in `SQL_ASCII` (taken with `BACKUP_DUMP_ENCODING=SQL_ASCII`, or of a `SQL_ASCII` database) or in `UTF8` from a UTF8 or `SQL_ASCII` database; it refuses anything else, changing nothing: a `LATIN1` database backed up without that option (as `LATIN1`, or as `UTF8` when its `client_encoding` is set to `UTF8`) would come out double-encoded; [UPGRADING.md section 42](../UPGRADING.md#42-database-encoding-must-be-utf8-a-481) says how to restore such a backup. Either way the restore requires typing `RESTORE`, runs in one transaction, and does not drop existing tables: restoring into a database that already has the application's tables fails and changes nothing. The recovery protection below is applied by the same SQL in both modes. Restore with the `APP_SECRET_KEY` (on an install without it, the `SETUP_TOKEN`) that was in use when the backup was taken, or TOTP secrets in it cannot be read (see [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). To roll back an upgrade, follow [UPGRADING.md](../UPGRADING.md#5-rolling-back).

The script sets a persistent recovery gate that pauses every escrow watcher's building, co-signing and sending, and holds every restored escrow settlement that has a stored transaction (Monero settlements; error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the node or wallet shows no broadcast of it. It also retires free Monero escrow accounts, moves active and sealed Monero escrow wallets to closing, and makes escrow checkout wait for a sweep that finds escrow wallets created after the backup. The script prints how many settlements it held, and adds one audit row recording the gate and those counts. It also prints `<N> images restored from a backup taken <time>; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again` (the time is when the dump was made, in UTC, read from the dump itself; "at an unknown time" if the client tools cannot read it; for a backup made before this release on a host not set to UTC it is off by that zone's offset from UTC): a staff takedown or an owner's removal deletes the image's row, so the restored database has every photo and avatar the backup had, including those removed since. Before users return, have a moderator go through Recent photos and take down again any image that had been removed. A database backup may predate an already-broadcast settlement, so reviewing only
the restored settlement rows is insufficient. Follow the [complete reconciliation and explicit unlock procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
before allowing user writes or restarting settlement sends. Validate recovered accounts, listings, orders and
settings before switching traffic. Never test a restore against your live database; a test restore in a
second checkout on the same host needs its own `COMPOSE_PROJECT_NAME` (for example `opsecmkt-rehearsal`, see
[above](#backups-and-recovery)), or its `docker compose` commands act on the live containers and volumes.

**Any restore not done by `scripts/restore.sh` skips this protection.** A host or volume snapshot (including a copy of the `postgres_data` volume), a managed-database point-in-time recovery or a manual `pg_restore` brings escrow settlements back with no recovery gate and no holds, and the escrow watchers broadcast or relay them as soon as the application starts, even those already sent after that point in time. After such a restore keep the application stopped and apply the protection of [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database with `docker compose run --rm app -set-restore-gate` before starting it, then follow the same reconciliation procedure and clear the gate with `-clear-restore-gate HANDLE` ([runbook](testnet-runbook.md#clear-the-gate)).

### Secrets, .env and onion identities

`scripts/backup.sh` dumps the database only. A restored database also needs the `.env` it ran with, and each onion address exists only as the keys in its Tor volume. Back both up whenever they change, encrypted to the same age recipient, and store them with the database backups (never next to the age identity).

| Secret | Kept in | Backup | Rotation |
| --- | --- | --- | --- |
| `APP_SECRET_KEY` | `.env` | with `.env` | Derives the CSRF/CAPTCHA key and the key that encrypts TOTP secrets. Restore a database with the key in use when it was dumped; under another key its TOTP secrets cannot be read. Changing it refuses every open form once and makes enrolled TOTP secrets unreadable. Rotate only when it is exposed, with the consequences [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token) describes. An install that predates it has none: its keys still derive from `SETUP_TOKEN` (the startup log says `keys: derived from SETUP_TOKEN (legacy)`); to move to it, set `APP_SECRET_KEY` to exactly the current `SETUP_TOKEN` value, which `./scripts/install.sh upgrade` does ([UPGRADING.md](../UPGRADING.md#app_secret_key-and-stricter-startup-checks)). |
| `SETUP_TOKEN` | `.env` | with `.env` | Authorizes the first-run `/setup` page only. The installer prints it once at the end of `init`; `./scripts/install.sh setup-token` shows it again (and `--new` reissues it) while setup is pending. The app refuses to start without it until setup is complete, and ignores it afterwards (while it is set, startup logs a hint); `/setup` then answers 403 whatever the token. Once `APP_SECRET_KEY` is set and setup is complete, `status`, `add-mirror` and `upgrade` remove it from `.env`. Without `APP_SECRET_KEY` it is also the legacy key root above: keep it, or copy it to `APP_SECRET_KEY` first; changing it then has every consequence listed for `APP_SECRET_KEY`, escrow keys included. |
| `MONERO_WALLET_SECRET` (only with [Monero escrow](#monero-escrow)) | `.env` | with `.env` | Every escrow wallet's password derives from it (HMAC-SHA256 with the wallet's name), so no wallet password is stored. It must be its own secret, not `SETUP_TOKEN` or `APP_SECRET_KEY`, so rotating those never locks wallets. A verifier of it is pinned in the database on the first start; with another secret Monero escrow stays disabled and opens or creates no wallet. **No tool re-keys existing wallets**: restore the original secret. Never change it while wallets exist. The installer generates it with `--monero-escrow yes`. |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` (only with the `monero-escrow-wallet` service) | `.env` | with `.env` | The escrow wallet-rpc's `--rpc-login` password, the same as in `ESCROW_XMR_WALLET_RPC_URL`; generated by the installer. Rotating it means changing both and restarting the service and the app. |
| `AUDIT_SIGNING_KEY` | `.env` | with `.env` | Signs audit exports ([Transparency](#transparency)). A new key (`openssl rand -hex 32`, then restart the app) is a new public key on `/canary`: announce it. |
| `POSTGRES_PASSWORD` (internal database) | `.env`, twice: `POSTGRES_PASSWORD` and the password in `DATABASE_URL`; the database keeps its own hash in the `postgres_data` volume | with `.env` | `POSTGRES_PASSWORD` is used only when an empty volume is initialized, so editing it later changes nothing. Rotate in the database first: `docker compose exec db psql -U opsecmkt -d opsecmkt -c '\password opsecmkt'` asks for the new password twice and sends `ALTER ROLE opsecmkt PASSWORD` with it already hashed, so it appears in no command line or server log. Then put it in both places in `.env` and run `docker compose up -d`. A hex password (`openssl rand -hex 24`) needs no escaping in the URL. For an external database, change it on that server and in `DATABASE_URL`. |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` (local Monero escrow wallet-rpc) | `.env`, repeated in `ESCROW_XMR_WALLET_RPC_URL` | with `.env` | Change the password and the same value in its URL together, then `docker compose up -d`. |
| Onion keys | the `tor_identity` volume (`tor_mirror_identity` for the mirror), named `<project>_tor_identity` by Compose, for example `opsecmkt_tor_identity` | the commands below | Losing or replacing the keys changes the address for good. A copied key lets anyone impersonate the address, so retire an exposed one: remove the volume, let Tor create a new address and publish it in your signed verification document. A key you bring yourself (a vanity address, `init --onion-key-dir`) is imported into the empty volume before Tor first starts ([Use your own onion address](install/tor.md#use-your-own-vanity-onion-address)); a key someone else generated, such as a vanity service, is held by them too. |

The commands run in bash from the repository root, with `age` installed as above, and use the Tor image Compose builds for the project (`<project>-tor`; on a new host run `docker compose build tor` first). Set the variables each command names in the same shell first. Every command runs in a subshell that stops at the first failure, reads the project name from `docker compose config` (so it follows `COMPOSE_PROJECT_NAME` in `.env`), runs its containers with no network, and never overwrites a file. `--pull never` makes a missing local image an error instead of a download of whatever image of that name a registry offers.

Back up `.env` (set `AGE_RECIPIENT`):

<!-- ops-cmd: env-backup -->
```sh
( set -eu
  : "${AGE_RECIPIENT:?set AGE_RECIPIENT}"
  backup="backups/env-$(date -u +%Y%m%dT%H%M%SZ).age"
  test ! -e "$backup"
  mkdir -p backups
  umask 077
  age -r "$AGE_RECIPIENT" -o "$backup" .env
  echo "Wrote $backup" )
```

Back up an onion identity (set `identity=tor_identity`, or `identity=tor_mirror_identity` for the mirror, and `AGE_RECIPIENT`). Tor may keep running: only its `marketplace` directory (keys and hostname), which Tor does not change once created, is copied, as `debian-tor` with no capabilities. The command prints the address the backup holds; record it with the file.

<!-- ops-cmd: tor-identity-backup -->
```sh
( set -eu -o pipefail
  : "${identity:?set identity=tor_identity or identity=tor_mirror_identity}" "${AGE_RECIPIENT:?set AGE_RECIPIENT}"
  project=$(docker compose config | sed -n 's/^name: //p')
  volume="${project}_$identity"
  backup="backups/$volume-$(date -u +%Y%m%dT%H%M%SZ).tar.age"
  docker volume inspect "$volume" >/dev/null
  test ! -e "$backup"
  mkdir -p backups
  umask 077
  docker run --rm --log-driver none --pull never --network none --cap-drop ALL --security-opt no-new-privileges:true -v "$volume:/var/lib/tor:ro" \
    --entrypoint tar "$project-tor" -C /var/lib/tor -cf - marketplace |
    age -r "$AGE_RECIPIENT" -o "$backup" || { rm -f "$backup"; exit 1; }
  docker run --rm --pull never --network none --cap-drop ALL -v "$volume:/var/lib/tor:ro" --entrypoint cat "$project-tor" \
    /var/lib/tor/marketplace/hostname
  echo "Wrote $backup" )
```

To recover on a new host (or after losing the volumes): check out the same revision, restore `.env`, run `docker compose build tor`, restore each onion identity **before Tor first starts there**, restore the database as described above, then `docker compose up -d`. Restore `.env` (set `backup` to the `.env` backup file and `AGE_IDENTITY`); it refuses to replace an existing `.env`:

<!-- ops-cmd: env-restore -->
```sh
( set -eu
  : "${backup:?set backup to the .env backup file}" "${AGE_IDENTITY:?set AGE_IDENTITY}"
  if [ -e .env ]; then echo 'Refusing: .env exists; move it aside first' >&2; exit 1; fi
  umask 077
  age -d -i "$AGE_IDENTITY" -o .env "$backup"
  echo 'Restored .env' )
```

Restore an onion identity (set `identity`, `backup` to its backup file, `AGE_IDENTITY`, and `address` to the onion address you published for that identity, for example in your signed verification document; never take it from the backup alone). The archive is decrypted straight into `./scripts/install.sh restore-onion`, which treats it as untrusted input and writes nothing until every check has passed (war room A-531):

- it refuses a volume that is not empty (or cannot be read), a running `tor` (or `tor-mirror`) service, and `tor_admin_identity`: the private administrator onion keeps an identity of its own;
- it never runs `tar -x` on the archive. A container with no network and no capabilities checks that it is an uncompressed tar archive (a compressed one is refused, never unpacked), lists it and refuses anything a backup never holds: any name other than `marketplace/`, `marketplace/hs_ed25519_secret_key`, `marketplace/hs_ed25519_public_key`, `marketplace/hostname` and an empty `marketplace/authorized_clients/`, so no `../` or absolute path and no other file; any symbolic or hard link, device or other type in their place; any entry twice; anything over 1 MiB. Only the key files and `hostname` are copied out, each by its exact name and bounded in size (96, 64 and 64 bytes);
- the keys then pass the same checks and offline Tor dry run as an [imported identity](install/tor.md#use-your-own-vanity-onion-address): exact sizes and headers, a public key that matches the secret key, and the address Tor derives from the key;
- that derived address must be `address`; run by hand without `--onion-address`, `restore-onion` asks you to confirm it on the terminal instead, and with neither it refuses. The archive's `hostname`, when present, must name the same address: Tor does not check `hostname` and overwrites a forged one only after it starts. The file itself is never restored; Tor writes it from the key;
- it refuses a secret key that the other public onion's volume or the administrator onion's already holds, compared byte for byte (a volume restored or prepared but not yet started has no `hostname` to compare), and refuses when it cannot read one to compare.

The containers that carry key bytes run with `--log-driver none`, so Docker's log driver keeps none of them. Only after every check does it create the volume (labelled as Compose labels its own) and stream the key files into it as root, giving them to `debian-tor` (directories 700, files 600), because Tor refuses keys it does not own. Its last line is the address derived from the key:

<!-- ops-cmd: tor-identity-restore -->
```sh
( set -eu -o pipefail
  : "${identity:?set identity=tor_identity or identity=tor_mirror_identity}" "${backup:?set backup to the identity backup file}" "${AGE_IDENTITY:?set AGE_IDENTITY}"
  : "${address:?set address to the onion address you published for this identity}"
  test -f "$backup"
  age -d -i "$AGE_IDENTITY" "$backup" | ./scripts/install.sh restore-onion "$identity" --onion-address "$address" )
```

After `docker compose up -d`, `./scripts/install.sh status` must show the printed address for `tor` (or `tor-mirror`); a different address means Tor did not use the restored keys. The restore refuses a volume that is not empty: Tor has already run on this host and created another address. If that address was never published, remove it and restore again: `docker compose rm -sf tor` (or `tor-mirror`), then `docker volume rm opsecmkt_tor_identity` (your project's volume name). `scripts/test-tor-identity-restore.sh` runs these four blocks verbatim, and feeds the restore forged, extended, linked, compressed, oversized and mismatched archives ([operations tests](operations-tests.md#documented-env-and-onion-identity-backup-and-restore)).

## Verification and maintenance

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
for script in scripts/install.sh scripts/backup.sh scripts/restore.sh; do bash -n "$script"; done
./scripts/install.sh check    # .env and the Compose configuration, as startup would judge them
./scripts/install.sh status   # health, setup state, schema and onion addresses
```

`check` ends with `docker compose config --quiet`, which reads `.env`. `docker compose ps` shows each service's health: the app is healthy once `/healthz` answers `ok` (its healthcheck runs `/app/server -healthcheck` inside the container), and each Tor service once it has written its onion address (the descriptor can take 1–3 more minutes to reach the network). `docker compose up -d --wait` returns when every service is healthy. `docker compose exec app /app/server -status` prints `installed=true|false schema=NNN keyroot=app_secret_key|setup_token` (setup complete, highest applied migration, and whether the app's keys derive from `APP_SECRET_KEY` or, on a legacy install, `SETUP_TOKEN`) and nothing secret. Startup logs show database connection/migration errors (see [Troubleshooting](#troubleshooting)). Review dependencies and rebuild periodically. The Compose examples use official [Go](https://hub.docker.com/_/golang) and [PostgreSQL](https://hub.docker.com/_/postgres) image families; production operators should pin reviewed image digests and plan PostgreSQL major-version upgrades explicitly. Tor is installed at a pinned version from the Tor Project's Debian repository ([Tor image](#tor-image)); the onion-service configuration follows the [Tor Project setup guide](https://community.torproject.org/onion-services/setup/) and adds the [denial-of-service defences](#onion-service-defences).

GitHub Actions repeats the race tests, vet, dependency verification, govulncheck and Compose validation using the Go toolchain pinned in `go.mod` (`toolchain` directive), the same version the Docker image builds with. CI supplies PostgreSQL 17 to the integration tests, which use an isolated database or schema per test. Locally set `TEST_DATABASE_URL` to a dedicated test database to include those tests. These checks do not substitute for a live Docker/Tor deployment test.

Backup/restore URLs require an explicit host and database. The helper decodes credentials into libpq environment variables so they are not included in process arguments. Standard TLS options are supported; unsupported query options fail closed. Environment variables remain visible to privileged host processes.

The encrypted backup/restore scripts were exercised against an isolated PostgreSQL 16.15 test cluster with age 1.3.2: two rows including Unicode round-tripped, encrypted output had mode 0600, existing backups were refused, a wrong identity failed without creating tables, and restoring into an occupied target rolled back without changing its rows. Both scratch databases and temporary keys/dumps were removed afterward. CI now also runs `scripts/test-internal-db-restore.sh`, which backs up and restores through the PostgreSQL 17 internal-db Compose service with no published port (synthetic tables, the recovery gate and holds, and backups following `DATABASE_URL` after a side-by-side restore); see [operations-tests.md](operations-tests.md). A full rehearsal on your own deployment, including the application and wallets, is still yours to run.

## Troubleshooting

If every form, including `/setup`, returns 403 "Origin does not match Host; if this market runs behind a reverse proxy, the proxy must forward the Host header.", the reverse proxy is rewriting `Host`. Forward the original header ([nginx example](#clearnet-and-onion-deployment)); these refusals are not logged.

The application logs to standard output (`docker compose logs app`). Every response with a 5xx status writes one line (after the log timestamp), and the error page the user sees ends with the same reference:

```text
http 5xx kind=page name=catalog status=500 ref=0bbdc379 cause=22021
```

- `kind` is `page` (a GET) or `action` (a form POST); `name` is the registered route: a page name such as `order`, an action path such as `/orders/pay`, a raw path (`/captcha`, `/admin/audit-export`, `/healthz`), an image route (`/img/` or `/desk/img/`, without the image id), or `unregistered`.
- `ref` is 8 random hex characters, shown to the user as `Reference: 0bbdc379`. Ask a user who reports an error for it and search the log for `ref=0bbdc379`.
- `cause` is a class, never the error's text: a PostgreSQL SQLSTATE, followed by the table and constraint when PostgreSQL names them (`23505 payouts payouts_order_id_key`); `timeout` (the 12 s request bound; a PostgreSQL statement timeout shows as `57014`); `canceled` (the client went away); `db-connection` (the database was unreachable or the connection broke); `template:<name>` (a page failed to render); otherwise the Go type of the error (for example `*market.httpError`, a refusal the code raised itself such as "Authentication is busy").
- The line never holds the URL path or query, form values, cookies, the client address, `Host`, `User-Agent`, a handle or user id. Reproduce the failure, or match the time and route, to learn more.
- At most 10 such lines are written a second. Beyond that, one line counts the rest, `http 5xx suppressed=N (more than 10 lines in 1s)`, whose references are then not in the log.
- A Monero escrow checkout refused because a restore paused Monero escrow is not a server error (A-559): it answers 503 on the refusal page without a reference and writes `http paused kind=action name=/orders/escrow-xmr status=503 cause=restore-gate` (or `cause=restore-sweep`), throttled the same way (`http paused suppressed=N`). *Admin → Payments* counts these refusals since the server started; they are not listed under *Recent server errors*.
- `57014` (statement timeout, 8 s) or `55P03` (lock timeout, 3 s) with status 503 means the application pool's [session timeouts](#migrations) stopped a query: the database is overloaded or a row stayed locked. Occasional lines under load are expected; a steady stream on one route points at a slow query.
- `57P01`, `57P02` or `57P03` (the database is shutting down, crashed or still starting), `53300` (too many connections), `08000`, `08001`, `08003`, `08004` or `08006` (a connection exception) or `db-connection`, with status 503, means the request's database connection was lost or refused, for example during a PostgreSQL restart or failover; the user is shown "The market is busy" or, when the first query of the request already failed, "Service unavailable", both with `Retry-After: 5`. A steady stream outside a restart points at the database's health or its `max_connections`.

**Overload.** The public listener accepts at most 2048 connections at once, plus 64 on the optional private administrator Unix listener. The database pool and CPU are shared across those listeners; the application admission limits below are the public listener's, and requests on the private administrator listener have small limits of their own (A-570: 8 pages, 8 public images, 2 CAPTCHA images, 4 public and 4 signed-in form posts, 4 session lookups and 2 password checks), so a flood on the public listener cannot refuse them, and filling them refuses no public request. The app serves at most 64 pages, 16 form posts that change a signed-in account's data and 16 public ones (sign-in, registration, the sign-in challenge, whoever sends them), 16 requests for public images (`/img/`, which take no page place, so a page of thumbnails or a flood of image requests cannot crowd out pages), 8 CAPTCHA images and 4 password checks at a time. A form post takes its place only after its body has been read (at most 64 KiB and 64 fields, within the 15 s read timeout) and its origin and form token have been checked, so a client that sends its body slowly, or posts junk, holds only a connection (a post that carries a session cookie then looks it up in one of 16 lookup places, waiting its turn for up to 2 s; pages never use them); sign-in and registration posts with a malformed handle or password, or a CAPTCHA that was not issued to that browser or has expired, are refused before they take a place. Public posts, anonymous or from a signed-in account, therefore cannot crowd out a signed-in user's or an administrator's changes (each account's own changes stop at its budget of 100 in ten minutes), but a flood of well-formed public posts can still delay real sign-ins. While all 2048 connections are taken, HTTP keep-alive is switched off (idle connections are closed) until no more than 1536 are in use. A request beyond the other limits is answered at once with 503 "The market is busy. Try again in a few seconds." and `Retry-After: 5`, and its connection is closed; these refusals are **not logged**, so a flood shows up as users reporting that message, not as log lines. Every other 503 also carries `Retry-After: 5`, and every 429 `Retry-After: 600`, except the one refusing a second payment request while the same account's first is still in progress, which carries `Retry-After: 5`. That one-at-a-time limit is per app process: with several app instances against one database, each allows one. `/healthz` takes no slot and pings the database on its own connection, so the container stays healthy under a flood of pages; it still waits for one of the 2048 connections. These limits are fixed in the code; on Tor they sit behind the onion service's own defences.

The escrow watchers log what they do to an escrow in lines that start `escrow:` and name the order by its first 8 characters and a class, never the node's text (which can quote an address): for example `escrow: settlement of order 1b4d8036 not sent yet: <class>`, `escrow: Monero escrow daemon not synced (<reason>); Monero escrow watcher passes skipped until it is` and `escrow: Monero escrow on (stagenet)`. A state change is logged once, not on every retry. `db-connection` (or `timeout`) alone means the watcher could not reach the database, or could not finish its pass within 5 minutes (for example while the pool was exhausted); the next pass picks up what it left; the line never holds `DATABASE_URL`, its user, database name, host or port. The recovery code reveal sweep logs `recovery code reveal sweep: <class>` the same way.

When the database connection fails at startup the server exits with its class and no part of `DATABASE_URL`: `authentication failed (SQLSTATE 28P01)` or `(SQLSTATE 28000)` for a wrong user or password or a `pg_hba.conf` rule, `database missing (SQLSTATE 3D000)`, `host not found`, `connection refused`, `timed out`, or `TLS required` (the server accepts only encrypted connections; set `sslmode=require` or `verify-full`).

**Volume ownership after a restore** (A-536). `db`, `monero` and `monero-escrow-wallet` start as their images' users (`70:70` for PostgreSQL, `1000:1000` for both Monero services) with no capabilities, so they cannot take back a volume restored as root (for example by `tar` as root from an archive without the original owners). `db` then exits with `initdb: error: could not access directory "/var/lib/postgresql/data": Permission denied`, `monero` exits with `Failed to open lmdb environment: Permission denied`, and `monero-escrow-wallet` starts but gets `Permission denied` when it creates or opens a wallet. Stop the service, give the volume back to that user and start it again (here the internal database, in the default `opsecmkt` project):

```sh
docker compose stop db
docker run --rm --network none -v opsecmkt_postgres_data:/v --entrypoint chown postgres:17-alpine -R 70:70 /v
docker compose up -d db
```

For `opsecmkt_monero_data` or `opsecmkt_monero_escrow_wallet` use `-R 1000:1000` (with the monerod image, for example). The line `chmod: /var/run/postgresql: Operation not permitted` at every start of `db` is harmless: that directory is in memory and PostgreSQL can write to it.

**Image scanner.** Photo and avatar uploads need the ClamAV scanner: `CLAMD_ADDRESS` is clamd's `host:port` on the scanner network (the installer writes `clamd:3310`; empty turns uploads off), and `SCANNER_MAX_SIGNATURE_AGE_DAYS` (default 30, from 7 to 90) is how old the daily signatures may be before uploads pause. The app refuses to start with a malformed value of either, naming the variable. No setting lets an upload skip the scan. The app asks clamd for `PING` and `VERSION` every 30 seconds; `/admin/system#image-scanner` shows the result, and each change of state writes one line:

```text
image scanner: Ready
image scanner: Unavailable (connection failed)
image scanner: Unavailable (timed out)
image scanner: Unavailable (unexpected reply)
image scanner: Signatures stale (31 days, limit 30)
image scanner: Signatures stale (signature date unreadable, limit 30)
```

- `Unavailable (not checked yet)` on the admin page: the app started less than a few seconds ago.
- `connection failed`: clamd is not running, is still loading its signatures (it refuses connections for the first minutes after a start; its healthcheck allows 10 minutes), or is not on the `scanner` network with the app at `CLAMD_ADDRESS`. Check `docker compose ps clamd` and `docker compose logs clamd`.
- `timed out`: clamd accepted the connection but did not answer within 10 seconds (connecting is given 3): it is overloaded, short of memory (`docker stats`) or reloading its database.
- `unexpected reply`: whatever answers at `CLAMD_ADDRESS` is not clamd.
- `Signatures stale`: run `./scripts/install.sh refresh-scanner` to pull the current scanner image; uploads resume within 30 seconds of clamd answering with the new date. `signature date unreadable` means clamd reported no daily database date, which counts as stale.
- A scan that clamd fails (no connection, no answer within 10 seconds) also marks the scanner Unavailable until the next successful check, so uploads are refused for up to 30 seconds afterwards. An uploader who gives up first does not.
- `/healthz` never contacts clamd: a scanner outage pauses uploads only, and never marks the app unhealthy or restarts it.

Each upload the scanner refuses writes one line, and the uploader's error page ends with the same reference:

```text
upload outcome=refused reason=infected sig=Win.Test.EICAR_HDB-1 ref=3f9a0c12 kind=listing
upload outcome=refused reason=timeout ref=0bbdc379 kind=avatar
```

`reason` is `infected` (a signature matched; `sig` is its name), `unavailable` (no connection, or clamd closed it without answering), `timeout` or `error` (clamd answered anything other than `stream: OK` or a match, such as `INSTREAM size limit exceeded`, or closed the connection before reading the whole upload; this refuses that upload only and does not pause the scanner). `kind` is `listing` or `avatar`. The line never holds a handle, user id, listing id, file name or size, and the reference is stored nowhere else: a vendor who believes a refusal was a false positive can quote it, and you find the signature with `docker compose logs app | grep ref=3f9a0c12`. The uploader is never shown the signature name, and their activity log records only "Upload refused by the malware scanner". The admin panel counts refusals by signature over about the last 24 hours, in memory (the counts start again when the app restarts) and never linked to accounts. Uploads refused because the scanner is off or its signatures are stale are not logged; the admin panel says why.

**Photo storage and upload limits** (A-186). Stored photos and avatars are re-encoded JPEGs in the `images` table, so they are in every database backup. `IMAGE_STORAGE_LIMIT_MB` (default 2048, from 1 to 1048576; the app refuses to start with anything else) caps what all of them may use together; once an upload would pass it, uploaders see "Photo storage on this market is full; contact the operator" and nothing is stored. `/admin/system#image-scanner` shows how many are stored and the megabytes they use against the limit. The other limits are fixed: 4 MB a file, 4096 pixels a side and 4096×3072 pixels in all (about 12.6 megapixels, so a 4032×3024 phone photo fits), 64 MiB of estimated decode memory (a progressive colour JPEG reaches it at about 4.4 megapixels with full-resolution colour, 7.4 with the usual 4:2:0), 100 JPEG scans, 4 photos a listing, one avatar an account, 50 MB of photos and avatar an account, 20 uploads an account in 10 minutes and one at a time, and 2 uploads (one being decoded) at once for the whole app; a busy upload slot answers 503 with `Retry-After: 5`. An admitted upload may take 90 seconds to arrive, longer than the server's 15-second limit for other requests. Upload memory is budgeted beside `maxConns` in `cmd/server/main.go`: about 220 MiB at most (the 64 MiB decode estimate bounds what the decoder allocates), plus at most 96 MiB of image bytes held by image responses being written (A-187: 64 MiB on `/img`, 32 MiB on `/desk/img`), about 320 MiB in all, inside the app's 400 MiB `GOMEMLIMIT`. A refused or failed upload stores nothing and logs no file name; a failure of the database while storing one logs the usual `http 5xx kind=action name=/listings/photo` (or `/account/avatar`) line with the reference the uploader sees.

**Serving images** (A-187). Stored images are served only at `/img/<id>-t` or `-f` (public: a published listing's photos, vendors' avatars, and staff avatars only while the public staff list is on and that account is not hidden, both off by default) and `/desk/img/<id>-t` or `-f` (signed in: the owner's own, any for an administrator, a staff avatar for the parties of an order that staff member decided and for moderators, once that member has decided a dispute, and an archived listing's photos for whoever may open that listing's page through an order on it). Anything else gets the same 404 as an unknown image id, and a POST gets 405. Responses still being written hold at most 64 MiB of image bytes on `/img` and, separately, 32 MiB on `/desk/img`, so signed-in requests cannot use up the public route's share; beyond that an image request gets the busy 503 with `Retry-After: 5` (not logged), so visitors reading slowly, or not at all, cannot exhaust the app's memory, while pages are unaffected. A visitor's browser may reuse a response for a day without asking again and keep it on disk until its cache is cleared (`Cache-Control: private, max-age=86400, immutable`; `max-age` sets freshness, not how long a copy stays); a reverse proxy in front of the app should not cache them (`private`) and must not add `ETag` or cookies. A database failure while serving one logs `http 5xx kind=page name=/img/` (or `/desk/img/`), never the image id.

**Public staff list** (A-189). `/admin/transparency#staff-list` has one switch, "Show a public staff list", off on every install until you turn it on (with your password, and authenticator code when enrolled; each change is in the audit trail as "Public staff list changed from off to on" or back). While it is off, `/canary` lists no staff and no staff avatar is served on the public image route. While it is on, `/canary#staff` lists each moderator and administrator who is not suspended and has chosen to be listed, with their handle, role and avatar; every staff account starts hidden, and a hidden member is not listed at all. Whatever the switch, the staff member who records a dispute decision has their avatar shown beside it on that order's page, to its buyer and vendor and to staff. Tell staff before they upload an avatar that a photo links them to every decision they make.
