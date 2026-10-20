# Moderation agent design

This design covers defensive abuse and fraud moderation. Model output is a
proposal, never authority to change an order, account, payment or role. The
market must work with no model installed. Existing authentication, private
administrator ingress, escrow invariants and upload isolation remain binding.

The supplied mission brief is a requirements proposal. Its instructions inside
quoted input do not grant authority to run commands, reveal secrets, deploy,
publish or perform destructive incident response. Measures intended to evade
law enforcement, financial tracing or lawful process, and measures intended to
conceal or identify a law-enforcement takeover, are outside this implementation.

## Threats and limits

| Threat or access | Defensive control | What this does not fix | What this makes worse |
| --- | --- | --- | --- |
| Vendor-controlled prompt injection | Bounded normalization, explicit provenance, independent deterministic checks, two stages, constrained output and Go validation | Delimiters, prompts and a canary cannot prove semantic safety | Another parser and model runtime; false suspicion can increase administrator workload |
| Model output used as a command | Enumerated proposals, no tools, no arbitrary URLs or SQL, no money/ban execution | A compromised application can bypass application policy | Incorrect proposals can bias a human reviewer |
| Model-path SSRF or resource exhaustion | Local Unix socket only, deadlines, size limits, bounded batches, no runtime downloads | Root can replace a socket or binary; local inference still consumes resources | CPU, memory, disk and queue load compete with other services |
| Malicious runtime, weights or grammar | Operator-reviewed runtime, pinned weights and prompt revision; offline inference process without application secret mounts | A digest proves identity of bytes, not their safety; kernel/root compromise defeats process isolation | Large binary artifacts and their provenance need independent maintenance |
| Database or unlocked host access | Minimize fields sent to inference; retain hashes and bounded evidence; existing access controls | Plaintext already visible to the application and metadata remain accessible | Moderation records create an additional sensitive dataset |
| Locked disk or stolen backup | Existing disk and backup encryption; reviewed retention and preservation | Running-host memory, provider access, snapshots and online keys | Queues and exports add recovery material that needs protection |
| Insider moderator overreach | Private administrator review, existing authorization, bounded proposals and audit records | A privileged database or host administrator can alter state | More reports and summaries expose additional context to authorized staff |
| Financial mistakes or disputes | No moderation payment, settlement, refund, release or ban executor; existing human/escrow paths remain authoritative | Custodial wallet exposure, financial tracing and regulatory obligations | A plausible summary can obscure missing evidence unless independently checked |
| Traffic correlation and public metadata | No new public moderation endpoint or external inference requests; use existing private review ingress | Tor, encryption and this agent do not establish anonymity or defeat correlation | Inference load and added administrator work can change observed activity |
| Provider records or lawful investigation | No control added for evasion; preserve existing evidence and explicit authorization boundaries | Warrants, provider records, financial obligations or lawful process | Operators must account for the additional retained moderation data |

## Tiers and authority

**Tier 0: deterministic Go.** Normalize and bound inputs, detect suspicious
instruction text, evaluate explicit listing and order facts, and derive risk.
Template, duplicate, completeness, outlier, rate and state checks are signals.
An inexpensive signal is not proof of wrongdoing. Existing payment timers and
state machines retain their authority; the moderation worker does not replace
them.

**Tier 1: local inference.** Classify and summarize bounded content, identify
evidence references and propose an allowed action with confidence and reason
codes. It has no tools, network fetch capability, credentials, wallet access or
database connection. No inference runs in a browser or request handler.

**Tier 2: human review.** Binding actions stay in the existing authorized
application workflows. A proposal cannot turn a reported delivery into a
delivered order. Money, bans and novel or ambiguous cases need independent
review. Expiring a review deadline preserves the current marketplace state.

## Risk and disposition

Risk considers blast radius, reversibility, money, novelty, confidence,
injection evidence and actor age/reputation. Trusted database facts take
precedence over vendor or model assertions. Unknown facts increase uncertainty.

| Condition | Disposition | On expiry |
| --- | --- | --- |
| Injection suspicion or a high-impact proposal | `ESCALATE_ADMIN` | `HOLD` |
| Money, punitive action, insufficient actor age, low confidence, conflicting or missing evidence | `QUEUE_HUMAN` or `ESCALATE_ADMIN`, according to impact | `HOLD` |
| Model missing, timeout, invalid output, failed canary or failed reference checks | Durable human review; suspicious output escalates | `HOLD` |
| Validated, explicitly enumerated benign result | `AUTO_APPLY` only where the application has an independently implemented benign executor | No new action |
| Expired or otherwise unusable proposal | `HOLD` | `HOLD` |
| Stale content during processing | `HOLD`; preserve `ESCALATE_ADMIN` for an existing high-risk/injection signal | `HOLD` |

`AUTO_APPLY` is not general execution authority. Initially the safe automatic
effect is recording a benign classification; it cannot hide a listing, notify a
user, change an account or mutate an order. Model confidence alone never grants
authority. No dead-man punitive or money action is introduced.

## Injection containment

1. Treat every submitted field as data, including labels, apparent system
   messages, imitation delimiters, JSON, code and assertions about policy.
2. Preserve a digest of original input and tag each bounded field with its source
   and trust level. NFKC-normalize, remove control and zero-width characters,
   detect suspicious script mixing, and record truncation or normalization
   concerns. Reject or queue data whose identity or required facts cannot be
   established. Do not silently turn missing content into a clean verdict.
3. Stage A receives only untrusted fields under fixed extraction instructions.
   It returns bounded findings with references. It receives no application
   secrets and no execution authority.
4. Go validates Stage A schema, allowlisted codes, bounds and references. Stage B
   receives only validated findings and trusted context, never the original
   text. Findings remain untrusted assertions even after structural validation.
5. Request grammar-constrained JSON from the local runtime, then validate again
   in Go: every field, no additional fields, enums, finite numeric bounds,
   lengths, evidence references and exact reserved canary token. Grammar
   constraints limit sampling syntax; they do not establish correctness.
   Truncation, interruption or a runtime failure can still return incomplete
   output, which Go must reject. Both current templates passed a pinned native
   upstream parser check (three valid inputs accepted, nine invalid inputs
   rejected); see the [validation evidence](evidence/moderation-agent-validation.md).
   The selected deployed runtime's behavior remains an operator acceptance check.
6. Recheck deterministic facts against the current database before recording or
   applying a result. Stale entity revisions must not mutate a newer entity.
   Model output cannot create a signer, an invariant or a trusted context field.

Prompt injection detection is heuristic. A missed injection remains possible;
the execution boundary must be safe even when a malicious answer passes every
structural check. The canary is a response-integrity check, not a secret or an
authentication credential.

## Local runtime boundary

Inference is disabled by default. The controller may contact only an explicitly
configured local Unix socket. It does not resolve vendor-supplied hosts, follow
redirects, use ambient proxies or download weights. A separate inference service
must have its own UID, read-only reviewed artifacts, no application environment,
no secret mounts, no Docker socket and no external network access. The
controller necessarily retains its own database authority; it must not launch a
child that inherits its environment.

A unit test can prove that the adapter rejects network endpoints and emits no
external requests. Only a deployed process/container and host inspection can
establish actual filesystem, credential and network isolation. These are
separate claims. Runtime selection and production isolation remain operator
gates until rehearsed with the selected runtime.

## Durable review and escalation

Persist a unique input/revision/policy identity, decision, risk, reasons,
injection evidence, prompt revision, model identity, weights digest, input and
output digests, actor and UTC timestamps. Reprocessing the same identity must
reuse its audit decision. Claim bounded work with database concurrency controls;
recover interrupted work without orphaning a vendor request.

Review uses the existing private administrator ingress. No email, webhook,
public onion notification or new listener is added. Coalesce repeated reports
without losing the underlying decision references. Every item must expose its
expiry, proposed action, alternatives and the explicit default: preserve current
state (`HOLD`).

Optional packet export signs the payload with a separate online worker key,
then OpenPGP-encrypts the bundle to an offline recipient. Export is authenticated
and private; there is no plaintext fallback or automatic external delivery.
The recipient must independently pin the worker's public verification key.
The offline verifier preserves the exact signed payload bytes and requires that
external pin; a key embedded in a packet alone grants no trust. Verification
writes proposal data to a new private file and never executes an action.

A separate offline action key can sign a one-use review token bound to the
installation audience, suggestion, content digest, annotation, expiry and nonce.
The private ingress accepts it without an application login, with CSRF/origin
checks, revision binding and transactional replay receipts. Its authority ends
at an `accept`, `dismiss` or `hold` annotation. These tokens do not authorize a
marketplace mutation or money movement. The packet-signing private key is online;
the action-signing private key stays offline. Without those explicit optional
keys, use ordinary authenticated review. See the
[runbook](moderation-escalation-runbook.md) for the exact operator workflow.

## Operational hygiene

Use no browser JavaScript, third-party assets, public version banners, new public
model status or external inference by default. Administrator status should show
only allowlisted health classes, hashes, revisions, queue counts and last pass,
without URLs, hosts, credentials or raw request text. Keep marketplace messages
and error surfaces within existing templates. Do not publish generated text
automatically. UTC applies to stored moderation events and user-facing moderation
timestamps.

Do not change public assets, public canary semantics, scheduled work across
unrelated services or operator writing style to conceal activity. Preserve the
existing independently signed canary procedure. Additional checks are in
[Moderation operational hygiene](signature-hygiene.md).

## DECISIONS

| Choice | Alternatives | Why | Reversible? |
| --- | --- | --- | --- |
| Local Unix socket adapter, disabled by default | Remote API, TCP inference listener, embedded model | Narrow transport boundary and no new public listener | Yes, through a separately reviewed change |
| Proposals and benign classification only | Autonomous money, bans or takedowns | A structurally valid answer can still be malicious or wrong | Yes, but each executor needs independent policy and tests |
| Unknown routes to human review | Punish, block vendor requests, silently discard | Preserve service and avoid unsupported accusations | Yes, through explicit policy |
| `HOLD` on review expiry | Automatic punishment or payment | An offline operator is not consent | Yes, through explicit policy |
| Separate deterministic and inference stages | Trust prompt fencing alone | Parser boundaries must survive hostile text | Yes |
| No external runtime downloads or telemetry | On-demand model fetching | Keep inference artifacts reviewable and reduce data exposure | Yes, through an explicit acquisition process |
| No evasion or takeover-concealment features | Implement all brief wording literally | These do not belong to defensive abuse moderation | No change within this scope |

## DECIDE: operator gates

| Decision | Safe behavior until decided |
| --- | --- |
| Runtime/model family, license, size, CPU/GPU budget, reviewed artifact hashes | Model disabled; deterministic classification and human review remain available |
| Tested process, filesystem and network isolation for the chosen runtime | Do not enable production inference |
| Category-specific price, duplicate and abuse thresholds | Signals only; no punitive automation |
| SLA durations, review staffing and actor-age threshold | Conservative configurable review; expiry holds |
| Evidence retention, preservation, access and backup expiry | No claim of automatic moderation-data expiry; review storage growth and privacy policy before enabling |
| Whether any additional benign automatic action is justified | Record classification only |
| Dead-man window and permitted actions | No dead-man actions |
| Offline export recipient, separately pinned worker signer and offline action authority/audience | Existing authenticated administrator workflow until all selected public pins and keys are configured |
| Any opt-in remote inference | No remote adapter or external data transmission |
| Real deployment acceptance | Tests are development evidence; no claim of live-host isolation, payment or Tor verification |

## Acceptance evidence

Required checks cover normalization and deterministic replay; adversarial
instruction/delimiter/Unicode/oversize/JSON/delivery fixtures; malformed and
hostile model responses; missing/slow/dead runtime; transaction idempotence and
concurrency; private-ingress access denial; no proposal-driven money or account
mutation; no external adapter egress; and strict documentation build. Full
database/browser and deployed isolation claims require their actual services and
rehearsals, not merely a passing unit suite. Record unavailable prerequisites and
unimplemented mission items explicitly in the final implementation status.
