# Hardening servers and restricting access

Use this procedure before starting a production instance, then repeat its checks after changes. It covers administrator-approved source addresses, Tor-only storefront access and encrypted server storage.

This is a target configuration. It does not attest that an existing deployment meets it. Record measured results and remaining gaps in your private deployment record; publish only a sanitized account.

The [existing deployment security review](security-review.md) and [storage assessment](storage-security-assessment.md) record current measurements. Healthy services, encrypted logical backups and provider network rules do not establish compliance with this procedure.

## Before starting

You need host administrator access, permission to change infrastructure firewalls and a trusted recovery workstation. Keep an authenticated recovery console available throughout access changes. Review the [production procedure](production.md), [service topology](service-topology.md) and [security boundaries](../security-operations.md).

Prepare these items before enforcing restrictions:

- A named infrastructure operator, provider-account MFA and a working SSH key. A marketplace administrator account does not automatically authorize cloud or host firewall changes.
- A private inventory of approved operator IPv4 `/32` and IPv6 `/128` addresses, exact service addresses, interfaces, ports and certificate names. Give every entry an owner and review date.
- A second approved operator connection or recovery console, plus the previous firewall and SSH configurations and a tested rollback procedure.
- Verified encrypted backups, an isolated restore and offline recovery keys before any storage migration or restart.

### Verify clock synchronization

Install one supported time-sync service and enable it at boot. Minimal OS images
may omit it. Correct time is required for TLS certificates, Tor and administrator
authenticator codes.

On a minimal Ubuntu host with no time-sync client already installed:

```sh
sudo apt-get install --no-install-recommends systemd-timesyncd
sudo timedatectl set-ntp true
sudo systemctl enable --now systemd-timesyncd
systemctl is-enabled systemd-timesyncd
systemctl is-active systemd-timesyncd
timedatectl show --property=NTP --property=NTPSynchronized
```

Expect `enabled`, `active`, then `NTP=yes` and `NTPSynchronized=yes` after
synchronization completes. A running client alone does not prove synchronization.
If either stays `no`, check the service, DNS and outbound access to your approved
time sources. A client using NTP needs outbound UDP `123` and response traffic;
it does not need a public inbound NTP listener. Repeat the check after a cold boot.
Use your distribution's equivalent procedure if another client is already in
use. See [Ubuntu's time synchronization instructions](https://ubuntu.com/server/docs/how-to/networking/timedatectl-and-timesyncd/).

## 1. Record the required connections

For each source, approve only the destination, protocol and port it needs. A private network is not an authorization boundary by itself. Prefer individual service addresses; use a dedicated CIDR only when every address in it is trusted for that connection. A shared project tag, whole private network or unrestricted IPv6 range can admit future unrelated hosts.

| Destination | Approved source | Allowed connection |
| --- | --- | --- |
| SSH on each server | Operator-approved public IPv4 `/32` and IPv6 `/128`, or a separately authenticated management network | TCP `22`, or the recorded SSH port |
| App ordinary handler | Gateway private address only | Verified HTTPS, example `8080` |
| App upload backend | Gateway private address only | Mutual TLS, example `8443` |
| Managed PostgreSQL | App private address and explicitly approved backup/recovery sources | Verified PostgreSQL TLS on the provider's **actual** service port |
| Payment HTTPS RPC front ends | App private address only | Authenticated TLS, examples `18443` and `18444`; confirm the wallet/daemon mapping in your configuration |
| Raw wallet/daemon RPC | Required same-host processes only | Loopback or an unpublished service network; never a public listener |
| ClamAV | Gateway process only | Loopback `3310` or an unpublished scanner network |

The [complete flow matrix](service-topology.md#allowing-only-required-service-flows) covers peer traffic, version reports and report delivery. Approved backup access must not silently grant the gateway or a maintenance worker a database login.

For a **Tor-only storefront**, expose no marketplace HTTP or HTTPS listener on any public IPv4 or IPv6 address. Bind the public Tor target to gateway loopback. Keep app listeners on their private addresses, protected by the gateway-only rules. Tor makes outbound connections to build onion-service circuits; a normal onion service needs no public inbound web port.

The public onion remains available to Tor visitors. The service cannot enforce an original visitor-IP allowlist: a local Tor connection or relay address is not the visitor's address. Protect the separate administrator onion with client authorization, application sign-in and role checks. Never treat forwarded headers as operator identity. See the [Tor Project overview](https://community.torproject.org/onion-services/overview/index.html) and [client authorization reference](https://community.torproject.org/onion-services/advanced/client-auth/).

## 2. Establish key-only operator access

Do this before removing an existing root login or tightening its source-address rule.

1. Create a named, non-root operator using your distribution's account tools. Install the operator's public SSH key and the required sudo policy. Keep private SSH keys on the workstation.
2. Open a **new** SSH session as that operator from an approved address. Verify the server's independently recorded host-key fingerprint and confirm that required sudo commands work.
3. Save the existing SSH configuration and recovery instructions privately. Keep the original session and recovery console open.
4. Configure key-only access using the distribution's supported SSH configuration files. The policy below assumes a Linux OpenSSH server with PAM and an existing `opsecmkt-operators` group containing the verified operator.

   ```text
   PubkeyAuthentication yes
   AuthenticationMethods publickey
   PasswordAuthentication no
   KbdInteractiveAuthentication no
   PermitEmptyPasswords no
   PermitRootLogin no
   UsePAM yes
   AllowGroups opsecmkt-operators
   DisableForwarding yes
   X11Forwarding no
   ```

   `UsePAM yes` retains PAM account/session checks; keyboard-interactive and password authentication remain disabled. Allow forwarding only through a separately reviewed requirement. Do not add the operator to the Docker group unless that root-equivalent authority is required.
5. Validate syntax before reloading:

   ```sh
   sudo /usr/sbin/sshd -t
   ```

   Expect no output and exit status zero. Inspect effective settings with `sshd -T`, including `-C` for each relevant `Match` case. Included files and first-value precedence can override the intended policy.
6. Reload the actual SSH unit. For Ubuntu/Debian installations using `ssh.service`:

   ```sh
   sudo systemctl reload ssh.service
   ```

   Use `sshd.service` only on distributions that provide that unit. Open another fresh operator session and verify sudo before closing the old session or disabling any remaining root access path.

Do not apply `PermitRootLogin no` to a root-only deployment before step 2 succeeds. Use the [OpenSSH configuration reference](https://man.openbsd.org/sshd_config) and your distribution's [OpenSSH instructions](https://ubuntu.com/server/docs/openssh-server/) to check supported directives and service behavior.

## 3. Enforce host and provider allowlists

Use one host firewall manager and its persistent configuration. Apply the same approved connection inventory to the provider firewall and managed-database trusted-source rules. Configure **default-deny inbound** for IPv4 and IPv6 on every interface. Preserve loopback, established response traffic and required network control traffic such as IPv6 neighbor discovery and path-MTU discovery.

1. Record the host's addresses, listeners and Docker mappings. These commands inspect state; they do not install rules:

   ```sh
   ip -brief address
   sudo ss -lntup
   sudo docker ps --format 'table {{.Names}}\t{{.Ports}}'
   ```
2. Export existing host/provider rules to a private location. Prepare a distribution-specific rollback that restores the working configuration. Test console access before making changes; a still-open SSH session alone cannot recover every mistake.
3. Add the exact approved operator SSH addresses and service flows before adding deny rules. Include IPv6 explicitly. If a service does not need IPv6, remove its IPv6 listener and verify rejection rather than leaving it outside the policy.
4. Apply the host restrictions, then the provider restrictions, testing a fresh approved session after each change. Persist rules with the supported firewall manager. Keep the recovery path available until denied-source and service tests pass.
5. Restrict managed PostgreSQL separately. Its private endpoint, TLS, database role and trusted-source list must all agree. Permit approved recovery access temporarily when practical, remove it afterward and record the change.

Never add `0.0.0.0/0` or `::/0` as an SSH recovery shortcut. Use the authenticated console to correct the approved source or revert the failed change.

### Account for Docker's forwarding rules

A host `INPUT` policy alone does not protect bridge-network published ports. Docker can route that traffic around UFW's ordinary host rules. Keep only required bindings, then enforce the source policy at the appropriate forwarding hook as well as the provider boundary. See [Docker's firewall behavior](https://docs.docker.com/engine/network/packet-filtering-firewalls/).

- With Docker's **iptables backend**, place reviewed restrictions in `DOCKER-USER`, before Docker's accept rules. Match the correct interface and destination; destination NAT has already translated the published address/port. Preserve required established traffic. See [Docker with iptables](https://docs.docker.com/engine/network/firewall-iptables/).
- With Docker's **native nftables backend**, there is no `DOCKER-USER` chain. Use a separate table/base chain with the documented hook and priority. Do not modify Docker-owned tables. See [Docker with nftables](https://docs.docker.com/engine/network/firewall-nftables/).
- `iptables-nft` compatibility tools do not, by themselves, mean Docker uses its native nftables backend. Inspect daemon configuration and actual rules. Host-network containers use the host listener policy instead.

Do not disable Docker's firewall management as a shortcut. Review the installed Docker version, backend and forwarding policy, and repeat checks after container recreation or a planned Docker restart. This guide deliberately avoids a universal firewall script: interface names, NAT, IPv6, boot persistence and management routes must match the host.

### Stage an explicit nftables ingress policy

The following **application-host candidate** uses its own table. It does not flush existing rules or edit Docker tables. Adapt it on a workstation or disposable host first. Every address and interface below is fictional: replace `192.0.2.10/32` with the operator's approved observed source, `10.20.0.10` with the app's private address, `10.20.0.20` with the gateway's private address, and `eth0`/`eth1` with **all** interfaces through which remote traffic can arrive.

The empty `admin6` set permits no new IPv6 SSH connection. Add only approved IPv6 `/128` elements if required. Add narrowly scoped DHCP renewal rules if the host uses DHCP; this candidate assumes static addressing. It is not a router, VPN, Swarm or overlay-network configuration.

```nft
define UPLINKS = { "eth0", "eth1" }
define APP_IPV4 = 10.20.0.10
define GATEWAY_IPV4 = 10.20.0.20

table inet opsecmkt_ingress {
    set admin4 {
        type ipv4_addr
        flags interval
        elements = { 192.0.2.10/32 }
    }
    set admin6 {
        type ipv6_addr
        flags interval
    }
    chain input {
        type filter hook input priority -10; policy drop;
        ct state invalid drop
        iifname "lo" accept
        ct state established,related accept
        ip protocol icmp icmp type { destination-unreachable, time-exceeded, parameter-problem } accept
        meta l4proto ipv6-icmp icmpv6 type { destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept
        ip6 hoplimit 255 icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
        ip saddr @admin4 tcp dport 22 counter accept
        ip6 saddr @admin6 tcp dport 22 counter accept
        ip saddr $GATEWAY_IPV4 ip daddr $APP_IPV4 tcp dport { 8080, 8443 } counter accept
        counter drop
    }
    chain forward {
        type filter hook forward priority -10; policy accept;
        ct state invalid drop
        ct state established,related accept
        iifname $UPLINKS jump inbound_publish
    }
    chain inbound_publish {
        meta l4proto tcp ip saddr $GATEWAY_IPV4 ct status dnat ct original ip daddr $APP_IPV4 ct original proto-dst { 8080, 8443 } counter accept
        counter drop
    }
}
```

`input` protects host listeners. `forward` runs before Docker's usual priority-zero filter chains. New traffic entering through the inventoried uplinks must match the explicit source, original private destination and published port after destination NAT, or it is dropped. Direct routing to a container does not satisfy the DNAT rule. An early `accept` still allows later Docker/security chains to reject the packet. Container-originated egress is outside this candidate and must be restricted separately under step 7.

The established-connection rules preserve existing sessions. Removing an address from an allowlist does not immediately revoke a connection already tracked as established. For urgent revocation, first verify console access and identify the exact source, destination, ports and affected session. Place a narrowly scoped deny **before** established acceptance in the relevant host/forward path, then terminate only the identified sessions and verify rejection from the revoked source. Do not flush global connection tracking or terminate the only working operator connection.

Create a separate candidate per host; do not reuse the app's service rules unchanged:

| Role | Required change to the candidate |
| --- | --- |
| Application | Use the app/gateway addresses and actual `8080`/`8443` bindings shown above. |
| Gateway/scanner, Tor-only | Remove the gateway-to-app `input` allow and the allow inside `inbound_publish`. Retain the final drops. Tor targets gateway/scanner loopback; no new public web connection is allowed. |
| Payments | Define the payment private address. Replace the service `input` rule with app-source to payment-destination TCP `18443`/`18444`. Replace the forwarding allow with the same app source, original payment destination and published RPC ports. Keep raw RPC on loopback/unpublished networks. |
| Maintenance worker | Remove both app service allows. Keep only the approved SSH rule and required network control traffic. |

For example, a payment host at `10.20.0.30` adds this definition:

```nft
define PAYMENTS_IPV4 = 10.20.0.30
```

Its service rule in `input` becomes:

```nft
ip saddr $APP_IPV4 ip daddr $PAYMENTS_IPV4 tcp dport { 18443, 18444 } counter accept
```

Its allow in `inbound_publish` becomes:

```nft
meta l4proto tcp ip saddr $APP_IPV4 ct status dnat ct original ip daddr $PAYMENTS_IPV4 ct original proto-dst { 18443, 18444 } counter accept
```

If a service uses IPv6, add its exact approved source, intended destination and port explicitly to both relevant paths before their terminal drop. Do not add a blanket IPv6 accept. Verify actual source addresses after any gateway NAT; certificate authentication remains required even with an IP allowlist.

Stage the reviewed app candidate as `/root/opsecmkt-ingress.reviewed.nft`, mode `0600`. Confirm `opsecmkt_ingress` does not already exist, that the nft binary path matches the host and that no other firewall manager will flush it. Validate **without applying**:

```sh
sudo /usr/sbin/nft --check --file /root/opsecmkt-ingress.reviewed.nft
```

Expect exit status zero. A syntax pass does not establish packet behavior. Test the rules with representative allowed/denied traffic in a disposable environment before the operator schedules live enforcement. The [`nft` reference](https://netfilter.org/projects/nftables/manpage.html) documents check mode and rule evaluation.

At a reviewed first installation, keep the recovery console and second session open. Schedule a rollback that removes **only this new table** before loading it:

```sh
sudo systemd-run --unit=opsecmkt-ingress-rollback --on-active=5m \
  /usr/sbin/nft delete table inet opsecmkt_ingress
sudo /usr/sbin/nft --file /root/opsecmkt-ingress.reviewed.nft
```

These are manual enforcement commands, not checks. Run them only after replacing the fictional inputs, reviewing the complete rules and confirming the provider perimeter remains in force. They are unsuitable when replacing an existing table; that requires an atomic replacement and restoration of its previous contents.

Complete fresh-session, service and unauthorized-source tests during the rollback window. If they pass, cancel the rollback timer:

```sh
sudo systemctl stop opsecmkt-ingress-rollback.timer
```

Persist this table through a reviewed distribution-specific loader that preserves other tables, starts before services and fails closed on error. Do not enable a default nftables configuration containing `flush ruleset` alongside Docker. Repeat the checks after an approved reboot before declaring persistence verified. The rollback returns to the prior host policy; it does not create a secure replacement firewall.

## 4. Verify allowed and denied connections

Run probes only against your own approved infrastructure. Record the source address, destination, port, expected result and actual result privately.

| Test location | Required result |
| --- | --- |
| Approved operator address | A **fresh** direct SSH handshake authenticates the named operator; password and direct root login are refused |
| Independent unapproved public address | SSH is rejected or times out; all marketplace HTTP/HTTPS, app/backend, database, scanner and RPC public ports are unreachable |
| Gateway private address | App HTTPS `/healthz` succeeds with certificate/hostname verification; backend mutual TLS works only with the gateway identity |
| Unapproved private host | App `8080`/`8443`, payment HTTPS RPC and managed database refuse the connection |
| App private address | Database TLS/authentication succeeds; required payment TLS/authentication succeeds when payments are configured |
| Separate Tor clients | Public onion works; admin onion requires its authorization key and application sign-in; protected public routes return `404` |

Test every assigned public IPv4/IPv6 address, each role's actual ports and each alternate listener. A missing IPv6 test is a pending check, not a pass.

Use a direct probe with proxy variables, SSH proxy settings and local forwarding understood. Confirm an actual remote SSH banner, verified TLS exchange or application response. A local TCP connection accepted by a proxy does not prove that the remote port is reachable; a failure caused by local DNS or proxy setup does not prove it is blocked. Correlate uncertain results with server-side packet counters or a short, narrowly scoped packet capture that avoids application payload collection.

For verified application HTTPS, replace `APP_HOST`, `APP_PRIVATE_IPV4` and the public CA path, then run **from the gateway**:

```sh
curl --noproxy '*' --fail --show-error --connect-timeout 5 \
  --cacert /etc/opsecmkt-gateway/app-ca.pem \
  --resolve APP_HOST:8080:APP_PRIVATE_IPV4 \
  https://APP_HOST:8080/healthz
```

Expect `ok`. Repeat from an unauthorized private source: it must not complete the remote TLS exchange. Do not use `--insecure` to turn a certificate failure into a passing result. Use the database check in the [production guide](production.md#2-create-and-verify-managed-postgresql) for database authorization.

## 5. Choose storage encryption for the threat

Disk encryption protects stored bytes while their keys are unavailable. A running, unlocked system still exposes keys and plaintext to authorized processes, root and a sufficiently privileged adversary.

| Storage/control | Protection and remaining limits |
| --- | --- |
| Provider encryption at rest | Protects media within the provider's documented service boundary. The provider operates the service and its keys; this does not exclude provider access to a running guest or database. |
| Guest-managed LUKS2 with an offline unlock secret | Protects a copied guest disk while locked, provided sensitive data and unlock material are covered. An unlocked guest exposes plaintext and keys to root; a normal VM does not exclude its hypervisor administrator. |
| Owner-controlled physical server with encrypted data disks | Protects locked disks. Boot tampering, memory acquisition and physical access to an unlocked system require additional controls. Verify boot integrity, firmware/boot-chain policy, console security and custody separately. |
| Managed PostgreSQL | Use the provider's documented encryption at rest, encrypted backups, access controls and verified TLS. You cannot configure guest LUKS inside a managed database service. Provider-held keys remain a trust boundary. |

For a physical-access threat, prefer a fresh Linux installation with LUKS2 covering the data-bearing root and state volumes. Keep a strong unlock secret off the server and its snapshots. Plan manual unlock after restart; an unattended plaintext keyfile on the same disk defeats protection against copying that disk. A provider-controlled key service or TPM auto-unlock has different boot and access assumptions and needs a separate review.

Use your distribution's supported encrypted-installation procedure. The [Debian cryptsetup documentation](https://cryptsetup-team.pages.debian.net/cryptsetup/) explains its boot integration. Do not run a generic `luksFormat` command against an existing server, root disk or mounted filesystem.

### Prepare encryption before running setup

For a new production deployment, complete these steps before credentials, onion
identities or application keys reach the server:

1. Install Linux with a LUKS2-encrypted operating-system root using the
   distribution's supported installer or a separately tested encrypted image.
   Record the unencrypted boot/firmware boundary. A copied encrypted image can
   share its volume key even after its passphrase changes; prepare a distinct
   LUKS volume key for each host.
2. Keep the unlock secret and protected header backup outside the server and
   its snapshots. Test the authenticated operator unlock route and independent
   recovery access. Do not embed the unlock secret in the boot image or
   cloud-init data. Prefer an interactive unlock prompt; scripted input must
   preserve the exact key bytes without an added newline. Follow the
   [manual-unlock retry and byte-handling guidance](../operator-guide.md#recovering-after-a-full-host-reboot).
3. Install Docker and a verified, persistent root-owned native binary on the
   encrypted systemd host, for example `/usr/local/bin/opsecmkt`. Resolve Docker's
   actual data root and containerd storage. Place any separate data filesystem
   on LUKS2 and disable active swap before restoring or generating private state.
4. Verify root, persistent paths and private input files as described below.
   Run `deploy init --mode prod` on that Linux host against its local Docker
   Engine. It checks storage before staging configuration or generating keys
   and records the expected LUKS identities.
5. Review the automatic Docker/containerd checks installed by production starts
   and add native-service dependencies before enabling automatic startup.
   Complete a locked boot, correct unlock and isolated restore rehearsal before
   accepting user data or payments.

The native Go controller checks production storage; it does not install an
encrypted operating system or convert an existing root. Its explicit
`deploy storage prepare` command prepares **one additional new, blank whole data
device**. It refuses root, mounted, used or partitioned devices. Development
deployments retain their existing storage behavior. Do not select development
mode to bypass production storage checks.

The production operator installation must remain on persistent encrypted
storage. The earlier RAM-unlocked production directory workflow is not accepted
by the new production start commands: boot checks need the retained configuration
after root unlock. Development RAM workflows remain available. Supporting a
RAM-only production controller needs a separate verified boot/unlock design.

### Secure recovery media before installing Linux

A rescue or recovery image is a separate operating environment. It can have
different SSH host keys and enable password login even when the installed OS
uses keys only. Complete this sequence before installing an encrypted replacement:

1. Use the authenticated out-of-band console to read the recovery environment's
   SSH host-key fingerprint. Compare it with the key presented by the SSH client
   before accepting a changed host identity. Keep the previous installed-system
   fingerprint separately; do not use disabled host-key checking or an unverified
   `ssh-keyscan` result as proof of identity.
2. Through the trusted console or an already verified session, install the
   operator's **existing approved public key** in the recovery account's
   `authorized_keys`, preserving any reviewed entries. Keep its private key on
   the workstation. Verify ownership and permissions (`0700` for `.ssh`,
   `0600` for `authorized_keys`), then open a fresh public-key session from the
   approved operator source.
3. Disable recovery password and keyboard-interactive authentication. Apply
   `PubkeyAuthentication yes`, `AuthenticationMethods publickey`,
   `PasswordAuthentication no`, `KbdInteractiveAuthentication no` and
   `PermitEmptyPasswords no` in the recovery environment's effective SSH policy.
   If recovery requires root, use `PermitRootLogin prohibit-password` until a
   verified named operator is available; do not disable the only working account.
   Apply the normal [key-only operator policy](#2-establish-key-only-operator-access)
   to the installed system afterwards.
4. Validate the recovery daemon with `sshd -t` and inspect `sshd -T`, including
   relevant `Match` cases and image-supplied configuration. Reload its actual
   SSH service, then verify another fresh public-key login before closing the
   original session. Confirm password and keyboard-interactive login are
   refused. Keep the trusted console available throughout.
5. Inspect block devices before selecting an install target:

   ```sh
   lsblk --bytes -o NAME,PATH,TYPE,SIZE,RO,FSTYPE,MOUNTPOINTS,MODEL,SERIAL
   findmnt
   ls -l /dev/disk/by-id
   ```

   Identify read-only configuration/metadata drives and zero-size entries that
   represent unattached devices or empty media. Record their role and why each
   is not an install candidate. Independently verify the intended replacement
   disk's stable identity, nonzero capacity, writable state and reviewed
   partition plan. Stop on an unexpected or ambiguous device; never weaken
   target checks, remove the positive-capacity requirement or treat a failed
   inventory check as permission to format another disk.
6. Start the reviewed encrypted installation only after access and target
   checks pass. Recovery SSH policy does not automatically configure the new
   OS. Reverify its host identity, key-only access and encrypted-root checks,
   then complete a real locked boot/manual unlock before transferring live
   state or declaring provider boot verified.

Check the installed OS's persistent network configuration after a cold boot.
An early-boot profile can keep an address or DNS working while masking a missing
or conflicting normal-OS configuration. Inspect which Netplan/networkd profile
actually owns each interface, its MAC match, runtime-generated profiles and
per-link DNS/default routing. Resolve confirmed stale early-boot profiles with
console access available, then verify normal-OS DNS and fresh signed package
indexes. Make the package check fail on repository errors; cached indexes alone
do not establish a successful refresh.

If an initramfs unlock helper calls a checksum tool, explicitly include that
executable and its required libraries in the built initramfs. Inspect the archive
and exercise the helper during a locked early boot. A checksum applet available
in the normal OS's BusyBox does not prove the initramfs contains a compatible
binary or its dependencies. Rebuild and verify the booted initramfs before
repeating the attended unlock test.

### Cover every sensitive path

Inventory the real backing storage, including named volumes and bind mounts:

- Native installation/input directories, runtime credentials, TLS keys, audit keys and any retained plaintext recovery material.
- Docker's actual data root, containerd's separate storage, and named volumes, including public and administrator onion identities. RAM unlock leaves Tor identity volumes persistent.
- Payment wallet files, passwords, wallet caches, RPC identities and recovery data. Keep the stable wallet secret and database pins through migrations.
- External retention ledger, database dumps, backup staging, exports and temporary files.
- Swap, hibernation images, crash/core dumps, persistent journals and container logs. Disable unnecessary dumps; protect any retained diagnostics.
- Snapshots and old disks. New encryption does not retroactively encrypt historical copies.

tmpfs can swap to disk. Native production checks require **no active swap**,
including encrypted swap. Disable it persistently and account for hibernation
and memory snapshots separately. Other deployment designs need their own review
before using encrypted swap. See the [Linux tmpfs reference](https://docs.kernel.org/filesystems/tmpfs.html)
and [RAM secret limits](ram-secrets-and-emergency.md).

Establish a crash-dump policy at every layer: host crash interception, service limits and container limits. `LimitCORE=0` and Docker core ulimits alone do **not** prevent collection by a piped handler such as Apport. Configure or disable that handler as required, inspect effective limits on running processes and validate behavior with a synthetic process on a disposable host. Never generate a production secret-bearing dump as a test. See [Ubuntu's Apport behavior](https://documentation.ubuntu.com/project/contributors/debugging/apport/) and [Linux core dump handling](https://man7.org/linux/man-pages/man5/core.5.html).

### Verify core limits in fresh sessions

Check both soft and hard core limits for operator sessions and running services.
A zero soft limit with an unlimited hard limit is not the intended `(0, 0)`
policy. An SSH service's `LimitCORE` settings alone do not establish the limits
of a login session after PAM processing.

For Linux-PAM, configure a reviewed root-owned file such as
`/etc/security/limits.d/99-opsecmkt-no-core.conf`:

```text
* soft core 0
* hard core 0
root soft core 0
root hard core 0
```

Wildcard and group limits exclude root, so include the literal `root` entries.
Review existing user-specific rules and any alternate `conf=` file selected by
the module. See the [PAM limits reference](https://manpages.debian.org/bookworm/libpam-modules/limits.conf.5.en.html).

Verify effective `UsePAM yes` and that the SSH PAM **session** stack, including
its distribution-managed includes, invokes `pam_limits.so`. Keep password and
keyboard-interactive authentication disabled. Review the stack before changing
it; an incorrect PAM policy can prevent operator access. Keep the recovery
console and original session available. See [pam_limits session configuration](https://man7.org/linux/man-pages/man8/pam_limits.8.html)
and [portable OpenSSH's PAM behavior](https://manpages.ubuntu.com/manpages/noble/man5/sshd_config.5.html).

Open a new connection using the existing approved account and pinned host.
Disable connection multiplexing so an old SSH master cannot supply the session.
With Python 3 installed, replace the account/host placeholder and run:

```sh
ssh -o ControlMaster=no -o ControlPath=none -o ControlPersist=no \
  -o StrictHostKeyChecking=yes OPERATOR@YOUR_HOST \
  'python3 -c "import resource; print(resource.getrlimit(resource.RLIMIT_CORE))"'
```

Expect `(0, 0)`. Repeat for each allowed login class and the actual sudo path
used for privileged work. Existing sessions retain their prior limits. For
services and containers, also inspect `/proc/PID/limits` for their real running
processes; unit configuration is not a substitute for process measurements.
Host-wide dump interception, `fs.suid_dumpable` and container limits remain
separate controls. The initramfs/preboot SSH environment has its own process and
crash policy; normal-OS PAM and systemd checks do not verify that earlier phase.

### Verify service and container core limits

A manager-wide `DefaultLimitCORE=0` only supplies a default. A packaged service
can explicitly set `LimitCORE=infinity` and override it. Inspect the effective
Docker and containerd units, including their drop-ins:

```sh
sudo systemctl cat docker.service containerd.service
sudo systemctl show docker.service containerd.service \
  --property=LimitCORE --property=LimitCORESoft --property=MainPID
```

For the reviewed no-core policy, create a root-owned
`60-opsecmkt-core-limits.conf` in each applicable service's
`/etc/systemd/system/SERVICE_NAME.service.d/` directory:

```ini
[Service]
LimitCORE=0
```

This single value sets both limits to zero. Reload systemd configuration and
restart the affected services during a planned maintenance window; running
processes retain their old limits until replaced. Recheck the effective unit
and `/proc/MAIN_PID/limits` using each service's current `MainPID`. Require zero
soft and hard values for `Max core file size`. See the
[systemd resource-limit reference](https://manpages.debian.org/trixie/systemd/systemd.exec.5.en.html).

Check pre-start helpers separately. The `+` executable prefix runs a command
with full privileges, so do not infer a helper's restrictions from its main
service. On the measured system, a `+`-prefixed `ExecStartPre` also bypassed the
unit's core limits. A privileged storage guard must set and verify its own soft
and hard core limits as zero before processing arguments, configuration or
storage inputs, and refuse startup if that fails. Omit `+` for guards already
running as root; retain it only for a reviewed root storage check attached to a
non-root service. Verify actual service startup, including the helper, rather
than checking the unit text alone. See
[systemd's executable prefixes](https://manpages.debian.org/trixie/systemd/systemd.service.5.en.html#COMMAND_LINES).

Configure container limits separately. Merge this fragment into the existing
Docker daemon configuration without replacing other settings:

```json
{
  "default-ulimits": {
    "core": {"Name": "core", "Soft": 0, "Hard": 0}
  }
}
```

Validate the merged file with `dockerd --validate --config-file
/etc/docker/daemon.json` before the planned daemon restart. Review explicit
container or Compose ulimits, which can override daemon defaults. Check the
actual process limits in a newly created disposable container using an approved
digest-pinned image without secrets or network access, then verify deployed
workloads. A valid daemon configuration or `docker inspect` result alone does
not prove running container limits. See
[Docker's default ulimit settings](https://docs.docker.com/reference/cli/dockerd/#default-ulimit-settings).

### Reduce RAM exposure and cold-boot risk

An unlocked service needs usable keys in memory. Application code cannot make
that live memory unreadable to every physical or privileged attacker. Review
three layers before claiming protection:

1. **Hardware and hosting:** verify active memory encryption and, where needed,
   an attested confidential VM with a threat model matching the adversary.
   A CPU feature flag alone does not prove encryption is active; see
   [Linux's memory-encryption states](https://www.kernel.org/doc/html/v6.11/arch/x86/amd-memory-encryption.html).
   Maintain firmware and boot integrity, and verify physical-access exclusions.
   For example, AMD describes a
   [physical motherboard attack outside SEV-SNP's threat model](https://www.amd.com/en/resources/product-security/bulletin/amd-sb-3024.html)
   and a separate
   [SEV-SNP firmware issue requiring platform updates](https://www.amd.com/en/resources/product-security/bulletin/amd-sb-3034.html).
   Confidential computing reduces specific risks; it is not a blanket guarantee
   against physical access.
2. **Host policy:** persistently disable swap, hibernation, unnecessary Apport or
   systemd-coredump collection, kernel crash dumps and VM memory snapshots.
   Verify effective settings after boot and the provider's snapshot policy.
   [Kdump can export a system memory image](https://cdn.kernel.org/doc/html/latest/admin-guide/kdump/kdump.html),
   so process core limits alone are insufficient. These controls reduce copies
   of RAM written to storage; they do not prevent acquisition of live RAM.
3. **Application design:** keep recovery private keys offline, minimize online
   secret lifetime and copies, and isolate the wallet behind its restricted RPC
   boundary. Clearing one Go buffer does not prove that strings, libraries or
   runtime copies have also been erased; garbage collection is not a secure
   erasure guarantee. [Memory locking](https://man7.org/linux/man-pages/man2/mlock.2.html)
   prevents paging of selected pages, but does not prevent physical RAM access
   or hibernation images.

An ordinary reboot, including the private admin reboot control, does not
guarantee RAM sanitization. It does not erase disk keys or provide automatic
destruction. The [recorded storage assessment](storage-security-assessment.md)
does not establish hardware memory encryption or confidential-VM protection for
the current deployment; it also records Apport enabled on all three hosts.
The controls above are required review work, not a claim that those live settings
have already changed.

## 6. Verify encryption before restoring state

On the Linux service host, inspect mappings without printing application secrets:

```sh
lsblk -o NAME,TYPE,FSTYPE,MOUNTPOINTS
findmnt --target /srv/opsecmkt
findmnt --target /srv/opsecmkt-retention
sudo docker info --format '{{.DockerRootDir}}'
swapon --show
```

Run `findmnt --target` for the returned Docker root, the actual containerd path
and every sensitive path from the inventory. Docker's image store can persist
under `/var/lib/containerd` separately from `/var/lib/docker`. Follow each mount's
parent devices with `lsblk`; expect every storage branch to pass through an active
LUKS2 mapping. Check nested bind mounts separately. Filesystem type `ext4` or `xfs`
alone does not establish encryption. Query each actual mapper name with:

```sh
sudo cryptsetup status YOUR_STATE_MAPPING
```

Expect an active LUKS2 mapping for the intended device. Confirm its header type with a read-only `cryptsetup luksDump` on the verified backing device when needed. Record evidence privately. These guest checks cannot verify a provider's physical disk encryption or exclude its control of the hypervisor.

The native read-only verifier checks actual Linux block ancestry. Supply existing
paths, replacing runtime examples with their observed locations:

```sh
./opsecmkt deploy storage verify --path / \
  --path /srv/opsecmkt-inputs --path /srv/opsecmkt-retention \
  --path /var/lib/docker --path /var/lib/containerd
```

Expect a verified path and LUKS UUID list for every path. A failure leaves storage
unchanged. `--expected-luks-uuid UUID` additionally requires every supplied path
to resolve to exactly that single UUID; use separate invocations when root and
state use different volumes. Keep actual UUIDs in the private operator record.

Production `init` saves each path's mount point, mount root, filesystem UUID and
protecting LUKS UUIDs. These are identity metadata, not unlock keys. `install`,
`up`, `redeploy` and `doctor` repeat the checks and reject a missing mount or
changed filesystem even when it shares the same outer encrypted volume. The
proof applies to the host running the CLI. Run it on the actual service host; remote Docker
contexts and a workstation's encrypted disk do not establish remote storage
protection. `plan`, `status`, `logs` and `down` remain available for inspection and
shutdown when storage verification prevents a start.

Production checks also refuse active swap and inspect host log, crash-report and
temporary-storage paths. This does not configure Apport, service/container core
limits or provider snapshots; complete the explicit crash policy above.

### Prepare an additional blank data device

This optional procedure does not encrypt the operating-system root. Use it only
after root encryption is established, or while preparing a secret-free replacement.
Linux root, an interactive terminal, native `cryptsetup` and filesystem tools are
required. The command never installs packages, stores a passphrase, edits
`/etc/fstab` or configures automatic unlocking.

1. Allocate a **new empty whole block device**. Verify its stable identifier,
   canonical device path and capacity with `lsblk`. It must have no filesystem,
   partitions, mounts or holders. Do not select a current root or production
   data disk. Never guess a `/dev/` name from an example.
2. Create an empty private mount directory whose parents are root-owned and not
   writable by other users:

   ```sh
   sudo install -d -m 700 /srv/opsecmkt-encrypted
   ```
3. Replace both device placeholders with the independently verified new device.
   The confirmation must equal its canonical resolved path. This command is
   **destructive to that selected device**:

   ```sh
   sudo ./opsecmkt deploy storage prepare \
     --device /dev/disk/by-id/YOUR_NEW_EMPTY_DEVICE \
     --confirm-device /dev/YOUR_NEW_EMPTY_DEVICE \
     --name opsecmkt-state --mount /srv/opsecmkt-encrypted
   ```

   Native `cryptsetup` handles passphrase entry directly. Expect a newly mounted
   LUKS2/ext4 filesystem and its public UUID; no service starts. Retain that UUID
   and an independently protected header backup. Keep the unlock secret off-host.
4. Verify the mounted path with the recorded UUID:

   ```sh
   ./opsecmkt deploy storage verify --path /srv/opsecmkt-encrypted \
     --expected-luks-uuid YOUR_RECORDED_LUKS_UUID
   ```

After an intentional shutdown closes the mapping, unlock the **same** volume
without formatting it:

```sh
sudo ./opsecmkt deploy storage unlock \
  --device /dev/disk/by-id/YOUR_EXISTING_ENCRYPTED_DEVICE \
  --expected-luks-uuid YOUR_RECORDED_LUKS_UUID \
  --name opsecmkt-state --mount /srv/opsecmkt-encrypted
```

The identity must match before passphrase entry. The mount directory must again
be empty and private. A missing device, wrong UUID or unavailable passphrase must
leave services stopped. Do not rerun `prepare` to recover an existing volume.

### Keep services stopped until storage is ready

Use mount dependencies and pre-start checks for the expected encrypted source;
directory existence alone is insufficient. A missing mount can otherwise create
fresh state beneath its ordinary directory. Do not store unlock keys in
`operator.json`, the boot filesystem, a Docker volume or a snapshot beside the
encrypted data.

Production `install`, `up` and `redeploy` require Linux root and systemd. Before
application startup they install per-project drop-ins for **both Docker and
containerd**, using the running operator binary from its persistent root-owned
path, then reload systemd configuration. They do not restart either daemon. The
drop-ins run `deploy storage check-bound` against the retained installation before
those daemons start; the check uses stored paths and UUIDs without calling Docker.

These are host-service checks: a failed project check can prevent Docker and
containerd from starting other workloads on the same host. Use the intended
dedicated production host and review existing workloads. Keep the binary and
configuration at their installed paths; inspect a differing existing drop-in
instead of overwriting it blindly.

To inspect an equivalent drop-in candidate without installing it:

```sh
./opsecmkt deploy storage boot-unit --dir /srv/opsecmkt/market-prod \
  --binary /usr/local/bin/opsecmkt
```

`boot-unit` prints configuration only. Review the binary path, retained
installation path, stored identities and mount ordering. Add equivalent checks
for native gateway, payment and backup services; the automatic daemon drop-ins
do not supervise them. Configure required mount ordering through the operating
system before enabling services, then test a missing/locked mount on a disposable
host. No command here provides automatic disk unlocking.

### Enabling an attended administrator reboot

Keep the web reboot control unavailable until encrypted-host migration and a
real OS reboot/manual-unlock rehearsal have passed. Follow the
[operator activation procedure](../operator-guide.md#enabling-the-private-application-host-reboot-control):
root runs `deploy host-reboot configure`, attaches the narrow socket with
`deploy up`, and records the completed rehearsal with
`deploy host-reboot ready --confirm-unlock-rehearsed`. Each command also needs
the retained `--dir DIR`.

The agent accepts only the configured application UID on its protected local
Unix socket. Keep its root-owned policy and durable receipt ledger on expected
encrypted storage. The app has no Docker/systemd socket or unlock material.
The private administrator form requires password, enrolled TOTP, CSRF, nonce
and an explicit downtime/manual-unlock acknowledgement. Its database audit
commits before one fixed app-host reboot request; acceptance has at least a
30-second response grace and a 10-minute cooldown. The agent rechecks storage
and policy before execution. Lost acknowledgements and restarted agents never
replay a request automatically.

Policy acknowledgement records the operator's statement; it cannot establish
key possession or successful recovery. It is bound to the current kernel boot
identity. After every full host reboot or replacement, root must acknowledge
manual unlock and verification again before the button becomes available.
The app UID remains trusted to perform
human authorization. An application compromise can misuse this limited reboot
capability, so enable it deliberately and retain separate operator access.
After every reboot, unlock through the trusted OS-level path and verify mounts,
services, retained identities and payment gates before resuming approved intake.

Keep the root agent's storage verification in the actual host mount namespace.
Service sandbox options that introduce private bind mounts can change mount
identities and make its saved-storage checks refuse startup. Review such unit
overrides against the encryption checks before applying them. The agent remains
a trusted root process with a narrow protocol and Unix-only communication.

### Migrate an existing unencrypted installation

Treat this as a separately approved maintenance operation. The new setup gate
does not convert existing disks. The [storage assessment](storage-security-assessment.md)
continues to describe the current deployment's pending migration.

1. Verify database, current retention ledger, native configuration, Tor identities and wallet recovery backups in isolation. Preserve original stable secrets and the database payment-recovery gate.
2. Prepare a **fresh server with an encrypted operating-system root** using the distribution's encrypted installation procedure. Add encrypted state volumes if needed. Verify device identity, mounts, key custody and boot recovery before copying state. Agree the concrete resources, temporary overlap cost and provider console requirements first.
3. Prepare an approved cutover window with payment intake paused and settlements reviewed. Define a consistent final backup and rollback; two applications must not operate the same custodial wallet concurrently. Stop an old Tor publisher before starting the same onion identity on its replacement.
4. Restore sensitive state directly onto verified encrypted storage. Install reviewed firewall and service configuration, including the exact required source addresses.
5. Run `deploy storage bind --dir /srv/opsecmkt/market-prod` on the replacement to record its verified storage identities. It preserves the retained keys, configuration and installation receipt; it is not a fresh `init` or a force bypass. Review/install boot checks and rehearse a locked reboot and manual unlock before starting production services.
6. Recheck routes, Tor identity, uploads, retention and recovery reconciliation before reopening. Resolve old unencrypted disks/snapshots through the provider's documented retention and disposal process.

Keep protected LUKS header backups and recovery passphrases apart from the server. They are recovery material, not routine deployment files. Manual emergency keyslot erasure has separate, irreversible consequences; it is not an installation or hardening step.

## 7. Maintain the boundary

Review approved addresses when operators, hosts or providers change. Add and verify a replacement address before removing the old approved one. Revoke departed operators' SSH keys, infrastructure access and Tor client authorization independently.

Maintain a separate egress inventory: Tor bootstrap/relays, managed database, RPC, DNS, time synchronization, reviewed OS/package and image sources, scanner updates, backup storage and version feeds. Default-deny new outbound connections by role/process or container network, then permit only documented destinations and transports. Keep the Tor process's relay access separate from the application's database/RPC access. Use a tested update/recovery path; fixed release URLs can resolve through changing CDN addresses. Broad inbound access is never an egress prerequisite. Verify Monero's intended Tor transport at the payment host, including refusal of direct fallback.

Repeat the connection and storage checks after approved reboots, firewall changes, new listeners and Docker updates. Keep a private change log with the approver, purpose, exact scope, rollback and verification result. Finish the [production checklist](production-checklist.md) and record unresolved findings as open launch gates.
