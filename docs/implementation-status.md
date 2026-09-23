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

Implemented on the Foundation state machine; every action goes through the server-side transition table, row lock and compare-and-set, so a forbidden move returns 403, a stale or repeated one 409, and nothing reports success without a recorded state change.

- **Payment request** (`/orders/pay`, buyer, draft → awaiting payment): refused with 409 "Payment unavailable for BTC/XMR" unless a test-network wallet provider is configured for that currency; the order page shows the step as unavailable instead of a button. On success one unit of stock is reserved and the live provider's address is stored. No address is ever invented by the application.
- **Cancellation** (`/orders/cancel`): buyer cancels a draft; buyer or vendor cancels an order awaiting payment; vendor cancels a paid order. Reserved stock is returned. Refunds of received funds are not performed by this action.
- **Shipping** (vendor, physical, paid → shipped) with an optional note; **digital delivery** (vendor, digital, paid → delivered) with 1–32000 characters of content; **completion** (buyer, shipped/delivered → completed).
- **Automatic delivery**: when the payment watcher marks a digital order paid and the listing has delivery content, the content is recorded and the order moves to delivered as the system actor in the same transaction.
- Delivery content is stored **unencrypted** in PostgreSQL, is written only in the same transaction as the delivered transition, and is shown only to the order's buyer and vendor.
- **Disputes** move a paid, shipped or delivered order to disputed. **Resolution** requires an outcome (release to vendor or refund to buyer) and a written decision, and is refused to a moderator or administrator who is party to the order. The outcome is recorded before the resolved transition so payment hooks can act on it; the moderation desk itself moves no funds.
- **Verified reviews**: one per completed order, by its buyer, rating 1–5 and up to 2000 characters. Product and vendor pages label them "Verified purchase" because each is keyed to a completed order; reviewer handles and order ids are not shown publicly and dates are shown by month.
- Order pages show the full transition history (actor and time), the moves available to the viewer, and the delivery and review panels. Orders, vendor desk and moderator desk list the relevant orders with their state.

Limits: service listings have no ship/deliver step, so a paid service order can only be cancelled by the vendor or disputed. There are no auto-completion timers, and a moderator cannot open the order page of an order they are not party to (the desk shows an order summary instead).

### P4 Inventory

Not implemented: listing editing, archiving/restoring and automatic delivery content.

### P5 Payments

Implemented, **test networks only** (disabled unless configured; see README "Feature configuration"):

- Bitcoin Core adapter (testnet3, testnet4, signet, regtest) over wallet JSON-RPC with Basic auth, and a monero-wallet-rpc adapter (stagenet, testnet) with HTTP Digest auth. Startup exits with an error when a node or wallet reports mainnet, when `BITCOIN_CHAIN`/`MONERO_NETWORK` disagree with the node, or when a configured wallet is unreachable. Wallet-returned addresses with a mainnet prefix are refused.
- One background watcher per deployment (PostgreSQL advisory lock) polls every `PAYMENT_POLL_INTERVAL`. Deposits are recorded in an idempotent ledger keyed by `(currency, txid, output)`. An order moves from `awaiting_payment` to `paid` only through the state machine, once confirmed deposits reach the order amount at the configured confirmation threshold.
- A credited deposit that later conflicts or disappears from the wallet is flagged in the order history and to moderators. The order is never reverted automatically, and unsent payouts are held. Deposits that confirm after settlement are flagged, not paid out.
- Payouts: completing an order queues a release to the vendor. Cancelling a funded order queues a refund to the buyer. Resolving a dispute queues a release or refund according to its recorded outcome. Each payout is a single wallet call: failed or stuck payouts are shown to administrators and never retried. A payout waits ("blocked") until the recipient saves a payout address, which is validated against the provider's test network.
- Order pages show the deposit address, amounts and confirmations only when they come from a live provider and the ledger, each labelled `TESTNET <network>`. Footer, catalog, checkout, product and vendor pages state "Payments disabled" or "TESTNET payments (<networks>) — no real funds". The admin page lists each provider's network, status, last poll and last error, plus recent payouts.
- The payment step and the other order actions (`/orders/pay`, ship, complete, cancel, dispute outcomes) belong to the Orders package. Without them, orders cannot reach `awaiting_payment` in this build.

Not implemented: multisig escrow, marketplace commission or fee accounting, automatic payout retries, and mainnet payments (by design). Funds sit in one pooled custodial wallet per currency. Bitcoin payouts deduct the network fee from the amount sent. Monero payout fees are paid from the pooled wallet. The regtest smoke test (`scripts/regtest-smoke.sh`) is manual and has not been run in this environment.

### P6 Transparency

Not implemented: signed warrant canary verification and signed audit export.

## Not yet implemented

Payment addresses, wallet custody, deposit monitoring, escrow, refunds, withdrawals, automated digital delivery, actual node/API connectivity, TOTP enrollment/challenge, recovery codes, CAPTCHA, PGP signature/key verification, XMPP delivery, signed canary publication, signed audit export, verified reviews, inventory editing/archiving, and automatic mirror orchestration are not implemented. Related interface states are explicitly unavailable; no mock balances or deposit addresses are presented as real.

Tor deployment isolates inbound exposure. Local full nodes use direct peer-network egress. Selecting external database/RPC services explicitly permits application egress; this is not an all-traffic-through-Tor configuration. Read README before deployment.

This is a functional application foundation and interface, not a completed production cryptocurrency exchange or escrow service.
