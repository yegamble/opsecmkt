# Upgrading from v0.1.0-alpha.1

This release adds test-network payments, signed audit exports and several database migrations. Read this
whole page before pulling the new code onto a running v0.1.0-alpha.1 deployment.

## Upgrade with the installer

From the directory you deploy from, with your age public key:

```sh
git describe --tags --always   # note the revision you run today, e.g. v0.1.0-alpha.1
git pull                       # or check out the release tag
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade
```

With an **external database** (`DATABASE_URL` names another host than `db`), the backup reads the database
only from `BACKUP_DATABASE_URL`, and needs Python 3 and PostgreSQL client tools matching or newer than the
server ([Backups and recovery](docs/operator-guide.md#backups-and-recovery)). Give both:

```sh
BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade
```

The value of `DATABASE_URL` in `.env` serves when this host can reach that server (quote it in single quotes). Without `BACKUP_DATABASE_URL`,
`upgrade` refuses before changing anything. Like any command-line value, it is visible to other local users
while the command runs and may be kept in your shell history.

Read [section 3](#3-bitcoin_rpc_url-now-turns-payments-on) first if `.env` sets a node URL, and
[section 7](#7-local-nodes-are-now-pruned) and [section 8](#8-node-images-are-pinned-to-release-tags) if you
run local nodes. `upgrade` then does, in order:

1. **Backup.** It runs `scripts/backup.sh` into `backups/pre-upgrade-<time>.dump.age` and stops, changing
   nothing, if that fails. Without `AGE_RECIPIENT` (and, for an external database, `BACKUP_DATABASE_URL`) it
   refuses to run unless you pass `--no-backup` (for example because you have just taken a backup; migrations
   are one-way).
2. **A copy of `.env`** as `.env.pre-upgrade-<time>` (mode `0600`): the configuration to roll back with.
3. **Missing keys.** It adds every non-secret key `.env.example` documents that `.env` lacks, with the value
   Compose already used while the key was absent, so nothing changes but what `.env` shows
   ([the appendix](#appendix-new-keys-by-hand) explains them). It records the
   `COMPOSE_PROJECT_NAME` your containers use and `APP_PORT`: the value in your shell when you set one, else
   the host port the running app container publishes (`docker compose port app 8080`), else `8080`. It never invents
   a secret: `AUDIT_SIGNING_KEY` and a local Monero wallet stay off until you add them.
4. **`APP_SECRET_KEY`** set to the current `SETUP_TOKEN` value when it is missing, so every key, session and
   TOTP secret stays the same ([details](#app_secret_key-and-stricter-startup-checks)).
5. **Pinned images.** `bitcoin/bitcoin`, `simple-monerod` and `simple-monero-wallet-rpc` on `latest` (or on no
   tag) become the verified releases ([section 8](#8-node-images-are-pinned-to-release-tags)). Another
   publisher's image on `latest` is kept, and the check below refuses it until you pin it.
6. **The onion mirror.** On Tor, when a `tor-mirror` container exists, `mirror` joins `COMPOSE_PROFILES`.
7. It prints what changed (key names, never secret values), runs `./scripts/install.sh check`, then
   `docker compose up -d --build --wait`; migrations run at startup.
8. Once setup is complete and the restarted app reports that its keys derive from `APP_SECRET_KEY`
   (`-status` prints `keyroot=app_secret_key`), it removes `SETUP_TOKEN`, which grants nothing any more, and
   recreates the app.
   On Tor, when `.env` changed, it recreates `tor` (and `tor-mirror`) once ([section 4](#4-deploy)).
9. **The malware scanner**, only when it is on (`scanner` in `COMPOSE_PROFILES`): it refreshes the scanner's
   signatures as `./scripts/install.sh refresh-scanner` does. A failed pull is only a warning. `upgrade` never
   turns the scanner on; it adds `CLAMD_ADDRESS` blank, which keeps photo uploads off
   ([Photo uploads and the malware scanner](docs/operator-guide.md#photo-uploads-and-the-malware-scanner)).
   ClamAV checks uploads for known malware; it does not review picture content.

Running it again changes nothing. Installer commands that change `.env` hold a lock, the directory
`.env.lock`, while they run; a second one refuses to start until the first ends. If a killed run left it
behind, remove it with `rmdir .env.lock`. If the check or a service fails, it says so and changes nothing more: fix
`.env`, or put the copy back with `mv .env.pre-upgrade-<time> .env`, and see
[Rolling back](#5-rolling-back). Then open the admin page as [section 4](#4-deploy) describes. The sections
below explain each change and how to make it by hand.

## 1. Take a backup and record what you run

Migrations are **one-way**. There are no down-migrations, and the alpha.1 code must not run against a
migrated database (for example, migration 001 replaces `orders.status` with `orders.state` and drops the old
unique constraint). `compose.yaml` builds the app from the checkout (`build: .`), so there is no saved image
to switch back to: rolling back means restoring this backup and rebuilding the old revision.
`./scripts/install.sh upgrade` takes the backup and the copy of `.env` for you. By hand, before upgrading, from
the directory you deploy from:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/pre-upgrade.dump.age
cp -p .env .env.pre-upgrade   # the configuration the old revision runs with (keep it private)
git describe --tags --always  # note the revision you run today, e.g. v0.1.0-alpha.1
```

Keep the backup, `.env.pre-upgrade` and the revision until you have run the new version for a while. See
[Rolling back](#5-rolling-back).

## 2. New keys in `.env`

`./scripts/install.sh upgrade` adds every missing key with the value Compose already used while it was absent,
so adding them changes nothing by itself. [The appendix](#appendix-new-keys-by-hand) lists them for an upgrade
by hand. Two changes need your attention either way: the checks on `SETUP_TOKEN` and the new `APP_SECRET_KEY`.

### Replace a placeholder `SETUP_TOKEN`

This release refuses to start when `SETUP_TOKEN` is not a random value: the old `.env.example` placeholder
(`REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS`), anything containing `REPLACE`, `CHANGEME`, `CHANGE_ME` or
`PLACEHOLDER`, fewer than 32 characters, fewer than 8 distinct characters, or a short pattern repeated. The
log then says `SETUP_TOKEN is still a placeholder value` or `SETUP_TOKEN is not random`. `scripts/install.sh`
always generated a random 64-character hex token, so only hand-written `.env` files are affected.

The placeholder is public, and `SETUP_TOKEN` authorizes `/setup` and, without `APP_SECRET_KEY` (see
[the next section](#app_secret_key-and-stricter-startup-checks)), derives the CSRF/CAPTCHA key and the key
that encrypts TOTP secrets. The same checks apply to `APP_SECRET_KEY`; when it is set, keys derive from it
alone, so rotating a placeholder `SETUP_TOKEN` changes only `/setup`, and a placeholder `APP_SECRET_KEY` is
the value to rotate, with the consequences below. An instance that ran with the placeholder must rotate it: put the output of
`openssl rand -hex 32` in `.env` as `SETUP_TOKEN='...'` and start the app again. What rotation does:

- Sessions stay signed in (they are stored as hashes in the database). A form or CAPTCHA that was open during
  the restart is refused once; reload it.
- **alpha.1 has no TOTP**, so an alpha.1 instance that rotates at this upgrade loses nothing else.
- On an instance that already ran this release with the placeholder, every stored TOTP secret, pending
  enrolment and not-yet-shown recovery-code list was encrypted under the old token and can no longer be read.
  There is no re-encryption tool, and those secrets must be treated as exposed anyway (the key was public,
  including in backups taken meanwhile). Affected users sign in with a recovery code (recovery codes are
  stored as plain hashes and keep working), turn TOTP off on `/totp` with their password and a recovery
  code, and enrol again. For a non-administrator with no recovery code left, confirm who they are out of
  band, then use *Admin → Reset a user's second factors*, which also ends their sessions and is audited.
  An administrator with no recovery code left needs the host command, because the web reset refuses
  administrators (and perhaps no administrator can sign in at all). Run it in the deployment directory; it
  works with the internal or an external database (replace `THE_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-factors THE_HANDLE
  ```

  In one audited transaction it turns off that administrator's TOTP, recovery codes and PGP sign-in and ends
  its sessions and pending sign-ins; the administrator then signs in with the password alone and enrols again.
  It refuses an account that is not an administrator (use the web reset above) and an unknown handle,
  changing nothing ([details](docs/operator-guide.md#authentication)).

A restored database needs the `SETUP_TOKEN` (or `APP_SECRET_KEY`, when set) that was in use when its backup
was taken for its TOTP secrets to be readable. Keep the rotated value, not the placeholder, in any `.env` you
restore later.

### `APP_SECRET_KEY` and stricter startup checks

**Your existing `.env` keeps working unchanged.** This release adds `APP_SECRET_KEY`, which becomes the only
root of the CSRF/CAPTCHA key and the key that encrypts TOTP secrets (the derivation is the same as before, only
the input changes). Without it the app derives them from `SETUP_TOKEN` exactly as before, byte for byte, and
logs `keys: derived from SETUP_TOKEN (legacy)` at startup. `./scripts/install.sh upgrade` moves to it without
changing any key, and removes `SETUP_TOKEN` once setup is complete. By hand, copy the current value (if `.env`
already has an `APP_SECRET_KEY` line, edit that line instead):

```sh
grep '^SETUP_TOKEN=' .env | sed 's/^SETUP_TOKEN=/APP_SECRET_KEY=/' >> .env   # same value, new name
docker compose up -d
```

Never put a new random value in `APP_SECRET_KEY` on an existing install: that is a key rotation, with the
consequences described in the previous section. Once `APP_SECRET_KEY` is set and setup is complete,
`SETUP_TOKEN` grants nothing (`/setup` answers 403 whatever the token) and can be removed from `.env`; while it
is set, startup logs `hint: SETUP_TOKEN is set but installation is complete`. Before setup is complete the
app refuses to start without `SETUP_TOKEN`, and names `./scripts/install.sh setup-token --new`, which issues
one. Compose itself no longer refuses a blank `SETUP_TOKEN`: the app does, when neither `APP_SECRET_KEY` nor
`SETUP_TOKEN` is set.

Startup now also stops, with a message naming the key and never its value, when:

- `COOKIE_SECURE` is anything but exactly `true` or `false` (for example `TRUE`, `1` or `yes`); unset still
  means `true`. `scripts/install.sh` only ever wrote `true` or `false`, so only hand-edited values are affected.
- `DATABASE_URL` still holds a placeholder such as `.env.example`'s `REPLACE_ME` or a `CHANGEME`.
- `APP_MODE` disagrees with the exposure overlay in `COMPOSE_FILE`, which Compose now passes to the app:
  `tor` needs `compose.tor.yaml`, `clearnet` needs `compose.clearnet.yaml`, and never both. A blank
  `COMPOSE_FILE` (plain `-f` options) is not checked.

`APP_MODE=tor` with `COOKIE_SECURE=true` only logs a warning. The app, `tor` and `tor-mirror` services now have
Compose healthchecks: `docker compose ps` shows their health and `docker compose up -d --wait` waits for it.

## 3. `BITCOIN_RPC_URL` now turns payments on

In alpha.1 the installer wrote `BITCOIN_RPC_URL` but nothing read it. Now a non-empty value enables Bitcoin
test-network payments. The app never starts against a mainnet node unless `BITCOIN_CHAIN=main` is set: startup
exits with `refusing to start: bitcoin chain "main" ...`. A node that is unreachable, still syncing or has no wallet
loaded no longer stops the site; Bitcoin simply shows as unavailable on the admin page until it works.

- **External mainnet node**: blank `BITCOIN_RPC_URL`, or point it at a testnet4/signet/regtest node.
- **Local node from alpha.1**: the old `bitcoin_data` volume holds **mainnet** data (alpha.1 ran `bitcoind`
  without `-chain`). The node service now runs `-chain=${BITCOIN_CHAIN:-testnet4}`. Either:
  - blank `BITCOIN_RPC_URL` and remove `bitcoin` from `COMPOSE_PROFILES` to keep Bitcoin off; or
  - set `BITCOIN_CHAIN='testnet4'` and start from a fresh volume. The mainnet block data is of no use to
    the marketplace and only wastes disk. If you ever kept a wallet on that volume, copy `/data/wallets`
    out first. Then:

    ```sh
    docker compose stop bitcoin && docker compose rm -f bitcoin
    docker volume rm opsecmkt_bitcoin_data
    ```

  Then create the test wallet as described in [docs/testnet-runbook.md](docs/testnet-runbook.md).
- **Local Monero node from alpha.1**: its `monero_data` volume is mainnet data too; the service now runs
  `--${MONERO_NETWORK:-stagenet}`. Remove the old volume the same way (`opsecmkt_monero_data`), add the
  `monero-wallet` profile and the keys above, then follow the runbook.

## 4. Deploy

**Local nodes: decide on pruning first** ([section 7](#7-local-nodes-are-now-pruned)). The first start
after this upgrade prunes an existing full Bitcoin or Monero chain unless `.env` says otherwise.

`./scripts/install.sh upgrade` runs this section for you, Tor recreate included. By hand:

```sh
git pull   # or check out the release tag
docker compose config --quiet   # fails loudly on a malformed .env
docker compose up -d --build
docker compose logs -f app      # migrations run at startup under an advisory lock
```

**Tor installs: recreate Tor once.** Tor looks up the app's address only when it starts, and the node
services from steps 2 and 3 can move the recreated app to a different address; Tor then forwards to the old
one and the onion stops answering. This release restarts Tor whenever Compose recreates or restarts the app,
which needs **Docker Compose 2.17 or later** (`docker compose version`); the mirror is included only when
`mirror` is in `COMPOSE_PROFILES` (step 2). That does not repair a Tor that is already forwarding to an old
address.
After `up -d`, run once:

```sh
docker compose up -d --force-recreate tor              # without the onion mirror
docker compose up -d --force-recreate tor tor-mirror   # with the onion mirror (naming it starts it)
```

If the onion does not answer after any later change, recreate Tor the same way. The onion address is kept in
the `tor_identity` volume (`tor_mirror_identity` for the mirror) and does not change. See
[Clearnet and onion deployment](docs/operator-guide.md#clearnet-and-onion-deployment).

Then open the admin page. **Payment providers** shows each currency as Enabled, Unavailable (with the
error; retried every poll), Refused (not a test network, or not mainnet on a mainnet market) or Disabled. The container stop grace period is now
60 seconds so a redeploy does not interrupt a payout that is being broadcast.

**Container logs now rotate, with Docker's `json-file` driver.** Every container keeps at most three 10 MB
log files, and for these containers this replaces a default logging driver you set in Docker's `daemon.json`
(for example `journald`). To keep another driver, set `LOG_DRIVER` in `.env` before `up -d`: `local` works as
is; a driver that does not take `max-size` and `max-file` (such as `journald` or `syslog`) needs the override
file described in [Resource limits and log rotation](docs/operator-guide.md#resource-limits-and-log-rotation).
Log lines written before the upgrade stay with the old containers' driver.

**External database behind a connection pooler.** The app now sends `statement_timeout`, `lock_timeout` and
`idle_in_transaction_session_timeout` when each database connection starts, and checks before migrating that
the database accepts them. PgBouncer and some hosted poolers refuse such parameters: startup then stops with
"database refused the application's session settings" and changes nothing. See
[Connection poolers](docs/operator-guide.md#connection-poolers).

## 5. Rolling back

Rolling back is **not** an image swap. Migrations are one-way, so older code must not run against the
upgraded database. From this release on, the server checks at startup: when `schema_migrations` records a
migration it does not include, it exits with an error that names that migration and points to this section,
before it changes or serves anything (`the database was upgraded by a newer release: it records migration(s)
NNN_name, which this server does not include. Nothing was changed. ...`). alpha.1 predates that check and
does not refuse, so never point it at the upgraded database: it expects the pre-upgrade tables (for example
`orders.status`, which migration 001 removes). Compose rebuilds the app from whatever revision is checked
out. Everything written after the upgrade is lost from the rolled-back site. Run the first two steps from
the **upgraded** checkout: its `scripts/restore.sh` can restore into the internal database; the older one cannot.

1. Stop the app and keep the database running:

   ```sh
   docker compose stop app
   ```

2. Restore the pre-upgrade backup into a new database next to the upgraded one (type `RESTORE` when asked).
   The upgraded `opsecmkt` database is left untouched in case you want to roll forward again.

   ```sh
   AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE=opsecmkt_rollback \
     ./scripts/restore.sh backups/pre-upgrade.dump.age
   ```

   After `./scripts/install.sh upgrade` the backup is `backups/pre-upgrade-<time>.dump.age` (it printed the
   name).

   With an external database, create an empty database on that server and restore with
   `RESTORE_DATABASE_URL` instead (see [backups and recovery](docs/operator-guide.md#backups-and-recovery)).
   The restore pauses payouts and holds queued ones (see step 6); alpha.1 has no payment processing.

3. Put back the old configuration and point it at the restored database:

   ```sh
   cp -p .env.pre-upgrade .env   # after install.sh upgrade: .env.pre-upgrade-<time>
   ```

   Edit `DATABASE_URL` in `.env` so the database name after the port is the restored one, for example
   `postgres://opsecmkt:PASSWORD@db:5432/opsecmkt_rollback?sslmode=disable` (keep the password as it is);
   `scripts/restore.sh` prints this line for the database it restored into.
   If you rotated `SETUP_TOKEN` during the upgrade, keep the new value rather than the placeholder.
   With an external database, point `BACKUP_DATABASE_URL` in your backup job at the restored database too:
   external backups follow `BACKUP_DATABASE_URL`, not `DATABASE_URL`.

4. Check out the revision you noted in step 1 and rebuild it:

   ```sh
   git checkout v0.1.0-alpha.1
   docker compose config --quiet
   docker compose up -d --build --remove-orphans
   docker compose logs -f app
   ```

   `--remove-orphans` removes the containers of services the older revision does not define, such as the
   malware scanner's `clamd` if you turned it on after the upgrade.

5. Keep backing up the database you now run. This release's `scripts/backup.sh` dumps the internal
   database named in `DATABASE_URL`, so it follows the switch and its success line names `opsecmkt_rollback`.
   The alpha.1 `scripts/backup.sh` you just checked out always dumps `opsecmkt`, which is now the abandoned
   upgraded database. While you run alpha.1 on the internal database, back up the rolled-back one directly:

   ```sh
   (set -o pipefail; umask 077
    docker compose exec -T db pg_dump -U opsecmkt -d opsecmkt_rollback --format=custom --no-owner --no-acl \
      | age -r age1YOUR_PUBLIC_RECIPIENT > backups/rollback.dump.age) || echo 'Backup FAILED'
   ```

   Use a new file name each time, as `scripts/backup.sh` does, and delete the file if the command failed.

## 6. Backups now need the wallets too

Once payments are on, the marketplace is custodial for test coins: the Bitcoin wallet lives in the
`bitcoin_data` volume and the Monero wallet in `monero_wallet`. **Database dumps do not include them.** Back
them up separately (see the runbook). `scripts/restore.sh` now pauses all outbound payouts with a persistent recovery gate, holds every
pending, sending, blocked or held payout (one held for an account suspension keeps its "Suspended account:"
text, so its payout address check stays), marks every failed payout as possibly sent after the backup (it
may have been requeued and sent since) and adds an audit row recording the gate and those counts. Follow the [reconciliation procedure](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts),
including orders paid out after the backup that have no payout row in it, before explicitly clearing the
gate. Restore with the application stopped and keep the site cut off from users until reconciliation is
complete; the procedure says when the app runs for you alone and when it must be stopped.

The script also prints `<N> images restored from a backup taken <time>; photo and avatar removals made after
that time are undone — check /moderator Recent photos and remove them again`. Removing a listing photo or an
avatar, by its owner or as a staff takedown, deletes it from the database, so a restore brings back every image
removed after the backup was taken. Before users return, have a moderator check the moderation desk's Recent
photos and take down again any image that had been removed. A restore not done by `scripts/restore.sh` prints
nothing, but undoes those removals all the same. The time is in UTC for backups made by this release's
`scripts/backup.sh`; for a backup made by an earlier one on a host (or, for the internal database, a container)
not set to UTC, it is off by that zone's offset from UTC.

**Any restore not done by `scripts/restore.sh` skips this payout protection.** A host or volume snapshot, a
managed-database point-in-time recovery or a manual `pg_restore` brings payouts back as `pending` with no
gate, and the application sends them again as soon as it starts, even if they were paid after that point in
time. After such a restore, and whenever the script reports that recovery protection failed, keep the
application stopped (`docker compose stop app`; for a volume snapshot start only the database with
`docker compose up -d --wait db`) and apply the protection below to the restored application database
**before starting the application**, then follow the same reconciliation procedure. It runs in one
transaction and adds one audit row ("set manually"); existing audit rows are not changed. Older databases
without a `payouts` table need only the `settings` and `audit_events` statements; omit the `send_ambiguous`
line if their `payouts` table has no such column, as migration 053 then marks their failed payouts ambiguous
itself, and omit the `settlements` line if the database has no `settlements.send_error` column (before migration
076, when no escrow settlement could have a stored transaction). That line holds every restored Bitcoin escrow
settlement with a stored transaction until an administrator confirms its rebroadcast on `/admin`.

<!-- runbook-sql: restore-protection -->
```sql
BEGIN;
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
UPDATE payouts SET state='held', updated=now(),
  error='Restored from backup: verify in the wallet before releasing; this payout may already have been sent.'
    || coalesce(' ' || substring(error from '(?:Suspended account:|Larger than the order price:).*'), '')
WHERE state IN ('pending','sending','blocked','held');
UPDATE payouts SET send_ambiguous=true WHERE state='failed';
UPDATE payouts SET updated=now(),
  error='Restored from backup: verify in the wallet before requeueing; this payout may have been requeued and sent after the backup. Last error: ' || error
WHERE state='failed';
UPDATE settlements SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast');
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Payout recovery gate set manually (UPGRADING.md section 6); all outbound payouts paused until an administrator clears the gate');
COMMIT;
```

Save it as `restore-protection.sql` and run it with `psql` against the restored database, for the internal
database `docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt_restored < restore-protection.sql`
(the database name in `DATABASE_URL`). It must end with `INSERT 0 1` and `COMMIT`; on an error nothing is
changed.

The payout gate is enforced by this release. An older application image may not understand it; keep wallet
RPC configuration disabled when inspecting a restored database with older code. Alpha.1 has no payment
processing, but any other older release requires its own recovery review.

## 7. Local nodes are now pruned

The `bitcoin` and `monero` services now run pruned unless `.env` opts out: `bitcoind -prune=${BITCOIN_PRUNE_MB:-2000}`
(and `-datadir=/data`, the directory the image already used) and `monerod --prune-blockchain --sync-pruned-blocks`
unless `MONERO_PRUNE_FLAGS` is set. The application does not need old blocks (it uses the nodes' current state and
wallet RPCs), so payments work the same either way. What happens to a volume you already have:

- **Bitcoin volume with a full chain:** on its first start bitcoind deletes old block files down to about
  2000 MiB. This cannot be undone without downloading the whole chain again, and afterwards a wallet backup
  older than the node's `pruneheight` can no longer be restored on this node without doing so. To keep the full
  chain, add `BITCOIN_PRUNE_MB='0'` to `.env` **before** `docker compose up -d`.
- **Monero volume with a full chain:** monerod prunes the existing database in place on its first start
  ("Pruning blockchain..." in its log). The database file does not shrink and pruning temporarily needs extra
  space; to reclaim disk, remove the `monero_data` volume afterwards and let it sync again pruned. Monero
  wallets (in the separate `monero_wallet` volume) are unaffected. To keep the full chain, add
  `MONERO_PRUNE_FLAGS=''` to `.env` **before** `docker compose up -d`; once pruned, the database stays pruned
  even if you blank the flags later.
- **Volumes from v0.1.0-alpha.1** hold mainnet data and are removed in [section 3](#3-bitcoin_rpc_url-now-turns-payments-on);
  the new test-network volumes start pruned.

Behaviour on an already-synced chain is taken from the upstream sources and has not been rehearsed here
(UNVERIFIED). Sources, disk sizes, switching between pruned and full later, and the wallet-restore trade-off:
[Local node pruning](docs/operator-guide.md#local-node-pruning).

## 8. Node images are pinned to release tags

`compose.nodes.yaml` now defaults to `bitcoin/bitcoin:31.1`, `ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1` and
`ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1` instead of `latest`: the payout code decides whether
a failed send can have been broadcast from the wallet's error, and that table is verified for monero-wallet-rpc
v0.18.5.1 only. A `BITCOIN_IMAGE`, `MONERO_IMAGE` or `MONERO_WALLET_IMAGE` in your `.env` still wins. If one of
them ends in `:latest` (an installer run from a pre-release checkout wrote that) or is an image you chose, change
it to the tag above, or follow the bump checklist in
[Node image versions](docs/operator-guide.md#node-image-versions) before running another version. Check with:

```sh
docker compose config --images
```

The application also checks the running wallet (A-174): at startup and on every watcher pass it reads
monero-wallet-rpc's `get_version`. While the wallet does not report RPC version 1.31 from a release build (what
v0.18.5.1 reports), or the call fails, every failed Monero payout is recorded as possibly broadcast, so a requeue
needs the wallet check, and the *Payment providers* panel of `/admin` names the version the wallet reported and
points to the bump checklist. That panel also shows the Bitcoin Core version (`getnetworkinfo`). The RPC version
changes only when the wallet's RPC does, so a later release with an unchanged RPC is not detected: until your
`.env` names the pinned tag, treat every failed Monero payout as possibly broadcast even when `/admin` shows no
warning.

## 9. Overpayments are returned (migration 064)

Migration 064 runs at startup: an order may now have a release **and** a return of the amount paid over the price
(`payouts.kind` `return`, `UNIQUE (order_id, kind)` instead of `UNIQUE (order_id)`). Payouts already queued are
unchanged; only orders that settle after the upgrade are split. A refund or return larger than the order price
is held for an administrator (*Admin → Payment providers*), and refunds and returns now pay their own network
fee on Monero too. The restore SQL in [section 6](#6-backups-now-need-the-wallets-too) keeps that hold's text
after its own marker. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5). See [overpayments](docs/operator-guide.md#overpayments-and-the-payment-qr-code).

## 10. Deposits after an order closed (migration 065)

Migration 065 runs at startup and adds `payments.after_close`. From this release the watcher reads every
cancelled, completed or resolved order's deposit address until the address is 30 days old (before, only while a
deposit was confirming or unhandled, or for 24 hours after the order changed), so each pass reads more addresses.
A deposit first recorded after its order closed is never refunded or paid out automatically, even when the
cancellation refunded nothing: it is flagged for payment review and the buyer is told once per order. Deposits
recorded before the upgrade keep their earlier treatment. See
[late deposits](docs/operator-guide.md#payments) and the
[runbook](docs/testnet-runbook.md#handle-a-payment-review-flag).

## 11. Payment-review dispositions (migration 066)

Migration 066 runs at startup and adds `payments.disposition`, `disposition_note`, `disposition_txid`,
`disposition_amount` and `disposed_at`; existing flags stay open. Moderators and administrators now record the
outcome of each payment-review flag on the moderation desk (no action with a reason, refunded by hand with the
transaction ID and amount, or escalated to an administrator) instead of the `psql` note the runbook described
before. Notes recorded that way earlier stay in the order history, but their flags stay open on the desk: record
each such refund there once as *refunded by hand*, with the transaction ID from its audit row. See the
[runbook](docs/testnet-runbook.md#handle-a-payment-review-flag).

## 12. Sign-ins in progress restart (migration 013)

Migration 013 runs at startup and adds `pending_logins.password_digest`, so a password change also ends a sign-in
whose password was checked just before it. A sign-in that passed the password step before the upgrade and has not
finished its second step is refused ("Sign-in verification expired. Sign in again."); the user enters the password
again. Sessions are unchanged. `/account` now lists each account's sessions by start time only (the market records
no IP address or browser), and *Sign out everywhere* also ends sign-ins waiting for their second step.

## 13. Monero releases pay their own network fee (A-232)

No migration. From this release every Monero payout, releases included, is sent with the network fee taken from
the amount (`subtract_fee_from_outputs`), as every Bitcoin payout already is: a vendor receives the price less the
fee, and no payout costs the pooled wallet its fee. A Monero release still queued when you upgrade is sent this
way, even one below the new minimum (the minimum applies when a payout is queued). The earlier version had no
minimum for Monero releases, so a pending one may be smaller than the network fee: its send now fails before
anything is broadcast (monero-wallet-rpc error -16, transaction not possible, recorded as a definite rejection),
and it is shown as *Failed — not retried; the wallet reported a pre-broadcast error, nothing broadcast*. Do not
requeue it (it would fail the same way). Settle it by hand as the runbook describes for payouts below the
minimum ([payment-review flags](docs/testnet-runbook.md#handle-a-payment-review-flag), step 4): agree with the
vendor whether to send it, and if so `transfer` the payout's amount from the wallet with the wallet paying the fee
(without `subtract_fee_from_outputs`), then record it with *Mark sent* and its transaction ID
([section 5](docs/testnet-runbook.md#5-recover-a-held-or-failed-payout)). One Monero minimum, 0.001 XMR, now
applies to every payout: a release below it is flagged for payment review instead of sent, like a refund.
Listings cannot be priced below 0.001 XMR; a listing priced lower before the upgrade takes no new XMR drafts or payment requests, and saving it is refused until its vendor raises the Monero
price. On an existing order at such a price, an overpayment of more than the price is held as one release for an
administrator's review. When matching Monero payouts sent before the upgrade in the wallet (the runbook's
[failed-payout check](docs/testnet-runbook.md#5-recover-a-held-or-failed-payout) and [restore reconciliation](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)), a release sent by the
earlier version paid its fee on top: its destination amount is the payout's.

## 14. One settlement per order, and resolving needs a password (migrations 069 and 070)

Migrations 069 and 070 run at startup. Migration 069 adds the unique index `payouts_one_settlement` on
`payouts(order_id)` for kinds `release` and `refund`: the database again refuses a release and a refund for the
same order (migration 064 had allowed it so a release could carry a `return`, which stays allowed). Migration 070
adds `payouts_refund_or_return` for kinds `refund` and `return`: a return sends a release's excess back to the
buyer, and a refund already sends back everything the buyer paid, so a refund beside a return would pay the excess
twice. An order therefore has a release with at most one return, or one refund. The application never queues
either pair, so an existing database normally upgrades without change. One that already holds a release and a
refund, or a refund and a return, for an order (only possible through hand-written SQL) is **not** changed: the
migration stops with an error naming the orders, every migration of that startup rolls back, and the server does
not start. Nothing is deleted, because any of those payouts may have been sent. While the server is stopped the
watcher sends nothing; settle each named order as below, then start the server again. Migration 073, in the same
start, also refuses an order left with a return and no release
([section 18](#18-a-return-needs-its-release-migration-073-a-268)): the steps below never leave one, and step 5
lists any that remain.

Run the SQL with `psql` against the application database. The internal-db service is shown; replace `opsecmkt`
after `-d` with the database name in `DATABASE_URL`. With an external database, make `db_sql` run your own
`psql -X -v ON_ERROR_STOP=1` connected to it, never with the password in its arguments.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt "$@"; }
```

1. **List every payout of the orders that clash, returns included, and keep the output** (here in
   `settlement-clash.txt`): once you delete rows, it is their only record apart from the pre-upgrade backup from
   [section 1](#1-take-a-backup-and-record-what-you-run), which holds them as they were before the upgrade;
   restoring that backup is the way back. `amount` is in the smallest unit of `currency`: satoshi for BTC
   (100 000 000 per BTC), piconero for XMR (10^12 per XMR). `user_id` is the recipient; `created` is when the row
   was queued and `updated` when it last changed:

   ```sh
   db_sql <<'SQL' | tee settlement-clash.txt
   SELECT p.id, p.order_id, o.state AS order_state, p.kind, p.state, p.send_ambiguous, p.currency, p.amount, p.address, p.user_id,
     p.txid, p.error, p.created, p.updated
   FROM payouts p JOIN orders o ON o.id = p.order_id
   WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
     HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
         OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1)
   ORDER BY p.order_id, p.kind;
   SQL
   ```

   Each listed order has two settlements: its **release together with its return** (the price to the vendor and
   the excess to the buyer; a return belongs to its release and is kept or deleted with it, even when the release
   row itself is gone, as after an earlier hand deletion: if such a return stands, it may not stay without a release
   (migration 073), so once this clash is settled, settle that order as
   [section 18](#18-a-return-needs-its-release-migration-073-a-268) describes: for a `completed` order or a release
   outcome its release is restored or recorded as paid, and for a `cancelled` order or a refund outcome a sent return
   is recorded as the order's refund and the rest of what the buyer is owed is sent by hand), and its **refund**.
2. **Find out which rows were sent.** A row in state `sent` was sent. A row in state `sending`, a `failed` row with
   `send_ambiguous` true, and any row whose error starts `Restored from backup:` may have been sent. Check every
   listed row in the wallet, whatever its state (a `pending`, `blocked`, `held` or definitely failed row may have
   been paid by hand): a transaction to its address and amount since before the row was created, with the complete
   checks in [runbook section 5](docs/testnet-runbook.md#5-recover-a-held-or-failed-payout). A row that may have
   been sent and that the wallet check does not clear counts as sent. Never delete a sent row, or one that may
   have been sent, except as step 3 describes for an order paid twice or for a sent release that the order's
   outcome did not owe.
3. **Decide which settlement stands.**
   - If a row of one settlement was sent and no row of the other, that settlement stands. A release stands when
     either it or its return was sent: keeping the refund after a return was sent would pay the excess twice. A
     sent return paid the buyer only the excess; the price still follows the order's outcome. For a `cancelled`
     order or a refund outcome whose release was not sent, the price belongs to the buyer: delete the unsent release
     and the refund, record the sent return as the order's refund (step 4) and send the buyer the price by hand, so
     that the vendor is not paid.

     When the sent row is a release or a refund that the order's outcome did not owe, the party it did owe is
     still unpaid and you choose how to settle. Record the choice for each such order, with the transaction ID of anything you
     pay by hand, beside `settlement-clash.txt`:
     - **A sent release on a `cancelled` order or a refund outcome, beside an unsent refund**, paid the vendor the
       price the buyer is owed. Either keep the refund: delete the sent release and its return (step 4), the
       watcher sends the refund, the buyer is made whole and the market has paid the order twice. Only do this if
       the release's return was not sent, or the refund would pay the excess a second time as well. Or delete the
       refund, keeping the release and its return (step 4), and settle the price with the buyer by hand: pay it,
       or recover it from the vendor outside the market and then pay the buyer.
     - **A sent refund alone on a `completed` order or a release outcome, beside an unsent release**, paid the
       buyer back and left the vendor owed the price. The refund stands: delete the release and its return
       (step 4). Then either pay the vendor the price by hand, so that the market pays the order twice, or recover
       it from the buyer outside the market and then pay the vendor.
   - If no row was sent, the settlement matching the order stands: the release for a `completed` order, the refund
     for a `cancelled` one, and for a `resolved` one the outcome its order history records.
   - If rows of both settlements were sent, the order was paid twice (a refund plus a release, or a refund plus a
     return that paid the excess a second time). Recover that outside the market first; the settlement whose
     payment you leave with its recipient stands. Only then delete the other settlement's rows, sent or not; the
     output of step 1 is their record.
4. **Delete the settlement that does not stand**, one order at a time with its full order ID. To keep the
   refund, delete the release **and its return** together; deleting only the release would leave its return
   beside the refund, and a pending return would then be sent, paying the excess a second time:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID <<'SQL'
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','return') RETURNING id, kind, state, txid;
   SQL
   ```

   To keep the release and its return, delete the refund:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID <<'SQL'
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'refund' RETURNING id, kind, state, txid;
   SQL
   ```

   For a `cancelled` order or a refund outcome whose return alone was sent (step 3), delete the unsent release
   together with the refund and record the sent return as the order's refund, in one transaction (a return may not
   stay without its release, [section 18](#18-a-return-needs-its-release-migration-073-a-268)); the row keeps its
   amount, state and transaction ID. Then send the buyer the price by hand:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID <<'SQL'
   BEGIN;
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','refund') RETURNING id, kind, state, txid;
   UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return' RETURNING id, kind, state, txid;
   COMMIT;
   SQL
   ```

   Each statement prints the rows it deleted or changed, which must be the rows you chose from step 1's output; a
   mistyped order ID changes nothing (`DELETE 0`, `UPDATE 0`). If a statement fails, `psql` stops and nothing in
   that block is changed.
5. **Check that no order is left with a release and a refund, a refund and a return, or a return without a
   release.** This must print no rows (`(0 rows)`):

   ```sh
   db_sql <<'SQL'
   SELECT order_id, string_agg(kind, ',' ORDER BY kind) AS kinds FROM payouts GROUP BY order_id
   HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
       OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1
       OR (count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0);
   SQL
   ```

   An order it lists with only `return` has a return without its release (from an earlier hand deletion, not from
   step 4): settle it as [section 18](#18-a-return-needs-its-release-migration-073-a-268) describes.

   Then start the new release again: migrations 069, 070 and 073 apply, and the watcher sends every row you kept that
   is `pending`. If the wallet showed in step 2 that a kept row was already sent but its state is not `sent` (it
   was paid by hand), do not start the server as it is, or that row is sent a second time: first apply the payout
   protection in [section 6](#6-backups-now-need-the-wallets-too), which pauses every payout behind the recovery
   gate, then record the row with *Mark sent* and clear the gate as the
   [runbook](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) describes. Rolling
   back past this release needs the pre-upgrade backup, as for every migration (section 5).

Resolving a dispute on the moderation desk (or `/disputes`) now also asks for the moderator's or administrator's
current password, and an authenticator code when TOTP is enrolled, like the other staff actions; the audit row
records the confirmation. Moderators still resolve disputes.

## 15. PGP sign-in with an unreadable key, and failed password confirmations (A-272, A-274)

No migration. An account with PGP sign-in on whose saved key this release can no longer read (for example a key
saved before the market refused keys like it, A-266) was signed in with the password alone by the earlier version.
It no longer is: with TOTP on it is asked for its authenticator code or a recovery code; otherwise the sign-in is
refused after the correct password, with a message naming the problem, the administrator's *Reset a user's second
factors* (for an administrator, `-reset-admin-factors` on the host) and the staff contact, and the owner is notified
once. Expect such requests after upgrading; confirm who is asking before resetting (see the
[operator guide](docs/operator-guide.md#authentication)).

The per-account limit on password-confirmed actions now counts only failed confirmations (a wrong password, or a
wrong authenticator or recovery code after the right one): staff are no longer paused after ten correct
confirmations in ten minutes (for example resolving many disputes). After ten failures the refusal now reads "Too
many failed password confirmations. Try again in ten minutes."

## 16. Issued-address collisions after a wallet restore (migration 071, A-288)

Migration 071 adds two columns to `payment_status` (a count and a time, 0 and empty on existing rows); nothing else
changes and nothing needs doing. A payment request that gets an address the database already issued, which happens
after a wallet is restored from an older backup or seed without being advanced, was answered "Unable to save
changes" with status 500. It is now refused with 503 "BTC payment addresses are being restored on this market.
Nothing was charged and your order is still a draft; try again later." (or XMR), logged as a `payments:` line naming
only the currency, and shown on the *Payment providers* panel of `/admin` until an administrator dismisses it. A
monitor that matched `cause=23505 payment_addresses payment_addresses_address_key` should match
`cause=payment-address-already-issued` instead. What to do when it appears:
[Restore the wallets](docs/testnet-runbook.md#restore-the-wallets).

## 17. An index for role changes in the audit log (migration 072, A-301)

Migration 072 adds one partial index, `audit_events_role_change`, holding only the audit rows that record a role
change. The moderation desk and `/disputes` read those rows on every load with an open dispute, to list evidence
holders removed from staff (A-294); without the index each load read the whole audit log, which grows with every
sign-in. No row is changed and the signed audit export is unaffected; nothing needs doing. Building the index reads
`audit_events` once during the first start, within `MIGRATION_TIMEOUT` (default 10 minutes; about 0.1 seconds
for a million audit rows on a test host). If your audit log is very large, raise it first as described under
[Migrations](docs/operator-guide.md#migrations).

## 18. A return needs its release (migration 073, A-268)

Migration 073 adds the constraint trigger `payouts_return_needs_release` (and its function of the same name) on
`payouts`. A return sends the excess of a release back to the buyer, so it is valid only beside the release of the
same order. Before, the database accepted a return with no release; only hand-written SQL could create one, for
example deleting a release and keeping its return while settling a clash as
[section 14](#14-one-settlement-per-order-and-resolving-needs-a-password-migrations-069-and-070) said before this
release. Now a transaction that leaves any order with a return and no release, whether it inserted, deleted or
changed rows, is refused when it commits, with "payouts: order ORDER_ID has a return but no release. … so this
transaction was rolled back and nothing was changed. Settle the order as UPGRADING.md describes (A return needs its
release, migration 073).", and nothing of it is kept. The check runs at commit, so one transaction may insert a
release and its return in either order, or delete both. The application queues a return only together with its
release, so nothing it does is refused.

A database that already holds a return without a release is **not** changed: the migration stops with an error
naming the orders ("payouts: order(s) … have a return but no release. Nothing was changed. …"), every migration of
that startup rolls back, and the server does not start. Nothing is deleted, because the return, or the release that
is missing, may have been sent. While the server is stopped the watcher sends nothing. If the error names migration
069 or 070 instead, settle section 14 first (its step 5 also lists the orders this section settles). Settle each
order named here as below, with `db_sql` defined as in section 14, then start the server again.

1. **List the payouts of those orders and keep the output** (here in `return-without-release.txt`). Each listed
   order has only its return: migrations 069 and 070 refuse the other pairs first. `order_state` and
   `dispute_outcome` (for a `resolved` order) say whom the order owes; `price`, like `amount`, is in the smallest
   unit of `currency` (satoshi or piconero):

   ```sh
   db_sql <<'SQL' | tee return-without-release.txt
   SELECT p.id, p.order_id, o.state AS order_state, d.outcome AS dispute_outcome, o.amount AS price, p.kind, p.state, p.send_ambiguous,
     p.currency, p.amount, p.address, p.user_id, p.txid, p.error, p.created, p.updated
   FROM payouts p JOIN orders o ON o.id = p.order_id LEFT JOIN disputes d ON d.order_id = p.order_id
   WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
     HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0)
   ORDER BY p.order_id, p.kind;
   SQL
   ```

2. **Check in the wallet whether each return was sent**, as in section 14 step 2: a row in state `sent` was sent;
   one in state `sending`, a `failed` row with `send_ambiguous` true, or one whose error starts
   `Restored from backup:` may have been sent; check every row in the wallet whatever its state, and count a row
   that may have been sent and that the wallet does not clear as sent. Then find the missing release: the output
   you kept when it was deleted (such as `settlement-clash.txt` from section 14), the pre-upgrade backup and the
   order history ("Queued … release of …") give its address and amount. Check in the wallet whether the vendor was
   paid the price, by that release or by hand.
3. **Settle each order**, one at a time with its full order ID. Each statement prints the row it inserted, changed
   or deleted; a mistyped order ID, or an order the statement does not fit, changes nothing (`INSERT 0 0`,
   `UPDATE 0`, `DELETE 0`).
   - **A `completed` order, or a `resolved` one with the `release` outcome**, owes the vendor the price, so its
     return keeps a release. If the wallet shows that the vendor was paid the price, record that release as sent,
     with the address and transaction ID the wallet shows:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID -v address=VENDOR_ADDRESS -v txid=TRANSACTION_ID <<'SQL'
     INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, txid)
     SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, :'address', 'sent', :'txid'
     FROM orders o JOIN products p ON p.id = o.product_id
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
     RETURNING id, kind, state, amount, address, txid;
     SQL
     ```

     Otherwise restore the release of the price to the vendor's saved payout address, held for an administrator:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, error)
     SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, CASE o.currency WHEN 'XMR' THEN v.payout_xmr ELSE v.payout_btc END, 'held',
       'Restored by hand (UPGRADING.md section 18): check the wallet before releasing it.'
     FROM orders o JOIN products p ON p.id = o.product_id JOIN users v ON v.id = p.vendor_id
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
     RETURNING id, kind, state, amount, address;
     SQL
     ```

     Once the server runs, an administrator releases it under *Payouts* on `/admin` after checking the wallet once
     more (and, if the vendor's account is suspended, checking the address with the vendor); the watcher then sends
     it once. Either way the return stays as it is: if it was not sent, the watcher sends it to the buyer.
   - **A `cancelled` order, or a `resolved` one with the `refund` outcome**, owes the buyer everything it received
     and the vendor nothing, so no release is restored. If the return was sent, record it as the order's refund; the
     row keeps its amount, state and transaction ID:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, amount, txid;
     SQL
     ```

     If it was not sent, delete it (neither statement touches a return beside its release, and this one never
     deletes a row in state `sent`):

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'return' AND state <> 'sent'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, txid;
     SQL
     ```

     Then send the buyer the rest by hand from the wallet: the price, and the return's amount too if it was not
     sent, unless you already paid it (as section 14 said for such an order before this release). Record the
     transaction ID beside `return-without-release.txt`; do not insert a payout or use *Mark sent* for it.
4. **Check that no order is left with a return and no release.** This must print no rows (`(0 rows)`):

   ```sh
   db_sql <<'SQL'
   SELECT order_id FROM payouts GROUP BY order_id
   HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0;
   SQL
   ```

   Then start the new release again: migration 073 applies. As in section 14, if a return you kept was paid by
   hand but its state is not `sent`, apply the payout protection in
   [section 6](#6-backups-now-need-the-wallets-too) first and record it with *Mark sent*, or the watcher sends it a
   second time. Rolling back past this release needs the pre-upgrade backup (section 5): the earlier release refuses
   to start on a database that records migration 073.

## 19. Bitcoin escrow tables (migration 074)

Migration 074 runs at startup. It adds the tables Bitcoin escrow uses (`user_escrow_keys`, `escrows` with each
escrow's pinned refund address and market-fee snapshot, `escrow_pubkeys`, `settlements`, the sequence
`escrow_market_index`), the column `payments.settlement_id`, and a trigger, `payouts_not_escrow`, that refuses a
`payouts` row for an order that has an escrow (an escrow order is settled from its escrow, never by a pooled payout).
The tables stay empty unless `ESCROW_BTC=on`, a regtest-only setting that is off by default ([operator
guide](docs/operator-guide.md#bitcoin-escrow-regtest-only)), and the trigger changes nothing for pooled orders; nothing
changes for users or payments. No action is needed. Rolling back past this release needs the pre-upgrade backup, as
for every migration (section 5).

## 20. Bitcoin escrow settlements (migration 075)

Migration 075 runs at startup. It adds to `settlements` what the server needs to build an escrow's settlement and take
one party's signature (the destination, requester, fee rate, network fee, amount spent, unsigned PSBT and its
transaction ID, which party signed and when, and why a build waits), a unique index so no two vendor claims pay the
same address, the tables `settlement_outputs` (each settlement's fixed outputs) and `settlement_signatures` (one
party's signature per input; the pasted PSBT is never stored), and two triggers: `settlements_frozen` refuses any
change to a settlement's transaction once a party has signed it, and `settlement_outputs_frozen` allows outputs to be
written only while their settlement is being built. Like migration 074 it changes nothing unless `ESCROW_BTC=on`
(regtest only, off by default); the new optional key `ESCROW_BTC_FEE_RATE` (sat/vB, default 2) sets the network fee
rate of settlements ([operator guide](docs/operator-guide.md#bitcoin-escrow-regtest-only)). No action is needed.
Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5).

## 21. Bitcoin escrow co-signing, broadcast, dispute decisions and stray refunds (migration 076)

Migration 076 runs at startup. It adds to `settlements` the final transaction the market stores before it broadcasts a
settlement (`raw_tx`, who completed it, when it was stored, broadcast and confirmed, the transaction that spent its
coins when another one did, and why it is not sent yet), a state `superseded` for the settlement of a dispute decision
an administrator changed (the one claim or refund per order no longer counts it), a unique index allowing one stray
refund in progress per order, the column `payments.escrow_left_out` (an escrow output a bounded build left for a stray
refund) and `escrows.recover_requested` (a buyer's "Recover coins at this escrow"). It replaces the function of the
trigger `settlements_frozen`: a settlement's transaction still never changes once a party has signed it, a party
signature that did not verify may be discarded before the final transaction exists, and the stored final transaction
never changes and the settlement never goes back; a new trigger, `settlement_signatures_kept`, refuses any other
change to stored signatures. Like migrations 074 and 075 it changes nothing unless `ESCROW_BTC=on` (regtest only, off
by default). With escrow on, this release also co-signs and broadcasts settlements, lets an administrator decide
escrow disputes, and writes three `settings` keys when used: `escrow_btc_fee_bps` and `escrow_btc_fee_address` (the
market fee an administrator saves on `/admin`, which then overrides `ESCROW_BTC_FEE_BPS` and
`ESCROW_BTC_FEE_ADDRESS`) and `escrow_btc_stray_check` (the daily stray-refund check). Escrow now also stays disabled
unless the arbiter wallet is an unencrypted descriptor wallet with private keys and no transactions ([operator
guide](docs/operator-guide.md#bitcoin-escrow-regtest-only)). No action is needed. Rolling back past this release needs
the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that
records migration 076.

## 22. Bitcoin escrow recovery and reconciliation (migration 077)

Migration 077 runs at startup. A new trigger, `settlements_moves`, lets a settlement with a final transaction move only
forward (signed to broadcast or external, broadcast to confirmed or external), with one step back for a reorganisation:
confirmed to broadcast, when its transaction drops out of the chain and is sent again (confirmed to external when a
conflicting transaction was mined instead); an external settlement never changes. It adds `settlements.reorg_safe_at`
(a confirmed settlement is followed until its transaction has 100 confirmations), a third value `network` for
`settlements.cosigned` (a settlement whose transaction the market found already on the network when it came to complete
it), `payments.spent_outside` and `payments.spent_by` (an escrow deposit spent by a transaction the market did not make,
with no settlement covering it, flagged for staff) and the table `escrow_orphans` (addresses the escrow watch-only wallet
holds with no escrow row, listed on `/admin`). Like migrations 074–076 it changes nothing unless `ESCROW_BTC=on`
(regtest only, off by default); pooled payments are unchanged. No action is needed. Rolling back past this
release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a
database that records migration 077.

## Appendix: new keys by hand

`./scripts/install.sh upgrade` writes these for you. To add them yourself, use your editor (values in single
quotes, as the installer writes them). Blank values keep a feature off.

| Key | What to put there |
| --- | --- |
| `AUDIT_SIGNING_KEY` | `openssl rand -hex 32` output. Enables signed audit exports; back it up with `.env`. |
| `BITCOIN_CHAIN` | Leave blank to accept whichever test chain the node reports, or `testnet4` / `signet` / `regtest`. `main` configures mainnet (real funds; [operator guide](docs/operator-guide.md#mainnet)); mainnet is refused otherwise. See step 3 before setting it for a local node. |
| `BITCOIN_WALLET` | Wallet name, default `opsecmkt`. |
| `MONERO_NETWORK` | Blank, `stagenet` or `testnet`; `mainnet` configures mainnet (a local node also needs `MONERO_NETWORK_FLAG=`). |
| `MONERO_WALLET_RPC_URL` | monero-wallet-rpc URL with credentials, for example `http://marketplace:PASSWORD@monero-wallet:18083`. Blank keeps Monero payments off. A daemon URL alone (`MONERO_RPC_URL`, which alpha.1 wrote) does **not** enable them. |
| `MONERO_WALLET_RPC_PASSWORD` | For the local `monero-wallet` service only: `openssl rand -hex 24` output, the same value as the password in `MONERO_WALLET_RPC_URL`. The service refuses to start without it. |
| `PAYMENT_CONFIRMATIONS_BTC` / `PAYMENT_CONFIRMATIONS_XMR` | Defaults 3 and 10. |
| `PAYMENT_POLL_INTERVAL` / `PAYMENT_EXPIRY` | Defaults `30s` and `24h`. |
| `BITCOIN_PRUNE_MB` / `MONERO_PRUNE_FLAGS` | Local nodes only. Absent keys mean **pruned** (`2000` and `'--prune-blockchain --sync-pruned-blocks'`); `BITCOIN_PRUNE_MB='0'` and `MONERO_PRUNE_FLAGS=''` keep a full node. Read [section 7](#7-local-nodes-are-now-pruned) before deploying if you run a local node. |
| `BITCOIN_IMAGE` / `MONERO_IMAGE` / `MONERO_WALLET_IMAGE` | Local nodes only. Absent keys mean the pinned release images the payout code was verified against; the images alpha.1 made you choose stay in use until you remove or change them (`upgrade` pins the default images left on `latest`). Read [section 8](#8-node-images-are-pinned-to-release-tags). |
| `COMPOSE_PROJECT_NAME` | Not needed: absent means `opsecmkt`, which is what your containers and volumes are named after. Never set another value on this install, or Compose starts it on new, empty volumes. A second checkout on the same host needs its own name; see [Backups and recovery](docs/operator-guide.md#backups-and-recovery). |
| `CLAMD_ADDRESS` / `CLAMAV_IMAGE` | The malware scanner photo uploads need. Leave `CLAMD_ADDRESS` blank (uploads off) and `CLAMAV_IMAGE` at `clamav/clamav:1.5.4-debian`; `./scripts/install.sh add-scanner` turns the scanner on. See [Photo uploads and the malware scanner](docs/operator-guide.md#photo-uploads-and-the-malware-scanner). |
| `DATABASE_CONNECT_TIMEOUT` / `MIGRATION_LOCK_TIMEOUT` / `MIGRATION_TIMEOUT` | Optional; leave blank for `15s`, `10m` and `10m`. Raise `MIGRATION_TIMEOUT` if your database is large; see [Migrations](docs/operator-guide.md#migrations). |

For a local Monero node, also add `monero-wallet` to `COMPOSE_PROFILES` (for example
`COMPOSE_PROFILES='internal-db,monero,monero-wallet'`). If you run the Tor onion mirror, add `mirror` there too
(for example `COMPOSE_PROFILES='internal-db,mirror'`) instead of starting it with a `--profile mirror` option,
which drops the other profiles for that command; without it in `.env`, `docker compose up -d` leaves the mirror
out and Compose does not restart its Tor with the app. See
[Optional onion mirror](docs/operator-guide.md#optional-onion-mirror).
