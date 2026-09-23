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

- Profile keys are parsed with OpenPGP (`internal/market/pgp.go`); garbage, private keys, revoked keys and multi-key blocks are rejected. The account page shows the fingerprint.
- Ownership is marked **Verified** (with UTC date) only after the user signs a server nonce (clearsigned or armored detached signature) or decrypts a nonce the server encrypted to the key, on `/pgp`. Decrypt challenges store only the nonce's SHA-256. Changing or removing the key clears verification, PGP sign-in and any open challenge; every change is audited with the fingerprint.
- Optional PGP sign-in verification (second factor), available only for a verified key that can receive encrypted messages. After the password, the user requests a one-time code (POST `/challenge/pgp`), decrypts it locally and submits it on `/challenge`; only its SHA-256 is stored and it is deleted with the pending sign-in.
- `/messages` rejects anything that is not an OpenPGP-encrypted message (plaintext, signed-only, malformed or fake armor). Each message shows "Encrypted (to recipient’s key)", "Encrypted (NOT to recipient’s key)" or "Encrypted (recipient unknown)", derived from the recipient key IDs in its packets compared with the recipient's saved key at send time. Messages stored before this check are inspected when displayed; their recipient match is reported as unknown.
- Limits: the server cannot decrypt messages, so a key-ID match does not prove the contents; hidden-recipient and passphrase-only messages are "recipient unknown". Losing or letting the key expire locks PGP sign-in unless another factor is enrolled; there is no administrator reset. Other users' key fingerprints and verification status are not yet shown on vendor pages.

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

- Vendors edit their own listings at `/listing-edit?id=` (title, description, category, region, fulfillment type, stock, BTC/XMR reference prices, optional automatic delivery content). Administrators may edit any listing; the audit log records "as administrator". Other vendors get 404.
- Create and edit share one validator. Only `physical` and `digital` fulfillment are accepted: the order state machine has no fulfillment step for a `service` kind, so the form no longer offers it.
- Archive/restore (`/listings/archive`, `/listings/restore`): archived listings disappear from the catalog, search, product, checkout and vendor pages, and new order drafts are refused (400). The owner still sees them on the vendor desk with an "Archived" badge. Existing orders are not changed.
- The fulfillment type cannot change while the listing has open orders (409), because orders read it from the listing. Orders keep the price recorded at creation.
- Stock edits are refused (409) when stock changed after the form was opened (payment requests reserve stock); saving other fields keeps the current stock.
- Automatic delivery content is digital-only, at most 32000 characters, **stored unencrypted in PostgreSQL**, and never loaded on public pages. It is released only by the order package's confirmed-payment step, so the editor says "Automatic delivery requires a payment provider" whenever no provider is configured.
- Migration `040_inventory.sql` adds `products.updated` and `products.archived_at`.

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

- **Operator key.** An administrator saves the operator's armored OpenPGP public key (`/admin/operator-key`); it is parsed (single, unrevoked public key; private keys rejected) and its fingerprint recorded in the audit log.
- **Warrant canary.** The administrator pastes a statement the operator clearsigned offline (`/admin/canary`). It is rejected with the reason unless it is exactly one clearsigned message, with nothing before or after it, that verifies against the operator key. Accepted text is stored as pasted in a single-row `canary` table (migration 060). `/canary` re-verifies it on every view and shows exactly one of: "No signed canary published", "Signature verified" (statement, grouped fingerprint, signature date, posted date, the signed message and public key for independent `gpg --verify`), or "INVALID" with the reason (for example after a key change or database tampering). The application never writes, edits or signs canary text.
- **Signed audit export.** Administrators download `/admin/audit-export?upto=N` as NDJSON (`id`, `user_id`, `handle`, `action`, `created` in RFC 3339 UTC, ordered by id) and `&sig=1` as a text file with the Ed25519 public key, SHA-256 and signature over the exact export bytes. The key is read only from `AUDIT_SIGNING_KEY`; when it is unset or malformed the export returns 409 and the admin and `/canary` pages say it is unavailable. `upto` must not exceed the latest settled event (a brief `SHARE` lock waits for in-flight audit writers), so the same `upto` always yields byte-identical files. Rate limit: 10 downloads per 10 minutes per administrator. The public key is published on `/canary`.
- **Verifier.** `go run ./cmd/verify-audit -pub <hex> export.jsonl export.sig` (standard library only) checks the signature with the pinned key, not the key named in the `.sig` file, and that events are well formed with ascending ids; exit status 0 or 1.
- **Limits.** The signature proves origin from the server-held key only. Anyone controlling the server (or `AUDIT_SIGNING_KEY`) can sign an altered history; exports are not a tamper-proof log. Canary verification proves which key signed the text, not that its author acts freely or that the statement is true.

## Not yet implemented

Payment addresses, wallet custody, deposit monitoring, escrow, refunds, withdrawals, automated digital delivery, actual node/API connectivity, TOTP enrollment/challenge, recovery codes, CAPTCHA, PGP signature/key verification, XMPP delivery, signed canary publication, signed audit export, verified reviews, inventory editing/archiving, and automatic mirror orchestration are not implemented. Related interface states are explicitly unavailable; no mock balances or deposit addresses are presented as real.

Tor deployment isolates inbound exposure. Local full nodes use direct peer-network egress. Selecting external database/RPC services explicitly permits application egress; this is not an all-traffic-through-Tor configuration. Read README before deployment.

This is a functional application foundation and interface, not a completed production cryptocurrency exchange or escrow service.
