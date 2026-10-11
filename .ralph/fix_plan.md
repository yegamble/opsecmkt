# OPSEC Market — thorough project review

Scope: user requested loops and agents to thoroughly go through this project.
Review existing behavior across marketplace, identity, operations and browser UI;
fix confirmed defects, validate changes, and perform fresh completion scrutiny.
Expanded scope: one-command/browser installation, easy administration, independent
BTC/XMR intake controls, encrypted dispute contacts, and partial-deposit/top-up
order journeys through browser tests and real PostgreSQL.
Preserve earlier README/loop edits. No deployment, mainnet, publishing or git
history changes are in scope. Document intentional limitations rather than
inventing features to fill every acceptance checkbox.

## Required

- [x] Independent marketplace/order/inventory/payment review.
- [x] Independent identity/session/PGP/TOTP/audit review.
- [x] Independent operations/CI/docs/loop review.
- [x] Browser/UI review and baseline real-PostgreSQL, race and browser tests.
- [x] Cross-examine findings; reproduce and fix confirmed defects with regressions.
- [x] Run applicable final verification and record unavailable live checks.

- [x] Product installation/admin/contact improvements and payment intake controls.
- [x] Separate wallet-backed browser suite exercises partial deposits and top-ups.

## Completion scrutiny

- [x] Separate final review of changes and original scope, with no unresolved
  required findings silently labelled complete.

## Evidence and handoff

Round 1 started 2026-09-23. Three independent read-only reviewers cover distinct
areas; the main session covers UI and test execution. Findings and final evidence
will be recorded in docs/project-review.md. Missing live wallet/Tor deployment
checks remain unverified, never inferred from fake providers or syntax checks.

Final: all required items verified locally. See docs/project-review.md for the
complete findings, fixes, test results and live-chain/Tor boundaries. Separate
final reviewers challenged the finished changes; all accepted findings were fixed.
No deployment or GitHub publication was performed. The user then expanded the
scope to accessibility and actual local-chain testing.

## Follow-up scope — accessibility and seeded local-chain checks

- [x] Fix search focus, readable typography/icons, footer alignment and consistent category selection.
- [x] Verify seeded front-facing workflows and doubled text size across browsers.
- [x] Exercise available isolated real test chains; distinguish simulated RPC, local chain and public-network evidence.
- [x] Fresh scrutiny and record any remaining coverage limits.

## Follow-up scope — local node image input recovery

- [x] Reject incomplete Docker image names/digests before saving configuration or starting services.
- [x] Document recovery for the protected `.env` left by the failed pull. Completed 2026-10-08 (war-room A-13): see `docs/operator-guide.md#recover-after-a-failed-first-install`. Historical note from when it was unticked on 2026-09-24:
  no such document exists. What exists is the installer's refusal message (`scripts/install.sh:14`: edit
  `.env`, then `docker compose up -d --build`) and README's "The installer refuses to overwrite an existing
  configuration"; the installer writes `.env` before `docker compose up` (`scripts/install.sh:173-176`).
- [x] Verify malformed inputs, valid references, all Python tests and shell syntax.

## Follow-up — fresh-install pooled removal (owner 2026-10-10)

- [x] Delete pooled creation, monitoring and payout writers; keep shared escrow helpers and historical readers.
- [x] Add refusing legacy upgrade migration and obsolete-config startup refusal; preserve all history.
- [x] Prove fresh success, undrained unchanged refusal, drained historical preservation and escrow address reuse refusals (native PostgreSQL 16 race checks; CI PostgreSQL 17 still pending).
- [ ] Align intake, installation, network graph and operator upgrade docs; independent war-room review and CI.

No existing market is available: this work proves fresh installation and synthetic legacy upgrades, not a real-market drain. Actual Sparrow and Ledger Flex signing still require separate device evidence.
