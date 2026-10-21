# Recover application configuration

`opsecmkt deploy recovery` exports and restores **application configuration only**.
It preserves configured secret/public/TLS files, project identity, endpoint and
path references, public administrator client authorization files, and managed
public/admin onion identities. It does not rotate keys or start services.

This archive is not a complete deployment backup. It excludes PostgreSQL data,
retention content and signing material, Monero wallet state and backups,
blockchain data, Docker volumes, upload/scanner state, dependency worker state,
host bootstrap configuration, systemd units, storage proofs and reboot readiness.
Recover those layers with their service-specific procedures. The original
`SETUP_TOKEN` and application keys remain essential for existing database state.

Export to an absent directory outside the installation. Export and restore refuse an `UPLOAD_CA` that shares a
certificate with `MONERO_RPC_CA` or `DATABASE_CA` (A-589); see [UPGRADING](../../UPGRADING.md) for a bundle exported
before that check.

```sh
opsecmkt deploy recovery export \
  --dir /srv/opsecmkt/app \
  --recipient-public-key /srv/operator/offline-recovery-public.asc \
  --out /srv/operator/app-recovery-YYYY-MM-DD
```

Use exactly one currently usable armored OpenPGP encryption public key. Private
recovery keys are refused. The new archive directory is mode 0700; all four files
are mode 0600:

- `private.pgp`: encrypted bounded recovery document containing all managed files.
- `deployment.json`: nonsecret project, image, path, mode and public onion metadata.
- `manifest.json`: SHA-256 hashes of the ciphertext, deployment metadata and guide.
- `RECOVERY.txt`: the recovery limitations and required operator actions.

Keep the archive and a trusted checksum copy in separate protected locations.
Hashes detect corruption; they do not independently authenticate a maliciously
replaced archive. The offline decryption tool must verify the complete encrypted
message, including its integrity protection. Test recovery using disposable
stagenet configuration, without copying operational private keys into test logs.

Retain each administrator browser's private `.auth_private` credential separately
on the trusted client; this archive contains public authorization records only.

A legacy Tor identity held only in a Docker volume cannot be recovered by this
export. Export refuses that configuration. First adopt and verify the existing
volume identity as managed key files; do not generate a replacement onion.

Fence the old application host before bringing its identities onto a replacement
host: ensure it cannot resume the application or Tor services. Production restore
requires `--old-host-fenced` as an explicit attestation. This flag records operator
intent for the invocation; it does not remotely fence or verify the old host.

Prepare the replacement host using the [binary installation](binary.md) and
[service topology](service-topology.md) instructions. Restore requires Linux and a
fresh target whose existing ancestors contain no symlinks. The target filesystem
must pass the persistent-storage check (tmpfs and ramfs are refused). Production
also checks the current host's LUKS2 storage and swap policy before staging any
private file; it requires the local Docker daemon for the normal storage probe.
Keep the recovery private key on a trusted offline machine. Decrypt there and
transfer the resulting document directly through a protected stream to:

```sh
opsecmkt deploy recovery restore \
  --dir /srv/opsecmkt/app \
  --old-host-fenced
```

The complete offline-decrypted recovery JSON document must arrive on stdin. The
command accepts no plaintext filename, private-key or password flag. Do not run
decryption with the private recovery key on the replacement application host.
A `secrets pack` plaintext bundle is a different format and is not accepted here.

Restore checks document version, unknown and repeated JSON fields, complete
per-file hashes, exact managed-file references, onion key/address agreement,
regular/private file permissions and bounded sizes. Managed paths cannot escape
the installation or contain symlinks. Limits are 64 managed files, 64 KiB per file,
1 MiB for the underlying bundle, and 2 MiB for the complete recovery document.
Invalid input fails before publication. Any existing destination, including an
empty directory or symlink, is refused. Restored directories are mode 0700 and
files mode 0600. Matching TLS leaf/key pairs are preserved even after certificate
expiry; the normal installation gate still refuses expired or not-yet-valid
certificates until they are renewed explicitly. The configured runtime UID/GID is retained for `secrets/`; root
is required when restoring files for a different runtime user.

Review all preserved references before installation. Retention, administrator
socket, dependency-report paths, project name and endpoint references still name
the original deployment's locations. Restore does not create external state or
rewrite those paths for the new host. Restore the database, wallets and retained
state separately; follow the payment restore gate and reconcile recovery state
before enabling transaction processing.

Production storage bindings are deliberately cleared, even if the same devices
are attached. Recheck the restored paths and mounts, then explicitly bind the
replacement host:

```sh
opsecmkt deploy plan --dir /srv/opsecmkt/app
opsecmkt deploy storage bind --dir /srv/opsecmkt/app
```

Installation/start remains refused until storage is rebound. Recreate the host's
boot storage guard and independently review reboot readiness on the replacement
host before following `deploy install`. Old host boot/reboot readiness is never
exported or adopted. Keep services stopped until excluded state and host controls
have been restored and verified. A successful configuration restore alone does
not prove service, wallet, payment or full disaster recovery.
