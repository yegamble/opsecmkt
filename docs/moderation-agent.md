# Moderation agent

The moderation service records abuse-review proposals. It does not change
listings, orders, accounts, roles or funds. Marketplace forms do not wait for a
model response. Existing staff and escrow workflows remain authoritative.

Read the [design and threat model](moderation-agent-design.md), the
[review runbook](moderation-escalation-runbook.md) and the
[operational checklist](signature-hygiene.md) before enabling it.

## Enable the controller without a model

Start the application once to apply migrations 125 through 127 and 134. The migration keeps
moderation enqueueing disabled. No model, runtime download or worker is required
for ordinary application startup.

Prepare a private, one-line database URL file owned by the controller service
account with mode `0600`. Keep it out of prompts, shell history and inference
mounts. Run the separately supervised controller from a clean environment:

```sh
env -i PATH=/usr/bin:/bin ./opsecmkt moderation-worker \
  --enabled --database-url-file /secure/moderation/database-url --once
```

Remove `--once` to keep running. The default is at most 20 jobs per pass, with a
one-minute interval. Enablement persists in the database: subsequent supported
content changes enqueue hashes and bounded metadata in their transaction. New
jobs do not copy listing, dispute, description or review prose. The worker reads
bounded current source text transiently, checks its digest, and sends only a
matching captured revision to inference; changed or deleted sources receive HOLD
without inference. Existing content is not
automatically backfilled. Stopping a worker preserves enqueueing and durable
jobs; it does not disable the feature. To stop future enqueueing:

```sh
env -i PATH=/usr/bin:/bin ./opsecmkt moderation-worker \
  --disable --database-url-file /secure/moderation/database-url
```

This preserves existing jobs, proposals and evidence. Do not delete tables or
rerun setup to clear a queue. `--replay` revisits eligible legacy snapshots;
completed metadata-only jobs are skipped and their count is reported. They
cannot be reinterpreted as empty content or as a newer source revision. An
existing decision for the same snapshot and policy is reused. Model output is not guaranteed bit-for-bit reproducible: the durable
store retains the first validated result for its stable identity.

| Controller option | Default or meaning |
| --- | --- |
| `--enabled` | Off; `MODERATION_ENABLED=on` also enables it |
| `--database-url-file` | Private one-line DSN; defaults to `DATABASE_URL_FILE`; `DATABASE_URL` is accepted only when no file is selected |
| `--once`, `--replay` | Off; one bounded pass, or explicit eligible legacy-job replay; completed metadata-only jobs are skipped |
| `--max-pending` | `256`; global active jobs, allowed 1 through 10000; each actor has at most three active jobs |
| `--max-records` | `4096`; lifetime allocation slots, allowed 1 through 1000000; extra legacy-policy evaluation also requires a slot |
| `--max-reserved-bytes` | `268435456` (256 MiB); conservative logical payload allocation, allowed 512 KiB through 1 TiB |
| `--interval`, `--batch` | `1m`, `20`; intervals 10 seconds through 1 hour, batches 1 through 100 |
| `--model` | Off; inference requires explicit opt-in |
| `--model-socket` | `MODERATION_MODEL_SOCKET`; absolute `.sock` path |
| `--weights` | `MODERATION_WEIGHTS_PATH`; reviewed local weights file |
| `--weights-sha256`, `--model-id` | `MODERATION_WEIGHTS_SHA256`, `MODERATION_MODEL_ID`; weights pin and bounded operator inventory label |
| `--prompt-version`, `--model-timeout` | `moderation-v4`, `20s`; prompt must match this build, timeout 1 through 30 seconds |
| `--sla`, `--confidence-floor` | `24h`, `0.9`; deadline is based on enqueue time, expiry means `HOLD` |
| `--new-actor-days`, `--abuse-threshold` | `14`, `5`; development policy values, not production measurements |
| `--escalation-recipient`, `--signing-key` | Optional OpenPGP public recipient and separate Ed25519 worker seed; both or neither, as described in the [runbook](moderation-escalation-runbook.md#encrypted-offline-packets) |

The worker reads only its selected database and optional escalation-signing
files. It does not import the application's other `_FILE` secrets. The trusted
controller still has database authority; process/file permissions must prevent
the model from obtaining it.

## Optional local inference

Select a reviewed runtime image supporting native Unix-socket HTTP and the
required llama.cpp completion, tokenize and grammar API. There is no selected or downloaded
default image/model. The standalone configuration is
[`deploy/moderation/compose.yaml`](../deploy/moderation/compose.yaml), separate
from application deployment and never started by application redeployment.

Set these non-secret artifact selections in the maintenance shell:

| Variable | Required selection |
| --- | --- |
| `MODERATION_RUNTIME_IMAGE` | Reviewed, already-present image pinned as `IMAGE@sha256:DIGEST` |
| `MODERATION_WEIGHTS_FILE` | Absolute path to reviewed local GGUF, readable by UID `65532` |
| `MODERATION_WEIGHTS_SHA256` | Exact lowercase 64-character SHA256 digest |
| `MODERATION_SOCKET_DIR` | Dedicated empty absolute directory, owned by `65532:65532`, mode `0770` or `02770` |

The controller needs UID or supplementary group access to `65532` for the socket,
plus read-only access to the same weights bytes for its pin check. Do not give
the model the controller's database file, escalation signer or installation
directory. After reviewing the effective configuration:

```sh
python3 deploy/moderation/preflight.py
docker compose --env-file /dev/null -f deploy/moderation/compose.yaml \
  --profile moderation-inference up -d --no-build --pull never
```

The launcher verifies weights, clears the inherited model environment and runs
`/app/llama-server` on `/run/moderation/inference.sock`. The reviewed image must
supply `/bin/sh`, `env`, `sha256sum` and that executable with native `.sock` host
support, `--no-webui` and `--log-disable`. No TCP port is published. The container
has no network, a read-only root and weights mount, a dedicated socket directory,
bounded temporary storage and resource limits. These declarations need a real
selected-runtime rehearsal; they do not attest image contents or a shared host
kernel. A stale socket is not removed automatically: verify the old runtime
stopped before an operator removes its dedicated socket file.

Then add the local adapter options to the controller command:

```sh
env -i PATH=/usr/bin:/bin ./opsecmkt moderation-worker \
  --enabled --database-url-file /secure/moderation/database-url \
  --model --model-socket /run/moderation/inference.sock \
  --weights /models/model.gguf --weights-sha256 REVIEWED_LOWERCASE_SHA256 \
  --model-id REVIEWED_MODEL_LABEL --prompt-version moderation-v4
```

Replace example paths and pins with the controller's actual read-only mounts
and reviewed values. The adapter follows no redirects, DNS, ambient proxy,
remote API or download path. A local weights hash verifies bytes available to
the controller; it cannot attest which bytes a compromised server loaded.

## Trust boundary

The application constructs deterministic context from its database. User text
cannot establish an order's state, a payment invariant, an actor's age or a
signer's authority. Every submitted field is untrusted data, even when it looks
like a system prompt, claims to be an administrator, names the output schema or
imitates a delimiter.

The Go package normalizes and bounds submitted text, tags provenance and records
injection signals. Deterministic checks provide review signals such as duplicate
content, incomplete fields, unusual prices and invalid state proposals. A signal
is not a binding accusation. Missing facts or a failed model call route to human
review; they do not block a vendor or authorize punishment.

Injection markers come in two strengths. Strong markers address the moderator
itself: "ignore previous instructions" and its French, Spanish, Chinese and
Russian forms, spaced-letter, punctuated or leetspeak spellings, "forget your
rules", claimed operator instructions, "system prompt", protocol field names,
underscore code names such as `NO_FINDING` or `AUTO_APPLY`, the canary format,
imitation delimiters, and any of these inside a base64 token or split across two
adjacent fields. A strong marker records `INJECTION_SUSPECTED`, skips the model
and escalates. Weak markers are ordinary words that attacks also use ("you are",
"act as", "decision", "confidence", "execute", "override", "disregard",
"schema", "canary", role labels, claimed staff approval): they add
`HUMAN_REVIEW` only, so the model still runs but the item cannot reach automatic
history. Likewise, any recognised external destination (onion, I2P, Telegram,
Session, SimpleX or XMPP contact, Monero or Bitcoin-like address) adds
`HUMAN_REVIEW`, wherever the solicitation is. `OFF_PLATFORM_CONTACT` remains the
narrower finding for a destination and solicitation in the same clause.
`tests/fixtures/injection/corpus.json` labels each case `escalate`,
`human_review` or `miss`; known misses (other languages, phrases split around an
intervening field, other encodings, paraphrased intent) reach the model, whose
recall is unmeasured.

Local inference uses two stages. Stage A extracts bounded findings from the
untrusted material. Go validates those findings before Stage B receives them
with trusted context. Stage B does not receive the original material, and every
validated Stage A finding stays in the final result whatever Stage B returns. Both
outputs are constrained and validated; neither stage receives tools or execution
authority. Structurally valid output can still be wrong or malicious, so an
independent deterministic and authorization boundary remains necessary.

The model never publishes a listing. Listing hold mode (*Admin → Vendors →
Listing review*, `products.review_state`, migration 136) is independent of
capture: in hold mode a vendor's new or edited listing stays hidden until a
moderator or administrator approves it on the moderation desk, whatever the
proposal says. Capture, its overflow, `AUTO_APPLY`, confidence and the model
path never read or write the review state (`TestModerationNeverWritesReviewState`),
so a listing whose capture overflowed or was never inferred stays held, and a
listing published in publish mode is not held by a proposal.

## Proposal protocol

The `moderation-v4` model response has exactly these seven JSON fields. Additional fields, missing fields,
unknown enums, invalid bounds and unrelated evidence references are refused.

| Field | Meaning |
| --- | --- |
| `item_id` | The fixed item alias `ITEM` supplied in the request |
| `action` | An allowlisted suggestion, such as `NO_ACTION` or `FLAG_REVIEW` |
| `confidence` | Finite value from 0 through 1; uncertainty must reduce confidence |
| `reason_codes` | Codes a model may assert: `SPAM_TEMPLATE`, `SCAM_TEMPLATE`, `OFF_PLATFORM_CONTACT`, `INJECTION_SUSPECTED`, `DETERMINISTIC_CONFLICT`, `HUMAN_REVIEW`, or `NO_FINDING` alone |
| `injection_suspected` | Whether hostile instructions or protocol manipulation were detected |
| `evidence_ids` | Aliases `E1`, `E2`, … for this item's references |
| `canary` | Exact reserved response-integrity token supplied for this request |

Stage A may report only `SPAM_TEMPLATE`, `SCAM_TEMPLATE`, `OFF_PLATFORM_CONTACT`,
`INJECTION_SUSPECTED`, `HUMAN_REVIEW` or `NO_FINDING` alone. Duplicate, price,
completeness, normalization, novelty, actor, money, confidence, model-failure,
protocol, policy and deadline codes are added by Go alone; both grammars and Go
validation refuse them from a model. Go maps the aliases back to trusted
references, so no item, content or actor hash reaches either prompt. Stage B sees
duplicate checks as counts and match outcomes, not hashes.

The pinned llama.cpp `/completion` tokenizes the prompt with special-token
parsing enabled. Go's JSON encoding already writes `<`, `>` and `&` as escapes,
and the prompt data also writes `[` and `]` inside string values as `\u005b` and
`\u005d`, so control tokens spelled with angle or square brackets, such as
`[INST]` or `<|im_start|>`, cannot be formed from listing text. The JSON meaning
is unchanged; a bracket-heavy item costs more tokens. Special tokens spelled
without these characters are not covered, and no selected model's vocabulary has
been checked. This is a tokenization guard, not a trust boundary: the data stays
untrusted.

Go derives review summaries and injection-evidence descriptions from validated
codes and references. Model-written prose is rejected. Historical records keep
their immutable bytes, while the current queue also renders their presentation
from codes. Reply drafting is excluded from model actions; v1, v2 and v3
protocol pins are refused.

The action names include suggestions for sensitive actions so those proposals
can be identified and escalated. Their presence in the schema grants no right
to execute them. A release or refund suggestion cannot satisfy a payment
invariant, and an arbitrary model assertion cannot become a human signature.

The canary is not secret and does not establish prompt-injection resistance by
itself. Go schema and reference validation supplement grammar-constrained
decoding; syntax alone cannot establish factual accuracy. Before each stage the
adapter counts the prompt with the runtime's `/tokenize` endpoint and refuses
one that cannot fit beside the 1536-token output limit in the launcher's
4096-token context. It accepts a completion only when the runtime reports an
end-of-sequence or stop-word finish (`stop_type`, or the older `stopped_*`
flags), `truncated` false and a matching `tokens_evaluated`. A runtime without
those fields or that endpoint gets `MODEL_FAILURE` and human review. Longer or
token-dense items, such as long non-Latin listings, therefore reach human review
without a model proposal. The embedded grammar still needs validation against
the operator's selected real runtime.

## Dispositions and failure behavior

| Disposition | Effect |
| --- | --- |
| `AUTO_APPLY` | Record only a benign `NO_ACTION`/`NO_FINDING` classification of text with no external destination or weak instruction marker; no marketplace entity is changed |
| `QUEUE_HUMAN` | Preserve the proposal for authorized review with a deadline |
| `ESCALATE_ADMIN` | Mark the item for administrator attention on the existing protected surface |
| `HOLD` | Preserve current marketplace state and evidence; take no proposed action |

Injection suspicion and high-impact cases escalate. Money, punitive actions,
new actors, novel cases, conflicting facts and low confidence cannot become
automatic punishment. An absent, slow or dead model leaves deterministic checks
available and uncertain cases reviewable. The default when a review deadline
expires is `HOLD`.

No new public listener, browser script, external inference API, model download,
automatic reply publication, money executor or ban executor is provided.

## Input coverage and remaining limits

| Source | Queued content and limits |
| --- | --- |
| Listing create/update | Title, description, category, region, kind and atomic price; bounded category-price sample and previous text fingerprints supply advisory baselines |
| New normalized upload | Description, image metadata, SHA256 of normalized bytes and optional gateway-computed dHash; no pixels or original image reaches inference |
| Dispute create/update | Reason, status/resolution and current order state; money-related cases escalate |
| Review create/update | Body and rating |
| Newly flagged payment | Flag/disposition and current order state; no transaction ID, deposit address or wallet details |

Encrypted messages, shipping addresses, delivery contents, keys and raw upload
bytes are excluded. Migration 127 stores an optional 64-bit perceptual dHash
computed on the upload gateway from the image it already decoded and normalized.
The worker compares a bounded sample of trusted hashes; Hamming distance at
most five, directly or against the hash's horizontal mirror (each row reversed
and complemented; stored hashes are unchanged), is an advisory duplicate signal,
not proof of copying. The gateway shrinks the image to 9×8 with an
area-averaging bilinear filter, so on synthetic textured fixtures recompression,
resizing and a 1% crop stay within five bits; real-photo rates are unmeasured,
and rotation or larger crops are not claimed. Old gateways
may omit dHash and retain exact SHA256 comparison; existing images are not
backfilled. Upgrade the app and apply migration 127 first, then update the
separate gateway: an old app's strict upload envelope rejects the added field,
while a new app accepts old gateways that omit it. Neither
hash comparison nor the text-only model validates listing claims against photos.
Image understanding and vision inference are outside the owner-approved scope;
images contribute duplicate/similarity hashes and basic metadata only.
Stage B receives validated finding codes and evidence aliases, not a full dispute
thread or its original prose. It cannot produce a faithful evidence-rich thread
summary or timeline from that limited context. The core's advisory
order/timer observations do not replace the market's full transactional state
machine or schedule new release/refund actions.

Every `FLAG_REVIEW` reaches the human queue or the higher-priority administrator
queue. A `NO_ACTION` label cannot hide review findings in automatic history.

## Records and recovery

Audit identity includes input and output digests, policy/prompt revision, model
identity and weights digest, decision, reason codes and actor. Repeated work must
reuse the same decision identity instead of creating conflicting outcomes.
Current entity revisions must be checked before a result can be recorded against
them. A review record is not proof that its underlying allegation is true.

New capture records retain source digests and allowlisted order/image metadata,
not source prose. New proposal inputs retain coded metadata only. Completing a
new job atomically records its decision and removes its active raw actor link
from the live row; earlier backups, WAL and dead row versions can still hold
the actor id until they are removed or overwritten. Pending jobs keep the raw
link until a worker completes them; enqueueing continues while the worker is
stopped, bounded by the global pending cap (256 by default);
per-job opaque references do not make the remaining target references anonymous.
Unkeyed digests can confirm guesses about predictable source values.
Source prose exists in memory during evaluation and remains governed by the
original source's access and retention rules.

Legacy snapshots and immutable proposal inputs remain historical records. This
upgrade does not rewrite them, delete evidence, erase backups or change authentic
audit timestamps. They can still contain plaintext and actor links after source
expiry or account erasure. They remain outside ordinary retention sweeps. Keep
the feature off where that conflicts with the selected policy until a reviewed
legacy retention path exists. Restrict database and backup access.

## Capacity and overload

Migration 134 serializes admission against a singleton capacity ledger. Each
actor has at most three active jobs; global pending, lifetime allocation and
reserved logical-byte limits also apply. Each allocation prepays 512 KiB for its
bounded lifecycle. With the default 256 MiB byte budget, bytes bind at 512
allocations, before the 4096-slot ceiling. Initial legacy inventory is accounted
once at migration; runtime admission does not scan the full history.

Duplicates, rollbacks and same-policy cache reuse do not consume another slot.
Completion releases active capacity once; it does not erase historical allocation
or reclaim its reservation. Lowering limits below allocated usage is refused.
Use the bounded controller flags to raise an approved budget, then restart the
controller. Unspecified flags preserve the stored policy; a partial update changes
only its named limits. A no-flag restart can drain prepaid legacy work even when
legacy inventory exceeds the admission limits. This changes collection capacity, never retention or marketplace
state. Application-only redeployment does not enlarge the budget.

At saturation, the listing, dispute or other source transaction still commits.
No clean classification is invented. A fixed, coalesced private overflow warning
records the affected kind and reason without copying content. The private queue
shows usage, configured limits and overflow even when the worker is absent.
Disputes/payment flags receive a five-minute age preference; older ordinary jobs
still overtake new priority work. Existing payment/state timers remain separate.

These are logical payload reservations and bounded row/allocation counts, not a
physical disk, index, WAL, MVCC or backup-size guarantee. Monitor actual database
storage and rehearse the chosen limits and recovery procedure before production.
Model prompts, raw transport errors and runtime endpoints do not belong in public
logs or panels.

Activity-rate signals count at most the newest 1000 captures in the preceding
hour, resolving current target ownership without retaining completed raw actor
links. Deleted targets stop contributing. V2 encrypted escalations coalesce by
source and pattern; unrelated targets are not grouped by actor. Legacy actor/pattern
rows remain unchanged.

## Review and service inventory

On the private administrator connection, open `/admin/moderation`. Ordinary
moderators and public ingress cannot reach this queue. It shows bounded health
classes, prompt and weights pins, last pass, job/proposal/escalation counts and
injection-suspicion counts. Endpoint identities and raw transport errors are
omitted. Current status is the last worker observation, not live runtime
attestation. **Accept**, **Dismiss** and **Hold** annotate a proposal only; the
existing marketplace entity is unchanged.

To include a selected llama.cpp runtime in the existing signed dependency
inventory, explicitly pass `--current llama.cpp=bNNNN` to the separate
`versions-worker`, using its reviewed installed build number. This opts that
worker into the fixed official release source. Existing installations without
that inventory entry make no additional runtime-release request. The app reads
the signed report; opening an administrator page does not query inference or
upstream releases. The reported version is operator inventory, not an observed
runtime binary. See [version-worker setup](install/service-topology.md#version-worker-host).

## Validation and proposal review

The [validation evidence](evidence/moderation-agent-validation.md) distinguishes
native grammar-parser checks, Go/database/browser tests, synthetic process
isolation and unverified production operation. Both current grammars
(unchanged from moderation-v3 in moderation-v4) passed the pinned upstream
parser: 25 cases (6 accepted, 19 rejected) and an independent rebuild with 22 cases, all as expected; this does not
establish correct model judgment.

The [supplied proposal comparison](moderation-proposal-comparison.md) records
which principles were retained, which integration/security examples were
rejected, and which capabilities remain unimplemented.

## Production gates

The operator must choose and review the model/runtime, immutable artifact pins,
CPU or GPU budget, real service isolation, review staffing, SLA and evidence
retention. Default values are conservative development policy, not measured
production tuning. There is no remote-inference or dead-man action path.

Run the package and database tests, the strict documentation build, and an
actual deployed isolation/failure rehearsal before production enablement. Unit
tests can establish adapter behavior; they cannot establish a host's mounts,
network namespace or root administrator trust. See the
[acceptance plan](acceptance.md#moderation-agent) for the required distinctions.
