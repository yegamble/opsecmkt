# Completion scrutiny loop

Adapted from Vidra's evidence-first review rounds, Ralph completion gates and
bounded Stop hook for this single Go/PostgreSQL repository.

1. Establish the scope and acceptance criteria from the user's request. Inspect
   git status first; preserve unrelated work. Read relevant code, templates,
   migrations, tests and docs before judging whether the task is done.
2. Implement one coherent slice. Stay within the requested scope; review is not
   permission to add features, weaken checks, deploy, push or merge.
3. Run relevant checks from README.md and .ralph/AGENT.md. Record commands and
   actual outcomes, including missing services and skipped suites. A Go pass
   without TEST_DATABASE_URL omits database integration coverage. Playwright
   without E2E_DATABASE_URL proves only the read-only preview.
4. When the slice looks done, switch to a fresh adversarial review pass. Re-read
   the original request and the final diff, including new files. Ask what would
   disprove each completion claim. Trace affected buyer, vendor, moderator and
   administrator journeys from HTML form through authorization, validation,
   transaction/state transition and the rendered result after reload.
5. Where relevant, scrutinise denied access, invalid inputs, empty/error states,
   concurrent or repeated requests, migrations, payment-provider failure,
   no-JavaScript use, mobile/keyboard access, operator setup and documentation.
   Keep payments test-network-only. Never represent preview data, fake wallet
   tests or an unrun deployment check as live payment/deployment evidence.
6. Record each finding with severity, file/line, reproducible trigger, user impact
   and a concrete verification step. Challenge speculative findings against the
   code; reject unnecessary complexity explicitly. Fix required in-scope issues
   and return to step 3. Read-only reviewers return findings to their parent;
   they do not edit. Do not spawn extra agents solely to satisfy this protocol.
7. Finish only after a review of the final state finds no unresolved required
   in-scope issue and applicable checks pass. If a prerequisite is unavailable,
   report what is implemented and what remains blocked/unverified separately.
   For documentation-only work, validate commands, paths and links against the
   repository; unrelated application suites need not be rerun.

For Ralph, persist findings and evidence in .ralph/fix_plan.md. Once its required
items appear complete, spend a separate iteration on completion scrutiny before
EXIT_SIGNAL: true. A new required finding reopens the plan and resets that gate.
An empty plan is not permission to invent a repository-wide project: establish
scope from the user or report a missing scope as blocked.

The Stop and SubagentStop hooks request one additional review continuation,
using Claude's stop_hook_active flag to avoid infinite recursion. They require
Python 3 and fail open on malformed input. They are reminders, not mechanical
proof of a clean review. Honour user stop instructions and genuine blockers;
never keep repeating a failed action without new evidence.
