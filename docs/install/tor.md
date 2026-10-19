# Onion service (Tor)

Use separate public and client-authorized administrator onions for native production. The public onion belongs to the upload/ingress host; the administrator onion reaches only the app's private Unix listener. Review the [CLI reference](cli-reference.md), [service boundaries](service-topology.md), and [standalone setup](binary.md) first.

The checkout/Bash walkthrough later on this page describes the legacy single-host arrangement. Keep it for development or an explicitly reviewed existing deployment; it does not establish the production host separation required by the native profile.

## Native production with separate onions

Prepare the existing external PostgreSQL, Monero and upload/ClamAV services, their verified TLS identities, and their narrowly authorized private routes. The app host must receive only the public gateway's authenticated private connections; the database, RPC and upload-backend ports are never public. Use stagenet for development and rehearsal. Mainnet holds real funds and has no recorded mainnet deployment verification.

The two ingress paths are:

| Onion | Destination | Access |
| --- | --- | --- |
| Public storefront | Tor on the separate upload host → loopback upload gateway → verified HTTPS app listener | Ordinary customer/moderator routes and gateway-scanned uploads. The app refuses `/setup`, `/admin*`, administrator-only actions and administrator sessions through public ingress when the private socket is enabled. |
| Private administrator | Separate `tor-admin` service → `/run/market-admin/http.sock` | Requires Tor v3 client authorization before application login. Normal role, session, CSRF and step-up checks remain. |

Public encoded/escaped forms of protected paths must also be refused. Do not route the private socket or its backend through the public gateway, and do not trust Host/forwarding headers as administrator authentication. Verify denials through the actual public onion after deployment. Administrator photo/avatar uploads are unavailable through the current isolated private-admin path; no public upload route grants general private administrator ingress. Logo uploads use a separately confirmed, one-use handoff to the configured public gateway; see [market branding](../operator-guide.md#changing-the-market-name-and-logo).

### Choose and retain separate service identities

Use a different onion service identity for the public storefront and private administrator ingress. An onion service's Ed25519 identity determines its full address; an administrator's X25519 client authorization key is a separate credential. Generating an identity locally does not start Tor, publish a descriptor or make the service reachable. The [Tor address specification](https://spec.torproject.org/rend-spec/encoding-onion-addresses.html) describes the public key, checksum and version encoded in a v3 address.

Keep encrypted backups of each service's `hostname`, `hs_ed25519_public_key` and `hs_ed25519_secret_key`, clearly labelled by role. Keep recovery copies offline with their decryption material protected separately. The service still needs its private identity online while running. Anyone who obtains that key can impersonate the onion; see [Tor's setup and key guidance](https://community.torproject.org/onion-services/setup/) and its [encrypted backup guidance](https://onionservices.torproject.org/apps/web/onionspray/guides/backup/). Do not send either service's private key to visitors or administrator browsers.

An optional vanity prefix changes the search effort, not how users authenticate the service. Verify the entire address through an authenticated channel. Other people can generate the same short prefix; Tor explains this [impersonation risk and resource cost](https://community.torproject.org/onion-services/advanced/vanity-addresses/).

For one fixed prefix, assuming independent uniformly distributed Base32 characters, the expected search is `32^N` candidate identities:

| Prefix length | Expected candidates |
| --- | --- |
| 2 | 1,024 |
| 4 | 1,048,576 |
| 6 | 1,073,741,824 |

These are probability calculations, not measurements of this implementation. A six-character search can take substantial CPU time and may take more attempts than its expectation. Estimate time from the actual generator throughput on the intended machine; no completion time is guaranteed. Use ordinary random generation when a prefix is unnecessary. Long searches are not part of normal tests or installation verification.

### Generate or import service identity files

On the trusted issuing machine, create two independent identities in new directories beneath an existing protected parent:

```sh
./opsecmkt keys generate onion --out-dir ./identities/storefront
./opsecmkt keys generate onion --out-dir ./identities/administrator
```

Each directory is private `0700`, with the three native identity files at `0600`. Existing directories are refused. Import an existing identity with its expected complete address:

```text
opsecmkt keys import onion --address FULL_V3_ADDRESS.onion \
  --public-key EXISTING_DIR/hs_ed25519_public_key \
  --secret-key EXISTING_DIR/hs_ed25519_secret_key --out-dir NEW_IDENTITY_DIR
```

The importer checks that both keys derive that address and creates a new private directory. It does not replace live keys. For a prefix, use `keys generate onion --out-dir NEW_DIR --prefix PREFIX [--workers N]`; only lowercase `a-z2-7` and at most six characters are accepted. Worker count defaults to at most eight, bounded by available Go parallelism; `--workers` can select `1`–`64`. Searches support cancellation with Ctrl-C, have no guaranteed completion time, and start no Tor connection. The [complete key reference](cli-reference.md#onion-service-identities) describes the flags and output format.

### Prepare the public gateway identity

Production's public Tor service is on the upload/gateway host. App-host `deploy install` does not provision it. Reuse its existing identity and service configuration when rebuilding the application. For a new service, transfer only the storefront identity triple to that host over an authenticated encrypted channel. Prepare its Tor `HiddenServiceDir` owned by the actual nonroot Tor account, with private directory/key permissions. Keep administrator service and client keys off this host.

Combine this service fragment with the gateway host's reviewed Tor configuration and hardening:

```text
HiddenServiceDir /var/lib/tor/public-market/
HiddenServiceVersion 3
HiddenServicePort 80 127.0.0.1:8080
```

The directory holds the selected storefront identity; `127.0.0.1:8080` is the local `upload-gateway`, which forwards ordinary requests over verified HTTPS and uploads over the private mutual-TLS backend. Never point this public onion directly at the app or administrator Unix socket. Provision the Tor account/image, signatures/scanner, network restrictions and supervisor separately, as described in the [worker-host guide](service-topology.md#upload-host).

When moving an existing public service, stop its previous instance before activating the retained identity on the replacement. Verify the new host's `hostname` matches the authenticated full address. Preserve old identity evidence if any key file is missing or mismatched; generating a replacement would change the address. No native command automatically exports/adopts a running service's volume, transfers its identity, or provisions this remote host.

### Generate the administrator's client key

On the administrator's trusted client machine, create independent authorization outputs in existing private/public directories:

```sh
./opsecmkt keys generate tor-client \
  --public-out ./admin-public/operator.auth \
  --private-out ./admin-private/operator.key
```

Copy only `operator.auth` to the allowlist source directory used during installation, for example `/srv/opsecmkt-admin-clients`. Keep `operator.key` off the server. The native importer accepts 1–256 regular public `.auth` files with canonical `descriptor:x25519:` Base32 records; it refuses empty directories, extra files, symlinks, malformed keys and private client records. It copies a validated public snapshot into the private installation's `admin-clients` directory. Editing the original source later does not silently change that snapshot.

### Initialize the native Tor application host

For a new market, add these options to the complete [production initialization](binary.md#initializing-the-production-application-host):

```text
--exposure tor
--gateway-url http://YOUR_ACTUAL_PUBLIC_V3_ONION.onion
--admin-tor-image YOUR_REVIEWED_TOR_IMAGE_WITH_SHA256_DIGEST
--admin-client-auth-dir /srv/opsecmkt-admin-clients
--retention-dir /var/lib/opsecmkt-retention
```

Production Tor initialization requires the administrator image and public client directory together. The image must provide native Tor under the repository Tor image contract; the controller calls Tor directly, without the Bash admin entrypoint. The generated app sets `ADMIN_SOCKET=/run/market-admin/http.sock`; its managed host socket directory is the installation's `admin` directory with mode `0750`.

By default, `init` generates the administrator service identity locally and retains its three files in `onion-admin/`. To use the existing administrator identity generated/imported above, add these three flags together:

```text
--admin-onion-address FULL_ADMINISTRATOR_V3_ADDRESS.onion
--admin-onion-public-key ./identities/administrator/hs_ed25519_public_key
--admin-onion-secret-key ./identities/administrator/hs_ed25519_secret_key
```

Alternatively, `--admin-onion-prefix PREFIX` searches for a new identity; it cannot be combined with imports. Development's managed public Tor role has equivalent `--onion-*` flags and stores `onion-public/`. Those public-role flags cannot provision production's remote gateway. Keep the two identities different. `init` and `plan` print their public addresses and backup guidance without printing private key contents.

A networkless `admin-tor-prepare` init service, using the reviewed app image, validates the selected administrator identity and public client snapshot before preparing `tor_admin_identity` with the saved runtime UID/GID. Its only mounts are the selected private Tor identity, the public client snapshot and the dedicated volume; it receives no app, database, wallet or retention secrets. Fresh volumes receive that selected identity. Populated ones must contain the same complete identity; partial, invalid or mismatched keys refuse startup without replacement. This avoids depending on a Tor image's default numeric user or Tor accepting a root-owned authorization bind mount. See the [helper flags and filesystem contract](cli-reference.md#private-tor-volume-preparation-entry-point).

The `tor-admin` service waits for successful preparation and app health, then runs as that same nonroot UID/GID. It receives its own identity volume and read-only socket/configuration, joins a separate egress network and publishes no administrator TCP port. Its service keys remain private in that persistent volume. The public Tor service, upload gateway and ClamAV remain on the separate upload host; native app deployment does not provision or supervise those remote processes.

Run `deploy plan`, then `deploy install`, using the same private directory. Complete browser setup through the client-authorized administrator onion, or run the native `-bootstrap-admin` host command after schema initialization. The public storefront cannot perform setup. For a RAM-unlocked installation, keep retention on its separately configured persistent path; see the [RAM and emergency procedure](ram-secrets-and-emergency.md).

The selected address is known before Tor starts, so client authorization can be bound to it in advance. After startup, confirm the service's public hostname matches `init`/`plan` using an explicitly selected project/context. Replace every capitalized token with its saved value; this is a direct read-only Docker command, not an additional CLI subcommand:

```text
docker --context SAVED_CONTEXT compose \
  --project-name SAVED_PROJECT --project-directory ABS_INSTALL_DIR \
  --env-file /dev/null --file ABS_INSTALL_DIR/compose.json \
  exec -T tor-admin cat /var/lib/tor/hostname
```

The native path is `/var/lib/tor/hostname`; the later legacy overlay uses `/var/lib/tor/admin/hostname`. Do not interchange them.

The native `tor-admin` health check tests only that its hostname file is nonempty.
That file can exist before Tor connects. A matching hostname and healthy app do
not prove descriptor publication or onion reachability. Verify both routes
through fresh independent Tor clients and record when they passed; earlier
success does not establish current availability.

The generated native configuration also provides a private Unix control socket
under Tor's `0700` RAM data directory, `/tmp/tor`. Use a reviewed local collector
that verifies the socket peer and authenticates with SAFECOOKIE. Group access to
the socket and cookie is disabled. `ControlPort 0` keeps TCP control disabled. Keep cookie contents,
raw control replies, descriptors and circuit identifiers private. This diagnostic
channel does not establish remote onion reachability.

### Bind the client key and verify ingress

Back on the administrator's trusted machine, bind its saved private key to the actual administrator onion address, using new output paths:

```text
opsecmkt keys import tor-client --in ./admin-private/operator.key \
  --public-out ./admin-public/bound-operator.auth \
  --private-out ./admin-private/operator.auth_private \
  --onion YOUR_ACTUAL_ADMINISTRATOR_V3_ONION.onion
```

The command validates the v3 onion checksum. Keep the resulting `.auth_private`
file private and preserve it with the administrator's recovery material. For an
existing installation, recover its saved client credential rather than generating
a replacement that the service has not authorized.

To authorize **desktop Tor Browser** on the trusted administrator machine:

1. Open `http://YOUR_ACTUAL_ADMINISTRATOR_V3_ONION.onion/login`. Tor Browser
   requests a private key; select the gray key icon beside the address if needed
   to open the authentication prompt.
2. Read the saved `.auth_private` record locally. Its format is
   `ONION_ADDRESS:descriptor:x25519:PRIVATE_KEY`. Enter **only the final
   52-character Base32 private key** in the browser prompt. Do not enter the whole
   record, the public `.auth` file, an onion service identity, or an OpenPGP
   recovery key. Keep the key out of chat and logs.
3. After Tor connects, sign in with the application administrator account. For a
   new market whose setup has not completed, open `/setup` instead and follow
   [First steps](first-steps.md#native-production-deployment).

Tor's `0xF4` error means no authorization key was provided; `0xF5` means the key
was incorrect or revoked. See the
[Tor Browser authentication instructions and error codes](https://support.torproject.org/tor-browser/features/onion-services/#onion-service-authentication).
Tor Browser for Android currently lists client authorization as unsupported in
its [known limitations](https://support.torproject.org/tor-browser/encountering-issues/known-issues/).

For a standalone Tor client using persistent configuration, install the **whole
`.auth_private` file** in the private directory configured by `ClientOnionAuthDir`,
then restart that Tor client. This file-based method differs from entering only
the key in Tor Browser's prompt; see
[Tor's client authorization reference](https://community.torproject.org/onion-services/advanced/client-auth/).
A client without a valid authorization key must fail to connect. An authorized
client must still sign in as the correct application account. Storefront and
administrator onions have distinct browser cookie scopes.

Preserve the selected `onion-admin/` files and protect the matching `tor_admin_identity` volume. An app-only `deploy redeploy` leaves the running Tor service unchanged. On a replacement app host, restore/unlock the retained managed identity or migrate its matching volume, retain external retention state, and run `install`/`up` before app-only redeployment. The helper can initialize a fresh volume from the same selected identity; it never replaces a populated volume with different keys. Legacy configurations without managed files require preservation/export/adoption of their already-valid identity through an explicitly reviewed recovery procedure; an empty legacy volume refuses startup.

New encrypted RAM bundles include configured managed identity triples, not whole Tor volumes. Tor runtime keys remain in persistent Docker volumes even when the operator directory is unlocked into tmpfs. Emergency RAM cleanup and ordinary container removal do not remove those volume keys. Keep their storage/recovery protections separate from the controller's RAM placement. There is no automatic live-volume export, identity rotation, remote migration or volume-backup command.

### Diagnosing a timeout after entering the client key

A timeout after entering the 52-character key does not establish that the key is
wrong. Preserve the existing bound credential and full onion address.

1. Confirm the intended administrator address and saved client record's address
   binding locally. Keep the key out of chat and diagnostic logs.
2. Record the exact error. `0xF0` means the descriptor was not found; it does not
   establish a bad client key. `0xF4` means authorization is missing; `0xF5` means
   the supplied authorization failed. A circuit TTL error or timeout is
   inconclusive, not proof of access denial. See
   [Tor's extended error definitions](https://spec.torproject.org/socks-extensions.html#extended-error-codes).
3. Check the application backend separately through its intended private listener
   and service identity. A local login response does not prove remote onion access.
4. Collect bounded, sanitized server observations: clock, current process,
   resource pressure, bootstrap and consensus validity. A valid cached consensus
   need not be the newest one; account for the installed version's download
   schedule before inferring a fault. Open introduction circuits alone do not
   prove descriptor availability. Keep the generated `ControlPort 0`; do not
   expose a control port or export raw circuit logs, peer identifiers or key files.
5. Preserve evidence before remediation. Review any reload/restart for the
   installed Tor version and exact scope. Do not repeatedly reboot, rotate keys,
   relax ingress or replay an uncertain action to clear the error. After an
   approved action, perform one bounded fresh authorized/unauthorized check and
   retain failed or inconclusive results too.

Accept private ingress only when the authorized client reaches the intended
login page and an independent unauthorized client receives explicit authorization
refusal. Application password/MFA and owner-browser acceptance remain separate.

## Legacy single-host checkout walkthrough

The following instructions use `scripts/install.sh` and the checkout's `.env`/Compose overlays. They remain available for existing operations and development; they do not replace the native production split above.

### Before you begin

- [Requirements](requirements.md) met, with **Docker Compose 2.17 or later**.
- A node choice. The installer cannot add nodes later:

| Choice | Result |
| --- | --- |
| `--nodes local` | A pruned **mainnet** Monero node: real funds. Size storage from the [current node guidance](service-topology.md#starting-hardware-budgets); synchronization depends on the dataset and host. |
| `--nodes local --monero-network stagenet` | A pruned Monero stagenet node. Measure the current stagenet dataset and synchronization workload. |
| `--nodes custom` | Choose Monero network, image and pruning. |
| `--nodes none` | No nodes. No payments with a bundled node. |

> **Warning:** mainnet is the default and moves real funds. No mainnet run of this software is recorded; mainnet is
> covered by unit, database and simulated-RPC tests only. See [Mainnet](../operator-guide.md#mainnet).

### Step 1: Get the release

Use the latest tag from [Releases](https://github.com/yegamble/opsecmkt/releases), not `main`.

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
git checkout v0.1.0-alpha.2
```

### Step 2: Run the installer

```sh
./scripts/install.sh init --tor --nodes local
```

External database: add `--external-db URL` ([details](external-database.md)). All options:
[installer reference](installer-reference.md).

Output:

```text
Onion address: abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcd.onion
Its descriptor can take 1-3 more minutes to reach the Tor network.
Ready. Open http://abcdef….onion/setup in Tor Browser to name your marketplace and create its administrator.
Setup token: 3f1c…
```

Payments remain paused unless you enabled Monero escrow. Follow [Enabling Monero payments](enable-payments.md) for setup and readiness checks.

### Step 3: Open the setup page

Wait 1–3 minutes. Open the setup link in Tor Browser, then follow [First steps](first-steps.md).

### Step 4: Back up the onion keys

The address exists only as keys in the `tor_identity` volume. Lose them and the address is gone. Back them up:
[Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).

### Step 5: Verify

```sh
./scripts/install.sh status
```

Every service is healthy, setup is complete, and the onion address is listed.

### Optional

| Add | Command |
| --- | --- |
| Second onion address ([mirror](../operator-guide.md#optional-onion-mirror)) | `./scripts/install.sh add-mirror` |
| Photo uploads ([scanner](../operator-guide.md#photo-uploads-and-the-malware-scanner)) | `./scripts/install.sh add-scanner` |
| Nodes over Tor | [Local nodes over Tor](../operator-guide.md#local-nodes-over-tor) |

Onion DoS defences are on by default: [Onion-service defences](../operator-guide.md#onion-service-defences).

### Private administrator onion

For the standalone native deployment, use [Native production with separate onions](#native-production-with-separate-onions) above. This subsection describes the legacy checkout overlay and its different identity-volume path.

The optional `compose.admin-tor.yaml` adds a second onion that requires Tor v3
client authorization. It forwards to a Unix HTTP socket shared only by `app`
and `tor-admin`. The storefront and public mirrors continue to use `app:8080`;
that listener refuses `/setup`, `/admin*`, other administrator-only actions,
administrator sign-in and authenticated administrator requests on shared staff
routes. Moderator accounts retain their public staff routes. Headers and host
names cannot select the private listener. Sign-in through the private onion has
its own failed-password, authenticator-code and PGP-code budgets and admission
limits, so failures and floods on the public onion cannot pause or refuse it
(A-570; uploads still share their limits; see the
[operator guide](../operator-guide.md#authentication)).

This procedure requires the updated source containing the overlay; older alpha
tags do not include it. Finish ordinary first-run setup before enabling the
overlay, or use `-bootstrap-admin` from the host. With it enabled, sign in and
administer the market through the private onion. Accounts and role checks remain
the same; storefront cookies do not transfer to a different onion.

#### Prepare client authorization

Generate an X25519 key on your trusted operator workstation, using OpenSSL and
GNU `base32`. Keep the private files there, outside the checkout, in encrypted
storage. Run the commands in bash with shell tracing disabled:

```sh
set -eu -o pipefail
set +x
umask 077
auth_dir=$(mktemp -d)
openssl genpkey -algorithm x25519 -out "$auth_dir/operator.pem"
openssl pkey -in "$auth_dir/operator.pem" -outform DER |
  tail -c 32 | base32 | tr -d '=\n' > "$auth_dir/operator-private.b32"
printf 'descriptor:x25519:' > "$auth_dir/operator.auth"
openssl pkey -in "$auth_dir/operator.pem" -pubout -outform DER |
  tail -c 32 | base32 | tr -d '=\n' >> "$auth_dir/operator.auth"
printf '\n' >> "$auth_dir/operator.auth"
```

Move the directory to your encrypted operator storage and record its location.
Copy **only** `operator.auth` to an absolute directory on the server, for example
`/srv/opsecmkt-admin-clients`. This directory must contain at least one valid
`.auth` file and no private key. Its public files must be readable by the
container's `debian-tor` user; mode 0755 on the directory and 0644 on these public
files permits that without depending on a host UID match. Parent directories
must also be traversable. Keep private files restricted to the workstation.

Every `.auth` file has a nonempty basename (for example `operator.auth`, never
just `.auth`) and contains exactly one line:

```text
descriptor:x25519:YOUR_52_CHARACTER_BASE32_PUBLIC_KEY
```

The startup wrapper refuses an empty allowlist, symlinks, files other than
valid public `.auth` records, and the filename `.auth`. It captures and validates
each record before copying it from the read-only `/run/tor-admin-clients` input
mount into a Tor-owned `authorized_clients` snapshot (directory 0700, files
0600). Tor reads that snapshot, avoiding a race with later host-file edits and
ownership conflicts on a read-only bind. Input edits take effect on restart.
Tor alone would ignore malformed files and could otherwise publish a public service; see
[Tor's client authorization reference](https://community.torproject.org/onion-services/advanced/client-auth/).

#### Enable the private listener

In the existing `.env`, set
`TOR_ADMIN_AUTH_DIR='/srv/opsecmkt-admin-clients'` and append
`:compose.admin-tor.yaml` to `COMPOSE_FILE`. Preserve the existing project name,
other overlays and profiles. Leave `ADMIN_SOCKET` blank: the overlay sets it to
`/run/market-admin/http.sock` inside the app container. The overlay must follow
`compose.yaml`, and the installation must already use `APP_MODE=tor` and
`compose.tor.yaml`.

```sh
./scripts/install.sh check
docker compose up -d --build --wait app tor-admin
docker compose exec -T tor-admin cat /var/lib/tor/admin/hostname
```

The output is the private admin onion address. Keep it in your operator records.
The overlay publishes no host TCP port and creates its own `tor_admin_identity`
and `admin_socket` volumes. On an already running install, restart the public
Tor services as described under
[Tor service restart behavior](../operator-guide.md#clearnet-and-onion-deployment)
if an app recreation leaves an old backend connection; then verify every public
address still works.

On the operator workstation, create the client credential using that address
without `.onion` and the saved private base32 key:

```sh
umask 077
admin_onion=YOUR_56_CHARACTER_ADMIN_ADDRESS_WITHOUT_SUFFIX
printf '%s:descriptor:x25519:%s\n' "$admin_onion" \
  "$(cat "$auth_dir/operator-private.b32")" > "$auth_dir/operator.auth_private"
```

If you moved the key directory, set `auth_dir` to its new location first. Use
this credential in the operator Tor client's `ClientOnionAuthDir`, or enter its
private key in Tor Browser's onion authentication prompt. Restart the Tor
client when using a configuration file. The private credential must never be
copied to the server, placed in `.env`, logged or published.

#### Verify access and maintain credentials

Use the
[administrator ingress checks](../security-operations.md#restrict-administrator-ingress)
with separate authorized and unauthorized Tor clients. A successful Compose
configuration check does not prove Tor network denial. If the socket cannot be
opened, the app refuses to serve; if the client allowlist cannot be loaded,
`tor-admin` refuses to start. Do not work around either error by forwarding the
admin onion to the public TCP listener.

Back up the new admin onion identity encrypted, separately from client private
credentials. The existing storefront backup copies `marketplace`; for the new
volume, use the same offline tar-to-age procedure but select
`<project>_tor_admin_identity`, image `<project>-tor-admin` and directory `admin`.
Keep its client public allowlist with the deployment configuration. A database
backup contains neither Tor identity.

To revoke one client, remove its public `.auth` file from the server allowlist
and restart `tor-admin`; leave at least one valid client. Test the revoked
client and the remaining operator before relying on the change. Revocation does
not recover a compromised application account or host. Removing the private
overlay reopens public administrator access and requires an explicit deployment
decision ([upgrade notes](../../UPGRADING.md#unreleased-security-hardening)).

### Use your own (vanity) onion address

For current native identity generation/import, use [the native key procedure](#generate-or-import-service-identity-files) above. This subsection describes the legacy Bash installer's separate flags.

By default Tor creates a new, random address. To use one you already have, such as a vanity address, give its key
directory at install time:

```sh
./scripts/install.sh init --tor --nodes local --onion-key-dir /root/onion-keys --onion-address ADDR.onion
```

`/root/onion-keys` holds `hs_ed25519_secret_key` and, optionally, `hs_ed25519_public_key`, as `mkp224o` writes them.
Keep it outside the checkout.

- **Generate it yourself, offline.** A key made by anyone else, including a vanity service, is held by them: they can
  run your address. Use [mkp224o](https://github.com/cathugger/mkp224o) on a machine you control.
- **Checked before use.** The installer checks both files byte for byte, has Tor derive the address offline and
  compares it with `--onion-address` (or asks you to confirm it). A mismatch writes nothing.
- **First install only.** The key goes into a new, empty `tor_identity` volume before Tor first starts. The
  installer never replaces the address of a running install.
- **Afterwards:** back up the identity encrypted as in Step 4, retain protected offline recovery copies, and review removal of extra online copies. Unlinking files does not guarantee secure erasure from disks, snapshots or backups.

For the mirror: `./scripts/install.sh add-mirror --mirror-onion-key-dir DIR --mirror-onion-address ADDR.onion`,
before the mirror first runs.

### Good to know

- Only the Tor container has internet access. With an external database, the app does too.
- Local nodes sync over a direct connection unless routed through Tor.
- Tor protects inbound connections. It does not guarantee anonymity.
- Onion stopped answering? Recreate Tor. The address stays:

```sh
docker compose up -d --force-recreate tor
```

### What now?

- [First steps](first-steps.md)
- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
