# Current source: Monero only

The current source also adds a native `opsecmkt` executable with embedded UI,
deployment commands, key generation/import, isolated upload gateway and a signed
dependency-version worker. Build with `go build -o opsecmkt ./cmd/server`; see
[Binary installation](docs/install/binary.md). Existing server flags and the shell
installer remain supported. The native server now defaults to `127.0.0.1:8080`;
containers continue to set `ADDR=:8080` explicitly. Rebuild after changing UI files.

The new deployment controller initializes a new private directory and refuses
overwrites. It does not automatically adopt existing Compose volumes or migrate
an old installer directory. Import existing secrets explicitly when preparing a
deployment; preserve the Monero wallet secret, seed recipient, database, retention
state and Tor identities. Do not initialize a second instance against live payment
state as an upgrade procedure. Backups/restores and upgrade adoption still use the
existing documented operations workflow.

## Listing review (migration 136)

Migration 136 adds `products.review_state` (`approved`, `pending` or `rejected`; every existing listing is
`approved`), `products.review_rejection_reason` (empty; a staff rejection's reason, shown to the vendor until their
resubmitting edit), the per-vendor `users.listing_auto_approve` flag (off) and the `listing_review_mode` setting with
the value `publish`. A rejection is not a takedown: it archives nothing and needs no administrator. Nothing
changes until an administrator chooses **hold for review** under *Admin → Vendors → Listing review*: listings stay
public and new ones publish at once, as before. Rolling the application back to a build without this migration
leaves the columns unused; a listing still pending or rejected would then be public again, so approve pending
listings, and take down rejected ones you do not want public, before downgrading.

## Encrypted support tickets (migration 133)

Migration 133 adds the support-ticket workflow at `/tickets` and `/ticket?id=…`. Ticket IDs and procedural history
are plaintext metadata; every initial body and reply is PGP-encrypted locally before submission, with no plaintext
subject field. This is separate from the direct-message inbox. Users and staff handle routine support through
the product pages; no SSH or manual database changes are needed for an ordinary ticket.

Before opening a ticket, the opener saves a usable public encryption key and selects an active staff recipient
whose usable key is visible through the staff list or configured staff contact. The form must not expose hidden
staff accounts. Encrypt each body to the opener and the current assigned staff member. An assignment or escalation
does not re-encrypt historical bodies or give a new assignee the old recipient's private key; arrange the next
reply for the current recipients. Already downloaded ciphertext or plaintext cannot be recalled.

Tickets record Open, Closed or Ignored state, current assignment and administrator escalation. The opener stays
a participant; moderators cannot access administrator-only or escalated tickets as staff, but retain their own
personal participant view if they opened one. Active authorized staff can manage normal tickets, including
assignment and escalation while Open. The opener can close their own Open ticket; ignoring or reopening needs
staff permission. Administrator queue access does not imply the ability to decrypt a body that was encrypted to
another staff member's key. Prior active staff participants can receive generic closure/ignore metadata after
reassignment; former moderator handlers of an escalated ticket gain no access to its protected thread. Frontend and XMPP
notifications retain current participant/role restrictions, and forwarding a ticket's encrypted armor needs the
same separate global and recipient content consent as encrypted messages. See the
[support instructions](docs/operator-guide.md#encrypted-support-tickets).

With retention maintenance enabled, ticket bodies expire after 90 days or earlier if their opener or message
sender is closed or erased, unless a relevant account hold applies. Holds cover current and historical ticket
participants and workflow staff. Closed/Ignored metadata is removed only after 90 days without an update, once
all bodies are gone and no hold applies; Open metadata remains for the workflow. Ticket and message deletions
are journaled for restore replay. This does not remove independent XMPP/client copies.

## PGP message forwarding and encrypted delivery (migration 132)

Migration 132 adds separate default-off permissions for forwarding already PGP-encrypted message armor over
XMPP. The administrator must enable **Allow PGP-encrypted message contents in XMPP** under
`/admin/site#xmpp-outgoing`, and each recipient must choose **Include PGP-encrypted message contents** under
`/account#xmpp-notifications`. A recipient also needs enabled XMPP and a usable PGP public encryption key;
PGP sign-in and proof of key ownership are not required for this preference. Confirm both settings with the
account's password and authenticator code when enrolled. Existing messages are not sent again.

Default alerts remain metadata-only. With both permissions enabled, a new received-message alert can include
the original encrypted armor, without decryption or a plaintext copy. Normal message/order authorization and
staff audience restrictions continue to apply. Global permission changes advance its revision and suppress
content from earlier queued alerts while retaining their summaries; changes to the recipient's preferences
invalidate earlier queued deliveries too. A current visible recipient key match is required: hidden-recipient
messages, replaced keys and unavailable content keep the summary alone. New stored buyer-encrypted digital
deliveries are eligible only for that buyer's explicitly consented alert, never another party's staff notice. XMPP services can retain notification text, routing metadata and ciphertext even though the
forwarded contents retain their PGP protection. Order-linked alerts include their order reference; direct
messages keep message references, while support tickets use their own workflow and ticket IDs (migration 133).

New drafts and checkout now require usable PGP public encryption keys on both buyer and vendor accounts,
for physical and digital listings. Add or renew missing, expired or unusable keys under `/account`; PGP sign-in
and key-ownership proof are optional. Existing orders remain accessible for their normal read, cancellation and
dispute actions if a key later becomes unusable.

New manual and automatic per-order digital deliveries are stored encrypted to the buyer's PGP key. Buyers
need their corresponding private key and PGP client to read the armor. Automatic-delivery listing templates
remain server-readable, and manual delivery text reaches the running application before encryption. A delivery
encryption failure refuses manual delivery or leaves automatic delivery pending without saving plaintext.
Migration 132 does not rewrite historical plaintext deliveries or erase their backup copies; those records
are never forwarded as XMPP contents. Review the updated order and delivery wording with vendors before
relying on the new workflow.

## Optional XMPP notifications (migration 131)

Rebuild the application; startup applies append-only migration `131_xmpp_notifications.sql`. It adds private
recipient opt-in/version state, notification audiences and a durable notification-delivery outbox. Existing
accounts start unsubscribed, and existing notifications are not backfilled for XMPP. Migration 098's removal of
old XMPP profile values remains unchanged: restoring an old backup does not constitute a new opt-in.

Historical notices addressed to current administrators, and recognizable old administrator broadcasts even
on former staff accounts, become administrator-only. Historical notices lack reliable audience metadata, so
the conservative restriction also hides a current administrator's old personal notices if they are later
demoted. New personal notices keep their normal owner access after a role change.

An administrator must configure an existing outgoing XMPP sender account under `/admin/site#xmpp-outgoing`
(server, port, verified STARTTLS or direct TLS, password and delivery switch). Each recipient, staff included,
then opts in under `/account#xmpp-notifications`. These forms require password/TOTP confirmation. Preserve the
application secret root so the encrypted outgoing password remains readable after a restart or restore.
A blank password field retains the existing credential; saved credentials are never returned in page settings.
See [XMPP setup](docs/operator-guide.md#xmpp-notifications) before enabling outbound access.

XMPP uses the same newly created, authorized frontend notification text and recipients. Admin-only alerts
remain administrator-only, and staff eligibility is checked again before frontend display and delivery after
role changes. Independent moderators now receive dispute notices for Monero escrow as well, while resolution
remains administrator-only. Payment-review escalation notices are administrator-only. Changing a recipient
address or opting out invalidates queued deliveries for the earlier preference. Delivery failure leaves the
frontend notification available; retries after ambiguous sends can duplicate alerts.

Outgoing XMPP connections are direct, including in Tor mode; no automatic Tor egress or end-to-end encryption
is added. The XMPP service can retain message bodies, sender/recipient addresses, timing and connection metadata.
Use disposable accounts to check the actual server and recipient before relying on delivery. Turning outgoing
XMPP off cancels pending sends when the worker processes them, while frontend notifications continue. Old backups can restore earlier recipient or
sender settings and queued state: review them before re-enabling outgoing delivery on a restored installation.

## A Monero re-request waits for a missing deposit (A-614)

No migration and no new setting. An administrator's re-request of a failed Monero settlement spends exactly the coins of
the transaction it replaces. If a deposit those coins came from is no longer listed by the escrow wallet (a
reorganisation or a double spend since the re-request was carried out), the new settlement now stays requested
(`send_error = 'deposit-missing'`), staff get one order note and notice each time it starts waiting, and it is built and sent once the wallet lists
the deposit again. It has no bounded wait (a first release or refund waits twice the threshold in blocks, A-554),
because it can be built from no other coins; past 24 hours it is an escrow alert like any settlement still requested,
and its monthly wallet is kept from destruction while it waits. It can never pay twice: both transactions spend the
same coins, and the database refuses any others.

An earlier build failed such a re-request `nothing-to-settle` while it still claimed the deposits (A-613 below, *Except while it
still claims deposits*). The upgrade does not revisit those rows: each keeps its alert, cannot be dismissed, is never built
again even when the deposit is listed again, and its coins are settled with the operator outside the market. Nothing
in the market clears that alert, so the order stays on the desk and under retention's `escrow_alert` reason. To list
them, use the query in the *Except while it still claims deposits* item below.

## Optional moderation proposals (migrations 125 through 127 and 134)

Application startup applies the append-only moderation tables/triggers and
offline review receipts. Enqueueing and inference remain off, so existing
installations need no model, worker, inference mount, network request or new
listener. Review backup capacity and sensitive-record access before enabling
the separate `opsecmkt moderation-worker`; it enqueues subsequent supported
changes, not a backfill of the current catalog.

Migration 127 adds optional normalized-image dHash metadata. Upgrade the app and
apply its migrations **before** upgrading the separate gateway: the old app's
strict upload envelope rejects the new field. Then upgrade the gateway to
compute dHash during its existing decode/normalization step;
the application still receives bounded metadata and normalized bytes only.
Old gateways may omit dHash, preserving exact SHA256 duplicate comparison.
Existing images are not decoded again or backfilled. Perceptual similarity
is advisory and cannot establish whether a listing's claims match a photo.

Migration 134 changes new capture to digests and limited metadata. The worker
reads source prose into memory only for the unchanged captured revision; stale
or removed sources receive HOLD. New archived inputs retain coded metadata only;
completed new jobs remove their active raw actor link. Legacy snapshots and
immutable proposals remain byte-stable and outside existing retention sweeps.
This is prospective minimization, not historical erasure or anonymization. Keep
the feature off where remaining legacy retention conflicts with policy.

The `moderation-v2` protocol rejects model-written summaries/evidence and retired
v1 pins. Go derives review text from codes, including the current presentation of
legacy decisions. Upgrade the app/migrations before the controller. Completed
metadata-only jobs are skipped by `--replay`; its output reports the count.

The `moderation-v3` protocol refuses v1 and v2 pins. Models may
no longer assert Go-owned failure, protocol, routing, deadline or deterministic
codes; Stage A findings stay in the final result; prompts carry evidence aliases
instead of hashes. The runtime must also serve llama.cpp's `/tokenize` and
report `stop_type` (or the older `stopped_*` flags), `truncated` and
`tokens_evaluated`. A prompt that cannot fit beside the output limit in the
launcher's 4096-token context, or an unfinished generation, gets `MODEL_FAILURE`
and human review. Stored v1/v2 decisions keep their bytes and still render.

The current `moderation-v4` protocol refuses v1, v2 and v3 pins: change the
controller's `--prompt-version` to `moderation-v4` when upgrading it. Prompt
data now writes `[` and `]` inside JSON strings as `\u005b` and `\u005d` (same JSON
meaning), because llama.cpp's `/completion` parses special tokens in the prompt
and bracket-spelled control tokens such as `[INST]` in vendor text could
otherwise become real ones. Instructions and grammars are unchanged. Text with
many brackets costs more tokens and reaches human review sooner. Stored v1–v3
decisions keep their bytes and still render.

Capture now has atomic limits: three active jobs per actor, 256 pending globally,
4096 lifetime allocation slots and 256 MiB reserved logical payload by default.
Each allocation prepays 512 KiB, so the default bytes bind at 512 allocations.
Legacy usage is inventoried once; no historical data is removed to fit a limit.
Private queue status shows usage, limits and bounded coalesced overflow warnings.
At saturation the original marketplace write still commits without a fabricated
clean result. Raise an approved budget using the controller's bounded capacity
flags; lowering below allocated usage is refused. Database/index/WAL/backup disk
size is not bounded by the logical payload reservation.

New gateway hashes omit weak/low-contrast similarity evidence. Useful legacy
hashes and exact SHA256 matching remain; no extra wire field, decoding boundary,
rotation/crop guarantee or image-understanding feature is introduced.

Updated gateways compute dHash with an area-averaging filter, so recompressed,
resized or slightly cropped copies match more reliably; comparison also tries
each hash's horizontal mirror. Hashes made by an older gateway and by an updated
one compare less reliably with each other. Nothing is backfilled or rewritten:
the worker compares only recent uploads, so the mix ages out as new uploads
arrive. Upgrade order is unchanged (app first, then gateway).

Weak instruction-like words ("you are", "act as", "confidence", "decision" and
similar) no longer mark an item `INJECTION_SUSPECTED` or escalate it; they add
`HUMAN_REVIEW`, as does any recognised external contact or payment destination,
so such items reach the human queue instead of automatic history. Strong
markers, now also covering several languages, simple encodings, code names and
adjacent-field splits, still escalate. Expect fewer administrator escalations
and more ordinary review-queue items; stored decisions are unchanged.

The worker never changes a listing, account, order or payment. Private
administrator queue actions and optional signed offline actions annotate review
metadata only. Deadline expiry holds current marketplace state. Keep existing
payment authorization/recovery gates and all ordinary staff action checks.

Existing application-only redeployment does not provision/restart a model or
controller. Preserve their reviewed configuration, weights, durable database
jobs and report/action verification pins independently. Do not delete immutable
moderation records to roll back a worker; disable future enqueueing with its
`--disable` flag while preserving evidence and existing jobs.

Optional signed offline review requires a separately pinned public action key
and a stable installation audience on the app, with its private key kept on the
offline workstation. It is distinct from the controller's online packet signer
and OpenPGP export recipient. See the [moderation guide](docs/moderation-agent.md)
and [review runbook](docs/moderation-escalation-runbook.md) for setup, defaults,
capability gaps and deployed-runtime acceptance gates.

## Stuck failed Monero settlements can be cleared (A-613)

Migration 128 runs at startup and adds the table `xmr_settlement_hand_settled`, with triggers that refuse a
re-request of a settlement recorded there and a deposit claim (`payments.xmr_settlement_id`) moved onto one; there is no
new setting. Versions 125 to 127 are left free for another
branch; the startup migration applies any version it has not recorded, so they apply whenever they arrive. It fixes the
A-560 alerts below that could never clear: a Monero settlement that failed before any transaction was built is never
re-requested, and one with a stored transaction whose re-request was refused because that transaction is in the chain
or a coin it spends is spent is never superseded, so their alerts kept the staff badge above zero, stayed on the desk
and kept the order's escrow records under retention's `escrow_alert` reason for good.

- **Refunds too few disappear at once.** A failed refund with `send_error = 'too-small'` is no longer an escrow alert,
  as a Bitcoin refund too few never was: its deposits are already left out, flagged under *Payment review* and returned
  by a stray refund. Their payment-review flags are unchanged.
- **Nothing-to-settle failures are dismissed, never paid by hand.** One that failed `nothing-to-settle` sent nothing,
  because no deposit of its escrow counted at that time, and nothing is owed for it now. Do **not** pay it by hand: if a
  deposit of that escrow is listed again, the market handles it automatically as usual (a stray refund returns it when
  the order's coins go back to the buyer) and flags it for review or raises its own alert, so a payment by hand would pay
  twice. An administrator who is not a party opens the order (from *Escrow alerts*) and uses *Dismiss settlement N
  (nothing owed now)* under the settlement, with a 10–500 character note and the password. That clears its alert; the
  note is staff-only and recorded once (kind `nothing-owed`), with an audit row. It works while `ESCROW_XMR` is off.
- **Except while it still claims deposits.** A failed settlement with no transaction built that still claims deposits
  (`payments.xmr_settlement_id`; a re-request that failed `nothing-to-settle`) is never dismissed: its coins are in the
  escrow wallet and may be owed, and the market does not settle them on its own. Its alert stays, and says to settle
  with the operator. To list them:
  `SELECT xs.id, xs.order_id FROM xmr_settlements xs WHERE xs.state = 'failed' AND xs.tx_hash IS NULL AND EXISTS (SELECT 1 FROM payments p WHERE p.xmr_settlement_id = xs.id);`
- **Final re-request refusals are marked settled by hand.** For a failed settlement with a stored transaction whose last
  re-request was refused `on-chain` or `key-image-spent`, the alert asks you to check the escrow wallet and the chain
  (that transaction, or another spending the same coins, may have been sent), settle anything still owed with the
  operator and then use *Mark settlement N settled by hand* the same way (kind `settled-by-hand`). It is never
  re-requested again. One refused `in-pool` is re-requested again later instead, and one with no refused re-request is
  re-requested first.
- **Retention resumes.** Once an order has no open alert, retention's next financial check no longer reports
  `escrow_alert` for it: the exception resolves, or the next blocker is reported, and the order's escrow records close
  and expire on the normal schedule. Nothing is deleted at upgrade. Account erasure treats the administrator who
  recorded a disposition as referenced, as it does other staff records.

To list the failed settlements that still alert after the upgrade:

```sql
SELECT xs.id, xs.order_id, xs.kind, xs.send_error, xs.tx_hash IS NOT NULL AS stored,
  (SELECT r.state || ' ' || r.reason FROM xmr_settlement_rerequests r WHERE r.settlement_id = xs.id ORDER BY r.id DESC LIMIT 1) AS last_rerequest
 FROM xmr_settlements xs
 WHERE xs.state = 'failed' AND NOT (xs.tx_hash IS NULL AND xs.kind = 'refund' AND xs.send_error = 'too-small')
   AND NOT EXISTS (SELECT 1 FROM xmr_settlement_hand_settled h WHERE h.settlement_id = xs.id) ORDER BY xs.id;
```

Rolling back to a build without migration 128 is refused at startup (the database records a migration it does not
know); restore the pre-upgrade backup instead.

## Failed Monero settlements are escrow alerts (A-560)

No migration and no new setting. A failed Monero escrow settlement that was not superseded, and one set aside because
building it stopped the escrow wallet-rpc, are now escrow staff alerts: listed under *Escrow alerts* on the moderation
desk, counted in the staff badge, readable by staff and named in the order page's staff help (also when its deposits are
flagged, with a link to *Payment review*). The entry says whether the settlement can be re-requested (its transaction is
stored: *Re-request settlement N*; or that a re-request is waiting, that the last one was refused and why, so check the
escrow wallet and the chain, or that Monero escrow is off), built again (*Build settlement N again*), or neither, in which case anything still
owed is settled by hand with the operator (since A-613, above, a *nothing-to-settle* failure is instead dismissed and
never paid by hand; [operator guide](docs/operator-guide.md), *Failed and set-aside settlements
are escrow alerts*). A payment-review row of a deposit that a failed settlement spends now says that it failed instead
of "the order's release or refund settles it". The parties see no change. Settlements that failed before the upgrade
appear at once. One that failed without a stored transaction is never re-requested, so its alert did not clear (A-613,
above, makes a refund too few no alert and lets an administrator dismiss any other as nothing owed); as for
every open escrow alert, data retention keeps that order's escrow records and lists it with the reason `escrow_alert`
(reopening one whose financial records it had closed). To list them:

```sql
SELECT id, order_id, kind, send_error, tx_hash IS NOT NULL AS stored FROM xmr_settlements
 WHERE state = 'failed' OR state = 'requested' AND send_error = 'build-set-aside' ORDER BY id;
```

## Paused sign-in rescue for every TOTP account (A-571)

No migration and no new setting. While a handle's sign-in is paused after failed passwords, any account with TOTP
on, not only a sole administrator, now signs in with its password and current authenticator code typed together on
`/login`. The code is checked before the password: a wrong or used code, an unknown handle or an account without TOTP
gets "Invalid handle or password" with no password check and never spends the account's sign-in code budget, so
guessing on this path can no longer spend the codes `/challenge` needs without a current code (the behaviour since
2026-10-13 charged every attempt). Wrong codes spend only the rescue path's own budget of ten per account per ten
minutes; once it is spent, the rescue is closed for that account until the window ends, while `/challenge` and normal
sign-in after the pause still work. Someone who knows the handle and keeps both the pause and that budget spent can
block password sign-in for that account for as long as they keep it up. A right code spends one of the account's ten
code attempts per ten minutes whether the password is right or wrong. The refusal
log line is renamed from `sole-admin rescue refused attempts=N` to `paused sign-in rescue refused attempts=N` and now
also counts wrong codes, unknown handles and accounts without TOTP; update any log alert that matches the old text
(for example `grep 'sign-in rescue'`). The sign-in page's help under the authenticator field says so.

## Official addresses in PGP sign-in codes (A-572)

The encrypted PGP sign-in code now reads, after decryption, "Sign-in code for HANDLE at MARKET NAME", the UTC time
it was requested and until when it is valid (at most 10 minutes), the market's official addresses and an instruction
to enter the code only on one of them, then `Code: …` on its last line. Users paste only that code, as before;
verification is unchanged. The addresses come from the new optional `OFFICIAL_ADDRESSES` setting, never from the
request: a comma-separated list of at most 8 HTTPS or HTTP v3 onion origins (your onion, your mirror onion, your
clearnet origin), for example `OFFICIAL_ADDRESSES=http://<56 characters>.onion,https://market.example`. Add it to
`.env` (Compose), `--set OFFICIAL_ADDRESSES=…` (native `init`) or `--official-addresses` / the environment (native
`serve`), then recreate the app. Never list the private administrator onion; a native configuration that records the administrator onion's address refuses it, but nothing else checks the list.
Left blank (the upgrade default), the code says "no official address configured". An invalid value stops startup
with a message naming the variable. No migration.

## Messages their recipient cannot read are refused (A-569)

No migration and no new setting. `/messages` now refuses, storing and notifying nothing, a message to an account
without a PGP key that can receive encrypted messages (409), a passphrase-only message and one whose visible recipient
key IDs are all other keys (400); each refusal says what to do. Ciphertext with every recipient key ID hidden
(`gpg --throw-keyids`) is sent only after the sender ticks a confirmation on the returned compose form, and is marked
"recipient unknown". Messages already stored keep their status, including "NOT to recipient’s key". An account without
a usable key can still send but can no longer receive messages, so ask staff and vendors to keep a current key saved.

## Staff uploads from the private administrator connection (A-551, A-590)

With isolated uploads and a private admin onion (`ADMIN_SOCKET`), the avatar panel (administrators and moderators)
and an administrator's listing Photos panel on the private connection now authorize one upload with the account
password (and authenticator code) and send the file to the upload gateway origin in `BRANDING_UPLOAD_ORIGIN`, the
setting the logo upload already uses. Upgrade the gateway binary on the upload host together with the app: an older
gateway does not know the new `/uploads/staff` form and refuses it, storing nothing. Without
`BRANDING_UPLOAD_ORIGIN` those panels now say why no upload is possible instead of offering a form that always
failed. The logo form now also follows the gateway's scanner report and pauses while it is missing, stale or not
*Ready*. No migration and no new setting; unused authorizations are held in memory and end with an app restart.
Uploads on the public address and integrated (single-host) uploads are unchanged.

## Upload gateway scanner reports (A-552)

With isolated uploads, the app now shows the upload gateway's scanner state and
pauses its avatar and listing-photo forms until the gateway reports a ready
scanner. Upgrade the gateway binary on the upload host together with the app: an
older gateway sends no reports, so the forms stay paused (the panel says
"Scanner status unknown — gateway not reporting") although that gateway still
scans uploads. A new gateway in front of an older app logs that its reports are
not delivered and otherwise works as before. No migration and no new setting:
reports use the existing upload backend address and client certificate.
Integrated (single-host) scanning is unchanged.

## Market fee shown read-only; host changes announced (A-550)

The Monero escrow market fee stays host configuration (`ESCROW_XMR_FEE_BPS`, `ESCROW_XMR_FEE_ADDRESS`); no
administrator form or settings row changes it. **Admin → Payments** now shows the rate and the full fee address
read-only with the command that changes them. With `ESCROW_XMR=on`, each start compares the configured fee with the
last values it recorded (settings row `escrow_xmr_fee_seen`, never read as the fee) and, on a change, writes one audit
row naming the old and new rate and address and notifies every administrator once. The first start after this upgrade
has no record yet, so it announces the configured fee once ("none recorded before"). No migration and no new setting.
See the operator guide's *Market fee* paragraph under Monero escrow.

## Service status and Monero escrow health (A-553)

*Service isolation and updates* gains a row for the market's own build and a
Health column, and Admin → Payments shows the Monero escrow watcher's health
([Service status and health](docs/operator-guide.md#service-status-and-health)).
No migration and no new setting: the watcher stores its health in one `settings`
row (`escrow_xmr_health`) after each pass, so the Monero rows read "No Monero
escrow watcher pass recorded yet" for the first seconds after the upgrade. The
app makes no new outbound connection; once an hour per process the watcher also
calls `get_version` on the escrow wallet-rpc (and on a restricted daemon RPC).
Release binaries now carry their commit; a Docker image built from source shows
`development` without one. The 10-minute payment-provider outage notice now
points to `/admin/payments#xmr-escrow-health` instead of the watcher logs.

## Native production encryption prerequisite

The native production controller now verifies guest-managed LUKS2 storage before
initialization writes private state. It requires an encrypted Linux
operating-system root, local Docker/containerd storage, the installation, private
inputs and persistent retention state. It records expected LUKS identities and
rechecks them on `install`, `up`, `redeploy` and `doctor`. Development behavior is
unchanged. The standalone `serve` process and legacy shell installer do not
configure operating-system encryption or replace these host controls.

Production starts now require Linux root, systemd, no active swap and a
persistent root-owned operator binary. They install per-project Docker and
containerd pre-start storage checks and reload systemd without restarting those
daemons. The former tmpfs/RAM-unlocked production operator directory is no longer
accepted; restore retained configuration onto persistent encrypted storage after
the reviewed migration. Development RAM workflows remain available. A RAM-only
production boot/unlock design needs separate implementation and verification.

Upgrading the executable does not convert an existing unencrypted root. Such an
installation can still use `plan`, `status`, `logs` and `down` for inspection or
shutdown, but must complete a reviewed migration before the new production
controller starts it. Prepare a fresh encrypted root, verify recovery and restore
the retained configuration, stable keys, onion/wallet state and current retention
ledger. Then run:

```sh
./opsecmkt deploy storage bind --dir /srv/opsecmkt/market-prod
./opsecmkt deploy doctor --dir /srv/opsecmkt/market-prod
```

`storage bind` updates verified storage bindings while retaining the installation
receipt and existing keys; it is not a force bypass or data-copy command. Install
and test storage-dependent startup before resuming. Review
[encryption setup and migration](docs/install/server-hardening.md#prepare-encryption-before-running-setup)
and the [restart/recovery runbook](docs/operator-guide.md#native-production-storage-and-restarts).
Agree replacement resources, temporary overlap cost, downtime and rollback before
cutover. Do not format the authoritative root or rerun `init` as an upgrade step.

`OPSECMKT_PROFILE=prod` refuses integrated uploads and known local dependency
endpoints, requires PostgreSQL TLS with certificate validation and authenticated HTTPS Monero services,
and requires both app TLS and a private mutual-TLS upload listener. Switching to
isolated uploads requires routing browser traffic through the separate upload
gateway before enabling this profile. The gateway processes originals and forwards
normalized JPEGs for listing photos and avatars, and normalized JPEG or restricted
static SVG for logos. Administrators protected by `ADMIN_SOCKET` cannot upload
listing photos or avatars through the public gateway. Logo uploads use a separate
preconfirmed, one-use authorization; preserve private administrator ingress. Dependency and
service-placement notices appear only in the administrator panel. The dependency
worker needs its own signing seed; mount only its public verification key and
signed report into the application. The app does not fetch release information.

The current source supports only Monero (XMR). Bitcoin checkout, key registration, signing, node provisioning and payment controls have been removed. Listing forms and new orders use XMR. Monero escrow remains custodial and off by default; its network and wallet safeguards remain in place.

There is no live deployment or payment cutover for this change. The release notes below describe earlier alpha versions and are historical.

## Site access and registration

Supporting releases initialize missing database settings for site access and new
registrations. The defaults are `public` and `open`; set `INITIAL_SITE_ACCESS`
and `INITIAL_REGISTRATIONS` before the first supporting startup to choose other
initial values. Existing saved settings are preserved. These variables, and the
equivalent native `--site-access` and `--registrations` flags, seed missing values
only; they do not override later administrator choices on restart.

Upgrade every application replica before relying on members-only access or
closed registration. Older binaries ignore these policies, so a rollback can
reopen guest browsing and account creation. Keep an external maintenance or
access gate in place while an older binary can serve requests during rollback.
Changing access cannot recall content already downloaded or publicly cached.

After upgrading, review **Admin → Security & access → Site access and registration** and verify both
a signed-out session and an existing account. Follow
[site access and registration](docs/operator-guide.md#site-access-and-registration)
for the choices and checks.

## Market branding

Migration 120 adds the current logo and temporary logo-upload authorizations.
Existing market names remain unchanged, and no custom logo is installed by the
upgrade. The header and footer now follow the saved name when using the text
wordmark. Back up the database before upgrading; older binaries that do not know
this migration refuse startup. Follow the existing database rollback procedure
rather than deleting migration records.

Update the application and upload gateway to the same supporting release before
uploading logos. For isolated uploads from a private administrator onion, set
`BRANDING_UPLOAD_ORIGIN` to the fixed public gateway origin. A blank setting
leaves that new upload flow unavailable while the existing app continues to run.
Logo confirmation stays private; only a scoped, expiring upload authorization and
the file go to the gateway. General private-admin ingress stays unchanged.

The name and logo are public even on a members-only site. Verify the header,
footer and sign-in page after an upgrade, name change, logo replacement or reset.
See [market branding](docs/operator-guide.md#changing-the-market-name-and-logo)
for the supported files, size limits and upload steps.

## Vendor onboarding

Migration 121 adds vendor applications, the verified-vendor mark and the vendor
policy settings (no bond, manual approval). Every existing vendor receives an
approved application and keeps selling; none is marked verified. Sign-up now
offers a buyer or a vendor account; a vendor account is a buyer account with an
application that an administrator decides. Older binaries that do not know this
migration refuse startup; follow the existing database rollback procedure.

After upgrading, review **Admin → Vendors → Vendor applications** (`/admin/vendors`). A bond is a
non-refundable fee that an administrator records by hand after checking their
own wallet; the market does not detect bond payments. See
[vendor applications](docs/operator-guide.md#vendor-applications).

## Administration sections

`/admin` is now an overview (staff attention, the setup checklist and section
links), and each administration panel moved to one section page: `/admin/users`,
`/admin/vendors`, `/admin/payments`, `/admin/security`, `/admin/site`,
`/admin/transparency` and `/admin/system`. No migration or setting changes; every
form keeps its POST address and a saved change returns to its section. Update
bookmarks and runbooks that open `/admin#panel` (for example
`/admin#image-scanner` is now `/admin/system#image-scanner`); an old fragment
opens the overview. Private administrator ingress covers the new pages as it
covers `/admin`. See
[administration sections](docs/operator-guide.md#administration-sections).

## Monero escrow funding deposits after a reorganisation (A-554)

Migration 122 adds `payments.missing_height`: the escrow wallet's height when a
deposit credited toward an order's funding was first found missing. From this
release a Monero release or refund is not built while such a deposit is below the
confirmation threshold again, waits up to twice the threshold in blocks for one
that is missing, and never leaves a credited deposit a release was built without
out for a stray refund to the buyer (one a refund was built without is returned to
the buyer when it is listed again; [escrow](docs/escrow.md), *Refunds and
strays*). A settlement whose credited deposits are all gone stays requested and
keeps its wallet from destruction until it is resolved by hand. Older binaries that do not
know this migration refuse startup; back up the database first.

The upgrade does not revisit settlements built before it. Before taking Monero
escrow orders again, an administrator can list the funding deposits an older
binary left out of a release (its stray refund, if any, paid them to the buyer),
and the settlements it failed as *nothing-to-settle*, and review those orders by
hand:

```sql
SELECT p.order_id, p.txid, p.amount, p.confirmations, p.xmr_settlement_id FROM payments p
 WHERE p.currency = 'XMR' AND p.credited AND p.escrow_left_out
   AND EXISTS (SELECT 1 FROM xmr_settlements s WHERE s.order_id = p.order_id
     AND s.kind = 'release' AND s.state IN ('built', 'relayed', 'confirmed'));
SELECT id, order_id, kind, state FROM xmr_settlements WHERE send_error = 'nothing-to-settle';
```

## Account bans (A-546)

Migration 123 adds `users.banned_at`, `banned_by`, `ban_reason` and
`ban_prior_suspended_at`, with checks that a ban has a 10–500 character reason
and that a banned account is also suspended, and the `banned_handles` table of
handles that stay reserved after a banned account is erased. No existing account is banned or changed. *Admin → Users* gains
*Ban or unban an account* (`POST /admin/ban`, administrators only): permanent
closure, distinct from suspension. Older binaries that do not know this
migration refuse startup; back up the database first and follow the existing
database rollback procedure to go back. See *Account ban* under
[authentication](docs/operator-guide.md#authentication) in the operator guide.

## Vendor bond paid in XMR (A-547)

Migration 124 adds `xmr_bond_wallets`, `vendor_bond_accounts` and `vendor_bond_deposits`. With `ESCROW_XMR=on`,
Monero escrow ready, new XMR payments on and a vendor bond set, the Monero escrow watcher now creates one more wallet
on the escrow wallet-rpc, `bond-01`, sealed to `ESCROW_XMR_SEED_KEY` like an escrow wallet, and gives each application
waiting for a bond a one-time address of it; the bond is recorded as received (`how='xmr'`) once deposits covering it
have the escrow confirmations. Applications opened before the upgrade get an address on the first pass after it; the
manual *Record bond received* and *Waive the bond* stay. The market never sends from the bond wallet: sweep it offline
from its sealed seed ([vendor bond wallet](docs/operator-guide.md#vendor-bond-wallet)). Back up the escrow wallet volume
as before (it now holds the bond wallet too). A database holding a bond wallet is pinned to its network like one
holding escrow wallets. `scripts/restore.sh` and the manual protection SQL of [section 6](#6-backups-now-need-the-wallets-too)
now also close the bond wallet in use (`closed`: still read and still crediting the applications recorded on it, but
never issuing again), so an address it gave after the backup is never given to another applicant; a fresh `bond-NN`
issues from then on. Applications opened before this upgrade (`vendor_applications.before_bond_addresses`) are told on
`/account` not to pay again at their new address if they already paid as your instructions said; record those bonds by
hand. No new setting. Older binaries that do not know this
migration refuse startup; back up the database first and follow the existing database rollback procedure to go back.

# Upgrading from v0.1.0-alpha.1 to `v0.1.0-alpha.2`

`v0.1.0-alpha.2` is a prerelease. Its installer now defaults new installs to mainnet (real funds), but an upgrade keeps
your network ([section 64](#64-new-installs-default-to-mainnet-a-523)). No mainnet run is recorded; mainnet is covered
by unit, database and simulated-RPC tests only ([section 62](#62-mainnet-escrow-opt-in-and-the-fee-descriptor-migration-109-a-460)). Upgrade to the tag, not to `main`
(`git fetch --tags && git checkout v0.1.0-alpha.2`). Read this summary and the sections it names before you
upgrade a running v0.1.0-alpha.1 deployment.

## Summary

### Unreleased security hardening

These optional controls require the updated source; earlier alpha tags do not
include them. They add no migration and do not change existing networks,
escrow pins or payment settings:

- `compose.admin-tor.yaml` adds a client-authorized admin onion and a separate
  Unix HTTP socket. Enable it after reviewing
  [the private administrator procedure](docs/install/tor.md#private-administrator-onion).
  Public setup and administrator sessions then receive 404; use the admin onion
  for sign-in and shared staff workflows. Keep the existing storefront and
  mirror addresses and back up the new admin identity separately.
- `AUDIT_EXPORT_RECIPIENT` optionally encrypts downloaded and host-exported
  audit JSONL and detached signature files. It changes their suffixes to
  `.jsonl.asc` and `.sig.asc`; decrypt both offline before verification.
  [Encrypted audit exports](docs/operator-guide.md#encrypted-audit-exports)
  describes the public-key mount and failure behavior.
- `scripts/package-deployment.py` creates an optional deployment source
  archive, manifest and checksums from a committed revision. Public release
  archives and upgrade rehearsal extraction retain their existing behavior.

Leaving the controls unconfigured preserves existing behavior. Disabling
private admin ingress deliberately reopens administrative routes on the public
listener: remove both its configuration and overlay only after reviewing that
access change. Removing the audit recipient restores plaintext exports; existing
ciphertext still needs its original private key. Do not treat either rollback as
a recovery procedure for a compromised host.

**What changes for an operator**

- **Payments are per-order escrow, off until you set them up.** Bitcoin uses 2-of-3 escrow on regtest, signet or
  testnet4; Monero escrow is custodial, on a local fakechain or stagenet. Mainnet needs a new database and is the
  installer's default for new installs only ([section 64](#64-new-installs-default-to-mainnet-a-523); no mainnet run
  is recorded; see also [section 62](#62-mainnet-escrow-opt-in-and-the-fee-descriptor-migration-109-a-460)). Bitcoin testnet3 and
  Monero testnet are refused. Upgrading turns no payment on ([per-order escrow](docs/escrow.md)).
- **There is no pooled wallet.** The server refuses a non-blank `BITCOIN_RPC_URL`, `BITCOIN_WALLET`, `MONERO_RPC_URL`
  or `MONERO_WALLET_RPC_URL` and a database holding pooled payment history, and the installer also refuses the
  `monero-wallet` profile ([section 58](#58-pooled-wallet-removal-for-fresh-installs-a-448), [section 59](#59-pooled-history-refused-migration-108-a-492)).
  Sections below that describe pooled payments document builds between alpha.1 and this one; they do not apply here.
- **Startup is stricter.** `SETUP_TOKEN` must be random and `APP_SECRET_KEY` is new ([section 2](#2-new-keys-in-env)),
  the database must be UTF8 ([section 42](#42-database-encoding-must-be-utf8-a-481)), and the server refuses a database
  that records a migration it does not include ([section 5](#5-rolling-back)).
- **Optional local nodes** are pruned ([section 7](#7-local-nodes-are-now-pruned)) and pinned to release tags
  ([section 8](#8-node-images-are-pinned-to-release-tags)). The bundled Bitcoin node defaults to signet when `BITCOIN_CHAIN` is blank
  ([section 61](#61-the-bundled-bitcoin-node-defaults-to-signet-a-513); a new install writes mainnet, with no mainnet run recorded,
  [section 64](#64-new-installs-default-to-mainnet-a-523)) and its RPC password is checked through a salted verifier
  ([section 54](#54-local-bitcoin-rpc-uses-a-salted-verifier-a-144)).
- Also new: signed audit exports, and listing photos and avatars (uploads stay off until a ClamAV scanner is set up).

**Migrations:** `001_foundation` to `109_escrow_fee_descriptor`, 69 files (listed below). The server applies them
once, in order, at startup. They are one-way: there are no down-migrations.

**Before you upgrade**

1. Take an encrypted backup and keep a copy of `.env` ([section 1](#1-take-a-backup-and-record-what-you-run));
   `./scripts/install.sh upgrade` does both.
2. If your alpha.1 `.env` sets `BITCOIN_RPC_URL` or `MONERO_RPC_URL` (alpha.1's installer wrote them for local nodes,
   but alpha.1 never read them, so no pooled funds exist), blank them. `install.sh upgrade` refuses until you do; its
   advice to drain pooled funds first is for later pooled builds, not alpha.1.
3. With a local node from alpha.1, its volume holds mainnet data: read [section 3](#3-bitcoin_rpc_url-now-turns-payments-on)
   for removing it, but do not add the `monero-wallet` profile or a pooled wallet it mentions (this build refuses both).
   Set `BITCOIN_CHAIN=testnet4` in `.env` if you want testnet4 rather than signet (section 61).
4. With an external PostgreSQL, check that `SHOW server_encoding` says `UTF8` (section 42).
5. With a hand-written `.env`, replace a placeholder `SETUP_TOKEN` (section 2).

A database holding pooled payment history is refused by migration 108; keep the release it runs on (section 59).
An alpha.1 database holds none. Then follow [Upgrade with the installer](#upgrade-with-the-installer).

**Rollback rule:** rolling back is not an image swap. Stop the app, restore the pre-upgrade backup into a new
database and rebuild the previous tag against it; everything written after the upgrade is lost from the rolled-back
site. alpha.1 does not refuse a newer schema, so never point it at the upgraded database. Keep the backup, the
`.env` copy and the previous tag until you trust the new release ([section 5](#5-rolling-back)).

## Migrations and changes in detail

The migration files included since alpha.1 are: `001_foundation`, `010_auth`, `011_account_suspension`,
`012_pending_factor_failed`, `013_pending_password_digest`, `020_pgp`, `030_orders`, `031_review_hidden`,
`040_inventory`, `041_listing_takedown`, `050_payments`, `051_payments_locked`, `052_payment_intake`,
`053_payout_send_ambiguous`, `054_payments_late_notice`, `055_monero_post_submit_failures`, `056_payments_regression`,
`057_payout_address_changed`, `058_payments_below_floor`, `060_transparency`, `061_captcha_spent`,
`062_products_catalog_index`, `063_images`, `064_payout_returns`, `065_payments_after_close`,
`066_payment_review_disposition`, `067_pgp_key_lifecycle`, `068_pending_login_next`, `069_payout_one_settlement`,
`070_payout_refund_or_return`, `071_payment_address_collisions`, `072_audit_role_change_index`,
`073_payout_return_needs_release`, `074_escrow_btc`, `075_escrow_settlements`, `076_escrow_cosign`,
`077_escrow_recovery`, `078_escrow_waiting_spent`, `079_escrow_physical`, `080_escrow_bumps`,
`081_escrow_release_guard`, `082_escrow_bump_seen`, `083_escrow_key_reserve`, `084_escrow_xmr`,
`085_escrow_xmr_settlement_status`, `086_escrow_xmr_destroy_intent`, `087_escrow_supersede_unbuilt`,
`088_escrow_key_layout`, `089_escrow_per_order`, `090_xmr_escrow_collisions`, `091_pooled_drain_blockers`,
`092_pooled_stalled_review`, `093_escrow_node_health`, `094_escrow_health_off_and_key_mismatch`,
`095_pooled_txid_prefix_privacy`, `096_reported_zero_confirmation_regressions`, `097_payout_txid_lookup`,
`098_remove_profile_xmpp`, `099_audit_event_indexes`, `100_form_submissions`, `101_provider_episodes`,
`102_handle_casefold_lookup`, `103_message_extra_recipient_count`, `104_notification_order_id`,
`105_order_listing_snapshot`, `106_form_confirmation_mode`, `107_remove_pooled_wallet`, `108_refuse_pooled_history`, and `109_escrow_fee_descriptor`. Migration 106 adds a boolean to short-lived
form receipts recording whether the original account, admin or escrow-key submission required password confirmation.
It stores no submitted password, authenticator code, recovery code, PGP key or practice PSBT. The sections
below explain the operator-visible changes and rollback considerations.

## Upgrade with the installer

From the directory you deploy from, with your age public key:

```sh
git describe --tags --always   # note the revision you run today, e.g. v0.1.0-alpha.1
git fetch --tags && git checkout v0.1.0-alpha.2   # the release tag; git pull on main is not a release upgrade
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade
```

With an **external database** (`DATABASE_URL` names another host than `db`), the backup reads the database
only from `BACKUP_DATABASE_URL`, and needs Python 3 and PostgreSQL client tools matching or newer than the
server ([Backups and recovery](docs/operator-guide.md#backups-and-recovery)). Give both:

```sh
BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade
```

The value of `DATABASE_URL` in `.env` serves when this host can reach that server (quote it in single quotes). Without `BACKUP_DATABASE_URL`,
`upgrade` refuses before changing anything. Like any command-line value, it is visible to other local users
while the command runs and may be kept in your shell history.

Read [section 3](#3-bitcoin_rpc_url-now-turns-payments-on) first if `.env` sets a node URL, and
[section 7](#7-local-nodes-are-now-pruned) and [section 8](#8-node-images-are-pinned-to-release-tags) if you
run local nodes. `upgrade` then does, in order:

1. **Backup.** It runs `scripts/backup.sh` into `backups/pre-upgrade-<time>.dump.age` and stops, changing
   nothing, if that fails. Without `AGE_RECIPIENT` (and, for an external database, `BACKUP_DATABASE_URL`) it
   refuses to run unless you pass `--no-backup` (for example because you have just taken a backup; migrations
   are one-way).
2. **A copy of `.env`** as `.env.pre-upgrade-<time>` (mode `0600`): the configuration to roll back with.
3. **Missing keys.** It adds every non-secret key `.env.example` documents that `.env` lacks, with the value
   Compose already used while the key was absent, so nothing changes but what `.env` shows
   ([the appendix](#appendix-new-keys-by-hand) explains them). It records the
   `COMPOSE_PROJECT_NAME` your containers use and `APP_PORT`: the value in your shell when you set one, else
   the host port the running app container publishes (`docker compose port app 8080`), else `8080`. It never invents
   a secret: `AUDIT_SIGNING_KEY` and a local Monero wallet stay off until you add them.
4. **`APP_SECRET_KEY`** set to the current `SETUP_TOKEN` value when it is missing, so every key, session and
   TOTP secret stays the same ([details](#app_secret_key-and-stricter-startup-checks)).
5. **Pinned images.** `bitcoin/bitcoin`, `simple-monerod` and `simple-monero-wallet-rpc` on `latest` (or on no
   tag) become the verified releases ([section 8](#8-node-images-are-pinned-to-release-tags)). Another
   publisher's image on `latest` is kept, and the check below refuses it until you pin it.
6. **The onion mirror.** On Tor, when a `tor-mirror` container exists, `mirror` joins `COMPOSE_PROFILES`.
7. It prints what changed (key names, never secret values), runs `./scripts/install.sh check`, then
   `docker compose up -d --build --wait`; migrations run at startup.
8. Once setup is complete and the restarted app reports that its keys derive from `APP_SECRET_KEY`
   (`-status` prints `keyroot=app_secret_key`), it removes `SETUP_TOKEN`, which grants nothing any more, and
   recreates the app.
   On Tor, when `.env` changed, it recreates `tor` (and `tor-mirror`) once ([section 4](#4-deploy)).
9. **The malware scanner**, only when it is on (`scanner` in `COMPOSE_PROFILES`): it refreshes the scanner's
   signatures as `./scripts/install.sh refresh-scanner` does. A failed pull is only a warning. `upgrade` never
   turns the scanner on; it adds `CLAMD_ADDRESS` blank, which keeps photo uploads off
   ([Photo uploads and the malware scanner](docs/operator-guide.md#photo-uploads-and-the-malware-scanner)).
   ClamAV checks uploads for known malware; it does not review picture content.

Running it again changes nothing. Installer commands that change `.env` hold a lock, the directory
`.env.lock`, while they run; a second one refuses to start until the first ends. If a killed run left it
behind, remove it with `rmdir .env.lock`. If the check or a service fails, it says so and changes nothing more: fix
`.env`, or put the copy back with `mv .env.pre-upgrade-<time> .env`, and see
[Rolling back](#5-rolling-back). Then open the admin page as [section 4](#4-deploy) describes. The sections
below explain each change and how to make it by hand.

## 1. Take a backup and record what you run

Migrations are **one-way**. There are no down-migrations, and the alpha.1 code must not run against a
migrated database (for example, migration 001 replaces `orders.status` with `orders.state` and drops the old
unique constraint). `compose.yaml` builds the app from the checkout (`build: .`), so there is no saved image
to switch back to: rolling back means restoring this backup and rebuilding the old revision.
`./scripts/install.sh upgrade` takes the backup and the copy of `.env` for you. By hand, before upgrading, from
the directory you deploy from:

```sh
AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/pre-upgrade.dump.age
cp -p .env .env.pre-upgrade   # the configuration the old revision runs with (keep it private)
git describe --tags --always  # note the revision you run today, e.g. v0.1.0-alpha.1
```

Keep the backup, `.env.pre-upgrade` and the revision until you have run the new version for a while. See
[Rolling back](#5-rolling-back).

## 2. New keys in `.env`

`./scripts/install.sh upgrade` adds every missing key with the value Compose already used while it was absent,
so adding them changes nothing by itself. [The appendix](#appendix-new-keys-by-hand) lists them for an upgrade
by hand. Two changes need your attention either way: the checks on `SETUP_TOKEN` and the new `APP_SECRET_KEY`.

### Replace a placeholder `SETUP_TOKEN`

This release refuses to start when `SETUP_TOKEN` is not a random value: the old `.env.example` placeholder
(`REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS`), anything containing `REPLACE`, `CHANGEME`, `CHANGE_ME` or
`PLACEHOLDER`, fewer than 32 characters, fewer than 8 distinct characters, or a short pattern repeated. The
log then says `SETUP_TOKEN is still a placeholder value` or `SETUP_TOKEN is not random`. `scripts/install.sh`
always generated a random 64-character hex token, so only hand-written `.env` files are affected.

The placeholder is public, and `SETUP_TOKEN` authorizes `/setup` and, without `APP_SECRET_KEY` (see
[the next section](#app_secret_key-and-stricter-startup-checks)), derives the CSRF/CAPTCHA key and the key
that encrypts TOTP secrets. The same checks apply to `APP_SECRET_KEY`; when it is set, keys derive from it
alone, so rotating a placeholder `SETUP_TOKEN` changes only `/setup`, and a placeholder `APP_SECRET_KEY` is
the value to rotate, with the consequences below. An instance that ran with the placeholder must rotate it: put the output of
`openssl rand -hex 32` in `.env` as `SETUP_TOKEN='...'` and start the app again. What rotation does:

- Sessions stay signed in (they are stored as hashes in the database). A form or CAPTCHA that was open during
  the restart is refused once; reload it.
- **Bitcoin escrow.** A new key root changes the immutable HMAC pin for the arbiter account key and, when configured,
  the escrow signer's account key. Bitcoin escrow fails closed even if the same wallets are still present. Restore the
  original `APP_SECRET_KEY` (or legacy `SETUP_TOKEN` root); never clear or replace these pins while escrows exist.
  Party-key rows also use that root. If a party row still needs recovery, re-register it only after the original root is
  restored and startup has verified the pinned node and, when configured, signer; the app checks the submitted key against existing
  escrow children before activating it.
- **alpha.1 has no TOTP**, so an alpha.1 instance that rotates at this upgrade loses nothing else.
- On an instance that already ran this release with the placeholder, every stored TOTP secret, pending
  enrolment and not-yet-shown recovery-code list was encrypted under the old token and can no longer be read.
  There is no re-encryption tool, and those secrets must be treated as exposed anyway (the key was public,
  including in backups taken meanwhile). Affected users sign in with a recovery code (recovery codes are
  stored as plain hashes and keep working), turn TOTP off on `/totp` with their password and a recovery
  code, and enrol again. For a non-administrator with no recovery code left, confirm who they are out of
  band, then use *Admin → Reset a user's second factors*, which also ends their sessions and is audited.
  An administrator with no recovery code left needs the host command, because the web reset refuses
  administrators (and perhaps no administrator can sign in at all). Run it in the deployment directory; it
  works with the internal or an external database (replace `THE_HANDLE`, exact and case-sensitive):

  ```sh
  docker compose run --rm app -reset-admin-factors THE_HANDLE
  ```

  In one audited transaction it turns off that administrator's TOTP, recovery codes and PGP sign-in and ends
  its sessions and pending sign-ins; the administrator then signs in with the password alone and enrols again.
  It refuses an account that is not an administrator (use the web reset above) and an unknown handle,
  changing nothing ([details](docs/operator-guide.md#authentication)).

A restored database needs the `SETUP_TOKEN` (or `APP_SECRET_KEY`, when set) that was in use when its backup
was taken for its TOTP secrets to be readable. Keep the rotated value, not the placeholder, in any `.env` you
restore later.

### `APP_SECRET_KEY` and stricter startup checks

**Your existing `.env` keeps working unchanged.** This release adds `APP_SECRET_KEY`, which becomes the only
root of the CSRF/CAPTCHA key and the key that encrypts TOTP secrets (the derivation is the same as before, only
the input changes). Without it the app derives them from `SETUP_TOKEN` exactly as before, byte for byte, and
logs `keys: derived from SETUP_TOKEN (legacy)` at startup. `./scripts/install.sh upgrade` moves to it without
changing any key, and removes `SETUP_TOKEN` once setup is complete. By hand, copy the current value (if `.env`
already has an `APP_SECRET_KEY` line, edit that line instead):

```sh
grep '^SETUP_TOKEN=' .env | sed 's/^SETUP_TOKEN=/APP_SECRET_KEY=/' >> .env   # same value, new name
docker compose up -d
```

Never put a new random value in `APP_SECRET_KEY` on an existing install: that is a key rotation, with the
consequences described in the previous section. Once `APP_SECRET_KEY` is set and setup is complete,
`SETUP_TOKEN` grants nothing (`/setup` answers 403 whatever the token) and can be removed from `.env`; while it
is set, startup logs `hint: SETUP_TOKEN is set but installation is complete`. Before setup is complete the
app refuses to start without `SETUP_TOKEN`, and names `./scripts/install.sh setup-token --new`, which issues
one. Compose itself no longer refuses a blank `SETUP_TOKEN`: the app does, when neither `APP_SECRET_KEY` nor
`SETUP_TOKEN` is set.

Startup now also stops, with a message naming the key and never its value, when:

- `COOKIE_SECURE` is anything but exactly `true` or `false` (for example `TRUE`, `1` or `yes`); unset still
  means `true`. `scripts/install.sh` only ever wrote `true` or `false`, so only hand-edited values are affected.
- `DATABASE_URL` still holds a placeholder such as `.env.example`'s `REPLACE_ME` or a `CHANGEME`.
- `APP_MODE` disagrees with the exposure overlay in `COMPOSE_FILE`, which Compose now passes to the app:
  `tor` needs `compose.tor.yaml`, `clearnet` needs `compose.clearnet.yaml`, and never both. A blank
  `COMPOSE_FILE` (plain `-f` options) is not checked.

`APP_MODE=tor` with `COOKIE_SECURE=true` only logs a warning. The app, `tor` and `tor-mirror` services now have
Compose healthchecks: `docker compose ps` shows their health and `docker compose up -d --wait` waits for it.

## 3. `BITCOIN_RPC_URL` now turns payments on

In alpha.1 the installer wrote `BITCOIN_RPC_URL` but nothing read it. Now a non-empty value enables Bitcoin
test-network payments. The app never starts against a mainnet node unless `BITCOIN_CHAIN=main` is set: startup
exits with `refusing to start: bitcoin chain "main" ...`. A node that is unreachable, still syncing or has no wallet
loaded no longer stops the site; Bitcoin simply shows as unavailable on the admin page until it works.

- **External mainnet node**: blank `BITCOIN_RPC_URL`, or point it at a testnet4/signet/regtest node.
- **Local node from alpha.1**: the old `bitcoin_data` volume holds **mainnet** data (alpha.1 ran `bitcoind`
  without `-chain`). The node service now runs `-chain=${BITCOIN_CHAIN:-testnet4}`. Either:
  - blank `BITCOIN_RPC_URL` and remove `bitcoin` from `COMPOSE_PROFILES` to keep Bitcoin off; or
  - set `BITCOIN_CHAIN='testnet4'` and start from a fresh volume. The mainnet block data is of no use to
    the marketplace and only wastes disk. If you ever kept a wallet on that volume, copy `/data/wallets`
    out first. Then:

    ```sh
    docker compose stop bitcoin && docker compose rm -f bitcoin
    docker volume rm opsecmkt_bitcoin_data
    ```

  Then create the test wallet as described in [docs/testnet-runbook.md](docs/testnet-runbook.md).
- **Local Monero node from alpha.1**: its `monero_data` volume is mainnet data too; the service now runs
  `--${MONERO_NETWORK:-stagenet}`. Remove the old volume the same way (`opsecmkt_monero_data`), add the
  `monero-wallet` profile and the keys above, then follow the runbook.

## 4. Deploy

**Local nodes: decide on pruning first** ([section 7](#7-local-nodes-are-now-pruned)). The first start
after this upgrade prunes an existing full Bitcoin or Monero chain unless `.env` says otherwise.

`./scripts/install.sh upgrade` runs this section for you, Tor recreate included. By hand:

```sh
git pull   # or check out the release tag
docker compose config --quiet   # fails loudly on a malformed .env
docker compose up -d --build
docker compose logs -f app      # migrations run at startup under an advisory lock
```

**Tor installs: recreate Tor once.** Tor looks up the app's address only when it starts, and the node
services from steps 2 and 3 can move the recreated app to a different address; Tor then forwards to the old
one and the onion stops answering. This release restarts Tor whenever Compose recreates or restarts the app,
which needs **Docker Compose 2.17 or later** (`docker compose version`); the mirror is included only when
`mirror` is in `COMPOSE_PROFILES` (step 2). That does not repair a Tor that is already forwarding to an old
address.
After `up -d`, run once:

```sh
docker compose up -d --force-recreate tor              # without the onion mirror
docker compose up -d --force-recreate tor tor-mirror   # with the onion mirror (naming it starts it)
```

If the onion does not answer after any later change, recreate Tor the same way. The onion address is kept in
the `tor_identity` volume (`tor_mirror_identity` for the mirror) and does not change. See
[Clearnet and onion deployment](docs/operator-guide.md#clearnet-and-onion-deployment).

Then open the admin page. **Payment providers** shows each currency as Enabled, Unavailable (with the
error; retried every poll), Refused (not a test network, or not mainnet on a mainnet market) or Disabled. The container stop grace period is now
60 seconds so a redeploy does not interrupt a payout that is being broadcast.

**Container logs now rotate, with Docker's `json-file` driver.** Every container keeps at most three 10 MB
log files, and for these containers this replaces a default logging driver you set in Docker's `daemon.json`
(for example `journald`). To keep another driver, set `LOG_DRIVER` in `.env` before `up -d`: `local` works as
is; a driver that does not take `max-size` and `max-file` (such as `journald` or `syslog`) needs the override
file described in [Resource limits and log rotation](docs/operator-guide.md#resource-limits-and-log-rotation).
Log lines written before the upgrade stay with the old containers' driver.

**External database behind a connection pooler.** The app now sends `statement_timeout`, `lock_timeout` and
`idle_in_transaction_session_timeout` when each database connection starts, and checks before migrating that
the database accepts them. PgBouncer and some hosted poolers refuse such parameters: startup then stops with
"database refused the application's session settings" and changes nothing. See
[Connection poolers](docs/operator-guide.md#connection-poolers).

## 5. Rolling back

Rolling back is **not** an image swap. Migrations are one-way, so older code must not run against the
upgraded database. From this release on, the server checks at startup: when `schema_migrations` records a
migration it does not include, it exits with an error that names that migration and points to this section,
before it changes or serves anything (`the database was upgraded by a newer release: it records migration(s)
NNN_name, which this server does not include. Nothing was changed. ...`). It also exits, changing nothing,
when a recorded migration number carries a different name from this server's migration of that number
(`the database was migrated by a different build: ...`), for example a database a development branch
migrated; start that build again or restore a backup taken before it ran. alpha.1 predates that check and
does not refuse, so never point it at the upgraded database: it expects the pre-upgrade tables (for example
`orders.status`, which migration 001 removes). Compose rebuilds the app from whatever revision is checked
out. Everything written after the upgrade is lost from the rolled-back site. Run the first two steps from
the **upgraded** checkout: its `scripts/restore.sh` can restore into the internal database; the older one cannot.

1. Stop the app and keep the database running:

   ```sh
   docker compose stop app
   ```

2. Restore the pre-upgrade backup into a new database next to the upgraded one (type `RESTORE` when asked).
   The upgraded `opsecmkt` database is left untouched in case you want to roll forward again.

   ```sh
   AGE_IDENTITY=/secure/backup-key.txt RESTORE_INTERNAL_DATABASE=opsecmkt_rollback \
     ./scripts/restore.sh backups/pre-upgrade.dump.age
   ```

   After `./scripts/install.sh upgrade` the backup is `backups/pre-upgrade-<time>.dump.age` (it printed the
   name).

   With an external database, create an empty database on that server and restore with
   `RESTORE_DATABASE_URL` instead (see [backups and recovery](docs/operator-guide.md#backups-and-recovery)).
   The restore pauses payouts and holds queued ones (see step 6); alpha.1 has no payment processing.

3. Put back the old configuration and point it at the restored database:

   ```sh
   cp -p .env.pre-upgrade .env   # after install.sh upgrade: .env.pre-upgrade-<time>
   ```

   Edit `DATABASE_URL` in `.env` so the database name after the port is the restored one, for example
   `postgres://opsecmkt:PASSWORD@db:5432/opsecmkt_rollback?sslmode=disable` (keep the password as it is);
   `scripts/restore.sh` prints this line for the database it restored into.
   If you rotated `SETUP_TOKEN` during the upgrade, keep the new value rather than the placeholder.
   With an external database, point `BACKUP_DATABASE_URL` in your backup job at the restored database too:
   external backups follow `BACKUP_DATABASE_URL`, not `DATABASE_URL`.

4. Check out the revision you noted in step 1 and rebuild it:

   ```sh
   git checkout v0.1.0-alpha.1
   docker compose config --quiet
   docker compose up -d --build --remove-orphans
   docker compose logs -f app
   ```

   `--remove-orphans` removes the containers of services the older revision does not define, such as the
   malware scanner's `clamd` if you turned it on after the upgrade.

5. Keep backing up the database you now run. This release's `scripts/backup.sh` dumps the internal
   database named in `DATABASE_URL`, so it follows the switch and its success line names `opsecmkt_rollback`.
   The alpha.1 `scripts/backup.sh` you just checked out always dumps `opsecmkt`, which is now the abandoned
   upgraded database. While you run alpha.1 on the internal database, back up the rolled-back one directly:

   ```sh
   (set -o pipefail; umask 077
    docker compose exec -T db pg_dump -U opsecmkt -d opsecmkt_rollback --format=custom --no-owner --no-acl \
      | age -r age1YOUR_PUBLIC_RECIPIENT > backups/rollback.dump.age) || echo 'Backup FAILED'
   ```

   Use a new file name each time, as `scripts/backup.sh` does, and delete the file if the command failed.

6. Only after the rolled-back site is verified (sign-in works, a known order and message are present, a
   fresh backup of `opsecmkt_rollback` restores) and you no longer want to roll forward, remove the abandoned
   upgraded database. This cannot be undone and the upgraded data is not kept anywhere else, so take one more
   encrypted backup of it first if there is any doubt. The command connects through the database you now run,
   because PostgreSQL will not drop the database a session is connected to; the app is stopped, so nothing else
   holds `opsecmkt`:

   ```sh
   docker compose exec -T db psql -U opsecmkt -d opsecmkt_rollback -c 'DROP DATABASE opsecmkt'
   ```

   With an external database, run the same statement as a user that may drop it, connected to the restored
   database: `python3 scripts/postgres-tool.py RESTORED_DATABASE_URL psql -c 'DROP DATABASE opsecmkt'`. If it
   answers that the database is being accessed by other users, stop whatever is connected and run it again.
   Afterwards alpha.1's `scripts/backup.sh`, which always dumps `opsecmkt`, fails instead of dumping the wrong
   database: keep using the direct backup command of step 5.

## 6. Backups now need the wallets too

Once payments are on, the marketplace is custodial: the Monero escrow wallets live in the `monero_escrow_wallet`
volume (bundled nodes) or on the payment host (native production). **Database dumps do not include them.** Back
them up separately (see the runbook). `scripts/restore.sh` now pauses all outbound sends with a persistent recovery
gate, holds every restored escrow settlement with a stored transaction and adds an audit row recording the gate and
those counts. (Pooled payouts are removed: migration 108 refuses a database that holds any, section 59.) Follow the
[reconciliation procedure](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) before
explicitly clearing the gate. Restore with the application stopped and keep the site cut off from users until reconciliation is
complete; the procedure says when the app runs for you alone and when it must be stopped.

The script also prints `<N> images restored from a backup taken <time>; photo and avatar removals made after
that time are undone — check /moderator Recent photos and remove them again`. Removing a listing photo or an
avatar, by its owner or as a staff takedown, deletes it from the database, so a restore brings back every image
removed after the backup was taken. Before users return, have a moderator check the moderation desk's Recent
photos and take down again any image that had been removed. A restore not done by `scripts/restore.sh` prints
nothing, but undoes those removals all the same. The time is in UTC for backups made by this release's
`scripts/backup.sh`; for a backup made by an earlier one on a host (or, for the internal database, a container)
not set to UTC, it is off by that zone's offset from UTC.

**Any restore not done by `scripts/restore.sh` skips this protection.** A host or volume snapshot, a
managed-database point-in-time recovery or a manual `pg_restore` brings escrow settlements back with no gate and
no holds, and the escrow watchers would broadcast or relay them as soon as the application starts, even if they
were sent after that point in time. Free escrow accounts that the live wallet handed out after that point would
also be offered again: the escrow watcher retires a wallet's free accounts itself only when the wallet holds an
account the restored records lack, which misses a claim made after that point with no account created after it
(checkout opens only after the watcher's first full pass after each start, so its check covers checkouts from then on). After such a restore, and whenever the script reports that recovery protection failed, keep the
application stopped and apply the protection below to the restored application database **before starting the
application**, then follow the same reconciliation procedure. On a Compose deployment run `docker compose stop app`
(for a volume snapshot start only the database with `docker compose up -d --wait db`). On a native deployment run
`./opsecmkt deploy down --dir DIR`, or stop a directly supervised `opsecmkt serve` through its service manager, and do
not run `deploy up` until the protection has committed. The block runs in one transaction; each line does the following:

- `payments_recovery_required` sets the persistent recovery gate, which pauses Monero escrow checkout, settlement
  building and relay until an administrator clears it after reconciliation.
- `retention_restore_required` makes startup replay the current external retention state before accepting traffic
  (start with `RETENTION_ENABLED=true` and the current `RETENTION_STATE_DIR`; see [retention](docs/retention.md)).
- `escrow_xmr_release_recheck` asks the Monero escrow watcher, on its first pass after the gate is cleared, to move every
  release deadline of a shipped Monero escrow order that has passed in the restored records to 72 hours later, telling
  each buyer.
- `xmr_escrow_sweep_required` makes Monero escrow checkout wait until the Monero escrow watcher has found the escrow
  wallets created after the backup (each is sealed to the seed key and kept, never offered to a new order) and checked
  every escrow wallet. After restoring only the escrow wallet volume from an older backup, run that settings line alone:
  the watcher then also raises the subaddress lookahead of each escrow wallet older than the database and rescans it, so
  deposits to the accounts it does not know are seen.
- The `xmr_settlements` line holds every restored Monero escrow settlement with a stored transaction (built or relayed)
  with the `Restored from backup:` marker until an administrator confirms its relay on `/admin`.
- The `xmr_escrow_slots` line retires every free Monero escrow account, and the `xmr_escrow_wallets` line moves every
  active or sealed Monero escrow wallet to closing, so no restored account or wallet takes an order again. Omit these
  three table lines if the database has no `xmr_settlements.send_error` column (before migration 085).
- The `xmr_bond_wallets` line closes the vendor bond wallet in use (A-547): it may have given bond addresses after the
  backup that the restored records lack, so it gives no new address; it is still read, and its recorded addresses still
  credit their applications. Omit it if the database has no `xmr_bond_wallets` table (before migration 124).
- The final line adds one audit row ("set manually"); existing audit rows are not changed.

<!-- runbook-sql: restore-protection -->
```sql
BEGIN;
INSERT INTO settings(key,value) VALUES ('payments_recovery_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('retention_restore_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('escrow_xmr_release_recheck','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
INSERT INTO settings(key,value) VALUES ('xmr_escrow_sweep_required','true')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;
UPDATE xmr_settlements SET send_error='Restored from backup: check that this escrow settlement''s transaction was not already sent or replaced, then confirm its rebroadcast on the admin page.', updated=now()
WHERE state IN ('built','relayed');
UPDATE xmr_escrow_slots SET state='retired', updated=now() WHERE state='free';
UPDATE xmr_escrow_wallets SET state='closing', updated=now() WHERE state IN ('active','sealed');
UPDATE xmr_bond_wallets SET state='closed', updated=now() WHERE state='sealed';
INSERT INTO audit_events(user_id,action) VALUES (NULL, 'Payout recovery gate set manually (UPGRADING.md section 6); all outbound payouts paused until an administrator clears the gate');
COMMIT;
```

Save it as `restore-protection.sql` and run it with `psql` against the restored database, for the internal
database `docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt_restored < restore-protection.sql`
(the database name in `DATABASE_URL`). For a managed database, run the same file with `psql -X -v ON_ERROR_STOP=1`
over the operator's protected, certificate-verified connection to the restored database. It must end with `INSERT 0 1`
and `COMMIT`; on an error nothing is changed.

The restore also rolls back every setting an administrator saved after the backup or recovery point, such as **New escrow
checkouts**, site access, registrations and the vendor application policy (the bond amount `vendor_bond_xmr` and the
approval mode in **Admin → Vendor applications**), and undoes bonds recorded as received after that point. Before
clearing the gate, re-check them on `/admin` against your operator record. The market fee address and rate (`ESCROW_XMR_FEE_ADDRESS`, `ESCROW_XMR_FEE_BPS`) are host configuration, not
database rows, so a database restore does not roll them back; confirm the host values are the ones you intend. The restore can
roll back the record of the last fee seen, so the next start may announce the current fee to administrators once more.

The payout gate is enforced by this release. An older application image may not understand it; keep wallet
RPC configuration disabled when inspecting a restored database with older code. Alpha.1 has no payment
processing, but any other older release requires its own recovery review.

## 7. Local nodes are now pruned

The `bitcoin` and `monero` services now run pruned unless `.env` opts out: `bitcoind -prune=${BITCOIN_PRUNE_MB:-2000}`
(and `-datadir=/data`, the directory the image already used) and `monerod --prune-blockchain --sync-pruned-blocks`
unless `MONERO_PRUNE_FLAGS` is set. The application does not need old blocks (it uses the nodes' current state and
wallet RPCs), so payments work the same either way. What happens to a volume you already have:

- **Bitcoin volume with a full chain:** on its first start bitcoind deletes old block files down to about
  2000 MiB. This cannot be undone without downloading the whole chain again, and afterwards a wallet backup
  older than the node's `pruneheight` can no longer be restored on this node without doing so. To keep the full
  chain, add `BITCOIN_PRUNE_MB='0'` to `.env` **before** `docker compose up -d`.
- **Monero volume with a full chain:** monerod prunes the existing database in place on its first start
  ("Pruning blockchain..." in its log). The database file does not shrink and pruning temporarily needs extra
  space; to reclaim disk, remove the `monero_data` volume afterwards and let it sync again pruned. Monero
  wallets (in the separate `monero_wallet` volume) are unaffected. To keep the full chain, add
  `MONERO_PRUNE_FLAGS=''` to `.env` **before** `docker compose up -d`; once pruned, the database stays pruned
  even if you blank the flags later.
- **Volumes from v0.1.0-alpha.1** hold mainnet data and are removed in [section 3](#3-bitcoin_rpc_url-now-turns-payments-on);
  the new test-network volumes start pruned.

Behaviour on an already-synced chain is taken from the upstream sources and has not been rehearsed here
(UNVERIFIED). Sources, disk sizes, switching between pruned and full later, and the wallet-restore trade-off:
[Local node pruning](docs/operator-guide.md#local-node-pruning).

## 8. Node images are pinned to release tags

`compose.nodes.yaml` now defaults to `bitcoin/bitcoin:31.1`, `ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1` and
`ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1` instead of `latest`: the payout code decides whether
a failed send can have been broadcast from the wallet's error, and that table is verified for monero-wallet-rpc
v0.18.5.1 only. A `BITCOIN_IMAGE`, `MONERO_IMAGE` or `MONERO_WALLET_IMAGE` in your `.env` still wins. If one of
them ends in `:latest` (an installer run from a pre-release checkout wrote that) or is an image you chose, change
it to the tag above, or follow the bump checklist in
[Node image versions](docs/operator-guide.md#node-image-versions) before running another version. Check with:

```sh
docker compose config --images
```

The application also checks the running wallet (A-174): at startup and on every watcher pass it reads
monero-wallet-rpc's `get_version`. While the wallet does not report RPC version 1.31 from a release build (what
v0.18.5.1 reports), or the call fails, every failed Monero payout is recorded as possibly broadcast, so a requeue
needs the wallet check, and the *Payment providers* panel of `/admin` names the version the wallet reported and
points to the bump checklist. That panel also shows the Bitcoin Core version (`getnetworkinfo`). The RPC version
changes only when the wallet's RPC does, so a later release with an unchanged RPC is not detected: until your
`.env` names the pinned tag, treat every failed Monero payout as possibly broadcast even when `/admin` shows no
warning.

## 9. Overpayments are returned (migration 064)

Migration 064 runs at startup: an order may now have a release **and** a return of the amount paid over the price
(`payouts.kind` `return`, `UNIQUE (order_id, kind)` instead of `UNIQUE (order_id)`). Payouts already queued are
unchanged; only orders that settle after the upgrade are split. A refund or return larger than the order price
is held for an administrator (*Admin → Payment providers*), and refunds and returns now pay their own network
fee on Monero too. The restore SQL in [section 6](#6-backups-now-need-the-wallets-too) keeps that hold's text
after its own marker. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5). See [overpayments](docs/operator-guide.md#overpayments-and-the-payment-qr-code).

## 10. Deposits after an order closed (migration 065)

Migration 065 runs at startup and adds `payments.after_close`. From this release the watcher reads every
cancelled, completed or resolved order's deposit address until the address is 30 days old (before, only while a
deposit was confirming or unhandled, or for 24 hours after the order changed), so each pass reads more addresses.
A deposit first recorded after its order closed is never refunded or paid out automatically, even when the
cancellation refunded nothing: it is flagged for payment review and the buyer is told once per open late-deposit
review episode. Deposits recorded before the upgrade keep their earlier treatment.

**Legacy notice exception (A-239):** upgrading from a version that notified only staff of late deposits does not
backfill buyer notifications. An existing open late-deposit flag (uncredited, not below the payout floor, not locked
and not spent outside the market) holds its order's notice episode open even if the earlier version never told the
buyer. Further late deposits are still flagged and dated in the order history, but do not produce another buyer or
staff notice while that episode remains open. During upgrade reconciliation, review these existing late flags and
confirm the buyer knows about the unpaid deposit. Record the actual disposition only after wallet reconciliation;
do not close a flag merely to restart notifications. Once all eligible late flags have dispositions that close them,
a later deposit starts a new episode and is announced normally. This is the documented legacy exception allowed by
A-239; it does not change deposit credit, refunds or payouts.

See
[late deposits](docs/operator-guide.md#payments) and the
[runbook](docs/testnet-runbook.md#handle-a-payment-review-flag).

## 11. Payment-review dispositions (migration 066)

Migration 066 runs at startup and adds `payments.disposition`, `disposition_note`, `disposition_txid`,
`disposition_amount` and `disposed_at`; existing flags stay open. Moderators and administrators now record the
outcome of each payment-review flag on the moderation desk (no action with a reason, refunded by hand with the
transaction ID and amount, or escalated to an administrator) instead of the `psql` note the runbook described
before. Notes recorded that way earlier stay in the order history, but their flags stay open on the desk: record
each such refund there once as *refunded by hand*, with the transaction ID from its audit row. See the
[runbook](docs/testnet-runbook.md#handle-a-payment-review-flag).

## 12. Sign-ins in progress restart (migration 013)

Migration 013 runs at startup and adds `pending_logins.password_digest`, so a password change also ends a sign-in
whose password was checked just before it. A sign-in that passed the password step before the upgrade and has not
finished its second step is refused ("Sign-in verification expired. Sign in again."); the user enters the password
again. Sessions are unchanged. `/account` now lists each account's sessions by start time only (the market records
no IP address or browser), and *Sign out everywhere* also ends sign-ins waiting for their second step.

## 13. Monero releases pay their own network fee (A-232)

No migration. From this release every Monero payout, releases included, is sent with the network fee taken from
the amount (`subtract_fee_from_outputs`), as every Bitcoin payout already is: a vendor receives the price less the
fee, and no payout costs the pooled wallet its fee. A Monero release still queued when you upgrade is sent this
way, even one below the new minimum (the minimum applies when a payout is queued). The earlier version had no
minimum for Monero releases, so a pending one may be smaller than the network fee: its send now fails before
anything is broadcast (monero-wallet-rpc error -16, transaction not possible, recorded as a definite rejection),
and it is shown as *Failed — not retried; the wallet reported a pre-broadcast error, nothing broadcast*. Do not
requeue it (it would fail the same way). Settle it by hand as the runbook describes for payouts below the
minimum ([payment-review flags](docs/testnet-runbook.md#handle-a-payment-review-flag), step 4): agree with the
vendor whether to send it, and if so `transfer` the payout's amount from the wallet with the wallet paying the fee
(without `subtract_fee_from_outputs`), then record it with *Mark sent* and its transaction ID
(the prior build's runbook section 5, since removed). One Monero minimum, 0.001 XMR, now
applies to every payout: a release below it is flagged for payment review instead of sent, like a refund.
Listings cannot be priced below 0.001 XMR; a listing priced lower before the upgrade takes no new XMR drafts or payment requests, and saving it is refused until its vendor raises the Monero
price. On an existing order at such a price, an overpayment of more than the price is held as one release for an
administrator's review. When matching Monero payouts sent before the upgrade in the wallet (the prior build's runbook
failed-payout check, since removed, and its [restore reconciliation](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)), a release sent by the
earlier version paid its fee on top: its destination amount is the payout's.

## 14. One settlement per order, and resolving needs a password (migrations 069 and 070)

Migrations 069 and 070 run at startup. Migration 069 adds the unique index `payouts_one_settlement` on
`payouts(order_id)` for kinds `release` and `refund`: the database again refuses a release and a refund for the
same order (migration 064 had allowed it so a release could carry a `return`, which stays allowed). Migration 070
adds `payouts_refund_or_return` for kinds `refund` and `return`: a return sends a release's excess back to the
buyer, and a refund already sends back everything the buyer paid, so a refund beside a return would pay the excess
twice. An order therefore has a release with at most one return, or one refund. The application never queues
either pair, so an existing database normally upgrades without change. One that already holds a release and a
refund, or a refund and a return, for an order (only possible through hand-written SQL) is **not** changed: the
migration stops with an error naming the orders, every migration of that startup rolls back, and the server does
not start. Nothing is deleted, because any of those payouts may have been sent. While the server is stopped the
watcher sends nothing; settle each named order as below, then start the server again. Migration 073, in the same
start, also refuses an order left with a return and no release
([section 18](#18-a-return-needs-its-release-migration-073-a-268)): the steps below never leave one, and step 5
lists any that remain.

Run the SQL with `psql` against the application database. The internal-db service is shown; replace `opsecmkt`
after `-d` with the database name in `DATABASE_URL`. With an external database, make `db_sql` run your own
`psql -X -v ON_ERROR_STOP=1` connected to it, never with the password in its arguments.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt "$@"; }
```

1. **List every payout of the orders that clash, returns included, and keep the output** (here in
   `settlement-clash.txt`): once you delete rows, it is their only record apart from the pre-upgrade backup from
   [section 1](#1-take-a-backup-and-record-what-you-run), which holds them as they were before the upgrade;
   restoring that backup is the way back. `amount` is in the smallest unit of `currency`: satoshi for BTC
   (100 000 000 per BTC), piconero for XMR (10^12 per XMR). `user_id` is the recipient; `created` is when the row
   was queued and `updated` when it last changed:

   ```sh
   db_sql <<'SQL' | tee settlement-clash.txt
   SELECT p.id, p.order_id, o.state AS order_state, p.kind, p.state, p.send_ambiguous, p.currency, p.amount, p.address, p.user_id,
     p.txid, p.error, p.created, p.updated
   FROM payouts p JOIN orders o ON o.id = p.order_id
   WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
     HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
         OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1)
   ORDER BY p.order_id, p.kind;
   SQL
   ```

   Each listed order has two settlements: its **release together with its return** (the price to the vendor and
   the excess to the buyer; a return belongs to its release and is kept or deleted with it, even when the release
   row itself is gone, as after an earlier hand deletion: if such a return stands, it may not stay without a release
   (migration 073), so once this clash is settled, settle that order as
   [section 18](#18-a-return-needs-its-release-migration-073-a-268) describes: for a `completed` order or a release
   outcome its release is restored or recorded as paid, and for a `cancelled` order or a refund outcome a sent return
   is recorded as the order's refund and the rest of what the buyer is owed is sent by hand), and its **refund**.
2. **Find out which rows were sent.** A row in state `sent` was sent. A row in state `sending`, a `failed` row with
   `send_ambiguous` true, and any row whose error starts `Restored from backup:` may have been sent. Check every
   listed row in the wallet, whatever its state (a `pending`, `blocked`, `held` or definitely failed row may have
   been paid by hand): a transaction to its address and amount since before the row was created, with the complete
   checks in the prior build's runbook section 5 (since removed). A row that may have
   been sent and that the wallet check does not clear counts as sent. Never delete a sent row, or one that may
   have been sent, except as step 3 describes for an order paid twice or for a sent release that the order's
   outcome did not owe.
3. **Decide which settlement stands.**
   - If a row of one settlement was sent and no row of the other, that settlement stands. A release stands when
     either it or its return was sent: keeping the refund after a return was sent would pay the excess twice. A
     sent return paid the buyer only the excess; the price still follows the order's outcome. For a `cancelled`
     order or a refund outcome whose release was not sent, the price belongs to the buyer: send the buyer the price
     by hand, then delete the unsent release and the refund and record the sent return as the order's refund, with
     a note of the price you sent (step 4), so that the vendor is not paid.

     When the sent row is a release or a refund that the order's outcome did not owe, the party it did owe is
     still unpaid and you choose how to settle. Record the choice for each such order, with the transaction ID of anything you
     pay by hand, beside `settlement-clash.txt`:
     - **A sent release on a `cancelled` order or a refund outcome, beside an unsent refund**, paid the vendor the
       price the buyer is owed. Either keep the refund: delete the sent release and its return (step 4), the
       watcher sends the refund, the buyer is made whole and the market has paid the order twice. Only do this if
       the release's return was not sent, or the refund would pay the excess a second time as well. Or delete the
       refund, keeping the release and its return (step 4), and settle the price with the buyer by hand: pay it,
       or recover it from the vendor outside the market and then pay the buyer.
     - **A sent refund alone on a `completed` order or a release outcome, beside an unsent release**, paid the
       buyer back and left the vendor owed the price. The refund stands: delete the release and its return
       (step 4). Then either pay the vendor the price by hand, so that the market pays the order twice, or recover
       it from the buyer outside the market and then pay the vendor.
   - If no row was sent, the settlement matching the order stands: the release for a `completed` order, the refund
     for a `cancelled` one, and for a `resolved` one the outcome its order history records.
   - If rows of both settlements were sent, the order was paid twice (a refund plus a release, or a refund plus a
     return that paid the excess a second time). Recover that outside the market first; the settlement whose
     payment you leave with its recipient stands. Only then delete the other settlement's rows, sent or not; the
     output of step 1 is their record.
4. **Delete the settlement that does not stand**, one order at a time with its full order ID. To keep the
   refund, delete the release **and its return** together; deleting only the release would leave its return
   beside the refund, and a pending return would then be sent, paying the excess a second time:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID <<'SQL'
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','return') RETURNING id, kind, state, txid;
   SQL
   ```

   To keep the release and its return, delete the refund:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID <<'SQL'
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'refund' RETURNING id, kind, state, txid;
   SQL
   ```

   For a `cancelled` order or a refund outcome whose return alone was sent (step 3), first send the buyer the price
   by hand from the wallet, and note the amount the wallet sent and its transaction ID. Then delete the unsent
   release together with the refund and record the sent return as the order's refund, in one transaction (a return
   may not stay without its release, [section 18](#18-a-return-needs-its-release-migration-073-a-268)); the row keeps
   its amount, state and transaction ID, so the order page shows only the returned excess as the refund. The same
   transaction adds a note to the order history, which the buyer and the vendor read, saying that the return is the
   refund and naming the amount you sent by hand and its transaction ID (the vendor's page hides the transaction ID,
   as for every payout to the buyer). Give the amount as the wallet shows it, in BTC or XMR, as plain decimal digits without a sign or unit (for example `0.001`; the block refuses `NaN`, `Infinity` and exponent forms),
   and the 64-character transaction ID in lowercase; no address goes into the note:

   ```sh
   db_sql -v order_id=FULL_ORDER_ID -v amount=AMOUNT_SENT -v txid=TRANSACTION_ID <<'SQL'
   BEGIN;
   DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','refund') RETURNING id, kind, state, txid;
   UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return' RETURNING id, kind, state, txid;
   INSERT INTO order_events(order_id, from_state, to_state, note)
   SELECT o.id, o.state, o.state, 'Refund recorded by hand (UPGRADING.md section 14): the overpayment returned to the buyer earlier is this order''s refund, and the remaining '
     || CASE WHEN :'amount' ~ '^[0-9]+(\.[0-9]+)?$' AND :'amount'::numeric > 0 THEN trim_scale(:'amount'::numeric)::text END || ' ' || o.currency
     || ' was sent to the buyer by hand from the market wallet in transaction ' || CASE WHEN :'txid' ~ '^[0-9a-f]{64}$' THEN :'txid' END || '.'
   FROM orders o
   WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'refund' AND r.updated = now())
   RETURNING order_id, note;
   COMMIT;
   SQL
   ```

   Each statement prints the rows it deleted, changed or inserted, which must be the rows you chose from step 1's
   output and the note you meant; a mistyped order ID changes nothing (`DELETE 0`, `UPDATE 0`, `INSERT 0 0`). The
   note is added only when the statement before it relabelled the return. If a statement fails, `psql` stops and
   nothing in that block is changed: a missing, empty or malformed amount or transaction ID stops it with an error
   (for example `syntax error at or near ":"` or `null value in column "note"`).
5. **Check that no order is left with a release and a refund, a refund and a return, or a return without a
   release.** This must print no rows (`(0 rows)`):

   ```sh
   db_sql <<'SQL'
   SELECT order_id, string_agg(kind, ',' ORDER BY kind) AS kinds FROM payouts GROUP BY order_id
   HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
       OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1
       OR (count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0);
   SQL
   ```

   An order it lists with only `return` has a return without its release (from an earlier hand deletion, not from
   step 4): settle it as [section 18](#18-a-return-needs-its-release-migration-073-a-268) describes.

   Then start the new release again: migrations 069, 070 and 073 apply, and the watcher sends every row you kept that
   is `pending`. If the wallet showed in step 2 that a kept row was already sent but its state is not `sent` (it
   was paid by hand), do not start the server as it is, or that row is sent a second time: first apply the payout
   protection in [section 6](#6-backups-now-need-the-wallets-too), which pauses every payout behind the recovery
   gate, then record the row with *Mark sent* and clear the gate as the
   [runbook](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts) describes. Rolling
   back past this release needs the pre-upgrade backup, as for every migration (section 5).

Resolving a dispute on the moderation desk (or `/disputes`) now also asks for the moderator's or administrator's
current password, and an authenticator code when TOTP is enrolled, like the other staff actions; the audit row
records the confirmation. Moderators still resolve disputes.

## 15. PGP sign-in with an unreadable key, and failed password confirmations (A-272, A-274)

No migration. An account with PGP sign-in on whose saved key this release can no longer read (for example a key
saved before the market refused keys like it, A-266) was signed in with the password alone by the earlier version.
It no longer is: with TOTP on it is asked for its authenticator code or a recovery code; otherwise the sign-in is
refused after the correct password, with a message naming the problem, the administrator's *Reset a user's second
factors* (for an administrator, `-reset-admin-factors` on the host) and the staff contact, and the owner is notified
once. Expect such requests after upgrading; confirm who is asking before resetting (see the
[operator guide](docs/operator-guide.md#authentication)).

The per-account limit on password-confirmed actions now counts only failed confirmations (a wrong password, or a
wrong authenticator or recovery code after the right one): staff are no longer paused after ten correct
confirmations in ten minutes (for example resolving many disputes). After ten failures the refusal now reads "Too
many failed password confirmations. Try again in ten minutes."

## 16. Issued-address collisions after a wallet restore (migration 071, A-288)

Migration 071 adds two columns to `payment_status` (a count and a time, 0 and empty on existing rows); nothing else
changes and nothing needs doing. A payment request that gets an address the database already issued, which happens
after a wallet is restored from an older backup or seed without being advanced, was answered "Unable to save
changes" with status 500. It is now refused with 503 "BTC payment addresses are being restored on this market.
Nothing was charged and your order is still a draft; try again later." (or XMR), logged as a `payments:` line naming
only the currency, and shown on the *Payment providers* panel of `/admin` until an administrator dismisses it. A
monitor that matched `cause=23505 payment_addresses payment_addresses_address_key` should match
`cause=payment-address-already-issued` instead. The pooled wallet and this warning are since removed; the runbook no longer has a "Restore the wallets" section.

## 17. An index for role changes in the audit log (migration 072, A-301)

Migration 072 adds one partial index, `audit_events_role_change`, holding only the audit rows that record a role
change. The moderation desk and `/disputes` read those rows on every load with an open dispute, to list evidence
holders removed from staff (A-294); without the index each load read the whole audit log, which grows with every
sign-in. No row is changed and the signed audit export is unaffected; nothing needs doing. Building the index reads
`audit_events` once during the first start, within `MIGRATION_TIMEOUT` (default 10 minutes; about 0.1 seconds
for a million audit rows on a test host). If your audit log is very large, raise it first as described under
[Migrations](docs/operator-guide.md#migrations).

## 18. A return needs its release (migration 073, A-268)

Migration 073 adds the constraint trigger `payouts_return_needs_release` (and its function of the same name) on
`payouts`. A return sends the excess of a release back to the buyer, so it is valid only beside the release of the
same order. Before, the database accepted a return with no release; only hand-written SQL could create one, for
example deleting a release and keeping its return while settling a clash as
[section 14](#14-one-settlement-per-order-and-resolving-needs-a-password-migrations-069-and-070) said before this
release. Now a transaction that leaves any order with a return and no release, whether it inserted, deleted or
changed rows, is refused when it commits, with "payouts: order ORDER_ID has a return but no release. … so this
transaction was rolled back and nothing was changed. Settle the order as UPGRADING.md describes (A return needs its
release, migration 073).", and nothing of it is kept. The check runs at commit, so one transaction may insert a
release and its return in either order, or delete both. The application queues a return only together with its
release, so nothing it does is refused.

A database that already holds a return without a release is **not** changed: the migration stops with an error
naming the orders ("payouts: order(s) … have a return but no release. Nothing was changed. …"), every migration of
that startup rolls back, and the server does not start. Nothing is deleted, because the return, or the release that
is missing, may have been sent. While the server is stopped the watcher sends nothing. If the error names migration
069 or 070 instead, settle section 14 first (its step 5 also lists the orders this section settles). Settle each
order named here as below, with `db_sql` defined as in section 14, then start the server again.

1. **List the payouts of those orders and keep the output** (here in `return-without-release.txt`). Each listed
   order has only its return: migrations 069 and 070 refuse the other pairs first. `order_state` and
   `dispute_outcome` (for a `resolved` order) say whom the order owes; `price`, like `amount`, is in the smallest
   unit of `currency` (satoshi or piconero):

   ```sh
   db_sql <<'SQL' | tee return-without-release.txt
   SELECT p.id, p.order_id, o.state AS order_state, d.outcome AS dispute_outcome, o.amount AS price, p.kind, p.state, p.send_ambiguous,
     p.currency, p.amount, p.address, p.user_id, p.txid, p.error, p.created, p.updated
   FROM payouts p JOIN orders o ON o.id = p.order_id LEFT JOIN disputes d ON d.order_id = p.order_id
   WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
     HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0)
   ORDER BY p.order_id, p.kind;
   SQL
   ```

2. **Check in the wallet whether each return was sent**, as in section 14 step 2: a row in state `sent` was sent;
   one in state `sending`, a `failed` row with `send_ambiguous` true, or one whose error starts
   `Restored from backup:` may have been sent; check every row in the wallet whatever its state, and count a row
   that may have been sent and that the wallet does not clear as sent. Then find the missing release: the output
   you kept when it was deleted (such as `settlement-clash.txt` from section 14), the pre-upgrade backup and the
   order history ("Queued … release of …") give its address and amount. Check in the wallet whether the vendor was
   paid the price, by that release or by hand.
3. **Settle each order**, one at a time with its full order ID. Each statement prints the row it inserted, changed
   or deleted; a mistyped order ID, or an order the statement does not fit, changes nothing (`INSERT 0 0`,
   `UPDATE 0`, `DELETE 0`).
   - **A `completed` order, or a `resolved` one with the `release` outcome**, owes the vendor the price, so its
     return keeps a release. If the wallet shows that the vendor was paid the price, record that release as sent,
     with the address and transaction ID the wallet shows:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID -v address=VENDOR_ADDRESS -v txid=TRANSACTION_ID <<'SQL'
     INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, txid)
     SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, :'address', 'sent', :'txid'
     FROM orders o JOIN products p ON p.id = o.product_id
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
     RETURNING id, kind, state, amount, address, txid;
     SQL
     ```

     Otherwise restore the release of the price to the vendor's saved payout address, held for an administrator:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, error)
     SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, CASE o.currency WHEN 'XMR' THEN v.payout_xmr ELSE v.payout_btc END, 'held',
       'Restored by hand (UPGRADING.md section 18): check the wallet before releasing it.'
     FROM orders o JOIN products p ON p.id = o.product_id JOIN users v ON v.id = p.vendor_id
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
     RETURNING id, kind, state, amount, address;
     SQL
     ```

     Once the server runs, an administrator releases it under *Payouts* on `/admin` after checking the wallet once
     more (and, if the vendor's account is suspended, checking the address with the vendor); the watcher then sends
     it once. Either way the return stays as it is: if it was not sent, the watcher sends it to the buyer.
   - **A `cancelled` order, or a `resolved` one with the `refund` outcome**, owes the buyer everything it received
     and the vendor nothing, so no release is restored. If the return was sent, first send the buyer the price by
     hand from the wallet, unless you already paid it (as section 14 said for such an order before this release),
     and note the amount the wallet sent and its transaction ID. Then record the return as the order's refund; the
     row keeps its amount, state and transaction ID, so the order page shows only the returned excess as the refund.
     The same transaction adds a note to the order history, which the buyer and the vendor read, saying that the
     return is the refund and naming the amount you sent by hand and its transaction ID (the vendor's page hides the
     transaction ID, as for every payout to the buyer). Give the amount as the wallet shows it, in BTC or XMR, as
     plain decimal digits without a sign or unit (for example `0.001`; the block refuses `NaN`, `Infinity` and exponent forms), and the 64-character transaction ID in lowercase; no address goes into the note:

     ```sh
     db_sql -v order_id=FULL_ORDER_ID -v amount=AMOUNT_SENT -v txid=TRANSACTION_ID <<'SQL'
     BEGIN;
     UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, amount, txid;
     INSERT INTO order_events(order_id, from_state, to_state, note)
     SELECT o.id, o.state, o.state, 'Refund recorded by hand (UPGRADING.md section 18): the overpayment returned to the buyer earlier is this order''s refund, and the remaining '
       || CASE WHEN :'amount' ~ '^[0-9]+(\.[0-9]+)?$' AND :'amount'::numeric > 0 THEN trim_scale(:'amount'::numeric)::text END || ' ' || o.currency
       || ' was sent to the buyer by hand from the market wallet in transaction ' || CASE WHEN :'txid' ~ '^[0-9a-f]{64}$' THEN :'txid' END || '.'
     FROM orders o
     WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'refund' AND r.updated = now())
     RETURNING order_id, note;
     COMMIT;
     SQL
     ```

     The note is added only when the statement before it relabelled the return. A missing, empty or malformed amount
     or transaction ID stops the block with an error (for example `syntax error at or near ":"` or
     `null value in column "note"`) and nothing is changed.

     If it was not sent, delete it (neither statement touches a return beside its release, and this one never
     deletes a row in state `sent`):

     ```sh
     db_sql -v order_id=FULL_ORDER_ID <<'SQL'
     DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'return' AND state <> 'sent'
       AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
     RETURNING id, kind, state, txid;
     SQL
     ```

     Then send the buyer the price and the return's amount by hand from the wallet, unless you already paid them. In
     either case record the transaction ID of what you sent by hand beside `return-without-release.txt`; do not
     insert a payout or use *Mark sent* for it.
4. **Check that no order is left with a return and no release.** This must print no rows (`(0 rows)`):

   ```sh
   db_sql <<'SQL'
   SELECT order_id FROM payouts GROUP BY order_id
   HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0;
   SQL
   ```

   Then start the new release again: migration 073 applies. As in section 14, if a return you kept was paid by
   hand but its state is not `sent`, apply the payout protection in
   [section 6](#6-backups-now-need-the-wallets-too) first and record it with *Mark sent*, or the watcher sends it a
   second time. Rolling back past this release needs the pre-upgrade backup (section 5): the earlier release refuses
   to start on a database that records migration 073.

## 19. Bitcoin escrow tables (migration 074)

Migration 074 runs at startup. It adds the tables Bitcoin escrow uses (`user_escrow_keys`, `escrows` with each
escrow's pinned refund address and market-fee snapshot, `escrow_pubkeys`, `settlements`, the sequence
`escrow_market_index`), the column `payments.settlement_id`, and a trigger, `payouts_not_escrow`, that refuses a
`payouts` row for an order that has an escrow (an escrow order is settled from its escrow, never by a pooled payout).
The tables stay empty unless `ESCROW_BTC=on`, a regtest-only setting that is off by default ([operator
guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)), and the trigger changes nothing for pooled orders; nothing
changes for users or payments. No action is needed. Rolling back past this release needs the pre-upgrade backup, as
for every migration (section 5).

## 20. Bitcoin escrow settlements (migration 075)

Migration 075 runs at startup. It adds to `settlements` what the server needs to build an escrow's settlement and take
one party's signature (the destination, requester, fee rate, network fee, amount spent, unsigned PSBT and its
transaction ID, which party signed and when, and why a build waits), a unique index so no two vendor claims pay the
same address, the tables `settlement_outputs` (each settlement's fixed outputs) and `settlement_signatures` (one
party's signature per input; the pasted PSBT is never stored), and two triggers: `settlements_frozen` refuses any
change to a settlement's transaction once a party has signed it, and `settlement_outputs_frozen` allows outputs to be
written only while their settlement is being built. Like migration 074 it changes nothing unless `ESCROW_BTC=on`
(regtest only, off by default); the new optional key `ESCROW_BTC_FEE_RATE` (sat/vB, default 2) sets the network fee
rate of settlements ([operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)). No action is needed.
Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5).

## 21. Bitcoin escrow co-signing, broadcast, dispute decisions and stray refunds (migration 076)

Migration 076 runs at startup. It adds to `settlements` the final transaction the market stores before it broadcasts a
settlement (`raw_tx`, who completed it, when it was stored, broadcast and confirmed, the transaction that spent its
coins when another one did, and why it is not sent yet), a state `superseded` for the settlement of a dispute decision
an administrator changed (the one claim or refund per order no longer counts it), a unique index allowing one stray
refund in progress per order, the column `payments.escrow_left_out` (an escrow output a bounded build left for a stray
refund) and `escrows.recover_requested` (a buyer's "Recover coins at this escrow"). It replaces the function of the
trigger `settlements_frozen`: a settlement's transaction still never changes once a party has signed it, a party
signature that did not verify may be discarded before the final transaction exists, and the stored final transaction
never changes and the settlement never goes back; a new trigger, `settlement_signatures_kept`, refuses any other
change to stored signatures. Like migrations 074 and 075 it changes nothing unless `ESCROW_BTC=on` (regtest only, off
by default). With escrow on, this release also co-signs and broadcasts settlements, lets an administrator decide
escrow disputes, and writes three `settings` keys when used: `escrow_btc_fee_bps` and `escrow_btc_fee_address` (the
market fee an administrator saves on `/admin`, which then overrides `ESCROW_BTC_FEE_BPS` and
`ESCROW_BTC_FEE_ADDRESS`) and `escrow_btc_stray_check` (the daily stray-refund check). Escrow now also stays disabled
unless the arbiter wallet is an unencrypted descriptor wallet with private keys that has never spent (A-471, section 38; [operator
guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)). No action is needed. Rolling back past this release needs
the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that
records migration 076.

## 22. Bitcoin escrow recovery and reconciliation (migration 077)

Migration 077 runs at startup. A new trigger, `settlements_moves`, lets a settlement with a final transaction move only
forward (signed to broadcast or external, broadcast to confirmed or external), with one step back for a reorganisation:
confirmed to broadcast, when its transaction drops out of the chain and is sent again (confirmed to external when a
conflicting transaction was mined instead); an external settlement never changes. It adds `settlements.reorg_safe_at` (a
confirmed settlement is followed until its transaction has 100 confirmations), a third value `network` for
`settlements.cosigned` (a settlement whose transaction the market found already on the network when it came to complete
it), `payments.spent_outside` and `payments.spent_by` (an escrow deposit spent by a transaction the market did not make,
with no settlement covering it, flagged for staff) and the table `escrow_orphans` (addresses the escrow watch-only
wallet holds with no escrow row, listed on `/admin`). Like migrations 074–076 it changes nothing unless `ESCROW_BTC=on`
(regtest only, off by default); pooled payments are unchanged. With escrow on, this release also pauses escrow
co-signing, settlement builds and every broadcast of a stored settlement transaction while the restore gate
(`payments_recovery_required`) is set, and `scripts/restore.sh` and the manual protection SQL in section 6 now also hold
every restored escrow settlement that has a stored transaction, which an administrator confirms on `/admin` before the
market broadcasts it again. While the gate is set, escrow checkout is also refused with a fixed 503 and the order stays
a draft. The escrow RPC user's `rpcwhitelist` needs one more method, `listtransactions` (it reads the
watch-only wallet only; the escrow watcher uses it to name the transaction that spent an escrow deposit): add it before
upgrading, or that lookup is refused and such a deposit is flagged without the transaction ID ([operator
guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)). No other action is needed. Rolling back past this release
needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database
that records migration 077.

## 23. Bitcoin escrow settlements spent while they wait (migration 078)

Migration 078 runs at startup. It changes two rules on `settlements` so the escrow watcher can close a settlement whose
escrow deposit another transaction spent while the settlement was still being built or was waiting for a party's
signature (A-356): the check `settlements_built` now also allows an `external` settlement that was never built (no
PSBT, transaction ID, fee or amount), and the function of the trigger `settlements_frozen` now also allows a settlement
waiting for a signature to store its final transaction when it moves to `signed` (the market found that exact
transaction already on the network: the buyer and the vendor signed it and sent it themselves). Every other rule of
migrations 075–077 is unchanged. Like them it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default);
pooled payments are unchanged. With escrow on, this release also checks a new escrow's address against the escrow
watch-only wallet before the escrow is created: if the wallet already watches it (an escrow that a restored database no
longer has), the next child keys are tried, at most 20 tries in all, and after the last the checkout is refused with a
fixed 503 and the order stays a draft. Only the whole composed address is checked ([runbook](docs/testnet-runbook.md#reconcile-a-restored-database-before-enabling-payouts)).
No action is needed. Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5):
the earlier release refuses to start on a database that records migration 078.

## 24. Bitcoin escrow for physical orders: dispatch windows and deadlines (migration 079)

Migration 079 runs at startup. It adds `products.dispatch_days` (a listing's dispatch window, 1 to 30 days; empty for
every listing that exists before the upgrade) and six columns to `escrows` (`release_days` and `dispatch_days`, the
release deadline length and dispatch window an escrow snapshots at checkout; `ship_by`, when a buyer may cancel an
unshipped paid order; `release_at`, the release deadline; `reminded_at`, the 72-hour reminder; `release_recheck`, set
once a restore has moved a deadline), and a trigger, `escrows_deadlines_frozen`, that refuses any change to a snapshot
or a deadline once it is set, except that a restore's recheck may move `release_at` later. Like migrations 074–078 it
changes nothing unless `ESCROW_BTC=on` (regtest only, off by default): with it off nothing reads or writes the new
escrow columns, and the only visible effect is that a physical listing created or saved from now on is stored with a
dispatch window (3 days unless its vendor gives another; the listing form shows the field only while escrow is on).
With escrow on, this release also offers escrow checkout for physical listings, adds the administrator's release
deadline length to `/admin` (setting `escrow_release_days`, default 14, 7 to 30 days; [operator
guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)) and a ship-by time and a release deadline to each physical
escrow order. A physical listing that exists before the upgrade has no dispatch window and takes no escrow checkout
until its vendor saves it; a save stores the default of 3 days even while escrow is off (the field is then hidden),
so once escrow is turned on such a listing takes escrow checkout with a 3-day window its vendor did not choose. A
listing that already has a window keeps it on save. No action is needed. Rolling back past this release needs the pre-upgrade backup, as for
every migration (section 5): the earlier release refuses to start on a database that records migration 079.

## 25. Bitcoin escrow fee bumps (migration 080)

Migration 080 runs at startup. It adds the tables `settlement_bumps` (a replacement of a broadcast settlement's
transaction that pays the same outputs with a higher network fee: who asked for it, its fee rate, unsigned PSBT and
transaction ID, which party signed first, the final transaction once stored, and its state) and
`settlement_bump_signatures` (one party's signature per input; the pasted PSBT is never stored), the function
`settlement_claimant`, and triggers that make a bump follow its rules in the database: requested only for a broadcast
settlement, built only by the market, signed first by the claimant (the vendor of a release, the buyer of a refund),
final once stored, moving only forward, and never deleted. The settlement's own row never changes. Like the earlier
escrow migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default); pooled payments are
unchanged. With escrow on, the claimant of a broadcast settlement that is not yet mined sees a form to raise its network
fee on the order page; the escrow RPC user's `rpcwhitelist` needs no new method. `scripts/restore.sh` and the manual SQL
in [section 6](#6-backups-now-need-the-wallets-too) hold a restored fee bump with a stored transaction until an
administrator confirms its settlement's rebroadcast (omit the `settlement_bumps` line of that SQL if you apply it to a
database from before this migration). No action is needed. Rolling back past this release needs the pre-upgrade
backup, as for every migration (section 5): the earlier release refuses to start on a database that records migration
080.

## 26. Bitcoin escrow: the market signs only when the order's state fits (migration 081)

Migration 081 runs at startup. It adds the function `escrow_market_may_sign` and two triggers, `settlements_market_fits`
and `settlement_bumps_market_fits`: a final transaction the market co-signed is stored only when the order's state fits
the settlement's kind (a release on a completed order or one decided for the vendor, a refund on a cancelled order or
one decided for the buyer, a stray refund on any closed order). It is the database's backstop for a physical order's
release, which is built when the order is shipped and which the buyer and the vendor complete with their own two
signatures before the order's outcome. Rows stored before the migration are not checked again. Like the earlier escrow
migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back
past this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to
start on a database that records migration 081.

## 27. Bitcoin escrow fee bumps seen on the network (migration 082)

Migration 082 runs at startup. It replaces the function of the trigger `settlement_bumps_frozen` and allows a third
value, `network`, for `settlement_bumps.cosigned`: a fee bump whose transaction is already on the network when the
market comes to follow it (the buyer and the vendor signed it and sent it themselves, or a restored database holds an
older row whose transaction was sent after the backup) is checked, stored and recorded as broadcast instead of being
treated as a spend outside the market. Every other rule of migration 080 is unchanged. Like the earlier escrow
migrations it changes nothing unless `ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back
past this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to
start on a database that records migration 082.

## 28. Bitcoin escrow keys after a restore (migration 083)

Migration 083 runs at startup. It adds the tables `escrow_reserved_keys` (each public key of an address the escrow
watch-only wallet holds with no escrow on record, with that address) and `escrow_index_floors` (for each account with an
escrow key, the lowest child index its next escrow may use). Both stay empty unless the market has run Bitcoin escrow
(`ESCROW_BTC=on`, regtest only, off by default) and has been restored from a backup. Every restore by
`scripts/restore.sh` (and the manual SQL in [section 6](#6-backups-now-need-the-wallets-too), which already carries
these lines together with the one that asks for a release deadline recheck) sets `escrow_release_recheck` and
`escrow_sweep_required`, which act only while escrow is on, and writes the floors and moves the market's escrow key
index 50 past the restored records; with escrow on, checkout then answers 503 until the escrow watcher has reserved the
keys of every orphan address, so that no escrow key is handed out twice (A-366). A dump taken before this migration has
no `escrow_index_floors` table when `scripts/restore.sh` runs, so that restore gets the sweep and the
recheck but not the margin. If an orphan's keys cannot be read, checkout stays paused until an operator deals with it:
see the [test-network runbook](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/testnet-runbook.md#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused).
No action is needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5): the earlier release refuses to start on a database that records migration 083.

## 29. Monero escrow tables (migration 084)

Migration 084 runs at startup. It adds the tables Monero escrow uses: `xmr_escrow_wallets` (each monthly escrow wallet's
name, primary address, restore height, seed sealed to the operator's OpenPGP key with that key's fingerprint, state and
retention dates), `xmr_escrow_slots` (each wallet account's index and address, and the order that claimed it),
`xmr_settlements` and `xmr_settlement_outputs` (each settlement's destination and outputs, and its stored transaction).
It adds `escrows.coin` (`BTC` for every existing row, which reads back unchanged), `escrows.xmr_slot_id` and
`payments.xmr_settlement_id`, makes `escrows.market_index` nullable (a check requires it for exactly the Bitcoin
escrows), and adds triggers that make the database enforce Monero escrow's rules: an escrow's coin is its order's
currency, a Bitcoin settlement refuses a Monero escrow and the other way round, a stored transaction never changes and
is stored only when the order's state fits (the rule of migration 081), states move only forward, and a Monero escrow
deposit names the account its order claimed. The migration only adds: no existing row is rewritten. **One change reaches
Bitcoin escrow too:** since this migration the database refuses to delete a row of `escrows` or of any `xmr_*` table
(the application never deletes one; a deleted `escrows` row would turn its order into a pooled one). Otherwise it
changes nothing while `ESCROW_XMR` is off, which is the default: the new tables stay empty and nothing writes them (the
admin page only reads them, to list held settlements and destroyed wallets), and pooled and Bitcoin escrow payments are
unchanged.

Monero escrow is a separate, off-by-default setting that runs on a local Monero fakechain only ([operator
guide](docs/operator-guide.md#monero-escrow)); it adds the optional keys `ESCROW_XMR`,
`ESCROW_XMR_WALLET_RPC_URL`, `ESCROW_XMR_DAEMON_URL`, `ESCROW_XMR_FEE_BPS`, `ESCROW_XMR_FEE_ADDRESS`,
`ESCROW_XMR_SLOTS`, `ESCROW_XMR_MAX_FEE` and `ESCROW_XMR_SEED_KEY`, and the secret `MONERO_WALLET_SECRET` (never
invented by `upgrade`; `openssl rand -hex 32`). Leave them blank to keep it off. `scripts/restore.sh` and the manual SQL
in [section 6](#6-backups-now-need-the-wallets-too) now also hold restored Monero escrow settlements, retire every free
account, move every active or sealed escrow wallet to closing and ask for the Monero escrow sweep (the restore writes
those rows whether or not `ESCROW_XMR` is on; they matter once it is). No action is needed at upgrade. Rolling back past
this release needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a
database that records migration 084.

## 30. Monero escrow settlement status and re-requests (migration 085)

Migration 085 runs at startup. It adds `xmr_settlements.send_error` (why a settlement is not built or sent yet, why it
failed, or that a restore holds it, as a fixed class and never text from a node) and the table
`xmr_settlement_rerequests` (an administrator's request to build a failed settlement again: who asked, when, and how it
was decided, refused with a fixed reason or done with the new settlement). A request is recorded only for a failed
settlement that has a stored transaction, at most one is open per settlement, its state moves only forward, and rows are
never deleted. Like migration 084 it only adds, and it changes nothing while `ESCROW_XMR` is off: nothing writes either.
With Monero escrow on, an administrator who is not a party to the order may re-request a failed settlement from the
order page (password step-up, audited), and a settlement set aside because building it made the escrow wallet-rpc stop
responding can be built again the same way; the held settlements of a restore are listed on `/admin` and confirmed there
([operator guide](docs/operator-guide.md#monero-escrow)). The restore's Monero hold is written to
`xmr_settlements.send_error`, so the three Monero lines of the SQL in [section 6](#6-backups-now-need-the-wallets-too)
need this migration (a database with 084 but not 085 exists only on a development build of this phase). No action is
needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration (section 5): the
earlier release refuses to start on a database that records migration 085.

## 31. Monero escrow destruction record (migration 086)

Migration 086 runs at startup. It adds `xmr_escrow_wallets.destroy_started_at`: when the Monero escrow watcher began
destroying a wallet, recorded before it replaces the wallet's password. A trigger makes the database enforce it: it is
set once, only on a retained wallet past its destruction date that still has its sealed seed, never changes or clears,
and the wallet then moves only to destroyed or missing, or back to retained when it opens again with the market's
password (its password change never took effect), to be destroyed once every precondition holds again. With it, a wallet
that no longer opens with the market's password ("Invalid password") is destroyed again automatically only when the
market itself began destroying it (a crash, or a restore from a backup taken in between); any other such wallet keeps
its sealed seed, is recorded missing and waits for an administrator's confirmation on `/admin` ([operator
guide](docs/operator-guide.md#monero-escrow)). So a wallet destroyed after the backup a database is
restored from is recorded missing, not destroyed again, unless that backup already recorded the start of its destruction
(never one taken before this migration). Like migrations 084 and 085 it only adds, and it changes nothing while
`ESCROW_XMR` is off: nothing writes the column. No action is needed at upgrade. Rolling back past this release needs the
pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that records
migration 086.

## 32. Bitcoin escrow: changing a decision whose settlement was never built (migration 087)

Migration 087 runs at startup. It lets a Bitcoin escrow release or refund that was never built (still building, or
failed, with no PSBT) be superseded when an administrator changes the dispute decision; before it, that change failed
with a server error (`settlements_built`, A-462) and the decision could not be changed at all. It replaces the check
`settlements_built` with one that also accepts such a superseded row (no PSBT, transaction ID, network fee or signer;
a failed row keeps the value and fee rate it failed at) and adds the trigger `settlements_supersede_keeps`: superseding
a settlement never changes its build, destination or signer, so a built one keeps its whole PSBT and an unbuilt one
never gains one, and the trigger `settlements_never_rebuilding`: no settlement moves back to building (nothing in the
market does). Every other rule of migrations 075 to 078 is unchanged, and no existing row is rewritten. It also adds
`payments.escrow_too_few_by`, the refund whose too-small failure left a deposit out for a stray refund, set only with
`escrow_left_out` and only to a refund of the deposit's own order (trigger `payments_too_few_refund`): when a changed
decision supersedes that refund, those deposits count for the order again.
Deposits a too-small refund left out before this migration on an order decided for the buyer (only a development build
with A-437 and without this migration wrote them) do not name their refund, so a change of that decision leaves them
for the stray refund to the buyer; an order whose refund failed so before A-437, with its deposits still counted, is
handled by the next escrow pass, which names the refund. Like the earlier escrow migrations it changes nothing unless
`ESCROW_BTC=on` (regtest only, off by default). No action is needed. Rolling back past this release needs the
pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database that
records migration 087.

## 33. Bitcoin node sync guard and the escrow chain pin (A-439)

No migration. The pooled Bitcoin watcher now also skips its pass while the node's headers are ahead of its blocks and,
on a public test network, while the node has no peers or its tip block is older than the new optional key
`BITCOIN_MAX_TIP_AGE` (default `2h`, 30m to 24h; an invalid value stops startup): a node that lost its peers used to
look synced, and an order paid while it was cut off could be cancelled for non-payment. The admin page's last error
then reads "Node is not synced (…)". A public-network node whose `getblockchaininfo` has no `time` field counts as
not synced too ("tip time unreadable: this Bitcoin Core is too old for the sync guard; use Bitcoin Core 31.1 or
later"): the guard needs Bitcoin Core 31.1 (the version the installer pins; earlier versions untested). A regtest
node is unaffected. With `ESCROW_BTC=on` the escrow watcher applies the
same rule, the app pins the escrow node's chain in `settings` (`escrow_btc_chain`; an existing regtest escrow database
is pinned at the first start), and the escrow user's `rpcwhitelist` gains `getnetworkinfo`
([operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)); on regtest the app does not call it, so the old
whitelist keeps working, but add it. No action is needed otherwise.

## 34. Bitcoin escrow refuses a pooled RPC user that can read the arbiter wallet (A-440)

No migration. Only with `ESCROW_BTC=on` and `BITCOIN_RPC_URL` set: at startup and on each retry escrow calls
`listdescriptors` (private=false), `gethdkeys` (private=false) and `walletprocesspsbt` (an empty PSBT, sign=false) on the
arbiter wallet's path as the pooled RPC user, and starts only when each call is refused with HTTP 403 or the pooled node
has no such wallet (another node); while escrow is on, the escrow watcher repeats the check once a day and disables
escrow if a call is no longer refused (QA4W3-1, QA4W3-8). A pooled user without an `rpcwhitelist`
line on the escrow node (unrestricted under `rpcwhitelistdefault=0`, as the guide used to leave it) could export the
market's escrow key, so escrow now stays disabled with "escrow: Bitcoin escrow disabled: the pooled Bitcoin RPC user
(BITCOIN_RPC_URL) was not refused listdescriptors, gethdkeys or walletprocesspsbt on the arbiter wallet, …" (with the
method that answered and its class, such as `(gethdkeys answered)`) and a line on `/admin`. Before upgrading,
give that user the pooled whitelist line of the [operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)
(*The pooled RPC user*) and restart bitcoind, or move escrow to a node of its own. The guide and the
[runbook](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) now also describe the
arbiter and watch-only wallet backup and restore. Pooled payments are unaffected.

## 35. Monero daemon sync guard, Monero escrow on stagenet and its network pin (A-449)

No migration. With `MONERO_RPC_URL` set, the pooled Monero watcher now also skips its pass while the daemon reports
`synchronized` false, has no peers (`offline`, or no connections on an unrestricted RPC) or its top block is older than
the new optional key `MONERO_MAX_TIP_AGE` (default `1h`, 20m to 24h; an invalid value stops startup): before, only a
`target_height` above the height counted, so a daemon that lost its peers looked synced and an order paid while it was
cut off could be cancelled for non-payment. The admin page's last error then reads "Node is not synced (…)". The app now
also calls `get_last_block_header` on the daemon; it is on monerod's restricted RPC, so the bundled `monero` service
answers it. A restricted RPC (the bundled service) hides the connection counts, so there only `offline` and the tip's age
are read. A market whose daemon is synced and receiving blocks sees no change. What this leaves (QA4W3-5): on a
restricted RPC `synchronized` stays true once set, `target_height` reads 0 and `offline` means only `--offline`, so a
daemon cut off from its peers looks synced until its top block is older than `MONERO_MAX_TIP_AGE` (default 1 hour, at
least 20 minutes). A deposit made in that window is not seen, and an order whose payment window ends in it can be
cancelled as unpaid; once the daemon sees the deposit it is recorded after the order closed: flagged for payment review
and returned by hand on a pooled order, returned to the buyer's refund address by a stray refund on a Monero escrow order.
Set a lower `MONERO_MAX_TIP_AGE` (down to `20m`) to narrow the window. A top block the daemon cannot report
(`get_last_block_header` fails) counts as not synced on the pooled check and skips the Monero escrow pass.

With `ESCROW_XMR=on`: Monero escrow also accepts a stagenet daemon (the escrow wallet-rpc then runs with `--stagenet`),
the accepted address forms follow the daemon's network, the height check (below 1,000,000) is gone, and the app pins the
daemon's network in `settings` (`escrow_xmr_network`). An existing fakechain escrow database is pinned `fakechain` at the
first start; pointed at a stagenet daemon instead it stays disabled and changes nothing, with a line under *Payment
providers* naming both networks: a database never changes network once it holds a Monero escrow or escrow wallet. The
startup line is now `escrow: Monero escrow on (fakechain)` (or `(stagenet)`) instead of `… on (local fakechain only)`; a
log filter on the old text needs updating. The Monero escrow watcher skips its pass while its daemon is not synced (as
above; a fakechain is exempt from the peer and tip-age checks)
([operator guide](docs/operator-guide.md#monero-escrow)). With the pooled Monero wallet configured
too (`MONERO_WALLET_RPC_URL`), Monero escrow now also waits until the pooled check has passed once, logging "escrow:
Monero escrow waiting: the pooled Monero wallet (MONERO_WALLET_RPC_URL) has not passed a check yet, …" (QA4W3-2): only
then is the pooled wallet's address known, and an escrow wallet-rpc whose open wallet has that address is the pooled one
under another URL, which is refused. No action is needed otherwise.

`compose.nodes.yaml` gains an optional `monero-escrow-wallet` service (Monero escrow's own stagenet wallet-rpc on the
bundled `monero` daemon: profile `monero-escrow-wallet`, volume `monero_escrow_wallet`, login password
`ESCROW_XMR_WALLET_RPC_PASSWORD`), and a new overlay `compose.escrow-xmr.yaml` mounts the seed key file
`escrow-xmr-seed-key.asc` into the app. Nothing starts unless the profile is in `COMPOSE_PROFILES`; a new install gets
them from `./scripts/install.sh --monero-escrow yes` (or its question), and an existing one can add them by hand as the
operator guide describes. `upgrade` derives the profile from `ESCROW_XMR_WALLET_RPC_URL` and the overlay from
`ESCROW_XMR_SEED_KEY` only when `.env` has no `COMPOSE_PROFILES` or `COMPOSE_FILE` line.

Compose now refuses a blank wallet-rpc password (A-466): with the `monero-wallet` profile active and
`MONERO_WALLET_RPC_PASSWORD` blank or missing, every `docker compose` command stops with `env file
…/MONERO_WALLET_RPC_PASSWORD-is-blank not found`, and likewise for `monero-escrow-wallet` and
`ESCROW_XMR_WALLET_RPC_PASSWORD`. Before, the bundled `monero-wallet` service started with the login `marketplace:` (an
empty password). The installer generates the password, so mainly a hand-made `.env` is affected: before
upgrading, set it (`openssl rand -hex 24`) in `.env` and as the password in `MONERO_WALLET_RPC_URL`
(`http://marketplace:PASSWORD@monero-wallet:18083`). `./scripts/install.sh check` names the missing key. An install
without these profiles needs neither key.

## 36. Bitcoin escrow broadcast honesty and the Tor-mode gate (A-441)

No migration, no new key. Only with `ESCROW_BTC=on`. A settlement recorded broadcast whose transaction the escrow
watch-only wallet has not seen 6 blocks later now tells staff once ("Queued or stuck: not seen by the network", an
order note and a "Payment review needed" notification) and is listed on `/admin` until the wallet sees it. With
`APP_MODE=tor`, escrow settlements are now broadcast only while the escrow node's `getnetworkinfo` shows a proxy for
IPv4 and IPv6 on Bitcoin Core 31.1 or later; otherwise they wait, with a line on `/admin`. A Tor-mode market using escrow
therefore needs `getnetworkinfo` on the escrow user's `rpcwhitelist` (it is called on regtest too in Tor mode) and the
escrow node started with `-proxy=<Tor SOCKS address>`, never `-connect` or `-onlynet`
([operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow), *Tor mode*). Clearnet mode and pooled payouts
are unchanged.

## 37. Escrow-only payment intake and the readiness panel (A-444)

No migration, no new key, nothing changes on upgrade. The per-currency intake setting (`payment_intake_BTC`,
`payment_intake_XMR` in `settings`, migration 052) takes a third value, `escrow`: new orders in that coin can only use
escrow checkout, and the pooled wallet issues no new payment address. The admin form under *New payment addresses* now
offers **On**, **Escrow only** and **Paused** per coin (password, audited, reversible), and offers escrow only and
paused even when that coin has no pooled wallet configured (On still needs one). A new *Escrow-only readiness* panel
below it counts and lists the active listings that cannot use escrow and the vendors with no active Bitcoin escrow key.
How to cut over and back: [operator guide](docs/operator-guide.md#escrow-checkouts-and-readiness).

Leave the pooled wallet configured after a cutover until the pooled orders still open are settled: the pooled watcher
runs only while the provider is configured.

Rolling back the binary while a coin is set to escrow only: a build that knows this database's migrations but predates
A-444 refuses every intake value other than `true` (its `lockPaymentIntake` compares the value with `true`, and its
escrow checkouts call the same check), so it treats that coin as **paused for pooled requests and escrow checkouts
alike**. Its admin page still reads "Enabled when wallet is available" (with a *Pause* button when that coin's pooled
wallet is configured) and its order page still offers *Request payment address*, which is refused with the paused
message. When that coin's pooled wallet is configured, set the coin back to **On** before rolling back (this build's
form), or with the older build's form afterwards.

When that coin has **no pooled wallet configured**, neither form can do it: the older build shows its intake form only
for a configured provider and its action refuses one that is not, and this build refuses **On** without a pooled
wallet. Escrow checkouts in that coin then stay refused on the older build until the setting reads `true`, which, with
no pooled wallet, admits escrow checkouts and still issues no pooled address. Run this once per escrow-only coin
(`BTC` or `XMR`) after stopping this build and before starting the older one:

```sh
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt \
  -c "UPDATE settings SET value='true' WHERE key='payment_intake_BTC' AND value='escrow'"
```

For Monero use `payment_intake_XMR`. It prints `UPDATE 1` for a coin that was escrow only and `UPDATE 0` otherwise
(`settings` has two columns, `key` and `value`). Coming back to this build later, set the coin to **Escrow only** again with its form. A release older than
this database's migrations does not start against it at all ([Rolling back](#5-rolling-back)).

## 38. Bitcoin escrow: a payment to the arbiter wallet no longer disables escrow (A-471)

No migration, no new key. Only with `ESCROW_BTC=on`. **Action: add `listsinceblock` to the escrow user's
`rpcwhitelist`** (the line in the [operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow) ends
`…,gettransaction,listtransactions,listsinceblock`) and restart bitcoind; without it escrow stays disabled with
"escrow: Bitcoin escrow disabled: the arbiter wallet's transactions could not be read …" and a line on `/admin`. It is
read-only and exports nothing.

The start check used to refuse an arbiter wallet with any transaction (`getwalletinfo` `txcount` above 0), but every
escrow order's descriptor shows the market's child key, whose address the arbiter wallet owns: any party could pay it a
few satoshis and escrow stayed disabled from the next restart on. Now only a spend from the arbiter wallet (an entry of
category `send`, which only its keys can make; `listsinceblock` with `include_change`, so a spend to the wallet's own
change addresses counts too) counts as using it as an ordinary wallet; coins paid to it are ignored and stay there. A
market whose escrow was disabled this way ("escrow: Bitcoin escrow disabled: the arbiter wallet must be used for the
escrow key only … (getwalletinfo not-dedicated)") starts escrow at its first start after the upgrade, unless the wallet
has spent; a spend is now logged as `(listsinceblock not-dedicated)`.

Reading every entry on every start let mass payments to that address keep escrow off instead (QA4W4A-1: 100,000 entries
outlasted startup). So the first passing check reads the whole wallet once, in one call, and records a checkpoint, the
`settings` key `escrow_btc_arbiter_checked` (the block `listsinceblock` reported, for the wallet's name and
`getwalletinfo` birthtime); later checks read only newer entries. A first read longer than startup allows leaves escrow
off with "escrow: Bitcoin escrow off while the arbiter wallet is checked: …" until the escrow watcher finishes it. A
different arbiter wallet, or one rebuilt with `importdescriptors`, is read whole again.

One case the checkpoint misses: `importdescriptors` into the same arbiter wallet keeps its name and birthtime, so a
descriptor whose keys have already spent brings in sends at or below the checkpoint block, which later checks do not
read. **After importing any descriptor into the arbiter wallet**, delete the checkpoint so that the next start reads the
whole wallet once, then restart the app:

```sh
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt \
  -c "DELETE FROM settings WHERE key='escrow_btc_arbiter_checked'"
```

It prints `DELETE 1`, or `DELETE 0` when there was no checkpoint. A stored checkpoint block that is not a block hash
(64 hex characters) is treated as no checkpoint, and so is a block the node refuses as malformed (`-8`) or does not know
(`-5`). The read refuses an answer with any one JSON token (a string, say) longer than 64 KiB as a read failure
(`bad-reply`); Core writes none that long. It reads each entry key by key, so an entry of any size passes (QA4W6-1: a
payment to the market child's address that its payer fee-bumped 1000 times makes Core list entries of about 67 KB,
which builds before this fix refused at every check, keeping escrow off for good; after upgrading, the next check passes
with no step), and the answer as a whole is limited only by the read's timeout. While such replacements are unconfirmed
the answer grows with the square of their number (1100: 82 MB, read in about 2 s; 3000: 605 MB, 16 s, and about
1.5 GB of the node's memory per read); once one is mined, Core lists the others no more.

A-479 (no step, no new key): after a read of the arbiter wallet that took 5 s or more or timed out, pass or failure, the
escrow watcher's next read waits 1 minute, doubling after each such read up to 1 hour; meanwhile a slow pass counts as a
pass for the other start checks. A quicker read ends the waits, a quick failure is read again every poll as before, and
restarting the app reads at once (the operator guide's *Reads are paced*).

What "never spent" cannot tell apart: `rpcwhitelist` allows methods, not wallets, so where the pooled payments share the
escrow node, a pooled user allowed `sendtoaddress` can spend coins paid to the arbiter wallet by naming its path, and
escrow then stays off from the next check (no key is exported). A node of escrow's own rules that out.

## 39. Bitcoin escrow key layout (migration 088, A-455)

Migration 088 runs at startup. It adds `user_escrow_keys.layout`, `core` (a Bitcoin Core wallet, as every key so far)
or `per_order` (a per-order wallet: Electrum with a BIP39 seed, or Sparrow), with `core` as the default, so every
existing key stays a Core key and stays active. It also adds two checks on `escrow_pubkeys`: a derivation suffix of
three or more steps (a per-order wallet's key, `/<i>/0/0`) may not start with `/0` or `/1`, for any party, and a
buyer's or vendor's may not use the reserved practice index `/2147483647`. Shape (a)'s two-step suffixes (`/0/<i>`) are
untouched. Both checks are added `NOT VALID` and then validated in the same migration; no release before this one
writes a suffix of three steps, so validation finds nothing to refuse. With `ESCROW_BTC=on`, the escrow key page then
lets a user register a per-order wallet key (the BIP48 multisig account key, `m/48h/1h/0h/2h`) and sign its practice
transaction in a per-order practice wallet, and listings and vendor profiles show the vendor's key type. Escrows with
per-order wallets are not built yet: a checkout where either party's active key is a per-order wallet key is refused
with nothing written, and while BTC intake is escrow only (section 37) such a vendor's listings are not available for
purchase in BTC and the readiness panel lists the vendor ([per-order escrow](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/escrow.md#per-order-key-layout-registration-and-practice-a-455-shape-b)).
No action is needed at upgrade. Rolling back past this release needs the pre-upgrade backup, as for every migration
(section 5): the earlier release refuses to start on a database that records migration 088.

## 40. Bitcoin escrow on signet and testnet4 (A-443)

No migration, no new key. Only with `ESCROW_BTC=on`. Escrow now starts against a node on `signet` or `testnet4` as well
as `regtest`; mainnet and testnet3 are still refused, and `BITCOIN_CHAIN`, when set beside `ESCROW_BTC=on`, may now be
`signet` or `testnet4` as well as `regtest`. A regtest market sees no change in behaviour: its chain pin stays
`regtest`, and a database that holds escrows never moves to another chain (start a signet or testnet4 market on a new
database). The startup refusals now name the chains escrow runs on ("refusing to start: ESCROW_BTC=on runs only on
regtest, signet or testnet4 …") and the log line reads "escrow: Bitcoin escrow on (test networks only)" where they said
regtest only; a log filter matching the old text needs updating. The operator guide's
section is now Bitcoin escrow (test networks only) (since section 62, [Bitcoin escrow](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow)), and the
[runbook](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/testnet-runbook.md#11-bitcoin-escrow-on-signet-or-testnet4) lists what a signet or testnet4 market needs
first. An escrow refund, claim, release or fee address given in capitals is now stored in lower case by the chain's
prefix (`TB1…` on signet and testnet4, as `BCRT1…` already was on regtest). The escrow pages name the chain. **No run on
public signet or testnet4 is recorded**: the real-chain legs ran on regtest and on a private signet
(`scripts/test-escrow-regtest.sh --private-signet`).

## 41. Monero escrow: refunds at or below the network fee end too few (A-467)

No migration, no new key. Only with `ESCROW_XMR=on`. A Monero refund or stray refund whose coins are at or below the
network fee is refused by `monero-wallet-rpc` (`transfer` answers -17, -16 or -46); the market now reads the fee the
wallet would take (`get_default_fee_priority` and `estimate_tx_size_and_weight` on the escrow wallet-rpc,
`get_fee_estimate` on the escrow daemon, which monerod's restricted RPC serves; nothing is built or sent) and treats such
a refund as too few, as it already treated one the wallet built with less than 0.001 XMR left after the fee: the refund
ends, its deposits are left out and the stray refund returns them with a later deposit. Before, it waited for good and
staff were told of an "outputs mismatch" or a wallet that "keeps refusing" to build it. A refund waiting so when you
upgrade ends on the next pass; its earlier order note stays. If the fee estimate fails, that refund waits and is
flagged once if it fails again on the next pass; the wallet's other orders carry on. Releases are unchanged
([per-order escrow](docs/escrow.md)).

## 42. Database encoding must be UTF8 (A-481)

No migration, no new key. A database whose `SHOW server_encoding` is not `UTF8` (an external PostgreSQL created as
`SQL_ASCII` or `LATIN1`, for example by a C-locale `initdb` without `-E UTF8`) now stops startup, before the first
migration statement, with `the database encoding is ..., not UTF8` and nothing changed. Before, it ran with silent
differences: on `SQL_ASCII` the image description limit of 250 counted bytes (250 "é" was refused there and
accepted on UTF8), and case folding and sorting follow the database's encoding and locale. The bundled Compose
PostgreSQL is initialised as UTF8 and is not affected, and a database already UTF8 needs nothing. Check yours with
`export DATABASE_URL` and then
`python3 scripts/postgres-tool.py DATABASE_URL psql -c 'SHOW server_encoding'` before you upgrade. The app also now sends `client_encoding=UTF8` on
every connection, and refuses a form field that is not valid UTF-8 or holds a NUL byte (400, "Invalid form: a field is
not valid UTF-8 or contains a NUL character. Nothing was changed."), where it used to accept it on a non-UTF8 database
and fail with a 500 on a UTF8 one. A page or image address (GET or HEAD) whose query string is not valid UTF-8 or holds
a NUL byte is refused the same way (400, "Invalid address: a query parameter is not valid UTF-8 or contains a NUL
character."), where `/product?id=%ff` answered 500 and the catalog search dropped the bytes. The administrator
password tools (`-bootstrap-admin`, `-reset-admin-password` and `install.sh --admin`) refuse a password that is not
valid UTF-8 or holds a NUL byte (typed in a terminal set to `LATIN1`, for one), which the sign-in form could never send;
`install.sh --admin` refuses it before it writes `.env` or starts anything.

To move a non-UTF8 database: the encoding cannot be changed in place, so the data is copied into a new UTF8 database.
Keep the old database until the new one has passed step 6. The market before A-481 sent PostgreSQL UTF-8 text and no
client encoding, so whatever the old database's encoding, the bytes it stored are the UTF-8 it was sent; the steps copy
those bytes unchanged. `scripts/test-utf8-migration.sh` rehearses them on a `SQL_ASCII` and a `LATIN1` database, and
the check's readings in a `WIN1252` and an `EUC_JP` one (CI operations job). In the commands, `OLD_DATABASE_URL` is the
database the market uses now and `NEW_DATABASE_URL` the new one; run them where `psql`, `pg_dump` and `pg_restore` as
new as the server are installed. Each `psql` command runs through `scripts/postgres-tool.py` with its URL variable
exported first (`export OLD_DATABASE_URL NEW_DATABASE_URL`), which passes the URL's password in `PGPASSWORD` and not on
the command line, where other users of the host can read it in the process list.

1. Stop the market (`docker compose stop app`) and pause provider snapshots and backup jobs. Keep public access and
   payment sending disconnected until step 7. Preserve the **current** external retention directory independently of
   the database being converted; see [retention recovery](docs/retention.md#restoring-without-reviving-deleted-data).
   If this original database has never enrolled, announce retention and inventory preservation duties first, then
   enroll it before taking its conversion backup. The host command can migrate a non-UTF8 original database without
   serving requests, running expiry or touching wallets. With Go and this checkout available on the maintenance host:

   ```sh
   retention_tools=$(mktemp -d)
   go build -o "$retention_tools/server" ./cmd/server
   RETENTION_ENABLED=on RETENTION_STATE_DIR=/srv/opsecmkt-retention DATABASE_URL="$OLD_DATABASE_URL" "$retention_tools/server" -enroll-retention
   ```

   Keep `/srv/opsecmkt-retention` private (`0700`, files `0600`) and durable. For an already enrolled original, keep its
   existing current directory and skip this enrollment command; a used ledger needs normal replay, never replacement.
   The command refuses a restored database with a pending recovery gate. If only an old backup remains and the current
   external state is unavailable, the later replay remains blocked; conversion cannot manufacture that missing state.
2. Check the old database for text that is not valid UTF-8:
   `python3 scripts/postgres-tool.py OLD_DATABASE_URL psql -X -f scripts/check-utf8.sql`.
   Before this release nothing checked that form input was UTF-8, and a `SQL_ASCII` or `LATIN1` database stores any
   bytes, so a review, a message or a listing may hold some that are not UTF-8 (a review `Great \xff seller`; in a `LATIN1` database, also
   text another client wrote in `LATIN1`, such as `psql` with its default client encoding). Such bytes would pass the
   restore into the new database unchanged, and every backup of that database would then fail to restore (`COPY failed
   for table "reviews": invalid byte sequence for encoding "UTF8": 0xff`). The check reads every column of type text,
   varchar or char, of an array of one of these (element by element) and of a domain over any of them, the types the
   market stores text in. For each such value it prints a line `not valid UTF-8: table public.reviews, column body, row
   where id = '6f0c2a9e41d7' (ctid (0,1)); no valid multibyte UTF-8 in it, so reading its bytes as LATIN1, this
   database's encoding, is a safe correction; starting: Great \377 seller` (`\377` is the byte 0xff) and it ends with an
   error (psql exits 3). Correct each value in the old database, one statement per line the check printed, with its
   table and its column as printed (quoted where SQL needs it; an array element with its subscript, `column tags[2]`,
   is corrected as `SET tags[2] = ...` with `tags[2]` in place of the column below) and the condition after `row where`
   (the row's primary key):

   - *no valid multibyte UTF-8 in it*: read its bytes as the encoding the line names: the database's own when it is a
     single-byte one (`LATIN1`, `WIN1252`, ...), and `LATIN1` in a `SQL_ASCII` database, which records no encoding, unless
     you know the text was written in another single-byte encoding (then that one). In `LATIN1` the byte 0xff becomes "ÿ":

     ```sql
     UPDATE public.reviews SET body = convert_from(convert(convert_to(body, 'SQL_ASCII'), 'LATIN1', 'UTF8'), 'SQL_ASCII') WHERE id = '6f0c2a9e41d7';
     ```

     Put the encoding the line names in place of `'LATIN1'`. In a `WIN1252` database the line names `WIN1252`: read as
     `LATIN1`, text a Windows client wrote there ("“quote” €") would turn into invisible control characters, and the
     check would pass on them. A byte the encoding does not define (0x81 in `WIN1252`, for one) stops the update with
     `... has no equivalent in encoding "UTF8"`: type the text it should hold then, as below.

   - *it also holds valid multibyte UTF-8*: the value mixes UTF-8 with bytes that are not (a title
     `Cr\303\250me \377 br\303\273l\303\251e`), and reading it as `LATIN1` would double-encode its UTF-8 part ("Crème"
     would become "CrÃ¨me"). In a database whose own encoding is multibyte (`EUC_JP`, for one) the line says so, and
     no reading of the bytes is safe either. Type the text it should hold instead, from a UTF-8 terminal, in
     `python3 scripts/postgres-tool.py OLD_DATABASE_URL psql` after the psql command `\encoding UTF8` (the tool lets
     no `PGCLIENTENCODING` through):

     ```sql
     UPDATE public.products SET title = convert_from(convert_to('Crème brûlée', 'UTF8'), 'SQL_ASCII') WHERE id = '0d3e7b5a92c4';
     ```

     This stores the text as UTF-8 whatever the database's encoding. A database refuses a character its encoding
     lacks (`character with byte sequence ... has no equivalent in encoding "LATIN1"`): there, give the text's UTF-8 in
     hexadecimal instead, `convert_from(decode('e69db1e4baac', 'hex'), 'SQL_ASCII')` for "東京".

   Use the row's key, not its ctid: an update moves the row, so a second bad value of the same row, named by the ctid
   the check printed before, would be left as it was. Only a row of a table without a primary key (the line says `use
   the ctid`, as it does for a key that is not plain ASCII) is corrected `WHERE ctid = '(0,1)'`, and then one statement
   per such row before you run the check again.

   Run the check again until it prints `Every value in the N text column(s) is valid UTF-8.` If a table refuses the
   update (escrow settlement records are frozen by triggers), stop and report it to the project. A correction to a row
   of `audit_events` changes what the signed audit export shows for it: note what you changed.
3. Take the encrypted backup with `BACKUP_DUMP_ENCODING=SQL_ASCII`:
   `BACKUP_DUMP_ENCODING=SQL_ASCII BACKUP_DATABASE_URL="$OLD_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh
   backups/pre-utf8.dump.age`. With it the dump holds every text as the bytes stored. Without it, a `LATIN1` database is
   dumped as `LATIN1` and converted to UTF8 on restore, so **every non-ASCII character will be double-encoded** in the
   new database ("Crème" becomes "CrÃ¨me"); so is one whose database or role has `client_encoding` set to `UTF8`
   (`ALTER DATABASE ... SET client_encoding`), which is dumped as UTF8 converted from `LATIN1`. A `SQL_ASCII` database
   is dumped unconverted either way. Use the variable for this move only: `backup.sh` accepts no other value, and on a
   UTF8 database it changes nothing.

   `scripts/restore.sh` therefore reads, before it asks for confirmation or changes anything, the encoding the
   backup's text was written in and the encoding of the database it was taken from (every dump records it), and
   restores only a dump written in `SQL_ASCII` (the bytes stored) or in `UTF8` from a UTF8 or `SQL_ASCII` database.
   Anything else it refuses, naming both: `Refusing: this backup was taken from a LATIN1 database and holds its text in
   LATIN1. ...` (or `... in UTF8.` for the `client_encoding` case). A dump that does not record its database's encoding
   (not made by `scripts/backup.sh`) is restored only if it was written in `SQL_ASCII`. If a refused backup is all you
   have of the old database (`install.sh upgrade` takes its automatic `backups/pre-upgrade-*.dump.age` without the
   variable, so for a `LATIN1` database it is one), restore it into a temporary database of the encoding of the
   database it was taken from (the first one the refusal names; `LATIN1` below), never the encoding its text was
   written in, which can be one no database can have (`SJIS`). There `pg_restore` turns the text back into the bytes
   that database stored, and you use it as the old database. Set `LATIN1_DATABASE_URL` to `NEW_DATABASE_URL` with the
   database name `opsecmkt_latin1`, and `MAINTENANCE_DATABASE_URL` to `NEW_DATABASE_URL` with the database name
   `postgres` (the maintenance database PostgreSQL creates; `opsecmkt_utf8`, which `NEW_DATABASE_URL` names, does not
   exist until step 4, so nothing can connect to it yet); `scripts/postgres-tool.py` passes each URL's password to the
   tool in `PGPASSWORD`, never on its command line, where other users of the host could read it:

   ```sh
   export MAINTENANCE_DATABASE_URL LATIN1_DATABASE_URL
   python3 scripts/postgres-tool.py MAINTENANCE_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_latin1 ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"
   age -d -i "$AGE_IDENTITY" backups/pre-upgrade-20260101T000000Z.dump.age | python3 scripts/postgres-tool.py LATIN1_DATABASE_URL pg_restore --dbname='' --no-owner --no-acl --exit-on-error
   ```

   Then run step 2 and this step with `OLD_DATABASE_URL="$LATIN1_DATABASE_URL"`. For this intermediate backup only, also set
   `BACKUP_RECOVERY_SOURCE=backups/pre-upgrade-20260101T000000Z.dump.age` on the `scripts/backup.sh` command.
   The helper validates that original encrypted archive and its `.retention.json` digest and gives the converted copy
   the **same original creation and expiry**, including an already-expired deadline. It does not grant a new 30 days or
   bypass a pending restore gate. If the original archive predates sidecars, enroll its authentic original dump creation
   time as [the retention guide describes](docs/retention.md#expiring-backups-and-signed-exports); missing evidence stops
   this recovery path. If a preservation duty requires an expired original or derived copy, register each specific copy
   under that duty before its expiry job can remove it. Keep the current external ledger from the original instance;
   never initialize a replacement ledger from this restored database. Go on from step 4, and drop
   `opsecmkt_latin1` after step 6. Never start the market on it: this plain `pg_restore` applies none of `restore.sh`'s
   payout recovery protection, which step 5 applies to the backup taken of it.
4. Create a UTF8 database next to it, as a role with `CREATEDB`. `opsecmkt_utf8` does not exist yet, so connect to the
   maintenance database: `export MAINTENANCE_DATABASE_URL` (`NEW_DATABASE_URL` with the database name `postgres`, as in
   step 3) and run
   `python3 scripts/postgres-tool.py MAINTENANCE_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_utf8 ENCODING 'UTF8' TEMPLATE template0"`
5. Restore the backup into that empty database with `RESTORE_DATABASE_URL="$NEW_DATABASE_URL" AGE_IDENTITY=...
   ./scripts/restore.sh backups/pre-utf8.dump.age` (the [restore procedure](docs/operator-guide.md#backups-and-recovery),
   including its payout recovery gate and reconciliation, applies unchanged). `pg_restore` does not check the bytes of a
   dump taken unconverted: it does not stop at bytes that are not UTF-8, so a clean step 2 and step 6 are what prove
   the text.
6. Supply the **current** external retention state from step 1 to the new UTF8 database, with the original installation's
   `APP_SECRET_KEY` (or legacy `SETUP_TOKEN`) and other required secrets. Start the current server once in isolation with
   `RETENTION_ENABLED=on`, `RETENTION_STATE_DIR=/srv/opsecmkt-retention`, `DATABASE_URL="$NEW_DATABASE_URL"`,
   `ADDR=127.0.0.1:18080`, `ESCROW_BTC=off` and `ESCROW_XMR=off`. Wait for `http://127.0.0.1:18080/healthz` to answer `ok`, then stop
   that isolated server. This is normal startup replay, not a new enrollment: it reapplies committed erasures, preservation
   and expiry before clearing `retention_restore_required`; payout reconciliation gates remain set. Do not clear the gate
   by hand. `scripts/backup.sh` refuses this new database until replay succeeds. Unset any temporary
   `BACKUP_RECOVERY_SOURCE` before taking ordinary backups of the replayed database.

   Before you switch, prove that the new database's own backup restores, into a scratch database you drop afterwards:

   ```sh
   export NEW_DATABASE_URL
   BACKUP_DATABASE_URL="$NEW_DATABASE_URL" AGE_RECIPIENT=age1... ./scripts/backup.sh backups/utf8-check.dump.age
   python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -c "CREATE DATABASE opsecmkt_utf8_check ENCODING 'UTF8' TEMPLATE template0"
   RESTORE_DATABASE_URL="$CHECK_DATABASE_URL" AGE_IDENTITY=... ./scripts/restore.sh backups/utf8-check.dump.age
   python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -c "DROP DATABASE opsecmkt_utf8_check"
   ```

   (`CHECK_DATABASE_URL` is `NEW_DATABASE_URL` with the database name `opsecmkt_utf8_check`.) The restore must print
   `Restore completed.` If it stops with `COPY failed for table "<table>": ... invalid byte sequence for encoding
   "UTF8"` instead, the new database holds bytes that are not UTF-8 and its backups are useless: do not switch.
   `python3 scripts/postgres-tool.py NEW_DATABASE_URL psql -X -f scripts/check-utf8.sql` names the rows; drop the new database, correct them in the
   old one (step 2) and start again from step 3. Then look at a few accounts and listings with accented characters.
7. Point `DATABASE_URL` (and `BACKUP_DATABASE_URL`) at the new database and start the app.

Not changed: every merged migration, and the signed audit export (but for a row corrected in step 2).

## 43. Monero escrow: refused deposits are recorded and cleared by staff (migration 090, A-480)

No new key. Migration 090 adds the table `xmr_escrow_collisions`. A Monero escrow deposit the database refuses as
another order's escrow account (`payments_xmr_escrow_idx`) is still never written to the ledger, and the order note and
the one "Payment review needed" notification to staff are unchanged; the watcher now also records the deposit (order,
wallet, account, transaction ID, when noted), once per order and transaction, and the record, not the note text, keeps
the note and notification to once. While a record is open, moderators and administrators who are not parties may read
the order, the moderation desk lists it under *Escrow alerts*, and the order page shows them a *Refused Monero
deposits* section where one of them records what was done (see [operator guide](docs/operator-guide.md), moderation);
that clears the alert. The parties see no change. The desk's *Escrow alerts* are now ordered by when each order's open
alert opened rather than by the order's age (QA5W2-3).

Notes written before the upgrade are backfilled: each system order note of the watcher's fixed form ("… Monero escrow
deposit `<20 hex digits>`… to wallet … account … was not recorded: the database refused it as another order's escrow
account. Moderator review required.") becomes one **open** record, noted at the note's time. The note named only the
first 20 characters of the transaction ID, so that is all the record holds (`txid_truncated`); the order page shows it
with an ellipsis. If the watcher meets the same deposit again it completes the transaction ID and writes nothing else
(no second note or notification). So after the upgrade any such order already noted appears under *Escrow alerts* until
staff record what was done about it, even if it was dealt with before; record that. Check for them with
`SELECT order_id, txid FROM xmr_escrow_collisions WHERE disposed_at IS NULL;`. A market that never ran with `ESCROW_XMR=on` has none.

## 44. Per-order Bitcoin escrows: creation and watching (migration 089, A-456a)

Migration 089 runs at startup. On `escrow_pubkeys` it adds a check that every step of every escrow key's derivation
suffix is unhardened and below 2^31, for every party, and that a per-order derivation suffix (three or more steps) is
exactly `/<i>/0/0` with `2 <= i < 2^31-1` (never the practice index, for the market's key either): migration 074 allowed
steps of ten digits, so `/2147483648/0/0` (which names the hardened `/0h/0/0`) passed migration 088's checks, as did a
Bitcoin Core escrow key's `/0/2147483648`. It lets the market's per-order key have an origin with no path (the escrow
signer's rooted account key), it allows the column `per_order_xpub` only on a per-order key (the buyer's, the
vendor's or the market's) and only as a tpub (per-order checkout stores there each order wallet key, the account key's
child `<i>`, the market's being the escrow signer's account key's child `<m>`: public data the market already holds,
derived from a registered or the signer's pinned account key and already in the watch-only wallet's descriptors, kept so
the order wallet can be rebuilt after a party registers another key, and without asking the escrow signer), and it adds
the table
`escrow_siblings` (each per-order escrow's sibling addresses). No release writes such a key or value, so the checks find
nothing to refuse; if it does, the migration stops before changing
anything, the app does not start, and the log reads `migration 089: N escrow key row(s) in escrow_pubkeys have a
derivation suffix (such as …) with a hardened step or a step of 2^31 or more …`: restore the pre-upgrade backup
(section 5) and report it to the project.

Only with `ESCROW_BTC=on`, checkout now picks the escrow's shape from both parties' active keys: two Bitcoin Core keys
make the Bitcoin Core escrow as before; a buyer and a vendor whose keys are of different kinds now read "The vendor's
Bitcoin escrow key is for a per-order wallet and yours is for Bitcoin Core" (or the reverse) instead of the "not
available on this market yet" refusal; two per-order wallet keys are still refused with that refusal. Per-order escrows
are built in this release but switched off in the code until the market can co-sign them through the escrow signer
(A-456b); see [per-order escrow](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/escrow.md#per-order-escrow-creation-and-watching-a-456a-shape-b). The restore floor
of `scripts/restore.sh` and of the restore-protection SQL in section 6 now reads an account's highest escrow index in both
shapes (`/0/<i>` and `/<i>/0/0`), one counter: if you keep a copy of that SQL, take the new one. The escrow RPC user's
`rpcwhitelist` needs no new method (per-order escrows use `deriveaddresses` with a range, `getdescriptorinfo`,
`importdescriptors`, `getaddressinfo` and `getwalletinfo`, all on it already), and there is no new key. With an escrow
signer pinned, the escrow key page now refuses a key that is the signer's own ("This key cannot be used here"). After
restoring the watch-only wallet, restart the app: its first watcher pass imports what the wallet lacks of each per-order
escrow again, rebuilt from the keys stored at checkout (the escrow signer is not needed), in one import that rescans
from the earliest escrow's creation (up to two minutes; it waits while the wallet is already rescanning, as after
`rescanblockchain`). While the watch-only wallet rescans, Core refuses the import an escrow checkout makes, of either
kind: the buyer reads "The escrow could not be created: the market's escrow wallet is rescanning the chain. Nothing was
changed and your order is still a draft; try again in a few minutes", and nothing is written. Expect that after
restoring the watch-only wallet, for as long as the repair's rescan (or your `rescanblockchain`) runs
([testnet runbook](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)).
Rolling back past this release
needs the pre-upgrade backup, as for every migration (section 5): the earlier release refuses to start on a database
that records migration 089.

Development databases only: migration 089 was changed before it was released, and a migration that is recorded as
applied is never run again, so a development database that applied an earlier phase-5 version of 089 keeps its
earlier checks (one refused the market's stored per-order key, so per-order checkout fails there) and must be
recreated. No released database has an earlier version of 089.

## 45. Per-order Bitcoin escrows: open to users, co-signed by the escrow signer (A-456b)

No migration. Only with `ESCROW_BTC=on`, a buyer and a vendor whose active escrow keys are both per-order wallet keys now get a
per-order escrow at checkout, and the market co-signs its settlements and fee bumps through the escrow signer
(`cmd/escrow-signer`), never the Core arbiter wallet, which keeps Bitcoin Core escrows. Per-order escrows are offered only
while `ESCROW_SIGNER_SOCKET` names a signer whose account key matches the pins: with no signer configured, or a signer
whose key differs (`escrow_signer_fingerprint`, `escrow_signer_xpub_hmac`), the order page says "Escrows with per-order
wallets need the market's escrow signer, which is not available on this market right now", checkout is refused with
nothing written and, under escrow-only Bitcoin intake, a per-order vendor's listings are not available for purchase in
BTC (the readiness panel on `/admin` lists the vendor). A signer that does not answer when a settlement waits for it
leaves the settlement signed by its party, with "The market's escrow signer could not co-sign it yet" on the order page
and one log line; the next pass tries again and nothing is rebuilt. No per-order key comes from the signer after
checkout, which stores them: the market contacts it at checkout (for its key), to co-sign, and in its health check (its
key, read at startup and on every watcher pass). While it is down the order pages, setup pages and copies of per-order
escrows are still served and the parties can still sign, a sibling address's stray refund included. Set up the signer
first: see the [operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#escrow-signer). The signer's RPC user needs no new method. A deposit to one of a per-order
escrow's sibling addresses is now returned by a stray refund spending that address's own script, co-signed by the
signer.

Also new for both parties of a per-order escrow: an order wallet setup page (`/escrow-setup?id=<order>`, linked from the
order page, never shown to staff or the other party), and the settlement sign form takes the signed PSBT as a file (at
most 64 KB) beside the paste; it is now a `multipart/form-data` form, the only one besides photo and avatar uploads. A
pasted or uploaded PSBT is no longer refused for a derivation entry under the market's key (Electrum and Sparrow write
one for every key); a signature under the market's key is still refused. No real Electrum, Sparrow or hardware wallet
has been tried with a per-order escrow (ledger items A-457 and A-458); see [per-order
escrow](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/escrow.md#per-order-escrow-settlement-setup-and-upload-a-456b-shape-b). Rolling back past this release
needs the pre-upgrade backup (section 5) once a per-order escrow exists: the earlier release cannot settle it.

## 46. Pooled wallet drain gate and hand settlement of a blocked payout (migration 091, A-445)

Migration 091 adds three read-only SQL functions and changes no table, column or row, so it needs no preparation. Like
every migration it is one-way: a build that does not include it refuses to start on a database that records it
([Rolling back](#5-rolling-back)).

- `pooled_drain_blockers()` lists, one row per item, what still needs the pooled wallet: open pooled orders, payouts
  not sent, flagged pooled deposits whose review is not recorded, pooled deposit addresses the watcher still reads, the
  restore gate (`payments_recovery_required`), issued-address warnings not dismissed, and pooled history on mainnet. A
  later release is planned to remove the pooled wallet's code and to refuse a database for which this list is not empty.
- `pooled_watch_horizon()` is the pooled watcher's time rule for an address (24 hours after its order last changed state,
  30 days after a closed order's address was issued), now read by both the watcher and the drain gate.
- `pooled_pending_deposit()` names a deposit recorded at a pooled address that is neither credited nor flagged and
  still reported by the wallet. The watcher now also reads the address of a closed pooled order past the horizon while
  it has such a deposit, so a deposit below the confirmation threshold when the horizon ended is flagged for review
  once it confirms, or ends once the wallet stops reporting it; before, it was never read again. Every other address
  is read exactly as before.

`/admin` shows the list read-only under *Pooled wallet drain*, below *Escrow-only readiness*, with a count per kind,
the pooled balances as advice, and a sweep note: the market never sweeps the pooled wallet; once the list is empty, move
what is left in it by hand. How to drain: that build's operator guide (section 58; this build's guide no longer carries it).

**Bitcoin pooled RPC user.** The advisory balance is read with `getbalances`, which is now on the pooled adapter's
method list. If you restricted the pooled user with `rpcwhitelist` (section 34), add `getbalances` to its line and
restart bitcoind:

```ini
rpcwhitelist=opsecmkt-pooled:getblockchaininfo,getwalletinfo,loadwallet,getnetworkinfo,getnewaddress,listreceivedbyaddress,gettransaction,sendtoaddress,validateaddress,getbalances
```

Without it the panel reads "balance unavailable" and nothing else changes: the balance is advice and decides nothing.
`getbalances` reads amounts only, of any wallet on the node, as `listreceivedbyaddress` already could. The Monero balance is read with monero-wallet-rpc's `get_balance` (account 0).

**Blocked payouts.** A payout blocked because its recipient never saved a payout address can now be paid by hand. The
simplest route is still to ask the recipient to save a payout address. Otherwise: hold it (*Resolve payout* → *Hold
to pay by hand*, a staff reason of 10–500 characters and your password, audited; a held payout keeps its hold when the
recipient saves an address), pay it from the pooled wallet to an address the recipient gave you through a channel you
trust, then record it (*Record sent by hand*, the wallet's transaction ID, 64 hexadecimal characters stored in lower
case, and your password, audited with the payout and the transaction ID). The market does not check the transaction
ID against the chain, and never sends a recorded payout. A queued payout can now be held through the same form, but
the watcher may claim it first. Wait for the successful hold and reload the page before paying by hand. A queued
payout must be held before recording a hand payment; one being sent or already sent cannot be held or recorded.
The refusal (409, nothing changed) means paying by hand could pay it twice. Releasing a hand hold needs the confirmation that nobody paid it by hand. No new
payout state: a hold is `held` (its reason starts "Held to be paid by hand:"), a recorded payout `sent`. An account
suspension and a restore from backup hold such a payout again and keep that reason after their own, so releasing it
then needs their check as well (the payout-address check, or the wallet check); `scripts/restore.sh` and the manual
restore SQL above keep the reason too.

An account suspension now also marks a payout held by a restore from backup (A-489; older releases did not, for any
restored hold): its "Restored from backup:" text is kept first and the suspension's added after it, so releasing it
needs the payout-address check besides the wallet check (and, for a hand hold, the not-paid confirmation). Before, a
payout restored without an address, given one by whoever had the account's password, could be released after the
suspension without the address check.

Development databases only: migration 091 was changed before it was released (the pending-deposit rule was added),
and a migration that is recorded as applied is never run again, so a development database that applied an earlier
version of 091 lacks `pooled_pending_deposit` and its watcher query fails there; recreate it. No released database has
an earlier version of 091.

## 47. Optional: local nodes over Tor (A-442)

No migration, no new key, and nothing changes unless you add the new `compose.nodes-tor.yaml` to `COMPOSE_FILE`. With
`APP_MODE=tor` and a local node, it gives the nodes a Tor client of their own (`tor-socks`, with its own
`deploy/torrc-socks`), makes the `node_egress` network internal so `bitcoin` and `monero` reach peers only through it,
and starts bitcoind with `-proxy=tor-socks:9050 -privatebroadcast`, which a Tor-mode market using Bitcoin escrow on the
local node needs to pass the broadcast gate of section 36. monerod's proxy flag is unverified, and pooled payouts are
never private. Add it after `compose.nodes.yaml`, run `./scripts/install.sh check` (it refuses the overlay in clearnet
mode or before `compose.nodes.yaml`) and `docker compose up -d --build`, which recreated the `node_egress` network and
restarted the nodes on it with Compose 5.1.1 (the guide says what to do if your Compose refuses). How it works, and exactly what it does and does not make private:
[operator guide](docs/operator-guide.md#local-nodes-over-tor).

## 48. Stalled pooled deposits reach staff review (A-487)

Migration 092 updates the drain panel's explanation; it does not move funds or change its blocker predicates.
For a terminal pooled order, an uncredited deposit still reported by the wallet below its confirmation threshold
is flagged for staff review 14 days after the later of its normal watch horizon and its first recorded observation.
The horizon is the later of 24 hours after the order closed and 30 days after its address was created. A wallet read
failure never starts this review from stale confirmations. A deposit the wallet has dropped or reports conflicted
is not flagged by this rule.

An open review replaces the extra address watch on the drain list. Staff must record a disposition on the moderation
desk; the flag never credits the deposit or queues a payment. The desk shows its confirmations and tells staff to
verify confirmation in the wallet before paying by hand. Confirmed, uncredited deposits belonging to legacy resolved
disputes with an empty settlement outcome also reach this review when the wallet is readable.

No new configuration is needed. Pooled-wallet removal remains gated on an empty real drain list and the public-network
operator evidence; this migration does not remove pooled writers.

## 49. Bitcoin escrow health and arbiter account-key pin (A-451, A-452)

Migrations 093 and 094 add one durable Bitcoin escrow health row with its last successful watcher pass, disabled
reason, node height, pin-match status, arbiter key-pool high-water mark and highest market index. The admin page
shows this history even after escrow is switched off; the row stores no RPC credentials, wallet names or account
keys. Startup with escrow off records that state without contacting the node.

The existing arbiter fingerprint pin gains an HMAC of the account key in `settings`. A fingerprint-only deployment
records it on the first upgraded start only if the observed fingerprint matches. Later account-key changes disable
escrow even when the fingerprint matches. The configured escrow signer's account key is pinned the same way. Restore
the original arbiter/signer wallet keys and original `APP_SECRET_KEY` (or legacy `SETUP_TOKEN` key root): changing the
root changes both HMACs and disables escrow. Never remove or replace these pins while escrows exist. Party-key rows
can be re-registered only after the original root is restored and startup has verified the pinned node and, when configured, signer.
No environment keys are added; see [the arbiter pin](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow).

## 50. Removed XMPP profile values (migration 098)

Migration 098 clears stored XMPP profile values from the live `users` rows. The application no longer reads, saves or renders this field; the database column remains for compatibility. A backup made before migration 098 can still contain the old values. If you restore one, upgrade it before use; deleting or rotating a live database does not erase separate backup files.

## 51. Audit-event ordering indexes (migration 099)

Migration 099 adds indexes for audit history ordered within one account and for the global newest-first audit view. It does not change or rewrite existing audit events. Startup applies it with the other pending migrations; take the normal encrypted pre-upgrade backup and use the standard rollback procedure if the new release must be reverted.

## 52. Durable form-submission receipts (migration 100)

Migration 100 creates the short-lived receipt table used to return the original result when supported forms are submitted again after a lost response. Receipts contain request hashes and routing/result metadata, not submitted form text. They expire after 24 hours and are swept in bounded batches. The application applies the migration at startup; use the normal encrypted pre-upgrade backup and rollback procedure.

## 53. Payment-provider alert episodes (migration 101)

Migration 101 stores a fixed provider key, outage/recovery episode timestamps and per-administrator delivery receipts so a sustained provider alert is sent once per episode across restarts and concurrent watchers. It does not store provider error text, credentials or wallet addresses. Startup applies it transactionally with the other pending migrations; use the normal encrypted pre-upgrade backup and rollback procedure.

## 54. Local Bitcoin RPC uses a salted verifier (A-144)

The local `bitcoin` Compose service no longer puts its RPC password in the container command line. It takes
`BITCOIN_RPC_AUTH=marketplace:SALT$HMAC` from private `.env` as Bitcoin Core's `-rpcauth` verifier. The app still
uses the **same** private `BITCOIN_RPC_PASSWORD` in `BITCOIN_RPC_URL` for HTTP Basic authentication. The verifier
cannot authenticate on its own, but a weak manually selected password can be guessed from it offline; use the
installer's random password. External Bitcoin RPC configurations and disabled local profiles need no verifier.

For an existing local-node install, use the normal backed-up `./scripts/install.sh upgrade`: after saving the prior
`.env`, it derives a fresh salted verifier from the **existing** password and adds `BITCOIN_RPC_AUTH` before
recreating the node. It does not rotate the password, change the app URL or touch the Bitcoin wallet volume.
The previous `.env` copy is mode 0600 for rollback. A second upgrade leaves the verifier unchanged. An existing
mismatched or malformed verifier is never silently replaced: `./scripts/install.sh check` refuses startup with
its key name. Repair it from the current private password, then check again; if you changed the password,
update `BITCOIN_RPC_URL` to the same value as well. Do not put the password on a command line or in a log.

For a manually maintained `.env` with the local `bitcoin` profile, set `BITCOIN_RPC_AUTH` to a Bitcoin Core
`rpcauth` verifier for user `marketplace` and the password in `BITCOIN_RPC_PASSWORD`; the format is
`marketplace:<32 lowercase hex salt>$<64 lowercase hex HMAC-SHA256(salt, password)>`. Use a private editor
and host OpenSSL with the password on standard input, as the checked helper inside `scripts/install.sh` does;
then run `./scripts/install.sh check` before starting the profile. The check
verifies the HMAC without printing either value. Compose refuses to create the local `bitcoin` service when
`BITCOIN_RPC_AUTH` is blank, while inactive profiles continue to render normally.

For a repeatable disposable check on a Docker host with the pinned image cached, run
`python3 scripts/test-bitcoin-rpcauth.py`. It creates only its own internal-network regtest project, checks the
runtime argument and correct/wrong authentication, and removes that project's container and volume. A
run of that saved script on 2026-10-08 passed against Core 31.1: one literal `$` in the actual `-rpcauth`
argument, no password in node arguments, correct-password RPC success, explicit wrong-password authentication
rejection, and the node remained healthy. Its owned project and volume were removed; no public node or wallet was
involved. CI runs the same script after its pinned Core image is cached in the `escrow-regtest` job.

## Appendix: new keys by hand

`./scripts/install.sh upgrade` writes these for you. To add them yourself, use your editor (values in single
quotes, as the installer writes them). Blank values keep a feature off.

| Key | What to put there |
| --- | --- |
| `AUDIT_SIGNING_KEY` | `openssl rand -hex 32` output. Enables signed audit exports; back it up with `.env`. |
| `BITCOIN_CHAIN` | Leave blank to accept whichever test chain the node reports, or `testnet4` / `signet` / `regtest`. `main` makes a mainnet market (real funds; new database; section 62 and the [operator guide](docs/operator-guide.md#mainnet)); `testnet3` is refused with Bitcoin escrow on. See step 3 before setting it for a local node. |
| `BITCOIN_WALLET`, `BITCOIN_RPC_URL`, `MONERO_RPC_URL`, `MONERO_WALLET_RPC_URL` | Obsolete pooled-wallet settings: leave them blank. This build stops at startup, before the database opens, if any is set (sections 58 and 59). Escrow uses the `ESCROW_BTC_*` and `ESCROW_XMR_*` keys of the operator guide instead. |
| `BITCOIN_RPC_AUTH` | Local `bitcoin` profile only: salted Core `rpcauth` verifier for user `marketplace` and the existing `BITCOIN_RPC_PASSWORD` (section 54). `upgrade` adds it for existing local-node installs; `check` refuses a missing or mismatched value. |
| `MONERO_NETWORK` | Blank or `stagenet`; `mainnet` makes a mainnet market (section 62). |
| `MONERO_NETWORK_FLAG` | Leave it out for the bundled Monero services' default `--stagenet`; a mainnet market with them needs the line present and blank, `MONERO_NETWORK_FLAG=` (section 62). |
| `PAYMENT_CONFIRMATIONS_BTC` / `PAYMENT_CONFIRMATIONS_XMR` | Defaults 3 and 10. |
| `PAYMENT_POLL_INTERVAL` / `PAYMENT_EXPIRY` | Defaults `30s` and `24h`. |
| `BITCOIN_MAX_TIP_AGE` | Default `2h` (30m to 24h): a public-network Bitcoin node whose tip is older counts as not synced (section 33). |
| `MONERO_MAX_TIP_AGE` | Default `1h` (20m to 24h): a public-network Monero daemon whose top block is older counts as not synced (section 35). |
| `ESCROW_XMR_WALLET_RPC_PASSWORD` | Only for the `monero-escrow-wallet` service (Monero escrow on stagenet): `openssl rand -hex 24` output, the same value as the password in `ESCROW_XMR_WALLET_RPC_URL` (section 35). Blank, Compose refuses the `monero-escrow-wallet` profile. |
| `ESCROW_XMR_TOR_RELAY` | Blank, or `clearnet-accepted` to relay Monero escrow settlements without the node Tor overlay in `APP_MODE=tor` (section 60). Any other value stops startup. |
| `BITCOIN_PRUNE_MB` / `MONERO_PRUNE_FLAGS` | Local nodes only. Absent keys mean **pruned** (`2000` and `'--prune-blockchain --sync-pruned-blocks'`); `BITCOIN_PRUNE_MB='0'` and `MONERO_PRUNE_FLAGS=''` keep a full node. Read [section 7](#7-local-nodes-are-now-pruned) before deploying if you run a local node. |
| `BITCOIN_IMAGE` / `MONERO_IMAGE` / `MONERO_WALLET_IMAGE` | Local nodes only. Absent keys mean the pinned release images the payout code was verified against; the images alpha.1 made you choose stay in use until you remove or change them (`upgrade` pins the default images left on `latest`). Read [section 8](#8-node-images-are-pinned-to-release-tags). |
| `COMPOSE_PROJECT_NAME` | Not needed: absent means `opsecmkt`, which is what your containers and volumes are named after. Never set another value on this install, or Compose starts it on new, empty volumes. A second checkout on the same host needs its own name; see [Backups and recovery](docs/operator-guide.md#backups-and-recovery). |
| `CLAMD_ADDRESS` / `CLAMAV_IMAGE` | The malware scanner photo uploads need. Leave `CLAMD_ADDRESS` blank (uploads off) and `CLAMAV_IMAGE` at `clamav/clamav:1.5.4-debian`; `./scripts/install.sh add-scanner` turns the scanner on. See [Photo uploads and the malware scanner](docs/operator-guide.md#photo-uploads-and-the-malware-scanner). |
| `DATABASE_CONNECT_TIMEOUT` / `MIGRATION_LOCK_TIMEOUT` / `MIGRATION_TIMEOUT` | Optional; leave blank for `15s`, `10m` and `10m`. Raise `MIGRATION_TIMEOUT` if your database is large; see [Migrations](docs/operator-guide.md#migrations). |

For a local Monero node, add `monero` to `COMPOSE_PROFILES` (for example `COMPOSE_PROFILES='internal-db,monero'`; add
`monero-escrow-wallet` only for Monero escrow, and never the removed `monero-wallet` profile). If you run the Tor onion mirror, add `mirror` there too
(for example `COMPOSE_PROFILES='internal-db,mirror'`) instead of starting it with a `--profile mirror` option,
which drops the other profiles for that command; without it in `.env`, `docker compose up -d` leaves the mirror
out and Compose does not restart its Tor with the app. See
[Optional onion mirror](docs/operator-guide.md#optional-onion-mirror).


## 58. Pooled-wallet removal for fresh installs (A-448)

Mainnet pooled history is itself a drain blocker even after its orders are settled.
Such a deployment must keep the prior release; do not delete records to force this
test-network-only build through migration 107.

Migration `107_remove_pooled_wallet` refuses an older database when
`pooled_drain_blockers()` returns any rows. The refusal names the blocker kinds,
changes nothing in that migration transaction, and records no migration 107.
A restored pre-removal database runs the same check again. Mainnet pooled history
is a blocker even when every order has closed: mainnet escrow is unsupported.

A fresh database needs no drain operation. A drained legacy database passes
migration 107, but migration 108 then refuses it while it holds any pooled payment
history (section 59). New payment intake uses `escrow` or `false` (paused); the
old `true` setting becomes `escrow`. Paused currencies stay paused. The server no
longer creates pooled addresses, polls pooled deposits or sends pooled payouts,
and its old pooled mutation routes cannot settle a legacy debt.

For an existing pooled market, **complete the drain using the previous
migration-106 drain-capable build before switching to this build**. Keep its
wallet recovery, secrets and encrypted database backups. Pause new pooled intake,
settle or record every payout, resolve deposit reviews, wait out address watch
horizons, and clear recovery and address-collision gates following
the legacy drain procedure (section *Draining the pooled wallet* of that build's own `docs/operator-guide.md`, for example at commit `5a0dabcb`; this build's guide no longer carries it).
Do not delete records or force migration 107 to bypass a blocker.

After the drain succeeds, remove the obsolete `BITCOIN_RPC_URL`, `BITCOIN_WALLET`,
`MONERO_RPC_URL` and `MONERO_WALLET_RPC_URL` settings from `.env` (blank also works).
The server refuses any nonblank value before opening the database or contacting
an RPC endpoint. Remove the legacy `monero-wallet` profile without deleting its
volume or recovery. Configure the separate `ESCROW_BTC_*` and `ESCROW_XMR_*`
settings only for supported test networks. Keep shared node authentication and
escrow credentials as required by the operator package.

After migration 107 succeeds, older binaries refuse the newer schema. For a
rollback, restore the encrypted pre-upgrade database into an isolated deployment
and use its matching drain-capable binary and secrets; restarting an older binary
on the new database is not a supported rollback.

No deployed pooled market exists in the current owner setup. Fresh-install and
seeded legacy tests prove the two database paths; they are not real-market drain
evidence. Existing public proof records retain their frozen source revisions.

## 59. Pooled history refused (migration 108, A-492)

No pooled market exists (owner directive 2026-10-11), so this build keeps no pooled payment history and has no page
that shows it: the order page's pooled payment record, the administrator payout table, the account's unsent payouts and
the staff payout count are removed, and so is the escrow checkout's check of a refund, claim or fee address against
old pooled deposit addresses.

Migration `108_refuse_pooled_history` refuses, before serving, any database that holds a row in `payouts` or
`payment_addresses`, even a drained one that migration 107 accepts. The refusal names both counts, changes nothing
(migrations 107 and 108 are applied in one transaction, so neither is recorded) and the server exits non-zero:

```
migration 108 refused: this database holds pooled payment history (N payouts row(s), M payment_addresses row(s)), which this build does not support. Nothing was changed. Keep the previous release on this database; see UPGRADING.md section 59.
```

Keep running the previous release on such a database, with its own backups and recovery procedure; do not delete
pooled rows to force this build through. A database without those rows (a fresh install, an alpha.1 database, or one
that never took a pooled payment) migrates normally; its other records, deposits included, are kept.

After migration 108 both tables refuse every new row (`payouts_refuse_insert` and `payment_addresses_refuse_insert`,
"pooled payments are removed: <table> takes no new rows"). The tables, their constraints and the drain functions
migration 107 reads stay, since migrations are append-only. `scripts/restore.sh` and the manual protection in
section 6 no longer touch `payouts`, and the runbook's gate-clearing SQL no longer reads `pooled_drain_blockers()`.
Older binaries refuse a database that records migration 108, as for any newer migration (section 5).

## 60. Monero escrow relays wait in Tor mode without the node Tor overlay (A-515)

With `APP_MODE=tor` and `ESCROW_XMR=on`, this build builds, checks and stores Monero escrow settlements but relays none
unless `COMPOSE_FILE` includes `compose.nodes-tor.yaml` or `.env` sets `ESCROW_XMR_TOR_RELAY=clearnet-accepted`: the escrow
wallet-rpc's `relay_tx` would otherwise send them from the host's IP address, as earlier builds did. A Tor install with
Monero escrow on and neither set will see "Monero relays held" on `/admin`, one startup log line and a warning from
`./scripts/install.sh check`. Choose one ([operator guide](docs/operator-guide.md#monero-escrow),
*Tor mode*), then restart the app: the stored transactions are relayed unchanged on the next pass, never rebuilt.
`upgrade` adds `ESCROW_XMR_TOR_RELAY` blank. Clearnet installs and installs with Monero escrow off are unchanged. Rolling
back to an earlier build relays every held settlement from the host's IP address on its first pass.

## 61. The bundled Bitcoin node defaults to signet (A-513)

When `BITCOIN_CHAIN` is blank, `compose.nodes.yaml` and `compose.nodes-tor.yaml` now start the bundled Bitcoin node on
signet; earlier builds started testnet4. The installer has always written `BITCOIN_CHAIN` for a local node, so an install
made by it is unchanged. An `.env` with the `bitcoin` profile and a blank `BITCOIN_CHAIN` would, on its next
`docker compose up -d`, start a signet node on a fresh `bitcoin_data` chain directory beside the testnet4 one (the old data
stays in the volume), and a market whose escrow already pinned testnet4 would refuse that node. Set `BITCOIN_CHAIN=testnet4` in `.env`
first if that node is yours. The installer's printed escrow commands now carry `-rpcport=8332` for `bitcoin-cli`
(the bundled node's pinned RPC port) and `--via-compose app` for the `--verify` check; see the
[operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).

## 62. Mainnet escrow opt-in and the fee descriptor (migration 109, A-460)

**Nothing changes for a test-network market.** Migration `109_escrow_fee_descriptor` is additive: it adds the nullable
columns `escrows.fee_descriptor` and `escrows.fee_index`, the sequence `escrow_fee_index`, and replaces the check that an
escrow with a market fee has a fee address with one that also accepts a fee descriptor. Existing escrows keep their
fixed fee address. A market with blank `BITCOIN_CHAIN` and a `MONERO_NETWORK` other than `mainnet` runs as before;
regtest, signet, testnet4, a local Monero fakechain and stagenet keep every check they had. Testnet3 and Monero testnet
are still refused. The Bitcoin escrow startup log line now reads "escrow: Bitcoin escrow on" (it said "escrow: Bitcoin
escrow on (test networks only)"), and the startup refusals name mainnet as the opt-in; update a log filter that matched
the old text. The operator guide's escrow sections are now [Bitcoin escrow](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow) and
[Monero escrow](docs/operator-guide.md#monero-escrow).

**Opting in to mainnet moves real funds, and no mainnet run is recorded.** Mainnet is covered only by unit and database
tests against a simulated node ([verification](docs/verification.md)). Development and tests never use it. To run a
mainnet market, follow the operator guide's [Mainnet](docs/operator-guide.md#mainnet) section in full; in short:

- Start it on a **new database**. A market is either all mainnet or all test networks: `BITCOIN_CHAIN=main` or
  `MONERO_NETWORK=mainnet` makes it a mainnet market, both must agree (`BITCOIN_CHAIN=main` beside a non-mainnet
  `MONERO_NETWORK`, or the reverse, stops startup), and a database that holds escrows of the other class is refused at
  startup. Do not convert a test-network database that holds escrows.
- **Bitcoin:** `BITCOIN_CHAIN=main` with a node that reports `main`. Parties register mainnet keys (`xpub` or `Zpub` on
  `m/48h/0h/<account>h/2h`); the arbiter wallet's account is `m/84h/0h/0h`; the escrow signer reads `BITCOIN_CHAIN` and
  uses `m/48h/0h/0h/2h`. `scripts/escrow-node-setup.sh --chain main` provisions the node users.
- **The Bitcoin market fee must go to a watch-only descriptor on mainnet.** `ESCROW_BTC_FEE_ADDRESS` (or the admin
  page's fee form) takes the fee wallet's receiving descriptor `wpkh([fingerprint/path]xpub/0/*)` or its key expression;
  a plain address is refused at startup and by the admin action on a mainnet market. Each escrow takes one fee index at
  its first release build. A test-network market may keep its fixed fee address or switch to a descriptor.
- **Monero:** `MONERO_NETWORK=mainnet` with a daemon that reports mainnet; the escrow fee address
  (`ESCROW_XMR_FEE_ADDRESS`) must be a mainnet address. With the bundled `monero` and `monero-escrow-wallet` services,
  write a blank `MONERO_NETWORK_FLAG=` line in `.env` (monerod has no `--mainnet` flag); the installer requires it.
  `MONERO_NETWORK_FLAG` defaults to `--stagenet`, so an `.env` without the line is unchanged.

**Restore.** `scripts/restore.sh` now also moves `escrow_fee_index` 50 past the restored records, as it moves
`escrow_market_index`, so a fee index used after the backup is not reused.

**Rollback.** As for any migration (section 5), an older binary refuses a database that records migration 109; restore
the pre-upgrade backup instead.

## 63. Pruned escrow nodes and the watch-only wallet repair (migration 110, A-527)

**Add two read-only methods to the escrow user's `rpcwhitelist`:** `getblockhash` and `getblockheader`. The line now
ends `…,gettransaction,listtransactions,listsinceblock,getblockhash,getblockheader` (the
[operator guide](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/operator-guide.md#bitcoin-escrow) has it, and `scripts/escrow-node-setup.sh` prints it); restart
bitcoind after changing it. The app calls them only on a pruned node, and only when the watch-only wallet repair of
per-order escrows is about to import. On a pruned node without them the repair waits and imports nothing, and the log
repeats `escrow watcher: escrow: watch-only wallet repair waits: the node's prune height could not be read: getblockhash
forbidden`; an unpruned node never calls them.

**What changes.** Before the repair imports a per-order escrow the watch-only wallet lacks (a wallet restored from an
older backup, say), it reads `getblockchaininfo` (`pruned`, `pruneheight`) and the time of the block at `pruneheight`.
An escrow created before that block, or less than 2 hours (Bitcoin Core's rescan window) after it, is no longer imported:
Core would keep its descriptor while answering the import failed, so a deposit made before that block was never seen and
the next pass took the address as held (the order could expire unpaid with nothing refunded). Such an escrow is now listed
on `/admin` (*Escrows the watch-only wallet cannot rescan*) with its order; so is an escrow whose import Core answers as
failed for any other reason, which is then imported again on every pass instead of being taken as repaired. While any
is listed its order does not expire and the orphan sweep leaves `escrow_sweep_required` set. Do not clear the restore gate
until the list is empty; the [runbook](https://github.com/yegamble/opsecmkt/blob/e1c082db8eff76e25516e23362a5b9a101751a68/docs/testnet-runbook.md#an-escrow-the-watch-only-wallet-cannot-rescan) gives the
two remedies (restore the whole `bitcoin_data` volume from a copy taken with bitcoind stopped, or run the node unpruned
until the watcher has rescanned them). Unpruned nodes, and escrows newer than the prune height, are repaired as before.

Migration `110_escrow_watch_unrepaired` adds the table `escrow_watch_unrepaired` (one row per listed escrow: its escrow
ID, why it is listed and since when); it is empty until the repair lists an escrow. **Rollback.** As for any migration
(section 5), an older binary refuses a database that records migration 110; restore the pre-upgrade backup instead.

## 64. New installs default to mainnet (A-523)

**An upgrade keeps your network.** `./scripts/install.sh upgrade` never changes a `BITCOIN_CHAIN` or `MONERO_NETWORK`
value you have. For a key your `.env` lacks it adds only a blank line, and a blank network still means a test network
(signet; stagenet). It never writes `MONERO_NETWORK_FLAG`: absent, the bundled Monero services run stagenet. Nothing in
your `.env`, volumes or database changes because of this section. Only a **new** `init` behaves differently.

**What changes for a new install.** No mainnet run of this software is recorded; mainnet is covered by unit, database
and simulated-RPC tests only. With that said, `init` now writes mainnet for each coin whose local node it runs:

| | Before | Now (new `init` with local nodes) |
| --- | --- | --- |
| Bitcoin | `BITCOIN_CHAIN=signet` | `BITCOIN_CHAIN=main`, `BITCOIN_PRUNE_MB=15000` (about two months of blocks, an estimate), `PAYMENT_CONFIRMATIONS_BTC=6` |
| Monero | `MONERO_NETWORK=stagenet` | `MONERO_NETWORK=mainnet`, a blank `MONERO_NETWORK_FLAG=` line |
| Test networks | the default | `--bitcoin-network signet\|testnet4\|regtest`, `--monero-network stagenet` |

- **Test networks by option.** Name a test network for one coin and the other follows (signet or stagenet, with
  `MONERO_NETWORK_FLAG=--stagenet`). Test chains keep `BITCOIN_PRUNE_MB=2000` and the Compose confirmation default.
- **Mixed classes are refused** before `.env` is written, and `check` reports a mixed `.env`.
- **Warnings are printed** before `.env` is written: real funds, the per-coin size and sync estimates, the prune window,
  the Tor peer-to-peer caveat and the custodial Monero escrow note. Sizes are estimates: Bitcoin Core v31.1 chainparams
  put a full mainnet node at about 856 GB of blocks + 14 GB of chain state, and Monero's README gave 45 GB pruned in
  April 2022 (more today).
- **The disk check only warns.** It compares Docker's data directory with the estimate and continues either way.
- **Monero escrow on mainnet is custodial, with real funds, and off by default.** It needs `--monero-escrow yes` and
  the seed key.
- **A plain Bitcoin fee address is refused on mainnet**: give the fee wallet's watch-only descriptor
  ([section 62](#62-mainnet-escrow-opt-in-and-the-fee-descriptor-migration-109-a-460)).
- **Node images carry their tag's `@sha256` digest** (`BITCOIN_IMAGE`, `MONERO_IMAGE`, `MONERO_WALLET_IMAGE`). An
  existing `.env` keeps the images it has, as before.

**Checks before you rely on it.** Run `grep -E '^(BITCOIN_CHAIN|MONERO_NETWORK)=' .env`: an upgraded install shows what
it showed before. If you meant a test market, install again with `--bitcoin-network signet --monero-network stagenet`.
Do not reuse a test market's database, wallets or signer seed for mainnet
([Enabling payments](docs/install/enable-payments.md#choose-a-network)).

**Rollback.** No migration: nothing to roll back. Section 61 still describes the Compose fallback for a blank
`BITCOIN_CHAIN`, which this section does not change.

## 65. Authentication and escrow address privacy fixes (migration 111)

Password-only sign-in now checks the account's current TOTP and PGP sign-in settings while holding its session-creation
lock. If a second factor was enabled after the password step read those settings, sign-in is refused; sign in again
to complete the new verification step. Existing authenticated sessions and normal factor challenges keep their behavior.

External Bitcoin and Monero refund and claim addresses must be fresh **for the account receiving them**. Another
account's prior use no longer prevents checkout or a claim, which otherwise reveals private payment history and lets
someone reserve another person's addresses. Market escrow, sibling, wallet and fee addresses stay forbidden, and a
vendor still cannot claim to the buyer's refund address for that order. Continue using a fresh address from your own
wallet for every order; this change does not prove address ownership.

Migration 111 backfills `escrow_external_addresses` from existing refund and settlement destinations, then replaces
global external-address uniqueness with transactional reservations per account and currency. Order outcome, deposit
ownership and escrow address constraints stay in place. The reservation records remain in the database and backups.
Run the normal encrypted backup and upgrade procedure before applying it. An older binary refuses a database recording
migration 111; rollback requires restoring the pre-upgrade backup, as in section 5.

The Bitcoin escrow signer now connects directly to `ESCROW_SIGNER_RPC_URL`, ignores environment proxy variables, and
refuses HTTP redirects. Its RPC bodies contain the signer's account private key. If the signer previously relied on
`HTTP_PROXY`, `HTTPS_PROXY`, or a redirecting RPC endpoint, configure a directly reachable node URL before upgrading.

## 66. Retention maintenance (migrations 112–117)

Retention is off by default. An upgrade adds activity/closure clocks, scoped preservation and operational exceptions, restore-journal markers,
financial-closure checks and narrowly authorized escrow retirement; it does not purge historical records until enrollment.
Read [Enabling and operating retention](docs/retention.md), publish the deadlines, inventory copies and register preservation duties before setting
`RETENTION_ENABLED=on`. The first enrollment gives ordinary historical data 30 days' grace and accounts without known activity a bounded 90-day reconfirmation period.

Keep the current external state at `RETENTION_STATE_DIR` independently of PostgreSQL. Compose supplies a private persistent `retention_state` volume.
For host expiry jobs, bind a private host directory and protect its manifest and `backup-preservations.json` register.
Do not replace an enrolled instance's state with an empty or older directory or turn retention off. Startup refuses missing or foreign state,
a sequence older than the database checkpoint and disabled retention; it cannot detect a database and matching ledger rolled back together. Always supply authoritative current state.

Backups now include `.dump.age.retention.json`; copy each archive and sidecar unchanged. Host audit exports likewise include an expiry sidecar.
Schedule `scripts/backup-retention.py expire ... --holds ... --delete` daily after an inventory dry run.
Original deadlines survive copying and export; provider snapshots, log stores and remote locations require corresponding controls.

The Bitcoin wallet RPC whitelist adds `setlabel` for guarded retirement. Regenerate the restricted whitelist with the documented node setup procedure
before retiring Bitcoin escrows. New order retirement rechecks fresh wallet finality, preserves non-reuse floors and hashes, and removes original order labels.
Imported Core descriptors and old wallet files remain an operator rotation/destruction task; destroyed Monero wallet files also require physical cleanup.
Review the wallet cleanup register and preserve any specifically required evidence before destroying copies.

Enroll the current original database and retain its external state before relying on a pre-enrollment rollback backup.
Restore stays isolated and needs that current external retention state to reapply lifecycle deletion before traffic.
It retains all payout and reconciliation gates. If an old snapshot predates a replacement administrator, use the documented fresh-account/host-role recovery;
do not revive erased credentials. An older binary refuses these migrations. Rolling back application code does not authorize restoring deleted data:
follow section 5 together with the current retention manifest and preservation duties.
