# CI and release delivery

Every pull request and every push to `main` runs `.github/workflows/ci.yaml`; other branches are tested through their pull request, or on demand with **Run workflow** (`workflow_dispatch`). The stable **CI passed** check succeeds only when every mandatory job succeeds, including all matrix entries. Branch protection on `main` requires that check; do not accept a skipped or cancelled suite as a pass.

The current Monero-only suite is configured to cover:

- Go formatting, module integrity, vet, vulnerability checks and race tests against PostgreSQL. Two complementary jobs select and skip `^(TestEscrow|TestXMR|FuzzEscrow)` so every top-level test runs once across the pair.
- Monero escrow against real daemon and wallet-rpc binaries on a local fakechain (`escrow-xmr-regtest`). This uses test coins and establishes no public-network or deployment result.
- Server-rendered preview and database browser journeys with JavaScript disabled. Monero escrow browser journeys use simulated RPC, not a live chain.
- Installer, Compose network isolation, resource limits, Tor configuration, scanner, encrypted backup/restore, and upgrade smoke checks.
- Required-job aggregation through **CI passed**. Historical Bitcoin jobs and signer builds have been removed.

Each parallel test process needs its own disposable database. No development or CI job uses mainnet. Preserve a failed run's sanitized logs and browser traces; do not change expectations until the failure is understood. See [development commands](development.md), [operations checks](operations-tests.md) and the dated [verification records](verification.md).

## Release a tested version

Push a version tag, for example `v0.1.0`, on the intended reviewed commit. The release workflow reruns the entire CI workflow against that tag before building any release output. It requires the tag to use `vMAJOR.MINOR.PATCH` (an optional prerelease suffix is supported). A tag with a suffix (any `-` in the name, such as `v0.1.0-alpha.2`) is created with `gh release create --prerelease`; a plain tag is not. The image is built without any Actions cache.

The pipeline creates a **draft GitHub release** with:

- `opsecmkt.oci.tar.gz`: Linux amd64 OCI image archive, with embedded BuildKit provenance and SBOM.
- `build.json`: exact source commit, tag, platform and Actions run URL.
- `opsecmkt-linux-amd64` and `opsecmkt-linux-arm64`: native application/operator
  binaries built from `cmd/server`, including public UI assets, key management,
  the upload gateway and the separate dependency-version worker. Follow
  [Binary installation](install/binary.md); Docker services still need reviewed
  image digests. These binaries are included in `SHA256SUMS`.
- `verify-audit-linux-amd64`, `verify-audit-linux-arm64`, `verify-audit-darwin-amd64`, `verify-audit-darwin-arm64`, `verify-audit-windows-amd64.exe`: static builds of `cmd/verify-audit`, the offline checker for signed audit exports (standard library only, no network access).
- `SHA256SUMS`: checksums for the archive, build record and every verifier binary.

The release notes open with `Pre-release. Test networks only. No real funds. Install: git checkout <tag>; see UPGRADING.md.` (the workflow writes the actual tag). They then state the reproducibility limits: the OCI archive is for inspection and registry import; scripts/install.sh builds the application from source on your host and is not bit-reproducible, so the archive is not what the installer runs; the verify-audit binaries are built on the runner with `CGO_ENABLED=0 GOOS=OS GOARCH=ARCH go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o verify-audit-OS-ARCH ./cmd/verify-audit`, and the notes end with the Go version (`go version`) of that runner. `tests/test_ci_release.py` fails if the conditional or this wording changes in `release.yaml` without this page. They also say: The default image CAPTCHA has no audio or text alternative.

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

## Tag-time checklist

Do these in order for each release tag (the first is `v0.1.0-alpha.2` or whatever the owner picks). Nothing here deploys anything.

1. **Merge first.** The tag goes on a reviewed commit on `main` whose CI run passed, including the **CI passed** check. Do not tag a branch.
2. **Replace "in no release".** Once the tag exists, those words are false. Change them to name the tag in `README.md` (the important note under the title), `docs/escrow.md`, `docs/operator-guide.md` (Escrow) and the "development branch and in no release" lines in `docs/acceptance.md`, in the same commit that precedes the tag. Keep the test-network and unverified-journey wording (mainnet is an unrun operator opt-in); a tag does not verify anything.
3. **Retitle UPGRADING.md** from "Upgrading from v0.1.0-alpha.1" to "Upgrading from v0.1.0-alpha.1 to `<tag>`", and put a one-page summary on top: what changed for an operator, the migration range (`001` to the last file in `internal/market/migrations`), the sections that need action before the upgrade (a database with payment history this build does not support is refused, sections 58 and 59), and the rollback rule.
4. **Check the notes.** Push the tag. The release workflow reruns CI and creates a draft marked **Pre-release**; open it and confirm the first line of the notes, the reproducibility paragraph, the line "The default image CAPTCHA has no audio or text alternative." and the Go version, then publish.
5. **Cite the tag's CI run** in [verification.md](verification.md): a row for the tag with the run URL of the **Release** workflow (confirm the commit and conclusion with `gh run view <id>`), as for every other row.
6. **Bump `UPGRADE_FROM_REF`.** After the tag exists, change the default in `scripts/test-upgrade.sh` (`from_ref=${UPGRADE_FROM_REF:-v0.1.0-alpha.1}`) to the new tag (the `upgrade` job in `ci.yaml` already fetches every tag) and update the tag `docs/operations-tests.md` names, so the next release is tested from this one. Keep the alpha.1 rehearsal until a decision to drop it is recorded.
7. **Owner settings.** Enable private vulnerability reporting (Settings, Code security, Private vulnerability reporting) so the link in [SECURITY.md](../SECURITY.md) works, and add a tag ruleset for `v*` that blocks deleting or moving a release tag.
8. **Tor Browser.** Either the owner has filled [Manual Tor Browser pass (owner)](verification.md#manual-tor-browser-pass-owner), or the README and notes keep saying it has not been tried.

## Workflow maintenance

Actions are pinned to verified full commit SHAs, permissions default to read-only, and checkout does not persist credentials. Only the release packaging job receives `contents: write`. Pull requests never receive release permissions or production secrets. Dependabot proposes dependency/action updates for review.

CI uses disposable PostgreSQL credentials that are intentionally committed test fixtures. Never substitute production URLs or credentials in these jobs. Do not commit local environment files, backups, browser authentication state, or private test traces. Browser artifacts can contain the generated fixture records used in CI.

Sources: [GitHub secure-use reference](https://docs.github.com/en/actions/reference/security/secure-use), [artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations), and [Docker build attestations](https://docs.docker.com/build/metadata/attestations/).
