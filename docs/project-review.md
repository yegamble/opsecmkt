# Project review and product verification — 2026-09-23

This review covers the existing application and the requested product journeys:
one-command local installation followed by browser setup, administration, per-currency
payment intake, partial deposits and top-ups, order fulfillment, and encrypted
contact with vendors and dispute staff. It includes independent agent reviews,
reproduction tests, fixes and a separate scrutiny pass. Test-network-only payments
remain an intentional boundary.

## Findings and changes

| Finding | Effect | Change / evidence |
| --- | --- | --- |
| Restoring an older backup could resend settlements | Blocked/reorg-held payouts and later-created payout rows were not protected | Persistent recovery lock plus holds; encrypted PostgreSQL 17 restore/restart rehearsal and payout-gate regressions |
| PGP login code outlived key rotation | A code for an old ownership proof could authenticate after replacement | Bind the code hash to the key fingerprint and exact ownership-proof timestamp; lock current enrollment during verification |
| Factor activation could overlap sensitive changes | Password-only confirmation could precede a concurrent TOTP activation | Lock the account before selecting confirmation requirements; serialization regression |
| Provider disappeared during payment request | Request could panic after reserving stock | Re-check provider before issuing the address; rollback regression |
| Conflicted credited deposit stopped being watched on a closed order | A held payout could never resume after the deposit recovered | Keep watching credited unsettled deposits within the documented 30-day window |
| Listing kind edit hid old digital delivery | Buyer could lose access to purchased content after completion | Render an existing delivery independently of current listing kind; access-control regression |
| Vendor demotion raced listing creation | A stale request could publish an active listing after losing the vendor role | Recheck and lock the current role through creation; concurrent demotion then archives accepted listings |
| Browser tests inherited operator wallet endpoints | Supposedly disabled-payment fixtures could contact configured wallets | Explicitly blank wallet URLs in the normal browser configuration |
| macOS WebKit skip-link check used the wrong keyboard shortcut | Otherwise working link traversal failed locally | Use platform-appropriate Option-Tab, preserving traversal/focus/activation assertions |

## Product journeys

- Installation: `scripts/install.sh --local` provisions the local app/database,
  waits for readiness and reports the correct browser URL. Existing configuration
  is never overwritten. Interactive deployment remains available for HTTPS/Tor.
- Browser setup: name the instance and create the first administrator, then arrive
  at an actionable getting-started checklist. Setup cannot run again.
- Payment controls: pause/resume new BTC and XMR payment addresses separately;
  existing deposits, top-ups, fulfillment and payouts continue. Connecting the
  underlying wallet service remains a deployment operation.
- Partial payments: the order retains its address and accumulates deposits;
  remaining-to-send guidance accounts for funds already received but still
  awaiting confirmations. Completion requires the confirmation threshold.
- Messaging: recipients' real public keys and ownership status can be looked up
  without JavaScript. Dispute participants can find independent staff contacts.
  The server accepts encrypted content and never decrypts it; users encrypt with
  their own PGP client. This does not add server-side plaintext chat.

## Verification ledger

All applicable local checks below passed. GitHub Actions was updated to run the
new installer and wallet journeys, but no branch was pushed and no hosted CI run
is claimed. Logs are in ignored `artifacts/project-review/`; permanent commands
and fixtures are tracked in the repository.

| Check | Result |
| --- | --- |
| `go test -race -count=1 ./...` with PostgreSQL 17 | Passed all packages; market package coverage 88.4% |
| `go vet ./...`, `go mod verify`, formatting | Passed |
| Standard Playwright suite, real PostgreSQL + six preview browser/viewport configurations | 302 passed |
| Follow-up PostgreSQL browser workflows | 15 passed across two runs: 14 existing cases, then the added seeded catalog/role/order case after correcting its selector |
| `npm run test:e2e:wallet`, real app/DB + simulated production RPC adapters | 4 complete journeys passed in 42.2s: BTC/XMR partial/top-up/fulfillment/payout/reviews; funded dispute/encrypted staff contact/refund; paid cancellation/refund/stock restoration; automatic delivery/admin payout recovery |
| Python helper/installer/hook tests | 28 passed |
| `python3 scripts/test-install.py` | Real one-command Docker installation, wrong-token rejection, setup, onboarding, custom title, setup lockout, configuration preservation and restart persistence passed |
| Production Docker image, internal and external PostgreSQL | Startup, setup, admin session, stylesheets, unavailable-wallet handling and signed audit export passed |
| Upgrade from `v0.1.0-alpha.1` | Populated database, migration 052 included, preserved accounts/orders and restart passed |
| Encrypted backup/restore on PostgreSQL 17 | Unicode, permissions, overwrite refusal, wrong key, transactional rollback, all recovery holds, legacy settings-only restore, persistent recovery lock and app restart passed |
| Compose configurations | All six clearnet/Tor × database/mirror combinations passed |
| Tor image and offline configuration | Built and passed `--verify-config` with networking disabled |
| Dependency checks | govulncheck: no reachable vulnerability; npm audit: zero reported vulnerabilities |
| Workflow lint, shell syntax, local documentation links | Passed |

During review, independent simultaneous Go test processes initially shared one
database and contended on the watcher's database-wide advisory lock. Final full
race verification used a dedicated database, with focused follow-up verification
in another database. Parallel runs should use separate databases even though
each Go test cleans up its own schema.

## Source and regression map

- Recovery lock: [restore script](../scripts/restore.sh),
  [payout sending](../internal/market/payments_watcher.go), and
  [recovery regressions](../internal/market/review_recovery_integration_test.go).
- Identity/factor corrections: [PGP factor](../internal/market/pgp_factor.go),
  [confirmation transaction](../internal/market/actions_auth.go), and
  [identity regressions](../internal/market/review_identity_integration_test.go).
- Provider disappearance and deposit recovery:
  [payment regressions](../internal/market/review_payment_recovery_test.go).
- Vendor demotion: [listing creation](../internal/market/actions_listings.go) and
  [concurrency regressions](../internal/market/review_listing_demotion_test.go).
- Preserved delivery: [order template](../web/templates/pages/order.html) and
  [delivery regression](../internal/market/review_delivery_integration_test.go).
- Currency controls: [intake policy](../internal/market/payments_intake.go),
  [migration 052](../internal/market/migrations/052_payment_intake.sql), and
  [intake regressions](../internal/market/payments_intake_test.go).
- Partial deposits: [Go lifecycle matrix](../internal/market/review_partial_payment_integration_test.go)
  and [browser payment/dispute journeys](../tests/e2e/wallet-journey.spec.ts).
- Dispute contacts: [staff access regression](../internal/market/dispute_contacts_integration_test.go)
  and [encrypted-message browser tests](../tests/e2e/db-pgp-messages.spec.ts).
- Seeded discovery and role boundaries:
  [database browser journey](../tests/e2e/db-seeded-marketplace.spec.ts).

## Completion scrutiny

Separate final passes examined the finished payment/identity locking changes,
installer/recovery/CI behavior, and browser payment/dispute journeys. The lock
review caught a possible notification foreign-key cycle; `FOR NO KEY UPDATE`
now serializes factor changes without blocking notification references, with a
regression for both invariants. No required finding remains unresolved in the
reviewed scope. This is evidence of the checks performed, not a guarantee that
all possible defects or deployment risks have been eliminated.

## Boundaries of the evidence

Wallet-backed browser tests use local deterministic JSON-RPC fixtures with the
production adapters. They demonstrate application behavior, not live Bitcoin or
Monero network operation, node synchronization, real fees or broadcasts. Fake
wallet results must not be presented as a successful live deployment.

Tor configuration checks run offline; public onion reachability and identity
recovery are separate operational checks. Mainnet, multisig escrow, commission
accounting, server-side message decryption and provisioning nodes from the web
administrator are outside this implementation.

## Accessibility and seeded workflow follow-up

The screenshot review found an input focus ring crossing the search button, tiny
arrow glyphs, category selection that only highlighted “All listings,” and footer
content with mismatched alignment. The search now uses a labelled 20px magnifier
and inset keyboard focus; all categories share the active style and `aria-current`.
The footer separates identity, navigation and payment status, with a stacked mobile
layout. Action icons scale separately from their labels.

Typography uses relative units: 16px default body/input text and a 14px secondary
text floor. Cards, navigation and footer reflow with enlarged text; checkboxes and
radio controls are 24px, primary controls and navigation links at least 44px high.
These are project design choices, not a claim that WCAG sets a minimum font size.
The checks use [WCAG text contrast](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html),
[text resizing](https://www.w3.org/WAI/WCAG22/Understanding/resize-text.html), and
[target sizing](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum.html)
as criteria. Browser regressions sample text/control contrast, keyboard focus,
seeded category content and 200% text resizing. They are not a full accessibility
conformance certification or a substitute for assistive-technology user testing.

Follow-up browser evidence: full preview matrix **306 passed**, followed by **30
final checks** against the last footer/icon changes. The latter ran category,
search, contrast, text-resize, footer geometry and navigation checks across all
six browser/viewport configurations. A final category-indicator styling pass
also passed all 24 accessibility checks across the six configurations. `go test ./...` also passed after template
changes (without a database; the earlier database/race evidence is listed above).

The database follow-up used a dedicated disposable PostgreSQL 17 instance and
JavaScript-disabled Chromium. **14 existing cases passed**, covering setup,
accounts/sessions, TOTP, inventory, role administration, encrypted messaging,
staff contacts and unavailable-payment states. The new **seeded marketplace case
passed separately** after correcting an exact-label selector; this is 15 passing
cases across two runs, not a single clean 15-case run. It creates buyer, vendor
and moderator accounts through forms, then physical, digital, sold-out and
archived listings. It verifies mobile search, category/region/currency filtering,
public vendor listings, anonymous checkout redirection, role restrictions,
duplicate-draft persistence and cancellation visible to both order parties.

The expanded simulated-wallet suite passed **all four journeys in one fresh
database run (42.2s)**. In addition to deposits, top-ups, intake controls and
fulfillment, it verifies public purchase reviews without the buyer's handle or
order ID, independent dispute resolution, vendor cancellation of a paid order,
stock restoration and refund. Automatic digital content stays hidden before
funding and appears after confirmation. A deliberately rejected payout remains
failed without automatic retry; an administrator's wrong password is refused,
then an explicitly authorized requeue is audited and sent. These checks use the
real application, PostgreSQL and production adapters with a loopback RPC
simulator; they do not by themselves prove real-chain broadcasts or fees.

Remaining browser coverage gaps are explicit: successful PGP ownership proof and
PGP sign-in, administrator release of a held payout, and marking a payout sent
after wallet reconciliation are covered by Go integration tests rather than
these browser journeys. The standard database browser run exercises the
unconfigured audit-signing state; configured signed exports have separate
integration/operations evidence. The role and workflow checks above do not imply
that every possible input, interleaving or assistive-technology interaction has
been exercised.

## Real isolated-chain verification

The follow-up used signed, checksum-verified Bitcoin Core 31.1 and Monero
0.18.5.1 binaries, separate disposable PostgreSQL databases, real application
forms and locally mined test coins. The [reproducible guide](local-chain-testing.md)
explains setup, browser execution and shutdown. No public-network test funds or
existing wallets were available; the user selected isolated local chains.

| Check | Result |
| --- | --- |
| Bitcoin Core regtest, peer networking disabled | Real adapter smoke passed; browser journey passed in 18.5s |
| Monero offline stagenet fork, fixed difficulty, native mining | Browser journey passed in 1.7m after fixing Digest connection handling |
| Reusable node setup and shutdown | Fresh second state directory passed startup, wallet creation, mining, unlocked fee reserves and safe shutdown |

Each currency's browser journey creates administrator, buyer, vendor and moderator
accounts, publishes a listing, pays 40% then 60% to the same order address, mines
confirmations, fulfills and completes the order, and verifies a single payout in
the recipient wallet. A second funded order goes through an independent moderator
refund and a single recipient-wallet transaction. Bitcoin receipts plus the actual
network fee equal the order amount; Monero receipts equal the order amount, with
fees paid separately by the market wallet. The payment UI now explains this fee
behavior before requesting an address and when showing a payout.

The real Monero run exposed a defect absent from the simulated RPC suite: the
client closed a nonempty HTTP 401 response without draining it. Go discarded that
TCP connection, but Monero ties the Digest nonce to that connection, so correct
credentials were rejected on retry. The fix drains bounded challenge bodies and
serializes Digest exchanges with cancellation-aware waiting under the existing
10-second deadline. New [race regressions](../internal/market/payments_digest_session_test.go) model
connection-bound nonces, concurrent calls, reconnection and cancellation.
Independent review found no remaining required issue in this change. The complete
real Monero journey passed afterward.

Logs: ignored `artifacts/real-chain/btc-browser.log` and
`artifacts/real-chain/xmr-browser-final.log`. Runtime files contain private local
RPC credentials and must not be published. These results prove actual local
transactions, not public testnet synchronization, current public-network protocol
compatibility, real-world fees or mainnet support. The offline Monero fork starts
at early protocol heights; its limitation is documented in the guide.

Final verification after the Monero fix passed: `go test -race -count=1 ./...`
with a dedicated PostgreSQL database (market package 130.6s), `go vet ./...`, and
all **29 Python tests**. The new Digest regressions also passed 20 race-enabled
repetitions. A server-forced reconnect in the initial regression could race an
idle connection reset; the final test explicitly retires the client's idle
connection and verifies a new session, without adding unsafe RPC retries.

The parallel helper run also exposed installer-test cleanup defects: timed-out
shells could leave Python stub children writing bytecode into temporary HOME
directories. The harness now disables bytecode writes, terminates its dedicated
process group on timeout, and uses lightweight shell stubs for repeated health
polls. Its child-cleanup regression and full suite passed afterward; no production
installer behavior changed in this follow-up.

Independent completion reviews covered the authentication fix, browser assertions,
node setup/shutdown and evidence limits. Disposable databases and node processes
were removed/stopped after verification; ignored logs, reports and local chain
data remain available for inspection. No commit, push or deployment was made.

## Local node defaults

The installer now uses release-following `latest` defaults for Bitcoin Core and
Monero daemon + wallet RPC images when their image prompts are left blank. It
validates the optional override, then asks for a supported test network. The
Compose services preserve the selected images' entrypoints, data ownership and
restricted wallet/daemon RPC behavior. Live choices fail before `.env` is written
because payment adapters explicitly refuse mainnet. The operator guide records
image provenance, override/digest guidance, and the live-network limitation.
