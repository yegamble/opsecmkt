# Bug-hunt house rules and known false positives

Every hunt agent reads this before reporting. It is how the hunt learns: when
the verifier rejects a finding, the chair adds the verifier's proposed rule
here (with the date and the finding it came from). A finding that matches a
known false positive is dropped, not re-argued. Remove a rule only when the
design it records changes.

## House rules

- Invariants in `.claude/war-room/repo-map.md` are binding. A change that needs
  browser JavaScript or touches mainnet is always a finding.
- Report only what you ran or can point to at `file:line`. A failing test or a
  replayable transcript beats an argument; without one, the verdict is at
  best PLAUSIBLE.
- Name the evidence tier. A fake provider or the wallet-RPC simulator is never
  chain evidence; preview specs prove layout only.
- No style notes, refactors or "consider adding" items. A bug is an observable
  wrong result for some input or state.
- Do not re-report open ledger rows (`.claude/war-room/ledger.md`); cite the
  ID if you have new evidence.

## Known false positives (intentional design — do not report)

- **Payout sends are single-attempt.** A failed or ambiguous send stays failed
  until an administrator acts; the watcher never retries (invariant 4).
- **The restore gate (`payments_recovery_required`) is cleared only with SQL
  by the operator**; clearing it is an operator assertion that reconciliation
  is complete (`docs/testnet-runbook.md`, reconcile section), so no web unlock
  exists by design.
- **No password reset for users, and no in-app administrator lockout
  recovery.** With no email channel, a reset path is a takeover path; the
  operator break-glass SQL in `docs/operator-guide.md` is deliberate.
- **Disputes have exactly two outcomes** (release to vendor, refund to buyer):
  `payouts.order_id` is unique, so split outcomes would need a second payout.
- **Preview mode refuses every POST** and shows labelled sample data.
- **The server never decrypts messages and never stores shipping addresses**;
  plaintext messages are labelled plaintext, not refused.
- **Admin "mark sent" is the only exit for some held payouts** (ledger D-1).
- **Tor onion reachability is environment evidence**, not a code bug (D-2).
- **A moderator who is party to an order cannot resolve its dispute**; the form
  shows "Unavailable" by design.

## Rules learned from rejected findings

<!-- Append: YYYY-MM-DD — <rule> — (from <finding id>, rejected by opsec-bug-verifier) -->
- 2026-09-23 — Archiving hides the listing, not its review history: completed-order reviews (with the product title) stay on the vendor page, so archiving cannot launder reputation. — (from HUNT-C-5, rejected by opsec-bug-verifier)
- 2026-09-23 — Handles are exact, case-sensitive identifiers; case-variant or look-alike handles are not a bug — identity is the PGP key and fingerprint shown on vendor, order and message pages. — (from HUNT-D-3, rejected by opsec-bug-verifier)
- 2026-09-23 — Row caps already documented in `docs/implementation-status.md` (orders page, admin order list, messages, notifications) are ledger A-16, not new findings; report only a cap that hides open work the docs do not mention. — (from HUNT-C-1, DUPLICATE A-16)
