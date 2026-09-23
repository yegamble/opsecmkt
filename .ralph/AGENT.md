# Build and verification

Run from the repository root. README.md contains copyable database setup,
preview, fixture-seeding and test commands. Do not read or print private .env
values into logs or reports.

- Go 1.26.8+, PostgreSQL 17+, Python 3; Node 22+ only for browser tests.
- Go changes: gofmt changed files, go mod verify, go vet ./..., then
  `TEST_DATABASE_URL=<dedicated-test-db> go test -race -count=1 ./...`.
- Templates/CSS or user journeys: npm ci, install Playwright browsers, and
  E2E_DATABASE_URL=<fresh-disposable-db> npm run test:e2e. No browser JavaScript.
- Python/hooks/installer helpers: python3 -m unittest discover -s tests -p 'test_*.py' -v.
- Shell: bash -n on changed scripts. Deployment/backup changes: follow
  docs/operations-tests.md and .github/workflows/ci.yaml for affected checks.
- Dependencies: CI also runs govulncheck and npm audit; follow its pinned commands.
- Documentation/config-only changes: validate examples, local links, JSON and
  shell syntax; test hook control flow if changed. Do not claim unrun CI gates.

Database tests create and clean isolated schemas. Concurrent test processes must
use separate databases because watcher advisory locks are database-wide. Browser database setup needs a
fresh database on each run and leaves fixtures behind. Preview is read-only and
never seeds PostgreSQL. Never use a real deployment database for any test.

Payment tests use fake RPC providers; real regtest and deployment checks are
separate evidence. Never configure mainnet or claim multisig escrow. Immutable
merged migrations stay unchanged; add numbered migrations for schema changes.
