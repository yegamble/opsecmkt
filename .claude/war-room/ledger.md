# War-room ledger

Source of truth for the war-room backlog across loop iterations. The chair
updates it after every Round D ruling and every verified fix. One row per item;
never delete a row — move it to Closed or Declined with the evidence.

Status: `open` · `in-progress (<branch>)` · `fixed (<branch/PR>)` ·
`verified (<PR>, CI run)` · `blocked (<what is needed>)`.
Evidence tier as in `repo-map.md`.

## Iteration log

| # | Date | Scope | Seats | Outcome |
|---|---|---|---|---|
| 0 | 2026-09-23 | Whole-codebase audit (pre-war-room: 5 area auditors + Fable ranking) | identity, commerce, payments, tests, ops auditors | Seeded this ledger; 5 fix-now items dispatched |

## P0 — release blockers

| ID | Item | Area | Evidence | Status |
|---|---|---|---|---|
| A-1 | Payout wallet calls clamped to 10 s (`payments_rpc.go:37,64-74`) under the 30 s send bound; ambiguous send recorded failed, admin requeue can double-pay | payments | code read, verified | in-progress (fix/payout-timeout) |
| A-2 | No working restore/rollback for the default internal-db deployment; `UPGRADING.md` rollback impossible with `build: .` | ops | code read, verified | in-progress (fix/ops-restore-setup-token) |
| A-3 | `.env.example` placeholder `SETUP_TOKEN` passes the length-only check (`app.go:78`); token also derives CSRF and TOTP sealing keys | ops/identity | code read, verified | in-progress (fix/ops-restore-setup-token) |

## P1 — required for a coherent product

| ID | Item | Area | Evidence | Status |
|---|---|---|---|---|
| A-4 | Role-change audit row lacks target/role (`actions_admin.go:32`); admin audit view lacks actor (`load.go:206`) | identity | code read, verified | in-progress (fix/identity-audit-credentials) |
| A-5 | No password change; no admin reset of a user's second factors (lost TOTP + spent codes = SQL-only) | identity | code read, verified | in-progress (fix/identity-audit-credentials) |
| A-6 | Moderator desk mixes open+resolved disputes `ORDER BY created DESC LIMIT 100` (`load.go:163`); old open disputes vanish; same cap feeds NeedsAction on orders/vendor desks | commerce | code read, verified | open |
| A-7 | Watcher re-polls funded non-terminal orders only if updated in 24 h (`payments_watcher.go:147-151`); extra/duplicate deposits on paid/shipped orders are silently included in the vendor release (`payments_hooks.go:71-73`) | payments | auditor + Fable read | open |
| A-8 | Moderators/admins not notified when a dispute opens; a dispute can have no eligible resolver (sole admin is the vendor) yet is accepted | commerce | auditor read | open |
| A-9 | One 15 s startup context covers ping, advisory lock, all migrations and payment init (`app.go:95-115`); a slow migration crash-loops | ops | auditor read | open |
| A-10 | Sealed plaintext recovery codes linger in `users.recovery_reveal` unless `/totp` is viewed within 10 min; docs claim only hashes are stored | identity | auditor + Fable read | open |
| A-11 | Rate-limit entry created before CAPTCHA check + evict-oldest lets junk requests reset victims' counters (`actions_auth.go:128-131`, `app.go:148`) | identity | auditor read | open |
| A-12 | Test infra: no fail-on-skip mode for DB tests; shared Playwright admin near its sign-in budget; TOTP 30 s boundary unguarded (`db-auth.spec.ts:61,82`) | tests | auditor read | open |
| A-13 | Docs drift: acceptance.md ops/P5 unticked despite evidence; implementation-status says regtest smoke never ran; `.ralph/fix_plan.md` ticks a non-existent `.env` recovery doc; PREVIEW_ADDR undocumented; APP_MODE is a no-op; Caddy example hardcodes :8080 | ops | auditor read | open |

## P2 — high-value improvements

| ID | Item | Area | Status |
|---|---|---|---|
| A-14 | No auto-completion / abandonment timers for shipped/delivered/paid orders (documented limitation; needs policy) | commerce | open |
| A-15 | A never-confirming 0-conf deposit blocks expiry forever and keeps stock reserved | payments | open |
| A-16 | 100/50-row caps without paging: admin user picker, payouts panel, messages, notifications, catalog | commerce/ui | open |
| A-17 | Real-chain reorg/conflict/double-spend/locked-transfer/restart never tested; chain suite + regtest smoke not in CI | tests | open |
| A-18 | Browser coverage gaps: successful PGP proof and PGP sign-in; Firefox/WebKit only on preview pages | tests | open |

## P3 — experiments

| ID | Item | Status |
|---|---|---|
| A-19 | Canary freshness indicator | open |
| A-20 | Legacy `service` listings still orderable; editing silently converts to physical | open |

## Declined

| ID | Item | Why |
|---|---|---|
| D-1 | Admin "mark sent" being the only exit for some held payouts | conservative by design (Fable ruling) |
| D-2 | Tor onion reachability as a roadmap item | environment evidence, not code; track under ops verification |

## Closed

| ID | Item | Evidence |
|---|---|---|
| C-1 | CI red on main: `pg_isready` Unix-socket init race | PR #5, main CI run 35891255332 green |
| C-2 | Go toolchain drift (tests 1.26.8, ships 1.27.1); govulncheck 1.1.4 panics on Go 1.27 | PR #5 |
| C-3 | Release workflow required a private repo; action pin drift; duplicate CI runs | PR #5 |
