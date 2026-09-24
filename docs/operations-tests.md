# Installer, backup, restore and upgrade regression tests

Run the URL/credential tests without any external service or third-party Python package:

```sh
python3 -m unittest discover -s tests -p 'test_*.py'
```

The installer tests cover the noninteractive `--local` shortcut, inherited-wallet isolation, persisted custom ports, readiness failure, HTTPS handoff, and existing-file preservation. They run the real installation script in temporary repositories with Docker and OpenSSL stubs. They cover internal/external databases, clearnet/Tor exposure, direct-egress overlays, local node profiles, protected configuration permissions, existing-file preservation, input rejection, and Compose validation failures that must not start services. They never read the user’s `.env`, invoke real Docker, or provision infrastructure.

The connection tests mock process execution and check decoded credentials, IPv6 hosts, ports, supported TLS/query settings, rejected target overrides and null bytes, removal of inherited libpq settings, and errors that do not expose connection secrets. They also verify that PostgreSQL client arguments contain no URL or password.

Run the encrypted round-trip integration test against a **disposable PostgreSQL server**, using a role with `CREATEDB` permission:

```sh
TEST_DATABASE_URL='postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable' \
  ./scripts/test-backup-restore.sh
```

Required executables are Bash, Python 3, Go (to build the server from this checkout), `psql`, `pg_dump`, `pg_restore`, `age`, and `age-keygen`. Use PostgreSQL clients matching the server major version (CI uses PostgreSQL 17). On Debian with the PostgreSQL Apt repository configured, install `postgresql-client-17`, `python3`, and `age`. On macOS, Homebrew provides `postgresql@17`, `python`, and `age`; include the PostgreSQL client's `bin` directory in `PATH`.

The script creates five uniquely named `opsecmkt_ops_*` databases, generates temporary encryption identities, and verifies:

- Actual encrypted dump and restore with Unicode text and primary keys preserved; the backup's success line names the `BACKUP_DATABASE_URL` database without printing the URL, and the restore says to point `BACKUP_DATABASE_URL` at the restored database.
- An age-encrypted output file with mode `0600`.
- Refusal to overwrite an existing backup, with its contents unchanged.
- A wrong decryption identity fails without creating tables.
- A schema collision rolls back the entire restore, including a preceding successful table creation, while preserving existing rows.
- After a restore, payouts that were `pending`, `sending`, `blocked` or `held` in the dump receive manual recovery holds with a "Restored from backup" error; a `failed` payout stays failed with its error kept behind the same marker (this synthetic dump has no `send_ambiguous` column, which the restore skips), and sent payouts are untouched. The script reports 4 held and 1 marked failed payout.
- A legacy settings-only dump (no payouts table) receives the recovery gate even when its saved value was false.
- The real application schema: the server built from this checkout migrates an empty database, a `pending` release payout, a `sent` refund payout, a `failed` release payout with a definite rejection (`send_ambiguous=false`) and a refund `held` for an account suspension ("Suspended account: ...") are added, and the encrypted dump is restored into another empty database. The restored pending payout is `held`, the suspension hold keeps its text behind the restore marker (so releasing it still asks for the payout address check), the failed one stays `failed` with the restore marker and `send_ambiguous=true` (possibly requeued and sent after the backup), and `payments_recovery_required=true` pauses all outbound payouts (the sent one and the source database are unchanged); the script reports 2 held and 1 marked failed payout and adds exactly one audit row with no user naming those counts, leaving the audit row from before the backup unchanged; `schema_migrations` is identical, and the server then starts on the restored database, answers `/healthz`, applies no migration again and leaves the held payout alone.
- A restore not done by `scripts/restore.sh`: the same application database is copied with plain `pg_dump`/`pg_restore` (standing in for a volume snapshot or point-in-time recovery). The copy has no recovery gate and its queued payout is `pending`, so the application would send it; the UPGRADING.md section 6 SQL, run verbatim, prints `INSERT 0 1`, `UPDATE 2`, `UPDATE 1`, `UPDATE 1`, `INSERT 0 1` and `COMMIT` and leaves the gate, every payout's state, restore marker and `send_ambiguous`, the suspension hold and the failed payout's error exactly as `scripts/restore.sh` does, plus one audit row with no user ("Payout recovery gate set manually ...").
- Payouts sent after the backup ([runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)): the dump also holds a funded `delivered` order and a funded `disputed` order with an open dispute and a locked transfer, neither with a payout row. The test extracts the runbook's `find-order`, `record-payout` and `clear-gate` SQL and the `restore-protection` SQL of [UPGRADING.md section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) (the blocks after `<!-- runbook-sql: NAME -->`) and runs them verbatim with `psql`. Before reconciliation, completing the delivered order would move it and queue a release (the transition's compare-and-set and `enqueuePayout`'s insert, run in a rolled-back transaction). The find query names the order from the amount; the check prints one row and writes nothing; a non-administrator handle, a kind that does not match the final state, an unreachable final state, a wrong amount, a malformed transaction ID, a blank address, an unknown order, an order that already has a payout and a transaction ID already recorded are each refused with nothing written. Recording writes one `sent` payout with the wallet transaction ID, moves the order to `completed` (and the disputed one to `resolved`, closing the dispute with the refund outcome), credits the counted deposits but not the locked transfer, and adds one order event by the administrator and one audit row each; earlier audit rows are unchanged. Completing the order again then changes nothing. The gate SQL refuses an unknown and a non-administrator handle ("Refused; nothing was changed: no administrator with that handle", rolled back: the gate stays set, no audit row); with the administrator's handle it clears the gate and adds one audit row by that administrator ("Payout recovery gate cleared by ops_admin ..."), and run again it is refused because the gate is not set; earlier audit rows are unchanged. After the server restarts there is still exactly one payout per order, and the restore's holds (including the suspension hold) are still held. This is database-level evidence: no wallet is involved and the completion is emulated in SQL, not driven through the web form.

Only the newly generated database names are used as backup/restore targets. The server runs on a free loopback port with an empty environment apart from the scratch database URL. An exit trap stops it, drops every scratch database and removes temporary keys, dumps, and logs. Use a dedicated local/CI server: the test role necessarily has database-creation permissions, and terminating the script with `SIGKILL` can prevent cleanup. The original database named in `TEST_DATABASE_URL` is used only as a connection for creating and dropping the scratch databases.

## Backup and restore through the internal-db Compose service

```sh
./scripts/test-internal-db-restore.sh
```

Requires Docker with Compose v2, `age`, `age-keygen` and Python 3; no host PostgreSQL tools or published
database port. The script copies `compose.yaml`, `compose.internal-db.yaml` and the backup/restore scripts
into a temporary directory with its own `.env` and a uniquely named Compose project, starts only the
`postgres:17-alpine` `db` service, checks that it publishes no port, and verifies:

- `scripts/backup.sh` without `BACKUP_DATABASE_URL` writes an age-encrypted `0600` dump through `docker compose exec -T db`.
- `scripts/restore.sh` refuses two destinations, no destination, an unsafe `RESTORE_INTERNAL_DATABASE` name and a wrong confirmation without creating a database; a wrong identity restores no tables.
- A side-by-side restore (`RESTORE_INTERNAL_DATABASE=opsecmkt_restored`) creates the new database, round-trips Unicode, holds `pending`, `sending`, `blocked` and `held` payouts, marks the `failed` one possibly sent (restore marker and `send_ambiguous=true`), sets `payments_recovery_required=true`, and leaves the live `opsecmkt` database unchanged.
- The side-by-side restore prints the database it restored into and the exact `DATABASE_URL` line for `.env`. After `DATABASE_URL` is switched to `opsecmkt_restored` and a row is written there, `scripts/backup.sh` names and dumps `opsecmkt_restored`; restoring that backup into a check database contains the row. Double-quoted, `export`-prefixed, unquoted-with-comment, percent-encoded-password, CRLF and repeated `DATABASE_URL` lines select the same database, which the test confirms is the one `docker compose config` resolves for the app; the URL and password are never printed.
- `scripts/backup.sh` refuses a `BACKUP_INTERNAL_DATABASE` that differs from the database in `DATABASE_URL`, and one set together with `BACKUP_DATABASE_URL`, without leaving a file; a matching one is accepted; when the name comes from Compose interpolation the script refuses to guess and uses `BACKUP_INTERNAL_DATABASE`.
- Restoring over the populated live database fails and rolls back completely, without applying the gate.
- After `docker compose down --volumes`, a fresh volume's empty `opsecmkt` database is restored into directly with the same gate and holds.
- With the `db` service stopped, the restore says how to start it.

The exit trap runs `docker compose down --volumes --remove-orphans` for the test project and removes the
temporary directory, keys and dumps. It never reads the checkout's `.env` or touches other projects.

## Real local installation and setup wizard

```sh
python3 scripts/test-install.py
```

Requires Docker Engine/Desktop with Compose v2, Python 3, Bash, OpenSSL and curl.
The script copies only deployment/build inputs into a temporary directory, runs
`./scripts/install.sh --local` with no answers, and starts a uniquely named real
Compose project with PostgreSQL 17 on a free loopback application port. It uses
HTTP form submissions and a cookie jar to verify the browser wizard: wrong-token
rejection, administrator creation, redirect to the branded admin onboarding,
setup lockdown, refusal to replace an existing `.env`, and authenticated admin
access after restarting the app. Browser rendering is covered separately by
Playwright.

The user's configuration and database are never used. Generated credentials stay
inside the protected temporary directory; logs do not print the setup token.
Commands have deadlines, and timeout/interruption cleanup stops process groups,
removes the test project's containers and volumes, and deletes its application
image and temporary files. Forced termination (`SIGKILL`) or an unavailable Docker
daemon can prevent cleanup; the unique project name is in the reported log path.
Evidence is written to `artifacts/install-test/<project>/`. CI runs this same
script as a required job and retains its logs and result for 14 days.

## Upgrade from v0.1.0-alpha.1

```sh
./scripts/test-upgrade.sh
```

Requires Docker, Go, Git with the `v0.1.0-alpha.1` tag present (`git fetch origin tag v0.1.0-alpha.1` in a shallow clone), and Python 3. `UPGRADE_FROM_REF` selects another starting tag. The script:

1. Starts a disposable `postgres:17-alpine` container on a random loopback port.
2. Builds the alpha.1 server from `git archive` of the tag, runs it once and populates it through alpha.1's own forms: first-admin setup, a vendor (role assigned by the admin) and two buyers, a listing, BTC and XMR order drafts carrying the old status text `Draft — payment unavailable`, and an armored message.
3. Stops it and starts the server built from this checkout against the same database, then checks: `/healthz` answers `ok`; `schema_migrations` lists exactly the files in `internal/market/migrations` by version and name; every order keeps its id, buyer, currency and amount, is in state `draft`, and the `status` column and old unique constraint are gone; the partial unique index `orders_one_draft` refuses a second draft for the same buyer, product and currency but allows one once the first leaves `draft`; messages, users and settings are unchanged. With the CAPTCHA switched off in settings (migration 010 turns it on), the alpha.1 buyer signs in with the alpha.1 password, sees the migrated order as `Draft — unfunded`, and ordering the same product and currency again returns that draft rather than creating another.
4. Starts the current server a second time and checks that no migration is applied again.
5. Rollback rehearsal: records a migration `999_from_a_newer_release` that this checkout does not include, as a newer release would, and checks that the current server exits non-zero within 30 seconds (it stops before listening), that its error names `999_from_a_newer_release` and points to `UPGRADING.md` "Rolling back", and that `schema_migrations` is unchanged. With that row removed again the server starts and answers `/healthz`.

Servers run with an empty environment apart from their database URL, setup token and loopback address, so local payment or signing settings cannot leak in. On failure, the server logs are printed. The container and temporary directory are always removed.

This automates database recovery and upgrade checks; it does not test backup scheduling, off-host storage retention, Tor identity recovery, or a full production disaster-recovery procedure.

## Real Bitcoin and Monero local chains

[Local-chain testing](local-chain-testing.md) documents signature verification,
`scripts/setup-local-chains.py`, real wallet funding and
`npm run test:e2e:chain`. These require operator-provided verified binaries and
are not part of the default CI simulator run. No production/mainnet wallet or
public-chain faucet is used.
