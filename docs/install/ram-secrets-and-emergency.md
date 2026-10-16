# RAM secrets and manual emergency response

The native controller can load a bounded, administrator-decrypted installation
bundle into Linux tmpfs. The offline workstation keeps the private OpenPGP
decryption key. Production servers receive only the runtime secrets they need.
Use this with the [separated service topology](service-topology.md) and
[native CLI reference](cli-reference.md).

This controls where runtime files are stored. A running application, root or a
Docker administrator can still read online secrets and memory. tmpfs can use
swap; crash dumps, hypervisor snapshots and live memory acquisition also matter.
Power loss is not a guarantee of secret destruction. Review encrypted swap or
disabled swap, crash-dump and snapshot policy with the host administrator. See
the [Linux tmpfs documentation](https://docs.kernel.org/filesystems/tmpfs.html).

## Prepare the installation offline

Use `deploy init` on a trusted workstation to generate or import the stable
application and wallet secrets. Configure the intended server endpoints, image
digests, Docker context, runtime UID/GID and TLS identities. Keep the offline
source installation protected; it contains plaintext runtime secrets.

Set `--retention-dir` to an absolute persistent directory on the intended server,
outside `/run/opsecmkt-secrets`. Keep the signed dependency report directory
outside this RAM tree too. Absolute paths in the saved configuration refer to
the server. Only the controller's default derived admin socket directory moves
with the unlocked installation; explicit external paths keep their meaning.

For a production Tor installation, import the operators' public `.auth` files
using `--admin-client-auth-dir` and pin `--admin-tor-image`. The matching private
client keys stay on the authorized workstations. Follow the
[private administrator onion guide](tor.md#native-production-with-separate-onions).

Encrypt the installation to an existing public OpenPGP encryption key:

```sh
opsecmkt secrets pack --dir /secure/offline-installation \
  --recipient-public-key /secure/admin-encryption-public.asc \
  --out /secure/installation.pgp
```

`pack` includes `operator.json`, referenced secret files, the validated public
administrator client snapshot, complete managed `onion-public`/`onion-admin`
identity triplets when configured, and an optional installation receipt. Each
identity's secret, public key and hostname must match its saved address. It excludes
retention records and generated Compose/runtime files. It refuses private PGP
key material, symlinks, unrelated paths and existing output files. The plaintext
bundle is bounded to 64 files, 64 KiB per file and 1 MiB total JSON. Only
ciphertext is staged in the output directory.

The command supports native OpenPGP encryption. An independently managed age
workflow may transport the same validated JSON format, but this CLI does not
generate age bundles or accept a decryption identity on the server.

## Unlock over authenticated SSH through Tor

Install the binary outside the secrets mount. On the Linux server, root creates
a fresh mount before unlocking:

```sh
sudo opsecmkt secrets mount --dir /run/opsecmkt-secrets
```

This creates a 16 MiB tmpfs with `nosuid,nodev,noexec,mode=0700`. An existing
destination must already be an empty, exact mount with these protections; the
command refuses to hide an ordinary disk directory. It checks actual filesystem
type, mount flags, ownership and every path component. Configure the boot
supervisor to create this empty mount and wait for the administrator. The CLI
does not install a boot unit or start services automatically.

On the administrator workstation, decrypt directly into the SSH input stream:

```sh
gpg --decrypt /secure/installation.pgp | \
  torify ssh -T operator@YOUR_SSH_ONION \
  'sudo -n /usr/local/bin/opsecmkt secrets unlock --dir /run/opsecmkt-secrets/market'
```

Replace the host with the authenticated SSH service, pin its SSH host key, and
use an operator account authorized for this specific command. `sudo -n` prevents
a password prompt from consuming the bundle stream. Decryption happens on the
administrator workstation; the plaintext crosses the encrypted SSH connection.
Provision Tor/`torify`, the SSH onion service and constrained sudo authorization
separately; the binary does not install or configure them.
No plaintext bundle file or decryption private key is written on the server.
Local process memory and the authorized transport can still see plaintext.

`unlock` reads JSON only from stdin. It requires root, checks the containing
hardened tmpfs, validates the complete bundle before publishing it atomically,
and refuses an existing instance. Controller metadata remains root-owned 0700,
with files 0600. The `secrets/` subtree is assigned to the configured runtime
UID/GID so Docker can bind the private files. Validated public Tor client records
become readable by the Tor service when the controller materializes its inputs.

Review and start the selected installation explicitly:

```sh
sudo opsecmkt deploy plan --dir /run/opsecmkt-secrets/market
sudo opsecmkt deploy install --dir /run/opsecmkt-secrets/market
```

After a reboot, remount, unlock and start the same configuration. Preserve its
project identity, Docker context and stable secrets. On the same host, the
persistent database/wallet and Tor identity Docker volumes survive container
removal. On a replacement app host, the matching managed identity in the encrypted
bundle can seed a fresh Tor volume. Restoring its protected volume backup also
works; a mismatched or partial existing identity refuses startup. Legacy
configurations without a managed source must preserve their valid existing
volume. Run `install` or `up` before using app-only `redeploy`, and follow the
normal database/payment recovery gate.

The native Tor helpers copy service keys into persistent Docker identity volumes.
Those copies are outside the RAM configuration and survive container removal and
power loss. RAM unlock does not make Tor service keys RAM-only. Protect the volume
storage separately; emergency stop retains it. The client authorization private
key remains on the administrator's client machine.

Runtime RAM files do not replace backups. Protect the encrypted installation
bundle, matching offline decryption key, stable application and wallet secrets,
onion identities, database and retention records independently. Never generate a
new wallet secret simply because the host rebooted.

## Review an emergency action before running it

These are manually invoked incident commands. A provider inquiry, failed canary,
unexpected reboot or other external signal never triggers an automatic wipe.
First identify the saved Compose project and review the selected action:

```sh
sudo opsecmkt emergency plan --dir /run/opsecmkt-secrets/market \
  --ram-dir /run/opsecmkt-secrets/market
```

Stopping requires the exact project printed by the plan:

```sh
sudo opsecmkt emergency stop --dir /run/opsecmkt-secrets/market \
  --confirm-project YOUR_SAVED_PROJECT \
  --ram-dir /run/opsecmkt-secrets/market
```

The controller verifies the installation receipt and pinned Docker context,
writes an emergency stop marker, kills the project's containers, then removes
them and any project orphans with a zero stop timeout. It retains Docker data
volumes and requires a final inventory to show zero remaining project containers.
Only after these checks succeed can `--ram-dir` remove the selected installation files.
This unlinks tmpfs files; it does not promise memory zeroization. Failed stopping
prevents RAM cleanup, disk erasure and reboot. Missing or expired application
keys do not prevent stopping an existing claimed installation.

Without `--ram-dir`, the private `emergency.json` marker remains and blocks
`install`, `up` and `redeploy` until an operator reviews the incident and
explicitly removes the marker. Removing the RAM installation removes its marker
too; do not unlock an old bundle or let another supervisor recreate containers
during incident response. Independent systemd jobs, other Compose projects and
separate service hosts require their own explicit stop procedures.

## Explicit LUKS keyslot erasure

`emergency erase-luks` requires Linux root, a real LUKS block device and the exact
device path repeated in `--confirm-device`. There is no default device. Plan the
same exact device before executing; a placeholder path is refused. See the
[CLI reference](cli-reference.md) for the full flag syntax.

Device erasure, RAM cleanup and reboot affect the host running the CLI. Docker
operations use the saved context, which may point elsewhere. Run this incident
procedure on the intended server and inspect that context before proceeding.

The command holds the initial block-device descriptor and pins its device
identity and LUKS UUID. It checks them again before passing the held descriptor
to the system `cryptsetup luksErase --batch-mode`. It runs only after
successful project shutdown and any requested RAM cleanup. The optional
`--reboot` requests an immediate unsynced reboot only after successful keyslot
erasure. This can lose unwritten data; the kernel's
[SysRq documentation](https://docs.kernel.org/admin-guide/sysrq.html) describes
that behavior.

LUKS erasure destroys keyslots, not every encrypted data sector. Existing header
backups and already-open kernel mappings can retain access. It can also make
legitimate recovery permanently impossible. Review protected header backups,
mapping closure, the exact device and the approved recovery consequences before
use. See the [cryptsetup luksErase manual](https://man7.org/linux/man-pages/man8/cryptsetup-luksErase.8.html).

The implementation is tested with fake disk/reboot operations. Ordinary tests and
installation commands never invoke real erasure or reboot. No generic evidence
deletion, Git history rewriting or automatic revocation publishing is included.

## Rotate identities from a trusted workstation

After assessing compromise, revoke affected operator authorization and replace
exposed service credentials through the normal recovery process. Prepare any
signed public revocation notice offline and publish it through an independently
authenticated channel. Never reuse an onion identity that is known compromised.
Coordinate existing Monero wallets, pending payouts and stable secret pins before
changing keys.
