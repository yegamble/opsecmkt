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

Not implemented: TOTP enrollment and challenge, recovery codes, CAPTCHA. A second-factor challenge step (`/challenge`) exists but no factor can be enrolled yet, so sign-in is password-only.

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

Not implemented: test-network wallet adapters, deposit monitoring and payouts. Payments are disabled; no address or balance is shown.

### P6 Transparency

Not implemented: signed warrant canary verification and signed audit export.

## Not yet implemented

Payment addresses, wallet custody, deposit monitoring, escrow, refunds, withdrawals, automated digital delivery, actual node/API connectivity, TOTP enrollment/challenge, recovery codes, CAPTCHA, PGP signature/key verification, XMPP delivery, signed canary publication, signed audit export, verified reviews, inventory editing/archiving, and automatic mirror orchestration are not implemented. Related interface states are explicitly unavailable; no mock balances or deposit addresses are presented as real.

Tor deployment isolates inbound exposure. Local full nodes use direct peer-network egress. Selecting external database/RPC services explicitly permits application egress; this is not an all-traffic-through-Tor configuration. Read README before deployment.

This is a functional application foundation and interface, not a completed production cryptocurrency exchange or escrow service.
