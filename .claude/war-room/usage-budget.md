# Usage budget

Owner directive (2026-10-19): be mindful of Claude usage limits. The owner then
decided to leave the agents alone, so every review seat, implementer and
verifier keeps its frontmatter model (Opus, effort high, except
`opsec-codegraph`). The seats cross-examine each other, so a weaker seat
weakens the check.

Usage is saved by spending fewer agent turns, never by downgrading a seat:

1. **Seat 3 by default.** Add a 4th or 5th seat only for a genuinely
   different perspective, and say why.
2. **Continue, don't respawn.** Use `SendMessage` for fix-round
   re-verification, cross-examination and follow-up questions. The agent's
   context is already paid for.
3. **Verify only what is in question.** A re-check covers the findings and
   acceptance criteria that failed, not the whole PR again.
4. **Skip Round B+C** when Round A produced no disputed BLOCKER or REQUIRED
   item. When it runs, it is one turn, only for the seats in a dispute, with a
   dispute brief instead of every seat's full report.
5. **No agent for mechanical work.** The chair does it directly:
   - ledger rows and status updates;
   - PR text;
   - renumbering;
   - wording swaps that are already decided;
   - checking paths, links and line references;
   - summarising logs;
   - cleaning up worktrees and branches.
6. **Watch CI and runs in Bash**, never in an agent.
7. **Round 0 retrieval** is one `Explore` agent with `model: "haiku"`, or
   `"sonnet"` when the scope spans many files. It retrieves evidence and does
   not judge.
8. **State the cost up front:** the number of seats, implementers and
   verifiers planned for the iteration.
9. **Read slices, not monoliths.** `ledger.md` is ~1 MB and the two big docs
   are ~250 KB each. Use `ledger.py` and `grep -n` + a line range. Every tool
   call re-sends the transcript, so one whole-file read is paid on every
   later turn of that agent.
10. **Hand over, don't make agents rediscover.** Paste the ledger slice,
    evidence pack, base commit, expected files and migration number into the
    prompt.
11. **Keep passing output out of context.** Run gates through `quiet.sh`;
    read CI failures with `gh run view --log-failed | tail`.

## Proposals considered and declined (2026-10-26)

An external token-budget review proposed these; each conflicts with a rule
above or with the repository:
- Haiku/Sonnet seats and a Haiku security critic: the owner keeps every seat
  on its own model.
- A headless `claude -p` bash loop that marks a task done when one unit test
  and one spec pass, then commits with `git add -A`: it skips the council,
  verifiers, the full DB gate and CI, and stages unrelated work.
- A separate JSONL task queue: a second source of truth that drifts from the
  ledger. `ledger.py live` reads the ledger itself.
- "Never run the full gate inside an agent": implementers must report it. It
  runs through `quiet.sh` instead.
- "Shrink CLAUDE.md and AGENTS.md below 150 lines": they are 36 and 71 lines
  already.

A second review the same day added these, also declined:
- Capping each seat at 3 reads beyond the evidence pack, and each implementer
  at 2 test runs: a seat's job is to find what the pack missed, and the
  implementer must run the full gate.
- Updating `internal/market/schema.sql` with every migration, or reading it
  for current table shape: it is the original foundation schema, unchanged
  since the first commit; migrations define the current schema.
- Requiring a `form_nonce_*_test.go` case for every POST: no such repository
  rule exists; follow the existing CSRF and submission-receipt patterns.
- Verifiers confirming criteria from the diff alone, without re-running the
  named tests: verification is the independent check (in iteration 90 the
  payments verifier reopened A-652a with two REQUIRED findings).
- `SendMessage` only for follow-ups under 500 tokens: continuing an agent is
  cheaper than respawning one that must rediscover the context.
- A `Touch:` column with line ranges: line numbers go stale between ruling and
  implementation. Rulings name files and functions instead, and a new column
  would change every table that `ledger.py live` reads.

Quality gates do not change: the full gate, CI passed, evidence tiers,
test-network only and no browser JavaScript.
