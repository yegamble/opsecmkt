# OPSEC Market security audit and fixes

Audit date: October 14, 2026. Baseline: `312dd8ede5d5ce5873a8da9047c74407b8a42269`.

The supplied `tor-opsec-infosec-critic` skill guided a source and configuration review of Tor isolation,
authentication, uploads, payment trust boundaries, and operational records. Three issues were confirmed and fixed.
The strongest evidence is local database integration and simulated RPC; it does not certify a deployed onion service.

**The harsh verdict.** The default Tor configuration has substantial protections, but authentication and payment
privacy had gaps that the deployment perimeter could not fix. No critical origin-IP leak or browser third-party asset
load was confirmed in the default deployment. The supplied scanner originally failed with a Python quote-escaping
syntax error; a temporary corrected copy reported 459 matches on the tracked baseline. Those included documentation,
dependency URLs, test fixtures, internal RPC URLs, and the unused design reference. Its severity labels were not
accepted as vulnerability evidence. The repository uses native Tor; the skill's GoBalance claims are not applicable
to the reviewed architecture.

1. **SEC-01 — High: concurrent factor enrollment allowed a password-only session.**
   In `internal/market/actions_auth.go`, `secondFactor` read enrolled factors outside the session transaction.
   `startSession` locked the user row and checked suspension and the password hash, but did not check current factor
   enrollment. A real TOTP activation or PGP sign-in enable could commit between those reads and session creation.
   The regression reproduced a new authenticated cookie and session after enrollment, without verifying that factor.
   The exposure requires a valid password and a concurrent enrollment window; it does not bypass a factor already
   observed by the initial login read.

2. **SEC-02 — Medium: external payment-address checks exposed other accounts' history.**
   Bitcoin and Monero checkout and claim validation queried global refund and settlement destinations. An unrelated
   account could submit a known external address to its own eligible order and distinguish previous market use from
   a fresh address, without funding the order. Global uniqueness also let that account reserve an address before its
   owner used it. The tests confirm unrelated orders remain unreadable while the former address-validation response
   exposed the history. This reveals market membership for a known address; it does not enumerate arbitrary wallets
   or establish the owner's real-world identity by itself.

3. **SEC-03 — Medium, conditional: the signer could send private-key RPCs through proxies or redirects.**
   `cmd/escrow-signer/core.go:43` now defines the transport policy. Previously, the default HTTP transport honored
   ambient proxy variables, and the client followed redirects. Startup and signing RPC descriptors contain the
   signer's account extended private key. A proxy receiving a plaintext RPC or a 307/308 redirect could receive the
   body outside the configured node. Local regressions used synthetic key text and reproduced both behaviors. The
   default Compose setup does not supply those proxy variables; this is a conditional signer transport exposure,
   not a demonstrated unauthenticated web attack on the default deployment.

**The threat chain.** For SEC-01, a password check observes no factor, enrollment commits, then the former session
path accepts that stale decision. For SEC-02, a signed-in account submits an external address on its own order; the
former cross-account query and unique indexes produce a different result depending on private history. For SEC-03,
a key-bearing RPC uses the default routing or redirect behavior, which can deliver it beyond the configured node.
None of these demonstrations used another person's deployment, real credentials, or real funds.

**The architect's blueprint.** The changes close those specific boundaries:

- `internal/market/actions_auth.go:1028` rechecks `totp_enabled` and the raw `pgp_2fa` flag while retaining the existing
  user-row share lock through session creation. A stale password-only attempt is refused before its old session is
  deleted, a cookie is issued, or a successful-login audit row is written. A fresh login uses the enrolled factor.
- `internal/market/escrow_address.go:9` and the Monero address checks separate global infrastructure restrictions from
  the recipient account's external history. Migration
  `internal/market/migrations/111_escrow_external_addresses.sql:5` backfills account-and-currency reservations and
  enforces them with transactional triggers. Same-account reuse remains forbidden across buyer and vendor roles;
  different accounts cannot reserve or query each other's external history through checkout or claim. Market escrow,
  sibling, wallet, fee, and same-order buyer-refund restrictions remain, as do settlement outcome and deposit guards.
- `cmd/escrow-signer/core.go:43` clears environment proxy handling and refuses every HTTP redirect. Configured-node
  authentication, response limits, fixed error classes, and the two-minute timeout remain. Operators must configure a
  directly reachable RPC URL; proxy-dependent or redirect-dependent signer setups now fail closed.

The new regressions are `cmd/escrow-signer/core_test.go`, `internal/market/auth_factor_race_test.go`, and
`internal/market/escrow_address_privacy_test.go`. They exercise actual client routing, real factor-enrollment actions,
and authenticated Bitcoin/Monero checkout and claim routes. Additional migration checks cover reservations, upgrade
backfill, and rollback of refused transactions. All three principal regressions failed on the old behavior and passed
after their fixes. Local command logs are retained in the ignored `artifacts/security-audit/` directory.

The reviewed controls also include restrictive no-JavaScript CSP and local browser assets; bounded request admission;
single-use session-bound raster CAPTCHAs; image scanning followed by JPEG re-encoding without uploaded metadata;
an internal Docker backend network with no published app port in the Tor overlay; Tor v3, proof of work, introduction
rate limits, and no public SOCKS/control port; and exclusions for plaintext onion keys plus encrypted backups.
Wildcard listening inside an unpublished isolated container was not treated as a host-exposure finding.

Operational limits remain: host access controls, firewall behavior, provider visibility, operator identity separation,
Tor Browser Safest journeys, live onion availability and traffic correlation need deployment evidence. Optional
external-egress and direct blockchain-node networking are documented choices, not evidence of an unnoticed browser
leak. Monero escrow remains custodial, and Bitcoin Core receives signer account key material by the documented design.
No mainnet run, live-chain proof, or production deployment is claimed.

Migration and compatibility steps are recorded in `UPGRADING.md`, section 65. An older binary refuses a database
recording migration 111; restoring the pre-upgrade backup is required for rollback.

Validation on Go 1.27.2 and a dedicated PostgreSQL 16.15 cluster:

- The complete general race suite passed (`go test -race -count=1 -timeout 30m -skip '^(TestEscrow|TestXMR|FuzzEscrow)' ./...`), with 1,443 passing cases including subtests. Four cases were skipped: the helper subprocess entry point, optional load test, real ClamAV test, and environment-dependent database-error test.
- The complete final escrow race suite passed (`go test -race -count=1 -timeout 30m -run '^(TestEscrow|TestXMR|FuzzEscrow)' ./...`), with 819 passing cases including subtests. Fifty real-chain or manual evidence cases were skipped because their runtime/configuration was absent. The earlier run exposed one obsolete global-uniqueness assertion; it was corrected to test same-account refusal and cross-account acceptance, and both its targeted race rerun and the complete final suite passed.
- All 439 Python regression tests passed; the final documentation-test-name check passed 12 cases.
- All 10 Bitcoin and 8 Monero browser escrow journeys passed with JavaScript disabled, using real PostgreSQL and simulated RPC. All 60 privacy-disclosure browser cases passed across the configured browsers and viewports.
- `go mod verify`, `go vet ./...`, formatting, and `git diff --check` passed. The pinned `govulncheck` reported no vulnerabilities reachable from this code; one vulnerability in a required module was outside imported/called code.

Docker was unavailable. Real Bitcoin/Monero chain legs, real ClamAV, deployed onion checks, and the documented PostgreSQL 17+ target were not exercised here. These results are local regression evidence, not live-chain or deployment evidence.

The signer redirect behavior follows the [Go HTTP client documentation](https://pkg.go.dev/net/http#Client.Do).
Tor protection settings are described in the [Tor Project onion-service DoS guidance](https://community.torproject.org/onion-services/advanced/dos/).
