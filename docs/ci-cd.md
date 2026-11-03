# CI and release delivery

Every pull request and every push to `main` runs `.github/workflows/ci.yaml`; other branches are tested through their pull request, or on demand with **Run workflow** (`workflow_dispatch`). The stable **CI passed** check succeeds only when every mandatory job succeeds, including all matrix entries. Branch protection on `main` requires that check; do not accept a skipped or cancelled suite as a pass.

The current Monero-only suite is configured to cover:

- Independent required Go quality, workflow/shell safety and dependency vulnerability checks, alongside race tests against PostgreSQL. Three parallel test jobs (`go`, `go-escrow` and `go-desk`) partition the top-level tests so each runs exactly once; see [Go test jobs](#go-test-jobs).
- Monero escrow against real daemon and wallet-rpc binaries on a local fakechain (`escrow-xmr-regtest`). This uses test coins and establishes no public-network or deployment result.
- Server-rendered preview and database browser journeys with JavaScript disabled. Monero escrow browser journeys use simulated RPC, not a live chain.
- Installer, Compose network isolation, resource limits, Tor configuration, scanner, encrypted backup/restore, and upgrade smoke checks.
- The native deployment controller (`deploy init`, `install`, `redeploy`, `doctor --start-check`, `down`) in development mode on the runner's Docker Engine (`scripts/test-native-deploy.sh`, container job, external-database row); production mode is not run.
- Required-job aggregation through **CI passed**. Historical Bitcoin jobs and signer builds have been removed.

Each parallel test process needs its own disposable database. No development or CI job uses mainnet. Preserve a failed run's sanitized logs and browser traces; do not change expectations until the failure is understood. See [development commands](development.md), [operations checks](operations-tests.md) and the dated [verification records](verification.md).

## Quality and security gates

`make go-quality` checks Go formatting/imports, module consistency/integrity and
the configured analyzers across all packages, including tests. It runs in its
own required job so PostgreSQL or browser failures cannot prevent analysis.
`make workflow-lint` runs actionlint; the workflow safety job also runs
hash-locked zizmor and checksum-pinned ShellCheck across tracked shell scripts.
The Go tools have an isolated `tools/go.mod` and `tools/go.sum`; they do not add
development-only dependencies to the application module. See
[contributing](../CONTRIBUTING.md) for the shared local commands and Lefthook
installation. Hooks are bypassable, so CI enforces their quality checks too.

The reusable [dependency security workflow](../.github/workflows/dependency-security.yaml)
runs `make vuln` (`scripts/go-fetch.sh` for the application's modules and the
scanner's, then `govulncheck -test ./...`), audits both npm lockfiles, and uses
pip-audit on the complete hash-locked documentation and scanner requirements.
These jobs are required on pull requests, pushes to `main`, manual CI and the
release workflow. A vulnerability in test or documentation tooling is included
even when it cannot affect the shipped server. GitGuardian remains the remote
PR secret check; local Gitleaks hooks provide earlier feedback.

`scripts/go-fetch.sh` is also the application Dockerfile's download step. It retries
a transport failure (a stream error, a truncated body, a connection reset, a TLS
or I/O timeout, a 502, 503 or 504, or an attempt that reached its time limit and
finished no module) up to three attempts in all, with the failure in the log. A
checksum refusal or any other error fails after one. The limit is `GO_FETCH_TIMEOUT`:
600 seconds as the Dockerfile's build argument, which every job building that image
outlasts at three stalled attempts (the scheduled image job passes 180 to fit its
25 minutes), and 180 in `make vuln`. An operator on a slow link
[raises it](install/troubleshooting.md#first-install-failed). CI's `GOPROXY` and `GOSUMDB` stay the defaults;
`tests/test_ci_go_fetch.py` refuses a workflow or build file that adds a `|direct`
fallback or turns checksum checking off.

The built application and Tor images are scanned with pinned Trivy. Their builds
apply Debian package updates between pinned base-image rebuilds. Full JSON
reports retain all known vulnerabilities, including unfixed and lower-severity
findings, for 14 days. Fixable HIGH or CRITICAL vulnerabilities fail the job;
scanner/download errors also fail. The application scan reuses the internal
database matrix entry's image, and the Tor scan reuses the offline-test image.
CI's image builds reuse the Actions layer cache, but pass the build day as
`SECURITY_REFRESH`, so the layer that runs `apt-get upgrade` is rebuilt at least
daily and a Debian security fix published since the last cached build reaches
the scan. The documented operator builds ([binary installation](install/binary.md))
pass the same day with `--pull`, and `scripts/install.sh` passes it to Compose.
CI's application image also carries `BUILD_VERSION=ci` and the commit, and
`scripts/ci-smoke.sh` checks that `version` and `/admin/system` name them. Release packaging additionally scans the actual OCI archive before creating the
draft. Trivy `--input` opens a Docker archive or an OCI layout directory, not an OCI tar, so packaging unpacks
into a layout directory the same `opsecmkt.oci.tar` that is then compressed into `opsecmkt.oci.tar.gz`, checks
that Trivy's first-descriptor choice reaches the linux/amd64 image rather than an attestation, and scans that
directory with the same gate. Its checksum, recorded before the scan, is checked again before compression, so
a changed archive is never published. No registry push is required for these scans.

The [scheduled security workflow](../.github/workflows/security.yaml) runs weekly
(Monday at 09:23 UTC) and on manual request. It reuses the same dependency audits,
rebuilds both images without cache to check current distro packages, and runs
two minutes of `FuzzDecodeReencode` image fuzzing with two workers. Reproducing
crash inputs are retained for 14 days. It does not run the full integration
matrix or contact wallets, public chains or deployed hosts. Scheduled failures
are actionable even when the source commit has not changed: vulnerability
databases can disclose new problems after an earlier green CI run.

## Release a tested version

Push a version tag, for example `v0.1.0`, on the intended reviewed commit. The release workflow first refuses a tag whose commit is not on `main` (an ancestry check against the checkout's `origin/main`; a checkout without `main` refuses too), then reruns the entire CI workflow against that tag before building any release output. It requires the tag to use `vMAJOR.MINOR.PATCH` (an optional prerelease suffix is supported). A tag with a suffix (any `-` in the name, such as `v0.1.0-alpha.2`) is created with `gh release create --prerelease`; a plain tag is not. The image is built without any Actions cache.

The image and the standalone binaries carry the tag and commit (the `BUILD_VERSION` and `BUILD_COMMIT` build arguments; `-X main.buildVersion` and `-X main.buildCommit`): the image's `version` prints `opsecmkt <tag> commit <sha>`, as the standalone binaries' version prints, and packaging fails if the amd64 binary prints anything else. `/admin/system` shows the same tag and the commit's first 12 characters.

**Dry run.** **Run workflow** on the Release workflow (`workflow_dispatch`, no inputs) runs the same pipeline on the selected branch under the fixed tag `v0.0.0-dry-run`: CI, the OCI archive and its scan, the binaries and `SHA256SUMS`. It keeps those files only as the run's `release-files` workflow artifact (14 days) and never creates a release or pushes a tag: the release job runs only for a `v*` tag push, and its release step refuses any other event or ref. A green dry run on `main` is the evidence that the pipeline works before the first tag is pushed.

After the OCI archive passes its vulnerability gate, a tag push hands the same workflow artifact to the release job, which checks `SHA256SUMS` and creates a **draft GitHub release** with:

- `opsecmkt.oci.tar.gz`: Linux amd64 OCI image archive, with embedded BuildKit provenance and SBOM.
- `build.json`: exact source commit, tag, platform and Actions run URL.
- `opsecmkt-linux-amd64` and `opsecmkt-linux-arm64`: native application/operator
  binaries built from `cmd/server`, including public UI assets, key management,
  the upload gateway and the separate dependency-version worker. Follow
  [Binary installation](install/binary.md); Docker services still need reviewed
  image digests. These binaries are included in `SHA256SUMS`.
- `verify-audit-linux-amd64`, `verify-audit-linux-arm64`, `verify-audit-darwin-amd64`, `verify-audit-darwin-arm64`, `verify-audit-windows-amd64.exe`: static builds of `cmd/verify-audit`, the offline checker for signed audit exports (standard library only, no network access).
- `SHA256SUMS`: checksums for the archive, build record and every verifier binary.

The release notes open with `Pre-release. Test networks only. No real funds. Install: git checkout <tag>; see UPGRADING.md.` (the workflow writes the actual tag). They then state the reproducibility limits: the OCI archive is for inspection and registry import; scripts/install.sh builds the application from source on your host and is not bit-reproducible, so the archive is not what the installer runs; the verify-audit binaries are built on the runner with `CGO_ENABLED=0 GOOS=OS GOARCH=ARCH go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o verify-audit-OS-ARCH ./cmd/verify-audit`, and the notes end with the Go version (`go version`) of that runner. `tests/test_ci_release.py` fails if the conditional or this wording changes in `release.yaml` without this page. They also say: The image CAPTCHA has no audio or text alternative; by default it appears only on sign-in under the attack signal.

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

Do these in order for each release tag (the first is `v0.1.0-alpha.2` or whatever the owner picks). Up to the tag the order is: doc edits, CI green, a dry run on that commit, then the tag. Nothing here deploys anything.

1. **Edit the docs.** These claims are false once the tag exists, so one reviewed pull request replaces them, naming the tag; that is the commit you will tag. For `v0.1.0-alpha.2` they were replaced and this list emptied; list any new release claim here before the next tag. `tests/test_ci_release.py` fails if a string listed here is missing from its file, or if another release claim in the same words is not listed, so the same pull request also removes each replaced row. Keep the test-network and unverified-journey wording (mainnet is an unrun operator opt-in); a tag does not verify anything.
2. **Finish UPGRADING.md in that pull request.** The summary does not move: it stays under its `# Upgrading from v0.1.0-alpha.1 to` heading, with the numbered sections that code and scripts cite and that `tests/test_upgrading_docs.py` reads from that heading down. Instead, retitle `# Current source` so it does not claim the sections below it are unreleased, and put a line above it linking to the summary, so a reader of the first page finds it. If the owner picks another tag, rename the heading and `HEADING` in that test. Retitling `### Unreleased security hardening` changes its anchor, so change the link to `unreleased-security-hardening` in `docs/install/tor.md` too; `test_links_into_upgrading_resolve` fails otherwise. Check the summary still gives what changed for an operator, the migration range (`001` to the last file in `internal/market/migrations`), the sections that need action before the upgrade (a database with payment history this build does not support is refused, sections 58 and 59), and the rollback rule.
3. **Merge and wait for CI.** Merge to `main`, and wait for that merge commit's CI run to pass, including the **CI passed** check. Cite the run ID and commit (`gh run view <id>`); a merge does not imply green CI.
4. **Dry run that commit.** Use **Run workflow** on the Release workflow, on `main`, while that commit is its head, and confirm the `headSha` and `conclusion` with `gh run view <id> --json headSha,conclusion`. A dry run of an earlier commit does not count: the last green one, 40701238128, is on `7b30970b`, well behind `main`. If `main` moves first, the tag still goes on the commit that CI and the dry run passed, or both run again on the new head.
5. **Tag that commit.** Tag the commit steps 3 and 4 passed, by its full SHA, and push only the tag. Do not tag a branch: the release workflow refuses a tag whose commit is not on `main`.
6. **Check the notes.** The release workflow reruns CI and creates a draft marked **Pre-release**; open it and confirm the first line of the notes, the reproducibility paragraph, the line "The image CAPTCHA has no audio or text alternative; by default it appears only on sign-in under the attack signal." and the Go version, then publish.
7. **Cite the tag's CI run** in [verification.md](verification.md): a row for the tag with the run URL of the **Release** workflow (confirm the commit and conclusion with `gh run view <id>`), as for every other row.
8. **Bump `UPGRADE_FROM_REF`.** After the tag exists, change the default in `scripts/test-upgrade.sh` (`from_ref=${UPGRADE_FROM_REF:-v0.1.0-alpha.1}`) to the new tag (the `upgrade` job in `ci.yaml` already fetches every tag) and update the tag `docs/operations-tests.md` names, so the next release is tested from this one. Keep the alpha.1 rehearsal until a decision to drop it is recorded.
9. **Owner settings.** Enable private vulnerability reporting (Settings, Code security, Private vulnerability reporting) so the link in [SECURITY.md](../SECURITY.md) works, and add a tag ruleset for `v*` that blocks deleting or moving a release tag.
10. **Tor Browser.** Either the owner has filled [Manual Tor Browser pass (owner)](verification.md#manual-tor-browser-pass-owner), or the README and notes keep saying it has not been tried.

## Workflow maintenance

Actions are pinned to verified full commit SHAs, permissions default to read-only, and checkout does not persist credentials. Only the release job, which runs for tag pushes alone, receives `contents: write`; the identity and packaging jobs, and every dry run, only read. Pull requests never receive release permissions or production secrets. Dependabot proposes dependency/action updates for review.

### Go test jobs

The Go race tests are split by top-level test name across three jobs, each with its own runner, PostgreSQL service, coverage file and artifact, and each in **CI passed**. No test is skipped and no timeout is raised: every job keeps `go test -race -count=1 -timeout 17m` and a 20-minute job limit.

| Job | Name in the checks list | Selection | Coverage artifact |
| --- | --- | --- | --- |
| `go-escrow` | Go escrow and Monero tests | `-run` with the prefixes `TestEscrow`, `TestXMR`, `FuzzEscrow` | `go-escrow-coverage` |
| `go-desk` | Go staff desk, moderation and administration tests | `-run` with the prefixes `TestAction`, `TestAdmin`, `TestBan`, `TestDesk`, `TestDispute`, `TestLimiter`, `TestListing`, `TestMedia`, `TestModeration`, `TestModerator`, `TestPaused`, `TestPerceptual`, `TestResolve`, `TestRetention`, `TestSoleAdmin`, `TestStaff`, `TestTicket`, `TestVendor` | `go-desk-coverage` |
| `go` | Go and PostgreSQL | `-skip` with both lists joined into one `^(...)` alternation | `go-coverage` |

Formatting, vet, actionlint and govulncheck run in the independent quality, workflow-safety and security jobs, not in these three. `go` installs cjpeg for `TestDecodeAllocationWithinEstimate`, which stays in `go`. A-465 split the escrow tests out when the single job reached 14 min 42 s of its test step. MOD72-9 added `go-desk` when the moderation and listing hold tests pushed `go` to its 20-minute limit (run 39498912726 was cancelled with `internal/market` alone at 1018 s); in a local measurement of the old `go` selection (`go test -race -json` on a loaded host, so only relative), the `go-desk` prefixes took 47% of its `internal/market` test time. The real proof is the job durations of the first CI run.

`tests/test_ci_go_split.py` proves the partition from the workflow text: each `-run` pattern must be `^(A|B|...)` of literal name prefixes; `go`'s `-skip` must equal `'^(' + go-escrow's prefixes + '|' + go-desk's prefixes + ')'`; no prefix of one job may be a prefix of the other's (with literal prefixes that is exactly the condition for no name matching both); every top-level test in the module's `*_test.go` files is selected by exactly one job; and every prefix selects at least one test (except `FuzzEscrow`, which A-465 reserved for a future escrow fuzz target), so a typo cannot silently leave its tests in `go`. To move tests between jobs, edit the `-run` pattern and the `-skip` pattern together in the same order, then check the names the toolchain lists:

```sh
go test -list . ./... | python3 tests/test_ci_go_split.py --list
```

It prints how many top-level tests each job runs and fails on any name run by no job or by two. Locally the split is optional: the two commands in [development](development.md#go-and-postgresql) (`-run` and `-skip` with the escrow pattern) still run every test exactly once, because `go` and `go-desk` together are the `-skip` half.

CI uses disposable PostgreSQL credentials that are intentionally committed test fixtures. Never substitute production URLs or credentials in these jobs. Do not commit local environment files, backups, browser authentication state, or private test traces. Browser artifacts can contain the generated fixture records used in CI.

Sources: [GitHub secure-use reference](https://docs.github.com/en/actions/reference/security/secure-use), [artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations), and [Docker build attestations](https://docs.docker.com/build/metadata/attestations/).
