# Upgrading from v0.1.0-alpha.1

This release adds test-network payments, signed audit exports and several database migrations. Read this
whole page before pulling the new code onto a running v0.1.0-alpha.1 deployment.

## 1. Take a backup and record what you run

Migrations are **one-way**. There are no down-migrations, and the alpha.1 code must not run against a
migrated database (for example, migration 001 replaces `orders.status` with `orders.state` and drops the old
unique constraint). `compose.yaml` builds the app from the checkout (`build: .`), so there is no saved image
to switch back to: rolling back means restoring this backup and rebuilding the old revision. Before
upgrading, from the directory you deploy from:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/pre-upgrade.dump.age
cp -p .env .env.pre-upgrade   # the configuration the old revision runs with (keep it private)
git describe --tags --always  # note the revision you run today, e.g. v0.1.0-alpha.1
```

Keep the backup, `.env.pre-upgrade` and the revision until you have run the new version for a while. See
[Rolling back](#5-rolling-back).

## 2. Add the new keys to your existing `.env`

`scripts/install.sh` refuses to touch an existing `.env`, so it will not add them for you. Add the lines
below with your editor (values in single quotes, as the installer writes them). Blank values keep a feature
off.

| Key | What to put there |
| --- | --- |
| `AUDIT_SIGNING_KEY` | `openssl rand -hex 32` output. Enables signed audit exports; back it up with `.env`. |
| `BITCOIN_CHAIN` | Leave blank to accept whichever test chain the node reports, or `testnet4` / `signet` / `regtest`. Mainnet is always refused. See step 3 before setting it for a local node. |
| `BITCOIN_WALLET` | Wallet name, default `opsecmkt`. |
| `MONERO_NETWORK` | Blank, `stagenet` or `testnet`. |
| `MONERO_WALLET_RPC_URL` | monero-wallet-rpc URL with credentials, for example `http://marketplace:PASSWORD@monero-wallet:18083`. Blank keeps Monero payments off. A daemon URL alone (`MONERO_RPC_URL`, which alpha.1 wrote) does **not** enable them. |
| `MONERO_WALLET_RPC_PASSWORD` | For the local `monero-wallet` service only: `openssl rand -hex 24` output, the same value as the password in `MONERO_WALLET_RPC_URL`. The service refuses to start without it. |
| `PAYMENT_CONFIRMATIONS_BTC` / `PAYMENT_CONFIRMATIONS_XMR` | Defaults 3 and 10. |
| `PAYMENT_POLL_INTERVAL` / `PAYMENT_EXPIRY` | Defaults `30s` and `24h`. |

For a local Monero node, also add `monero-wallet` to `COMPOSE_PROFILES` (for example
`COMPOSE_PROFILES='internal-db,monero,monero-wallet'`).

### Replace a placeholder `SETUP_TOKEN`

This release refuses to start when `SETUP_TOKEN` is not a random value: the old `.env.example` placeholder
(`REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS`), anything containing `REPLACE`, `CHANGEME`, `CHANGE_ME` or
`PLACEHOLDER`, fewer than 32 characters, fewer than 8 distinct characters, or a short pattern repeated. The
log then says `SETUP_TOKEN is still a placeholder value` or `SETUP_TOKEN is not random`. `scripts/install.sh`
always generated a random 64-character hex token, so only hand-written `.env` files are affected.

The placeholder is public, and `SETUP_TOKEN` authorizes `/setup` and derives the CSRF/CAPTCHA key and the key
that encrypts TOTP secrets. An instance that ran with it must rotate it: put the output of
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
  The administrator's own account, or an instance where no administrator can sign in, needs the operator
  reset in the database (internal-db shown; replace `THE_HANDLE`):

  ```sh
  docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -v handle=THE_HANDLE -U opsecmkt -d opsecmkt <<'SQL'
  BEGIN;
  UPDATE users SET totp_enabled=false,totp_secret='',totp_pending='',totp_last_step=0,recovery_reveal='',recovery_reveal_until=NULL
    WHERE handle=:'handle';
  DELETE FROM recovery_codes WHERE user_id=(SELECT id FROM users WHERE handle=:'handle');
  INSERT INTO audit_events(user_id,action) SELECT id,'TOTP reset by the operator after SETUP_TOKEN rotation' FROM users WHERE handle=:'handle';
  COMMIT;
  SQL
  ```

  Check that the `UPDATE` reports one row; the user then signs in with the password alone and enrols again.

A restored database needs the `SETUP_TOKEN` that was in use when its backup was taken for its TOTP secrets
to be readable. Keep the rotated value, not the placeholder, in any `.env` you restore later.

## 3. `BITCOIN_RPC_URL` now turns payments on

In alpha.1 the installer wrote `BITCOIN_RPC_URL` but nothing read it. Now a non-empty value enables Bitcoin
test-network payments. The app never starts against a mainnet node: startup exits with
`refusing to start: bitcoin chain "main" ...`. A node that is unreachable, still syncing or has no wallet
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

```sh
git pull   # or check out the release tag
docker compose config --quiet   # fails loudly on a malformed .env
docker compose up -d --build
docker compose logs -f app      # migrations run at startup under an advisory lock
```

Then open the admin page. **Payment providers** shows each currency as Enabled, Unavailable (with the
error; retried every poll), Refused (not a test network) or Disabled. The container stop grace period is now
60 seconds so a redeploy does not interrupt a payout that is being broadcast.

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

   With an external database, create an empty database on that server and restore with
   `RESTORE_DATABASE_URL` instead (see [backups and recovery](docs/operator-guide.md#backups-and-recovery)).
   The restore pauses payouts and holds queued ones (see step 6); alpha.1 has no payment processing.

3. Put back the old configuration and point it at the restored database:

   ```sh
   cp -p .env.pre-upgrade .env
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
   docker compose up -d --build
   docker compose logs -f app
   ```

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
pending, sending, blocked or held payout, and marks every failed payout as possibly sent after the backup (it
may have been requeued and sent since). Follow the [reconciliation procedure](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts),
including orders paid out after the backup that have no payout row in it, before explicitly clearing the
gate. Restore with the application stopped and keep the site cut off from users until reconciliation is
complete; the procedure says when the app runs for you alone and when it must be stopped.
If the script reports recovery protection failed, do not start the application. Apply the protections to
the restored application database in one transaction first (older databases without a `payouts` table need
only the settings update; omit the `send_ambiguous` line if their `payouts` table has no such column, as
migration 053 then marks their failed payouts ambiguous itself):

```sql
BEGIN;
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
UPDATE payouts SET state='held', updated=now(),
  error='Restored from backup: verify in the wallet before releasing; this payout may already have been sent.'
WHERE state IN ('pending','sending','blocked','held');
UPDATE payouts SET send_ambiguous=true WHERE state='failed';
UPDATE payouts SET updated=now(),
  error='Restored from backup: verify in the wallet before requeueing; this payout may have been requeued and sent after the backup. Last error: ' || error
WHERE state='failed';
COMMIT;
```

The payout gate is enforced by this release. An older application image may not understand it; keep wallet
RPC configuration disabled when inspecting a restored database with older code. Alpha.1 has no payment
processing, but any other older release requires its own recovery review.
