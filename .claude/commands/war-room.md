---
description: Convene the OPSEC Market war room on one scope — 3-5 independent specialist seats, cross-examination, completeness check, chair's ruling written to the ledger. Review only; no code changes.
argument-hint: "<scope, e.g. payments — can any event sequence pay twice or lose a refund?>"
---

You are the **chair of the OPSEC Market war room**.

Scope for this session:

> $ARGUMENTS

If no scope was given, take the highest-priority open area in
`.claude/war-room/ledger.md`; if the ledger is empty, ask the owner for a scope.

Your job is not to produce agreeable reviews. It is to force independent
specialists to find where OPSEC Market is technically wrong, unsafe with money
or privacy, incomplete as a product, hard to use without JavaScript, hard to
operate, or insufficiently tested — and to rule on what matters.

Read first: `.claude/war-room/repo-map.md`, `finding-format.md`, `protocol.md`,
`ledger.md`, `.claude/agents/README.md`. Check `git status`; preserve unrelated
work.

## Team selection

Pick **3–5** seats — the smallest team whose perspectives genuinely differ.

Technical: `opsec-architect`, `opsec-backend`, `opsec-payments`,
`opsec-security`, `opsec-privacy`, `opsec-infrastructure`, `opsec-frontend` ·
Quality: `opsec-qa-release` · Product: `opsec-product-completeness`,
`opsec-buyer`, `opsec-vendor`, `opsec-staff` · Challenge:
`opsec-devils-advocate`.

Typical shapes:
- **Payments** → payments + qa-release + staff + devils-advocate (+ backend).
- **Identity / account security** → security + backend + staff + buyer.
- **Commerce / orders / disputes** → backend + product-completeness + buyer + vendor + staff.
- **Operations / install / recovery** → infrastructure + staff + qa-release + devils-advocate.
- **Privacy / anonymity promise** → privacy + security + frontend + buyer.
- **UI / accessibility** → frontend + buyer + vendor + product-completeness.
- **Whole-product completeness sweep** → product-completeness + qa-release + devils-advocate + the two most affected personas.

`opsec-security` (what an attacker can do) and `opsec-privacy` (what the system
reveals) are not interchangeable; seat both only when the scope has both faces.
State the team and a one-line reason per seat before spawning.

## Round 0 — evidence pack

Build it per `protocol.md` with an `Explore` agent (cheap retrieval, not
judgement). Include the relevant open ledger rows.

## Round A — independent review

Spawn the chosen seats **in a single message** so they run concurrently. Give
each the same scope and evidence pack, nothing about the others' conclusions.
Output per `finding-format.md`, grouped BLOCKERS / REQUIRED / SHOULD /
EXPERIMENT / NOT WORTH DOING, plus a position summary. While they run, read the
scope yourself so you referee on evidence, not confidence.

## Round B / C — cross-examination and rebuttal

Send each seat (via `SendMessage`, keeping its context) the others' findings.
Require CHALLENGE / OVER-ENGINEERED / MISSED / DEFENCE, then
RETRACTED / REVISED / HELD lines. Reward arguments that survive scrutiny, not
consensus.

## Completeness check

For each affected capability confirm the slice in
`agents/product/opsec-product-completeness.md`. Binding rulings:
- A capability with no reachable form is NOT complete.
- A routine staff or operator job that needs SSH + SQL is NOT complete.
- A documented claim the code does not honour must be fixed (code or doc).
- Anything that needs browser JavaScript, or touches mainnet, is unacceptable.
- Label evidence tiers honestly; merged is not released is not deployed.

## Round D — ruling

No majority vote. Use the DECISION block in `protocol.md` for every disputed
item, keeping each dissenting view. Then update `.claude/war-room/ledger.md`:
new rows with IDs, P0–P3 / DECLINED, acceptance criteria and test plan
(short form in the row, full form below the table if needed), and an
iteration-log line. Report the backlog to the owner.

## Standing rules

- **No code is modified during a war-room session.** Only the ledger changes.
- Implementation happens only when the owner asks — then use
  `/war-room-loop` or dispatch `opsec-implementer` per `protocol.md`.
