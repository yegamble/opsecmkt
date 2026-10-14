# OPSEC Market repo map — binding context for every war-room seat

Read this once, before you form any opinion. Then read `CLAUDE.md`,
`.claude/review-loop.md`, `.ralph/AGENT.md`, `.claude/war-room/finding-format.md`,
`.claude/war-room/protocol.md` and the evidence pack the chair hands you.

## One repository

| Path | Owns |
|---|---|
| `cmd/server` | process entry: config, startup, graceful shutdown |
| `cmd/verify-audit` | offline verifier for signed audit exports (released as binaries) |
| `internal/market` | the whole application: routes/actions, auth, TOTP, CAPTCHA, PGP, orders state machine, inventory, messaging, contacts, payments (per-order escrow: Bitcoin Core watch wallet and escrow signer, monero-wallet-rpc escrow accounts, watchers, settlements, recovery), canary, audit export, migrations |
| `internal/market/migrations` | numbered, append-only SQL migrations applied at startup under an advisory lock (`migrate.go`) |
| `web/templates`, `web/static` | server-rendered HTML pages/partials and CSS — the entire UI |
| `scripts/` | installer, backup/restore, CI smoke, upgrade test, local chains |
| `compose*.yaml`, `Dockerfile`, `deploy/` | Docker Compose deployment (clearnet/Tor, internal/external PostgreSQL, optional nodes, Tor mirror) |
| `tests/e2e` | Playwright: `preview*.spec.ts` (read-only preview), `db-*.spec.ts` (real PostgreSQL), `escrow-*journey.spec.ts` (Bitcoin and Monero escrow against simulated nodes and signer; `playwright.escrow*.config.ts`) |
| `tests/*.py` | installer, PostgreSQL helper and hook regressions |
| `docs/` | `implementation-status.md` (capability claims), `acceptance.md`, `operator-guide.md`, `testnet-runbook.md`, `ci-cd.md`, `operations-tests.md`, `local-chain-testing.md`, `project-review.md` |

## Non-negotiable invariants

1. **No browser JavaScript.** Every workflow is an HTML form + server response.
   A design that needs client script is a BLOCKER, not a preference.
2. **Per-order escrow is the only payment path; one network class per market.** Bitcoin is
   2-of-3 multisig (buyer, vendor, market arbitrator keys; the server can never
   spend alone); Monero is a per-order account, custodial until multisig is
   validated. Escrow is off by default (`ESCROW_BTC`, `ESCROW_XMR`). A market is
   all mainnet (`BITCOIN_CHAIN=main`, `MONERO_NETWORK=mainnet`; A-460) or all test
   networks (Bitcoin regtest/signet/testnet4, Monero stagenet/fakechain); mixed
   classes, Bitcoin testnet3 and Monero testnet are refused (`escrow_runtime.go`,
   `escrow_rpc.go`), and a mainnet market needs a new database. The installer
   writes mainnet by default for the coins enabled and a test network only when
   chosen (owner directive 2026-10-13, A-523); a blank network still means a test
   network, so upgrades never switch a market. No mainnet run is recorded. The pooled wallet is
   removed (owner directive 2026-10-11: no pooled testing or drain): its history
   (`payouts`, `payment_addresses`) is read-only, migration 107 refuses a legacy
   database that still has pooled drain blockers, and migration 108
   (A-492) refuses any pooled history rows and any new ones. Development, tests, CI and war-room evidence
   never use mainnet (`tests/test_ci_never_mainnet.py` guards installer calls); never claim escrow for a path that
   is not implemented and verified.
3. **Orders move only through the server-side transition table**
   (`orders_state.go`) with row locks and compare-and-set; every move is logged
   in `order_events`. Nothing reports success without a recorded state change.
4. **Payments never pay twice.** One settlement per escrow: a release or a
   refund, enforced by the one-outcome partial unique indexes
   `settlements_one_outcome` (076, Bitcoin) and `xmr_settlements_one_outcome`
   (084, Monero), with bumps held to one open and one confirmed
   (`settlement_bumps_one_open`, `settlement_bumps_one_confirmed`, 080). Sends are
   one wallet or node call, never retried automatically; the restore gate
   (`payments_recovery_required`) pauses settlement building, co-signing and
   broadcast. (Historical `payouts` constraints, 064–073, only guard the
   read-only pooled history.)
5. **The server never decrypts messages** and never stores shipping addresses.
6. **`SETUP_TOKEN` derives keys** (CSRF, TOTP/recovery sealing). Rotating it has
   consequences; treat it as key material.
7. **Migrations are append-only and numbered.** A merged migration is immutable.
8. **Signed audit export must stay byte-stable**: existing `audit_events` rows
   are never rewritten.

## Verification gates (quote these; do not invent others)

| Change | Gate |
|---|---|
| Go | `"$(go env GOROOT)/bin/gofmt" -l cmd internal` empty · `go mod verify` · `go vet ./...` · `TEST_DATABASE_URL=<dedicated db> go test -race -count=1 -timeout 30m -run '^(TestEscrow|TestXMR|FuzzEscrow)' ./...` and the same with `-skip` in place of `-run` (CI's two halves; one race process for all of `internal/market` no longer fits 30 minutes on a loaded host) · `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` |
| Templates/CSS/journeys | `npx playwright test` (preview projects) and `E2E_DATABASE_URL=<fresh db> npm run test:e2e` |
| Escrow UI journeys | `E2E_ESCROW_DATABASE_URL=<fresh db> npm run test:e2e:escrow` and `E2E_ESCROW_XMR_DATABASE_URL=<fresh db> npm run test:e2e:escrow-xmr` |
| Shell/installer/ops | `for f in scripts/*.sh; do bash -n "$f"; done` (a single `bash -n a b` checks only `a`), `python3 -m unittest discover -s tests -p 'test_*.py'`, the matching `scripts/test-*.sh` |
| Workflows | `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` |
| Everything | GitHub Actions **CI passed** (required on `main`) |

## Evidence tiers — never promote one to another

unit (no DB) · DB integration (`TEST_DATABASE_URL`) · browser preview · browser
DB · simulated wallet RPC · real local chain (regtest / offline stagenet fork) ·
public test network · deployed instance. Fake-provider or simulated-RPC results
are **not** live-chain evidence. Merged is not released is not deployed.

## Traps that have burned this project

- **`gofmt` is aliased to `gofmt -w .` in the owner's shell.** Always run
  `"$(go env GOROOT)/bin/gofmt" -l cmd internal`; the alias rewrites files.
- **Without `TEST_DATABASE_URL`, ~66% of Go tests silently skip** (coverage
  drops from ~88% to ~35%). A local green run without a database proves little.
- Concurrent Go test processes need **separate databases** (the watcher's
  advisory lock is database-wide).
- `pg_isready` over the Unix socket accepts PostgreSQL's temporary init server;
  always check readiness with `-h 127.0.0.1`.
- Only failures spend the per-account budgets: wrong passwords at sign-in
  (A-153), and wrong passwords or codes in a password confirmation (A-274),
  10 per 10 minutes. A test that probes a refusal with wrong passwords spends
  that account's budget, so give such legs a fresh account.
- Preview mode is read-only and refuses POSTs; a preview spec proves layout,
  not behaviour.
- `scripts/ci-smoke.sh` (CI's container runtime jobs) asserts exact redirects
  and page text against the production image. When a change renames a URL,
  flag or message, grep `scripts/`, `deploy/` and `.github/` as well as
  `internal`, `web`, `tests` and `docs`, and run `ci-smoke.sh internal scanner`
  (A-117 broke it by leaving `&saved=1` in the expected listing redirect).
- Several agents share the host and the wr-pg cluster: kill only processes you
  started, by PID. Never `pkill -f market.test` (or any broad pattern) — it kills
  other agents' test runs (QA22a and QA22b each lost a race run this way).
- A single Playwright failure under load is not a finding until re-run
  unloaded; navigation races (click → re-query before load) have caused flakes.
- The pooled `scripts/regtest-smoke.sh` was deleted with the pooled runtime (A-492). Real-chain Bitcoin escrow legs
  run in CI via `scripts/test-escrow-regtest.sh`; Monero's real-wallet legs run
  on a fakechain in CI too (`escrow-xmr-regtest` job, A-498, via
  `scripts/test-escrow-xmr-regtest.sh`).
- **macOS `/bin/bash` is 3.2**: a failing `[[ ]]` does not stop a `set -e` script, so
  `scripts/test-*.sh` can pass falsely there. Run them with bash 5 (Homebrew) or rely on CI (Ubuntu).
- **govulncheck may need the toolchain pinned locally**: when the tool binary is
  built with an older Go than `go.mod`'s `toolchain` line, it exits 1 with
  "requires newer Go version"; run `GOTOOLCHAIN=go1.27.1 go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...`
  and check the exit status (a piped `tail` hides the failure).
- Migration 073's constraint trigger `payouts_return_needs_release` is deferred to COMMIT: a later migration
  that writes `payouts` rows and then alters or indexes `payouts` in the same startup transaction fails with
  "pending trigger events" unless it runs `SET CONSTRAINTS payouts_return_needs_release IMMEDIATE` first.
- Merging to `main` requires the owner; CI's **CI passed** check is required.
