---
description: Codebase-aware review of one OPSEC Market pull request or branch — callers, siblings, invariants and tests around every change — with ranked file:line comments. Posts to the PR only with `post`.
argument-hint: "<PR number | branch> [post]"
---

Review $ARGUMENTS with `opsec-pr-reviewer` (if the agent is not in this
session's list yet, dispatch a `general-purpose` agent told to read
`.claude/agents/hunt/opsec-pr-reviewer.md` and follow it exactly).

1. Resolve the target: a number is a GitHub PR (`gh pr view`, `gh pr diff`);
   anything else is a branch diffed against `main`.
2. Dispatch one reviewer with the target, `.claude/bug-hunt/rules.md` and the
   latest codegraph path under `artifacts/bug-hunt/` if one exists. For a
   diff over ~1500 changed lines, split by area and dispatch up to three
   reviewers in one message.
3. Dispatch `opsec-bug-verifier` on every BLOCKER or REQUIRED comment; drop
   the rejected ones and add their proposed rules to `.claude/bug-hunt/rules.md`.
4. Show the owner the surviving comments. Post only when the arguments include
   `post`, as a single `gh pr review --comment` (never approve or request
   changes on the owner's behalf).
