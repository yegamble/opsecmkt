# Model tiering and usage budget

Owner directive (2026-10-19): be mindful of Claude usage limits and give
low-complexity work to a cheaper model. Each agent's frontmatter sets its
**default** model. The chair overrides it per dispatch with the Agent tool's
`model` parameter, choosing by **what the task risks**, not by which seat it is.

## Tiers

| Tier | Model | Use for |
|---|---|---|
| T1: judgement under adversarial or money risk | `opus` (effort high) | Payments, escrow, wallets, restore and the recovery gate; authentication, sessions, CSRF/Origin, admin ingress and Tor client auth; PGP and crypto; migrations that touch money or identity; devil's-advocate challenge; verification of a P0/P1 item; implementing a P0/P1 item in those areas. |
| T2: careful but bounded | `sonnet` (effort medium) | Persona and product walkthroughs (buyer, vendor, staff, product-completeness); UI, copy and accessibility review; P2/P3 implementation outside the T1 areas; adding tests a verifier already specified; doc-sync fixes with real content; codegraph indexing; live workflow probing; re-verifying a P2/P3 or doc finding. |
| T3: mechanical | `haiku`, or the chair directly with no agent | Ledger rows and status updates; renumbering; wording swaps that are already decided (e.g. "sixteen" → "seventeen"); checking paths, links, line references and grep claims; summarising logs; Round 0 evidence retrieval when the scope is a named file list. |

## Rules

1. **Pick the tier from the riskiest thing the task touches.** A P3 change to
   `escrow_xmr_*.go`, `vendor_bond*.go`, `actions_auth.go`, `admin_ingress.go`,
   `pgp_*.go`, `scripts/restore.sh` or a migration is T1. A P1 copy fix in a
   template is T2.
2. **Never downgrade the challenger for a T1 scope.** `opsec-devils-advocate`,
   `opsec-security`, `opsec-payments` and `opsec-privacy` stay `opus` when the
   scope is T1. A weaker challenger weakens cross-examination (the original
   Vidra rationale, kept for T1 only).
3. **Re-verification is usually one tier down.** If round 1 found only T2/T3
   findings, the same seat re-checks on `sonnet`. If it found a REQUIRED money
   or security finding, it re-checks on `opus`.
4. **Escalate, don't guess.** If a T2/T3 agent reports UNVERIFIED, conflicting
   evidence or a possible money/security defect, re-dispatch that question at
   T1. Record the escalation in the ledger row.
5. **Spend fewer agents before spending smaller ones:**
   - Seat 3 by default; 4–5 only when the extra perspective genuinely differs.
   - Continue a seat with `SendMessage` (its context is already paid for)
     rather than spawning a fresh one.
   - Verify only the findings and acceptance criteria in question, not the
     whole PR again.
   - Wait for CI, poll checks and watch runs in Bash, never in an agent.
   - Do bookkeeping (ledger, PR text, cleanup) in the chair, without an agent.
   - Keep Round 0 to one `Explore` agent at low/medium effort.
   - Skip Rounds B/C when Round A has no disputed BLOCKER/REQUIRED item.
6. **State the tier.** When announcing a team or a wave, give each dispatch's
   model and a one-line reason, and include the expected agent count by tier in
   the up-front cost note to the owner.
7. **Quality gates do not change with the tier.** Evidence tiers, the full gate,
   CI passed, test-network only and no browser JavaScript apply to every model.
   A cheaper model's claim is checked the same way, and never counts as T1
   verification of a T1 item.

## Seat defaults (frontmatter)

| Agent | Default | Notes |
|---|---|---|
| `opsec-payments`, `opsec-security`, `opsec-privacy`, `opsec-devils-advocate`, `opsec-architect`, `opsec-backend`, `opsec-infrastructure` | `opus` / high | Drop to `sonnet` only for a T2 scope (e.g. backend on a copy-only change). |
| `opsec-qa-release`, `opsec-bug-hunter`, `opsec-bug-verifier`, `opsec-pr-reviewer`, `opsec-implementer` | `opus` / high | The chair passes `model: "sonnet"` for T2 work and `"haiku"` for T3. |
| `opsec-buyer`, `opsec-vendor`, `opsec-staff`, `opsec-product-completeness`, `opsec-frontend`, `opsec-workflow-prober` | `sonnet` / medium | The chair passes `model: "opus"` when the scope is T1 (e.g. staff judging payout recovery, frontend on a CSRF or ingress change). |
| `opsec-codegraph` | `sonnet` / medium | Enumeration only. |
| `Explore` (Round 0) | inherits; pass `haiku` or `sonnet` | Retrieval, not judgement. |

The Fable advisor for the chair (`protocol.md`) is unchanged: it steers panel
selection and the Round D ruling only.
