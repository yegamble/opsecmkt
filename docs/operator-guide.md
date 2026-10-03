# OPSEC Market operator guide

Deployment, feature configuration, migrations and recovery reference. Start with
[the README](../README.md) for product context and local development. Commands
below run from the repository root.

## Installer

`scripts/install.sh` installs a checkout and maintains it afterwards. It fetches nothing itself (Docker builds and pulls the images). After `init` it changes `.env` only by rewriting the lines it names through a temporary file moved into place (mode `0600`; every other line stays byte for byte). It prints one secret, the setup token, and only at the end of `init` and from `setup-token`.

| Command | What it does |
| --- | --- |
| `init` (the default: `./scripts/install.sh` and `./scripts/install.sh --local` run it too) | Refuses when `.env` exists, when containers of the Compose project come from another checkout, and when any volume of the project exists. Asks its questions (none when flags answer them), writes `.env` with distinct random `APP_SECRET_KEY` and `SETUP_TOKEN`, runs `check`, then `docker compose up -d --build --wait`. With `--external-db` it first reads that database with a one-off `docker compose run --rm --no-deps app -status`: when a market's setup is already complete there, it removes the new `.env`, starts nothing and says to restore that market's `.env` instead (new secrets would leave its TOTP secrets unreadable). On Tor it waits up to 90 seconds for the onion address. It ends with the setup link and token, and for clearnet behind a proxy with Caddy and nginx snippets for `APP_PORT`. |
| `init --admin HANDLE [--site-name NAME]` | The same, but creates the first administrator in the terminal ([First administrator from the host](#authentication)): it asks for the password twice without showing it and pipes it to `-bootstrap-admin` on standard input, never in a command line, the environment or `.env`. Then it removes `SETUP_TOKEN` from `.env`, recreates the app and prints no token. |
| `check` | Validates `.env` and the Compose configuration and starts nothing: the app's startup refusals (placeholder or weak secrets, `COOKIE_SECURE`, a placeholder `DATABASE_URL`, `APP_MODE` against `COMPOSE_FILE`), `POSTGRES_PASSWORD` against the password in `DATABASE_URL`, `COMPOSE_PROFILES` against the database and node settings, node images on `latest`, the mirror outside Tor, the [malware scanner](#photo-uploads-and-the-malware-scanner) (its profile, `CLAMD_ADDRESS` and `compose.scanner.yaml` only together, a readable `deploy/clamd.conf`, `CLAMAV_IMAGE` pinned to a release with signatures and, from `docker compose config`, `clamd` alone with the app on the internal `scanner` network with no port or volume), the project's containers created from another checkout (or this checkout's containers under another project name), and `docker compose config`. When the directory those containers were created from no longer exists, the checkout was probably moved: `check` says so, and `docker compose up -d --force-recreate` run in the moved checkout recreates them with its directory, keeping the volumes (a plain `up -d` leaves them as they are). Do not change `COMPOSE_PROJECT_NAME` for this. Messages name keys, never values. |
| `status` | Mode, each service's health, whether setup is pending or complete, the schema version, every onion address and the malware scanner's state with the version and date of its signatures. |
| `setup-token [--new]` | While setup is pending: shows the token and setup link, or with `--new` issues a new token and recreates the app. After setup it exits 1: the token grants nothing then. It never shows a token that is also the key root (an install without its own `APP_SECRET_KEY`); `--new` issues a separate one and keeps the keys. |
| `add-mirror` | See [Optional onion mirror](#optional-onion-mirror). |
| `add-scanner`, `refresh-scanner` | Add the ClamAV malware scanner that photo uploads need, and refresh its signatures: see [Photo uploads and the malware scanner](#photo-uploads-and-the-malware-scanner). |
| `upgrade [--no-backup]` | After `git pull`: see [UPGRADING.md](../UPGRADING.md). |

Flags answer `init`'s questions: `--tor` or `--clearnet`; `--local` (loopback HTTP, internal database, no nodes); `--external-db URL` (without it, a flagged install gets the internal database); `--behind-proxy yes|no` (clearnet: secure cookies behind an HTTPS proxy, or local HTTP only); `--nodes local|none|custom` (`local` runs pruned Bitcoin testnet4 and Monero stagenet nodes with the [default images](#node-image-versions), `custom` asks per coin); and per coin `--bitcoin` or `--monero` `local|external|disabled`, `--bitcoin-image`, `--bitcoin-network testnet4|signet`, `--bitcoin-prune yes|no`, `--bitcoin-rpc-url`, and the Monero equivalents with `--monero-wallet-rpc-url`. `--uploads yes` also sets up the [malware scanner](#photo-uploads-and-the-malware-scanner) as `add-scanner` does (the default, `no`, leaves it out and asks nothing). `./scripts/install.sh --help` lists them. A question without its flag is asked. A value given as a flag, such as an RPC URL with a password, is visible to other local users while the installer runs; answer the question instead to keep it off the command line.

The setup token opens `/setup` once. After setup, `status`, `add-mirror` and `upgrade` remove it from `.env` when `APP_SECRET_KEY` is set and the running app reports `keyroot=app_secret_key` (the next `docker compose up -d` drops it from the running app); it grants nothing by then. An app started before `.env` had `APP_SECRET_KEY` still derives its keys from the token, so the token stays until the app is recreated.

Commands that change `.env` (`init`, `setup-token --new`, `add-mirror`, `add-scanner`, `upgrade`, and `status` when it removes the token) hold the lock directory `.env.lock` while they run, so two of them cannot overwrite each other's changes; a second one refuses with a message (`status` only leaves the token). If a killed run left the lock behind, remove it with `rmdir .env.lock`. The installer switches off shell tracing, so `SHELLOPTS=xtrace` cannot print its secrets.

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

`APP_MODE` in `.env` (`clearnet` or `tor`, set by the installer) is only checked at startup: any other value stops the server, but it changes no behaviour. The Compose overlay in `COMPOSE_FILE` decides exposure, and `COOKIE_SECURE` decides whether cookies are marked Secure. Compose passes `COMPOSE_FILE` to the app, which refuses to start when it disagrees with `APP_MODE`: `tor` needs `compose.tor.yaml` in it, `clearnet` needs `compose.clearnet.yaml`, and never both (a blank `COMPOSE_FILE` skips this check). `COOKIE_SECURE` must be exactly `true` or `false` (unset means `true`); `APP_MODE=tor` with `COOKIE_SECURE=true` only logs a warning: the onion is served over plain HTTP, so the Secure session cookie works only in browsers that treat `.onion` addresses as secure (the installer writes `false` for Tor).

Configure DNS, firewall and the certificate issuer for your domain. A proxy in another container needs a private shared network instead of this host-loopback configuration. Never expose the database or node RPC ports.

**External PostgreSQL and time zones.** Every database connection the app opens (the web app, its migrations, `-bootstrap-admin`, `-reset-admin-password`, `-reset-admin-factors` and `-set-role`) sets `TimeZone=UTC` itself, overriding the server, database and role defaults and any `TimeZone` in `DATABASE_URL`. An external or host PostgreSQL left on local time therefore needs no change: pages show every time in UTC, labelled " UTC", and do not reveal the server's UTC offset. Other clients, such as `psql` or the backup scripts, still use the server's own zone.

For onion service deployment run `./scripts/install.sh init --tor` (or choose `tor` in the installer). The base and Tor Compose files publish **no host ports**; Tor connects to `app:8080` on the private Docker network. The installer prints the onion address, and `./scripts/install.sh status` shows it later. By hand:

```sh
docker compose exec tor cat /var/lib/tor/marketplace/hostname
```

The persistent `tor_identity` volume contains the onion private keys. Preserve it to retain the address; back it up encrypted with the [documented commands](#secrets-env-and-onion-identities). The setup uses HTTP inside the authenticated onion circuit and sets `COOKIE_SECURE=false` for that endpoint. The application and database share an internal Docker network with no direct Internet egress. Only the Tor gateway has a separate egress network. Selecting external database or RPC endpoints adds `compose.external-egress.yaml`, which explicitly permits direct application egress; that configuration provides onion-only inbound exposure, not Tor-routed outbound traffic. Local nodes have their own direct egress network to synchronize with peers. Docker network isolation does not protect against host administrators or a compromised container runtime. Use the dedicated Tor Compose file without the clearnet overlay. If changing an existing clearnet deployment, stop it before changing `COMPOSE_FILE` and recreate containers and the backend network so published ports are removed and the network becomes internal. Remove only the unused network, not persistent volumes. Never run both overlays together.

**Tor restarts with the app; this needs Docker Compose 2.17 or later** (check with `docker compose version`). Tor looks up the app's container address once, when it starts ([`deploy/torrc`](../deploy/torrc): `HiddenServicePort 80 app:8080`), and a recreated app container can get a different address, for example when an upgrade adds node services. Tor then keeps forwarding to the old address and the onion stops answering. `compose.tor.yaml` therefore gives `tor` and `tor-mirror` a `restart: true` dependency on the app: whenever Compose recreates the app (`docker compose up -d` after a rebuild or a `.env` change) or restarts it (`docker compose restart app`), it restarts Tor too. The mirror is included only when the command sees the `mirror` profile, so add `mirror` to `COMPOSE_PROFILES` in `.env` if you run it (see [Optional onion mirror](#optional-onion-mirror)). Compose does not restart Tor for `docker compose stop app` followed by `up -d`, and the setting does not repair a Tor that already forwards to an old address. On a Tor install deployed before this change, recreate Tor once:

```sh
docker compose up -d --force-recreate tor              # without the onion mirror
docker compose up -d --force-recreate tor tor-mirror   # with the onion mirror
```

Name `tor-mirror` only if you run the mirror: naming it starts it, which creates a second onion address. **If the onion does not answer after any change, recreate Tor** with the same command. Recreating keeps the `tor_identity` and `tor_mirror_identity` volumes, so the addresses stay the same.

## Optional onion mirror

In a Tor deployment, create a second onion address with its own persistent identity:

```sh
./scripts/install.sh add-mirror
```

It adds `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there, starts the mirror, waits up to 90 seconds for its address and prints both addresses and the command that backs up the mirror's keys. It refuses on a clearnet install, and running it again changes nothing. To do the same by hand, first add `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there: the installer's default Tor install has `COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet'`, which becomes `COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet,mirror'`; `COMPOSE_PROFILES='internal-db'` becomes `COMPOSE_PROFILES='internal-db,mirror'`, and an empty value becomes `COMPOSE_PROFILES='mirror'`. Then:

<!-- ops-cmd: onion-mirror -->
```sh
docker compose config --services | grep -x tor-mirror   # prints tor-mirror; nothing means .env lacks the profile
docker compose up -d
docker compose exec tor-mirror cat /var/lib/tor/marketplace/hostname
```

Wait for the mirror's Tor process to initialize before reading its hostname. Do not start the mirror with a `--profile mirror` option instead: an option replaces `COMPOSE_PROFILES` for that command rather than adding to it, so with the internal database the command fails (`service "app" depends on undefined service "db"`), and a mirror started that way is left out of later `docker compose up -d` runs. With the profile in `.env`, every `docker compose` command includes the mirror and Compose restarts its Tor whenever it recreates the app (see [Clearnet and onion deployment](#clearnet-and-onion-deployment)). If you already run a mirror that is not in `COMPOSE_PROFILES`, add it now. This optional profile publishes no host ports and reaches the same application/database over the private backend network. The second address is a same-host alternate URL; it does not provide independence from host, application or database failures. Preserve the separate `tor_mirror_identity` volume to retain its address, and back it up like the main one ([Secrets, .env and onion identities](#secrets-env-and-onion-identities)).

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

put it in `.env` (the value below is only an example), then run the `docker compose build tor` sequence under [Onion-service defences](#onion-service-defences):

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
| `bitcoin` | 2g | 2 | 256 | A pruned test-network node: its database cache and mempool (at most 300 MB by default) plus headroom for the initial sync. Raise it for an unpruned node or a larger `-dbcache`. |
| `monero` | 2g | 2 | 512 | monerod starts threads per core for sync and verification. |
| `monero-wallet` | 512m | 1 | 128 | |
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

The installer asks for each coin (bare `./scripts/install.sh`, or `init --nodes custom`; `--nodes local` takes every default below) whether to run a `local` node in Docker (the default; pruned unless you answer `no`, see [Local node pruning](#local-node-pruning)), use an `external` node you run elsewhere, or leave that coin `disabled`. `scripts/install.sh --local` (developer mode) never starts nodes. For a local node, the installer defaults to [`bitcoin/bitcoin:31.1`](https://hub.docker.com/r/bitcoin/bitcoin) for Bitcoin Core and Seth for Privacy's [`simple-monerod:v0.18.5.1`](https://github.com/sethforprivacy/simple-monerod-docker) plus [`simple-monero-wallet-rpc:v0.18.5.1`](https://github.com/sethforprivacy/simple-monero-wallet-rpc-docker) images for Monero (see [Node image versions](#node-image-versions)). Press Enter to use these defaults or supply an image override. The Bitcoin image is community-published and explicitly unofficial; it verifies upstream Bitcoin Core binaries. The Monero images are community-maintained, not published by the Monero project. Review publisher and image provenance before use. A tag can be re-pushed by its publisher; append an immutable `@sha256:<64 lowercase hex characters>` digest in `.env` for a reproducible deployment. Bitcoin and Monero use the same daemon image on test and live networks; the network is selected with daemon flags, not by changing the image. The installer offers Bitcoin `testnet4`, `signet` or `live`, and Monero `stagenet`, `testnet` or `live`. `live` writes `BITCOIN_CHAIN=main` or `MONERO_NETWORK=mainnet` (plus a blank `MONERO_NETWORK_FLAG`, because monerod and monero-wallet-rpc have no `--mainnet` flag) and makes this a [mainnet market](#mainnet). One market cannot mix the two: choosing `live` for one coin and a test network for the other exits before writing `.env`. A mainnet node needs far more disk and a much longer first sync than the test-network estimates below.

Both nodes store their blockchain in dedicated volumes and publish no ports. Even pruned, they need several GB of disk and hours for the first synchronization (sizes below). Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payments run on test networks unless you configure mainnet** ([Mainnet](#mainnet)). With a wallet RPC configured (see [Payments](#payments)), the app issues deposit addresses, watches confirmations and sends single-attempt releases and refunds from the operator's pooled wallet. That is custodial holding, not multisig escrow, and there is no fee accounting. On a test-network market, do not send mainnet coins. Providers are enabled by environment variables and a restart. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

### Node image versions

The node images default to release tags, never `latest`: `bitcoin/bitcoin:31.1` (Bitcoin Core v31.1), `ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1` and `ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1` (Monero v0.18.5.1). They are the versions the payout failure rules were checked against. When a send fails, the application decides from the wallet's error whether nothing was broadcast (a definite rejection an administrator may requeue) or the send may have gone out (ambiguous: requeueing needs a wallet check, see [Payments](#payments) and the [runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)). The Monero "nothing broadcast" table (`moneroPreSubmitCodes` in `internal/market/payments_watcher.go`: -2, -16, -17, -18, -19, -20, -37) holds for **monero-wallet-rpc v0.18.5.1**; the Bitcoin rule that every `sendtoaddress` error is definite was read from Bitcoin Core v31.1. A newer wallet could raise one of those codes after it has submitted the transaction, and the administrator would then be offered a requeue that pays twice.

`BITCOIN_IMAGE`, `MONERO_IMAGE` and `MONERO_WALLET_IMAGE` in `.env` override the defaults. An `.env` that sets any of them to `:latest` (as the installer did before these tags were pinned) or to no tag keeps floating: change it to the tags above, optionally with `@sha256:<digest>` appended (`docker buildx imagetools inspect <image:tag>` prints the digest). CI's Compose job fails if a default in `compose.nodes.yaml` is `latest` or untagged; it cannot see your `.env`.

Before changing a tag (default or `.env`):

1. Monero: read the new release's wallet source and re-verify every code in `moneroPreSubmitCodes`: each must still be raised only before `wallet2::commit_tx` submits anything (`src/wallet/wallet_rpc_server_error_codes.h`; `validate_transfer`, `on_transfer` and `handle_rpc_exception` in `src/wallet/wallet_rpc_server.cpp`; `commit_tx` in `src/wallet/wallet2.cpp`). Remove any code that can now follow a submission, and update the version and line references in the comment and in [implementation status P5](implementation-status.md#p5-payments). Check that `transfer` still honours `subtract_fee_from_outputs` (every payout, releases included, takes the fee from the amount sent). Set `moneroVerifiedWalletRelease` to the new tag and `moneroVerifiedWalletRPC` to the `WALLET_RPC_VERSION` it defines (`src/wallet/wallet_rpc_server_commands_defs.h`, `(major<<16)|minor`); until both change, the application treats the new wallet as unverified. Move `MONERO_IMAGE` and `MONERO_WALLET_IMAGE` to the same release.
2. Bitcoin: check that `sendtoaddress` still stores the transaction in the wallet before submitting it and returns the txid, not an error, when relay fails (`src/wallet/rpc/spend.cpp`, `CWallet::CommitTransaction` in `src/wallet/wallet.cpp`), and update the `walletRejected` comment.
3. Change the tag in `compose.nodes.yaml`, `.env.example` and `scripts/install.sh` in the same commit. `tests/test_installer.py` fails if the three disagree, if a default is `latest`, or if the versions named in the `payments_watcher.go` comments or `moneroVerifiedWalletRelease` do not match the tags.
4. Run the payment tests (`TEST_DATABASE_URL=<dedicated test database> go test -race -count=1 -timeout 30m ./...` and `E2E_WALLET_DATABASE_URL=<fresh database> npm run test:e2e:wallet`, both against simulated wallets) and, preferably, the [local-chain journey](local-chain-testing.md) against the new images.

**Runtime wallet check.** monero-wallet-rpc does not report its release. Its `get_version` answers the version of its RPC interface as one number, `(major<<16)|minor`, and whether it is a release build; v0.18.5.1 reports 1.31 (65567) from a release build (`WALLET_RPC_VERSION` in [`src/wallet/wallet_rpc_server_commands_defs.h`](https://github.com/monero-project/monero/blob/v0.18.5.1/src/wallet/wallet_rpc_server_commands_defs.h); v0.18.5.0 reports 1.30, v0.18.4.1 to v0.18.4.5 report 1.29). The application reads `get_version` at startup and at the start of every watcher pass. While the answer is anything but 1.31 from a release build, the call fails, or the last check failed before reading it, every Monero send failure is recorded as possibly broadcast (so requeueing it needs the wallet confirmation), and the *Payment providers* panel of `/admin` says "XMR wallet version not verified", names the version the wallet reported and points here; that payouts take the fee from the amount sent is then unverified too. The panel's *Version* column also shows the Bitcoin Core version from `getnetworkinfo`; the Bitcoin rule does not depend on it. The RPC version changes only when the wallet's RPC changes, not with every release, so a later release with an unchanged RPC reports 1.31 and is not detected. With a wallet version nobody has verified (a newer tag, a `latest` left in `.env`, or your own image), **treat every Monero send failure as possibly broadcast** even if the panel shows no warning: check the wallet as for an ambiguous failure ([runbook step 5](testnet-runbook.md#5-recover-a-held-or-failed-payout)) before requeueing a failed XMR payout.

### Local node pruning

Local nodes are **pruned by default**; a full node is an explicit opt-in per coin. The application only needs a node's current state and its wallet: Bitcoin is used through `getblockchaininfo`, `validateaddress` and wallet RPCs (`getwalletinfo`, `loadwallet`, `getnewaddress`, `listreceivedbyaddress`, `gettransaction`, `sendtoaddress`), Monero through the daemon's `get_info` and `monero-wallet-rpc`. Nothing in it reads old blocks.

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

**Wallet restore trade-off.** A Bitcoin wallet can only be loaded or rescanned over blocks the node still holds. Restoring a backup (`restorewallet`) whose last synchronised block is older than the node's `pruneheight` fails with "Prune: last wallet synchronisation goes beyond pruned data. You need to -reindex (download the whole blockchain again in case of a pruned node)" ([`src/wallet/wallet.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.1/src/wallet/wallet.cpp)), and `rescanblockchain` cannot go below `pruneheight` either (UNVERIFIED here: needs a synced, pruned chain). Record the block height next to each wallet backup ([runbook](testnet-runbook.md#6-back-up-the-wallets)) and compare it with `pruneheight` before relying on the backup. To restore an older one, either restore the whole `bitcoin_data` volume from a copy taken with bitcoind stopped (the pruned chain and the wallet in it then agree, and the node catches up from there), or run the node unpruned as above until the wallet has loaded and rescanned, then set `BITCOIN_PRUNE_MB` back. Monero is not affected: a pruned blockchain "is otherwise identical in functionality to the full blockchain" ([README](https://github.com/monero-project/monero/blob/v0.18.5.1/README.md#pruning)) and keeps the full transaction history, so `monero-wallet-rpc` can restore and refresh a wallet through a pruned daemon.

**Mirrors, development and CI stay pruned.** The `mirror` profile runs a second Tor gateway, not a node. Keep the pruned defaults on a Tor-mirror deployment, a secondary or test instance, and any development or CI configuration: `.env.example`, the Compose defaults and CI's Compose check all use them. Choose a full node only where you want to keep the whole chain for your own reasons. The regtest and offline stagenet chains used by the [local chain tests](local-chain-testing.md) are tiny and are left unpruned.

## Migrations

`internal/market/schema.sql` is the frozen baseline. Startup applies it, then every `internal/market/migrations/NNN_name.sql` not yet listed in `schema_migrations`, in numeric order, inside one transaction under an advisory lock, so concurrent app starts are safe and a failed migration changes nothing. A version missing from the table is applied even when higher versions already are. A version in the table that the running server does not include means a newer release migrated the database: the server then exits with an error naming it, before changing or serving anything (see [Rolling back](../UPGRADING.md#5-rolling-back)). Never edit a merged migration; add a new file. Reserved blocks: 001–009 Foundation, 010–019 authentication, 020–029 PGP, 030–039 orders, 040–049 inventory, 050–059 payments, 060–069 transparency. Take an encrypted backup before upgrading; there are no automatic down-migrations.

Startup runs in separately bounded phases, each set by an optional Go duration in `.env` (blank uses the default):

- `DATABASE_CONNECT_TIMEOUT` (default `15s`, from 1s to 10m): reaching PostgreSQL.
- `MIGRATION_LOCK_TIMEOUT` (default `10m`, from 1s to 24h): waiting for the migration lock while another app instance migrates, or completes first-run setup on, the same database.
- `MIGRATION_TIMEOUT` (default `10m`, from 1s to 24h): applying the baseline and pending migrations. An upgrade whose migrations rewrite large tables can need longer; raise it before deploying.
- Payment provider startup (connecting to the configured wallets and nodes) keeps a fixed 15-second bound; a provider that answers with an error is shown as unavailable and retried, as described under [Payments](#payments).

When a phase runs out of time the server exits with an error naming it and the setting to raise, for example `database migration timed out after 10m0s and was rolled back; nothing was changed. Raise MIGRATION_TIMEOUT ...`. A timed-out lock wait or migration is rolled back and its database session ended before the process exits, so the next start begins from the unchanged database with the lock free. An invalid value stops startup with a message naming the variable and its accepted range.

The application's pool opens with fixed per-connection limits (startup parameters, set by the app, not in PostgreSQL's configuration): `statement_timeout` 8 s, `lock_timeout` 3 s and `idle_in_transaction_session_timeout` 15 s, so one slow query or held row lock cannot tie up the pool for longer. A payment request, and whatever may queue behind one (a checkout, listing edit, archive or takedown, a cancellation returning stock, a payment-intake change, a payout to the buyer, and an administrator's suspension, second-factor reset or role change), waits up to 11 s for a row lock instead (its statement limit is raised to 11 s with it), because the payment request holds its rows while the wallet is asked for an address (up to 10 s). Each account has at most one payment request in progress at a time; another is refused at once with 429 "Your previous request is still being processed" and `Retry-After: 5`. Startup opens this pool and checks that the database accepts it **before** migrating, so a database or pooler that refuses these parameters stops startup with nothing changed (see [Connection poolers](#connection-poolers)). Migrations then run on their own database connection without the limits; the migration transaction also lifts any that are set on the database role. `-status`, `-bootstrap-admin`, `-reset-admin-password`, `-set-role`, `scripts/backup.sh`, `scripts/restore.sh` and `psql` sessions do not have them either. A page or form stopped by one answers 503 ("The market is busy") and logs cause `57014` (statement) or `55P03` (lock); see [Troubleshooting](#troubleshooting).

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
- **Password-confirmation pauses**: every action confirmed with the account password (resolving disputes, payment reviews, payout actions, admin settings, key, TOTP and password changes) shares one per-account limit of 10 failed confirmations per ten minutes: a **wrong** password, or a wrong authenticator or recovery code after the right one. After that the account's confirmations are refused (HTTP 429, "Too many failed password confirmations. Try again in ten minutes.") until that window ends, even with the right password. Correct confirmations do not count, so staff can resolve any number of disputes in a row. Only someone signed in to the account can spend it; restarting the app clears it, as for sign-in pauses.
- **Password change**: users change their own password on *Account → Change password* with the current password (plus an authenticator code or an unused recovery code when TOTP is on). Every other session and pending sign-in of that account ends. There is no email or administrator password reset: a user who forgets their password cannot be recovered through the application.
- **Second-factor reset**: an administrator can turn off TOTP (deleting its secret and recovery codes) and PGP sign-in for a non-administrator account under *Admin → Reset a user's second factors* by entering its handle (exact, case-sensitive; any account, not only those in the list of up to 100 shown there), confirmed with the administrator's own password (and authenticator code when enrolled). The account's PGP key and verification stay; its sessions and pending sign-ins end, both accounts get an audit row and the user is notified. Anyone who knows that account's password can then sign in, so confirm the request really comes from its owner (for example with a message signed by their verified PGP key) before resetting.
- **Unreadable PGP key**: an account with PGP sign-in on whose saved key can no longer be read (for example one saved before this market refused keys like it) is never signed in with the password alone. With TOTP on it is asked for its authenticator code or a recovery code; otherwise the sign-in is refused, after the correct password, with a message that names the problem, the second-factor reset and the staff contact, and the owner is notified once. Such a user needs the *Second-factor reset* above (an administrator, *Administrator lockout* below); confirm who is asking first, since the key that would prove it is the one that cannot be read. After the reset they sign in with the password, paste a current export of their public key and prove ownership again.
- **Account suspension**: to contain a compromised or abusive account, an administrator enters its handle under *Admin → Suspend or restore an account*, chooses *Suspend* and confirms with their own password (and authenticator code when enrolled). Its sessions and pending sign-ins end at once and it cannot sign in until restored: after a correct password it is told "This account is suspended." followed by the staff contact (below) Its role, listings and orders are unchanged (change the role to buyer as well if a vendor's listings should be archived), but orders waiting on this account stop until it is restored; the other party can still complete, cancel where allowed, or open a dispute. A suspended moderator does not count as a dispute resolver: it is not notified of disputes or payment reviews and is not offered as a dispute contact, so if it was the only moderator who is not a party to a disputed order, that dispute is treated as having no eligible resolver until you restore it or another moderator exists. Suspension also holds the account's unsent payouts: whoever had its password may have changed its payout address (the owner is notified of every change). In the same step every `pending` or `blocked` payout to it, and any the watcher was holding, becomes held ("Held: account suspended — check the payout address before releasing" under *Payouts*), and a payout queued for it later (an order completed or refunded while suspended) starts held. Restoring the account does not release them, and the watcher never does. Release each one from *Payouts* only after checking its payout address, shown in the release form, with the account owner through a channel you trust; the release needs your password and the "Payout address checked" confirmation (without it the server answers 409 and changes nothing), is audited with the address, and the payout is then sent once. A held payout with no payout address cannot be released until the recipient saves one (after you restore the account); check that address with them first. A held payout that already has an address keeps it when the owner saves a new one (they are told which orders still use the previous address): if the owner says the payout's address is not theirs, check the new address with them and use *Use the account's current address* on that payout (see below) before releasing it. The panel lists suspended accounts; *Restore* with the same form allows sign-in again. Both changes are audited on both accounts and the user is notified. Administrator accounts, including your own, cannot be suspended there: to contain one, remove its administrator role from the host first ([Administrators](#administrators)).
- **Staff contact**: set *Admin → Staff contact* to how people reach the staff — a handle, onion address or XMPP address, at most 200 characters on one line — confirmed with your own password (and authenticator code when enrolled); the old and new values are audited. Anyone can read it, signed in or not: it is shown on the Transparency page (`/canary`), to a suspended account after its correct password, on `/challenge` for someone who lost their second factors, beside the CAPTCHA, in the expired- and unreadable-PGP-key sign-in refusals, to a vendor whose listing an administrator took down, with a photo the malware scanner refused, on the two-factor page after a sign-in with the last recovery code, and under notifications that tell people to contact the market staff (such as the payout-address change notice). It is always plain text; if it is exactly the handle of a moderator or administrator who is not suspended and whose PGP key can receive encrypted messages, signed-in users also get a link to message that handle. While it is empty those places say "Ask the operator of this market." — so set it soon after installation. Save it empty to remove it.
- **Administrator lockout** (lost second factors) is not recoverable in the application: the web reset refuses the administrator's own account and other administrators. Keep the administrator's recovery codes offline: with one left, the administrator recovers alone at sign-in (*Lost authenticator* above). Without one, and also after a key rotation left the stored TOTP secret unreadable, an operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-factors ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL` (the internal or an external database), starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands, it turns off TOTP (its secret, any unconfirmed enrollment and the recovery codes, including any not yet shown) and PGP sign-in (the PGP key and its verification stay), ends every session and pending sign-in of that account, writes the audit row "Second factors reset by host command: … turned off; ended N session(s) and any pending sign-ins", notifies the account and prints a one-line summary. The administrator then signs in with the password alone and enrolls TOTP (or PGP sign-in) again. It refuses, changing nothing: an account that is not an administrator (reset those on the web under *Admin → Reset a user's second factors*, above), an unknown handle, an administrator with no second factor enrolled, and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password` below. The password is not changed: if it is lost too, also run `-reset-admin-password`.
- **First administrator from the host** (instead of entering `SETUP_TOKEN` in the browser on `/setup`; `./scripts/install.sh init --admin ADMIN_HANDLE` does this during installation): an operator on the host, in the deployment directory (with its `.env`), after the app has started once so the database schema exists, runs (replace `ADMIN_HANDLE`: 3–32 letters, digits or underscores, exact and case-sensitive; `-site-name` is optional):

  ```sh
  docker compose run --rm app -bootstrap-admin ADMIN_HANDLE -site-name "My Market"
  ```

  It connects with `DATABASE_URL`, starts no web server and needs no setup token. It prompts twice for the administrator's password without showing it (12–72 bytes, as on `/setup`) and then, in one transaction under the same lock as `/setup` (so it cannot race the browser form or another run), creates that one administrator, records the marketplace name (default `OPSMKT`, at most 80 bytes; change it later on `/admin`), marks the market installed and writes the same audit row as `/setup` ("Account created: admin"). It prints only the handle; sign in on `/login` with that handle and password. From then on `/setup` refuses every token. It refuses, changing nothing, when the market is already set up (by `/setup` or an earlier run), a handle, password or name that breaks the rules above, a database the server has never started against, and a database upgraded by a newer release. Arguments go after the service name, as for `-reset-admin-password` below. Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input (`docker compose run --rm -T app -bootstrap-admin ADMIN_HANDLE`); it is never accepted as an argument. Anyone who can run this command with the deployment's `.env` can create the first administrator, just as they could read `SETUP_TOKEN` from it; there is no web equivalent.
- **Forgotten administrator password**: the application has no web password reset. An operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-password ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL`, starts no web server, prompts twice for the new password without showing it (12–72 bytes, the registration rule) and, in one transaction, stores its hash, ends every session and pending sign-in of that account and writes the audit row "Password reset by the operator on the host". It refuses an account that is not an administrator, an unknown handle, and a database upgraded by a newer release (as startup does); nothing changes then. Arguments go after the service name because the image's entrypoint is the server itself (do not repeat `/app/server`). Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input; it is never accepted as an argument. The site can keep running. Second factors are unchanged: if they are lost too, also run `-reset-admin-factors` (above).

### Administrators

The web page never adds, removes or changes an administrator: *Admin → Assign an account role* offers only buyer, vendor and moderator and refuses administrators' handles, and suspension and the second-factor reset refuse administrators too. Administrators are managed from the host with `-set-role`, by an operator in the deployment directory (with its `.env`). Replace `HANDLE` (exact and case-sensitive) and `ROLE` (`buyer`, `vendor`, `moderator` or `admin`):

```sh
docker compose run --rm app -set-role HANDLE ROLE
```

It connects with `DATABASE_URL`, starts no web server and reads nothing from standard input, so it runs the same from a script. The site can keep running. In one transaction, under the same lock as startup migrations and the other host commands (so two runs happen one after the other), it changes the role, writes the audit row "Role changed from OLD to NEW by host command" on the account and prints a one-line summary. It does what *Assign an account role* does for the new role: an account that stops being a vendor has its active listings archived (audited as "Archived N active listing(s): role changed to NEW by host command"), and an administrator or moderator who stops being staff loses their staff avatar and goes back to hidden on the public staff list. Removing an administrator (any role other than `admin`) also ends every session and pending sign-in of that account. It refuses, changing nothing: an unknown handle, a role other than the four above, the role the account already has, removing the last administrator that is not suspended, making a suspended account an administrator (restore it on the web first), and a database upgraded by a newer release (as startup does). Arguments go after the service name, as for `-reset-admin-password`.

- **Add an administrator**: the person registers an ordinary account on `/register` and, preferably, sets up TOTP and saves the recovery codes. Confirm out of band that the handle is theirs, then run `docker compose run --rm app -set-role HANDLE admin`. Their open session becomes an administrator's at once. A vendor made an administrator has its listings archived, as for a moderator.
- **Remove an administrator**: `docker compose run --rm app -set-role HANDLE buyer` (or `moderator`, or `vendor`). The account is signed out everywhere and signs in again with the new role. The last administrator cannot be removed: add another first.
- **Contain a compromised administrator** (someone else knows its password, or its owner acts against the market):
  1. Demote it at once: `docker compose run --rm app -set-role HANDLE buyer`. This ends its sessions and pending sign-ins, and a password-confirmed change of its own that is not yet saved when the demotion commits (a suspension, factor reset, payout change, review or listing takedown, payment intake or staff list switch) is refused with 403, changing nothing. Whoever has the password can still sign in as a buyer until the next step.
  2. As another administrator, suspend it on the web: *Admin → Suspend or restore an account*, choose *Suspend* and confirm with your own password. A suspended account cannot sign in, and its unsent payouts are held for a payout address check (see *Account suspension* above).
  3. Review what it did. There is no filter by account yet: the *Audit trail* at the bottom of `/admin` shows only the 100 most recent events of every account, newest first. Look there for rows with its handle in the *Account* column and for changes naming it. For older events, download the *Signed audit export* on `/admin` (when `AUDIT_SIGNING_KEY` is set, see [Transparency](#transparency)) and search it for the handle, or run a query in `psql` against the application database, such as `SELECT e.created, e.action FROM audit_events e JOIN users u ON u.id=e.user_id WHERE u.handle='HANDLE' ORDER BY e.id`. Its own rows name every account it changed ("Suspended account …", "Changed role of …", "Reset second factors of …"). Also check the rows on the accounts it touched: suspensions and factor resets name it ("… by administrator HANDLE"), but role changes it made say only "by an administrator". Check the payouts it released or changed, the marketplace settings, the sign-in protection, the operator key and canary, and undo what it changed.
  4. To reinstate the account later: restore it on the web, run `-set-role HANDLE admin`, then give it a new password with `-reset-admin-password` (which works on administrators only) and, if someone else may know its second factors, reset them with `-reset-admin-factors HANDLE` (see [Authentication](#authentication)).

### PGP identity

No configuration. Users paste an ASCII-armored public key on `/account`; it is stored without armor headers or surrounding text (only public key, subkey, user ID and signature packets are accepted: a key with a GnuPG photo ID is refused, with the `gpg --edit-key` … `deluid` steps to remove it), and the fingerprint and user IDs are shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in; renewing the same key (same fingerprint, for example `gpg --quick-set-expire`) keeps them unless it drops a revocation or rolls back the encryption subkey. `/account` and `/pgp` show when the key and its encryption subkey expire, and each process checks all saved keys daily and notifies owners once 30 days before and once after encryption to their key stops. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled or an administrator resets the account's second factors (see Authentication). Vendor pages and order pages show the other party's armored public key, fingerprint and whether ownership is verified, and "Message vendor"/"Contact vendor" links open `/messages?to=<handle>` with the recipient filled in; without a saved key, the page says messages cannot be sent until one is added.

### Orders

No configuration. Requesting payment on an order requires a test-network wallet provider for its currency (see Payments); without one the step is shown as unavailable. Migration 030 adds `deliveries`, `reviews` and `disputes.outcome`. Digital delivery content is stored unencrypted in the database and shown only to the order's buyer and vendor, and read-only to moderators and administrators once the order is disputed; vendors should encrypt sensitive content to the buyer's PGP key before delivering it.

**There is no shipping address field, by design.** For a paid physical order the order page asks the buyer to encrypt their address to the vendor's PGP key in their own PGP application and send it through `/messages`, which accepts only OpenPGP-encrypted messages; the server keeps that encrypted message (and who sent it to whom and when) but cannot read it. A vendor without a key that can receive encrypted messages (none saved, or one that has expired or can only sign) cannot receive addresses, so buyers cannot request payment for that vendor's physical listings until the vendor adds one; digital listings are unaffected. If the vendor's key was changed or its ownership proved again after the order was placed, the buyer's order page says so and asks them to confirm the key with the vendor before sending the address. The market cannot stop an address being typed into a plaintext field: the vendor's note to the buyer, cancellation reasons, dispute reasons, dispute replies and review text are stored unencrypted in the database and every backup. Each of those fields warns that it is unencrypted, says who can read it and asks users never to include an address, real name or tracking number but to send them encrypted from the order page (the dispute forms point to the "Contact dispute staff" keys). `/canary#records` lists what is kept.

Opening a dispute notifies every moderator and administrator who is not the order's buyer or vendor and is not suspended. A moderator or administrator who is a party cannot resolve it, and a suspended one cannot sign in, so keep at least one moderator who does not trade: if every staff account that is not suspended is a party (for example the only administrator is the vendor and the only moderator is suspended or absent), the dispute is still accepted, the order history records that no independent resolver was available, every administrator is notified that it needs a moderator who is not a party, and the moderation desk marks it "No eligible resolver" for administrators until such a moderator exists.

The party who did not open a dispute can put one reply on record from the order page while it is open (plain text in the order history, shown to both parties and staff); the resolve form says whether it did. For each open dispute the moderation desk also lists which moderators and administrators each party messaged since it opened, with the count and the latest time, from message metadata only: ask those holders what the encrypted evidence shows before deciding, since nobody else can read it.

Resolving as a refund returns the whole counted amount to the buyer; resolving as a release pays the vendor the price and returns any excess worth sending to the buyer (see [overpayments](#payments)). A refund or return larger than the price waits for an administrator's review; the payment watcher sends the rest automatically. The resolve form states each outcome's amount, recipient, difference from the price and the recipient's payout state, and needs a ticked confirmation and the resolver's current password (and authenticator code when enrolled); if another deposit is counted after the form was shown, resolving is refused with 409 and the moderator reviews the new amount. Completing an order and a vendor's cancel of a paid order work the same way.

Moderators and administrators open the order page of a disputed (or resolved) order they are not party to, read-only, from the moderation desk; buyer and vendor actions stay unavailable to them. Roles are assigned under *Admin → Assign an account role* by entering the account's handle (exact, case-sensitive; the list there shows only the 100 newest accounts). Changing a vendor's role to buyer or moderator archives their active listings in the same audited transaction and blocks new drafts and payment requests on them; existing orders continue.

Reviews are published as soon as the buyer writes them. To take one down (for example text that names or locates a vendor), sign in as an administrator, open the listing or the vendor's page, open *Hide this review* under it and confirm with your password (and authenticator code when enrolled). The review is kept but leaves both public pages and their rating; the buyer and vendor still see it on the order, and administrators see it labelled with *Restore this review*. Both actions are audited without the review text and the reviewer is notified. Migration 031 adds `reviews.hidden_at`.

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after a payment provider confirms payment; with payments disabled it is never released automatically. Administrators can edit other vendors' listings but cannot view or change their automatic delivery content: the editor shows only whether it is set, and only the vendor can change or remove it.

To remove another vendor's listing (for example counterfeit or prohibited goods) while the vendor keeps selling everything else, open its edit page as an administrator, open *Take down listing*, give the vendor a reason of 10–500 characters and confirm with your password (and authenticator code when enrolled). The listing is archived; the vendor is notified with the reason and sees it on the listing and the vendor desk, can still edit the listing, but cannot restore it. The listing leaves the marketplace, but the buyers of its existing orders, and moderators and administrators reviewing those orders, can still open its page (marked Archived, without the reason) from the order. The reason is stored unencrypted and kept in both accounts' activity logs; it is never shown publicly, but do not put an address or real name in it. While the listing is taken down, paid orders on it are not delivered automatically: they wait for the vendor to deliver by hand or cancel and refund (cancelling a funded [Bitcoin escrow](#bitcoin-escrow-regtest-only) order requests the refund of its escrow, a settlement the buyer or the vendor signs, which the market then co-signs and broadcasts (when both sign it, the market broadcasts it without signing)). *Restore listing* on the same page, confirmed the same way, undoes it. To take away all of a vendor's listings, change their role instead (that archive carries no reason and the vendor can restore those listings if made a vendor again). Migration 041 adds `products.admin_archive_reason`.

### Payments

**Test networks by default; these payments move no real funds.** The application refuses to start if a node or wallet is on mainnet without `BITCOIN_CHAIN=main`/`MONERO_NETWORK=mainnet`, or disagrees with `BITCOIN_CHAIN`/`MONERO_NETWORK` (blank accepts whichever test network the node reports). To take real payments see [Mainnet](#mainnet). A node that is unreachable, still syncing or has no wallet loaded does **not** stop the site: that currency shows as unavailable on the admin page, payment requests for it are refused, and every watcher pass checks it again (reloading the Bitcoin wallet or reopening the Monero wallet after a node restart). A node that reports another network after startup disables its currency until the app is restarted. A currency with a blank URL stays disabled, and the site then says "Payments disabled". Step-by-step setup, wallet creation and payout recovery: [docs/testnet-runbook.md](testnet-runbook.md). Upgrading from v0.1.0-alpha.1: [UPGRADING.md](../UPGRADING.md).

- Bitcoin: `BITCOIN_RPC_URL` is a wallet-enabled Bitcoin Core RPC. Credentials go in the URL userinfo (`http://marketplace:PASSWORD@bitcoin:8332`); they are sent as HTTP Basic auth and never logged. `BITCOIN_WALLET` (default `opsecmkt`) must exist; the app loads it if needed. Create it once as shown in the [runbook](testnet-runbook.md#2-create-the-bitcoin-wallet) (inside the compose container `bitcoin-cli` needs `-rpcport=8332 -rpcuser=marketplace`). `BITCOIN_CHAIN` (blank, `testnet4`, `signet`, `regtest`, or `main` for mainnet) must match the node when set; the local node service runs `testnet4` when it is blank. `PAYMENT_CONFIRMATIONS_BTC` defaults to 3; use 1 for regtest.
- Monero: `MONERO_WALLET_RPC_URL` is monero-wallet-rpc with its `--rpc-login` credentials as userinfo (for example `http://marketplace:PASSWORD@monero-wallet:18083`), sent as HTTP Digest auth. The local `monero-wallet` service requires `MONERO_WALLET_RPC_PASSWORD` (the installer generates it) and no longer runs with `--disable-rpc-login`. The app uses account 0 and opens a wallet file named `opsecmkt` (empty password) if none is open. Create it first with the `create_wallet` RPC ([runbook](testnet-runbook.md#3-create-the-monero-wallet)). `MONERO_RPC_URL` (the daemon) is optional; when set, its network and sync state are checked too. `MONERO_NETWORK` (blank, `stagenet`, `testnet`, or `mainnet`) must match when set. `PAYMENT_CONFIRMATIONS_XMR` defaults to 10.
- `PAYMENT_POLL_INTERVAL` (default `30s`, from 1s to 1h) sets how often the single background watcher polls wallets and sends queued payouts. Several app instances can share a database; only one polls at a time.
- `PAYMENT_EXPIRY` (Go duration, default `24h`, from 10m to 720h) is the payment window. An order still awaiting payment that long after its address was issued, with no deposit seen, is cancelled by the watcher and its reserved stock is returned. An order with a deposit still confirming stays open. An underpaid order (every deposit confirmed, together short of the price) is cancelled the same way once its address is older than the window and no deposit has arrived for the window either; its confirmed deposits are refunded to the buyer's saved payout address (blocked until the buyer saves one; below the minimum automatic payout, flagged for staff) and the buyer is notified. The order page states the time from which the order can be cancelled (address issue time or latest deposit, whichever is later, plus the window), which a new deposit moves; changing `PAYMENT_EXPIRY` moves it for every open order. A buyer or vendor who cancels an order awaiting payment after a deposit arrived sees the refund it queues and must confirm it. A buyer can hold at most 3 orders awaiting payment at once. Requesting payment records the listing's current price.

#### Overpayments and the payment QR code

The buyer's order page shows a QR code for the wallet payment request of exactly the amount still due (`bitcoin:` BIP21 or `monero:` with `tx_amount`), so a wallet that scans it pre-fills the right amount; deposits already seen are subtracted and the code disappears once the full amount is seen. The request is also printed as text under the code, so a browser that blocks inline SVG (Tor Browser's Safest level, for one) still shows the address and exact amount. If a buyer still pays too much:

- **Completing (or a dispute resolved as a release)** pays the vendor exactly the price and returns the excess to the buyer's saved payout address as a second payout, with the network fee taken from it.
- **An excess below the minimum automatic payout** (0.0001 BTC, 0.001 XMR) is not returned: it goes to the vendor with the release, and both parties are told. Sending it back would cost more than it is worth and invite fee abuse.
- **A refund or return larger than the order price is held** ("Held: larger than the order price") and every administrator is notified: it is a likely mistake. On **Admin → Payment providers**, open the order, check that the payment looks like a genuine mistake, then release it with the *Reviewed* confirmation; otherwise leave it held (or return it by hand and mark it sent). The two differ: a refund is held when it exceeds the price, a return only when the excess itself exceeds the price (the buyer paid more than twice the price and the order was completed).
- **The hold does not stop coin swaps.** Refunds and returns up to the order price are sent automatically, with no review: a buyer who pays and has the order cancelled, lets an underpaid order expire, or has it cancelled and then sends to its address gets the deposits back. Anyone can therefore use the market's pooled wallet to swap coins up to a listing's price without review (the coins sent back are not the coins received); the hold only catches amounts above the price, and a buyer can open more orders.
- **Every payout is sent with the network fee taken from the amount**, on both coins and for every kind: releases, refunds and returns (Bitcoin `subtractfeefromamount`; Monero `subtract_fee_from_outputs`, for releases too since A-232). No payout costs the market its fee, so repeatedly paying and cancelling, or a vendor buying its own listing, does not spend the market's funds on fees. The tests check the request the market sends (fake wallet and simulated wallet RPC), and on local chains (Bitcoin regtest and an offline Monero stagenet fork, at `1c62647`) every payout, Monero releases and overpayment returns included, arrived as its amount less the fee ([evidence table](verification.md#evidence-table)); no public test network run is recorded.

#### Mainnet

Mainnet (live Bitcoin and Monero, real funds) is off unless you name it: set `BITCOIN_CHAIN=main` and/or
`MONERO_NETWORK=mainnet` and restart. A blank setting never accepts a mainnet node or wallet, so an external
node that turns out to be on mainnet still stops startup. Once set:

- **The whole market is mainnet.** If both currencies are configured, both must be mainnet; one mainnet and
  one test-network currency stops startup. The pages, badges, order notes and notifications the application
  writes say `MAINNET` and "real funds" instead of `TESTNET`. Text written by hand does not: a note you type
  when recording a manual refund says what you write, and rows written by an earlier test-network install are
  never rewritten (a mainnet market starts on a fresh database, below).
- **The database must be new to mainnet.** Startup refuses a database whose deposit addresses were issued on a
  test network (and a test-network market refuses one with mainnet deposits), so a mainnet wallet never pays
  out for test coins. Start a mainnet market on a fresh database; do not repoint a test market's database.
- **Addresses.** Bitcoin deposit and payout addresses are native SegWit (`bc1`) only; `tb1`/`bcrt1` and legacy
  (`1`/`3`) addresses are rejected. Monero accepts mainnet (`4`/`8`) addresses only.
- **Nodes and wallets.** A local Monero node needs `MONERO_NETWORK_FLAG=` (blank) in `.env` as well, because
  monerod and monero-wallet-rpc have no `--mainnet` flag; the installer writes it when you choose `live`.
  A node that later reports another network disables that currency until the app restarts.
- **Real money.** The pooled wallets hold every buyer's deposit until release or refund: back up the wallet
  files, protect the wallet RPC credentials and the host, and follow the backup, restore and payout-recovery
  procedures in [docs/testnet-runbook.md](testnet-runbook.md). They were written and exercised on test
  networks only: the commands are the same on mainnet (with mainnet addresses and a mainnet node), the
  runbook's SQL takes the `MAINNET`/`TESTNET` label from the order's deposit address, and none of it has been
  run against real funds. Consider raising `PAYMENT_CONFIRMATIONS_BTC` (default 3) for large amounts. This is
  still custodial holding, not escrow, and the market's legal and tax obligations are the operator's.

**Before accepting real funds.** Mainnet support is the test-network code behind an opt-in; it has not been
proven with real funds. Know these before you take a real payment:

- **No mainnet run is recorded** ([verification: not verified](verification.md#not-verified)): the mainnet
  refusals, labels and network-history guard are tested against fake wallets and simulated wallet RPC only.
  Payouts as they are now sent (fee taken from the amount, Monero releases included), overpayment returns, the
  large-payout review, underpaid cancel and expiry, and a deposit after the close have been run on local chains
  only (Bitcoin regtest and an offline Monero stagenet fork, at `1c62647`; see the
  [evidence table](verification.md#evidence-table)). No public test network run is recorded.
- **Coin swaps up to a listing's price are not reviewed.** Refunds and returns up to the order price are sent
  automatically (see [Overpayments](#overpayments-and-the-payment-qr-code)), so anyone can use the pooled
  wallet to swap coins up to any listing's price without review; only a refund above the price, or a return of
  more than the price, is held.
- **With local nodes, payouts in Tor mode leave from the host's own IP address.** The Tor overlay hides the
  market's web service, but the local Bitcoin and Monero nodes connect to peers and broadcast every payout directly over
  clearnet from the host, so peers can link the market's payouts to the host's IP address. Nothing in the
  Compose files routes the nodes through Tor; running them over Tor is not tested here.
- **Open items** in the project's review ledger (`.claude/war-room/ledger.md`), each still unfixed:
  - A-226: the [runtime wallet check](#node-image-versions) treats any monero-wallet-rpc that does not report
    RPC 1.31 from a release build as unverified, but a later release whose RPC did not change also reports
    1.31 and is not detected. Keep `MONERO_WALLET_IMAGE` on the pinned tag until the bump checklist is done.
  - A-90 and A-95: the payout broadcast from the host's IP address above, and whether the nodes can run over
    Tor (not measured).
  - A-105: a wallet that disowns an issued address, or a Monero wallet behind its daemon, reads as "no
    deposits", so a part-paid order can be cancelled as expired.
  - A-76: after a restore from backup, the watcher can queue a refund that was already sent after the backup;
    the recovery gate stops it being sent, but nothing retires it.

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

- Deposits for all orders sit in **one pooled custodial wallet** per currency. This is not multisig escrow. Whoever controls the node wallet controls the funds. Per-order escrow is in no release ([per-order escrow](escrow.md)): its Phase 1 is built and tested on regtest only, behind `ESCROW_BTC`, which is off by default ([Bitcoin escrow](#bitcoin-escrow-regtest-only)). The pooled wallet stays the default payment mode.
- There is **no commission or fee accounting**. Every payout on both coins, releases included, deducts the network fee from the amount sent (Bitcoin `subtractfeefromamount`; Monero `subtract_fee_from_outputs`), so the recipient bears it and no payout costs the market its fee. When you pay one by hand, send it the same way. Payouts below a minimum are never queued but flagged to moderators: 0.0001 BTC for any Bitcoin payout (a smaller one cannot cover the fee) and 0.001 XMR for any Monero payout (the fee would take too large a share); listings cannot be priced below either minimum, and one priced lower before its rule (Bitcoin before A-99, Monero before A-232) takes no new orders in that currency until its vendor raises the price (an overpayment above such a price on an existing order is held for an administrator's review). The fee-from-amount rule covers the fee's amount only: how many small outputs the pooled Monero wallet accumulates, and what spending them later costs, is not measured (A-221(c)).
- Payouts are **single-attempt**. A failed or interrupted wallet call is shown on the admin page and never retried automatically, because the transaction may already have been broadcast. After checking the wallet, an administrator can release a held payout, requeue a failed or stuck one, or record one as sent with its transaction ID (password-confirmed and audited). A failure the wallet rejected with a pre-broadcast error (nothing broadcast) requeues with the password alone; requeueing one whose outcome is unknown (timeout, dropped connection, or a Monero error such as -38 that can follow a submission; see the [runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)) or a stuck send, or releasing a payout held by (or requeueing a failed payout marked by) a restore from backup, also needs an explicit, audited confirmation that the wallet shows no broadcast transaction. A wallet send may take up to 30 seconds (ordinary wallet reads 10 seconds). A shutdown or redeploy lets an in-flight send finish and its outcome be recorded (at most 30 seconds plus 10 to record; even added to the 10-second HTTP drain this stays within the app's 60-second stop grace period).
- The watcher never cancels an order for non-payment unless it read that order's deposit address from the wallet in the same pass, and it skips a currency entirely while its node is still syncing (Bitcoin `initialblockdownload`; Monero daemon `target_height` above `height`, when `MONERO_RPC_URL` is set) or while the node's tip is below the highest tip it has recorded for that currency (Bitcoin `blocks`; the Monero wallet's `get_height`), as after a node or wallet restarted or restored from an older state. The admin page's last error then reads "Node is behind the highest tip recorded ...". If you deliberately replace a chain with a shorter one (a reset regtest, for example), clear the record so the watcher resumes: `UPDATE payment_status SET tip_height=NULL WHERE currency='BTC';` (or `'XMR'`).
- A credited deposit that later conflicts, disappears or drops below the confirmation threshold (a reorg) holds the order's unsent payout and, while the order is open or any of its payouts is unsent (including an overpayment return after the release was sent), is announced once per episode to the order history, the buyer, the vendor and every moderator and administrator not party to the order and not suspended; staff can then read the order and find it under *Payment review*. The payout resumes automatically once the deposit confirms again, however old the order, which ends the episode; a later regression is announced again. The order state is never reverted automatically. On *Payouts* such a held payout shows the deposit, its address and confirmations instead of a *Release* form.
- A payout is sent only in a watcher pass that has just read its order's deposit address from the wallet, so a deposit reversed while the payout waited (blocked, held or failed, then released or requeued) is caught before the send. Orders with a payout waiting to be sent are watched whatever their age, so a released payout normally goes out on the next pass.
- Funded orders that are not yet completed, cancelled or resolved stay watched for as long as they are open. A deposit first seen after an order was funded is noted once in the order history and to the buyer and vendor; it is part of the order's settlement (a release with any excess returned to the buyer, or a refund) if it has the confirmation threshold when the order settles; otherwise it is not paid out automatically (see late deposits below).
- Late deposits: every cancelled, completed or resolved order's address is watched until it is **30 days** old. A deposit recorded before a cancellation that refunded nothing is refunded to the buyer when it confirms (blocked until the buyer saves an address). Anything first seen after the order closed (a buyer topping up an underpaid order after the payment window cancelled and refunded it, a stray payment to an old address) and extra funds that confirm after the order's settlement was queued are **never paid out or refunded automatically**: they are flagged for payment review, and the buyer's order page shows them as "Arrived after the order closed — not paid out; market staff notified". The buyer is notified once per episode with the amount (not the transaction ID; the vendor is not told) and each moderator and administrator once, however many transactions arrive; return the funds by hand from the wallet and record it on the moderation desk (*Payment review → Record the review → refunded by hand*, or *no action* with a reason; a moderator may escalate to an administrator). The buyer's page then shows the outcome instead of "market staff notified", and a later deposit starts a new notice episode ([runbook](testnet-runbook.md#handle-a-payment-review-flag)). Deposits to older addresses are not seen by the watcher (unless the order still has a payout waiting to be sent or held, or changed in the last 24 hours); handle them by hand from the wallet. The watch set is therefore about 30 days of orders: each pass makes one Bitcoin `listreceivedbyaddress` per 200 addresses plus one `gettransaction` per deposit transaction to them (Monero: one `get_transfers` per 200 addresses); keep the node local.
- Monero transfers with a non-zero `unlock_time` (locked transfers) are recorded and shown, but never counted toward payment or paid out. The order history notes "locked transfer ignored" once and moderators and the buyer are notified.
- Recipients must save a payout address on their account page. Payouts wait ("blocked") until they do. Saving or removing a payout address needs the current password, plus an authenticator code when TOTP is enrolled, and notifies the account (naming the currency, not the address).
- Saving a payout address never moves an unsent payout that already has one, since anyone with the account's password can save an address. The notification names the orders instead: "N unsent payout(s) still use your previous address (orders …); an administrator must confirm the change" for held payouts and definite failures, and separately those queued, being sent or possibly broadcast, which stay on the previous address. On *Payouts*, a held payout or a failure the wallet rejected before broadcast whose recipient has since saved a different address offers *Use the account's current address*: it shows both addresses and when the saved one last changed, needs your password and the "Current address checked" confirmation after you check the address with the account owner through a channel you trust, audits the old and new address, and changes only the destination (release or requeue it afterwards). A payout being sent, stuck in sending, with an unknown send outcome, or held or marked by a restore from backup is never moved; the server refuses it (409) ([runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)).

`scripts/regtest-smoke.sh` is a manual check against a local `bitcoind -regtest` (`BITCOIN_REGTEST_RPC_URL=http://user:pass@127.0.0.1:18443`); see the [runbook](testnet-runbook.md#7-manual-regtest-check-bitcoin). It is not part of CI.

#### Bitcoin escrow (regtest only)

**Off by default, regtest only, and not released.** [Per-order escrow](escrow.md) is being built in phases. This
build contains its first phase for Bitcoin on regtest: a user registers the key their own wallet signs escrow with
and activates it by signing a practice transaction, and the buyer of a digital listing with automatic delivery can
pay into a 2-of-3 escrow instead of the pooled wallet; the order completes when the escrow is funded. The vendor's
claim, refunds, an administrator's dispute decisions and the return of coins sent after an order closed are built by
the server, signed by the buyer or the vendor in their own wallet, and co-signed, broadcast and followed to their
confirmations by the market (see *Settlements* below). Every other order still goes through the pooled wallet above,
which stays the default. It is built and tested on regtest only (see *What was checked* below); nothing about escrow
has run on a public test network. Never switch it on for a market that takes real payments.

- `ESCROW_BTC=on` turns it on (blank or `off` keeps it off). The app then refuses to start unless the node at
  `ESCROW_BTC_RPC_URL` reports chain `regtest`, and `BITCOIN_CHAIN`, when set, is `regtest`: "refusing to start:
  ESCROW_BTC=on runs only on regtest in this release …". An unreachable node or an unreadable arbiter wallet
  leaves escrow disabled with one log line starting `escrow: Bitcoin escrow disabled:`, and the market runs
  without it. The escrow watcher then runs the same checks again every `PAYMENT_POLL_INTERVAL`, with the same
  guards (regtest only, the pinned arbiter fingerprint, a watch-only wallet without private keys), and escrow
  starts without a restart once they pass ("escrow: Bitcoin escrow on (regtest only)"). A line is logged when the
  state changes, not on every retry. A retry that finds the node on another chain, or refusing
  `ESCROW_BTC_FEE_ADDRESS`, cannot stop the running market: it logs "escrow: Bitcoin escrow disabled until the app
  is restarted: …" and stops retrying.
- `ESCROW_BTC_RPC_URL`: the regtest node's RPC URL for a user only the app's escrow code uses, restricted with
  `rpcwhitelist` as below (`http://opsecmkt-escrow:PASSWORD@bitcoin:18443`).
- `ESCROW_BTC_ARBITER_WALLET` (default `opsecmkt-arbiter`): a Bitcoin Core descriptor wallet that holds only the
  market's arbitrator key, never `BITCOIN_WALLET`. Create it once as the operator user, loaded at every node start
  (`bitcoin-cli -regtest -named createwallet wallet_name=opsecmkt-arbiter load_on_startup=true`), and back it up
  with the operator user: its seed is the market's escrow key. Use it for nothing else. At startup and on each retry
  the app checks with `getwalletinfo` exactly this: a descriptor wallet with private keys, not encrypted (the escrow
  watcher co-signs without a passphrase, so an encrypted wallet could never sign), and no transaction ever paid to or
  from it (`txcount` 0: escrow coins never belong to it, so a transaction means it is also used as an ordinary
  wallet). Otherwise escrow is disabled with "escrow: Bitcoin escrow disabled: the arbiter wallet must be used for
  the escrow key only …". It cannot check which descriptors the wallet holds (`listdescriptors` is not on the
  whitelist, since it can export the private key). Before it co-signs, the watcher checks that the escrow's market
  child key is in the wallet's derived key range (`deriveaddresses` and `getaddressinfo`) and, when it is not,
  extends the range 1000 keys past the market index with `keypoolrefill` (checked on Core 31.1 regtest under the
  whitelist at index 2501); if it still is not, the settlement waits ("market-key-missing" in the log).
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
  the regtest address it is to be paid to, required when the fee is above 0 and checked with the node at startup.
  They apply until an administrator saves the fee on `/admin` (*Payment providers → Bitcoin escrow market fee*:
  basis points from 0 to 10000 and, above 0, a regtest address the node accepts and the market has not used as an
  escrow, refund, deposit or settlement address; confirmed with the administrator's password and authenticator code
  when enrolled; the audit row names the old and new basis points, never the address); the saved values are the
  `settings` keys `escrow_btc_fee_bps` and `escrow_btc_fee_address` and override the environment from then on. Each
  escrow keeps the fee and destination in force at its checkout; changing them affects only later escrows. One fixed
  address is allowed only because escrow runs on regtest (startup logs "escrow: the market fee is paid to one fixed
  address …"); a mainnet market will need a watch-only descriptor giving a fresh fee address per settlement, which
  this release does not have. The fee is an output of the vendor's claim (below 0.0001 BTC it is added to the
  vendor's output instead).
- `ESCROW_BTC_FEE_RATE` (default `2`): the network fee rate of every settlement the app builds, a whole number of
  sat/vB from 1 to 100 (anything else stops startup). A settlement whose fee at this rate would exceed 0.001 BTC is
  not built; lower the rate and restart.
- `PAYMENT_CONFIRMATIONS_BTC` (default 3), `PAYMENT_EXPIRY` and `PAYMENT_POLL_INTERVAL` apply to escrows as to pooled
  Bitcoin payments. An escrow order that has received nothing when its payment window ends is cancelled. One that is
  underpaid (every deposit confirmed, short of the price, and no top-up for a whole window) is cancelled too, as a
  pooled order is, and its refund requested; the buyer is told. A top-up within the window still completes it.
- **Fingerprint pin.** At the first start with escrow on, the app reads the arbiter wallet's master key fingerprint
  (from a fresh change address, with `getrawchangeaddress` and `getaddressinfo`) and pins it in the database
  (`settings` key `escrow_btc_arbiter_fingerprint`). If a later start finds another fingerprint, for example a
  different wallet under that name, escrow is disabled with "escrow: Bitcoin escrow disabled: the arbiter wallet's
  key fingerprint differs from the one pinned in this database". Load the right wallet; the watcher's next retry
  enables escrow (or restart). On a regtest market where you mean to replace the arbiter key, delete that settings
  row first, and only while no escrow exists (`SELECT count(*) FROM escrows` is 0): every escrow holds a child of
  the arbiter key, and with another key the market can never co-sign for it. Neither the fingerprint nor any key is ever logged.
- **Switching escrow off with escrow orders open.** Setting `ESCROW_BTC` back to `off` (or escrow being disabled
  because its node is down) moves no coins and closes no order, but it stops the escrow watcher: an open escrow
  order is not cancelled when its payment window ends, and deposits to its escrow are not recorded, so it is not
  marked paid, until escrow is on and enabled again. The coins stay in the order's 2-of-3 escrow; the pooled wallet
  never holds them. The order page still shows the escrow panel with its address and descriptor and "Bitcoin escrow
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
  one is flagged for payment review as a pooled late deposit is: one order note, the buyer told the amount once per
  episode (not the vendor) and each moderator and administrator once. The order page's escrow status reads
  "Cancelled; a deposit arrived after the order closed …". The coins are not in the pooled wallet: the moderation
  desk does not offer *refunded by hand* for an escrow deposit (and refuses one, 409); record *no action* once the
  stray refund has returned it, or escalate.
- **Site-wide notices.** With escrow on, the footer, catalog, listing and draft notices say Bitcoin escrow runs on
  regtest even when no pooled provider is configured, and the vendor desk's *Bitcoin escrow* tile counts the viewer's
  escrow orders that may hold coins (with `ESCROW_BTC` off too), so no page says escrow is inactive or no funds are
  held while escrow orders exist. With escrow off and no escrow orders the pooled wording is unchanged.
- **Compose** passes these variables from `.env` to the app container (`compose.yaml`; blank in `.env.example`, so
  escrow stays off). The bundled `bitcoin` node of `compose.nodes.yaml` follows `BITCOIN_CHAIN` and has no
  `rpcwhitelist` user for escrow, so `ESCROW_BTC_RPC_URL` must point at a regtest node you configure as below; the
  checked set-up is still the app run outside Compose (`go run ./cmd/server` against a regtest node), as
  `scripts/test-escrow-regtest.sh` does. Passing them through Compose was checked with `docker compose config`
  only.

**The app's RPC user.** Give the escrow code its own RPC user with `rpcauth` and allow it exactly these methods;
keep a separate operator user with full rights for creating and backing up wallets. In `bitcoin.conf`:

```ini
rpcauth=opsecmkt-escrow:SALT$HASH
rpcwhitelistdefault=0
rpcwhitelist=opsecmkt-escrow:getblockchaininfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions
```

`rpcwhitelist` allows or refuses whole methods, not their arguments, so the list leaves out every method that
can export or copy a key: `listdescriptors` (with `private=true` it prints the arbiter's private key; probed on
Core 31.1 regtest), `dumpprivkey`, `dumpwallet`, `backupwallet` and everything else not listed. `keypoolrefill`
is on it: it exports nothing and lets the arbiter wallet sign for high escrow indices. `descriptorprocesspsbt`
is used only with throwaway practice keys, never a wallet key. `getwalletinfo`, `getaddressinfo`,
`listreceivedbyaddress`, `gettransaction` and `listtransactions` read the watch-only wallet (the arbiter wallet only
answers `getwalletinfo` and `getaddressinfo`; they export nothing). `listtransactions` was added in P1-d1 (UPGRADING.md section 22): without it a
deposit spent outside the market is still flagged, but without the spending transaction's ID. `rpcwhitelistdefault=0` leaves other users (the
operator's, the pooled wallet's) unrestricted. Make SALT and HASH with Core's `share/rpcauth/rpcauth.py`, or as
the comment in `scripts/test-escrow-regtest.sh` shows.

**What users see.** With `ESCROW_BTC` off, `/escrow-key` does not exist: it answers 404 like any unknown page,
signed in or not. With it on, `/account` gains a *Bitcoin escrow key* row linking to `/escrow-key`. There a user registers
one extended public key with its origin, `[fingerprint/path]tpub…`, confirmed with their password (and
authenticator code when enrolled); keys on another path than BIP48's `m/48h/1h/<account>h/2h` also need their
confirmation that the wallet is used only for this market. Private keys, recovery words, descriptors, several
keys, mainnet keys and single-signature (SLIP-132 `upub`/`vpub`) keys are refused before anything is stored, and
the same key cannot be registered on two accounts. The key stays inactive until its owner signs the page's
practice transaction in Bitcoin Core and pastes it back; it spends a made-up output, so it can never be
broadcast. The server keeps the key sealed with `APP_SECRET_KEY` (see
[What the server keeps](implementation-status.md#what-the-server-keeps)); changing `APP_SECRET_KEY` makes
registered keys unreadable ([Secrets](#secrets-env-and-onion-identities)): `/escrow-key` then says "Your escrow key
can no longer be read on this market; register it again."

A rotation never hides an escrow or changes the keys an escrow uses: escrows are attributed by account and key
origin, not by the sealed key. A user whose key already has escrows registers the same key again; the app confirms
with the node (`deriveaddresses`) that it derives the key the latest escrow holds, and the key is active again at
once (practice signing is never offered for a key with escrows). Another key under that origin is refused. Once a key
has escrows its origin cannot be changed: the same key registered with another fingerprint or path is refused with
"This key has escrows, so its origin (fingerprint and path) cannot be changed …", with or without a rotation (the
same key is recognised by its HMAC or, after a rotation, by the node deriving an escrow's child key from it), and
its escrows keep the origin recorded with them. This build has no tool to change a recorded origin, and nothing
about an existing escrow changes. Before a key has escrows, its owner can still correct its fingerprint or path and
sign the practice transaction again. The check that stops two accounts registering the same key starts again after a
rotation: a key registered before it is not recognised on another account until its owner registers it again.

On the order page of a BTC draft on a digital listing with automatic delivery, the buyer is offered *Pay into an
escrow* beside the pooled payment request, once both the buyer's and the vendor's escrow keys are active. The buyer
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

- *Requesting.* The vendor of a completed escrow order requests the claim on the order page with a fresh regtest
  address from their own wallet, confirmed with their password (and authenticator code when enrolled); an address the
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
  wallets.
- *Signing.* Each party sees the expected outputs (address, amount and whom each pays, and the network fee) and its
  own copy of the PSBT, with its key's origin and the other keys bare, in the page and as a download. Either party
  signs it in Bitcoin Core (`walletprocesspsbt` without finalizing, or the GUI's PSBT dialog) and pastes it back. The
  paste is refused, with nothing stored, unless it is the stored transaction (its unsigned transaction ID), it carries
  that party's signature on every input and no other key's, it has no entry under the market's key, every signature
  is `SIGHASH_ALL` and nothing is finalized. Only the party's signatures are kept, never the pasted PSBT; the
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
  transaction ID), pays exactly the stored outputs (each value, and each script decoded from its regtest address), its
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
- *Orphan addresses.* The daily check (below) also lists, under *Admin → Payment providers → Bitcoin escrow
  recovery and reconciliation*, every address the watch-only wallet holds with no escrow on record
  (`listreceivedbyaddress` with `include_empty`: each escrow descriptor is imported with the label `escrow:<order
  ID>`, which puts its address in the wallet's address book; `listdescriptors` stays off the whitelist): a descriptor
  imported for a checkout that rolled back after the import, or an escrow created after the backup a restored
  database came from. The market builds nothing for them; coins there move only with two of their three keys.
- *Bounded wait* (A-342). A claim or refund whose build waits on a deposit recorded before the order closed that does
  not confirm, or on an escrow holding more than 50 deposits, waits at most 24 hours (about 144 blocks) from its
  request. It is then built without the unconfirmed deposits and beyond the 50 largest; those are left for a stray
  refund, with an order note and a notice to the buyer.
- *Disputes.* A buyer or vendor may dispute a funded escrow order that is paid, shipped or delivered, as a pooled
  one. Only an administrator decides it: a moderator's `/resolve` of an escrow dispute is refused (403) and the desk
  says so. The decision needs the administrator's password (and authenticator code when enrolled) and states the
  escrowed amount; a refund to the buyer is requested at once, and after a release the vendor claims with a fresh
  address. On a physical order the release built when it was marked shipped is the release of a decision for the
  vendor (no new claim); a decision for the buyer supersedes it while it has no final transaction, and is refused
  (409) once both parties' signatures or the network have made it final. Until the settlement of the decision has a stored final transaction, an administrator who is not a party
  may change the decision on the order page (*Change the decision*, with a reason and their password): the
  settlement of the old decision is *superseded* (it and its signatures are kept as a record and never used; its
  deposits are freed) and the new one requested; both parties are notified and the change is audited. Once the
  settlement is signed by the market (or both parties), broadcast, confirmed or spent outside the market, the
  decision is fixed (409).
- *Stray refunds.* Escrows stay imported in the watch-only wallet with no age limit. Coins first recorded at an escrow
  after its order closed, and deposits a bounded build left out, go back to the buyer's refund address by a *stray
  refund*: requested by the escrow watcher once they confirm, built, signed by either party and co-signed like any
  settlement; one is in progress per order at a time and a later deposit gets the next one. The buyer of a closed
  escrow order can ask on the order page (*Recover coins at this escrow*, with their password): the next pass reads
  that escrow whatever its age, so this also covers an escrow cancelled while Bitcoin escrow was unavailable, and
  requests the stray refund, or tells the buyer it found nothing. A daily check reads every escrow (the first one 24
  hours after escrow first runs on the database; `settings` key `escrow_btc_stray_check`), one
  `listreceivedbyaddress` per 200 escrows plus one `gettransaction` per deposit transaction.

**After a restore from backup.** The pooled restore gate (`payments_recovery_required`, set by `scripts/restore.sh`
or the manual protection SQL in [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too)) also
pauses escrow: while it is set the escrow watcher builds no settlement, co-signs none and broadcasts no stored
transaction, and sends none again; it still reads deposits, follows broadcast and confirmed settlements, and records
as broadcast (without sending it) a held settlement the watch-only wallet shows on the network. Signing on the order
page still works. The restore also holds every restored settlement that has a stored final transaction (*signed* or
*broadcast*; `settlements.send_error` starting `Restored from backup:`): the restored records may predate its
broadcast or something that replaced it. Under *Admin → Payment providers → Bitcoin escrow recovery and
reconciliation*, each held or paused settlement is listed with its transaction ID; check it on the chain and in the
watch-only wallet (`gettransaction` of the txid, `gettxout` of its inputs), then confirm that it was not already sent
and that no other transaction spent the escrow's coins, with your password (and authenticator code when enrolled):
the watcher then broadcasts it exactly once, gate or not, and the confirmation is audited and noted on the order. The
order pages and `/admin` say what is paused and why. Clearing the gate (the runbook's SQL) resumes building,
co-signing and broadcasting; a settlement the restore held stays held until an administrator confirms it. A restore
not done by `scripts/restore.sh` or that SQL holds nothing. The restore also asks for a release deadline recheck
(`escrow_release_recheck`): on its next pass, gate or not, the escrow watcher moves every release deadline of a shipped
physical order that has passed in the restored records to 72 hours later and tells the buyer; while the gate is set no
order is released at its deadline.

**What was checked.** `scripts/test-escrow-regtest.sh` (manual, not in CI) runs key registration and practice
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
form led to exactly one `sendrawtransaction` over 16 passes, and the runbook's clear-gate SQL resumed building and
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
address, because the real watch-only wallet still holds the lost one; the lost escrow is listed as an orphan. In
every leg the app runs inside the Go test against the real node, not as the server binary. A browser journey
(`npm run test:e2e:escrow`, [development](development.md)) drives the escrow forms by keyboard in Chromium at 320 px
with JavaScript off against the real server and a simulated escrow node; it is not chain evidence. Nothing about
escrow has run on a public test network or mainnet, and the escrow pages have not been run in Firefox or WebKit.

### Transparency

`AUDIT_SIGNING_KEY` (64 hex characters, an Ed25519 seed generated by the installer with `openssl rand -hex 32`) enables signed audit exports. It is read only from the environment, never stored in the database; blank or malformed values leave the export visibly unavailable. Changing it changes the public key, so treat it like any long-lived signing key: back it up with `.env` and announce rotations.

**Exports are private operator evidence.** An export names every account's handle and its activity: sign-ins, account changes, order actions, key fingerprints, short order IDs and transaction IDs. Store it like a database backup and never publish it. Visitors do not receive exports; `/canary` publishes only the public key, so that you can prove an export you hold came from this server. `/canary#records` tells users what the server keeps and that there is no account or message deletion.

**Pin the audit public key.** The admin page and `/canary` show the hex Ed25519 public key. Record it once through a channel you trust (for example in the operator-signed canary text) and verify every export you hold against that pinned value, not against the key printed in the `.sig` file:

```sh
go run ./cmd/verify-audit -pub <pinned-hex-key> audit-events-upto-N.jsonl audit-events-upto-N.sig
```

The export for a given `upto` is byte-for-byte reproducible, so two downloads can be compared. A download cut short leaves a partial file that fails verification with "export sha256 does not match the signature file", the same failure as an altered file: download the same `upto` again (each attempt counts against the 10 downloads per 10 minutes). The export route has its own time limits instead of the server's 12 s request bound and 20 s write timeout: up to one minute to read the events from the database, then 20 seconds plus one second per 32 KB to send the file (a 50 MB export gets about 27 minutes; onion throughput measured 0.19–0.45 MB/s). Every other route keeps the server's limits. **A server compromise can produce validly signed exports**: whoever controls the server or `AUDIT_SIGNING_KEY` can sign an altered history. The signature proves origin from the configured key, not that the log is complete or untampered.

**Warrant canary.** In the admin page, paste the operator's OpenPGP public key, then paste a statement the operator wrote and signed offline (`gpg --clearsign canary.txt`). The application stores it as pasted only if it verifies, re-verifies it on every view of `/canary`, and shows INVALID with the reason if the key changes or the stored text is altered. Keep the operator's private key off the server; the application never writes or signs canary text.

## Backups and recovery

Install [age](https://age-encryption.org/) and generate an identity offline (`age-keygen -o backup-key.txt`). Store the private key separately from backups and use its public recipient for encryption:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/market.dump.age
```

For external databases also set `BACKUP_DATABASE_URL`, install Python 3 and PostgreSQL client tools matching or newer than the server. `./scripts/install.sh upgrade` backs up through this script too, so on an external database give it both: `BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade` (the value of `DATABASE_URL` in `.env` serves when this host can reach that server; quote it in single quotes); without `BACKUP_DATABASE_URL` it refuses before changing anything. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage ([commands below](#secrets-env-and-onion-identities)). Database dumps do not include onion keys or blockchain volumes, and in particular **not the custodial payment wallets** (the `bitcoin_data` and `monero_wallet` volumes, or your external wallets): back those up on their own (see the [runbook](testnet-runbook.md#6-back-up-the-wallets)). Maintain offline copies and rehearse recovery.

Rehearse recovery on another host, or in a second checkout with its **own Compose project name**. Compose names containers, volumes and networks after the project (`opsecmkt` unless `.env` sets `COMPOSE_PROJECT_NAME`), not the directory: a second checkout under the same name recreates the live containers with its own secrets, and its `docker compose down --volumes` deletes the live volumes. Install the rehearsal copy with, for example, `COMPOSE_PROJECT_NAME=opsecmkt-rehearsal APP_PORT=8081 ./scripts/install.sh` (add `--local` for a quick local copy); the installer saves the name in that checkout's `.env`, so its later `docker compose`, `backup.sh` and `restore.sh` commands stay on the rehearsal project, and it refuses to install when containers of the chosen project were created from another directory, or when any of that project's volumes exist. If you copy a `.env` into the rehearsal checkout instead, change `COMPOSE_PROJECT_NAME` (add the line if it is missing) and `APP_PORT` in the copy before running any `docker compose` command there. Never change `COMPOSE_PROJECT_NAME` on an existing install: it would start on new, empty volumes (`<project>_postgres_data` and the others).

Without `BACKUP_DATABASE_URL`, the script dumps the internal database through `docker compose exec -T db`, so it needs no published database port or host PostgreSQL tools. It dumps the database named in `DATABASE_URL` in `.env` (the name after the port), so after a side-by-side restore or rollback that switches `DATABASE_URL`, backups follow the database the application uses. The success line names the database dumped (never the URL or password). If the script cannot read a plain lower-case name there (for example the name comes from Compose variable interpolation), set `BACKUP_INTERNAL_DATABASE` to it; a `BACKUP_INTERNAL_DATABASE` that differs from the name in `DATABASE_URL` is refused, and so is setting it together with `BACKUP_DATABASE_URL`.

**External databases:** `BACKUP_DATABASE_URL` is used exactly as given; the script does not look at `DATABASE_URL`. When you switch `DATABASE_URL` to a restored database, point `BACKUP_DATABASE_URL` (in your backup job or cron entry) at that database too, or every later backup silently dumps the old one. The success line names the database it dumped; check it.

Restore into an explicitly named **empty** destination. Stop the application first. For the internal database (the default deployment, which publishes no database port), keep the `db` service running and name a database inside it; client tools run in the container:

```sh
docker compose stop app
AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE=opsecmkt_restored \
  ./scripts/restore.sh backups/market.dump.age
```

A database that does not exist yet is created next to the current one, which stays untouched. Point the app at it by changing the database name in `DATABASE_URL` in `.env` (for example `postgres://opsecmkt:PASSWORD@db:5432/opsecmkt_restored?sslmode=disable`); the script prints this exact line for the database it restored into. With payments configured, cut the site off from users before starting the app on it, as the [reconciliation procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) describes; otherwise run `docker compose up -d`. Later `scripts/backup.sh` runs then dump `opsecmkt_restored`, not the abandoned `opsecmkt`. If the `postgres_data` volume itself was lost, `docker compose up -d --wait db` initializes an empty `opsecmkt` database and `RESTORE_INTERNAL_DATABASE=opsecmkt` restores into it with no `.env` change. For an external or otherwise host-reachable server, create an empty database there and restore with Python 3 and compatible PostgreSQL client tools:

```sh
RESTORE_DATABASE_URL='postgres://user:password@localhost/recovery?sslmode=verify-full' \
  AGE_IDENTITY=/secure/backup-key.txt ./scripts/restore.sh backups/market.dump.age
```

After switching `DATABASE_URL` to that database, set `BACKUP_DATABASE_URL` to it as well (see above).

Set exactly one of the two destinations. Either way the restore requires typing `RESTORE`, runs in one transaction, and does not drop existing tables: restoring into a database that already has the application's tables fails and changes nothing. The payout protection below is applied by the same SQL in both modes. Restore with the `APP_SECRET_KEY` (on an install without it, the `SETUP_TOKEN`) that was in use when the backup was taken, or TOTP secrets in it cannot be read (see [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). To roll back an upgrade, follow [UPGRADING.md](../UPGRADING.md#5-rolling-back).

The script sets a persistent recovery gate that pauses all outbound payouts,
including payouts created after restoration, and converts pending, sending, blocked and held payouts into
manual recovery holds (error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the wallet shows no broadcast transaction for it. Failed payouts are marked as possibly sent (same error prefix): one the wallet rejected before the backup may have been requeued and sent after it, so requeueing it needs the same confirmation. A payout held for an account suspension keeps that hold behind the restore marker, so releasing it also needs the payout address check. The script prints how many payouts it held and how many failed payouts it marked, and adds one audit row recording the gate and those counts. It also prints `<N> images restored from a backup taken <time>; photo and avatar removals made after that time are undone — check /moderator Recent photos and remove them again` (the time is when the dump was made, in UTC, read from the dump itself; "at an unknown time" if the client tools cannot read it; for a backup made before this release on a host not set to UTC it is off by that zone's offset from UTC): a staff takedown or an owner's removal deletes the image's row, so the restored database has every photo and avatar the backup had, including those removed since. Before users return, have a moderator go through Recent photos and take down again any image that had been removed. A database backup may predate an already-sent payout's creation, so reviewing only
existing payout rows is insufficient. Follow the [complete reconciliation and explicit unlock procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
before allowing user writes or restarting payment sends. Validate recovered accounts, listings, orders and
settings before switching traffic. Never test a restore against your live database; a test restore in a
second checkout on the same host needs its own `COMPOSE_PROJECT_NAME` (for example `opsecmkt-rehearsal`, see
[above](#backups-and-recovery)), or its `docker compose` commands act on the live containers and volumes.

**Any restore not done by `scripts/restore.sh` skips this payout protection.** A host or volume snapshot (including a copy of the `postgres_data` volume), a managed-database point-in-time recovery or a manual `pg_restore` brings payouts back as `pending` with no recovery gate, and the application sends them as soon as it starts, even those already paid after that point in time. After such a restore keep the application stopped and apply the protection SQL in [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database before starting it, then follow the same reconciliation procedure.

### Secrets, .env and onion identities

`scripts/backup.sh` dumps the database only. A restored database also needs the `.env` it ran with, and each onion address exists only as the keys in its Tor volume. Back both up whenever they change, encrypted to the same age recipient, and store them with the database backups (never next to the age identity).

| Secret | Kept in | Backup | Rotation |
| --- | --- | --- | --- |
| `APP_SECRET_KEY` | `.env` | with `.env` | Derives the CSRF/CAPTCHA key and the key that encrypts TOTP secrets. Restore a database with the key in use when it was dumped; under another key its TOTP secrets cannot be read. Changing it refuses every open form once and makes enrolled TOTP secrets unreadable. On a market running [Bitcoin escrow](#bitcoin-escrow-regtest-only) it also makes every registered escrow key unreadable (`/escrow-key` asks the user to register it again; existing escrows keep their keys, and the same key registered again is active at once) and restarts the check that stops two accounts registering the same key: a key registered before the change is not recognised until its owner registers it again. Rotate only when it is exposed, with the consequences [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token) describes. An install that predates it has none: its keys still derive from `SETUP_TOKEN` (the startup log says `keys: derived from SETUP_TOKEN (legacy)`); to move to it, set `APP_SECRET_KEY` to exactly the current `SETUP_TOKEN` value, which `./scripts/install.sh upgrade` does ([UPGRADING.md](../UPGRADING.md#app_secret_key-and-stricter-startup-checks)). |
| `SETUP_TOKEN` | `.env` | with `.env` | Authorizes the first-run `/setup` page only. The installer prints it once at the end of `init`; `./scripts/install.sh setup-token` shows it again (and `--new` reissues it) while setup is pending. The app refuses to start without it until setup is complete, and ignores it afterwards (while it is set, startup logs a hint); `/setup` then answers 403 whatever the token. Once `APP_SECRET_KEY` is set and setup is complete, `status`, `add-mirror` and `upgrade` remove it from `.env`. Without `APP_SECRET_KEY` it is also the legacy key root above: keep it, or copy it to `APP_SECRET_KEY` first; changing it then has every consequence listed for `APP_SECRET_KEY`, escrow keys included. |
| `AUDIT_SIGNING_KEY` | `.env` | with `.env` | Signs audit exports ([Transparency](#transparency)). A new key (`openssl rand -hex 32`, then restart the app) is a new public key on `/canary`: announce it. |
| `POSTGRES_PASSWORD` (internal database) | `.env`, twice: `POSTGRES_PASSWORD` and the password in `DATABASE_URL`; the database keeps its own hash in the `postgres_data` volume | with `.env` | `POSTGRES_PASSWORD` is used only when an empty volume is initialized, so editing it later changes nothing. Rotate in the database first: `docker compose exec db psql -U opsecmkt -d opsecmkt -c '\password opsecmkt'` asks for the new password twice and sends `ALTER ROLE opsecmkt PASSWORD` with it already hashed, so it appears in no command line or server log. Then put it in both places in `.env` and run `docker compose up -d`. A hex password (`openssl rand -hex 24`) needs no escaping in the URL. For an external database, change it on that server and in `DATABASE_URL`. |
| `BITCOIN_RPC_PASSWORD`, `MONERO_WALLET_RPC_PASSWORD` (local nodes) | `.env`, each repeated in `BITCOIN_RPC_URL` / `MONERO_WALLET_RPC_URL` | with `.env` | Change a password and the same value in its URL together, then `docker compose up -d`. The wallets behind them are separate backups ([runbook](testnet-runbook.md#6-back-up-the-wallets)). |
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

GitHub Actions repeats the race tests, vet, dependency verification, govulncheck and Compose validation using the Go toolchain pinned in `go.mod` (`toolchain` directive), the same version the Docker image builds with. CI supplies PostgreSQL 17 to the integration tests, which use an isolated schema. Locally set `TEST_DATABASE_URL` to a dedicated test database to include those tests. These checks do not substitute for a live Docker/Tor deployment test.

Backup/restore URLs require an explicit host and database. The helper decodes credentials into libpq environment variables so they are not included in process arguments. Standard TLS options are supported; unsupported query options fail closed. Environment variables remain visible to privileged host processes.

The encrypted backup/restore scripts were exercised against an isolated PostgreSQL 16.15 test cluster with age 1.3.2: two rows including Unicode round-tripped, encrypted output had mode 0600, existing backups were refused, a wrong identity failed without creating tables, and restoring into an occupied target rolled back without changing its rows. Both scratch databases and temporary keys/dumps were removed afterward. CI now also runs `scripts/test-internal-db-restore.sh`, which backs up and restores through the PostgreSQL 17 internal-db Compose service with no published port (synthetic tables, payout gate and holds, and backups following `DATABASE_URL` after a side-by-side restore); see [operations-tests.md](operations-tests.md). A full rehearsal on your own deployment, including the application and wallets, is still yours to run.

## Troubleshooting

If every form, including `/setup`, returns 403 "Origin does not match Host; if this market runs behind a reverse proxy, the proxy must forward the Host header.", the reverse proxy is rewriting `Host`. Forward the original header ([nginx example](#clearnet-and-onion-deployment)); these refusals are not logged.

The application logs to standard output (`docker compose logs app`). Every response with a 5xx status writes one line (after the log timestamp), and the error page the user sees ends with the same reference:

```text
http 5xx kind=page name=catalog status=500 ref=0bbdc379 cause=22021
```

- `kind` is `page` (a GET) or `action` (a form POST); `name` is the registered route: a page name such as `order`, an action path such as `/orders/pay`, a raw path (`/captcha`, `/admin/audit-export`, `/healthz`), an image route (`/img/` or `/desk/img/`, without the image id), or `unregistered`.
- `ref` is 8 random hex characters, shown to the user as `Reference: 0bbdc379`. Ask a user who reports an error for it and search the log for `ref=0bbdc379`.
- `cause` is a class, never the error's text: a PostgreSQL SQLSTATE, followed by the table and constraint when PostgreSQL names them (`23505 payouts payouts_order_id_key`); `timeout` (the 12 s request bound; a PostgreSQL statement timeout shows as `57014`); `canceled` (the client went away); `db-connection` (the database was unreachable or the connection broke); `template:<name>` (a page failed to render); `payment-address-already-issued` (a payment request refused because the wallet handed out an address the database already issued, see below); otherwise the Go type of the error (for example `*market.httpError`, a refusal the code raised itself such as "Authentication is busy").
- The line never holds the URL path or query, form values, cookies, the client address, `Host`, `User-Agent`, a handle or user id. Reproduce the failure, or match the time and route, to learn more.
- At most 10 such lines are written a second. Beyond that, one line counts the rest, `http 5xx suppressed=N (more than 10 lines in 1s)`, whose references are then not in the log.
- `57014` (statement timeout, 8 s) or `55P03` (lock timeout, 3 s) with status 503 means the application pool's [session timeouts](#migrations) stopped a query: the database is overloaded or a row stayed locked. Occasional lines under load are expected; a steady stream on one route points at a slow query.
- `57P01`, `57P02` or `57P03` (the database is shutting down, crashed or still starting), `53300` (too many connections), `08000`, `08001`, `08003`, `08004` or `08006` (a connection exception) or `db-connection`, with status 503, means the request's database connection was lost or refused, for example during a PostgreSQL restart or failover; the user is shown "The market is busy" or, when the first query of the request already failed, "Service unavailable", both with `Retry-After: 5`. A steady stream outside a restart points at the database's health or its `max_connections`.

**Overload.** The app accepts at most 2048 connections at once and serves at most 64 pages, 16 form posts that change a signed-in account's data and 16 public ones (sign-in, registration, the sign-in challenge, whoever sends them), 16 requests for public images (`/img/`, which take no page place, so a page of thumbnails or a flood of image requests cannot crowd out pages), 8 CAPTCHA images and 4 password checks at a time. A form post takes its place only after its body has been read (at most 64 KiB and 64 fields, within the 15 s read timeout) and its origin and form token have been checked, so a client that sends its body slowly, or posts junk, holds only a connection (a post that carries a session cookie then looks it up in one of 16 lookup places, waiting its turn for up to 2 s; pages never use them); sign-in and registration posts with a malformed handle or password, or a CAPTCHA that was not issued to that browser or has expired, are refused before they take a place. Public posts, anonymous or from a signed-in account, therefore cannot crowd out a signed-in user's or an administrator's changes (each account's own changes stop at its budget of 100 in ten minutes), but a flood of well-formed public posts can still delay real sign-ins. While all 2048 connections are taken, HTTP keep-alive is switched off (idle connections are closed) until no more than 1536 are in use. A request beyond the other limits is answered at once with 503 "The market is busy. Try again in a few seconds." and `Retry-After: 5`, and its connection is closed; these refusals are **not logged**, so a flood shows up as users reporting that message, not as log lines. Every other 503 also carries `Retry-After: 5`, and every 429 `Retry-After: 600`, except the one refusing a second payment request while the same account's first is still in progress, which carries `Retry-After: 5`. That one-at-a-time limit is per app process: with several app instances against one database, each allows one. `/healthz` takes no slot and pings the database on its own connection, so the container stays healthy under a flood of pages; it still waits for one of the 2048 connections. These limits are fixed in the code; on Tor they sit behind the onion service's own defences.

**Issued-address collisions (A-288).** A wallet restored from an older backup or seed hands out addresses the database has already issued to other orders. Each payment request that gets one is refused with 503 "BTC payment addresses are being restored on this market. Nothing was charged and your order is still a draft; try again later." (or XMR), the order stays a draft with no address, and the app logs one line with the currency and the number of refusals since the warning was last dismissed, never the address or the wallet's or database's error text:

```text
payments: BTC wallet returned a payment address already issued to another order (restored from an older backup?); payment request refused, order kept as a draft; collisions=3; see "Restore the wallets" in docs/testnet-runbook.md
```

The *Payment providers* panel of `/admin` shows "BTC wallet handed out payment addresses this market had already issued" with the count and the time of the last refusal. Follow [Restore the wallets](testnet-runbook.md#restore-the-wallets) (advance the wallet past the issued addresses and rescan), then press *Dismiss the BTC warning*: the warning stays until an administrator dismisses it (audited), even once payment requests succeed again, because buyers' retries walk the wallet forward without the rescan; a later collision shows it again. Requests are not held back meanwhile: each asks the wallet again and moves it on by one address the database already holds.

Each payout the watcher sends is logged whether it succeeds or fails, including while the application is stopping:

```text
payout id=12 order=5f2c9e7a currency=BTC amount=0.001 outcome=sent txid=<txid> recorded=yes
payout id=13 order=1b4d8036 currency=XMR amount=0.5 outcome=failed error=ambiguous/timeout recorded=yes
```

`error` is `definite` (the wallet refused before broadcasting) or `ambiguous` (it may have been broadcast), then `rpc:<code>` for a wallet JSON-RPC error or one of the classes above. `recorded=no cause=<class>` means the outcome could not be saved and the payout stays `sending`: the line keeps the txid of such a send, so check it in the wallet before you do anything with that payout. The wallet's own error text is on the admin *Payouts* panel and in no log line.

After a watcher pass that met errors, one more line lists them, separated by `; `, with only the currency, `order <short id>`, `payout <id>` and a class:

```text
payment watcher: XMR: pass skipped: wallet check failed: rpc:-13; BTC: payout 14: definite/rpc:-6
payment watcher: BTC: wallet read failed: *errors.errorString; BTC: order 1b4d8036: 40001
payment watcher: db-connection
```

`pass skipped` means that currency was not polled (no deposits read, no expiry, no payouts): `wallet check failed: <class>`, `refused by the network guard` (the provider stays disabled until the node is fixed and the application restarted), or `tip comparison failed`. A wallet class is `rpc:<code>` for a JSON-RPC error or one of the classes above; a wallet that cannot be reached shows as the Go type of its error. The wallet's text (which can quote an address) is the provider's last error on the admin page; neither this line nor the payout line prints it. `db-connection` (or `timeout`) alone means the watcher could not reach the database, or could not finish its pass within 5 minutes (for example while the pool was exhausted); the next pass picks up what it left; the line never holds `DATABASE_URL`, its user, database name, host or port. The recovery code reveal sweep logs `recovery code reveal sweep: <class>` the same way.

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
