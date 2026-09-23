# Verification

## Review fixes — 2026-09-23

A fresh-eyes review of the merged packages found eight defects. Each was reproduced by a failing test first (`internal/market/review_fixes_integration_test.go`), then fixed:

1. Stale draft price: resubmitting checkout now updates the draft's amount, and requesting payment reprices the draft from the listing in the same transaction (`TestDraftRepricedOnResubmitAndPay`).
2. Rate-limiter lockout: a full limiter table evicts the entry that expires first; `/challenge` and `/challenge/pgp` look up the pending login, and sign-in validates the handle, before creating a limiter key (`TestLimiterFloodDoesNotLockOutUsers`).
3. Unbounded unpaid reservations: at most 3 orders awaiting payment per buyer (409), and the watcher cancels orders unpaid after `PAYMENT_EXPIRY` (default 24h) with no deposit seen, returning their stock (`TestPayCapsOpenAwaitingPaymentOrders`, `TestWatcherExpiresUnpaidOrders`).
4. Session-only takeover: payout-address changes, turning PGP sign-in off, and changing or removing the key while it is on need the current password, plus an authenticator code when TOTP is enrolled (`TestSensitiveChangesNeedPassword`).
5. Late deposits after close: closed orders stay watched for 30 days after address issue while a deposit is confirming or unhandled; funds confirming after a cancellation are refunded, extra funds on settled orders flagged (`TestLateDepositAfterCloseIsHandled`).
6. Reorg to 0 confirmations: payouts are held while any credited deposit is below the threshold and resume when it re-confirms (`TestPayoutHeldWhileCreditedDepositReconfirms`).
7. Monero locked transfers: `unlock_time` ≠ 0 is recorded as locked, never credited, reported once (`TestMoneroLockedTransfersAreNotCredited`; migration 051).
8. Legacy PGP key: an unchanged stored key is not re-parsed on profile save (`TestLegacyUnparseableKeySavesUnchanged`).

Results after the fixes: `gofmt -l` clean; `go vet ./...` clean; `go test -count=1 ./...` passes on a fresh database (the new tests plus the lifecycle and payment tests also pass with `-race`); Playwright 263 of 264 (only the known WebKit mobile skip-link failure); Python tests 18 pass; `govulncheck` v1.1.4 reports no called vulnerabilities.

## Feature completion — 2026-09-23

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

- Live Docker image builds, container startup and Tor/onion reachability were not exercised in this effort.
- Live Bitcoin regtest/testnet run: the Bitcoin Core adapter is tested only against local HTTP fakes. `scripts/regtest-smoke.sh` is provided for the operator to run against `bitcoind -regtest`; it was not run here.
- Monero stagenet live run: the monero-wallet-rpc adapter is tested only against local HTTP fakes.
- Mainnet payments: unsupported by design. Startup refusal of mainnet chains and addresses is tested against fakes.
- The GitHub Actions run for this branch (including the full `-race` suite on PostgreSQL 17) is not recorded here.
- Browser checks are not a complete WCAG AAA audit.

## Earlier baseline (before the feature packages)

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

The repository is now private on GitHub at https://github.com/yegamble/opsecmkt. Automated pipelines replace the earlier manual-only checks. PostgreSQL 17 integration, both production-container database modes, deployment matrices, and encrypted restore have passed on GitHub runners. The Tor image builds and validates its configuration locally without networking. Seventeen Python regression tests pass locally.

The JavaScript-disabled mobile browser journey exposed a real form-submission issue: `Referrer-Policy: no-referrer` caused Chromium to submit `Origin: null`. The header now uses `same-origin`; cross-origin checks and session-bound CSRF validation remain enforced. The complete setup/listing/buyer/order/profile/login journey passed after the fix.

Playwright contains 126 preview cases across Chromium/Firefox/WebKit plus one database-backed mobile journey. CI uploads failure traces/screenshots and HTML reports. The release workflow reruns the complete suite before creating private draft OCI image artifacts, checksums and provenance/SBOM metadata. It never deploys to a live host.
