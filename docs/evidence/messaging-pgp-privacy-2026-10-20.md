# Messaging, PGP and vendor privacy review — 2026-10-20

Scope: [PR #104](https://github.com/yegamble/opsecmkt/pull/104). The owner authorized implementation, commit, push and merge after tests and war-room scrutiny.

## Evidence and review method

The chair assembled source, test, documentation and ledger evidence before independent security, privacy and Tor/operations review. Seats completed blind review, cross-examination and rebuttal. A separate QA/release seat checks the integrated result. Reviewers used the available Codex agents; the repository's Claude-specific model bindings were unavailable. The managed execution environment failed provisioning, so source edits used immutable GitHub blobs, with only the chair updating the isolated branch. Local execution is unverified. Runtime evidence comes from GitHub Actions.

Authentication sources: `internal/market/pgp_access.go`, `pgp_actions.go`, `pgp_factor.go`, `totp_actions.go`, `actions_auth.go`, `admin_factors.go`, `cmd/server`, migrations 129–130. Messaging: `actions_messages.go`, `contacts.go`, `load.go`, `types.go`. Commerce and obligations: `actions_orders.go`, `orders_actions.go`, `orders_load.go`, `escrow_readiness.go`. Vendor privacy: `vendor_availability.go`, `vendor_applications.go`, `image_routes.go`, `image_pages.go`, `retention.go`. Account, registration, PGP, vendor, product, checkout, message and order templates provide server-rendered flows.

The existing private-administrator ingress stays in force. Challenges name the configured official addresses and account; private administration addresses are not published. Database, simulated-wallet, browser and fakechain tests do not establish public-Tor, deployed-host or real-funds behavior.

## Round D: A-616 — live contact authorization and necessary order replies

DECISION: ACCEPT security findings SEC1–SEC3.
WHY: A stale sender role could retain access to private customers after demotion. Separate eligibility and key reads could expose a key after the recipient became private. Conversely, a blanket retention restriction denied necessary communication on financially open orders.
DISSENTING VIEW: Applying retention exclusion to every vendor contact was simpler but would obstruct existing obligations; the exception must not expose unrelated buyers or historical contacts.
USER IMPACT: Buyers and vendors retain communication for their own unresolved financial orders, while strangers and demoted staff cannot retrieve private contact keys. Away-message refusal still wins.
OPERATOR IMPACT: No new service or configuration.
TECHNICAL COST/RISK: Short transactions share-lock current sender and recipient rows for authorization and key retrieval. Lock contention is refused; transactions end before rendering.
ACCEPTANCE CRITERIA: Current database role governs access; key retrieval shares its authorization transaction; public vendor and dispute staff loaders recheck eligibility; restricted vendors are reachable only by actual buyers of financially open orders, and only when their away-message preference allows it.
TEST PLAN: `message_role_race_test.go` covers stale role snapshots, denied sends without writes, sender/recipient lock contention, public-profile key checks, existing-order keys, restricted-vendor replies and staff demotion.

## Round D: A-617 — withdrawing activity sharing without a usable key

DECISION: ACCEPT.
WHY: An expired key prevented a vendor from turning off last-seen sharing while keeping the same availability mode.
DISSENTING VIEW: Requiring a usable key for every settings save enforced a uniform vendor rule, but privacy withdrawal must remain possible.
USER IMPACT: Vendors can stop sharing activity and reduce availability even when their key needs repair.
OPERATOR IMPACT: No configuration change.
TECHNICAL COST/RISK: Require a usable key only when resuming active business or publishing a hidden storefront; purchases independently enforce key usability.
ACCEPTANCE CRITERIA: Same-mode privacy changes and availability reductions work without a usable key; reopening business requires one. Opt-out erases the saved last-seen time; restricted, closed and suspended accounts do not refresh it.
TEST PLAN: `vendor_availability_test.go` and `vendor_retention_activity_test.go`.

## Round D: A-618 — protect established PGP identity and recover explicitly

DECISION: MODIFY and ACCEPT Tor/security finding; block merge until implemented and tested.
WHY: A stolen password could replace the saved public key and then decrypt with the attacker's key. Allowing the same password to enroll a first authenticator would bypass a TOTP exception.
DISSENTING VIEW: Mandatory MFA for all accounts was considered excessive. A usable-key-only guard was rejected because expiry or unreadable packets would weaken an established identity. A timed account-wide recovery grant was rejected because a password holder could race its consumption.
USER IMPACT: Changing or removing any nonempty PGP binding requires current-session old-key decryption or fresh confirmation with an authenticator already enrolled before the operation. First authenticator enrollment, activation and access to pending MFA/recovery material on a bound account require old-key proof. First key enrollment clears inactive MFA pending material; migration clears legacy pending material on keyed accounts with MFA off. First key enrollment and safe same-fingerprint renewal remain possible; unsafe renewal is a key change. Purchases and messages always need decryption with the saved current key.
OPERATOR IMPACT: Verify identity through a trusted recovery channel before directly replacing a lost public key in the existing privileged reset. Web reset still refuses administrator targets. The existing host reset accepts an optional public-key file for last-administrator recovery, without relaxing strict login.
TECHNICAL COST/RISK: Recovery rejects private, mixed private-subkey and extra/photo packets and verifies the canonical public key can receive encryption. It accepts a bounded usable public key only, invalidates sessions, pending sign-ins and challenges, advances key generation and leaves ownership unverified. No private key, timed recovery grant or global policy bypass.
ACCEPTANCE CRITERIA: Password-only replacement/removal and first-TOTP bootstrap fail for valid, expired and unreadable saved bindings; prior TOTP and current-key proof permit confirmed changes; safe renewal and first enrollment still work. Explicit privileged recovery works even with both optional factors off and preserves strict policy; replacement must pass real decryption.
TEST PLAN: Auth key-binding/TOTP regressions, privileged-reset tests and integrated strict administrator host recovery; full Go/PostgreSQL race, browser, upgrade and operational CI.

## Integration and evidence limits

The first CI runs found fixture assumptions that predated mandatory PGP access. Corrections explicitly enroll/prove commerce fixtures, preserve keyless negative cases and keep refusal, side-effect and authorization assertions. No global request-helper bypass, test timeout increase or skipped required job is accepted. Source guards remain unchanged.

At creation of this record, CI run [39458740871](https://github.com/yegamble/opsecmkt/actions/runs/39458740871) tests commit `a630ba410aa944fb5d709f65c50313d5ea0440af`; A-618 integration and the final gate are pending. The final exact-head CI result and reviewer verification must be recorded in the PR before merge. This record makes no deployment or live Tor claim.

A-577 is amended to the owner's manual vacation/deactivation design; no automatic expiry or public note was requested. A-582's older ownership-verification proposal remains separate. A-608 closes only with integrated passing forbidden-keyless-recipient coverage. Unrelated A-575, A-578, A-609, A-615 and operator/mainnet prerequisites A-555/A-562 remain open.
