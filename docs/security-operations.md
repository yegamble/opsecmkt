# Security and OPSEC operations

Use this guide to choose deployment controls, protect private keys, and review
what a compromised host could expose. OPSEC Market is an alpha; no deployment
can promise that an adversary gets only ciphertext after taking control of a
running server.

## Define the threat before choosing a control

| Adversary access | Controls that help | Remaining exposure |
| --- | --- | --- |
| Public visitor, including someone who knows the storefront onion | Application authorization, CSRF, admission limits, optional private administrator ingress | Public listings and published information remain public. Tor does not authenticate an application account. |
| Copy of an encrypted backup or encrypted audit export | Encryption to a public recipient whose private key stays offline | File size, filenames and anything published separately remain visible. |
| Database dump or unlocked host disk | Messages encrypted by their senders; Monero seed sealing; host disk encryption while locked | Account, order, dispute and audit metadata, plaintext notes, deliveries and images remain readable. |
| Root, Docker administrator or live application compromise | Separation of operator workstations, restricted wallet access, independent offline records | An attacker can inspect memory, alter future requests or keys, access mounted files and use online wallet services. |
| Source repository | Secret-free source and reviewed publication copies | Public code, generic deployment templates and public verification evidence are intentionally readable. |

Read [what the server keeps](implementation-status.md#what-the-server-keeps)
before inviting users. An application administrator is not the only person who
can read a database: a host or database administrator can also access it.

## Keep keys in separate roles

Keep the following private keys on an operator-controlled offline workstation
or hardware device, and rehearse recovery with disposable test data:

- The age identity that decrypts backups. Give the production backup job only
  its public recipient.
- The OpenPGP private key matching `ESCROW_XMR_SEED_KEY`. The application needs
  only the public file to seal recovery seeds.
- The OpenPGP private key matching `AUDIT_EXPORT_RECIPIENT`, when encrypted
  audit exports are enabled.
- The key used to sign the operator verification document and canary.

Tor client authorization private keys belong on the authorized client
workstation. They are used by its running Tor client and are not permanently
offline. Give the onion service only the corresponding public `.auth` files.
Use a separate credential for each operator so one can be revoked without
replacing everyone else's credential.

Several secrets remain online. `APP_SECRET_KEY` decrypts TOTP and other sealed
application state; `AUDIT_SIGNING_KEY` signs audit exports; RPC credentials and
`MONERO_WALLET_SECRET` authorize online services. The dedicated Monero wallet-rpc holds online escrow keys. Database seed encryption to an offline recipient protects the sealed recovery copy; it does not make the live wallet noncustodial.

## Restrict administrator ingress

For an onion deployment, enable the optional
[private administrator onion](install/tor.md#private-administrator-onion).
The public HTTP listener refuses setup and administrative routes, and refuses
authenticated administrators even on routes shared with moderators. A separate
Unix HTTP socket grants private ingress only to the dedicated Tor admin service.
Caller-supplied `Host`, `X-Tor-Onion-Role` and forwarding headers grant no such
access. Application sign-in, role checks and form confirmations still apply.

Keep the socket volume out of other containers. Anyone with Docker or host
access can mount it or connect locally; client authorization protects the Tor
entry point, not a compromised host. The ordinary storefront and any public
mirror continue to use the public listener. Moderator accounts still use their
existing routes on the storefront.

Before inviting operators, verify both cases with separate Tor clients:

1. Without the authorization private key, the admin onion cannot be reached.
2. With the matching key, sign in on the admin onion and perform a harmless
   administrative change, then reload to check the result.
3. On the public onion and every mirror, `/setup`, `/admin` and
   `/admin/audit-export` return 404, including requests that claim the admin
   onion in `Host` or send `X-Tor-Onion-Role: admin`.
4. An administrator session presented to the public listener receives 404 on
   shared staff routes; a moderator's ordinary dispute workflow still works.

The Tor service must refuse startup with an empty or malformed allowlist,
including a file named only `.auth`. It reads a validated snapshot in its own
private directory, rather than the host's read-only public input mount. Tor
itself enables client authorization only when it successfully loads at least
one authorization file, and ignores malformed files. See the
[Tor Project client authorization reference](https://community.torproject.org/onion-services/advanced/client-auth/).

## Encrypt exports before writing them

Database backups already stream `pg_dump` directly into age encryption:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/market.dump.age
```

Keep the private identity off the production backup job. This command exports
the database only; back up the matching application secrets, onion identities
and escrow wallets separately. Follow
[backups and recovery](operator-guide.md#backups-and-recovery), including the
payment recovery gate and a restore into a disposable database.

For audit downloads and the host export command, configure
[encrypted audit exports](operator-guide.md#encrypted-audit-exports).
`AUDIT_EXPORT_RECIPIENT` names a public OpenPGP key file. Each export and its
detached signature are encrypted in memory before output; an unusable configured
key refuses the export. Decrypt both files offline before using `verify-audit`.
The signing key and database audit rows remain online. Signed audit exports
prove possession of the signing key; they are not an independently anchored
hash chain or proof that the live host never changed earlier records.

## Protect the host and runtime secrets

The supported installer stores a mode-0600 `.env` on disk. Encrypt its backups,
restrict host and Docker access, and keep it out of source archives and AI
prompts. Full-disk encryption protects a powered-off locked disk; choose and
test unlock, reboot and restore procedures before depending on it.

Moving `.env` to tmpfs alone does not create a RAM-only secret deployment.
Compose puts the configured values into container environments and Docker can
retain them in its metadata. The installer and maintenance scripts also expect
the checkout's `.env`. Removing it after startup breaks that maintenance path
and does not erase Docker metadata. Docker recommends file-based secrets for
sensitive values; the bundled stack does not yet support a complete file-based
secret lifecycle. See [Docker's environment guidance](https://docs.docker.com/compose/how-tos/environment-variables/set-environment-variables/).

Choose swap, crash-dump, disk and snapshot policies at the host level. Audit
them with the host administrator and verify recovery; do not run a generic
firewall or boot script against a live host. In particular, an `output` rule
matching a container's numeric UID does not establish a Docker forwarding
boundary. Keep application, PostgreSQL, scanner and wallet RPC ports private.
Review both ingress and egress after every Compose override.

Tor storefront ingress does not route node traffic automatically. Use
[local nodes over Tor](operator-guide.md#local-nodes-over-tor) when appropriate
and review the documented Monero relay and verification limits. Image pulls,
package downloads, host administration, provider billing and public source
hosting can still identify or correlate a deployment.

## Package and publish deliberately

Create an optional deployment source package from a reviewed commit:

```sh
python3 scripts/package-deployment.py --ref HEAD --output /secure/market-source.tar.gz --tor-only
```

The package excludes operational evidence, cloud documentation publishing
tooling, CI configuration and AI workflow files. `--tor-only` also excludes the
clearnet Compose override. Review the generated `.manifest.json` and `.SHA256SUMS` sidecars
before transferring it. Checksums detect changed bytes; verify the source release
through a trusted channel to establish publisher identity. The package is source, not encrypted state, and includes
guides plus generic configuration examples. It does not include uncommitted
changes, replace release verification, scrub arbitrary secrets from tracked
files, or remove information already published elsewhere. Removing a clearnet
override from a package is not a firewall. Root and Tor build contexts also
exclude evidence and documentation publishing tooling through their separate
`.dockerignore` files; this reduces material sent to builders from an ordinary
checkout.

Public verification evidence and development workflows remain in the source
repository. Use synthetic data in automated tests. Before publishing a manual
run, review a separate publication copy for credentials, addresses, provider
identifiers, wallet identifiers, workstation paths and unnecessary timing or
device details. Preserve the protected original and the authentic source
revision, times and checksums needed to assess the evidence. Do not rewrite
history or fabricate dates to make a test look anonymous.

## Respond to compromise with a reviewed recovery plan

Restrict ingress and pause payment processing using the deployment's established
incident procedure. Preserve the evidence needed to establish what happened,
record which secrets and wallets may have been exposed, and assess pending or
already-broadcast settlements before restarting a recovered market.

Use a trusted operator workstation to prepare credential revocations and a
fresh host. Restore only from reviewed backups, follow the payment recovery
gate, and verify the destination addresses independently. Coordinate key
rotation with existing escrow pins and encrypted records; replacing
`APP_SECRET_KEY` or `MONERO_WALLET_SECRET` blindly can make recovery impossible.
An expired or missing canary calls for investigation and signature verification;
it does not by itself establish the cause.

Disk erasure, volume deletion, force-pushed history, emergency reboot and public
revocation notices require an owner-approved plan naming the targets and
consequences. This guide provides no automatic disk-wipe or dead-man switch.

## Applying outside audits

Treat audit text as a proposal. Check every cited path, setting and command
against the current source before implementing it. The Gemini proposal supplied
for this hardening work contained several claims that must not become product
guarantees:

| Proposal | Current implementation or decision |
| --- | --- |
| Only offline keys can read all stored state | Not implemented. Many application records are plaintext, and online secrets decrypt other records. |
| `MARKET_ADMIN_SEAL_PUBKEY` seals all records | No such setting exists. Use the scoped `AUDIT_EXPORT_RECIPIENT` and existing `ESCROW_XMR_SEED_KEY`; broader sealing needs a design for readers, search, disputes, retention and recovery. |
| Argon2id passwords and hash-chained audit history | Passwords use bcrypt; audit exports use a detached Ed25519 signature. |
| Noncustodial or air-gapped Monero escrow | The dedicated wallet-rpc holds online spend-capable wallets. Seed encryption protects recovery copies, not the running wallet. |
| Erase evidence, normalize dates, wipe disks on suspicion | Preserve truthful verification and use reviewed publication packages and owner-approved incident response. |
| Rewrite every guide with stronger guarantees | Update the affected guides, tests and capability boundaries together, with explicit unverified deployment checks. |

The [production checklist](install/production-checklist.md) covers operator
acceptance. [Contributing](../CONTRIBUTING.md) describes the code, guide and
evidence review process.
