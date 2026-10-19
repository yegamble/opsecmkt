# Production checklist

Check every applicable item before inviting users. Follow the ordered
[production deployment guide](production.md), then record the actual outcome of
each check. The [sanitized deployment notes](deployment-notes.md) show decisions,
encountered issues and checks that are still pending for the current deployment.

Review [Security and OPSEC operations](../security-operations.md) first. Check
the threat boundaries against your deployment; encryption of backups does not
encrypt every database record or protect secrets on a running compromised host.
Follow the [server hardening steps](server-hardening.md). The
[storage assessment](storage-security-assessment.md) distinguishes current checks
from historical findings; proposed controls must not be marked as deployed.

## Installation and private routes

- [ ] Linux was installed with an independently unlocked LUKS2-encrypted root
  before production inputs, onion identities or application keys were written.
  Root/Docker/containerd storage, retention and private input paths pass native
  verification. The CLI runs on the actual host against its local Docker Engine.
  Existing plaintext installations completed a reviewed replacement migration;
  installing the new binary alone was not counted as encryption.
- [ ] Source revision reviewed and pinned to a release tag or commit. Native
  binaries and digest-pinned application images match that revision.
- [ ] Native configuration uses `--mode prod`; `deploy plan --dir DIR` and
  `deploy doctor --dir DIR` pass. The saved Docker context points to the intended
  local engine. Doctor validates configuration, encrypted-storage bindings and
  Compose availability, not remote service readiness.
- [ ] Application, upload/scanner gateway and payment services use their
  separate hosts. Managed PostgreSQL is remote; no local database container was
  installed as a substitute.
- [ ] `deploy status --dir DIR` shows the application healthy after migrations.
  The public gateway returns `200` and `ok` from `/healthz`. These checks alone
  do not prove uploads, recovery or payments work.
- [ ] Database uses its private direct endpoint, a dedicated application role,
  the correct public CA and `sslmode=verify-full`. A real connection confirms the
  intended database, UTF8 encoding and TLS. Backup clients use their own
  host-readable CA path.
- [ ] A named operator has verified key-only SSH and required sudo access.
  Root/password/keyboard-interactive login and unneeded forwarding are disabled
  only after a fresh non-root login and recovery-console check succeed.
- [ ] Administrator-approved source IPv4 `/32` and IPv6 `/128` addresses and
  exact private service flows are recorded. Provider **and host** firewalls
  default-deny inbound; Docker's forwarding backend enforces the same policy.
  Shared role tags or whole private-network ranges do not silently expand trust.
- [ ] Fresh allowed/denied connections are verified from authorized and
  independent unauthorized sources over IPv4/IPv6. Actual remote responses,
  forwarding hooks and Docker-published ports are checked. Database, raw wallet
  RPC, daemon RPC and ClamAV are unreachable from the internet.
- [ ] Gateway-to-app HTTPS verifies its certificate and hostname. The separate
  upload backend uses mutual TLS and rejects a client without the gateway's
  valid identity. Only leaf private keys are on runtime hosts; CA signing keys
  remain with the issuer.

## Public and administrator ingress

- [ ] The entire public site passes through the separate upload gateway. The
  gateway, scanner and public ingress have supervisors and recover after a
  controlled host restart.
- [ ] HTTPS only: sign-in and other forms work through the real proxy with the
  original Host preserved and secure cookies enabled.
- [ ] Tor only: completed the
  [native separate-onion procedure](tor.md#native-production-with-separate-onions).
  Full public and administrator addresses match their retained native keys.
  A short vanity prefix is not treated as identity verification.
- [ ] Tor only: no marketplace HTTP/HTTPS listener is reachable on a public
  IPv4 or IPv6 address. Public Tor points at gateway loopback. Operator IP rules
  apply to SSH/private services; no relay/forwarded address is treated as the
  original Tor visitor's IP.
- [ ] Private administrator onion: a client without its authorization key cannot
  connect; every public onion or mirror returns `404` for `/setup`, `/admin`,
  encoded protected paths and private audit routes. Empty or invalid client
  allowlists refuse administrator Tor startup.
  Verify fresh authorized and unauthorized clients. A nonempty hostname, app
  health or a previous successful test does not establish current publication.
  Descriptor-unavailable and timeout results are inconclusive; retain their exact
  codes and timestamps rather than marking unauthorized refusal passed.
- [ ] Forged Host or role headers cannot grant private access. Administrator
  sessions are refused on public shared staff routes; moderator workflows still
  work. The current isolated private-admin path's image-upload limitation is
  understood.
- [ ] A normal account and vendor listing work through public ingress. A JPEG
  or PNG upload survives reload; scanner outage and stale signatures refuse
  uploads while ordinary pages remain available. Test content is removed.

## Accounts

- [ ] First-run setup is complete through the intended administrator path or
  host bootstrap command. Setup credentials are kept out of reports.
- [ ] Administrator has an authenticator app enrolled and recovery codes stored
  offline.
- [ ] The operator can perform the documented host-side administrator reset
  ([Authentication](../operator-guide.md#authentication)).

## Backups and recovery

- [ ] Encrypted recovery copies preserve the native private installation,
  application/setup/audit secrets, gateway configuration, leaf identities and
  selected onion identities. A legacy `.env` copy alone does not back up a
  native deployment.
- [ ] Retention is enabled and enrolled on the current original database. Its
  current external ledger is persistent, protected and retained across app
  rebuilds. Recovery never initializes an empty ledger for an old database.
- [ ] Managed database backup and point-in-time recovery policies, where
  available, are enabled and understood. VM snapshots are accounted for but do
  not replace a database, configuration, retention or wallet recovery plan.
- [ ] Encrypted logical backups run with `scripts/backup.sh` from a matching
  source checkout and an explicitly configured host-side database URL. Each
  archive and its `.retention.json` sidecar reach approved off-host storage;
  backup completion and age are monitored.
- [ ] A backup has been restored into a new isolated database with
  `scripts/restore.sh`. Recovery used preserved application keys and a private
  copy of the **current** external retention ledger; deletion replay completed
  and payment recovery gates stayed closed until reconciliation.
- [ ] Backup age identities, escrow seed-decryption keys and administrator client
  keys are stored apart from runtime hosts and backup archives. Copy locations,
  expiry and certificate renewal are recorded under the
  [retention policy](../data-retention-privacy-policy.md).
- [ ] When sensitive audit exports are used, `AUDIT_EXPORT_RECIPIENT` contains
  only the public key; an export and its signature have been decrypted and
  verified offline ([instructions](../operator-guide.md#encrypted-audit-exports)).

## Encrypted storage and physical access

- [ ] Every sensitive mount is traced to verified guest-managed LUKS2 storage:
  operating-system root, native inputs/secrets, Docker **and containerd** data,
  onion volumes, wallet/RPC state,
  retention ledger, logs, temporary/staged exports and local backups. Plain
  filesystem types, backup encryption and provider at-rest claims are not
  treated as evidence of guest encryption.
- [ ] No active swap is present; native production checks refuse it. Hibernation,
  host crash handlers, service core limits and container core limits have an
  explicit verified policy. Provider disk/memory snapshots
  and historical unencrypted disks/backups are included in the inventory.
- [ ] Unlock secrets and protected LUKS header backups are kept off-host.
  Boot cannot start Docker/application/payment services on missing encrypted
  mounts. An approved locked reboot, manual unlock and isolated restore have
  passed; migration/reboot plans identify concrete targets and rollback.
- [ ] Expected storage identities are recorded and checked before services
  start. A replacement installation used `deploy storage bind` only after
  verified state transfer; keys and the installation receipt stayed intact.
  Docker, containerd and native-service startup dependencies were reviewed and
  installed before enabling automatic startup. Native production starts installed
  their per-project Docker/containerd drop-ins from a persistent root-owned
  binary on the systemd host; the operator configuration remains available on
  persistent encrypted storage after root unlock.
- [ ] The operator rehearsed both a service-only restart with storage mounted
  and a full locked reboot with OS-level unlock. Missing/wrong storage leaves
  services stopped. Recovery does not format disks, erase keyslots or reopen
  payment intake automatically.
- [ ] Managed database at-rest encryption, backups and key custody are checked
  against the service contract. Provider-managed encryption does not exclude
  provider access. Unlocked guests, hypervisor control and physical boot or
  memory attacks remain documented limits.

## Optional private application-host reboot control

- [ ] The application host has a verified encrypted root and completed a real
  OS reboot/manual-unlock/service-recovery rehearsal. Off-host unlock material
  and an authenticated recovery console remain available.
- [ ] Root ran `deploy host-reboot configure --dir DIR`, attached the app-only
  Unix socket with `deploy up`, then acknowledged the completed rehearsal with
  `deploy host-reboot ready --dir DIR --confirm-unlock-rehearsed`. The policy,
  receipt history, application UID/GID and expected storage pins were reviewed.
  Readiness is bound to the current OS boot and is re-acknowledged after each
  later manual unlock/recovery check; copied policies do not enable a new host.
- [ ] The private admin form is unavailable without a ready agent and enrolled
  authenticator. Password, TOTP, CSRF, nonce and downtime/unlock acknowledgement
  are required. Public/spoofed ingress cannot request the action.
- [ ] The request ID is visible before submission. The database audit commits
  before host dispatch; the acceptance receipt precedes at least 30 seconds of
  grace and a 10-minute cooldown. Unknown responses and replay do not schedule a
  second reboot. No live reboot is counted as passed from fake executor tests.
- [ ] Recovery verifies unlock, expected mounts, services, identities and
  existing payment/recovery gates. The root agent does not gain or store a disk
  unlock key; enabling its app-UID capability is an explicit operator decision.

## Payments

Leave intake paused when payments are not enabled or a required check is pending.

- [ ] Only Monero prices, checkout and intake controls are shown. The selected
  `MONERO_NETWORK` is recorded explicitly: native initialization defaults to
  stagenet; mainnet holds real funds. Evidence for one network is not represented
  as evidence for another.
- [ ] The operator accepts that escrow is custodial: the market holds wallet
  keys and sees order and payment metadata. Read
  [what Monero escrow protects](../escrow.md).
- [ ] The separate daemon and dedicated escrow wallet RPC use authenticated
  HTTPS over narrowly authorized private routes. Network pin, daemon and wallet
  synchronization, stable wallet secret and public seed-encryption key pass the
  application's readiness checks.
- [ ] Tor mode: payment-host Tor egress and blocked direct fallback have been
  checked at the actual daemon network boundary. Only after those checks is
  `ESCROW_XMR_TOR_RELAY=remote-tor` declared. The application cannot verify the
  remote proxy or firewall; see the
  [relay transport procedure](../operator-guide.md#declaring-separate-payment-host-tor-egress).
  Legacy local overlays cover only their bundled daemon.
- [ ] Remote wallet files, database, native application secrets and offline
  seed-decryption key have separate encrypted backups. Payment and wallet
  recovery have been rehearsed on stagenet; real funds are not deployment tests.
- [ ] Actual payment readiness is recorded separately from process health.
  **New escrow checkouts** is set to **Escrow** in Admin only after the applicable
  readiness and recovery checks pass.

## Publishing

- [ ] Deployment source manifest reviewed; evidence, cloud documentation
  publishing tooling and CI/AI artifacts excluded from the optional
  [deployment package](../security-operations.md#package-and-publish-deliberately).
- [ ] Publication copies contain no production credentials, provider account
  identifiers or unnecessary operator details. Protected originals and
  authentic verification dates are retained.
- [ ] Tor only: the complete onion address and any mirrors are published in an
  operator-signed document. Canary is published from `/admin/transparency`.

## Maintenance

- [ ] The separate version worker delivers signed reports verified against the
  independently pinned public key. Health, scanner signatures, certificate
  expiry, database capacity and backup age have alerts and an accountable owner.
- [ ] App upgrades use reviewed images and `deploy redeploy --dir DIR
  --app-image IMAGE@sha256:DIGEST`, after backup and migration review. Endpoints,
  keys, retention state and existing remote services are preserved; an image
  rollback is not treated as a schema rollback.
- [ ] Gateway/scanner/ingress, payment-host and OS maintenance have independent
  procedures. Current scanner signatures are delivered through reviewed image
  replacement; the native controller has no `refresh-scanner` command.
- [ ] Per-role egress is inventoried and restricted, including DNS/time, Tor,
  database/RPC, updates, scanner signatures, backup storage and version feeds.
  Rule persistence and denied-source tests are repeated after approved host or
  Docker restarts; operator-address and key revocation have a tested procedure.
- [ ] Recovery rehearsals repeat after material changes. Cost review covers
  hosts, managed database availability/storage, registry, backups, retained
  copies and transfer; published guide examples contain no fixed provider prices.
- [ ] Incident response names the owner, traffic and payment shutdown steps,
  evidence preservation and recovery checks. Destructive actions require
  approval of concrete targets and consequences.
