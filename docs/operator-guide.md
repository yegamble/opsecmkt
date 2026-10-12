# OPSEC Market operator guide

Deployment, feature configuration, migrations and recovery reference. Start with
[the README](../README.md) for product context and local development. Commands
below run from the repository root.

**Current build:** the pooled custodial wallet has been removed. Only per-order escrow
on the supported test networks can request payment (Bitcoin regtest, signet and
testnet4; Monero fakechain and stagenet; mainnet and testnet3 are refused until
A-460), and both escrow adapters stay off by default. Nonblank pooled RPC settings
(`BITCOIN_RPC_URL`, `BITCOIN_WALLET`, `MONERO_WALLET_RPC_URL`, `MONERO_RPC_URL`) and the
legacy `monero-wallet` profile are refused, and migrations 107 and 108 refuse a
database that holds pooled payment history; see
[UPGRADING section 58](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448)
and [section 59](../UPGRADING.md#59-pooled-history-refused-migration-108-a-492).
This guide describes only this build. The pooled-payment, mainnet, cutover and drain
procedures of the earlier builds were removed from it; this server cannot run or drain
them (owner directive: no pooled testing or drain), so a deployment with pooled
history finishes that on the build that has it before upgrading.

Current Bitcoin operator setup provisions the escrow RPC user; configure the
dedicated signer RPC user separately using the signer instructions below. It does
not provision a pooled user. Before reusing an old node, remove the old pooled
user's `rpcauth` and `rpcwhitelist` entries and unload its pooled wallet
only after saving recovery. Provision the new escrow users with
`scripts/escrow-node-setup.sh`; separate arbiter, watch and signer protections
still apply.

## Installer

`scripts/install.sh` installs a checkout and maintains it afterwards. It fetches nothing itself (Docker builds and pulls the images). After `init` it changes `.env` only by rewriting the lines it names through a temporary file moved into place (mode `0600`; every other line stays byte for byte). It prints one secret, the setup token, and only at the end of `init` and from `setup-token`.

| Command | What it does |
| --- | --- |
| `init` (the default: `./scripts/install.sh` and `./scripts/install.sh --local` run it too) | Refuses when `.env` exists, when containers of the Compose project come from another checkout, and when any volume of the project exists. Asks its questions (none when flags answer them), writes `.env` with distinct random `APP_SECRET_KEY` and `SETUP_TOKEN`, runs `check`, then `docker compose up -d --build --wait`. With `--external-db` it first reads that database with a one-off `docker compose run --rm --no-deps app -status`: when a market's setup is already complete there, it removes the new `.env`, starts nothing and says to restore that market's `.env` instead (new secrets would leave its TOTP secrets unreadable). On Tor it waits up to 90 seconds for the onion address. It ends with the setup link and token, and for clearnet behind a proxy with Caddy and nginx snippets for `APP_PORT`. When it started a local Bitcoin node it also prints the two [`scripts/escrow-node-setup.sh`](#escrow-node-set-up-and-the-public-network-smoke) commands that turn Bitcoin escrow on (set-up, then `--verify`); Bitcoin escrow stays off, and the installer writes no `ESCROW_BTC_*` line, until you run them and paste what they print. |
| `init --admin HANDLE [--site-name NAME]` | The same, but creates the first administrator in the terminal ([First administrator from the host](#authentication)): it asks for the password twice without showing it, refuses one that does not match, is not 12-72 bytes or is not valid UTF-8 before it writes or starts anything, and pipes it to `-bootstrap-admin` on standard input, never in a command line, the environment or `.env`. Then it removes `SETUP_TOKEN` from `.env`, recreates the app and prints no token. |
| `check` | Validates `.env` and the Compose configuration and starts nothing: the app's startup refusals (placeholder or weak secrets, `COOKIE_SECURE`, a placeholder `DATABASE_URL`, `APP_MODE` against `COMPOSE_FILE`), [`compose.nodes-tor.yaml`](#local-nodes-over-tor) only with `APP_MODE=tor` and `compose.tor.yaml` and after `compose.nodes.yaml`, `ESCROW_XMR_TOR_RELAY` blank or `clearnet-accepted` with Monero escrow on (and a warning when `APP_MODE=tor` and `ESCROW_XMR=on` would hold Monero relays: neither that value nor `compose.nodes-tor.yaml`, A-515), `POSTGRES_PASSWORD` against the password in `DATABASE_URL`, `COMPOSE_PROFILES` against the database and node settings, node images on `latest`, the mirror outside Tor, the [malware scanner](#photo-uploads-and-the-malware-scanner) (its profile, `CLAMD_ADDRESS` and `compose.scanner.yaml` only together, a readable `deploy/clamd.conf`, `CLAMAV_IMAGE` pinned to a release with signatures and, from `docker compose config`, `clamd` alone with the app on the internal `scanner` network with no port or volume), the project's containers created from another checkout (or this checkout's containers under another project name), and `docker compose config`. When the directory those containers were created from no longer exists, the checkout was probably moved: `check` says so, and `docker compose up -d --force-recreate` run in the moved checkout recreates them with its directory, keeping the volumes (a plain `up -d` leaves them as they are). Do not change `COMPOSE_PROJECT_NAME` for this. Messages name keys, never values. |
| `status` | Mode, each service's health, whether setup is pending or complete, the schema version, every onion address and the malware scanner's state with the version and date of its signatures. |
| `setup-token [--new]` | While setup is pending: shows the token and setup link, or with `--new` issues a new token and recreates the app. After setup it exits 1: the token grants nothing then. It never shows a token that is also the key root (an install without its own `APP_SECRET_KEY`); `--new` issues a separate one and keeps the keys. |
| `add-mirror` | See [Optional onion mirror](#optional-onion-mirror). |
| `add-scanner`, `refresh-scanner` | Add the ClamAV malware scanner that photo uploads need, and refresh its signatures: see [Photo uploads and the malware scanner](#photo-uploads-and-the-malware-scanner). |
| `upgrade [--no-backup]` | After `git pull`: see [UPGRADING.md](../UPGRADING.md). |

Flags answer `init`'s questions: `--tor` or `--clearnet`; `--local` (loopback HTTP, internal database, no nodes); `--external-db URL` (without it, a flagged install gets the internal database); `--behind-proxy yes|no` (clearnet: secure cookies behind an HTTPS proxy, or local HTTP only); `--nodes local|none|custom` (`local` runs pruned Bitcoin testnet4 and Monero stagenet nodes with the [default images](#node-image-versions), `custom` asks per coin); and per coin `--bitcoin` or `--monero` `local|disabled`, `--bitcoin-image`, `--bitcoin-network regtest|testnet4|signet`, `--bitcoin-prune yes|no`, and the Monero equivalents restricted to stagenet. For externally hosted nodes, use `--nodes none` and configure the separate `ESCROW_BTC_*` or `ESCROW_XMR_*` credentials afterwards; the installer does not create a pooled wallet or enable Bitcoin escrow by creating a node. `--uploads yes` also sets up the [malware scanner](#photo-uploads-and-the-malware-scanner) as `add-scanner` does (the default, `no`, leaves it out and asks nothing). `./scripts/install.sh --help` lists them. A question without its flag is asked. A value given as a flag, such as an RPC URL with a password, is visible to other local users while the installer runs; answer the question instead to keep it off the command line.

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

`APP_MODE` in `.env` (`clearnet` or `tor`, set by the installer) must be one of those two: any other value stops the server. It changes two behaviours: with `APP_MODE=tor` and [Bitcoin escrow](#bitcoin-escrow-test-networks-only) on, a settlement is broadcast only through an escrow node that sends through a proxy for both IPv4 and IPv6 on Bitcoin Core 31.1 or later (*Tor mode* under Bitcoin escrow, A-441); otherwise escrow broadcasts wait. With [Monero escrow](#monero-escrow-fakechain-or-stagenet) on, a settlement is relayed only when `COMPOSE_FILE` includes `compose.nodes-tor.yaml` or `ESCROW_XMR_TOR_RELAY=clearnet-accepted` (*Tor mode* under Monero escrow, A-515); otherwise Monero relays wait. It routes nothing itself. Routing the local nodes through Tor is [`compose.nodes-tor.yaml`](#local-nodes-over-tor), which `check` accepts only with `APP_MODE=tor`. The Compose overlay in `COMPOSE_FILE` decides exposure, and `COOKIE_SECURE` decides whether cookies are marked Secure. Compose passes `COMPOSE_FILE` to the app, which refuses to start when it disagrees with `APP_MODE`: `tor` needs `compose.tor.yaml` in it, `clearnet` needs `compose.clearnet.yaml`, and never both (a blank `COMPOSE_FILE` skips this check). `COOKIE_SECURE` must be exactly `true` or `false` (unset means `true`); `APP_MODE=tor` with `COOKIE_SECURE=true` only logs a warning: the onion is served over plain HTTP, so the Secure session cookie works only in browsers that treat `.onion` addresses as secure (the installer writes `false` for Tor).

Configure DNS, firewall and the certificate issuer for your domain. A proxy in another container needs a private shared network instead of this host-loopback configuration. Never expose the database or node RPC ports.

**External PostgreSQL and the database encoding.** The database must be UTF8. Startup checks `SHOW server_encoding` before it runs the first migration and, for any other encoding (`SQL_ASCII`, `LATIN1`), exits with `the database encoding is SQL_ASCII, not UTF8 ...` and changes nothing. The length limits (for example the 250-character image description), handle rules, case folding and sorting all assume characters; on `SQL_ASCII` a limit counts bytes, so 250 "é" is refused there and accepted on UTF8. The internal PostgreSQL of the Compose files is initialised with `--encoding=UTF8`. On an external server create the database like this (from `psql` as a role with `CREATEDB`; `TEMPLATE template0` is what lets you choose an encoding the server's default differs from):

```sql
CREATE DATABASE opsecmkt ENCODING 'UTF8' TEMPLATE template0;
```

Check an existing database with `export DATABASE_URL` and `python3 scripts/postgres-tool.py DATABASE_URL psql -c 'SHOW server_encoding'` (it must print `UTF8`; the tool keeps the URL's password off the command line, where other users of the host could read it), or `SELECT datname, pg_encoding_to_char(encoding) FROM pg_database;` for all of them. The encoding of a database cannot be changed in place: to move a non-UTF8 one, follow [UPGRADING.md section 42](../UPGRADING.md#42-database-encoding-must-be-utf8-a-481), which checks the old database for text that is not valid UTF-8 (`scripts/check-utf8.sql`), copies the text byte for byte and proves the new database's own backup restores before the switch. Every connection the app opens also sends `client_encoding=UTF8`, overriding the database and role defaults and any `client_encoding` in `DATABASE_URL`.

**External PostgreSQL and time zones.** Every database connection the app opens (the web app, its migrations, `-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors` and `-set-role`) sets `TimeZone=UTC` itself, overriding the server, database and role defaults and any `TimeZone` in `DATABASE_URL`. An external or host PostgreSQL left on local time therefore needs no change: pages show every time in UTC, labelled " UTC", and do not reveal the server's UTC offset. Other clients, such as `psql` or the backup scripts, still use the server's own zone.

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

It adds `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there, starts the mirror, waits up to 90 seconds for its address and prints both addresses and the command that backs up the mirror's keys. It refuses on a clearnet install, and running it again changes nothing. To do the same by hand, first add `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there: the installer's default Tor install has `COMPOSE_PROFILES='internal-db,bitcoin,monero'`, which becomes `COMPOSE_PROFILES='internal-db,bitcoin,monero,mirror'`; `COMPOSE_PROFILES='internal-db'` becomes `COMPOSE_PROFILES='internal-db,mirror'`, and an empty value becomes `COMPOSE_PROFILES='mirror'`. Then:

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

Listing photos and vendor and staff avatars need ClamAV's malware scanner, `clamd`, an optional service in [`compose.scanner.yaml`](../compose.scanner.yaml). The app stores an upload only after `clamd` has scanned the file it received and found nothing, and refuses uploads whenever it cannot ask. There is no setting that accepts uploads without it: with `CLAMD_ADDRESS` blank, uploads are off. Vendors (and administrators, on any listing) add up to 4 photos on each listing's edit page, under **Photos**, and vendors, moderators and administrators set an avatar on `/account`; each owner can remove their own there (war room A-188). Catalog, product and vendor pages show a published listing's photos and a vendor's avatar to anyone; staff avatars follow the staff list rules below (war room A-189). ClamAV checks uploads for known malware; it does not review picture content: staff take down pictures that break the market's rules (below). What an image row keeps and who can see it is in [What the server keeps](implementation-status.md#what-the-server-keeps), which `/canary#records` tells users.

What uploaders see follows the scanner's state on `/admin#image-scanner`: *Ready* shows the upload forms; *Unavailable* or *Signatures stale* keeps them on the page but disabled, with "Photo uploads are paused: …" and the reason (never the signature date); *Disabled* (no `CLAMD_ADDRESS`) replaces them with "Photo uploads are off on this market: they require the ClamAV malware scanner, which the operator has not set up." Listing details still save in every state, and stored photos and avatars can still be removed.

Moderators and administrators also choose on `/account` whether to **hide themselves from the public staff list**; everyone is hidden until they untick it, and each change is recorded in their activity log. A hidden staff member's avatar can be shown only to the buyer and vendor of orders whose dispute they decided (and to moderators and administrators). The public staff list itself is off until an administrator turns it on (war room A-189). Changing a moderator's role on `/admin` deletes their avatar, which was uploaded under these rules, tells them so and resets their choice to hidden; a vendor you make a moderator, or a former moderator you promote again, starts hidden (PR #28 review).

**Taking a photo down.** Moderators remove a vendor's listing photo or avatar that breaks the market's rules, and administrators any listing photo or avatar (staff avatars and administrators' own listings included), from the moderation desk's **Recent photos** (`/moderator#recent-photos`: the 50 newest images anyone can see; a moderator sees vendors' only), or from the *Remove photo N* and *Remove this avatar* forms on the product and vendor pages, with a reason of 10–500 characters. The image is deleted from the database at once (there is no hidden copy to restore; the vendor uploads another), the owner is notified with the reason, the vendor sees "Photo removed by staff: <reason>" on the listing's edit page, and both accounts' activity logs record it. Moderators can do nothing else to listings. Copies remain in backups made before the takedown: restoring one brings the image back (see [Backups and recovery](#backups-and-recovery)). Until PostgreSQL reuses the space, the deleted image's bytes also remain in PostgreSQL's data files and write-ahead log, readable by anyone with access to the database server's disk (the `postgres_data` volume for the internal database); the same holds for an owner's removal or an avatar replaced by a new one.

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

Every service has a memory, CPU and process ceiling, so a flood, a leak or a runaway sync in one container slows down or restarts that container instead of starving the host, and the database with it:

| Service | `mem_limit` | CPUs | `pids_limit` | Notes |
| --- | --- | --- | --- | --- |
| `app` | 512m | 2 | 512 | `GOMEMLIMIT=400MiB` makes Go's garbage collector work harder before the memory limit is reached; Go sizes its scheduler to the 2-CPU limit. |
| `db` | 1g | 2 | 256 | Internal database only. Every PostgreSQL connection is a process; 256 leaves room above the default 100 connections. |
| `tor`, `tor-mirror` | 256m | 1 | 64 | `MaxMemInQueues 128 MB` in `deploy/torrc` keeps Tor's queues inside this ([Onion-service defences](#onion-service-defences)). |
| `tor-socks` | 256m | 1 | 64 | The nodes' Tor client, only with [`compose.nodes-tor.yaml`](#local-nodes-over-tor); `MaxMemInQueues 128 MB` in `deploy/torrc-socks`. |
| `bitcoin` | 2g | 2 | 256 | A pruned test-network node: its database cache and mempool (at most 300 MB by default) plus headroom for the initial sync. Raise it for an unpruned node or a larger `-dbcache`. |
| `monero` | 2g | 2 | 512 | monerod starts threads per core for sync and verification. |
| `monero-escrow-wallet` | 1g | 1 | 128 | Dedicated Monero escrow wallet-rpc, only with its profile. |
| `escrow-signer` | 128m | 1 | 64 | Dedicated Bitcoin escrow signer, only with its profile. |
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

To change a limit, leave the tracked Compose files alone: put the services you change in a file of your own, for example `compose.local.yaml` beside them, and add it at the end of `COMPOSE_FILE` in `.env` (for example `COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.local.yaml'`). Then run `./scripts/install.sh check` and `docker compose up -d`. Change CPUs with `cpu_quota`; adding `cpus` next to it makes Docker refuse the container. For a busier market with an unpruned Bitcoin node:

```yaml
services:
  app:
    mem_limit: 1g
    environment:
      GOMEMLIMIT: 800MiB   # about 80% of mem_limit
  bitcoin:
    mem_limit: 4g
    cpu_quota: 400000      # 4 CPUs
```

## Optional cryptocurrency nodes

The installer can run local Bitcoin and Monero **daemon** containers, or leave them disabled. Local nodes are infrastructure only: adding a node does not turn payments on. Bitcoin escrow is off unless `ESCROW_BTC=on` is configured after its escrow RPC users and wallets are provisioned; it supports regtest, signet and testnet4. Monero escrow is off unless `ESCROW_XMR=on` is configured with its separate wallet-rpc, seed key and secret; the bundled node setup supports stagenet. Mainnet and Monero testnet are not supported for escrow, and this build refuses mainnet configurations.

The installer defaults to [`bitcoin/bitcoin:31.1`](https://hub.docker.com/r/bitcoin/bitcoin) and Seth for Privacy's [`simple-monerod:v0.18.5.1`](https://github.com/sethforprivacy/simple-monerod-docker) (see [Node image versions](#node-image-versions)). The separate [`simple-monero-wallet-rpc:v0.18.5.1`](https://github.com/sethforprivacy/simple-monero-wallet-rpc-docker) image is used only for Monero escrow. Press Enter to use the pinned defaults or supply an image override. The Bitcoin image is community-published and explicitly unofficial; it verifies upstream Bitcoin Core binaries. The Monero images are community-maintained, not published by the Monero project. Review publisher and image provenance before use. A tag can be re-pushed by its publisher; append an immutable `@sha256:<64 lowercase hex characters>` digest in `.env` for a reproducible deployment.

Local nodes store their chains in dedicated volumes and publish no ports. Even pruned, they need several GB of disk and hours for the first synchronization (sizes below). Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon alone is not a wallet RPC. For externally hosted escrow nodes, configure the `ESCROW_BTC_*` or `ESCROW_XMR_*` settings manually; the installer does not provision external-node credentials. The obsolete pooled settings (`BITCOIN_RPC_URL`, `BITCOIN_WALLET`, `MONERO_RPC_URL` and `MONERO_WALLET_RPC_URL`) are refused, not used for escrow.

### Node image versions

The bundled defaults are release tags, never `latest`: `bitcoin/bitcoin:31.1` (Bitcoin Core v31.1), `ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1` and, for the Monero escrow wallet-rpc, `ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1` (Monero v0.18.5.1). `MONERO_WALLET_IMAGE` overrides the escrow wallet-rpc image; it is not a pooled payment provider. The Bitcoin node and Monero daemon serve escrow only when the corresponding escrow configuration is enabled. The Bitcoin image is community-published and explicitly unofficial; the Monero images are community-maintained. Review publisher and image provenance before use.

`BITCOIN_IMAGE`, `MONERO_IMAGE` and `MONERO_WALLET_IMAGE` in `.env` override the defaults. Keep release tags pinned (optionally append `@sha256:<digest>`); do not use `:latest`. CI checks the Compose defaults but cannot see a deployment's `.env`. When changing a node image, review its release provenance and verify its RPC behavior against the supported escrow setup. Run the relevant escrow regression (`scripts/test-escrow-regtest.sh`, `scripts/test-escrow-xmr-regtest.sh`) and configuration tests before adopting it. Public-network claims remain limited to the frozen evidence records linked from the Bitcoin and Monero escrow sections; do not infer other journey coverage from a node version change.

Before changing a tag (default or `.env`):

1. Review the publisher, release and immutable digest; keep the Bitcoin daemon, Monero daemon and escrow wallet-rpc tags pinned to reviewed releases.
2. Change the corresponding defaults in `compose.nodes.yaml`, `.env.example` and `scripts/install.sh` together, then run installer and Compose configuration tests.
3. For Bitcoin, run `scripts/escrow-node-setup.sh --chain regtest --verify` and `scripts/test-escrow-regtest.sh`; for Monero, run `scripts/test-escrow-xmr-regtest.sh`. These local regression checks do not create new public-network evidence. `scripts/test-escrow-public.sh` contacts public nodes; run it only as a separately scoped public-network check and do not treat a node image change as evidence for an untested journey.

**Runtime wallet check.** The former pooled-payment runtime check for Monero payout error codes was removed with pooled payments. Current Monero escrow readiness checks the daemon network and hard fork, the escrow wallet-rpc's attachment and height, the database's pinned wallet secret, and the seed-key fingerprint; it does not classify payout errors or authorize pooled sends.

### Local node pruning

Local nodes are **pruned by default**; a full node is an explicit opt-in per coin. Bitcoin escrow uses chain state and wallet RPCs of its dedicated node; Monero escrow uses the daemon's restricted `get_info` and a separate escrow wallet-rpc. Historical pooled payments no longer run. Nothing in the application reads old blocks, though a wallet restore may need blocks older than a pruned node retains (see the restore trade-off below).

| `.env` key | Default | Full node |
| --- | --- | --- |
| `BITCOIN_PRUNE_MB` | `2000` (blank also means 2000): `bitcoind -prune=2000` | `0` (`-prune=0` disables pruning) |
| `MONERO_PRUNE_FLAGS` | `'--prune-blockchain --sync-pruned-blocks'` (also used when the key is absent) | blank: `MONERO_PRUNE_FLAGS=''` |

Put any comment on its own line in `.env`. Compose strips a ` # comment` after a value, but not after an unquoted blank value: it reads `MONERO_PRUNE_FLAGS= # full node` as the value `# full node`, which monerod receives as the arguments `#`, `full` and `node`, and `BITCOIN_PRUNE_MB= # note` as `-prune=# note`. Quoted blanks (`MONERO_PRUNE_FLAGS=''`) are safe, and the installer and `.env.example` never write a comment on a value line.

`-prune=<n>` keeps about `n` MiB of the most recent block and undo files and deletes older ones. Use `0` (no pruning) or at least 550: bitcoind refuses to start with 2 to 549, and `1` means manual pruning only, which this deployment does not do. The last 288 blocks are always kept ([`src/init.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/init.cpp), [`src/node/blockmanager_args.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/node/blockmanager_args.cpp), [`src/validation.h`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/validation.h), Bitcoin Core v31.1). The 2000 MiB default is about four times the minimum, so that older wallet backups can still be loaded (see the restore trade-off below) for about 1.5 GB more disk. How many days of blocks that is depends on block sizes on your chain (not measured here); check it with `getblockchaininfo`, whose `pruneheight` is the oldest block still held. `--prune-blockchain` keeps one eighth of the prunable transaction data (ring signatures and proofs) plus everything in the last 5500 blocks ([`cryptonote_config.h`](https://github.com/monero-project/monero/blob/v0.18.5.1/src/cryptonote_config.h), monerod v0.18.5.1), and `--sync-pruned-blocks` lets a pruned monerod download already-pruned blocks instead of full ones ([`cryptonote_protocol_handler.inl`](https://github.com/monero-project/monero/blob/v0.18.5.1/src/cryptonote_protocol/cryptonote_protocol_handler.inl)). monerod has no `--prune-blockchain=false`, which is why the Monero setting is the flag list itself.

**Disk sizing (estimates).** The Bitcoin figures are the estimates Bitcoin Core v31.1 ships in [`src/kernel/chainparams.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/kernel/chainparams.cpp) (`m_assumed_blockchain_size`, `m_assumed_chain_state_size`); both chains keep growing. Upstream publishes no Monero stagenet or testnet size; the ratio is the [Monero README](https://github.com/monero-project/monero/blob/v0.18.5.1/README.md#pruning)'s mainnet example (130 GB full, 45 GB pruned in April 2022).

| Chain | Full node | Pruned (default) |
| --- | --- | --- |
| Bitcoin testnet4 | about 31 GB of blocks + 2 GB chain state | about 2 GB of blocks + 2 GB chain state; budget 5 GB |
| Bitcoin signet | about 24 GB of blocks + 4 GB chain state | about 2 GB of blocks + 4 GB chain state; budget 8 GB |
| Monero stagenet/testnet | not published upstream (UNVERIFIED) | about a third of the full size; reserve 20 GB (UNVERIFIED estimate) |

A pruned bitcoind still downloads and validates the whole chain once; it only discards old blocks as it goes, so the first sync takes as long as a full node's. A pruned monerod started from an empty volume downloads about a third of the chain ([Moneropedia: Pruning](https://www.getmonero.org/resources/moneropedia/pruning.html)).

**Choosing a full node.** Answer `no` to the installer's "Prune the … node" question, or later set the key in `.env` as in the table and recreate the node with `docker compose up -d bitcoin` or `docker compose up -d monero`. Switching an existing node:

- **Bitcoin, full to pruned:** bitcoind prunes the existing block files down to the target when it next starts ("initial blockstore prune" in `src/init.cpp`). Freed space is available at once. Any wallet backup older than the new `pruneheight` then needs the full chain to be restored (below).
- **Bitcoin, pruned to full:** once blocks have been deleted, bitcoind refuses to start with `-prune=0` and says "You need to rebuild the database using -reindex to go back to unpruned mode. This will redownload the entire blockchain" ([`src/node/chainstate.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/node/chainstate.cpp)). Add `reindex=1` to the node's `bitcoin.conf` (every command-line option may be set there: [`doc/bitcoin-conf.md`](https://github.com/bitcoin/bitcoin/blob/v31.1/doc/bitcoin-conf.md)), set `BITCOIN_PRUNE_MB='0'`, and recreate the node; this works whether or not the node is running:

  ```sh
  docker compose run --rm --no-deps --entrypoint sh bitcoin -c 'echo reindex=1 >> /data/bitcoin.conf'
  # set BITCOIN_PRUNE_MB='0' in .env, then:
  docker compose up -d bitcoin
  docker compose logs -f bitcoin    # wait for "Reindexing finished", then remove the line again:
  docker compose exec -T bitcoin sed -i '/^reindex=1$/d' /data/bitcoin.conf
  ```

  Leaving the line in place makes every restart reindex (bitcoind warns about it at startup). The node then downloads the whole chain again.
- **Monero, full to pruned:** `--prune-blockchain` prunes an existing database in place when monerod starts ("Pruning blockchain..." in the log), but the database file does not shrink: the freed pages are reused as the chain grows ([Moneropedia](https://www.getmonero.org/resources/moneropedia/pruning.html)), and the [README](https://github.com/monero-project/monero/blob/v0.18.5.1/README.md#pruning) warns that pruning an existing chain temporarily needs space for both. To get the space back, stop monerod, remove the `monero_data` volume (the chain only; the wallet is in `monero_wallet`) and let it sync again pruned. `monero-blockchain-prune`, which writes a new pruned copy, is not in the default image, which ships only `monerod`.
- **Monero, pruned to full:** blanking `MONERO_PRUNE_FLAGS` does **not** unprune an existing database. The database records that it is pruned and monerod keeps pruning each new batch of blocks whatever the flags ([`db_lmdb.cpp`](https://github.com/monero-project/monero/blob/v0.18.5.1/src/blockchain_db/lmdb/db_lmdb.cpp) `prune_worker`, [`blockchain.cpp`](https://github.com/monero-project/monero/blob/v0.18.5.1/src/cryptonote_core/blockchain.cpp) `update_blockchain_pruning`). Stop monerod, remove the `monero_data` volume and let it sync again with the flags blank.

The effects on an already-synced chain above come from reading the upstream sources; they have not been rehearsed on a synced test chain here (UNVERIFIED). Rehearsed on fresh volumes: `getblockchaininfo` reports `"pruned": true` with a 2000 MiB target on signet; `reindex=1` written to `/data/bitcoin.conf` (with `docker compose exec`) is honoured with `BITCOIN_PRUNE_MB=0`; monerod v0.18.5.1 on stagenet logs "Pruning blockchain..." and a blank `MONERO_PRUNE_FLAGS` starts it without either flag.

**Wallet restore trade-off.** A Bitcoin wallet can only be loaded or rescanned over blocks the node still holds. Restoring a backup (`restorewallet`) whose last synchronised block is older than the node's `pruneheight` fails with "Prune: last wallet synchronisation goes beyond pruned data. You need to -reindex (download the whole blockchain again in case of a pruned node)" ([`src/wallet/wallet.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/wallet/wallet.cpp)), and `rescanblockchain` cannot go below `pruneheight` either (UNVERIFIED here: needs a synced, pruned chain). Record the block height next to each wallet backup ([runbook](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)) and compare it with `pruneheight` before relying on the backup. To restore an older one, either restore the whole `bitcoin_data` volume from a copy taken with bitcoind stopped (the pruned chain and the wallet in it then agree, and the node catches up from there), or run the node unpruned as above until the wallet has loaded and rescanned, then set `BITCOIN_PRUNE_MB` back. Monero is not affected: a pruned blockchain "is otherwise identical in functionality to the full blockchain" ([README](https://github.com/monero-project/monero/blob/v0.18.5.1/README.md#pruning)) and keeps the full transaction history, so `monero-wallet-rpc` can restore and refresh a wallet through a pruned daemon.

**Mirrors, development and CI stay pruned.** The `mirror` profile runs a second Tor gateway, not a node. Keep the pruned defaults on a Tor-mirror deployment, a secondary or test instance, and any development or CI configuration: `.env.example`, the Compose defaults and CI's Compose check all use them. Choose a full node only where you want to keep the whole chain for your own reasons. The regtest and offline stagenet chains used by the [local chain tests](local-chain-testing.md) are tiny and are left unpruned.

### Local nodes over Tor

`compose.nodes-tor.yaml` (A-442) sends the local nodes' peer traffic through a Tor client of their own. It is for `APP_MODE=tor` only, and does nothing for an external node.

**Turning it on.** Add it at the end of `COMPOSE_FILE` in `.env`, after `compose.nodes.yaml`, for example `COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml:compose.nodes-tor.yaml'`, run `./scripts/install.sh check`, then `docker compose up -d --build`. `check` refuses it without `APP_MODE=tor` and `compose.tor.yaml`, and before `compose.nodes.yaml` (whose node commands would then replace the overlay's and drop the proxy). The installer never writes it. Adding or removing it changes the `node_egress` network: Docker Compose 5.1.1 recreated the network, restarting the containers on it, during `up -d`; if your Compose reports instead that the network's configuration differs, run `docker compose down` (volumes are kept) and `up -d`. To turn it off, remove it from `COMPOSE_FILE` and run `docker compose up -d --remove-orphans`, which also removes `tor-socks`.

**What it changes.**

- **`tor-socks`**, run whenever the `bitcoin` or `monero` profile is: the onion container's pinned Tor build ([Tor image](#tor-image)) with its own [`deploy/torrc-socks`](../deploy/torrc-socks): one SOCKS port on its own address on the node network, `10.199.42.2:9050`, and a `SocksPolicy` that accepts that network (`10.199.42.0/28`) except its gateway (`10.199.42.1`, the host) and refuses everything else; no onion service, `ControlPort 0`. Read-only root, no capabilities, no new privileges, no published port, 256m, 1 CPU and 64 processes. It is healthy once its SOCKS port accepts connections, which does not mean Tor has built a circuit (its log says `Bootstrapped 100%` when it has). Its guards and directory cache stay in the `tor_socks_data` volume. It is a separate image: when you rebuild `tor`, run `docker compose build tor-socks` too. The onion containers (`tor`, `tor-mirror`, [`deploy/torrc`](../deploy/torrc)) are unchanged and keep `SocksPort 0`.
- **`node_egress` becomes internal**, on that pinned subnet: `bitcoin` and `monero` lose their direct route out and reach peers only through `tor-socks`, the one service on its own egress network (`tor_socks_egress`). The app, the onion containers and the wallets are not on `node_egress` and cannot reach `tor-socks`. If `10.199.42.0/28` is already in use on the host, change it in `compose.nodes-tor.yaml` and `deploy/torrc-socks` together (`tests/test_tor_deploy.py` checks that they agree).
- **bitcoind** runs with `-proxy=tor-socks:9050 -privatebroadcast`, never `-connect` or `-onlynet` (they stall its sync). With `-proxy`, Bitcoin Core also turns off listening, discovery and NAT-PMP, so the node takes no inbound peers, and asks its DNS seeds through the proxy by name instead of looking them up itself.
- **monerod** runs with `--proxy=10.199.42.2:9050` (`monerod --help` documents an IP address and port, not a host name). **Unverified:** monerod has not run through Tor here. With no network at all, v0.18.5.1 accepted the flag and addressed every peer attempt (its built-in stagenet seeds) to that proxy. `--tx-proxy`, which keeps local transactions for onion peers, is not set and untested.

**What is private, and what is not.**

- **Escrow settlements** on the local `bitcoin` node: `-privatebroadcast` makes `sendrawtransaction` send a transaction only over short-lived connections through the proxy, and with `APP_MODE=tor` the node passes the [Tor-mode gate](#bitcoin-escrow-test-networks-only) (`getnetworkinfo` shows the proxy for IPv4, IPv6 and onion). On Bitcoin Core 31.1 regtest through a SOCKS stand-in, private broadcast tried onion addresses only: with none known, or with only an IPv4 address known, it returned the txid, kept the transaction out of its own mempool and its peers', listed it in `getprivatebroadcastinfo` with no peers and kept retrying, and it did not use the IPv4 peer it was already connected to through the proxy. That local probe did not establish an onion-only destination rule on public chains. The 2026-10-09 public signet run observed both settlements in the private-broadcast queue, an onion acknowledgement for the refund, a private peer send for the claim, and both settlements confirmed ([record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)). Every node network was configured through Tor; the ten-second sampler did not capture a claim acknowledgement. [Core 31 release notes](https://bitcoincore.org/en/releases/31.0/) describe separate private-broadcast connections over Tor or I2P. Until delivery, the stall alert reports the settlement as *Queued or stuck*.
- **The Bitcoin node's peer traffic** (block sync, address discovery, relaying other transactions) goes through Tor, so peers see a Tor exit instead of the host's IP address (on regtest, the other node saw the stand-in's address as its peer, not the node's).
- **Wallet sends are never private**, with or without the overlay. A transaction sent with `sendtoaddress` (which the removed pooled payouts used and no current path does) is relayed over Core's ordinary long-lived peer connections, not by private broadcast, so a node's peers can link such sends to one another. Bitcoin escrow settlements go out with `sendrawtransaction`, which `-privatebroadcast` covers (see *Tor mode* under [Bitcoin escrow](#bitcoin-escrow-test-networks-only), A-441).
- **Monero:** unverified (above); treat its traffic as not private. With `APP_MODE=tor` and Monero escrow on, settlements are relayed only with this overlay or `ESCROW_XMR_TOR_RELAY=clearnet-accepted` ([*Tor mode* under Monero escrow](#monero-escrow-fakechain-or-stagenet), A-515).

Earlier local mechanics evidence: the resolved Compose configuration (`tests/test_tor_deploy.py`, CI's Compose job); `deploy/torrc-socks` under Debian's tor 0.4.9.11 on internal networks only (it listened only on `10.199.42.2:9050`, accepted a node, refused the gateway and was unreachable from the app's network); Bitcoin Core 31.1 regtest nodes through a SOCKS stand-in for Tor. The later [2026-10-09 public signet record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md) adds real Tor delivery on a separately provisioned internal-network node with the same proxy/privatebroadcast flags; it is not a deployed-site rehearsal or a complete live Compose deployment test.

## Migrations

`internal/market/schema.sql` is the frozen baseline. Startup applies it, then every `internal/market/migrations/NNN_name.sql` not yet listed in `schema_migrations`, in numeric order, inside one transaction under an advisory lock, so concurrent app starts are safe and a failed migration changes nothing. A version missing from the table is applied even when higher versions already are. A version in the table that the running server does not include means a newer release migrated the database: the server then exits with an error naming it, before changing or serving anything (see [Rolling back](../UPGRADING.md#5-rolling-back)). Never edit a merged migration; add a new file. Reserved blocks: 001–009 Foundation, 010–019 authentication, 020–029 PGP, 030–039 orders, 040–049 inventory, 050–059 payments, 060–069 transparency; 070–106 contain later payments, privacy, escrow, operations and receipt migrations. Migration 106 records whether a dynamic receipt originally required password confirmation; existing receipts default to false and retain their fixed-route checks. Migration 099 adds indexes for account-filtered and global audit-event ordering; migration 100 adds short-lived form-submission receipts; migration 101 persists provider-alert episode state and delivery deduplication receipts. Take an encrypted backup before upgrading; there are no automatic down-migrations.

Startup runs in separately bounded phases, each set by an optional Go duration in `.env` (blank uses the default):

- `DATABASE_CONNECT_TIMEOUT` (default `15s`, from 1s to 10m): reaching PostgreSQL.
- `MIGRATION_LOCK_TIMEOUT` (default `10m`, from 1s to 24h): waiting for the migration lock while another app instance migrates, or completes first-run setup on, the same database.
- `MIGRATION_TIMEOUT` (default `10m`, from 1s to 24h): applying the baseline and pending migrations. An upgrade whose migrations rewrite large tables can need longer; raise it before deploying.
- Payment provider startup (connecting to the configured wallets and nodes) keeps a fixed 15-second bound; a provider that answers with an error is shown as unavailable and retried, as described under [Payments](#payments).

When a phase runs out of time the server exits with an error naming it and the setting to raise, for example `database migration timed out after 10m0s and was rolled back; nothing was changed. Raise MIGRATION_TIMEOUT ...`. A timed-out lock wait or migration is rolled back and its database session ended before the process exits, so the next start begins from the unchanged database with the lock free. An invalid value stops startup with a message naming the variable and its accepted range.

The application's pool opens with fixed per-connection limits (startup parameters, set by the app, not in PostgreSQL's configuration): `statement_timeout` 8 s, `lock_timeout` 3 s and `idle_in_transaction_session_timeout` 15 s, so one slow query or held row lock cannot tie up the pool for longer. A payment request, and whatever may queue behind one (a checkout, listing edit, archive or takedown, a cancellation returning stock, a payment-intake change, and an administrator's suspension, second-factor reset or role change), waits up to 11 s for a row lock instead (its statement limit is raised to 11 s with it), because the payment request holds its rows while the escrow node is asked about the escrow address (up to 10 s). Each account has at most one payment request in progress at a time; another is refused at once with 429 "Your previous request is still being processed" and `Retry-After: 5`. Startup opens this pool and checks that the database accepts it **before** migrating, so a database or pooler that refuses these parameters stops startup with nothing changed (see [Connection poolers](#connection-poolers)). Migrations then run on their own database connection without the limits; the migration transaction also lifts any that are set on the database role. `-status`, `-bootstrap-admin`, `-reset-admin-password`, `-set-role`, `scripts/backup.sh`, `scripts/restore.sh` and `psql` sessions do not have them either. A page or form stopped by one answers 503 ("The market is busy") and logs cause `57014` (statement) or `55P03` (lock); see [Troubleshooting](#troubleshooting).

### Connection poolers

An external database reached through a connection pooler such as PgBouncer, or a hosted provider's pooler, may refuse startup parameters it does not know. Startup then exits with `database refused the application's session settings (SQLSTATE 08P01): ...` (the code depends on the pooler) before any migration runs. Either:

- connect `DATABASE_URL` to PostgreSQL directly, or to the provider's direct (session) endpoint; or
- let the pooler drop the parameters and set them on the application's role instead. In PgBouncer, add them to `ignore_startup_parameters` (a comma-separated list; keep any entries already there):

  ```ini
  ignore_startup_parameters = statement_timeout,lock_timeout,idle_in_transaction_session_timeout
  ```

  PgBouncer then discards them, so the limits apply only once they are set for the role the app connects as (`opsecmkt` below; use the user in your `DATABASE_URL`). They take effect for new sessions:

  ```sql
  ALTER ROLE opsecmkt SET statement_timeout = '8s';
  ALTER ROLE opsecmkt SET lock_timeout = '3s';
  ALTER ROLE opsecmkt SET idle_in_transaction_session_timeout = '15s';
  ```

  The migration transaction lifts these itself, and `pg_dump` and the restore script set their own. `-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors` and `-set-role` then run under them too, which their short transactions do not notice. Without the role settings the app runs with no statement or lock limit, as before A-177.

The pool also sets `TimeZone` to UTC as a startup parameter; PgBouncer passes `TimeZone` through by itself.

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

All keys below are listed in `.env.example` and passed by `compose.yaml`. Blank or absent values keep a feature disabled.

### Authentication

No environment keys. Second-factor secrets are encrypted with a key derived from `APP_SECRET_KEY` (on an install without it, from `SETUP_TOKEN`: see [Secrets](#secrets-env-and-onion-identities)); changing that key makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off (the sign-in's *I lost my authenticator* checkbox does both at once) and enroll again (an administrator resets a non-administrator with no recovery code left under *Admin → Reset a user's second factors*; an administrator with no recovery code left needs the host command in *Administrator lockout* below). Startup refuses a placeholder or obviously non-random `APP_SECRET_KEY` or `SETUP_TOKEN`; use `openssl rand -hex 32`.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code and the account password, and 10 one-time recovery codes are shown once. Turning TOTP or PGP sign-in on or off, replacing recovery codes and adding, changing or removing the PGP key all need the account password (and a current code when TOTP is on), and each notifies the account; so do sign-ins with a recovery code and the first failed second step after a correct password. A user who reports such a notification they did not cause should change their password at once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **Lost authenticator**: a user (including an administrator) with an unused recovery code recovers without staff. On the sign-in's *Use a recovery code instead* form, they tick *I lost my authenticator — turn two-step sign-in off*: the code signs them in and TOTP is turned off in the same step (the other recovery codes are deleted; PGP sign-in, if on, stays on), then they set up a new authenticator on `/totp`. Without the checkbox, a recovery-code sign-in lands on `/totp` showing how many codes remain, where TOTP can still be turned off with the password and another recovery code. Only a user with no recovery code left needs the *Second-factor reset* below (or, for an administrator, the operator procedure).
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio alternative, so turning it off may be needed for users who cannot read it; the form's "Can’t read the image?" help shows the staff contact (below). First-run setup never shows it. Showing a challenge writes nothing to the database; the id of each answered challenge is kept in `captcha_spent` until it expires (at most 10 minutes) so it cannot be reused. A challenge shown before an upgrade to this version is refused after it; the user reloads the page.
- **Sign-in pauses**: after 10 failed passwords for one handle within ten minutes, sign-in for that handle is refused (HTTP 429) until that window ends, at most 10 minutes, **even with the right password**; the page says sign-in for this handle is paused, that someone else may have caused it and that the password may be fine. Correct sign-ins, registrations and setup attempts do not count. The limit is per handle because there is no per-client signal over Tor, so **anyone who knows a handle can trigger it, including for your administrator account**, and keep it paused by repeating 10 wrong guesses every ten minutes: each guess needs a solved CAPTCHA while CAPTCHA is on, and costs nothing when it is off. While any handle is paused, the app logs `sign-in limiter refusing handles=N` (a count, never a handle) at most once a minute, when a sign-in fails or is refused; watch for it with `docker compose logs app | grep 'sign-in limiter'`. Break-glass: the limits are held in memory, so restarting the app (`docker compose restart app`) clears every pause; sign in immediately afterwards, before an attacker can spend the budget again. Other users' pauses are cleared too, and a restart does not stop the attacker from repeating it.
  The administrator panel lists currently paused handles for this application instance only; the list is in memory and can include handles with no account. When there is exactly one administrator account (suspended administrators still count) and it has active TOTP, that administrator can enter their current password and six-digit authenticator code together on `/login` during a pause. CAPTCHA and the bounded password-check slot still apply; the per-account TOTP budget is charged only after a correct password, and the code’s replay guard and session commit together. Other accounts use the existing sign-in and operator recovery paths. New registrations, setup and CLI bootstrap refuse case-insensitive handle collisions; a non-unique index lets legacy case variants coexist. Sign-in resolves an exact handle first, then a case-insensitive match only when unique. Staff lookup and host role/reset commands still require the exact handle.

- **Password-confirmation pauses**: every action confirmed with the account password (resolving disputes, payment reviews, admin settings, key, TOTP and password changes) shares one per-account limit of 10 failed confirmations per ten minutes: a **wrong** password, or a wrong authenticator or recovery code after the right one. After that the account's confirmations are refused (HTTP 429, "Too many failed password confirmations. Try again in ten minutes.") until that window ends, even with the right password. Correct confirmations do not count, so staff can resolve any number of disputes in a row. Only someone signed in to the account can spend it; restarting the app clears it, as for sign-in pauses.
- **Password change**: users change their own password on *Account → Change password* with the current password (plus an authenticator code or an unused recovery code when TOTP is on). Every other session and pending sign-in of that account ends. There is no email or administrator password reset: a user who forgets their password cannot be recovered through the application.
- **Second-factor reset**: an administrator can turn off TOTP (deleting its secret and recovery codes) and PGP sign-in for a non-administrator account under *Admin → Reset a user's second factors* by entering its handle (exact, case-sensitive; any account, not only those in the list of up to 100 shown there), confirmed with the administrator's own password (and authenticator code when enrolled). The account's PGP key and verification stay; its sessions and pending sign-ins end, both accounts get an audit row and the user is notified. Anyone who knows that account's password can then sign in, so confirm the request really comes from its owner (for example with a message signed by their verified PGP key) before resetting.
- **Unreadable PGP key**: an account with PGP sign-in on whose saved key can no longer be read (for example one saved before this market refused keys like it) is never signed in with the password alone. With TOTP on it is asked for its authenticator code or a recovery code; otherwise the sign-in is refused, after the correct password, with a message that names the problem, the second-factor reset and the staff contact, and the owner is notified once. Such a user needs the *Second-factor reset* above (an administrator, *Administrator lockout* below); confirm who is asking first, since the key that would prove it is the one that cannot be read. After the reset they sign in with the password, paste a current export of their public key and prove ownership again.
- **Account suspension**: to contain a compromised or abusive account, an administrator enters its handle under *Admin → Suspend or restore an account*, chooses *Suspend* and confirms with their own password (and authenticator code when enrolled). Its sessions and pending sign-ins end at once and it cannot sign in until restored: after a correct password it is told "This account is suspended." followed by the staff contact (below) Its role, listings and orders are unchanged (change the role to buyer as well if a vendor's listings should be archived), but orders waiting on this account stop until it is restored; the other party can still complete, cancel where allowed, or open a dispute. A suspended moderator does not count as a dispute resolver: it is not notified of disputes or payment reviews and is not offered as a dispute contact, so if it was the only moderator who is not a party to a disputed order, that dispute is treated as having no eligible resolver until you restore it or another moderator exists. The pooled build also held the account's unsent payouts; none exist now (migration 108), so suspension and restore touch only the account, its sessions and its notices. The panel lists suspended accounts; *Restore* with the same form allows sign-in again. Both changes are audited on both accounts and the user is notified. Administrator accounts, including your own, cannot be suspended there: to contain one, remove its administrator role from the host first ([Administrators](#administrators)).
- **Staff contact**: set *Admin → Staff contact* to how people reach the staff — a handle, onion address or XMPP address, at most 200 characters on one line — confirmed with your own password (and authenticator code when enrolled); the old and new values are audited. Anyone can read it, signed in or not: it is shown on the Transparency page (`/canary`), to a suspended account after its correct password, on `/challenge` for someone who lost their second factors, beside the CAPTCHA, in the expired- and unreadable-PGP-key sign-in refusals, to a vendor whose listing an administrator took down, with a photo the malware scanner refused, on the two-factor page after a sign-in with the last recovery code, and under notifications that tell people to contact the market staff (such as the sign-in and security notices). It is always plain text; if it is exactly the handle of a moderator or administrator who is not suspended and whose PGP key can receive encrypted messages, signed-in users also get a link to message that handle. While it is empty those places say "Ask the operator of this market." — so set it soon after installation. Save it empty to remove it.
- **Administrator lockout** (lost second factors) is not recoverable in the application: the web reset refuses the administrator's own account and other administrators. Keep the administrator's recovery codes offline: with one left, the administrator recovers alone at sign-in (*Lost authenticator* above). Without one, and also after a key rotation left the stored TOTP secret unreadable, an operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-factors ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL` (the internal or an external database), starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands, it turns off TOTP (its secret, any unconfirmed enrollment and the recovery codes, including any not yet shown) and PGP sign-in (the PGP key and its verification stay), ends every session and pending sign-in of that account, writes the audit row "Second factors reset by host command: … turned off; ended N session(s) and any pending sign-ins", notifies the account and prints a one-line summary. The administrator then signs in with the password alone and enrolls TOTP (or PGP sign-in) again. It refuses, changing nothing: an account that is not an administrator (reset those on the web under *Admin → Reset a user's second factors*, above), an unknown handle, an administrator with no second factor enrolled, and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password` below. The password is not changed: if it is lost too, also run `-reset-admin-password`.
- **First administrator from the host** (instead of entering `SETUP_TOKEN` in the browser on `/setup`; `./scripts/install.sh init --admin ADMIN_HANDLE` does this during installation): an operator on the host, in the deployment directory (with its `.env`), after the app has started once so the database schema exists, runs (replace `ADMIN_HANDLE`: 3–32 letters, digits or underscores, exact and case-sensitive; `-site-name` is optional):

  ```sh
  docker compose run --rm app -bootstrap-admin ADMIN_HANDLE -site-name "My Market"
  ```

  It connects with `DATABASE_URL`, starts no web server and needs no setup token. It prompts twice for the administrator's password without showing it (12–72 bytes, as on `/setup`; valid UTF-8 without a NUL byte, as the sign-in form sends, so a terminal set to `LATIN1` is refused) and then, in one transaction under the same lock as `/setup` (so it cannot race the browser form or another run), creates that one administrator, records the marketplace name (default `OPSMKT`, at most 80 characters; change it later on `/admin`), marks the market installed and writes the same audit row as `/setup` ("Account created: admin"). It prints only the handle; sign in on `/login` with that handle and password. From then on `/setup` refuses every token. It refuses, changing nothing, when the market is already set up (by `/setup` or an earlier run), a handle, password or name that breaks the rules above, a database the server has never started against, and a database upgraded by a newer release. Arguments go after the service name, as for `-reset-admin-password` below. Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input (`docker compose run --rm -T app -bootstrap-admin ADMIN_HANDLE`); it is never accepted as an argument. Anyone who can run this command with the deployment's `.env` can create the first administrator, just as they could read `SETUP_TOKEN` from it; there is no web equivalent.
- **Forgotten administrator password**: the application has no web password reset. An operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-password ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL`, starts no web server, prompts twice for the new password without showing it (12–72 bytes, the registration rule; valid UTF-8 without a NUL byte) and, in one transaction, stores its hash, ends every session and pending sign-in of that account and writes the audit row "Password reset by the operator on the host". It refuses an account that is not an administrator, an unknown handle, and a database upgraded by a newer release (as startup does); nothing changes then. Arguments go after the service name because the image's entrypoint is the server itself (do not repeat `/app/server`). Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input; it is never accepted as an argument. The site can keep running. Second factors are unchanged: if they are lost too, also run `-reset-admin-factors` (above).

### Administrators

The web page never adds, removes or changes an administrator: *Admin → Assign an account role* offers only buyer, vendor and moderator and refuses administrators' handles, and suspension and the second-factor reset refuse administrators too. Administrators are managed from the host with `-set-role`, by an operator in the deployment directory (with its `.env`). Replace `HANDLE` (exact and case-sensitive) and `ROLE` (`buyer`, `vendor`, `moderator` or `admin`):

```sh
docker compose run --rm app -set-role HANDLE ROLE
```

It connects with `DATABASE_URL`, starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands (so two runs happen one after the other), it changes the role, writes the audit row "Role changed from OLD to NEW by host command" on the account and prints a one-line summary. It does what *Assign an account role* does for the new role: an account that stops being a vendor has its active listings archived (audited as "Archived N active listing(s): role changed to NEW by host command"), and an administrator or moderator who stops being staff loses their staff avatar and goes back to hidden on the public staff list. Removing an administrator (any role other than `admin`) also ends every session and pending sign-in of that account. It refuses, changing nothing: an unknown handle, a role other than the four above, the role the account already has, removing the last administrator that is not suspended, making a suspended account an administrator (restore it on the web first), and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password`.

- **Add an administrator**: the person registers an ordinary account on `/register` and, preferably, sets up TOTP and saves the recovery codes. Confirm out of band that the handle is theirs, then run `docker compose run --rm app -set-role HANDLE admin`. Their open session becomes an administrator's at once. A vendor made an administrator has its listings archived, as for a moderator.
- **Remove an administrator**: `docker compose run --rm app -set-role HANDLE buyer` (or `moderator`, or `vendor`). The account is signed out everywhere and signs in again with the new role. The last administrator cannot be removed: add another first.
- **Contain a compromised administrator** (someone else knows its password, or its owner acts against the market):
  1. Demote it at once: `docker compose run --rm app -set-role HANDLE buyer`. This ends its sessions and pending sign-ins, and a password-confirmed change of its own that is not yet saved when the demotion commits (a suspension, factor reset, review or listing takedown, payment intake or staff list switch) is refused with 403, changing nothing. Whoever has the password can still sign in as a buyer until the next step.
  2. As another administrator, suspend it on the web: *Admin → Suspend or restore an account*, choose *Suspend* and confirm with your own password. A suspended account cannot sign in (see *Account suspension* above).
  3. Review what it did. On `/admin`, enter the exact case-sensitive handle in *Filter audit by account*; the page shows at most its 100 newest matching events, or says when none are recorded. Invalid handles are refused rather than trimmed or truncated. The filter is account-scoped, so it shows events recorded on that account; also inspect rows on accounts it changed, or download the *Signed audit export* (when `AUDIT_SIGNING_KEY` is set, see [Transparency](#transparency)) and search it. Role-change rows on the target account identify the acting administrator by handle; older rows retain their original text. Check the escrow decisions and settings it made, marketplace settings, sign-in protection, operator key and canary, and undo what it changed.
  4. To reinstate the account later: restore it on the web, run `-set-role HANDLE admin`, then give it a new password with `-reset-admin-password` (which works on administrators only) and, if someone else may know its second factors, reset them with `-reset-admin-factors HANDLE` (see [Authentication](#authentication)).

### PGP identity

No configuration. Users paste an ASCII-armored public key on `/account`; it is stored without armor headers or surrounding text (only public key, subkey, user ID and signature packets are accepted: a key with a GnuPG photo ID is refused, with the `gpg --edit-key` … `deluid` steps to remove it), and the fingerprint and user IDs are shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in; renewing the same key (same fingerprint, for example `gpg --quick-set-expire`) keeps them unless it drops a revocation or rolls back the encryption subkey. `/account` and `/pgp` show when the key and its encryption subkey expire, and each process checks all saved keys daily and notifies owners once 30 days before and once after encryption to their key stops. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled or an administrator resets the account's second factors (see Authentication). Vendor pages and order pages show the other party's armored public key, fingerprint and whether ownership is verified, and "Message vendor"/"Contact vendor" links open `/messages?to=<handle>` with the recipient filled in; without a saved key, the page says messages cannot be sent until one is added.

### Orders

Migration 105 copies a listing’s title, full description and fulfillment kind to the order in the same transaction that first reserves stock and requests payment (Bitcoin or Monero escrow checkout). A draft keeps showing the live listing until then. Later edits, archive and takedown leave the purchased copy unchanged; the order and dispute pages show the saved terms while their product link opens the **current** listing. An order without a copy has no trustworthy recorded terms and shows “Not recorded” rather than using today’s title or description as purchase history. The saved description is visible only through an order page its buyer, vendor or eligible reviewing staff may open. It remains in the database and encrypted backups with that order; editing or archiving the listing does not erase it. Delivery content is still governed by its existing delivery record and is not copied into this snapshot.

No configuration. Requesting payment on an order requires that coin's escrow to be on and ready, on a supported test network (see Payments); without it the step is shown as unavailable. Migration 030 adds `deliveries`, `reviews` and `disputes.outcome`. Digital delivery content is stored unencrypted in the database and shown only to the order's buyer and vendor, and read-only to moderators and administrators once the order is disputed; vendors should encrypt sensitive content to the buyer's PGP key before delivering it.

**There is no shipping address field, by design.** For a paid physical order the order page asks the buyer to encrypt their address to the vendor's PGP key in their own PGP application and send it through `/messages`, which accepts only OpenPGP-encrypted messages; the server keeps that encrypted message (and who sent it to whom and when) but cannot read it. A vendor without a key that can receive encrypted messages (none saved, or one that has expired or can only sign) cannot receive addresses, so buyers cannot request payment for that vendor's physical listings until the vendor adds one; digital listings are unaffected. If the vendor's key was changed or its ownership proved again after the order was placed, the buyer's order page says so and asks them to confirm the key with the vendor before sending the address. The market cannot stop an address being typed into a plaintext field: the vendor's note to the buyer, cancellation reasons, dispute reasons, dispute replies and review text are stored unencrypted in the database and every backup. Each of those fields warns that it is unencrypted, says who can read it and asks users never to include an address, real name or tracking number but to send them encrypted from the order page (the dispute forms point to the "Contact dispute staff" keys). `/canary#records` lists what is kept.

Opening a dispute notifies every moderator and administrator who is not the order's buyer or vendor and is not suspended. A moderator or administrator who is a party cannot resolve it, and a suspended one cannot sign in, so keep at least one moderator who does not trade: if every staff account that is not suspended is a party (for example the only administrator is the vendor and the only moderator is suspended or absent), the dispute is still accepted, the order history records that no independent resolver was available, every administrator is notified that it needs a moderator who is not a party, and the moderation desk marks it "No eligible resolver" for administrators until such a moderator exists.

The party who did not open a dispute can put one reply on record from the order page while it is open (plain text in the order history, shown to both parties and staff); the resolve form says whether it did. For each open dispute the moderation desk also lists which moderators and administrators each party messaged since it opened, with the count and the latest time, from message metadata only: ask those holders what the encrypted evidence shows before deciding, since nobody else can read it.

Resolving as a refund has the escrow watcher build the refund of the whole counted amount to the buyer; resolving as a release has it build the release to the vendor (see [overpayments](#payments) and [per-order escrow](escrow.md)). The resolve form states each outcome's amount, recipient and settlement (a release pays the vendor the escrow less the market fee and the network fee; an amount paid above the price returns to the buyer's refund address, unless it is below the minimum output, 0.0001 BTC or 0.001 XMR, when it is added to the vendor's output; a refund returns everything, less the network fee, to the buyer's refund address), and needs a ticked confirmation and the resolver's current password (and authenticator code when enrolled); if another deposit is counted after the form was shown, resolving is refused with 409 and the moderator reviews the new amount. A vendor's cancel of a paid order and a party's concession work the same way.

Moderators and administrators open the order page of a disputed (or resolved) order they are not party to, read-only, from the moderation desk; buyer and vendor actions stay unavailable to them. They may also read, the same way, an order with a deposit flagged for payment review, and an escrow order while it has an open staff alert (A-470): a Bitcoin settlement queued or stuck (until the network sees it), a Bitcoin release too small to build (while it stands), or a Monero escrow deposit the database refused as another order's escrow account (A-480, until staff record what was done about it). Opening it changes nothing, and the only action offered is that record; once the alert clears, the order is 404 to them again unless another of these rules admits them. The moderation desk lists such an order under *Escrow alerts*, linked, until its alert clears (a release too small also appears there with its deposits under *Payment review*), newest alert first: by when the order's open alert opened (a stall's order note, the failed release, the refused deposit's record; the latest when an order has several), not by the order's age (QA5W2-3). The list shows the 100 newest; with more open it says how many more orders it leaves out (an older one is listed once newer alerts clear). A refused Monero deposit was never written to the ledger: check it in the escrow wallet and deal with it outside the market (for example refund it to the buyer by hand, or confirm it belongs to the other order); then a moderator or administrator who is not a party records what was done under *Refused Monero deposits* on the order page (10–500 characters, confirmed with their password and authenticator code when enrolled, one audit row; a second record of the same deposit is refused). Only staff see that section and its note; the parties see the order note they saw before. Each refused deposit is one record, so the watcher meeting it again on later passes writes nothing more, and a new refused transaction opens a new record and a new notice. The order page says why staff may read it: for an escrow alert alone it names the open alert and links *Escrow alerts*, for a flagged deposit it links *Payment review*, and *Escrow status and payment intake* on `/admin` links each open stall. Roles are assigned under *Admin → Assign an account role* by entering the account's handle (exact, case-sensitive; the list there shows only the 100 newest accounts). Changing a vendor's role to buyer or moderator archives their active listings in the same audited transaction and blocks new drafts and payment requests on them; existing orders continue.

Reviews are published as soon as the buyer writes them. To take one down (for example text that names or locates a vendor), sign in as an administrator, open the listing or the vendor's page, open *Hide this review* under it and confirm with your password (and authenticator code when enrolled). The review is kept but leaves both public pages and their rating; the buyer and vendor still see it on the order, and administrators see it labelled with *Restore this review*. Both actions are audited without the review text and the reviewer is notified. Migration 031 adds `reviews.hidden_at`.

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). The vendor desk's readiness panel lists the current encryption-key and escrow-key setup needed for new orders; a vendor can also open it from the setup link on `/account`. Check the panel after changing a key or the market's escrow configuration. Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after an escrow is funded; with escrow off it is never released automatically. Administrators can edit other vendors' listings but cannot view or change their automatic delivery content: the editor shows only whether it is set, and only the vendor can change or remove it.

To remove another vendor's listing (for example counterfeit or prohibited goods) while the vendor keeps selling everything else, open its edit page as an administrator, open *Take down listing*, give the vendor a reason of 10–500 characters and confirm with your password (and authenticator code when enrolled). The listing is archived; the vendor is notified with the reason and sees it on the listing and the vendor desk, can still edit the listing, but cannot restore it. The listing leaves the marketplace, but the buyers of its existing orders, and moderators and administrators reviewing those orders, can still open its page (marked Archived, without the reason) from the order. The reason is stored unencrypted and kept in both accounts' activity logs; it is never shown publicly, but do not put an address or real name in it. While the listing is taken down, paid orders on it are not delivered automatically: they wait for the vendor to deliver by hand or cancel and refund (cancelling a funded [Bitcoin escrow](#bitcoin-escrow-test-networks-only) order requests the refund of its escrow, a settlement the buyer or the vendor signs, which the market then co-signs and broadcasts (when both sign it, the market broadcasts it without signing)). *Restore listing* on the same page, confirmed the same way, undoes it. To take away all of a vendor's listings, change their role instead (that archive carries no reason and the vendor can restore those listings if made a vendor again). Migration 041 adds `products.admin_archive_reason`.

### Payments

Per-order escrow is the only payment path in this build. It is **off by default and runs on test networks only**; these payments move no real funds. Until a coin's escrow is configured and ready, its checkout is unavailable (the order page says so, and `/admin` shows why): set up [Bitcoin escrow](#bitcoin-escrow-test-networks-only) (regtest, signet or testnet4) and [Monero escrow](#monero-escrow-fakechain-or-stagenet) (a local fakechain or stagenet). Step-by-step wallet backup, restore and moderation: [docs/testnet-runbook.md](testnet-runbook.md). Upgrading from v0.1.0-alpha.1: [UPGRADING.md](../UPGRADING.md).

**Refused at startup.** Bitcoin mainnet and testnet3 and Monero mainnet and testnet are refused for escrow, and there is no mainnet opt-in ([Mainnet](#mainnet)). The settings of the removed pooled wallet, `BITCOIN_RPC_URL`, `BITCOIN_WALLET`, `MONERO_WALLET_RPC_URL` and `MONERO_RPC_URL`, stop startup before the database opens when any is non-blank; the error names the variables, never their values. Remove them from `.env` (and the old `monero-wallet` profile from `COMPOSE_PROFILES`). A database that holds pooled payment history is refused too: [UPGRADING section 58](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448) and [section 59](../UPGRADING.md#59-pooled-history-refused-migration-108-a-492).

The shared settings that still apply to escrow:

- `BITCOIN_CHAIN` (blank, `testnet4`, `signet` or `regtest`; `main` and `testnet3` are refused) must match the escrow node when set. `PAYMENT_CONFIRMATIONS_BTC` defaults to 3; use 1 for regtest. `MONERO_NETWORK=mainnet` is refused. `PAYMENT_CONFIRMATIONS_XMR` defaults to 10. The escrow RPC URLs, wallets and credentials are the `ESCROW_BTC_*` and `ESCROW_XMR_*` settings of the two escrow sections below.
- `PAYMENT_POLL_INTERVAL` (default `30s`, from 1s to 1h) sets how often the single background escrow watcher polls. Several app instances can share a database; only one polls at a time.
- `BITCOIN_MAX_TIP_AGE` (Go duration, default `2h`, from 30m to 24h; anything else stops startup) is how old the tip block of a Bitcoin node on a public test network (testnet4 or signet) may be before the node counts as not synced (A-439). Core's `initialblockdownload` turns false once and stays false, so a node that lost its peers or stopped receiving blocks still reports itself synced while new deposits are invisible to it. The Bitcoin escrow watcher therefore skips its pass (no deposits read, no expiry, no settlements) while the node is in initial block download, its headers are ahead of its blocks, or, on a public chain, it has no peers (`getnetworkinfo` `connections` 0) or its tip is older than this. Raise it only if your chain's blocks are often slower than this (testnet4 can be); a false alarm only delays a cancellation, never a payment. A public-chain node whose `getblockchaininfo` has no `time` field (the tip block's time) also counts as not synced, with its own reason, "tip time unreadable: this Bitcoin Core is too old for the sync guard; use Bitcoin Core 31.1 or later": the guard needs Bitcoin Core 31.1 (the version the installer pins; earlier versions untested). Nothing is stored for this check; it is made again on every pass. A regtest node is exempt from the peer and tip-age checks (a lone regtest node has neither). A regtest node still reports initial block download at height 0 and after a restart when its tip is older than 24 hours (Core's `-maxtipage`); with no peers, waiting does not end it: mine one block (`bitcoin-cli -regtest generatetoaddress 1 <address>`) or restart bitcoind with a larger `-maxtipage`. The admin page says so on regtest.
- `MONERO_MAX_TIP_AGE` (Go duration, default `1h`, from 20m to 24h; anything else stops startup) is the same bound for a Monero daemon on a public network (stagenet) (A-449). A monerod that lost its peers can keep reporting `synchronized` with `target_height` not above its height while new deposits are invisible to it, so the [Monero escrow](#monero-escrow-fakechain-or-stagenet) watcher skips its pass (no deposits read, no expiry, no settlements) while `get_info` reports `synchronized` false or a `target_height` above its height, or, on a public network, no connections (`outgoing_connections_count` plus `incoming_connections_count` 0) or a top block (`get_last_block_header`) older than this. Nothing is stored for this check; it is made again on every pass. A local fakechain (Monero escrow's test chain) is exempt from the connection and tip-age checks, and `get_last_block_header` is not called on it. Both calls are on monerod's restricted RPC, so the bundled `monero` service (which serves restricted RPC) answers them. A Monero block is due every 2 minutes; raise this only if your network's blocks are often slower (stagenet can be). **The window this leaves on a restricted RPC (QA4W3-5).** A restricted RPC (the bundled `monero` service, and most remote nodes) hides the connection counts, `synchronized` stays true once it is set, `target_height` reads 0, and `offline` means only that monerod was started with `--offline`; so a daemon cut off from its peers looks synced until its top block is older than `MONERO_MAX_TIP_AGE`. For up to that long (default 1 hour, at least 20 minutes) after the last block it received, the watcher keeps running against a daemon that sees no new transactions: a deposit made then is not seen, and an order whose payment window ends in that time can be cancelled as unpaid. When the daemon is back, that deposit is recorded after the order closed and goes back to the buyer's refund address by a stray refund once it confirms ([Monero escrow](#monero-escrow-fakechain-or-stagenet)). A lower `MONERO_MAX_TIP_AGE` (down to `20m`) narrows the window; a false alarm only delays a pass.
- `PAYMENT_EXPIRY` (Go duration, default `24h`, from 10m to 720h) is the payment window. An order still awaiting payment that long after its escrow was created, with no deposit seen, is cancelled by the escrow watcher and its reserved stock is returned. An order with a deposit still confirming stays open. An underpaid order (every deposit confirmed, together short of the price) is cancelled the same way once its escrow is older than the window and no deposit has arrived for the window either; the refund is requested from the escrow and the buyer is notified. The order page states the time from which the order can be cancelled (escrow creation or latest deposit, whichever is later, plus the window), which moves when the node observes a new deposit, not when the buyer sends it; watcher delay can therefore leave less time to top up. Changing `PAYMENT_EXPIRY` moves it for every open order. A buyer or vendor who cancels an order awaiting payment after a deposit arrived must confirm the amount the form shows. A buyer can hold at most 3 orders awaiting payment at once. Requesting payment records the listing's current price.

Administrators set **new escrow checkouts** independently for Bitcoin and Monero under **Admin → New escrow checkouts**: **Escrow** or **Paused** (no new escrow checkout). Each change requires the administrator's password (and TOTP when enrolled), is audited, and takes effect without restarting. A pause does not stop monitoring existing escrows, top-ups, refunds or settlements, and does not turn off the node or wallet daemon. Migration 052 supplies the persisted per-currency policy; migration 107 turned the old *On* setting into *Escrow* and left a pause paused. See [Cutting over to escrow only](#cutting-over-to-escrow-only).

On an open order the page shows the amount **remaining to send**, subtracting valid confirmed and unconfirmed deposits from the required amount. Send only the remainder to the same address; once it reaches zero, wait for confirmations instead of paying twice. Locked and conflicted transfers do not count. The order is paid only when the confirmed total reaches the required amount.

**Awaiting-payment confirmation wait (A-15).** A deposit that stays below the required confirmation threshold no longer reserves a listing indefinitely. Its cancellation cutoff is the later of the original escrow payment-window end and the first eligible deposit the wallet recorded plus the longer of 14 days and `PAYMENT_EXPIRY`. The first recorded deposit stays the clock anchor even if the wallet later drops or marks it conflicted; small top-ups or replacements cannot restart it. The watcher cancels only after a successful, synced read of that order's address, returns reserved stock and never credits or sends unconfirmed funds. A deposit that confirms after cancellation follows the escrow's refund or stray-refund path. A short `PAYMENT_EXPIRY` never reduces the 14-day confirmation wait, while a longer one extends it. The order page states the applicable cutoff, with a separate warning while confirmation is pending.

Operational limits to accept before enabling escrow:

- Bitcoin escrow is 2-of-3 multisig; **Monero escrow is custodial**: the market holds the keys of every escrow wallet and can move those coins. Whoever controls the server or its Monero wallet secret controls them. See [per-order escrow](escrow.md) for what each protects and does not protect.
- Settlements are built by the escrow watchers and broadcast under each coin's own rules. A Monero settlement is stored before it is relayed and is never rebuilt; an administrator can re-request a failed build under guards. Failed or interrupted sends are shown on the admin page, never retried blindly.
- The escrow watcher never cancels an order for non-payment in a pass that did not read its escrow, and skips a coin entirely while its node is syncing or not synced (the tip-age bounds above).
- A deposit first seen after an order closed is never counted toward its settlement. It is flagged to moderators and returned to the buyer's refund address by a stray refund, or stays in the escrow when it is too small to spend; see [Stray refunds](#bitcoin-escrow-test-networks-only) for Bitcoin and [Monero escrow](#monero-escrow-fakechain-or-stagenet).
- Monero transfers with a non-zero `unlock_time` (locked transfers) are recorded and shown, but never counted toward payment or paid out. The order history notes "locked transfer ignored" once and moderators and the buyer are notified.
- There is **no payout address** to save any more: refunds and stray refunds go to the refund address the buyer pinned at checkout. Payout addresses saved before escrow stay in the database as history; no page shows them and nothing uses them.

#### Overpayments and the payment QR code

The pooled build showed a payment QR code and split an overpayment into a release and a return; both were removed with it. In escrow the order page shows the escrow address, the amount remaining and the confirmations. A release returns an amount paid above the price in full to the buyer's refund address as its own output, except that one below the minimum output (0.0001 BTC; 0.001 XMR) is added to the vendor's output; a refund returns everything, less the network fee. Coins that no settlement covers (a deposit that arrives after the settlement was built) are handled as the last bullet above says. While an order is paid, shipped, delivered or disputed the order page states which applies, and says a refund is no longer possible once a dispute's release is final (A-508, A-509).

#### Mainnet

Mainnet (live Bitcoin and Monero, real funds) is **not available**. Bitcoin escrow refuses `BITCOIN_CHAIN=main` and `testnet3` and a node that reports either; Monero escrow refuses `MONERO_NETWORK=mainnet` and a daemon that reports mainnet or testnet. The pooled build's mainnet opt-in was removed with it, and a mainnet escrow path is not built (A-460). There is no setting to turn mainnet on, no real-funds checklist to follow, and no mainnet run is recorded ([verification: not verified](verification.md#not-verified)). Never switch escrow on for a market that takes real payments.

#### Bitcoin escrow (test networks only)

**Off by default, test networks only (regtest, signet or testnet4), and in no release.** [Per-order escrow](escrow.md)
is built for Bitcoin through its physical-order phase and per-order wallet keys; what is not built is listed there
(mainnet, A-460, is refused, and public testnet4 is unrun). A user registers the key their own wallet signs escrow with
and activates it by signing a practice transaction, and the buyer of an eligible listing can pay into a 2-of-3 escrow;
the order completes when the escrow is funded. The vendor's claim, refunds, an administrator's dispute decisions and
the return of coins sent after an order closed are built by the server, signed by the buyer or the vendor in their own
wallet, and co-signed, broadcast and followed to their confirmations by the market (see *Settlements* below). Other
orders cannot use Bitcoin payment unless they qualify for escrow checkout; there is no other payment path.
It is built and tested on regtest and on a private signet (see *What was checked* below).
Since A-443 it also runs on signet and testnet4, with the preconditions below. The recorded public signet
smoke covers practice signatures, a digital claim and underpaid buyer refund; public testnet4 and other
public journeys remain unverified ([runbook](testnet-runbook.md#11-bitcoin-escrow-on-signet-or-testnet4)). Never switch it on for a market that takes real payments.

- `ESCROW_BTC=on` turns it on (blank or `off` keeps it off). The app then refuses to start unless the node at
  `ESCROW_BTC_RPC_URL` reports chain `regtest`, `signet` or `testnet4` (mainnet `main` and testnet3 `test` are refused:
  escrow uses test-network key versions and coin type 1), and `BITCOIN_CHAIN`, when set, names one of those:
  "refusing to start: ESCROW_BTC=on runs only on regtest, signet or testnet4 …". An unreachable node or an unreadable arbiter wallet
  leaves escrow disabled with one log line starting `escrow: Bitcoin escrow disabled:`, and the market runs
  without it. The escrow watcher then runs the same checks again every `PAYMENT_POLL_INTERVAL`, with the same
  guards (an escrow chain, the chain pin and the pinned arbiter fingerprint below, and a watch-only wallet without private keys), and escrow
  starts without a restart once they pass ("escrow: Bitcoin escrow on (test networks only)"). A line is logged when the
  state changes, not on every retry. A retry that finds the node on another chain, or refusing
  `ESCROW_BTC_FEE_ADDRESS`, cannot stop the running market: it logs "escrow: Bitcoin escrow disabled until the app
  is restarted: …" and stops retrying.
- `ESCROW_BTC_RPC_URL`: the escrow node's RPC URL (regtest, signet or testnet4) for a user only the app's escrow code
  uses, restricted with `rpcwhitelist` as below (`http://opsecmkt-escrow:PASSWORD@bitcoin:18443` on regtest; Core's
  default RPC port is 38332 on signet and 48332 on testnet4).
- **Signet and testnet4 (A-443).** Escrow accepts them only with the safeguards built for a public chain, all of which
  apply as this section describes: the sync guard with its peer and tip-age checks (*Sync guard*), the chain pin with
  the signet challenge (*Chain pin*), the arbiter-wallet ownership and transaction checks below and, with `APP_MODE=tor`, the broadcast gate and the stall alert
  (*Tor mode*, *Queued or stuck*). Both chains use `tb1…` addresses; an address given in capitals (`TB1…`, as some
  wallets show it for a QR code) is stored and paid in lower case, as `BCRT1…` is on regtest. A database never
  changes chain once it holds an escrow, so start a signet or testnet4 market on a new database. What has run: the
  real-chain legs on a private signet (*What was checked*); not public signet or testnet4.
- `ESCROW_BTC_ARBITER_WALLET` (default `opsecmkt-arbiter`): a Bitcoin Core descriptor wallet that holds only the
  market's arbitrator key, never `BITCOIN_WALLET`. Create it once as the operator user, loaded at every node start
  (`bitcoin-cli -regtest -named createwallet wallet_name=opsecmkt-arbiter load_on_startup=true`; `-signet` or
  `-testnet4` on those chains, as for every `bitcoin-cli` example here), and back it up
  as the operator user, encrypted, as the runbook's
  [arbiter and watch-only wallet backup](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)
  describes: its seed is the market's escrow key, and `scripts/backup.sh` does not hold it. Use it for nothing else. At startup and on each retry
  the app checks with `getwalletinfo` exactly this: a descriptor wallet with private keys, not encrypted (the escrow
  watcher co-signs without a passphrase, so an encrypted wallet could never sign), and, when `txcount` is above 0, with
  `listsinceblock` that it has never spent: escrow coins never belong to it and the market never sends from it, so a
  spend (an entry of category `send`, which only the wallet's keys can make) means it is also used as an ordinary wallet.
  The read sets `include_change`, so a spend whose outputs all go to the wallet's own change addresses counts too (it
  leaves no `send` entry in `listtransactions`; probed on Core 31.1). Coins paid to it do not count (A-471): every
  escrow order's descriptor shows the market's child key, whose address the arbiter wallet owns, so anyone can pay it;
  such coins stay in the wallet and change nothing. Nor do its payer's fee bumps of such a payment (QA4W6-1): while
  they are unconfirmed, Core lists every replacement as an entry whose `walletconflicts` names all the others, so 1000
  bumps make entries of about 67 KB each and an answer of about bumps² × 67 bytes (1100 bumps: 82 MB, which the app
  read in about 2 s on Core 31.1 regtest); once one replacement is mined, Core lists the others in no read again (full
  or from the checkpoint, before or after a node restart), and the mined one keeps them all in its `walletconflicts`.
  The check reads each entry key by key and decodes only `category`, so neither keeps escrow off. Only while a payer
  holds thousands of replacements unconfirmed does each read grow with their square, and Core builds each answer in
  memory: 3000 bumps (measured once on Core 31.1 regtest) made a 605 MB answer, 15 to 18 s from Core and 16 s for the
  app's read, and Core's memory grew by about 1.5 GB per such answer without giving it back, so a node held to 5 GiB
  (a Docker memory limit) was killed by the third read in a row. **Run escrow on a node of its own**, so that such a
  burst cannot starve the application or other node workloads, and give that node memory to spare (each such read grew Core by
  about 1.5 GB). A read that fails or outlasts the 5-minute timeout below leaves escrow off ("could not be read",
  never "must be used for the escrow key only") until a replacement is mined; then Core lists the others no more (3000
  bumps: one 202 KB entry) and the next read passes on its own, once the wait after a slow read has passed (*Reads are
  paced*, below; restarting the app reads at once). Escrow checks the arbiter wallet at startup and on
  retries while it is off, not on every pass while it is on. Nothing needs doing but waiting for a block (on regtest,
  mine one);
  `bitcoin-cli -rpcwallet=<arbiter wallet> getwalletinfo` shows the wallet's `txcount` meanwhile. Otherwise escrow is disabled with "escrow: Bitcoin escrow disabled:
  the arbiter wallet must be used for the escrow key only …". So that mass payments to that address cannot keep escrow
  off by making the read slow (QA4W4A-1: 100,000 entries took longer than startup allows when every start read them all),
  a passing check records a checkpoint in the database (`settings` key `escrow_btc_arbiter_checked`: the block
  `listsinceblock` reported, for the wallet's name and `getwalletinfo` birthtime), and later checks read only what is
  newer than that block, plus anything unconfirmed (100,000 entries: about 5 s for the one full read on a loaded host,
  under 0.2 s after it). The whole wallet is read once, in one call, when there is no checkpoint, when the checkpoint
  belongs to another wallet (another name, or another birthtime, as a wallet rebuilt with `importdescriptors` has; one
  restored from its backup file keeps both) or names a block the node does not know (or one that is not a block hash),
  and every time for a Core that reports no birthtime.

  The checkpoint misses one case (QA4W5-3): `importdescriptors` into the arbiter wallet it was recorded for keeps the
  wallet's name and birthtime, so the checkpoint still applies, and if the imported descriptor's keys have spent
  before, those old spends sit at or below the checkpoint block, where the next check does not read. **After importing
  any descriptor into the arbiter wallet**, delete the checkpoint, so that the next check reads the whole wallet once:

  ```sh
  docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt \
    -c "DELETE FROM settings WHERE key='escrow_btc_arbiter_checked'"
  ```

  It prints `DELETE 1` (or `DELETE 0` when there was none); then restart the app, whose start runs the check. A wallet
  rebuilt blank with `importdescriptors` (the runbook's fallback) gets a new birthtime and is read whole without this
  step, but running it does no harm.

  When that read takes longer than startup allows, escrow stays off with "escrow: Bitcoin escrow
  off while the arbiter wallet is checked: …" and `/admin` says so; the read goes on, and the escrow watcher starts
  escrow once it has passed, without reading again. A read that fails (a timeout, an unreachable node, an RPC error
  such as `listsinceblock` missing from the whitelist, or an answer with one string or other single JSON token longer
  than 64 KiB, which Core never writes: `bad-reply`) is never a pass: escrow stays disabled with "escrow:
  Bitcoin escrow disabled: the arbiter wallet's transactions could not be read …" (and an `/admin` line naming the
  class), and the check is retried every poll, or after a wait when the read took 5 s or more or timed out (*Reads are
  paced*, below). The answer as a whole has no size limit
  (100,000 entries are about 71 MB), only the read's 5-minute timeout. A reused node may still have a legacy pooled RPC user. This release refuses its obsolete URL, and the application no longer probes that identity. Remove the old credentials and whitelist entries after draining the prior installation; restrict the escrow RPC user to its documented methods, because RPC method whitelists are not wallet-level permissions. It cannot check which descriptors the wallet holds (`listdescriptors` is not on the
  whitelist, since it can export the private key). Before it co-signs, the watcher checks that the escrow's market
  child key is in the wallet's derived key range (`deriveaddresses` and `getaddressinfo`) and, when it is not,
  extends the range 1000 keys past the market index with `keypoolrefill` (checked on Core 31.1 regtest under the
  whitelist at index 2501); if it still is not, the settlement waits ("market-key-missing" in the log).

  **Reads are paced (A-479).** The escrow watcher retries the checks every `PAYMENT_POLL_INTERVAL` (default 30 s)
  while escrow is off, and before A-479 each retry read the arbiter wallet again: during a burst of 3000 fee bumps the
  market itself asked the node every 30 s for a 605 MB answer that cost Core about 1.5 GB of memory, enough to get a
  capped node killed. Now only a *slow* read is paced: one
  that took 5 s or more, or timed out (the 5-minute timeout), whether it passed, found a spend or failed. It makes the
  next read wait 1 minute, then 2, 4, 8, 16 and 32 after each further slow read, and at most 1 hour, counted from when
  the read ended. A read that answers in under 5 s, pass or failure, ends the waits: a failure that answers at once (a
  refused `listsinceblock`, an RPC error, a node that is down) costs the node nothing and is read again at every poll,
  as before. While a wait runs, a retry does not read the wallet and takes what the slow read found, then runs every
  other start check as usual:
  - a slow pass counts as a pass, so escrow starts as soon as the other checks pass (a watch-only wallet loaded late,
    say), with no "could not be read" line. A running escrow never reads the arbiter wallet again, so trusting a pass
    at most an hour old is no weaker;
  - a slow failure keeps "escrow: Bitcoin escrow disabled: the arbiter wallet's transactions could not be read …" and
    its `/admin` alert, which names the next check ("The next check is in about 4 minutes."; after a quick failure it
    says "The next check is at the next poll.");
  - a slow read that found a spend keeps "escrow: Bitcoin escrow disabled: the arbiter wallet must be used for the
    escrow key only …".

  Each slow read logs one line, "escrow: the arbiter wallet's read took 16s and passed; the next read waits about 1
  minute" ("… and failed (`<class>`); …" or "… and found a spend; …"). A read already in progress (one that outlived
  startup) is still taken over, never started twice. Startup's read is never held back: the wait is kept in memory only,
  so **restarting the app reads at once**, which is the way to bring escrow back without waiting out the hour once a
  block has confirmed a replacement. No setting changes this schedule. What it bounds is the market's own load on the
  node (at 3000 bumps, one such read per wait at most instead of one per poll). It cannot keep anyone else from loading
  the node, and on a public test network, where fee bumps cost almost nothing, a payer can repeat the burst every block:
  while the node copes, reads still pass (slowly: 3000 bumps took 16 s) and escrow comes on as before; a node that runs
  short of memory fails them, and escrow stays off until one passes. A node of escrow's own with memory to spare is what
  keeps such a burst away from everything else.
- `ESCROW_BTC_WATCH_WALLET` (default `opsecmkt-escrow-watch`): a descriptor wallet with private keys disabled into
  which the app imports each escrow's descriptor (public keys only) before it shows the escrow address, and from
  which the escrow watcher reads deposits. Create it once as the operator user
  (`bitcoin-cli -regtest -named createwallet wallet_name=opsecmkt-escrow-watch disable_private_keys=true blank=true load_on_startup=true`);
  it must differ from the arbiter wallet and `BITCOIN_WALLET`. At startup the app checks it with `getwalletinfo`;
  a wallet that is not loaded or holds private keys leaves escrow disabled with "escrow: Bitcoin escrow disabled:
  the watch-only wallet could not be read …". Before each import the checkout asks it with `getaddressinfo` whether
  it already holds the escrow address (Core 31.1 reports an imported one as `ismine`, with its `escrow:<order ID>`
  label): after a database restore, an escrow created after the backup is skipped and the next child keys are tried
  (A-359; see the [runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) for what this
  does and does not check). Keep this wallet when you restore the database: it is what remembers those escrows.
- `ESCROW_BTC_FEE_BPS` (default `0`) and `ESCROW_BTC_FEE_ADDRESS`: the market fee in basis points (250 = 2.5%) and
  the address on the escrow node's chain it is to be paid to, required when the fee is above 0 and checked with the node at startup.
  They apply until an administrator saves the fee on `/admin` (*Escrow status and payment intake → Bitcoin escrow market fee*:
  basis points from 0 to 10000 and, above 0, an address on the escrow chain the node accepts and the market has not used as an
  escrow, refund, deposit or settlement address; confirmed with the administrator's password and authenticator code
  when enrolled; the audit row names the old and new basis points, never the address); the saved values are the
  `settings` keys `escrow_btc_fee_bps` and `escrow_btc_fee_address` and override the environment from then on. Each
  escrow keeps the fee and destination in force at its checkout; changing them affects only later escrows. One fixed
  address is allowed because Bitcoin escrow runs on supported test networks only; mainnet escrow is refused. The fee is an output of the vendor's claim (below 0.0001 BTC it is added to the
  vendor's output instead).
- `ESCROW_BTC_FEE_RATE` (default `2`): the network fee rate of every settlement the app builds, a whole number of
  sat/vB from 1 to 100 (anything else stops startup). A settlement whose fee at this rate would exceed 0.001 BTC is
  not built; lower the rate and restart. **Keep it well below 100** (A-388): the network fee is this rate times an
  upper bound of the transaction's size, while completing a settlement checks the fee at its real size against 100
  sat/vB, so near 100 (from about 85 for a one-deposit settlement with three outputs, about 92 with one output) a settlement can be built and signed and then
  refused when the market completes it (log: "settlement of order … not sent yet: final-fee"). Its transaction
  cannot change once a party has signed it, and the buyer and the vendor could then only move the escrow together
  from their own wallets. A fee bump does not have this gap (below).
- **Sync guard (A-439).** Each escrow watcher pass first checks the node (`getblockchaininfo`, and on a public chain
  `getnetworkinfo`). While it is not synced (initial block download, headers ahead of blocks, or on a public chain no
  peers or a tip older than `BITCOIN_MAX_TIP_AGE`, see [Payments](#payments)) the pass reads no deposit, expires no
  order, flags nothing (no spent-outside or regression flags) and builds, co-signs or broadcasts no settlement; only the
  release deadlines, which need no node, still apply. It logs one line when the state changes, "escrow: Bitcoin escrow
  node not synced (<reason>); escrow watcher passes skipped until it is" and "escrow: Bitcoin escrow node synced;
  escrow watcher passes resumed", and `/admin` shows "Bitcoin escrow node not synced: <reason>." under *Payment
  providers* until it is synced. The reason is one of `initial block download`, `headers ahead of blocks`, `no peers`
  or `tip older than BITCOIN_MAX_TIP_AGE`: wait for the node to sync, or give it peers (a `getnetworkinfo` the node
  refuses is a failed node check instead, logged on every pass as `escrow watcher: escrow: node check failed: …`). On
  a public chain it can also be `tip time unreadable: this Bitcoin Core is too old for the sync guard; use Bitcoin
  Core 31.1 or later`: the node's `getblockchaininfo` has no `time`, so the tip's age is unknown and the pass is
  skipped until the node is upgraded (the guard needs Bitcoin Core 31.1, the version the installer pins; earlier
  versions untested). Nothing is stored for it. On signet and testnet4 every check applies; on regtest only the
  first two apply, and in clearnet mode `getnetworkinfo` is never called, so a whitelist without it keeps working there. Waiting does
  not help a regtest node in initial block download: Core reports it at height 0 and after a restart when its tip is
  older than 24 hours (`-maxtipage`), and a regtest node has no peers to sync from. Mine one block
  (`bitcoin-cli -regtest generatetoaddress 1 <address>`) or restart bitcoind with a larger `-maxtipage`; on regtest
  the `/admin` line says so. In `APP_MODE=tor` the same check also reads `getnetworkinfo` on regtest, for the Tor-mode
  gate below.
- **Tor mode (A-441): broadcasts wait for a proxied node.** With `APP_MODE=tor` the escrow watcher reads
  `getnetworkinfo` once per pass and broadcasts a settlement (`sendrawtransaction`, first send or resend) only while it
  shows a proxy for both `ipv4` and `ipv6` and a `version` of at least 310100 (Bitcoin Core 31.1, the first with
  `-privatebroadcast`). Otherwise, and when `getnetworkinfo` fails (a whitelist without it, say), nothing is sent and no
  settlement changes state (co-signing still stores the final transaction; deposits, builds and everything else go on);
  the log says once per change "escrow: APP_MODE=tor: escrow broadcasts wait (<reason>); start the escrow node with
  -proxy=<Tor SOCKS address> on Bitcoin Core 31.1 or later, never -connect or -onlynet" and, when it passes again,
  "… escrow broadcasts resumed", and `/admin` shows "Bitcoin escrow broadcasts wait (APP_MODE=tor): <reason>." under
  *Escrow status and payment intake*. The reason is `no proxy for IPv4 or IPv6`, `no proxy for IPv4`, `no proxy for IPv6`, `Bitcoin Core
  older than 31.1` or `getnetworkinfo failed: <class>`. Configure the escrow node with `-proxy=<Tor SOCKS address>` (it
  sets the proxy of every network) and `-privatebroadcast`; never `-connect` or `-onlynet`, which stall the node's
  sync. For the local `bitcoin` service, [`compose.nodes-tor.yaml`](#local-nodes-over-tor) does this; that section says
  when a private broadcast reaches the network. `-onion` alone does not pass: it proxies onion peers only, and `getnetworkinfo`'s onion `reachable` reports
  configuration, not whether Tor answers, so the gate reads neither. The gate checks configuration only: a proxy that
  is configured but down still accepts the broadcast into a private queue, which the stall alert under *Settlements*
  (*Queued or stuck*) reports. What is private then: a settlement's transaction leaves only through the proxy. What is
  not: deposits sent from a buyer’s own wallet, or legacy pooled sends made by a previous release. The Tor broadcast guard applies to the
  node's own block and peer traffic beyond what `-proxy` covers. Clearnet mode is unaffected. Checked against Core 31.1
  regtest nodes with and without `-proxy` (`TestEscrowRegtestPrivateBroadcast`); no node has run over real Tor here.
- **Chain pin (A-439).** At startup (and on each retry) the app pins the escrow node's chain in the database
  (`settings` key `escrow_btc_chain`: the chain name, such as `regtest`, followed by `:` and the node's
  `signet_challenge` on signet, so two signets are told apart). **A database never changes chain once it holds an
  escrow**: when the node reports another chain while any Bitcoin escrow exists, escrow stays disabled with "escrow:
  Bitcoin escrow disabled: the escrow node is not on the chain this database's escrows are on (escrow_btc_chain) …"
  and `/admin` shows, under *Escrow status and payment intake*, "Bitcoin escrow disabled: the escrow node reports chain <node's>, but
  this database's escrows are on <pinned>."; nothing is changed. Point `ESCROW_BTC_RPC_URL` back at the right node
  (the watcher's next retry enables escrow); a new chain needs a new database. While no escrow exists the pin follows
  the node. A database from before this check that already holds escrows is pinned to `regtest` at the first start
  when the node reports regtest, and refused on any other chain. Two regtest nodes cannot be told apart (both report
  `regtest`).
- **Persisted node health (A-451).** `/admin` under *Escrow status and payment intake* shows the current `ESCROW_BTC` configuration, whether the latest startup or watcher state enabled escrow, the last disabled reason class, the timestamp of the latest status update, the last successful watcher pass, and the last observed chain, height, pin result and keypool/index high-water marks. The row stores fixed reason classes and allowlisted chain names only; it never stores node error text, wallet names or RPC secrets. If `ESCROW_BTC=off`, startup makes no Bitcoin RPC call and records class `off`; the last successful pass and node measurements remain from the prior enabled run and are marked stale in the panel. Turning escrow on again refreshes them after startup checks and watcher passes. A same-fingerprint arbiter account-key substitution is shown as `arbiter_key_mismatch`.
- `PAYMENT_CONFIRMATIONS_BTC` (default 3), `PAYMENT_EXPIRY` and `PAYMENT_POLL_INTERVAL` apply to escrows
  (*Payments* above). An escrow order that has received nothing when its payment window ends is cancelled. One that is
  underpaid (every deposit confirmed, short of the price, and no top-up for a whole window) is cancelled too, and its
  refund requested; the buyer is told. A top-up within the window still completes it. A
  deposit Core reports as conflicted (negative confirmations), or no longer lists, does not count, so it does not keep
  the order open; if it confirms after the order was cancelled and no refund spends it, it is returned by a stray
  refund (*Stray refunds* below; A-428), on an order whose refund failed because the escrow's coins were too few
  together with those coins (A-435; on an order decided for the buyer too, A-437).
- **Arbiter key pin.** At the first start with escrow on, the app reads the arbiter wallet's master key fingerprint
  and BIP84 account key (from a fresh change address, with `getrawchangeaddress` and `getaddressinfo`). It pins the
  fingerprint in `settings` (`escrow_btc_arbiter_fingerprint`) and a key-rooted HMAC of the account key's chain code
  and public key (`escrow_btc_arbiter_xpub_hmac`); the account key itself is not stored. On an older database that
  has only the fingerprint pin, the first upgraded start records the HMAC only when the observed fingerprint matches
  that existing pin. A later fingerprint change, or a different account key with the same fingerprint, disables
  escrow with a fixed operator line. Load the right wallet; the watcher's next retry enables escrow (or restart).
  On a regtest market where you mean to replace the arbiter key, delete both settings rows first, and only while no
  escrow exists (`SELECT count(*) FROM escrows` is 0): every escrow holds a child of the arbiter key, and with another
  key the market can never co-sign for it. Neither the fingerprint, account key, nor HMAC is ever logged.
  Changing `APP_SECRET_KEY` (or the legacy `SETUP_TOKEN` key root) changes the HMAC too and disables escrow even
  with the right arbiter wallet. Restore the original key root; never clear these pins while an escrow exists.
- **Switching escrow off with escrow orders open.** Setting `ESCROW_BTC` back to `off` (or escrow being disabled
  because its node is down) moves no coins and closes no order, but it stops the escrow watcher: an open escrow
  order is not cancelled when its payment window ends, and deposits to its escrow are not recorded, so it is not
  marked paid, until escrow is on and enabled again. The coins stay in the order's 2-of-3 escrow; there is no pooled wallet in this release. The order page still shows the escrow panel with its address and descriptor and "Bitcoin escrow
  is unavailable on this market right now …", never the pooled payment panel (the descriptor download is offered
  only with escrow on). A cancelled escrow order then reads "This escrow cannot be checked right now …", never
  "nothing was received" (nor "0 BTC" for its figures, which read "Not read while Bitcoin escrow is unavailable";
  one whose address is no longer watched reads "Not watched any more"). Nothing is built, co-signed or broadcast
  while escrow is not enabled; a settlement waiting then continues once it is. Cancelling an escrow order with a
  recorded deposit requests its refund, which the escrow watcher builds once escrow is enabled again; while deposits
  are not read, an order that shows no deposit can still be cancelled by its buyer or vendor (the buyer's cancel form
  says, before its button, that coins already sent stay at the escrow address and are not seen until escrow is
  available again, not to cancel after sending, and that they stay recoverable with *Recover coins at this escrow*; the
  vendor's cancel form says that the buyer may already have sent coins that are not seen yet and can recover them),
  and coins already sent to it then arrive after the order closed: a stray refund returns them (below).
- **Deposits after an escrow order closed** (cancelled or completed) are recorded once escrow reads them (the escrow watcher reads
  an escrow for 30 days after checkout, and for 24 hours after its order last changed, such as when it closed; past both the
  address is no longer watched and a later deposit is not recorded; the order page then says so instead of "nothing was
  received"), never counted and never spent by the order's claim or refund; once confirmed, a stray refund returns
  them to the buyer's refund address (*Stray refunds* below). A confirmed
  one is flagged for payment review as a late deposit: one order note, the buyer told the amount once per
  episode (not the vendor) and each moderator and administrator once. The order page's escrow status reads
  "Cancelled; a deposit arrived after the order closed …". The funds remain at the escrow address: the moderation
  desk does not offer *refunded by hand* for an escrow deposit (and refuses one, 409); record *no action* once the
  stray refund has returned it, or escalate. A deposit left out of its settlement before the close, credited or not (a
  refund too few could not spend it, so it no longer counts: QA462D-2), is listed as "Escrow deposit left out of its
  settlement", dated by the note naming it, and handled the same way; one spent outside the market is open and
  recordable, credited or not, but its coins are gone and it does not hold the notice episode (QA462D-1). An escrow
  deposit that counts for the order (credited, or recorded before the close and still confirming at funding) is never
  open: the desk says the order's release or refund settles it, offers no form, refuses a review (409), and its flag
  does not hold the notice episode open, so a later deposit after the close is announced again once staff recorded
  every open late flag (QA462C).
  On a Monero escrow a waiting settlement does not flag a deposit recorded after the close: it is flagged when it is
  announced, so the buyer is told of it once even when the stray refund that returns it waits (QA462C-4).
- **Site-wide notices.** With escrow on, the footer, catalog, listing and draft notices say which chain Bitcoin escrow
  runs on (regtest, signet or testnet4) even though pooled providers have been removed (and, with `ESCROW_XMR` on, which
  network Monero escrow runs on: a local fakechain or Monero stagenet, "network not verified yet" until the daemon was
  verified), and the vendor desk's *Bitcoin escrow* tile counts the viewer's
  escrow orders that may hold coins (with `ESCROW_BTC` off too), so no page says escrow is inactive or no funds are
  held while escrow orders exist. With escrow off and no escrow orders, payment intake stays paused.
- **Compose** passes these variables from `.env` to the app container (`compose.yaml`; blank in `.env.example`, so
  escrow stays off). The bundled `bitcoin` node of `compose.nodes.yaml` follows `BITCOIN_CHAIN` and has no
  `rpcwhitelist` user for escrow, so `ESCROW_BTC_RPC_URL` must point at a node (regtest, signet or testnet4) you
  configure as below; the checked set-up is still the app run outside Compose (inside `go test` against a regtest node
  or a private signet), as `scripts/test-escrow-regtest.sh` does. Passing them through Compose was checked with `docker compose config`
  only.

**The app's RPC user.** Give the escrow code its own RPC user with `rpcauth` and allow it exactly these methods;
keep a separate operator user with full rights for creating and backing up wallets. In `bitcoin.conf`:

```ini
rpcauth=opsecmkt-escrow:SALT$HASH
rpcwhitelistdefault=0
rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getnetworkinfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions,listsinceblock
```

`getnetworkinfo` (read-only: the node's peer count for the sync guard above, and its proxies and version for the
Tor-mode gate) was added in A-439; the app calls it only on a public chain or with `APP_MODE=tor`, so a clearnet regtest
node whose whitelist predates it keeps working, but add it now. `getprivatebroadcastinfo` and `abortprivatebroadcast`
are not on the list: the app never reads or cancels the private broadcast queue (a user allowed to abort could suppress
the market's broadcasts); use the operator user to inspect it.

`rpcwhitelist` allows or refuses whole methods, not their arguments, so the list leaves out every method that
can export or copy a key: `listdescriptors` (with `private=true` it prints the arbiter's private key; probed on
Core 31.1 regtest), `dumpprivkey`, `dumpwallet`, `backupwallet` and everything else not listed. `keypoolrefill`
is on it: it exports nothing and lets the arbiter wallet sign for high escrow indices. `descriptorprocesspsbt`
is used only with throwaway practice keys, never a wallet key. `getwalletinfo`, `getaddressinfo`,
`listreceivedbyaddress`, `gettransaction` and `listtransactions` read the watch-only wallet; on the arbiter wallet the
app reads with `getwalletinfo`, `getaddressinfo` and `listsinceblock` (its start check, above; until QA4W4A-1 that check
paged through the wallet with `listtransactions`, which now reads only the watch-only wallet), besides
`getrawchangeaddress`, `keypoolrefill` and the signing `walletprocesspsbt`; none of them exports a key. `listtransactions` was added in P1-d1 (UPGRADING.md section 22): without it a
deposit spent outside the market is still flagged, but without the spending transaction's ID. `listsinceblock` was added
with the arbiter wallet's checkpoint (UPGRADING.md section 38): without it the start check cannot read the arbiter
wallet, and escrow stays disabled with the read-failure line. `rpcwhitelistdefault=0`
leaves a user with no `rpcwhitelist` line unrestricted: keep that for the operator user alone, and give each user the
app connects as its own line. With `rpcwhitelistdefault` unset instead, Core gives every
user without its own line an empty list once any `rpcwhitelist` is set: the operator user could then call nothing.
Make SALT and HASH with Core's `share/rpcauth/rpcauth.py`, or as
the comment in `scripts/test-escrow-regtest.sh` shows, or let `scripts/escrow-node-setup.sh` generate them with these
lines ([below](#escrow-node-set-up-and-the-public-network-smoke)).

**Retired pooled RPC identity.** This build refuses `BITCOIN_RPC_URL` before startup
and no longer makes the old A-440 pooled-user probes. Remove the prior pooled
user's credentials and whitelist entries from a reused escrow node after its
legacy drain; retaining an unused unrestricted user would still expose the node.
The escrow user's method whitelist, arbiter/watch separation and dedicated signer
checks remain. The previous check is documented historically in
[UPGRADING section 34](../UPGRADING.md#34-bitcoin-escrow-refuses-a-pooled-rpc-user-that-can-read-the-arbiter-wallet-a-440).

##### Escrow node set-up and the public-network smoke

Two scripts make up the operator package for an escrow node (A-446). Neither reads `.env`, and neither writes a
password to a file or a log.

**`scripts/escrow-node-setup.sh`** sets up the escrow node on `regtest`, `signet` or `testnet4` (`--chain`; `main`,
`mainnet`, `test` and `testnet3` are refused, as the app refuses them). It reaches the node only through your own
`bitcoin-cli` (`--cli`, default `bitcoin-cli`; the script adds `-chain=<chain>`), so your cookie or operator credentials
stay where `bitcoin-cli` finds them. For the bundled image, use for example
`--cli 'docker exec -u bitcoin <container> bitcoin-cli'`. It:

1. refuses unless the node reports the chain you named, and on signet prints the `signet_challenge` and whether it is
   public signet's;
2. creates the arbiter wallet (`ESCROW_BTC_ARBITER_WALLET`, default `opsecmkt-arbiter`: a descriptor wallet with
   private keys) and the watch-only wallet (`ESCROW_BTC_WATCH_WALLET`, default `opsecmkt-escrow-watch`: private keys
   disabled, blank), both loaded at every node start. A wallet that already exists is never overwritten: it is loaded
   if needed and kept only when it has the right kind (an encrypted arbiter wallet, one without private keys, or a
   watch-only wallet with private keys is refused);
3. prints the arbiter wallet's master key fingerprint, read as the app reads it (`getrawchangeaddress`,
   `getaddressinfo`). Record it with the wallet's backup
   ([runbook](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)); the app pins it at
   its first start;
4. generates only the escrow user's password on this host with `openssl` and prints
   it once as part of `bitcoin.conf` (`rpcauth`, `rpcwhitelistdefault=0` and the escrow
   whitelist) and `ESCROW_BTC_*` environment lines. The helper never creates a pooled
   RPC user. Port defaults are 18443 on regtest, 38332 on signet and 48332 on testnet4;
   the bundled Compose node pins RPC to **8332**, so pass `--rpc-port 8332` there.
   Progress and the fingerprint go to standard error. Nothing is printed for a failed check.

Copy the lines and add the `bitcoin.conf` ones to the escrow node's configuration, at top level or under `[<chain>]`.
Restart `bitcoind`, then check the node with the escrow URL in the environment (exported for this one command, never read
from `.env`):

```sh
ESCROW_BTC_RPC_URL='http://opsecmkt-escrow:…@127.0.0.1:38332' \
  scripts/escrow-node-setup.sh --chain signet --verify
```

`--verify` passes when the escrow user answers `getblockchaininfo` on that chain and `getwalletinfo` on both wallets
(private keys on the arbiter wallet only), and is refused `listdescriptors` and `gethdkeys` with HTTP 403. A node that
reports another chain, or none, is refused before any wallet call, as in set-up mode.
An inherited nonblank `BITCOIN_RPC_URL` is refused before RPC; clear obsolete pooled
settings before verifying this build. The URLs reach `curl` on its standard
input and are never printed. A command typed with a password on it can stay in your shell history: clear the line
afterwards, or set the variables from a file only you can read. Checked on a local Bitcoin Core 31.1 regtest container:
set-up, restart, `--verify`, then the app logged "escrow: Bitcoin escrow on (test networks only)". The public signet operator package was exercised at the frozen proof revision; the
current removal helper is verified separately by its configuration tests, not a new public run.

**`scripts/test-escrow-public.sh`** is the public-network smoke (P4-R16), the owner's run (A-447). Run it by hand,
never in CI, on public signet or testnet4 only. Before anything else touches the node, it refuses regtest, mainnet,
testnet3 and a private signet: a node on signet counts as public signet only when it reports public signet's
`signet_challenge`, Bitcoin Core's default, `512103ad5e…52ae`. The Go test refuses them again. It runs three legs from
this checkout (`TestEscrowPublicSmoke`) against your node, on a disposable PostgreSQL database:

- **practice-sign**: a buyer's and a vendor's Core wallets register their keys and sign the practice transaction;
- **claim**: one escrow, funded at the price, is claimed by the vendor and followed to confirmation;
- **refund**: one escrow, funded short of the price, is cancelled by the buyer and refunded.

The run makes its own wallets on the node (`escrow-smoke-<random>-…`, unloaded afterwards), never the market's. It pays
the escrows from `ESCROW_PUBLIC_FUNDING_WALLET`, a wallet on that node with at least 0.0016 test coins from a faucet,
and every settlement pays back to that wallet. Settings, all from the environment:

- `ESCROW_PUBLIC_OPERATOR_RPC_URL` and `ESCROW_PUBLIC_APP_RPC_URL`: the operator user and the escrow user;
- `ESCROW_PUBLIC_NODE_IMAGE`: the image the node runs; its digest from `docker image inspect` goes in the record;
- `TEST_DATABASE_URL`: a disposable database, never a market's;
- `ESCROW_PUBLIC_CONFIRMATIONS` (1 to 100, default 1): the confirmations funding and settlements wait for;
- `ESCROW_PUBLIC_FEE_RATE` (sat/vB, 1 to 100, default 2): the rate of the funding sends and of every settlement. On
  testnet4, set it above the node's `mempoolminfee`;
- `ESCROW_PUBLIC_WAIT_MINUTES` (10 to 1440, default 240): how long each of the two waits for confirmations may take;
- `BITCOIN_MAX_TIP_AGE` and `APP_MODE`: passed to the app as they are set.

The checkout must be clean, because the record names the commit that ran. When every leg passed, the script writes
`docs/evidence/public-testnet/<UTC date>-<commit, 12 hex>/<chain>/` for you to commit. It holds `manifest.json` and
`transcript.log`. The manifest records:

- the full commit, start and finish times;
- the chain and its `signet_challenge`;
- the node's version, image and image digest, and the Go version;
- the confirmations and the fee rate;
- the start and end heights;
- each leg's result and its txids (funding and settlement for the claim and the refund);
- the transcript's sha256.

A record is never overwritten. Before anything is written there, the transcript, then the record, must pass a secret
scan: no password from the URLs it was given, no `rpcpassword` or `rpcauth` line, no salt$hash, no wallet passphrase,
no cookie, no URL carrying a password, and no serialized or WIF private key. A failed, skipped or incomplete run writes
no record; its transcript stays under `artifacts/real-chain/` (gitignored).

What was checked here: the refusals, the manifest and the secret scan, offline with a fake node
(`tests/test_escrow_public_script.py`), and the legs' body on Bitcoin Core 31.1 regtest (`TestEscrowRegtestSmoke` in
`scripts/test-escrow-regtest.sh`). The later [2026-10-09 public signet run](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md) passed the three legs with real confirmations, restricted RPC and measured Tor delivery; public testnet4 remains unrun.

**Public Monero stagenet smoke.** `python3 scripts/test-escrow-xmr-public-stagenet.py` is a separate manual,
funded digital-claim check, never CI. Its header lists the required environment. Use a clean checkout, a disposable
loopback PostgreSQL database with a CREATEDB role, an already synced public stagenet daemon, a dedicated open funder
wallet with at least **0.015 unlocked stagenet XMR** (the 0.01 order plus a 0.005 funding-fee cushion), and an initially empty escrow wallet directory. All three
authenticated RPCs must be loopback ports published by the named Docker containers; the runner verifies their
actual image digests and the escrow wallet directory mount. Keep credentials in a private environment file.

Before any wallet call, both the runner and Go test check stagenet identity, current hard fork, peers, sync and tip
freshness, then fetch the public explorer's HTTPS block API and match its near-tip height/hash against the daemon.
The explorer source must be exactly `https://stagenet.xmrchain.net/block/<height-or-hash>` or
`https://xmr-stagenet.librenode.com/block/<height-or-hash>`. Both checks fetch `/api/block/<height>` from the
selected host with a 30-second timeout and a 2 MiB response limit, without proxies or redirects. The selected
API must be available; there is no automatic fallback, and a source URL alone is insufficient.
The check sends 0.01 test XMR through checkout and the vendor claim, waits for ten real
confirmations on funding and settlement, and verifies the planned claimant amount arrived back in the funder wallet.
It neither mines blocks nor changes the application's clock or deadlines.

Only a complete pass writes `docs/evidence/public-testnet/<UTC date>-<commit, 12 hex>/stagenet-xmr/manifest.json`
and `transcript.log`, after scanning both for secrets. The public transcript contains validated proof fields;
raw Go output stays in a private file outside the checkout. The record includes the fetched explorer anchor,
container/image identity, transaction hashes, chain heights and funding/request/build/relay/confirmation times.
This one digital-claim sample does **not** measure refund timing or validate A-482's experimental 24-hour alert.
The [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) passed on frozen ac71878dfde4.
Funding and release each reached ten confirmations; the exact planned 0.00996962 sXMR claim arrived in the test
wallet. Wrapper elapsed time was 50m04s; claim-request-to-confirmed was 27m44s. The fresh pruned cloud node was
first observed fully synced at 14:38:01Z after an unsynced sample at 12:36:28Z, with about 7.03 GB in its whole
Monero data directory. The record preserves this completion bracket, start settings and observations; it does
not claim an exact sync duration. The preceding funded attempt failed proof capture and emitted no PASS
manifest; the verified retry reads the durable build event. Final private recovery was saved and reopened in
a separate container using read-only RPCs. Only the temporary task host was deleted after verification.
No blockchain files were downloaded locally. Public refunds, deployment and A-482 threshold validation remain unverified.

**What users see.** With `ESCROW_BTC` off, `/escrow-key` does not exist: it answers 404 like any unknown page,
signed in or not. With it on, `/account` gains a *Bitcoin escrow key* row linking to `/escrow-key`. There a user registers
one extended public key with its origin, `[fingerprint/path]tpub…`, confirmed with their password (and
authenticator code when enrolled); keys on another path than BIP48's `m/48h/1h/<account>h/2h` also need their
confirmation that the wallet is used only for this market. Private keys, recovery words, descriptors, several
keys, mainnet keys and single-signature (SLIP-132 `upub`/`vpub`) keys are refused before anything is stored, and
the same key cannot be registered on two accounts. The key stays inactive until its owner signs the page's
practice transaction in Bitcoin Core and pastes it back; it spends a made-up output, so it can never be
broadcast. The server keeps the key sealed with `APP_SECRET_KEY` (see
[What the server keeps](implementation-status.md#what-the-server-keeps)). With a different `APP_SECRET_KEY` (or
legacy `SETUP_TOKEN` key root), registered party-key rows cannot be opened. More importantly, the root also keys the
immutable HMAC pin for the arbiter account key and, when configured, the escrow signer's account key. Changing it
therefore disables Bitcoin escrow even when the same node and wallets are available. Restore the original key root;
never clear or replace these pins while any escrow exists. See [Secrets](#secrets-env-and-onion-identities).

An unreadable party-key row can be re-registered only after the original key root is in use and startup has verified
the pinned node and, when configured, signer. The app confirms with Core (`deriveaddresses`) or the pinned signer that
the submitted key derives the child an existing escrow holds; it then activates that party key without changing any
escrow row (practice signing is never offered for a key with escrows). Another key under that origin is refused. Once
a key has escrows its origin cannot be changed: a same key registered with another fingerprint or path is refused with
"This key has escrows, so its origin (fingerprint and path) cannot be changed …"; its escrows keep their recorded
origin. This build has no tool to change a recorded origin. Before a key has escrows, its owner can still correct its
fingerprint or path and sign the practice transaction again. Changing the key root also changes party-key HMACs, so
the cross-account reuse check is not reliable until the original root is restored and each affected key is registered
again.

On the order page of a BTC draft on a digital listing with automatic delivery (or, with Phase 2 below, a physical listing with a dispatch window), the buyer is offered *Pay into an
escrow checkout once both the buyer's and the vendor's escrow keys are active. The buyer
gives a refund address from their own wallet, fixed for the order; an address the market already used as an escrow,
refund, fee or deposit address is refused. The app then builds the escrow: each party's next child key of the
registered key (`/0/1`, `/0/2`, …, one more than the highest index any of that account's escrows used, under any
key or origin; `/0/0` is the practice child) and a fresh child of the arbiter key, expanded by the node, the
witness script checked (three distinct keys, exactly one from the arbiter wallet) and the public descriptor
imported into the watch-only wallet, before the address exists anywhere. The order page shows the escrow address and
the descriptor with its checksum, each party's own key with its origin and the other two as plain keys, with a
download, the claim ("the market cannot spend this escrow without you or the other party, if the key shown as yours
is yours") and the notice that the other party can trace the escrow's funding and settlement on chain. The escrow
watcher records each deposit; at the confirmation threshold the order is paid, its content delivered and the order
completed. No payout is ever queued for an escrow order: its coins leave only by a settlement (below). A funded
escrow order that was not delivered automatically can be disputed; only an administrator decides it (below). A funded escrow order on a listing an administrator took down is not
delivered automatically: the vendor can deliver by hand, or cancel it, which requests the refund of its escrow. If
another escrow already holds one of the child keys a checkout would use, or the orphan sweep reserved it after a restore
([runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)), the checkout skips it and uses
that party's next index (the log says "a child key it composed is already held by an escrow or reserved by the orphan
sweep"); after 100 such tries it is refused ("another escrow on this market already holds one of its keys …").

**Settlements.** A settlement is a transaction that spends an escrow: the vendor's claim on a completed order (or one
whose dispute was decided for the vendor), the buyer's refund of a cancelled one (or one decided for the buyer), or a
stray refund of coins that are not the order's. The server builds it, the buyer or the vendor signs it in their own
wallet, and the market co-signs and broadcasts it; every page and notice says nothing has been sent until it is
broadcast.

- *Requesting.* The vendor of a completed escrow order requests the claim on the order page with a fresh address on
  the escrow chain from their own wallet, confirmed with their password (and authenticator code when enrolled); an address the
  market already used (an escrow, refund, fee, deposit or other claim address) is refused, and it cannot be changed
  afterwards. A refund is requested when a funded escrow order is cancelled: by the buyer or the vendor while it
  awaits payment with a deposit (the cancel form states the escrowed amount and needs its confirmation), by the
  vendor once it is paid, or by the escrow watcher when every deposit has confirmed short of the price and a whole
  payment window has passed without a top-up. It pays the refund address fixed at checkout. The database allows one
  claim or refund per order.
- *Building.* The escrow watcher builds each requested settlement at the end of its pass, never during a form post.
  It waits until every deposit it recorded for the escrow before the order closed has `PAYMENT_CONFIRMATIONS_BTC`
  confirmations (a deposit first recorded after the order closed is not spent: see below), checks each with
  `gettxout` (unspent, confirmed, the recorded value and escrow address), and asks the node for exactly those inputs
  and the planned outputs (`createpsbt`, no coin selection, then `utxoupdatepsbt` with the escrow's public
  descriptor). A claim pays the vendor the escrow less the market fee fixed at checkout and less any amount paid
  above the price, which goes back to the buyer's refund address; a refund pays everything to the buyer. The network
  fee is `ESCROW_BTC_FEE_RATE` (sat/vB, default 2, at most 100) times an upper bound of the transaction's size, and
  comes from the claimant's output only; the market fee is never reduced. A market fee or return below 0.0001 BTC is
  added to the claimant's output, with an order note. The server checks the node's PSBT (inputs, output amounts,
  each input's escrow script and value) and stores it, without any key origin, with its unsigned transaction ID; the
  settlement then waits for a signature and both parties are notified. A build that cannot proceed yet (a deposit
  not yet confirmed, an input the node reports spent or different, a network fee above the cap of 0.001 BTC, more
  than 50 deposits) stays pending, is tried again on every pass and is shown on the order page; the log says
  "escrow: settlement of order … not built yet: <class>" once per change, never a key, address or PSBT. An escrow
  too small to pay the fee and an output of at least 0.0001 BTC fails ("not built"), with a note and a notice to both
  parties; its coins stay in the escrow, where the buyer and the vendor can move them together from their own
  wallets. A refund that fails so, on a cancelled order or one whose dispute was decided for the buyer, also leaves
  those coins out for a stray refund (flagged, one note, the buyer and staff told; *Stray refunds* below, A-435,
  A-437). A release that fails so (a completed order, or one decided for the vendor) leaves them counted: they are
  the vendor's, and only the two parties are told.
- *Signing.* Each party sees the expected outputs (address, amount and whom each pays, and the network fee) and its
  own copy of the PSBT, with its key's origin and the other keys bare, in the page and as a download. Either party
  signs it in Bitcoin Core (`walletprocesspsbt` without finalizing, or the GUI's PSBT dialog) and pastes it back. The
  paste is refused, with nothing stored, unless it is the stored transaction (its unsigned transaction ID), it carries
  that party's signature on every input and no other key's, it has no signature under the market's key, every signature
  is `SIGHASH_ALL` and nothing is finalized (a derivation entry for any key, the market's included, is ignored since
  A-456b: Electrum and Sparrow write one for every key, and nothing but the signatures is kept). Only the party's signatures are kept, never the pasted PSBT; the
  settlement is then *signed by the buyer* (or vendor), the other party is notified, and the database refuses any
  change to its transaction from then on. The other party may sign it too; with both signatures the market does not
  sign. **A pasted signature's validity is not checked when it is pasted** (the server does not verify signatures):
  it is checked only when the market finalizes the transaction (next item).
- *Co-signing and broadcast.* On its next pass the escrow watcher, and nothing else, completes each signed settlement
  of an order in a closed state whose outcome it pays (a claim on a completed order or one decided for the vendor, a
  refund on a cancelled order or one decided for the buyer, a stray refund on any closed order). The arbiter wallet
  signs the market's own stored copy (`walletprocesspsbt` with `SIGHASH_ALL` only), which must come back carrying
  exactly the market's signature on every input; it is combined, market copy first, with the stored party
  signatures (`combinepsbt`) and finalized (`finalizepsbt`). With both parties' signatures the two are finalized alone.
  The final transaction is then re-checked in Go: it is the stored transaction (same unsigned serialization and
  transaction ID), pays exactly the stored outputs (each value, and each script decoded from its address on the escrow chain), its
  fee is the stored one and within 0.001 BTC and 100 sat/vB, and every input's witness is the escrow's 2-of-3 with two
  `SIGHASH_ALL` signatures; then `testmempoolaccept` must allow it at a 100 sat/vB `maxfeerate`. Only then is the raw
  transaction stored (the settlement is *signed*) and, after that, broadcast (`sendrawtransaction`, same `maxfeerate`;
  an answer of its transaction ID, or error -27 for one already in a block, counts as sent). Only that stored
  transaction is ever sent again: a broadcast that failed, or a restart between storing and broadcasting, sends the
  same stored transaction on the next pass, and a settlement with a stored transaction is never rebuilt or signed
  again (the database refuses it). A settlement that is broadcast is followed in the watch-only wallet: still
  unconfirmed, it is sent again (the same transaction); at `PAYMENT_CONFIRMATIONS_BTC` confirmations it is
  *confirmed*. The market-signed PSBT is never stored or shown; the raw transaction and its ID are shown to the
  parties (and staff reading the order) only once it is broadcast. A re-check or `testmempoolaccept` refusal for
  any reason but a signature leaves the settlement signed by the party and waiting, with the log line "escrow:
  settlement of order … not sent yet: <class>" once per change and a line on the order page.
- *Queued or stuck* (A-441). A settlement recorded broadcast means the node accepted the transaction, not that the
  network has it: with `-privatebroadcast` and a proxy that does not answer, Core answers the transaction ID, keeps it
  in a private queue and never puts it in its mempool (probed on Core 31.1 regtest). In every mode, when none of the
  settlement's transactions (its own and any fee bump's) is in the watch-only wallet 6 blocks after it was broadcast,
  staff are told once: an order note "Queued or stuck: not seen by the network. …" and a "Payment review needed"
  notification to each moderator and administrator, and the log says "escrow: settlement of order … queued or stuck,
  not seen by the network after 6 blocks; staff notified". `/admin` lists it under *Escrow status and payment intake* ("Bitcoin escrow
  settlements queued or stuck: not seen by the network.", with the order and the transaction ID) and the order page, as
  staff reading the order see it, carries a staff line. While the alert is open any moderator or administrator who is
  not a party may read the order page, read-only (A-470; the `/admin` line and the moderation desk's *Escrow alerts* link it), and when it clears that access
  ends unless the order is or was disputed or has a flagged deposit. The settlement stays broadcast (no new
  state), and the market keeps sending the same stored transaction on each pass as before. When the wallet sees the transaction, one order
  note ("Seen by the network: …") clears the alert and the lines go. Blocks are counted from the broadcast, in memory:
  after a restart the count starts again at the first pass, so the notice can come up to 6 blocks later, and never
  twice. What staff do: an administrator checks the escrow node as the operator user: its peers
  (`getconnectioncount`), with `-privatebroadcast` whether its Tor proxy answers (`getprivatebroadcastinfo` lists each
  queued transaction and the peers it reached), and whether the transaction is in its mempool (`getmempoolentry`).
  Restore the proxy or the peers and the next resend goes out. The buyer and the vendor can also broadcast the final
  transaction the order page shows from their own node. Never abort the queue entry with `abortprivatebroadcast` to
  "retry": the market only ever resends the same transaction.
- *A signature that does not verify* (A-339): when `finalizepsbt` cannot complete the transaction although the
  market's own signature is there, or `testmempoolaccept` refuses a script, the party's signature is discarded and
  the settlement returns to waiting for a signature, with a fixed order note ("The vendor's signature … did not
  verify …") and a notice to both parties; the log says "a party signature did not verify". When both parties signed
  and the two do not complete it, the market co-signs with whichever one verifies and discards the other. Checked on
  regtest with a signature made over a changed input amount and with r = s = 1.
- *Spent outside the market.* When an escrow output a settlement spends is gone before or after it was sent (spent by
  a transaction the market did not make: the wallet reports a conflict, or the node no longer has the output nor
  this transaction), the settlement is recorded *external* (with the conflicting transaction ID when the wallet names
  it, or, failing that, the transaction the watch-only wallet lists as spending its input: `listtransactions` and
  `gettransaction`), its deposits are flagged and every moderator and administrator is notified. A settlement waiting
  for a party's signature is checked on every pass too (A-356; `gettxout` of each input): spent by its own
  transaction (the parties signed their copies and broadcast it themselves) it is recorded as broadcast, as below;
  spent by any other transaction it is *external* the same way, before any party signed it, and its order page stops
  asking for a signature.
- *Already on the network* (A-344). When a party-signed settlement's coins are already spent as the market comes to
  complete it, the watcher first asks the watch-only wallet for the settlement's own transaction (`gettransaction` of
  its txid). If the wallet has it, in the mempool or a block and not conflicted, the buyer and the vendor broadcast
  the transaction they both signed themselves (or a restored database held an older party-signed row): its hex is
  checked like any final transaction, stored, and the settlement moves to *broadcast* and on to *confirmed*, with an
  order note saying the market did not sign or broadcast it ("completed by" both parties when its witnesses carry
  their stored signatures, otherwise "network"). A transaction with the stored txid that fails the checks waits with
  its class; only another transaction is *external*.
- *Reorganisations.* A confirmed settlement is followed in the watch-only wallet until its transaction has 100
  confirmations (a depth, not a time: a node that was down catches a reorganisation up when it returns; 100 is
  Bitcoin's coinbase maturity). Back at 0 confirmations it returns to *broadcast*, the parties are told, and the
  stored transaction is sent again (not while the restore gate below is set); conflicted, a conflicting transaction
  was mined instead and it is *external*, flagged. A deeper reorganisation is not followed.
- *Deposits spent with no settlement.* Each pass checks every confirmed escrow deposit no settlement covers
  (`gettxout`): one that is no longer unspent was spent by a transaction the market did not make, for example the
  buyer and the vendor co-spending it from their own wallets. It is marked, with the spending transaction's ID when
  the watch-only wallet names it, flagged for payment review like a settlement's deposits (an order note, a notice to
  both parties and to every moderator and administrator), never counted towards funding and never spent by a
  settlement; the order page lists it as spent outside the market. A deposit of an order whose settlement is being
  built is left to that build, which waits one pass on an input that is spent (a spend still in the mempool may be
  evicted); still spent on the next pass, the settlement is *external* without being built (A-356, migration 078)
  and the deposit is marked as above. Deposits of that escrow still unspent stay there: the market builds nothing
  more for them, and the buyer and the vendor can move them together from their own wallets.
- *Orphan addresses.* The daily check (below) also lists, under *Admin → Escrow status and payment intake → Bitcoin escrow
  recovery and reconciliation*, every address the watch-only wallet holds with no escrow on record
  (`listreceivedbyaddress` with `include_empty`: each escrow descriptor is imported with the label `escrow:<order
  ID>`, which puts its address in the wallet's address book; `listdescriptors` stays off the whitelist): a descriptor
  imported for a checkout that rolled back after the import, or an escrow created after the backup a restored
  database came from. The market builds nothing for them; coins there move only with two of their three keys.
- *Bounded wait* (A-342). A claim or refund whose build waits on a deposit recorded before the order closed that does
  not confirm, or on an escrow holding more than 50 deposits, waits at most 24 hours (about 144 blocks) from its
  request. It is then built without the unconfirmed deposits and beyond the 50 largest; those are left for a stray
  refund, with an order note and a notice to the buyer.
- *Disputes.* A buyer or vendor may dispute a funded escrow order that is paid, shipped or delivered. Only an administrator decides it: a moderator's `/resolve` of an escrow dispute is refused (403) and the desk
  says so. The decision needs the administrator's password (and authenticator code when enrolled) and states the
  escrowed amount; a refund to the buyer is requested at once, and after a release the vendor claims with a fresh
  address. On a physical order the release built when it was marked shipped is the release of a decision for the
  vendor (no new claim); a decision for the buyer supersedes it while it has no final transaction, and is refused
  (409) once both parties' signatures or the network have made it final. Until the settlement of the decision has a stored final transaction, an administrator who is not a party
  may change the decision on the order page (*Change the decision*, with a reason and their password): the
  settlement of the old decision is *superseded* (it and its signatures are kept as a record and never used; its
  deposits are freed) and the new one requested; both parties are notified and the change is audited. This holds
  for a settlement never built too: one still being built (waiting for a deposit to confirm, or not yet tried) or one
  that failed, for example because the coins were too few (migration 087, A-462). When the old decision was a refund
  whose too-small failure left its deposits out for a stray refund, those deposits count for the order again (an
  order note says so) and the vendor's release includes them; no stray refund returns them. Their flag and any review
  recorded on it are cleared (the order history keeps the review): should a later change leave them out again, that is
  a new, open flag to record (QA462E-1). Deposits left out for any
  other reason (by the bounded wait, or one that did not count when the dispute was decided) stay for a stray refund
  to the buyer. A release that fails because the coins are too few tells staff once ("Payment review needed", A-461),
  as a refund that fails so does, with an order note naming the deposits it tried and any other credited deposit it
  flags, and flags the order's credited deposits (those it was funded with, also when it tried none of them, QA462C-2),
  so staff can open the order (also a completed order never disputed; the failed release itself admits them while it
  stands, A-470) and the moderation desk lists them, dated by that
  note, after the open flags, as escrow deposits counted for the order with no action offered; a deposit it
  tried that counts for the order but was not credited (still confirming, or conflicted, when the escrow was funded,
  and mined since) is not flagged, and should another check flag one, the desk says it is counted for the order,
  offers no action either and refuses a review (409). Its coins stay counted for the vendor until the decision is changed. Once the
  settlement is signed by the market (or both parties), broadcast, confirmed or spent outside the market, the
  decision is fixed (409).
- *Stray refunds.* Escrows stay imported in the watch-only wallet with no age limit. Coins first recorded at an escrow
  after its order closed, deposits a bounded build left out, and a deposit that did not count when its order was
  cancelled (conflicted, or no longer listed) and has confirmed since, when no refund spends it (flagged once: an
  order note, the buyer and staff told, and the order page reads "Cancelled; a deposit to this escrow did not count
  for the order …"; A-428), go back to the buyer's refund address by a *stray refund*: requested by the escrow watcher once they confirm, built, signed by either party and co-signed like any
  settlement; one is in progress per order at a time and a later deposit gets the next one. The buyer of a closed
  escrow order can ask on the order page (*Recover coins at this escrow*, with their password): the next pass reads
  that escrow whatever its age, so this also covers an escrow cancelled while Bitcoin escrow was unavailable, and
  requests the stray refund, or tells the buyer it found nothing. A daily check reads every escrow (the first one 24
  hours after escrow first runs on the database; `settings` key `escrow_btc_stray_check`), one
  `listreceivedbyaddress` per 200 escrows plus one `gettransaction` per deposit transaction.
  A stray refund whose coins are too few to pay the network fee and an output of at least 0.0001 BTC fails once
  ("not built", a note and a notice to both parties). No other is requested while the coins it could spend are worth
  no more than then (a deposit gaining confirmations changes nothing); a further deposit, once confirmed, makes them
  more and the next pass tries again. Meanwhile the order page says the coins are held at the escrow address because
  they are too few to return after the network fee (A-434).
  Lowering `ESCROW_BTC_FEE_RATE` below the rate that failed, or the buyer's *Recover coins at this escrow* (one build
  per request, whatever the coins are worth), tries again; if that fails too it is noted and told like the first.
  An order cancelled (A-435), or decided for the buyer (A-437), whose refund failed because the escrow's coins were
  too few: the failure leaves those coins out (flagged once: an order note saying the refund could not be built because they were too few, the buyer
  and staff told) and holds them as a failed stray refund's coins are held, so no stray refund of the same coins is
  requested; the failure is the latest word on them also when a stray refund of other coins (a deposit after the
  close) was requested and built while the refund still waited, so those coins fail too-small once. A stray refund
  requested while the refund waited but still being built when the refund fails (both in the same pass) adds those
  coins to its own: built, it returns them, and the refund's failure then holds nothing more (a later deposit gets its
  own stray refund; a coin of the refund's it leaves out, beyond the 50 largest or worth less than its own spending
  cost, gets one more try, failing too-small once if it is still too few, and is then held); failing too-small, it makes the same coins fail a second time, at the grown value, after which
  they are held. A later deposit left out on that order (one that did not count at the cancellation or the decision
  and has confirmed since, noted as above) makes them worth more: the next pass requests a stray refund of them all.
  The order page reads "Cancelled; the market could not build the refund …", or on an order decided for the buyer
  "The dispute was decided for the buyer, but the market could not build the refund …". On a database upgraded from
  before A-435 (a cancelled order) or A-437 (decided for the buyer), the deposits of an order whose refund had already
  failed so are left out, with the same note, by the first pass that reads its escrow (at the latest the daily
  check). A stray refund leaves out of its build a deposit worth no more
  than its own spending cost at the current fee rate (105 vB times `ESCROW_BTC_FEE_RATE`) while another deposit
  remains; the deposit left out stays for a later stray refund (A-436). A cancelled order's status follows its stray
  refund: held, awaiting signatures, broadcast, then confirmed.

**Physical orders (Phase 2).** Everything above applies to physical listings too, with these additions. All of it is
test networks only, behind `ESCROW_BTC`, and tested as [per-order escrow](escrow.md#phase-2-bitcoin-escrow-for-physical-orders-on-regtest)
lists.

- *Dispatch window.* With escrow on, a vendor's listing form has *Dispatch within (days)*, 1 to 30, 3 by default. A
  physical listing without a window takes no escrow checkout; one that exists from before migration 079 has none until
  its vendor saves it, which stores the default of 3 days even while escrow is off and the field is hidden: once escrow
  is turned on, such a listing takes escrow checkout with a window its vendor did not choose. Each escrow keeps the window in force at its checkout. The buyer may
  cancel a paid order the vendor has not marked shipped from the payment's confirmation plus the window plus 48 hours;
  before that the cancel is refused with its date.
- *Release deadline length.* *Admin → Escrow status and payment intake → Bitcoin escrow release deadline* sets how many days after
  "shipped" the order completes and the market co-signs the vendor's signature, if the buyer has neither signed nor
  disputed: a whole number from 7 to 30, default 14 (setting `escrow_release_days`), confirmed with your password (and
  authenticator code when enrolled), audited with the old and new days. It applies to escrows created after the
  change: each escrow keeps the length in force at its checkout, so a change never moves a running deadline. Only a restore's recheck moves one, later.
- *The flow, from the operator's side.* The vendor marks the paid order shipped with a fresh claim address and their
  password, and the escrow watcher builds the release to that address within a pass. The vendor and the buyer sign it
  on the order page; with both signatures the watcher completes the order and broadcasts the transaction they signed,
  and the arbiter wallet is never asked to sign. A buyer who has signed cannot dispute. At the deadline, with no dispute,
  the order completes and the arbiter wallet co-signs the stored signature of the vendor (or the buyer); a dispute
  before it stops the clock for good and an administrator decides, as above. The buyer is reminded in the market 72 hours
  before, and `/orders` shows a banner for orders under 72 hours. The market sends no email or other message, so a buyer
  who never signs in is not reached.
- *Fee bumps.* A broadcast settlement that is not mined can be replaced by one that pays the same outputs with only
  the claimant's smaller (the vendor for a release, the buyer for a refund or stray refund): the claimant asks on the
  order page with a higher rate and their password, signs the copy the watcher builds, and the market co-signs (or
  the other party signs too and the market does not). The form's highest rate keeps the fee within the 100 sat/vB and
  0.001 BTC caps at the transaction's smallest real size: 71 sat/vB for a release of one deposit with two outputs,
  and the form shows the figure for each settlement. No new `rpcwhitelist` method is needed.
- *`scripts/restore.sh` for escrow.* Besides the settlement holds above, it holds restored fee bumps that have a stored
  transaction (`settlement_bumps.send_error`), sets `escrow_release_recheck` (the watcher's next pass, gate or not,
  moves every passed release deadline of a shipped order with no dispute to 72 hours later and tells each buyer; held
  while escrow is off), sets `escrow_sweep_required` (escrow checkout answers 503 until the orphan sweep has read, with
  `getaddressinfo` on the watch-only wallet, the keys of every address it holds with no escrow row and reserved them in
  `escrow_reserved_keys`), and, when the schema has `escrow_index_floors` (migration 083; a dump from before it does
  not), writes each account's index floor and moves the market's escrow key index 50 past the restored records. A sweep
  that cannot read an orphan's keys keeps checkout paused: the
  [runbook](testnet-runbook.md#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused) says how to investigate
  and, with its privacy consequence, how to clear it.
- *The parties' wallets.* A party's Core wallet signs only keys inside its descriptor range (1000 by default). Once an
  account's next escrow child index passes 900, `/escrow-key` advises `keypoolrefill` in that wallet, and so does the
  order page for that party's child key in that escrow; the market does not do it for them. After a restore, an account that registers its key again after the backup should use a fresh
  wallet account (the market cannot detect a reused key then; [runbook](testnet-runbook.md#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused)).

**After a restore from backup.** The restore gate (`payments_recovery_required`, set by `scripts/restore.sh`
or the manual protection SQL in [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too)) also
pauses escrow: while it is set the escrow watcher builds no settlement, co-signs none and broadcasts no stored
transaction, and sends none again; it still reads deposits, follows broadcast and confirmed settlements, and records
as broadcast (without sending it) a held settlement the watch-only wallet shows on the network. Signing on the order
page still works. The restore also holds every restored settlement that has a stored final transaction (*signed* or
*broadcast*; `settlements.send_error` starting `Restored from backup:`): the restored records may predate its
broadcast or something that replaced it. Under *Admin → Escrow status and payment intake → Bitcoin escrow recovery and
reconciliation*, each held or paused settlement is listed with its transaction ID; check it on the chain and in the
watch-only wallet (`gettransaction` of the txid, `gettxout` of its inputs), then confirm that it was not already sent
and that no other transaction spent the escrow's coins, with your password (and authenticator code when enrolled):
the watcher then broadcasts it exactly once, gate or not, and the confirmation is audited and noted on the order. The
order pages and `/admin` say what is paused and why. Clearing the gate ([the runbook's SQL](testnet-runbook.md#clear-the-gate)) resumes building,
co-signing and broadcasting; a settlement the restore held stays held until an administrator confirms it. A restore
not done by `scripts/restore.sh` or that SQL holds nothing. The restore also asks for a release deadline recheck
(`escrow_release_recheck`): on its next pass, gate or not, the escrow watcher moves every release deadline of a shipped
physical order that has passed in the restored records to 72 hours later and tells the buyer; while the gate is set no
order is released at its deadline.

**What was checked.** `scripts/test-escrow-regtest.sh` (by hand, and in CI by the `escrow-regtest` job, [CI and release delivery](ci-cd.md)) runs key registration and practice
signing end to end with a real Bitcoin Core 31.1 regtest node and the whitelist above: the app user is refused
`listdescriptors`, `dumpwallet` and `backupwallet`, runs `keypoolrefill` and every call key activation makes, and
unsigned, other-wallet, `SIGHASH_NONE`, `SIGHASH_SINGLE|ANYONECANPAY`, corrupted and forged pastes are refused. Its
second leg activates a buyer's and a vendor's keys from their own Core wallets, creates an escrow through the checkout
form, has Core confirm the descriptor each party is shown (checksum and address), that the watch-only wallet holds
only that escrow's public descriptor and that its script is the 2-of-3 of the stored keys, funds it from a regtest
wallet and completes the order on funding. Its third leg has the vendor claim a completed escrow (paid above its
price, with a market fee): one watcher pass builds the claim, Core's `decodepsbt` agrees on its transaction ID and
fee, real Core signatures with `SIGHASH_NONE` or from the other party's wallet are refused, and the vendor's own
wallet signs its copy; then the buyer of an underpaid escrow cancels it and signs the refund in their wallet. For
each, Core confirms that the stored transaction with the market's signature (made by the test, not the app) would
finalize with the stored outputs and pass `testmempoolaccept`. Its fourth leg has the app do the rest, with every
party signature made by that party's own Core wallet: a vendor claim at a market child index past the arbiter
wallet's initial key range, so the watcher runs `keypoolrefill` under the whitelist before it co-signs, broadcast and
confirmed with each output on chain as planned; a refund whose co-signature is stored and the app restarted before
the broadcast, then the stored transaction broadcast unchanged; a settlement both parties signed, which Core
finalizes from the two party signatures alone to exactly the broadcast transaction; a vendor signature made over a
changed input amount and an r = s = 1 signature, each discarded before the vendor's good signature completes it; a
stray refund of coins sent to a cancelled order's escrow; and an administrator's dispute decision for the buyer. Its
fifth leg (P1-d1): both parties sign a claim and broadcast it themselves before the watcher's pass, once left in the
mempool and once mined, and the watcher records it as broadcast and confirms it; `invalidateblock` takes a
confirmed settlement back to broadcast (sent again) and `reconsiderblock` confirms it again; a reorganisation with
the parties' double-spend makes a confirmed settlement external with the replacing transaction's ID; a co-spend with
no settlement is flagged with its transaction ID; and a labelled watch-only import with no escrow is listed as an
orphan. The restore gate was checked once by hand on the real server binary against Core 31.1 regtest (not
scripted): a backup taken with `scripts/backup.sh` before two stored settlements were broadcast, restored with
`scripts/restore.sh`, held both; one the original market had broadcast after the backup was recorded as broadcast
without being sent, nothing was built, co-signed or sent, the administrator's confirmation through the `/admin`
form led to exactly one `sendrawtransaction` over 16 passes, and the runbook's [clear-gate SQL](testnet-runbook.md#clear-the-gate) resumed building and
co-signing. Its sixth leg (P1-d2, `TestEscrowRegtestGate`): a key field holding two keys is refused before any RPC
(Core reads the descriptor built from them as a 2-of-4); a vendor's Core-signed copy with a signature added under
the market's key is refused and changes nothing; the parties co-spend an escrow while its claim waits for a
signature, and before its claim is built (the build refuses the spent input, then the claim is *external*), each
recorded with the co-spend's transaction ID (A-356); once an administrator's refund decision is broadcast,
changing it is refused and the database refuses a second outcome; a party-signed claim is not co-signed while the
restore gate is set, and its stored transaction, held as `scripts/restore.sh` holds it (written by the test), is
broadcast once after the administrator's confirmation; one transaction funds two escrows and each claim spends only
its own output; ten deposits at 100 sat/vB exceed the fee cap, so nothing is built until the rate is lowered; and a
restored database never re-issues an escrow address (A-359): an escrow created after a backup is lost by a restore simulated in SQL,
escrow checkout answers 503 while the restore gate is set, and once it is clear the same buyer and vendor get a fresh
address, because the real watch-only wallet still holds the lost one; the lost escrow is listed as an orphan. Its
A-428 leg (`TestEscrowRegtestConflicted`): a deposit replaced by a conflicting transaction that is mined (Core's
`gettransaction` reports it with -1 confirmations and `listreceivedbyaddress` no longer lists it) does not count, so
the order is cancelled unpaid at the end of its window; once `invalidateblock` removes the replacement's block and the
deposit is mined after all, it is flagged once and returned by a stray refund the buyer's wallet signs, broadcast and
confirmed to the refund address. Its A-453 leg (`TestEscrowRegtestBIP32`, no product caller yet): the Go per-order key
derivation of [per-order escrow](escrow.md#per-order-key-derivation-a-453-shape-b-groundwork) agrees with Core's
`deriveaddresses` for three wallets' BIP48 accounts at several indexes, and a wrong index or a changed chain code is
refused. Its A-454 leg (`TestEscrowRegtestSigner`; needs the signer's RPC user, below): the
[escrow signer](#escrow-signer), built from the tree and started with a fresh seed, co-signs a hand-built per-order
escrow through its socket, which the buyer's Core wallet signs too and which is broadcast and mined; it refuses a
SIGHASH_NONE input, another index, two of its keys in one script and two escrows in one request; the seed is backed up,
restored and signs identically; the app reports a pin mismatch and a signer that is down. Its A-440 leg (the original legs of the pooled RPC user went with the pooled wallet): `TestEscrowRegtestArbiterRestore`, where the arbiter wallet, backed up as the operator user before any escrow, is
unloaded with a vendor-signed claim waiting (nothing is co-signed), restored with `restorewallet` under a new name (the
harness cannot delete files inside the node) and co-signs that claim to confirmation with the fingerprint pin unchanged,
and a blank wallet given the operator-exported descriptors (`importdescriptors`, timestamp `now`) co-signs a second
claim the same way; the encryption step of the runbook is not part of it. Its A-455 leg
(`TestEscrowRegtestPerOrderPractice`, no escrow uses a per-order wallet yet): a per-order wallet key's practice
transaction is signed by Core from the seed and what the practice page shows ([per-order
escrow](escrow.md#per-order-key-layout-registration-and-practice-a-455-shape-b)), and the key is active; the same seed's
Core wallet cannot sign it. Its A-456a leg (`TestEscrowRegtestPerOrderEscrow`): a per-order escrow is created through
the checkout form, Core derives its address from the parties' private roots as Go does ([per-order
escrow](escrow.md#per-order-escrow-creation-and-watching-a-456a-shape-b)), and deposits to two sibling addresses are seen
by the watch-only wallet and recorded left out. Its A-456b legs (`TestEscrowRegtestShapeBRelease`,
`TestEscrowRegtestShapeBSiblingStray`, `TestEscrowRegtestShapeBSiblingStraySignerDown`, `TestEscrowRegtestShapeBBump`,
with the real [escrow signer](#escrow-signer) built from the tree): a per-order escrow's claim, signed by the vendor's
order wallet and uploaded as a PSBT file, is co-signed by the signer, broadcast and confirmed; a deposit to a sibling
address is returned by a stray refund spending that sibling's script, co-signed and confirmed, and one is shown,
downloaded and signed while the signer is down, waiting only for the co-signature; a claim is fee-bumped and the bump
co-signed and confirmed ([per-order escrow](escrow.md#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b)). Its
QA5W4A leg (`TestEscrowRegtestPerOrderRepair`): the app restarted onto empty watch-only wallets standing in for restored
ones repairs such an escrow, every address solvable and the deposits found, with the escrow signer up, with it down (the
order wallet keys, the market's included, are the ones stored at checkout) and after the vendor registered another key. In every leg the app runs inside the Go test against the real node, not as the server binary. A browser journey
(`npm run test:e2e:escrow`, [development](development.md)) drives the escrow forms by keyboard in Chromium at 320 px
with JavaScript off against the real server and a simulated escrow node; it is not chain evidence. The separate public signet smoke and Monero stagenet digital claim are recorded,
but they do not prove the additional physical/regtest journeys above. Mainnet escrow is refused, and the
escrow pages have not been run in Firefox or WebKit.
Since A-443 the same script's `--private-signet` mode (by hand, not in CI) starts two Core 31.1 nodes with
`-signetchallenge=51` on a Docker network with no route off the host and runs every leg above that needs no
regtest-only RPC against them (all but `TestEscrowRegtestRecovery`, `TestEscrowRegtestConflicted` and
`TestEscrowRegtestPrivateBroadcast`, with the reasons it prints), plus `TestEscrowRegtestIsolated`: the app's node
leaves the network, an escrow order past its payment window is not cancelled (the sync guard: no peers), and once the
node is back its deposit, paid and mined on the other node meanwhile, is credited. That is a private signet, not
public: local chain evidence for the signet code paths, not a public-network run.
The Phase 2 legs of the same script (also manual, real Core 31.1, the app reaching the node only as the whitelisted
user, every party signature made by that party's own Core wallet): `TestEscrowRegtestPhysical` (the release built at
shipment, signed by both wallets, completed and broadcast with no arbiter signing call; and a release the parties
broadcast themselves), `TestEscrowRegtestDeadline` (no arbiter signing call before the deadline and one after),
`TestEscrowRegtestBump` (a replacement at a higher rate mined, a 100 sat/vB request refused and the form's highest
rate confirmed) and `TestEscrowRegtestReserve` (the keys of lost escrows read from the real watch-only wallet and not
reused). The physical-order browser journey (`tests/e2e/escrow-physical-journey.spec.ts`, same command) uses the
simulated node too and moves the clock through SQL; it is not chain evidence.

#### Escrow signer

**Development branch, test networks only.** Per-order (shape (b)) Bitcoin escrows, for wallets that cannot sign with a
key Bitcoin Core derives from the market's account (ruling P4-R2; see
[per-order escrow](escrow.md#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b)), need a market key that is not
in the Core arbiter wallet. The escrow signer, `/app/escrow-signer` in the app's image (`cmd/escrow-signer`), holds that
key. Since A-456b it co-signs every per-order escrow's settlements and fee bumps: the app sends it the stored PSBT and
the escrow's per-order market index, after checking its account key against the pins and the escrow's market key. The
Core arbiter wallet above stays the market's key for Bitcoin Core escrows. Per-order escrows are offered only while
`ESCROW_SIGNER_SOCKET` is set and the signer's key matches the pins: without a signer, two users with per-order wallet
keys are told "Escrows with per-order wallets need the market's escrow signer, which is not available on this market
right now" and, under escrow-only Bitcoin intake, a per-order vendor's listings are not available in BTC (listed in the
readiness panel on `/admin`). A settlement waiting for the signer logs "escrow: settlement of order … not sent yet:
signer-waiting" (or `signer-refused`, after one line naming the signer's refusal class) and its order page says "The
market's escrow signer could not co-sign it yet"; it is tried again on every pass and never rebuilt.

What it does and refuses (ruling P4-R17):

- Its seed is a fresh one of its own (never the arbiter wallet's, never derived from `APP_SECRET_KEY` or
  `SETUP_TOKEN`): 32 random bytes in a mode-0400 file in its own volume (`escrow_signer_seed`). The seed is never in
  `.env`, the database, `scripts/backup.sh`'s output, the signer's arguments, environment, logs or errors. Its
  account is `m/48h/1h/0h/2h` of that seed; the market key of the escrow with per-order index `i` is `account/i/0/0`,
  and it also signs for that order wallet's sibling keys `account/i/0/k` and `account/i/1/k` (k < 20), so coins sent
  to a sibling address can be returned.
- It listens only on a Unix socket (mode 0660; no TCP listener), shared with the app through the
  `escrow_signer_socket` volume. Requests are JSON lines with a `version` field (`{"version":1,"op":"info"}`,
  `{"version":1,"op":"sign","psbt":"<base64>","index":<i>}`); `info` answers the protocol version, the account key
  (rooted: depth 0, no parent) and its fingerprint, and a refusal answers only a fixed class (`{"error":"sighash"}`).
- It does no elliptic-curve arithmetic with its private key: the escrow node computes its account's public key
  (`getdescriptorinfo`, at every start of `init`, `info`, `restore` and `serve`) and every signing key. It signs through
  the escrow node's `descriptorprocesspsbt` as its own RPC user, with `ALL` hard-coded, and hands Core its account key
  (`m/48h/1h/0h/2h`, never the seed's master key) only inside the descriptors of those two calls (Core logs the method
  and user, never the key, with `-debug=rpc`; checked on Core 31.1 regtest). Before anything is signed it refuses (each
  with its class, nothing returned): an input with any sighash type but ALL (`sighash`); an input whose witness script
  is not exactly `2 <three sorted distinct compressed keys> 3 CHECKMULTISIG` (`not-escrow-script`) or whose
  `witness_utxo` does not pay P2WSH of that script (`witness-utxo`); inputs of more than one escrow (`mixed-scripts`); a
  script without its key for the requested index (`key-not-found`) or with two of them (`two-signer-keys`). Core's
  result must differ from the request only by one added signature per input under its key, otherwise nothing is returned
  (`result-mismatch`). The other classes are `bad-psbt` (not base64, or a PSBT Core cannot decode or that is not version
  0), `core-refused` (Core answered `descriptorprocesspsbt` with an error) and `core-unavailable` (the node did not
  answer, or the signer's user may not call the method); the protocol's own are `version`, `malformed`, `unknown-op`,
  `bad-index` and `too-large`. The signer's log names the class and the order index, nothing else. It keeps no memory of
  what it signed, so a script holding its keys of two different order indexes is not detected by the signer: the app
  must never build one.

**Set-up** (Compose, `compose.nodes.yaml`'s `escrow-signer` profile; checked with `docker compose config` only, not
yet run under Compose):

1. Give the signer its own RPC user on the escrow node, allowed exactly the three methods it calls, beside the app's
   user (`rpcwhitelistdefault=0` is already set):

   ```ini
   rpcauth=opsecmkt-signer:SALT$HASH
   rpcwhitelist=opsecmkt-signer:decodepsbt,descriptorprocesspsbt,getdescriptorinfo
   ```

   A signer from before QA4W2-1 needed only the first two; without `getdescriptorinfo` every command but `backup`
   now stops with "getdescriptorinfo: the escrow node did not answer (… whitelisted to decodepsbt,
   descriptorprocesspsbt and getdescriptorinfo)".

2. In `.env`: `ESCROW_SIGNER_RPC_URL=http://opsecmkt-signer:PASSWORD@<escrow node>:<port>` (given to the signer
   service only, never the app), `ESCROW_SIGNER_SOCKET=/run/escrow-signer/signer.sock` (the app's side), and
   `escrow-signer` added to `COMPOSE_PROFILES` (comma-separated, keeping the profiles already there; a
   `--profile` flag would replace them for that command). The signer joins the `backend` network only to reach that
   RPC URL; it publishes no port.
3. Make the seed, with the escrow node running: `docker compose run --rm --no-deps escrow-signer init`. It refuses a
   seed file that exists, asks the node for the account key (it writes nothing if the node does not answer), and
   prints the account key and its fingerprint: write the fingerprint down. `info`, `restore` and `serve` ask the node
   the same way at every start; `serve` exits if it cannot, and Compose restarts it.
4. Back the seed up now (below), then start it with `docker compose up -d escrow-signer` and restart the app
   (`docker compose up -d app`). The app logs "escrow: escrow signer ready (shape (b); its account key matches the
   pins in this database)".

Outside Compose, build it with `go build ./cmd/escrow-signer`, run `escrow-signer serve` as its own user whose group
the app shares (the socket is mode 0660), with `ESCROW_SIGNER_RPC_URL` in its environment and `-seed` and `-socket`
(or `ESCROW_SIGNER_SEED_FILE`, `ESCROW_SIGNER_SOCKET`) naming the seed file and socket path; `serve` refuses a seed
file its group or others can read.

**Pins and what the app shows.** With `ESCROW_BTC=on` and `ESCROW_SIGNER_SOCKET` set, the app asks the signer for its
account key at startup and on every escrow watcher pass. At first contact it pins the key's fingerprint and an HMAC of
the key (keyed from `APP_SECRET_KEY`, as registered escrow keys are) in `settings` (`escrow_signer_fingerprint`,
`escrow_signer_xpub_hmac`); a pin is never rewritten. Each state change logs one line and, until the signer is ready,
`/admin` shows a line under *Escrow status and payment intake*:

- A signer whose key differs from the pins: "escrow: shape (b) unavailable: the escrow signer's account key differs
  from the one pinned in this database …". Nothing else changes: escrow with the Core arbiter wallet stays on, and the
  pins and every order stay as they are. Check that `ESCROW_SIGNER_SOCKET` reaches the signer whose seed this market
  pinned (compare `escrow-signer info` with the pinned fingerprint), or restore that seed from its backup. Rotating
  `APP_SECRET_KEY` changes the HMAC, so the right signer then reads as a mismatch too. While the key differs, no
  per-order escrow is offered and per-order settlements wait. Only while no per-order escrow exists may an
  administrator delete the two rows to pin another signer (a per-order escrow can be co-signed only by the signer it
  was built with):
  `DELETE FROM settings WHERE key IN ('escrow_signer_fingerprint','escrow_signer_xpub_hmac')`.
- A signer that does not answer: "escrow: escrow signer not reachable at ESCROW_SIGNER_SOCKET; anything that needs it
  waits (nothing is rebuilt)". Start the service; the next pass finds it. Only new per-order checkouts and the market's
  co-signature need it: order pages, setup pages, copies and the parties' signatures go on (QA5W5A-1).
- A reply the app does not understand, such as another protocol version: "escrow: shape (b) unavailable: the escrow
  signer's reply was not understood …".

**Backup and restore.** The seed is a second backup item beside the database: `scripts/backup.sh` never sees it.
Encrypt it to the operator's OpenPGP public key (the file holds that one key) and keep the result off the server:

```sh
docker compose run --rm -T --no-deps -v "$PWD/operator.asc:/operator.asc:ro" \
  escrow-signer backup -key /operator.asc > escrow-signer-seed.asc
```

To restore into an empty seed volume (it refuses to overwrite a seed file), with the escrow node running, decrypt on
the operator's machine and pipe it straight in. `-fingerprint` is required: restore asks the node for the account key
and writes nothing unless its fingerprint is the pinned one:

```sh
gpg --decrypt escrow-signer-seed.asc | docker compose run --rm -T --no-deps \
  escrow-signer restore -fingerprint <escrow_signer_fingerprint>
```

Rehearsed on regtest by `TestEscrowRegtestSigner` (`scripts/test-escrow-regtest.sh`): a backup to a throwaway key,
decrypted, restored with `-fingerprint`, and a signer from the restored seed gave the identical signature.

**Break-glass** (the signer host is lost and a settlement is queued): on a machine without network access, start a
Bitcoin Core 31.1 node of a test network with no peers (for example `bitcoind -regtest -networkactive=0`) with the
signer's `rpcauth` and `rpcwhitelist` lines, restore the seed as above into a new directory with
`ESCROW_SIGNER_RPC_URL` naming that node, run `escrow-signer serve` against it, and send the queued PSBT and its order
index through the socket:

```sh
python3 -c 'import json,socket,sys; s=socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.sendall((json.dumps({"version":1,"op":"sign","psbt":open(sys.argv[2]).read().strip(),"index":int(sys.argv[3])})+"\n").encode()); print(s.makefile().readline())' \
  /path/to/signer.sock queued.psbt <index>
```

The reply's `psbt` carries the market's signature; the refusals above still apply. The rehearsal signed against the
same regtest node; signing on an offline node was not rehearsed. Delete the restored seed copy afterwards.

**Upgrade order.** Upgrade the signer first, then the app: each request carries the protocol `version` (1), and the
signer refuses any other (`{"error":"version"}`), which the app reports as a reply it does not understand. A release
that changes the protocol says so in `UPGRADING.md`.

#### Monero escrow (fakechain or stagenet)

**Off by default, custodial, test networks only.** [Per-order escrow](escrow.md) Phase 3 lets the
buyer of an XMR draft on a digital listing with automatic delivery, or on a physical listing with a dispatch window, pay into
an account of a monthly escrow wallet the market holds. **The market holds the keys and can
move these coins**: unlike [Bitcoin escrow](#bitcoin-escrow-test-networks-only) there is no 2-of-3, no key of the buyer's or
vendor's and nothing for them to sign. It runs against a local Monero fakechain (`monerod --regtest`) or, since A-449, Monero
stagenet (`monerod --stagenet`); coins on both have no value. It refuses mainnet, testnet and anything else. The [public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) covers one funded claim; broader public journeys
remain unverified. Offline-fork evidence is separate ([per-order escrow](escrow.md#monero-escrow-on-stagenet-a-449)).
Never switch it on for a market that takes real payments. With Monero escrow off, Monero checkout is unavailable; this release has no pooled-wallet fallback. What it does, protects and does not protect, and the tests behind it, are in
[per-order escrow, Phase 3](escrow.md#phase-3-monero-escrow-on-a-local-fakechain) and
[Monero escrow on stagenet](escrow.md#monero-escrow-on-stagenet-a-449).

You need four things before switching it on: a fakechain or stagenet daemon, a monero-wallet-rpc of its own on that network,
an OpenPGP key whose private half stays offline, and a secret. A regtest chain makes a block only when something mines one
(the checks use the daemon's `generateblocks`); the market never does, so on a fakechain nothing confirms a deposit until you
mine. On stagenet the public network mines blocks about every 2 minutes; the daemon must be synced and have peers (below).

**Settings.** All are read at startup, from `.env` through Compose. With `ESCROW_XMR` blank or `off` the escrow configuration
is not used, no seed-key file is opened and no escrow node is contacted. Historical Monero escrow recovery state remains visible
under `/admin` when present; turning escrow off does not delete wallets, orders or settlements.

| Setting | Default | What it takes |
| --- | --- | --- |
| `ESCROW_XMR` | blank (off) | `on` or `off`; any other value stops startup. Mainnet configurations (`MONERO_NETWORK=mainnet` or `BITCOIN_CHAIN=main`) are refused. |
| `ESCROW_XMR_WALLET_RPC_URL` | none; required when on | `http(s)://[user:password@]host:port` of the dedicated Monero escrow wallet-rpc (credentials are answered with HTTP Digest). Keep it dedicated to escrow wallets and attach it to the daemon configured below. The obsolete pooled `MONERO_WALLET_RPC_URL` setting is refused before startup. |
| `ESCROW_XMR_DAEMON_URL` | none; required when on | The same form, for the fakechain or stagenet `monerod` used by this escrow wallet-rpc. The bundled `monero` service may be used when configured for stagenet and can serve restricted RPC; set this key to `http://monero:18081` rather than the retired `MONERO_RPC_URL`. |
| `ESCROW_XMR_FEE_BPS` | `0` | The market fee on a fulfilled order, a whole number of basis points from 0 to 10000 (250 = 2.5%). Each escrow keeps the fee in force at its checkout. There is no form for it on `/admin`. |
| `ESCROW_XMR_FEE_ADDRESS` | none | Required above 0 basis points, ignored at 0: a standard address or subaddress of the escrow daemon's network (95 characters; stagenet ones start with 5 or 7, fakechain ones with 4 or 8; integrated addresses are refused) whose keys are valid, checked in Go at startup: one of the other network stops startup once the daemon is reached. A fee below 0.001 XMR is added to the vendor's output instead. |
| `ESCROW_XMR_SLOTS` | `20` | The free accounts the watcher keeps ready on the active wallet, a whole number from 5 to 200. Checkout takes one; none free is a 503 until the watcher makes more. |
| `ESCROW_XMR_MAX_FEE` | `10000000000` (0.01 XMR) | The highest network fee a settlement may pay, in piconero, a whole number from 1 to `100000000000` (0.1 XMR). A transaction the wallet builds above it is discarded unrelayed and its settlement waits. A young fakechain charged about 0.0018 XMR for a one-input, two-output transfer. |
| `MONERO_WALLET_SECRET` | none; required when on | At least 32 characters, random (not a placeholder, at least 8 distinct characters, no repeating pattern), and different from `SETUP_TOKEN` and `APP_SECRET_KEY`: `openssl rand -hex 32`. Each wallet's password derives from it. |
| `ESCROW_XMR_SEED_KEY` | none; required when on | The path, inside the app container, of a file holding one armored OpenPGP public key (at most 64 KiB). A private key block, several keys, a revoked key or one with no encryption key usable now (signing-only, expired) stops startup. |
| `PAYMENT_CONFIRMATIONS_XMR` | `10` | A whole number from 1 to 1000. A Monero escrow deposit counts from the larger of it and 10. |
| `ESCROW_XMR_TOR_RELAY` | blank | Blank or `clearnet-accepted`; any other value stops startup. With `APP_MODE=tor`, `clearnet-accepted` lets Monero escrow settlements be relayed without the node Tor overlay (*Tor mode*, below). Ignored in clearnet mode. |

`PAYMENT_EXPIRY` (the payment window), `PAYMENT_POLL_INTERVAL` and `MONERO_MAX_TIP_AGE` apply to Monero escrow. The
refusal messages name the setting and never quote its value.

**At startup.** A configuration error, an unusable seed key file, a mainnet configuration, or an escrow node that answers but is
not what Monero escrow needs stops startup with a message that names the setting (for a node: "refusing to start:
ESCROW_XMR=on runs only on a local Monero fakechain or on stagenet in this release, and the daemon at ESCROW_XMR_DAEMON_URL
failed that check …", or the same for the wallet-rpc). The daemon must report nettype `fakechain` (with no network flag) or
`stagenet` (with the stagenet flag) and a hard fork version of at least 16 (a daemon still syncing reports the hard fork of
the height it has reached, so a lower version is not refused while `get_info` says it is syncing: Monero escrow stays
disabled with `… could not be checked at startup (hard_fork_info daemon-syncing)` and is checked again every poll, so a
fresh stagenet node delays Monero escrow, not the market), and the escrow wallet-rpc must, after a refresh,
be within 10 blocks of that daemon (so a wallet-rpc attached to another chain is refused). The daemon's height is read
again after the refresh before a wallet more than 10 blocks from it is refused, and a wallet within 10 blocks of the span
between the two readings passes (A-475), so a daemon that gains blocks while the wallet refreshes (catching up after
downtime, say) does not disable Monero escrow. Before A-449 the daemon also had to
be below height 1,000,000 (no fakechain gets there; every public network is past it): that heuristic is gone, and the network
pin below tells networks apart instead. The escrow wallet-rpc must be attached to the configured escrow daemon; its refreshed
height is checked against the daemon's height, and the daemon must report local fakechain or stagenet. A wallet-rpc with no wallet open is not a failure. Addresses (refund, claim,
the fee address, and every address the wallet-rpc returns) are accepted only in the forms of the network the daemon was
verified on: stagenet 5… and 7…, a fakechain 4… and 8… (it uses mainnet's address format). A node that cannot be
reached leaves Monero escrow not ready with `escrow: Monero escrow disabled: the escrow daemon or wallet-rpc could not be
checked at startup (<class>)` while the market runs; the escrow watcher retries every `PAYMENT_POLL_INTERVAL` and logs
`escrow: Monero escrow on (stagenet)` (or `(fakechain)`) when it passes. A line is logged when the state changes. A node that
answers as another network after startup disables Monero escrow until the app is restarted. While Monero escrow is
configured but not ready, for any of these reasons, `/admin` also shows an alert under *Escrow status and payment intake* ("Monero escrow
not ready: …") with the current failed check and its remedy. The escrow-only readiness panel reads `XMR escrow: on (not ready: …)`,
and *Escrow status and payment intake* names no Monero escrow network until a daemon was verified on one; the alert goes once Monero
escrow is ready (QA4W4B-1). Two more lines mean the pins below have
refused: `escrow: Monero escrow disabled: MONERO_WALLET_SECRET differs from the secret this database was first started with
…` (nothing is created or opened) and `escrow: Monero escrow wallet creation stopped: ESCROW_XMR_SEED_KEY is not the seed key
pinned in this database …` (existing wallets are still read and their orders funded; no wallet is created).

**The network pin (A-449).** The database records the network its Monero escrows are on in `settings` (key
`escrow_xmr_network`: `fakechain` or `stagenet`). While the database holds no Monero escrow and no escrow wallet, the pin
follows the daemon and may change. Once it holds either (a wallet counts: its free accounts are addresses of one network),
a daemon on the other network leaves Monero escrow disabled and changes nothing, with one line, `escrow: Monero escrow
disabled: the escrow daemon is not on the network this database's Monero escrows are on (escrow_xmr_network) …`, and a line
under *Escrow status and payment intake* on `/admin` naming both networks; the check is retried every `PAYMENT_POLL_INTERVAL`, so pointing
`ESCROW_XMR_DAEMON_URL` (and the escrow wallet-rpc) back at the pinned network brings it back. A new network needs a new
database: there is no supported way to move a database's Monero escrows to another network. A database from before A-449
that holds Monero escrow state and no pin is pinned `fakechain` at the first start against a fakechain daemon (the only
network Monero escrow ran on before); against a stagenet daemon it is refused as above, and nothing is written.

**When the daemon is not synced (A-449).** Each watcher pass first reads the escrow daemon (`get_info`, and on stagenet
`get_last_block_header`). While it reports `synchronized` false or a `target_height` above its height, or, on stagenet, has
no peers (`offline`, or no connections on an unrestricted RPC; a restricted RPC hides the counts) or a top block older than
`MONERO_MAX_TIP_AGE` (default 1h; [Payments](#payments)), the pass opens no wallet, creates or destroys none, reads no
deposit, funds or expires no order and builds, relays or follows no settlement; release deadlines still apply. One line is
logged when this starts (`escrow: Monero escrow daemon not synced (<reason>); Monero escrow watcher passes skipped until it
is`) and one when it ends (`escrow: Monero escrow daemon synced; Monero escrow watcher passes resumed`), and `/admin` shows
the reason under *Escrow status and payment intake*. Nothing is stored for this. A fakechain is exempt from the peer and tip-age checks.
If the daemon cannot be read at all, or its top block cannot (`get_last_block_header` fails), the pass applies only the
deadlines and logs `monero escrow watcher: monero escrow: daemon check failed: <class>`. On a restricted RPC (the bundled
`monero` service) a daemon cut off from its peers looks synced until its top block is older than `MONERO_MAX_TIP_AGE`
(default 1 hour, at least 20 minutes): a deposit made in that window is not seen, an order whose payment window ends in it
can be cancelled as unpaid, and the deposit then goes back to the buyer's refund address by a stray refund once the daemon
sees it and it confirms. A lower `MONERO_MAX_TIP_AGE` narrows the window ([Payments](#payments)).

**Tor mode (A-515): relays wait without the node Tor overlay.** The escrow wallet-rpc relays a settlement (`relay_tx`)
through its daemon, and a daemon not behind Tor sends it to its peers from this host's IP address, which links the market's
host to its escrow transactions. So with `APP_MODE=tor` and `ESCROW_XMR=on`, the watcher still builds, checks and stores each
settlement but relays nothing unless either (a) `COMPOSE_FILE` includes `compose.nodes-tor.yaml`, which runs the local
`monero` daemon through its own Tor client ([Local nodes over Tor](#local-nodes-over-tor); Monero over Tor is
**unverified** there, A-95; the overlay covers only that bundled daemon, so point `ESCROW_XMR_DAEMON_URL` at it: an external escrow daemon is not covered and the app cannot tell), or (b) `ESCROW_XMR_TOR_RELAY=clearnet-accepted`, which
accepts the exposure and relays without Tor. One line is logged at startup (`escrow: APP_MODE=tor: Monero escrow relays held
(Tor mode without the node Tor overlay): …`), `/admin` shows "Monero relays held: Tor mode without the node Tor overlay — see
operator guide" under *Escrow status and payment intake*, the order page says the settlement is built and not sent yet, and
`./scripts/install.sh check` warns. Both conditions are read at startup: after adding either, restart the app, and the next
pass relays each stored transaction unchanged (it is never built again, and one settlement per escrow still holds). Bitcoin
escrow has its own gate (*Tor mode* under [Bitcoin escrow](#bitcoin-escrow-test-networks-only), A-441).

**Stagenet.** Point `ESCROW_XMR_DAEMON_URL` at a synced stagenet `monerod` (the bundled `monero` service with
`MONERO_NETWORK=stagenet`, `http://monero:18081`, is one; its restricted RPC answers every call escrow makes) and run the
escrow wallet-rpc with `--stagenet` against the same daemon (the `monero-escrow-wallet` Compose service below does, and the
installer sets it up). The bundled `monero` daemon may serve the escrow wallet-rpc; the wallet-rpc itself must be dedicated to escrow. Give buyers' refund and vendors' claim addresses from stagenet wallets (5… or 7…), and an `ESCROW_XMR_FEE_ADDRESS`
on stagenet. Stagenet coins come from the public faucet or your own mining; they have no value. Keep `ESCROW_XMR_MAX_FEE`
in mind: the default 0.01 XMR cap was sized for a fakechain and is not measured on stagenet.

**The dedicated escrow wallet-rpc.** Run a dedicated `monero-wallet-rpc` for escrow wallets, attached to the escrow
daemon, with its own login and volume. Keep historical pooled-wallet recovery material separate; this release refuses its old
`MONERO_WALLET_RPC_URL` setting.

*On stagenet, with the bundled nodes (A-449).* `compose.nodes.yaml` has a `monero-escrow-wallet` service for it, in its
own profile (`monero-escrow-wallet`) and volume (`monero_escrow_wallet`), with its own login (`escrow`,
`ESCROW_XMR_WALLET_RPC_PASSWORD`), run with `--stagenet` against the bundled `monero` daemon, which must then run stagenet
(`MONERO_NETWORK=stagenet`, the installer's default). Its memory limit is 1 GiB (PAYE-E2 measured 574 MB at peak over five
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
  --wallet-dir=/home/monero/wallet --rpc-bind-port=18083 --rpc-login 'marketplace:PASSWORD'
```

Use the pinned wallet-rpc release the escrow checks used (0.18.5.1).
The fakechain daemon is started as the header of `scripts/test-escrow-xmr-regtest.sh` starts one (`--regtest
--keep-fakechain --offline --fixed-difficulty 1`; without `--keep-fakechain`, monerod deletes the fakechain each time it
starts); it is addressed through `ESCROW_XMR_DAEMON_URL`; the obsolete `MONERO_RPC_URL` must remain blank. No wallet needs creating: the escrow
watcher creates every wallet, named `escrow-YYYYMM-NN`, on its first pass. The escrow wallet-rpc must be dedicated to escrow and attached to this daemon; readiness checks verify its daemon network and refreshed height, not a pooled-wallet URL or address.

**The seed key.** Every wallet's 25-word seed is encrypted to this key when the wallet is created, so that the database never
holds a seed in the clear. Generate it **offline**, on a machine that never runs the market, and keep its private half offline.
It must never be the canary's operator key: that key can be changed from `/admin`, and taking over an administrator account
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
  docker run --rm --network none -v opsecmkt_escrow_wallets:/wallets:ro --entrypoint tar \
    ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 -C /wallets -cf - . | age -r "$AGE_RECIPIENT" -o "backups/escrow-wallets-$(date -u +%Y%m%dT%H%M%SZ).tar.age"
  docker start opsecmkt-escrow-wallet
  ```

Check that the file is not empty before you rely on it. While it is stopped, deposits are not read and nothing is built or sent; the watcher carries on when it is back. To restore the
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
`/admin` (below); retires every free account; moves every active or sealed wallet to closing, so that no account of a
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
of the two transactions can ever be mined.

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
the seed (step 3 above) that it holds nothing before you delete the files `/admin` then lists. A retained wallet past its 90
days that still holds coins (an unclaimed paid account, coins at account 0, a failed settlement) is not destroyed, and staff
are flagged once. Any other wallet that will not open keeps its sealed seed and is recorded missing: its file gone, or "Invalid
password" when the market did not begin destroying it (for example a wallet you restored from its seed with a mistyped
`MONERO_WALLET_SECRET`, another install's file of the same name, or a damaged keys file). A missing wallet past its 90 days is
listed on `/admin`, where an administrator confirms its destruction (password step-up, audited) once you have checked that its
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
  primary address, the fee address, or one used before).
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
  marker again (*Restore*, above) so that the sweep raises its lookahead. Past its 90 days it is listed on `/admin` for an
  administrator to confirm its destruction, as under *Destruction* above. A file the market did not create that is
  found under a wallet's name, and does not open with the derived password, is recorded as an orphan, never opened, and
  announced the same way.
- *Other flags*, each noted on the order or sent to the administrators once: a retained wallet past its 90 days that still holds
  coins (it is not destroyed), a deposit with an unlock time (never counted), a
  deposit the database refused as another order's account (recorded once per deposit; staff record what was done about
  it on the order page, which clears its escrow alert, A-480), coins at an account no order claimed or at account 0 (which the
  market never spends), a settlement whose build failed a check (the fee above `ESCROW_XMR_MAX_FEE`, a claimant output below
  0.001 XMR, a transaction that does not match its plan, an address mismatch) and a settlement spent outside the market.
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
tests with PostgreSQL against a fake wallet-rpc, fifteen legs against a real fakechain `monerod` and `monero-wallet-rpc`
0.18.5.1 (`scripts/test-escrow-xmr-regtest.sh`, run by the `escrow-xmr-regtest` CI job;
[runbook](testnet-runbook.md#9-manual-monero-escrow-fakechain-check)), and a simulated-RPC browser journey added in this
phase. Nothing has run on stagenet, testnet or mainnet, with Monero multisig, or as a deployment.

#### Cutting over to escrow only

There is no cutover to perform in this build: escrow is the only payment path, so a coin's intake is **Escrow** or
**Paused** (*Admin → New escrow checkouts*, [Payments](#payments)), and `POST /orders/pay` is removed (it answers
404, nothing is written). A fresh database starts with each coin on Escrow. An installation upgraded from the earlier
builds had its old *On* setting turned into Escrow by migration 107 and any pause left in place; a database that
still held pooled history was refused (migrations 107 and 108,
[UPGRADING sections 58 and 59](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448)). Choosing Escrow
for a coin does not turn that coin's escrow on: the configuration sections above do, and buying in a coin whose
escrow is off is unavailable.

Before leaving a coin on Escrow for buyers:

1. Turn its escrow on (`ESCROW_BTC=on` or `ESCROW_XMR=on`, sections above) and check that `/admin` shows it working. With
   a coin's escrow off, nothing can be bought in that coin; the readiness panel says so.
2. Read *Escrow readiness* under the intake forms. It counts and lists the active listings that cannot use escrow
   (a digital listing without automatic delivery content, a physical listing without a dispatch window, a service
   listing) and the vendors with active listings but no active Bitcoin escrow key (Bitcoin escrow needs one from each
   party; Monero escrow needs none). Ask those vendors to fix their listings and register their keys first: until they
   do, their listings cannot be bought in that coin.

What buyers and vendors see:

- A listing that can use escrow is bought through escrow checkout; its draft's order page offers it, not a payment
  address request.
- A listing that cannot use escrow stays visible in the catalog. Its page says "Not available for purchase in BTC" (or
  XMR) with the reason, and the checkout page offers only the coins it can still be bought in; with every coin blocked
  there is no buy link and no draft form, and a draft posted anyway is refused (409). Its vendor reads the same on the
  listing page, with what to do: add delivery content, set a dispatch window, register an escrow key, or (when the
  coin's escrow is off) that only the operator can change it.
- A draft saved before the pooled removal says why it cannot be paid and, when its listing cannot use escrow, why it
  cannot be paid at all; the buyer can cancel it.

Rolling the binary back to an older build is covered in UPGRADING.md (section 37): an older build reads Escrow as
Paused, for escrow checkouts too, and that section gives the SQL step to run between stopping this build and
starting the older one.

While a coin's escrow is on but not ready (its node or wallet-rpc unreachable at startup, say), the readiness panel
reads "on (not ready: …)" with the state, and for Monero escrow an alert under *Escrow status and payment intake* names the state and
what to do (QA4W4B-1). A checkout in that coin is refused until it is ready; listings keep their buy links meanwhile.

How it was tested: `TestEscrowIntakeOnlyAllowsEscrowOrPaused`, `TestEscrowOrderActionMatrix`,
`TestPaymentIntakeConfirmedReceiptAndLostAcknowledgement`, `TestPreviewAdminRendersWithoutEscrowReadinessData` and
`TestVendorReadinessTracksEscrowIntakeWithoutLeakingHistoricalAddress` (PostgreSQL with the simulated escrow nodes;
simulated RPC, not chain evidence), and `tests/e2e/db-escrow-cutover.spec.ts` (the form and the panel in Chromium at
320 px with JavaScript off, on a market with no escrow).

#### Draining the pooled wallet

This build cannot drain a pooled wallet, and nothing here tests or plans one (owner directive, 2026-10-11): there is
no pooled code, no *Pooled wallet drain* panel on `/admin` and no payout to hold or record by hand. Migration 107
refuses to start on a database whose pooled obligations are not settled, and migration 108 on any database that
holds a `payouts` or `payment_addresses` row (both tables then refuse new rows). If your database was refused:

1. Do not edit it. Restore the earlier build that wrote it (the migration-106 release), settle every open pooled
   order and pay out every payout with that build's own tools and its own guide (section *Draining the pooled wallet*
   of `docs/operator-guide.md` at that build, for example at commit `5a0dabcb`), move what is left in the pooled wallet
   yourself with the wallet's own tools, and take a backup.
2. Then follow [UPGRADING section 58](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448) and
   [section 59](../UPGRADING.md#59-pooled-history-refused-migration-108-a-492). A database with pooled history that is
   not erased cannot be used with this build; a new database is the supported start.
3. Remove `BITCOIN_RPC_URL`, `BITCOIN_WALLET`, `MONERO_WALLET_RPC_URL` and `MONERO_RPC_URL` from `.env` and the
   `monero-wallet` profile from `COMPOSE_PROFILES`; the application refuses to start while any of them is set.

### Transparency

`AUDIT_SIGNING_KEY` (64 hex characters, an Ed25519 seed generated by the installer with `openssl rand -hex 32`) enables signed audit exports. It is read only from the environment, never stored in the database; blank or malformed values leave the export visibly unavailable. Changing it changes the public key, so treat it like any long-lived signing key: back it up with `.env` and announce rotations.

**Exports are private operator evidence.** An export names every account's handle and its activity: sign-ins, account changes, order actions, key fingerprints, short order IDs and transaction IDs. Store it like a database backup and never publish it. Visitors do not receive exports; `/canary` publishes only the public key, so that you can prove an export you hold came from this server. `/canary#records` tells users what the server keeps and that there is no account or message deletion.

**Pin the audit public key.** The admin page and `/canary` show the hex Ed25519 public key. Record it once through a channel you trust (for example in the operator-signed canary text) and verify every export you hold against that pinned value, not against the key printed in the `.sig` file:

```sh
go run ./cmd/verify-audit -pub <pinned-hex-key> audit-events-upto-N.jsonl audit-events-upto-N.sig
```

The server prepares and sends one export at a time, retaining its slot until the download completes or fails. Another export receives 503 with `Retry-After: 5`; wait for the active download to finish before retrying. This bounds concurrent export buffers. The export for a given `upto` is byte-for-byte reproducible, so two downloads can be compared. Web exports accept an exclusive `from` and inclusive `upto`; `from=0` preserves the original whole-history export bytes and signature. When the audit log has not changed between requests, adjacent ranges can be concatenated in ID order to reproduce the bounded whole export, but verify each range against its own signature before concatenating. A download cut short leaves a partial file that fails verification with "export sha256 does not match the signature file", the same failure as an altered file: download the same `upto` again (each attempt counts against the 10 downloads per 10 minutes). The export route has its own time limits instead of the server's 12 s request bound and 20 s write timeout: up to one minute to read the events from the database, then 20 seconds plus one second per 32 KB to send the file (a 50 MB export gets about 27 minutes; onion throughput measured 0.19–0.45 MB/s). Every other route keeps the server's limits. **A server compromise can produce validly signed exports**: whoever controls the server or `AUDIT_SIGNING_KEY` can sign an altered history. The signature proves origin from the configured key, not that the log is complete or untampered.


**Host-only ranged export.** For an export from event ID 800 exclusive through 900 inclusive, run the command on the server host from the release checkout. The directory must already exist and should be restricted to the operator (for example `install -d -m 700 /secure/audit`). The JSONL and signature files are created with mode 0600 and refuse to overwrite existing files. Standard output reports the saved paths and reminds you to keep both files private; keep both files private. This command makes a read-only database query and reads the signing key, but starts no HTTP server or wallet:

```sh
set -a
. ./.env
set +a
go run ./cmd/server -export-audit 900 -export-audit-from 800 -export-audit-dir /secure/audit
```

`-export-audit-from` is an exclusive lower bound and `-export-audit N` is the inclusive upper bound; omit `-export-audit-from` to export from the beginning. Keep `DATABASE_URL` and `AUDIT_SIGNING_KEY` in the environment and never put their values in shell arguments or logs. The `-export-audit` command validates its arguments before connecting and writes the range as `800 < event ID <= 900`. Verify the pair with the pinned public key as above.

**Warrant canary.** In the admin page, paste the operator's OpenPGP public key, then paste a statement the operator wrote and signed offline (`gpg --clearsign canary.txt`). The application stores it as pasted only if it verifies, re-verifies it on every view of `/canary`, and shows INVALID with the reason if the key changes or the stored text is altered. Keep the operator's private key off the server; the application never writes or signs canary text.

## Backups and recovery

Install [age](https://age-encryption.org/) and Python 3, then generate an identity offline (`age-keygen -o backup-key.txt`). Store the private key separately from backups and use its public recipient for encryption:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/market.dump.age
```

For external databases also set `BACKUP_DATABASE_URL` and install PostgreSQL client tools matching or newer than the server. `./scripts/install.sh upgrade` backs up through this script too, so on an external database give it both: `BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade` (the value of `DATABASE_URL` in `.env` serves when this host can reach that server; quote it in single quotes); without `BACKUP_DATABASE_URL` it refuses before changing anything. Before it creates a temporary archive or streams dump data, the script checks free space on the output filesystem against 125% of the source database size plus a fixed reserve. The default reserve is 256 MiB; set `BACKUP_SPACE_HEADROOM_BYTES` to a non-negative byte count to change it, or explicitly set it to `0`. The estimate is deliberately conservative, but concurrent filesystem writers can still consume space after the check. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Its success line reports the `pg_dump` client version actually selected for that backup. Restore reads the producing version from the archive and refuses a selected `pg_restore` older than that version before confirmation or database writes. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage ([commands below](#secrets-env-and-onion-identities)). Database dumps do not include onion keys or blockchain volumes, and in particular **not the custodial payment wallets** (the `bitcoin_data`, `monero_wallet` and, with Monero escrow, `monero_escrow_wallet` volumes, or your external wallets): back those up on their own (the escrow wallets: see the [runbook](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)). Maintain offline copies and rehearse recovery.

Run database backups on a schedule that matches your recovery-point needs. For example, this cron entry runs daily at 02:15 in the cron daemon's local time and creates a dated archive whose filename uses UTC (replace the recipient and checkout path):

```cron
15 2 * * * cd /srv/opsecmkt && AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh "backups/market-$(date -u +\%Y\%m\%dT\%H\%M\%SZ).dump.age" >> backups/backup.log 2>&1
```

For an external database, keep `BACKUP_DATABASE_URL` in a root-owned mode-0600 environment file or wrapper and have the cron job invoke that; do not put its credential in the crontab. Copy completed `.age` files unchanged to an approved off-host encrypted-storage location, for example `rsync -a -- backups/market-YYYYMMDDTHHMMSSZ.dump.age backup-host:/srv/opsecmkt/backups/` using the exact archive name. Periodically verify an off-host copy by restoring it to a new disposable database as shown below. Choose a retention period to match your needs. This example only lists old candidates; review the list and confirm a verified off-host copy before manually removing anything. It does not delete backups:

```sh
KEEP_DAYS=90
find backups -maxdepth 1 -type f -name 'market-*.dump.age' -mtime "+$KEEP_DAYS" -print
```

Rehearse recovery on another host, or in a second checkout with its **own Compose project name**. Compose names containers, volumes and networks after the project (`opsecmkt` unless `.env` sets `COMPOSE_PROJECT_NAME`), not the directory: a second checkout under the same name recreates the live containers with its own secrets, and its `docker compose down --volumes` deletes the live volumes. Install the rehearsal copy with, for example, `COMPOSE_PROJECT_NAME=opsecmkt-rehearsal APP_PORT=8081 ./scripts/install.sh` (add `--local` for a quick local copy); the installer saves the name in that checkout's `.env`, so its later `docker compose`, `backup.sh` and `restore.sh` commands stay on the rehearsal project, and it refuses to install when containers of the chosen project were created from another directory, or when any of that project's volumes exist. If you copy a `.env` into the rehearsal checkout instead, change `COMPOSE_PROJECT_NAME` (add the line if it is missing) and `APP_PORT` in the copy before running any `docker compose` command there. On the same host, also drop `compose.nodes-tor.yaml` from the copy's `COMPOSE_FILE` (or the `bitcoin` and `monero` profiles from its `COMPOSE_PROFILES`): its pinned node network `10.199.42.0/28` already exists for the live project, and Docker refuses a second one as overlapping ([Local nodes over Tor](#local-nodes-over-tor); a different subnet means editing `compose.nodes-tor.yaml` and `deploy/torrc-socks` together). Never change `COMPOSE_PROJECT_NAME` on an existing install: it would start on new, empty volumes (`<project>_postgres_data` and the others).

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

The script sets a persistent recovery gate that pauses every escrow watcher's building, co-signing and sending, and holds every restored escrow settlement that has a stored transaction (Bitcoin settlements and their fee bumps, and Monero settlements; error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the node or wallet shows no broadcast of it. It also retires free Monero escrow accounts, moves active and sealed Monero escrow wallets to closing, and makes escrow checkout wait for a sweep that finds escrow wallets created after the backup. The script prints how many settlements it held, and adds one audit row recording the gate and those counts. (Pooled payouts no longer exist: migration 108 refuses a database that holds any, so there are no payout rows to hold.) It also prints `<N> images restored from a backup taken <time>; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again` (the time is when the dump was made, in UTC, read from the dump itself; "at an unknown time" if the client tools cannot read it; for a backup made before this release on a host not set to UTC it is off by that zone's offset from UTC): a staff takedown or an owner's removal deletes the image's row, so the restored database has every photo and avatar the backup had, including those removed since. Before users return, have a moderator go through Recent photos and take down again any image that had been removed. A database backup may predate an already-broadcast settlement, so reviewing only
the restored settlement rows is insufficient. Follow the [complete reconciliation and explicit unlock procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
before allowing user writes or restarting settlement sends. Validate recovered accounts, listings, orders and
settings before switching traffic. Never test a restore against your live database; a test restore in a
second checkout on the same host needs its own `COMPOSE_PROJECT_NAME` (for example `opsecmkt-rehearsal`, see
[above](#backups-and-recovery)), or its `docker compose` commands act on the live containers and volumes.

**Any restore not done by `scripts/restore.sh` skips this protection.** A host or volume snapshot (including a copy of the `postgres_data` volume), a managed-database point-in-time recovery or a manual `pg_restore` brings escrow settlements back with no recovery gate and no holds, and the escrow watchers broadcast or relay them as soon as the application starts, even those already sent after that point in time. After such a restore keep the application stopped and apply the protection SQL in [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database before starting it, then follow the same reconciliation procedure.

### Secrets, .env and onion identities

`scripts/backup.sh` dumps the database only. A restored database also needs the `.env` it ran with, and each onion address exists only as the keys in its Tor volume. Back both up whenever they change, encrypted to the same age recipient, and store them with the database backups (never next to the age identity).

| Secret | Kept in | Backup | Rotation |
| --- | --- | --- | --- |
| `APP_SECRET_KEY` | `.env` | with `.env` | Derives the CSRF/CAPTCHA key and the key that encrypts TOTP secrets. Restore a database with the key in use when it was dumped; under another key its TOTP secrets cannot be read. Changing it refuses every open form once and makes enrolled TOTP secrets unreadable. On a market running [Bitcoin escrow](#bitcoin-escrow-test-networks-only), it also changes the immutable HMAC pin for the arbiter account key and (when configured) the escrow signer account key, disabling escrow. Restore the original key root; never clear or replace these pins while any escrow exists. Party-key rows are sealed and HMACed under that root too. A row that remains unreadable can be re-registered only once the original root is in use and startup has verified the pinned node and, when configured, signer; the app confirms the key derives the existing escrow child before activating it. Rotate only when it is exposed, with the consequences [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token) describes. An install that predates it has none: its keys still derive from `SETUP_TOKEN` (the startup log says `keys: derived from SETUP_TOKEN (legacy)`); to move to it, set `APP_SECRET_KEY` to exactly the current `SETUP_TOKEN` value, which `./scripts/install.sh upgrade` does ([UPGRADING.md](../UPGRADING.md#app_secret_key-and-stricter-startup-checks)). |
| `SETUP_TOKEN` | `.env` | with `.env` | Authorizes the first-run `/setup` page only. The installer prints it once at the end of `init`; `./scripts/install.sh setup-token` shows it again (and `--new` reissues it) while setup is pending. The app refuses to start without it until setup is complete, and ignores it afterwards (while it is set, startup logs a hint); `/setup` then answers 403 whatever the token. Once `APP_SECRET_KEY` is set and setup is complete, `status`, `add-mirror` and `upgrade` remove it from `.env`. Without `APP_SECRET_KEY` it is also the legacy key root above: keep it, or copy it to `APP_SECRET_KEY` first; changing it then has every consequence listed for `APP_SECRET_KEY`, escrow keys included. |
| `MONERO_WALLET_SECRET` (only with [Monero escrow](#monero-escrow-fakechain-or-stagenet)) | `.env` | with `.env` | Every escrow wallet's password derives from it (HMAC-SHA256 with the wallet's name), so no wallet password is stored. It must be its own secret, not `SETUP_TOKEN` or `APP_SECRET_KEY`, so rotating those never locks wallets. A verifier of it is pinned in the database on the first start; with another secret Monero escrow stays disabled and opens or creates no wallet. **No tool re-keys existing wallets**: restore the original secret. Never change it while wallets exist. The installer generates it with `--monero-escrow yes`. |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` (only with the `monero-escrow-wallet` service) | `.env` | with `.env` | The escrow wallet-rpc's `--rpc-login` password, the same as in `ESCROW_XMR_WALLET_RPC_URL`; generated by the installer. Rotating it means changing both and restarting the service and the app. |
| `AUDIT_SIGNING_KEY` | `.env` | with `.env` | Signs audit exports ([Transparency](#transparency)). A new key (`openssl rand -hex 32`, then restart the app) is a new public key on `/canary`: announce it. |
| `POSTGRES_PASSWORD` (internal database) | `.env`, twice: `POSTGRES_PASSWORD` and the password in `DATABASE_URL`; the database keeps its own hash in the `postgres_data` volume | with `.env` | `POSTGRES_PASSWORD` is used only when an empty volume is initialized, so editing it later changes nothing. Rotate in the database first: `docker compose exec db psql -U opsecmkt -d opsecmkt -c '\password opsecmkt'` asks for the new password twice and sends `ALTER ROLE opsecmkt PASSWORD` with it already hashed, so it appears in no command line or server log. Then put it in both places in `.env` and run `docker compose up -d`. A hex password (`openssl rand -hex 24`) needs no escaping in the URL. For an external database, change it on that server and in `DATABASE_URL`. |
| `BITCOIN_RPC_PASSWORD`, `BITCOIN_RPC_AUTH` (local Bitcoin) | `.env`: the operator password of the local node (the `marketplace` user `-rpcauth` creates; it is not an app credential and is never in `ESCROW_BTC_RPC_URL`); `BITCOIN_RPC_AUTH` is its salted HMAC verifier, passed to Core as `-rpcauth` instead of a password argument | with `.env` | Change the password and the verifier together, then `./scripts/install.sh check` before `docker compose up -d`. `upgrade` derives a missing verifier from the current password without rotating it ([UPGRADING.md](../UPGRADING.md#54-local-bitcoin-rpc-uses-a-salted-verifier-a-144)). A weak manually chosen password may be guessed offline from the verifier. |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` (local Monero escrow wallet-rpc) | `.env`, repeated in `ESCROW_XMR_WALLET_RPC_URL` | with `.env` | Change the password and the same value in its URL together, then `docker compose up -d`. |
| Onion keys | the `tor_identity` volume (`tor_mirror_identity` for the mirror), named `<project>_tor_identity` by Compose, for example `opsecmkt_tor_identity` | the commands below | Losing or replacing the keys changes the address for good. A copied key lets anyone impersonate the address, so retire an exposed one: remove the volume, let Tor create a new address and publish it in your signed verification document. |

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
  docker run --rm --pull never --network none --cap-drop ALL --security-opt no-new-privileges:true -v "$volume:/var/lib/tor:ro" \
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

Restore an onion identity (set `identity`, `backup` to its backup file and `AGE_IDENTITY`). The archive is decrypted straight into a container; it runs as root (`--user 0`) so it can write into the volume whatever its current owner, then gives the files to `debian-tor` with mode 700, because Tor refuses keys it does not own. It prints the restored address:

<!-- ops-cmd: tor-identity-restore -->
```sh
( set -eu -o pipefail
  : "${identity:?set identity=tor_identity or identity=tor_mirror_identity}" "${backup:?set backup to the identity backup file}" "${AGE_IDENTITY:?set AGE_IDENTITY}"
  project=$(docker compose config | sed -n 's/^name: //p')
  volume="${project}_$identity"
  test -f "$backup"
  docker image inspect "$project-tor" >/dev/null
  age -d -i "$AGE_IDENTITY" "$backup" |
    docker run --rm -i --pull never --network none --user 0 --security-opt no-new-privileges:true -v "$volume:/var/lib/tor" \
      --entrypoint sh "$project-tor" -euc '
        if [ -n "$(ls -A /var/lib/tor)" ]; then echo "Refusing: volume $0 is not empty" >&2; exit 1; fi
        tar -C /var/lib/tor -xf - marketplace
        chown -R debian-tor:debian-tor /var/lib/tor
        chmod 700 /var/lib/tor /var/lib/tor/marketplace
        cat /var/lib/tor/marketplace/hostname' "$volume" )
```

After `docker compose up -d`, compare the printed address with `docker compose exec tor cat /var/lib/tor/marketplace/hostname` (`tor-mirror` for the mirror); a different address means Tor did not use the restored keys. The restore refuses a volume that is not empty: Tor has already run on this host and created another address. If that address was never published, remove it and restore again: `docker compose rm -sf tor` (or `tor-mirror`), then `docker volume rm opsecmkt_tor_identity` (your project's volume name). `scripts/test-tor-identity-restore.sh` runs these four blocks verbatim ([operations tests](operations-tests.md#documented-env-and-onion-identity-backup-and-restore)).

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
- `57014` (statement timeout, 8 s) or `55P03` (lock timeout, 3 s) with status 503 means the application pool's [session timeouts](#migrations) stopped a query: the database is overloaded or a row stayed locked. Occasional lines under load are expected; a steady stream on one route points at a slow query.
- `57P01`, `57P02` or `57P03` (the database is shutting down, crashed or still starting), `53300` (too many connections), `08000`, `08001`, `08003`, `08004` or `08006` (a connection exception) or `db-connection`, with status 503, means the request's database connection was lost or refused, for example during a PostgreSQL restart or failover; the user is shown "The market is busy" or, when the first query of the request already failed, "Service unavailable", both with `Retry-After: 5`. A steady stream outside a restart points at the database's health or its `max_connections`.

**Overload.** The app accepts at most 2048 connections at once and serves at most 64 pages, 16 form posts that change a signed-in account's data and 16 public ones (sign-in, registration, the sign-in challenge, whoever sends them), 16 requests for public images (`/img/`, which take no page place, so a page of thumbnails or a flood of image requests cannot crowd out pages), 8 CAPTCHA images and 4 password checks at a time. A form post takes its place only after its body has been read (at most 64 KiB and 64 fields, within the 15 s read timeout) and its origin and form token have been checked, so a client that sends its body slowly, or posts junk, holds only a connection (a post that carries a session cookie then looks it up in one of 16 lookup places, waiting its turn for up to 2 s; pages never use them); sign-in and registration posts with a malformed handle or password, or a CAPTCHA that was not issued to that browser or has expired, are refused before they take a place. Public posts, anonymous or from a signed-in account, therefore cannot crowd out a signed-in user's or an administrator's changes (each account's own changes stop at its budget of 100 in ten minutes), but a flood of well-formed public posts can still delay real sign-ins. While all 2048 connections are taken, HTTP keep-alive is switched off (idle connections are closed) until no more than 1536 are in use. A request beyond the other limits is answered at once with 503 "The market is busy. Try again in a few seconds." and `Retry-After: 5`, and its connection is closed; these refusals are **not logged**, so a flood shows up as users reporting that message, not as log lines. Every other 503 also carries `Retry-After: 5`, and every 429 `Retry-After: 600`, except the one refusing a second payment request while the same account's first is still in progress, which carries `Retry-After: 5`. That one-at-a-time limit is per app process: with several app instances against one database, each allows one. `/healthz` takes no slot and pings the database on its own connection, so the container stays healthy under a flood of pages; it still waits for one of the 2048 connections. These limits are fixed in the code; on Tor they sit behind the onion service's own defences.

The escrow watchers log what they do to an escrow in lines that start `escrow:` and name the order by its first 8 characters and a class, never the node's text (which can quote an address): for example `escrow: settlement of order 1b4d8036 not sent yet: <class>`, `escrow: Bitcoin escrow node not synced (<reason>); escrow watcher passes skipped until it is` and `escrow: Bitcoin escrow on (test networks only)`. A state change is logged once, not on every retry. The pooled build's `payout id=… outcome=…` and `payment watcher: …` lines no longer exist. `db-connection` (or `timeout`) alone means the watcher could not reach the database, or could not finish its pass within 5 minutes (for example while the pool was exhausted); the next pass picks up what it left; the line never holds `DATABASE_URL`, its user, database name, host or port. The recovery code reveal sweep logs `recovery code reveal sweep: <class>` the same way.

When the database connection fails at startup the server exits with its class and no part of `DATABASE_URL`: `authentication failed (SQLSTATE 28P01)` or `(SQLSTATE 28000)` for a wrong user or password or a `pg_hba.conf` rule, `database missing (SQLSTATE 3D000)`, `host not found`, `connection refused`, `timed out`, or `TLS required` (the server accepts only encrypted connections; set `sslmode=require` or `verify-full`).

**Image scanner.** Photo and avatar uploads need the ClamAV scanner: `CLAMD_ADDRESS` is clamd's `host:port` on the scanner network (the installer writes `clamd:3310`; empty turns uploads off), and `SCANNER_MAX_SIGNATURE_AGE_DAYS` (default 30, from 7 to 90) is how old the daily signatures may be before uploads pause. The app refuses to start with a malformed value of either, naming the variable. No setting lets an upload skip the scan. The app asks clamd for `PING` and `VERSION` every 30 seconds; `/admin#image-scanner` shows the result, and each change of state writes one line:

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

**Photo storage and upload limits** (A-186). Stored photos and avatars are re-encoded JPEGs in the `images` table, so they are in every database backup. `IMAGE_STORAGE_LIMIT_MB` (default 2048, from 1 to 1048576; the app refuses to start with anything else) caps what all of them may use together; once an upload would pass it, uploaders see "Photo storage on this market is full; contact the operator" and nothing is stored. `/admin#image-scanner` shows how many are stored and the megabytes they use against the limit. The other limits are fixed: 4 MB a file, 4096 pixels a side and 4096×3072 pixels in all (about 12.6 megapixels, so a 4032×3024 phone photo fits), 64 MiB of estimated decode memory (a progressive colour JPEG reaches it at about 4.4 megapixels with full-resolution colour, 7.4 with the usual 4:2:0), 100 JPEG scans, 4 photos a listing, one avatar an account, 50 MB of photos and avatar an account, 20 uploads an account in 10 minutes and one at a time, and 2 uploads (one being decoded) at once for the whole app; a busy upload slot answers 503 with `Retry-After: 5`. An admitted upload may take 90 seconds to arrive, longer than the server's 15-second limit for other requests. Upload memory is budgeted beside `maxConns` in `cmd/server/main.go`: about 220 MiB at most (the 64 MiB decode estimate bounds what the decoder allocates), plus at most 96 MiB of image bytes held by image responses being written (A-187: 64 MiB on `/img`, 32 MiB on `/desk/img`), about 320 MiB in all, inside the app's 400 MiB `GOMEMLIMIT`. A refused or failed upload stores nothing and logs no file name; a failure of the database while storing one logs the usual `http 5xx kind=action name=/listings/photo` (or `/account/avatar`) line with the reference the uploader sees.

**Serving images** (A-187). Stored images are served only at `/img/<id>-t` or `-f` (public: a published listing's photos, vendors' avatars, and staff avatars only while the public staff list is on and that account is not hidden, both off by default) and `/desk/img/<id>-t` or `-f` (signed in: the owner's own, any for an administrator, a staff avatar for the parties of an order that staff member decided and for moderators, once that member has decided a dispute, and an archived listing's photos for whoever may open that listing's page through an order on it). Anything else gets the same 404 as an unknown image id, and a POST gets 405. Responses still being written hold at most 64 MiB of image bytes on `/img` and, separately, 32 MiB on `/desk/img`, so signed-in requests cannot use up the public route's share; beyond that an image request gets the busy 503 with `Retry-After: 5` (not logged), so visitors reading slowly, or not at all, cannot exhaust the app's memory, while pages are unaffected. A visitor's browser may reuse a response for a day without asking again and keep it on disk until its cache is cleared (`Cache-Control: private, max-age=86400, immutable`; `max-age` sets freshness, not how long a copy stays); a reverse proxy in front of the app should not cache them (`private`) and must not add `ETag` or cookies. A database failure while serving one logs `http 5xx kind=page name=/img/` (or `/desk/img/`), never the image id.

**Public staff list** (A-189). `/admin#staff-list` has one switch, "Show a public staff list", off on every install until you turn it on (with your password, and authenticator code when enrolled; each change is in the audit trail as "Public staff list changed from off to on" or back). While it is off, `/canary` lists no staff and no staff avatar is served on the public image route. While it is on, `/canary#staff` lists each moderator and administrator who is not suspended and has chosen to be listed, with their handle, role and avatar; every staff account starts hidden, and a hidden member is not listed at all. Whatever the switch, the staff member who records a dispute decision has their avatar shown beside it on that order's page, to its buyer and vendor and to staff. Tell staff before they upload an avatar that a photo links them to every decision they make.
