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
settlement with a stored transaction until an administrator confirms its rebroadcast on `/admin`; the
`settlement_bumps` line holds every restored fee bump of one with a stored transaction until that confirmation of its
settlement (omit it if the database has no `settlement_bumps` table, before migration 080). The
`escrow_release_recheck` line asks the escrow watcher's next pass to move every release deadline of a shipped physical
escrow order that has passed in the restored records to 72 hours later, telling each buyer; the
`escrow_xmr_release_recheck` line asks the same of the Monero escrow watcher for Monero escrow orders, on its first pass
after the restore gate is cleared (both lines act only while that escrow is on). The `escrow_sweep_required`
line makes escrow checkout wait until the escrow watcher has reserved the keys of every escrow its watch-only wallet
holds and the restored records lack, and the `escrow_index_floors` and `setval` lines move every account's next escrow
child index and the market's escrow key index 50 past the restored records, so no escrow key is handed out twice (A-366;
omit those two lines if the database has no `escrow_index_floors` table, before migration 083). For Monero escrow
(`ESCROW_XMR`, local fakechain only), the `xmr_settlements` line holds every restored Monero escrow settlement with a
stored transaction (built or relayed) with the same marker until an administrator confirms its relay on `/admin`, the
`xmr_escrow_slots` line retires every free Monero escrow account and the `xmr_escrow_wallets` line moves every active or
sealed Monero escrow wallet to closing, so no restored wallet takes an order again (omit those three lines if the
database has no `xmr_settlements.send_error` column, before migration 085); the `xmr_escrow_sweep_required` line makes
Monero escrow checkout wait until the Monero escrow watcher has found the escrow wallets created after the backup
(each is sealed to the seed key and kept, never offered to a new order) and checked every escrow wallet. After restoring
only the escrow wallet volume from an older backup, run that settings line alone: the watcher then also raises the
subaddress lookahead of each escrow wallet older than the database and rescans it, so deposits to the accounts it does
not know are seen.

<!-- runbook-sql: restore-protection -->
```sql
BEGIN;
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_xmr_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('xmr_escrow_sweep_required','true')
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
UPDATE settlement_bumps SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
WHERE raw_tx IS NOT NULL AND state IN ('signed','broadcast');
UPDATE xmr_settlements SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
WHERE state IN ('built','relayed');
UPDATE xmr_escrow_slots SET state='retired', updated=now() WHERE state='free';
UPDATE xmr_escrow_wallets SET state='closing', updated=now() WHERE state IN ('active','sealed');
INSERT INTO escrow_index_floors(user_id,next_index)
SELECT k.user_id, GREATEST(COALESCE(max(split_part(p.derivation_suffix,'/',3)::bigint),0)+1, COALESCE(max(f.next_index),1))+50
FROM user_escrow_keys k
LEFT JOIN escrow_pubkeys p ON p.user_id=k.user_id AND p.derivation_suffix ~ '^/0/[1-9][0-9]{0,9}$'
LEFT JOIN escrow_index_floors f ON f.user_id=k.user_id
GROUP BY k.user_id
ON CONFLICT (user_id) DO UPDATE SET next_index=EXCLUDED.next_index;
DO $$ BEGIN PERFORM setval('escrow_market_index', nextval('escrow_market_index')+49); END $$;
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
     order or a refund outcome whose release was not sent, the price belongs to the buyer: send the buyer the price
     by hand, then delete the unsent release and the refund and record the sent return as the order's refund, with
     a note of the price you sent (step 4), so that the vendor is not paid.

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

   For a `cancelled` order or a refund outcome whose return alone was sent (step 3), first send the buyer the price
   by hand from the wallet, and note the amount the wallet sent and its transaction ID. Then delete the unsent
   release together with the refund and record the sent return as the order's refund, in one transaction (a return
   may not stay without its release, [section 18](#18-a-return-needs-its-release-migration-073-a-268)); the row keeps
   its amount, state and transaction ID, so the order page shows only the returned excess as the refund. The same
   transaction adds a note to the order history, which the buyer and the vendor read, saying that the return is the
   refund and naming the amount you sent by hand and its transaction ID (the vendor's page hides the transaction ID,
   as for every payout to the buyer). Give the amount as the wallet shows it, in BTC or XMR, as plain decimal digits without a sign or unit (for example `0.001`; the block refuses `NaN`, `Infinity` and exponent forms),
   and the 64-character transaction ID in lowercase; no address goes into the note:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID -v amount=AMOUNT_SENT -v txid=TRANSACTION_ID <<'SQL'
   BEGIN;
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','refund') RETURNING id, kind, state, txid;
   UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return' RETURNING id, kind, state, txid;
   INSERT INTO order_events(order_id, from_state, to_state, note)
   SELECT o.id, o.state, o.state, 'Refund recorded by hand (UPGRADING.md section 14): the overpayment returned to the buyer earlier is this order''s refund, and the remaining '
     || CASE WHEN :'amount' ~ '^[0-9]+(\.[0-9]+)?$' AND :'amount'::numeric > 0 THEN trim_scale(:'amount'::numeric)::text END || ' ' || o.currency
     || ' was sent to the buyer by hand from the market wallet in transaction ' || CASE WHEN :'txid' ~ '^[0-9a-f]{64}$' THEN :'txid' END || '.'
   FROM orders o
   WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'refund' AND r.updated = now())
   RETURNING order_id, note;
   COMMIT;
   SQL
   ```

   Each statement prints the rows it deleted, changed or inserted, which must be the rows you chose from step 1's
   output and the note you meant; a mistyped order ID changes nothing (`DELETE 0`, `UPDATE 0`, `INSERT 0 0`). The
   note is added only when the statement before it relabelled the return. If a statement fails, `psql` stops and
   nothing in that block is changed: a missing, empty or malformed amount or transaction ID stops it with an error
   (for example `syntax error at or near ":"` or `null value in column "note"`).
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
     and the vendor nothing, so no release is restored. If the return was sent, first send the buyer the price by
     hand from the wallet, unless you already paid it (as section 14 said for such an order before this release),
     and note the amount the wallet sent and its transaction ID. Then record the return as the order's refund; the
     row keeps its amount, state and transaction ID, so the order page shows only the returned excess as the refund.
     The same transaction adds a note to the order history, which the buyer and the vendor read, saying that the
     return is the refund and naming the amount you sent by hand and its transaction ID (the vendor's page hides the
     transaction ID, as for every payout to the buyer). Give the amount as the wallet shows it, in BTC or XMR, as
     plain decimal digits without a sign or unit (for example `0.001`; the block refuses `NaN`, `Infinity` and exponent forms), and the 64-character transaction ID in lowercase; no address goes into the note:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID -v amount=AMOUNT_SENT -v txid=TRANSACTION_ID <<'SQL'
     BEGIN;
     UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, amount, txid;
     INSERT INTO order_events(order_id, from_state, to_state, note)
     SELECT o.id, o.state, o.state, 'Refund recorded by hand (UPGRADING.md section 18): the overpayment returned to the buyer earlier is this order''s refund, and the remaining '
       || CASE WHEN :'amount' ~ '^[0-9]+(\.[0-9]+)?$' AND :'amount'::numeric > 0 THEN trim_scale(:'amount'::numeric)::text END || ' ' || o.currency
       || ' was sent to the buyer by hand from the market wallet in transaction ' || CASE WHEN :'txid' ~ '^[0-9a-f]{64}$' THEN :'txid' END || '.'
     FROM orders o
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'refund' AND r.updated = now())
     RETURNING order_id, note;
     COMMIT;
     SQL
     ```

     The note is added only when the statement before it relabelled the return. A missing, empty or malformed amount
     or transaction ID stops the block with an error (for example `syntax error at or near ":"` or
     `null value in column "note"`) and nothing is changed.

     If it was not sent, delete it (neither statement touches a return beside its release, and this one never
     deletes a row in state `sent`):

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'return' AND state <> 'sent'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, txid;
     SQL
     ```

     Then send the buyer the price and the return's amount by hand from the wallet, unless you already paid them. In
     either case record the transaction ID of what you sent by hand beside `return-without-release.txt`; do not
     insert a payout or use *Mark sent* for it.
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
guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)), and the trigger changes nothing for pooled orders; nothing
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
rate of settlements ([operator guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)). No action is needed.
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
unless the arbiter wallet is an unencrypted descriptor wallet with private keys that has never spent (A-471, section 38; [operator
guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)). No action is needed. Rolling back past this release needs
the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that
records migration 076.

## 22. Bitcoin escrow recovery and reconciliation (migration 077)

Migration 077 runs at startup. A new trigger, `settlements_moves`, lets a settlement with a final transaction move only
forward (signed to broadcast or external, broadcast to confirmed or external), with one step back for a reorganisation:
confirmed to broadcast, when its transaction drops out of the chain and is sent again (confirmed to external when a
conflicting transaction was mined instead); an external settlement never changes. It adds `settlements.reorg_safe_at` (a
confirmed settlement is followed until its transaction has 100 confirmations), a third value `network` for
`settlements.cosigned` (a settlement whose transaction the market found already on the network when it came to complete
it), `payments.spent_outside` and `payments.spent_by` (an escrow deposit spent by a transaction the market did not make,
with no settlement covering it, flagged for staff) and the table `escrow_orphans` (addresses the escrow watch-only
wallet holds with no escrow row, listed on `/admin`). Like migrations 074–076 it changes nothing unless `ESCROW_BTC=on`
(regtest only, off by default); pooled payments are unchanged. With escrow on, this release also pauses escrow
co-signing, settlement builds and every broadcast of a stored settlement transaction while the restore gate
(`payments_recovery_required`) is set, and `scripts/restore.sh` and the manual protection SQL in section 6 now also hold
every restored escrow settlement that has a stored transaction, which an administrator confirms on `/admin` before the
market broadcasts it again. While the gate is set, escrow checkout is also refused with a fixed 503 and the order stays
a draft. The escrow RPC user's `rpcwhitelist` needs one more method, `listtransactions` (it reads the
watch-only wallet only; the escrow watcher uses it to name the transaction that spent an escrow deposit): add it before
upgrading, or that lookup is refused and such a deposit is flagged without the transaction ID ([operator
guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)). No other action is needed. Rolling back past this release
needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database
that records migration 077.

## 23. Bitcoin escrow settlements spent while they wait (migration 078)

Migration 078 runs at startup. It changes two rules on `settlements` so the escrow watcher can close a settlement whose
escrow deposit another transaction spent while the settlement was still being built or was waiting for a party's
signature (A-356): the check `settlements_built` now also allows an `external` settlement that was never built (no
PSBT, transaction ID, fee or amount), and the function of the trigger `settlements_frozen` now also allows a settlement
waiting for a signature to store its final transaction when it moves to `signed` (the market found that exact
transaction already on the network: the buyer and the vendor signed it and sent it themselves). Every other rule of
migrations 075–077 is unchanged. Like them it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default);
pooled payments are unchanged. With escrow on, this release also checks a new escrow's address against the escrow
watch-only wallet before the escrow is created: if the wallet already watches it (an escrow that a restored database no
longer has), the next child keys are tried, at most 20 tries in all, and after the last the checkout is refused with a
fixed 503 and the order stays a draft. Only the whole composed address is checked ([runbook](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)).
No action is needed. Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5):
the earlier release refuses to start on a database that records migration 078.

## 24. Bitcoin escrow for physical orders: dispatch windows and deadlines (migration 079)

Migration 079 runs at startup. It adds `products.dispatch_days` (a listing's dispatch window, 1 to 30 days; empty for
every listing that exists before the upgrade) and six columns to `escrows` (`release_days` and `dispatch_days`, the
release deadline length and dispatch window an escrow snapshots at checkout; `ship_by`, when a buyer may cancel an
unshipped paid order; `release_at`, the release deadline; `reminded_at`, the 72-hour reminder; `release_recheck`, set
once a restore has moved a deadline), and a trigger, `escrows_deadlines_frozen`, that refuses any change to a snapshot
or a deadline once it is set, except that a restore's recheck may move `release_at` later. Like migrations 074–078 it
changes nothing unless `ESCROW_BTC=on` (regtest only, off by default): with it off nothing reads or writes the new
escrow columns, and the only visible effect is that a physical listing created or saved from now on is stored with a
dispatch window (3 days unless its vendor gives another; the listing form shows the field only while escrow is on).
With escrow on, this release also offers escrow checkout for physical listings, adds the administrator's release
deadline length to `/admin` (setting `escrow_release_days`, default 14, 7 to 30 days; [operator
guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)) and a ship-by time and a release deadline to each physical
escrow order. A physical listing that exists before the upgrade has no dispatch window and takes no escrow checkout
until its vendor saves it; a save stores the default of 3 days even while escrow is off (the field is then hidden),
so once escrow is turned on such a listing takes escrow checkout with a 3-day window its vendor did not choose. A
listing that already has a window keeps it on save. No action is needed. Rolling back past this release needs the pre-upgrade backup, as for
every migration (section 5): the earlier release refuses to start on a database that records migration 079.

## 25. Bitcoin escrow fee bumps (migration 080)

Migration 080 runs at startup. It adds the tables `settlement_bumps` (a replacement of a broadcast settlement's
transaction that pays the same outputs with a higher network fee: who asked for it, its fee rate, unsigned PSBT and
transaction ID, which party signed first, the final transaction once stored, and its state) and
`settlement_bump_signatures` (one party's signature per input; the pasted PSBT is never stored), the function
`settlement_claimant`, and triggers that make a bump follow its rules in the database: requested only for a broadcast
settlement, built only by the market, signed first by the claimant (the vendor of a release, the buyer of a refund),
final once stored, moving only forward, and never deleted. The settlement's own row never changes. Like the earlier
escrow migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default); pooled payments are
unchanged. With escrow on, the claimant of a broadcast settlement that is not yet mined sees a form to raise its network
fee on the order page; the escrow RPC user's `rpcwhitelist` needs no new method. `scripts/restore.sh` and the manual SQL
in [section 6](#6-backups-now-need-the-wallets-too) hold a restored fee bump with a stored transaction until an
administrator confirms its settlement's rebroadcast (omit the `settlement_bumps` line of that SQL if you apply it to a
database from before this migration). No action is needed. Rolling back past this release needs the pre-upgrade
backup, as for every migration (section 5): the earlier release refuses to start on a database that records migration
080.

## 26. Bitcoin escrow: the market signs only when the order's state fits (migration 081)

Migration 081 runs at startup. It adds the function `escrow_market_may_sign` and two triggers, `settlements_market_fits`
and `settlement_bumps_market_fits`: a final transaction the market co-signed is stored only when the order's state fits
the settlement's kind (a release on a completed order or one decided for the vendor, a refund on a cancelled order or
one decided for the buyer, a stray refund on any closed order). It is the database's backstop for a physical order's
release, which is built when the order is shipped and which the buyer and the vendor complete with their own two
signatures before the order's outcome. Rows stored before the migration are not checked again. Like the earlier escrow
migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back
past this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to
start on a database that records migration 081.

## 27. Bitcoin escrow fee bumps seen on the network (migration 082)

Migration 082 runs at startup. It replaces the function of the trigger `settlement_bumps_frozen` and allows a third
value, `network`, for `settlement_bumps.cosigned`: a fee bump whose transaction is already on the network when the
market comes to follow it (the buyer and the vendor signed it and sent it themselves, or a restored database holds an
older row whose transaction was sent after the backup) is checked, stored and recorded as broadcast instead of being
treated as a spend outside the market. Every other rule of migration 080 is unchanged. Like the earlier escrow
migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back
past this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to
start on a database that records migration 082.

## 28. Bitcoin escrow keys after a restore (migration 083)

Migration 083 runs at startup. It adds the tables `escrow_reserved_keys` (each public key of an address the escrow
watch-only wallet holds with no escrow on record, with that address) and `escrow_index_floors` (for each account with an
escrow key, the lowest child index its next escrow may use). Both stay empty unless the market has run Bitcoin escrow
(`ESCROW_BTC=on`, regtest only, off by default) and has been restored from a backup. Every restore by
`scripts/restore.sh` (and the manual SQL in [section 6](#6-backups-now-need-the-wallets-too), which already carries
these lines together with the one that asks for a release deadline recheck) sets `escrow_release_recheck` and
`escrow_sweep_required`, which act only while escrow is on, and writes the floors and moves the market's escrow key
index 50 past the restored records; with escrow on, checkout then answers 503 until the escrow watcher has reserved the
keys of every orphan address, so that no escrow key is handed out twice (A-366). A dump taken before this migration has
no `escrow_index_floors` table when `scripts/restore.sh` runs, so that restore gets the sweep and the
recheck but not the margin. If an orphan's keys cannot be read, checkout stays paused until an operator deals with it:
see the [test-network runbook](docs/testnet-runbook.md#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused).
No action is needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5): the earlier release refuses to start on a database that records migration 083.

## 29. Monero escrow tables (migration 084)

Migration 084 runs at startup. It adds the tables Monero escrow uses: `xmr_escrow_wallets` (each monthly escrow wallet's
name, primary address, restore height, seed sealed to the operator's OpenPGP key with that key's fingerprint, state and
retention dates), `xmr_escrow_slots` (each wallet account's index and address, and the order that claimed it),
`xmr_settlements` and `xmr_settlement_outputs` (each settlement's destination and outputs, and its stored transaction).
It adds `escrows.coin` (`BTC` for every existing row, which reads back unchanged), `escrows.xmr_slot_id` and
`payments.xmr_settlement_id`, makes `escrows.market_index` nullable (a check requires it for exactly the Bitcoin
escrows), and adds triggers that make the database enforce Monero escrow's rules: an escrow's coin is its order's
currency, a Bitcoin settlement refuses a Monero escrow and the other way round, a stored transaction never changes and
is stored only when the order's state fits (the rule of migration 081), states move only forward, and a Monero escrow
deposit names the account its order claimed. The migration only adds: no existing row is rewritten. **One change reaches
Bitcoin escrow too:** since this migration the database refuses to delete a row of `escrows` or of any `xmr_*` table
(the application never deletes one; a deleted `escrows` row would turn its order into a pooled one). Otherwise it
changes nothing while `ESCROW_XMR` is off, which is the default: the new tables stay empty and nothing writes them (the
admin page only reads them, to list held settlements and destroyed wallets), and pooled and Bitcoin escrow payments are
unchanged.

Monero escrow is a separate, off-by-default setting that runs on a local Monero fakechain only ([operator
guide](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet)); it adds the optional keys `ESCROW_XMR`,
`ESCROW_XMR_WALLET_RPC_URL`, `ESCROW_XMR_DAEMON_URL`, `ESCROW_XMR_FEE_BPS`, `ESCROW_XMR_FEE_ADDRESS`,
`ESCROW_XMR_SLOTS`, `ESCROW_XMR_MAX_FEE` and `ESCROW_XMR_SEED_KEY`, and the secret `MONERO_WALLET_SECRET` (never
invented by `upgrade`; `openssl rand -hex 32`). Leave them blank to keep it off. `scripts/restore.sh` and the manual SQL
in [section 6](#6-backups-now-need-the-wallets-too) now also hold restored Monero escrow settlements, retire every free
account, move every active or sealed escrow wallet to closing and ask for the Monero escrow sweep (the restore writes
those rows whether or not `ESCROW_XMR` is on; they matter once it is). No action is needed at upgrade. Rolling back past
this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a
database that records migration 084.

## 30. Monero escrow settlement status and re-requests (migration 085)

Migration 085 runs at startup. It adds `xmr_settlements.send_error` (why a settlement is not built or sent yet, why it
failed, or that a restore holds it, as a fixed class and never text from a node) and the table
`xmr_settlement_rerequests` (an administrator's request to build a failed settlement again: who asked, when, and how it
was decided, refused with a fixed reason or done with the new settlement). A request is recorded only for a failed
settlement that has a stored transaction, at most one is open per settlement, its state moves only forward, and rows are
never deleted. Like migration 084 it only adds, and it changes nothing while `ESCROW_XMR` is off: nothing writes either.
With Monero escrow on, an administrator who is not a party to the order may re-request a failed settlement from the
order page (password step-up, audited), and a settlement set aside because building it made the escrow wallet-rpc stop
responding can be built again the same way; the held settlements of a restore are listed on `/admin` and confirmed there
([operator guide](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet)). The restore's Monero hold is written to
`xmr_settlements.send_error`, so the three Monero lines of the SQL in [section 6](#6-backups-now-need-the-wallets-too)
need this migration (a database with 084 but not 085 exists only on a development build of this phase). No action is
needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5): the
earlier release refuses to start on a database that records migration 085.

## 31. Monero escrow destruction record (migration 086)

Migration 086 runs at startup. It adds `xmr_escrow_wallets.destroy_started_at`: when the Monero escrow watcher began
destroying a wallet, recorded before it replaces the wallet's password. A trigger makes the database enforce it: it is
set once, only on a retained wallet past its destruction date that still has its sealed seed, never changes or clears,
and the wallet then moves only to destroyed or missing, or back to retained when it opens again with the market's
password (its password change never took effect), to be destroyed once every precondition holds again. With it, a wallet
that no longer opens with the market's password ("Invalid password") is destroyed again automatically only when the
market itself began destroying it (a crash, or a restore from a backup taken in between); any other such wallet keeps
its sealed seed, is recorded missing and waits for an administrator's confirmation on `/admin` ([operator
guide](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet)). So a wallet destroyed after the backup a database is
restored from is recorded missing, not destroyed again, unless that backup already recorded the start of its destruction
(never one taken before this migration). Like migrations 084 and 085 it only adds, and it changes nothing while
`ESCROW_XMR` is off: nothing writes the column. No action is needed at upgrade. Rolling back past this release needs the
pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that records
migration 086.

## 32. Bitcoin escrow: changing a decision whose settlement was never built (migration 087)

Migration 087 runs at startup. It lets a Bitcoin escrow release or refund that was never built (still building, or
failed, with no PSBT) be superseded when an administrator changes the dispute decision; before it, that change failed
with a server error (`settlements_built`, A-462) and the decision could not be changed at all. It replaces the check
`settlements_built` with one that also accepts such a superseded row (no PSBT, transaction ID, network fee or signer;
a failed row keeps the value and fee rate it failed at) and adds the trigger `settlements_supersede_keeps`: superseding
a settlement never changes its build, destination or signer, so a built one keeps its whole PSBT and an unbuilt one
never gains one, and the trigger `settlements_never_rebuilding`: no settlement moves back to building (nothing in the
market does). Every other rule of migrations 075 to 078 is unchanged, and no existing row is rewritten. It also adds
`payments.escrow_too_few_by`, the refund whose too-small failure left a deposit out for a stray refund, set only with
`escrow_left_out` and only to a refund of the deposit's own order (trigger `payments_too_few_refund`): when a changed
decision supersedes that refund, those deposits count for the order again.
Deposits a too-small refund left out before this migration on an order decided for the buyer (only a development build
with A-437 and without this migration wrote them) do not name their refund, so a change of that decision leaves them
for the stray refund to the buyer; an order whose refund failed so before A-437, with its deposits still counted, is
handled by the next escrow pass, which names the refund. Like the earlier escrow migrations it changes nothing unless
`ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back past this release needs the
pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that
records migration 087.

## 33. Bitcoin node sync guard and the escrow chain pin (A-439)

No migration. The pooled Bitcoin watcher now also skips its pass while the node's headers are ahead of its blocks and,
on a public test network, while the node has no peers or its tip block is older than the new optional key
`BITCOIN_MAX_TIP_AGE` (default `2h`, 30m to 24h; an invalid value stops startup): a node that lost its peers used to
look synced, and an order paid while it was cut off could be cancelled for non-payment. The admin page's last error
then reads "Node is not synced (…)". A public-network node whose `getblockchaininfo` has no `time` field counts as
not synced too ("tip time unreadable: this Bitcoin Core is too old for the sync guard; use Bitcoin Core 31.1 or
later"): the guard needs Bitcoin Core 31.1 (the version the installer pins; earlier versions untested). A regtest
node is unaffected. With `ESCROW_BTC=on` the escrow watcher applies the
same rule, the app pins the escrow node's chain in `settings` (`escrow_btc_chain`; an existing regtest escrow database
is pinned at the first start), and the escrow user's `rpcwhitelist` gains `getnetworkinfo`
([operator guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)); on regtest the app does not call it, so the old
whitelist keeps working, but add it. No action is needed otherwise.

## 34. Bitcoin escrow refuses a pooled RPC user that can read the arbiter wallet (A-440)

No migration. Only with `ESCROW_BTC=on` and `BITCOIN_RPC_URL` set: at startup and on each retry escrow calls
`listdescriptors` (private=false), `gethdkeys` (private=false) and `walletprocesspsbt` (an empty PSBT, sign=false) on the
arbiter wallet's path as the pooled RPC user, and starts only when each call is refused with HTTP 403 or the pooled node
has no such wallet (another node); while escrow is on, the escrow watcher repeats the check once a day and disables
escrow if a call is no longer refused (QA4W3-1, QA4W3-8). A pooled user without an `rpcwhitelist`
line on the escrow node (unrestricted under `rpcwhitelistdefault=0`, as the guide used to leave it) could export the
market's escrow key, so escrow now stays disabled with "escrow: Bitcoin escrow disabled: the pooled Bitcoin RPC user
(BITCOIN_RPC_URL) was not refused listdescriptors, gethdkeys or walletprocesspsbt on the arbiter wallet, …" (with the
method that answered and its class, such as `(gethdkeys answered)`) and a line on `/admin`. Before upgrading,
give that user the pooled whitelist line of the [operator guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only)
(*The pooled RPC user*) and restart bitcoind, or move escrow to a node of its own. The guide and the
[runbook](docs/testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) now also describe the
arbiter and watch-only wallet backup and restore. Pooled payments are unaffected.

## 35. Monero daemon sync guard, Monero escrow on stagenet and its network pin (A-449)

No migration. With `MONERO_RPC_URL` set, the pooled Monero watcher now also skips its pass while the daemon reports
`synchronized` false, has no peers (`offline`, or no connections on an unrestricted RPC) or its top block is older than
the new optional key `MONERO_MAX_TIP_AGE` (default `1h`, 20m to 24h; an invalid value stops startup): before, only a
`target_height` above the height counted, so a daemon that lost its peers looked synced and an order paid while it was
cut off could be cancelled for non-payment. The admin page's last error then reads "Node is not synced (…)". The app now
also calls `get_last_block_header` on the daemon; it is on monerod's restricted RPC, so the bundled `monero` service
answers it. A restricted RPC (the bundled service) hides the connection counts, so there only `offline` and the tip's age
are read. A market whose daemon is synced and receiving blocks sees no change. What this leaves (QA4W3-5): on a
restricted RPC `synchronized` stays true once set, `target_height` reads 0 and `offline` means only `--offline`, so a
daemon cut off from its peers looks synced until its top block is older than `MONERO_MAX_TIP_AGE` (default 1 hour, at
least 20 minutes). A deposit made in that window is not seen, and an order whose payment window ends in it can be
cancelled as unpaid; once the daemon sees the deposit it is recorded after the order closed: flagged for payment review
and returned by hand on a pooled order, returned to the buyer's refund address by a stray refund on a Monero escrow order.
Set a lower `MONERO_MAX_TIP_AGE` (down to `20m`) to narrow the window. A top block the daemon cannot report
(`get_last_block_header` fails) counts as not synced on the pooled check and skips the Monero escrow pass.

With `ESCROW_XMR=on`: Monero escrow also accepts a stagenet daemon (the escrow wallet-rpc then runs with `--stagenet`),
the accepted address forms follow the daemon's network, the height check (below 1,000,000) is gone, and the app pins the
daemon's network in `settings` (`escrow_xmr_network`). An existing fakechain escrow database is pinned `fakechain` at the
first start; pointed at a stagenet daemon instead it stays disabled and changes nothing, with a line under *Payment
providers* naming both networks: a database never changes network once it holds a Monero escrow or escrow wallet. The
startup line is now `escrow: Monero escrow on (fakechain)` (or `(stagenet)`) instead of `… on (local fakechain only)`; a
log filter on the old text needs updating. The Monero escrow watcher skips its pass while its daemon is not synced (as
above; a fakechain is exempt from the peer and tip-age checks)
([operator guide](docs/operator-guide.md#monero-escrow-fakechain-or-stagenet)). With the pooled Monero wallet configured
too (`MONERO_WALLET_RPC_URL`), Monero escrow now also waits until the pooled check has passed once, logging "escrow:
Monero escrow waiting: the pooled Monero wallet (MONERO_WALLET_RPC_URL) has not passed a check yet, …" (QA4W3-2): only
then is the pooled wallet's address known, and an escrow wallet-rpc whose open wallet has that address is the pooled one
under another URL, which is refused. No action is needed otherwise.

`compose.nodes.yaml` gains an optional `monero-escrow-wallet` service (Monero escrow's own stagenet wallet-rpc on the
bundled `monero` daemon: profile `monero-escrow-wallet`, volume `monero_escrow_wallet`, login password
`ESCROW_XMR_WALLET_RPC_PASSWORD`), and a new overlay `compose.escrow-xmr.yaml` mounts the seed key file
`escrow-xmr-seed-key.asc` into the app. Nothing starts unless the profile is in `COMPOSE_PROFILES`; a new install gets
them from `./scripts/install.sh --monero-escrow yes` (or its question), and an existing one can add them by hand as the
operator guide describes. `upgrade` derives the profile from `ESCROW_XMR_WALLET_RPC_URL` and the overlay from
`ESCROW_XMR_SEED_KEY` only when `.env` has no `COMPOSE_PROFILES` or `COMPOSE_FILE` line.

Compose now refuses a blank wallet-rpc password (A-466): with the `monero-wallet` profile active and
`MONERO_WALLET_RPC_PASSWORD` blank or missing, every `docker compose` command stops with `env file
…/MONERO_WALLET_RPC_PASSWORD-is-blank not found`, and likewise for `monero-escrow-wallet` and
`ESCROW_XMR_WALLET_RPC_PASSWORD`. Before, the bundled `monero-wallet` service started with the login `marketplace:` (an
empty password). The installer generates the password, so mainly a hand-made `.env` is affected: before
upgrading, set it (`openssl rand -hex 24`) in `.env` and as the password in `MONERO_WALLET_RPC_URL`
(`http://marketplace:PASSWORD@monero-wallet:18083`). `./scripts/install.sh check` names the missing key. An install
without these profiles needs neither key.

## 36. Bitcoin escrow broadcast honesty and the Tor-mode gate (A-441)

No migration, no new key. Only with `ESCROW_BTC=on`. A settlement recorded broadcast whose transaction the escrow
watch-only wallet has not seen 6 blocks later now tells staff once ("Queued or stuck: not seen by the network", an
order note and a "Payment review needed" notification) and is listed on `/admin` until the wallet sees it. With
`APP_MODE=tor`, escrow settlements are now broadcast only while the escrow node's `getnetworkinfo` shows a proxy for
IPv4 and IPv6 on Bitcoin Core 31.1 or later; otherwise they wait, with a line on `/admin`. A Tor-mode market using escrow
therefore needs `getnetworkinfo` on the escrow user's `rpcwhitelist` (it is called on regtest too in Tor mode) and the
escrow node started with `-proxy=<Tor SOCKS address>`, never `-connect` or `-onlynet`
([operator guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only), *Tor mode*). Clearnet mode and pooled payouts
are unchanged.

## 37. Escrow-only payment intake and the readiness panel (A-444)

No migration, no new key, nothing changes on upgrade. The per-currency intake setting (`payment_intake_BTC`,
`payment_intake_XMR` in `settings`, migration 052) takes a third value, `escrow`: new orders in that coin can only use
escrow checkout, and the pooled wallet issues no new payment address. The admin form under *New payment addresses* now
offers **On**, **Escrow only** and **Paused** per coin (password, audited, reversible), and offers escrow only and
paused even when that coin has no pooled wallet configured (On still needs one). A new *Escrow-only readiness* panel
below it counts and lists the active listings that cannot use escrow and the vendors with no active Bitcoin escrow key.
How to cut over and back: [operator guide](docs/operator-guide.md#cutting-over-to-escrow-only).

Leave the pooled wallet configured after a cutover until the pooled orders still open are settled: the pooled watcher
runs only while the provider is configured.

Rolling back the binary while a coin is set to escrow only: a build that knows this database's migrations but predates
A-444 refuses every intake value other than `true` (its `lockPaymentIntake` compares the value with `true`, and its
escrow checkouts call the same check), so it treats that coin as **paused for pooled requests and escrow checkouts
alike**. Its admin page still reads "Enabled when wallet is available" (with a *Pause* button when that coin's pooled
wallet is configured) and its order page still offers *Request payment address*, which is refused with the paused
message. When that coin's pooled wallet is configured, set the coin back to **On** before rolling back (this build's
form), or with the older build's form afterwards.

When that coin has **no pooled wallet configured**, neither form can do it: the older build shows its intake form only
for a configured provider and its action refuses one that is not, and this build refuses **On** without a pooled
wallet. Escrow checkouts in that coin then stay refused on the older build until the setting reads `true`, which, with
no pooled wallet, admits escrow checkouts and still issues no pooled address. Run this once per escrow-only coin
(`BTC` or `XMR`) after stopping this build and before starting the older one:

```sh
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt \
  -c "UPDATE settings SET value='true' WHERE key='payment_intake_BTC' AND value='escrow'"
```

For Monero use `payment_intake_XMR`. It prints `UPDATE 1` for a coin that was escrow only and `UPDATE 0` otherwise
(`settings` has two columns, `key` and `value`). Coming back to this build later, set the coin to **Escrow only** again with its form. A release older than
this database's migrations does not start against it at all ([Rolling back](#5-rolling-back)).

## 38. Bitcoin escrow: a payment to the arbiter wallet no longer disables escrow (A-471)

No migration, no new key. Only with `ESCROW_BTC=on`. **Action: add `listsinceblock` to the escrow user's
`rpcwhitelist`** (the line in the [operator guide](docs/operator-guide.md#bitcoin-escrow-test-networks-only) ends
`…,gettransaction,listtransactions,listsinceblock`) and restart bitcoind; without it escrow stays disabled with
"escrow: Bitcoin escrow disabled: the arbiter wallet's transactions could not be read …" and a line on `/admin`. It is
read-only and exports nothing.

The start check used to refuse an arbiter wallet with any transaction (`getwalletinfo` `txcount` above 0), but every
escrow order's descriptor shows the market's child key, whose address the arbiter wallet owns: any party could pay it a
few satoshis and escrow stayed disabled from the next restart on. Now only a spend from the arbiter wallet (an entry of
category `send`, which only its keys can make; `listsinceblock` with `include_change`, so a spend to the wallet's own
change addresses counts too) counts as using it as an ordinary wallet; coins paid to it are ignored and stay there. A
market whose escrow was disabled this way ("escrow: Bitcoin escrow disabled: the arbiter wallet must be used for the
escrow key only … (getwalletinfo not-dedicated)") starts escrow at its first start after the upgrade, unless the wallet
has spent; a spend is now logged as `(listsinceblock not-dedicated)`.

Reading every entry on every start let mass payments to that address keep escrow off instead (QA4W4A-1: 100,000 entries
outlasted startup). So the first passing check reads the whole wallet once, in one call, and records a checkpoint, the
`settings` key `escrow_btc_arbiter_checked` (the block `listsinceblock` reported, for the wallet's name and
`getwalletinfo` birthtime); later checks read only newer entries. A first read longer than startup allows leaves escrow
off with "escrow: Bitcoin escrow off while the arbiter wallet is checked: …" until the escrow watcher finishes it. A
different arbiter wallet, or one rebuilt with `importdescriptors`, is read whole again.

One case the checkpoint misses: `importdescriptors` into the same arbiter wallet keeps its name and birthtime, so a
descriptor whose keys have already spent brings in sends at or below the checkpoint block, which later checks do not
read. **After importing any descriptor into the arbiter wallet**, delete the checkpoint so that the next start reads the
whole wallet once, then restart the app:

```sh
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt \
  -c "DELETE FROM settings WHERE key='escrow_btc_arbiter_checked'"
```

It prints `DELETE 1`, or `DELETE 0` when there was no checkpoint. A stored checkpoint block that is not a block hash
(64 hex characters) is treated as no checkpoint, and so is a block the node refuses as malformed (`-8`) or does not know
(`-5`). The read refuses an answer with any one JSON token (a string, say) longer than 64 KiB as a read failure
(`bad-reply`); Core writes none that long. It reads each entry key by key, so an entry of any size passes (QA4W6-1: a
payment to the market child's address that its payer fee-bumped 1000 times makes Core list entries of about 67 KB,
which builds before this fix refused at every check, keeping escrow off for good; after upgrading, the next check passes
with no step), and the answer as a whole is limited only by the read's timeout. While such replacements are unconfirmed
the answer grows with the square of their number (1100: 82 MB, read in about 2 s; 3000: 605 MB, 16 s, and about
1.5 GB of the node's memory per read); once one is mined, Core lists the others no more.

A-479 (no step, no new key): after a read of the arbiter wallet that took 5 s or more or timed out, pass or failure, the
escrow watcher's next read waits 1 minute, doubling after each such read up to 1 hour; meanwhile a slow pass counts as a
pass for the other start checks. A quicker read ends the waits, a quick failure is read again every poll as before, and
restarting the app reads at once (the operator guide's *Reads are paced*).

What "never spent" cannot tell apart: `rpcwhitelist` allows methods, not wallets, so where the pooled payments share the
escrow node, a pooled user allowed `sendtoaddress` can spend coins paid to the arbiter wallet by naming its path, and
escrow then stays off from the next check (no key is exported). A node of escrow's own rules that out.

## 39. Bitcoin escrow key layout (migration 088, A-455)

Migration 088 runs at startup. It adds `user_escrow_keys.layout`, `core` (a Bitcoin Core wallet, as every key so far)
or `per_order` (a per-order wallet: Electrum with a BIP39 seed, or Sparrow), with `core` as the default, so every
existing key stays a Core key and stays active. It also adds two checks on `escrow_pubkeys`: a derivation suffix of
three or more steps (a per-order wallet's key, `/<i>/0/0`) may not start with `/0` or `/1`, for any party, and a
buyer's or vendor's may not use the reserved practice index `/2147483647`. Shape (a)'s two-step suffixes (`/0/<i>`) are
untouched. Both checks are added `NOT VALID` and then validated in the same migration; no release before this one
writes a suffix of three steps, so validation finds nothing to refuse. With `ESCROW_BTC=on`, the escrow key page then
lets a user register a per-order wallet key (the BIP48 multisig account key, `m/48h/1h/0h/2h`) and sign its practice
transaction in a per-order practice wallet, and listings and vendor profiles show the vendor's key type. Escrows with
per-order wallets are not built yet: a checkout where either party's active key is a per-order wallet key is refused
with nothing written, and while BTC intake is escrow only (section 37) such a vendor's listings are not available for
purchase in BTC and the readiness panel lists the vendor ([per-order escrow](docs/escrow.md#per-order-key-layout-registration-and-practice-a-455-shape-b)).
No action is needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5): the earlier release refuses to start on a database that records migration 088.

## 40. Bitcoin escrow on signet and testnet4 (A-443)

No migration, no new key. Only with `ESCROW_BTC=on`. Escrow now starts against a node on `signet` or `testnet4` as well
as `regtest`; mainnet and testnet3 are still refused, and `BITCOIN_CHAIN`, when set beside `ESCROW_BTC=on`, may now be
`signet` or `testnet4` as well as `regtest`. A regtest market sees no change in behaviour: its chain pin stays
`regtest`, and a database that holds escrows never moves to another chain (start a signet or testnet4 market on a new
database). The startup refusals now name the chains escrow runs on ("refusing to start: ESCROW_BTC=on runs only on
regtest, signet or testnet4 …") and the log line reads "escrow: Bitcoin escrow on (test networks only)" where they said
regtest only; a log filter matching the old text needs updating. The operator guide's
section is now [Bitcoin escrow (test networks only)](docs/operator-guide.md#bitcoin-escrow-test-networks-only), and the
[runbook](docs/testnet-runbook.md#11-bitcoin-escrow-on-signet-or-testnet4) lists what a signet or testnet4 market needs
first. An escrow refund, claim, release or fee address given in capitals is now stored in lower case by the chain's
prefix (`TB1…` on signet and testnet4, as `BCRT1…` already was on regtest). The escrow pages name the chain. **No run on
public signet or testnet4 is recorded**: the real-chain legs ran on regtest and on a private signet
(`scripts/test-escrow-regtest.sh --private-signet`).

## 41. Monero escrow: refunds at or below the network fee end too few (A-467)

No migration, no new key. Only with `ESCROW_XMR=on`. A Monero refund or stray refund whose coins are at or below the
network fee is refused by `monero-wallet-rpc` (`transfer` answers -17, -16 or -46); the market now reads the fee the
wallet would take (`get_default_fee_priority` and `estimate_tx_size_and_weight` on the escrow wallet-rpc,
`get_fee_estimate` on the escrow daemon, which monerod's restricted RPC serves; nothing is built or sent) and treats such
a refund as too few, as it already treated one the wallet built with less than 0.001 XMR left after the fee: the refund
ends, its deposits are left out and the stray refund returns them with a later deposit. Before, it waited for good and
staff were told of an "outputs mismatch" or a wallet that "keeps refusing" to build it. A refund waiting so when you
upgrade ends on the next pass; its earlier order note stays. If the fee estimate fails, that refund waits and is
flagged once if it fails again on the next pass; the wallet's other orders carry on. Releases are unchanged
([per-order escrow](docs/escrow.md)).

## 42. Database encoding must be UTF8 (A-481)

No migration, no new key. A database whose `SHOW server_encoding` is not `UTF8` (an external PostgreSQL created as
`SQL_ASCII` or `LATIN1`, for example by a C-locale `initdb` without `-E UTF8`) now stops startup, before the first
migration statement, with `the database encoding is ..., not UTF8` and nothing changed. Before, it ran with silent
differences: on `SQL_ASCII` the image description limit of 250 counted bytes (250 "é" was refused there and
accepted on UTF8), and case folding and sorting follow the database's encoding and locale. The bundled Compose
PostgreSQL is initialised as UTF8 and is not affected, and a database already UTF8 needs nothing. Check yours with
`psql "$DATABASE_URL" -c 'SHOW server_encoding'` before you upgrade. The app also now sends `client_encoding=UTF8` on
every connection.

To move a non-UTF8 database (the encoding cannot be changed in place; keep the old database until the new one has
been checked):

1. Stop the market (`docker compose stop app`) and take the encrypted backup with `./scripts/backup.sh`; for an
   external database that is `BACKUP_DATABASE_URL=... AGE_RECIPIENT=... ./scripts/backup.sh`.
2. Create a UTF8 database next to it, from `psql` as a role with `CREATEDB`:
   `CREATE DATABASE opsecmkt_utf8 ENCODING 'UTF8' TEMPLATE template0;`
3. Restore the backup into that empty database with `RESTORE_DATABASE_URL=... AGE_IDENTITY=... ./scripts/restore.sh
   backups/market.dump.age` (the [restore procedure](docs/operator-guide.md#backups-and-recovery), including its
   payout recovery gate and reconciliation, applies unchanged). `pg_restore` converts from the dump's encoding; it
   stops, changing nothing, at text that is not valid in UTF8 and names the table. The app wrote UTF-8, but a
   `LATIN1` database may hold non-ASCII text stored double-encoded: look at accounts and listings with accented
   characters after the restore.
4. Point `DATABASE_URL` (and `BACKUP_DATABASE_URL`) at the new database and start the app.

Not changed: the signed audit export and every merged migration.

## 43. Monero escrow: refused deposits are recorded and cleared by staff (migration 090, A-480)

No new key. Migration 090 adds the table `xmr_escrow_collisions`. A Monero escrow deposit the database refuses as
another order's escrow account (`payments_xmr_escrow_idx`) is still never written to the ledger, and the order note and
the one "Payment review needed" notification to staff are unchanged; the watcher now also records the deposit (order,
wallet, account, transaction ID, when noted), once per order and transaction, and the record, not the note text, keeps
the note and notification to once. While a record is open, moderators and administrators who are not parties may read
the order, the moderation desk lists it under *Escrow alerts*, and the order page shows them a *Refused Monero
deposits* section where one of them records what was done (see [operator guide](docs/operator-guide.md), moderation);
that clears the alert. The parties see no change. The desk's *Escrow alerts* are now ordered by when each order's open
alert opened rather than by the order's age (QA5W2-3).

Notes written before the upgrade are backfilled: each system order note of the watcher's fixed form ("… Monero escrow
deposit `<20 hex digits>`… to wallet … account … was not recorded: the database refused it as another order's escrow
account. Moderator review required.") becomes one **open** record, noted at the note's time. The note named only the
first 20 characters of the transaction ID, so that is all the record holds (`txid_truncated`); the order page shows it
with an ellipsis. If the watcher meets the same deposit again it completes the transaction ID and writes nothing else
(no second note or notification). So after the upgrade any such order already noted appears under *Escrow alerts* until
staff record what was done about it, even if it was dealt with before; record that. Check for them with
`SELECT order_id, txid FROM xmr_escrow_collisions WHERE disposed_at IS NULL;`. A market that never ran with `ESCROW_XMR=on` has none.

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
| `MONERO_WALLET_RPC_PASSWORD` | For the local `monero-wallet` service only: `openssl rand -hex 24` output, the same value as the password in `MONERO_WALLET_RPC_URL`. Compose refuses the `monero-wallet` profile without it (section 35). |
| `PAYMENT_CONFIRMATIONS_BTC` / `PAYMENT_CONFIRMATIONS_XMR` | Defaults 3 and 10. |
| `PAYMENT_POLL_INTERVAL` / `PAYMENT_EXPIRY` | Defaults `30s` and `24h`. |
| `BITCOIN_MAX_TIP_AGE` | Default `2h` (30m to 24h): a public-network Bitcoin node whose tip is older counts as not synced (section 33). |
| `MONERO_MAX_TIP_AGE` | Default `1h` (20m to 24h): a public-network Monero daemon whose top block is older counts as not synced (section 35). |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` | Only for the `monero-escrow-wallet` service (Monero escrow on stagenet): `openssl rand -hex 24` output, the same value as the password in `ESCROW_XMR_WALLET_RPC_URL` (section 35). Blank, Compose refuses the `monero-escrow-wallet` profile. |
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
