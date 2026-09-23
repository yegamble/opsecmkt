---
description: Scrutinise whether a scoped OPSEC Market task is actually complete
argument-hint: "<scope or completion claim>"
---

Review $ARGUMENTS using .claude/review-loop.md. If no scope was supplied, use the
current user task. Review the implementation, tests and documentation against
that scope. Stay read-only unless the current user task authorises fixes.
Return evidence-backed required findings, checks performed, unverified areas,
and a completion verdict. If fixes are authorised, iterate through fixes,
verification and fresh scrutiny before reporting completion.
