# Acceptance checklist and capability boundaries

This is a verification plan, not a declaration that every check has passed. Record actual results separately. The application uses Go server-rendered HTML and CSS with PostgreSQL; React is not required.

## Installation and deployment

- [ ] A fresh install with no external database starts the PostgreSQL Compose service, waits for readiness, and applies migrations.
- [ ] External database configuration starts no PostgreSQL container; connection failures stop setup with a useful, secret-free error.
- [ ] The setup wizard requires the operator's installation secret, creates one administrator atomically, and rejects further setup attempts after completion, including concurrent requests and after restart.
- [ ] Internal database ports and blockchain RPC ports are not published publicly. The app cannot access the Docker socket.
- [ ] Clearnet deployment documents its HTTPS termination boundary and corresponding secure-cookie configuration.
- [ ] Tor-only deployment publishes no application host port and loads no third-party fonts, scripts, analytics, or images. Confirm the effective Compose configuration, not only individual YAML files.
- [ ] Mirror documentation distinguishes a shared database deployment from independent instances; it does not imply replicated balances or safe simultaneous spending.
- [ ] Persistent data survives container replacement. Backup and restore instructions cover database records, deployment secrets, and any onion identity keys; a restore rehearsal is recorded.

## Authentication and request security

- [ ] Passwords use adaptive hashing; password and username bounds are enforced before expensive work. No production default credentials exist.
- [ ] Sessions use cryptographic randomness, expire server-side, rotate on authentication, and are revoked on logout. Cookies are HttpOnly, have an explicit SameSite policy, and use Secure when the configured browser-facing origin is HTTPS.
- [ ] Login, registration, setup, and authenticated mutations reject missing or invalid CSRF tokens. Cross-origin browser submissions are rejected. GET requests cannot change state.
- [ ] Sensitive endpoints have bounded request bodies, server timeouts, and rate limits. Forwarded IP or scheme headers are not trusted without an explicit proxy boundary.
- [ ] Each order, conversation, notification, dispute, and administration endpoint enforces authorization in SQL or application logic, including guessed resource identifiers and forged form parameters.
- [ ] Database queries bind values as parameters. User content renders through `html/template` without casts to trusted HTML, JavaScript, or URLs.
- [ ] Secrets, session tokens, full database URLs, message contents, and private keys are absent from application logs and committed files.
- [ ] CSP, frame protection, no-sniff, and referrer restrictions are verified on actual responses. No debugging or profiling endpoint is publicly exposed.

Go's [HTML template documentation](https://pkg.go.dev/html/template) describes contextual escaping and the assumption that template authors are trusted. [The HTTP package documentation](https://pkg.go.dev/net/http) documents server limits, cookie attributes, and `CrossOriginProtection`. These are implementation references, not a substitute for application-level authorization and tests.

## Marketplace behavior

- [ ] Catalog filters, product detail, account routes, and every navigation link render valid pages, including empty and not-found states.
- [ ] Order creation persists the selected product, quantity, currency, and a server-calculated price snapshot. Client-supplied prices and roles are ignored.
- [ ] Currency values use integer atomic units or exact decimal arithmetic. Unsupported currencies and invalid quantities fail safely.
- [ ] State transitions reject unauthorized or repeated actions; concurrent requests cannot duplicate financial or delivery effects.
- [ ] User-facing errors preserve safe input, identify the problem, and do not report success after failed database writes.

## Explicit capability boundaries

Starting Bitcoin or Monero nodes only provides node infrastructure. It does **not** implement wallet custody, receiving-address assignment, payment attribution, chain-reorganization handling, escrow settlement, refunds, or transaction signing. External blockchain APIs likewise require authenticated, tested adapters and reconciliation. Until those exist, checkout must remain explicitly non-payable: no live-looking deposit address, no pretend confirmation counter, and no claim that funds are held in escrow.

Saving a PGP public key is not proof of ownership and does not encrypt a message. An encryption or signature indicator requires actual cryptographic verification. Plaintext messaging must be described as plaintext. TOTP enrollment must verify a code before activation, and recovery codes need real one-time semantics. Unimplemented TOTP, CAPTCHA, XMPP, evidence upload, and digital-delivery actions must be visibly unavailable rather than return cosmetic success.

A canary is operator-authored content. The application must not invent signatures or assertions about legal orders. Tor support and security controls are deployment properties to verify; neither guarantees anonymity or establishes production readiness.

## Feature packages

Each package records its own acceptance checks here and edits only its subsection.

### P1 Authentication

- [ ] TOTP cannot activate without a valid code; recovery codes are hashed and single-use.
- [ ] Sign-in for an enrolled account always passes through `/challenge`.
- [ ] CAPTCHA is a same-origin PNG, needs no JavaScript, is single-use, expires, and is rate-limited. No audio alternative or QR code is claimed.
- [ ] RFC 6238 SHA-1 test vectors pass; codes outside ±1 step, and any code for an already accepted step, are rejected — including when the same code is submitted concurrently (`totp_test.go`, `TestTOTPConcurrentReplay`).
- [ ] The TOTP secret is stored sealed, never in plaintext; recovery codes appear exactly once and are stored only as SHA-256 hashes (`TestTOTPEnrollmentChallengeAndRecovery`).
- [ ] Replacing recovery codes needs a current code and invalidates old ones; turning TOTP off needs the password and a code, deletes all recovery codes, and is audited.
- [ ] The account page says "Enabled" only while TOTP is active, "Not enrolled (setup not confirmed)" for an unconfirmed secret, otherwise "Not enrolled".
- [ ] CAPTCHA: missing, wrong, reused, expired or other-session answers return 400 before any password hashing; the image is served only to the issuing session with `Cache-Control: no-store`; the 31st challenge in ten minutes shows a visible error instead of an image; setup never requires it (`TestCaptchaOnRegisterAndLogin`, `TestSetupSkipsCaptcha`).
- [ ] Turning the CAPTCHA off or on is administrator-only and recorded in the audit trail.
- [ ] Browser: `db-setup.spec.ts` rejects registration with a wrong CAPTCHA, then turns it off; `db-auth.spec.ts` enrolls TOTP with a code computed from the displayed secret, signs in through the challenge and spends a recovery code once; `preview-auth.spec.ts` checks the pages at every viewport without scripts.

### P2 PGP identity

- [ ] Saving a key rejects garbage, private keys, revoked keys and multiple keys; the account page shows the fingerprint and "Not verified" until a proof succeeds.
- [ ] "Verified" (with date) appears only after a signature over the current challenge verifies with the saved key, or the decrypted challenge nonce matches; a wrong key, wrong text or stale challenge is rejected.
- [ ] Changing or removing the key clears verification, PGP sign-in and open challenges, and is audited with the fingerprint.
- [ ] PGP sign-in cannot be turned on without a verified key; once on, sign-in always passes through `/challenge` and needs the decrypted one-time code, which works once.
- [ ] Challenge nonces and sign-in codes are stored only as SHA-256; GET requests never create them.
- [ ] Message status reflects packet inspection: plaintext, signed-only and fake armor are rejected at send; stored messages show "to recipient’s key", "NOT to recipient’s key" or "recipient unknown", never "Encrypted" for plaintext.

### P3 Orders

- [ ] Every transition matches the state table; forbidden moves return 403, stale or repeated moves 409. Users who are not party to an order get 404 from order actions, as from the order page.
- [ ] Without a payment provider for the currency, requesting payment returns 409 "Payment unavailable for BTC/XMR", changes nothing, and the order page shows the step as unavailable; no address is shown.
- [ ] Requesting payment reserves one unit of stock (409 when none is left or the listing is archived); cancelling from awaiting payment or paid returns it; drafts never hold stock.
- [ ] Digital content is released only by the delivered transition (vendor action, or automatic after the watcher marks a digital order paid), is labelled as stored unencrypted, and is invisible to third parties and moderators.
- [ ] Concurrent completion of one order yields exactly one success and one event row.
- [ ] Resolution requires a release/refund outcome, is refused to a moderator who is party to the order, and records the outcome before the resolved transition.
- [ ] Reviews require a completed order and its buyer; a second review returns 409; public pages label them "Verified purchase" without reviewer handles.

### P4 Inventory

- [ ] Only the owner or an administrator can edit; archived listings accept no new drafts and existing orders are unaffected.

### P5 Payments

- [ ] Mainnet chains/addresses stop startup with a clear error; no address is shown unless returned by a live test-network provider.
- [ ] Ledger entries are unique per `(currency, txid, output)`; state changes happen only through transitions.
- [ ] An order becomes `paid` exactly once, only after confirmed deposits reach its amount, even with concurrent watchers.
- [ ] A conflicted credited deposit is flagged to moderators without reverting the order, and its unsent payouts are held.
- [ ] Each payout is sent at most once. Failed and stuck (`sending`) payouts are shown to administrators and never retried. Payouts wait for a valid test-network payout address.
- [ ] Every address, amount and confirmation count carries a `TESTNET <network>` label. Pages say "Payments disabled" when no provider is configured.
- [ ] Manual: `scripts/regtest-smoke.sh` passes against a real `bitcoind -regtest` (not exercised in CI).

### P6 Transparency

- [ ] `/canary` shows exactly one of: no statement, verified (fingerprint and dates), or invalid with a reason.
- [ ] Audit exports and signatures are byte-stable for a given range and verify with the published Ed25519 key.

## Accessibility and visual verification

- [ ] All surfaces work at 320px and desktop widths without page-level horizontal overflow. Long identifiers wrap or have local scrolling.
- [ ] The first focusable element is a skip link. Navigation, forms, tabs, dialogs, and actionable cards have logical keyboard behavior and visible focus.
- [ ] Inputs have associated labels; errors and asynchronous statuses have appropriate accessible announcements. Status does not depend on color alone.
- [ ] Text contrast is measured for every foreground/background pairing: target 7:1 for normal text, at least 4.5:1 where the requested design explicitly permits AA. Record exceptions instead of claiming universal AAA.
- [ ] Reduced-motion preferences are respected, and disabled controls explain unavailable capabilities.

## Release evidence

Record `go test ./...`, relevant race tests, fresh-install and persistence results, internal/external database checks, setup lockout/CSRF/authorization negative tests, backup-restore results, and browser checks. Mark deployment or payment checks unverified when they were not exercised. A passing build alone is not evidence of secure deployment or payment functionality.
