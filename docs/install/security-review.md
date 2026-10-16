# Production security review

Review begun on 2026-10-15, in the operator's timezone. This record covers the
current source and a read-only inspection of the application, upload/gateway and
payment hosts. Exact addresses, provider identifiers and raw command output stay
in the protected operator evidence.

**Original inspection:** the public storefront had no public-IP listener; cloud
firewalls restricted SSH and a separate client-authorized onion protected
administrator ingress. Compute disks lacked guest encryption, host firewalls did
not independently enforce the allowlist, and some service grants used dynamic
role tags. These were hardening gaps, not evidence of compromise. The findings
below retain that original scope. All three roles have since migrated state to
encrypted replacements, with host firewalls, verified payment endpoint/network
cutover and a production app reboot/manual unlock. Original plaintext hosts remain
fenced and retained. Old-peer access pruning and fresh connection checks passed;
current-boot host readiness was acknowledged without requesting reboot. Public
and authorized private login routes passed again. Independent unauthorized-client
refusal verification is inconclusive; owner sign-in/MFA remain pending.
See the [current deployment checkpoint](deployment-notes.md#evidence-and-remaining-gates).
No funded-wallet transaction was performed.

Read the [server hardening procedure](server-hardening.md) before making changes, and
the [storage assessment](storage-security-assessment.md) before selecting an
encryption migration. This review changed documentation only.

## Scope and evidence

The requested policy permits the public Tor storefront while refusing direct
storefront access on public IPs. SSH and private service traffic must come only
from sources approved by the administrator. Client authorization for the private
administrator onion is an additional credential check. It does not check the
browser's original IP address: Tor hides that address from the service. See the
[Tor onion-service overview](https://community.torproject.org/onion-services/overview/).

The source review covered listener publication, administrator routing, Tor key
format and authorization, browser assets and CSP, image processing, and runtime
egress contracts. Live evidence includes provider firewall records and each
role's protected `*-network.txt`, `*-flow-probes.json` and storage inspection
outputs. Current observations supersede earlier deployment notes where their
scope differs.

## Live network samples

All 15 sampled TCP connection checks matched their recorded policy. Four
approved private connections succeeded; eleven unapproved or public-listener
connections timed out or were refused.

| Source role | Destination and ports | Observed result |
| --- | --- | --- |
| Gateway | App private `8080` and `8443` | Both connected. |
| App | Payment private `18443` and `18444` | Both connected. |
| Gateway | Payment private `18443`/`18444` and app public SSH/`8080` | The payment and SSH checks timed out; public `8080` was refused. |
| App | Gateway scanner `3310` and gateway public `80`/`443`/`8080` | All four timed out. |
| Payments | App private `8080`/`8443` and gateway public SSH | All three timed out. |

These samples support the current source restrictions and listener bindings. A
TCP connection does not prove TLS, account authorization or a complete user
journey. The checks cover the listed role pairs and ports at the observed time;
they do not establish all-address coverage, an independent host firewall,
post-restart persistence or effective egress confinement.

## Findings to resolve

### DEP-01 — High: compute data is readable from an unencrypted disk

**Evidence:** All three hosts use an ext4 root filesystem on `/dev/vda1`, without
an active dm-crypt mapping. Docker storage, `/srv` and logs resolve to that
filesystem. Persistent application secrets, application and gateway onion
identity material, TLS keys and payment service volumes are therefore outside a
guest-controlled encrypted boundary. The presence of a wallet volume does not
prove that it holds a funded wallet.

**Failure chain:** An adversary obtains a readable offline root disk, snapshot or
equivalent provider-side copy, then reads the service files directly. An
encrypted logical backup does not encrypt the original source files. A private
network and SSH allowlist do not prevent this offline read.

**Action and verification:** Use the staged migration and cold-boot acceptance
checks in the [storage assessment](storage-security-assessment.md). Cover the
actual Docker root, service state, logs, dumps, swap and retained copies. Keep the
unlock secret outside the VM and its backups. Confirm that a detached encrypted
copy cannot be mounted without it, then rehearse an authenticated unlock and
restore. A mounted encrypted disk still exposes plaintext to the running host;
guest encryption does not protect against a compromised hypervisor or modified
boot environment.

### DEP-02 — Medium: private-service rules do not express the exact IP policy

**Evidence:** The cloud SSH rule permits the current operator `/32`. Application
ports `8080` and `8443` permit the gateway role tag; payment ports `18443` and
`18444` permit the application role tag. The managed database trusts the
application resource identity only. Actual application and payment host bindings
use private IPs; the gateway and scanner use loopback.

**Failure chain:** A resource newly assigned a trusted tag joins that source
group without an independent IP approval. A mistaken tag change or control-plane
actor allowed to manage those tags can broaden service access. This is a mismatch
with an explicit per-IP policy. It is not an unauthenticated bypass of the cloud
firewall or the upload/payment mutual TLS checks.

**Action and verification:** Record each approved initiator, destination, port,
IPv4 `/32` and any IPv6 `/128`. Use exact private source IPs for service flows
where supported. Retain a resource identity for a managed database only when its
provider interface requires it, and document that boundary. Remove broad or
dynamic grants after positive and negative checks. Repeat the connection samples
after changing rules, and test an independent unapproved external source. Review the union of all
firewalls on each resource. The provider documents both tag/IP source selectors
and combined firewall behavior in its
[firewall rule reference](https://docs.digitalocean.com/products/networking/firewalls/how-to/configure-rules/).

### DEP-03 — Medium: the host has no independent default-deny ingress policy

**Evidence:** UFW is inactive; host IPv4 and IPv6 `INPUT` and `OUTPUT` policies
accept traffic; `DOCKER-USER` has no restrictions. SSH listens on IPv4 and IPv6.
No global IPv6 address was observed, but link-local IPv6 exists. The cloud
perimeter is currently enforcing source restrictions; unrestricted internet SSH
was not demonstrated.

**Failure chain:** A removed, detached or broadened cloud rule leaves the host
without an independent source restriction for its listeners. Future IPv6 or port
publication changes can introduce a path omitted from an IPv4-only checklist.

**Action and verification:** Install a persistent, narrowly scoped host policy
for IPv4, IPv6 and container forwarding. Preserve existing operator access and a
tested console recovery path while staging it. Use the filtering hook appropriate
to Docker's actual backend; UFW `INPUT` alone does not cover published container
ports. Docker explains this behavior in its
[packet filtering guidance](https://docs.docker.com/engine/network/packet-filtering-firewalls/).
Prove allowed and denied access again after a Docker restart and a planned host
reboot. Do not flush Docker-generated rules.

### DEP-04 — Medium: runtime destinations remain broadly permitted

**Evidence:** Cloud egress permits TCP to any IPv4 destination, plus broad DNS,
NTP and ICMP. Host output/forwarding policy does not narrow application and
gateway destinations. Native deployment creates a routed `backend` bridge at
`internal/operator/render.go:63`; production needs remote database and RPC
connections and does not automatically receive a network allowlist.

**Failure chain:** Code execution in a runtime with a general route can contact
attacker infrastructure or unrelated private services. This enlarges the
consequences of a compromise. No source-level SSRF or actual exfiltration was
demonstrated. The earlier payment-daemon direct-route refusal is useful evidence
for that daemon namespace, not proof of confinement of every runtime.

**Action and verification:** Permit the app only its approved database and RPC
destinations; permit the gateway only its application, upload backend and local
scanner paths. Keep Tor's relay connections in its separate runtime. Document
maintenance, DNS, time and backup exceptions by role. Verify that required flows
work and direct connections to an unrelated public destination fail from the
actual app and gateway namespaces. `ESCROW_XMR_TOR_RELAY=remote-tor` is an
operator declaration; `internal/operator/operator.go:574` and
`internal/market/dependency_status.go:211` explicitly require remote verification.

### DEP-05 — Medium: crash dumping can create another persistent secret copy

**Evidence:** Apport is enabled and active on the three hosts. Running containers
have unlimited soft/hard core limits and no explicit core ulimit. No existing dump
files were observed in the checked crash directories. The native gateway's soft
limit is zero, with an unlimited hard limit. No crash was induced.

**Failure chain:** If a process produces a collected core, its memory may be
written to persistent storage with credentials or plaintext data. That copy may
survive application cleanup or a RAM-secret shutdown. A configured collector is
not proof that such a dump has already been produced.

**Action and verification:** Review or disable the collector as appropriate, set
explicit service and container core limits, and cover retained diagnostics with
encryption and access/retention controls. Core limits alone do not stop a piped
collector. Verify effective settings on newly started processes without crashing
production. See the
[storage assessment](storage-security-assessment.md).

### DEP-06 — Low: SSH grants more privilege and forwarding than required

**Evidence:** Password and keyboard-interactive authentication are disabled, but
global SSH configuration permits root login, TCP forwarding and X11 forwarding.
The cloud source allowlist continues to limit reachability.

**Failure chain:** A stolen authorized root key gives direct privileged access;
unused forwarding adds pivoting capability to an otherwise restricted account.
The current key-only policy is not a password-authentication finding.

**Action and verification:** Move routine administration to named accounts with
reviewed privilege escalation. Disable root login and unused forwarding after a
second session and console recovery have been verified. Test the effective SSH
configuration for the actual account/source combination before reload.

## Source controls that survived review

| Control | Source evidence and practical limit |
| --- | --- |
| Private host publication | `internal/operator/config.go:387` accepts explicit loopback/private bindings. `internal/operator/render.go:60` publishes those selected bindings. A wildcard listener inside a container is not by itself public host exposure. |
| Administrator segregation | `internal/market/admin_ingress.go:12` creates the private capability only in the socket handler; `internal/market/app.go:489` refuses public administrator routes before body/database work. Host or forwarding headers cannot grant that capability. Session, role, CSRF and step-up checks remain necessary. |
| Fail-closed client authorization preparation | `internal/operator/admin_tor.go:24` requires 1–256 canonical public records; `internal/operator/admin_tor_prepare.go:49` validates before changing the volume and installs valid records before removing old ones at line 155. This guards Tor's documented behavior of enabling authorization only when at least one record loads. |
| Native onion identity preservation | `internal/onionidentity/identity.go:123` expands and clamps new seed material; line 142 retains all 64 expanded bytes. Import at line 180 validates native headers, full keypair and complete address. No GoBalance/OnionBalance descriptor signer was found. The attachment's named GoBalance claim is not applicable evidence against native Tor. |
| Local browser resources | `web/assets.go:8` embeds templates/static assets. No external resource URL was found in `web/`. `internal/market/app.go:476` sets restrictive self-only styles/images/fonts and `default-src 'none'`; scripts are not enabled. Clickable documentation links and release-worker HTTPS calls are distinct from browser resource loads. |
| CAPTCHA and upload boundaries | `internal/market/captcha.go:23` binds expiring HMAC challenges to a session; `captcha_actions.go:77` renders PNG and line 109 consumes challenges once. Gateway scanning and decoding happen at `upload_gateway.go:203`; `images.go:538` re-encodes JPEG, and `upload_gateway.go:561` validates a bounded metadata-free envelope without an app-side image decoder. The gateway still handles browser credentials. |
| Tor DoS settings | `deploy/torrc:23` enables PoW and introduction limits. `deploy/Dockerfile.tor:47` checks the PoW module. Verify the effective gateway configuration and build support; PoW is not a guarantee of availability under attack. |

Tor's primary documentation explains
[client authorization activation and revocation](https://community.torproject.org/onion-services/advanced/client-auth/)
and [PoW, introduction limits and build checks](https://community.torproject.org/onion-services/advanced/dos/).
Keep service identity keys, browser authorization keys and account factors
separate. Cloud infrastructure remains a provider/hypervisor trust boundary;
using it alone does not prove origin disclosure or operator identification.

## Verification and rejected scanner claims

The attached `audit_tor_site.py` failed before scanning because two raw-string
patterns contain unescaped quotes at lines 26 and 48. The original failure output
is retained privately. A private copy with only those syntax errors corrected
ran against a 991-file source snapshot made from Git-listed working files;
ignored runtime/private directories were excluded.

That corrected scan returned 479 pattern hits: 477 URL matches and two listener
matches. Manual triage found documentation and test URLs, dependency/build
downloads, fixed separate-worker release sources, local development RPC examples
and two explicitly clearnet HTTPS examples. No hit was in the embedded `web/`
assets. None of those pattern labels alone establishes a production browser leak,
public bind, exposed key or compromise. The script does not implement several
checks claimed in its docstring, so its result is not a security certification.

The following local checks passed:

- `go test ./internal/onionidentity -count=1`: the complete identity package.
- `go test ./internal/operator -run 'Test.*(AdminTor|Onion|Identity|PrivateBind|Production)' -count=1`:
  focused administrator-Tor, identity and production-contract tests.
- Nine non-database market tests covering public administrator refusal with
  spoofed headers, private-session routing rules, JPEG-envelope bounds, expiring
  upload tickets, gateway TLS authority separation and CAPTCHA session binding.
  A sandboxed rerun blocked the TLS fixture's loopback listener; the focused
  rerun with local listeners permitted passed all nine. Both results and the
  exact test names remain in protected validation records.
- The example firewall candidate in the hardening guide passed `nft --check` in
  an isolated network namespace. This validates syntax only; it was not installed
  on production and has no enforcement or persistence result.

The live connection samples above complement these local tests. Outstanding
checks include firewall restart/reboot persistence, independent external and
all-address coverage, runtime egress confinement, Tor Browser Safest user
journeys, disk unlock/restore rehearsals and the remaining
[production launch gates](production-checklist.md). Database-backed account and
funded-payment workflows were not run as part of this review.
