# Encrypted hosts from a provider rescue image

This guide turns cloud virtual machines into hosts that meet the
[storage](server-hardening.md#5-choose-storage-encryption-for-the-threat) and
[access](server-hardening.md#2-establish-key-only-operator-access) rules in
[server hardening](server-hardening.md). It applies to a provider whose machines boot
a stock image with no encrypted root, and which offers a rescue or recovery image and
an out-of-band console. It was followed for a three-host stagenet market (application,
payments, gateway), each reinstalled onto a LUKS2 root (A-799).

That run is the evidence for these steps. It is not production evidence: no live
funds, public traffic or long-running operation were involved. Read
[What this does not protect](#what-this-does-not-protect) before relying on it.
Keep the provider, region, resource names and addresses in the private operator
record, not in this repository.

## Who does what

Some steps need the owner at the provider's control panel or console; the provider's
API cannot do them. The operator runs the rest from a workstation over pinned SSH.

| Step | Owner (control panel) | Operator (workstation) |
| --- | --- | --- |
| Boot the rescue image | Select it for the machine | Power-cycle the machine |
| Pin the rescue host key | Read the fingerprint in the console | Compare and pin |
| Encrypt the root | Type the passphrase | Never sees it |
| Install the OS | | Run the install script |
| First boot and unlock | Select the disk boot, type the passphrase | Power-cycle, check host keys |
| Header backup | File it in the private record | Copy it off the host |

Each host gets its own passphrase and its own LUKS volume key. Keep every
passphrase and header backup outside the servers and their snapshots.

## Before you start

- An approved operator SSH key with a passphrase, generated on the workstation.
  Give the provider only its public half.
- One provider tag or label on every resource you create, so a
  [label-keyed teardown](#tear-down) removes only those resources.
- A project or folder and a private network of your own, so the machines join no other
  project.
- A private, mode-`0700` folder on the workstation for IDs, addresses, pinned
  `known_hosts` files and the SSH config. Keep addresses out of logs, tickets and
  transcripts; mask them in any output you share.

## 1. Create the machines and their firewalls

Create each machine with no provider agent, no provider backups and no IPv6. Provider
backups are not encrypted at rest under your key
([storage assessment](storage-security-assessment.md)), and with no IPv6 there are no
IPv6 paths to police. The stock image is a placeholder: step 4 replaces it.

Create each host's provider firewall straight away and attach it to that machine.
Allow SSH only from the operator's `/32`, and service ports only from the private
address of the host that needs them:

| Host | Inbound |
| --- | --- |
| Application | 22 from the operator; 8080 and 8443 from the gateway's private address |
| Payments | 22 from the operator; the payment proxy ports from the application's private address |
| Gateway | 22 from the operator only (the storefront is Tor-only) |

Outbound for all three: TCP to any address, UDP 53 and 123, ICMP. Record each
resource's ID privately; some providers cannot label firewalls.

## 2. Boot the rescue image

The owner selects the rescue or recovery image for the machine. The operator then
power-cycles it through the provider's API and confirms the power cycle is recorded
after the change.

Use a full power cycle. A soft restart reboots the installed system and may ignore
the rescue selection; a password prompt for the machine's own hostname means it is
still the stock image.

Open the out-of-band console and get a root shell. **Read what the rescue image says
about SSH.** A rescue image may turn on SSH with a random root password shown on
screen. The firewall from step 1 is then the only thing limiting it until step 3
locks it.

## 3. Pin and lock the rescue SSH

In the console, one line at a time (a browser console can drop lines from a
multi-line paste):

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
mkdir -p /root/.ssh && chmod 700 /root/.ssh
echo 'OPERATOR_PUBLIC_KEY' >> /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
```

If the first line reports no file, run `ssh-keygen -A` and repeat it. The owner sends
the `SHA256:` fingerprint to the operator, who compares it with the key SSH presents
and pins it only on an exact match:

```sh
ssh-keyscan -t ed25519 HOST > scan; ssh-keygen -lf scan
```

Never accept the key from `ssh-keyscan` alone, and never disable host-key checking.
Pin each host key under an alias in its own `known_hosts` file, as
[Pinned aliases](#pinned-aliases) shows.

Then lock the rescue SSH, check it, and lock the random password:

```sh
cat > /etc/ssh/sshd_config.d/00-keyonly.conf <<'EOF'
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PermitRootLogin prohibit-password
EOF
sshd -t && systemctl reload ssh
passwd -l root
```

From the workstation, a fresh key session must succeed and a password attempt must
end in `Permission denied (publickey)`.

## 4. Encrypt the root and install the OS

Inventory the disks first and stop on anything unexpected
([secure recovery media](server-hardening.md#secure-recovery-media-before-installing-linux)):

```sh
lsblk --bytes -o NAME,PATH,TYPE,SIZE,RO,FSTYPE,MOUNTPOINTS,MODEL
lsblk -dno RO /dev/vda      # must print 0
```

The install script expects the target `/dev/vda` with the stock Ubuntu 24.04 cloud
layout: `vda1` root, `vda14` BIOS boot, `vda15` EFI and `vda16` `/boot`. A small
read-only drive is usually the cloud-init configuration, an optical drive the rescue
image and zero-size devices are unattached; none of them is a target. Stop if your
layout differs.

The **owner** encrypts the root in the console. `luksFormat` erases the stock root;
it asks for `YES` and the passphrase twice:

```sh
cryptsetup luksFormat --type luks2 /dev/vda1
cryptsetup open /dev/vda1 cryptroot
```

The operator checks the result (`cryptsetup status cryptroot`: `LUKS2`,
`aes-xts-plain64`, 512-bit key; `cryptsetup luksDump /dev/vda1`: `argon2id`), copies
[`scripts/rescue-luks-install.sh`](https://github.com/yegamble/opsecmkt/blob/main/scripts/rescue-luks-install.sh)
to the rescue system and runs it. The network values come from the provider's console
or metadata and travel on standard input, never as arguments:

```sh
# On the rescue system; each value is read from standard input.
bash /root/rescue-luks-install.sh HOSTNAME 'OPERATOR_PUBLIC_KEY' > /root/install.log 2>&1 <<'EOF'
PUBLIC_ADDRESS/PREFIX
PUBLIC_GATEWAY
PRIVATE_ADDRESS/PREFIX
RESOLVER1,RESOLVER2
UBUNTU_MIRROR_URL
EOF
```

The script refuses to start unless `cryptroot` is an open LUKS2 device on a writable
`/dev/vda` and every network value parses. Then it:

- formats the LUKS root, `/boot` and the EFI partition, and installs Ubuntu 24.04 with
  `debootstrap` (installing `debootstrap` into the rescue system if it is missing);
- writes `fstab`, `crypttab` (`cryptroot UUID=… none luks,discard,initramfs`), a static
  netplan for `eth0` and `eth1`, and the hostname;
- installs the kernel, GRUB, `cryptsetup-initramfs`, `dropbear-initramfs`, OpenSSH,
  chrony and nftables, and masks hibernation;
- configures the pre-boot unlock: dropbear on port 22 accepts only the operator key,
  restricted to `cryptroot-unlock` with no forwarding;
- sets key-only SSH for the installed system and a kernel command line that keeps the
  `eth0`/`eth1` names, gives the initramfs a static address for dropbear and shows the
  unlock prompt on the console;
- checks that the initramfs contains `cryptsetup`, `dropbear` and `crypttab`, prints the
  two host-key fingerprints (installed OS and pre-boot), unmounts and closes LUKS.

Read the whole log. It must end in `ready for disk boot`, show `initramfs has` for all
three files and no `INITRAMFS LACKS`, and print both fingerprints. Record the
fingerprints: they are what the host must present in step 5. If a line says `LACKS`,
inspect the initramfs before booting (`/boot` is not encrypted, so no passphrase is
needed):

```sh
mount -o ro /dev/vda16 /mnt/b
lsinitramfs /mnt/b/initrd.img-* | grep -E 'sbin/cryptsetup$|bin/dropbear$|cryptroot/crypttab$'
```

## 5. First boot and manual unlock

The owner selects the disk boot again; the operator power-cycles the machine. Do not
power-cycle while the install is still running.

The host then waits in its initramfs. Before anyone types a passphrase, the operator
checks that the pre-boot SSH presents the pre-boot fingerprint from step 4 and pins it.
The owner unlocks with the host's passphrase, either in the console at
`Please unlock disk cryptroot:` or over pinned SSH:

```sh
ssh -F PRIVATE/ssh_config HOST-preboot
```

After the unlock the operator checks that the installed OS presents its own recorded
fingerprint, pins it, and confirms on the host:

```sh
findmnt -no SOURCE,FSTYPE /                 # /dev/mapper/cryptroot ext4
cryptsetup isLuks --type luks2 /dev/vda1    # exit 0
swapon --show                               # no output
timedatectl show -p NTPSynchronized         # yes
```

This is the locked cold boot and manual unlock the
[storage assessment](storage-security-assessment.md) requires. If the host does not
come back, nothing is lost: boot the rescue image again, open LUKS and repair.

## Pinned aliases

One address presents three different host keys over its life: the rescue system,
the pre-boot dropbear and the installed OS. Pinning them all against the bare address
in `~/.ssh/known_hosts` produces `REMOTE HOST IDENTIFICATION HAS CHANGED` at every
unlock. Give each its own alias and file instead:

```text
Host HOST-preboot
  HostName ADDRESS
  User root
  IdentityFile ~/.ssh/OPERATOR_KEY
  IdentitiesOnly yes
  UserKnownHostsFile PRIVATE/known_hosts_HOST-preboot
  HostKeyAlias HOST-preboot
  StrictHostKeyChecking yes
  RequestTTY yes

Host HOST
  HostName ADDRESS
  User OPERATOR
  IdentityFile ~/.ssh/OPERATOR_KEY
  IdentitiesOnly yes
  UserKnownHostsFile PRIVATE/known_hosts_HOST
  HostKeyAlias HOST
  StrictHostKeyChecking yes
  PasswordAuthentication no
  KbdInteractiveAuthentication no
```

A changed-key warning on one of these aliases is real: stop and investigate.

## 6. Back up the LUKS header

Take each header backup off the host, check it arrived intact and destroy the server
copy:

```sh
ssh HOST 'umask 077; sudo cryptsetup luksHeaderBackup /dev/vda1 --header-backup-file /root/hdr.img; sudo sha256sum /root/hdr.img'
scp HOST:/root/hdr.img PRIVATE/HOST-luks-header.img && shasum -a 256 PRIVATE/HOST-luks-header.img
ssh HOST 'sudo shred -u /root/hdr.img'
```

The two checksums must match. File the backup in the private record with its
passphrase. A header backup together with its passphrase opens the volume, so guard
it as you guard the passphrase.

## 7. Operator access, core dumps and Docker

These follow [server hardening](server-hardening.md):

1. **Named operator.** Create `opsec` in an `opsecmkt-operators` group with the
   operator key and sudo. The rehearsal used passwordless sudo because the account has
   no password; choose and record your own policy. Open a fresh session as the operator
   and check `sudo -n id -un` before going further.
2. **Key-only operator SSH.** Apply the policy in
   [step 2 of server hardening](server-hardening.md#2-establish-key-only-operator-access)
   (`PermitRootLogin no`, `AllowGroups opsecmkt-operators`, `DisableForwarding yes`),
   run `sshd -t`, reload `ssh.service`, then check a fresh operator session works and
   root is refused with `Permission denied (publickey)`.
3. **No core dumps.** Add `/etc/security/limits.d/99-opsecmkt-no-core.conf` with zero
   soft and hard limits for `*` and `root`, `DefaultLimitCORE=0` in
   `/etc/systemd/system.conf.d/`, and `kernel.core_pattern=|/bin/false` with
   `fs.suid_dumpable=0` in `/etc/sysctl.d/`. A fresh session with multiplexing off
   (`-o ControlMaster=no -o ControlPath=none`) and its `sudo` path must both report
   `(0, 0)` ([verify core limits](server-hardening.md#verify-core-limits-in-fresh-sessions)).
4. **Docker.** The minimal install has no `gpg`, so store Docker's armoured key as a
   `.asc` file, which apt reads directly:

   ```sh
   sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
   echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
     | sudo tee /etc/apt/sources.list.d/docker.list
   sudo apt-get update && sudo apt-get install -y --no-install-recommends \
     docker-ce docker-ce-cli containerd.io docker-compose-plugin
   ```

   Add `LimitCORE=0` drop-ins for `docker.service` and `containerd.service`
   ([service core limits](server-hardening.md#verify-service-and-container-core-limits)),
   restart both and check `Max core file size` is `0 0` in `/proc/MAIN_PID/limits`.
   Docker now manages its own forwarding rules: apply the
   [host nftables policy](server-hardening.md#3-enforce-host-and-provider-allowlists)
   before any container publishes a port. Until then the provider firewall is the
   only port filter.

## 8. Managed PostgreSQL

Create the cluster in the same private network, PostgreSQL 17 (the tested version),
and label it. Once it is online, allow only the application host's private address
as a trusted source. A managed cluster may still have a public endpoint closed only
by these rules; connect to the private host with `sslmode=verify-full` using the
cluster CA from the provider's authenticated API. The database and role steps
continue in [production deployment](production.md); three more details apply:

- **Credentials travel on standard input.** Pipe the provider's connection details and
  the role's password into the application host's `psql` over SSH standard input, and
  read them into environment variables there. Never put either in a command argument,
  even on the workstation: process arguments are visible to other users of that
  machine. Never print the provider's connection details unredirected.
- **A provider admin role may not create a database owned by another role** until it
  is a member of that role. As the admin, run `GRANT market_app TO ADMIN_ROLE;`, then
  `CREATE DATABASE market OWNER market_app ENCODING 'UTF8' TEMPLATE template0;`
  and, connected to `market`, `ALTER SCHEMA public OWNER TO market_app;`.
- **`/srv/opsecmkt-inputs` is root-only (mode `0700`),** so a client running as the
  operator cannot read `database-ca.pem` inside it. Run the read-only check through
  `sudo`, not by loosening the directory. Expect `market|market_app`, `UTF8`, `t`, and
  `market_app` as the owner of schema `public`.

## What this does not protect

- `/boot` and the EFI partition are not encrypted, and there is no measured boot or
  TPM. Someone who can change the boot chain can capture the passphrase at the next
  unlock.
- The hypervisor and anyone with access to it can read live memory and the unlocked
  disk while the host runs. LUKS protects a stopped disk, deleted volumes and
  snapshots, not a running host.
- A managed database is encrypted by the provider, who holds its keys.
- Each host's network is static, set at install. If the provider reassigns an address,
  the host and its pre-boot unlock lose the network until fixed from the rescue image.
- Deleting a machine does not attest erasure of its media.

## Tear down

Delete only what carries your label, and look before deleting: list the machines,
database, firewalls, private network, SSH key and project you recorded, check each
belongs to this work, then delete them one by one, answering every confirmation
yourself. Never delete another project's resources, and never reuse a retired onion
identity. Destroy the header backups and passphrases of deleted hosts per your
record-keeping policy.
