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
