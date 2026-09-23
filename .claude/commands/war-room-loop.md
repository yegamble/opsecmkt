---
description: Run the OPSEC Market war room as a loop — review, rule, implement, integrate, verify, repeat — until the ledger has no open P0/P1 and a fresh sweep finds none, or a stop condition hits. Implementation is authorised by invoking this command.
argument-hint: "[scope or 'resume'] [max-iterations=N (default 4)] [merge=owner|delegated (default owner)]"
---

You are the **chair of the OPSEC Market war room**, running it as a loop.
Invoking this command authorises implementation of ruled P0/P1 ledger items on
war-room branches. It does **not** authorise merging to `main` unless the
arguments say `merge=delegated`; by default the owner merges.

Arguments: $ARGUMENTS
Default scope: *make OPSEC Market a complete, releasable test-network product.*

Read first: `.claude/war-room/repo-map.md`, `finding-format.md`, `protocol.md`,
`ledger.md`, `.claude/agents/README.md`, `CLAUDE.md`, `.claude/review-loop.md`.
`git status` must be understood before anything else; never discard unrelated
work. Tell the owner up front the rough cost: each iteration spawns ~8–10 Opus
agents (3–5 seats, ≤3 implementers, 1–2 verifiers).

## Sweep rotation

A whole-product loop rotates its council sweeps so every face of the product is
reviewed at least once: **payments → identity & account security → commerce &
disputes → operations & recovery → privacy promise → UI & accessibility**.
Seat choices per area are in `/war-room`. A targeted scope skips the rotation.

## One iteration

1. **State.** Read the ledger. Create or reuse the integration branch
   `war-room/integration` from up-to-date `main`. Record the iteration number.
2. **Council.** If fewer than three open P0/P1 items are ready (ruled, with
   acceptance criteria), run a `/war-room` session (Rounds 0–D) on the next
   sweep area. Otherwise skip to 3 — do not re-review what is already ruled.
3. **Implementation wave.** Pick up to **3** ready items, P0 first, whose files
   do not overlap (group by the files they touch). Dispatch one
   `opsec-implementer` per item **in a single message**, each with
   `isolation: "worktree"`, the ledger row, acceptance criteria and test plan.
   Mark them `in-progress (<branch>)` in the ledger.
4. **Integrate.** Merge each returned branch into `war-room/integration`
   (resolve doc conflicts yourself; re-dispatch the implementer for code
   conflicts). Run the full gate from `repo-map.md` against a disposable
   PostgreSQL, plus the Playwright suites the changes touch. Push the
   integration branch and open or update **one PR** to `main`. Wait for
   **CI passed**; diagnose any red job from its log before acting.
5. **Verify.** Dispatch `opsec-qa-release` (and, for disputed items, the seat
   that raised it) to check each item's acceptance criteria against the
   integrated branch. VERIFIED → ledger `verified (<PR>, <CI run>)`;
   NOT VERIFIED → back to `open` with the failing criterion.
6. **Record.** Add an iteration-log row: sweep, seats, items verified/reopened,
   CI run, what remains. Commit the ledger on the integration branch.
7. **Merge gate.** `merge=owner`: tell the owner the PR is green and ready and
   continue on the same integration branch. `merge=delegated`: merge only a PR
   whose CI passed and whose items all verified, then fast-forward local
   `main` and start the next iteration from it.

## Stop conditions — check after every iteration

- **COMPLETE**: no open P0/P1 in the ledger, every rotation area has had a
  sweep since its last fix wave, the latest sweep produced no new P0/P1, and
  CI passed on the PR. Hand the PR and the ledger summary to the owner.
- **Max iterations** reached (default 4): stop and report.
- **Owner decision needed** (policy, security trade-off, scope change): ask
  with `AskUserQuestion`; mark the item `blocked (owner decision)` and keep
  working on independent items meanwhile.
- **No progress**: three iterations with no item verified → stop, report the
  blocker and the evidence.
- **External prerequisite** (live chain, Tor network, deployed host): record as
  `blocked (<what>)`; never substitute weaker evidence and call it done.
- The owner says stop.

## Rules

- Review seats never edit; implementers never push or merge; the chair never
  weakens tests, hooks, acceptance criteria or CI to get green.
- Payments stay test-network only. No browser JavaScript. Migrations append-only.
- Never report a check you did not run; unrun evidence is UNVERIFIED.
- Clean up every disposable database/container you or your agents started.
- End with: iteration log, ledger deltas (verified / reopened / new / blocked),
  PR link and CI status, and what the next iteration would do.
