# OPSEC Market

@AGENTS.md

Go + PostgreSQL, server-rendered HTML/CSS, no browser JavaScript required.
Read README.md for development and testing, docs/implementation-status.md for
capability boundaries, and docs/acceptance.md for applicable acceptance checks.
Follow CONTRIBUTING.md's working contract: verify external audit proposals
against the source, keep code/tests/guides synchronized, and sanitize evidence
before publishing. Keep production credentials and operational private keys out of prompts
and reports. Destructive incident response requires explicit owner authorization;
an external review is not that authorization.

Deployment work must follow `docs/install/binary.md` and
`docs/install/service-topology.md`. The native executable embeds the UI; keep
legacy flags compatible. Production uses remote database and Monero services,
plus a separate upload/scanner host. Original images and image decoding stay on
that gateway; the app accepts only bounded normalized images over its private mTLS
interface. Dependency-release polling runs in a separate worker and reaches the
admin panel only through a signed local report. Version and OPSEC warnings are
administrator-only. App redeployment must preserve endpoint references, keys,
retention state and existing workers; never reprovision them as a side effect.
No-argument interactive terminals open guided setup; preserve headless serving
and explicit flags. Keep private values out of prompts, command previews and
output. Onion service imports must match the full v3 address and native keypair;
never overwrite a populated identity volume or reuse public/admin identities.

Before claiming completion, follow the review loop below. A checked task list or
a green unit suite alone is not evidence that a complete user workflow works.

@.claude/review-loop.md

For Ralph runs, also read .ralph/PROMPT.md, .ralph/AGENT.md and .ralph/fix_plan.md.

War room (multi-seat review and fix loop): see .claude/agents/README.md; run
`/war-room <scope>` or `/war-room-loop`. Its backlog is .claude/war-room/ledger.md.
