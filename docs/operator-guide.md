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
| `check` | Validates `.env` and the Compose configuration and starts nothing: the app's startup refusals (placeholder or weak secrets, `COOKIE_SECURE`, a placeholder `DATABASE_URL`, `APP_MODE` against `COMPOSE_FILE`), `POSTGRES_PASSWORD` against the password in `DATABASE_URL`, `COMPOSE_PROFILES` against the database and node settings, node images on `latest`, the mirror outside Tor, the project's containers created from another checkout (or this checkout's containers under another project name), and `docker compose config`. When the directory those containers were created from no longer exists, the checkout was probably moved: `check` says so, and `docker compose up -d --force-recreate` run in the moved checkout recreates them with its directory, keeping the volumes (a plain `up -d` leaves them as they are). Do not change `COMPOSE_PROJECT_NAME` for this. Messages name keys, never values. |
| `status` | Mode, each service's health, whether setup is pending or complete, the schema version and every onion address. |
| `setup-token [--new]` | While setup is pending: shows the token and setup link, or with `--new` issues a new token and recreates the app. After setup it exits 1: the token grants nothing then. It never shows a token that is also the key root (an install without its own `APP_SECRET_KEY`); `--new` issues a separate one and keeps the keys. |
| `add-mirror` | See [Optional onion mirror](#optional-onion-mirror). |
| `upgrade [--no-backup]` | After `git pull`: see [UPGRADING.md](../UPGRADING.md). |

Flags answer `init`'s questions: `--tor` or `--clearnet`; `--local` (loopback HTTP, internal database, no nodes); `--external-db URL` (without it, a flagged install gets the internal database); `--behind-proxy yes|no` (clearnet: secure cookies behind an HTTPS proxy, or local HTTP only); `--nodes local|none|custom` (`local` runs pruned Bitcoin testnet4 and Monero stagenet nodes with the [default images](#node-image-versions), `custom` asks per coin); and per coin `--bitcoin` or `--monero` `local|external|disabled`, `--bitcoin-image`, `--bitcoin-network testnet4|signet`, `--bitcoin-prune yes|no`, `--bitcoin-rpc-url`, and the Monero equivalents with `--monero-wallet-rpc-url`. `./scripts/install.sh --help` lists them. A question without its flag is asked. A value given as a flag, such as an RPC URL with a password, is visible to other local users while the installer runs; answer the question instead to keep it off the command line.

The setup token opens `/setup` once. After setup, `status`, `add-mirror` and `upgrade` remove it from `.env` when `APP_SECRET_KEY` is set and the running app reports `keyroot=app_secret_key` (the next `docker compose up -d` drops it from the running app); it grants nothing by then. An app started before `.env` had `APP_SECRET_KEY` still derives its keys from the token, so the token stays until the app is recreated.

Commands that change `.env` (`init`, `setup-token --new`, `add-mirror`, `upgrade`, and `status` when it removes the token) hold the lock directory `.env.lock` while they run, so two of them cannot overwrite each other's changes; a second one refuses with a message (`status` only leaves the token). If a killed run left the lock behind, remove it with `rmdir .env.lock`. The installer switches off shell tracing, so `SHELLOPTS=xtrace` cannot print its secrets.

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

**External PostgreSQL and time zones.** Every database connection the app opens (the web app, its migrations, `-bootstrap-admin` and `-reset-admin-password`) sets `TimeZone=UTC` itself, overriding the server, database and role defaults and any `TimeZone` in `DATABASE_URL`. An external or host PostgreSQL left on local time therefore needs no change: pages show every time in UTC, labelled " UTC", and do not reveal the server's UTC offset. Other clients, such as `psql` or the backup scripts, still use the server's own zone.

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

The difference between two consecutive onion-service lines, divided by 1 800 seconds, is the rate of introductions (visitors asking to connect) and of rendezvous circuits (connections Tor let through). Introductions growing far faster than rendezvous circuits mean the proof-of-work queue is discarding requests: more visitors, or an attacker, than `HiddenServicePoWQueueRate` lets through. A sudden jump in open circuits is often the first sign of a flood. Tor logs the suggested puzzle effort only at the `info` level. To see it for a while, add `Log [rend]info stdout` after `Log notice stdout` in `deploy/torrc` and rebuild as above; Tor then also logs every queued request (`Enqueued rendezvous request with effort: N. Queued requests: M`), which is noisy under attack, so remove it afterwards.

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

The second command needs the internal database; run the same query in `psql` against an external one. The app holds at most 12 database connections.

- **The app is busy:** its CPU is at its limit or most of the 12 connections are `active`, and it answers with `503`. This is real overload, and proof of work is doing its job if visitors with Tor Browser still get in, slowly. If it persists, lower `HiddenServicePoWQueueRate` towards what the app handles (30 / 300 is the conservative setting).
- **The app is idle:** the probe fails or takes tens of seconds while the app's CPU is low and its connections are `idle`. This is the **OnionFlation** signature (Lee, Kim and Kang, "Onions Got Puzzled", USENIX Security 2025): short bursts of requests make Tor raise the suggested puzzle effort for everyone although the app has spare capacity, and real visitors' puzzles become too hard to finish in time. **Raise `HiddenServicePoWQueueRate` (with the burst at about ten times the rate); do not lower it.** A faster queue drains the bursts before they look like congestion; a slower one makes the queue look congested sooner and pushes the effort higher still. Raise it in steps (for example 60, 120, then 250, Tor's default), keep the introduction limit above it (see the table) and return to 60 once the attack stops.

### Tor image

[`deploy/Dockerfile.tor`](../deploy/Dockerfile.tor) installs tor from the Tor Project's own Debian repository as its [installation instructions](https://support.torproject.org/little-t-tor/getting-started/installing/) describe: the archive signing key, refused unless its fingerprint is `A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89`, is trusted for that repository only, and apt verifies the package with it. The Debian base image is pinned by digest and tor by exact package version (`TOR_VERSION`, now `0.4.9.13-1~d12.bookworm+1`), and the build fails unless `tor --list-modules` reports `pow: yes`. Building needs access to `deb.torproject.org` (HTTPS) and `deb.debian.org`; the built image runs offline until Tor itself connects.

The Tor Project's repository lists only its newest release. When it has moved past the pinned version, the build installs the repository's current release instead, but only if it is the same or newer than the pin, and prints `tor ... is no longer published; installing the newer ...`; it never installs an older tor, and the fingerprint and `pow: yes` checks still apply. An existing install keeps using the image it built. To build one specific release instead, print the version the Tor Project currently publishes

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

The installer asks for each coin (bare `./scripts/install.sh`, or `init --nodes custom`; `--nodes local` takes every default below) whether to run a `local` node in Docker (the default; pruned unless you answer `no`, see [Local node pruning](#local-node-pruning)), use an `external` node you run elsewhere, or leave that coin `disabled`. `scripts/install.sh --local` (developer mode) never starts nodes. For a local node, the installer defaults to [`bitcoin/bitcoin:31.1`](https://hub.docker.com/r/bitcoin/bitcoin) for Bitcoin Core and Seth for Privacy's [`simple-monerod:v0.18.5.1`](https://github.com/sethforprivacy/simple-monerod-docker) plus [`simple-monero-wallet-rpc:v0.18.5.1`](https://github.com/sethforprivacy/simple-monero-wallet-rpc-docker) images for Monero (see [Node image versions](#node-image-versions)). Press Enter to use these defaults or supply an image override. The Bitcoin image is community-published and explicitly unofficial; it verifies upstream Bitcoin Core binaries. The Monero images are community-maintained, not published by the Monero project. Review publisher and image provenance before use. A tag can be re-pushed by its publisher; append an immutable `@sha256:<64 lowercase hex characters>` digest in `.env` for a reproducible deployment. Bitcoin and Monero use the same daemon image on test and live networks; the network is selected with daemon flags, not by changing the image. The installer offers Bitcoin `testnet4` or `signet`, and Monero `stagenet` or `testnet`. **Live is not supported by the payment adapters:** selecting it exits before writing `.env`, because the application refuses mainnet wallets and addresses. Full live-payment support needs a separate adapter and payout-safety change.

Both nodes store their blockchain in dedicated volumes and publish no ports. Even pruned, they need several GB of disk and hours for the first synchronization (sizes below). Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payments are implemented for test networks only; mainnet is refused at startup.** With a wallet RPC configured (see [Payments](#payments)), the app issues deposit addresses, watches confirmations and sends single-attempt releases and refunds from the operator's pooled test-network wallet. That is custodial holding, not multisig escrow, and there is no fee accounting. Do not send mainnet coins. Providers are enabled by environment variables and a restart. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

### Node image versions

The node images default to release tags, never `latest`: `bitcoin/bitcoin:31.1` (Bitcoin Core v31.1), `ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1` and `ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1` (Monero v0.18.5.1). They are the versions the payout failure rules were checked against. When a send fails, the application decides from the wallet's error whether nothing was broadcast (a definite rejection an administrator may requeue) or the send may have gone out (ambiguous: requeueing needs a wallet check, see [Payments](#payments) and the [runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)). The Monero "nothing broadcast" table (`moneroPreSubmitCodes` in `internal/market/payments_watcher.go`: -2, -16, -17, -18, -19, -20, -37) holds for **monero-wallet-rpc v0.18.5.1**; the Bitcoin rule that every `sendtoaddress` error is definite was read from Bitcoin Core v31.1. A newer wallet could raise one of those codes after it has submitted the transaction, and the administrator would then be offered a requeue that pays twice.

`BITCOIN_IMAGE`, `MONERO_IMAGE` and `MONERO_WALLET_IMAGE` in `.env` override the defaults. An `.env` that sets any of them to `:latest` (as the installer did before these tags were pinned) or to no tag keeps floating: change it to the tags above, optionally with `@sha256:<digest>` appended (`docker buildx imagetools inspect <image:tag>` prints the digest). CI's Compose job fails if a default in `compose.nodes.yaml` is `latest` or untagged; it cannot see your `.env`.

Before changing a tag (default or `.env`):

1. Monero: read the new release's wallet source and re-verify every code in `moneroPreSubmitCodes`: each must still be raised only before `wallet2::commit_tx` submits anything (`src/wallet/wallet_rpc_server_error_codes.h`; `validate_transfer`, `on_transfer` and `handle_rpc_exception` in `src/wallet/wallet_rpc_server.cpp`; `commit_tx` in `src/wallet/wallet2.cpp`). Remove any code that can now follow a submission, and update the version and line references in the comment and in [implementation status P5](implementation-status.md#p5-payments). Move `MONERO_IMAGE` and `MONERO_WALLET_IMAGE` to the same release.
2. Bitcoin: check that `sendtoaddress` still stores the transaction in the wallet before submitting it and returns the txid, not an error, when relay fails (`src/wallet/rpc/spend.cpp`, `CWallet::CommitTransaction` in `src/wallet/wallet.cpp`), and update the `walletRejected` comment.
3. Change the tag in `compose.nodes.yaml`, `.env.example` and `scripts/install.sh` in the same commit. `tests/test_installer.py` fails if the three disagree, if a default is `latest`, or if the versions named in the `payments_watcher.go` comments do not match the tags.
4. Run the payment tests (`TEST_DATABASE_URL=<dedicated test database> go test -race -count=1 ./...` and `E2E_WALLET_DATABASE_URL=<fresh database> npm run test:e2e:wallet`, both against simulated wallets) and, preferably, the [local-chain journey](local-chain-testing.md) against the new images.

With a wallet version nobody has verified (a newer tag, a `latest` left in `.env`, or your own image), **treat every Monero send failure as possibly broadcast**: whatever the *Payouts* panel says, check the wallet as for an ambiguous failure ([runbook step 5](testnet-runbook.md#5-recover-a-held-or-failed-payout)) before requeueing a failed XMR payout.

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

The application's pool opens with fixed per-connection limits (startup parameters, set by the app, not in PostgreSQL's configuration): `statement_timeout` 8 s, `lock_timeout` 3 s and `idle_in_transaction_session_timeout` 15 s, so one slow query or held row lock cannot tie up the pool for longer. A payment request, and whatever may queue behind one (a checkout, listing edit, archive or takedown, a cancellation returning stock, a payment-intake change, a payout to the buyer, and an administrator's suspension, second-factor reset or role change), waits up to 11 s for a row lock instead (its statement limit is raised to 11 s with it), because the payment request holds its rows while the wallet is asked for an address (up to 10 s). Each account has at most one payment request in progress at a time; another is refused at once with 429 "Your previous request is still being processed" and `Retry-After: 5`. Startup opens this pool and checks that the database accepts it **before** migrating, so a database or pooler that refuses these parameters stops startup with nothing changed (see [Connection poolers](#connection-poolers)). Migrations then run on their own database connection without the limits; the migration transaction also lifts any that are set on the database role. `-status`, `-bootstrap-admin`, `-reset-admin-password`, `scripts/backup.sh`, `scripts/restore.sh` and `psql` sessions do not have them either. A page or form stopped by one answers 503 ("The market is busy") and logs cause `57014` (statement) or `55P03` (lock); see [Troubleshooting](#troubleshooting).

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

  The migration transaction lifts these itself, and `pg_dump` and the restore script set their own. `-bootstrap-admin` and `-reset-admin-password` then run under them too, which their short transactions do not notice. Without the role settings the app runs with no statement or lock limit, as before A-177.

The pool also sets `TimeZone` to UTC as a startup parameter; PgBouncer passes `TimeZone` through by itself.

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

All keys below are listed in `.env.example` and passed by `compose.yaml`. Blank or absent values keep a feature disabled.

### Authentication

No environment keys. Second-factor secrets are encrypted with a key derived from `APP_SECRET_KEY` (on an install without it, from `SETUP_TOKEN`: see [Secrets](#secrets-env-and-onion-identities)); changing that key makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off (the sign-in's *I lost my authenticator* checkbox does both at once) and enroll again (an administrator resets a non-administrator with no recovery code left under *Admin → Reset a user's second factors*; the administrator's own account needs the operator reset in [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). Startup refuses a placeholder or obviously non-random `APP_SECRET_KEY` or `SETUP_TOKEN`; use `openssl rand -hex 32`.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code and the account password, and 10 one-time recovery codes are shown once. Turning TOTP or PGP sign-in on or off, replacing recovery codes and adding, changing or removing the PGP key all need the account password (and a current code when TOTP is on), and each notifies the account; so do sign-ins with a recovery code and the first failed second step after a correct password. A user who reports such a notification they did not cause should change their password at once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **Lost authenticator**: a user (including an administrator) with an unused recovery code recovers without staff. On the sign-in's *Use a recovery code instead* form, they tick *I lost my authenticator — turn two-step sign-in off*: the code signs them in and TOTP is turned off in the same step (the other recovery codes are deleted; PGP sign-in, if on, stays on), then they set up a new authenticator on `/totp`. Without the checkbox, a recovery-code sign-in lands on `/totp` showing how many codes remain, where TOTP can still be turned off with the password and another recovery code. Only a user with no recovery code left needs the *Second-factor reset* below (or, for an administrator, the operator procedure).
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio alternative, so turning it off may be needed for users who cannot read it. First-run setup never shows it. Showing a challenge writes nothing to the database; the id of each answered challenge is kept in `captcha_spent` until it expires (at most 10 minutes) so it cannot be reused. A challenge shown before an upgrade to this version is refused after it; the user reloads the page.
- **Sign-in pauses**: after 10 failed passwords for one handle within ten minutes, sign-in for that handle is refused (HTTP 429) until that window ends, at most 10 minutes, **even with the right password**; the page says sign-in for this handle is paused, that someone else may have caused it and that the password may be fine. Correct sign-ins, registrations and setup attempts do not count. The limit is per handle because there is no per-client signal over Tor, so **anyone who knows a handle can trigger it, including for your administrator account**, and keep it paused by repeating 10 wrong guesses every ten minutes: each guess needs a solved CAPTCHA while CAPTCHA is on, and costs nothing when it is off. While any handle is paused, the app logs `sign-in limiter refusing handles=N` (a count, never a handle) at most once a minute, when a sign-in fails or is refused; watch for it with `docker compose logs app | grep 'sign-in limiter'`. Break-glass: the limits are held in memory, so restarting the app (`docker compose restart app`) clears every pause; sign in immediately afterwards, before an attacker can spend the budget again. Other users' pauses are cleared too, and a restart does not stop the attacker from repeating it.
- **Password change**: users change their own password on *Account → Change password* with the current password (plus an authenticator code or an unused recovery code when TOTP is on). Every other session and pending sign-in of that account ends. There is no email or administrator password reset: a user who forgets their password cannot be recovered through the application.
- **Second-factor reset**: an administrator can turn off TOTP (deleting its secret and recovery codes) and PGP sign-in for a non-administrator account under *Admin → Reset a user's second factors* by entering its handle (exact, case-sensitive; any account, not only those in the list of up to 100 shown there), confirmed with the administrator's own password (and authenticator code when enrolled). The account's PGP key and verification stay; its sessions and pending sign-ins end, both accounts get an audit row and the user is notified. Anyone who knows that account's password can then sign in, so confirm the request really comes from its owner (for example with a message signed by their verified PGP key) before resetting.
- **Account suspension**: to contain a compromised or abusive account, an administrator enters its handle under *Admin → Suspend or restore an account*, chooses *Suspend* and confirms with their own password (and authenticator code when enrolled). Its sessions and pending sign-ins end at once and it cannot sign in until restored: after a correct password it is told "This account is suspended. Contact the market staff." Its role, listings and orders are unchanged (change the role to buyer as well if a vendor's listings should be archived), but orders waiting on this account stop until it is restored; the other party can still complete, cancel where allowed, or open a dispute. A suspended moderator does not count as a dispute resolver: it is not notified of disputes or payment reviews and is not offered as a dispute contact, so if it was the only moderator who is not a party to a disputed order, that dispute is treated as having no eligible resolver until you restore it or another moderator exists. Suspension also holds the account's unsent payouts: whoever had its password may have changed its payout address (the owner is notified of every change). In the same step every `pending` or `blocked` payout to it, and any the watcher was holding, becomes held ("Held: account suspended — check the payout address before releasing" under *Payouts*), and a payout queued for it later (an order completed or refunded while suspended) starts held. Restoring the account does not release them, and the watcher never does. Release each one from *Payouts* only after checking its payout address, shown in the release form, with the account owner through a channel you trust; the release needs your password and the "Payout address checked" confirmation (without it the server answers 409 and changes nothing), is audited with the address, and the payout is then sent once. A held payout with no payout address cannot be released until the recipient saves one (after you restore the account); check that address with them first. A held payout that already has an address keeps it when the owner saves a new one (they are told which orders still use the previous address): if the owner says the payout's address is not theirs, check the new address with them and use *Use the account's current address* on that payout (see below) before releasing it. The panel lists suspended accounts; *Restore* with the same form allows sign-in again. Both changes are audited on both accounts and the user is notified. Administrator accounts, including your own, cannot be suspended there.
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

  Check that the `UPDATE` affected one row, then sign in with the password and enroll TOTP again.
- **First administrator from the host** (instead of entering `SETUP_TOKEN` in the browser on `/setup`; `./scripts/install.sh init --admin ADMIN_HANDLE` does this during installation): an operator on the host, in the deployment directory (with its `.env`), after the app has started once so the database schema exists, runs (replace `ADMIN_HANDLE`: 3–32 letters, digits or underscores, exact and case-sensitive; `-site-name` is optional):

  ```sh
  docker compose run --rm app -bootstrap-admin ADMIN_HANDLE -site-name "My Market"
  ```

  It connects with `DATABASE_URL`, starts no web server and needs no setup token. It prompts twice for the administrator's password without showing it (12–72 bytes, as on `/setup`) and then, in one transaction under the same lock as `/setup` (so it cannot race the browser form or another run), creates that one administrator, records the marketplace name (default `OPSMKT`, at most 80 bytes; change it later on `/admin`), marks the market installed and writes the same audit row as `/setup` ("Account created: admin"). It prints only the handle; sign in on `/login` with that handle and password. From then on `/setup` refuses every token. It refuses, changing nothing, when the market is already set up (by `/setup` or an earlier run), a handle, password or name that breaks the rules above, a database the server has never started against, and a database upgraded by a newer release. Arguments go after the service name, as for `-reset-admin-password` below. Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input (`docker compose run --rm -T app -bootstrap-admin ADMIN_HANDLE`); it is never accepted as an argument. Anyone who can run this command with the deployment's `.env` can create the first administrator, just as they could read `SETUP_TOKEN` from it; there is no web equivalent.
- **Forgotten administrator password**: the application has no web password reset. An operator on the host, in the deployment directory (with its `.env`), runs (replace `ADMIN_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-password ADMIN_HANDLE
  ```

  It connects with `DATABASE_URL`, starts no web server, prompts twice for the new password without showing it (12–72 bytes, the registration rule) and, in one transaction, stores its hash, ends every session and pending sign-in of that account and writes the audit row "Password reset by the operator on the host". It refuses an account that is not an administrator, an unknown handle, and a database upgraded by a newer release (as startup does); nothing changes then. Arguments go after the service name because the image's entrypoint is the server itself (do not repeat `/app/server`). Without a terminal, for example from a script, add `-T` and pipe the password as a single line on standard input; it is never accepted as an argument. The site can keep running. Second factors are unchanged: if they are lost too, also run the SQL above.

### PGP identity

No configuration. Users paste an ASCII-armored public key on `/account`; it is stored without armor headers or surrounding text, and the fingerprint and user IDs are shown and ownership is proven on `/pgp` by signing a server text or decrypting a message encrypted to the key (for example `gpg --clearsign`, `gpg --decrypt`). A verified key can be used as a sign-in second factor. Changing the key clears verification and PGP sign-in. `/messages` accepts only OpenPGP-encrypted messages and labels each by whether its recipient key IDs match the recipient's saved key; the server never decrypts or holds private keys. Losing the private key locks PGP sign-in unless another second factor is enrolled or an administrator resets the account's second factors (see Authentication). Vendor pages and order pages show the other party's armored public key, fingerprint and whether ownership is verified, and "Message vendor"/"Contact vendor" links open `/messages?to=<handle>` with the recipient filled in; without a saved key, the page says messages cannot be sent until one is added.

### Orders

No configuration. Requesting payment on an order requires a test-network wallet provider for its currency (see Payments); without one the step is shown as unavailable. Migration 030 adds `deliveries`, `reviews` and `disputes.outcome`. Digital delivery content is stored unencrypted in the database and shown only to the order's buyer and vendor, and read-only to moderators and administrators once the order is disputed; vendors should encrypt sensitive content to the buyer's PGP key before delivering it.

**There is no shipping address field, by design.** For a paid physical order the order page asks the buyer to encrypt their address to the vendor's PGP key in their own PGP application and send it through `/messages`, which accepts only OpenPGP-encrypted messages; the server keeps that encrypted message (and who sent it to whom and when) but cannot read it. A vendor without a saved key cannot receive addresses until they add one. The market cannot stop an address being typed into a plaintext field: the vendor's note to the buyer, cancellation reasons, dispute reasons and review text are stored unencrypted in the database and every backup. Each of those fields warns that it is unencrypted, says who can read it and asks users never to include an address, real name or tracking number but to send them encrypted from the order page (the dispute forms point to the "Contact dispute staff" keys). `/canary#records` lists what is kept.

Opening a dispute notifies every moderator and administrator who is not the order's buyer or vendor and is not suspended. A moderator or administrator who is a party cannot resolve it, and a suspended one cannot sign in, so keep at least one moderator who does not trade: if every staff account that is not suspended is a party (for example the only administrator is the vendor and the only moderator is suspended or absent), the dispute is still accepted, the order history records that no independent resolver was available, every administrator is notified that it needs a moderator who is not a party, and the moderation desk marks it "No eligible resolver" for administrators until such a moderator exists.

Resolving queues one payout of the whole counted amount to one party; the payment watcher sends it automatically. There is no split, and nothing refunds an overpayment automatically. The resolve form states each outcome's amount, recipient, difference from the price and the recipient's payout state, and needs a ticked confirmation; if another deposit is counted after the form was shown, resolving is refused with 409 and the moderator reviews the new amount. Completing an order and a vendor's cancel of a paid order work the same way.

Moderators and administrators open the order page of a disputed (or resolved) order they are not party to, read-only, from the moderation desk; buyer and vendor actions stay unavailable to them. Roles are assigned under *Admin → Assign an account role* by entering the account's handle (exact, case-sensitive; the list there shows only the 100 newest accounts). Changing a vendor's role to buyer or moderator archives their active listings in the same audited transaction and blocks new drafts and payment requests on them; existing orders continue.

Reviews are published as soon as the buyer writes them. To take one down (for example text that names or locates a vendor), sign in as an administrator, open the listing or the vendor's page, open *Hide this review* under it and confirm with your password (and authenticator code when enrolled). The review is kept but leaves both public pages and their rating; the buyer and vendor still see it on the order, and administrators see it labelled with *Restore this review*. Both actions are audited without the review text and the reviewer is notified. Migration 031 adds `reviews.hidden_at`.

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after a payment provider confirms payment; with payments disabled it is never released automatically. Administrators can edit other vendors' listings but cannot view or change their automatic delivery content: the editor shows only whether it is set, and only the vendor can change or remove it.

To remove another vendor's listing (for example counterfeit or prohibited goods) while the vendor keeps selling everything else, open its edit page as an administrator, open *Take down listing*, give the vendor a reason of 10–500 characters and confirm with your password (and authenticator code when enrolled). The listing is archived; the vendor is notified with the reason and sees it on the listing and the vendor desk, can still edit the listing, but cannot restore it. The reason is stored unencrypted and kept in both accounts' activity logs; it is never shown publicly, but do not put an address or real name in it. While the listing is taken down, paid orders on it are not delivered automatically: they wait for the vendor to deliver by hand or cancel and refund. *Restore listing* on the same page, confirmed the same way, undoes it. To take away all of a vendor's listings, change their role instead (that archive carries no reason and the vendor can restore those listings if made a vendor again). Migration 041 adds `products.admin_archive_reason`.

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
- There is **no commission or fee accounting**. Bitcoin payouts deduct the network fee from the amount sent (`subtractfeefromamount`). Monero payout fees are paid by the pooled wallet on top of the payout, so keep a small test-coin buffer in it. Payouts below a minimum are never queued but flagged to moderators: 0.0001 BTC for any Bitcoin payout (a smaller one cannot cover the fee) and 0.001 XMR for a Monero refund (not worth the fee); listings cannot be priced below 0.0001 BTC.
- Payouts are **single-attempt**. A failed or interrupted wallet call is shown on the admin page and never retried automatically, because the transaction may already have been broadcast. After checking the wallet, an administrator can release a held payout, requeue a failed or stuck one, or record one as sent with its transaction ID (password-confirmed and audited). A failure the wallet rejected with a pre-broadcast error (nothing broadcast) requeues with the password alone; requeueing one whose outcome is unknown (timeout, dropped connection, or a Monero error such as -38 that can follow a submission; see the [runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)) or a stuck send, or releasing a payout held by (or requeueing a failed payout marked by) a restore from backup, also needs an explicit, audited confirmation that the wallet shows no broadcast transaction. A wallet send may take up to 30 seconds (ordinary wallet reads 10 seconds). A shutdown or redeploy lets an in-flight send finish and its outcome be recorded (at most 30 seconds plus 10 to record; even added to the 10-second HTTP drain this stays within the app's 60-second stop grace period).
- The watcher never cancels an order for non-payment unless it read that order's deposit address from the wallet in the same pass, and it skips a currency entirely while its node is still syncing (Bitcoin `initialblockdownload`; Monero daemon `target_height` above `height`, when `MONERO_RPC_URL` is set) or while the node's tip is below the highest tip it has recorded for that currency (Bitcoin `blocks`; the Monero wallet's `get_height`), as after a node or wallet restarted or restored from an older state. The admin page's last error then reads "Node is behind the highest tip recorded ...". If you deliberately replace a chain with a shorter one (a reset regtest, for example), clear the record so the watcher resumes: `UPDATE payment_status SET tip_height=NULL WHERE currency='BTC';` (or `'XMR'`).
- A credited deposit that later conflicts, disappears or drops below the confirmation threshold (a reorg) holds the order's unsent payout and, while the order is open or its payout unsent, is announced once per episode to the order history, the buyer, the vendor and every moderator and administrator not party to the order and not suspended; staff can then read the order and find it under *Payment review*. The payout resumes automatically once the deposit confirms again, however old the order, which ends the episode; a later regression is announced again. The order state is never reverted automatically. On *Payouts* such a held payout shows the deposit, its address and confirmations instead of a *Release* form.
- A payout is sent only in a watcher pass that has just read its order's deposit address from the wallet, so a deposit reversed while the payout waited (blocked, held or failed, then released or requeued) is caught before the send. Orders with a payout waiting to be sent are watched whatever their age, so a released payout normally goes out on the next pass.
- Funded orders that are not yet completed, cancelled or resolved stay watched for as long as they are open. A deposit first seen after an order was funded is noted once in the order history and to the buyer and vendor; it is part of the order's single release or refund if it has the confirmation threshold when the order settles; otherwise it is not paid out automatically (see late deposits below).
- Late deposits: closed orders stay watched while their address is less than **30 days** old and a deposit is still confirming or unhandled. Funds that confirm after a cancellation that refunded nothing are refunded to the buyer (blocked until the buyer saves an address). Extra funds on completed or resolved orders, or after a refund was already queued, are flagged to moderators, not paid out. Deposits to older addresses are not seen by the watcher (unless the order still has a payout waiting to be sent or held); handle them by hand from the wallet.
- Monero transfers with a non-zero `unlock_time` (locked transfers) are recorded and shown, but never counted toward payment or paid out. The order history notes "locked transfer ignored" once and moderators and the buyer are notified.
- Recipients must save a payout address on their account page. Payouts wait ("blocked") until they do. Saving or removing a payout address needs the current password, plus an authenticator code when TOTP is enrolled, and notifies the account (naming the currency, not the address).
- Saving a payout address never moves an unsent payout that already has one, since anyone with the account's password can save an address. The notification names the orders instead: "N unsent payout(s) still use your previous address (orders …); an administrator must confirm the change" for held payouts and definite failures, and separately those queued, being sent or possibly broadcast, which stay on the previous address. On *Payouts*, a held payout or a failure the wallet rejected before broadcast whose recipient has since saved a different address offers *Use the account's current address*: it shows both addresses and when the saved one last changed, needs your password and the "Current address checked" confirmation after you check the address with the account owner through a channel you trust, audits the old and new address, and changes only the destination (release or requeue it afterwards). A payout being sent, stuck in sending, with an unknown send outcome, or held or marked by a restore from backup is never moved; the server refuses it (409) ([runbook](testnet-runbook.md#5-recover-a-held-or-failed-payout)).

`scripts/regtest-smoke.sh` is a manual check against a local `bitcoind -regtest` (`BITCOIN_REGTEST_RPC_URL=http://user:pass@127.0.0.1:18443`); see the [runbook](testnet-runbook.md#7-manual-regtest-check-bitcoin). It is not part of CI.

### Transparency

`AUDIT_SIGNING_KEY` (64 hex characters, an Ed25519 seed generated by the installer with `openssl rand -hex 32`) enables signed audit exports. It is read only from the environment, never stored in the database; blank or malformed values leave the export visibly unavailable. Changing it changes the public key, so treat it like any long-lived signing key: back it up with `.env` and announce rotations.

**Exports are private operator evidence.** An export names every account's handle and its activity: sign-ins, account changes, order actions, key fingerprints, short order IDs and transaction IDs. Store it like a database backup and never publish it. Visitors do not receive exports; `/canary` publishes only the public key, so that you can prove an export you hold came from this server. `/canary#records` tells users what the server keeps and that there is no account or message deletion.

**Pin the audit public key.** The admin page and `/canary` show the hex Ed25519 public key. Record it once through a channel you trust (for example in the operator-signed canary text) and verify every export you hold against that pinned value, not against the key printed in the `.sig` file:

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
manual recovery holds (error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the wallet shows no broadcast transaction for it. Failed payouts are marked as possibly sent (same error prefix): one the wallet rejected before the backup may have been requeued and sent after it, so requeueing it needs the same confirmation. A payout held for an account suspension keeps that hold behind the restore marker, so releasing it also needs the payout address check. The script prints how many payouts it held and how many failed payouts it marked, and adds one audit row recording the gate and those counts. A database backup may predate an already-sent payout's creation, so reviewing only
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
| `APP_SECRET_KEY` | `.env` | with `.env` | Derives the CSRF/CAPTCHA key and the key that encrypts TOTP secrets. Restore a database with the key in use when it was dumped; under another key its TOTP secrets cannot be read. Changing it refuses every open form once and makes enrolled TOTP secrets unreadable, so rotate only when it is exposed, with the consequences [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token) describes. An install that predates it has none: its keys still derive from `SETUP_TOKEN` (the startup log says `keys: derived from SETUP_TOKEN (legacy)`); to move to it, set `APP_SECRET_KEY` to exactly the current `SETUP_TOKEN` value, which `./scripts/install.sh upgrade` does ([UPGRADING.md](../UPGRADING.md#app_secret_key-and-stricter-startup-checks)). |
| `SETUP_TOKEN` | `.env` | with `.env` | Authorizes the first-run `/setup` page only. The installer prints it once at the end of `init`; `./scripts/install.sh setup-token` shows it again (and `--new` reissues it) while setup is pending. The app refuses to start without it until setup is complete, and ignores it afterwards (while it is set, startup logs a hint); `/setup` then answers 403 whatever the token. Once `APP_SECRET_KEY` is set and setup is complete, `status`, `add-mirror` and `upgrade` remove it from `.env`. Without `APP_SECRET_KEY` it is also the legacy key root above: keep it, or copy it to `APP_SECRET_KEY` first. |
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

- `kind` is `page` (a GET) or `action` (a form POST); `name` is the registered route: a page name such as `order`, an action path such as `/orders/pay`, a raw path (`/captcha`, `/admin/audit-export`, `/healthz`), or `unregistered`.
- `ref` is 8 random hex characters, shown to the user as `Reference: 0bbdc379`. Ask a user who reports an error for it and search the log for `ref=0bbdc379`.
- `cause` is a class, never the error's text: a PostgreSQL SQLSTATE, followed by the table and constraint when PostgreSQL names them (`23505 payouts payouts_order_id_key`); `timeout` (the 12 s request bound; a PostgreSQL statement timeout shows as `57014`); `canceled` (the client went away); `db-connection` (the database was unreachable or the connection broke); `template:<name>` (a page failed to render); otherwise the Go type of the error (for example `*market.httpError`, a refusal the code raised itself such as "Authentication is busy").
- The line never holds the URL path or query, form values, cookies, the client address, `Host`, `User-Agent`, a handle or user id. Reproduce the failure, or match the time and route, to learn more.
- At most 10 such lines are written a second. Beyond that, one line counts the rest, `http 5xx suppressed=N (more than 10 lines in 1s)`, whose references are then not in the log.
- `57014` (statement timeout, 8 s) or `55P03` (lock timeout, 3 s) with status 503 means the application pool's [session timeouts](#migrations) stopped a query: the database is overloaded or a row stayed locked. Occasional lines under load are expected; a steady stream on one route points at a slow query.

**Overload.** The app accepts at most 2048 connections at once and serves at most 64 pages, 16 form posts that change a signed-in account's data and 16 public ones (sign-in, registration, the sign-in challenge, whoever sends them), 8 CAPTCHA images and 4 password checks at a time. A form post takes its place only after its body has been read (at most 64 KiB, within the 15 s read timeout) and its origin and form token have been checked, so a client that sends its body slowly, or posts junk, holds only a connection (a post that carries a session cookie then looks it up in one of 16 lookup places, waiting its turn for up to 2 s; pages never use them); sign-in and registration posts with a malformed handle or password, or a CAPTCHA that was not issued to that browser or has expired, are refused before they take a place. Public posts, anonymous or from a signed-in account, therefore cannot crowd out a signed-in user's or an administrator's changes (each account's own changes stop at its budget of 100 in ten minutes), but a flood of well-formed public posts can still delay real sign-ins. While all 2048 connections are taken, HTTP keep-alive is switched off (idle connections are closed) until no more than 1536 are in use. A request beyond the other limits is answered at once with 503 "The market is busy. Try again in a few seconds." and `Retry-After: 5`, and its connection is closed; these refusals are **not logged**, so a flood shows up as users reporting that message, not as log lines. Every other 503 also carries `Retry-After: 5`, and every 429 `Retry-After: 600`. `/healthz` takes no slot and pings the database on its own connection, so the container stays healthy under a flood of pages; it still waits for one of the 2048 connections. These limits are fixed in the code; on Tor they sit behind the onion service's own defences.

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

`pass skipped` means that currency was not polled (no deposits read, no expiry, no payouts): `wallet check failed: <class>`, `refused by the test-network guard` (the provider stays disabled until the node is fixed and the application restarted), or `tip comparison failed`. A wallet class is `rpc:<code>` for a JSON-RPC error or one of the classes above; a wallet that cannot be reached shows as the Go type of its error. The wallet's text (which can quote an address) is the provider's last error on the admin page; neither this line nor the payout line prints it. `db-connection` (or `timeout`) alone means the watcher could not reach the database, or could not finish its pass within 5 minutes (for example while the pool was exhausted); the next pass picks up what it left; the line never holds `DATABASE_URL`, its user, database name, host or port. The recovery code reveal sweep logs `recovery code reveal sweep: <class>` the same way.

When the database connection fails at startup the server exits with its class and no part of `DATABASE_URL`: `authentication failed (SQLSTATE 28P01)` or `(SQLSTATE 28000)` for a wrong user or password or a `pg_hba.conf` rule, `database missing (SQLSTATE 3D000)`, `host not found`, `connection refused`, `timed out`, or `TLS required` (the server accepts only encrypted connections; set `sslmode=require` or `verify-full`).
