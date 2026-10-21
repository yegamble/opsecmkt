# Native application-host bootstrap

Use `host` to prepare a **dedicated Ubuntu 24.04 application host with static
systemd-networkd networking**, then use `deploy` to run the application. The
binary embeds the UI, not PostgreSQL, Tor, Docker or other service executables.
Gateway/scanner, payment, database and maintenance roles remain separate. This
profile permits no public web ingress, including for a Tor storefront.

The commands are explicit administrator actions. Normal `serve` startup neither
changes the firewall nor needs root. `host plan` works without root and exports
reusable files; `packages`, `apply`, `confirm` and `rollback` require Linux root.
No command formats a disk or configures a provider firewall.

## Prepare the host and inventory

Install an encrypted operating-system root and persistent storage following
[server hardening](server-hardening.md), before transferring production secrets.
Keep an independent authenticated console and the previous access configuration.
Verify time synchronization, DNS, static addresses/routes and key fingerprints.
DHCP and unlisted non-Docker interfaces are refused. Routers, VPN, Swarm,
overlay networks and hosts with another enabled firewall manager require their
own reviewed policy; do not disable an existing firewall just to pass this check.

Create a named nonroot operator, install its public SSH key, configure sudo, and
prove a **new** operator SSH session and sudo before restricting access. SSH must
already have the key-only policy documented in
[establish key-only operator access](server-hardening.md#2-establish-key-only-operator-access),
including `AuthenticationMethods publickey`, `DisableForwarding yes` and no root,
password or keyboard-interactive login. Bootstrap checks effective `sshd -T`
settings for inventoried sources; it does not rewrite or reload SSH. Review all
other `Match` cases and socket-activated listener ports separately.

This fictional inventory illustrates the format. Replace every address,
interface, port and account with the actual approved values. Operator sources
must be individual canonical IPv4 `/32` or IPv6 `/128` addresses. The app and
gateway must have distinct private addresses in the same IP family.

```json
{
  "version": 1,
  "os": "ubuntu-24.04",
  "role": "app",
  "operator": "operator",
  "ssh_port": 22,
  "operator_sources": ["192.0.2.10/32", "2001:db8::10/128"],
  "uplinks": ["eth0", "eth1"],
  "app_address": "10.20.0.10",
  "gateway_address": "10.20.0.20",
  "app_ports": [8080, 8443]
}
```

Keep the recipe private operational inventory. It contains no credentials.
Gateway TLS/mTLS, configured app listener addresses and provider rules must agree
with it. A different private-IP family needs a separate implementation/profile;
this version supports one gateway-to-app address pair, not dual-family service
listeners. SSH can use both families; other new IPv4/IPv6 ingress is denied.

```sh
./opsecmkt host plan --config host.json --out host-plan
```

A new mode-0700 directory receives mode-0600 `host.json`, `ingress.nft`,
`security-defaults.conf` and `BOOTSTRAP.txt`. Existing output is refused. Review
those files; apply regenerates rules from the inventory rather than executing an
editable script from that directory.

If dependencies are absent, the optional command below installs Ubuntu's
`docker.io`, `docker-compose-v2`, `nftables`, `openssh-server`,
`unattended-upgrades` and `iproute2` from the existing distribution repositories.
It updates package indexes, downloads packages and may start dependency daemons.
It starts no application. Existing Docker vendor packages may conflict: reconcile
package provenance/version requirements first. This is not a package upgrade or
an unattended reboot command. Configure a supported time-sync client separately.

```sh
sudo ./opsecmkt host packages --config host-plan/host.json
```

## Apply, test a fresh session, confirm

The local Docker engine must be running so bootstrap can identify its real bridge
interfaces. Every other network interface must appear in `uplinks`; a `br-` or
`veth` name alone is insufficient. Ensure required service bindings and static
network configuration match the inventory.

```sh
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host apply \
  --config host-plan/host.json --operator-access-verified
```

The flag attests that fresh operator SSH/sudo and console access were tested.
Apply snapshots its previous table and managed files, retains its executable in
root-only `/var/lib/opsecmkt-host`, arms a five-minute systemd rollback, then
atomically replaces **only** `inet opsecmkt_host`. It never flushes the ruleset or
edits Docker-owned tables. Other chains can still reject permitted traffic.

Open another SSH connection as the configured operator from an approved source.
In that new session, before five minutes elapse:

```sh
sudo --preserve-env=SSH_CONNECTION ./opsecmkt host confirm \
  --config host-plan/host.json
```

Confirmation requires the same inventory/boot, a different SSH connection tuple,
the named sudo operator, and unchanged managed files and live firewall rules.
It verifies operator-supplied session metadata; it is not independent network
attestation. A privileged caller can already change firewall policy directly.
Without confirmation, rollback restores the previous owned table and managed
files. `sudo ./opsecmkt host rollback` performs that restoration immediately;
repeat rollback is harmless. A pending boot also invokes rollback before the
candidate policy and Docker/containerd. The recovery helper/timer definitions
remain installed; without pending state they perform no restoration.

Rollback restores the prior protection level, which can be less restrictive.
It retains established connections and does not undo apt installation, remove
remote resources or restore application data. Package installation and SSH
preparation are separate actions. If rollback cannot restore its protected files
or nftables state, use the recovery console; do not assume a failed command
preserved access. An interrupted apply before the timer is armed changes no
firewall policy, but leaves pending state requiring explicit rollback.

## Installed security policy

- Host INPUT denies new ingress except loopback, required ICMP/IPv6 discovery,
  exact operator SSH addresses, and gateway-to-private-app TCP ports.
- Forwarded new traffic from the inventoried uplinks must match the gateway,
  original private destination and original published TCP port after DNAT.
  Direct routing to a container and other new uplink ingress is denied.
- Egress is retained for service dependencies and Tor circuits. This is not an
  egress allowlist or an anonymity guarantee. The public Tor gateway and private
  administrator onion remain independently configured and authenticated.
- A systemd boot helper loads the owned policy before Docker/containerd and SSH service/socket startup.
  Missing or invalid policy fails the helper and blocks those units rather than skipping protection; recover through the independent console.
- Persistent sysctl defaults disable setuid dumps, restrict kernel pointers and
  disable IPv4/IPv6 redirect acceptance and IPv4 redirect sending. Coredump
  configuration disables systemd-coredump storage and processing. These files
  apply to subsequent boots/processes; apply does not change live sysctls or
  retroactively remove prior dumps. Inspect other configuration precedence.
- Ubuntu's periodic security-update mechanism is enabled with automatic reboot
  disabled. Preserve the separately reviewed boot/kernel/update policy and
  attended encrypted-root unlock procedure for the deployment.

Reapplying is explicit and stages another confirmation/rollback window; a pending
apply must finish before another starts. Host mutations take a process lock.
Existing conntrack sessions survive allowlist removal. For urgent revocation,
follow the narrowly scoped session procedure in the hardening guide.

Before production use, verify fresh allowed and denied public/private-source
connections, every assigned IPv4/IPv6 address, actual Docker-published listeners,
gateway TLS/mTLS, public Tor and authorized/unauthorized administrator access.
Rehearse timer rollback and a locked reboot on a disposable host first. Unit and
isolated container checks do not establish actual SSH reachability, systemd boot
ordering, disk unlock or a production Tor service.

## CI/CD and replacement

Keep the versioned host recipe and release/image digest under deployment review;
keep secrets in protected files or an encrypted recovery bundle. Automation can
run `plan` and `apply`; arrange the separate fresh SSH confirmation within its
window. Do not hide confirmation failures or disable rollback in CI.

Follow [application configuration recovery](recovery.md) to export/restore retained
keys and identities. Prepare new storage and host policy first, fence the old
application, restore retained configuration and current external retention state,
update only changed local addresses/certificates deliberately, explicitly rebind
storage, then run `deploy plan` and `deploy install`. The database and other
workers continue independently. A new image still needs migration compatibility
review and coordinated backups; changing an image digest cannot undo a schema
migration.

## Automated packet-policy verification

With Linux root, Python 3, nftables and iproute2 installed, run
`python3 scripts/test-host-bootstrap.py /absolute/path/to/opsecmkt` from a source
checkout. The helper always enters a new network namespace before changing any
addresses or rules. It exercises allowed/denied IPv4/IPv6 INPUT, gateway-only
DNAT forwarding, direct-route denial and retention of an unrelated nftables table.
The required CI packet-policy job runs this fixture. It does not run host apply,
prove Docker's own rules, authenticate SSH or boot systemd.
