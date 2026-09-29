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
itself.

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
