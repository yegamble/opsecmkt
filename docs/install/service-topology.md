# Separating services and reusing dependency hosts

Keep PostgreSQL, Monero and upload processing independent of the application so an app rebuild can reuse their existing state. This guide describes production host boundaries, starting hardware budgets and the app-only redeployment procedure.

## Before starting

You need administrative access to the intended hosts, their firewall rules and service managers. Review the [standalone binary setup](binary.md), [external PostgreSQL procedure](external-database.md) and [production checklist](production-checklist.md). Use stagenet for development and rehearsal; mainnet is an explicit real-funds choice with no recorded mainnet deployment verification.

The production controller manages the application host. It does not provision remote servers, install their operating systems, configure their firewalls or rewrite existing database, Monero or scanner services. Prepare and verify those services before connecting the app. Existing services can be reused when they meet the required network, authentication and compatibility contracts.

## Choosing service boundaries

Use separate operating-system hosts for these five roles:

| Host | Processes | Persistent material |
| --- | --- | --- |
| Application | `opsecmkt serve`; optional private administrator ingress | Private operator directory or verified RAM-unlocked instance, application key root, database/RPC credentials, stable Monero wallet secret, public recovery key, leaf TLS identities, separately persistent retention state and pinned report verification key; private Tor deployments also retain managed `onion-admin/` identity files and the app-host `tor_admin_identity` volume. |
| Database | PostgreSQL | Market database, normalized images, WAL, database TLS identity and database-side access rules. |
| Payments | `monerod`, dedicated `monero-wallet-rpc`, required authenticated RPC front end | Blockchain, escrow wallet files, RPC identities and authentication configuration. The daemon and wallet RPC may share this host. |
| Upload/scanner | Separate gateway, ClamAV and public Tor or HTTPS ingress processes/containers | Gateway client identity, separate public onion or HTTPS identity, scanner configuration/signatures and bounded temporary scan storage. Keep private onion-key recovery copies encrypted/offline. No database login, wallet credentials, application master keys or wallet files. |
| Version worker | `opsecmkt versions-worker`; optional dedicated Tor SOCKS client | Dedicated report-signing seed, operator-supplied version inventory and public signed reports. No application, database or payment secrets. |

These are independent processes. The binary has an application role, an upload-gateway role and a versions-worker role; it does not embed PostgreSQL, Monero or the ClamAV engine.

Optional [moderation](../moderation-agent.md) adds a trusted controller worker
and a separate, untrusted inference process. The controller needs a database
connection for bounded snapshots, durable work and proposal records. The model
must receive none of that controller's credentials or environment. Keep the
model behind a local Unix socket with no network, application secret mounts or
wallet access. This is a separate process boundary, not proof that a shared
kernel or host administrator cannot cross it. The market requires no model to
start or serve requests.

For development, those processes may share one host and use separate containers. Containers on one Docker engine still share a host kernel and daemon authority. Separate container networks, users and resource limits reduce access and resource contention; they do not provide separate host compromise boundaries. Docker describes the [kernel and daemon security boundaries](https://docs.docker.com/engine/security/).

A separate VM provides a separate guest kernel and daemon, but its hypervisor and infrastructure administrators remain a shared trust boundary. Decide whether separate physical machines or infrastructure providers are needed for your threat model. Different DNS names, IP addresses or containers cannot attest physical separation or guarantee anonymity.

## Starting hardware budgets

**Every hardware figure in this table is a starting engineering estimate, not a benchmark, capacity guarantee or measured production minimum.** Budgets include room for the operating system and ordinary service overhead. Measure CPU, resident memory, storage growth, disk latency, scan latency and rejected work under your own workload before relying on them.

| Host | Starting CPU | Starting RAM | Starting SSD budget | Reassess when |
| --- | --- | --- | --- | --- |
| Application | 2 vCPU | 2 GiB | 20 GiB, excluding remote database/media and off-host backups | Image serving, password checks, database waits, connection limits or image-layer/log storage approach their bounds. |
| Database | 2–4 vCPU | 4–8 GiB | Derive from actual database/media bytes, WAL, temporary work, retained backups and restore workspace; see below | Image inventory, indexes, write volume, backup retention or query latency grows. |
| Payments | 4 vCPU | 8 GiB | Initial budget of 320 GiB for a pruned mainnet node or 750 GiB for a full node; add wallet/backup space and revise for current chain growth | Initial synchronization, wallet rescans, chain growth, settlement volume or disk free space becomes limiting. |
| Upload/scanner | 4 vCPU | 8 GiB | 40 GiB for OS/images/signatures and controlled logs; size temporary scan space separately | Concurrent scans, signature reloads, image processing or Tor load exhausts the configured budgets. |
| Version worker | 1 vCPU | 1 GiB | 10 GiB | OS/image updates or an optional Tor client exceed the available headroom. Reports themselves are small. |

ClamAV's official documentation recommends at least 3 GiB RAM for its standard signature database; its Docker guidance prefers 4 GiB. Those figures cover ClamAV itself. Leave additional memory for the gateway, image normalization, Tor and the OS. See [ClamAV system requirements](https://docs.clamav.net/#recommended-system-requirements) and [ClamAV Docker memory guidance](https://docs.clamav.net/manual/Installing/Docker.html#memory-ram-requirements).

Monero's official node guide reports approximately 105 GiB pruned and 265 GiB full chain data **as of 2026-10-05**, with larger available-SSD recommendations for growth. The table rounds those recommendations upward and includes no prediction of future chain size. Verify the [current Monero node storage guidance](https://docs.getmonero.org/running-node/monerod-systemd/#assumptions), actual data-directory usage and free space when provisioning. Stagenet has a different dataset; measure it rather than applying a historical mainnet figure.

### Deriving database storage and memory

Include normalized photo/avatar data in the database estimate: the current application stores those bytes in PostgreSQL. On the database connection, this read-only query reports the current database size:

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

Budget SSD capacity for that live database, expected growth over the maintenance interval, WAL accumulation, query/migration temporary space and any backups kept locally. Reserve additional workspace for the restore procedure you will actually use. Keep recovery copies off the database host; a second copy on its disk is not an independent backup. PostgreSQL documents [database size functions](https://www.postgresql.org/docs/17/functions-admin.html#FUNCTIONS-ADMIN-DBSIZE) and warns that failed archiving can fill WAL storage and stop the server in its [continuous archiving guidance](https://www.postgresql.org/docs/17/continuous-archiving.html).

Tune memory to the actual PostgreSQL workload and connection count. PostgreSQL suggests `shared_buffers` around a quarter of RAM as a starting point on a dedicated database server; query work memory and the OS cache need their own budget. See [PostgreSQL resource consumption](https://www.postgresql.org/docs/17/runtime-config-resource.html). Increasing a container memory limit alone does not tune the server.

## Allowing only required service flows

The following matrix is the intended production policy. Ports are examples from the binary setup; use the ports and certificate names configured for your services. Apply equivalent restrictions to IPv4, IPv6, host networking and container-published ports. Established response traffic is implicit.

| Initiator | Destination | Required transport and access | Exposure to refuse |
| --- | --- | --- | --- |
| Visitor | Upload/ingress host | Public HTTPS front end, or the Tor onion service forwarding to a loopback gateway listener | Direct visitor access to the app, database, payment RPC, ClamAV or upload backend. |
| Gateway | App public-handler listener, e.g. `8080` | HTTPS with hostname/CA verification; firewall permits only the gateway's private path | Arbitrary network sources and caller-supplied authentication/forwarding headers. |
| Gateway | App upload backend, e.g. `8443` | Mutual TLS using the dedicated gateway client identity and trusted backend certificate | Missing, invalid or unrelated client identities; upload-CA certificates without clientAuth or that are CAs (404); an `UPLOAD_CA` shared with the database or Monero CA (startup refusal); routing this listener through public ingress. |
| App | PostgreSQL, commonly `5432` | Private route, database role authentication and `sslmode=verify-full` | Gateway/scanner/version-worker access and public database ingress. |
| App | Dedicated Monero wallet and restricted daemon RPC | Private HTTPS with verified certificate names and credentials or a configured mutual TLS identity | Public RPC access and gateway/scanner/version-worker access. |
| Wallet RPC | Its Monero daemon: start `monero-wallet-rpc` with `--daemon-address` naming the same `monerod` that `ESCROW_XMR_DAEMON_URL` reaches, and with `--shared-ringdb-dir=WALLET_DIR/.shared-ringdb` so its ring database stays in its wallet directory ([stale-ring break-glass](../operator-guide.md#stale-ring-break-glass-a-618)) | Loopback or an explicitly authenticated private service connection | Unrelated wallets controlling the market's escrow wallet files; a wallet attached to a different daemon. |
| Gateway | ClamAV, commonly `3310` | Loopback or an unpublished scanner-only container network on the upload host | Internet or application-host access to unauthenticated clamd TCP. |
| Monero daemon | Monero peers | Deliberately configured peer transport and egress policy | Treating the storefront's Tor mode as proof that Monero peer traffic uses Tor. |
| Version worker | Official release sources | HTTPS to the fixed PostgreSQL, Go, Tor and official GitHub release endpoints; optional explicit Tor SOCKS5 | Application/database/payment RPC credentials or access, and arbitrary configured release URLs. |
| Reviewed report-transfer job | App report directory | Transfer only signed `report.json`; stage and atomically replace it with a narrowly authorized identity | Access to app secrets, the Docker socket, the worker seed or the separately pinned verification key. |
| Moderation controller, when enabled | PostgreSQL and dedicated local inference Unix socket | Database authentication plus narrowly permissioned socket directory; no model TCP listener | Model access to database, wallet, application environment, administrator keys or external network. |

The escrow wallet RPC relays settlements through the daemon named by its `--daemon-address`, while the application reads transaction locations and key-image spends from `ESCROW_XMR_DAEMON_URL`. They must be the same `monerod`. The application refuses a wallet more than 10 blocks from that daemon but cannot prove that both use one daemon, and a payment-host Tor egress check made for one daemon says nothing about another.

`monerod` and `monero-wallet-rpc` write their own log files besides standard output (`bitmonero.log` in the daemon's data directory; for the wallet RPC, the file `--log-file` names, or its default), and by default keep up to 50 files of 100 MB each. The deployment controller does not manage the payment host, so cap them there as the bundled containers do ([Resource limits and log rotation](../operator-guide.md#resource-limits-and-log-rotation), A-538): for example `--max-log-file-size=10485760 --max-log-files=3` for the daemon and `--max-log-file-size=1048576 --max-log-files=2` for the wallet RPC, at the default log level. These logs can name peer IP addresses, transaction IDs, wallet addresses and key images.

Keep administrator ingress on its [separate authorized path](tor.md#private-administrator-onion). The public gateway does not authenticate an administrator merely because it forwards a request. Service operators also need deliberate DNS, time synchronization, package/image maintenance and backup paths; document those exceptions separately from runtime application traffic.

Use the [production application initialization flags](binary.md#initializing-the-production-application-host) to import the prepared service endpoints and public trust roots. `--mode prod` records the production profile; native `serve` uses `--profile prod`. Startup rejects known local/shared endpoint declarations and missing production protections. It cannot inspect another host's daemon configuration, detect every DNS alias, prove firewall behavior or attest the underlying machines.

## Running the upload and version workers

### Upload host

Provision the upload host independently: install or deploy its reviewed gateway binary/image, ClamAV with current signatures, public Tor/HTTPS ingress, service accounts, leaf identities, bounded temporary storage, firewall rules and supervisor. The app-host controller does not create these remote services, install their packages or configure their boot units. Run the gateway under a service manager or in its own container. Keep ClamAV and Tor/HTTPS ingress in separate processes or containers on that upload host. The [gateway command example](binary.md#initializing-the-production-application-host) connects the prepared services to the app's HTTPS listener and mutual-TLS upload backend.

Original uploads terminate at the gateway. Scanning and normalization complete there before the app receives the bounded JPEG result. Keep scanner temporary files on its bounded temporary filesystem and prevent original uploads from reaching application storage. Preserve scan failure/staleness refusals and resource limits; a clean malware verdict is not proof that an image is harmless. The gateway also reports its scanner state (versions, signature age, last check and scan, refusal counts; no hosts or file data) to the app's upload backend every 30 seconds with its client certificate; without a report for 3 minutes the app shows the scanner state as unknown and pauses its upload forms ([operator guide](../operator-guide.md#photo-uploads-and-the-malware-scanner)).

The same-origin gateway proxies ordinary account requests as well as uploads. It therefore handles browser credentials and session cookies. A gateway compromise can affect those users even without database access. Host separation reduces the consequences of image-parser or scanner compromise; it does not make the gateway an untrusted relay.

### Version-worker host

Create a dedicated report-signing key on the maintenance host. The directories below are examples owned by its service account:

```sh
install -d -m 700 ./worker-private
install -d -m 755 ./worker-public ./reports
./opsecmkt keys generate audit \
  --out ./worker-private/versions.seed \
  --public-out ./worker-public/versions.pub
./opsecmkt versions-worker \
  --report ./reports/report.json \
  --signing-key ./worker-private/versions.seed \
  --interval 24h
```

The last command checks immediately, then remains running. Supervise it with the host's service manager. Add `--once` for a single refresh. Add `--socks5 127.0.0.1:9050` when using a separately configured local Tor SOCKS client; requests and DNS then use SOCKS. Without that flag, the worker uses direct HTTPS and ignores ambient HTTP proxy settings.

Supply each known installed version with a repeated `--current SERVICE=VERSION` flag. Supported service names are `postgresql`, `monerod`, `monero-wallet-rpc`, `tor`, `clamav` and `go`; versions must be numeric stable versions. These values are **operator-supplied inventory**, not remote measurements. The admin panel substitutes observed app Go and connected PostgreSQL versions, and the ClamAV engine version from the app's cached scanner state (integrated scanning) or the upload gateway's latest scanner report (isolated uploads). With Monero escrow on, it also substitutes the monerod release the escrow watcher read from `get_info`; a restricted daemon RPC hides it, and monero-wallet-rpc reports only its RPC interface version, so those releases stay reported or unknown. Tor is never observed. Without this worker's report the panel still shows installed versions and health, and names this setup step in place of the upstream comparison ([Service status and health](../operator-guide.md#service-status-and-health)).

For a separately selected moderation runtime, `--current llama.cpp=bNNNN`
explicitly adds operator-reported llama.cpp inventory and its fixed official
GitHub release source. Without that explicit entry, the worker makes no extra
runtime-release request and old signed reports remain accepted. The
administrator-only panel shows this row only from the authenticated report;
it does not contact inference or attest the installed runtime. Keep this
maintenance worker separate from networkless inference.

Transfer only the signed report to the directory selected by `--dependency-report-dir` during app initialization. Import `versions.pub` separately with `--dependency-report-public-key`; it becomes a trust pin outside the replaceable report directory. Keep `versions.seed` outside report delivery and off every application, database, payment and upload host. A missing, invalid or older-than-72-hour report produces an administrator warning.

The worker refreshes reports, not software. It never upgrades the app, dependencies or signatures. Periodic writes replace only a report authenticated by the same signing key. Intentional signer rotation requires a new report path, independently changing the app's trusted public key and reviewing delivery permissions.

## Reusing services when rebuilding the app

Keep dependency service lifecycles separate from application releases:

| Component | What survives an app redeploy | When to change it separately |
| --- | --- | --- |
| PostgreSQL | Database, images, roles, certificate and endpoint | Planned database upgrade, credential/certificate rotation, capacity or recovery work. App startup can still apply its schema migrations. |
| Monero | Chain data, network selection, escrow wallet files, dedicated RPC services and credentials | Planned Monero upgrade, wallet recovery, RPC identity rotation or storage work. |
| Upload/scanner/Tor | Gateway process, clamd process/signatures, ingress identities and routing | Reviewed gateway/protocol upgrade, signature refresh, TLS renewal or ingress maintenance. |
| Version worker | Worker process, report-signing identity, inventory inputs and report delivery | Worker upgrade, inventory change, signing-key rotation or release-feed maintenance. |
| Optional moderation | Durable database jobs/proposals, separately supervised controller/model, reviewed weights and private socket configuration | Explicit worker/runtime upgrade, policy or model-pin change, review-key rotation or resource-budget maintenance. Do not start or reprovision it as an app redeploy side effect. |
| Private app installation | Endpoints, keys, project/context identity, settings and report verification pin; current retention state remains separately persistent | Explicit configuration change or tested recovery; retain this state through app rebuilds. RAM instances must be unlocked again after restart. |
| Private administrator Tor | Separate managed service identity, authorized client snapshot, Unix ingress contract and persistent `tor_admin_identity` volume | Deliberate authorization/identity maintenance. Networkless initialization verifies the selected identity and prepares records/ownership; app-only redeploy does not run missing dependencies or move the named volume. |

Reuse these dependencies for the **same installation**. Development and production must retain separate databases, credentials and wallets. Do not point a second independent installation at the same custodial wallet RPC or treat a running production database as a test fixture.

### Redeploying only the application

1. Build or obtain a reviewed application image for the desired revision. Record its immutable `IMAGE@sha256:DIGEST` reference and check migration and gateway compatibility.
2. Take and verify the documented [backups](../operator-guide.md#backups-and-recovery), including the private installation, database, wallet state, external retention state and relevant identities. An application-only restart can still migrate the database.
3. Inspect the existing installation with `deploy plan` and `deploy status`, always passing the same `--dir DIR`. Keep the saved project and Docker context.
4. Replace only the application image. Replace `DIR` and `IMAGE@sha256:DIGEST` with the existing installation directory and reviewed image reference:

   ```sh
   ./opsecmkt deploy redeploy --dir DIR \
     --app-image IMAGE@sha256:DIGEST
   ```

5. Check `deploy status`, the administrator service/update panel, gateway upload behavior and payment readiness on the configured network. Rehearse transaction workflows on stagenet; do not send real funds as a deployment test.

`redeploy` targets only the app service with dependency startup disabled. It preserves saved endpoints, secrets, worker images, project identity, retention state, report trust pins and data volumes. Leave the original database, payment, upload/scanner/public Tor and version-worker processes and their existing configuration running on their own hosts. App redeployment does not send them lifecycle or configuration commands, reinstall the database, resynchronize Monero or rotate keys. `deploy up` instead resumes the installation's configured services; use `redeploy` when the intended change is app-only.

If startup fails, the requested app digest remains saved for inspection and retry. Check the error and migration state before retrying or choosing another compatible digest. No automatic image or schema rollback occurs. Do not delete the installation directory or rerun `init` to recover an existing market.

Replacing the app's entire host requires a separate recovery plan: restore/unlock the protected configuration and current external persistent retention state, preserve or deliberately reissue its TLS identity, verify private routing and permissions, and stop the old app before starting the replacement. With private administrator Tor, retain the selected managed identity files or migrate the matching `tor_admin_identity` volume. New RAM bundles can include that identity triple, but never the whole volume. Run `install`/`up` to prepare a fresh matching volume and create a missing administrator service before app-only `redeploy`. Populated mismatched/partial volumes refuse startup; legacy configurations without a managed source require explicit preservation/adoption of their existing identity. Existing database, Monero, public upload/Tor and version-worker hosts can remain in place. Check restored data and payment reconciliation requirements before enabling settlements. See the [RAM and emergency procedure](ram-secrets-and-emergency.md) for temporary secret placement and its limits.

## Remaining trust and recovery boundaries

The app keeps online database/RPC credentials and keys needed for its work. A compromised running app can use them. Monero escrow is custodial: separating its host does not remove the application's authorized ability to request spending. Keep the private seed-decryption key offline and give the app only its public half.

Preserve and test backups for each role independently. Tor ingress identities, wallet files, database data and the private operator directory have different recovery requirements. Encryption at rest protects different threats from a live compromised host. None of the hardware budgets, topology declarations or signed version reports establishes anonymity, physical host separation or production performance.

Keep public storefront and private administrator Tor keys distinct. Both service keys are online while their respective Tor process runs; encrypted offline copies are recovery material. Managed Tor identity volumes remain persistent even with a RAM-unlocked operator directory, and emergency RAM cleanup does not remove their keys. Generating or importing an identity does not publish an onion service. See the [Tor identity and backup guidance](tor.md#choose-and-retain-separate-service-identities).
