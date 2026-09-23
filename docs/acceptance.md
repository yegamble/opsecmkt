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
- [ ] Order creation persists the selected product, currency, and a server-calculated price snapshot; quantity is fixed at one unit per order (there is no quantity field). Client-supplied prices and roles are ignored.
- [ ] Currency values use integer atomic units or exact decimal arithmetic. Unsupported currencies and invalid quantities fail safely.
- [ ] State transitions reject unauthorized or repeated actions; concurrent requests cannot duplicate financial or delivery effects.
- [ ] User-facing errors preserve safe input, identify the problem, and do not report success after failed database writes.

## Explicit capability boundaries

Starting Bitcoin or Monero nodes only provides node infrastructure. Payments additionally need a configured **test-network** wallet provider (Bitcoin Core wallet RPC or monero-wallet-rpc); the application then assigns receiving addresses, attributes confirmed deposits, flags conflicted ones, and sends single-attempt releases and refunds from the operator's pooled test-network wallet. That is custodial holding, not escrow: there is no multisig settlement or fee accounting, and mainnet is refused at startup. Third-party blockchain APIs are not supported. For any currency without a provider, checkout must remain explicitly non-payable: no live-looking deposit address, no pretend confirmation counter, and no claim that funds are held in escrow.

Saving a PGP public key is not proof of ownership and does not encrypt a message. An encryption or signature indicator requires actual cryptographic verification. Plaintext messaging must be described as plaintext. TOTP enrollment must verify a code before activation, and recovery codes need real one-time semantics. Unimplemented features (XMPP delivery, dispute evidence upload, QR-code enrollment, audio CAPTCHA) must be visibly unavailable or absent rather than return cosmetic success.

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
- [ ] Random pending-login cookies and malformed handles create no rate-limiter entries, and a full limiter table evicts the oldest entry instead of refusing new keys: a flood of junk requests leaves real sign-ins and writes working (`TestLimiterFloodDoesNotLockOutUsers`).
- [ ] Password change needs the current password (and a code or unused recovery code when TOTP is enrolled), applies the registration rule, ends other sessions and pending sign-ins, rotates the current session, stops the old password working and is audited; missing CSRF, cross-site and anonymous requests are refused (`TestPasswordChangeRevokesOtherSessions`, `TestPasswordChangeNeedsSecondFactorWhenEnrolled`).
- [ ] An administrator, confirmed with their own password (and code when enrolled), can turn off TOTP and PGP sign-in for a non-administrator account; the key and its verification stay, sessions end, both accounts are audited and the user is notified and signs in with the password only. Self, other administrators, non-administrators and wrong confirmation are refused without change (`TestAdminResetsSecondFactors`).
- [ ] Role changes write an audit row on the administrator (naming the account, old and new role) and on the account in one transaction; administrator is never assignable and an administrator cannot change their own role; the admin audit trail shows each row's account (`TestAdminRoleChangeAuditsBothAccountsAndGuards`).
- [ ] Browser: `db-setup.spec.ts` rejects registration with a wrong CAPTCHA, then turns it off; `db-identity.spec.ts` changes a password (with and without TOTP) and checks the other browser is signed out; `db-admin.spec.ts` resets a TOTP account's second factors, which then signs in with its password only; `db-auth.spec.ts` enrolls TOTP with a code computed from the displayed secret, signs in through the challenge and spends a recovery code once; `preview-auth.spec.ts` checks the pages at every viewport without scripts.

### P2 PGP identity

- [ ] Saving a key rejects garbage, private keys, revoked keys and multiple keys; the account page shows the fingerprint and "Not verified" until a proof succeeds.
- [ ] "Verified" (with date) appears only after a signature over the current challenge verifies with the saved key, or the decrypted challenge nonce matches; a wrong key, wrong text or stale challenge is rejected.
- [ ] Changing or removing the key clears verification, PGP sign-in and open challenges, and is audited with the fingerprint.
- [ ] PGP sign-in cannot be turned on without a verified key; once on, sign-in always passes through `/challenge` and needs the decrypted one-time code, which works once.
- [ ] While PGP sign-in is on, turning it off and changing or removing the key need the current password (and an authenticator code when TOTP is enrolled); a session alone gets 400/401 (`TestSensitiveChangesNeedPassword`).
- [ ] Saving the profile with an unchanged stored key that no longer parses succeeds (`TestLegacyUnparseableKeySavesUnchanged`).
- [ ] Challenge nonces and sign-in codes are stored only as SHA-256; GET requests never create them.
- [ ] Message status reflects packet inspection: plaintext, signed-only and fake armor are rejected at send; stored messages show "to recipient’s key", "NOT to recipient’s key" or "recipient unknown", never "Encrypted" for plaintext.

### P3 Orders

- [ ] Every transition matches the state table; forbidden moves return 403, stale or repeated moves 409. Users who are not party to an order get 404 from order actions, as from the order page.
- [ ] Without a payment provider for the currency, requesting payment returns 409 "Payment unavailable for BTC/XMR", changes nothing, and the order page shows the step as unavailable; no address is shown.
- [ ] Requesting payment reserves one unit of stock (409 when none is left or the listing is archived); cancelling from awaiting payment or paid returns it; drafts never hold stock.
- [ ] Resubmitting checkout and requesting payment both record the listing's current price (`TestDraftRepricedOnResubmitAndPay`); a fourth order awaiting payment is refused with 409 (`TestPayCapsOpenAwaitingPaymentOrders`).
- [ ] Digital content is released only by the delivered transition (vendor action, or automatic after the watcher marks a digital order paid), is labelled as stored unencrypted, and is invisible to third parties; moderators and administrators see it only once the order is disputed (or resolved).
- [ ] Moderators and administrators who are not party to an order open its page read-only only when it is disputed or resolved (summary, history, dispute reason and decision, delivery record); other orders return 404, no buyer/vendor action forms render, and the actions return 404. The moderation desk links each dispute to its order (`TestModeratorViewsDisputedOrderReadOnly`).
- [ ] Open work is never hidden behind newer closed history: every open dispute is listed first (oldest first) with a notice when resolved history is cut to the 100 most recent, and the vendor desk's "To fulfil" count and list include every paid order on the vendor's listings (`TestModeratorDeskListsEveryOpenDisputeFirst`, `TestVendorDeskCountsAndListsEveryPaidOrder`).
- [ ] Vendor and order pages show the counterparty's armored public key, grouped fingerprint and "Ownership verified" only when the proof matches the saved key; a missing key says messages cannot be sent until one is added. Contact links open `/messages?to=<handle>` with the recipient filled in (invalid handles ignored). A paid physical order tells the buyer to send the shipping address encrypted to the vendor's key; the server stores no addresses (`TestCounterpartyKeysAndMessagePrefill`).
- [ ] Signed-in users see "Notifications" in the main navigation at every width, with the unread count as text (`TestUnreadNotificationCountInNavigation`).
- [ ] Concurrent completion of one order yields exactly one success and one event row.
- [ ] Resolution requires a release/refund outcome, is refused to a moderator who is party to the order, and records the outcome before the resolved transition.
- [ ] Reviews require a completed order and its buyer; a second review returns 409; public pages label them "Verified purchase" without reviewer handles.

### P4 Inventory

- [ ] Only the owner or an administrator can edit, archive or restore; other vendors get 404, buyers and moderators 403 (`inventory_integration_test.go`).
- [ ] Archived listings are absent from catalog, search, product, checkout and vendor pages, accept no new drafts (400), stay visible to the owner with a badge, and existing orders are unaffected; restore reverses it (`TestArchiveHidesListingAndRefusesDrafts`, `db-inventory.spec.ts`).
- [ ] Fulfillment type cannot change under open orders; stale stock edits are refused instead of overwriting reservations.
- [ ] Automatic delivery content is labelled as stored unencrypted, never rendered on public pages, and shown as "Automatic delivery requires a payment provider" when none is configured.
- [ ] Edits, archives and restores are recorded in the audit log.
- [ ] Changing a vendor's role to buyer or moderator archives their active listings in the same transaction (audited for the administrator and the account); new drafts, payment requests and restores on listings whose owner is not a vendor or administrator are refused, while existing orders continue (`TestVendorDemotionArchivesListingsAndRefusesNewOrders`).

### P5 Payments

- [ ] Mainnet chains/addresses stop startup with a clear error; no address is shown unless returned by a live test-network provider.
- [ ] Ledger entries are unique per `(currency, txid, output)`; state changes happen only through transitions.
- [ ] An order becomes `paid` exactly once, only after confirmed deposits reach its amount, even with concurrent watchers.
- [ ] A conflicted credited deposit is flagged to moderators without reverting the order, and its unsent payouts are held. A credited deposit back below the threshold holds the payout until it re-confirms (`TestPayoutHeldWhileCreditedDepositReconfirms`).
- [ ] Orders awaiting payment past `PAYMENT_EXPIRY` with no deposit seen are cancelled by the system and their stock returned; orders with a deposit still confirming stay open (`TestWatcherExpiresUnpaidOrders`).
- [ ] A deposit that confirms after a cancellation (within the 30-day watch window) is refunded to the buyer, who is notified; extra funds on completed orders are flagged, not paid; older addresses are not polled (`TestLateDepositAfterCloseIsHandled`).
- [ ] Funded, unsettled orders are watched whatever their age: a deposit first seen after funding is noted once in the order history and to the buyer and vendor, and the single release pays the sum of settled deposits; a funding deposit that disappears on an order unchanged for days is flagged and holds the payout (`TestWatcherAnnouncesDepositAfterFundingOnOldPaidOrder`, `TestWatcherDetectsConflictOnOldShippedOrder`).
- [ ] Monero transfers with a non-zero `unlock_time` are shown but never credited or paid out, with one "locked transfer ignored" note (`TestMoneroLockedTransfersAreNotCredited`).
- [ ] Saving or removing a payout address needs the current password, plus an authenticator code when TOTP is enrolled.
- [ ] Each payout is sent at most once. Failed and stuck (`sending`) payouts are shown to administrators and never retried. Payouts wait for a valid test-network payout address.
- [ ] A payout send may take up to 30 s (reads 10 s) and is recorded as sent (`TestSlowWalletSendIsRecordedSentByTheWatcher`). A failed send is recorded as a definite rejection or as ambiguous; requeueing an ambiguous failure or a stuck send is refused without the audited "not broadcast" confirmation (`TestAmbiguousFailedPayoutRequeueNeedsBroadcastConfirmation`).
- [ ] Every address, amount and confirmation count carries a `TESTNET <network>` label. Pages say "Payments disabled" when no provider is configured.
- [ ] Manual: `scripts/regtest-smoke.sh` passes against a real `bitcoind -regtest` (not exercised in CI).

### P6 Transparency

- [ ] `/canary` shows exactly one of: no statement, verified (fingerprint and dates), or invalid with a reason.
- [ ] The canary is re-verified on every render: changing the operator key or editing the stored text shows INVALID; the application never writes, edits or signs canary text.
- [ ] Publishing a canary without an operator key returns 409; unverifiable, tampered, other-key, or padded submissions return 400 with the reason and store nothing. Only administrators can set the key or publish.
- [ ] Audit exports and signatures are byte-stable for a given range and verify with the published Ed25519 key.
- [ ] `verify-audit -pub <pinned key>` exits 0 for a genuine export and 1 for tampered files, other keys, or malformed input.
- [ ] Without a valid `AUDIT_SIGNING_KEY` the export returns 409 and the admin and `/canary` pages show it as unavailable; non-administrators receive 403 and exports are rate-limited.

## Accessibility and visual verification

- [ ] All surfaces work at 320px and desktop widths without page-level horizontal overflow. Long identifiers wrap or have local scrolling.
- [ ] The first focusable element is a skip link. Navigation, forms, tabs, dialogs, and actionable cards have logical keyboard behavior and visible focus.
- [ ] Inputs have associated labels; errors and asynchronous statuses have appropriate accessible announcements. Status does not depend on color alone.
- [ ] Text contrast is measured for every foreground/background pairing: target 7:1 for normal text, at least 4.5:1 where the requested design explicitly permits AA. Record exceptions instead of claiming universal AAA.
- [ ] Reduced-motion preferences are respected, and disabled controls explain unavailable capabilities.

## Release evidence

Record `go test ./...`, relevant race tests, fresh-install and persistence results, internal/external database checks, setup lockout/CSRF/authorization negative tests, backup-restore results, and browser checks. Mark deployment or payment checks unverified when they were not exercised. A passing build alone is not evidence of secure deployment or payment functionality.
