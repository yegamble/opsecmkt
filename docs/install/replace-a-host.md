# Replace a dead or compromised host

Use this guide when a server dies, is deleted or may have been taken over. It tells you which case you are in, then
gives the steps for each host role: application, payments, gateway and database. It adds no new tool. Every command
here is one the other guides already document, and each step links to the guide that explains it in full.

> **Not yet rehearsed.** No replacement described here has been run on a real host. Read
> [what has been rehearsed](#4-what-has-been-rehearsed) before you rely on it, and rehearse on stagenet first.

The examples use these names. Replace them with your own.

| Example | Meaning |
| --- | --- |
| `10.20.0.10` | The old application host's private address |
| `10.20.0.30` | The replacement application host's private address |
| `10.20.0.20` | The gateway host's private address, as in [server hardening](server-hardening.md) |
| `app.internal` | The name the gateway uses for the application, as in [production](production.md#1-plan-hosts-and-private-routes) |
| `/srv/opsecmkt/market-prod` | The application installation directory |
| `/srv/opsecmkt-inputs` | Private input files on the application host |
| `/srv/opsecmkt-retention` | The external retention directory |

[Application configuration recovery](recovery.md) and the testnet runbook's
[replacement application host leg](../testnet-runbook.md#leg-replacement-application-host-a-698) use `10.20.0.20` for
the replacement application. Here that address is the gateway, as in the hardening guides, so the replacement is
`10.20.0.30`.

## 1. Decide which case you are in

| Case | What happened | What you do |
| --- | --- | --- |
| **Dead host, not compromised** | Hardware or provider failure, a deleted machine, a lost disk. Nobody else had the host. | Build a new host and restore the same keys and identities from your off-host backups. |
| **Compromised or suspected compromised** | Someone else may have had a shell, root, the disk or a memory image. Signs include unknown logins, changed files, an unexplained reboot or a provider notice. | Keep the evidence. Isolate the host; do not delete it. Treat every secret it held as exposed and replace each one that a command can replace. |
| **Mirror or standby** | You want a second address or a spare before anything fails. | Read what the `mirror` profile is. There is no hot standby. |

If you cannot tell whether a host was compromised, treat it as compromised.

### Dead host, not compromised

The host's secrets are still yours. Bring the same identities back on a new host: the same administrator onion, the same
public onion, the same keys and wallet secret. Changing them would cost you something for no gain: a new onion address
that users must learn, unreadable second factors, or wallets that no longer open.

Fence the old host first. Make sure it cannot start again, even if the provider brings it back. An onion address must
never run from two hosts, and two applications must never use the same database or wallets.

Then follow the steps for its role in [part 2](#2-replace-a-host-role-by-role).

### Compromised or suspected compromised

Treat every secret the host held as exposed. Do not restore its keys, onion identity or wallet secrets onto the new
host where you can avoid it. Today you can avoid it only in part: the table below says which secrets a command
replaces, and which have **no command** that replaces them in an existing market.

**First, contain the host and keep the evidence.**

1. Cut the host off from its peers. Remove its address from the database's trusted sources, from the allowlists on
   the other hosts and from the provider firewall. Keep only the access you need for the investigation. If it is the
   application host, the market goes offline. That is intended.
2. Keep the evidence. Take a provider snapshot or disk image, keep its logs, and record times in UTC in the private
   operator record. Keep the original with its authentic timestamps; work on copies. The snapshot holds secrets too, so
   protect it like the host.
3. Isolate the host rather than deleting it, and leave it isolated until the owner decides its fate. Deleting the host,
   wiping or reinstalling it, deleting a volume, or running the
   [manual emergency commands](ram-secrets-and-emergency.md#review-an-emergency-action-before-running-it) destroys
   evidence and can make recovery impossible. Each needs the owner's explicit approval of that concrete action and its
   data loss. This guide, a review or a backlog entry is not that approval.
4. If the application host is **not** the compromised one, pause new payments from a trusted administrator client:
   **Admin → New escrow checkouts → Paused** ([operator guide](../operator-guide.md#escrow-checkouts-and-readiness)).
5. Write down which secrets the host held, using the table below, and what it could read.

**Then replace what it held.** Build the new host with the steps in [part 2](#2-replace-a-host-role-by-role), using the
new material from this table. Where the table says there is no command, stop: the owner decides how to proceed.

| Host | Secret | How to replace it | What it costs or leaves |
| --- | --- | --- | --- |
| Application | `APP_SECRET_KEY` | No command replaces it in an existing installation. Only `deploy init` sets it, from `--app-secret-key-file` or generated. | A new key refuses every open form once. TOTP secrets, unrevealed recovery-code lists and the saved outgoing XMPP password are sealed under it and become unreadable. Users sign in with a recovery code and enrol again; an administrator with none left needs `-reset-admin-factors HANDLE` ([details](../../UPGRADING.md#replace-a-placeholder-setup_token)). |
| Application | Setup token | No native rotation command ([CLI reference](cli-reference.md#moving-from-bash-to-go)). A new installation gets a new one. | With `APP_SECRET_KEY` set it derives no key, and once setup is complete it opens nothing. |
| Application | Audit signing key | `keys generate audit --out FILE --public-out FILE` makes one on the trusted workstation. Only `deploy init --audit-signing-key-file` installs it; no command replaces it in an existing installation. | `/canary` shows a new public key: announce it. Exports signed earlier keep the old signature. |
| Application | Application HTTPS leaf, upload backend leaf and their CAs (TLS and mutual-TLS) | New CAs and leaves on the issuing workstation with `keys generate tls-ca`, `tls-server` and `tls-client`, installed with `deploy tls replace` as shown [below](#rotate-the-application-tls-leaves). The gateway gets the new `app-ca.pem`, `upload-ca.pem` and client pair. | Replace the CAs, not only the leaves. Nothing in the code checks certificate revocation, so a stolen leaf stays valid under its old CA until it expires. |
| Application | Administrator onion identity (`onion-admin/` and the `tor_admin_identity` volume) | Only `deploy init` sets it: generated, `--admin-onion-prefix`, or the three `--admin-onion-*` imports. There is no identity-rotation command ([CLI reference](cli-reference.md#all-deployment-initialization-flags)). | A new address needs a new installation. A retired onion is never reused, for this market or any other. |
| Application | Administrator client authorizations | The host holds only the public `.auth` records. The private keys stay on the administrators' machines; replace one only if that machine was exposed, with `keys generate tor-client` there. | The installed snapshot is copied at `deploy init`; no native command changes it. |
| Application | Database credentials | Change the application role's password at the provider, write a new URL file, then run `deploy endpoint set` as shown [below](#move-or-rotate-the-database-connection). | Update the backup host's URL file too. |
| Application | Wallet and daemon RPC credentials, Monero client identity | Change them on the payment host's HTTPS front end. On the application side there is no command: `deploy tls replace` does not cover the Monero URLs, client identity or `MONERO_RPC_CA`. | Changing them needs a new installation. Until then, the old host's network access is what you can remove. |
| Application | `MONERO_WALLET_SECRET` | Cannot be replaced while wallets exist. No tool re-keys existing wallets, and the database pins a verifier of the secret ([operator guide](../operator-guide.md#secrets-env-and-onion-identities)). Carry it over by restoring the configuration or with `--monero-wallet-secret-file`. | With this secret and a copy of the wallet files, anyone can spend every escrow's coins ([escrow](../escrow.md)). Keep the wallet files only on the payment host, and remove the old application host from its allowlist at once. |
| Application | Database records | Nothing to rotate. The running application could read every record. | Account, order, dispute and audit metadata are disclosed ([security operations](../security-operations.md#define-the-threat-before-choosing-a-control)). |
| Payments | Escrow and bond wallets (files, and the keys of open wallets in the wallet RPC's memory) | No command moves escrow funds to new wallets or re-keys them. Orders settle through their normal release or refund. The bond wallet is swept offline from its sealed seed ([vendor bond wallet](../operator-guide.md#vendor-bond-wallet)). | The market is custodial: whoever holds the wallets can move their coins. The owner decides. Rehearse anything on stagenet first. |
| Payments | Wallet seeds | Seeds are sealed in the database to `ESCROW_XMR_SEED_KEY`, whose private half stays offline, so no market host holds them in the clear. Only if that offline key was exposed does it need replacing, by the re-pin in [Monero escrow](../operator-guide.md#monero-escrow). | Seeds already sealed stay sealed to the old key; the market never re-seals them. Native installations set the seed key only at `deploy init`. |
| Payments | RPC credentials and the front end's TLS leaf | Replace them on the payment host. The application side has no command, as above. | The old leaf stays valid under `MONERO_RPC_CA` until it expires. |
| Gateway | Public storefront onion identity | Make a new identity with `keys generate onion` on the issuing machine and install it as a [new public service](tor.md#prepare-the-public-gateway-identity). Publish the new address in your signed verification document and in `OFFICIAL_ADDRESSES`, and in `BRANDING_UPLOAD_ORIGIN` if you set it. | The old address is never reused. The application's saved `gateway_url` still names the old onion, and no command changes it. |
| Gateway | Gateway client identity for the upload backend (mutual-TLS) | New upload CA, backend leaf and client pair; install the application side with `deploy tls replace --upload-cert --upload-key --upload-ca`. | The gateway proxies account requests too, so treat the session cookies and passwords sent through it while it was compromised as exposed ([service topology](service-topology.md#upload-host)). |
| Database | Application role and provider administrator credentials | Change both at the provider. Apply the new application URL with `deploy endpoint set`. | The records are disclosed, as above. |
| Every host | SSH host keys, LUKS volume key and passphrase | A new host has new ones. Give each host its own LUKS volume key ([hardening](server-hardening.md#prepare-encryption-before-running-setup)). | If an operator's SSH key was used from the host, revoke it everywhere ([maintain the boundary](server-hardening.md#7-maintain-the-boundary)). |

To use the replacement steps for a compromised **application** host, the owner chooses between two paths. Neither has
been run:

- **Restore the configuration export.** `deploy recovery restore` brings back every key the old host held. After it,
  replace the database credentials and TLS material with the commands below. The keys with no command stay exposed:
  the application key, audit key, administrator onion, Monero RPC credentials and wallet secret.
- **A new installation with `deploy init`,** generating new keys and importing only the wallet secret and the seed
  key's public half, against the existing database. No guide covers this for a market with data, and the
  [binary guide](binary.md#rebuilding-or-replacing-only-the-application) warns against using `init` to rebuild one.

When nothing in the market must survive, the documented path is a
[fresh empty reset](storage-security-assessment.md#performing-a-fresh-empty-reset), which the owner approves.
For the wider response, see [respond to compromise](../security-operations.md#respond-to-compromise-with-a-reviewed-recovery-plan)
and [rotate identities from a trusted workstation](ram-secrets-and-emergency.md#rotate-identities-from-a-trusted-workstation).

### Mirror or standby

In this repository a mirror is the Compose `mirror` profile: the `tor-mirror` service in `compose.tor.yaml`, added by
`./scripts/install.sh add-mirror` ([optional onion mirror](../operator-guide.md#optional-onion-mirror)). It gives the
**same instance** a second onion address on the same host. There is one application, one database and one set of
wallets behind both addresses.

- It adds reachability, not redundancy. When the host dies, both addresses go with it.
- It exists only in the legacy Compose installation. Native installations have no mirror provisioning
  ([CLI reference](cli-reference.md#moving-from-bash-to-go)).
- Back up its `tor_mirror_identity` volume like the main identity, so the mirror keeps its address after a restore.

There is no hot standby. Nothing replicates the database, the wallets or the configuration to a second host, and
nothing fails over by itself. The retired deployment's managed database ran one node without a standby; a standby node is a provider
option that needs its own cost approval ([deployment notes](deployment-notes.md)).

What you can prepare instead is a spare: a host that is already encrypted, hardened and pinned, holding no market
secrets until you need it. It shortens a replacement but changes none of its steps. Never start a second application
against the same database or wallets, and never publish the same onion from two hosts.

## 2. Replace a host, role by role

### Every new host

Do these on each replacement, whatever its role, before any private file reaches it.

1. **Create the host with an encrypted root.** Provider-neutral: [prepare encryption](server-hardening.md#prepare-encryption-before-running-setup)
   and [secure recovery media](server-hardening.md#secure-recovery-media-before-installing-linux). On a cloud provider with a rescue image:
   [create the machine and its firewall](encrypted-hosts.md#1-create-the-machines-and-their-firewalls), then
   [boot the rescue image](encrypted-hosts.md#2-boot-the-rescue-image),
   [pin the rescue SSH](encrypted-hosts.md#3-pin-and-lock-the-rescue-ssh),
   [encrypt the root](encrypted-hosts.md#4-encrypt-the-root-and-install-the-os) and
   [unlock the first boot](encrypted-hosts.md#5-first-boot-and-manual-unlock).
2. **Pin the new host keys.** The replacement has new SSH host keys: for the rescue system, the pre-boot unlock and
   the installed system. Read each fingerprint from the authenticated console and pin it under its own alias
   ([pinned aliases](encrypted-hosts.md#pinned-aliases)). Never accept a changed key without that check, and never
   disable host-key checking.
3. **Back up the LUKS header** off the host ([header backup](encrypted-hosts.md#6-back-up-the-luks-header)). Keep the
   passphrase and header apart from the server and its snapshots.
4. **Set up operator access.** A named operator with key-only SSH, zero core limits, no swap and Docker
   ([operator access](encrypted-hosts.md#7-operator-access-core-dumps-and-docker),
   [hardening](server-hardening.md#2-establish-key-only-operator-access)).
5. **Restrict the network.** Allow only the flows in the [service flow matrix](service-topology.md#allowing-only-required-service-flows),
   at the host and at the provider. Then change the **other** hosts' rules so they accept the new host's private
   address, and remove the old one once it is retired.

### Application host

**What you restore, and from where.**

- The configuration and identities: the encrypted [recovery export](recovery.md). It holds the keys, TLS files,
  endpoint references, the public administrator client records and the administrator onion.
- The **current** retention directory, never an older copy ([retention](../retention.md#before-enabling)).
- The public dependency-report directory, at its saved path.
- Not the database or wallets: they stay on their own hosts. If the database was lost too, see [Database](#database).

If the old host's disk is lost, its current retention directory is lost with it, unless you kept a current copy
elsewhere. Neither the recovery export nor `scripts/backup.sh` copies it. Missing current state is an operator recovery
problem: never start the market on an empty or older directory to get past it ([retention](../retention.md#restoring)).
If the database was lost as well, recovery stays blocked until an authoritative current state is available.

**Steps.** These follow the runbook's [replacement leg](../testnet-runbook.md#leg-replacement-application-host-a-698)
and [recovery](recovery.md).

1. Prepare the new host as in [every new host](#every-new-host). Install the tagged binary at `/usr/local/bin/opsecmkt`.
   Apply the application-host policy with the new address in `app_address` ([bootstrap](server-bootstrap.md)), in a
   new output directory:

   ```sh
   ./opsecmkt host plan --config host.json --out host-plan-new
   sudo --preserve-env=SSH_CONNECTION ./opsecmkt host apply --config host-plan-new/host.json --operator-access-verified
   sudo --preserve-env=SSH_CONNECTION ./opsecmkt host confirm --config host-plan-new/host.json
   ```

   Run `confirm` from a **new** SSH session within five minutes, or the policy rolls back.
2. Check the encrypted storage:

   ```sh
   install -d -m 700 /srv/opsecmkt /srv/opsecmkt-inputs /srv/opsecmkt-retention
   sudo /usr/local/bin/opsecmkt deploy storage verify --path / --path /srv/opsecmkt --path /srv/opsecmkt-inputs --path /srv/opsecmkt-retention
   ```

3. Let the new host in. Move the database's **trusted source** from `10.20.0.10` to `10.20.0.30`
   ([managed PostgreSQL](encrypted-hosts.md#8-managed-postgresql)), and allow `10.20.0.30` on the payment host's RPC
   front ends. Keep the old entries only until the old host is fenced.
4. **Fence the old host.** If it still answers, stop it:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-prod
   ```

   If it does not answer, make sure it cannot come back: power it off at the provider and keep it off the network.
   `--old-host-fenced` below records that you did this; it does not check it.
5. Copy the current retention directory to its saved path, here `/srv/opsecmkt-retention`, through an authenticated
   encrypted channel. Put the dependency-report directory back at its saved path. Restore accepts a missing
   dependency-report directory, but every later command refuses it.
6. **Restore with the new host's own address.** Decrypt the export on the offline machine and stream the document
   straight into this command on the new host. Never decrypt it on a server.

   ```sh
   sudo /usr/local/bin/opsecmkt deploy recovery restore --dir /srv/opsecmkt/market-prod --old-host-fenced --app-bind 10.20.0.30:8080 --backend-bind 10.20.0.30:8443
   ```

   Without `--app-bind` and `--backend-bind` the old host's listeners come back, and only another restore into an
   absent directory changes them.
7. Bind the storage. `deploy tls replace` and `deploy endpoint set` refuse until this has run.

   ```sh
   sudo /usr/local/bin/opsecmkt deploy storage bind --dir /srv/opsecmkt/market-prod
   ```

8. Replace the TLS leaves only if a name in them changed, or for a compromised host
   ([below](#rotate-the-application-tls-leaves)). A leaf that names only `app.internal` still fits.
9. Check, then install. `plan` prints both listeners: stop unless they name `10.20.0.30`.

   ```sh
   sudo /usr/local/bin/opsecmkt deploy plan --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy doctor --dir /srv/opsecmkt/market-prod
   sudo /usr/local/bin/opsecmkt deploy install --dir /srv/opsecmkt/market-prod
   ```

10. **Move the name.** On the gateway host, make `app.internal` resolve to `10.20.0.30`, by private DNS or the hosts
    file. Then restart the gateway so no old connection remains:

    ```sh
    systemctl restart opsecmkt-gateway
    ```

11. The public market was down, and the escrow watcher could not see it. Ask for the
    [deadline recheck](../operator-guide.md#after-an-outage) from the new host. It needs a host-readable database URL
    file, which the recovery export does not carry: write it as [production section 2](production.md#2-create-and-verify-managed-postgresql)
    shows.

    ```sh
    DATABASE_URL_FILE=/srv/opsecmkt-inputs/database.url /usr/local/bin/opsecmkt -request-deadline-recheck
    ```

**Prove it works.**

- `deploy status` shows the services healthy. The administrator onion is the same address as before.
- Probes as in [verify allowed and denied connections](server-hardening.md#4-verify-allowed-and-denied-connections):
  approved SSH passes, other SSH fails, only the gateway reaches ports `8080` and `8443`.
- Two fresh Tor clients: the public onion answers, and returns `404` for `/setup` and `/admin`. The authorized client
  reaches the private `/login`; the unauthorized one gets `0xF4`. A timeout proves nothing.
- An upload through the gateway works. **Admin → Payments** shows payment readiness.
- Reboot the host, unlock it by hand, and check that the services return by themselves
  ([full host reboot](../operator-guide.md#recovering-after-a-full-host-reboot)):

  ```sh
  sudo /usr/local/bin/opsecmkt deploy storage check-bound --dir /srv/opsecmkt/market-prod
  ```

  If you use reboot control, acknowledge the new boot with `deploy host-reboot ready --dir DIR --confirm-unlock-rehearsed`.

**Retire the old host.** Keep it fenced. Remove `10.20.0.10` from the database's trusted sources, the payment host's
allowlist and the provider firewall. If it still runs, check that a new connection from it is refused. Keep its disk and snapshots
until the owner decides. Deleting them, and the old installation's leftovers listed in the runbook's
[retirement table](../testnet-runbook.md#what-the-owner-must-approve), needs the owner's approval.

#### Rotate the application TLS leaves

For a compromised application host, or a leaf whose name changed. On the issuing workstation, make new CAs and leaves
as in [production section 3](production.md#3-prepare-application-and-gateway-identities). The CA private keys stay
there.

```sh
umask 077
install -d -m 700 ./service-tls-new
./opsecmkt keys generate tls-ca --name 'Application transport CA' --cert-out ./service-tls-new/app-ca.pem --key-out ./service-tls-new/app-ca.key
./opsecmkt keys generate tls-server --name 'Application HTTPS' --host app.internal --ca-cert ./service-tls-new/app-ca.pem --ca-key ./service-tls-new/app-ca.key --cert-out ./service-tls-new/app.pem --key-out ./service-tls-new/app.key
./opsecmkt keys generate tls-ca --name 'Upload transport CA' --cert-out ./service-tls-new/upload-ca.pem --key-out ./service-tls-new/upload-ca.key
./opsecmkt keys generate tls-server --name 'Private upload backend' --host app.internal --ca-cert ./service-tls-new/upload-ca.pem --ca-key ./service-tls-new/upload-ca.key --cert-out ./service-tls-new/backend.pem --key-out ./service-tls-new/backend.key
./opsecmkt keys generate tls-client --name 'Upload gateway' --ca-cert ./service-tls-new/upload-ca.pem --ca-key ./service-tls-new/upload-ca.key --cert-out ./service-tls-new/gateway-client.pem --key-out ./service-tls-new/gateway-client.key
```

Copy `app.pem`, `app.key`, `upload-ca.pem`, `backend.pem` and `backend.key` to a new mode-`0700` directory on the
application host, here `/srv/opsecmkt-inputs-new`. After `deploy storage bind`, with the installation stopped:

```sh
sudo /usr/local/bin/opsecmkt deploy tls replace --dir /srv/opsecmkt/market-prod --tls-cert /srv/opsecmkt-inputs-new/app.pem --tls-key /srv/opsecmkt-inputs-new/app.key --upload-cert /srv/opsecmkt-inputs-new/backend.pem --upload-key /srv/opsecmkt-inputs-new/backend.key --upload-ca /srv/opsecmkt-inputs-new/upload-ca.pem
```

Give the gateway `app-ca.pem`, `upload-ca.pem`, `gateway-client.pem` and `gateway-client.key` in
`/etc/opsecmkt-gateway`, owned by its service account, and restart it.

#### Move or rotate the database connection

Write the new URL into a mode-`0600` file in the [production format](production.md#2-create-and-verify-managed-postgresql)
(`sslmode=verify-full`, host CA path). With the installation stopped:

```sh
sudo /usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-prod
sudo /usr/local/bin/opsecmkt deploy endpoint set --dir /srv/opsecmkt/market-prod --database-url-file /srv/opsecmkt-inputs/database-new.url --database-ca /srv/opsecmkt-inputs/database-ca.pem
sudo /usr/local/bin/opsecmkt deploy up --dir /srv/opsecmkt/market-prod
```

It replaces only the database URL and its CA. Every other key, endpoint and pin stays as it was.

### Payment host

The application host's controller does not manage this host. You build it by hand, as
[enabling payments](enable-payments.md#native-production-with-separate-payment-services) describes.

**What you restore, and from where.**

- The escrow wallet directory, from the newest **consistent** wallet backup: one taken with the application and the
  wallet RPC both stopped ([wallet backups](../testnet-runbook.md#back-up-and-restore-monero-escrow-wallets)). Its
  `.shared-ringdb` is inside it.
- The HTTPS front end's TLS leaf, key and RPC credentials, from the payment host's encrypted backup.
- Not the blockchain. It is public and rebuildable, so the new daemon synchronizes it again.

**Steps.**

1. Pause new payments: **Admin → New escrow checkouts → Paused**. Then stop the application, which must not run while
   the wallets are replaced:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy down --dir /srv/opsecmkt/market-prod
   ```

2. Prepare the new host as in [every new host](#every-new-host). Allow only the application host to reach the RPC
   front ends.
3. Install the daemon and the dedicated wallet RPC on the same network as before; stagenet for every rehearsal. Start
   the wallet RPC with `--daemon-address` naming this daemon and `--shared-ringdb-dir=WALLET_DIR/.shared-ringdb`.
   Restore the wallet directory onto encrypted storage before the wallet RPC first starts.
4. Serve the RPC front ends **under the same names, with the same credentials and a leaf from the same CA** as
   before. No command changes the application's saved wallet and daemon URLs or its `MONERO_RPC_CA`, so the new host
   must answer where those URLs point. Then make those names resolve to the new host where the application runs.
   For a compromised payment host this keeps credentials the attacker may hold: read its rows in the
   [rotation table](#compromised-or-suspected-compromised) first.
5. If the wallet copy is older than the database, follow the operator guide's steps for a wallet volume restored from
   an older backup: set the sweep marker before the application starts, and restore any wallet created after the
   backup from its sealed seed ([Monero escrow](../operator-guide.md#monero-escrow)).
6. Start the application again:

   ```sh
   sudo /usr/local/bin/opsecmkt deploy up --dir /srv/opsecmkt/market-prod
   ```

**Prove it works.** Wait until the daemon and wallet have synchronized. **Admin → Payments** must accept the endpoints,
network, wallet secret and seed key. If you set the sweep marker, the sweep must have finished. Compare each wallet's balance with the
market's records. In a stagenet rehearsal, take a small order through to a confirmed settlement. Only then set
**New escrow checkouts** back to **Escrow**.

**Retire the old host.** Disable its daemon and wallet RPC so they cannot start again, the way the runbook's
[re-initialization](../testnet-runbook.md#re-initialize-from-the-tagged-build) step 1 disables the old units. Two wallet
RPCs must never serve the same wallets. The old wallet files hold funds: deleting them needs the owner's approval.

### Gateway host

**What you restore, and from where.** From the gateway's encrypted backup: `app-ca.pem`, `upload-ca.pem`,
`gateway-client.pem` and `gateway-client.key`; the public onion's `hostname`, `hs_ed25519_public_key` and
`hs_ed25519_secret_key`; the scanner's `clamd.conf` and the gateway's unit file. For a compromised gateway, use new
client material ([below](#rotate-the-application-tls-leaves)) and a new onion identity instead.

**Steps.**

1. Prepare the new host as in [every new host](#every-new-host). A Tor storefront needs no public inbound web port.
2. Let it in on the application host. Put its private address in the `gateway_address` of `host.json`, then run
   `host plan` into a new output directory, `host apply` and `host confirm` as in the
   [application steps](#application-host). Change the provider firewall the same way.
3. Run ClamAV and the gateway service as in [production section 5](production.md#5-run-the-scanner-and-supervised-gateway).
   Make `app.internal` resolve to the application's private address on this host.
4. **Stop the old public Tor service first,** then install the identity in the new Tor `HiddenServiceDir`, owned by
   the Tor account ([public gateway identity](tor.md#prepare-the-public-gateway-identity)). Check that its `hostname`
   is the full address you expect. For a compromised gateway, make a new identity on the issuing machine instead:

   ```sh
   install -d -m 700 ./identities-new
   ./opsecmkt keys generate onion --out-dir ./identities-new/storefront
   ```

   Publish the new address in your signed verification document. Put it in `OFFICIAL_ADDRESSES`, and in
   `BRANDING_UPLOAD_ORIGIN` if you set one: change those entries in the `settings` of `operator.json`, then run
   `deploy plan` and `deploy up`. The saved `gateway_url` keeps naming the old onion; no command changes it.
5. Start the gateway:

   ```sh
   systemctl daemon-reload
   systemctl enable --now opsecmkt-gateway
   systemctl is-active opsecmkt-gateway
   ```

6. Ask for the [deadline recheck](../operator-guide.md#after-an-outage) on the application host, as in its step 11,
   because public ingress was down.

**Prove it works.** `systemctl is-active` prints `active`. A ClamAV PING answers. Two fresh Tor clients reach the
public onion, which answers `404` for `/setup` and `/admin`. An upload through the gateway passes the scan and appears
in the listing. **Admin** shows the scanner state as current, not unknown. To read a refusal, run the unit's command
in the foreground as [production section 5](production.md#5-run-the-scanner-and-supervised-gateway) shows.

**Retire the old host.** Disable its gateway and Tor units so they cannot start again:

```sh
systemctl disable --now opsecmkt-gateway
```

Do the same for its Tor service, under that unit's own name. Remove its address from the application host's policy and
the provider firewall. Its onion identity directory and disk stay until the owner decides; a compromised identity is
never published again.

### Database

The database is managed PostgreSQL, so there is no server of yours to rebuild. Three things can happen to it.

**The application host moved.** Change the trusted source to the **new application host's private address**, and
remove the old one. Check that the new host connects with verified TLS and that a new connection from the old host is
refused. For a managed cluster, see [managed PostgreSQL](encrypted-hosts.md#8-managed-postgresql).

**The database was lost.** Create a new database and role as in
[production section 2](production.md#2-create-and-verify-managed-postgresql). Allow only the application host's
private address and the approved backup host. Then, from the matching source checkout on the backup host, restore the
newest off-host backup with `scripts/restore.sh`. It asks you to type `RESTORE` and sets the payment restore gate:

```sh
export RESTORE_DATABASE_URL="$(cat /secure/opsecmkt/new-database.url)"
AGE_IDENTITY=/secure/offline/backup-key.txt ./scripts/restore.sh /secure/backups/market.dump.age
unset RESTORE_DATABASE_URL
```

Point the application at it with [`deploy endpoint set`](#move-or-rotate-the-database-connection), keeping the
current retention directory in place. Start it with public ingress still closed, and follow the
[reconciliation procedure](../testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts). Clear the gate
only after that, with the application stopped ([clear the gate](../testnet-runbook.md#clear-the-gate)):

```sh
DATABASE_URL_FILE=/srv/opsecmkt-inputs/database-new.url /usr/local/bin/opsecmkt -clear-restore-gate YOUR_ADMIN_HANDLE
```

A provider point-in-time recovery or snapshot restore does **not** set the gate. Before the application first starts
on such a database, set it yourself ([production section 7](production.md#7-establish-backups-and-recovery)):

```sh
DATABASE_URL_FILE=/srv/opsecmkt-inputs/database-new.url /usr/local/bin/opsecmkt -set-restore-gate
```

**It was compromised.** Change the application role's and the provider administrator's credentials, and apply the
new URL with `deploy endpoint set`. The records were readable; nothing makes them secret again.

**Prove it works.** The read-only check in [production section 2](production.md#2-create-and-verify-managed-postgresql)
shows the intended database and role, `UTF8` and `ssl = t`. The application starts, retention replay finishes, and the
gates are as the reconciliation says. Point the backup job's `BACKUP_DATABASE_URL` at the new database, then take a
backup and restore it into a disposable database.

**Retire the old host.** Keep the old cluster until the owner decides. Deleting it, and letting its provider backups
expire, needs the owner's approval. Remove its URL from every backup job.

## 3. What you need before disaster

Check each item now. A replacement is only as good as what survives the host. Items marked **UNVERIFIED** have no
test or rehearsal behind them in this repository.

- [ ] Off-host encrypted database backups, each with its `.retention.json` sidecar, and a restore into a disposable
      database that you have actually run ([backups](../operator-guide.md#backups-and-recovery)).
- [ ] Consistent wallet backups, taken with the application and wallet RPC stopped.
- [ ] A recovery export (`deploy recovery export`) taken after every configuration change, with a checksum copy kept
      apart ([recovery](recovery.md)). The offline private key that decrypts it.
- [ ] A current copy of the retention directory that survives the application host. **UNVERIFIED:** no command keeps
      one, and nothing here proves a copy is current.
- [ ] Each host's LUKS header backup and passphrase, off the host and apart from its snapshots.
- [ ] The CA material: both transport CAs and the Monero CA, with their private keys on the issuing workstation and
      protected recovery copies.
- [ ] The gateway's private files and the public onion identity, encrypted and labelled by role.
- [ ] Each host's recorded host-key fingerprints: recovery, pre-boot and installed system.
- [ ] The private operator record: addresses, resource names, onions, image digests and where each key is kept.
- [ ] A tested restore of the whole set on a spare host. **UNVERIFIED:** the runbook's
      [restore rehearsal](../testnet-runbook.md#leg-restore-rehearsal-a-698) has not run.

## 4. What has been rehearsed

Be clear about what has run before you depend on it.

- None of the replacement legs has been run on a real host. The runbook's
  [restore rehearsal](../testnet-runbook.md#leg-restore-rehearsal-a-698) and
  [replacement application host](../testnet-runbook.md#leg-replacement-application-host-a-698) legs are A-698, which
  is still blocked. Replacing a payment, gateway or database host has no rehearsal at all.
- The application-host restore order runs only in a Go test, against a fake encrypted host and a fake Docker
  (`TestRecoveryLegRestoreBindTLSReplaceRunsInWrittenOrder`).
- `tests/test_replace_a_host_docs.py` checks that every `opsecmkt` command and flag in this guide exists. It does not
  check that a command works at that point.
- The [encrypted-host procedure](encrypted-hosts.md) was run for three stagenet hosts. That covers host creation
  and the encrypted root, not a market's recovery.
- The [deployment notes](deployment-notes.md) record a planned move of the retired deployment's three roles to
  encrypted hosts, with the old hosts still available. That deployment is retired, and it did not follow this guide.
- No recovery time has been measured. This guide states none.

Record a rehearsal as the runbook's [recording the results](../testnet-runbook.md#recording-the-results) says, and keep
failures with the passes.
