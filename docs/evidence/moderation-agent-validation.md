# Moderation validation evidence

This records local development checks performed on 2026-10-20 UTC. It is not a
production deployment, real-model quality evaluation, live Tor-circuit test or
real-funds payment run. The moderation implementation was independently reviewed
against the supplied proposal; see the [comparison](../moderation-proposal-comparison.md).

The initial implementation commits were `50166094`, `9c56ef94` and `4b245e01`.
Later review fixes were integrated through `cc9fd4f839d76e2e038fee3f64d0aa05a560eda2`.
Results below identify narrower checks where appropriate; they do not certify a
later merge or a new CI run.

## Historical moderation-v1 GBNF parser check

The actual upstream llama.cpp grammar parser and validator were built from
[commit 3bb63cbcbfb62056c84fe9e521b0f3d756d29c32](https://github.com/ggml-org/llama.cpp/tree/3bb63cbcbfb62056c84fe9e521b0f3d756d29c32).
The [official source archive](https://codeload.github.com/ggml-org/llama.cpp/tar.gz/3bb63cbcbfb62056c84fe9e521b0f3d756d29c32)
was downloaded over verified HTTPS and its SHA256
recorded. No inference executable, model weights or additional repository
dependency was downloaded or installed. GCC/G++ 14.2.0 compiled the upstream
`tests/test-gbnf-validator.cpp`, grammar, vocabulary, Unicode and logging sources,
with unused inference code removed by linker section collection.

The initial moderation-v1 grammar templates were substituted with the current
allowlists, item `listing:7`, and the canary derived from an input hash of 64
lowercase `a` characters. The canary was
`moderation-0ea8f6c5a914382e930188af72d6aeb1`. Both grammars parsed successfully.

| Fixture | Observed result |
| --- | --- |
| Stage A coded findings with valid canary | Accepted |
| Proposal with valid fields and canary | Accepted |
| Proposal summary containing escaped quotes/newline and UTF-8 | Accepted |
| Proposal with wrong canary | Rejected |
| Proposal with `EXECUTE_SQL` action | Rejected |
| Proposal referring to another item | Rejected |
| Proposal with confidence `1.01` | Rejected |
| Proposal with an unknown reason code | Rejected |
| Proposal with an additional field | Rejected |
| Truncated proposal | Rejected |
| Stage A finding with a free-form quotation field | Rejected |
| Stage A with wrong canary | Rejected |

All 12 assertions passed: three valid inputs accepted and nine invalid inputs
rejected. This upstream utility exits zero even for rejected input; the harness
checked its explicit acceptance/rejection text, not just its exit status.

| Artifact | SHA256 |
| --- | --- |
| Official source archive | `ff9ecfc1aab2b115ef08ef49e6d1460497e3a696f80ab0c595fb8ad223a5cbe0` |
| Upstream `src/llama-grammar.cpp` | `97d76071a2ed1b0c4ce827f002f554b12a25593776adb525dde33ac4cb1eb1e9` |
| Upstream `tests/test-gbnf-validator.cpp` | `21eb0bac296c0e0dd497cc438ab8f10ba9cad66488c00288321d939afbdf310f` |
| Repository Stage A grammar template | `8c83be9aedea7476dccff45ac49d7150719873491f745153da653671a076fbd0` |
| Repository proposal grammar template | `88b3481133685465687b02955f7e2970488ff271906ea1db582bb08a797a5bc7` |
| Substituted Stage A grammar | `8fa01c36208fd15816bb60900f03147c28307350cc16d9b9987f530a40f23456` |
| Substituted proposal grammar | `cae50a2cf7221154777be7fc6a60572fa080bc3872220bc2998fde1657cc2aa5` |
| Twelve-case results JSON | `6ccfa729427627aa041cb88bb4ac7ac672f2c66cbd60f67eaeba3d5eb86ff817` |

The original synthetic inputs, substituted grammars and results were retained in
the validation workspace under `/workspace/toolchains/llama-validation/fixtures/`.
That workspace is not a release artifact. To reproduce independently, acquire the
pinned source archive, check its hash, generate the two grammar substitutions
using `grammarLiteral`/`grammarEnums` from `internal/moderation/inference.go`, and
test the table above with the native validator. A minimal source-only build is:

```sh
# Inside the verified upstream source tree; no model is required.
mkdir -p validator-build
printf '%s\n' '#pragma once' '#define GGML_VERSION "source-validation"' \
  '#define GGML_COMMIT "3bb63cbcbfb62056c84fe9e521b0f3d756d29c32"' \
  > validator-build/ggml-version.h
gcc -std=c11 -D_GNU_SOURCE -O1 -ffunction-sections -fdata-sections \
  -Ivalidator-build -Iggml/include -Iggml/src -c ggml/src/ggml.c \
  -o validator-build/ggml.o
g++ -std=c++17 -O1 -ffunction-sections -fdata-sections \
  -Iinclude -Isrc -Iggml/include -Iggml/src \
  tests/test-gbnf-validator.cpp src/llama-grammar.cpp src/unicode.cpp \
  src/unicode-data.cpp src/llama-impl.cpp src/llama-vocab.cpp \
  validator-build/ggml.o -Wl,--gc-sections -pthread \
  -o validator-build/test-gbnf-validator
validator-build/test-gbnf-validator STAGE_GRAMMAR.gbnf INPUT.json
```

This establishes compatibility with that native parser, not every runtime build
or correct inference. Truncated generation can still return incomplete JSON.
Independent Go validation remains mandatory for duplicate/missing/unknown/null
fields, enum and numeric bounds, lengths, reference integrity and canaries. Those
checks and the Unix-socket adapter tests passed under Go 1.27.2 with the race
detector: `go test -race ./internal/moderation ./internal/dependencies`.

## Process isolation: synthetic executable, not inference

`scripts/test-moderation-isolation.sh` builds the repository's small static Go
probe into a local scratch image and applies the resolved optional inference
container controls. It uses synthetic public weights and a disposable host
secret. This is an operating-system/container test, separate from fake model
responses in unit tests and separate from running a selected llama.cpp build.

The probe checks the runtime UID/GID, absence of credential/proxy environment
variables, denied host-secret/control paths, read-only root and weights, bounded
scratch availability, no effective capabilities, no-new-privileges, disabled core
dumps, loopback-only interfaces, failed external TCP connection and usable Unix
socket. It neither exposes production secrets nor changes a production service.

Reproduce with a local Docker daemon/Compose and the required Go toolchain:

```sh
MODERATION_TEST_GO=/path/to/reviewed/go bash scripts/test-moderation-isolation.sh
python3 -m unittest discover -s tests -p 'test_moderation_isolation.py' -v
```

The configured model-serving image, its launcher, actual model bytes, production
mounts, host placement and resource budgets still require their own acceptance
rehearsal. An environment allowlist, container recipe, synthetic probe or weights
digest alone does not prove a selected runtime is safe or correctly deployed.

## Initial PR #99 verification

The following table predates moderation-v2 and remains historical initial-PR
evidence. Local checks distinguish their source revision and evidence tier. The
branch also incorporates main `00346cd6`; its full remote CI gate was pending when this evidence was prepared. Final
check results are recorded on [PR #99](https://github.com/yegamble/opsecmkt/pull/99).
Earlier local passes must not be relabeled final-source CI.

| Check | Observed state when this evidence was prepared |
| --- | --- |
| Moderation and dependency package race tests | Passed, including strict output handling, canary/reference refusal, two-stage data separation, proxy/redirect denial and absent/interrupted runtime cases |
| Public retained-copy regression | Failed first for missing notices, then passed against a disposable PostgreSQL database; distinctive plaintext remains in both job and proposal copies after ordinary listing expiry, and disclosure remains after capture is disabled |
| Related records/deletion/hold race tests | Passed, 5.742 s on the isolated disclosure-fix branch |
| Records disclosure browser check, JavaScript disabled | Four Chromium widths and WebKit mobile passed. Firefox passed in the complete Firefox project rerun. |
| Full PostgreSQL-backed Go halves | Escrow half passed (market 578.333 s); general half was still running against `cc9fd4f8` at evidence capture; its final outcome is recorded on PR #99. The escrow half uses simulated RPC; environment-gated chain tests are not live-chain evidence. |
| Ordinary browser suite and focused fixes | Initial run: 614 passed, 121 failed, one skipped. Failures were the Firefox launch environment plus five private-navigation assertions. After the fix, those five unchanged Chromium/WebKit overview checks and five records disclosure checks passed. The complete Firefox rerun passed all 118 tests (114 preview, one setup, three real database upload/refusal journeys) in 2.2 minutes after the main merge. It required workspace XDG/temp directories and an approved outer-exec sandbox exception for `/proc/self/uid_map`; Firefox's own sandbox remained enabled and `HOME` was unchanged. |
| Simulated Monero escrow browser suite | All nine journeys passed (3.4 minutes) with real PostgreSQL, simulated wallet RPC and JavaScript disabled. |
| Full Python suite | 393 tests passed in 202.138 s. The four subsequently added signer-input tests also passed without skips; their full-suite rerun was in progress at evidence capture. |
| Synthetic isolation executable | `bash scripts/test-moderation-isolation.sh` passed with exit 0 against disposable synthetic artifacts, then passed again after the main merge. It is now required in CI operations; no selected model was run. |
| Modules, vet, formatting, shell syntax, whitespace and strict MkDocs | Passed on the integrated branch; vet and pinned actionlint repeated after the main merge. |
| Reachable vulnerability check | Pinned `govulncheck@v1.8.0` passed: zero vulnerabilities affecting called code; one module-level advisory does not affect called/imported packages. |
| Main merge regression | Moderation, freshness, source guards, private navigation and paused sign-in rescue race checks passed after merging `00346cd6` (92.930 s). |
| Independent review-fix verification | MOD69-1/2 passed four PostgreSQL race tests (3.389 s); MOD69-3/4 passed literal-SQL, navigation, early denial, freshness and image isolation checks (12.909 s). MOD70-5 passed four bounded signer-input regressions and real OpenSSL-to-Go race interoperability (1.202 s). |

## Production gates and limits

- Select and review the runtime/model, licenses, immutable artifact pins and
  CPU/GPU budget. Test real timeout, interruption and recovery with those bytes.
- Rehearse the actual process, filesystem and network boundary; inspect the
  deployed mounts and service identities. No live-host or hypervisor immunity is
  established by these development checks.
- Verify private administrator access with real Tor client authorization. Header
  spoofing refusals in local tests are not a live Tor-circuit test.
- Decide evidence minimization, retention and purge policy. Historical snapshots
  and proposal inputs currently have no automatic expiry; disabling capture and
  ordinary content/account expiry do not remove them.
- Validate useful moderation quality with representative, independently reviewed
  data. Coded findings do not establish a faithful full-thread timeline or safe
  public reply publication. Image understanding is outside scope by owner
  decision; dHash and basic metadata support duplicate/similarity signals only.
- Preserve existing human/transactional authority for funds, bans and target
  mutations. Review annotations and model confidence do not grant that authority.
- Run final-source CI and deployment acceptance. No real funds were used and no
  deployment, merge or green CI result follows from this document alone.


## Moderation-v2 hardening follow-up

The subsequent rating/review was checked against source; the accepted changes
are recorded in war-room iteration 71 and the proposal comparison. A score is
not a test result. The new work removes the model prose channel, minimizes new
capture to metadata, enforces atomic collection budgets and preserves real
human-review visibility. Existing immutable history remains unchanged.

Both v2 templates passed the same pinned native upstream grammar validator:
17 cases, four accepted and thirteen rejected. Accepted inputs were coded Stage
A, a normal closed proposal, a coded injection proposal and an empty-reference
context. Rejected cases covered foreign Stage A/B references, nonempty references
with empty context, legacy summary/evidence fields, wrong item/action/reason/
confidence/canary, extra fields, truncation and a Stage A free quotation.
V1 artifacts remain untouched. V2 artifacts are in
`/workspace/toolchains/llama-validation/v2/` in the development workspace.

| V2 artifact | SHA256 |
| --- | --- |
| Stage A template | `916c91b831a7e03bff40c44f45bd3ba38086dd7bb057d3c28915b98dd01bb23e` |
| Proposal template | `a73f123e37effc915f025295c879ff71543b7fa4d6cee7adbe82b222de5d0021` |
| Seventeen-case result JSON | `bbf2a0d6426cea16a5fba190cfee407833ccaf62c3d13ff7c3ee28800223dd14` |

Focused local verification completed before the final-source CI run:

- Closed protocol, custom-adapter prose removal, routing and coded provenance
  passed package race tests; CLI retired-protocol/capacity refusals passed.
- New admission/privacy concurrency and full moderation/source/image/registry
  checks passed with real PostgreSQL (19.093 s); final lease/replay allocation
  regressions passed (5.921 s).
- Independent release review passed the full moderation race suite (34.866 s).
  Two independent actual HTTP workflows passed (5.952 s): deterministic scams
  appear in private review; saturated capture leaves listing creation and a valid
  funded simulated-wallet dispute, order transition and staff notice working.
- Independent privacy review passed tracked storage/retention/source checks
  (9.950 s), core checks (1.172 s) and ten unchanged/changed source cases across
  all five kinds (10.082 s). It scanned every moderation table for unique source
  and malicious model-prose markers, verified actor removal/output hashes/replay,
  and checked coded private presentation. The overlay cases were then adopted
  as permanent regression tests.
- Authentic pre-128 upgrade (the capacity migration, since renumbered 134), original-source expiry and collection disablement
  preserved historical snapshot/input/decision bytes (3.648 s with token checks).
- Gateway weak-hash fixtures passed pure races (1.902 s) and legacy omission,
  upgrade and image-decoding/source guards (5.740 s). These are advisory hash
  tests, not semantic image or rotation/crop guarantees.

The complete local Go halves, browser and Python runs were in progress when this
evidence was prepared. PR #99 records their final outcomes and the exact final
commit's CI run; earlier green results above do not certify a later source state.
Real model quality, deployed confinement/private Tor, legacy retention policy,
and actual physical database/WAL/backup storage remain separate operator gates.
