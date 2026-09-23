# OPSEC Market — Ralph loop

Read CLAUDE.md, .claude/review-loop.md, .ralph/AGENT.md and .ralph/fix_plan.md.
This is one repository: cmd/, internal/market/, web/, scripts/, tests/ and docs/.
Do not copy Vidra's multi-repository assumptions or frontend framework rules.

Each iteration:
1. Confirm the user's scope and choose the highest-priority required plan item.
   If no scope exists, report BLOCKED and request one; do not invent a backlog.
2. Inspect the relevant implementation and acceptance checks. Implement one
   coherent slice, with necessary tests and documentation.
3. Run the applicable checks. Record commands, results and any skipped coverage.
4. Scrutinise the final diff using .claude/review-loop.md. Reopen required issues
   with a concrete trigger and verification step; fix them in subsequent slices.
5. Update fix_plan.md with evidence, outstanding blockers and the next action.
   Preserve unrelated edits and report git state honestly. Commit, push, merge
   or deploy only when authorised by the user's task; never force-push or delete
   branches as loop housekeeping. Only one writing loop may use this checkout.

When all required items look complete, keep EXIT_SIGNAL false and run a separate
completion-review iteration on the final state. Check the original request,
changed user workflows, relevant negative cases, test coverage and documentation.
Required findings reopen the plan. Exit only when that review is clean and all
applicable gates passed. Missing PostgreSQL/browser/deployment evidence is
UNVERIFIED, not PASSING. Stop pursuing a repeated blocker after three iterations
without measurable progress; report what external input or service is needed.
Do not weaken the hook, tests, acceptance criteria or exit rules to obtain a pass.

End each iteration with exactly one block (choose one value for each field):

```text
---RALPH_STATUS---
STATUS: IN_PROGRESS | COMPLETE | BLOCKED
TASKS_COMPLETED_THIS_LOOP: <number>
FILES_MODIFIED: <number>
TESTS_STATUS: PASSING | FAILING | NOT_RUN
WORK_TYPE: IMPLEMENTATION | TESTING | DOCUMENTATION | REFACTORING | DEBUGGING
EXIT_SIGNAL: false | true
RECOMMENDATION: <next action or concrete blocker; mention skipped checks>
---END_RALPH_STATUS---
```

EXIT_SIGNAL is true only for COMPLETE: all required in-scope items verified,
applicable checks passing, and the separate final scrutiny recorded as clean.
User-requested stop takes precedence. Respect Ralph's circuit breakers and budget.
