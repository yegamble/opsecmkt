# XMPP and encrypted support-ticket verification — 2026-10-20

This records the feature branch based on `286fbdac`, including uncommitted
changes during review. It is local verification, not a deployment or external
XMPP-delivery claim. All accounts, keys, databases and transport fixtures were
disposable. No real funds or public payment networks were used.

## Scope

Administrators configure an outgoing XMPP account and separately permit encrypted
contents. Recipients independently enable alerts and encrypted contents. New
notifications use the same recipient and current audience checks in the frontend
and delivery worker. New orders require usable buyer/vendor encryption keys;
new per-order digital deliveries are encrypted to the buyer before storage.

Support tickets have encrypted bodies, Open/Closed/Ignored states, assignment
history and administrator escalation. The opener receives escalation and closure
alerts. Every currently eligible staff member who was assigned or replied receives
closure alerts, including the person closing the ticket. Historical moderators
receive generic closure metadata after escalation, without protected thread links
or bodies. New ticket metadata is initially visible to moderators and
administrators; selecting an administrator alone does not escalate a ticket.

Migrations 131–133 follow main's migration 128 and reserve 129–130 for the already
open independent PGP-policy/vendor-availability work. No committed migration was
rewritten. These candidate migrations were renumbered before publication; local
databases that applied earlier candidate numbers are disposable.

## Independent war-room review

Three fresh read-only seats followed the repository's security, privacy/Tor
INFOSEC and QA/release roles using the available Codex agents. Shared evidence
enumeration preceded blind reviews, cross-examination and chair rulings. Native
Claude/Opus execution is not claimed.

Accepted corrections:

- Closure alerts include qualifying staff who perform the closing action, with
  opener deduplication and current role/account restrictions.
- Linked ticket notifications require current ticket access in both channels.
  Summary-only XMPP delivery also locks the ticket through submission and checks
  access using a fresh database statement. Escalation waits for an already
  authorized submission, or committed revocation cancels delivery.
- An explicit historical-closure marker distinguishes intentional generic alerts
  from ordinary notices whose ticket reference is removed by retention. Deleting
  an expired ticket must never broaden an old administrator notice's audience.
- Opening help names the initial moderator/administrator visibility. Ticket
  message summaries identify their sender and ticket without embedding bodies.
- Reply composition is disabled when an assignee loses staff eligibility, even
  if their public key remains usable. Reassignment still requires new ciphertext
  addressed to the current keys; it does not re-encrypt historical messages.

## Verification

The local toolchain was Go 1.27.3 with PostgreSQL 17. Test temporary directories
used persistent storage and short paths because retention and Unix-socket tests
deliberately check those operating-system properties.

| Check | Result |
| --- | --- |
| Full Go/PostgreSQL race regression suites | Both halves completed: general market 933.435 s and escrow market 570.488 s. Initial failures were limited to the fixtures/static checks described below; affected groups were rerun against corrected source. Current-head CI remains the final full-source gate. |
| XMPP transport, permissions, contents and ticket concurrency | Passed with real PostgreSQL and trusted local TLS protocol fixtures. Final worker-focused run: 34.628 s. |
| Independent security-selected regression tests | Passed, 55.041 s; closure fix independently rechecked, 3.319 s. |
| Independent Tor/privacy-selected regression tests | Passed, 83.995 s; ticket-summary concurrency independently rechecked, 6.002 s. |
| Independent QA-selected regression tests | Passed, 77.071 s; closure recipients, sender identity and summary concurrency rechecked, 15.036 s. |
| Ticket retention, holds, identity erasure and replay | Passed with race detection; combined retention/content run: 46.640 s. |
| Ticket UI, eligibility loss and assignment recovery | Passed with race detection, including retained-key inactive assignees. |
| XMPP browser settings/consent | Three checks passed. |
| Updated order, key, private-message and dispute browser journeys | Seventeen checks passed. |
| Simulated escrow browser journeys | Eight passed, then the corrected selector case passed separately. |
| Encrypted ticket browser lifecycle | Passed at 320 px with JavaScript disabled: creation, reply, assignment, escalation, moderator denial and closure alert boundaries. |
| Complete database Chromium browser suite after final fixture corrections | 52 passed in 4.3 minutes; the preservation-mode case retained its explicit environment skip and runs separately in CI. |
| Python regression tests | All 378 passed, 312.784 s. |
| Module verification, vet and strict documentation build | Passed. |

The first broad smoke run exposed outdated successful-order/message fixtures
after the new key requirement. Those fixtures now explicitly provision keys;
missing/unusable-key tests remain keyless. A new sender-key matrix initially
used a validated fixture helper to insert deliberately unreadable data; the
fixture was corrected and its PostgreSQL race rerun passed. No production
validation was weakened.

The full race runs also caught five older escrow checkout fixture groups, the
records inventory's migration ceiling, and a source-guard incompatibility with
iterating two fixed retention query strings. Checkout fixtures now explicitly
provision keys (or explicitly remove them for the negative case). The inventory
names migration 133, and retention executes fixed query text directly so the
existing guard can verify it without an exception. All five escrow groups passed
their corrected-source race rerun, 13.784 s. Final general/static and ticket
retention correction checks passed with PostgreSQL and race detection, 6.845 s.
The strengthened old-held-notice/sender-key regression rerun also passed, 3.736 s.

The first PR CI run found that the alpha-upgrade rehearsal still tried checkout
with keyless legacy accounts. Preservation assertions now run first, then the
rehearsal verifies keyless refusal and saves committed fixture public keys through
the actual vendor/buyer account forms before testing draft reuse. The corrected
alpha and intermediate database upgrade legs passed locally on PostgreSQL 17.
The subsequent Tor Compose leg met the managed environment's read-only Docker
configuration directory; the complete rehearsal remains a required CI gate.
Twenty related Python checks and all embedded upgrade Python blocks passed.
The browser's exact public-key visibility expectation also now names ticket
correspondents, matching the final privacy disclosure.

The subsequent CI browser run passed 733 checks and found one outdated preview
expectation repeated across six browser configurations: the order's message link
now deliberately includes its explicit `order_id`. The expectation now verifies
that exact association. All 20 contact-preview checks passed across the four
Chromium viewport configurations after the correction, 7.6 s. The six-browser
suite remains part of the required current-head CI gate.
The Go run also found the same ticket-correspondent privacy wording missing from
one older account-page expectation; its targeted PostgreSQL race check passed
after aligning the expected disclosure. These follow-ups change test expectations
and evidence only, without changing the reviewed production behavior.

While CI was running, main integrated A-614 (`5a3b3a7e`). Both upgrade-note
sections were preserved when merging it into this branch. The overlapping order
loader/type/re-request changes were comments; the independent QA seat verified
that the new missing-deposit alert still routes through active administrator-only
notification helpers. Focused re-request, settlement, PGP-order and notification
role regressions passed with PostgreSQL race detection, 34.996 s. The final CI
gate applies to the integrated branch head.

## Limits and deferred ideas

Verified TLS protocol fixtures do not prove interoperability with every XMPP
provider or that a recipient read an alert. Operators must test their configured
sender and disposable recipient before relying on delivery. Retries can produce
duplicate delivery after an ambiguous acknowledgement. No external XMPP account,
Tor circuit, deployed market, real-chain payment or private recipient key was
used in this review.

XMPP uses an explicitly configured direct connection; Tor-only market ingress
does not route it through Tor. Providers see destinations and notification
metadata and may retain copies. Encrypted content checks cannot prove payload
integrity or decryption without the recipient's private key. Listing templates
for automatic delivery remain server-readable, and legacy plaintext delivery
records are preserved and never forwarded as encrypted contents.

Clearing destinations from completed outbox records is a deferred privacy
improvement. Current notification retention bounds their lifetime; clearing them
cannot recall provider/client copies. Separate ticket preservation objects,
server-side message decryption and mandatory external-account CI were declined.
