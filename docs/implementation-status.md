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

Not implemented: listing editing, archiving/restoring and automatic delivery content.

### P5 Payments

Not implemented: test-network wallet adapters, deposit monitoring and payouts. Payments are disabled; no address or balance is shown.

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
