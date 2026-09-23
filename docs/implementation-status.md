# Implementation status

The Figma reference is adapted to Go-rendered HTML and CSS. The browser loads no JavaScript or third-party resources. Product artwork is decorative CSS illustration, not product photography. The original `[OPSECMKT]` wordmark is retained.

## Working application behavior

- Token-protected, transactional first-run administrator setup; repeated setup is rejected.
- Buyer registration, bcrypt password hashing, rotating hashed sessions, session revocation, CSRF validation, bounded forms and authentication concurrency.
- Server-enforced buyer, vendor, moderator, and administrator roles.
- PostgreSQL-backed listing creation, catalog search/category/region filters, product and vendor pages.
- Atomic-unit BTC/XMR price storage; idempotent **unfunded draft** creation and owner-only order access. Draft creation does not reserve stock or collect payment.
- Orders carry a canonical state (`draft`, `awaiting_payment`, `paid`, `shipped`, `delivered`, `completed`, `disputed`, `resolved`, `cancelled`). A server-side transition table, row locks and compare-and-set updates guard every change and log it in `order_events`. Only drafts can be created today; no user-facing transition beyond the draft exists yet.
- Versioned, idempotent schema migrations applied at startup under an advisory lock (see README "Migrations").
- Storage and delivery of locally encrypted armored messages, recipient notifications, and account audit history. Armor is format-checked, not cryptographically authenticated.
- Moderator dispute decisions can be recorded for eligible funded states; this version cannot create funded states or transfer funds.
- Desired node settings are saved for operator review. Deployment remains operator-controlled.
- Docker internal/external database selection, separate clearnet/Tor exposure, optional operator-supplied full-node containers, encrypted database backup and transactional restore scripts.

## Feature packages

Each package updates only its own subsection when it lands.

### P1 Authentication

- **TOTP second factor** (`/totp`): RFC 6238 (HMAC-SHA-1, 30-second steps, 6 digits, one step of clock skew either way) implemented with the Go standard library. Enrollment generates a random 160-bit secret, stored encrypted (AES-256-GCM, key derived from `SETUP_TOKEN`) and inactive until the user confirms a correct code. Every accepted code advances a per-account step counter, so a code cannot be used twice, including by concurrent requests. Turning TOTP off requires the password and a current code or recovery code.
- **Sign-in challenge**: after a correct password, an account with TOTP enabled always receives a 10-minute pending login and must complete `/challenge`; no session is issued before that. Code guesses are limited to 10 per pending login and 10 per account per ten minutes (in-memory limits, per application instance).
- **Recovery codes**: 10 codes of 80 random bits each, issued on activation and on replacement (which requires a current code and invalidates all earlier codes). Only SHA-256 hashes are stored; each code is marked used in the same transaction that signs in. The plaintext codes are displayed once, on the first `/totp` view within 10 minutes of issue.
- **Image CAPTCHA** on `/login` and `/register` (not on first-run setup): six characters from a 30-character alphabet, drawn server-side as a same-origin PNG from a built-in 5×7 bitmap font with jitter, shear, wave and noise. Each challenge is bound to the browser session, single-use (a wrong answer consumes it), expires after 10 minutes, and at most 30 are issued per session per ten minutes. Only a hash of the answer is stored. It is on for new and upgraded installations; administrators can turn it off or on from `/admin`, and each change is audited.
- Not provided: QR codes for enrollment (the secret and `otpauth://` URI are shown as text), an audio or other non-visual CAPTCHA alternative, WebAuthn/security keys, and administrator-forced TOTP. A CAPTCHA only slows automated sign-ups; it does not stop a determined attacker or a solving service.

### P2 PGP identity

Not implemented: key parsing and fingerprints, ownership proof, PGP second factor, message encryption inspection. Saved keys and messages are format-checked only.

### P3 Orders

Not implemented: payment request, cancellation, shipping, delivery, completion, dispute outcomes and verified reviews. The state machine exists; users can only create drafts.

### P4 Inventory

Not implemented: listing editing, archiving/restoring and automatic delivery content.

### P5 Payments

Not implemented: test-network wallet adapters, deposit monitoring and payouts. Payments are disabled; no address or balance is shown.

### P6 Transparency

Not implemented: signed warrant canary verification and signed audit export.

## Not yet implemented

Payment addresses, wallet custody, deposit monitoring, escrow, refunds, withdrawals, automated digital delivery, actual node/API connectivity, TOTP enrollment/challenge, recovery codes, CAPTCHA, PGP signature/key verification, XMPP delivery, signed canary publication, signed audit export, verified reviews, inventory editing/archiving, and automatic mirror orchestration are not implemented. Related interface states are explicitly unavailable; no mock balances or deposit addresses are presented as real.

Tor deployment isolates inbound exposure. Local full nodes use direct peer-network egress. Selecting external database/RPC services explicitly permits application egress; this is not an all-traffic-through-Tor configuration. Read README before deployment.

This is a functional application foundation and interface, not a completed production cryptocurrency exchange or escrow service.
