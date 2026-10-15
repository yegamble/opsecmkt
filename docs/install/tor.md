# Onion service (Tor)

Run the market as a Tor onion service. No inbound port is opened on the host. Recommended for production.

**Time:** about 10 minutes, plus node sync. **Result:** a market at a `.onion` address with an administrator.

## Before you begin

- [Requirements](requirements.md) met, with **Docker Compose 2.17 or later**.
- A node choice. The installer cannot add nodes later:

| Choice | Result |
| --- | --- |
| `--nodes local` | A pruned **mainnet** Monero node: real funds. At least 45 GB of disk, days to sync (estimates). |
| `--nodes local --monero-network stagenet` | A pruned Monero stagenet node. About 20 GB, hours to sync. |
| `--nodes custom` | Choose Monero network, image and pruning. |
| `--nodes none` | No nodes. No payments with a bundled node. |

> **Warning:** mainnet is the default and moves real funds. No mainnet run of this software is recorded; mainnet is
> covered by unit, database and simulated-RPC tests only. See [Mainnet](../operator-guide.md#mainnet).

## Step 1: Get the release

Use the latest tag from [Releases](https://github.com/yegamble/opsecmkt/releases), not `main`.

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
git checkout v0.1.0-alpha.2
```

## Step 2: Run the installer

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

## Step 3: Open the setup page

Wait 1–3 minutes. Open the setup link in Tor Browser, then follow [First steps](first-steps.md).

## Step 4: Back up the onion keys

The address exists only as keys in the `tor_identity` volume. Lose them and the address is gone. Back them up:
[Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).

## Step 5: Verify

```sh
./scripts/install.sh status
```

Every service is healthy, setup is complete, and the onion address is listed.

## Optional

| Add | Command |
| --- | --- |
| Second onion address ([mirror](../operator-guide.md#optional-onion-mirror)) | `./scripts/install.sh add-mirror` |
| Photo uploads ([scanner](../operator-guide.md#photo-uploads-and-the-malware-scanner)) | `./scripts/install.sh add-scanner` |
| Nodes over Tor | [Local nodes over Tor](../operator-guide.md#local-nodes-over-tor) |

Onion DoS defences are on by default: [Onion-service defences](../operator-guide.md#onion-service-defences).

## Private administrator onion

The optional `compose.admin-tor.yaml` adds a second onion that requires Tor v3
client authorization. It forwards to a Unix HTTP socket shared only by `app`
and `tor-admin`. The storefront and public mirrors continue to use `app:8080`;
that listener refuses `/setup`, `/admin*`, other administrator-only actions,
administrator sign-in and authenticated administrator requests on shared staff
routes. Moderator accounts retain their public staff routes. Headers and host
names cannot select the private listener.

This procedure requires the updated source containing the overlay; older alpha
tags do not include it. Finish ordinary first-run setup before enabling the
overlay, or use `-bootstrap-admin` from the host. With it enabled, sign in and
administer the market through the private onion. Accounts and role checks remain
the same; storefront cookies do not transfer to a different onion.

### Prepare client authorization

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

### Enable the private listener

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

### Verify access and maintain credentials

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

## Use your own (vanity) onion address

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
- **Afterwards:** back up the volume as in Step 4, then securely delete `/root/onion-keys`.

For the mirror: `./scripts/install.sh add-mirror --mirror-onion-key-dir DIR --mirror-onion-address ADDR.onion`,
before the mirror first runs.

## Good to know

- Only the Tor container has internet access. With an external database, the app does too.
- Local nodes sync over a direct connection unless routed through Tor.
- Tor protects inbound connections. It does not guarantee anonymity.
- Onion stopped answering? Recreate Tor. The address stays:

```sh
docker compose up -d --force-recreate tor
```

## What now?

- [First steps](first-steps.md)
- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
