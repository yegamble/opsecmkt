# Supplied proposal and implemented boundaries

The owner supplied two formatting variants of a Gemini response. The response
begins by saying its author could not mount the repository and had not inspected
the migration sequence or handler interfaces. It is design input, not evidence
that its suggested code works in this repository. The shared application URL was
not publicly readable; this comparison uses the supplied text instead.

Source identities are SHA256 `680ce5e255dfccee582caf75157758ef807fb7d32829141b6afb3fa824cfadbb`
for the canonical Markdown variant and
`92ebb9cb1852db72a5b9edb7101d8650323bd3827e0603de2c24fc61a2ad76b4`
for the later formatting variant. No proposal code was installed or executed
verbatim. Claims and integration points were checked against the repository.

| Proposal element | Implemented decision and reason |
| --- | --- |
| Separate deterministic, inference and human tiers; asynchronous worker | Retained. Transactional snapshots and a separate worker keep inference out of marketplace request handlers; unavailability routes uncertain work to durable review. The proposal's worker integration was a TODO, not a runnable repository integration. |
| Local inference, bounded inputs, no tools, two-stage extraction and grammar | Retained with narrower authority. Stage B receives coded findings and references, never raw Stage A prose. Independent Go validation checks structure and references; native parser checks are recorded in the [validation evidence](evidence/moderation-agent-validation.md). |
| Random fences make delimiter breakout impossible | Not a security guarantee. Delimiters and labels are data conventions; grammar and a canary cannot prove semantic safety. No answer gains execution authority. |
| Header-based private-onion middleware | Rejected. The supplied `RequirePrivateAdminOnionMiddleware` trusts `X-Opsecmkt-Onion-Plane` (canonical lines 2328–2339). Only the repository's private listener context grants private ingress; caller-supplied headers never do. |
| Runtime image named `opsecmkt/llama-server:pinned` | Rejected as an immutable pin. A tag remains mutable; the optional recipe requires an actual reviewed `@sha256:` digest and locally pinned weights. |
| Second order graph, timer executor or proposal-driven financial/punitive actions | Not added. Existing authenticated, locked and audited marketplace state transitions retain authority. Moderation records proposals and review annotations; it does not move funds, ban accounts, hide listings or publish replies. |
| Secret isolation or zero egress inferred from fake-client tests | Not accepted as process evidence. Adapter tests, resolved configuration tests and an executed synthetic container probe are separate tiers. Actual selected-runtime operation remains an operator gate. |
| Dispute timelines and photo/claim consistency | Coded findings lack a full thread. Image understanding is explicitly excluded by the owner; dHash and basic image metadata remain advisory duplicate/similarity signals. |
| Retention-duration defaults in configuration | Not an implemented deletion lifecycle. Captured snapshots and proposal inputs persist outside ordinary expiry sweeps. Public records/account notices disclose this, including after capture stops; a guarded minimization/retention/purge policy remains undecided. |
| Offline encrypted, signed escalation and action tokens | Retained with explicit pins and reduced authority. Packet verification uses an independently pinned worker key; one-use, state/expiry/audience-bound action tokens annotate a proposal only. Existing entity and financial authorization is unchanged. |
| Law-enforcement evasion, timing camouflage, takeover concealment or financial-tracing countermeasures | Outside scope. The implementation provides defensive abuse moderation and does not promise anonymity or resistance to lawful process. |

No synchronous inference, caller-controlled authorization header, remote model
API, new public status listener, third-party browser asset or autonomous target
executor was added. Queue retention, real-model usefulness and actual deployment
isolation are [explicit gates](moderation-agent.md#production-gates), not inferred
from the proposal's checklists or sample tests.
