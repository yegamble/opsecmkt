# Installer, backup, restore and upgrade regression tests

Run the URL/credential tests without any external service or third-party Python package:

```sh
python3 -m unittest discover -s tests -p 'test_*.py'
```

The installer tests cover the noninteractive `--local` shortcut, inherited-wallet isolation, persisted custom ports, readiness failure, HTTPS handoff, and existing-file preservation. They run the real installation script in temporary repositories with Docker and OpenSSL stubs. They cover internal/external databases, clearnet/Tor exposure, direct-egress overlays, local node profiles (pruned by default: Enter at every prompt gives pruned local nodes with the same prune target as `compose.nodes.yaml`; `no` writes full-node settings; `--local` writes no node settings), the Tor `COMPOSE_FILE`/`COMPOSE_PROFILES` values that the [mirror-command check](#documented-onion-mirror-commands) copies, protected configuration permissions, existing-file preservation, input rejection, and Compose validation failures that must not start services. They also cover the subcommands: `init` flags that need no prompt, distinct `APP_SECRET_KEY` and `SETUP_TOKEN`, the token printed only in init's final block and no other secret printed by any command, the 90-second onion wait, refusal on existing project volumes, `init --admin` (password read twice, piped on stdin, never in any argv, environment, output or `.env`; mismatches refused before anything is written), the `.env` editor (other lines byte for byte, CRLF and a missing final newline kept, mode `0600`), every `check` refusal, `status`, `setup-token [--new]`, `add-mirror`, and `upgrade` (backup or `--no-backup`, the `0600` pre-upgrade copy, defaults equal to `.env.example` for every non-secret key, the key-root copy, pinned images, the mirror profile, Tor recreate, token retirement and a second run that changes nothing), and the malware scanner (A-184): `add-scanner` (refused below 3 GiB with nothing changed, a warning below 4 GiB, the three keys written together and idempotently, a failed check putting `.env` back, the bounded 10-minute wait naming `docker compose logs clamd`), `refresh-scanner` (a pull even when the tag is present, then clamd recreated and the signature version before and after), `init --uploads`, every scanner `check` refusal (fed `docker compose config` output, and when Docker is installed the real output for both modes), `status`, and `upgrade` refreshing the scanner only when it is on and never turning it on; a `clamd` that never becomes healthy fails none of `init` (with and without `--admin`), `upgrade`, `add-mirror` and `setup-token --new`, and floating image tags (`stable-debian`, `1.5`, `_base` and so on) are refused. No command runs `curl`, `wget` or `git`; the only image the installer has Docker pull is the scanner's. They never read the user’s `.env`, let the installer invoke real Docker, or provision infrastructure (one scanner test runs `docker compose config` itself, when Docker is installed, to hand the stub real output). Without Docker Compose that test and two in `tests/test_compose_scanner.py` are skipped; CI's operations job fails on any skipped test.

The connection tests mock process execution and check decoded credentials, IPv6 hosts, ports, supported TLS/query settings, rejected target overrides and null bytes, removal of inherited libpq settings, and errors that do not expose connection secrets. They also verify that PostgreSQL client arguments contain no URL or password.

`tests/test_compose_wallet_passwords.py` (A-466) checks, with Docker Compose installed, that Compose refuses the `monero-wallet` and `monero-escrow-wallet` profiles while `MONERO_WALLET_RPC_PASSWORD` or `ESCROW_XMR_WALLET_RPC_PASSWORD` is blank or missing, renders each wallet's `--rpc-login` with its user and password once set, and needs neither password for installs whose profiles leave the wallets out; without Docker only its text checks run.

Run the encrypted round-trip integration test against a **disposable PostgreSQL server**, using a role with `CREATEDB` permission:

```sh
TEST_DATABASE_URL='postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable' \
  ./scripts/test-backup-restore.sh
```

Required executables are Bash, Python 3, Go (to build the server from this checkout), `psql`, `pg_dump`, `pg_restore`, `age`, and `age-keygen`. Use PostgreSQL clients matching the server major version (CI uses PostgreSQL 17). On Debian with the PostgreSQL Apt repository configured, install `postgresql-client-17`, `python3`, and `age`. On macOS, Homebrew provides `postgresql@17`, `python`, and `age`; include the PostgreSQL client's `bin` directory in `PATH`.

The script creates five uniquely named `opsecmkt_ops_*` databases, generates temporary encryption identities, and verifies:

- Actual encrypted dump and restore with Unicode text and primary keys preserved; the backup's success line names the `BACKUP_DATABASE_URL` database without printing the URL, and the restore says to point `BACKUP_DATABASE_URL` at the restored database.
- The restore's wallet reminder names the runbook section [Back up and restore the escrow arbiter and watch-only wallets](testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) (A-142, A-497), and that heading exists.
- An age-encrypted output file with mode `0600`.
- Refusal to overwrite an existing backup, with its contents unchanged.
- A wrong decryption identity fails without creating tables.
- A schema collision rolls back the entire restore, including a preceding successful table creation, while preserving existing rows.
- After a restore, payouts that were `pending`, `sending`, `blocked` or `held` in the dump receive manual recovery holds with a "Restored from backup" error; a `failed` payout stays failed with its error kept behind the same marker (this synthetic dump has no `send_ambiguous` column, which the restore skips), and sent payouts are untouched. The script reports 4 held and 1 marked failed payout.
- A legacy settings-only dump (no payouts table) receives the recovery gate even when its saved value was false.
- The restored-images line (A-190, ruling D4): "0 images restored from a backup taken <time>; …" for a dump without an images table, and for the application schema the stored count with the time the backup was taken, in UTC, although the backup runs under `TZ=Asia/Tokyo` and the restore under `TZ=America/New_York`.
- The real application schema: the server built from this checkout migrates an empty database, a `pending` release payout, a `sent` refund payout, a `failed` release payout with a definite rejection (`send_ambiguous=false`) and a refund `held` for an account suspension ("Suspended account: ...") are added, with two Bitcoin escrow settlements that have a stored transaction (one `broadcast`, one `confirmed`), and the encrypted dump is restored into another empty database. The restored pending payout is `held`, the suspension hold keeps its text behind the restore marker (so releasing it still asks for the payout address check), the failed one stays `failed` with the restore marker and `send_ambiguous=true` (possibly requeued and sent after the backup), and `payments_recovery_required=true` pauses all outbound payouts (the sent one and the source database are unchanged), and `escrow_release_recheck=true` and `escrow_xmr_release_recheck=true` ask for the Bitcoin and Monero escrow release deadline rechecks (as does the manual protection); the broadcast escrow settlement is held with the same restore marker (the confirmed one is not), and the manual UPGRADING.md protection holds it the same way; a Monero escrow (an active monthly escrow wallet with a free account and an account whose completed order has a built release) comes back with the release held by the same marker, the free account retired, the wallet closing and `xmr_escrow_sweep_required=true`, the same after the manual protection, and a dump without the Monero escrow tables restores with no Monero line; the script reports 2 held and 1 marked failed payout, 1 held escrow settlement and 1 held Monero escrow settlement, and adds exactly one audit row with no user naming those counts, leaving the audit row from before the backup unchanged; `schema_migrations` is identical, and the server then starts on the restored database, answers `/healthz`, applies no migration again and leaves the held payout alone.
- A restore not done by `scripts/restore.sh`: the same application database is copied with plain `pg_dump`/`pg_restore` (standing in for a volume snapshot or point-in-time recovery). The copy has no recovery gate and its queued payout is `pending`, so the application would send it; the UPGRADING.md section 6 SQL, run verbatim, prints `INSERT 0 1`, `UPDATE 2`, `UPDATE 1`, `UPDATE 1`, `INSERT 0 1` and `COMMIT` and leaves the gate, every payout's state, restore marker and `send_ambiguous`, the suspension hold and the failed payout's error exactly as `scripts/restore.sh` does, plus one audit row with no user ("Payout recovery gate set manually ...").
- Payouts sent after the backup ([runbook](testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)): the dump also holds a funded `delivered` order and a funded `disputed` order with an open dispute and a locked transfer, neither with a payout row. The test extracts the runbook's `find-order`, `record-payout` and `clear-gate` SQL and the `restore-protection` SQL of [UPGRADING.md section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) (the blocks after `<!-- runbook-sql: NAME -->`) and runs them verbatim with `psql`. Before reconciliation, completing the delivered order would move it and queue a release (the transition's compare-and-set and `enqueuePayout`'s insert, run in a rolled-back transaction). The find query names the order from the amount; the check prints one row and writes nothing; a non-administrator handle, a kind that does not match the final state, an unreachable final state, a wrong amount, a malformed transaction ID, a blank address, an unknown order, an order that already has a payout and a transaction ID already recorded are each refused with nothing written. Recording writes one `sent` payout with the wallet transaction ID, moves the order to `completed` (and the disputed one to `resolved`, closing the dispute with the refund outcome), credits the counted deposits but not the locked transfer, and adds one order event by the administrator and one audit row each; earlier audit rows are unchanged. Completing the order again then changes nothing. The gate SQL refuses an unknown and a non-administrator handle ("Refused; nothing was changed: no administrator with that handle", rolled back: the gate stays set, no audit row); with the administrator's handle it clears the gate and adds one audit row by that administrator ("Payout recovery gate cleared by ops_admin ..."), and run again it is refused because the gate is not set; earlier audit rows are unchanged. After the server restarts there is still exactly one payout per order, and the restore's holds (including the suspension hold) are still held. This is database-level evidence: no wallet is involved and the completion is emulated in SQL, not driven through the web form.

Only the newly generated database names are used as backup/restore targets. The server runs on a free loopback port with an empty environment apart from the scratch database URL. An exit trap stops it, drops every scratch database and removes temporary keys, dumps, and logs. Use a dedicated local/CI server: the test role necessarily has database-creation permissions, and terminating the script with `SIGKILL` can prevent cleanup. The original database named in `TEST_DATABASE_URL` is used only as a connection for creating and dropping the scratch databases.

## Documented onion-mirror commands

```sh
./scripts/test-mirror-commands.sh            # or: internal / external
```

Requires Docker with Compose v2 and OpenSSL; nothing is built, pulled, created or started. In a temporary copy
of the Compose files, under a unique project name, the script writes a `.env` with `COMPOSE_FILE` and
`COMPOSE_PROFILES` exactly as `scripts/install.sh` writes them for Tor, with the internal or an external
database and with or without local nodes (`tests/test_installer.py` runs the installer and fails if these
values drift). Each documented command is resolved with `docker compose <its global options> config --services`,
and every service it names, or `tor-mirror` for a bare `up`, must be active:

- Before `mirror` is added, a bare `up -d` leaves `tor-mirror` out, and on the internal database the command
  earlier guides gave, `docker compose --profile mirror up -d tor-mirror`, fails with
  `depends on undefined service "db"` (the option replaces `COMPOSE_PROFILES` for that command).
- With `mirror` added to `COMPOSE_PROFILES`, as the [operator guide](operator-guide.md#optional-onion-mirror)
  says, every line of its `<!-- ops-cmd: onion-mirror -->` block, the `--force-recreate tor tor-mirror`
  command in the guide and UPGRADING.md, and the runbook's recovery steps 1 and 9 (`stop tor-mirror`, `up -d`,
  `ps tor-mirror`) resolve, and the internal database stays in the project. Each inline command must still
  appear verbatim in its document.
- `./scripts/install.sh check` accepts each configuration, before and after `mirror` is added (what
  `install.sh add-mirror` writes).

`docker compose up --dry-run` is not used: with Compose 5.5.1 it sometimes failed spuriously
(`app is missing dependency db`) or left lines out of its plan (see [verification](verification.md)). The
check therefore proves which services each command would include, not that Tor starts or the mirror answers.

## Malware scanner container

```sh
./scripts/test-scanner.sh    # CLAMAV_IMAGE=clamav/clamav:<tag> ./scripts/test-scanner.sh tries another image
```

Requires Docker with Compose v2, Python 3 and OpenSSL, and pulls the ClamAV image (about 540 MB) when it is
missing; it never reads the checkout's `.env`, and its exit trap removes the container. It resolves the `clamd`
service of `compose.scanner.yaml` with `docker compose config` (refusing a definition that lost a hardening
key or gained a volume, port, network, environment or dependency) and runs it with `docker run` and exactly
those settings, but `--network none` instead of the `scanner` network. It then checks, against the real image
(war room A-184, rulings C1 and C6):

- The image's own entrypoint stops in a read-only container (`Read-only file system`), which is why Compose
  runs `clamd` directly; the script only reports it if a newer image no longer does.
- `clamd` loads the image's signatures and answers `PING` within 10 minutes (with the Compose healthcheck,
  `clamdcheck.sh`); the container is read-only, runs as `clamav` with every capability dropped and
  `no-new-privileges`, mounts only `deploy/clamd.conf` (read-only), has no route but loopback, and runs no
  `freshclam`; the signatures and `clamd.conf` cannot be written, `/tmp` can.
- `VERSION` reports the daily signature version of the image's own `daily.cvd` (read with `sigtool`), with no
  volume; `SHUTDOWN`, `RELOAD` and `STATS` answer `COMMAND UNAVAILABLE` and `clamd` keeps running.
- Over `INSTREAM`, framed as the app sends it: a text file is `stream: OK`, the EICAR test string (assembled at
  run time) is `FOUND`, and 9 MiB plus one byte is `INSTREAM size limit exceeded. ERROR`.
- `clamconf` shows the options of `deploy/clamd.conf` in force, and the script prints the container's memory
  use and peak.

`tests/test_compose_scanner.py` checks the Compose file and `deploy/clamd.conf` as text, and with Docker
installed the configuration Compose resolves in each mode; CI's Compose job repeats those assertions. CI runs
`scripts/test-scanner.sh` in the container job (internal-database row), and the Python tests, including the
scanner's installer and Compose tests, in the operations job, which fails if any test is skipped (so the
Docker-backed ones cannot be skipped silently).

## Malware scanner leg in CI

```sh
docker build -t opsecmkt:ci .
./scripts/ci-smoke.sh internal scanner    # or: external scanner
```

Requires Docker with Compose v2, Go (it builds `cmd/verify-audit`, as the plain run does), Python 3 and curl;
pulls the ClamAV image of `compose.scanner.yaml` when it is missing (one signature loads in a second or two).
Like the plain `ci-smoke.sh internal|external`, it runs the `opsecmkt:ci` image under a unique project name on
loopback port 18090, with the internal or an external PostgreSQL, ignores `.env`, and its exit trap removes
every container, volume and network it created. The app
runs with `CLAMD_ADDRESS=clamd:3310` and the default 30-day signature age limit, and `clamd` exactly as
`compose.scanner.yaml` defines it (hardened, `scanner` network only) except for one read-only bind mount over
`/var/lib/clamav`, which hides the image's own signatures:

- The only signature is `deploy/testdata/clamav-test.ndb`, a text file with one signature,
  `Opsecmkt.Test.Marker`, matching the marker string the Go and browser tests' fake clamd also looks for.
  The script packs it inside the pinned image, with no network, into an unsigned `daily.cud` built at that
  moment (`sigtool --build=daily.cud --unsigned`). Checked with ClamAV 1.5.4: clamd then answers `VERSION` with
  `ClamAV 1.5.4/1/<the .cud's build time>`, so `/admin` shows the database 0 days old and uploads are Ready on
  any calendar day. (Had it not, the fallback was the image's own signatures plus the test signature with
  `SCANNER_MAX_SIGNATURE_AGE_DAYS=90`; it is not needed.) The EICAR test string, assembled at run time and
  sent over `INSTREAM` inside the clamd container, is `stream: OK`: the image's own signatures are not loaded.
- Every request is sent as a browser without JavaScript sends it: curl with a cookie jar, the CSRF token read
  from the page, an `Origin` header, form posts, and the Photos form's multipart body with `csrf` first, then
  `id`, `description` and the photo. It completes first-run setup, publishes a listing as the administrator,
  and waits until the listing's Photos form is offered (the app polls clamd every 30 seconds).
- A PNG with the marker in a tEXt chunk passes the app's own caps and is refused by clamd: 422, the "flagged
  this file" copy with a reference and without the signature name, no row in `images`, and the one log line
  naming `Opsecmkt.Test.Marker.UNOFFICIAL`. A PNG with other text in a tEXt chunk and no marker is stored, and the
  product page's photo is served publicly as `image/jpeg` without `tEXt`, `IHDR`, that text or the marker.
- `docker compose stop clamd`: an upload gets 503 with "the malware scanner did not respond, so nothing was
  stored", no row is added, `/healthz` answers `ok`, the catalog 200, and the Photos panel says uploads are
  paused. After `docker compose start clamd` the form is offered again (within about 30 seconds) and an upload
  is stored.
- `SHUTDOWN` sent to `clamd:3310` from inside the app container (bash's `/dev/tcp`) answers
  `COMMAND UNAVAILABLE`; clamd still answers `PING`, has not stopped or restarted, and the next upload is
  stored.
- From inside clamd: `getent hosts app` resolves (the control) but `getent hosts db` does not; `nc` reaches
  `app:8080` (the control) but not the database's address on the backend network or `1.1.1.1:443`; and there
  is no default route.
- `/admin#image-scanner` shows Ready, `daily database 1` built during the run, 0 days old (limit 30), the counts
  "3 stored · 1 refused as infected · 1 refused because the scanner failed", and the signature in the table.

CI runs the leg in the container job for both database modes, after the plain smoke test. It proves the app's
use of a real clamd, not what ClamAV detects: no production signature is loaded, so it is no evidence about a
production signature feed or its freshness.

## Backup and restore through the internal-db Compose service

```sh
./scripts/test-internal-db-restore.sh
```

Requires Docker with Compose v2, `age`, `age-keygen` and Python 3; no host PostgreSQL tools or published
database port. The script copies `compose.yaml`, `compose.internal-db.yaml` and the backup/restore scripts
into a temporary directory with its own `.env` and a uniquely named Compose project, starts only the
`postgres:17-alpine` `db` service, checks that it publishes no port, and verifies:

- `scripts/backup.sh` without `BACKUP_DATABASE_URL` writes an age-encrypted `0600` dump through `docker compose exec -T db`.
- `scripts/restore.sh` refuses two destinations, no destination, an unsafe `RESTORE_INTERNAL_DATABASE` name and a wrong confirmation without creating a database; a wrong identity is refused before the confirmation, when the dump's encoding cannot be read, and creates no database either.
- A side-by-side restore (`RESTORE_INTERNAL_DATABASE=opsecmkt_restored`) creates the new database, round-trips Unicode, holds `pending`, `sending`, `blocked` and `held` payouts, marks the `failed` one possibly sent (restore marker and `send_ambiguous=true`), sets `payments_recovery_required=true`, and leaves the live `opsecmkt` database unchanged.
- The side-by-side restore prints the database it restored into and the exact `DATABASE_URL` line for `.env`, and its last line names the `SETUP_TOKEN` and `AUDIT_SIGNING_KEY` the restored database needs from the same backup set. After `DATABASE_URL` is switched to `opsecmkt_restored` and a row is written there, `scripts/backup.sh` names and dumps `opsecmkt_restored`; restoring that backup into a check database contains the row. Double-quoted, `export`-prefixed, unquoted-with-comment, percent-encoded-password, CRLF and repeated `DATABASE_URL` lines select the same database, which the test confirms is the one `docker compose config` resolves for the app; the URL and password are never printed.
- `scripts/backup.sh` refuses a `BACKUP_INTERNAL_DATABASE` that differs from the database in `DATABASE_URL`, and one set together with `BACKUP_DATABASE_URL`, without leaving a file; a matching one is accepted; when the name comes from Compose interpolation the script refuses to guess and uses `BACKUP_INTERNAL_DATABASE`.
- Restoring over the populated live database fails and rolls back completely, without applying the gate.
- After `docker compose down --volumes`, a fresh volume's empty `opsecmkt` database is restored into directly with the same gate and holds.
- With the `db` service stopped, the restore says how to start it.

The exit trap runs `docker compose down --volumes --remove-orphans` for the test project and removes the
temporary directory, keys and dumps. It never reads the checkout's `.env` or touches other projects.

## Documented .env and onion identity backup and restore

```sh
docker build -t opsecmkt-tor:ci -f deploy/Dockerfile.tor deploy   # once; or set TOR_IMAGE to another tag
./scripts/test-tor-identity-restore.sh
```

Requires Docker with Compose v2, the Tor image, `age`, `age-keygen` and OpenSSL; nothing is built or pulled,
and every container runs with `--network none`. In a temporary copy of the Compose files with a Tor `.env`
under a unique project name, the script tags the Tor image as the project's `<project>-tor` and runs the
[operator guide's](operator-guide.md#secrets-env-and-onion-identities) `env-backup`, `env-restore`,
`tor-identity-backup` and `tor-identity-restore` blocks (after their `<!-- ops-cmd: NAME -->` markers)
verbatim with bash, with a test-only age identity:

- `.env` is backed up encrypted with mode `0600` and restored byte for byte into a checkout without one; a
  wrong identity creates no `.env`, and an existing `.env` is refused and left unchanged.
- For `tor_identity` and `tor_mirror_identity`: Tor creates an onion offline, the backup prints its hostname
  and writes a `0600` archive holding the secret key; restoring over the live volume is refused; after
  `docker volume rm` the restore prints the same hostname, and Tor, started as `compose.tor.yaml` runs it (no
  capabilities, no new privileges), bootstraps without an error and keeps the same hostname and secret key
  (compared by digest; the key is never printed).
- A root-owned archive (all entries `root/root`, as a copy taken by hand as root would be) restored into a
  volume created and chowned to root beforehand: Tor starts with the same identity. Without the restore's
  `chown`, Tor exits with `Directory /var/lib/tor/marketplace cannot be read: Permission denied`. (Docker 29
  gives an empty volume the image directory's owner when it is mounted, so the volume's root-owned top
  directory alone does not break Tor there.)
- With the project's Tor image removed, the backup and the restore both fail with `No such image` (the
  commands pass `--pull never`, so nothing of that name is fetched from a registry) and no backup file is left.

The exit trap removes every container and volume of the test project, the image tag and the temporary
directory with its keys. It proves the commands against the real Tor image offline, not that the restored
onion is reachable over the Tor network.

## Real local installation and setup wizard

```sh
python3 scripts/test-install.py
```

Requires Docker Engine/Desktop with Compose v2, Python 3, Bash and OpenSSL.
The script copies only deployment/build inputs into temporary directories, runs
`./scripts/install.sh --local` with no answers, and starts a uniquely named real
Compose project with PostgreSQL 17 on a free loopback application port. It checks
that the installer printed the `.env` setup token exactly once and no other
secret, that `status` reports setup pending, and then uses HTTP form submissions
and a cookie jar to verify the browser wizard: wrong-token rejection, the printed
token creating the administrator, redirect to the branded admin onboarding and
setup lockdown. The token, `APP_SECRET_KEY` and the database password must not
appear in `docker compose logs`; `status` then reports setup complete and removes
the token from `.env`, `setup-token` exits 1, the installer refuses to replace the
existing `.env`, and the admin session survives recreating and restarting the app.
It then installs a second copy of the inputs on the same host: under the first
project's name the installer refuses, names the first checkout and writes no
`.env`; under its own `COMPOSE_PROJECT_NAME`, `init --local --admin` creates the
administrator from a password on standard input (no token in its output or `.env`,
no password in its output, `.env` or Compose logs), that administrator signs in,
and a `docker compose down --volumes` there (with the name taken from `.env` alone)
leaves the first installation's containers unchanged and its admin session intact.
A third copy runs from an `.env` in the v0.1.0-alpha.1 layout (`SETUP_TOKEN` only,
no `APP_PORT` or project name, a `latest` wallet image). After setup, `upgrade`
refuses without a backup choice, then with `--no-backup` copies the token to
`APP_SECRET_KEY`, records the project and port, pins the image, removes the token
and keeps the signed-in session and the CSRF token issued before it (sign-out
with that token succeeds); a second `upgrade` changes nothing. All three projects
and their images are removed afterwards. Browser rendering is covered separately
by Playwright.

The user's configuration and database are never used. Generated credentials stay
inside the protected temporary directory; the evidence log holds the installer's
output with every `.env` secret replaced by its key name.
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

This automates database recovery and upgrade checks; it does not test backup scheduling, off-host storage retention or a full production disaster-recovery procedure. Tor identity recovery is covered separately, offline ([above](#documented-env-and-onion-identity-backup-and-restore)).

## Moving a non-UTF8 database to UTF8

```sh
./scripts/test-utf8-migration.sh
```

Requires Docker, Go, Python 3 and age; no host PostgreSQL client (the client tools run inside the container, through
wrappers put first on `PATH`). `UTF8_MIGRATION_CONTAINER` names the container. It rehearses [UPGRADING.md section
42](../UPGRADING.md#42-database-encoding-must-be-utf8-a-481) (A-481; QA5W3B-1 and -2, QA5W4B-2 to -4, QA5W5B-1 to -3, -6 and -8) with `scripts/check-utf8.sql`,
`scripts/backup.sh` and `scripts/restore.sh`:

1. Starts a disposable `postgres:17-alpine` container on a random loopback port; the server built from this checkout
   migrates a UTF8 seed database, whose schema and rows are copied byte for byte into a `SQL_ASCII` and a `LATIN1`
   database, with a listing, an order and a review added as UTF-8 bytes, as the market before A-481 stored them.
2. `SQL_ASCII`, a review `Great \xff\xfe seller`: `check-utf8.sql` names the table, column, key and ctid and exits 3; a
   plain backup and restore succeed, but the new database's own backup fails to restore with `COPY failed for table
   "reviews": ... 0xff` (the round trip of step 6), and the check fails on the new database too. A second listing is
   added whose title mixes UTF-8 with a stray byte (`Cr\xc3\xa8me \xff br\xc3\xbbl\xc3\xa9e`) and whose description is
   `LATIN1` (`\xe9t\xe9`): the check names three values by their row's key, tells the title to be typed (it also holds
   valid multibyte UTF-8) and the other two to be read as `LATIN1`. After the section's corrections, by key, both
   values of that listing in the same round, the check passes, the round trip restores, the first listing's title is
   byte for byte as written, the review reads `Great ÿþ seller`, the second listing `Crème brûlée` and `été`, and the
   server starts on the new database.
3. The check's reach and readings: in a `SQL_ASCII` database, bad bytes in a `text[]` element, a domain over `text`
   and an element of a domain over `varchar[]` are each named (an element by its subscript, `tags[2]`), a composite
   key is written `a = 'x' AND b = '1'`, a table and column whose names need quoting are printed quoted, and
   `東京 \xff` is to be typed; corrected by the printed statements, the check passes. In a `WIN1252` database
   `\x93quote\x94 \x80 5` is to be read as `WIN1252` and comes out `“quote” € 5`; in an `EUC_JP` one the text is to be
   typed.
4. `LATIN1`, `Crème brûlée — 東京 🔒`: `restore.sh` refuses a backup taken as `LATIN1` (its restore would
   double-encode every non-ASCII character) with a message naming the database's encoding and citing section 42,
   leaving the new database empty, and so a backup taken as `UTF8` with `client_encoding` set to `UTF8` for the
   database (its DATABASE entry still records `LATIN1`); the section's way out for either backup (a plain `pg_restore`
   into a temporary `LATIN1` database, backed up again with `BACKUP_DUMP_ENCODING=SQL_ASCII`, then `restore.sh`)
   restores it exactly. A UTF8 database dumped in `SJIS` is refused with a way out through a `UTF8` database, never an
   `SJIS` one. `BACKUP_DUMP_ENCODING=LATIN1`
   is refused with no file written; with `BACKUP_DUMP_ENCODING=SQL_ASCII` the old database restores exactly, and the
   same option's round trip of the new UTF8 database changes no byte.

The container and temporary directory are always removed; on failure the logs are printed. CI runs it in the
operations job.

## Real Bitcoin and Monero local chains

Current Bitcoin escrow uses `scripts/test-escrow-regtest.sh` against a real
isolated regtest node; Monero uses `scripts/test-escrow-xmr-regtest.sh` on a
local fakechain. Neither uses real funds or establishes public-network proof.
[Local-chain testing](local-chain-testing.md) retains the prior pooled suite's
signature verification, funding and browser commands as historical evidence;
those require the prior drain-capable revision. The current
`scripts/setup-local-chains.py` only stops registered historical processes
without deleting their data and refuses new pooled-chain setup.
