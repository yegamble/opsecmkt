#!/bin/bash
# Install Ubuntu 24.04 onto an opened LUKS2 root from a cloud provider's rescue image.
#
# Run as root inside the rescue system, after the owner has run, in the console:
#   cryptsetup luksFormat --type luks2 /dev/vda1
#   cryptsetup open /dev/vda1 cryptroot
# Usage: rescue-luks-install.sh HOSTNAME 'OPERATOR_SSH_PUBLIC_KEY' < network-values
# Standard input holds five lines: PUBLIC_ADDRESS/PREFIX, PUBLIC_GATEWAY, PRIVATE_ADDRESS/PREFIX,
# RESOLVER1,RESOLVER2 and an Ubuntu mirror URL. They come from the provider's console or
# metadata and never appear as arguments or in the output.
#
# It never reads or stores the passphrase, writes no address to stdout, refuses to run unless
# /dev/mapper/cryptroot is an open LUKS2 device on a writable /dev/vda, and closes LUKS when done.
# It reformats /dev/vda1 (inside LUKS), /dev/vda15 (EFI) and /dev/vda16 (/boot): the stock
# Ubuntu 24.04 cloud layout. Check that layout with lsblk first. See docs/install/encrypted-hosts.md.
set -euo pipefail
HOSTNAME_NEW=${1:?hostname}
PUBKEY=${2:?operator public key}
[ -b /dev/mapper/cryptroot ] || { echo "cryptroot not open"; exit 1; }
[ "$(cryptsetup status cryptroot | awk '/type:/{print $2}')" = LUKS2 ] || { echo "not LUKS2"; exit 1; }
[ "$(lsblk -dno RO /dev/vda | tr -d ' ')" = 0 ] || { echo "vda read-only"; exit 1; }
findmnt -rn /mnt >/dev/null 2>&1 && { echo "/mnt already mounted"; exit 1; }
command -v debootstrap >/dev/null || { DEBIAN_FRONTEND=noninteractive apt-get -qq update >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get -qq install -y debootstrap >/dev/null 2>&1; }
command -v debootstrap >/dev/null || { echo "no debootstrap"; exit 1; }
read -r PUB_CIDR; read -r PUB_GW; read -r PRIV_CIDR; read -r RESOLVERS; read -r MIRROR
valid() { python3 -c "import ipaddress,sys; ipaddress.ip_interface(sys.argv[1])" "$1" 2>/dev/null; }
validip() { python3 -c "import ipaddress,sys; ipaddress.ip_address(sys.argv[1])" "$1" 2>/dev/null; }
if ! { valid "$PUB_CIDR" && valid "$PRIV_CIDR" && validip "$PUB_GW"; }; then echo "network values do not parse"; exit 1; fi
for r in ${RESOLVERS//,/ }; do validip "$r" || { echo "resolver does not parse"; exit 1; }; done
case "$MIRROR" in http://*|https://*) ;; *) echo "mirror is not a URL"; exit 1;; esac
PUB_IP=${PUB_CIDR%/*}
PUB_MASK=$(python3 -c "import ipaddress,sys;print(ipaddress.ip_interface(sys.argv[1]).netmask)" "$PUB_CIDR")
RESOLVER_LIST=$(printf '%s' "$RESOLVERS" | sed 's/,/, /g')
echo "network values read"

mkfs.ext4 -q -F -L root /dev/mapper/cryptroot
mkfs.ext4 -q -F -L boot /dev/vda16
mkfs.vfat -F 32 -n EFI /dev/vda15 >/dev/null
mount /dev/mapper/cryptroot /mnt
mkdir -p /mnt/boot && mount /dev/vda16 /mnt/boot
mkdir -p /mnt/boot/efi && mount /dev/vda15 /mnt/boot/efi
echo "filesystems made and mounted"

debootstrap --arch=amd64 noble /mnt "$MIRROR" >/var/log/debootstrap.out 2>&1 \
  || { tail -5 /var/log/debootstrap.out; exit 1; }
echo "debootstrap done"

cat > /mnt/etc/apt/sources.list <<EOF
deb $MIRROR noble main restricted universe
deb $MIRROR noble-updates main restricted universe
deb http://security.ubuntu.com/ubuntu noble-security main restricted universe
EOF
ROOT_UUID=$(blkid -s UUID -o value /dev/mapper/cryptroot)
LUKS_UUID=$(blkid -s UUID -o value /dev/vda1)
BOOT_UUID=$(blkid -s UUID -o value /dev/vda16)
EFI_UUID=$(blkid -s UUID -o value /dev/vda15)
cat > /mnt/etc/fstab <<EOF
UUID=$ROOT_UUID / ext4 errors=remount-ro 0 1
UUID=$BOOT_UUID /boot ext4 defaults,nodev,nosuid 0 2
UUID=$EFI_UUID /boot/efi vfat umask=0077 0 1
EOF
echo "cryptroot UUID=$LUKS_UUID none luks,discard,initramfs" > /mnt/etc/crypttab
echo "$HOSTNAME_NEW" > /mnt/etc/hostname
printf '127.0.0.1 localhost\n127.0.1.1 %s\n' "$HOSTNAME_NEW" > /mnt/etc/hosts
mkdir -p /mnt/etc/netplan
cat > /mnt/etc/netplan/10-static.yaml <<EOF
network:
  version: 2
  ethernets:
    eth0:
      addresses: [$PUB_CIDR]
      routes: [{to: default, via: $PUB_GW}]
      nameservers: {addresses: [$RESOLVER_LIST]}
    eth1:
      addresses: [$PRIV_CIDR]
EOF
chmod 600 /mnt/etc/netplan/10-static.yaml

for d in dev dev/pts proc sys run; do mount --bind /$d /mnt/$d; done
cat > /mnt/root/inside.sh <<'IN'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get -qq update
apt-get -qq install -y --no-install-recommends linux-image-virtual grub-pc cryptsetup cryptsetup-initramfs \
  dropbear-initramfs openssh-server netplan.io systemd-resolved chrony sudo ca-certificates curl nftables \
  less vim-tiny locales >/var/log/inside-apt.out 2>&1 || { tail -5 /var/log/inside-apt.out; exit 1; }
systemctl enable systemd-networkd systemd-resolved ssh chrony >/dev/null 2>&1
systemctl mask hibernate.target hybrid-sleep.target suspend-then-hibernate.target >/dev/null 2>&1
IN
chroot /mnt bash /root/inside.sh
rm -f /mnt/root/inside.sh
echo "packages installed"

echo "no-port-forwarding,no-agent-forwarding,no-X11-forwarding,command=\"/usr/bin/cryptroot-unlock\" $PUBKEY" \
  > /mnt/etc/dropbear/initramfs/authorized_keys
chmod 600 /mnt/etc/dropbear/initramfs/authorized_keys
echo 'DROPBEAR_OPTIONS="-p 22 -s -j -k -I 300"' > /mnt/etc/dropbear/initramfs/dropbear.conf
install -d -m 700 /mnt/root/.ssh
echo "$PUBKEY" > /mnt/root/.ssh/authorized_keys && chmod 600 /mnt/root/.ssh/authorized_keys
cat > /mnt/etc/ssh/sshd_config.d/00-keyonly.conf <<EOF
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
PermitRootLogin prohibit-password
EOF
sed -i "s|^GRUB_CMDLINE_LINUX=.*|GRUB_CMDLINE_LINUX=\"net.ifnames=0 biosdevname=0 ip=$PUB_IP::$PUB_GW:$PUB_MASK:$HOSTNAME_NEW:eth0:none console=ttyS0 console=tty1\"|" /mnt/etc/default/grub
sed -i 's|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=""|' /mnt/etc/default/grub
grep -q '^GRUB_TIMEOUT=' /mnt/etc/default/grub && sed -i 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=3/' /mnt/etc/default/grub
chroot /mnt bash -c 'update-initramfs -u -k all >/dev/null 2>&1 && grub-install --target=i386-pc /dev/vda >/dev/null 2>&1 && update-grub >/dev/null 2>&1'
echo "initramfs and grub installed"

I=$(find /mnt/boot -maxdepth 1 -name "initrd.img-*" -type f | sort | head -1)
lsinitramfs "$I" | grep 'usr/sbin/cryptsetup$' >/dev/null && echo "initramfs has cryptsetup" || echo "INITRAMFS LACKS cryptsetup"
lsinitramfs "$I" | grep 'usr/sbin/dropbear$' >/dev/null && echo "initramfs has dropbear" || echo "INITRAMFS LACKS dropbear"
lsinitramfs "$I" | grep 'cryptroot/crypttab$' >/dev/null && echo "initramfs has crypttab" || echo "INITRAMFS LACKS crypttab"
chroot /mnt sshd -t && echo "installed sshd config ok"
echo "--- fingerprints (public) ---"
echo -n "installed-os ssh: "; ssh-keygen -lf /mnt/etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2, $NF}'
chroot /mnt dropbearkey -y -f /etc/dropbear/initramfs/dropbear_ed25519_host_key 2>/dev/null | grep '^ssh-' > /tmp/dbk.pub
echo -n "preboot dropbear: "; ssh-keygen -lf /tmp/dbk.pub | awk '{print $2, $NF}'
for d in run sys proc dev/pts dev; do umount /mnt/$d; done
umount /mnt/boot/efi /mnt/boot /mnt
cryptsetup close cryptroot
echo "unmounted and LUKS closed; ready for disk boot"
