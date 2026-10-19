# Standalone CLI reference and Bash migration

The `opsecmkt` binary runs the application, upload gateway and dependency-version worker, generates/imports keys, and manages an explicit Docker Compose installation. This reference describes commands present in the current source. For a guided deployment, use [Running the standalone binary](binary.md); for separate hosts and sizing, use [Separating services](service-topology.md).

Examples use `./opsecmkt`. Rename the release file for your platform or substitute its actual path. Native commands do not require a source checkout or a Go compiler. `serve` requires PostgreSQL; `deploy` requires Docker Engine and Compose 2.17 or newer. The binary does not embed PostgreSQL, Monero, ClamAV or Tor, install packages or provision remote hosts. Production lifecycle commands install storage-check drop-ins; optional `deploy host-reboot configure` installs its narrow systemd agent. Operators supervise the other native services separately.

## Command map and argument rules

| Command | Purpose |
| --- | --- |
| `help`, `--help`, `-h` | Print the root command list. |
| `version` | Print the release build version; development builds may print a VCS revision or `development`. Takes no arguments. |
| `setup` | Open guided deployment/key/management prompts explicitly, including when input is a pipe. Accepts no configuration flags; `setup --help` prints help. |
| `serve` | Run the application in the foreground, with embedded UI assets and migrations. |
| `upload-gateway` | Run the separate upload/scanner front door in the foreground. |
| `versions-worker` | Refresh signed dependency reports once or periodically. |
| `host-control-agent` | Serve the root-only, fixed application-host reboot capability through a private Unix socket. Installed through `deploy host-reboot configure`; never a public HTTP service. |
| `keys` | Generate/import independent secrets, OpenPGP recovery keys and TLS identities. |
| `secrets` | Pack configuration offline, mount protected tmpfs, and unlock runtime material from stdin. |
| `emergency` | Inspect or explicitly target a manual project shutdown/LUKS incident action. |
| `admin-tor-prepare` | Prepare public client authorization and ownership in the dedicated private Tor identity volume. Normally invoked by the generated init service. |
| `tor-identity-prepare` | Initialize or verify a selected public onion identity in its dedicated volume. Normally invoked by development's generated init service. |
| `deploy` | Manage a private installation directory, encrypted-storage checks and its Compose project. |
| `init`, `plan`, `install`, `redeploy`, `status`, `up`, `down`, `logs`, `doctor`, `storage`, `host-reboot` | Exact shortcuts for the corresponding `deploy` command. |
| Legacy top-level flags, such as `-bootstrap-admin` | Run host administration or probe commands; see [Host commands](#legacy-host-commands). |

Run `serve --help`, `upload-gateway --help`, `versions-worker --help`, `keys help`, `deploy --help`, `deploy init --help`, or `deploy storage help`. `secrets --help` and `emergency --help` describe their separate roles. Key operations are listed by `keys help`; onion generation/import also provide nested `--help`. Other nested key invocations, such as `keys generate secret --help`, are not separate help interfaces.

Flags accept one or two leading hyphens. Examples use `--flag VALUE` for new commands. Put flags before positional arguments. Go boolean flags use `--flag` or `--flag=false`; `--flag false` leaves `false` as a positional argument. All new commands reject unexpected positional arguments. `serve --escrow-xmr on` is a string setting; `deploy init --escrow-xmr` is a boolean switch.

**`status` and `-status` differ:** `status` reports Compose containers; `-status` queries the database's setup/schema/key-root status. Likewise, `serve` accepts its documented flags, while legacy host flags belong directly after the binary name.

## Guided interactive setup

Run the binary in a terminal without arguments, or select guided setup explicitly:

```sh
./opsecmkt
./opsecmkt setup
```

Automatic prompting requires **both stdin and stdout to be real terminals**. Bare `init` and `deploy init` also open the guide in that situation. Any configuration flag bypasses the guide and uses the normal flag parser. Headless no-argument invocation retains the environment-configured server behavior for existing services; use explicit `serve` in new service definitions. With redirected input/output, `deploy init` requires its flags. Explicit `setup` opts into prompts even over a pipe; use the flag interface for stable automation rather than depending on prompt order.

The guide uses numbered task choices and plain text:

| Task | Guided behavior |
| --- | --- |
| New deployment | Choose dev/prod, private directory, Docker context, digest-pinned app image, exposure, initial site access and registration. Production asks for prepared database/TLS/gateway inputs; development optionally adds ClamAV. Import stable key files or generate independent new secrets, optionally configure dedicated Monero RPC and select each managed Tor identity. |
| Onion identity | Generate random/prefix identity files or import the complete expected address and matching native files into a new private directory. No Tor service is started. |
| Existing installation | Default to `plan`, or choose `status`, `up`, `down` or app-only `redeploy` using the retained directory. Lifecycle changes require a separate review; they do not regenerate keys or remote workers. |

Enter accepts the displayed default. `?` explains the current question; `quit`, `q`, EOF or Ctrl-C cancels. Control characters are rejected without echoing them. Input uses bounded file paths for secrets, never password or credential-URL values. Local checks explain invalid answers and re-prompt; the same native operator/key validators check the final command.

Before saving, inspect the equivalent quoted command and the generated/imported key choices. `edit` reopens the deployment questions with cached defaults. Use `:clear` to remove an optional cached import or prefix; required answers cannot be cleared. Creating configuration defaults to **no**. Refusing or cancelling before confirmation writes no configuration. Accepted creation runs native `init` and `plan`, prints exact private key/token locations and encrypted-backup guidance, then asks separately whether to start. Startup also defaults to **no** and calls `install` only after that second confirmation. Cancelling after saving retains the saved installation and starts nothing further.

Safe defaults are development profile, loopback clearnet binding, stagenet and disabled escrow. Blank key imports generate new independent secrets for new state; retaining existing wallets/data requires their established keys. Mainnet must be selected explicitly. Production still requires independently prepared dependency hosts and verified transports. The guide does not install packages, provision those hosts, create a service unit, or run emergency erasure actions. Advanced settings, local development Monero containers, report-feed pins and explicit runtime IDs remain available through the complete flag reference below.

Production's encrypted-root requirement also applies to guided creation. Prepare
and verify Linux LUKS2 storage before importing private inputs or confirming
creation. The guide does not format a root disk or establish a preboot unlock
route; see [encryption-first setup](server-hardening.md#prepare-encryption-before-running-setup).

## Serving the application

```text
opsecmkt serve [flags]
```

The process starts database checks/migrations before serving, then runs until stopped. Headless no-argument invocation runs the same server startup using the environment and legacy flag parser; terminal no-argument invocation opens guided setup. Use explicit `serve` under a service manager and `deploy` for Compose lifecycle management. `SIGINT` and `SIGTERM` request graceful shutdown. Preserve enough stop time for in-flight requests and payment work; generated Compose uses a 60-second stop grace period.

### Configuration precedence and private files

`serve` overrides an environment variable only when its corresponding flag is explicitly supplied. An omitted flag preserves the inherited environment. An explicit empty string clears that flag's variable, except `--site-access` and `--registrations`, which require one of their listed choices. Application defaults apply after this step. There is no `serve --config`, automatic `.env` reader or general `serve --set` flag. Guided setup creates the operator configuration; `operator.json` is read by `deploy`, not by `serve`.

The application and legacy host commands load these private file inputs before startup:

| File variable | Resolved secret |
| --- | --- |
| `DATABASE_URL_FILE` | `DATABASE_URL` |
| `APP_SECRET_KEY_FILE` | `APP_SECRET_KEY` |
| `SETUP_TOKEN_FILE` | `SETUP_TOKEN` |
| `MONERO_WALLET_SECRET_FILE` | `MONERO_WALLET_SECRET` |
| `ESCROW_XMR_WALLET_RPC_URL_FILE` | `ESCROW_XMR_WALLET_RPC_URL` |
| `ESCROW_XMR_DAEMON_URL_FILE` | `ESCROW_XMR_DAEMON_URL` |
| `AUDIT_SIGNING_KEY_FILE` | `AUDIT_SIGNING_KEY` |

A file must be a readable regular file, without a symlink, inaccessible to group/others, and at most 64 KiB. It must contain one nonempty line without NUL/CR characters; one final newline is accepted. Use mode `0600` or stricter. Setting both a nonempty `NAME` and `NAME_FILE` is an error: a file flag does not silently override a raw secret already present in the environment.

Use paths rather than passwords or credential-bearing URLs in arguments. Paths and handles remain visible in the process list; file contents stay out of the argument list. The process can access its resolved secrets. Keep private files and online custody keys out of reports and chat.

### All serve flags

Every flag in this table takes a string unless marked boolean. Defaults describe the application when neither the flag nor its environment variable is set.

| Flag | Environment | Default and meaning |
| --- | --- | --- |
| `--addr` | `ADDR` | `127.0.0.1:8080`; application listener. Explicit `:8080` binds all interfaces, so use deliberate private routing/firewalls. |
| `--preview` | None | Boolean, `false`; read-only sample UI without a database. Listener must be loopback. `--addr` also selects preview address; legacy preview uses `PREVIEW_ADDR`. |
| `--profile` | `OPSECMKT_PROFILE` | Unset; `dev` permits development topology, `prod` enforces production declarations and transport checks. Declare `prod` explicitly in production. |
| `--app-mode` | `APP_MODE` | `clearnet`; accepts `clearnet` or `tor`. This declaration does not start Tor. |
| `--app-host` | `OPSECMKT_APP_HOST` | Unset; required production private application host identity, also used as the TLS healthcheck server name. |
| `--gateway-url` | `OPSECMKT_GATEWAY_URL` | Unset; production's separate public HTTPS gateway URL or HTTP `.onion` URL. No credentials, query or fragment. |
| `--database-url-file` | `DATABASE_URL_FILE` | Unset; required database connection via this file or `DATABASE_URL`, except preview. Production requires a separate host and `sslmode=verify-full`. |
| `--app-secret-key-file` | `APP_SECRET_KEY_FILE` | Unset; stable application key root for CSRF and sealed account secrets. Legacy `SETUP_TOKEN` fallback remains supported. |
| `--setup-token-file` | `SETUP_TOKEN_FILE` | Unset; first-run browser setup authorization. Required for browser setup until installed; ignored as authorization afterwards. |
| `--site-access` | `INITIAL_SITE_ACCESS` | `public`; first-start default: `public` or `members` (sign-in required for site content). Stored in PostgreSQL. |
| `--registrations` | `INITIAL_REGISTRATIONS` | `open`; independent first-start default: `open` or `closed`. Existing accounts can still sign in when closed. |
| `--branding-upload-origin` | `BRANDING_UPLOAD_ORIGIN` | Unset; fixed public upload-gateway origin for private-admin logo uploads. Verified HTTPS or HTTP v3 onion, without credentials, path, query or fragment. Blank disables that upload flow with a separate private admin listener. |
| `--official-addresses` | `OFFICIAL_ADDRESSES` | Unset; comma-separated public market addresses (at most 8 HTTPS or HTTP v3 onion origins: onion, mirror, clearnet) named in every encrypted PGP sign-in code. Never the private administrator onion. Blank makes the code say no official address is configured. |
| `--cookie-secure` | `COOKIE_SECURE` | `true`; string `true` or `false`. Native Tor setup does not automatically set it to `false`; choose the value for your actual browser-facing origin. |
| `--admin-socket` | `ADMIN_SOCKET` | Unset; optional private administrator Unix listener. Requires Tor mode and a clean absolute path in a private directory; see [private admin ingress](tor.md#private-administrator-onion). |
| `--audit-signing-key-file` | `AUDIT_SIGNING_KEY_FILE` | Unset; independent 64-hex Ed25519 signing seed. Signed audit export is unavailable without it. |
| `--monero-network` | `MONERO_NETWORK` | Blank/stagenet is a test market; `mainnet` explicitly selects real funds. Development, tests and CI use stagenet. |
| `--escrow-xmr` | `ESCROW_XMR` | `off`; accepts `on` or `off`. Enabling requires the wallet/daemon URLs, stable wallet secret and public seed key below. |
| `--monero-wallet-rpc-url-file` | `ESCROW_XMR_WALLET_RPC_URL_FILE` | Unset; dedicated escrow wallet-RPC URL. Production requires verified HTTPS and credentials or mutual TLS. |
| `--monero-daemon-url-file` | `ESCROW_XMR_DAEMON_URL_FILE` | Unset; restricted daemon RPC URL. Production requires verified HTTPS and credentials or mutual TLS. |
| `--monero-wallet-secret-file` | `MONERO_WALLET_SECRET_FILE` | Unset; stable independent wallet-password derivation secret, distinct from app/setup secrets. |
| `--monero-seed-key` | `ESCROW_XMR_SEED_KEY` | Unset; armored, currently usable OpenPGP **public** recovery key. Private decryption key stays offline. |
| `--monero-rpc-ca` | `MONERO_RPC_CA` | Unset; Monero HTTPS uses system trust unless this public PEM CA is supplied. Hostname verification stays enabled. |
| `--monero-rpc-cert` | `MONERO_RPC_CERT` | Unset; optional Monero client certificate. Supply with its key. |
| `--monero-rpc-key` | `MONERO_RPC_KEY` | Unset; optional Monero client private key. Supply with its certificate. |
| `--clamd-address` | `CLAMD_ADDRESS` | Unset; disables integrated uploads. Development integrated scanning uses `HOST:PORT`; production app must leave it blank. |
| `--upload-mode` | `UPLOAD_MODE` | `integrated`; `isolated` refuses direct raw uploads and requires the private backend listener. Production requires `isolated`. |
| `--upload-backend-addr` | `UPLOAD_BACKEND_ADDR` | Unset; private mutual-TLS upload backend address, required with isolated mode. Never publish it. |
| `--upload-ca` | `UPLOAD_CA` | Unset; public CA authenticating upload-gateway client certificates. Required for the backend. |
| `--upload-cert` | `UPLOAD_CERT` | Unset; upload backend server certificate. Required for the backend. |
| `--upload-key` | `UPLOAD_KEY` | Unset; upload backend server private key. Required for the backend. |
| `--tls-cert` | `TLS_CERT` | Unset; application HTTPS leaf certificate. Required with `--tls-key`, and required in production. |
| `--tls-key` | `TLS_KEY` | Unset; application HTTPS private key. The upload backend uses its separate pair above. |
| `--dependency-report` | `DEPENDENCY_REPORT_FILE` | Unset; local signed worker report. The application does not fetch release data. |
| `--dependency-report-public-key` | `DEPENDENCY_REPORT_PUBLIC_KEY` | Unset; independently pinned 64-hex Ed25519 public verification key. Never obtain this pin from the replaceable report. |

TLS/public-key settings already take file paths; they do not use an additional `_FILE` suffix. Configure other startup settings through the environment; see [Additional runtime settings](#additional-runtime-settings).

### Reusing prepared services

For a development database already prepared as UTF8:

```sh
./opsecmkt serve --profile dev --addr 127.0.0.1:8080 \
  --database-url-file ./private/database.url \
  --app-secret-key-file ./private/app.key \
  --setup-token-file ./private/setup.key --cookie-secure false
```

For production, reuse your existing external database/payment services and imported keys. Start with the full [production application procedure](binary.md#initializing-the-production-application-host), including isolated uploads, HTTPS and mutual TLS. A restart or application rebuild is not permission to generate new app/wallet secrets, replace seed recipients, reset database pins, or change networks.

## Running the upload gateway

```text
opsecmkt upload-gateway [flags]
```

Run this process on the separate upload/ClamAV host behind the public onion or HTTPS front end. Route the entire browser-facing origin through the gateway; it intercepts uploads and proxies other requests while preserving the browser's Host. It handles sessions and cookies but needs no database login, application key root, wallet credentials or wallet secret.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8080` | HTTP listener behind Tor or HTTPS termination. This command does not provide public HTTPS termination itself. |
| `--app-url` | Required | Application origin for ordinary requests. Use verified HTTPS for a remote app. No URL credentials, path, query or fragment. |
| `--app-ca` | System trust | Optional public PEM CA for the ordinary application HTTPS connection. Separate from the backend CA. |
| `--backend-url` | Required | Private **HTTPS** upload-backend origin, with no credentials/path/query/fragment. |
| `--ca` | Required | Backend public CA used for server verification. |
| `--cert` | Required | Gateway's client certificate for mutual TLS. |
| `--key` | Required | Gateway's client private key. |
| `--clamd-address` | `127.0.0.1:3310` | Local scanner address; no internet exposure or main-app access. |
| `--max-signature-age-days` | `30` | Whole number from `7` through `90`; older/unreadable signatures pause uploads. |
| `--allow-private-http` | Boolean, `false` | Permits HTTP app proxying for local development or an independently authenticated encrypted tunnel. A plain private LAN is insufficient. Does not disable backend mutual TLS. |

```sh
./opsecmkt upload-gateway --addr 127.0.0.1:8080 \
  --app-url https://app.internal:8080 --app-ca ./app-ca.pem \
  --backend-url https://app.internal:8443 \
  --ca ./upload-ca.pem --cert ./gateway-client.pem --key ./gateway-client.key \
  --clamd-address 127.0.0.1:3310
```

The two HTTPS clients verify certificates/hostnames and ignore ambient HTTP proxy variables. Backend redirects are refused. Provision this remote host separately: neither this command nor app-host `deploy install` installs ClamAV, refreshes its signatures, creates public Tor/HTTPS ingress, installs a service unit or configures certificates/firewalls. The process stays in the foreground and requires supervision and deliberate log handling on its host. Keep gateway, ClamAV and ingress in their documented boundaries; a successful scan is not proof that a file is harmless.

## Running the version worker

```text
opsecmkt versions-worker --report FILE --signing-key FILE [flags]
```

Run this on the separate maintenance/version-worker host. It checks immediately, then sleeps between refreshes until stopped. It writes a signed report atomically and never updates software or scanner signatures.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--report` | Required | Public signed report output file; its parent directory must exist. Keep private signing material outside the delivered directory. |
| `--signing-key` | Required | Dedicated private `0600` file holding a 64-hex Ed25519 seed; maximum 64 KiB. Must differ from the report path/file. |
| `--current` | None | Repeat `SERVICE=VERSION` once per known service. Inventory is supplied by the operator, not discovered remotely. |
| `--interval` | `24h` | Go duration from `1h` through `168h`/7 days. Ignored after the first refresh when `--once` is set. |
| `--once` | Boolean, `false` | Write one report and exit. |
| `--socks5` | Unset | Explicit Tor SOCKS5 `HOST:PORT`; requests and DNS use SOCKS. No credentials or URL scheme. Without it, direct HTTPS is used. |

Supported `--current` service names are `postgresql`, `monerod`, `monero-wallet-rpc`, `tor`, `clamav` and `go`. Values must be stable numeric versions with two to four numeric components; common `v`, `go` and `clamav-` prefixes are normalized. Duplicate service entries and prerelease suffixes are refused.

The fixed release sources are PostgreSQL's `versions.json`, Go's JSON download feed, Tor's distribution directory, and the official Monero and Cisco-Talos ClamAV GitHub latest-release APIs. There is no arbitrary release URL flag. Direct queries expose the worker's source IP and query timing to those services; `--socks5` uses the explicitly configured SOCKS path instead. Ambient `HTTP_PROXY`/`HTTPS_PROXY` do not change this behavior. No application/database/payment credentials or endpoint identities are sent in the version report.

Generate its independent signing identity on the worker/issuing machine, with existing destination directories:

```sh
./opsecmkt keys generate audit --out ./worker-private/versions.seed \
  --public-out ./worker-public/versions.pub
./opsecmkt versions-worker --report ./reports/report.json \
  --signing-key ./worker-private/versions.seed \
  --current postgresql=17.0 --current monerod=0.18.5.1 \
  --current monero-wallet-rpc=0.18.5.1 --interval 24h --socks5 127.0.0.1:9050
```

Versions above illustrate syntax, not a claim that they are current. Transfer only `report.json` to the app's report directory, and import `versions.pub` independently as its pin. App administrator diagnostics warn when a report is missing, invalid or older than **72 hours**. Unknown installed/latest versions remain unknown; a network failure is not proof that software is current. The panel can substitute observed app Go, connected PostgreSQL and available integrated scanner versions for reported inventory.

`--once` exits successfully when a report was written even if release checks inside it were unavailable. Read the report/admin status instead of interpreting exit `0` as proof that every source succeeded. The continuously running mode handles `SIGINT`/`SIGTERM` cleanly; use host supervision for restart policy.

## Generating and importing keys

```text
opsecmkt keys help
```

All output paths must be distinct and their parent directories must already exist. Existing files and symlinks are never overwritten. Inputs are bounded to 64 KiB. Private inputs require regular non-symlink files with mode `0600` or stricter. Public certificate/OpenPGP/signing-key outputs use `0644`; private outputs use `0600`. Tor client authorization files both use `0600`. Onion service identities use a new private `0700` output directory containing three `0600` files. Commands report paths, public addresses or OpenPGP fingerprints; they do not print private contents.

### Application and signing secrets

```sh
./opsecmkt keys generate secret --out ./private/app.key
./opsecmkt keys generate audit --out ./private/audit.seed --public-out ./public/audit.pub
./opsecmkt keys import secret --in ./existing/app.key --out ./private/imported-app.key
./opsecmkt keys import audit --in ./existing/audit.seed --out ./private/imported-audit.seed \
  --public-out ./public/imported-audit.pub
```

`secret` and `audit` generate independent 32-byte random seeds encoded as 64 hexadecimal characters. Imports accept that same format, with an optional final newline. `--public-out` is optional for `audit` and derives its Ed25519 public verification key. Use independent secrets for application, setup, Monero wallet derivation, audit signing, and dependency-report signing; retain each stable secret with the state that depends on it.

### Offline OpenPGP recovery identity

```sh
./opsecmkt keys generate pgp --name 'Escrow recovery' \
  --public-out ./public/escrow.asc --private-out ./offline/escrow-private.asc
./opsecmkt keys import pgp --in ./existing/public.asc --public-out ./public/imported.asc
./opsecmkt keys import pgp --in ./existing/private.asc \
  --public-out ./public/imported-recovery.asc --private-out ./offline/imported-private.asc
```

Generation requires `--name`, `--public-out` and `--private-out`. Import requires `--in` and `--public-out`; private input additionally requires explicit `--private-out`, while public input cannot produce a private output. Only one armored key with a currently usable encryption key is accepted. Run generation/private import offline and give runtime services only the public recovery/audit recipient key. Private outputs are files, not a password-protected vault; protect their offline/encrypted storage separately.

### TLS CA, server and client identities

```sh
./opsecmkt keys generate tls-ca --name 'Upload service CA' \
  --cert-out ./public/upload-ca.pem --key-out ./offline/upload-ca.key
./opsecmkt keys generate tls-server --name 'Upload backend' --host app.internal,10.20.0.10 \
  --ca-cert ./public/upload-ca.pem --ca-key ./offline/upload-ca.key \
  --cert-out ./public/backend.pem --key-out ./private/backend.key
./opsecmkt keys generate tls-client --name 'Upload gateway' \
  --ca-cert ./public/upload-ca.pem --ca-key ./offline/upload-ca.key \
  --cert-out ./public/gateway.pem --key-out ./private/gateway.key
./opsecmkt keys import tls --cert-in ./existing/service.pem --key-in ./existing/service.key \
  --cert-out ./public/imported-service.pem --key-out ./private/imported-service.key
```

Generation requires `--name`, `--cert-out`, and `--key-out`. Server/client issuance also requires `--ca-cert` and `--ca-key`. Server generation additionally requires `--host`: comma-separated DNS names/IPs without URL schemes or ports. CA generation defaults to `3650` days; server/client generation defaults to `365` days. Optional `--days N` accepts `1` through `3650`, and a leaf's expiry is capped at its issuer's expiry.

Server and client certificates have separate TLS usages. Use application/backend **server** leaf identities on the app and a **client** identity on the gateway. Monero mutual TLS uses its own authorized client identity. Import validates the certificate/private-key pair and refuses malformed or encrypted private PEM. Keep CA signing keys on the issuing machine; only public CA certificates and required runtime leaf keys leave it. There is no automatic renewal, remote CA service, or key rotation command.

### Onion service identities

Generate separate identities on a trusted issuing machine. The parent `identities` directory must already exist; each target directory must be new:

```sh
./opsecmkt keys generate onion --out-dir ./identities/storefront
./opsecmkt keys generate onion --out-dir ./identities/administrator
```

Import a retained identity into a new directory:

```text
opsecmkt keys import onion --address FULL_V3_ADDRESS.onion \
  --public-key EXISTING_DIR/hs_ed25519_public_key \
  --secret-key EXISTING_DIR/hs_ed25519_secret_key --out-dir NEW_IDENTITY_DIR
```

| Flag | Applies to | Default and meaning |
| --- | --- | --- |
| `--out-dir` | Both | Required new directory; its parent must exist. Existing targets are refused. |
| `--prefix` | Generate | Empty for random generation; optional fixed lowercase `a-z2-7` prefix of at most six characters. |
| `--workers` | Generate | `0` selects at most eight workers, bounded by `GOMAXPROCS`; explicit counts are `1`–`64`. Select a smaller count to reduce CPU use. |
| `--address` | Import | Required complete checksum-valid v3 address matching both supplied key files. |
| `--public-key` | Import | Required native Tor `hs_ed25519_public_key` file. |
| `--secret-key` | Import | Required private native Tor `hs_ed25519_secret_key` file. |

Each successful command writes `hostname`, `hs_ed25519_public_key` and `hs_ed25519_secret_key`, then prints only the complete public address. Import checks the key pair and expected address. These are Tor's native binary key files, not PEM or Go's Ed25519 seed/private-key serialization. The resulting directory is plaintext private key material; encrypt its offline backup. Generation/import is local and starts no Tor process, descriptor publication or network connection. A running onion service needs its own private identity online.

Prefix generation stays in the foreground until it finds a match or is cancelled. `SIGINT`/`SIGTERM` cancel the search; there is no timeout or progress-rate flag. Six characters can require substantial computation. See [expected search effort and identity precautions](tor.md#choose-and-retain-separate-service-identities). A vanity prefix does not authenticate a service: compare the whole address. Never reuse the same identity for public storefront and private administrator ingress.

### Tor administrator client authorization

Generate each administrator's X25519 client identity on that administrator's machine:

```sh
./opsecmkt keys generate tor-client --public-out ./public/operator.auth \
  --private-out ./private/operator.key
```

This writes a server-side public `descriptor:x25519:KEY` record and a raw private Base32 key. Give only the `.auth` file to the administrator onion service allowlist. Keep the private file on the browser/client machine. Both files use `0600`.

After the service's v3 onion address is known, bind the private key to it:

```text
opsecmkt keys import tor-client --in RAW_OR_BOUND_PRIVATE_KEY \
  --public-out NEW_PUBLIC_FILE.auth --private-out NEW_CLIENT_FILE.auth_private \
  --onion VALID_V3_ADDRESS.onion
```

Use new output paths because existing files are refused. The import accepts a raw key or an existing bound client record, validates its 32-byte canonical Base32 representation, and validates the supplied onion's v3 checksum. Install the resulting `.auth_private` only in the administrator's Tor client configuration. Alternatively, `keys generate tor-client` accepts `--onion ADDRESS` to create a bound file immediately when the address is already known. It does not import or generate the onion service's own private identity. Tor client authorization admits the connection; application login, role, CSRF and step-up checks still apply.

## Managing a Compose installation

```text
opsecmkt deploy COMMAND [flags]
```

Each lifecycle command has an equivalent root alias, such as `opsecmkt redeploy`, and accepts `--dir DIR`, default `./opsecmkt-deployment`. Storage subcommands use the flags in their separate reference below. The private directory stores strict `operator.json`, managed key files, generated Compose/runtime inputs and an installation identity receipt. Retention state uses its separately configured persistent directory. Reuse this directory for the same market.

### Deployment commands

| Command | Effect and limits |
| --- | --- |
| `init` | Validate inputs, then create fresh private state and import/generate independent keys. Production verifies local encrypted storage before staging or keys and records storage identities. Starts nothing. Refuses any existing target directory. See all flags below. |
| `plan` | Validate configuration/material and print topology with secret values omitted. Starts nothing and writes no runtime inputs. |
| `install` | Claim an unused project, generate runtime files, and start configured services with a readiness wait. Refuses unrelated existing project containers/volumes. |
| `up` | Resume the recorded installation, retaining keys and volumes. Rewrites generated runtime inputs and starts its configured services. Use `redeploy` for app-only updates. |
| `redeploy --app-image IMAGE@sha256:DIGEST` | Require an existing installation, persist its desired new app image, update Compose, and run `up -d --wait --wait-timeout 600 --no-deps app`. Workers/configuration/keys/endpoints are retained. Failure leaves desired state in place; there is no automatic rollback. |
| `status` | Show all project containers and health. Does not report the database setup/schema status; use legacy `-status` for that. |
| `down` | Stop/remove project containers/networks with a 60-second stop timeout. Retains all persistent volumes and private files; not a backup or erase command. |
| `logs` | Development: at most 100 lines per service, no follow, bounded output with known secret/URL-credential redaction. Inspect before sharing. Production: explain that app Docker logging is disabled; use administrator diagnostics/status. |
| `doctor` | Validate startup material, recheck production storage bindings and check Docker Compose availability. Does not prove remote readiness, firewall correctness or physical host separation. |
| `host-reboot configure --dir DIR` | Root on the encrypted production app host: install its private fixed-action reboot agent and attach its configuration to the retained installation. New unlock-readiness policies are disabled. Run `up` afterwards to attach the app-only socket. |
| `host-reboot ready --dir DIR --confirm-unlock-rehearsed` | Record root's real OS reboot/manual-unlock acknowledgement for the current kernel boot identity. Repeat after each later host reboot. Requires a configured agent and verified storage; does not request a reboot. |

There are no additional `--app-image` flags on `up`/`install`: use `redeploy` to change it. Deployment mutations use a process lock on Linux/macOS; concurrent changes refuse and process exit releases the lock. Nonroot development operators must use their own runtime UID/GID. Production start commands require Linux root, systemd and a persistent root-owned operator binary. Existing expired certificates do not prevent `status`, `logs` or `down`; startup/plan/doctor/redeployment still reject expired material.

Production `install`, `up`, `redeploy` and `doctor` require the recorded storage
bindings to match this Linux host's actual encrypted paths. Bindings include
mount point/root, filesystem UUID and protecting LUKS UUIDs; they contain no
unlock key. Run the CLI on the service host against its local Docker Engine;
a remote context cannot establish
remote storage proof. `plan`, `status`, `logs` and `down` do not require a passing
storage gate. A storage failure starts nothing; retain the installation and
investigate instead of reinitializing it or selecting development mode.

Production starts refuse active swap and require the operator installation on
persistent encrypted storage. Before application startup, `install`, `up` and
`redeploy` install per-project Docker and containerd storage-check drop-ins and
reload systemd. They do not restart those daemons. The checks affect their whole
host services, including other projects; use the intended dedicated host. Missing
mounts or changed identities stop startup without formatting or erasing storage.

### Preparing and checking production storage

```text
opsecmkt deploy storage COMMAND [flags]
```

| Command | Effect and limits |
| --- | --- |
| `prepare` | Explicitly prepare one new, blank **whole data device** as LUKS2/ext4 and mount it. Linux root and an interactive terminal are required. Refuses root, existing signatures, partitions, mounts and holders. Does not convert an OS root. |
| `unlock` | Verify an existing LUKS2 UUID, prompt through native `cryptsetup`, and mount its existing ext4 filesystem. Does not format it. |
| `verify` | Read-only proof that each path is backed entirely by active LUKS2 storage. Supports Linux ext4/XFS. Unknown, remote, volatile, overlay and unsupported filesystems fail closed. |
| `bind --dir DIR` | After a reviewed production migration, verify current storage and record its identities in the retained installation. Preserves keys, configuration and installation receipt; no data copy or force bypass. |
| `boot-unit --dir DIR --binary /ABSOLUTE/PATH/opsecmkt` | Print an equivalent daemon pre-start drop-in candidate using the retained installation's storage bindings. Print-only; does not install it or unlock a disk. Production lifecycle starts install their Docker/containerd checks separately. |
| `check-bound --dir DIR` | Verify saved path/UUID bindings without querying the Docker daemon. Intended for a reviewed pre-start check. |

`prepare` and `unlock` accept these flags:

| Flag | Requirement |
| --- | --- |
| `--device` | Absolute block-device path; prefer its stable `/dev/disk/by-id/` identifier. |
| `--name` | Mapper name for this operation. |
| `--mount` | Clean absolute empty private destination. Its existing parents must be root-owned and not writable by other users. |
| `--confirm-device` | **Prepare only:** exact canonical resolved device path, independently checked as the new empty data disk. Formatting is destructive to that selected device. |
| `--expected-luks-uuid` | **Unlock only:** required canonical lowercase UUID of the existing LUKS2 volume; checked before passphrase entry. |

`verify` requires repeatable `--path PATH` arguments. An optional
`--expected-luks-uuid UUID` requires each path to have exactly that single LUKS
identity. Inspect separate volumes in separate invocations when using a UUID pin:

```sh
./opsecmkt deploy storage verify --path /srv/opsecmkt-encrypted \
  --expected-luks-uuid YOUR_RECORDED_LUKS_UUID
```

Passphrases go directly to native `cryptsetup`, never flags, environment
variables, configuration receipts or key files. These commands do not install
packages, configure automatic unlock, edit `/etc/fstab`, start services, migrate
an operating-system root, export recovery keys or invoke emergency erasure.
Inspect the [safe data-device procedure](server-hardening.md#prepare-an-additional-blank-data-device)
before preparing a disk, and the
[boot/migration procedure](server-hardening.md#keep-services-stopped-until-storage-is-ready)
before resuming an existing installation.

### Configuring private application-host reboot control

`deploy host-reboot configure` requires the retained production installation,
private administrator ingress, verified encrypted storage and a persistent
root-owned binary. It installs `opsecmkt-host-control-PROJECT.service`, a root
agent with a single fixed application-host reboot operation. It starts a new
policy with `unlock_rehearsed=false`. `deploy up` mounts only its dedicated Unix
socket directory into the app and sets `HOST_CONTROL_SOCKET`; it grants no
Docker/systemd socket or root credential.

After the real OS reboot/unlock rehearsal, root may run `host-reboot ready` with
the acknowledgement flag. It binds the statement to the current kernel boot
identity; a copied policy or later host boot cannot enable the button. Root must
acknowledge each new boot after manual unlock and verification. This remains a
statement by the operator, not proof of recovery. The web form additionally requires private ingress, administrator
role, current password, enrolled TOTP, CSRF, a form nonce and explicit manual-unlock
acknowledgement. A committed database audit precedes its one immediate host
request. Acceptance has a minimum 30-second response grace and 10-minute cooldown;
uncertain responses and form/agent restarts never schedule a retry automatically.
See the [activation, receipt and recovery runbook](../operator-guide.md#enabling-the-private-application-host-reboot-control).

For service-definition inspection, the root entry point is:

```text
opsecmkt host-control-agent --deployment-dir DIR \
  --socket /run/opsecmkt-host-control-PROJECT/control.sock \
  --state-dir /var/lib/opsecmkt-host-control/PROJECT \
  --app-uid UID --app-gid GID \
  --unlock-ready-file /etc/opsecmkt-host-control/PROJECT.json
```

Use the saved project and positive numeric app UID/GID. The root peer checks
and encrypted receipt/policy checks are mandatory. The app cannot select a host,
command, argument or unlock secret in a request. Starting this service requests
no reboot. Use `deploy host-reboot help` and `host-control-agent --help` for the
implemented options.

Docker commands use the explicitly saved context name, not ambient `DOCKER_HOST`, `DOCKER_CONTEXT` or Compose controls. Inspect the selected context: a context name can itself point to a remote engine. Normal Docker client authentication/configuration still applies. The tool does not invoke Bash wrappers or install Docker.

### All deployment initialization flags

Only `init` accepts these flags. Unless a default is shown, a flag is unset/optional, subject to the production/escrow requirements below.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dir` | `./opsecmkt-deployment` | New private installation directory; its parent must exist. |
| `--mode` | `dev` | `dev` or `prod`. Production manages only the app host; it cannot start dependency containers there. |
| `--docker-context` | `default` | Explicit saved Docker context name. |
| `--project` | Generated `opsecmkt-` plus random suffix | Stable project identity. Cannot be changed after installation. |
| `--app-image` | Required | The packaged Opsecmkt application to run. Follow [build, publish and get the image reference](binary.md#building-and-publishing-the-application-image), then pass the resulting value as `--app-image "$APP_IMAGE"`. |
| `--postgres-image` | Pinned PostgreSQL 17 Alpine image | Development local DB image; ignored when using an external DB. Exact current reference is printed by `init --help`. |
| `--tor-image` | Unset | Development public Tor image matching the repository Tor image contract; required for development `--exposure tor`. |
| `--admin-tor-image` | Unset | Private administrator Tor image including digest; required for production Tor exposure. |
| `--admin-client-auth-dir` | Unset | Source directory of public administrator `descriptor:x25519` `.auth` records. Validated and copied into a managed snapshot. Never give it client private keys. |
| `--onion-address` | Generate for managed public Tor | Import the existing full public v3 service address, together with both public-role key flags below. Only for development's managed `--tor-image`. |
| `--onion-public-key` | Generate with public identity | Existing Tor `hs_ed25519_public_key` file matching `--onion-address`. |
| `--onion-secret-key` | Generate with public identity | Private `0600` Tor `hs_ed25519_secret_key` file matching that address and public key. |
| `--onion-prefix` | Empty/random | Optional lowercase `a-z2-7` prefix, at most six characters, for a new managed public identity. Cannot be combined with imports. |
| `--admin-onion-address` | Generate for managed admin Tor | Import the full private administrator v3 service address, together with both admin-role key flags below. Requires `--admin-tor-image`. |
| `--admin-onion-public-key` | Generate with admin identity | Existing administrator Tor `hs_ed25519_public_key` file matching `--admin-onion-address`. |
| `--admin-onion-secret-key` | Generate with admin identity | Private `0600` administrator Tor `hs_ed25519_secret_key` file matching that address and public key. |
| `--admin-onion-prefix` | Empty/random | Optional lowercase `a-z2-7` prefix, at most six characters, for a new private administrator identity. Cannot be combined with imports. |
| `--retention-dir` | Installation directory plus `/retention` | Clean absolute persistent retention ledger directory. Use a path outside runtime tmpfs for RAM-unlocked installations. |
| `--host-control-dir` | Unset | Optional app-only host-control socket directory. Production private admin only; must exactly match `/run/opsecmkt-host-control-PROJECT` for the saved project. Prefer `deploy host-reboot configure` to install the disabled agent after setup. |
| `--clamav-image` | Unset | Development ClamAV image with signatures; enables integrated uploads. |
| `--monero-image` | Unset | Development pruned stagenet daemon image. Requires wallet image and escrow. |
| `--monero-wallet-image` | Unset | Development dedicated escrow wallet-RPC image. Requires daemon image and escrow. |
| `--exposure` | `clearnet` | `clearnet` or `tor`; production Tor ingress runs on the separate gateway host. |
| `--site-access` | `public` | First-start site access: `public` or `members`. Saved as `INITIAL_SITE_ACCESS` for initialization in PostgreSQL. |
| `--registrations` | `open` | First-start registration policy: `open` or `closed`. Saved as `INITIAL_REGISTRATIONS`; independent of site access. |
| `--branding-upload-origin` | Unset | Fixed public upload-gateway origin for the confirmed private-admin logo handoff. Saved as `BRANDING_UPLOAD_ORIGIN`. |
| `--app-bind` | `127.0.0.1:8080` | Host app listener, explicit loopback/private IP and port. |
| `--backend-bind` | `127.0.0.1:8443` | Production upload backend host listener, explicit loopback/private IP and port. |
| `--app-host` | Unset | Required production app host identity. |
| `--gateway-url` | Unset | Required production separate HTTPS or HTTP onion gateway origin. |
| `--runtime-uid` | Operator UID, or `65532` when root | Nonroot app runtime UID and private mounted-file ownership. |
| `--runtime-gid` | Operator GID, or `65532` when root | Nonroot app runtime GID and private mounted-file ownership. |
| `--database-url-file` | Local generated DB in dev | Import external PostgreSQL URL from a private file. Required for production. |
| `--database-ca` | System trust | Import public external DB TLS CA and rewrite managed URL's `sslrootcert` to its mounted path. |
| `--app-secret-key-file` | Generate | Import stable existing app key instead of generating it. |
| `--setup-token-file` | Generate | Import first-run setup token instead of generating it. |
| `--audit-signing-key-file` | Generate | Import an independent Ed25519 seed instead of generating it. |
| `--audit-recipient-key` | Unset | Import OpenPGP public audit-export recipient; enables encrypted exports. |
| `--monero-network` | `stagenet` | `stagenet` or explicit real-funds `mainnet`; development requires stagenet. |
| `--escrow-xmr` | Boolean, `false` | Enable custodial Monero escrow. |
| `--monero-wallet-rpc-url-file` | Unset | Import existing dedicated wallet endpoint; cannot be combined with local Monero images. |
| `--monero-daemon-url-file` | Unset | Import existing daemon endpoint; cannot be combined with local Monero images. |
| `--monero-wallet-secret-file` | Generate with escrow | Import existing stable wallet secret. Required when retaining existing wallets. |
| `--monero-seed-key` | Unset | Import only the OpenPGP public recovery key; required with escrow. |
| `--monero-rpc-ca` | System trust | Import Monero HTTPS public CA. |
| `--monero-rpc-cert` | Unset | Import Monero mutual-TLS client certificate; pair with key. |
| `--monero-rpc-key` | Unset | Import Monero mutual-TLS client private key. |
| `--tls-cert` | Unset | Import app HTTPS server certificate; required in production with key. |
| `--tls-key` | Unset | Import app HTTPS server private key. |
| `--upload-ca` | Unset | Import upload client-auth CA; required in production. |
| `--upload-cert` | Unset | Import upload backend server certificate; required in production. |
| `--upload-key` | Unset | Import upload backend server private key; required in production. |
| `--dependency-report-dir` | Unset | Existing public signed-report directory, mounted read-only into the app. Keep worker seed elsewhere. |
| `--dependency-report-public-key` | Unset | Independently import/pin worker public key outside replaceable report directory. Required with report directory. |
| `--set KEY=VALUE` | None | Repeat for supported nonsecret runtime settings listed below. This is not an arbitrary environment override. |

Every selected image must have a real repository digest with 64 lowercase hexadecimal characters. Local dependency containers are a **development topology**, not an alternative image-address syntax. There is no local-tag/image-ID adoption mode, automatic Docker image build, `latest` fallback, or implicit app image. Publish/import a reviewed digest-addressable image beforehand, or run native `serve` without an app container.

Production requires separate declared app/database/gateway hosts, verified database TLS, app HTTPS, isolated uploads with backend mutual TLS, and no local database/payment/scanner/public-ingress images. Optional private administrator Tor ingress is a separate app-host service using only its private Unix socket and public authorization snapshot; production Tor exposure requires that path. When escrow is on, both Monero URLs need verified HTTPS and credentials or a configured client identity, separate from app/database/gateway. Daemon and dedicated wallet RPC may share their Monero host. Host declarations cannot prove underlying physical separation or detect every DNS alias.

For managed private administrator ingress, pass `--admin-tor-image` and `--admin-client-auth-dir` together with `--exposure tor`. The source allowlist must contain 1–256 regular non-symlink files named safely with a `.auth` suffix, each holding only a canonical public `descriptor:x25519:` record with an optional final newline. Extra files, private client keys, malformed/noncanonical keys and an empty directory are refused. Initialization snapshots those public records; later changes to the source directory do not silently change authorization.

For each enabled managed Tor role, initialization generates a local random identity unless its three import flags are supplied together. The imported address must match the public/private Tor key pair; a hostname alone cannot restore an identity. Prefix generation and imports are mutually exclusive. Public and private administrator identities must differ. Private directories `onion-public/` and `onion-admin/` retain the selected role's `hostname` and both Tor key files; `init` and `plan` print only the public addresses and backup guidance. No Tor connection or publication occurs during generation/import. Six-character prefix searches can require substantial CPU time; see the [effort calculation](tor.md#choose-and-retain-separate-service-identities). Production's public onion belongs to the separate gateway host and is generated/imported there through the key CLI and that host's reviewed Tor setup.

The generated networkless `admin-tor-prepare` init service uses the app image to validate/copy the public snapshot and selected administrator identity into `tor_admin_identity` and assign ownership. It receives only that snapshot, the selected private Tor identity and the dedicated identity volume. The separate `tor-admin` service waits for successful preparation and app health, then runs native Tor as the saved numeric runtime UID/GID. It has its own egress network, persistent identity volume and read-only app Unix socket/configuration. Neither Tor service receives database, wallet, app-secret or retention mounts, and neither publishes an administrator TCP port. This avoids relying on an image's default Tor user or on a root-owned authorization bind mount being accepted by Tor. Development public Tor has its own networkless identity helper and separate `tor_identity` volume.

The public storefront cannot obtain private ingress context by spoofing Host/forwarding headers. Administrator photo/avatar uploads remain unavailable on the current isolated private-admin path. Logo uploads instead use a preconfirmed, one-use handoff to `--branding-upload-origin`; this does not grant general private ingress. See [market branding](../operator-guide.md#changing-the-market-name-and-logo). Obtain the administrator onion's public hostname from that selected service and bind each client's key as described above; ordinary app redeployment does not generate another onion identity.

For a replacement application host, retain the managed administrator identity files or restore its matching `tor_admin_identity` volume, alongside configuration and persistent retention state. Run `install`/`up` to prepare a fresh volume from the selected identity and create a missing administrator Tor service before relying on `redeploy --no-deps app`. Copying an operator/RAM bundle does not copy the whole named volume. An already populated volume must match the selected identity; mismatch or partial state refuses startup without replacing keys. Legacy configurations without a managed identity source preserve only a valid populated volume and refuse an empty one. There is no automatic live-volume export, identity-rotation or volume-backup command; use the documented recovery procedure with reviewed targets.

Private imports are copied into the new installation. Changing an import's original file afterwards does not rotate an installed key or trust pin. `init` never adopts an existing `.env`, database, Docker project or wallet automatically. For an existing market, preserve its keys, wallet secret, seed recipient, audit identity, retention state and network pins; follow the [reuse and replacement procedure](binary.md#rebuilding-or-replacing-only-the-application).

### Private Tor volume preparation entry point

```text
opsecmkt admin-tor-prepare --source PUBLIC_AUTH_DIR \
  [--identity-source PRIVATE_IDENTITY_DIR] --destination TOR_IDENTITY_DIR \
  --uid NONROOT_UID --gid NONROOT_GID
opsecmkt tor-identity-prepare [--identity-source PRIVATE_IDENTITY_DIR] \
  --destination TOR_VOLUME_DIR --uid NONROOT_UID --gid NONROOT_GID \
  [--subdir marketplace]
```

The generated services invoke these commands automatically during `install`/`up`; operators normally do not run them against host directories. Each is a one-shot filesystem command, with no network listener or background process. Both provide `--help`.

| Flag | Applies to | Default and meaning |
| --- | --- | --- |
| `--source` | Admin helper | Required existing clean absolute directory of validated 1–256 public `.auth` records. Cannot be `/` or overlap the destination. |
| `--identity-source` | Both | Selected private native identity directory. Required to initialize a fresh volume; omit only to preserve a complete valid legacy identity already in the volume. Cannot overlap the destination. |
| `--destination` | Both | Required existing clean absolute dedicated Tor volume directory; cannot be `/`. |
| `--uid` | Both | Default `0` is invalid. Required numeric Tor runtime UID from `1` through `2147483647`. |
| `--gid` | Both | Default `0` is invalid. Required numeric Tor runtime GID from `1` through `2147483647`. |
| `--subdir` | Public helper | Empty uses the volume root; the only accepted nonempty value is `marketplace`, used by generated development public Tor. |

The selected native identity and administrator records are validated before use. Fresh identity files are created exclusively; existing valid files are compared and retain their bytes. A partial, invalid or different identity refuses startup. The admin helper also installs valid public records before removing obsolete authorization files, refuses unsafe volume entries and syncs its changes. Root can assign the configured IDs; a nonroot invocation must select its own effective UID/GID and have access to the destination. Generated helpers run as root with only the required ownership capabilities, `network_mode: none`, no restart loop and explicit role-specific mounts. The Tor processes run without root. Helpers do not generate replacement identities, export live service keys, rotate addresses or create backups; the destination contains private service keys.

### Additional runtime settings

Choose initial access and registration when creating an installation. For example,
add `--site-access members --registrations closed` to `deploy init` for a site
that requires sign-in and accepts no new accounts. The same flags are available
on `serve`; its environment equivalents are `INITIAL_SITE_ACCESS=members` and
`INITIAL_REGISTRATIONS=closed`.

These values initialize database settings only when absent. Later changes under
**Admin → Site access and registration** survive application restarts and
redeployments; changing an initial flag or environment value does not replace
them. First-administrator setup remains available. See
[managing site access](../operator-guide.md#site-access-and-registration) for the
four combinations and verification steps.

For managed deployments, use repeated `init --set KEY=VALUE`, or deliberately edit the corresponding `settings` entry in private `operator.json` before `plan` and `up`. For native `serve`, use the same environment variable names. `serve` does not accept `--set`.

| Variable | Default | Accepted setting |
| --- | --- | --- |
| `INITIAL_SITE_ACCESS` | `public` | `public` or `members`; initializes the saved site-access setting only when absent. |
| `INITIAL_REGISTRATIONS` | `open` | `open` or `closed`; initializes the saved registration setting only when absent. |
| `BRANDING_UPLOAD_ORIGIN` | Unset | Fixed public gateway origin for private-admin logo uploads. Unlike the `INITIAL_*` policy settings, this runtime routing value is read on each application startup. |
| `OFFICIAL_ADDRESSES` | Unset | Comma-separated public market addresses (at most 8 HTTPS or HTTP v3 onion origins), read on each startup and named in every encrypted PGP sign-in code. `init` and every command that loads `operator.json` refuse the private administrator onion. |
| `MONERO_NETWORK` | Managed: `stagenet` | `stagenet` or explicit `mainnet`; development remains stagenet. |
| `ESCROW_XMR` | `off` | `on` or `off`, with all escrow prerequisites. |
| `PAYMENT_CONFIRMATIONS_XMR` | `10` | Integer `1`–`1000`; effective deposit threshold is at least `10`. |
| `PAYMENT_POLL_INTERVAL` | `30s` | Go duration `1s`–`1h`. |
| `PAYMENT_EXPIRY` | `24h` | Go duration `10m`–`720h`. |
| `MONERO_MAX_TIP_AGE` | `1h` | Go duration `20m`–`24h`. |
| `ESCROW_XMR_FEE_BPS` | `0` | Integer `0`–`10000`; nonzero fee needs a matching valid fee address. Host-only with the address: no admin form changes either, and a change is announced to administrators at the next start. |
| `ESCROW_XMR_FEE_ADDRESS` | Unset | Monero fee address on the configured network. |
| `ESCROW_XMR_SLOTS` | `20` | Integer `5`–`200`; ready escrow accounts. |
| `ESCROW_XMR_MAX_FEE` | `10000000000` | Piconero, `1`–`100000000000`; default is `0.01 XMR`, maximum `0.1 XMR`. |
| `ESCROW_XMR_TOR_RELAY` | Blank | Blank keeps Tor relay held unless the legacy local node overlay is present. `remote-tor` declares operator-configured Tor egress on a separate production payment host; requires `prod`, Tor exposure, escrow enabled and authenticated HTTPS RPC endpoints. `clearnet-accepted` accepts direct relay exposure. Neither value configures Tor. |
| `IMAGE_STORAGE_LIMIT_MB` | `2048` | Whole number `1`–`1048576`; aggregate stored-photo/avatar cap in MB, using 1024×1024 bytes per MB. |
| `SCANNER_MAX_SIGNATURE_AGE_DAYS` | `30` | Whole number `7`–`90`. Gateway scanning uses its own `--max-signature-age-days`. |
| `DATABASE_CONNECT_TIMEOUT` | `15s` | Go duration `1s`–`10m`. |
| `MIGRATION_LOCK_TIMEOUT` | `10m` | Go duration `1s`–`24h`. |
| `MIGRATION_TIMEOUT` | `10m` | Go duration `1s`–`24h`. |
| `RETENTION_ENABLED` | `off` | `on`/`off`; enable only with the documented retention enrollment/recovery procedure. |

This table is the complete managed `--set` allowlist. Direct native environments additionally use paths such as `RETENTION_STATE_DIR` and `AUDIT_EXPORT_RECIPIENT`; they have no corresponding `serve` flag. Retention requires a clean absolute persistent state path. Audit export's recipient is a public armored key. See [operator configuration](../operator-guide.md#feature-configuration) for operational consequences.

`HOST_CONTROL_SOCKET` is an optional fixed local socket path for native app-host
reboot control. It has no `serve` flag or managed `--set` entry. The generated
production environment supplies it only when `HostControlDir` is configured;
web input cannot change its destination. Readiness and explicit operator
activation remain required even when that path is set.

For a production payment host whose daemon is already restricted to Tor egress,
add `--set ESCROW_XMR_TOR_RELAY=remote-tor` to `deploy init`. For an existing
installation, set `settings.ESCROW_XMR_TOR_RELAY` to `remote-tor` in its private
`operator.json`, then run `deploy plan --dir DIR` and `deploy up --dir DIR`.
The plan and administrator panel identify this as an operator declaration.
The application cannot inspect the remote proxy or firewall. Verify the
[payment-host routing checks](../operator-guide.md#declaring-separate-payment-host-tor-egress)
before enabling relay and after network maintenance. Stored transactions resume
unchanged after restart. Keep the value blank while that routing is unverified.

## Offline bundles and RAM unlock

The tmpfs workflow below remains available for development and inspection. New
production start commands require a persistent LUKS2-backed operator directory
and reject the former RAM-unlocked production layout. Production boot checks
need the retained configuration after encrypted root unlock; a RAM-only
production boot/unlock design is not implemented. Offline `pack` still creates a
bounded encrypted configuration copy, not a whole-system backup or migration.

```text
opsecmkt secrets pack --dir OFFLINE_INSTALL_DIR --recipient-public-key PUBLIC.asc --out BUNDLE.pgp
opsecmkt secrets mount [--dir /run/opsecmkt-secrets]
opsecmkt secrets unlock --dir /run/opsecmkt-secrets/INSTANCE
```

These commands separate preparation on the issuing machine from temporary runtime access on the application host. They start no application/worker service. They do not accept a private recovery key, passphrase, plaintext bundle or secret value as a flag.

| Command/flag | Default | Meaning |
| --- | --- | --- |
| `pack --dir` | Required | Existing private offline installation directory, containing `operator.json` and referenced managed files. Packing is available on supported desktop/server platforms; no mount is performed. |
| `pack --recipient-public-key` | Required | One armored, currently usable OpenPGP public encryption key. Private recovery keys are refused. |
| `pack --out` | Required | New armored encrypted output file, mode `0600`; parent must exist and existing output is refused. |
| `mount --dir` | `/run/opsecmkt-secrets` | Linux root only: create a fresh 16 MiB tmpfs with `nosuid,nodev,noexec` and root-owned mode `0700`. |
| `unlock --dir` | Required | Linux root only: new clean absolute instance path under a verified hardened tmpfs parent. Reads the decrypted JSON bundle **only from stdin**. Refuses an existing destination. |

Packing includes `operator.json`, referenced `secrets/*`, configured public `admin-clients/*.auth`, managed `onion-public/` and `onion-admin/` identity triples, and an optional `installation.json` receipt. Each included onion key pair, hostname and configured address must match; public/admin addresses must differ. It excludes retention state, generated Compose/runtime inputs and whole wallet/blockchain/onion Docker volumes. Extra unreferenced files in managed key directories are refused rather than silently packed. The bundle format is versioned JSON with base64 file bytes, bounded to **64 files**, **64 KiB per file**, and **1 MiB total plaintext JSON**. Paths cannot traverse directories or introduce symlinks; private OpenPGP recovery blocks are refused. Large administrator allowlists must also fit this aggregate bundle bound.

Prepare `--retention-dir` on separate persistent storage before packing. Set the intended server `retention_dir` and `dependency_report_dir` in the offline configuration; those absolute external paths are preserved. Keep persistent state on storage with the required recovery guarantees, since a non-tmpfs result alone cannot attest a backing provider's durability. The standard managed `SOURCE_INSTALL_DIR/admin` socket path is relocated to the unlocked instance's `admin` directory. An explicitly customized `admin_socket_dir` remains as configured, so review it for the intended server. Review network endpoints, runtime UID/GID, trust pins and Docker context for that server. Do not change established custody keys or database/network pins while preparing a new bundle.

Run packing on the offline/issuing machine:

```sh
./opsecmkt secrets pack --dir ./offline-market \
  --recipient-public-key ./public/deployment-recovery.asc \
  --out ./encrypted/market.pgp
```

On the application host, use an already-authorized Linux root execution path:

```sh
sudo /usr/local/bin/opsecmkt secrets mount --dir /run/opsecmkt-secrets
```

An existing destination is accepted only when empty and already the exact verified hardened tmpfs mount. An ordinary existing directory, nonempty mount, symlink, missing mount capability, or missing required mount flags is refused. `unlock` independently verifies the real containing mount; naming a disk directory `ram` does not qualify.

Decrypt on the administrator's trusted machine and stream through an authenticated encrypted connection. This example assumes the remote account already has narrowly authorized noninteractive execution of the shown unlock command; it is not a request to grant general passwordless root access:

```sh
gpg --decrypt ./encrypted/market.pgp | \
  ssh -T admin@APP_HOST 'sudo -n /usr/local/bin/opsecmkt secrets unlock --dir /run/opsecmkt-secrets/market'
```

Keep the recovery private key and decrypted bundle off the host's persistent storage. Do not redirect plaintext into a server file or mix a sudo password into the bundle stream. After successful unlock, inspect the retained configuration with `deploy plan --dir /run/opsecmkt-secrets/market`. Only a development installation can use the shown RAM instance for `install`/`up` or existing-service `redeploy`; production starts require persistent encrypted configuration. Controller metadata stays root-owned; only the mounted secret subtree is assigned to the configured runtime UID/GID. Public administrator records are prepared for the separate Tor reader during installation.

The retention ledger must remain outside the RAM instance on verified persistent storage; unlock refuses a retention path inside the instance or on known tmpfs. Preserve that ledger independently across restarts and database restoration. `pack` can retain configured managed onion identity triples, but does not back up databases, wallets or whole Docker volumes. Re-unlocking an old configuration bundle cannot replace current retention state or erase an incident's external recovery requirements. Tor runtime identity volumes remain persistent outside this RAM instance.

This is temporary filesystem placement, not a claim that plaintext can never reach storage. tmpfs can interact with swap; core dumps, hibernation, snapshots, the administrator's terminal/session, Docker metadata and a live privileged host remain relevant. Configure those host controls and deliberate logging separately. Reboot loses the tmpfs instance; `secrets mount`/`unlock` do not auto-start, auto-unlock, wipe disks, unmount an active service, or guarantee memory zeroization. A `noexec` secret mount also does not host the executable; install the binary separately.

## Legacy host commands

These commands connect to the configured database and exit without starting the HTTP server, wallet runtime or background worker, except `-preview`, which serves the read-only UI. They use environment settings and the private `_FILE` loader above. They do **not** read `operator.json` or accept `serve` flags. Choose one host action per invocation.

For example, this passes only a path in the environment and asks for the password interactively:

```sh
DATABASE_URL_FILE=./private/database.url ./opsecmkt -bootstrap-admin ADMIN_HANDLE -site-name 'My Market'
```

| Flag/form | Input, result and requirements |
| --- | --- |
| `-bootstrap-admin HANDLE` | Create the first administrator after the app has initialized the schema. Refuses an already-installed market. Needs database access; no setup token. Password is prompted twice without echo, or read as one line from stdin. |
| `-site-name NAME` | Only with bootstrap; default `OPSMKT`, at most 80 characters. Put before any positional role argument in other commands; do not mix host actions. |
| `-set-role HANDLE ROLE` | Existing account role: `buyer`, `vendor`, `moderator` or `admin`. Reads no stdin. Host-only way to add/remove administrators; refuses removal of the last active admin. Role changes have documented listing/session effects. |
| `-reset-admin-password HANDLE` | Administrator break-glass password change; ends its sessions/pending sign-ins. Reads a new password from stdin or a hidden terminal prompt; does not reset second factors. |
| `-reset-admin-factors HANDLE` | Administrator break-glass: disable TOTP, recovery codes and PGP sign-in; end sessions/pending sign-ins. Reads no stdin. Password and saved PGP public key remain. |
| `-export-audit N` | Export retained settled audit events through positive ID `N`, with detached signature. Requires database and `AUDIT_SIGNING_KEY`/its `_FILE`. Refuses existing output files. |
| `-export-audit-from N` | With export: exclude IDs at or below this nonnegative bound; default `0`, cannot exceed upper bound. |
| `-export-audit-dir DIR` | With export: existing output directory, default `.`. Private `.jsonl`/`.sig` and retention metadata are written; configured recipient adds encryption suffixes. Empty retained range creates no files. |
| `-enroll-retention` | Enroll original database and current external retention state before the documented backup/encoding procedure. Requires `RETENTION_ENABLED=on` and `RETENTION_STATE_DIR`. Starts no expiry/replay/wallet process. |
| `-status` | Read-only database query, no migration. Prints `installed=true\|false schema=NNN keyroot=app_secret_key\|setup_token`; an empty/unmigrated DB reports schema `000`. Needs database URL. |
| `-healthcheck` | Probe the configured listener's `/healthz`, same port as `ADDR`, default `8080`; success requires HTTP `200` and body `ok`, with no success output. Uses verified HTTPS when `TLS_CERT` is set, and `OPSECMKT_APP_HOST` for its TLS name. Starts no server. |
| `-preview` | Foreground read-only sample UI. No DB needed; `PREVIEW_ADDR` defaults to `127.0.0.1:8080` and must name a loopback IP. |

Bootstrap/password-reset passwords must be 12–72 bytes, valid UTF8, and contain no NUL. A pipe/file supplies one line; extra nonempty lines are refused. Prefer the hidden terminal prompt. If automation supplies a private stdin file, pass it by input redirection rather than putting its contents in a command argument.

Examples below use existing private files and an existing export directory:

```sh
DATABASE_URL_FILE=./private/database.url ./opsecmkt -status
DATABASE_URL_FILE=./private/database.url ./opsecmkt -set-role ACCOUNT_HANDLE moderator
DATABASE_URL_FILE=./private/database.url ./opsecmkt -reset-admin-password ADMIN_HANDLE
DATABASE_URL_FILE=./private/database.url ./opsecmkt -reset-admin-factors ADMIN_HANDLE
DATABASE_URL_FILE=./private/database.url AUDIT_SIGNING_KEY_FILE=./private/audit.seed \
  AUDIT_EXPORT_RECIPIENT=./public/audit-recipient.asc \
  ./opsecmkt -export-audit 1000 -export-audit-from 500 -export-audit-dir ./exports
RETENTION_ENABLED=on RETENTION_STATE_DIR=/srv/opsecmkt/retention \
  DATABASE_URL_FILE=./private/database.url ./opsecmkt -enroll-retention
./opsecmkt -preview
```

The standalone `verify-audit` release executable verifies a decrypted export against a separately pinned public key. It is not an `opsecmkt verify-audit` subcommand. See [audit verification](../ci-cd.md#release-a-tested-version) and [encrypted exports](../operator-guide.md#encrypted-audit-exports).

## Exit status and supervision

| Exit | Meaning |
| --- | --- |
| `0` | Successful command/help, healthy probe, completed report write, or graceful normal termination. It is not proof that remote deployment boundaries or every worker release check succeeded. |
| `1` | New-command parse/validation/runtime errors, private file failure, DB/admin operation failure, unhealthy probe, or server startup/runtime failure. |
| `2` | Legacy Go flag parser errors, unexpected legacy positional arguments, explicitly rejected legacy flag combinations, or `-enroll-retention=false`. |
| `130` | Interrupt during the hidden password prompt, after terminal echo is restored. Other abrupt signals may have OS/service-manager-specific statuses. |

Use `--help` with the new server/gateway/deployment interfaces or `keys help`. Legacy host action validation generally returns `1`; not every bad argument returns `2`. The version worker can exit `0` after writing unavailable-source entries; deployment `status`/`doctor` are inspection commands, not full recovery evidence.

Guided review answered `n` is a successful cancellation (`0`). `quit`/EOF and context-cancelled prompts/searches return an error (`1`); none of those starts the next unconfirmed operation. If configuration was already saved, it remains saved.

`serve`, `upload-gateway`, and continuous `versions-worker` stay in the foreground. A successful background shell launch is not a healthy deployment. Configure service restart limits, startup/stop deadlines and deliberate logging on each host. Managed production app Docker logging is `none`; development uses bounded rotation. Other host/service logs need their own policy. No command automatically reacts to a provider inquiry, canary event or external signal by destroying data. The explicitly confirmed emergency LUKS operation below destroys keyslots; it is not a log-history scrubber, audit timestamp rewrite, backup secure-deletion tool, or Git-history operation.

## Manual emergency commands

```text
opsecmkt emergency plan --dir DIR [--device /dev/DEVICE] [--ram-dir DIR] [--reboot]
opsecmkt emergency stop --dir DIR --confirm-project PROJECT [--ram-dir DIR]
opsecmkt emergency erase-luks --dir DIR --confirm-project PROJECT \
  --device /dev/DEVICE --confirm-device /dev/DEVICE [--ram-dir DIR] [--reboot]
```

These are explicit incident operations, not automatic triggers. Review `plan` first and authorize the concrete saved project, device and host consequences before running a mutation. No device has a default, and no device name in this reference identifies a real target.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dir` | `./opsecmkt-deployment` | Private saved installation directory identifying the Compose project. |
| `--confirm-project` | Unset | Required exact saved project name for `stop` or `erase-luks`. |
| `--device` | Unset | Exact clean absolute `/dev/` path to an existing non-symlink LUKS block device. Supplying it to `plan` performs read-only device/LUKS checks. |
| `--confirm-device` | Unset | For `erase-luks`, repeat the identical device path. Missing or mismatched confirmation refuses erasure. |
| `--ram-dir` | Unset | Optional cleanup of this same installation at `/run/opsecmkt-secrets/INSTANCE`, only after selected containers have stopped and tmpfs is verified again. |
| `--reboot` | Boolean, `false` | Request an immediate **whole-host unsynced reboot** after successful keyslot erasure. Requires an explicit device; unsupported for ordinary `stop`. |

`plan` prints the selected order without stopping containers, deleting files, erasing keyslots or rebooting. `stop` force-stops/removes the selected Compose containers and retains volumes; it is not the graceful `deploy down` flow. An `emergency.json` marker blocks ordinary restart of that retained installation pending owner recovery review; no automatic reset command exists. If the RAM instance itself is removed or lost, preserve the incident/recovery decision separately and do not resume an old bundle without that review. Forced payment interruption may need reconciliation before settlement work resumes.

`erase-luks` first stops that selected deployment. If stop fails, RAM cleanup, erasure and reboot do not run. Optional RAM cleanup unlinks only the verified named instance. Device validation is repeated immediately before invoking `cryptsetup luksErase --batch-mode` against the exact confirmed device. LUKS device operations require **Linux root**, an existing block device, and a trusted system `cryptsetup` executable at `/usr/sbin/cryptsetup` or `/sbin/cryptsetup`. There is no arbitrary shell command argument.

Erasing all LUKS keyslots can make encrypted data permanently unrecoverable. It does **not** wipe the disk's data blocks, erase header backups, close already-open mappings, remove remote copies, or guarantee memory zeroization. Existing mappings and saved headers can retain access. Optional unsynced reboot affects every service on the host and can lose unrelated in-flight filesystem writes. Preserve the authorized incident record and assess those boundaries explicitly; do not treat successful command output as a secure-wipe guarantee.

There is no automatic provider/canary monitor connected to these actions and no general history-wipe or `secrets wipe` command. A stopped or erased deployment is not automatically restored, rekeyed or restarted.

## Moving from Bash to Go

The native commands below execute Go code and direct Docker invocations. They do not wrap or source the Bash installer. Existing checkout-based installations continue to use their existing `.env`/Compose controls until an explicit, tested migration is prepared; do not point two independent installations at the same escrow wallets or accidentally reuse their project names.

| Existing workflow | Native coverage and remaining work |
| --- | --- |
| `scripts/install.sh init` / `--local` | Fresh development: guided `setup`, or `deploy init --mode dev --app-image DIGEST`, then `plan` and `install`. Flag-based `init` starts nothing and does not prompt. It defaults to stagenet rather than the Bash installer's mainnet local-node choice. |
| `init --external-db URL` | `deploy init --database-url-file FILE`; add production profile/TLS/separation flags when appropriate. No credential URL in argv. Does not automatically adopt an existing market's key/state. |
| `init --admin HANDLE` | After schema initialization, invoke native `-bootstrap-admin HANDLE`, using the saved DB URL file and stdin password. No combined `deploy init --admin` flag. |
| `install.sh check` | `deploy plan` validates native configuration/material; `doctor` also checks Compose availability. They do not reproduce every source-checkout `.env`/overlay check or attest remote host placement. |
| `install.sh status` | `deploy status` for containers, legacy `-status` for DB setup/schema/key root, administrator diagnostics for scanner/dependency state. No single native command reproduces every printed onion/setup field. |
| `install.sh setup-token` / `setup-token --new` | No managed native token-display/rotation command. The initial private token file exists, and key tools can generate a new independent secret, but applying a token rotation requires the documented reviewed procedure. Never rotate the application key accidentally. |
| `install.sh add-mirror` / onion identity import | Native `keys generate onion` / `keys import onion` manage local identity files; fresh managed Tor roles accept matching `--onion-*` / `--admin-onion-*` imports. No native public mirror provisioning, live-volume export or automatic legacy identity adoption exists. Tor client authorization keys remain independent. |
| `install.sh add-scanner` | Fresh development can use `init --clamav-image DIGEST`. Production gateway/ClamAV lives on its separate prepared host. There is no native `add-scanner` command for automatically modifying an existing worker. |
| `install.sh refresh-scanner` | No native signature-refresh command. Maintain the separate scanner and review updated image/signature inputs explicitly; old signatures fail closed. App redeployment does not refresh it. |
| `install.sh upgrade` | Review compatibility/migrations, take documented backups, then `redeploy --app-image DIGEST --dir EXISTING_DIR` for the app only. Native redeploy does not check out source, back up data, update workers, or perform automatic rollback. |
| `scripts/backup.sh`, `scripts/restore.sh` | Remain separate scripts with their documented tools, encryption, retention metadata and payment/restore gates. No native backup/restore command exists; `down` and copying the operator directory are not substitutes. |
| Tor backup/restore, UTF8 maintenance and packaging helpers | Remain documented maintenance tools until native equivalents exist. Preserve onion identities, wallet files, external retention state and authenticated evidence. |
| `scripts/test-*.sh`, `scripts/ci-smoke.sh`, Python/SQL/CI checks | Remain development/release verification tooling. They are not deployment-time dependencies of native `serve` or CLI wrappers to remove blindly. |

For application rebuilds, retain the installation directory, project/context identity, application and wallet secrets, public recipients, TLS trust roots, database/network pins, report pin and current retention ledger. `redeploy` reuses prepared servers/workers. A replacement app host requires the [documented state transfer and certificate/routing review](binary.md#rebuilding-or-replacing-only-the-application); a new app build does not justify reinitializing the database or generating new custody keys.

Follow [Backups and recovery](../operator-guide.md#backups-and-recovery), [upgrade/rollback requirements](../../UPGRADING.md), and [operations verification](../operations-tests.md) for workflows not yet implemented as native commands. Moving them to Go is future implementation work, not an implied capability of this release.
