# Running the standalone binary

The `opsecmkt-linux-amd64` and `opsecmkt-linux-arm64` release files include the application, HTML/CSS assets, migrations, deployment commands and key tools. Running `serve` needs PostgreSQL; running `deploy` also needs Docker Engine and Docker Compose 2.17 or later. No source checkout or Go compiler is needed on the target server.

Check both `docker version` and `docker compose version` before deployment;
installing the engine alone does not establish that the Compose v2 plugin is
available. See the [production prerequisites](production.md#before-starting).

The release workflow builds these binaries after CI and attaches them to a draft release. This source change does not publish or deploy a release. Verify the matching entry in `SHA256SUMS`, make the binary executable, and rename it to `opsecmkt` if convenient. `./opsecmkt --help` lists commands, and `./opsecmkt serve --help` lists runtime flags. Existing host commands, including `-bootstrap-admin`, `-status`, `-reset-admin-password` and `-export-audit`, remain available.

See the [complete CLI reference and Bash migration matrix](cli-reference.md) for every command, flag, default, key operation and remaining script dependency.

For an ordered deployment walkthrough with a managed database, separate gateway,
TLS, supervision and recovery checks, use [Deploying a production instance](production.md).

## Starting guided setup

In a terminal, run `./opsecmkt` with no arguments. The guide can create a new deployment, generate/import an onion identity, or manage an existing installation. `./opsecmkt setup` selects it explicitly. Bare `init`/`deploy init` also open it when both stdin and stdout are terminals; any configuration flags select the normal noninteractive command instead. Headless no-argument startup keeps its existing server behavior. Use explicit `serve` for supervised services.

Defaults favor development, loopback binding, stagenet and disabled escrow. Enter accepts a displayed default; `?` explains a question; `quit`, EOF or Ctrl-C cancels. Secret questions ask for private file paths, never their contents. Review the equivalent command, select `edit` to change cached answers, and use `:clear` to remove an optional cached value. Both configuration creation and the separate startup offer default to **no**. Successful save prints the generated/imported key paths, exact setup-token location, backup checklist and next command. See [guided interactive setup](cli-reference.md#guided-interactive-setup) for the full behavior and advanced flag alternatives.

## Choosing a topology

Development may use one server. Production uses separate application, PostgreSQL, Monero, and upload/scanner hosts. Monero's dedicated wallet RPC and restricted daemon RPC may share their Monero host. Keep ClamAV beside the upload gateway, away from the application and database. Never publish database, wallet RPC, daemon RPC, ClamAV, or the application's upload backend to the internet.

The production profile requires verified TLS for PostgreSQL and both Monero RPC endpoints, authentication for Monero, HTTPS for the application proxy connection, and mutual TLS for the upload backend. It rejects a local database, local Monero or local ClamAV deployment. It also rejects matching configured host identities across those layers. Hostnames alone cannot prove physical separation: verify placement, aliases, firewall rules, and private routing yourself.

The gateway is the public HTTP front door: route the entire public onion or HTTPS site through it. It proxies ordinary requests to the app and intercepts raw photo/avatar uploads for scanning and normalization. The app's production listener refuses direct raw uploads before reading their body. The gateway has no application database credentials, application key root, Monero credentials, or wallet secret. It does handle browser session cookies, so gateway compromise remains security-sensitive. A clean malware scan cannot guarantee a harmless image.

A compromised running application can access its configured keys and data. Encrypted backups and offline recovery keys protect different boundaries. Monero escrow remains custodial, and mainnet has real funds. Mainnet payment and settlement workflows remain unverified; deployment and readiness checks do not establish real-funds settlement. Use stagenet for development and payment rehearsal.

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

## Building and publishing the application image

`deploy` runs the application in a container, so it needs a packaged application
image as well as the standalone deployment binary. The installer does not select
an official published image for you. Build the repository's `Dockerfile` from the
reviewed source revision matching your binary; it packages the server and sets
`/app/server` as the entrypoint automatically. That path is the executable inside
the container.

Before starting, have Docker with a working builder, the reviewed source checkout,
and permission to push to a container registry your application host can reach.
Keep runtime keys and private input files outside the build context. Registry
storage and transfers may have separate charges; check your chosen service's
terms. Private registries need push credentials on the build machine and preferably
separate read-only pull credentials on the application host.

A **tag**, such as `:release-1`, is a name that can later point to another build.
A **digest**, such as `@sha256:` followed by 64 hexadecimal characters, identifies
the exact published image. `--app-image` requires the full repository plus digest.
Docker supplies the digest; do not invent it or substitute a Git commit hash.

1. **Choose your repository and tag.** In the reviewed source checkout, replace
   the example registry, account/team and tag with your own values:

   ```sh
   APP_REGISTRY='registry.example.com'
   APP_REPOSITORY="$APP_REGISTRY/YOUR_TEAM/opsecmkt"
   APP_TAG='YOUR_REVIEWED_RELEASE_OR_COMMIT'
   ```

   For example, an account using GitHub Container Registry would choose
   `ghcr.io/YOUR_ACCOUNT/opsecmkt` as its repository. These are naming examples,
   not existing project images. Use the same values in each later shell.

2. **Sign in to your registry.** Run this on the build machine and follow its
   authentication prompt. Supply a password or token through the prompt, never
   as a command argument:

   ```sh
   docker login "$APP_REGISTRY"
   ```

3. **Build and publish the image.** Run from the checkout directory containing
   `Dockerfile`. This example targets a Linux AMD64 application host:

   ```sh
   docker build --platform linux/amd64 -t "$APP_REPOSITORY:$APP_TAG" .
   docker push "$APP_REPOSITORY:$APP_TAG"
   ```

   Retain the successful push's digest with the source revision and build record.
   For another host architecture, choose its matching single platform and verify
   builder/base-image support. If you need multiple architectures, use a reviewed
   [Docker multi-platform build](https://docs.docker.com/build/building/multi-platform/)
   and retain the registry digest for that published result.

4. **Get the full immutable reference on the application host.** Set the same
   repository/tag values there and authenticate with pull access if necessary.
   Use the Docker context that will run the deployment:

   ```sh
   docker pull --platform linux/amd64 "$APP_REPOSITORY:$APP_TAG"
   APP_IMAGE=$(docker image inspect --format '{{index .RepoDigests 0}}' "$APP_REPOSITORY:$APP_TAG")
   printf '%s\n' "$APP_IMAGE"
   ```

   Expect your own repository followed by `@sha256:` and 64 lowercase hexadecimal
   characters. Confirm the digest matches the successful push, then validate its
   shape and repository in the same shell:

   ```sh
   case "$APP_IMAGE" in
     "$APP_REPOSITORY"@sha256:*) ;;
     *) printf '%s\n' 'Unexpected image repository or missing digest' >&2; exit 1 ;;
   esac
   APP_DIGEST=${APP_IMAGE#"$APP_REPOSITORY"@sha256:}
   case "$APP_DIGEST" in
     ''|*[!0-9a-f]*) printf '%s\n' 'Invalid image digest' >&2; exit 1 ;;
   esac
   test "${#APP_DIGEST}" -eq 64 || exit 1
   docker image inspect --format '{{.Os}}/{{.Architecture}} {{json .Config.Entrypoint}}' "$APP_IMAGE"
   ```

   For this example, expect `linux/amd64 ["/app/server"]`. Stop if the repository,
   digest, platform or entrypoint differs. See Docker's
   [build](https://docs.docker.com/reference/cli/docker/buildx/build/),
   [push](https://docs.docker.com/reference/cli/docker/image/push/),
   [digest pull](https://docs.docker.com/reference/cli/docker/image/pull/#pull-an-image-by-digest-immutable-identifier)
   and [image inspection](https://docs.docker.com/reference/cli/docker/image/inspect/)
   references for these commands.

5. **Pass the result to initialization.** Keep `APP_IMAGE` set to that complete
   inspected reference. The next development example uses
   `--app-image "$APP_IMAGE"`; production uses it in the
   [complete new-install command](production.md#4-initialize-and-start-the-application).
   Prepare payment flags before that one production initialization. Existing
   installations use `redeploy`, never a second `init`.

## Initializing a development deployment

First [build, publish and inspect the application image](#building-and-publishing-the-application-image).
PostgreSQL defaults to the same pinned PostgreSQL 17 image as the existing Compose deployment.

With `APP_IMAGE` set in the application host's shell, run:

```sh
./opsecmkt deploy init --dir ./market-dev --mode dev \
  --app-image "$APP_IMAGE"
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

Install Linux with a LUKS2-encrypted operating-system root and test its independent
unlock/recovery path **before** preparing private inputs. Run the deployment CLI
on that host with its local Docker Engine. Production initialization verifies
root, the installation, Docker/containerd storage, retention and private input
paths before staging configuration or generating keys. It saves the expected
LUKS identities; `install`, `up`, `redeploy` and `doctor` recheck them and refuse
active swap. Keep the operator installation on persistent encrypted storage;
production starts no longer accept a tmpfs/RAM-unlocked operator directory.

The [storage procedure](server-hardening.md#prepare-encryption-before-running-setup)
covers encrypted OS installation, verification, optional additional blank data
disks and startup checks. The Go tool does not convert an existing root or set up
preboot unlocking. A remote Docker context cannot prove the remote host's disks.
Existing unencrypted installations need the migration below, preserving their
keys; rerunning `init` is not a migration.

Production starts require Linux root, systemd and a persistent root-owned binary,
such as `/usr/local/bin/opsecmkt`. They install per-project Docker/containerd
storage-check drop-ins and reload systemd before starting the application;
they do not restart those daemons. Review these host-wide daemon checks before
using a shared host. Configure native gateway/payment supervisors separately.

Prepare private one-line files for these URLs:

- PostgreSQL: `postgres://USER:PASSWORD@db.internal/MARKET?sslmode=verify-full`.
- Monero wallet: `https://USER:PASSWORD@monero.internal:PORT`.
- Restricted Monero daemon: `https://USER:PASSWORD@monero.internal:PORT`.

Use separate, dedicated credentials; these examples are formats, not usable credentials. Supply RPC origins without `/json_rpc`; the application appends each RPC endpoint. Monero can use a client certificate/key instead of URL credentials. For a private PostgreSQL CA, `--database-ca` copies its public certificate and sets `sslrootcert` to the mounted path. Monero's `--monero-rpc-ca` supplies its verified TLS trust root.

The following initializes only the application host. Set `APP_IMAGE` using the
[build and publish procedure](#building-and-publishing-the-application-image).
The endpoint names must match the imported certificate SANs and the actual private network:

```sh
./opsecmkt deploy init --dir ./market-prod --mode prod \
  --app-image "$APP_IMAGE" \
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
./opsecmkt deploy doctor --dir ./market-prod
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
available.

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

For a replacement **production application host**, stop the old application cleanly before starting its replacement. Transfer the retained configuration/key material and current external persistent retention ledger through an authenticated encrypted channel directly onto verified encrypted storage. The new production start commands require a persistent encrypted operator installation; do not restore it into the older tmpfs/RAM-unlocked layout. Preserve permissions, the saved project/context identity, application key, Monero wallet secret, seed recipient, audit signing key, TLS trust roots, dependency-report verification pin, and remote database/Monero/gateway URLs. Configure the saved local Docker context on the new host, and ensure its runtime UID/GID can read the retained private files.

Update only the application host address/bindings and relevant routing if the host changes. Reissue the application and upload-backend leaf TLS certificates when their hostname changes; keep the issuing CA keys offline. Restore the public dependency-report synchronization directory at its saved path. Verify that the gateway reaches the new application's verified HTTPS and mutual-TLS backend endpoints. If private administrator Tor is enabled, retain its selected `onion-admin/` identity files in the protected configuration or migrate its matching `tor_admin_identity` volume. Run `install`/`up` only after the storage-binding checks below to initialize a fresh volume from that same identity and create the missing administrator service. Existing volumes must match and are never overwritten with another address. Legacy configurations without managed identity files must preserve their valid volume or explicitly export/adopt the identity through the reviewed recovery procedure. Use app-only `redeploy` after dependencies exist; it cannot create them with `--no-deps`. On the same host with existing services, continue with `plan` and `redeploy`. Do not run `init`, create a new project, generate replacement application/wallet/onion keys, reset trust pins, or move the database and workers merely because the app was rebuilt.

On a replacement encrypted Linux host, record its verified storage before
starting the retained installation:

```sh
./opsecmkt deploy storage bind --dir ./market-prod
./opsecmkt deploy plan --dir ./market-prod
./opsecmkt deploy doctor --dir ./market-prod
```

`storage bind` changes storage bindings after a reviewed migration; it preserves
configuration, keys and the installation receipt. It does not copy data, unlock
storage or bypass encryption checks. Review the
[boot checks](server-hardening.md#keep-services-stopped-until-storage-is-ready)
before `install`/`up`. A same-host redeployment on matching storage does not need
rebinding. Keep `plan`, `status`, `logs` and `down` available for inspection or
shutdown while correcting a refused startup.

A development installation also contains local database and wallet **Docker volumes**. Copying its operator directory alone does not move those volumes; use the documented database, wallet, onion-identity and retention recovery procedures before moving that deployment.

## Operating and recovering the deployment

| Command | Result |
| --- | --- |
| `deploy plan --dir DIR` | Validates private configuration and prints topology without secret values. Starts nothing. |
| `deploy doctor --dir DIR` | Validates configuration, checks production storage bindings and Docker Compose availability. Does not prove remote connectivity or physical host separation. |
| `deploy install` or `deploy up --dir DIR` | Regenerates private runtime inputs and starts the same project with a bounded readiness wait. Preserves every key and data volume. |
| `deploy redeploy --dir DIR --app-image IMAGE@sha256:DIGEST` | Updates and starts only the app; preserves worker configuration and all keys. Requires an existing installation. |
| `deploy status --dir DIR` | Shows this project's containers and health. |
| `deploy logs --dir DIR` | Development: prints at most 100 recent lines per service, with known secret values and URL credential fragments redacted. Production: explains that persistent application container logs are disabled; use administrator diagnostics and status. |
| `deploy down --dir DIR` | Stops/removes project containers and networks. Retains every persistent volume and private installation file. |

Add `--dir DIR` to every command when using a non-default directory. Configuration is strict JSON in `operator.json`; unknown fields/settings are refused. Generated Compose JSON is an implementation artifact: edit the operator configuration, then run `plan` and `up`. Never change the saved project or Docker context identity after installation. For a replacement host, configure the retained context name against its intended engine. Concurrent mutations fail while the installation's process lock is held; the operating system releases that lock if the process exits.

Production app containers use Docker's `none` logging driver, without log rotation options. This prevents Docker from retaining their standard output/error stream. Development keeps bounded `json-file` rotation. Use the administrator's bounded in-memory diagnostics and `deploy status` for production troubleshooting; a process restart clears that memory. This control does not govern the host's journal, proxy, Tor, database, Monero or separate worker logs, and does not guarantee that no timestamps exist elsewhere. Configure those hosts separately.

A failed Docker start leaves configuration, keys, and volumes in place. Correct the error, inspect status and administrator diagnostics (or `deploy logs` in development), then retry `deploy up`; do not delete the installation directory to bypass initialization safeguards. Docker daemon administrators can inspect containers and mounts. The PostgreSQL container receives its password through a private environment file; other app secrets use private read-only file mounts. When run as root, initialization assigns runtime secrets to UID/GID 65532 by default. Otherwise it uses the operator's nonroot UID/GID.

This controller does not install Docker or system packages, replace an existing installation, schedule backups, migrate a wallet host, or provide a backup/restore command. Follow [Backups and recovery](../operator-guide.md#backups-and-recovery), retaining the private installation files, database, wallets, onion identities, and external retention ledger. The existing backup scripts require a source checkout and their documented tools. Rehearse recovery before inviting users. Updating a container digest is an operator-controlled configuration change; take the documented backups and check migration compatibility before `deploy up`.
