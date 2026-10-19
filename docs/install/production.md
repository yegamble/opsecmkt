# Deploying a production instance

Deploy OPSEC Market with a separate application host, upload gateway and managed PostgreSQL database. This procedure works with any provider that supplies private networking, verified PostgreSQL TLS and the required host controls.

The base command in this walkthrough disables payments. For a payment-enabled installation, prepare the separate Monero services and append the payment flags before the single initialization in step 4. Keep intake paused until application, payment readiness and recovery checks pass. A started process, a healthy application and a market ready to accept payments are different milestones.

Use your provider's equivalent tools for provisioning. The order is: plan private
roles → prove encrypted boot and trusted access → verify PostgreSQL/TLS → inspect
the app image → initialize once → secure the administrator and pause intake →
verify public journeys and recovery. Keep exact resource identifiers, addresses,
keys and command evidence in your private operator record.

## Before starting

You need permission to create hosts, a managed database, private network rules, DNS records and encrypted backup storage. You also need host administrator access and a trusted workstation for issuing service certificates.

- Read the [capability boundaries](../implementation-status.md), [service topology and sizing](service-topology.md) and [security operations guide](../security-operations.md).
- Install each Linux server with a LUKS2-encrypted operating-system root **before** transferring credentials, creating onion identities or initializing the application. Keep its unlock secret off-host and test a locked boot and recovery. Use the [encryption-first procedure](server-hardening.md#prepare-encryption-before-running-setup); the Go tool does not convert an operating-system root.
- Complete the remaining [server hardening procedure](server-hardening.md) for each role: establish a named SSH operator, approve exact source addresses and enforce both firewall layers. Existing unencrypted hosts require a separately reviewed migration; do not format a live disk.
- Choose a reviewed release tag or commit. Use matching application images and native binaries; record the source revision and image digests.
- Install Docker Engine and Compose 2.17 or later on the application and scanner hosts. Install the standalone `opsecmkt` Linux binary on both; [verify its release checksum](binary.md). Production lifecycle commands require Linux root, systemd and a persistent root-owned binary, such as `/usr/local/bin/opsecmkt`; substitute that path for `./opsecmkt` in the examples when needed.
- Keep a matching source checkout on the backup operator host. The backup scripts need Bash, Python 3, age and PostgreSQL client tools matching or newer than the database server.
- Review the [retention policy](../data-retention-privacy-policy.md) and [enrollment requirements](../retention.md#before-enabling). This walkthrough explicitly enables retention maintenance and retains its current external ledger for recovery; enrollment provides a 30-day grace period before ordinary historical expiry.
- Choose HTTPS or Tor ingress. HTTPS needs a domain, DNS and a certificate-managing reverse proxy. Tor needs separate public and authorized administrator onion identities and a reviewed Tor image.
- Create an offline age identity for backups. Keep its private half apart from the hosts and backups. Give backup jobs only the public recipient.

Verify the Docker Engine and Compose plugin separately on each host that runs
Compose commands, including the payment host when enabled:

```sh
docker version
docker compose version
```

Both commands must succeed; Docker Engine alone does not include a working
Compose plugin. Install Compose v2 from your chosen trusted, signed package
source and confirm version 2.17 or later. Distribution versions may include a
package revision after the upstream version; review that suffix rather than
requiring the output to equal an upstream tag verbatim.

This is a production deployment profile for prerelease software. It does not replace the [production checklist](production-checklist.md) or verification of your actual network boundaries.

## 1. Plan hosts and private routes

Create the roles below in the same private network where possible. A managed PostgreSQL service supplies the database role; do not install PostgreSQL on the application host.

| Role | Required services | Public ingress |
| --- | --- | --- |
| Application | Application container and its private upload backend | None; gateway private addresses only |
| Upload gateway | `opsecmkt upload-gateway`, ClamAV, HTTPS proxy or public Tor | HTTPS or the public onion service |
| Managed database | PostgreSQL, private endpoint, role authentication, TLS | None; application and approved backup/recovery hosts only |
| Maintenance worker | Signed dependency reports and restricted report transfer | None |
| Payments, when enabled | Monero daemon, dedicated escrow wallet RPC, authenticated HTTPS RPC front end | Deliberately configured Monero peer ports only |

The commands use these example names and paths. Replace them with your own private addresses and certificate names before running them:

| Example | Meaning |
| --- | --- |
| `app.internal`, `10.20.0.10` | Application host private DNS name and address |
| `db.internal`, port `5432` | Database endpoint and its actual service port |
| `market.example.com` | Public HTTPS domain; use your public `.onion` URL for Tor |
| `/srv/opsecmkt/market-prod` | New private application installation directory |
| `/srv/opsecmkt-inputs` | Private imported URLs, public certificates and leaf keys |
| `/srv/opsecmkt-retention` | Persistent external retention state, retained through rebuilds |

Allow only the gateway's approved private source address to reach application ports `8080` and `8443`. Allow only the application and approved recovery sources to reach the database's actual private service port. Keep ClamAV on gateway-host loopback or a scanner-only container network. Enforce default-deny inbound at both host and provider boundaries, including IPv6 and Docker forwarding. Broad private-network ranges and mutable role tags are not equivalent to an exact approved-IP policy. The full [service flow matrix](service-topology.md#allowing-only-required-service-flows) covers optional workers and payments.

For a Tor-only storefront, bind public Tor's target to gateway loopback and expose no marketplace HTTP/HTTPS listener on any public IP. Source-IP allowlists protect SSH and private service connections. They do not identify a visitor behind Tor; the separate administrator onion uses client authorization and application roles.

Confirm that `app.internal` resolves to the private application address from the gateway. Certificate names must match the names in the URLs; replacing a DNS name with an IP address also changes the required certificate identity.

### Verify encrypted hosts before transferring private files

Run production deployment commands **on the Linux application host**, against its
local Docker Engine. A remote Docker context cannot prove that host's storage
from a workstation. Inspect the actual Docker data root and containerd storage;
encrypt both, along with the operating-system root, installation, input files and
external retention ledger. Disable active swap before setup. A plaintext bind
mount beneath an encrypted directory still fails the storage requirement. Keep
the operator installation on persistent encrypted storage; the new production
start commands do not accept a tmpfs/RAM-unlocked operator directory.

Prepare and inspect the destination paths before continuing:

```sh
install -d -m 700 /srv/opsecmkt /srv/opsecmkt-inputs /srv/opsecmkt-retention
./opsecmkt deploy storage verify --path / \
  --path /srv/opsecmkt --path /srv/opsecmkt-inputs \
  --path /srv/opsecmkt-retention
```

Expect every path to be verified with its LUKS identity. Check the runtime's
actual paths through the [complete storage procedure](server-hardening.md#6-verify-encryption-before-restoring-state).
`deploy init --mode prod` repeats its own host and input-path checks before
staging configuration or generating keys, then saves expected storage identities.
`install`, `up`, `redeploy` and `doctor` recheck them and refuse active swap.
Production starts install per-project Docker and containerd pre-start checks and
reload systemd configuration before starting the application. They do not restart
those daemons. The checks do not format a disk, configure boot unlocking or
attest another host. Repeat storage and boot acceptance on the gateway, payment
and worker hosts independently.

## 2. Create and verify managed PostgreSQL

1. Create a PostgreSQL instance with an appropriate availability plan and a UTF8 database. PostgreSQL 17 is covered by the repository's database tests. Size storage for database rows, normalized image bytes, indexes, WAL, growth and recovery workspace.
2. Create a dedicated application database and role. The role must own the application schema and apply startup migrations. Keep provider administration credentials separate.
3. Select the private **direct database endpoint**. The application uses transactions and connection state; do not select a transaction pooler without reviewing [connection pooler requirements](../operator-guide.md#connection-poolers).
4. Restrict database access to the application host and approved backup/recovery hosts. Download the service's public TLS CA certificate through its authenticated control plane.
5. Write the connection URL into a regular mode-`0600` file through a private editor or secret manager. Do not put its credential in a command argument, shell history, report or Git commit.

On the application host, create the inputs directory before transferring files:

```sh
install -d -m 700 /srv/opsecmkt-inputs
```

Save the URL as `/srv/opsecmkt-inputs/database.url` and the public CA as `/srv/opsecmkt-inputs/database-ca.pem`. The URL format is:

```text
postgres://USER:URL_ENCODED_PASSWORD@db.internal:5432/MARKET?sslmode=verify-full&sslrootcert=/srv/opsecmkt-inputs/database-ca.pem
```

Use the provider's actual port, database name and certificate hostname. Percent-encode special characters in the username or password. `verify-full` verifies both the certificate chain and hostname. A private endpoint alone does not replace TLS.

From the source checkout on a host allowed by the database firewall, run a read-only check. Give that host its own protected URL file with a host-readable CA path:

```sh
export BACKUP_DATABASE_URL="$(cat /srv/opsecmkt-inputs/database.url)"
python3 scripts/postgres-tool.py BACKUP_DATABASE_URL psql \
  -X -v ON_ERROR_STOP=1 \
  -c 'SELECT current_database(), current_user;' \
  -c 'SHOW server_encoding;' \
  -c 'SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid();'
unset BACKUP_DATABASE_URL
```

Expect the intended database and role, `UTF8`, and `ssl = t`. If the service refuses the diagnostic view, confirm connection encryption with its documented equivalent before continuing.

Keep the original host-side URL for backup tools. During application initialization, `--database-ca` copies the CA and rewrites the imported URL's `sslrootcert` to its container path. That rewritten URL is for the application container.

## 3. Prepare application and gateway identities

Issue service certificates on the trusted issuing workstation. Keep the CA private keys there. These commands use the native key tool; an existing certificate authority can issue equivalent identities.

```sh
umask 077
install -d -m 700 ./service-tls
./opsecmkt keys generate tls-ca --name 'Application transport CA' \
  --cert-out ./service-tls/app-ca.pem --key-out ./service-tls/app-ca.key
./opsecmkt keys generate tls-server --name 'Application HTTPS' \
  --host app.internal \
  --ca-cert ./service-tls/app-ca.pem --ca-key ./service-tls/app-ca.key \
  --cert-out ./service-tls/app.pem --key-out ./service-tls/app.key
./opsecmkt keys generate tls-ca --name 'Upload transport CA' \
  --cert-out ./service-tls/upload-ca.pem --key-out ./service-tls/upload-ca.key
./opsecmkt keys generate tls-server --name 'Private upload backend' \
  --host app.internal \
  --ca-cert ./service-tls/upload-ca.pem --ca-key ./service-tls/upload-ca.key \
  --cert-out ./service-tls/backend.pem --key-out ./service-tls/backend.key
./opsecmkt keys generate tls-client --name 'Upload gateway' \
  --ca-cert ./service-tls/upload-ca.pem --ca-key ./service-tls/upload-ca.key \
  --cert-out ./service-tls/gateway-client.pem \
  --key-out ./service-tls/gateway-client.key
```

Transfer files over an authenticated encrypted connection:

| Destination | Files |
| --- | --- |
| Application inputs directory | `app.pem`, `app.key`, `upload-ca.pem`, `backend.pem`, `backend.key` |
| Gateway private configuration directory | `app-ca.pem`, `upload-ca.pem`, `gateway-client.pem`, `gateway-client.key` |
| Issuing workstation only | Both CA private keys and their protected recovery copies |

Leaf private keys must be mode `0600`. Restrict access to their service account. The gateway receives no database credentials, application secret, audit signing secret or wallet secret.

For encrypted audit exports or payments, also [generate a recovery OpenPGP pair offline](binary.md#importing-and-generating-keys). Copy only its public half to the required host. Record renewal dates for every certificate; generated leaf certificates default to one year.

## 4. Initialize and start the application

The encryption checks in step 1 must pass before this step. Check payment-host wallet/RPC state separately. An encrypted backup or RAM-unlocked configuration does not encrypt persistent volumes. Follow [storage verification](server-hardening.md#6-verify-encryption-before-restoring-state); install the reviewed boot checks before enabling automatic service startup.

Follow [build, publish and get the application image digest](binary.md#building-and-publishing-the-application-image).
That procedure packages the reviewed source, publishes it to your chosen registry
and sets `APP_IMAGE` to the full inspected repository/digest reference on the
application host. A tag alone is refused; Docker supplies the digest, so you do
not replace a SHA placeholder manually.

The native controller manages only this host. It creates private configuration and keys; it does not create the managed database, gateway, scanner or remote payment services.

Choose the payment mode **before** running `init`. For a market that must take payments, first prepare the [native production payment prerequisites](enable-payments.md#native-production-with-separate-payment-services), then append that section's escrow flag block to the same new-install command below. The example command alone leaves payments off. Native deployment has no command that automatically adds remote escrow to an existing installation; do not try to enable it later by rerunning `init`.

Choose first-start browsing and registration too. The defaults are public browsing
and open registration. Add `--site-access members --registrations closed` to the
same command if only existing members should enter. Later administrator settings
are saved in PostgreSQL and take precedence over these initial flags.

### HTTPS storefront

With `APP_IMAGE` set to the verified reference in the application host's shell, run:

```sh
install -d -m 700 /srv/opsecmkt
./opsecmkt deploy init --dir /srv/opsecmkt/market-prod --mode prod \
  --app-image "$APP_IMAGE" \
  --app-host app.internal --gateway-url https://market.example.com \
  --app-bind 10.20.0.10:8080 --backend-bind 10.20.0.10:8443 \
  --database-url-file /srv/opsecmkt-inputs/database.url \
  --database-ca /srv/opsecmkt-inputs/database-ca.pem \
  --tls-cert /srv/opsecmkt-inputs/app.pem \
  --tls-key /srv/opsecmkt-inputs/app.key \
  --upload-ca /srv/opsecmkt-inputs/upload-ca.pem \
  --upload-cert /srv/opsecmkt-inputs/backend.pem \
  --upload-key /srv/opsecmkt-inputs/backend.key \
  --retention-dir /srv/opsecmkt-retention \
  --set RETENTION_ENABLED=on
./opsecmkt deploy plan --dir /srv/opsecmkt/market-prod
./opsecmkt deploy doctor --dir /srv/opsecmkt/market-prod
./opsecmkt deploy install --dir /srv/opsecmkt/market-prod
./opsecmkt deploy status --dir /srv/opsecmkt/market-prod
```

`init` exits without starting services and refuses an existing target directory.
It requires verified local encrypted storage in production. Review the plan and
the [automatic storage boot checks](server-hardening.md#keep-services-stopped-until-storage-is-ready)
before `install`. These checks affect the host's Docker and containerd services;
review other workloads before using a shared host. The saved Docker context
defaults to `default`; a selected production context must still use this host's
local engine.

After the storage/core/firewall startup guards are installed, enable persistent
Docker and containerd startup on every role host that runs them:

```sh
systemctl enable docker.service containerd.service
systemctl is-enabled docker.service containerd.service
systemctl is-active docker.service containerd.service
```

Expect both `enabled` and `active` before the locked reboot rehearsal. `enable`
does not restart running services; Docker may also enable its packaged socket.
An active daemon alone does not prove next-boot startup. Follow the
[complete guarded-startup check](server-hardening.md#keep-services-stopped-until-storage-is-ready)
on each separate host before enabling it.

Expect the application to become healthy after it connects, applies migrations and enrolls the current retention state. This configuration creates no local database or Monero node and leaves `ESCROW_XMR=off`. It generates distinct application, setup and audit secrets. Retain them; recreating the directory with new keys is not a recovery method.

### Tor storefront

Prepare the public gateway onion and authorized administrator clients **before** application initialization. Follow [native production with separate onions](tor.md#native-production-with-separate-onions) in order. It includes public identity generation/import, an optional vanity prefix, client authorization and the gateway's Tor configuration.

Use the same database, private TLS and upload flags above, replacing the HTTPS gateway URL and adding:

```text
--exposure tor
--gateway-url http://YOUR_PUBLIC_ONION.onion
--admin-tor-image YOUR_TOR_IMAGE_WITH_SHA256_DIGEST
--admin-client-auth-dir /srv/opsecmkt-inputs/admin-clients
```

For optional logo uploads from the private administrator onion, also set
`--branding-upload-origin http://YOUR_PUBLIC_ONION.onion` to the public gateway
origin in that same command. The confirmation remains private; only a limited
upload authorization and the file pass through the gateway. See
[market branding](../operator-guide.md#changing-the-market-name-and-logo).

These are additional arguments to the single `deploy init` command. Do not run a second initialization. The public Tor service runs on the gateway; the separate administrator Tor service runs on the application host and uses its private Unix socket. Production Tor refuses startup without the authorized administrator ingress. Use separate public and administrator identities and keep their private keys and client authorization keys in encrypted recovery storage.

Independent Tor probes verify only the clients used in that test. Separately
launch a signature-verified desktop Tor Browser, import the bound administrator
client token, and complete owner sign-in/MFA. A browser signature failure must be
resolved through a verified installation; it is not a reason to disable signature
checks, onion authorization or the private administrator boundary.

### Optional application-host reboot control

This optional control requires the Tor production topology with its
client-authorized private administrator onion. After encrypted-host setup, a
root operator can configure the fixed application-host OS reboot control:

```sh
sudo /usr/local/bin/opsecmkt deploy host-reboot configure \
  --dir /srv/opsecmkt/market-prod
sudo /usr/local/bin/opsecmkt deploy up \
  --dir /srv/opsecmkt/market-prod
```

The new host policy starts disabled. Complete a real OS reboot, off-host manual
unlock and service-recovery rehearsal before the root operator runs
`deploy host-reboot ready --dir DIR --confirm-unlock-rehearsed`. This acknowledgement
applies to the current OS boot; repeat it after each later host reboot and
manual-unlock verification. Enroll the
administrator's authenticator, then follow the
[complete activation and use procedure](../operator-guide.md#enabling-the-private-application-host-reboot-control).
The web control requires the private administrator connection, current password,
TOTP and explicit downtime/unlock acknowledgement. It records the request before
dispatch and provides a receipt, grace period and cooldown. Manual unlocking
remains outside the web application. Keep operator acknowledgement, owner browser
acceptance and actual recovery evidence separate. The
[deployment checkpoint](deployment-notes.md#evidence-and-remaining-gates) records
what has passed on the current hosts; this guide does not mark those gates complete.

## 5. Run the scanner and supervised gateway

Prepare the scanner and gateway now, but keep public Tor/HTTPS ingress fenced
until administrator setup and paused intake are verified in step 6. Tor's separate
private administrator onion remains the setup path. For HTTPS setup, temporarily
permit only the approved operator source until the launch checks pass.

On the separate gateway host, run ClamAV with current signatures. A signature-free image cannot serve uploads. The repository supplies `deploy/clamd.conf` and a tested container contract in `compose.scanner.yaml`; use a reviewed digest-pinned image with that contract.

For a standalone scanner container, copy `deploy/clamd.conf` to `/etc/opsecmkt-scanner/clamd.conf`, replace the image placeholder, then run:

```sh
docker run -d --name opsecmkt-clamd --restart unless-stopped \
  --user clamav --entrypoint clamd \
  --read-only --tmpfs /tmp:size=64m \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --memory 3g --cpus 2 --pids-limit 256 \
  --log-driver none \
  --publish 127.0.0.1:3310:3310 \
  --mount type=bind,src=/etc/opsecmkt-scanner/clamd.conf,dst=/etc/clamav/clamd.conf,readonly \
  YOUR_SCANNER_IMAGE_WITH_SHA256_DIGEST \
  --foreground --config-file=/etc/clamav/clamd.conf
```

Loading signatures can take several minutes. Allow memory for ClamAV, the gateway and the OS; see [starting hardware budgets](service-topology.md#starting-hardware-budgets). Keep port `3310` inaccessible from other hosts and restrict scanner egress according to your host policy.

Create a dedicated gateway service account. On a systemd Linux host using `useradd`, run:

```sh
useradd --system --user-group --create-home \
  --home-dir /var/lib/opsecmkt-gateway \
  --shell /usr/sbin/nologin opsecmkt-gateway
install -d -m 700 -o opsecmkt-gateway -g opsecmkt-gateway \
  /etc/opsecmkt-gateway
install -m 755 ./opsecmkt /usr/local/bin/opsecmkt
```

Place the gateway's four files from step 3 in `/etc/opsecmkt-gateway`. Make the gateway account their owner and set its private key to mode `0600`. Use your distribution's equivalent account command if needed.

On a systemd Linux host, save this as `/etc/systemd/system/opsecmkt-gateway.service`. Replace the account if your service account has another name:

```ini
[Unit]
Description=OPSEC Market upload gateway
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=opsecmkt-gateway
Group=opsecmkt-gateway
ExecStart=/usr/local/bin/opsecmkt upload-gateway --addr 127.0.0.1:8080 --app-url https://app.internal:8080 --app-ca /etc/opsecmkt-gateway/app-ca.pem --backend-url https://app.internal:8443 --ca /etc/opsecmkt-gateway/upload-ca.pem --cert /etc/opsecmkt-gateway/gateway-client.pem --key /etc/opsecmkt-gateway/gateway-client.key --clamd-address 127.0.0.1:3310
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
UMask=0077
StandardOutput=null
StandardError=null

[Install]
WantedBy=multi-user.target
```

Install the gateway's own reviewed encrypted-storage startup dependency and
[core-limit controls](server-hardening.md#verify-service-and-container-core-limits)
before enabling automatic startup. The application host's daemon guards do not
cover a separate gateway. Check the actual loopback listener and a ClamAV PING;
a requested Docker port mapping alone does not prove the port was published.
When a separate updater deliberately has no health check, wait for the scanner's
real health/PING and the exact required running services instead of requiring an
updater health state that does not exist.

```sh
systemctl daemon-reload
systemctl enable --now opsecmkt-gateway
systemctl is-active opsecmkt-gateway
```

Expect `active`. On another service manager, preserve the same account, arguments, permissions and restart behavior. Confirm recovery after a controlled restart of the gateway and application hosts.

`--app-ca` authenticates ordinary application HTTPS. `--ca`, `--cert` and `--key` authenticate the mutual-TLS upload backend. These are separate connections; omitting the application CA commonly causes proxy certificate failures. Use normal hostname verification throughout.

### Attach public ingress

Route the **entire site** through `127.0.0.1:8080` on the gateway host. The gateway proxies ordinary requests and intercepts raw image uploads. Do not route the public site directly to the application.

For HTTPS, a Caddy site block is:

```caddyfile
market.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The reverse proxy must preserve the original `Host` header. Caddy does so by default. With nginx, set `proxy_set_header Host $host;`, or `$http_host` when the public URL includes a non-default port. Otherwise forms fail origin checks. Keep proxy access logs off or apply the reviewed logging policy, because URLs, addresses and request metadata can be sensitive.

For Tor, point the gateway's public `HiddenServicePort` at this loopback gateway listener using the [public gateway ingress procedure](tor.md#prepare-the-public-gateway-identity). Keep the app connection private HTTPS even when browser ingress uses an HTTP onion URL.

## 6. Complete first-run setup and verify journeys

For HTTPS, open `https://YOUR_DOMAIN/setup`. For production Tor, open the **authorized administrator onion** `/setup` in the configured Tor Browser. Public production onion routes must not expose setup or administrator pages.

Read the setup token locally from `/srv/opsecmkt/market-prod/secrets/setup-token`; do not paste it into reports or chat. Create the marketplace name and first administrator, then enroll an authenticator and store recovery codes offline. See [first steps](first-steps.md) for field requirements.

Alternatively, after application migrations complete, initialize from the application host without exposing browser setup:

```sh
DATABASE_URL_FILE=/srv/opsecmkt-inputs/database.url \
  ./opsecmkt -bootstrap-admin YOUR_ADMIN_HANDLE -site-name 'Your market'
```

The command prompts for a password and refuses an already-installed market. It uses the original host-readable database URL and CA path.

Before opening public ingress, sign in through the intended administrator path.
For a payment-enabled instance, under **Admin → New escrow checkouts**, select
**Paused**, confirm with the current password and enrolled authenticator, then
select **Save XMR intake**. Verify the saved `payment_intake_XMR=false` value
through an approved read-only database connection and retain the audit/readback
evidence. Leave it paused until payment and recovery gates pass; a daemon that
is still syncing is not ready. If payments were not configured, keep escrow off.

Choose [site access and registration](first-steps.md#choose-browsing-registration-and-branding)
and, optionally, the market name/logo. Verify a signed-out session and an
existing account after access changes. Verify the saved name/logo after reload
through the real gateway; an isolated private-admin logo upload needs the
configured public upload origin and a functioning scanner.

Verify these outcomes before inviting users:

1. `deploy status` reports a healthy application. A request through the actual public gateway to `/healthz` returns HTTP `200` with body `ok`.
2. Administrator sign-in and a normal account form succeed through the intended ingress. Secure HTTPS cookies require an HTTPS browser URL.
3. Create a vendor test account and listing, upload a JPEG or PNG through the public gateway, reload it, then remove the test content. A scanner outage or stale signatures must refuse uploads while ordinary pages continue working. The current private administrator onion does not support isolated image uploads.
4. A connection from an unauthorized source cannot reach application ports, database ports or ClamAV. The upload backend rejects clients without a valid gateway certificate.
5. For production Tor, an unauthorized browser cannot connect to the administrator onion, and public onions return `404` for `/setup`, `/admin` and private audit routes.

The production app disables persistent container logs. Use `deploy status`, health checks and administrator diagnostics; protect any temporary diagnostic capture and remove it under your retention policy.

## 7. Establish backups and recovery

Keep intake paused while rehearsing recovery. A normal application restart uses
the retained native installation and does not need a disk-unlock key when root is
already mounted. A full OS reboot closes the encrypted root and needs the separate
early-boot identity, off-host unlock material and a new normal-OS acceptance check.
Never rerun `init`, format a device or change a key to repair an ordinary restart.

Enable managed database backups and point-in-time recovery if your service offers them. Record retention and restoration procedures. Managed-database backups do not include application keys, the external retention ledger or gateway identities. VM snapshots may include those sensitive files, but do not replace coordinated encrypted recovery backups or verification of consistency and retention.

**A provider point-in-time recovery or snapshot restore skips the payment restore gate.** Only `scripts/restore.sh` sets it automatically. A database restored any other way comes back with no recovery gate and no settlement holds: free escrow accounts the live wallet already handed out can be offered again, and the escrow watcher relays restored settlements as soon as the application starts. Before pointing the application at such a database, keep it stopped with the native controller (`./opsecmkt deploy down --dir /srv/opsecmkt/market-prod`; an `opsecmkt serve` process supervised directly is stopped through its service manager), not with Compose commands. Apply the protection SQL in [UPGRADING.md section 6](../../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database with `psql` over the protected database connection, then run `deploy up` with public gateway ingress still closed and follow the [reconciliation procedure](../testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) before clearing the gate.

Confirm that the current original instance has enrolled retention state before relying on rollback backups. Step 4 enables it for a new instance. For an existing never-enrolled market, follow [retention enrollment](../retention.md#enabling-the-service) on its current original database first. An old restored database cannot initialize an empty ledger, even if its backup predates enrollment.

From the matching source checkout on the approved backup host, use a private URL file containing that host's CA path:

```sh
export BACKUP_DATABASE_URL="$(cat /secure/opsecmkt/database.url)"
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT \
  ./scripts/backup.sh /secure/backups/market.dump.age
unset BACKUP_DATABASE_URL
```

Schedule uniquely named backups to meet your recovery-point target. Copy each encrypted `.dump.age` archive and its `.retention.json` sidecar together to approved off-host storage. The script checks space, client versions and retention replay state before dumping.

Also encrypt and retain the private application installation, current external retention directory, gateway configuration and leaf identities. Keep offline recovery keys separately. Include onion identities and payment wallet state when used. Preserve ownership and permissions when recovering files.

Inventory systemd base units, overrides and their authoritative source copies,
storage/boot guards and update policy separately. A role-directory archive does
not automatically cover `/etc`. Reinstall reviewed matching overrides before
daemon reload or service startup, then inspect the effective commands. A logical
database restore or authenticated header archive does not prove a full service
or wallet recovery.

Before launch, create a new empty managed database for a recovery rehearsal. Keep it isolated from users and payment services. On the recovery host:

```sh
export RESTORE_DATABASE_URL="$(cat /secure/opsecmkt/rehearsal-database.url)"
AGE_IDENTITY=/secure/offline/backup-key.txt \
  ./scripts/restore.sh /secure/backups/market.dump.age
unset RESTORE_DATABASE_URL
```

The restore asks for the explicit `RESTORE` confirmation and refuses schema conflicts. Start only the isolated recovery app with the preserved application keys, retention enabled and a private copy of the **current** external retention ledger. Keep it separate from the running original instance's ledger. Confirm that retention replay finishes, expected data is available and payment recovery gates remain closed. Never generate an empty ledger to reopen an old backup. Follow [restore replay](../retention.md#restoring-without-reviving-deleted-data) and [backups and recovery](../operator-guide.md#backups-and-recovery) for the complete process.

Inventory all copy locations, including provider snapshots and WAL archives. Schedule expiry under the [retention policy](../data-retention-privacy-policy.md), using the [copy expiry procedure](../retention.md#expiring-backups-and-signed-exports). Check backup completion and off-host copies regularly, and repeat restore rehearsals after material changes.

## 8. Add monitoring, payments and maintenance

### Monitor the operating instance

Run the [dependency version worker on its separate maintenance host](service-topology.md#version-worker-host). Import its independently pinned public verification key and deliver only signed reports to the app. The worker reports versions; it does not update software.

Monitor public health, application readiness, gateway restarts, database connections/storage, certificate expiry, backup age and scanner signature age. Review administrator diagnostics. A successful `/healthz` response does not prove uploads, recovery or payments work; keep the journey checks above in your runbook.

### Enable payments deliberately

Prepare the separate Monero daemon and dedicated escrow wallet RPC, authenticated HTTPS endpoints, stable wallet secret, public seed-encryption key and wallet backups. The [native payment procedure](enable-payments.md#native-production-with-separate-payment-services) supplies the escrow flags used during the single initialization in step 4. Native production does not provision remote Monero services.

If this instance was initialized without payments, it needs a deliberately reviewed configuration migration before remote escrow can be added; this guide does not provide an automatic in-place conversion. Preserve the installation and its keys. Native initialization defaults to **stagenet**, while the legacy shell installer defaults local nodes to **mainnet**. Select the intended network explicitly; a market that has pinned its escrow network cannot switch to another network.

For mainnet, read the custodial and real-funds limits before passing `--monero-network mainnet`. Mainnet payment and settlement workflows remain unverified; deployment and readiness checks do not establish real-funds settlement. Rehearse payment and recovery workflows on stagenet; do not use real funds as a deployment test.

A Tor storefront also needs deliberate payment-host Tor routing. Follow the [Monero relay transport procedure](../operator-guide.md#declaring-separate-payment-host-tor-egress) before adding `--set ESCROW_XMR_TOR_RELAY=remote-tor` to initialization. This setting records the operator's declaration; it does not verify the remote daemon's peer traffic. The storefront onion and successful HTTPS RPC requests cannot prove Tor peer routing.

A newly started daemon can still be synchronizing its blockchain. Keep intake paused until daemon synchronization, wallet synchronization, RPC authentication and the application's readiness checks all pass. Only then enable **New escrow checkouts** in Admin. Record payment readiness separately from application health and infrastructure creation.

### Rehearse maintenance tooling before stopping writers

On an isolated encrypted target, rehearse the exact create, inspect and start
sequence with synthetic state, reviewed images and the production Docker/Compose
versions. Confirm scripts handle both newly created and healthy containers before
stopping application or payment writers. Preserve failed observations and use a
separately reviewed recovery step instead of repeating a partially completed job.

- A newly created container can omit `State.Health`. Use a guarded optional
  lookup, such as the read-only command below; directly accessing a missing map
  entry can fail even inside a conditional.
- Automatic bridge IPAM can omit `IPRange` or return an empty string. Accept only
  those equivalent forms after checking all other expected fields. Keep fixed
  ranges exact and reject unexpected fields or nonempty automatic ranges.
- Use `docker ps --all --quiet --no-trunc` when comparing full container IDs.
  A shortened listing is not evidence of an identity change.

```sh
docker inspect --format '{{with index .State "Health"}}{{json .Status}}{{else}}null{{end}}' CONTAINER
```

### Upgrade the application

Review release migrations and gateway compatibility, take verified backups and keep the current retention ledger. Then replace only the application image:

```sh
./opsecmkt deploy plan --dir /srv/opsecmkt/market-prod
./opsecmkt deploy redeploy --dir /srv/opsecmkt/market-prod \
  --app-image YOUR_NEW_APP_IMAGE_WITH_SHA256_DIGEST
./opsecmkt deploy status --dir /srv/opsecmkt/market-prod
```

`redeploy` preserves the installation's endpoints, keys, project, context and worker configuration. It does not reinstall the managed database, scanner, gateway or Monero services. Schema migrations can prevent image-only rollback; use the [app redeployment and recovery procedure](service-topology.md#redeploying-only-the-application).

Refresh scanner signatures by reviewing and replacing the scanner image on the gateway host. The native controller has no `refresh-scanner` command. Renew leaf certificates before expiry and schedule database/OS maintenance independently.

## Troubleshooting

| Symptom | Check and correction |
| --- | --- |
| Database timeout or connection refused | Verify private endpoint resolution, actual service port, routes and both provider/host allowlists. Test from the application host. |
| Database TLS or hostname failure | Use the service's matching hostname, correct CA and `sslmode=verify-full`. Check the CA path in the relevant host or container. |
| Gateway returns `503`, or public proxy returns `502` | Check application health, private DNS/ports and `--app-ca`. Verify the application server certificate matches `--app-url`. |
| Forms return `403` for origin mismatch | Preserve the browser's original `Host` through public ingress and use the intended public URL. |
| Upload unavailable | Check ClamAV health/signature freshness, gateway/backend CA pairing, client certificate and private port `8443`. |
| Setup unavailable on a public onion | Use the separate administrator onion with its configured authorization key. This separation is intentional. |
| Backup cannot load the database CA | Use a backup URL whose `sslrootcert` names a file on the backup host. The application's mounted path is different. |
| App healthy but payment intake blocked | Inspect synchronization and escrow readiness. Infrastructure creation does not complete initial blockchain sync. |

Keep a private deployment record with dates, source revision, image digests, topology decisions, command outcomes, issues and their resolutions. Publish only a sanitized account without credentials, account identifiers, private addresses or operational keys. Finish the [production checklist](production-checklist.md) before inviting users.
