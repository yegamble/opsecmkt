# Installer, backup, and restore regression tests

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

Required executables are Bash, Python 3, `psql`, `pg_dump`, `pg_restore`, `age`, and `age-keygen`. Use PostgreSQL clients matching the server major version (CI uses PostgreSQL 17). On Debian with the PostgreSQL Apt repository configured, install `postgresql-client-17`, `python3`, and `age`. On macOS, Homebrew provides `postgresql@17`, `python`, and `age`; include the PostgreSQL client's `bin` directory in `PATH`.

The script creates two uniquely named `opsecmkt_ops_*` databases, generates temporary encryption identities, and verifies:

- Actual encrypted dump and restore with Unicode text and primary keys preserved.
- An age-encrypted output file with mode `0600`.
- Refusal to overwrite an existing backup, with its contents unchanged.
- A wrong decryption identity fails without creating tables.
- A schema collision rolls back the entire restore, including a preceding successful table creation, while preserving existing rows.
- After a restore, payouts that were `pending` or `sending` in the dump are `held` with a "Restored from backup" error; other payout states are untouched.

Only the newly generated database names are used as backup/restore targets. An exit trap drops both scratch databases and removes temporary keys, dumps, and logs. Use a dedicated local/CI server: the test role necessarily has database-creation permissions, and terminating the script with `SIGKILL` can prevent cleanup. The original database named in `TEST_DATABASE_URL` is used only as a connection for creating and dropping the scratch databases.

This automates database recovery checks; it does not test backup scheduling, off-host storage retention, Tor identity recovery, or a full production disaster-recovery procedure.
