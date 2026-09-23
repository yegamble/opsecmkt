# CI and private release delivery

Every branch push and pull request runs `.github/workflows/ci.yaml`. The stable **CI passed** check succeeds only when every mandatory job succeeds, including all matrix entries. Configure that check as required on the default branch; do not accept a skipped or cancelled suite as a pass.

The suite covers:

- Workflow linting, Go formatting, module integrity, vet, race detection, coverage and the real PostgreSQL marketplace integration flow.
- Reachable Go vulnerabilities with a pinned govulncheck version, plus high/critical npm dependency vulnerabilities. Node and Playwright are development tools only; the delivered application remains Go with server-rendered HTML/CSS.
- Shell syntax, installer selection/secret-file regressions and Python PostgreSQL helper regressions; encrypted backup and restore against disposable PostgreSQL 17 databases.
- Six supported Compose configurations spanning clearnet/Tor, internal/external PostgreSQL and Tor mirror enabled/disabled. Optional node and Monero wallet-RPC profiles are parsed, not connected to real cryptocurrency networks.
- Tor image build and offline configuration validation with networking disabled.
- Production Docker image startup with internal and external PostgreSQL; health, first-admin bootstrap, authenticated admin access and setup lockdown. External mode verifies the application Compose project did not start a database service.
- Playwright browser regressions against both the preview and a separate disposable PostgreSQL application (a `db-setup` project initializes the database once; `database-chromium` runs every other `db-*.spec.ts` after it). The browser suite checks mobile/tablet/desktop geometry, style, behavior and reviewed ARIA snapshots across Chromium, Firefox and WebKit.

GitHub retains coverage and browser reports/traces for 14 days. Inspect failed Actions jobs and the `playwright-results` artifact before updating a regression expectation. Runtime smoke tests isolate containers, volumes and networks with a unique project name and ignore local `.env` files.

## Release a tested version

Push a version tag, for example `v0.1.0`, on the intended reviewed commit. The private release workflow reruns the entire CI workflow against that tag before building any release output. It requires the repository to be private and the tag to use `vMAJOR.MINOR.PATCH` (an optional prerelease suffix is supported).

The pipeline creates a **draft private GitHub release** with:

- `opsecmkt.oci.tar.gz`: Linux amd64 OCI image archive, with embedded BuildKit provenance and SBOM.
- `build.json`: exact source commit, tag, platform and Actions run URL.
- `SHA256SUMS`: checksums for the archive and build record.

It does not publish a registry package, contact a production host, or deploy. There are no deployment credentials to configure. Review the draft and its green checks before publishing it to repository readers. A pre-existing release makes a rerun fail instead of silently replacing release assets; delete an unwanted draft explicitly before retrying packaging.

To retrieve and inspect a release on an authorized workstation:

```sh
gh release download v0.1.0 --repo OWNER/opsecmkt --dir release-v0.1.0
cd release-v0.1.0
sha256sum -c SHA256SUMS
gunzip -k opsecmkt.oci.tar.gz
skopeo copy oci-archive:opsecmkt.oci.tar docker-daemon:opsecmkt:v0.1.0
```

The OCI archive is intended for an OCI-aware importer; do not assume every Docker version can load it directly. Review embedded metadata in the archive before import; copying to a Docker daemon can discard attestations. The checksums detect corruption and BuildKit metadata records the build; neither is an independent cryptographic signature. GitHub signed artifact attestations for private repositories require Enterprise Cloud and are not assumed here.

A future deployment workflow needs an explicitly selected target, environment protection, deployment credentials, backup policy, and rollback decision. For rollback, keep the previously tested private image release and a verified encrypted database backup. Do not restore a database simply to revert application code without reviewing schema compatibility and data loss implications.

## Workflow maintenance

Actions are pinned to verified full commit SHAs, permissions default to read-only, and checkout does not persist credentials. Only the release packaging job receives `contents: write`. Pull requests never receive release permissions or production secrets. Dependabot proposes dependency/action updates for review.

CI uses disposable PostgreSQL credentials that are intentionally committed test fixtures. Never substitute production URLs or credentials in these jobs. Do not commit local environment files, backups, browser authentication state, or private test traces. Browser artifacts can contain the generated fixture records used in CI.

Sources: [GitHub secure-use reference](https://docs.github.com/en/actions/reference/security/secure-use), [private artifact attestation requirements](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations), and [Docker build attestations](https://docs.docker.com/build/metadata/attestations/).
