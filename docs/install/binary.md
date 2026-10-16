# Running the standalone binary

The `opsecmkt-linux-amd64` and `opsecmkt-linux-arm64` release files include the application, HTML/CSS assets, migrations, deployment commands and key tools. Running `serve` needs PostgreSQL; running `deploy` also needs Docker Engine and Docker Compose 2.17 or later. No source checkout or Go compiler is needed on the target server.

The release workflow builds these binaries after CI and attaches them to a draft release. This source change does not publish or deploy a release. Verify the matching entry in `SHA256SUMS`, make the binary executable, and rename it to `opsecmkt` if convenient. `./opsecmkt --help` lists commands, and `./opsecmkt serve --help` lists runtime flags. Existing host commands, including `-bootstrap-admin`, `-status`, `-reset-admin-password` and `-export-audit`, remain available.

See the [complete CLI reference and Bash migration matrix](cli-reference.md) for every command, flag, default, key operation and remaining script dependency.

## Starting guided setup

In a terminal, run `./opsecmkt` with no arguments. The guide can create a new deployment, generate/import an onion identity, or manage an existing installation. `./opsecmkt setup` selects it explicitly. Bare `init`/`deploy init` also open it when both stdin and stdout are terminals; any configuration flags select the normal noninteractive command instead. Headless no-argument startup keeps its existing server behavior. Use explicit `serve` for supervised services.

Defaults favor development, loopback binding, stagenet and disabled escrow. Enter accepts a displayed default; `?` explains a question; `quit`, EOF or Ctrl-C cancels. Secret questions ask for private file paths, never their contents. Review the equivalent command, select `edit` to change cached answers, and use `:clear` to remove an optional cached value. Both configuration creation and the separate startup offer default to **no**. Successful save prints the generated/imported key paths, exact setup-token location, backup checklist and next command. See [guided interactive setup](cli-reference.md#guided-interactive-setup) for the full behavior and advanced flag alternatives.

## Choosing a topology

Development may use one server. Production uses separate application, PostgreSQL, Monero, and upload/scanner hosts. Monero's dedicated wallet RPC and restricted daemon RPC may share their Monero host. Keep ClamAV beside the upload gateway, away from the application and database. Never publish database, wallet RPC, daemon RPC, ClamAV, or the application's upload backend to the internet.

The production profile requires verified TLS for PostgreSQL and both Monero RPC endpoints, authentication for Monero, HTTPS for the application proxy connection, and mutual TLS for the upload backend. It rejects a local database, local Monero or local ClamAV deployment. It also rejects matching configured host identities across those layers. Hostnames alone cannot prove physical separation: verify placement, aliases, firewall rules, and private routing yourself.

The gateway is the public HTTP front door: route the entire public onion or HTTPS site through it. It proxies ordinary requests to the app and intercepts raw photo/avatar uploads for scanning and normalization. The app's production listener refuses direct raw uploads before reading their body. The gateway has no application database credentials, application key root, Monero credentials, or wallet secret. It does handle browser session cookies, so gateway compromise remains security-sensitive. A clean malware scan cannot guarantee a harmless image.

A compromised running application can access its configured keys and data. Encrypted backups and offline recovery keys protect different boundaries. Monero escrow remains custodial, and mainnet has real funds with no recorded mainnet run. Development and verification use stagenet only.

## Running directly with flags

Create a UTF8 PostgreSQL database using the [external database procedure](external-database.md), and put its connection URL in a private one-line file. Native serving needs no Docker image:

```sh
./opsecmkt keys generate secret --out ./app-secret.key
./opsecmkt keys generate secret --out ./setup-token.key
./opsecmkt serve --profile dev --addr 127.0.0.1:8080 \
  --database-url-file ./database.url \
  --app-secret-key-file ./app-secret.key --setup-token-file ./setup-token.key \
  --cookie-secure false
```

This development example uses loopback HTTP and stagenet. Production serving requires the same separate hosts, verified TLS, isolated upload gateway and private files as the managed deployment below. Use a service manager to restart and stop a native process. The binary does not install itself as a system service.

## Initializing a development deployment

Use a reviewed application image built from the matching source revision. Supply its immutable `IMAGE@sha256:DIGEST` reference; the tool never invents or silently selects an application image. The image must use the repository Dockerfile contract: `/app/server` is its entrypoint. PostgreSQL defaults to the same pinned PostgreSQL 17 image as the existing Compose deployment.

Replace `YOUR_APP_IMAGE_WITH_SHA256_DIGEST` before running:

```sh
./opsecmkt deploy init --dir ./market-dev --mode dev \
  --app-image YOUR_APP_IMAGE_WITH_SHA256_DIGEST
./opsecmkt deploy plan --dir ./market-dev
./opsecmkt deploy install --dir ./market-dev
./opsecmkt deploy status --dir ./market-dev
```

The Docker context is explicit and saved in configuration: `default` unless `--docker-context NAME` is supplied during initialization. On Docker Desktop, select its local context explicitly if necessary. Ambient `DOCKER_HOST` and `DOCKER_CONTEXT` never redirect deployment commands. Inspect a selected context before using it; a named context may itself target a remote engine.

The app listens on `127.0.0.1:8080`. The database has no published port. `init` creates private configuration and distinct random application, setup, audit-signing, and database secrets, then exits without starting services. Keep the installation directory and its keys: `init` refuses any existing target directory, and `install` refuses pre-existing containers or volumes belonging to another installation's project.

Complete first-run setup at `http://127.0.0.1:8080/setup` using the token in `market-dev/secrets/setup-token`. Treat that file as a password; do not paste it into reports or chat. `deploy up` resumes the same installation without generating new keys.

Optional development services require explicit digest-pinned images:

| Flags | Effect |
| --- | --- |
| `--clamav-image IMAGE@sha256:DIGEST` | Enables integrated photo scanning; the image must include current signatures. |
| `--exposure tor --tor-image IMAGE@sha256:DIGEST` | Runs a Tor image matching `deploy/Dockerfile.tor`; removes the host app port. The onion identity remains in `tor_identity`. |
| `--escrow-xmr --monero-image IMAGE@sha256:DIGEST --monero-wallet-image IMAGE@sha256:DIGEST --monero-seed-key PUBLIC.asc` | Runs pruned stagenet Monero and a dedicated authenticated escrow wallet RPC. The image contracts match the existing `compose.nodes.yaml`. |
| `--database-url-file FILE` | Uses an existing PostgreSQL database instead of starting the local one. |

The development scanner runs in a separate container on an internal scanner network. This is a development convenience; it does not provide production host separation. Tor inbound exposure does not by itself route outbound Monero traffic over Tor. The existing relay hold remains active until the operator configures the documented Tor routing or explicitly accepts direct exposure.

## Importing and generating keys

Generate secrets directly into private files:

```sh
./opsecmkt keys generate secret --out ./app-secret.key
./opsecmkt keys generate audit --out ./audit-signing.key --public-out ./audit-signing.pub
```

Pass existing files to `deploy init` using `--app-secret-key-file`, `--setup-token-file`, `--audit-signing-key-file`, and `--monero-wallet-secret-file`. Imports are copied into the private installation, so later changes to the source file do not rotate the installation's keys. Secret imports require regular, non-symlink files with mode `0600` or stricter. URLs and passwords remain out of the process command line.

Generate OpenPGP recovery keys on an offline machine:

```sh
./opsecmkt keys generate pgp --name 'Escrow recovery' \
  --public-out ./escrow-public.asc --private-out ./escrow-private.asc
```

Give the app only `escrow-public.asc` through `--monero-seed-key`. An optional `--audit-recipient-key PUBLIC.asc` encrypts audit exports. Private OpenPGP key imports are refused by the deployment tool. Keep private recovery keys offline and back them up separately. Changing an installed application or wallet secret can make existing sealed data or wallets unreadable; initialization and restart never rotate them automatically.

The key CLI also generates/imports TLS identities. Run `./opsecmkt keys help` for the exact flags. Keep CA signing keys on the issuing machine. Copy only leaf private keys and public certificates to their runtime host. The application and gateway need separate leaf identities with the appropriate server/client usage and hostnames.

Generate separate Tor service identities with `keys generate onion --out-dir NEW_DIR`, or import a complete address and matching native public/private Tor files with `keys import onion`. The [onion key reference](cli-reference.md#onion-service-identities) lists every flag. Optional lowercase Base32 prefixes have a six-character limit and can require substantial computation; generation is local and does not publish a service. Keep encrypted offline backups of all three identity files. Service private keys remain online while Tor runs, and they are separate from administrator browser authorization keys.

## Initializing the production application host

Prepare private one-line files for these URLs:

- PostgreSQL: `postgres://USER:PASSWORD@db.internal/MARKET?sslmode=verify-full`.
- Monero wallet: `https://USER:PASSWORD@monero.internal:PORT/json_rpc`.
- Restricted Monero daemon: `https://USER:PASSWORD@monero.internal:PORT/json_rpc`.

Use separate, dedicated credentials; these examples are formats, not usable credentials. Monero can use a client certificate/key instead of URL credentials. For a private PostgreSQL CA, `--database-ca` copies its public certificate and sets `sslrootcert` to the mounted path. Monero's `--monero-rpc-ca` supplies its verified TLS trust root.

The following initializes only the application host. The endpoint names must match the imported certificate SANs and the actual private network:

```sh
./opsecmkt deploy init --dir ./market-prod --mode prod \
  --app-image YOUR_APP_IMAGE_WITH_SHA256_DIGEST \
  --app-host app.internal --gateway-url https://market.example \
  --app-bind 10.20.0.10:8080 --backend-bind 10.20.0.10:8443 \
  --database-url-file ./inputs/database.url --database-ca ./inputs/database-ca.pem \
  --escrow-xmr --monero-network stagenet \
  --monero-wallet-rpc-url-file ./inputs/wallet.url \
  --monero-daemon-url-file ./inputs/daemon.url \
  --monero-rpc-ca ./inputs/monero-ca.pem \
  --monero-seed-key ./inputs/escrow-public.asc \
  --tls-cert ./inputs/app.pem --tls-key ./inputs/app.key \
  --upload-ca ./inputs/upload-ca.pem \
  --upload-cert ./inputs/backend.pem --upload-key ./inputs/backend.key
./opsecmkt deploy plan --dir ./market-prod
./opsecmkt deploy install --dir ./market-prod
```

Select `--exposure tor` and an HTTP `.onion` `--gateway-url` for a Tor storefront; the public Tor service runs on the gateway host. Production Tor also requires `--admin-tor-image DIGEST` and `--admin-client-auth-dir DIR`; this separate app-host administrator service reaches only the private Unix socket. See [native dual-onion setup](tor.md#native-production-with-separate-onions). Application and upload backend ports must accept only the gateway over the authenticated private network. `--app-bind` and `--backend-bind` accept explicit loopback or private IPs only. A loopback bind can be reached through an operator-managed authenticated encrypted tunnel.

For each enabled managed Tor role, `init` creates a local random identity before startup unless you select a prefix or import the complete matching address/key pair. Development public Tor uses `--onion-prefix` or the three `--onion-address`, `--onion-public-key`, `--onion-secret-key` imports. Private administrator Tor uses the corresponding `--admin-onion-*` flags. Prefix generation and import cannot be combined. Public and administrator addresses must differ. Identities stay in private `onion-public/` and `onion-admin/` directories, and `init`/`plan` show only the public addresses. Production public Tor is managed on the gateway host; use the [native identity and ingress procedure](tor.md#prepare-the-public-gateway-identity) there.

Private Tor startup includes a networkless `admin-tor-prepare` init service from the app image. It verifies the selected service identity and validated public client records, prepares the private Tor volume and assigns ownership to the configured nonroot Tor user. It has no application, database or wallet secret mounts. A fresh volume receives the selected identity; a populated one must match, and partial/mismatched keys refuse startup without replacement. Development public Tor has its own identity helper and separate volume. Legacy configurations without a managed source require an existing valid volume and explicit identity adoption before using a fresh one.

Encrypted RAM bundles include configured managed identity files, but not whole Tor Docker volumes. Runtime Tor keys are copied into persistent `tor_identity`/`tor_admin_identity` volumes; they do not become RAM-only when the controller directory is in tmpfs. Container removal and emergency RAM cleanup retain those volumes. Protect them with the host's separately reviewed storage and recovery controls.

`deploy install` starts the configured application-host services. It does not connect to another host to provision the public gateway, ClamAV, public Tor/HTTPS ingress, database, Monero or version worker. On each dependency host, prepare the service account, executable/image, certificates, restricted listener/firewall, storage and supervisor separately. The gateway command below assumes those prerequisites already exist. Reuse an existing service when it meets the required contracts; review the [service flows and worker setup](service-topology.md#allowing-only-required-service-flows) before opening ingress.

Run the same binary on the separate upload/scanner host, with the local ClamAV service ready:

```sh
./opsecmkt upload-gateway --addr 127.0.0.1:8080 \
  --app-url https://app.internal:8080 \
  --backend-url https://app.internal:8443 \
  --ca ./upload-ca.pem --cert ./gateway-client.pem --key ./gateway-client.key \
  --clamd-address 127.0.0.1:3310
```

The configured upload CA authenticates the private upload backend. The regular
application HTTPS proxy uses the host's system trust store; install the application
issuing CA there or pass `--app-ca FILE`. These are separate trust settings:
`--ca` never automatically authenticates the ordinary application proxy, and its
client certificate is never sent on that connection. Terminate the public HTTPS
site or onion at this gateway. `--allow-private-http` is reserved for a separately
authenticated encrypted tunnel or local development; a plain private LAN is
insufficient.

For private administrator access, follow [native production with separate onions](tor.md#native-production-with-separate-onions). The production installation and firewall must preserve that separate administrative path and deny it through the public storefront. Do not infer that the public upload gateway authenticates administrators. Existing role, session, CSRF and step-up checks remain required.

With `ADMIN_SOCKET` enabled, administrator photo/avatar uploads are unavailable
in isolated mode, including through the existing private socket. Those actions
need a dedicated authenticated administrator upload ingress; the public gateway
cannot grant that capability. Other private administration workflows remain
available. Logo uploads use a separately confirmed, one-use handoff to the configured gateway; see [market branding](../operator-guide.md#changing-the-market-name-and-logo).

## Receiving dependency update warnings

Run `versions-worker` on a separate worker host with its own dedicated audit-style Ed25519 key. It checks official release sources on startup and periodically, signs a public report, and never upgrades software. Pass `--socks5 HOST:PORT` to route its requests and DNS through the worker's Tor SOCKS endpoint; otherwise it uses direct egress. `--current SERVICE=VERSION` records operator-supplied inventory, not observed service telemetry.

During deployment initialization, add `--dependency-report-dir DIR --dependency-report-public-key FILE`. Sync only the signed `report.json` into that directory. The app mounts it read-only and verifies reports against a separately copied public key; replacing a key beside the report cannot change that pin. Keep the worker signing key outside the report directory and off the app, database, Monero and upload hosts. Ensure the application's runtime UID can traverse the report directory. Missing, stale or invalid reports remain visible as administrator warnings and do not trigger software changes.

## Rebuilding or replacing only the application

Keep the private installation directory. Existing PostgreSQL, Monero, upload/ClamAV and dependency-worker hosts remain the same services; a new main-app build does not adopt, provision, or update them automatically. The saved endpoints, trust roots, settings, project identity, application keys and wallet-secret pin remain authoritative.

Before changing the app image, review the new release's migration and compatibility notes. Take the documented encrypted database and configuration backups, retain the current external retention ledger, and verify the recovery procedure. Migrations run at application startup and may be irreversible. A rollback can require a new restored database and can lose later writes; switching back to an old image is not a migration rollback.

Redeploy the app with a reviewed digest:

```sh
./opsecmkt deploy plan --dir ./market-prod
./opsecmkt deploy redeploy --dir ./market-prod \
  --app-image YOUR_NEW_APP_IMAGE_WITH_SHA256_DIGEST
./opsecmkt deploy status --dir ./market-prod
```

`redeploy` records the desired image, updates the generated Compose definition, and invokes `up --no-deps app` with a readiness wait. It targets only the application service. It does not restart or regenerate worker services, worker configuration, secrets, data volumes, endpoints, or the retention directory. If startup fails, it retains the desired image and current private state for inspection. Correct the cause and repeat `redeploy` with the intended digest. There is no automatic rollback or fallback to an unreviewed image.

For a replacement **production application host**, stop the old application cleanly before starting its replacement. Transfer the retained configuration/key material and current external persistent retention ledger through an authenticated encrypted channel; for RAM deployments use the [offline bundle and RAM-unlock procedure](ram-secrets-and-emergency.md), not a plaintext persistent copy. Keep the ledger outside the temporary instance. Preserve permissions, the saved project/context identity, application key, Monero wallet secret, seed recipient, audit signing key, TLS trust roots, dependency-report verification pin, and remote database/Monero/gateway URLs. Configure the saved Docker context on the new host, and ensure its runtime UID/GID can read the retained private files.

Update only the application host address/bindings and relevant routing if the host changes. Reissue the application and upload-backend leaf TLS certificates when their hostname changes; keep the issuing CA keys offline. Restore the public dependency-report synchronization directory at its saved path. Verify that the gateway reaches the new application's verified HTTPS and mutual-TLS backend endpoints. If private administrator Tor is enabled, retain its selected `onion-admin/` identity files in the protected configuration or migrate its matching `tor_admin_identity` volume. Run `plan`, then `install`/`up` on a replacement engine to initialize a fresh volume from that same identity and create the missing administrator service. Existing volumes must match and are never overwritten with another address. Legacy configurations without managed identity files must preserve their valid volume or explicitly export/adopt the identity through the reviewed recovery procedure. Use app-only `redeploy` after dependencies exist; it cannot create them with `--no-deps`. On the same host with existing services, continue with `plan` and `redeploy`. Do not run `init`, create a new project, generate replacement application/wallet/onion keys, reset trust pins, or move the database and workers merely because the app was rebuilt.

A development installation also contains local database and wallet **Docker volumes**. Copying its operator directory alone does not move those volumes; use the documented database, wallet, onion-identity and retention recovery procedures before moving that deployment.

## Operating and recovering the deployment

| Command | Result |
| --- | --- |
| `deploy plan --dir DIR` | Validates private configuration and prints topology without secret values. Starts nothing. |
| `deploy doctor --dir DIR` | Validates configuration and checks Docker Compose availability. Does not prove remote connectivity or physical host separation. |
| `deploy install` or `deploy up --dir DIR` | Regenerates private runtime inputs and starts the same project with a bounded readiness wait. Preserves every key and data volume. |
| `deploy redeploy --dir DIR --app-image IMAGE@sha256:DIGEST` | Updates and starts only the app; preserves worker configuration and all keys. Requires an existing installation. |
| `deploy status --dir DIR` | Shows this project's containers and health. |
| `deploy logs --dir DIR` | Development: prints at most 100 recent lines per service, with known secret values and URL credential fragments redacted. Production: explains that persistent application container logs are disabled; use administrator diagnostics and status. |
| `deploy down --dir DIR` | Stops/removes project containers and networks. Retains every persistent volume and private installation file. |

Add `--dir DIR` to every command when using a non-default directory. Configuration is strict JSON in `operator.json`; unknown fields/settings are refused. Generated Compose JSON is an implementation artifact: edit the operator configuration, then run `plan` and `up`. Never change the saved project or Docker context identity after installation. For a replacement host, configure the retained context name against its intended engine. Concurrent mutations fail while the installation's process lock is held; the operating system releases that lock if the process exits.

Production app containers use Docker's `none` logging driver, without log rotation options. This prevents Docker from retaining their standard output/error stream. Development keeps bounded `json-file` rotation. Use the administrator's bounded in-memory diagnostics and `deploy status` for production troubleshooting; a process restart clears that memory. This control does not govern the host's journal, proxy, Tor, database, Monero or separate worker logs, and does not guarantee that no timestamps exist elsewhere. Configure those hosts separately.

A failed Docker start leaves configuration, keys, and volumes in place. Correct the error, inspect status and administrator diagnostics (or `deploy logs` in development), then retry `deploy up`; do not delete the installation directory to bypass initialization safeguards. Docker daemon administrators can inspect containers and mounts. The PostgreSQL container receives its password through a private environment file; other app secrets use private read-only file mounts. When run as root, initialization assigns runtime secrets to UID/GID 65532 by default. Otherwise it uses the operator's nonroot UID/GID.

This controller does not install Docker or system packages, replace an existing installation, schedule backups, migrate a wallet host, or provide a backup/restore command. Follow [Backups and recovery](../operator-guide.md#backups-and-recovery), retaining the private installation files, database, wallets, onion identities, and external retention ledger. The existing backup scripts require a source checkout and their documented tools. Rehearse recovery before inviting users. Updating a container digest is an operator-controlled configuration change; take the documented backups and check migration compatibility before `deploy up`.
