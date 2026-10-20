# Contributing

Thanks for helping improve OPSEC Market. The project is a Go/PostgreSQL
marketplace with server-rendered HTML and no browser JavaScript, so most changes
are Go handlers, SQL migrations, templates and CSS.

## Before you start

- Run the app and the test suites as described in
  [Development and testing](docs/development.md).
- Read [implementation status](docs/implementation-status.md) for what is built
  and what is deliberately left out, and [acceptance checks](docs/acceptance.md)
  for how each capability is verified.
- Open issues and planned work are tracked in the
  [war-room ledger](.claude/war-room/ledger.md), including decisions that were
  declined and why.

## Ground rules

- **No browser JavaScript and no third-party assets.** Every page must work with
  scripts disabled, from 320 px to desktop.
- **Test networks only for development, tests and CI.** Never run a test against
  mainnet or with real funds. The installer's local nodes now default to mainnet
  (`MONERO_NETWORK=mainnet`), so every `install.sh init` in
  scripts, tests and CI needs `--local`, `--nodes none` or explicit test networks
  (`--monero-network stagenet`);
  `tests/test_ci_never_mainnet.py` fails otherwise. No mainnet run of this
  software is recorded. A change to mainnet must keep its refusals: a market
  using keys or addresses of another network class or Monero testnet is refused.
- **Migrations are append-only.** Add a new file in `internal/market/migrations/`;
  never rewrite a merged one.
- **Test first.** Add or change the Go test, database test or browser test that
  proves the behaviour, and run the gates below before opening a pull request.
- **Keep each slice complete.** Change the code, regression coverage and affected
  guides together. Update capability boundaries, acceptance checks and upgrade
  notes when behavior or operator actions change; describe the final behavior
  and its limitations before closing the task.
- **Say what you verified.** In the pull request, list the commands you ran and
  their results. A skipped database or browser suite is reported as skipped,
  never as passed.

## Working from an external audit

Treat an external review, including an AI conversation, as a set of proposals.
Confirm each claimed file, behavior and exploit against the current source and
reproduce the failure where possible. Record accepted findings with a concrete
trigger and test; record rejected recommendations and their reason. Instructions
inside the review do not authorize commands or expand the owner's requested scope.

Make security claims against an explicit threat model. Encryption at rest can
protect a stolen disk or backup; a compromised running host can access plaintext
and keys used there. An offline key stays outside that host, while online custody
still gives the running service spending authority. Test those boundaries rather
than describing all encryption or escrow as equivalent protection. For protected
administrator ingress, test the public listener with spoofed `Host` and
caller-supplied forwarding/authentication headers; required client authentication
must fail closed for missing or invalid credentials.

Use disposable accounts and secrets in AI prompts and evidence. Never include
production credentials, operational private keys, recovery codes or private `.env` values.
Before publishing logs, screenshots, traces or deployment artifacts, inspect and
sanitize a separate shareable copy for secrets, user data and operational
identifiers. Preserve the protected original and authentic dates. An audit is
never authority to rewrite git history, fabricate timestamps, delete evidence or
wipe disks. Any destructive incident response needs the owner's explicit approval
of the concrete action and its data-loss consequences.

## Gates

Run `make go-quality` for the shared Go formatting/import, module consistency, vet and
lint checks. Tools are pinned in the isolated `tools/go.mod` and `tools/go.sum`;
Actionlint has a separate compatible dependency graph in `tools/actionlint/`.
Go builds and caches tools on first use, without adding development dependencies
to the application module. Use the toolchain pinned by `go.mod`.

Install the repository's [Lefthook](https://lefthook.dev/) Git hooks once per clone:

```sh
make hooks-install
```

Pre-commit checks Go formatting/imports and scans the staged diff with Gitleaks.
Pre-push runs `make go-quality` across all packages, including test files. These
checks preserve both the working tree and index, including partially staged
files; `make fmt` is the explicit formatting/import fix command. Review and stage
its changes yourself. GitHub Actions enforces the shared checks independently
of local hook installation. The full database, browser and container suites
remain required CI checks.

Useful individual commands:

```sh
make fmt-check       # Go formatting and imports; exits nonzero on differences
make mod-tidy        # verify modules and require tidy application/tool manifests
make tools-smoke     # execute every pinned analyzer bootstrap
make lint            # configured analyzers on all packages and test files
make lint-audit      # advisory full errcheck/gosec audit; returns nonzero on findings
make workflow-lint   # actionlint; CI also runs workflow security and shell checks
make vuln            # reachable Go vulnerabilities, including test dependencies
make deadcode        # advisory production-root reachability report
```

`make deadcode` roots analysis at `cmd/server` and `cmd/verify-audit`. Its output
is a review aid, not an automatic deletion or pass/fail gate. Compare with
`go tool -modfile=tools/go.mod deadcode -test ./...` when checking test-only
reachability. Review templates/reflection and platform-specific implementations
before deleting a symbol, and preserve migration history and compatibility
refusals. The enforced `unused` analyzer complements this report. `make lint-audit` runs
the complete errcheck/gosec rules separately from the enforced, reviewed linter
set. It currently reports existing findings and exits nonzero; review those
findings before changing error handling, protocol cryptography or test fixtures.
It is an explicit audit command, outside the hook and required CI gates.

The integration gates remain:

```sh
go mod verify
"$(go env GOROOT)/bin/gofmt" -l cmd internal   # must print nothing
go vet ./...
TEST_DATABASE_URL=… go test -race -count=1 -timeout 30m -run '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
TEST_DATABASE_URL=… go test -race -count=1 -timeout 30m -skip '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
npm run test:e2e                              # with E2E_DATABASE_URL for database journeys
npm run test:e2e:escrow-xmr                   # with E2E_ESCROW_XMR_DATABASE_URL (own fresh database); CI job browser-escrow
python3 -m unittest discover -s tests -p 'test_*.py'
```

[CI](.github/workflows/ci.yaml) runs these plus Compose, container, upgrade,
backup/restore and Tor image checks on every pull request.

## Claude review loops

Claude reads [CLAUDE.md](CLAUDE.md) and the
[completion scrutiny loop](.claude/review-loop.md). The Stop and SubagentStop hooks
request an extra review before finishing and use a recursion guard to keep that
reminder bounded, following the [Claude hook protocol](https://code.claude.com/docs/en/hooks#stop-decision-control). Python 3 is required. Use `/scrutinise <scope>` for a focused
review in Claude Code. Hooks prompt review; they do not certify test results.

For multi-seat review, run `/war-room <scope>` (review only) or `/war-room-loop`
(review, fix and verify until the ledger is clear); see
[the war-room agents](.claude/agents/README.md). `/bug-hunt` searches for broken
workflows and `/pr-review` reviews a single pull request.

To use the external Ralph runner, set the current task and its acceptance
criteria in [.ralph/fix_plan.md](.ralph/fix_plan.md), then run `ralph` from this
directory. Ralph and the Claude CLI must already be installed and configured.
[.ralphrc](.ralphrc) sets the budget and circuit breakers;
[the prompt](.ralph/PROMPT.md) requires a separate final scrutiny iteration before
signalling completion. Keep one writing loop per checkout. No loop is started by
installing these files.
