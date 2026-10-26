# Monero test-network payments runbook

This runbook covers Monero escrow on a local fakechain or stagenet. Mainnet moves real funds and is the legacy `scripts/install.sh` default for local nodes (native `opsecmkt deploy init` defaults to stagenet); no real-funds mainnet transaction is recorded. Use `--monero-network stagenet` for rehearsal. Escrow is custodial and off by default. See [Enabling Monero payments](install/enable-payments.md).

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
7. Check orders funded or settled after the backup, compare balances and transaction history, and make another encrypted backup of the reconciled state. Re-check the Admin settings against your operator record: the restore rolls back every setting saved after the backup or recovery point, such as **New escrow checkouts**, site access, registrations, the vendor bond amount and approval mode in **Admin → Vendor applications** and the held-release refund period for banned vendors (`ban_hold_refund_days`, **Admin → Payments**, A-652f), and undoes bonds recorded as received after that point, bans and unbans, and held releases refunded to buyers after that point (check banned vendors' orders against your operator record before refunding one again). The fee address and rate (`ESCROW_XMR_FEE_ADDRESS`, `ESCROW_XMR_FEE_BPS`) are host configuration, so a database restore does not roll them back; confirm the host values instead.
8. Stop the app, then [clear the gate](#clear-the-gate) with the host command below, which refuses while the app runs. Start only the app again and verify Admin readiness. Clearing the gate asserts that reconciliation is complete; it does not discover missing obligations or release individual holds.
9. Once reconciliation and readiness checks pass, run `docker compose up -d` with the preserved profiles to restart public Tor ingress and the enabled mirror. On mirror deployments, check `docker compose ps tor-mirror` and confirm its saved onion address answers. On clearnet deployments, reopen the reverse proxy after these checks.

For the SQL checks in these guides, use the operator's protected database connection. For the bundled database, define
`db_sql` as below; an external database needs its protected `psql` connection instead.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt "$@"; }
```

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
`WrongSecret`, `Destroy`, `RefundBelowFee` and `Bond`, each `TestXMREscrowRegtest<Leg>` in `internal/market/`; what each proves is listed in
[per-order escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain). A pass is evidence for the code and the 0.18.5.1
nodes on a local fakechain, not for stagenet, testnet or mainnet.

## 10. Monero escrow on stagenet

Since A-449, Monero escrow (`ESCROW_XMR=on`, off by default, custodial) may run on Monero stagenet as well as on a local
fakechain: the escrow daemon must report nettype `stagenet` with hard fork 16 or later, the escrow wallet-rpc must run with
`--stagenet` against the same daemon, and refund, claim and fee addresses must be stagenet ones (5… or 7…). Setup, the
network pin and the sync guard are in the [operator guide](operator-guide.md#monero-escrow). The [public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) proves one funded digital claim with real peers, fees and confirmations.
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
by the [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) (A-447/A-491), scoped to one digital claim. The
offline fork proves the stagenet address formats, the refusal of an old hard fork and the escrow sync guard
against real daemons; the network pin is covered by the database tests against the simulated daemon
(`TestXMREscrowNetworkPinMismatch`, `TestXMREscrowNetworkPinBackfillAndRepin`), not by the fork.

The separate manual public runner is `scripts/test-escrow-xmr-public-stagenet.py`; its guarded digital-claim journey
uses actual confirmations and a fetched public explorer anchor. See the [Monero operator guide](operator-guide.md#monero-escrow)
and the script header for its dedicated funded wallet, empty escrow wallet directory and Docker/RPC requirements.
Select an exact HTTPS block URL on `stagenet.xmrchain.net` or `xmr-stagenet.librenode.com`; both the runner and Go
test fetch the selected host's block API before wallet work. An unavailable or mismatching source refuses the run.
Its source/offline checks are not a funded public result. The recorded public digital claim passed with ten confirmations on both funding and release; it does not establish public refund timing, physical-order deadlines or deployment readiness.
