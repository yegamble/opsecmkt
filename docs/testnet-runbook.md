# Monero test-network payments runbook

This runbook covers Monero escrow on a local fakechain or stagenet. Mainnet moves real funds and is the legacy `scripts/install.sh` default for local nodes (native `opsecmkt deploy init` defaults to stagenet); no real-funds mainnet transaction is recorded. Use `--monero-network stagenet` for rehearsal. Escrow is custodial and off by default. See [Enabling Monero payments](install/enable-payments.md). The owner's rebuild on stagenet and its rehearsals are in [Fresh stagenet market and rehearsals](#fresh-stagenet-market-and-rehearsals).

## Back up and restore Monero escrow wallets

The database holds encrypted recovery seeds and order mappings, not the wallet files. Back up `.env`, the database and the `monero_escrow_wallet` volume separately in encrypted storage. Keep the seed recipient's private key offline, apart from those backups. The wallet secret and the original seed key must match their database pins.

Stop the app and dedicated escrow wallet-rpc before taking a consistent wallet-volume copy. Record the Compose project name and wallet names. Restore copies into the same named volume of a separate rehearsal project, with restrictive permissions and the original wallet secret. Never run test tools against a live deployment database or wallet.

The [operator guide](operator-guide.md#monero-escrow) describes monthly wallets, seed recovery, guarded destruction and restore sweeps. Rehearse recovery before relying on backups.

## Reconcile a restored database before enabling payouts

`scripts/restore.sh` sets `payments_recovery_required`, holds restored settlements with a stored transaction and retires free accounts. Checkout waits for the watcher to inspect wallets, including wallets created after the backup. Preserve these guards until the records, daemon and wallet agree. A restore made any other way (a provider point-in-time recovery or snapshot, or a manual `pg_restore`) has none of them until you apply the protection in [UPGRADING.md section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) before the application starts.

Run Compose commands from the deployment directory with its restored `.env`. Preserve `COMPOSE_FILE` and every entry in `COMPOSE_PROFILES`; a deployment with an onion mirror must keep `mirror` in that list.

1. Pause XMR intake and stop public access before restoring. On Tor deployments, run `docker compose stop tor app`; if the mirror is enabled, also run `docker compose stop tor-mirror`. On clearnet deployments, stop the public reverse proxy and app. Keep public ingress stopped throughout reconciliation.
2. Restore the database and wallet copies, with the dedicated escrow wallet-rpc stopped while replacing its volume. Restore the original wallet secret and seed-recipient configuration.
3. Before app startup, set `RETENTION_ENABLED=true` and mount the **current** external `RETENTION_STATE_DIR`, separate from the database backup. Startup must replay its deletion and preservation state before accepting traffic; never replace it with an older copy. See [retention restore safeguards](retention.md).
4. Start only the daemon, wallet-rpc and app on their pinned network. Keep the public Tor gateways or clearnet proxy stopped; use the protected administrator connection for reconciliation.
5. Wait for the Monero wallet sweep. Admin shows wallets missing from disk and settlements held for review. An unresolved wallet or balance remains a recovery blocker.
6. For every held settlement, check its transaction and key images in the daemon and its transfer history in the wallet. Admin's password-confirmed relay check sends the stored transaction unchanged; it never builds a replacement after an ambiguous send.
7. Check orders funded or settled after the backup, compare balances and transaction history, and make another encrypted backup of the reconciled state. Review escrow orders shipped, delivered or cancelled around the backup point: the restored records lack those made after it, with their order messages and notices, and the wallet's transfer history shows deposits and claims, not a shipment or delivery. Your evidence is the old database, if it survives; the parties can only add to it once public access returns at step 9, or through a contact you already hold outside the market. A restore cannot move a paid order's ship-by time or delivery deadline, so until the deadline recheck has run a buyer's no-ship cancel past that deadline is refused and no order completes on a passed deadline (A-714), and the recheck, on the watcher's first pass after the gate is cleared, moves every release deadline and dispute window that has passed or ends within 72 hours to 72 hours after that pass and tells each buyer and vendor (A-728, A-737), so none of those orders completes in that time unless its buyer confirms. It also records a grace of 72 hours (`escrow_xmr_cancel_wait_until`, A-720): until it ends, a buyer's cancel of a paid order whose restored deadline passes before then is refused, and the vendor can still ship or deliver it. Tell vendors to re-ship or re-deliver in that time: to use **Mark shipped** again for goods sent after the backup (they need no second dispatch) and to deliver digital content again. The grace and the buyers' 72 hours run from that first pass, not from step 9, so reopen public access promptly. Once it has ended the buyer can cancel for a refund, even if the vendor shipped or delivered the order after the backup and did not repeat it. With `ESCROW_XMR=off` no watcher runs: the recheck stays pending, a buyer's cancel of a passed deadline is refused with "try again later" for as long as it stays off, and the grace starts on the first pass after you turn Monero escrow back on; do not delete the setting by hand. Re-check the Admin settings against your operator record: the restore rolls back every setting saved after the backup or recovery point, such as **New escrow checkouts**, site access, registrations, the vendor bond amount and approval mode in **Admin → Vendor applications** and the held-release refund period for banned vendors (`ban_hold_refund_days`, **Admin → Payments**, A-652f), and undoes bonds recorded as received after that point, bans and unbans, and held releases refunded to buyers after that point (check banned vendors' orders against your operator record before refunding one again). The fee address and rate (`ESCROW_XMR_FEE_ADDRESS`, `ESCROW_XMR_FEE_BPS`) are host configuration, so a database restore does not roll them back; confirm the host values instead.
8. Stop the app, then [clear the gate](#clear-the-gate) with the host command below, which refuses while the app runs. Start only the app again and verify Admin readiness. Clearing the gate asserts that reconciliation is complete; it does not discover missing obligations or release individual holds.
9. Once reconciliation and readiness checks pass, run `docker compose up -d` with the preserved profiles to restart public Tor ingress and the enabled mirror. On mirror deployments, check `docker compose ps tor-mirror` and confirm its saved onion address answers. On clearnet deployments, reopen the reverse proxy after these checks. Then tell vendors what the restored records lack ([orders around the backup point](#orders-around-the-backup-point)): this is the first moment they can read it or act on it.

**After an outage, not a restore** (A-730): none of the steps above apply. A watcher pass that finds more than an hour, or twice `PAYMENT_POLL_INTERVAL` plus the 10-minute pass timeout if that is longer (2 hours 10 minutes at the 1-hour maximum) since the last deadline step moves every shipped or delivered order's deadline that has passed or ends within 72 hours to 72 hours later, and tells each buyer and vendor it was an outage; a buyer's no-ship cancel then waits the same 72 hours for the vendor. The watcher cannot see public ingress down while the process runs, and it keeps completing orders whose deadlines pass. Ask for the recheck as soon as ingress fails, with the host command:

```sh
# Compose, with the bundled or an external database
docker compose run --rm --no-deps app -request-deadline-recheck
# Native production installation: the URL file for the database the installation uses now
DATABASE_URL_FILE=/srv/opsecmkt-inputs/database.url ./opsecmkt -request-deadline-recheck
```

On a native installation, name the host-readable URL file of the [production layout](install/production.md) for the database the installation uses now. After a [native rollback](../UPGRADING.md#rolling-back-a-native-installation) that is `/srv/opsecmkt-inputs/database-rollback.url`: `database.url` still names the upgraded database, so the command would succeed there and print "now pending" while this market's deadlines keep passing. Do not use `DIR/secrets/database-url`: it names the CA by its path inside the container. A native development installation's local database is not reachable this way, and running its generated Compose file by hand is not supported, so this command is not available there. Whichever form you run, confirm on **Admin → Payments** that the recheck shows pending, or that it has just run: the command's success says only that the database its URL names has the request.

The command works with the app running and writes the request and nothing else. It prints whether the recheck is now pending or was already pending; while a restore's recheck is pending, or the restore gate is set, it changes nothing and says so. With Monero escrow on, the next pass consumes the request; with `ESCROW_XMR=off` the request stays pending until the first pass after it is on again, and a buyer's no-ship cancel past its time waits until then. Repeat it within every 72 hours until ingress returns, and run it once more when ingress returns: otherwise a buyer can come back with minutes left of the last request's 72 hours. It cannot undo a completion: an order already completed stays completed, its release stands, and its buyer is told nothing. **Admin → Payments** shows a recheck pending, and when the last one ran, its cause and how many order deadlines it moved.

A cancel and a dispute do not wait for the first check: with Monero escrow on, once the stored time of the last step is older than the threshold, the action itself refuses a no-ship cancel past its time, creating no refund, and accepts a dispute on a passed deadline, by the database's clock. The order page does not read that time, so until the check has run it can still offer that cancel, which is then refused with its reason, and not offer that dispute. A dispute on a deadline that passed in the outage is refused as late, and a no-ship cancel past its time accepted, only, and that cancel requests a refund, which relays once a watcher runs: when the gap is no longer than the threshold, which is no outage and starts no recheck; when there is no stored time, or one that cannot be read (the first step after the upgrade that added it, or a hand's work); and for the whole time `ESCROW_XMR=off`, when marking an order shipped answers 404, a dispute is refused while no recheck is pending, and a cancel is accepted unless a recheck is pending or the vendor's grace from an earlier recheck has not ended; if it was off for longer than the threshold, the recheck runs on the first pass after it is on again, and a shorter period runs none. Repeated outages each extend these deadlines again, with no cap. A forward jump of the database's clock, or a VM pause, longer than the threshold gives a false recheck. A jump delays every release deadline and dispute window it reaches, and the buyer's cancel grace, by the jump plus 72 hours of true time, market-wide, and no form undoes it: a deadline only moves later. So that one can be seen, each recheck writes one log line (`monero escrow deadline recheck: cause=outage moved=N`) and one audit row, naming no order. Tested against a database and a simulated wallet RPC, with no public network and no real outage.

For the SQL checks in these guides, use the operator's protected database connection. For the bundled database, define
`db_sql` as below; an external database needs its protected `psql` connection instead.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt "$@"; }
```

### Orders around the backup point

Public access is stopped during reconciliation, so you review as far as the evidence allows (the old database if it survives, and the wallet's history), and tell vendors and buyers once public access returns. No form corrects the record: nothing in the market adds a lost settlement, shipment, delivery or cancellation to a restored order, and hand-typed SQL is not a supported remedy. Each case below is an order as the restore left it.

| After the backup | Restored order | What the market does | What you do |
| --- | --- | --- | --- |
| Vendor shipped | `paid` | Accepts **Mark shipped** again until the buyer cancels; the grace of step 7 holds the buyer's cancel of a passed deadline | Tell the vendor to use **Mark shipped** again, with no second dispatch, within the grace |
| Vendor delivered digital content | `paid` | Accepts the delivery again, which opens a new 48-hour dispute window | Tell the vendor to deliver again within the grace |
| Buyer confirmed receipt, or the order completed, with no claim or release sent | `shipped` or `delivered` | Keeps the coins; a deadline passed or ending within 72 hours moves to 72 hours after the recheck | Tell the buyer **Confirm receipt** can be used again; otherwise the moved deadline completes the order |
| Vendor claimed, and the wallet history shows the release with no settlement in the records | `completed`, `shipped` or `delivered` | Notices nothing until asked. A new claim or release is accepted, then waits as an outputs mismatch flagged to staff; nothing is built or sent | Tell the vendor not to claim again: the wallet history is the only record of that payment. **No tool** records it |
| Order cancelled and refunded, and the wallet history shows the refund with no settlement in the records | `paid` | Notices nothing until asked, and still accepts **Mark shipped**. The release that follows waits as an outputs mismatch flagged to staff, so a vendor who ships is never paid | **Tell the vendor not to ship**, and the buyer that the refund stands. **No tool** records it. The vendor's cancel moves the order to `cancelled` but its refund waits as an outputs mismatch too, so it pays nothing twice and corrects nothing |
| Order cancelled, with no refund in the wallet history | `paid` | The coins are still in the order's account. The vendor's cancel works, and its refund is built once the gate is cleared. The buyer's cancel is refused until the deadline and the grace have passed | Tell the vendor to cancel the order again |
| Order cancelled before any deposit | `awaiting_payment` | The payment window ends it as usual | Nothing |

A wallet copy older than a send may not list that send's destination; the wallet history then shows coins leaving the account, not whether they went to the vendor or the buyer, and the old database or the parties are your only evidence of which row applies.

What the market does in the claim and refund rows is checked against the simulated wallet RPC only (`TestXMREscrowSpentAfterBackupWaitsAsOutputsMismatch`), where the coins are spent by a transaction the records lack. A real restored monero-wallet-rpc reaching that state is UNVERIFIED: the fakechain `Restore` leg covers a send whose settlement the records keep, not one they lack.

### Clear the gate

Clear the gate with the host command (A-505), in a terminal, with the application stopped, naming the administrator in
whose name the reconciliation is asserted. On a Compose deployment:

<!-- runbook-command: clear-gate -->
```sh
docker compose run --rm app -clear-restore-gate YOUR_ADMIN_HANDLE
```

On a native deployment with an external database run `./opsecmkt -clear-restore-gate YOUR_ADMIN_HANDLE` with
`DATABASE_URL_FILE=/srv/opsecmkt-inputs/database.url`, the host-readable URL file of the [production layout](install/production.md) whose `sslrootcert` names a CA path on this host. Do not use `DIR/secrets/database-url`: it names the CA by its path inside the container, and in local-database mode its host `db` resolves only on the Compose network (see the other [host commands](install/cli-reference.md#legacy-host-commands)). A native development
installation's local database is reachable only on its generated Compose network, and running that generated Compose file
by hand is not supported (it is an implementation artifact of `opsecmkt deploy`), so these commands are not supported
there: rehearse restores on a Compose installation (`scripts/install.sh`) or on a native installation with an external
database. The command connects with `DATABASE_URL` and starts no web server, payment provider or background work. It
refuses, and changes nothing, when:

- the application is running against the database, or starting: every serving application of this release or later holds
  a database lock the command needs (stop it first; an application of an earlier release holds no such lock and is not
  detected);
- stdin is not a terminal: no flag or pipe answers for the operator;
- the handle is not an administrator's (handles are exact and case-sensitive);
- the gate is not set (it was already cleared);
- no application has started on the restored database yet (`retention_restore_required` is still set: steps 3 and 4
  come first);
- the database records migrations this binary does not include.

The lock check is a guard against forgetting to stop the app, not a guarantee: a serving application whose database
session ended (a database restart, a terminated session) holds no lock until it takes it again, within about 10 seconds.
The refusal says how many sessions hold the lock and when each started, never their address. If no application runs and
the command still refuses, find the holders over the protected database connection:

```sh
db_sql -c "SELECT a.pid, a.application_name, a.backend_start FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE l.locktype = 'advisory' AND l.classid = 0 AND l.objid = 782499 AND l.objsubid = 1 AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())"
```

A serving application's session is named `opsecmkt-serving`. Find and stop that application first (an older container,
a second host, a service manager restarting it); only a holder left by an application that is gone may be ended, with
`db_sql -c "SELECT pg_terminate_backend(PID)"`. Ending the session of an application that still runs only removes the
lock until it takes it again.

If the Monero escrow restore sweep has not completed (`xmr_escrow_sweep_required` is still set) it says so first; that
is not a refusal, but Monero escrow checkout stays paused until the sweep completes. Then it asks you to confirm, one
statement at a time, that steps 1 to 7 above are done (five statements); type `yes`
to each. Any other answer refuses at that step. Then, in one transaction that makes the checks again, it sets
`payments_recovery_required` to `false` and adds the audit row "Payout recovery gate cleared by HANDLE by host command after reconciling the restored database
with the wallets" on that administrator. It ends with `Restore gate cleared by administrator HANDLE; audited.` and prints
no database URL, key or wallet value. The other restore settings and the settlement holds are unchanged. Then start the
app and verify readiness.


## Handle a payment-review flag

Review flagged deposits from the moderation desk. Check their transaction, amount, confirmations, unlock time and account in the dedicated wallet. Record the reason and disposition with password confirmation. Do not disable settlement guards, reuse an address or manually send a second payment to resolve an ambiguous relay. A moderator can escalate; only an administrator decides a Monero escrow dispute.

## 9. Manual Monero escrow fakechain check

`scripts/test-escrow-xmr-regtest.sh` runs the nineteen legs of Monero escrow ([per-order escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain))
against a real `monerod --regtest` and real `monero-wallet-rpc` 0.18.5.1. CI runs it too (the `escrow-xmr-regtest` job, A-498). **It is for a local
fakechain only**: it refuses any daemon that does not report nettype `fakechain` with hard fork version 16 or later before any
wallet call, and each Go leg checks the same again. It needs Docker for the nodes, Go, `curl`, `tar` with bzip2 and a disposable PostgreSQL
database (never a real deployment's). The `StaleRingRemedyB` leg runs the official `monero-wallet-cli` 0.18.5.1 for Linux
x86-64 (on an ARM host, through Docker's amd64 emulation); the script downloads its archive from getmonero.org into
`artifacts/cache/` once (or takes `ESCROW_XMR_REGTEST_CLI_ARCHIVE`) and refuses it unless its SHA-256 is the pinned one of
the release's signed `hashes.txt`. The legs run the app inside the Go test: this is not a deployment check.

Start a fakechain daemon and three wallet-rpc instances on one Docker network (the app's escrow wallet-rpc, a funder
standing in for the buyer's wallet, and one used only to restore a sealed seed). The wallet-rpcs listen without RPC login on
`127.0.0.1` only; the image's entrypoint adds `--non-interactive` and the bind address itself, and
`RPC_EXPOSE_UNAUTHENTICATED=1` lets the wallet-rpc answer on the published loopback port:

```sh
docker network create xmr-escrow-regtest
docker run -d --name xmr-escrow-monerod --network xmr-escrow-regtest -p 127.0.0.1:28381:18081 \
  ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 --regtest --keep-fakechain --offline --fixed-difficulty 1 \
  --rpc-bind-ip 0.0.0.0 --rpc-bind-port 18081 --confirm-external-bind --no-igd
for w in escrow:28382 funder:28383 restore:28384; do
  docker run -d --name "xmr-escrow-wallet-${w%%:*}" --network xmr-escrow-regtest -p "127.0.0.1:${w##*:}:18083" \
    -v "xmr-escrow-wallet-${w%%:*}:/home/monero/wallet" -e RPC_EXPOSE_UNAUTHENTICATED=1 ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
    --daemon-address xmr-escrow-monerod:18081 --trusted-daemon --wallet-dir /home/monero/wallet --disable-rpc-login \
    --shared-ringdb-dir /home/monero/wallet/.shared-ringdb --rpc-bind-port 18083 --allow-mismatched-daemon-version
done
```

Wait until the four containers report healthy (`docker ps`), then run the script with a disposable database. The legs create
their own databases from it, so give it a database you can drop afterwards:

```sh
ESCROW_XMR_REGTEST_DAEMON_URL=http://127.0.0.1:28381 ESCROW_XMR_REGTEST_WALLET_URL=http://127.0.0.1:28382 \
ESCROW_XMR_REGTEST_FUNDER_URL=http://127.0.0.1:28383 ESCROW_XMR_REGTEST_RESTORE_URL=http://127.0.0.1:28384 \
ESCROW_XMR_REGTEST_WALLET_CONTAINER=xmr-escrow-wallet-escrow ESCROW_XMR_REGTEST_DAEMON_CONTAINER=xmr-escrow-monerod \
TEST_DATABASE_URL=postgres://USER:PASSWORD@HOST:PORT/a_disposable_database?sslmode=disable ./scripts/test-escrow-xmr-regtest.sh
docker rm -f xmr-escrow-monerod xmr-escrow-wallet-escrow xmr-escrow-wallet-funder xmr-escrow-wallet-restore
docker volume rm xmr-escrow-wallet-escrow xmr-escrow-wallet-funder xmr-escrow-wallet-restore
docker network rm xmr-escrow-regtest
```

`ESCROW_XMR_REGTEST_WALLET_CONTAINER` is needed by `StaleRing`, which stops it, moves the ring database aside on its volume
and starts it (the operator guide's [stale-ring break-glass](operator-guide.md#stale-ring-break-glass-a-618), remedy A), and by
`StaleRingRemedyB`, which stops it and clears the stuck rings with `monero-wallet-cli` (remedy B), and by `ReRequest`, which
applies remedy A when its re-request's build stays refused for a stale ring (A-639); all three fail as skipped without it; with it,
`RestartMidSend` also restarts that container.
`ESCROW_XMR_REGTEST_DAEMON_CONTAINER` is required: `ReRequest` stops and starts that monerod container around each relay,
which is why monerod runs with `--keep-fakechain` (without it, monerod deletes the fakechain when it starts). The legs run
in sequence and took about 11 minutes in one run on a 4-CPU host (the script allows 45 minutes for the whole run); they may
run again against the same nodes, since each puts the watcher's clock in a month of its own. The transcript is written to
`ESCROW_XMR_REGTEST_LOG` (default `artifacts/real-chain/test-escrow-xmr-regtest-<UTC time>.log`, gitignored); no seed,
password or URL is printed. The script reports each leg from its own result line, so a failed, skipped or missing leg is
named and the run fails; it ends with one `PASS:` line listing what was shown. The legs are: `Wallets`, `Funding`,
`TwoAccountsOneTx`, `Physical`, `RestartMidSend`, `RelayDropped`, `TopUp`, `Reorg`, `FundingReorg`, `External`, `ReRequest`, `StaleRing`, `StaleRingRemedyB`, `Restore`, `Lookahead`,
`WrongSecret`, `Destroy`, `RefundBelowFee`, `SupersededMined`, `FeeAndReturn` and `Bond`, each `TestXMREscrowRegtest<Leg>` in `internal/market/`; what each proves is listed in
[per-order escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain). A pass is evidence for the code and the 0.18.5.1
nodes on a local fakechain, not for stagenet, testnet or mainnet.

## 10. Monero escrow on stagenet

Since A-449, Monero escrow (`ESCROW_XMR=on`, off by default, custodial) may run on Monero stagenet as well as on a local
fakechain: the escrow daemon must report nettype `stagenet` with hard fork 16 or later, the escrow wallet-rpc must run with
`--stagenet` against the same daemon, and refund, claim and fee addresses must be stagenet ones (5… or 7…). Setup, the
network pin and the sync guard are in the [operator guide](operator-guide.md#monero-escrow). The [public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) proves one funded digital claim with real peers, fees and confirmations. Before A-709 funding completed the order, so that run proves a superseded journey: the dispute window and delivered state have no public-network run.
Public refunds, physical deadlines, fee/ring-member stress and reorganisations remain unverified.

`scripts/test-escrow-xmr-stagenet.sh` (manual, not CI) runs the legs an **offline stagenet fork, not the public stagenet**,
can run: stagenet daemons built from the genesis block with the local monerod 0.18.5.1 image, peered only with each other
on a Docker network with no route out, mining locally at fixed difficulty, plus one `--offline` daemon and a stagenet
wallet-rpc. It refuses any daemon that is not stagenet below height 10,000 (the public stagenet is past 1,900,000). Such a
chain is at hard fork version 1 (version 2 starts at height 32,000, 16 at 1,151,720), so Monero escrow refuses to start on
it, as it must; the nineteen fakechain legs of section 9 need escrow to start and cannot run on it. The script's legs show
that refusal names the hard fork rather than the network, that a real stagenet wallet's addresses are accepted under the
stagenet tags and refused under the fakechain's, and that the Monero escrow sync guard reads a restricted stagenet
daemon (as the bundled `monero` service serves) as synced with a peer and a fresh tip and an `--offline` one as having no
peers. The commands are in the script's header comment; the transcript goes to `ESCROW_XMR_STAGENET_LOG` (default
`artifacts/real-chain/test-escrow-xmr-stagenet-<UTC time>.log`, gitignored).

**What the offline fork cannot prove (A-449 acceptance).** No funded Monero escrow leg can run on it: a stagenet built
from genesis is at hard fork version 1, which Monero escrow refuses, and the fakechain legs mine with `generateblocks`,
which monerod answers only on a fakechain (`--regtest`), so the fork has only the blocks its local miner finds. Funded
Monero escrow on stagenet (a deposit seen, an order funded, a settlement built, relayed and confirmed) is therefore proven
by the [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) (A-447/A-491), scoped to one digital claim. Before A-709 funding completed the order, so that run proves a superseded journey: the dispute window and delivered state have no public-network run. The
offline fork proves the stagenet address formats, the refusal of an old hard fork and the escrow sync guard
against real daemons; the network pin is covered by the database tests against the simulated daemon
(`TestXMREscrowNetworkPinMismatch`, `TestXMREscrowNetworkPinBackfillAndRepin`), not by the fork.

The separate manual public runner is `scripts/test-escrow-xmr-public-stagenet.py`; its guarded digital-claim journey
reaches completion through the buyer's Confirm receipt form and uses actual confirmations and a fetched public explorer
anchor. It edits no deadline, so the dispute window's expiry is not exercised; no run of this journey is recorded. See the [Monero operator guide](operator-guide.md#monero-escrow)
and the script header for its dedicated funded wallet, empty escrow wallet directory and Docker/RPC requirements.
Select an exact HTTPS block URL on `stagenet.xmrchain.net` or `xmr-stagenet.librenode.com`; both the runner and Go
test fetch the selected host's block API before wallet work. An unavailable or mismatching source refuses the run.
Its source/offline checks are not a funded public result. The recorded public digital claim passed with ten confirmations on both funding and release; it does not establish public refund timing, physical-order deadlines or deployment readiness.

## Fresh stagenet market and rehearsals

This is the owner's step list for A-699: retire the mainnet-configured, intake-paused market, build an empty stagenet market on the same hosts from the tagged build (which also does A-669's rebuild), then run the rehearsals A-562 and A-698 and the public journey PM-3 on it. A network switch in place is not possible: the network pin refuses it once escrow or bond wallet rows exist, and no command imports remote escrow material into an existing installation ([enable payments](install/enable-payments.md#native-production-with-separate-payment-services)). None of this has been run. Every `opsecmkt` flag below was checked against the source (`tests/test_runbook_rehearsals.py`), not on a host, so a refusal is a finding: stop and record it. Replace capitalized words with your own values, and keep private values in mode-`0600` files.

### What is retired

The whole market that runs now, and nothing in it comes across: its database, its escrow and bond wallets on the payment host, its private installation directory with the application key, setup token, audit signing key and wallet secret, its external retention ledger, its public onion identity on the gateway host, its administrator onion identity in `onion-admin/` and the `tor_admin_identity` volume, and the administrator client keys bound to that onion. Its mainnet daemon stops running. The new market generates all of these afresh.

Retired first means stopped, not deleted. The rebuild needs no deletion: it uses a new directory, database, wallet directory, retention directory and onion directory, so it never overwrites a populated identity volume or a wallet. A retired public or administrator identity is never reused, for this market or any other.

### What the owner must approve

These are destructive, and a review, a ledger row or this runbook does not authorize them: the owner approves the concrete action and its data loss, naming the exact resource, in the private operator record. The 2026-10-16 authorization covered that rebuild, not these. First confirm what would be lost: run this read-only on the protected connection to the old database, and read each old wallet's balance on the payment host. The market is recorded as empty with no real-funds transaction: orders, escrows and settlements must all be 0 and every wallet balance zero, and anything else stops this list. `users` counts the old administrator, so it is not 0; record it in the private record with the approval.

```sql
SELECT (SELECT count(*) FROM users) AS users, (SELECT count(*) FROM orders) AS orders,
       (SELECT count(*) FROM escrows) AS escrows, (SELECT count(*) FROM xmr_settlements) AS settlements;
```

| Action needing approval | What is lost, for good |
| --- | --- |
| Delete the old market database, and its provider backups when they expire | Accounts, audit history, images and the sealed wallet seeds. Provider deletion does not attest erasure of the media or of every historical backup |
| Delete the old mainnet wallet files on the payment host | Any coins in them, unless a seed survives. Decide this together with the database, which holds the sealed seeds: losing both loses the funds outright |
| Delete the old installation directory, retention ledger and retired keys | Everything sealed under those keys becomes unreadable, the old database's retention can no longer be replayed, and no further audit export can be signed under the old key |
| If the old installation configured reboot control: remove `opsecmkt-host-control-OLD_PROJECT.service`, its policy `/etc/opsecmkt-host-control/OLD_PROJECT.json` and its state `/var/lib/opsecmkt-host-control/OLD_PROJECT` | The old market's reboot receipts and readiness acknowledgement |
| Delete the old public onion directory on the gateway, and the old `onion-admin/` files and `tor_admin_identity` volume on the application host | Those two addresses, permanently. Nobody can publish them again |

Deleting the old installation directory has two traps. Production `install` left a storage-check drop-in for the old project on both Docker and containerd, which runs against that directory at every daemon start, so removing the directory alone stops Docker and containerd starting on the next boot. And if reboot control was configured, the agent `opsecmkt-host-control-OLD_PROJECT.service` is an enabled unit ordered before Docker, which checks that directory when it starts and restarts on failure: with the directory gone it fails and retries at each boot, and whether systemd holds Docker behind its pending start job is unverified. `deploy down` does not stop this agent, so it is still running and enabled after step 1 below; that is harmless while the old directory exists, since the only application that could ask it is gone. Get the old project name from `deploy plan --dir OLD_DIR` first, and check for the agent with `systemctl list-unit-files opsecmkt-host-control-OLD_PROJECT.service`. Then, only with the approval above, in this order, skipping the first four commands if there is no such unit:

```sh
systemctl disable --now opsecmkt-host-control-OLD_PROJECT.service
rm /etc/systemd/system/opsecmkt-host-control-OLD_PROJECT.service
rm /etc/opsecmkt-host-control/OLD_PROJECT.json
rm -r /var/lib/opsecmkt-host-control/OLD_PROJECT
rm /etc/systemd/system/docker.service.d/50-opsecmkt-storage-OLD_PROJECT.conf
rm /etc/systemd/system/containerd.service.d/50-opsecmkt-storage-OLD_PROJECT.conf
systemctl daemon-reload
docker volume ls --filter label=com.docker.compose.project=OLD_PROJECT
```

Delete only the volumes that list names, one at a time, and only then the directory. Until approval, leave all of it in place, unstarted.

### What is preserved

The three hosts and their encrypted roots, LUKS headers and unlock material; operator SSH access and sudo; provider firewalls and host policy; the managed PostgreSQL cluster (a new database in it, not the old one); the private issuing CAs, and leaf certificates whose hostnames stay the same; ClamAV and its signatures; the version worker, its signing key and the report directory with its pinned public key; the old installation, wallet and onion material until the approval above; historical recovery archives and the private operator record; and every published record in `docs/evidence/` and the [deployment notes](install/deployment-notes.md), which stay as history.

### Re-initialize from the tagged build

Prerequisite: A-694 to A-697 and the tag (A-680) have landed. The steps keep the order of [the production procedure](install/production.md). The controller manages only the application host; the others are by hand.

1. **Stop the old market**, with the binary it was installed with. This deletes nothing: `deploy down` retains every volume and private file. Stopping is not enough on the other hosts. A reboot or package restart before step 6 would bring back the old Tor service, whose fragment still names `/var/lib/tor/public-market/`, and once `install` has run the gateway would forward that retired onion to the new market. So disable all of them, so that none returns: the gateway and the old public Tor service on the gateway host, and the mainnet daemon and wallet RPC on the payment host. The gateway unit is the one `production.md` names; the others' names and shapes are your own, so use your own. For a container, the equivalent is `docker update --restart=no CONTAINER` and then `docker stop CONTAINER`; under another supervisor, its own disable.

   ```sh
   ./opsecmkt deploy down --dir OLD_DIR
   systemctl disable --now opsecmkt-gateway
   systemctl disable --now YOUR_OLD_TOR_UNIT
   systemctl disable --now YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT
   ```

2. **Get the tagged build.** On the application and gateway hosts, verify the release binary and install it where production starts require it. The old project's drop-in now runs this binary at each boot, so check it accepts the old directory before any reboot. In the tagged checkout, [build and publish the application image](install/binary.md#building-and-publishing-the-application-image) and the [Tor image](install/binary.md#building-and-publishing-the-tor-image), so `APP_IMAGE` and `TOR_IMAGE` hold full digest references. Apply the hosts' security updates and check the A-669 fix: expect `deb12u2` or later. Keep the digests and this output in the private record.

   ```sh
   gh release download YOUR_TAG --repo OWNER/opsecmkt --dir release-YOUR_TAG
   (cd release-YOUR_TAG && sha256sum -c SHA256SUMS)
   install -m 755 release-YOUR_TAG/opsecmkt-linux-amd64 /usr/local/bin/opsecmkt
   /usr/local/bin/opsecmkt version
   /usr/local/bin/opsecmkt deploy storage check-bound --dir OLD_DIR
   docker run --rm --network none --entrypoint dpkg "$APP_IMAGE" -l libssl3
   docker run --rm --network none --entrypoint dpkg "$TOR_IMAGE" -l libssl3
   ```

3. **Prepare the new private inputs**, all in a new directory, here `/srv/opsecmkt-inputs-stagenet`:
   - A new empty UTF8 database and its own role, per the [external database procedure](install/external-database.md), with its URL in a mode-`0600` file.
   - On the payment host, a stagenet daemon and a dedicated wallet RPC with a new data directory and an empty wallet directory, per [enable payments](install/enable-payments.md#native-production-with-separate-payment-services) and the [service flows](install/service-topology.md#allowing-only-required-service-flows): `--stagenet` on both, `--daemon-address` naming that daemon, `--shared-ringdb-dir=WALLET_DIR/.shared-ringdb`, authenticated HTTPS for the application host only, and fresh RPC credentials in two mode-`0600` URL files.
   - [Payment-host Tor egress](operator-guide.md#declaring-separate-payment-host-tor-egress), checked as that section says. The `remote-tor` setting below only records your declaration; without it, native production Tor holds every settlement relay and A-562 cannot run.
   - A fresh [seed-recovery OpenPGP pair](install/binary.md#importing-and-generating-keys), made offline, with only its public half here.
   - A new external retention directory: an old ledger never goes with a new database.
   - The TLS files step 5 names. `database-ca.pem` is the managed database's public CA, and `monero-ca.pem` the public CA of the payment proxy, which must differ from `upload-ca.pem`. For `app.pem`, `app.key`, `upload-ca.pem`, `backend.pem` and `backend.key`, copy the old inputs if the hostnames stay the same, through an authenticated encrypted channel, or reissue them as in [production section 3](install/production.md#3-prepare-application-and-gateway-identities), with CA private keys staying on the issuing machine. `app.key` and `backend.key` are mode `0600`, and the public PEM files mode `0644` or stricter. A private key's contents never go on a command line or in a report: the commands below name only its path.

4. **Make fresh identities** on the trusted machines: the storefront identity on the issuing machine, and the administrator's client key on that administrator's machine ([steps](install/tor.md#generate-the-administrators-client-key)). Copy only the `.auth` file to `admin-clients/`. `init` generates the administrator onion itself.

   ```sh
   ./opsecmkt keys generate onion --out-dir ./identities-stagenet/storefront
   ./opsecmkt keys generate tor-client --public-out ./admin-public/stagenet.auth --private-out ./admin-private/stagenet.key
   ```

5. **Initialize once**, in a new directory; `init` refuses an existing one. Add `--monero-rpc-cert` and `--monero-rpc-key` if the payment proxy uses mutual TLS instead of URL credentials. `--site-access` and `--registrations` are your choice, as long as a buyer and a vendor account can be made.

   ```sh
   /usr/local/bin/opsecmkt deploy init --dir /srv/opsecmkt/market-stagenet --mode prod \
     --app-image "$APP_IMAGE" --exposure tor \
     --gateway-url http://YOUR_NEW_PUBLIC_ONION.onion --app-host app.internal \
     --admin-tor-image "$TOR_IMAGE" --admin-client-auth-dir /srv/opsecmkt-inputs-stagenet/admin-clients \
     --app-bind 10.20.0.10:8080 --backend-bind 10.20.0.10:8443 \
     --database-url-file /srv/opsecmkt-inputs-stagenet/database.url \
     --database-ca /srv/opsecmkt-inputs-stagenet/database-ca.pem \
     --tls-cert /srv/opsecmkt-inputs-stagenet/app.pem --tls-key /srv/opsecmkt-inputs-stagenet/app.key \
     --upload-ca /srv/opsecmkt-inputs-stagenet/upload-ca.pem \
     --upload-cert /srv/opsecmkt-inputs-stagenet/backend.pem --upload-key /srv/opsecmkt-inputs-stagenet/backend.key \
     --retention-dir /srv/opsecmkt-retention-stagenet --set RETENTION_ENABLED=on \
     --dependency-report-dir YOUR_REPORT_DIR --dependency-report-public-key YOUR_PINNED_KEY_FILE \
     --escrow-xmr --monero-network stagenet \
     --monero-wallet-rpc-url-file /srv/opsecmkt-inputs-stagenet/wallet.url \
     --monero-daemon-url-file /srv/opsecmkt-inputs-stagenet/daemon.url \
     --monero-rpc-ca /srv/opsecmkt-inputs-stagenet/monero-ca.pem \
     --monero-seed-key /srv/opsecmkt-inputs-stagenet/escrow-public.asc \
     --set ESCROW_XMR_TOR_RELAY=remote-tor
   /usr/local/bin/opsecmkt deploy plan --dir /srv/opsecmkt/market-stagenet
   /usr/local/bin/opsecmkt deploy doctor --dir /srv/opsecmkt/market-stagenet
   /usr/local/bin/opsecmkt deploy install --dir /srv/opsecmkt/market-stagenet
   /usr/local/bin/opsecmkt deploy status --dir /srv/opsecmkt/market-stagenet
   ```

   Stop if `plan` does not say stagenet. If the old installation used [reboot control](install/production.md#optional-application-host-reboot-control), configure it for the new directory. The old agent's unit, policy and state are named for the old project, so they do not collide by name, but whether the old agent interferes is unverified, so stop on a refusal.

6. **Prepare the gateway, with public ingress still fenced.** Transfer only the storefront identity to the gateway host, into a new `HiddenServiceDir` such as `/var/lib/tor/public-market-stagenet/` or a new volume, and never overwrite the populated `/var/lib/tor/public-market/` ([steps](install/tor.md#prepare-the-public-gateway-identity)). Point the Tor fragment at the new directory and remove the old one's from the configuration, leaving Tor disabled, then enable the gateway; nothing can publish the old address now, and the new one waits for step 8.

   ```sh
   systemctl enable --now opsecmkt-gateway
   ```

7. **Bind the client key** to the new administrator onion that `plan` prints, as in [bind the client key](install/tor.md#bind-the-client-key-and-verify-ingress), keeping `.auth_private` off the server:

   ```sh
   ./opsecmkt keys import tor-client --in ./admin-private/stagenet.key --public-out ./admin-public/bound-stagenet.auth --private-out ./admin-private/stagenet.auth_private --onion YOUR_NEW_ADMINISTRATOR_ONION.onion
   ```

8. **Set up and verify.** Create the administrator at the administrator onion's `/setup` or with the command below, enroll an authenticator, and set **New escrow checkouts** to **Paused**. Before starting public Tor, confirm that the old Tor service and the old daemon and wallet RPC are inactive and disabled, on their own hosts, as below; `is-active` must print `inactive` and `is-enabled` `disabled` (for a container, stopped with restart policy `no`), and if a unit name is reused for the new service, confirm instead that its configuration names only the new directory or stagenet. Then enable public Tor with its supervisor, confirm its `hostname` file is the new address, and run the checks in [production section 6](install/production.md#6-complete-first-run-setup-and-verify-journeys). Stagenet synchronization takes as long as it takes; measure it. When the daemon and wallet are synced and **Admin → Payments** accepts the network, wallet secret and seed key, take the first encrypted backups, then set **New escrow checkouts** to **Escrow**.

   ```sh
   systemctl is-active YOUR_OLD_TOR_UNIT YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT
   systemctl is-enabled YOUR_OLD_TOR_UNIT YOUR_OLD_MONEROD_UNIT YOUR_OLD_WALLET_RPC_UNIT
   DATABASE_URL_FILE=/srv/opsecmkt-inputs-stagenet/database.url /usr/local/bin/opsecmkt -bootstrap-admin YOUR_ADMIN_HANDLE -site-name 'Stagenet rehearsal'
   ```

**Record:** in the private record, the old market's answers, the approvals, digests and `libssl3` output; in the [deployment notes](install/deployment-notes.md), a new dated checkpoint in role names, as [Recording the results](#recording-the-results) says. This shows a healthy empty stagenet market and A-669's rebuild, and it does not show that any payment works.

### Legs and their order

**No runner exists for any of these.** `scripts/test-escrow-xmr-public-stagenet.py` is one digital claim with an empty wallet directory, loopback RPC and a disposable loopback PostgreSQL, so it never touches the deployed `--mode prod` topology, and `scripts/test-host-bootstrap.py` runs in a network namespace with no host apply. Every leg here is by hand, on the deployed market. Do not edit a deadline in the database to save time (A-729): the leg would then prove nothing about it.

| Run order | Leg | Real time |
| --- | --- | --- |
| 1 | A-562 digital claim | About an hour: the one recorded sample took 22 minutes to fund and 28 from claim to confirmed |
| 2 | PM-3, which is also A-562's physical refund | About an hour of confirmations, plus your own pace |
| 3 | A-562 deadline release, started with leg 1 | The real 48 hours, then about 30 minutes for its claim |
| 4 | Outage (a), then (b) | Over an hour each, with (a) timed to span the end of a dispute window |
| 5 | A-698 restore, redeployment, host policy, replacement host | Hours each, and a spare encrypted host |

**Do not stop the application for over the outage threshold while leg 3 runs.** The threshold is more than an hour, or twice `PAYMENT_POLL_INTERVAL` plus the 10-minute pass timeout if that is longer; at the default 30 seconds, one hour. A longer stop, including a locked reboot or a host move, moves every shipped or delivered order's deadline that ends within 72 hours to 72 hours later, and starts the 72-hour grace on a buyer's no-ship cancel, as it should, and leg 3 waits again. That is why the A-698 legs come last.

### Leg: production wallet topology (A-562)

"Rehearse on stagenet with the native `--mode prod` topology wallet creation, transfer do_not_relay, relay_tx and Digest under concurrent polling through the TLS proxy — one digital claim, one physical refund, one deadline release." **No runner.** Do it in Tor Browser with a buyer and a vendor account on the public onion and the administrator on the private onion; approve the vendor under [Vendor applications](operator-guide.md#vendor-applications). Pay from a dedicated stagenet wallet you own. Keep `PAYMENT_POLL_INTERVAL` at its default, and during each send keep reloading `/admin/payments` in another tab, so the watcher and the pages call the wallet through the proxy while it builds and relays.

1. **Digital claim.** Publish a digital listing with automatic delivery. The buyer checks out, giving a stagenet refund address, and pays. Funding takes 10 confirmations; the order is then **Delivered**, with its 48-hour dispute window. The buyer uses **Confirm receipt** with their password, the vendor claims with a fresh stagenet address and their password, and the watcher builds, relays and confirms the release. Check the wallet received the planned amount.
2. **Physical refund.** Run PM-3, below.
3. **Deadline release.** Alongside step 1, place a second digital order and have the buyer do nothing for the real 48 hours. Expect the watcher to complete it with the note "dispute window ended"; then the vendor claims. A physical order's release deadline is at least 7 days (the setting's minimum on `/admin/payments`, for orders placed afterwards), and only there does the market build the release at the deadline with no vendor action. Run that as well if the 7 days are affordable; otherwise say it was not run.

After each send read the row, on the protected connection. A `tx_hash` present before `relayed_at`, ending `confirmed`, shows build then relay. Expect exactly one row per outcome for an order.

```sql
SELECT kind, state, tx_hash, fee, created, relayed_at, confirmed_at FROM xmr_settlements WHERE order_id = 'ORDER_ID' ORDER BY id;
```

**Record:** a row in [verification.md](verification.md#evidence-table) with tier "deployed instance on public stagenet", the tagged commit, and for each order its kind, transaction hash, heights and durations. Note whether `wallet.url` carries credentials: with mutual TLS alone, Digest was not exercised, so say so. Omit addresses, handles, onions and hostnames.

**Does not prove:** mainnet or real funds; fee or ring stress, reorganisations or a restart mid-send (the fakechain legs of section 9 cover some of these, on a fakechain); concurrency beyond one operator's reloads; a physical deadline release unless you ran one; that anyone but you can use the market.

### Leg: public physical journey (PM-3)

PM-3 is the "public-stagenet run of the physical journey (fund → ship with claim address → dispute → refund decision → refund confirmed) over Tor, recorded in docs/evidence/public-testnet/". **No runner.** Run on this market it is also A-562's physical refund.

1. The vendor publishes a physical listing, through the public onion in Tor Browser.
2. The buyer checks out, giving a stagenet refund address, and pays. Funding takes 10 confirmations; the order is **Paid**.
3. The vendor uses **Mark shipped** with a fresh stagenet claim address and their password. Nothing is built yet, and the release deadline is fixed.
4. The buyer opens a dispute at `/disputes` with **Submit dispute**, before that deadline. The vendor may reply.
5. On the private onion, an administrator who is not a party opens the dispute on `/moderator`, selects the refund outcome, writes the decision (20 to 5000 characters, stored unencrypted, so nothing private) and uses **Resolve dispute** with their password and code.
6. The watcher builds and relays the refund. Wait for 10 confirmations and check that the buyer's wallet received it and the claim address nothing. Read the row as in the A-562 leg, expecting one `refund`, `confirmed`. Reload the order as the buyer and as the vendor.

**Record:** a new directory `docs/evidence/public-testnet/<UTC date>-<commit, 12 hex>/stagenet-xmr-physical/README.md`, written by hand and saying so: tagged commit, node versions, UTC time and observed height of each step, funding and refund transaction hashes, fee, amounts, confirmations, and what ran over Tor (both browsers on the onions; the daemon only as far as your egress checks showed). Sanitize a separate copy, per [CONTRIBUTING](../CONTRIBUTING.md): no addresses, handles, onion addresses, hostnames, IPs, order IDs, screenshots, decision text or raw logs. Keep the originals, with their real timestamps, in the protected record. Run `python3 -m unittest discover -s tests -p 'test_public_evidence_privacy.py' -v`, which limits cloud JSON only, so read your prose too. Add a line to [its index](evidence/public-testnet/README.md) and cite it in verification.md.

**Does not prove:** anonymity, or that Monero traffic used Tor (`remote-tor` is your declaration, and your egress check is one observation); public refund timing beyond this one sample; a release to the vendor; a physical deadline; a shipment, which is only a form here; the [Manual Tor Browser pass](verification.md#manual-tor-browser-pass-owner), which has its own list.

### Leg: real outages (DA104-E1)

**No runner.** The tests behind these (`TestXMROutageMovesNearDeadlines` and others) use a database and a simulated wallet RPC, with no real outage. Run these after leg 3's claim has confirmed, with no settlement mid-send.

**(a) The application is down.** Fund a digital order, O, a couple of hours after leg 3's order, so that leg 3 finishes first, and let it sit **Delivered**. When its order page says the window ends in about 30 minutes, stop the application. Leave it down until the window has passed and over the threshold has elapsed, then start it.

```sh
/usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-stagenet
/usr/local/bin/opsecmkt deploy up --dir /srv/opsecmkt/market-stagenet
```

Expect, after the first watcher pass: O still **Delivered**; one order note, "Dispute window moved to … after an outage (Monero escrow)"; one notice to the buyer and one to the vendor, naming the outage; a new end 72 hours after that pass; and nothing completed. In the first moments the order page may not offer a dispute yet ([after an outage](#reconcile-a-restored-database-before-enabling-payouts)); after the first pass it should be offered and accepted. If you try that, use another order, since a dispute takes O out of (b).

**(b) Public ingress is down, and the watcher cannot see it.** About an hour. Stop the public Tor service on the gateway host, and confirm with an independent Tor client that the storefront no longer answers. Wait five minutes: expect no new note on O, since the application runs on and notices nothing. Then, from the operator's real shell on the application host, ask for the recheck. The database is remote, so there is no `db` container to run SQL in.

```sh
DATABASE_URL_FILE=/srv/opsecmkt-inputs-stagenet/database.url /usr/local/bin/opsecmkt -request-deadline-recheck
```

Over the private administrator onion, which stays up, check that `/admin/payments` shows the request (A-741), and copy what it says into the private record. Expect O's window, which now ends within 72 hours, to move once more on the next watcher pass: exactly one more note and one more notice to each party, and nothing completed. Keep ingress down for the rest of the hour, start Tor again, and run the same command once more now that ingress has returned. Expect O's window to move again on the next watcher pass, to 72 hours after that pass: one more note and one more notice to each party, and nothing completed. Read those notices as the buyer and as the vendor through the storefront. Finish O with **Confirm receipt** and a claim.

**Record:** a row each in verification.md with tier "deployed instance on public stagenet", stating the stop and start times in UTC, the threshold in force, the note and notice counts, the deadline before and after, and what `/admin/payments` said. Omit order IDs, handles and onions. A failure also goes in the [deployment notes](install/deployment-notes.md#issues-during-the-fresh-reset) as an issue, kept, not retried away.

**Does not prove:** that a completion can be undone (it cannot: an order that completed before the request stays completed); that anyone reads a notice, which is in the market only; a database outage, a clock jump or a VM pause; an outage longer than 72 hours, which needs the request repeated within every 72 hours.

### Leg: restore rehearsal (A-698)

Acceptance line: "Persistent data survives container replacement. Backup and restore instructions cover database records, deployment secrets, and any onion identity keys; a restore rehearsal is recorded." **No runner on these hosts**; `scripts/test-backup-restore.sh` and `scripts/test-tor-identity-restore.sh` are local. Use a spare encrypted host, the same one as the replacement leg, and a new empty database, isolated from users and payment services. Never restore onto, or run tools against, the live database or wallet.

1. Take the backup from a matching source checkout on the backup host, and export the configuration to an absent directory ([recovery](install/recovery.md)):

   ```sh
   export BACKUP_DATABASE_URL="$(cat /secure/opsecmkt/database.url)"
   AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh /secure/backups/stagenet.dump.age
   unset BACKUP_DATABASE_URL
   /usr/local/bin/opsecmkt deploy recovery export --dir /srv/opsecmkt/market-stagenet --recipient-public-key /srv/operator/offline-recovery-public.asc --out /srv/operator/stagenet-recovery-YYYY-MM-DD
   ```

2. Restore into the rehearsal database, which asks for the explicit `RESTORE` confirmation:

   ```sh
   export RESTORE_DATABASE_URL="$(cat /secure/opsecmkt/rehearsal-database.url)"
   AGE_IDENTITY=/secure/offline/backup-key.txt ./scripts/restore.sh /secure/backups/stagenet.dump.age
   unset RESTORE_DATABASE_URL
   ```

3. On the spare host, start one isolated application on loopback with the preserved application key and a private copy of the **current** retention ledger, with no Tor service and no wallet. Running the managed installation here instead would publish the live administrator onion from a second host, and its wallet URL names the live wallet.

   ```sh
   RETENTION_ENABLED=on RETENTION_STATE_DIR=/secure/rehearsal/retention-copy /usr/local/bin/opsecmkt serve --profile dev --addr 127.0.0.1:8080 --database-url-file /secure/rehearsal/database.url --app-secret-key-file /secure/rehearsal/app-secret.key --setup-token-file /secure/rehearsal/setup-token.key --cookie-secure false
   ```

   Expect retention replay to finish, the expected rows to be present, and `payments_recovery_required` still set. Stop it, then clear the gate as [above](#clear-the-gate), answering for the rehearsal only:

   ```sh
   DATABASE_URL_FILE=/secure/rehearsal/database.url /usr/local/bin/opsecmkt -clear-restore-gate YOUR_ADMIN_HANDLE
   ```

4. Stop the live wallet RPC briefly, copy the wallet directory to encrypted storage on the spare host, restart it, and open the copy with a separate stagenet wallet RPC there, using the original wallet secret. Compare balances with the live wallet. Remove that wallet RPC and copy afterwards under your retention policy.

**Record:** a row in verification.md and a line in the deployment notes, in role names: dates, whether row counts match, retention replay, gate state before and after, wallet balance match. Keep logs and copies in the protected record, and destroy the rehearsal database when finished.

**Does not prove:** the reconciliation of a restored application with its wallet: `deploy endpoint set` replaces only a database reference, so no supported command points an isolated application at a copied wallet, and that path stays with the fakechain `Restore` leg of section 9. Nor a provider point-in-time recovery, a restore holding real users' records, or offline seed decryption unless you also ran it.

### Leg: app-only redeployment (A-698)

Acceptance line: "An actual app-only container redeployment preserves the saved access and registration settings." CI has ticked that line for development mode, and its host half is still open, as the line says. **No runner on these hosts**: `scripts/test-native-deploy.sh` covers development mode on a CI engine. Take backups as [redeploying only the application](install/service-topology.md#redeploying-only-the-application) says, change the saved site-access and registration settings to values you can recognize, and build a second image from the tagged commit under another name so that its digest differs.

```sh
/usr/local/bin/opsecmkt deploy plan --dir /srv/opsecmkt/market-stagenet
/usr/local/bin/opsecmkt deploy status --dir /srv/opsecmkt/market-stagenet
/usr/local/bin/opsecmkt deploy redeploy --dir /srv/opsecmkt/market-stagenet --app-image SECOND_IMAGE@sha256:DIGEST
/usr/local/bin/opsecmkt deploy status --dir /srv/opsecmkt/market-stagenet
```

Expect the settings to survive a reload; the `tor-admin` container, its onion address, every volume, the retention directory and `operator.json` other than the image to be as before; no change on the database, payment, gateway or worker hosts; and an upload through the gateway and **Admin → Payments** readiness to still pass. Stay under the outage threshold, or expect its recheck.

**Record:** the deployment notes in role names, and update the sentence ending "on the owner's hosts is tracked as A-698" in [acceptance](acceptance.md) with the date and its verification.md row. Digests stay private.

**Does not prove:** an upgrade with a migration, since both images are one commit; a rollback; a change on any other host.

### Leg: host policy and pending boot (A-698)

Acceptance line: "Supported-host apply/confirm/rollback and pending boot are rehearsed with fresh allowed/denied SSH, gateway mTLS and actual Docker forwarding on every public address; independent public/private Tor clients pass." **No runner**; `scripts/test-host-bootstrap.py` proves only packet rules in a namespace.

The profile is an Ubuntu 24.04 application host on static systemd-networkd with no other firewall manager ([bootstrap](install/server-bootstrap.md)). If the existing host is refused, record the refusal and do not disable anything to pass it: that host needs its own reviewed policy, and the line stays open for it. Rehearse on the spare host first, and keep an independent authenticated console open throughout, since a wrong policy locks you out until rollback. Every apply needs a fresh operator SSH session and sudo proven beforehand.

```sh
./opsecmkt host plan --config host.json --out host-plan
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host apply --config host-plan/host.json --operator-access-verified
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host confirm --config host-plan/host.json
sudo ./opsecmkt host rollback
```

1. **Timer rollback.** `apply`, then do nothing for the five minutes. Expect the previous table and managed files back.
2. **Explicit rollback.** `apply`, then `rollback`. A repeat changes nothing.
3. **Pending boot.** `apply`, then reboot before confirming, and unlock the root by hand. Expect rollback to run before the candidate policy and before Docker and containerd.
4. **Confirm.** `apply`, then, from a **new** SSH connection within five minutes, `confirm`. Its connection tuple must differ from `apply`'s.
5. **Probes**, as in [verify allowed and denied connections](install/server-hardening.md#4-verify-allowed-and-denied-connections), each fresh, on every assigned IPv4 and IPv6 address: approved SSH passes and unapproved SSH does not; the gateway's `/healthz` passes with verified TLS, and its backend mutual TLS works only with the gateway identity; each Docker-published port is unreachable from an independent public address. A missing IPv6 probe is pending, not a pass.
6. **Independent Tor clients**, two fresh ones: the public onion answers `/healthz` and returns `404` for `/setup` and `/admin`; the authorized client reaches the private `/login`, and the unauthorized one gets `0xF4`. A timeout or circuit error is inconclusive, never a denial ([diagnosis](install/tor.md#diagnosing-a-timeout-after-entering-the-client-key)).

Then take the same steps on the production application host, at a time no order waits on it.

**Record:** the source, destination, port, expected and actual result of each probe stay private; the deployment notes get the matrix by role and count, as the 2026-10-16 checkpoint did, plus each failure, kept. verification.md gets a row.

**Does not prove:** the provider firewall or the database, payment and gateway hosts' policies, which this profile does not manage; that the policy still holds after a later change; lasting onion availability; independent network attestation, since `confirm` checks session metadata.

### Leg: replacement application host (A-698)

Acceptance line: "A replacement production app host restores retained configuration, identities and current external retention state on verified encrypted storage, rebinds explicitly, installs against retained services, and passes isolated restore and locked-boot checks." **No runner**: the configuration-bundle round-trip tests are partial evidence. Follow [recover application configuration](install/recovery.md) and [rebuilding or replacing only the application](install/binary.md#rebuilding-or-replacing-only-the-application). The outage threshold will probably pass, so expect the recheck. Allow the spare host in the database's, payment proxy's and gateway's rules first.

1. On the spare host, verify LUKS2, static addressing, host policy and the isolated restore above. Export the configuration again if it has changed.
2. **Fence the old host**, and make sure it cannot resume the application or its onion: an administrator onion must never run from two hosts.

   ```sh
   /usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-stagenet
   ```

3. Decrypt offline and stream the recovery document directly to the new host. Copy the current retention directory through an authenticated encrypted channel. If hostnames change, reissue the leaf certificates, keeping CA keys offline, and install them with `deploy tls replace`. Then:

   ```sh
   /usr/local/bin/opsecmkt deploy recovery restore --dir /srv/opsecmkt/market-stagenet --old-host-fenced
   /usr/local/bin/opsecmkt deploy storage bind --dir /srv/opsecmkt/market-stagenet
   /usr/local/bin/opsecmkt deploy plan --dir /srv/opsecmkt/market-stagenet
   /usr/local/bin/opsecmkt deploy doctor --dir /srv/opsecmkt/market-stagenet --start-check
   /usr/local/bin/opsecmkt deploy install --dir /srv/opsecmkt/market-stagenet
   ```

4. Expect the same administrator onion, retained from `onion-admin/` into a fresh volume; a populated identity volume with another address refuses, and is never overwritten. Point the gateway at the new private address, and repeat the probes and Tor clients of the previous leg, a gateway upload, and a small funded order that reaches a confirmed settlement.
5. **Locked boot.** Reboot the replacement, unlock by hand, and expect services to return with no other action, the storage check that `install` set up passing, and readiness again. Re-acknowledge reboot control if you use it.

**Record:** the deployment notes and verification.md in role names: dates, each step's result, and that the old host stayed fenced. Keep it fenced until the owner decides its fate, which is another approval.

**Does not prove:** the old host's erasure; a move of the database, payment or gateway hosts; that no one kept a copy of a key; recovery with the old host unavailable, unless you rehearsed it that way.

### Recording the results

Evidence follows [CONTRIBUTING](../CONTRIBUTING.md): inspect and sanitize a separate copy for secrets, user data and operational identifiers, and keep the protected original with its authentic date. Never fabricate or reshape a timestamp, and keep failures alongside passes.

- **Protected operator record:** approvals, exact resource names, addresses, onions, image digests, key locations, raw transcripts and logs. A-669 asks for digests in the deployment notes, but that file's own rule keeps them here; publish there the `libssl3` version and that the digests are recorded privately.
- **[Deployment notes](install/deployment-notes.md):** a dated checkpoint in role names only.
- **[verification.md](verification.md#evidence-table):** one row per leg. A result on these hosts is its own tier, never promoted to or from a simulated wallet or fakechain row.
- **[Public evidence](evidence/public-testnet/README.md):** PM-3, in the directory named there.
- **[Acceptance](acceptance.md):** tick a line only once its record exists, citing that record. A partly run leg leaves the line open, with what remains named.

The whole set does not prove that mainnet works, that real funds are safe, or anonymity. The decision to take real funds stays separate, and this market stays on stagenet until the owner decides otherwise.
