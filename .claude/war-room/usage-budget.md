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
4. **Skip Rounds B/C** when Round A produced no disputed BLOCKER or REQUIRED
   item.
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

Quality gates do not change: the full gate, CI passed, evidence tiers,
test-network only and no browser JavaScript.
