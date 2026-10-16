# Native CLI and service separation verification — 2026-10-15

This records local working-tree verification based on commit `3047be15` plus the
uncommitted implementation diff. It is not a release, production deployment,
mainnet payment run or live Tor-circuit test. Fixtures use disposable keys,
accounts, databases and Docker projects. No actual disk erasure or host reboot
was performed.

## Scope and independent review

The requested scope is a portable Monero-only executable, deliberate migration
of deployment operations from Bash to Go, reusable service hosts, production
separation checks, administrator-only diagnostics, a separate dependency worker,
private administrator Tor ingress, RAM runtime configuration and manual incident
operations. The existing Tor OPSEC critic and backend/infrastructure war-room
seats reviewed the changes. External chat/audit text was treated as proposed
design input and checked against source and official documentation.

Required findings corrected during review:

- Main-app raw image decoding was moved behind a separate gateway/scanner role.
  Mutual TLS admission, bounded one-use upload tickets and final application
  ownership/session/quota checks prevent direct submission to the private backend.
- App proxy TLS and upload client authentication use independent trust settings;
  the upload client identity is never sent to the ordinary application origin.
- Production refuses declared local/collocated service identities and unsafe
  transports. Different names still do not attest physical host separation.
- Public ingress cannot acquire private administrator capabilities through Host,
  forwarding headers or an administrator session. Tor ignores malformed client
  records, so the controller validates the entire public allowlist and refuses
  an empty or malformed snapshot.
- Actual offline Tor startup rejected the original host-owned read-only client
  directory. A networkless native helper now prepares the dedicated Tor volume
  before Tor runs as the configured nonroot runtime UID/GID.
- The controller's Docker context and project are pinned in a private receipt.
  Emergency stopping compares the confirmed target under the mutation lock,
  removes orphans and confirms that zero project containers remain before success.
- LUKS operations hold the selected device descriptor, pin its identity/UUID,
  recheck them and use the inherited descriptor for the trusted system executable.
  Manual confirmation is required; no external event triggers erasure.
- RAM publication uses Linux exclusive atomic rename. Retention state must live
  outside tmpfs. An emergency marker inside a removed RAM instance does not
  survive; trusted manual unlock/recovery is the restart gate.
- Existing scanner/source-language guards were updated narrowly for explicit
  transport settings and deletion of forwarding headers. Reads/writes of those
  headers remain forbidden. Documentation does not label scanned files harmless.

## Tests and checks

The Go toolchain was 1.27.2. PostgreSQL integration used disposable PostgreSQL 17
databases; earlier focused gateway tests also used PostgreSQL 16.

| Check | Actual outcome |
| --- | --- |
| `TEST_DATABASE_URL=... go test -race -count=1 -timeout 30m -run '^(TestEscrow\|TestXMR\|FuzzEscrow)' ./...` | Passed, including the market package at 828.301 s. Real-chain tests retain their normal explicit environment gates; simulated RPC evidence is not a live-chain claim. |
| `TEST_DATABASE_URL=... go test -race -count=1 -timeout 30m -skip '^(TestEscrow\|TestXMR\|FuzzEscrow)' ./internal/market` | Fresh full nonpayment market suite passed, 816.287 s. |
| Final native CLI, fake emergency and RAM package race run | Passed; server package 33.599 s, emergency 2.098 s, RAM 2.260 s. |
| Final deployment controller race run | Passed, 7.299 s. Additional backend final pass covered the helper and target/orphan guards. |
| Dependency worker, key material and protected file packages | Race tests passed; independent admin dependency HTTP/database tests passed. |
| Public/private administrator ingress and production profile tests | Backend final race run passed: market 19.311 s, server 26.843 s. |
| Linux emergency and RAM test executables in networkless Docker containers | Passed. The descriptor test uses harmless regular files; device/reboot operations are faked. |
| Existing Python suite | Initial full run: 377 tests, one skip, nine errors under system Python 3.9/sandbox. Eight required local sockets; one legacy installer stub exceeded its 30 s deadline. All 13 affected fixture tests passed under Python 3.13 with local sockets and the ordinary deadline (13.851 s). No product workaround or test assertion weakening was added. |
| Release workflow tests and pinned actionlint 1.7.12 | Passed. |
| `go mod verify`, `go vet ./...`, Go formatting and `git diff --check` | Passed. |
| CLI/documentation coverage | 18 help interfaces, all 94 emitted flag names, 35 shell examples and 58 local file/fragment links checked. Strict MkDocs build passed. |

The first broad nonpayment run found three new static-guard incompatibilities
and one dispute regression failure. The static checks were corrected narrowly;
the dispute test passed unchanged on its isolated race rerun (17.122 s). The
fresh full market rerun then passed, including the corrected static guards and
the unchanged dispute test.

## Actual local Docker and native execution

- The executable served preview HTML/CSS outside the checkout with embedded
  assets. A real development Compose installation reached healthy app/database
  state and served health, login and CSS endpoints.
- A new immutable app image was pushed only to a disposable loopback registry.
  App-only redeployment replaced the application container, retained the exact
  database container ID and left every installation key byte unchanged. Health,
  login and CSS requests succeeded after the replacement.
- GPG decrypted a disposable encrypted installation bundle directly into Docker
  exec stdin. The Linux controller accepted an actual `nosuid,nodev,noexec`,
  root-owned 0700 tmpfs, unlocked metadata as root 0600/0700, assigned the private
  secret subtree to its runtime UID/GID, and validated the deployment plan.
  No plaintext bundle file was written. A first harness attempt correctly refused
  the ordinary nonroot app user; the root-controller rerun passed.
- The native administrator preparation helper and cached Tor image ran with no
  network. Tor accepted the client authorization as arbitrary UID/GID 65532,
  retained its onion private identity across helper rerun/Tor restart. The helper
  refused malformed authorization. Temporary container/volume were removed.
- Real emergency stop targeted only the disposable local market. It removed its
  containers, retained data volumes, wrote its marker and refused ordinary
  restart. Only after that verification were its disposable volumes removed.
  No device, RAM cleanup or reboot option was supplied.

## Limits

No remote worker was provisioned or production endpoint supplied. The controller
cannot prove physical host isolation, firewall/egress policy, provider behavior
or transport anonymity. The gateway handles browser credentials and sessions.
Offline keys do not prevent a live-root attacker reading online wallet/app
secrets. tmpfs may swap; snapshots and crash dumps remain host concerns. LUKS
erasure does not erase data blocks/header backups or close live mappings. Actual
destructive device behavior must be validated separately on an approved target.

Backup/restore, mirror provisioning and some maintenance remain documented Bash
workflows. Native equivalents are not claimed. No browser end-to-end suite or
public Tor client-authorization circuit was run for this slice. The release
workflow was validated locally. These results were recorded before creating the
feature PR; consult that PR for its commit and CI status. No production release
or deployment is claimed.

Final review found no unresolved required issue within the implemented scope.
The disposable development market, its volumes, loopback registry, PostgreSQL
test services and synthetic bundle private keys were cleaned up. Linux release
candidate binaries and checksums remain in the ignored `artifacts/native/`
directory; the source workflow will build its own artifacts for a release.

## Follow-up: managed onion identities and interactive setup

The owner subsequently requested native onion address/key import or generation,
optional vanity prefixes up to six characters, a feature PR, and guided setup
when flags are omitted in an interactive terminal.

- Native service identities use Tor's expanded Ed25519 key-file format. The
  pinned `filippo.io/edwards25519` 1.2.0 dependency derives and validates the
  public key from that expanded secret; standard Go seed/public private-key
  serialization is not substituted. Imports must match the complete v3 address.
- Fresh enabled Tor roles generate private managed identity directories during
  initialization. Public and admin identities remain distinct. Existing complete
  volume identities are compared and retained; mismatched, partial or unmanaged
  replacement state refuses startup. The full identity triplet is included in
  configured encrypted RAM bundles and checked again during unlock.
- Vanilla key generation and one-character vanity tests passed. Seven-character
  prefixes are refused; active search cancellation is tested without performing
  a six-character search. A separate native executable test generated a
  one-character identity and imported exactly the same secret into a new private
  directory. Prefix length is not a completion-time guarantee.
- Actual networkless Docker/Tor checks passed for both administrator and public
  roles with the selected generated address and secret bytes. Helper rerun and
  Tor restart retained the keys; a different selected source was refused.
- Real macOS PTY tests opened guided setup with no arguments and with bare
  `deploy init`. Context help, quit and immediate Ctrl-C cancellation worked.
  Explicit `setup` through stdin saved a reviewed development configuration,
  retained stagenet and private key permissions, and created no Compose runtime
  file or Docker service. Headless no-argument serving remains compatible.
- The wizard tests cover default-no creation/start, separate lifecycle consent,
  cached answer edits and clearing, local validation, control-character refusal,
  EOF/cancellation and secret-content nonprinting. Completed output distinguishes
  generated/imported materials and lists the saved paths and backup/next steps.
- The final focused race run passed: interactive 2.046 s, onion identities
  3.631 s, RAM bundles 2.405 s, key tools 2.517 s, operator 12.577 s and server
  25.527 s. The earlier broad payment/nonpayment runs above precede this
  CLI-only follow-up; marketplace/payment handlers were unchanged by it.
- Repository-pinned govulncheck 1.8.0 over `./...` reported zero vulnerabilities
  affecting called code, zero in imported packages and one in a required module
  that the code does not call. Release/documented-test-name checks passed 21
  tests. Module verification, vet and formatting checks passed.
- Follow-up documentation checks covered 22 help interfaces, 110 emitted flag
  names, 39 shell examples and 106 local links. Independent guided-flow review
  identified an optional-start cancellation gap; a context check now guards the
  final install call, matching the other mutation boundaries.

Persistent Tor Docker volumes still contain online copies of the service keys;
RAM configuration does not make those copies volatile. No live public Tor circuit,
six-character search, disk erasure or reboot is claimed by this follow-up.
