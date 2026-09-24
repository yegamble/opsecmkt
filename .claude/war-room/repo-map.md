# OPSEC Market repo map — binding context for every war-room seat

Read this once, before you form any opinion. Then read `CLAUDE.md`,
`.claude/review-loop.md`, `.ralph/AGENT.md`, `.claude/war-room/finding-format.md`,
`.claude/war-room/protocol.md` and the evidence pack the chair hands you.

## One repository

| Path | Owns |
|---|---|
| `cmd/server` | process entry: config, startup, graceful shutdown |
| `cmd/verify-audit` | offline verifier for signed audit exports (released as binaries) |
| `internal/market` | the whole application: routes/actions, auth, TOTP, CAPTCHA, PGP, orders state machine, inventory, messaging, contacts, payments (Bitcoin Core / monero-wallet-rpc adapters, watcher, payouts), canary, audit export, migrations |
| `internal/market/migrations` | numbered, append-only SQL migrations applied at startup under an advisory lock (`migrate.go`) |
| `web/templates`, `web/static` | server-rendered HTML pages/partials and CSS — the entire UI |
| `scripts/` | installer, backup/restore, CI smoke, upgrade test, local chains |
| `compose*.yaml`, `Dockerfile`, `deploy/` | Docker Compose deployment (clearnet/Tor, internal/external PostgreSQL, optional nodes, Tor mirror) |
| `tests/e2e` | Playwright: `preview*.spec.ts` (read-only preview), `db-*.spec.ts` (real PostgreSQL), `wallet-journey` (simulated wallet RPC), `chain-journey` (real local chains, manual) |
| `tests/*.py` | installer, PostgreSQL helper and hook regressions |
| `docs/` | `implementation-status.md` (capability claims), `acceptance.md`, `operator-guide.md`, `testnet-runbook.md`, `ci-cd.md`, `operations-tests.md`, `local-chain-testing.md`, `project-review.md` |

## Non-negotiable invariants

1. **No browser JavaScript.** Every workflow is an HTML form + server response.
   A design that needs client script is a BLOCKER, not a preference.
2. **Test networks only.** Bitcoin testnet3/testnet4/signet/regtest and Monero
   stagenet/testnet. Mainnet is refused at startup and at runtime. Never propose
   mainnet support, custody of real funds or multisig-escrow claims.
3. **Orders move only through the server-side transition table**
   (`orders_state.go`) with row locks and compare-and-set; every move is logged
   in `order_events`. Nothing reports success without a recorded state change.
4. **Payments never pay twice.** `payouts.order_id` is unique, sends are a
   single wallet call claimed by pending→sending compare-and-set, never retried
   automatically; the restore gate (`payments_recovery_required`) pauses payouts.
5. **The server never decrypts messages** and never stores shipping addresses.
6. **`SETUP_TOKEN` derives keys** (CSRF, TOTP/recovery sealing). Rotating it has
   consequences; treat it as key material.
7. **Migrations are append-only and numbered.** A merged migration is immutable.
8. **Signed audit export must stay byte-stable**: existing `audit_events` rows
   are never rewritten.

## Verification gates (quote these; do not invent others)

| Change | Gate |
|---|---|
| Go | `"$(go env GOROOT)/bin/gofmt" -l cmd internal` empty · `go mod verify` · `go vet ./...` · `TEST_DATABASE_URL=<dedicated db> go test -race -count=1 ./...` · `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` |
| Templates/CSS/journeys | `npx playwright test` (preview projects) and `E2E_DATABASE_URL=<fresh db> npm run test:e2e` |
| Payments UI journeys | `E2E_WALLET_DATABASE_URL=<fresh db> npm run test:e2e:wallet` |
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
- The Playwright DB specs share one admin that already uses 9 of the 10
  sign-ins allowed per handle per 10 minutes — new specs must use fresh accounts.
- Preview mode is read-only and refuses POSTs; a preview spec proves layout,
  not behaviour.
- A single Playwright failure under load is not a finding until re-run
  unloaded; navigation races (click → re-query before load) have caused flakes.
- `chain-journey` and `regtest-smoke.sh` never run in CI; their evidence is
  local logs under `artifacts/real-chain/` (gitignored).
- **macOS `/bin/bash` is 3.2**: a failing `[[ ]]` does not stop a `set -e` script, so
  `scripts/test-*.sh` can pass falsely there. Run them with bash 5 (Homebrew) or rely on CI (Ubuntu).
- Merging to `main` requires the owner; CI's **CI passed** check is required.
