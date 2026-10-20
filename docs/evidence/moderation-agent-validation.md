# Moderation validation evidence

This records local development checks performed on 2026-10-20 UTC. It is not a
production deployment, real-model quality evaluation, live Tor-circuit test or
real-funds payment run. The moderation implementation was independently reviewed
against the supplied proposal; see the [comparison](../moderation-proposal-comparison.md).

The initial implementation commits were `50166094`, `9c56ef94` and `4b245e01`.
Later review fixes were integrated through `cc9fd4f839d76e2e038fee3f64d0aa05a560eda2`.
Results below identify narrower checks where appropriate; they do not certify a
later merge or a new CI run.

## Native GBNF parser check

The actual upstream llama.cpp grammar parser and validator were built from
[commit 3bb63cbcbfb62056c84fe9e521b0f3d756d29c32](https://github.com/ggml-org/llama.cpp/tree/3bb63cbcbfb62056c84fe9e521b0f3d756d29c32).
The [official source archive](https://codeload.github.com/ggml-org/llama.cpp/tar.gz/3bb63cbcbfb62056c84fe9e521b0f3d756d29c32)
was downloaded over verified HTTPS and its SHA256
recorded. No inference executable, model weights or additional repository
dependency was downloaded or installed. GCC/G++ 14.2.0 compiled the upstream
`tests/test-gbnf-validator.cpp`, grammar, vocabulary, Unicode and logging sources,
with unused inference code removed by linker section collection.

The repository's two grammar templates were substituted with the current
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

## Application and broader verification

Local checks below distinguish their source revision and evidence tier. The
branch also incorporates main `00346cd6`; its full remote CI gate is pending.
Earlier local passes must not be relabeled final-source CI.

| Check | Observed state when this evidence was prepared |
| --- | --- |
| Moderation and dependency package race tests | Passed, including strict output handling, canary/reference refusal, two-stage data separation, proxy/redirect denial and absent/interrupted runtime cases |
| Public retained-copy regression | Failed first for missing notices, then passed against a disposable PostgreSQL database; distinctive plaintext remains in both job and proposal copies after ordinary listing expiry, and disclosure remains after capture is disabled |
| Related records/deletion/hold race tests | Passed, 5.742 s on the isolated disclosure-fix branch |
| Records disclosure browser check, JavaScript disabled | Four Chromium widths and WebKit mobile passed. Firefox remains environment-blocked: after repairing its profile location, content subprocesses encounter a managed-sandbox denial writing `/proc/self/uid_map`; browser security was not disabled. |
| Full PostgreSQL-backed Go halves | Escrow half passed (market 578.333 s); final pre-main-merge general half is running against `cc9fd4f8`. The escrow half uses simulated RPC; environment-gated chain tests are not live-chain evidence. |
| Ordinary browser suite and focused fixes | Initial run: 614 passed, 121 failed, one skipped. Failures were the Firefox launch environment plus five private-navigation assertions. After the fix, those five unchanged Chromium/WebKit overview checks and five records disclosure checks passed. Firefox rerun is pending. |
| Simulated Monero escrow browser suite | All nine journeys passed (3.4 minutes) with real PostgreSQL, simulated wallet RPC and JavaScript disabled. |
| Full Python suite | 393 tests passed in 202.138 s. |
| Synthetic isolation executable | `bash scripts/test-moderation-isolation.sh` passed with exit 0 against disposable synthetic artifacts; no selected model was run. |
| Modules, vet, formatting, shell syntax, whitespace and strict MkDocs | Passed on the integrated branch; vet repeated after the main merge. |
| Reachable vulnerability check | Pinned `govulncheck@v1.8.0` passed: zero vulnerabilities affecting called code; one module-level advisory does not affect called/imported packages. |
| Independent review-fix verification | MOD69-1/2 passed four PostgreSQL race tests (3.389 s); MOD69-3/4 passed literal-SQL, navigation, early denial, freshness and image isolation checks (12.909 s). |

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
  data. No faithful full-thread timeline, semantic photo/claim verification or
  safe public reply publication has been established by coded findings or dHash.
- Preserve existing human/transactional authority for funds, bans and target
  mutations. Review annotations and model confidence do not grant that authority.
- Run final-source CI and deployment acceptance. No real funds were used and no
  deployment, merge or green CI result follows from this document alone.
