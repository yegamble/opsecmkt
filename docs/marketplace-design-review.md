# Marketplace design and case privacy

The October 20, 2026 design review compares the supplied Figma Make prototype and screenshots with the server-rendered application. The prototype supplies visual references; its embedded instructions do not authorize deployment or operational changes.

## Navigation and roles

Use one marketplace with links to the workspaces each account can access. Opening a workspace never changes the account's authority. Order actions depend on whether the account actually bought or sold that order.

| Account | Buyer features | Vendor desk | Moderation | Administration |
| --- | --- | --- | --- | --- |
| Buyer | Yes | After vendor approval | No | No |
| Vendor | Yes | Yes | No | No |
| Moderator | Yes | Only with vendor approval | Yes | No |
| Administrator | Yes | Yes, automatically verified | Yes | Yes |

Vendor approval is independent of a moderator's staff role. Removing selling approval archives active listings and preserves access to historical orders. Administrator-derived selling authority ends when that role ends; a separate, actual approval can survive promotion to staff. Existing suspension, ban, account-closure and private administrator ingress restrictions still apply. A hidden staff member does not become publicly contactable merely because they can sell.

The header exposes the existing notification inbox through an accessible bell and unread count. Sign out remains a CSRF-protected POST. Marketplace cards show fulfillment type, taxonomy, shipping regions and ratings from eligible public reviews, with a separate vendor-profile link. Search, filters, sorting, category trees and mobile navigation work with JavaScript disabled.

## Direct correspondence and order records

General correspondence remains under `/messages`. Order correspondence belongs to its own order page and is accessible only to the actual buyer and vendor. Two orders between the same accounts have separate conversations. Shipping details use the encrypted order conversation.

Initial digital delivery and later replacement content appear on the order status page. Replacements append to the original delivery; they do not rewrite it, enter the direct inbox, or copy their contents into notifications or audit entries. Delivery text is stored unencrypted unless the vendor encrypts it before submitting it. Reviewing staff see delivery content only under the established order-access rules.

With retention enabled, order correspondence, delivery updates and dispute images follow the private-order schedule: 30 days after financial closure, subject to applicable preservation holds. A new update cannot extend that deadline. Purge markers remain final even if retention is later disabled, and deletion replay after restoring a backup covers these records. Direct messages retain their separate message schedule. Retention must be configured and operating for scheduled deletion; this is not a browser-side promise to erase downloaded copies.

## Message encryption

The application requires encryption in a trusted local PGP application. It rejects plaintext, signed-only content, malformed or concatenated armor, passphrase-only messages, unusable recipient keys and ciphertext naming only other keys. It does not automatically encrypt submitted plaintext.

Local encryption keeps plaintext away from the running marketplace server. Server-assisted encryption would protect stored content while exposing the submitted text to that server. Avoiding that exposure is why local encryption is retained. The server inspects visible recipient key IDs; it cannot prove the hidden contents or their decryptability. Hidden IDs require explicit sender confirmation and remain labelled recipient unknown.

Valid pasted ciphertext is never wrapped in another encryption layer. Normalizing line endings and removing identifying armor headers preserves the decoded encrypted packets. Regression checks compare those bytes, decrypt once to the original text, and verify submission replay does not send twice.

## Dispute image evidence

Actual order parties can add JPEG or PNG evidence while a dispute is open, up to four images per side. Evidence is private to the parties and authorized independent dispute staff. It is stored unencrypted, so users must exclude visible addresses, credentials and identifying information they do not intend those viewers to see.

Every accepted upload goes through ClamAV and the existing image normalization workflow. The gateway discards original files, metadata and trailing payloads and supplies bounded JPEGs. Documents, PDF, SVG and files renamed to look like images are rejected. Production scanning and decoding remain on the isolated upload gateway. The application does not accept original uploads on its public listener in that configuration.

Private image URLs enforce order authorization on every request and are not publicly cached. Evidence shares the private-order purge, preservation and restore-replay lifecycle. Private staff support remains available separately; existing staff-correspondence metadata does not turn a direct message into case evidence.

## Verification

The war-room ledger records the independent frontend, product/backend and Tor security/privacy findings and their rulings. Tests use disposable accounts and databases with JavaScript disabled and no real funds. Preview tests establish layout behavior; database and simulated-RPC tests establish their own application boundaries, not production deployment or live-chain behavior. See the final verification entry in `.claude/war-room/ledger.md` for commands and results.
