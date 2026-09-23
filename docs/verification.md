# Verification — 2026-09-23

## Passed

- Go 1.26.8: `go test -race ./...` against an isolated local PostgreSQL 16.15 database. Tests cover exact atomic amounts, malformed/overflow prices, roles, CSRF, cookie flags, request bounds, setup lockout, registration, listing ownership, idempotent order drafts, private messages/orders, HTML escaping, session revocation, and restart persistence.
- `go vet ./...` and `go mod verify`.
- `govulncheck`: zero reachable vulnerabilities and zero advisories in imported packages. One module-wide advisory concerns the unused, deprecated `golang.org/x/crypto/openpgp` package; this application imports bcrypt, not that package.
- The initial machine toolchain (Go 1.26.2) reported standard-library vulnerabilities. Project minimum and Docker build moved to 1.26.8, then tests and scanning passed. [Go release history](https://go.dev/doc/devel/release).
- All 17 UI routes inspected at a 320px browser viewport: no page-level horizontal overflow. Catalog also inspected at 1280px; desktop filters render. Mobile search filters sample results correctly. Skip-link Tab/Enter behavior reaches main content.
- Original `[OPSECMKT]` wordmark verified in mobile browser. No application JavaScript or remote browser assets are shipped.
- Internal/external database and Tor/clearnet Compose configuration parsing, installer choice simulations, shell syntax, and optional mirror profile validation.
- Actual age-encrypted PostgreSQL backup/restore roundtrip, including Unicode data, 0600 output, overwrite refusal, wrong-key rejection, and transaction rollback on conflicting restore. Temporary backup databases and key material removed.

## Not verified or not implemented

- Live Docker image builds, container startup, live onion reachability, blockchain synchronization, external providers, and real host-failure recovery were not exercised.
- No payment/escrow or cryptocurrency funds were handled. See implementation-status.md for the unavailable application capabilities.
- Browser checks are not a complete WCAG AAA audit. Screen-reader interoperability, every color pairing, and exhaustive keyboard interaction remain release checks.
- PostgreSQL 17 integration runs are configured in CI but were not executed on GitHub during this session.
