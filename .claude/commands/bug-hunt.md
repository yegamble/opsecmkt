---
description: Proactively hunt OPSEC Market for workflows that don't work and real bugs — index the codebase, run live workflows and static hunters in parallel, adversarially verify every finding, file the survivors in the war-room ledger and learn from the rejects. Finds and proves; does not fix.
argument-hint: "[all | <area> | since=<git-ref>] [probe-only | static-only]"
---

You are the **chair of an OPSEC Market bug hunt**. Scope: $ARGUMENTS
(default `all`). Areas: identity · commerce · payments · messaging ·
transparency · staff · ops. `since=<ref>` narrows the hunt to files changed
since that ref, plus their callers.

Read first: `.claude/war-room/repo-map.md`, `.claude/war-room/finding-format.md`,
`.claude/bug-hunt/rules.md`, the open rows of `.claude/war-room/ledger.md`.
Check `git status`; never discard unrelated work. Tell the owner the cost up
front: one indexer (Sonnet), 1–2 probers and 2–3 hunters (Opus, in
parallel), and one verifier per batch of findings.

Agent definitions load when a session starts. If `opsec-codegraph` and the
other hunt agents are not in this session's agent list yet, dispatch
`general-purpose` agents told to read `.claude/agents/hunt/<name>.md` and
follow it exactly.

## 1. Index

Dispatch `opsec-codegraph` for `HEAD`. It writes (or reuses)
`artifacts/bug-hunt/<sha>/codegraph.md`. Everything below cites that path.

## 2. Hunt — one message, all in parallel

- `opsec-workflow-prober` × 1–2 (split by role: buyer+vendor, staff+account),
  each with its own ports and database. Skip if `static-only`.
- `opsec-bug-hunter` × 2–3, one area each, **`isolation: "worktree"`**,
  starting from the codegraph's hot spots and thin-coverage lists. Skip if
  `probe-only`.

Give each the commit SHA to hunt (worktrees start from `main`, so hunters must
check it out), the scope, the codegraph path, the rules file and the ledger IDs to
skip. Nothing else, so their results stay independent.

## 3. Verify

Batch every returned finding (drop exact duplicates first; keep the better
repro) and dispatch `opsec-bug-verifier` on each batch. Only `CONFIRMED`
findings, and `PLAUSIBLE` ones rated BLOCKER, go forward.

## 4. File and learn

- Write the run to `artifacts/bug-hunt/<sha>/report.md`: every finding with
  its verdict, repro and the workflows that passed.
- Add each surviving finding to `.claude/war-room/ledger.md` as a new row in
  the right priority table, with status `open (bug-hunt <date>, unruled)`,
  its evidence tier and the repro test name. The war room rules on it; the
  hunt never changes code.
- For each `REJECTED` finding, append the verifier's proposed rule to
  `.claude/bug-hunt/rules.md` under "Rules learned" with the date and ID,
  unless an existing rule already covers it.
- Confirm every database, server, wallet simulator and worktree the hunt
  started is gone.

## Report to the owner

Findings by severity (CONFIRMED / PLAUSIBLE), the workflows that were driven
and passed, what was rejected and the rules learned, the ledger rows added,
and what was not covered. Never call a workflow "working" that no agent ran.

To make the hunt recurring, the owner can run `/loop 24h /bug-hunt since=main`
locally, or `/schedule` a cloud routine; do not set one up unasked.
