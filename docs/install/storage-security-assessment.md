# Storage and physical-access assessment

This records the read-only storage review begun on 2026-10-15 for the application,
gateway and payment hosts. It separates measured guest settings from provider
statements and the work still needed before claiming protection of locked disks.

**Original review result:** the three compute hosts lacked guest-managed disk
encryption for persistent application state, and crash interception was enabled.
The findings below preserve that review's observations. All three roles have
since moved state to encrypted replacements, with payment runtime and application
endpoint cutover verified. Original plaintext hosts remain fenced and retained.
The production application passed an attended OS reboot/manual-unlock recovery. See the [current deployment checkpoint](deployment-notes.md#evidence-and-remaining-gates).
No disks were changed, services restarted, keys read, or production crashes
induced during the original read-only review.

## Findings from the running hosts

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

For the provider used by this deployment, DigitalOcean's
[Droplet responsibility model](https://www.digitalocean.com/security/shared-responsibility-model-droplets)
states that virtual disks on hypervisor local storage are not encrypted at rest.
Its [managed PostgreSQL security guide](https://docs.digitalocean.com/products/databases/postgresql/how-to/secure/)
states that managed database data is encrypted at rest with LUKS. These statements
describe different products; do not transfer one product's guarantee to another.

Weekly compute backups are enabled for this installation. The provider's
[VM backup limits](https://docs.digitalocean.com/products/backups/details/limits/)
state that these backups are not encrypted at rest, although they are not
externally accessible. They preserve another sensitive disk copy and must be
included in the migration and retention inventory. The separately encrypted
logical recovery archives have a different protection boundary.

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

## Preparing an encrypted replacement

Use this sequence to prepare a reviewed migration. It is not a command sequence
for formatting an existing production disk. Device selection, capacity, unlock
transport, recovery, downtime and any added billable storage require a concrete
migration plan before execution.

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
   backup units need their own reviewed checks. This code change has not migrated
   the existing plaintext hosts.
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

Until these checks pass, describe guest disk encryption and cold-boot recovery as
**pending**. A firewall, managed database, encrypted archive or RAM mount does not
complete this storage acceptance gate.
