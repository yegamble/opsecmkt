# Verification

## Evidence table

Updated 2026-09-24. Each row is evidence that can be cited: a GitHub Actions run (its commit and conclusion confirmed with `gh run view <id>`) or a local log. Ticks in [acceptance.md](acceptance.md) point here. Evidence tiers are those of the war room: unit, DB integration, browser preview, browser DB, simulated wallet RPC, real local chain, public test network, deployed instance. A simulated wallet RPC or the in-process fake provider is not chain evidence. Merged is not released and released is not deployed.

Every CI row is the `CI` workflow ([`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml)) with all 15 jobs green, including the required **CI passed** job. Its tiers are: unit and DB integration (`go test -race -count=1 ./...` with `TEST_DATABASE_URL` on PostgreSQL 17); browser preview and browser DB (Playwright `npm run test:e2e`: preview projects in Chromium, Firefox and WebKit, database projects in Chromium); simulated wallet RPC (`npm run test:e2e:wallet`); and operations (encrypted backup/restore, restore through the internal-db Compose service, upgrade from `v0.1.0-alpha.1`, the local installer followed by the setup wizard over HTTP and an app restart (`scripts/test-install.py`), the production image with internal and external PostgreSQL, the Compose configuration matrix and the offline Tor image check). The DB test helper skips only when `TEST_DATABASE_URL` is unset ([`testenv_test.go`](../internal/market/testenv_test.go)), and CI sets it, so the database tests ran. `go test` prints individual test names only on failure, so a named test counts as passed because its package reported `ok` in that run.

| Check | Evidence tier | Commit | CI run or log | Date (UTC) |
|---|---|---|---|---|
| CI, PR #9 (A-13, A-27, A-82) | CI tiers above | `08a9437` | [36000605614](https://github.com/yegamble/opsecmkt/actions/runs/36000605614) | 2026-09-24 |
| CI, push to `main` (merge of PR #8) | CI tiers above | `ab7cd9f` | [35994268519](https://github.com/yegamble/opsecmkt/actions/runs/35994268519) | 2026-09-24 |
| CI, PR #8 (A-9, A-28, A-29) | CI tiers above | `0a1b75d` | [35958151032](https://github.com/yegamble/opsecmkt/actions/runs/35958151032) | 2026-09-24 |
| CI, PR #8 (A-8, A-10, A-11, A-74) | CI tiers above | `30be215` | [35956392431](https://github.com/yegamble/opsecmkt/actions/runs/35956392431) | 2026-09-24 |
| CI, PR #8 (A-53, A-54, A-26, A-71) | CI tiers above | `bf1ddc9` | [35954364843](https://github.com/yegamble/opsecmkt/actions/runs/35954364843) | 2026-09-24 |
| CI, PR #8 (A-66, A-67, A-72) | CI tiers above | `766c87f` | [35952354841](https://github.com/yegamble/opsecmkt/actions/runs/35952354841) | 2026-09-24 |
| CI, push to `main` (merge of PR #7) | CI tiers above | `4c360d2` | [35951223304](https://github.com/yegamble/opsecmkt/actions/runs/35951223304) | 2026-09-24 |
| CI, PR #7 (A-30, A-47, A-48, A-52) | CI tiers above | `cdb69f2` | [35913448670](https://github.com/yegamble/opsecmkt/actions/runs/35913448670) | 2026-09-23 |
| CI, PR #7 (A-23, A-24, A-25) | CI tiers above | `f66ab6b` | [35908986249](https://github.com/yegamble/opsecmkt/actions/runs/35908986249) | 2026-09-23 |
| CI, PR #7 (A-6, A-7, A-21) | CI tiers above | `699ea45` | [35905170138](https://github.com/yegamble/opsecmkt/actions/runs/35905170138) | 2026-09-23 |
| CI, push to `main` (merge of PR #6) | CI tiers above | `509356d` | [35899168645](https://github.com/yegamble/opsecmkt/actions/runs/35899168645) | 2026-09-23 |
| CI, PR #6 (A-1 to A-5) | CI tiers above | `2771d6b` | [35896554121](https://github.com/yegamble/opsecmkt/actions/runs/35896554121) | 2026-09-23 |
| `chain-journey.spec.ts` 1/1 (4 real payouts, none ambiguous) and `scripts/regtest-smoke.sh` PASS; bitcoind 31.1 regtest and monerod 0.18.5.1 offline stagenet fork | real local chain; **UNVERIFIED** here | `cdb69f2` | QA report in `.claude/war-room/ledger.md` (A-13 ruling, A-52 row); a local run, not CI; the log is not kept under `artifacts/real-chain/` in this checkout | 2026-09-23 |
| Same chain journey 1/1 (BTC and XMR) and regtest smoke PASS | real local chain; **UNVERIFIED** here | `699ea45` | QA report in `.claude/war-room/ledger.md` (A-7 row, A-13 ruling); local run, log not retained | 2026-09-23 |
| Same chain journey 1/1 (BTC and XMR) and regtest smoke PASS | real local chain; **UNVERIFIED** here | `509356d` | QA report in `.claude/war-room/ledger.md` (iteration 3 log); local run, log not retained | 2026-09-23 |
| `chain-journey.spec.ts` 1/1 (BTC and XMR: partial deposits, release and moderator refund, 4 real payouts sent, none ambiguous); bitcoind v31.1.0 regtest and monerod/monero-wallet-rpc v0.18.5.1 offline stagenet fork; `TestRegtestSmoke` was skipped in that session | real local chain | `ab7cd9f` (a `git archive` of it plus one unbuilt probe test file; the log itself names no commit) | local logs (gitignored, not CI) with SHA-256 sums: `artifacts/real-chain/ab7cd9f/` (`chain-journey.log`, `SHA256SUMS`); the payout-row summary was read from a database since deleted | 2026-09-24 |
| `TestRegtestSmoke` PASS through `scripts/regtest-smoke.sh`; chain journey BTC 1/1 (18.5 s) and XMR 1/1 (1.7 min) | real local chain | uncommitted tree before `f38dade`; no pinned commit | local logs (gitignored, not CI): `artifacts/real-chain/bitcoin-regtest-smoke.log`, `artifacts/real-chain/btc-browser.log`, `artifacts/real-chain/xmr-browser-final.log`; see [project review](project-review.md#real-isolated-chain-verification) | 2026-09-23, 15:15–15:27 |

Reading the real-chain rows:

- The three QA rows are reported in the war-room ledger only. Their SHA-pinned logs were not found on this machine when this table was written, so they are marked UNVERIFIED and the manual regtest line in acceptance.md stays unticked. The last row has logs, but it predates the payout-timeout change (`12b3e71`) to `payments_rpc.go`, `payments_bitcoin.go` and `payments_monero.go`, so it does not cover the current adapters.
- The Bitcoin and Monero adapters (`payments_bitcoin.go`, `payments_monero.go`, `payments_rpc.go`) are unchanged from `cdb69f2` to `ab7cd9f`. After `cdb69f2`, payout queueing in the watcher and hooks changed (A-54, `c40a224`) and so did the admin payout actions (A-53, `ae6a49a`); both are in `bf1ddc9`. The `ab7cd9f` chain-journey row covers the adapters and the watcher's normal path with that code, but not the specific A-53/A-54 branches (a restore marking failed payouts, an unavailable provider). A rerun with a manifest pinned to the release commit is still needed before a release (war-room A-103).

No row exists, so nothing is claimed, for: a public test network (testnet4, signet, stagenet or testnet), a deployed instance, Tor onion reachability, or a release. The only tag is `v0.1.0-alpha.1`, still a draft GitHub release. Monero behaviour when the daemon is killed mid-send is untested (war-room A-45).

## Onion-mirror commands with the installer's `.env` (war-room A-140) — 2026-09-24

Local Compose configuration check, not a running stack: Docker 29.8.0, Compose 5.5.1, a scratch `.env` in the
installer's format for Tor with the internal database (`COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml'`).
With `COMPOSE_PROFILES='internal-db'`, the old guide command failed both ways:
`docker compose --profile mirror config --quiet` and `docker compose --profile mirror up -d --dry-run tor-mirror`
printed `service "app" depends on undefined service "db": invalid compose project`. With
`COMPOSE_PROFILES='internal-db,mirror'`, `docker compose --dry-run up -d` planned `db`, `app`, `tor` and
`tor-mirror` and `config --services` listed all four. Dry runs were not reliable enough for CI: 20 repeats of
`docker compose --dry-run up -d` gave 1 or 2 spurious failures (`app is missing dependency db`), and an
earlier dry-run version of the check failed 3 of 8 runs (that error, or a plan missing the `tor-mirror` lines).
`scripts/test-mirror-commands.sh` therefore resolves commands with `config --services`; it passed for all
four installer configurations in 10 of 10 runs, and failed with the message above when the guide's `up -d`
line was temporarily replaced by the old command. CI runs it in the Tor rows with the mirror on; no CI run of
it exists yet.

## Tor restarts with the app (war-room A-134) — 2026-09-24

Local Compose check with stand-in containers, not onion traffic: Docker 29.8.0, Compose 5.5.1, project `a134churn`, a scratch copy of `compose.yaml` and `compose.tor.yaml` with an override that replaces the app and Tor images by `alpine:3.20` running `sleep` (nothing built or pulled). Sequence: `up -d app tor`; then `up -d` with a changed app setting and one added service on the backend network (standing in for UPGRADING's node profiles); then `docker compose restart app`; then `down -v`.

| `compose.tor.yaml` | App address after the change | Tor `StartedAt` after the app was recreated | After `restart app` |
|---|---|---|---|
| before (`depends_on: [app]`) | moved 172.19.0.2 → 172.19.0.4; the new service took .2 | unchanged (14:24:34.458) | unchanged |
| after (`restart: true`) | unchanged in that run (172.18.0.2) | Compose logged `tor Stopping` → `app Recreated` → `tor Started`; 14:28:36.556 → 14:29:42.146 | restarted |

With the mirror profile, `tor` and `tor-mirror` both restarted when the app was recreated; a mirror started with `--profile mirror up -d tor-mirror` was **not** restarted by a later plain `up -d` without the profile (hence the documents' advice to put `mirror` in `COMPOSE_PROFILES`). Limits found in the same session: pulling the new `compose.tor.yaml` alone does not restart a running Tor (only an app recreate or restart does, which then also restarts the pre-existing Tor container); `docker compose stop app` followed by `up -d` does not restart Tor; and `docker compose up --dry-run --force-recreate tor tor-mirror` on a project without the mirror profile creates and starts `tor-mirror`, which is why the documents name `tor-mirror` only for installs that run it. CI checks the setting in every Tor row of the Compose job; the runtime behaviour above is a local run, not CI. Onion reachability after churn is still not verified here.

## Review fixes — 2026-09-23

A fresh-eyes review of the merged packages found eight defects. Each was reproduced by a failing test first (`internal/market/review_fixes_integration_test.go`), then fixed:

1. Stale draft price: resubmitting checkout now updates the draft's amount, and requesting payment reprices the draft from the listing in the same transaction (`TestDraftRepricedOnResubmitAndPay`).
2. Rate-limiter lockout: a full limiter table evicts the entry that expires first; `/challenge` and `/challenge/pgp` look up the pending login, and sign-in validates the handle, before creating a limiter key (`TestLimiterFloodDoesNotLockOutUsers`). Superseded in part by war-room A-11: eviction now takes the lowest-count non-blocking entry, never a blocking one, and sign-in counts only requests that passed the CAPTCHA (see `docs/implementation-status.md`).
3. Unbounded unpaid reservations: at most 3 orders awaiting payment per buyer (409), and the watcher cancels orders unpaid after `PAYMENT_EXPIRY` (default 24h) with no deposit seen, returning their stock (`TestPayCapsOpenAwaitingPaymentOrders`, `TestWatcherExpiresUnpaidOrders`).
4. Session-only takeover: payout-address changes, turning PGP sign-in off, and changing or removing the key while it is on need the current password, plus an authenticator code when TOTP is enrolled (`TestSensitiveChangesNeedPassword`).
5. Late deposits after close: closed orders stay watched for 30 days after address issue while a deposit is confirming or unhandled; funds confirming after a cancellation are refunded, extra funds on settled orders flagged (`TestLateDepositAfterCloseIsHandled`).
6. Reorg to 0 confirmations: payouts are held while any credited deposit is below the threshold and resume when it re-confirms (`TestPayoutHeldWhileCreditedDepositReconfirms`).
7. Monero locked transfers: `unlock_time` ≠ 0 is recorded as locked, never credited, reported once (`TestMoneroLockedTransfersAreNotCredited`; migration 051).
8. Legacy PGP key: an unchanged stored key is not re-parsed on profile save (`TestLegacyUnparseableKeySavesUnchanged`).

Results after the fixes: `gofmt -l` clean; `go vet ./...` clean; `go test -count=1 ./...` passes on a fresh database (the new tests plus the lifecycle and payment tests also pass with `-race`); Playwright 263 of 264 (only the known WebKit mobile skip-link failure); Python tests 18 pass; `govulncheck` v1.1.4 reports no called vulnerabilities.

## Feature completion — 2026-09-23

Historical record, true when written. Where a later run changed a statement, a dated note says so; current evidence is in the [evidence table](#evidence-table).

Scope: the Foundation refactor and packages P1–P6 (authentication, PGP, orders, inventory, test-network payments, transparency) merged on `finish-codebase`, plus the cross-package lifecycle test. Results are from a local macOS machine with PostgreSQL 16 unless stated.

### Passed

- `gofmt -l cmd internal` reports nothing; `go vet ./...` is clean.
- `go test -count=1 ./...` with `TEST_DATABASE_URL` (isolated schema per test): all packages pass, including every package's integration tests and `TestLifecycleDigitalAndDisputedPhysical`. That test drives the HTTP handlers with the in-process fake wallet: a vendor lists a digital item with delivery content and saves a payout address; the buyer drafts and requests payment (TESTNET-labelled address shown, stock reserved); a deposit below the confirmation threshold leaves the order awaiting payment; at the threshold one watcher pass marks it paid and auto-delivers as the system actor (content visible to the buyer, 404 for third parties and moderators); completion sends exactly one release payout to the vendor's address across repeated passes; one review is accepted and a second returns 409. A physical order is then paid, disputed, resolved as a refund by a moderator, and exactly one refund is sent to the buyer's address.
- Race detector: focused tests (including the lifecycle test) pass with `-race`. The full database suite was run **without** `-race` locally: real bcrypt cost-12 hashing takes about 29 s under the race detector on this machine, beyond the 12 s request timeout, so setup/login tests return 503 there. CI runs the full suite with `-race`.
- Playwright: 263 of 264 passed — every `preview*.spec.ts` in the six Chromium/Firefox/WebKit viewport projects, and `db-setup` plus all `db-*.spec.ts` against a fresh database. The one failure is the WebKit mobile skip-link focus check in `preview.spec.ts`, which also fails on the pre-package commit on this Mac.
- Compose configuration matrix: 6 of 6 (clearnet/Tor × internal/external database × mirror on/off), with node and `monero-wallet` profiles parsed.
- `govulncheck` v1.1.4: no vulnerabilities in imported packages; one module-level advisory in code the application does not call.
- Python regression tests: 18 pass.

### Not verified

- Live Docker image builds, container startup and Tor/onion reachability were not exercised in this effort. *Superseded in part, 2026-09-24:* every CI row in the evidence table builds the production image and starts it with internal and external PostgreSQL. Tor/onion reachability is still unverified.
- Live Bitcoin regtest/testnet run: the Bitcoin Core adapter is tested only against local HTTP fakes. `scripts/regtest-smoke.sh` is provided for the operator to run against `bitcoind -regtest`; it was not run here. *Superseded, 2026-09-24:* the smoke test and real local-chain browser journeys were run later on 2026-09-23; see the real-chain rows of the evidence table. No public testnet run is recorded.
- Monero stagenet live run: the monero-wallet-rpc adapter is tested only against local HTTP fakes. *Superseded in part, 2026-09-24:* real local-chain journeys on an offline stagenet fork are in the evidence table. The public stagenet has not been used.
- Mainnet payments: unsupported by design. Startup refusal of mainnet chains and addresses is tested against fakes.
- The GitHub Actions run for this branch (including the full `-race` suite on PostgreSQL 17) is not recorded here. *Superseded, 2026-09-24:* later CI runs, with commits, are in the evidence table.
- Browser checks are not a complete WCAG AAA audit.

## Earlier baseline (before the feature packages)

Historical record, superseded by the [evidence table](#evidence-table) where they differ (CI now runs PostgreSQL 17 integration and starts the production image).

### Passed


- Go 1.26.8: `go test -race ./...` against an isolated local PostgreSQL 16.15 database. Tests cover exact atomic amounts, malformed/overflow prices, roles, CSRF, cookie flags, request bounds, setup lockout, registration, listing ownership, idempotent order drafts, private messages/orders, HTML escaping, session revocation, and restart persistence.
- `go vet ./...` and `go mod verify`.
- `govulncheck`: zero reachable vulnerabilities and zero advisories in imported packages. One module-wide advisory concerns the unused, deprecated `golang.org/x/crypto/openpgp` package; this application imports bcrypt, not that package.
- The initial machine toolchain (Go 1.26.2) reported standard-library vulnerabilities. Project minimum and Docker build moved to 1.26.8, then tests and scanning passed. [Go release history](https://go.dev/doc/devel/release).
- All 17 UI routes inspected at a 320px browser viewport: no page-level horizontal overflow. Catalog also inspected at 1280px; desktop filters render. Mobile search filters sample results correctly. Skip-link Tab/Enter behavior reaches main content.
- Original `[OPSECMKT]` wordmark verified in mobile browser. No application JavaScript or remote browser assets are shipped.
- Internal/external database and Tor/clearnet Compose configuration parsing, installer choice simulations, shell syntax, and optional mirror profile validation.
- Actual age-encrypted PostgreSQL backup/restore roundtrip, including Unicode data, 0600 output, overwrite refusal, wrong-key rejection, and transaction rollback on conflicting restore. Temporary backup databases and key material removed.

### Not verified or not implemented

- Live Docker image builds, container startup, live onion reachability, blockchain synchronization, external providers, and real host-failure recovery were not exercised.
- No payment/escrow or cryptocurrency funds were handled. See implementation-status.md for the unavailable application capabilities.
- Browser checks are not a complete WCAG AAA audit. Screen-reader interoperability, every color pairing, and exhaustive keyboard interaction remain release checks.
- PostgreSQL 17 integration runs are configured in CI but were not executed on GitHub during this session.

### CI/CD follow-up

The repository is on GitHub at https://github.com/yegamble/opsecmkt. Automated pipelines replace the earlier manual-only checks. PostgreSQL 17 integration, both production-container database modes, deployment matrices, and encrypted restore have passed on GitHub runners. The Tor image builds and validates its configuration locally without networking. Seventeen Python regression tests pass locally.

The JavaScript-disabled mobile browser journey exposed a real form-submission issue: `Referrer-Policy: no-referrer` caused Chromium to submit `Origin: null`. The header now uses `same-origin`; cross-origin checks and session-bound CSRF validation remain enforced. The complete setup/listing/buyer/order/profile/login journey passed after the fix.

Playwright contains 126 preview cases across Chromium/Firefox/WebKit plus one database-backed mobile journey. CI uploads failure traces/screenshots and HTML reports. The release workflow reruns the complete suite before creating private draft OCI image artifacts, checksums and provenance/SBOM metadata. It never deploys to a live host.
