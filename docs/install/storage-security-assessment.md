# Storage and physical-access assessment

This records storage findings and the controls needed for locked-disk protection.
It separates measured guest settings, provider statements and recovery checks.

## Current reset checkpoint

On 2026-10-16, the owner confirmed that the previous market was unused and held
no funds, then authorized discarding its state. The old dedicated compute roles
and managed database were deleted. Three fresh LUKS2 roots, fresh keys and a new
managed database have been created. Off-host encrypted LUKS header copies passed
local authentication checks. All three bare hosts passed cold boot/manual unlock
and OS acceptance. Named-operator login and sudo were verified before normal
root SSH was disabled. Final aggregate provider role-firewall verification passed,
including every direct and tag attachment; each host has exactly one role policy.
The bootstrap firewall was removed after fresh operator/root-login checks. Public
runtime artifacts are staged on all three encrypted hosts. Guarded packages and
exact offline image/platform checks passed on all three; the private database
TLS/ordinary-role permission probe passed. Five on-host TLS leaf keys have public
certificates signed locally. Application bootstrap, private administrator TOTP
enrollment and audited XMR-intake pause reached independent read-only SQL
acceptance. Native gateway and container units are now active; loopback health
returned `200` and the isolated-scanner relay returned PONG. The actual relay
runs under an unprivileged UID/GID with no supplementary groups and zero
effective, permitted and ambient capabilities. The gateway certificate directory
has verified root ownership, intended service-group access and final mode `0750`;
TLS file bytes, ownership and modes were preserved. Five coordinated encrypted
captures passed off-host age EOF authentication and isolated logical PostgreSQL
restore, including exact table rows, live schema/sequence contracts, retention,
reboot-ledger and paused-intake checks. All three roles explicitly resumed after
fresh paused-intake/private-health verification. Independent Tor clients passed
public access, authorized private admin login and explicit unauthorized-client
refusal; guest homepage/navigation checks and owned-resource cleanup passed.
The operator desktop browser failed local signature verification, so its sign-in
and MFA/recovery-code journey remain unaccepted. Real uploads and wallet/full-service
recovery remain pending. All-role runtime cold-boot acceptance is recorded below.

Docker and containerd are now verified enabled as well as active on all three
hosts after a reviewed enable-only correction. No daemon restart or workload
change was performed by enablement. All three hosts subsequently passed actual
runtime reboots, one manual unlock each and automatic service return, followed by
fresh paused-intake SQL. The gateway’s first runtime boot exposed a socket cycle;
its controlled repair and second actual locked boot passed separately. The first
failed proof is preserved. A separate native acknowledgement bound app reboot
readiness to its new boot, with GET-only agent verification and an unchanged empty
receipt ledger. Restore instructions must apply the final socket correction and
reinstall boot enablement; the earlier role archive predates both.

Narrow boot-package holds/exclusions, explicit no automatic reboot, immutable
boot hashes and conservative pre-transaction app-readiness revocation are now
installed and verified on all three hosts. Public policy copies were matched
and retained off-host. Policy installation itself performed no package upgrade,
reboot or service change. The separate runtime rehearsals above prove service
return for the recorded boots, not full service/wallet disaster recovery.

The [current deployment checkpoint](deployment-notes.md#evidence-and-remaining-gates)
records the remaining gates. Earlier migration and storage observations below
are historical evidence. They do not describe the fresh hosts or prove that the
new application, wallets or backups recover successfully.

## Historical findings from the plaintext hosts

The original read-only review began on 2026-10-15. The three compute hosts lacked
guest-managed encryption for persistent application state, and crash interception
was enabled. No disks were changed, services restarted, keys read or production
crashes induced during that review. These hosts have since been deleted under
the owner's explicit reset authorization.

| Control | Observed state | Consequence |
| --- | --- | --- |
| Root and persistent state | All three roots are direct `ext4` partitions. No active device-mapper crypt target was present. Docker's data root, service directories and host logs resolve to those roots. | Guest-managed block encryption does not protect these files. File permissions and read-only container mounts do not encrypt their backing storage. |
| Application installation | Native secret inputs, the administrator onion source identity and the persistent administrator Tor volume reside on the application root filesystem. The secret directory is `0700`; private input files are `0600`. | Runtime secrets and onion identity storage persist across shutdown. The documented RAM-unlock workflow is available, but this installation is using disk-backed inputs. |
| Retention records | The persistent retention directory resolves to the same root filesystem and has restrictive directory/file modes. | Preserve current retention state during recovery and include it in the encrypted storage scope. An encrypted database backup alone does not protect these live records. |
| Gateway keys and Tor data | The gateway TLS private-key file and public storefront onion private-key file reside on its root filesystem, with restrictive directory/file modes. Tor's persistent data directory is disk-backed. | A copied readable guest disk can expose service identities. RAM-only application inputs would not cover this separate host. |
| Payment storage | Wallet, chain and Tor-state volumes resolve to the payment root filesystem. The RPC TLS private-key file is also disk-backed. | Include these volumes and service configuration in storage protection. Wallet contents, wallet-file encryption and balances were not inspected. No claim of a funded wallet or recovery success follows from a mounted volume. |
| Swap | `swapon --show` returned no active entries on any host. | No active swap was observed. This does not establish the next boot's configuration or provider memory-snapshot policy. |
| Hibernation | The kernel advertises disk sleep; the resume device is `0:0`. Hibernate units are static, rather than explicitly masked. | No configured resume device was observed. Hibernation is not established as disabled. |
| Crash interception | Apport is enabled and active on every host. `kernel.core_pattern` pipes to Apport; `fs.suid_dumpable` is `2`. Running Docker containers have unlimited core-file limits. | A crash can reach a collector that may persist process memory. The native gateway service has a zero soft core limit, which does not by itself stop a piped collector. |
| Existing crash files | No regular files were present in the inspected crash-report and systemd core-dump directories. | This is a point-in-time directory check. It does not prove that crash collection is disabled or that no report exists elsewhere. |
| Logging | Application and administrator Tor containers use Docker's `none` driver. Gateway Tor and payment containers use `json-file`; host logs are disk-backed. | App logging policy does not cover other services. Review their contents, rotation, retention and encrypted storage separately. |

No secret bytes, container environments, wallet contents or database records were
read for this storage review. Protected evidence retains the exact host mappings
and metadata. This published copy omits resource identifiers, addresses, onion
identities and operator-specific paths.

## Provider encryption is a separate control

For any hosting service, verify compute disks, attached volumes, managed databases,
snapshots and backups separately. Record which party holds each encryption key,
what a locked-disk copy exposes and how recovery works. A private network or a
managed database's encryption guarantee does not establish compute-disk encryption.

For historical context, the provider's published
[compute responsibility model](https://www.digitalocean.com/security/shared-responsibility-model-droplets)
says its local VM disks lack encryption at rest. Its
[managed PostgreSQL security guide](https://docs.digitalocean.com/products/databases/postgresql/how-to/secure/)
describes LUKS encryption for database storage. Its
[VM backup limits](https://docs.digitalocean.com/products/backups/details/limits/)
say compute backups are not encrypted at rest. These product-specific statements
were checked separately; use your chosen service's current documentation.

Weekly compute backups had been enabled for the earlier installation. That
historical schedule does not establish fresh-host backup coverage. Inventory
retained images and recovery archives separately; guest LUKS protects encrypted
root blocks while boot material can remain readable. Verify off-host encrypted
state backups and isolated restore independently of provider backup listings.

The guest checks above cannot independently attest to physical media handling,
provider key custody, backup encryption or hypervisor access. Provider-managed
database encryption also does not prevent an authorized database session or
provider-controlled running service from reading data. Continue to use verified
TLS, a least-privilege database role, a private endpoint and explicit trusted
sources. Keep separately encrypted recovery copies under operator control.

Guest encryption primarily protects a locked volume whose unlock material is
unavailable to the adversary. A running guest exposes plaintext to its authorized
processes and host administrators. A cloud provider or compromised hypervisor can
have additional access outside this model. See Ubuntu's
[full-disk encryption overview](https://documentation.ubuntu.com/security/security-features/storage/encryption-full-disk/)
for the at-rest protection boundary. No measured or documented control here
establishes protection from live provider access or physical memory acquisition.

## Performing a fresh empty reset

Use this path only when the owner explicitly authorizes losing the selected
installation's data and funds. Record that decision privately. If anything must
survive, use the retained-state migration procedure below instead.

1. **Freeze the reset scope.** Inventory the exact compute roles, database,
   firewalls, volumes, snapshots and local archives. Record which resources will
   be deleted and which copies must remain. Delete only the authorized resources;
   never select deletion targets by a loose name prefix.
2. **Create independent recovery custody.** Generate fresh operator and disk
   credentials, application secrets and separate public/admin onion identities.
   Keep browser authorization and recovery private keys off runtime hosts. Record
   where the operator can recover each item without the servers. Do not import
   historical wallet or retention state into an empty reset.
3. **Install encrypted roots before staging secrets.** Pin the recovery system's
   SSH identity through an authenticated console. Establish a fresh key-only
   session before sending any private input. Verify resource identity, actual
   disk capacity, auxiliary configuration drives, network MACs and the exact
   partition plan before formatting. Keep unlock material independent of disk,
   boot files, cloud-init and snapshots. Follow [server hardening](server-hardening.md).
4. **Verify each bare host boot.** Shut down and boot into locked root, then
   manually unlock through the pinned early-boot service. Verify the distinct
   normal SSH pin, expected LUKS/filesystem identities, network/DNS ownership,
   synchronized clock, no swap and zero core limits. Prove the named operator's
   login and sudo before disabling normal root SSH. Preserve failed evidence;
   stop for review instead of automatically repeating unlock or formatting.
5. **Verify the managed database.** Use its private endpoint, a dedicated ordinary
   application role, verified TLS identity and an application-only source rule.
   Check SQL privileges through that connection. Handle database-administrator
   credentials only in verified volatile storage and remove them after use;
   never retain them as application inputs.
6. **Prepare guarded runtime services.** Install package prerequisites without
   allowing daemons to start before storage, core and firewall guards exist.
   Verify the [immutable application image](binary.md#building-and-publishing-the-application-image)
   on the intended Docker engine. Generate TLS leaves on their owning encrypted
   hosts and sign only public CSRs off-host. Recheck storage identities at every
   start. Keep the Tor storefront loopback-only and restrict SSH/private services
   to their exact trusted sources; verify both allowed and denied connections.
7. **Establish launch and recovery gates.** Bootstrap the private app and verify
   administrator client authorization. Pause payment intake through the normal
   audited action and verify its saved database value before starting public
   ingress. Rehearse coordinated off-host backups and isolated restore, scanner
   failures, service return after manual unlock and wallet recovery. Mainnet
   synchronization or a running process alone does not satisfy these gates.

Keep per-host header backups, public SSH pins, unlock procedures and the observed
boot/restore results in the private operator recovery record. An authenticated
header archive is useful recovery evidence; it is not a restored operating system
or application. Follow the [production procedure](production.md) for configuration
and the [launch checklist](production-checklist.md) for remaining acceptance.

## Keeping manual unlock reliable across updates

Treat boot software as part of the recovery procedure. A one-time package
version selection is not a persistent update policy. Inventory the installed
renderer, kernel, cryptroot, early SSH and naming/handoff files; retain their
actual versions and generated initramfs hashes before maintenance.

Apply narrowly justified holds/exclusions to the installed boot path, explicitly
prevent unattended reboot, and preserve unrelated security updates. Before each
package transaction, conservatively revoke any app-host reboot acknowledgement
through the existing root-owned policy without changing its schema or saved
boot identity. Do not acknowledge it automatically afterward. A post-transaction
check should compare an immutable reviewed boot manifest and retain a protected
notice if any bound file, package or boot target changes or disappears.

Serialize package maintenance with operator readiness/reboot work. APT hooks do
not cover a direct raw package-manager invocation or privileged manual edit;
require the same explicit pre-maintenance revocation and post-maintenance check
for those reviewed exceptions. Stop on unknown state rather than silently
replacing a baseline.

Held security fixes still need prompt maintenance: review signed package/source
changes, stage the updated encrypted boot, derive the new handoff checks from
the new source, inspect the initramfs, and perform an attended off-host unlock
plus normal service/network return rehearsal before controlled rollout. Preserve
rollback/recovery material. Keep boot-policy installation, successful cold boot,
logical restore and wallet recovery as separate evidence.

## Preparing an encrypted replacement

Use this sequence when accounts, funds or existing service state must survive.
It is not a command sequence for formatting an existing production disk. Device
selection, capacity, unlock transport, recovery, downtime and any added billable
storage require a concrete migration plan before execution.

1. **Inventory every persistent copy.** Map the root, Docker data root, service
   configuration, private runtime inputs, all Tor identities and state, wallet
   volumes, retention state, upload temporary files, logs and backup staging.
   Include swap, hibernation images, crash collectors, snapshots and old recovery
   copies. Public chain data may be separated only after confirming it contains
   no wallet or private service state.
2. **Choose the storage boundary.** Prefer a fresh installation with an encrypted
   root and encrypted persistent data, using the operating system's supported
   LUKS2 procedure. If using a separate encrypted data volume, place every
   sensitive persistent path on it and keep plaintext copies off the root.
   Record the remaining unencrypted boot and firmware exposure. Encrypting an
   empty new volume does not protect files left on the existing root.
3. **Keep unlocking independent of the copied disk.** Hold recovery credentials
   offline. For the locked-disk threat, do not place an automatic unlock key on
   the same readable root, in container metadata, cloud-init data or an included
   snapshot. Prepare an authenticated, host-key-pinned operator unlock path and
   a tested recovery path. Unattended key release changes the threat model and
   needs separate review.
4. **Make boot wait for protected storage.** Configure mount dependencies and
   explicit mount checks for Docker and every native service. A missing or locked
   volume must prevent startup. Test that an ordinary empty directory cannot
   silently receive new secrets, logs or wallet files when a mount fails. The
   updated application controller installs Docker/containerd storage-check
   drop-ins before production starts and can print an equivalent candidate.
   Review mount ordering before enabling services. Native gateway, payment and
   backup units need their own reviewed checks. Installing a guard does not
   migrate data or encrypt any existing plaintext copies.
5. **Close memory-to-disk paths.** Establish disabled swap; new native production
   starts refuse every active swap device, including encrypted swap. Explicitly
   disable hibernation and unintended crash interception, and
   configure service and container core limits. A zero core limit alone does not
   stop a piped crash handler: Ubuntu documents this behavior for
   [Apport](https://documentation.ubuntu.com/project/contributors/debugging/apport/),
   as does the Linux [core manual](https://man7.org/linux/man-pages/man5/core.5.html).
   Recheck the effective kernel handler and running-process limits after boot.
   Review provider memory snapshots separately.
6. **Rehearse on isolated replacement hosts.** Restore protected configuration,
   identities, current retention records and wallets through the existing
   [recovery gates](../operator-guide.md#backups-and-recovery). Keep the old
   installation authoritative during the rehearsal and avoid a second active
   wallet-spending instance. Use disposable data for crash-policy tests.
7. **Approve and perform the cutover.** Record a coordinated quiescence and
   rollback plan before stopping the original services. After the reviewed
   maintenance window, verify private ingress, identities, data consistency,
   payment readiness and backup/restore behavior. Review old disks, VM backups
   and snapshots as retained sensitive copies; migration does not retroactively
   encrypt them.

The [RAM secret workflow](ram-secrets-and-emergency.md) can reduce runtime file
persistence in development. New native production starts require a persistent
encrypted operator installation and reject the former tmpfs layout. RAM does
not replace encrypted persistent storage:
managed Tor identity volumes remain persistent, and
[Linux tmpfs can use swap](https://docs.kernel.org/filesystems/tmpfs.html).
Keep stable wallet and application secrets through migration; regenerating them
can break existing escrow and sealed application records.

## Verifying the replacement before launch

Run read-only checks as a host administrator and retain their output privately.
Use the actual service paths from the protected inventory; do not print secret
files or unfiltered container inspection output.

```sh
lsblk --output NAME,TYPE,FSTYPE,MOUNTPOINTS
findmnt --target / --output TARGET,SOURCE,FSTYPE,OPTIONS
sudo dmsetup ls --target crypt
sudo docker info --format '{{.DockerRootDir}}'
swapon --show --output NAME,TYPE,SIZE,USED
sysctl kernel.core_pattern fs.suid_dumpable
cat /sys/power/state
cat /sys/power/resume
```

For each persistent service path, run `findmnt --target PATH` and trace its source
through the block-device tree. On a guest-encrypted filesystem, expect a crypt
mapping below the mounted filesystem and the intended encrypted device below
that mapping. A filesystem name alone does not prove encryption. Inspect selected
Docker mount fields and resolve each host source similarly; include named volume
mountpoints and bind sources.

Acceptance requires retained evidence for all of the following:

- Every private persistent path resolves through the intended encrypted boundary.
- With storage locked on a disposable boot, protected services refuse startup
  and create no fallback plaintext state.
- After a controlled shutdown of a disposable replacement, a copied locked disk
  cannot mount its protected filesystem without the independently held unlock
  material. This test does not assess remanent memory or live provider access.
- An authorized operator can unlock after boot, recover from a deliberately
  unavailable normal unlock path, and restore a backup on an isolated host.
- Effective swap, hibernation and crash-collection settings match the reviewed
  policy after reboot. A secret-free disposable crash test confirms the intended
  collector behavior; never crash production services to test this.
- Offline recovery credentials, protected LUKS header backups and encrypted state
  backups are recoverable by the authorized operator. Copies and retention rules
  are recorded separately from the production disk.

Report installation, cold-boot unlock, runtime service return and isolated restore
as separate results. Describe each uncompleted recovery check as **pending**.
A firewall, managed database, encrypted archive or RAM mount does not complete
this storage acceptance gate.
