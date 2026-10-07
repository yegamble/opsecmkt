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
- **Test networks only.** Never add a code path that accepts mainnet chains or
  addresses.
- **Migrations are append-only.** Add a new file in `internal/market/migrations/`;
  never rewrite a merged one.
- **Test first.** Add or change the Go test, database test or browser test that
  proves the behaviour, and run the gates below before opening a pull request.
- **Say what you verified.** In the pull request, list the commands you ran and
  their results. A skipped database or browser suite is reported as skipped,
  never as passed.

## Gates

```sh
go mod verify
"$(go env GOROOT)/bin/gofmt" -l cmd internal   # must print nothing
go vet ./...
TEST_DATABASE_URL=… go test -race -count=1 -timeout 30m -run '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
TEST_DATABASE_URL=… go test -race -count=1 -timeout 30m -skip '^(TestEscrow|TestXMR|FuzzEscrow)' ./...
npm run test:e2e                              # with E2E_DATABASE_URL for database journeys
npm run test:e2e:escrow                       # with E2E_ESCROW_DATABASE_URL (own fresh database); CI job browser-escrow
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
