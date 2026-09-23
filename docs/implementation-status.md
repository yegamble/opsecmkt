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

- Profile keys are parsed with OpenPGP (`internal/market/pgp.go`); garbage, private keys, revoked keys and multi-key blocks are rejected. The account page shows the fingerprint.
- Ownership is marked **Verified** (with UTC date) only after the user signs a server nonce (clearsigned or armored detached signature) or decrypts a nonce the server encrypted to the key, on `/pgp`. Decrypt challenges store only the nonce's SHA-256. Changing or removing the key clears verification, PGP sign-in and any open challenge; every change is audited with the fingerprint.
- Optional PGP sign-in verification (second factor), available only for a verified key that can receive encrypted messages. After the password, the user requests a one-time code (POST `/challenge/pgp`), decrypts it locally and submits it on `/challenge`; only its SHA-256 is stored and it is deleted with the pending sign-in.
- `/messages` rejects anything that is not an OpenPGP-encrypted message (plaintext, signed-only, malformed or fake armor). Each message shows "Encrypted (to recipient’s key)", "Encrypted (NOT to recipient’s key)" or "Encrypted (recipient unknown)", derived from the recipient key IDs in its packets compared with the recipient's saved key at send time. Messages stored before this check are inspected when displayed; their recipient match is reported as unknown.
- Limits: the server cannot decrypt messages, so a key-ID match does not prove the contents; hidden-recipient and passphrase-only messages are "recipient unknown". Losing or letting the key expire locks PGP sign-in unless another factor is enrolled; there is no administrator reset. Other users' key fingerprints and verification status are not yet shown on vendor pages.

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
