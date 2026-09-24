# CI and release delivery

Every pull request and every push to `main` runs `.github/workflows/ci.yaml`; other branches are tested through their pull request, or on demand with **Run workflow** (`workflow_dispatch`). The stable **CI passed** check succeeds only when every mandatory job succeeds, including all matrix entries. Branch protection on `main` requires that check; do not accept a skipped or cancelled suite as a pass.

The suite covers:

- Workflow linting, Go formatting, module integrity, vet, race detection and coverage. PostgreSQL integration tests cover schema migrations, the order state machine, every feature package (TOTP/CAPTCHA, PGP, orders, inventory, payments, canary/audit export) and one cross-package lifecycle (`lifecycle_integration_test.go`: listing → payment → automatic delivery → completion → release payout → review, and dispute → refund payout). Payments are exercised against an in-process fake wallet and the Bitcoin/Monero adapters against local HTTP fakes; no real node or wallet is contacted.
- Reachable Go vulnerabilities with a pinned govulncheck version, plus high/critical npm dependency vulnerabilities. Node and Playwright are development tools only; the delivered application remains Go with server-rendered HTML/CSS.
- Shell syntax, installer selection/secret-file regressions and Python PostgreSQL helper regressions; encrypted backup and restore against disposable PostgreSQL 17 databases, both for synthetic tables and for the real application schema (migrated by the server, restored into an empty database with its queued payout held, then served again by the server), and backup and restore through the internal-db Compose service, which publishes no database port (`scripts/test-internal-db-restore.sh`: side-by-side and fresh-volume restores with the payout gate). See [operations-tests.md](operations-tests.md).
- Upgrade from `v0.1.0-alpha.1` (`scripts/test-upgrade.sh`): the alpha.1 server, built from its tag, populates a PostgreSQL 17 database through its own forms; the current server then migrates it. Health, every migration recorded once, orders moved to `draft` with the old status column gone, the one-draft-per-buyer/product/currency rule (database and order form), preserved messages/users/settings and alpha.1 sign-in are checked, and a second start applies nothing.
- Six supported Compose configurations spanning clearnet/Tor, internal/external PostgreSQL and Tor mirror enabled/disabled. Optional node and Monero wallet-RPC profiles are parsed, not connected to real cryptocurrency networks. In the Tor rows the resolved configuration (`config --format json`) must give `tor`, and `tor-mirror` when the mirror is on, a `service_started` dependency on the app with `restart: true`, so Tor restarts whenever Compose recreates the app. In every row the resolved `bitcoin` command must contain `-datadir=/data` and `-prune=<n>` with `n` at least 550, and the `monero` command `--prune-blockchain --sync-pruned-blocks` (the pruned defaults); with `BITCOIN_PRUNE_MB=0` and a blank `MONERO_PRUNE_FLAGS` the same check requires `-prune=0` and no Monero prune flag (full nodes). Those rows pass `--profile` flags explicitly, so the Tor rows with the mirror on also run `scripts/test-mirror-commands.sh` for their database: it writes `COMPOSE_FILE` and `COMPOSE_PROFILES` into `.env` as the installer does and checks every documented onion-mirror command against it (see [operations-tests.md](operations-tests.md#documented-onion-mirror-commands)). This checks the configuration only; no Compose stack is started.
- Tor image build and offline configuration validation with networking disabled.
- Production Docker image startup with internal and external PostgreSQL (`scripts/ci-smoke.sh`); health, first-admin bootstrap, authenticated admin access and setup lockdown. External mode verifies the application Compose project did not start a database service. The image runs with `AUDIT_SIGNING_KEY` set and a `BITCOIN_RPC_URL` (testnet4) that nothing answers: the site must stay healthy without restarting, the admin page must show Bitcoin as unavailable and the signed audit export as available with the pinned key, and a downloaded export must pass `cmd/verify-audit` (a tampered copy must fail). Every stylesheet the layout links must be served as `text/css` with this checkout's bytes.
- Playwright browser regressions against both the preview and a separate disposable PostgreSQL application. Every `preview*.spec.ts` (layout, auth, PGP, orders, inventory, payments, transparency) runs in six Chromium/Firefox/WebKit viewport projects; a `db-setup` project initializes the database once, then `database-chromium` runs every other `db-*.spec.ts` (account, TOTP sign-in and recovery, orders, inventory, payments disabled without a provider). `npm run test:e2e` runs all projects, so new specs matching those names are picked up without workflow changes. The browser suite checks mobile/tablet/desktop geometry, style, behavior and reviewed ARIA snapshots.

The browser job also runs `npm run test:e2e:wallet` against a separate fresh database and loopback JSON-RPC simulators, exercising the production Bitcoin/Monero adapters through partial deposits, top-ups, intake controls, fulfillment and single payouts. This is application integration evidence, not a live-chain test.

GitHub retains coverage and browser reports/traces for 14 days. Inspect failed Actions jobs and the `playwright-results` artifact before updating a regression expectation. Runtime smoke tests isolate containers, volumes and networks with a unique project name and ignore local `.env` files.

## Release a tested version

Push a version tag, for example `v0.1.0`, on the intended reviewed commit. The release workflow reruns the entire CI workflow against that tag before building any release output. It requires the tag to use `vMAJOR.MINOR.PATCH` (an optional prerelease suffix is supported). The image is built without any Actions cache.

The pipeline creates a **draft GitHub release** with:

- `opsecmkt.oci.tar.gz`: Linux amd64 OCI image archive, with embedded BuildKit provenance and SBOM.
- `build.json`: exact source commit, tag, platform and Actions run URL.
- `verify-audit-linux-amd64`, `verify-audit-linux-arm64`, `verify-audit-darwin-amd64`, `verify-audit-darwin-arm64`, `verify-audit-windows-amd64.exe`: static builds of `cmd/verify-audit`, the offline checker for signed audit exports (standard library only, no network access).
- `SHA256SUMS`: checksums for the archive, build record and every verifier binary.

It does not publish a registry package, contact a production host, or deploy. There are no deployment credentials to configure. A draft is visible only to people with write access to the repository. The repository is public, so publishing a release makes its assets available to anyone: review the draft and its green checks first. A pre-existing release makes a rerun fail instead of silently replacing release assets; delete an unwanted draft explicitly before retrying packaging.

To retrieve and inspect a release on an authorized workstation:

```sh
gh release download v0.1.0 --repo OWNER/opsecmkt --dir release-v0.1.0
cd release-v0.1.0
sha256sum -c SHA256SUMS
gunzip -k opsecmkt.oci.tar.gz
skopeo copy oci-archive:opsecmkt.oci.tar docker-daemon:opsecmkt:v0.1.0
```

To check a signed audit export downloaded from **Admin → Signed audit export**, verify the release checksums as above, then run the verifier for your platform with the Ed25519 public key you pinned from the market's `/canary` page (not the key named inside the `.sig` file):

```sh
chmod +x verify-audit-linux-amd64
./verify-audit-linux-amd64 -pub <pinned-hex-key> audit-events-upto-N.jsonl audit-events-upto-N.sig
```

It exits 0 only when the signature over the exact export bytes verifies with that key and every line is a well-formed event in ascending id order; otherwise it prints the reason and exits 1. From a source checkout, `go run ./cmd/verify-audit` is equivalent. A valid signature proves the export came from the holder of the signing key; it does not prove the history was never altered on the server before export.

The OCI archive is intended for an OCI-aware importer; do not assume every Docker version can load it directly. Review embedded metadata in the archive before import; copying to a Docker daemon can discard attestations. The checksums detect corruption and BuildKit metadata records the build; neither is an independent cryptographic signature. The workflow does not create GitHub artifact attestations (available to public repositories via `actions/attest-build-provenance`).

A future deployment workflow needs an explicitly selected target, environment protection, deployment credentials, backup policy, and rollback decision. Database migrations are one-way (there are no down-migrations, and migration 001 drops `orders.status`), so an older image cannot simply be redeployed against a migrated database: rolling back means restoring the encrypted backup taken before the upgrade into a fresh database and rebuilding the previous tagged revision against it (Compose builds the app from the checkout), losing everything written since. Keep the previous release tag, its `.env` and that verified pre-upgrade backup until the new release is trusted. See [UPGRADING.md](../UPGRADING.md).

## Workflow maintenance

Actions are pinned to verified full commit SHAs, permissions default to read-only, and checkout does not persist credentials. Only the release packaging job receives `contents: write`. Pull requests never receive release permissions or production secrets. Dependabot proposes dependency/action updates for review.

CI uses disposable PostgreSQL credentials that are intentionally committed test fixtures. Never substitute production URLs or credentials in these jobs. Do not commit local environment files, backups, browser authentication state, or private test traces. Browser artifacts can contain the generated fixture records used in CI.

Sources: [GitHub secure-use reference](https://docs.github.com/en/actions/reference/security/secure-use), [artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations), and [Docker build attestations](https://docs.docker.com/build/metadata/attestations/).
