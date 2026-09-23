# Installer, backup, restore and upgrade regression tests

Run the URL/credential tests without any external service or third-party Python package:

```sh
python3 -m unittest discover -s tests -p 'test_*.py'
```

The installer tests run the real installation script in temporary repositories with Docker and OpenSSL stubs. They cover internal/external databases, clearnet/Tor exposure, direct-egress overlays, local node profiles, protected configuration permissions, existing-file preservation, input rejection, and Compose validation failures that must not start services. They never read the user’s `.env`, invoke real Docker, or provision infrastructure.

The connection tests mock process execution and check decoded credentials, IPv6 hosts, ports, supported TLS/query settings, rejected target overrides and null bytes, removal of inherited libpq settings, and errors that do not expose connection secrets. They also verify that PostgreSQL client arguments contain no URL or password.

Run the encrypted round-trip integration test against a **disposable PostgreSQL server**, using a role with `CREATEDB` permission:

```sh
TEST_DATABASE_URL='postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable' \
  ./scripts/test-backup-restore.sh
```

Required executables are Bash, Python 3, Go (to build the server from this checkout), `psql`, `pg_dump`, `pg_restore`, `age`, and `age-keygen`. Use PostgreSQL clients matching the server major version (CI uses PostgreSQL 17). On Debian with the PostgreSQL Apt repository configured, install `postgresql-client-17`, `python3`, and `age`. On macOS, Homebrew provides `postgresql@17`, `python`, and `age`; include the PostgreSQL client's `bin` directory in `PATH`.

The script creates four uniquely named `opsecmkt_ops_*` databases, generates temporary encryption identities, and verifies:

- Actual encrypted dump and restore with Unicode text and primary keys preserved.
- An age-encrypted output file with mode `0600`.
- Refusal to overwrite an existing backup, with its contents unchanged.
- A wrong decryption identity fails without creating tables.
- A schema collision rolls back the entire restore, including a preceding successful table creation, while preserving existing rows.
- After a restore, payouts that were `pending` or `sending` in the dump are `held` with a "Restored from backup" error; other payout states are untouched.
- The real application schema: the server built from this checkout migrates an empty database, a `pending` release payout and a `sent` refund payout are added, and the encrypted dump is restored into another empty database. The restored payout is `held` (the sent one and the source database are unchanged), `schema_migrations` is identical, and the server then starts on the restored database, answers `/healthz`, applies no migration again and leaves the held payout alone.

Only the newly generated database names are used as backup/restore targets. The server runs on a free loopback port with an empty environment apart from the scratch database URL. An exit trap stops it, drops every scratch database and removes temporary keys, dumps, and logs. Use a dedicated local/CI server: the test role necessarily has database-creation permissions, and terminating the script with `SIGKILL` can prevent cleanup. The original database named in `TEST_DATABASE_URL` is used only as a connection for creating and dropping the scratch databases.

## Upgrade from v0.1.0-alpha.1

```sh
./scripts/test-upgrade.sh
```

Requires Docker, Go, Git with the `v0.1.0-alpha.1` tag present (`git fetch origin tag v0.1.0-alpha.1` in a shallow clone), and Python 3. `UPGRADE_FROM_REF` selects another starting tag. The script:

1. Starts a disposable `postgres:17-alpine` container on a random loopback port.
2. Builds the alpha.1 server from `git archive` of the tag, runs it once and populates it through alpha.1's own forms: first-admin setup, a vendor (role assigned by the admin) and two buyers, a listing, BTC and XMR order drafts carrying the old status text `Draft — payment unavailable`, and an armored message.
3. Stops it and starts the server built from this checkout against the same database, then checks: `/healthz` answers `ok`; `schema_migrations` lists exactly the files in `internal/market/migrations` by version and name; every order keeps its id, buyer, currency and amount, is in state `draft`, and the `status` column and old unique constraint are gone; the partial unique index `orders_one_draft` refuses a second draft for the same buyer, product and currency but allows one once the first leaves `draft`; messages, users and settings are unchanged. With the CAPTCHA switched off in settings (migration 010 turns it on), the alpha.1 buyer signs in with the alpha.1 password, sees the migrated order as `Draft — unfunded`, and ordering the same product and currency again returns that draft rather than creating another.
4. Starts the current server a second time and checks that no migration is applied again.

Servers run with an empty environment apart from their database URL, setup token and loopback address, so local payment or signing settings cannot leak in. On failure, the server logs are printed. The container and temporary directory are always removed.

This automates database recovery and upgrade checks; it does not test backup scheduling, off-host storage retention, Tor identity recovery, or a full production disaster-recovery procedure.
