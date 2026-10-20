# Data retention and private information policy

Version 1.0 · Policy date: 2026-10-14 · Owner: the instance operator.

This policy sets requirements for limiting retained data and server-readable private information on an OPSEC Market instance.
It covers PostgreSQL, application records, audit exports, logs, wallet files, replicas, backups, snapshots and recovery copies.

**Implementation status: retention is opt-in.** [Retention maintenance](retention.md) implements database expiry, authenticated deletion requests,
scoped preservation, guarded escrow retirement and restore replay. Backup and host audit-export tools record original deadlines and validate expiry.
Operators must enable the service, schedule copy expiry and track external wallet, log, snapshot and media cleanup before claiming instance-wide enforcement.
Disputes, order notes, automatic-delivery listing templates and distinct-unit inventory remain plaintext. New initial and replacement per-order manual and automatic delivery records are PGP-encrypted to the buyer, but the running server still sees the template, inventory or submitted manual text before encryption. Historical plaintext delivery records remain retained. These changes do not establish local recipient encryption for every private field.
See [current storage and access](implementation-status.md#what-the-server-keeps) and [current backup behavior](operator-guide.md#backups-and-recovery).

## Ownership and rules

The operator owns the data inventory, retention jobs, backup expiry and public disclosure. A named administrator handles deletion requests.
The operator designates a preservation owner to assess applicable duties and review exceptions. One person may hold these roles on a small instance.
Maintainers must implement the database and encryption controls without weakening escrow safety.

Every retained category must have a documented purpose, deadline and responsible owner. Encryption, suspension, hiding and archiving do not remove a retention deadline.
Collect only information needed for a transaction or a defined operational purpose. Do not collect real names, addresses or private delivery material in public profiles, reviews or ordinary notes.

The periods below are maximum operational defaults with stated purposes, rather than statutory periods or a claim of compliance in every jurisdiction.
Before adoption, the preservation owner must record the instance's applicable accounting, tax, dispute and preservation obligations.
Any required longer period belongs in a scoped exception with its authority and end condition; it does not extend every account, message or backup.
Shorter periods and earlier erasure apply when the purpose ends sooner or applicable rights require them.

## Retention schedule

Deadlines use UTC and elapsed days. A scheduled job must remove eligible live records within 24 hours of their deadline.
Opening a page, exporting data, copying a backup or sending another message must not restart a record's retention clock.

**Financial closure** means the order has ended, funds and late deposits within the published payment window are reconciled,
all settlements and refunds are final, confirmation and reorganisation monitoring is complete, and no dispute, payment flag or recovery gate remains open.
A displayed completed, resolved or cancelled status alone does not establish financial closure.
For each enabled adapter, publish confirmation and reorganisation thresholds and a late-payment monitoring window.
Ordinary monitoring must end within 90 days of terminal order status. If unresolved funds, a dispute or another concrete
failure prevents closure, register a scoped operational exception covering only necessary state and review it every 30 days.

| Record | Maximum retention and trigger | Purpose and required disposition |
| --- | --- | --- |
| Accounts, profile, PGP public key and authentication material | While actively used; close after 12 months without successful authenticated use, following 30 days' notice. Remove profile and credentials within 30 days of closure or a verified deletion request. | Maintain access to the service. Track last successful authenticated use explicitly. Unresolved obligations permit only the access and identity fields needed to complete them; remove unrelated profile data on schedule. |
| Account bans (A-546): when, by which administrator, the reason and any earlier suspension time; the banned handle | Ban details: until the ban is lifted or the account is closed (then removed with the profile; the audit rows that also carry the reason follow the audit retention). Banned handle (lower case only, with the time it was reserved): kept indefinitely, also after the account is erased; only lifting the ban while the account still exists removes it. | Keep a permanently closed account's handle from being registered again, for example to impersonate it. Store nothing else about the erased account. |
| Direct messages, including ciphertext, sender, recipient, time and recipient-status fields | 90 days from each message's creation, or earlier on an accepted deletion request. | Allow correspondence and a bounded opportunity to save a local copy. Expire individual messages, not entire conversations based on their latest activity. Dispute evidence requires a scoped exception. |
| Unsubmitted order drafts | 30 days after creation. | Allow checkout to resume. Remove the draft and its private fields; funded or submitted orders follow financial closure rules. |
| Orders, order events, disputes and payment records | 180 days after financial closure. | Support reconciliation and post-transaction complaints. Then delete personal records and links, including participant mappings, addresses and transaction associations, except narrowly preserved records. |
| Private notes, order correspondence and shipping ciphertext, dispute images, and initial/replacement digital delivery | 30 days after financial closure; earlier when no longer needed. | Complete delivery and resolve the transaction. Remove bodies and sensitive attachments separately from minimal order state. Warn buyers to save deliveries before expiry. A recorded dispute or legal need may preserve only relevant evidence. |
| Reusable digital listing content | While offered for sale; remove within 30 days after withdrawal or replacement. | Fulfil the listing. Sensitive reusable secrets must not enter server-readable automatic delivery under this policy. Delivered copies have their own deadline above. |
| Distinct digital unit inventory and its retained fingerprints | Unassigned entries follow the listing withdrawal deadline; reserved entries follow the order's private-content deadline. | Allocate separate entries to purchased units and reject retained duplicates. Erase content and fingerprint together; consumed entries never return automatically to available stock. Do not keep a permanent content-derived hash ledger after erasure. |
| Listings, images and reviews | While published and needed; remove within 30 days of withdrawal or account closure. Retain an order-specific copy only while necessary for an open dispute, then for at most 30 days after financial closure. | Present the catalog and resolve actual disputes. Hiding a listing or review does not satisfy deletion. Keep only necessary excerpts; do not preserve an entire catalog as order evidence. |
| Support-ticket bodies and workflow metadata | Bodies: 90 days after creation, or earlier when the opener or sender is closed/erased. Closed/Ignored metadata: 90 days after last update and only when bodies are gone. Open metadata remains for workflow. Related account holds can delay both. | Resolve support requests while bounding private ciphertext retention; journal deletion for restore replay. Prior recipients and XMPP services may retain separate copies. |
| Notifications and closed provider-outage receipts | 30 days from notification creation or outage closure. | Give users and operators time to act. Keep notification bodies free of private evidence and delivery secrets. |
| Audit events and signed exports | 180 days from the event's creation. An export expires no later than its earliest included event. | Investigate access, administrative changes and incidents over a bounded six-month window. Remove identity-linked events and every operator-controlled export copy at expiry. |
| Operational, proxy, database and backup-job logs | 14 days from the event's creation, with both age and size limits. | Diagnose faults and recent abuse. Do not log request bodies, credentials, private text, full addresses or ciphertext. A specific incident may justify preserving a restricted excerpt. |
| Temporary authentication records and form receipts | Sessions: 12 hours; pending sign-ins and spent CAPTCHA IDs: 10 minutes; PGP ownership challenges: 30 minutes; form receipts: 24 hours. Remove within one hour of expiry. | Authenticate and prevent replay. Expired credentials must already be unusable. Preserve the existing shorter recovery-code reveal cleanup. Scheduled sweeps must replace opportunistic cleanup where necessary. |
| Market branding and logo-upload authorizations | Keep the current name and normalized logo while configured; replace or remove the prior live logo immediately on an accepted change. Logo-upload authorizations expire after 10 minutes and follow temporary-authentication cleanup. | Identify the site before and after sign-in. Keep no logo history in the live branding table. SVG visible text, presentation and supported labels may remain. Backups and database remnants follow the recovery-copy and media rules below. |
| Ordinary recovery backups, snapshots, archived WAL and point-in-time recovery | 30 days from original creation; all copies inherit the original expiry. | Recover recent faults and deletion mistakes. Cover local, off-host, provider, upgrade, rollback and wallet-volume copies. Destroy temporary decrypted exports and rehearsal databases within 24 hours after use. |
| Escrow wallets and dedicated recovery material | Keep necessary material while obligations remain open. Monero: 90 days after the wallet's last financially closed order, subject to its destruction guards. | Recover and settle funds safely. Include wallet labels, descriptors, sealed seeds and dedicated recovery copies. Follow the safeguards below; ordinary snapshot expiry still applies. |

Recipients' local copies and public blockchain records are outside the instance's deletion control.
The operator must explain that limitation. It does not justify retaining the market's own identity-to-transaction mappings indefinitely.
Pseudonyms, hashed identifiers and encrypted bodies can still identify people; they remain subject to this schedule.
Only aggregates that cannot reasonably be linked back to a person may be retained without a personal-data deadline.

## Preservation exceptions

Applicable court orders, preservation duties, statutory recordkeeping and necessary evidence for specific legal claims take precedence over routine destruction.
Do not destroy relevant records while such a duty applies. An open dispute or unresolved escrow also permits necessary operational retention.
Preserve the minimum relevant fields; unrelated messages, profile details and private content keep their normal deadlines.

The preservation owner must record the exception before a purge can affect it. The register must contain:

- A case identifier, scope of records and systems, and whether existing backup copies are specifically required.
- The applicable authority or concrete operational reason, plus the date the exception began.
- The responsible owner, authorized readers and permitted use.
- The release condition or legal expiry, and a next review date no more than 30 days away.

Restrict held material to authorized case handlers in encrypted storage. Do not use it for ordinary analytics or marketing.
Where legally permitted, tell the affected user what categories remain, why and when the next review occurs.
An exception needs documented renewal at each review; a valid legal duty must remain protected even if a review is overdue.
On release, apply the original deadlines and purge already-expired records within 24 hours. Do not start a new full retention period.

A statutory financial-record exception normally preserves the required receipt or ledger fields, not entire messages or delivery payloads.
Extract scoped evidence instead of extending every backup's life where lawful and technically adequate.
If an original backup must be preserved, register that specific copy, restrict access and destroy it when the duty ends.
Delete identifying preservation-register entries within 180 days after release, retaining only the minimum record of the
decision during that period. Any longer legal need requires its own recorded basis; restore-manifest entries follow their separate expiry.

## Deleting data and preventing its return

1. Accept deletion requests through a published authenticated route, with an alternative PGP ownership check when sign-in is unavailable.
   Do not require new identity documents solely to process a request. Acknowledge within seven days and complete ordinary requests within 30 days,
   or sooner when applicable law requires. Explain retained categories and their basis, subject to lawful disclosure restrictions.
2. Revoke sessions and remove sign-in capability when account closure takes effect. For unresolved transactions, provide only the access needed to finish them.
   Delete profile and authentication material independently of necessary transaction evidence. Remove or unlink dependent rows through a reviewed lifecycle design.
3. Run bounded, repeatable sweeps against the inventory, checking preservation exceptions in the same transaction as deletion.
   Include replicas, caches, files, staff exports and provider-controlled copies. Record counts, categories, time and failures without copying deleted content.
4. Maintain a restricted deletion manifest outside the database's rollback scope, containing only opaque record identifiers, deletion scope and time.
   Keep each entry until all recoverable copies containing that record have expired, plus seven days.
   Normally this is at most 37 days after live deletion; a registered older preserved copy extends only the necessary manifest entries.
5. Restore in isolation with user access and payment sending paused. Apply the current manifest and preservation register, rerun expiry sweeps,
   and validate the result before reopening. Retain the existing escrow reconciliation gates and key-index protections.
6. Verify backup expiry daily across every location, including storage versions and provider snapshots. Ordinary backups containing deleted records
   must stay beyond ordinary use until destroyed within the 30-day backup window; use them only for isolated recovery with deletion reapplied.
   A preservation exception must be explicitly registered before a copy exceeds that window.

Deletion from PostgreSQL is logical removal. Old pages, WAL, snapshots and storage media may still contain bytes until reclaimed or destroyed.
Use encrypted storage, bounded recovery copies and documented media sanitization; do not promise immediate forensic erasure.
Destroying a shared backup key is not a substitute for erasing one person's data while other recovery obligations still need that key.

### Escrow and audit safeguards

Do not disable escrow deletion guards, reset address-allocation counters, discard a recoverable settlement or delete wallets with unresolved funds to meet a deadline.
Current database triggers and foreign keys require a reviewed retirement migration before account or order erasure can operate safely.
Keep only the minimum settlement/recovery state during an exception, review it every 30 days and alert the operator when closure is overdue.
Wallets whose balance cannot be verified must not be presumed empty.

After closure and the stated retention window, remove order-linked wallet labels, descriptors, addresses, seeds, files and dedicated recovery material where applicable.
Protect non-reuse and double-payment invariants with minimal technical state; retained allocation counters must not preserve a personal lookup history.
Do not retain identifying address reservations forever as a substitute for a safe retirement design.
Before wallet destruction, tell parties the payment cutoff and that later deposits may be unrecoverable.
Keep current recovery material separately from expiring snapshots; a dedicated wallet archive does not authorize keeping an entire old database.

Keep audit events append-only during their retention window. Implement expiry through a restricted maintenance path that records a minimal purge event.
Audit action text must use fixed descriptions or bounded non-sensitive categories, without dispute narratives, delivery content or personal addresses.
Existing takedown and review reasons in audit text must receive the same minimization and expiry treatment.

Issue signed exports for bounded, documented ranges. Preserve issued bytes while retained, and expire the data file and signature together.
When `AUDIT_EXPORT_RECIPIENT` is configured, preserve the issued encrypted pair unchanged and remove decrypted working copies on schedule.
Encrypted downloads use fresh randomness; export encryption does not protect the live audit rows or remove their deadline.
Account erasure or changing a retained range can change future exports because handles are currently joined from live accounts.
Revise the current promise that the same underlying JSONL export boundary always produces identical bytes before deploying erasure.
Never present a rewritten export as the original signed artifact. A signature proves signing-key origin, not a complete or tamper-proof history.

## Recipient encryption and plaintext limits

Private content must be encrypted locally to the intended recipient's OpenPGP key before submission.
Recipients keep their private keys outside the market. The existing no-JavaScript interface can use an offline PGP client and pasted ciphertext.
Encryption under an application or backup key protects storage but remains readable to an operator holding that key.

| Channel | Required handling |
| --- | --- |
| Shipping instructions and ordinary order notes | Put names, addresses, tracking numbers and other private instructions in recipient-encrypted messages to the vendor or buyer. Keep order notes to status and non-sensitive references. |
| Disputes | Store only the complaint category, procedural state and minimal outcome in plaintext. Send sensitive evidence separately to the other party and the independent staff member handling the case when necessary. Redact unrelated details and disclose who receives each copy. Staff decrypt locally; the server must not store their plaintext working notes. |
| Manual digital deliveries | For content that must never reach the server as plaintext, encrypt the complete payload to the buyer before submitting it through the order delivery form. The form preserves valid buyer-recipient ciphertext; ordinary manual text reaches the running server and is encrypted there before storage. Keep fulfillment and replacement content on the order page. |
| Automatic digital deliveries | Use server-readable delivery only for content deliberately classified as non-sensitive and clearly disclosed to the vendor. Private goods, credentials, access links or individualized secrets require a supported manual per-buyer encrypted fulfilment route or a reviewed workflow that prepares ciphertext without exposing plaintext or private keys to the server. |
| Public listings, profiles and reviews | Allow only material intended for public disclosure. Warn at entry that plaintext and visible image content can be copied by anyone. |
| Notifications, audit actions and logs | Use fixed status text and minimal identifiers. Do not copy private bodies or sensitive notes into these records. Optional XMPP message forwarding may send existing recipient-encrypted armor only with separate administrator permission and recipient consent, preserving normal recipient authorization; the outbox references the original body instead of making another database copy. Providers and clients can retain separate external copies. |

Digital listings support manual fulfillment, reusable automatic content and distinct-unit automatic inventory. New per-order delivery encryption
does not protect the reusable template, inventory or incoming manual text from the running application. Private goods that must
never be server-readable still require locally prepared recipient ciphertext or a reviewed fulfilment workflow that
keeps plaintext outside the server. Do not use placeholder automatic content to bypass checkout requirements.

Require a usable recipient encryption key and show its fingerprint before sending private content.
Encourage users to verify that fingerprint independently, and clearly warn when the key changes.
Require public-key recipient packets; passphrase-only encryption does not satisfy a private recipient channel.
Reject plaintext and visible recipient-key mismatches in private channels. If hidden recipient IDs prevent a match check,
require explicit sender confirmation and disclose that the server could not validate the recipient.
Packet inspection cannot prove that the recipient can decrypt or that the payload is safe.
Do not silently add an operator or moderator as an encryption recipient.

Messages enforce these rules (A-569): a message to an account without a usable encryption key is refused, and so are
plaintext, passphrase-only messages and messages whose visible recipient key IDs are all other keys; nothing is stored.
A message whose recipient key IDs are all hidden is stored only after the sender ticks a confirmation that the server
could not check the recipient, and is marked "recipient unknown". Messages stored before these checks keep their
recorded status. Both buyer and vendor now need usable public encryption keys before creating a draft or checking out,
and the buyer must prove PGP challenge decryption in the current session before purchasing. Opening or replying
to support tickets, direct messages, order correspondence and disputes also requires current-session decryption proof. New initial and replacement manual and automatic per-order delivery records are stored
as PGP armor encrypted to the buyer; unavailable encryption refuses manual delivery or leaves automatic delivery
pending without storing plaintext. The running server sees manual text, unit inventory or the reusable listing template before that
encryption, and historical plaintext delivery records are preserved. This does not satisfy a requirement that private
plaintext never reach the server. Only encrypted new deliveries are eligible for explicitly consented XMPP forwarding;
historical plaintext deliveries and reusable templates are never forwarded.

Support ticket bodies are locally encrypted to both the opener and current assigned staff member, with no
plaintext subject. Assignment and administrator escalation change workflow permissions, not prior ciphertext.
The ticket's metadata history remains server-readable, and generic closure/ignore metadata can reach prior
active staff handlers without disclosing the body or granting access to an escalated thread. Tickets and their
external notification copies have separate retention boundaries. With retention enabled, ticket bodies expire
90 days after creation or earlier when their opener or sender is closed or erased, except while a related account
hold preserves the ticket. Holds cover current and historical participants, message senders and staff in workflow
history as well as the opener/current assignee. Closed or Ignored metadata expires 90 days after its last update
only after all bodies are gone and holds are released; Open metadata remains for staff workflow. Journaled deletion
and restore replay apply to tickets and bodies, while external provider/client copies cannot be recalled.

PGP does not conceal sender, recipient, time, message length or visible encryption-key identifiers.
The server also stores recipient-match status and a count of additional visible recipient keys.
Retain only metadata needed for routing, access control and documented dispute coordination, under the message deadline.
Never advertise relationship anonymity or hidden transaction metadata as a consequence of content encryption.

## Operator-access disclosure

Publish this access model on the instance's records page and link it from registration, account, message, dispute and delivery forms.
List actual controls and unresolved gaps, deployed retention periods, backup expiry, exceptions and a deletion contact.
Update the [README disclosure](../README.md#how-it-works) and implementation status when behavior changes; do not announce guarantees from this policy alone.

The following describes **current access**, including the difference between application roles and control of the host:

| Information | Access through the application | Access with host/database control or a decrypted backup |
| --- | --- | --- |
| Support tickets | The opener and authorized current staff obtain ciphertext; administrator-only/escalated tickets exclude moderator staff access while preserving the opener’s own participant view. Status, assignment and history are procedural metadata. A new assignee cannot decrypt earlier bodies unless they already held a matching private key. | Ticket bodies remain ciphertext; participant links, IDs, states, assignment history and timing are readable. Prior recipients can retain copies. Optional XMPP copies require the separate consent and key checks and may survive outside the market. |
| PGP message bodies | Sender and recipient obtain ciphertext; anyone holding a matching private key can decrypt. Staff have no general inbox reader, but can receive messages as recipients. | Ciphertext and metadata are available. Proper recipient-encrypted content cannot be decrypted without a recipient private key. |
| Message relationships and timing | Parties see their messages. Independent staff see counts and latest times of messages from each disputing party to staff during an open dispute. Those counts do not prove which order a message concerned. | All retained sender/recipient links, times, lengths and stored recipient-status fields are available. Visible key IDs remain in ciphertext. |
| Orders, notes and dispute text | Buyer and vendor; nonparty moderators and administrators when a dispute record, flagged payment or open escrow alert grants order access. Dispute access can persist after resolution. | All retained plaintext, participant links, status history and payment associations are readable. |
| Digital originals/replacements, reusable templates and unit inventory | New per-order delivery armor: buyer and vendor, and independent staff when a dispute record exists; decrypting it needs the buyer's private key. Historical plaintext deliveries keep their existing reader permissions. Reusable templates and undisclosed unit inventory: the vendor through listing management. | New stored delivery ciphertext requires the buyer's private key. Historical plaintext deliveries, reusable templates and retained unit content remain readable; a running compromised application can capture incoming manual text and automatic content before encryption. Unit contents and fingerprints expire together under their listing or order deadline; consumed entries never return automatically to stock. |
| Accounts and authentication | Administrators see handles, roles, suspension and factor status. A public key is shown to staff, to the owner's trading partners and accounts the owner has messaged, to every signed-in account when the owner is an active vendor, a listed staff member or the staff contact, and, for a moderator or administrator, to both parties of every disputed or resolved order (see "Who can be messaged" in implementation-status.md); vendor-profile keys follow the site's public or members-only access policy. Passwords and TOTP secrets are not exposed in ordinary views. | Password hashes can be attacked offline; they are not plaintext passwords. TOTP and temporary recovery reveals can be opened with their application secrets. Public keys and retained profile data are readable. |
| Audit, notifications and public content | Users see their own activity and notifications; administrators can inspect/export audit events, subject to any configured private ingress. With `AUDIT_EXPORT_RECIPIENT` set, downloaded and host-written exports require the offline recipient's private key to decrypt. Published catalog, reviews and eligible images follow the site's public or members-only access policy. Staff visibility rules still apply to hidden material. | Live audit events and other retained database records are available. Optional offline-key encryption protects export files, not the live database. Public copies may survive elsewhere after removal. |
| Escrow and wallet recovery material | Parties see permitted escrow/payment details; independent staff see authorized cases. | Payment mappings and retained sealed keys/seeds are available. Application-sealed material needs its application secret; sealed Monero seeds need the offline recipient's private key. Wallet files live outside database backups and need access to their separate store. The market controls custodial Monero funds. |

Operators with privileged control can change the service, capture future server-readable submissions or substitute a public key.
Recipient encryption therefore depends on independently verified keys and private keys remaining outside the server.
This policy requires least-privilege access and audited case handling; it cannot make a compromised host trustworthy.

## Adoption and acceptance

Within 30 days of adopting this policy, the operator must inventory every data store and copy, record applicable preservation duties,
publish the current access gaps and deletion contact, and apply the backup and log limits with monitoring.
Before deleting any existing copy, verify needed recovery coverage and preservation requirements; do not create a fresh copy merely to reset its expiry.
Within 90 days, maintainers and the operator must implement and verify live expiry, deletion requests, restore filtering and private-channel enforcement.
An unmet milestone must remain visible as a gap with an owner and remediation date.

Apply deadlines to existing records using their original dates. Notify users at least 30 days before the first bulk purge so they can save messages and deliveries.
Where historical account activity is unknown, offer a one-time reconfirmation period of at most 90 days, then close accounts with no evidence of continued use.
Do not invent a last-use date or grant legacy records a new indefinite lifetime. Minimize existing plaintext rather than silently encrypting it to an operator key.

An instance may claim enforcement only after it demonstrates:

- Eligible accounts, individual messages, private payloads, closed orders and audit records expire, including dependent rows and wallet mappings.
- Scoped exceptions block only relevant purges, survive restores, and release back to the original deadlines.
- A backup predating an account/message deletion restores without exposing the deleted data; payment recovery protections remain effective.
- Every backup location and storage version expires on schedule, and temporary recovery copies are removed.
- Private fields reject plaintext and recipient mismatches, key changes are disclosed, and server logs/audits contain no submitted private bodies.
- Ordinary users, staff and host-level access match the published disclosure, with retained ciphertext distinguished from plaintext.

Alert the operator on missed sweeps, overdue non-legal exceptions and failed backup destruction.
Review retention justifications and access controls quarterly and after schema or delivery changes; review the policy at least annually.
Report compliance using aggregate counts and age ranges, without retaining a replacement history of deleted users.

## Basis for the policy

The defaults implement purpose-based retention and minimization rather than a universal legal retention period.
The ICO's [storage limitation guidance](https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/data-protection-principles/a-guide-to-the-data-protection-principles/storage-limitation/)
supports documented, justified periods and distinguishes anonymization from pseudonymization.
Its [erasure guidance](https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/individual-rights/individual-rights/right-to-erasure/)
addresses backups put beyond use and exceptions for legal obligations and legal claims.
These sources inform the design; applicability and any specific statutory duration must be recorded for the instance.
