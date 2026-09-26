# OPSEC Market operator guide

Deployment, feature configuration, migrations and recovery reference. Start with
[the README](../README.md) for product context and local development. Commands
below run from the repository root.

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

`APP_MODE` in `.env` (`clearnet` or `tor`, set by the installer) is only checked at startup: any other value stops the server, but it changes no behaviour. The Compose overlay in `COMPOSE_FILE` decides exposure, and `COOKIE_SECURE` decides whether cookies are marked Secure.

Configure DNS, firewall and the certificate issuer for your domain. A proxy in another container needs a private shared network instead of this host-loopback configuration. Never expose the database or node RPC ports.

**External PostgreSQL and time zones.** Every database connection the app opens (the web app, its migrations and `-reset-admin-password`) sets `TimeZone=UTC` itself, overriding the server, database and role defaults and any `TimeZone` in `DATABASE_URL`. An external or host PostgreSQL left on local time therefore needs no change: pages show every time in UTC, labelled " UTC", and do not reveal the server's UTC offset. Other clients, such as `psql` or the backup scripts, still use the server's own zone.

For onion service deployment choose `tor` in the installer. The base and Tor Compose files publish **no host ports**; Tor connects to `app:8080` on the private Docker network. After initialization, obtain the address with:

```sh
docker compose exec tor cat /var/lib/tor/marketplace/hostname
```

The persistent `tor_identity` volume contains the onion private keys. Preserve it to retain the address; encrypt any offline copy. The setup uses HTTP inside the authenticated onion circuit and sets `COOKIE_SECURE=false` for that endpoint. The application and database share an internal Docker network with no direct Internet egress. Only the Tor gateway has a separate egress network. Selecting external database or RPC endpoints adds `compose.external-egress.yaml`, which explicitly permits direct application egress; that configuration provides onion-only inbound exposure, not Tor-routed outbound traffic. Local nodes have their own direct egress network to synchronize with peers. Docker network isolation does not protect against host administrators or a compromised container runtime. Use the dedicated Tor Compose file without the clearnet overlay. If changing an existing clearnet deployment, stop it before changing `COMPOSE_FILE` and recreate containers and the backend network so published ports are removed and the network becomes internal. Remove only the unused network, not persistent volumes. Never run both overlays together.

**Tor restarts with the app; this needs Docker Compose 2.17 or later** (check with `docker compose version`). Tor looks up the app's container address once, when it starts ([`deploy/torrc`](../deploy/torrc): `HiddenServicePort 80 app:8080`), and a recreated app container can get a different address, for example when an upgrade adds node services. Tor then keeps forwarding to the old address and the onion stops answering. `compose.tor.yaml` therefore gives `tor` and `tor-mirror` a `restart: true` dependency on the app: whenever Compose recreates the app (`docker compose up -d` after a rebuild or a `.env` change) or restarts it (`docker compose restart app`), it restarts Tor too. The mirror is included only when the command sees the `mirror` profile, so add `mirror` to `COMPOSE_PROFILES` in `.env` if you run it (see [Optional onion mirror](#optional-onion-mirror)). Compose does not restart Tor for `docker compose stop app` followed by `up -d`, and the setting does not repair a Tor that already forwards to an old address. On a Tor install deployed before this change, recreate Tor once:

```sh
docker compose up -d --force-recreate tor              # without the onion mirror
docker compose up -d --force-recreate tor tor-mirror   # with the onion mirror
```

Name `tor-mirror` only if you run the mirror: naming it starts it, which creates a second onion address. **If the onion does not answer after any change, recreate Tor** with the same command. Recreating keeps the `tor_identity` and `tor_mirror_identity` volumes, so the addresses stay the same.

## Optional onion mirror

In a Tor deployment, create a second onion address with its own persistent identity. First add `mirror` to `COMPOSE_PROFILES` in `.env`, keeping the profiles already there: the installer's default Tor install has `COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet'`, which becomes `COMPOSE_PROFILES='internal-db,bitcoin,monero,monero-wallet,mirror'`; `COMPOSE_PROFILES='internal-db'` becomes `COMPOSE_PROFILES='internal-db,mirror'`, and an empty value becomes `COMPOSE_PROFILES='mirror'`. Then:

<!-- ops-cmd: onion-mirror -->
```sh
docker compose config --services | grep -x tor-mirror   # prints tor-mirror; nothing means .env lacks the profile
docker compose up -d
docker compose exec tor-mirror cat /var/lib/tor/marketplace/hostname
```

Wait for the mirror's Tor process to initialize before reading its hostname. Do not start the mirror with a `--profile mirror` option instead: an option replaces `COMPOSE_PROFILES` for that command rather than adding to it, so with the internal database the command fails (`service "app" depends on undefined service "db"`), and a mirror started that way is left out of later `docker compose up -d` runs. With the profile in `.env`, every `docker compose` command includes the mirror and Compose restarts its Tor whenever it recreates the app (see [Clearnet and onion deployment](#clearnet-and-onion-deployment)). If you already run a mirror that is not in `COMPOSE_PROFILES`, add it now. This optional profile publishes no host ports and reaches the same application/database over the private backend network. The second address is a same-host alternate URL; it does not provide independence from host, application or database failures. Preserve and encrypt the separate `tor_mirror_identity` volume to retain its address.

Publish both addresses in an operator-signed verification document distributed through a trusted channel. Users should verify that signature with a previously trusted operator key; merely displaying an address or a “canonical” label on a page does not authenticate a mirror. Signing and distribution remain operator tasks.

## Optional cryptocurrency nodes

The interactive installer asks for each coin whether to run a `local` node in Docker (the default; pruned unless you answer `no`, see [Local node pruning](#local-node-pruning)), use an `external` node you run elsewhere, or leave that coin `disabled`. `scripts/install.sh --local` (developer mode) never starts nodes. For a local node, the installer defaults to [`bitcoin/bitcoin:latest`](https://hub.docker.com/r/bitcoin/bitcoin) for Bitcoin Core and Seth for Privacy's [`simple-monerod`](https://github.com/sethforprivacy/simple-monerod-docker) plus [`simple-monero-wallet-rpc`](https://github.com/sethforprivacy/simple-monero-wallet-rpc-docker) images for Monero. Press Enter to use these defaults or supply an image override. The Bitcoin image is community-published and explicitly unofficial; it verifies upstream Bitcoin Core binaries. The Monero images are community-maintained, not published by the Monero project. Review publisher and image provenance before use. `latest` follows new releases; set an immutable `@sha256:<64 lowercase hex characters>` digest in `.env` for a reproducible deployment. Bitcoin and Monero use the same daemon image on test and live networks; the network is selected with daemon flags, not by changing the image. The installer offers Bitcoin `testnet4` or `signet`, and Monero `stagenet` or `testnet`. **Live is not supported by the payment adapters:** selecting it exits before writing `.env`, because the application refuses mainnet wallets and addresses. Full live-payment support needs a separate adapter and payout-safety change.

Both nodes store their blockchain in dedicated volumes and publish no ports. Even pruned, they need several GB of disk and hours for the first synchronization (sizes below). Bitcoin RPC is password protected; its allowlist covers common private Docker subnets and must be adjusted for custom networking. Monero exposes restricted daemon RPC only inside the deployment network. A Monero daemon is **not** a wallet RPC service. Full-node availability alone does not provide payment processing.

**Payments are implemented for test networks only; mainnet is refused at startup.** With a wallet RPC configured (see [Payments](#payments)), the app issues deposit addresses, watches confirmations and sends single-attempt releases and refunds from the operator's pooled test-network wallet. That is custodial holding, not multisig escrow, and there is no fee accounting. Do not send mainnet coins. Providers are enabled by environment variables and a restart. Saving settings in the web application never installs software, launches containers or executes Docker. Operators apply deployment changes in `.env` and Compose themselves; the web container has no Docker socket.

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

Migration 001 replaces the free-text order status with a checked `state` column (existing unfunded drafts become `draft`) and limits each buyer to one open draft per listing and currency.

## Feature configuration

All keys below are listed in `.env.example` and passed by `compose.yaml`. Blank or absent values keep a feature disabled.

### Authentication

No environment keys. Second-factor secrets are encrypted with a key derived from `SETUP_TOKEN`; rotating `SETUP_TOKEN` makes existing TOTP enrollments unreadable, so affected users must sign in with a recovery code, turn TOTP off and enroll again (an administrator resets a non-administrator with no recovery code left under *Admin → Reset a user's second factors*; the administrator's own account needs the operator reset in [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). Startup refuses a placeholder or obviously non-random `SETUP_TOKEN`; use `openssl rand -hex 32`.

- **TOTP** is optional per account: *Account → Manage TOTP* (`/totp`). Users type the shown secret (or paste the `otpauth://` URI) into an authenticator app; no QR code is generated. TOTP turns on only after a correct code, and 10 one-time recovery codes are shown once. Server clocks must be accurate (NTP); codes are accepted within ±30 seconds.
- **CAPTCHA** on sign-in and registration is on by default for new and upgraded installations. Administrators turn it off or on under *Admin → Sign-in protection*; the change is audited. It is an image only, with no audio alternative, so turning it off may be needed for users who cannot read it. First-run setup never shows it.
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

### Inventory

No configuration. Vendors manage listings from the vendor desk (Edit / Archive / Restore). Automatic delivery content for digital listings is stored unencrypted in the database — include it in your threat model and backups — and is only released after a payment provider confirms payment; with payments disabled it is never released automatically. Administrators can edit other vendors' listings but cannot view or change their automatic delivery content: the editor shows only whether it is set, and only the vendor can change or remove it.

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

For external databases also set `BACKUP_DATABASE_URL`, install Python 3 and PostgreSQL client tools matching or newer than the server. The script streams a custom-format dump directly into encryption, uses restrictive file permissions, refuses overwrite and fails if either command fails. Back up `.env`, deployment settings and the Tor identity separately in encrypted storage. Database dumps do not include onion keys or blockchain volumes, and in particular **not the custodial payment wallets** (the `bitcoin_data` and `monero_wallet` volumes, or your external wallets): back those up on their own (see the [runbook](testnet-runbook.md#6-back-up-the-wallets)). Maintain offline copies and rehearse recovery.

Rehearse recovery on another host, or in a second checkout with its **own Compose project name**. Compose names containers, volumes and networks after the project (`opsecmkt` unless `.env` sets `COMPOSE_PROJECT_NAME`), not the directory: a second checkout under the same name recreates the live containers with its own secrets, and its `docker compose down --volumes` deletes the live volumes. Install the rehearsal copy with, for example, `COMPOSE_PROJECT_NAME=opsecmkt-rehearsal APP_PORT=8081 ./scripts/install.sh` (add `--local` for a quick local copy); the installer saves the name in that checkout's `.env`, so its later `docker compose`, `backup.sh` and `restore.sh` commands stay on the rehearsal project, and it refuses to install when containers of the chosen project were created from another directory, or when that project's volumes exist without containers. If you copy a `.env` into the rehearsal checkout instead, change `COMPOSE_PROJECT_NAME` (add the line if it is missing) and `APP_PORT` in the copy before running any `docker compose` command there. Never change `COMPOSE_PROJECT_NAME` on an existing install: it would start on new, empty volumes (`<project>_postgres_data` and the others).

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

Set exactly one of the two destinations. Either way the restore requires typing `RESTORE`, runs in one transaction, and does not drop existing tables: restoring into a database that already has the application's tables fails and changes nothing. The payout protection below is applied by the same SQL in both modes. Restore with the `SETUP_TOKEN` that was in use when the backup was taken, or TOTP secrets in it cannot be read (see [UPGRADING.md](../UPGRADING.md#replace-a-placeholder-setup_token)). To roll back an upgrade, follow [UPGRADING.md](../UPGRADING.md#5-rolling-back).

The script sets a persistent recovery gate that pauses all outbound payouts,
including payouts created after restoration, and converts pending, sending, blocked and held payouts into
manual recovery holds (error text starting `Restored from backup:`); releasing one on the admin page requires confirming that the wallet shows no broadcast transaction for it. Failed payouts are marked as possibly sent (same error prefix): one the wallet rejected before the backup may have been requeued and sent after it, so requeueing it needs the same confirmation. A payout held for an account suspension keeps that hold behind the restore marker, so releasing it also needs the payout address check. The script prints how many payouts it held and how many failed payouts it marked, and adds one audit row recording the gate and those counts. A database backup may predate an already-sent payout's creation, so reviewing only
existing payout rows is insufficient. Follow the [complete reconciliation and explicit unlock procedure](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)
before allowing user writes or restarting payment sends. Validate recovered accounts, listings, orders and
settings before switching traffic. Never test a restore against your live database; a test restore in a
second checkout on the same host needs its own `COMPOSE_PROJECT_NAME` (for example `opsecmkt-rehearsal`, see
[above](#backups-and-recovery)), or its `docker compose` commands act on the live containers and volumes.

**Any restore not done by `scripts/restore.sh` skips this payout protection.** A host or volume snapshot (including a copy of the `postgres_data` volume), a managed-database point-in-time recovery or a manual `pg_restore` brings payouts back as `pending` with no recovery gate, and the application sends them as soon as it starts, even those already paid after that point in time. After such a restore keep the application stopped and apply the protection SQL in [UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database before starting it, then follow the same reconciliation procedure.

## Verification and maintenance

```sh
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
for script in scripts/install.sh scripts/backup.sh scripts/restore.sh; do bash -n "$script"; done
docker compose config --quiet
```

`docker compose config` requires `.env` or equivalent environment variables. Startup logs show database connection/migration errors (see [Troubleshooting](#troubleshooting)). Review dependencies and rebuild periodically. The Compose examples use official [Go](https://hub.docker.com/_/golang) and [PostgreSQL](https://hub.docker.com/_/postgres) image families; production operators should pin reviewed image digests and plan PostgreSQL major-version upgrades explicitly. Tor is installed from Debian's package repositories; the onion-service configuration follows the [Tor Project setup guide](https://community.torproject.org/onion-services/setup/).

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

`pass skipped` means that currency was not polled (no deposits read, no expiry, no payouts): `wallet check failed: <class>`, `refused by the test-network guard` (the provider stays disabled until the node is fixed and the application restarted), or `tip comparison failed`. A wallet class is `rpc:<code>` for a JSON-RPC error or one of the classes above; a wallet that cannot be reached shows as the Go type of its error. The wallet's text (which can quote an address) is the provider's last error on the admin page; neither this line nor the payout line prints it. `db-connection` (or `timeout`) alone means the watcher could not reach the database; the line never holds `DATABASE_URL`, its user, database name, host or port. The recovery code reveal sweep logs `recovery code reveal sweep: <class>` the same way.

When the database connection fails at startup the server exits with its class and no part of `DATABASE_URL`: `authentication failed (SQLSTATE 28P01)` or `(SQLSTATE 28000)` for a wrong user or password or a `pg_hba.conf` rule, `database missing (SQLSTATE 3D000)`, `host not found`, `connection refused`, `timed out`, or `TLS required` (the server accepts only encrypted connections; set `sslmode=require` or `verify-full`).
