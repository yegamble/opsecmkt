# Enabling and operating retention

Retention maintenance applies the [data retention and private information policy](data-retention-privacy-policy.md).
It is opt-in so an upgrade cannot silently purge an existing market before users receive notice and preservation duties are recorded.
Recipient-encryption enforcement remains a separate implementation gap.

Abuse-review copies are a separate lifecycle gap. The ordinary sweeps do not erase captured plaintext or its review records when
the original listing, image description, dispute text or review expires, or when an account is erased. Disabling future collection
does not remove historical copies. These records currently have no automatic expiry. The operator must decide their minimization,
retention and purge policy before relying on ordinary expiry as a complete data-removal policy. The public records inventory and
account-deletion notice disclose this exception; they remain visible regardless of whether collection is currently enabled.

## Before enabling

Inventory database replicas, backups, audit exports, wallet files, snapshots, log stores and every off-host copy.
Identify applicable preservation duties and the responsible case handlers. Publish the retention schedule and tell users to save messages and digital deliveries.
Schedule backup and export expiry wherever copies are stored; the server cannot delete a provider's snapshots or a recipient's saved files.

Keep the external retention directory separate from PostgreSQL. It contains the deletion manifest, pending-operation recovery state,
instance identity and preservation register. Restore its **current** contents rather than the older copy associated with a database backup.
Protect it with mode `0700`, files with mode `0600`, and restrict access to the application and responsible operator.

## Enabling the service

Set `RETENTION_ENABLED=on` in `.env`, then rebuild and restart the application using the install's existing Compose files.
For offline preparation, including an original database that needs encoding repair, initialize its current state without serving traffic:

```sh
docker compose run --rm app -enroll-retention
```

The Compose command uses the configured retention volume and application identity. For a native process, run `server -enroll-retention` as the service user
with `RETENTION_ENABLED=on` and an absolute `RETENTION_STATE_DIR`. Keep `DATABASE_URL` in the protected environment and point it at the **current original database**. The command applies schema migrations and initializes enrollment,
but starts no server, wallet or purge job. It refuses restore-pending sources and existing deletion obligations; it is not a way to invent an empty ledger for an old backup.
Keep the initialized directory for normal startup and every later restore. A retry preserves its original grace deadline.
Compose mounts `retention_state` at `/var/lib/opsecmkt-retention`, independently of the PostgreSQL volume.
For a service running outside Compose, also set `RETENTION_STATE_DIR` to an absolute, clean path owned by that service.

To run host backup-expiry jobs against the same preservation register, use a host bind instead of the default named volume.
For example, create a private directory owned by the app's container UID `65532` and add this override to the existing `COMPOSE_FILE`:

```yaml
services:
  app:
    volumes:
      - type: bind
        source: /srv/opsecmkt-retention
        target: /var/lib/opsecmkt-retention
```

Do not switch an already-enrolled instance to an empty directory. Move its current external state while the application is stopped,
preserving ownership and permissions. The database pins the directory's instance identity and sequence; missing, corrupt or foreign state stops startup,
as does a sequence older than the database checkpoint.
It cannot detect a database and matching ledger rolled back together, so always supply the authoritative current external state.
After enrollment, disabling retention also stops startup rather than discarding previously accepted deletion obligations.

The first enrollment provides a 30-day grace period before ordinary historical data is purged.
Existing accounts without observed authentication history receive a bounded 90-day reconfirmation period.
Accounts created after enrollment that never authenticate enter the 30-day notice period 12 months after creation.
Sign-in and authenticated use record activity; they do not cancel an explicit deletion request.
Legacy last-use dates are not invented from account creation dates.

## What expires

| Data | Live-system deadline |
| --- | --- |
| Abuse-review records, legacy and new: legacy copied content; new record and order references, capture times, text digests, exact and perceptual image fingerprints; findings, decisions and staff reviews | No automatic expiry; separate from the ordinary content/account deadlines below, including after the original content or image is removed, after account deletion and after collection stops. A digest or fingerprint can confirm a guessed text or a matching image |
| Individual direct messages, ciphertext and routing metadata | 90 days after creation |
| Support-ticket bodies | 90 days after creation, or earlier when the opener/message sender is closed or erased, unless preserved |
| Closed or Ignored support-ticket metadata | 90 days after the last update, once all bodies are gone and no related hold applies |
| Notifications, their XMPP outbox receipts and closed outage-delivery receipts | 30 days |
| Activity events | 180 days after creation |
| Sessions, pending sign-ins, CAPTCHA receipts, PGP challenges and form receipts | Their existing validity deadline, followed by scheduled removal |
| Unfunded drafts | 30 days after creation |
| Private order text, encrypted order correspondence, digital originals/replacements and dispute images | 30 days after verified financial closure |
| Reserved digital unit contents and deduplication fingerprints | The associated order's private-content deadline |
| Ordinary closed-order records | 180 days after verified financial closure |
| Withdrawn listing content, images and reviews | 30 days after withdrawal, unless necessary for an open transaction or preserved case; abuse-review records of them (first row) are separate |
| Accounts | 12 months without observed authenticated use, then 30 days' notice; a verified deletion request has a 30-day deadline |

New initial and replacement per-order manual and automatic digital deliveries are stored as PGP armor encrypted to the buyer. Their
content deadline remains 30 days after verified financial closure; encryption does not extend retention.
Reusable automatic-delivery templates and distinct-unit inventory remain server-readable, and older plaintext delivery records retain their
existing deadline and reader permissions. The upgrade does not rewrite or erase those historical copies.
XMPP forwards only eligible encrypted contents after separate global permission and recipient consent; deleting
a notification also removes its delivery receipt, but cannot recall copies retained by an XMPP provider or client.

With retention enabled, support-ticket bodies expire 90 days after creation, or earlier when their opener or
message sender is closed or erased. A relevant account preservation hold retains the related ticket and bodies:
this includes the opener, current or historical participants, message senders and staff in the workflow history.
An overdue hold review does not release it. Closed or Ignored ticket metadata is removed only when its last
update is at least 90 days old and no bodies remain; Open ticket metadata stays for the staff workflow.
Ticket and ticket-message deletion uses the existing journal and restore replay so an older backup does not
resurrect expired records. XMPP providers and clients may retain independent copies after market cleanup.

Closure removes sign-in capability, credentials and unrelated profile data. Open funded escrows or the last usable administrator defer credential erasure
under a visible operational exception with a 30-day review. Unrelated profile data is scrubbed on schedule; a restricted account can settle existing orders,
add image evidence to an existing open dispute, and update sign-in or PGP security, while new obligations, unrelated direct messages and listing/avatar uploads are blocked. Once the obligation ends, credentials are removed.
Necessary financial dependencies may retain a minimal account reference until the associated records can be safely retired.
A preservation hold can retain its specified records.
The jobs run bounded batches at startup and every 30 minutes, draining eligible backlogs. Failures retry after one minute.
Expired authentication records cannot authenticate while awaiting cleanup.

Users request deletion from **Account → Retention and account deletion**, confirming their password and enrolled authenticator.
The account page shows an accepted request. The last active administrator cannot request closure until another active administrator exists.
For a user who cannot sign in, use the published staff contact and independently verify ownership; the authenticated form does not add a new recovery bypass.

## Preservation and financial exceptions

Administrators register and review scoped account, message, order or activity-event holds in **Control room → Retention and preservation**.
Record the actual authority, reason, authorized readers and release condition. Prefer individual record scopes to an entire account.
An order hold covers the order-bound correspondence, delivery updates and dispute images. It cannot infer which earlier direct messages concerned that order; preserve those direct-message IDs separately.
It also covers that order's reserved digital inventory. Unassigned unit contents follow listing withdrawal and preservation rules. Unit content and its fingerprint are erased together; a consumed unit remains unavailable until its record is retired. The application retains no permanent content-hash ledger after this erasure. Restored-backup deletion replay covers these records as well.
The recorded reader restrictions govern case handling; holds do not replace existing application permissions or encrypt plaintext records.
These holds protect lifecycle deletion, not an immutable archive. Explicit owner cleanup of undisclosed listing content follows its existing controls; preserve required evidence separately before permitting that cleanup.
Privileged operators can still read retained plaintext. Order correspondence requires local recipient encryption. Image evidence is unencrypted and visible to both parties and authorized case staff; only upload evidence intended for those viewers. Private staff support remains a separate direct channel.
Review within 30 days. An overdue legal hold stays protected until its duty ends.
Release requires password confirmation; the original deletion deadlines then apply. Released identifying register entries expire after 180 days.

The same panel lists account-closure exceptions and financial closure or retirement blockers. Review records responsibility for another 30 days;
it does not override a wallet check, preservation duty or settlement safeguard.
Financial closure requires terminal order state, completed reconciliation and reorganisation monitoring, and no unresolved dispute, alert or recovery gate.
Ordinary late-payment monitoring lasts 90 days. Unresolved funds create an explicit blocker rather than being destroyed at an age threshold.

Retirement must preserve allocation and key non-reuse protections. Never disable escrow triggers or reset indices to delete an order.
Monero wallet destruction retains its existing zero-balance, empty-pool and settlement guards.
When retention is enabled, the service records a durable destruction intent in the external state before the irreversible wallet-password change.
An ambiguous RPC result keeps recovery pending and blocks normal startup until it is reconciled; a later database restore must replay the destruction record.
Destroyed wallet files and their copies still require operator cleanup; a database purge cannot remove files on another wallet host.
Guarded database retirement removes order/payment mappings, addresses, keys, signatures and original wallet labels.
It preserves allocation high-water marks and global key/address hashes solely to prevent reuse or resurrection from stale wallets.
These hashes can still be linked using outside information; they are not anonymous data. Retain them only while this service or its recovery lineage
can reuse those keys or wallets, then destroy the guard state within 30 days of permanent decommissioning after all obligations and preservation duties end.
Wallet cleanup remains an operator task, tracked separately from database retirement.
Publish the payment cutoff: funds sent after retirement or wallet destruction may be unrecoverable.

### Physical wallet cleanup register

**Control room → Retention and preservation → Physical wallet cleanup** shows the protected wallet name, original cleanup deadline and next review.
Monero obligations are registered when the existing guarded 90-day destruction occurs.
A shared wallet uses its earliest outstanding deadline; reviewing it does not restart that clock.

For Monero, remove the already-destroyed wallet's `NAME`, `NAME.keys` and all corresponding copies only after the existing destruction checks and preservation review.

Using the operator's protected database administration connection, record a review of the exact row:

```sql
UPDATE retention_wallet_cleanup
SET review_due=clock_timestamp()+interval '30 days'
WHERE coin='XMR' AND wallet_name='EXACT_OLD_WALLET_NAME' AND confirmed_at IS NULL;
```

After verifying actual cleanup in every location, record completion on that same row:

```sql
UPDATE retention_wallet_cleanup
SET confirmed_at=clock_timestamp(), confirmed_by='RESPONSIBLE_OPERATOR'
WHERE coin='XMR' AND wallet_name='EXACT_OLD_WALLET_NAME' AND confirmed_at IS NULL;
```

An acknowledgement records an operator attestation; it does not delete or inspect files. Completed register entries expire after 180 days.
Any still-required older copy needs a scoped preservation record and restricted recovery use. Keep wallet inventory and acknowledgements private.

## Expiring backups and signed exports

`scripts/backup.sh` writes an encrypted `.dump.age` archive plus a `.retention.json` sidecar with a SHA-256 digest and its original 30-day deadline.
Copy the archive and sidecar together, unchanged. Filesystem modification time does not control expiry.
For an existing archive, enroll its original creation time explicitly:

```sh
python3 scripts/backup-retention.py record /srv/backups/market.dump.age /srv/backups/market.dump.age \
  --created-at ORIGINAL_UTC_TIMESTAMP
```

Use the dump's authentic creation time in `YYYY-MM-DDTHH:MM:SSZ` format. If that evidence is missing, preserve the copy for investigation;
do not manufacture a new date to restart retention.

Host audit exports also create a sidecar. Keep the issued `.jsonl` and `.sig` pair, or their encrypted `.asc` forms, with that sidecar and their issued basenames.
Expiry is 180 days after the earliest included event, rather than after download. Empty host ranges create no files.
HTTP exports expose `X-Retention-Expires`; operators remain responsible for tracking downloaded copies.
Existing signatures remain over the exact issued bytes. Future exports can change after event expiry or account erasure.

The external directory contains `backup-preservations.json`, initially a private empty version-1 register.
Register any specifically held copy there before a destructive expiry run. Every hold identifies the artifact kind, absolute primary path,
digest, original creation/expiry dates, case, owner, authorized readers, permitted use, review dates and release condition.
The format is validated by `scripts/backup-retention.py`; an overdue review does not release a hold.
Keep this register current in every expiry location. The preservation owner must remove released identifying entries within 180 days after release,
after confirming required copy destruction and allowing the manifest's eight-day release grace. This register cleanup is an operator duty; the expiry helper does not prune it.
Do not edit the application's checksummed deletion manifest by hand.

A complete version-1 register has this shape. Replace the example path, zero digest, dates and case details with the real copy's sidecar and duty;
use the primary `.jsonl` or `.jsonl.asc` path and `kind: "audit_export"` for a signed audit pair. Keep every required field, including `released_at: null`.
Write the file privately and coordinate edits with expiry jobs. Set `released_at` to the actual UTC release time when the duty ends.

```json
{
  "version": 1,
  "holds": [
    {
      "case_id": "CASE-2026-014",
      "kind": "postgresql_dump",
      "archive_path": "/srv/backups/market.dump.age",
      "sha256": "0000000000000000000000000000000000000000000000000000000000000000",
      "created_at": "2026-10-01T00:00:00Z",
      "expires_at": "2026-10-31T00:00:00Z",
      "reason": "Specific preservation duty requiring this original copy",
      "owner": "Named preservation owner",
      "authorized_readers": [
        "Assigned case handler"
      ],
      "permitted_use": "Isolated recovery for this case only",
      "started_at": "2026-10-14T00:00:00Z",
      "reviewed_at": "2026-10-14T00:00:00Z",
      "next_review_at": "2026-11-13T00:00:00Z",
      "release_condition": "Written confirmation that the duty ended",
      "released_at": null
    }
  ]
}
```

First inspect a dry run, passing every locally accessible copy location:

```sh
python3 scripts/backup-retention.py expire /srv/backups /srv/offsite-copies /srv/private-audit \
  --holds /srv/opsecmkt-retention/backup-preservations.json
```

After confirming the inventory and current holds, schedule the same command daily with `--delete`.
Run it as an account that can read the private register. It validates all managed copies before deleting expired archives or export pairs.
Unknown, incomplete, tampered or conflicting metadata stops the scan. Wallet files are outside this tool's deletion scope.
Remote/provider copies need equivalent expiry and preservation controls; including a local replica in this command does not delete its remote source.

## Restoring without reviving deleted data

Before relying on a rollback backup from a never-enrolled instance, enroll its **current original database** once and retain that external state separately.
The enrollment grace gives time to prepare ordinary expiry. A legacy backup can adopt this existing current ledger even if the backup predates enrollment.
A restored database cannot initialize an empty ledger: if both the original database and current state are lost, recovery needs evidence reconciliation
and remains blocked until an authoritative current state is available.

The replay guarantee covers retention lifecycle erasures. Manual photo/avatar removal and replacement outside that lifecycle are not journaled;
check restored media and repeat those takedowns before reopening, as the restore script warns.

Stop user access, payment sending and backup/snapshot jobs against the restore destination. Restore PostgreSQL with `scripts/restore.sh` and separately supply the current external retention directory.
The script sets `retention_restore_required` in addition to the existing payout-recovery gates.
Startup reconciles committed deletion operations, reapplies current preservation/deletion state and drains expired records before clearing the retention gate.
It does not clear the escrow reconciliation gates or authorize rebroadcasts.
The backup script refuses a database with retention replay still pending; resume backups only after current-state replay succeeds.
Apply the same restriction to provider snapshots and any other backup tools.

An unresolved operation without a matching database commit marker stops startup, because an old restored database cannot prove whether it committed.
Do not replace the directory with an empty manifest or guess at the outcome. Preserve the current files and reconcile against the current operation evidence.
Missing current state is an operator recovery problem, not permission to expose an old database.

If replay removes a former administrator from a snapshot that predates their replacement, keep public traffic isolated.
After current-state replay succeeds, register a fresh account through the ordinary `/register` form on the isolated instance and appoint it from the host:

```sh
docker compose run --rm app -set-role NEW_HANDLE admin
```

Sign in with that fresh account before reopening staff access. Host role assignment refuses closed or closing accounts;
never restore erased passwords, factors or public keys. The first-run setup route remains closed. Payment reconciliation gates still apply.

Check the latest completed pass and any error in the retention panel. Unavailable preservation state pauses mutations and fails health checks.
Treat missed expiry, overdue reviews and failed copy destruction as actionable faults.
Deletion is logical database removal; residual pages, WAL, disks and external copies require the policy's separate storage and media controls.

## Verification

Database regressions cover deadline boundaries, preservation and release, account confirmation/replay, dependent credential removal,
rollback/crash reconciliation, large-batch draining, financial closure guards and restoration of a backup predating deletion.
Backup/export regressions cover original clocks across copied files, digest validation, held/released copies and encrypted pairs.
`scripts/test-backup-restore.sh` exercises encrypted PostgreSQL recovery with the current external state and retained payout protections.

For a fresh disposable browser database, run the new form regressions with a private state directory:

```sh
E2E_DATABASE_URL=DISPOSABLE_DATABASE_URL E2E_RETENTION_STATE_DIR=PRIVATE_ABSOLUTE_TEST_DIRECTORY \
  npx playwright test tests/e2e/db-retention.spec.ts --project=database-chromium
```

The browser setup initializes that database; never point it at an existing market. Retention form checks run with JavaScript disabled at 320 px.

New moderation capture (migration 134) stores digests and limited metadata, not
source prose. Completed jobs remove the active raw actor link from the live row (backups, WAL and dead row versions can keep it until removed or overwritten); pending jobs keep it until a worker completes them, bounded by the pending cap. This prospective
minimization does not erase legacy copies, immutable audit records or backups,
and does not make retained target references anonymous. New records also have
no automatic expiry. See the
[moderation records and capacity rules](moderation-agent.md#records-and-recovery).

Migration 143 adds two small moderation tables. `moderation_backfill` holds one
row per target whose capture was refused: kind, target reference and first/last
refusal times, with no content, digest or actor. A row is deleted when the
target's current revision is queued, or when the worker finds the source gone,
archived, hidden or unflagged; while capture is disabled, rows wait until it is
enabled again. At most 65536 rows exist. `moderation_actor_allocations` holds an
account's 24-hour allocation window (account, window start, count) with no
target; an expired row is deleted by the next capture admission or worker pass,
disabling capture deletes every row, and account erasure
deletes the account's row (`ON DELETE CASCADE`, an enumerated relationship in the
account-erasure inventory). New proposal inputs no longer contain per-field
digests, and non-listing proposals no longer store a text fingerprint.
