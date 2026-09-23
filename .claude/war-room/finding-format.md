# War-room finding format — every seat uses exactly this

One block per finding. No prose reports. The shared shape lets a payments
engineer and a buyer advocate argue about the same object.

```
FINDING <seat>-<n>: <one-line title>
Severity:    BLOCKER | REQUIRED | SHOULD | EXPERIMENT | NIT
Confidence:  high | medium | low
Area:        identity | commerce | payments | messaging | transparency | ops | ui | tests

Affected:
  files:     path:line, path:line — real paths you opened

Observed:
  Concrete evidence: quote the line or name the symbol. Anything you could not
  check is written "UNVERIFIED: <what and why>", never asserted.

Failure:
  What is wrong, missing or incomplete — and what actually happens as a result.

Perspective:
  buyer | vendor | moderator | administrator | operator | attacker | developer

Recommendation:
  The smallest coherent change that closes the failure. Not the ideal design.

Acceptance criteria:
  Observable conditions someone else can check without reading your report.

Tests:
  Named file and case in the repo's idiom: Go DB integration via newTestApp /
  newPayEnv (testenv_test.go, payments_watcher_integration_test.go); fake
  provider (payments_fake.go) or httptest JSON-RPC for adapters; Playwright
  preview-*.spec.ts / db-*.spec.ts / wallet-journey.spec.ts; Python unittest in
  tests/; scripts/test-*.sh for operations.

Evidence tier:
  The strongest tier that currently proves this area (see repo-map.md).

Challenge:
  The strongest counterargument to your own finding.
```

## Severity (shared, non-negotiable)

- **BLOCKER** — do not release: funds or payout safety, data loss, an
  authorization/privacy bypass, a leak that deanonymises users, an operator
  cannot recover, browser JavaScript required, or a headline workflow unusable.
- **REQUIRED** — the product is incoherent without it: a capability with no
  reachable form, a form with no working handler, a job that needs SSH + SQL,
  a documented claim the code does not honour.
- **SHOULD** — a real improvement with a real cost; worth scheduling.
- **EXPERIMENT** — plausible, unproven; name the measurement that settles it.
- **NIT** — correct but small. At most three per seat.

## Rules of evidence

1. Cite files you opened. No `path:line` → `Confidence: low`, say so.
2. Distinguish implemented / merged / released / deployed.
3. Name the evidence tier; never cite a fake-provider test as chain evidence.
4. If two readings are possible, give both and pick one.
5. Never report a test result you did not run in this session.
