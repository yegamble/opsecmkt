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

Not implemented: payment request, cancellation, shipping, delivery, completion, dispute outcomes and verified reviews. The state machine exists; users can only create drafts.

### P4 Inventory

- Vendors edit their own listings at `/listing-edit?id=` (title, description, category, region, fulfillment type, stock, BTC/XMR reference prices, optional automatic delivery content). Administrators may edit any listing; the audit log records "as administrator". Other vendors get 404.
- Create and edit share one validator. Only `physical` and `digital` fulfillment are accepted: the order state machine has no fulfillment step for a `service` kind, so the form no longer offers it.
- Archive/restore (`/listings/archive`, `/listings/restore`): archived listings disappear from the catalog, search, product, checkout and vendor pages, and new order drafts are refused (400). The owner still sees them on the vendor desk with an "Archived" badge. Existing orders are not changed.
- The fulfillment type cannot change while the listing has open orders (409), because orders read it from the listing. Orders keep the price recorded at creation.
- Stock edits are refused (409) when stock changed after the form was opened (payment requests reserve stock); saving other fields keeps the current stock.
- Automatic delivery content is digital-only, at most 32000 characters, **stored unencrypted in PostgreSQL**, and never loaded on public pages. It is released only by the order package's confirmed-payment step, so the editor says "Automatic delivery requires a payment provider" whenever no provider is configured.
- Migration `040_inventory.sql` adds `products.updated` and `products.archived_at`.

### P5 Payments

Not implemented: test-network wallet adapters, deposit monitoring and payouts. Payments are disabled; no address or balance is shown.

### P6 Transparency

Not implemented: signed warrant canary verification and signed audit export.

## Not yet implemented

Payment addresses, wallet custody, deposit monitoring, escrow, refunds, withdrawals, automated digital delivery, actual node/API connectivity, TOTP enrollment/challenge, recovery codes, CAPTCHA, PGP signature/key verification, XMPP delivery, signed canary publication, signed audit export, verified reviews, inventory editing/archiving, and automatic mirror orchestration are not implemented. Related interface states are explicitly unavailable; no mock balances or deposit addresses are presented as real.

Tor deployment isolates inbound exposure. Local full nodes use direct peer-network egress. Selecting external database/RPC services explicitly permits application egress; this is not an all-traffic-through-Tor configuration. Read README before deployment.

This is a functional application foundation and interface, not a completed production cryptocurrency exchange or escrow service.
