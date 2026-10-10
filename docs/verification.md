# Verification

## Evidence table

Updated 2026-10-10. Each row is evidence that can be cited: a GitHub Actions run (its commit and conclusion confirmed with `gh run view <id>`) or a local log. Ticks in [acceptance.md](acceptance.md) point here. Evidence tiers are those of the war room: unit, DB integration, browser preview, browser DB, simulated wallet RPC, real local chain, public test network, deployed instance. A simulated wallet RPC or the in-process fake provider is not chain evidence. Merged is not released and released is not deployed.

Every CI row is the `CI` workflow ([`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml)), including the required **CI passed** job. The earlier runs had 15 jobs, then 16 with the A-438 `escrow-regtest` job; later runs include additional escrow, installer and Compose jobs. The required gate checks every configured test job. Its tiers are: unit and DB integration (`go test -race -count=1 ./...` with `TEST_DATABASE_URL` on PostgreSQL 17); browser preview and browser DB (Playwright `npm run test:e2e`: preview projects in Chromium, Firefox and WebKit, database projects in Chromium); simulated wallet RPC (`npm run test:e2e:wallet`); and operations (encrypted backup/restore, restore through the internal-db Compose service, upgrade from `v0.1.0-alpha.1`, the local installer followed by the setup wizard over HTTP and an app restart (`scripts/test-install.py`), the production image with internal and external PostgreSQL, the Compose configuration matrix and the offline Tor image check). The DB test helper skips only when `TEST_DATABASE_URL` is unset ([`testenv_test.go`](../internal/market/testenv_test.go)), and CI sets it, so the database tests ran. `go test` prints individual test names only on failure, so a named test counts as passed because its package reported `ok` in that run.

| Check | Evidence tier | Commit | CI run or log | Date (UTC) |
|---|---|---|---|---|
| Public Monero stagenet digital claim: funding and release each ten confirmations, exact planned claimant receipt | public test network, own authenticated loopback daemon/wallet RPCs, PostgreSQL 17 | `ac71878dfde42daed9dc7b4e883c8d24600816f3` | [Manifest, unaltered transcript and supplemental observations](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md); 16:56:32–17:46:36Z, 50m04s; final wallet recovery reopened read-only and verified, task droplet deleted. Digital claim only; no refund/threshold or deployed-market claim. | 2026-10-10 |
| Proof compatibility fixes and durable build-event regression: all required checks passed | unit, full PG17 race suites, browser preview/DB, simulated wallets, real Core regtest, operations/upgrades/Tor containers | `a52ce770f1b6deba08c5c0a92e64a239f8b66db0` | [38069776541](https://github.com/yegamble/opsecmkt/actions/runs/38069776541); 22 successful workflow jobs including CI passed. Focused cloud fake-wallet PG17 race PublicRefusal/PublicBuildEvidence/StoredBeforeRelay also passed at ac71878 (6.391s, no skips); independent review passed. | 2026-10-10 |
| Completion PR #43, strict public Monero explorer selection and pooled hold/drain regressions; all required checks passed | unit, full PG17 race suites, browser preview/DB, simulated wallets, real Core regtest, operations/upgrades/Tor containers | `a59c7a88adc74af7b52ef8c511317e66989c85ac` | [37977057728](https://github.com/yegamble/opsecmkt/actions/runs/37977057728); base for the live public-proof compatibility fixes; the manual funded Monero journey is separate | 2026-10-09 |
| Public signet operator smoke: practice signatures, funded digital claim and buyer underpaid refund; outputs matched and both settlements confirmed once | public test network; actual Tor proxy/private broadcast, restricted RPC and PG17 | `9885beef29d3e222026fd68a907e94f2ac6c7041` | [manifest and unaltered transcript](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md); 17:44:51–18:20:29 UTC; private recovery backup verified, temporary host deleted | 2026-10-09 |
| Completion PR #43 including the guarded public Monero harness; all required checks passed | unit, full PG17 race suites, browser preview/DB, simulated wallets, real Core regtest, operations/upgrades/Tor containers | `8f2ee79bb1a070830198a3e60462bd57a58a7036` | [37962121589](https://github.com/yegamble/opsecmkt/actions/runs/37962121589); 22 CI jobs passed, GitGuardian neutral | 2026-10-09 |
| Completion PR #43, approved authentication receipts and patched Go runtime; all required checks passed | unit, full PG17 race suites, browser preview/DB, simulated wallets, real Core regtest, operations/upgrades/Tor containers | `9885beef29d3e222026fd68a907e94f2ac6c7041` | [37957076020](https://github.com/yegamble/opsecmkt/actions/runs/37957076020); Go 1.27.2 govulncheck reports no affecting vulnerabilities | 2026-10-09 |
| CI, PR #33 (war-room iteration 18 through A-103 and A-320) | CI tiers above | `dd777b8` | [36746860618](https://github.com/yegamble/opsecmkt/actions/runs/36746860618) | 2026-09-30 |
| CI, push to `main` (merge of PR #9, `war-room/integration`) | CI tiers above | `7048c2e` | [36274132492](https://github.com/yegamble/opsecmkt/actions/runs/36274132492) | 2026-09-26 |
| CI, push to `main` (integration work through A-115, pushed directly) | CI tiers above | `8b4685f` | [36015165046](https://github.com/yegamble/opsecmkt/actions/runs/36015165046) | 2026-09-24 |
| CI, PR #9 (A-13, A-27, A-82) | CI tiers above | `08a9437` | [36000605614](https://github.com/yegamble/opsecmkt/actions/runs/36000605614) | 2026-09-24 |
| CI, push to `main` (merge of PR #8) | CI tiers above | `ab7cd9f` | [35994268519](https://github.com/yegamble/opsecmkt/actions/runs/35994268519) | 2026-09-24 |
| CI, PR #8 (A-9, A-28, A-29) | CI tiers above | `0a1b75d` | [35958151032](https://github.com/yegamble/opsecmkt/actions/runs/35958151032) | 2026-09-24 |
| CI, PR #8 (A-8, A-10, A-11, A-74) | CI tiers above | `30be215` | [35956392431](https://github.com/yegamble/opsecmkt/actions/runs/35956392431) | 2026-09-24 |
| CI, PR #8 (A-53, A-54, A-26, A-71) | CI tiers above | `bf1ddc9` | [35954364843](https://github.com/yegamble/opsecmkt/actions/runs/35954364843) | 2026-09-24 |
| CI, PR #8 (A-66, A-67, A-72) | CI tiers above | `766c87f` | [35952354841](https://github.com/yegamble/opsecmkt/actions/runs/35952354841) | 2026-09-24 |
| CI, push to `main` (merge of PR #7) | CI tiers above | `4c360d2` | [35951223304](https://github.com/yegamble/opsecmkt/actions/runs/35951223304) | 2026-09-24 |
| CI, PR #7 (A-30, A-47, A-48, A-52) | CI tiers above | `cdb69f2` | [35913448670](https://github.com/yegamble/opsecmkt/actions/runs/35913448670) | 2026-09-23 |
| CI, PR #7 (A-23, A-24, A-25) | CI tiers above | `f66ab6b` | [35908986249](https://github.com/yegamble/opsecmkt/actions/runs/35908986249) | 2026-09-23 |
| CI, PR #7 (A-6, A-7, A-21) | CI tiers above | `699ea45` | [35905170138](https://github.com/yegamble/opsecmkt/actions/runs/35905170138) | 2026-09-23 |
| CI, push to `main` (merge of PR #6) | CI tiers above | `509356d` | [35899168645](https://github.com/yegamble/opsecmkt/actions/runs/35899168645) | 2026-09-23 |
| CI, PR #6 (A-1 to A-5) | CI tiers above | `2771d6b` | [35896554121](https://github.com/yegamble/opsecmkt/actions/runs/35896554121) | 2026-09-23 |
| `chain-journey.spec.ts` 1/1 (4 real payouts, none ambiguous) and `scripts/regtest-smoke.sh` PASS; bitcoind 31.1 regtest and monerod 0.18.5.1 offline stagenet fork | real local chain; **UNVERIFIED** here | `cdb69f2` | QA report in `.claude/war-room/ledger.md` (A-13 ruling, A-52 row); a local run, not CI; the log is not kept under `artifacts/real-chain/` in this checkout | 2026-09-23 |
| Same chain journey 1/1 (BTC and XMR) and regtest smoke PASS | real local chain; **UNVERIFIED** here | `699ea45` | QA report in `.claude/war-room/ledger.md` (A-7 row, A-13 ruling); local run, log not retained | 2026-09-23 |
| Same chain journey 1/1 (BTC and XMR) and regtest smoke PASS | real local chain; **UNVERIFIED** here | `509356d` | QA report in `.claude/war-room/ledger.md` (iteration 3 log); local run, log not retained | 2026-09-23 |
| `chain-journey.spec.ts` 1/1 (BTC and XMR: partial deposits, release and moderator refund, 4 real payouts sent, none ambiguous); bitcoind v31.1.0 regtest and monerod/monero-wallet-rpc v0.18.5.1 offline stagenet fork; `TestRegtestSmoke` was skipped in that session | real local chain | `ab7cd9f` (a `git archive` of it plus one unbuilt probe test file; the log itself names no commit) | local logs (gitignored, not CI) with SHA-256 sums: `artifacts/real-chain/ab7cd9f/` (`chain-journey.log`, `SHA256SUMS`); the payout-row summary was read from a database since deleted | 2026-09-24 |
| `TestRegtestSmoke` PASS through `scripts/regtest-smoke.sh`; chain journey BTC 1/1 (18.5 s) and XMR 1/1 (1.7 min) | real local chain | uncommitted tree before `f38dade`; no pinned commit | local logs (gitignored, not CI): `artifacts/real-chain/bitcoin-regtest-smoke.log`, `artifacts/real-chain/btc-browser.log`, `artifacts/real-chain/xmr-browser-final.log`; see [project review](project-review.md#real-isolated-chain-verification) | 2026-09-23, 15:15–15:27 |
| `TestRegtestSmoke` PASS through `scripts/regtest-smoke.sh` against a real `bitcoind -regtest`; `chain-journey.spec.ts` 1/1 (BTC and XMR: partial deposits, confirmation, fulfillment and moderator refund, 4 real payouts sent, none ambiguous); bitcoind v31.1.0 regtest and monerod/monero-wallet-rpc v0.18.5.1 offline stagenet fork via `scripts/setup-local-chains.py` | real local chain | `7048c2e` (clean checkout; the logs themselves name no commit) | local logs (gitignored, not CI) with SHA-256 sums: `artifacts/real-chain/7048c2e/` (`regtest-smoke.log`, `chain-journey.log`, `chain-setup.log`, `SHA256SUMS`) | 2026-09-26 |
| `chain-journey.spec.ts` 5/5 on BTC and XMR (12.7 min): partial deposits, release and moderator refund; an overpaid order (one release, one return); a return larger than the price held, released by an administrator on `/admin` and sent once; an underpaid order cancelled by the buyer (one refund); an underpaid order expired by the watcher (one refund) and then topped up late (recorded after the close, flagged, never paid out). 16 payouts sent, none ambiguous, each received in the transaction the market recorded with the fee taken from the amount (Monero releases included). `scripts/regtest-smoke.sh` PASS. Official Bitcoin Core 31.1 and Monero v0.18.5.1 Linux archives verified against their signed manifests | real local chain | `1c62647` (clean detached checkout; its tree equals `dd777b8`'s apart from the war-room ledger) | local logs (gitignored, not CI), run by the implementer: `artifacts/real-chain/1c62647/` (`MANIFEST.txt`, `binary-verification.txt`, `chain-setup.log`, `regtest-smoke.log`, `chain-journey.log`, `playwright-report.json`, `payouts.tsv`, `payments.tsv`, `SHA256SUMS`) | 2026-09-30 |
| Independent rerun of the row above by war-room QA (QA30): the same 5/5 (12.9 min), the same 16 payouts and 16 payments by kind, amount, fee and state; manifests fetched afresh from bitcoincore.org and getmonero.org, byte-identical to the kept copies, signatures valid (fanquake `CFB16E21…26C1`, binaryFate `81AC591F…DF92`); no credential in the JSON or HTML report; mutations of the late-deposit crediting, the large-return hold and the fixture's Monero fee each failed the run | real local chain | `1c62647` (clean detached checkout) | local logs (gitignored, not CI): `artifacts/real-chain/1c62647-qa30/` (the files above plus `html-report/index.html`, `SHA256SUMS`); mutation logs in `artifacts/real-chain/qa30-mutations/` | 2026-09-30 |

Reading the real-chain rows:

- The three QA rows are reported in the war-room ledger only. Their SHA-pinned logs were not found on this machine when this table was written, so they are marked UNVERIFIED. The manual regtest line in acceptance.md rests on the `7048c2e` row instead, whose logs are kept. The row for the uncommitted tree before `f38dade` has logs, but it predates the payout-timeout change (`12b3e71`) to `payments_rpc.go`, `payments_bitcoin.go` and `payments_monero.go`, so it does not cover the current adapters.
- The Bitcoin and Monero adapters (`payments_bitcoin.go`, `payments_monero.go`, `payments_rpc.go`) are unchanged from `cdb69f2` to `ab7cd9f`. After `cdb69f2`, payout queueing in the watcher and hooks changed (A-54, `c40a224`) and so did the admin payout actions (A-53, `ae6a49a`); both are in `bf1ddc9`. The `ab7cd9f` chain-journey row covers the adapters and the watcher's normal path with that code, but not the specific A-53/A-54 branches (a restore marking failed payouts, an unavailable provider).
- The two `1c62647` rows are the release evidence war-room A-103 asked for: a run pinned to a commit, with a manifest, covering the payout changes since `cdb69f2` (fee-deducted payouts on both coins, overpayment returns, the large-return review, underpaid cancel and expiry, and a deposit after the close). They do not cover a restore marking failed payouts, an unavailable provider, a reorg, or the Monero daemon killed mid-send (war-room A-45). The payment adapters, watcher and hooks at `1c62647` are those of PR #33's head `dd777b8`; a later change to them needs a new pinned run before a release.

No row exists, so nothing is claimed, for: a public test network (testnet4, signet, stagenet or testnet), a deployed instance, Tor onion reachability, or a release. The only tag is `v0.1.0-alpha.1`, still a draft GitHub release. Monero behaviour when the daemon is killed mid-send is untested (war-room A-45).

## Monero escrow wallet scale, PAYE-E2 (war-room A-429, P3-R12) — 2026-10-04

Local runs on one host, not CI and not public-network evidence: real `monerod` and `monero-wallet-rpc` 0.18.5.1
(`ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1`, `simple-monero-wallet-rpc:v0.18.5.1`) on a local fakechain, driven
by `scripts/test-escrow-xmr-payee2.sh` (`TestXMREscrowPAYEE2`) through the app's own adapter and its timeouts. One escrow
wallet gets 10,000 accounts; 200 chosen at random are funded two per block over 110 blocks while it is closed; then a
watcher pass runs over 5 copies of that wallet. Reports are gitignored.

The first run, `artifacts/real-chain/paye-e2-2a93cf716406.txt` (run at commit `2a93cf7` by the journey slice), measured
the wallet as the market then created it (monero-wallet-rpc's default lookahead of 200 addresses per account):
`create_account` ×10,000 took 71.7 min (median 390 ms per call, rising from 15 ms to 806 ms); the wallet cache was 78.8 MiB;
peak wallet-rpc memory 429 MB, and 574 MB during the 5-wallet pass; `open_wallet` 1.7 s; `get_accounts` 1.8 s with a
2.4 MiB reply; a refresh over 1,000 empty blocks 311 ms; a seed restore with `set_subaddress_lookahead` (minor 1) and a
rescan about 4 s in all, finding all 202 funded accounts; and the 5-wallet watcher pass 27.6 s, against the 10-minute
pass timeout and the 30 s default poll interval. Verdict PASS, with one finding: the catch-up refresh over the 110 funding
blocks (200 deposits) took 2.5 min (747 ms per deposit received), longer than the 2-minute bound refresh then had, so
after downtime with many deposits the watcher's refresh would have timed out on every pass.

Fix (A-429). Refresh got its own 8-minute bound (`xmrWalletRefreshTimeout`), leaving an `open_wallet` room inside the pass.
A refresh that still times out is not lost: on the same nodes a `rescan_blockchain` whose client gave up after 2 s held a
following `get_version` for 15.7 s more, so monero-wallet-rpc finishes the call and answers nothing else meanwhile, and the
next `open_wallet` stores the wallet before opening one; the pass reads nothing from a wallet whose refresh failed
(`TestXMREscrowRefreshOutlastsSlowTimeout`, `TestXMREscrowRefreshTimeoutCatchUp`, simulated RPC). And each new escrow
wallet now looks one address ahead per account (`set_subaddress_lookahead` major 50, minor 1, right after
`create_wallet`; it persists in the keys file, `TestXMREscrowWalletLookaheadMinor`), since an escrow address is always an
account's address 0. Reduced runs of 2,000 accounts and 200 deposits over 110 blocks, on the same nodes at commit
`b3d3057` with the change in the working tree (`artifacts/real-chain/paye-e2-b3d305767b11-dirty-lookahead-minor200.txt`
and `-lookahead-minor1.txt`; the default-lookahead run's report, `paye-e2-b3d305767b11-dirty.txt`, was later overwritten
by a full-size run of the same name, so its numbers below are copied from that report before it was overwritten; the
script now adds a reduced size to the report name):

| 2,000 accounts | Default lookahead (no call) | Minor 200 set (`ESCROW_XMR_PAYEE2_LOOKAHEAD_MINOR=200`) | Minor 1 |
|---|---|---|---|
| Catch-up refresh, 200 deposits | 20.95 s (104.8 ms per deposit) | 24.22 s (121.1 ms) | 0.70 s (3.5 ms) |
| `create_account` ×2,000 | 2.1 min (median 54.2 ms) | 2.1 min (median 55.1 ms) | 14.9 s (median 6.8 ms) |
| Wallet cache | 16.0 MiB | 16.1 MiB | 0.5 MiB |
| Seed restore finds every funded account | 202 of 202 | 202 of 202 | 202 of 202 |

The full size as the watcher now creates wallets (minor 1), `artifacts/real-chain/paye-e2-b19d406ca6b1.txt` (commit
`b19d406`, about 4 minutes in all): `create_account` ×10,000 took 1.3 min (median 7.5 ms per call); the wallet cache
1.0 MiB; `open_wallet` 0.62 s; the catch-up refresh over 110 blocks with 200 deposits 1.03 s (5.1 ms per deposit); a
refresh over 1,000 empty blocks 302 ms; `get_accounts` 1.9 s with the same 2.4 MiB reply; the seed restore found all 202
funded accounts; the 5-wallet pass 18.6 s. Verdict PASS, no finding. Peak memory was not measured cleanly in these runs (the wallet-rpc
container had served the earlier runs; its resident memory stayed near 35 MB). Not measured: more than 10,000 accounts or
200 deposits a month, another host, and any public network (stagenet repeat: Phase 4).

## Onion-mirror commands with the installer's `.env` (war-room A-140) — 2026-09-24

Local Compose configuration check, not a running stack: Docker 29.8.0, Compose 5.5.1, a scratch `.env` in the
installer's format for Tor with the internal database (`COMPOSE_FILE='compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml'`).
With `COMPOSE_PROFILES='internal-db'`, the old guide command failed both ways:
`docker compose --profile mirror config --quiet` and `docker compose --profile mirror up -d --dry-run tor-mirror`
printed `service "app" depends on undefined service "db": invalid compose project`. With
`COMPOSE_PROFILES='internal-db,mirror'`, `docker compose --dry-run up -d` planned `db`, `app`, `tor` and
`tor-mirror` and `config --services` listed all four. Dry runs were not reliable enough for CI: 20 repeats of
`docker compose --dry-run up -d` gave 1 or 2 spurious failures (`app is missing dependency db`), and an
earlier dry-run version of the check failed 3 of 8 runs (that error, or a plan missing the `tor-mirror` lines).
`scripts/test-mirror-commands.sh` therefore resolves commands with `config --services`; it passed for all
four installer configurations, and failed with the message above when the guide's `up -d`
line was temporarily replaced by the old command. CI runs it in the Tor rows with the mirror on (first green in
run 36034975984). QA then found one check flaky: it piped `config --services` into `grep -q` under `pipefail`,
so Compose sometimes hit a broken pipe after the first match (4/10 full-script runs under load on macOS, 2/200 of
the pipeline on `ubuntu:24.04`); the list is now read in full before matching.

## Tor restarts with the app (war-room A-134) — 2026-09-24

Local Compose check with stand-in containers, not onion traffic: Docker 29.8.0, Compose 5.5.1, project `a134churn`, a scratch copy of `compose.yaml` and `compose.tor.yaml` with an override that replaces the app and Tor images by `alpine:3.20` running `sleep` (nothing built or pulled). Sequence: `up -d app tor`; then `up -d` with a changed app setting and one added service on the backend network (standing in for UPGRADING's node profiles); then `docker compose restart app`; then `down -v`.

| `compose.tor.yaml` | App address after the change | Tor `StartedAt` after the app was recreated | After `restart app` |
|---|---|---|---|
| before (`depends_on: [app]`) | moved 172.19.0.2 → 172.19.0.4; the new service took .2 | unchanged (14:24:34.458) | unchanged |
| after (`restart: true`) | unchanged in that run (172.18.0.2) | Compose logged `tor Stopping` → `app Recreated` → `tor Started`; 14:28:36.556 → 14:29:42.146 | restarted |

With the mirror profile, `tor` and `tor-mirror` both restarted when the app was recreated; a mirror started with `--profile mirror up -d tor-mirror` was **not** restarted by a later plain `up -d` without the profile (hence the documents' advice to put `mirror` in `COMPOSE_PROFILES`). Limits found in the same session: pulling the new `compose.tor.yaml` alone does not restart a running Tor (only an app recreate or restart does, which then also restarts the pre-existing Tor container); `docker compose stop app` followed by `up -d` does not restart Tor; and `docker compose up --dry-run --force-recreate tor tor-mirror` on a project without the mirror profile creates and starts `tor-mirror`, which is why the documents name `tor-mirror` only for installs that run it. CI checks the setting in every Tor row of the Compose job; the runtime behaviour above is a local run, not CI. Onion reachability after churn is still not verified here.

## Onion identity and .env backup and restore (war-room A-136) — 2026-09-26

Local run, not CI: Docker 29.8.0, Compose 5.5.1, the `opsecmkt-tor:ci` image built from `deploy/Dockerfile.tor` (Tor 0.4.9.11), age 1.3.1 and again with age 1.1.1 (the version Ubuntu 24.04 installs in CI), GNU bash 5.3 on macOS. `scripts/test-tor-identity-restore.sh` ran the operator guide's four `ops-cmd` blocks verbatim and passed: `.env` restored byte for byte (wrong identity and existing `.env` refused), and for `tor_identity` and `tor_mirror_identity` backup, refused restore over the live volume, `docker volume rm`, restore, and Tor starting offline with the same hostname and secret-key digest; a root-owned archive restored into a root-owned volume also kept the address. With the guide's `chown -R debian-tor:debian-tor /var/lib/tor` line deleted, the root-owned case failed (`Directory /var/lib/tor/marketplace cannot be read: Permission denied`, Tor exited); with the non-empty-volume refusal deleted, the test failed at `tor_identity restore accepted a non-empty volume`; with `--pull never` deleted from the backup's `tar` container, the missing-image case failed because Docker tried to pull the image (`pull access denied`). Each line was restored and the test passed again. `scripts/test-internal-db-restore.sh` failed before `restore.sh` gained its last line naming `SETUP_TOKEN` and `AUDIT_SIGNING_KEY`, and passed after. Separately, `\password opsecmkt` through `psql` in a `postgres:17-alpine` container changed the role's password (the new one authenticated over SCRAM, the old one was refused) without the password appearing in the server log. Not verified: the CI tor-image job running the test, reachability of a restored onion over the Tor network, and a full restore rehearsal on a real deployment.

## Review fixes — 2026-09-23

A fresh-eyes review of the merged packages found eight defects. Each was reproduced by a failing test first (`internal/market/review_fixes_integration_test.go`), then fixed:

1. Stale draft price: resubmitting checkout now updates the draft's amount, and requesting payment reprices the draft from the listing in the same transaction (`TestDraftRepricedOnResubmitAndPay`).
2. Rate-limiter lockout: a full limiter table evicts the entry that expires first; `/challenge` and `/challenge/pgp` look up the pending login, and sign-in validates the handle, before creating a limiter key (`TestLimiterFloodDoesNotLockOutUsers`). Superseded in part by war-room A-11: eviction now takes the lowest-count non-blocking entry, never a blocking one, and sign-in counts only requests that passed the CAPTCHA (see `docs/implementation-status.md`).
3. Unbounded unpaid reservations: at most 3 orders awaiting payment per buyer (409), and the watcher cancels orders unpaid after `PAYMENT_EXPIRY` (default 24h) with no deposit seen, returning their stock (`TestPayCapsOpenAwaitingPaymentOrders`, `TestWatcherExpiresUnpaidOrders`).
4. Session-only takeover: payout-address changes, turning PGP sign-in off, and changing or removing the key while it is on need the current password, plus an authenticator code when TOTP is enrolled (`TestSensitiveChangesNeedPassword`).
5. Late deposits after close: closed orders stay watched for 30 days after address issue; a deposit recorded before a cancellation that refunded nothing is refunded when it confirms, while one first recorded after the order closed, or confirmed after the settlement was queued, is flagged and shown to the buyer as not paid out, with one buyer and one staff notice per order (`TestLateDepositAfterCloseIsHandled`, `TestLateTopUpAfterUnderpaidRefundIsDisclosed`, `TestLateDepositFloodIsAnnouncedOnce`).
6. Reorg to 0 confirmations: payouts are held while any credited deposit is below the threshold and resume when it re-confirms (`TestPayoutHeldWhileCreditedDepositReconfirms`).
7. Monero locked transfers: `unlock_time` ≠ 0 is recorded as locked, never credited, reported once (`TestMoneroLockedTransfersAreNotCredited`; migration 051).
8. Legacy PGP key: an unchanged stored key is not re-parsed on profile save (`TestLegacyUnparseableKeySavesUnchanged`).

Results after the fixes: `gofmt -l` clean; `go vet ./...` clean; `go test -count=1 ./...` passes on a fresh database (the new tests plus the lifecycle and payment tests also pass with `-race`); Playwright 263 of 264 (only the known WebKit mobile skip-link failure); Python tests 18 pass; `govulncheck` v1.1.4 reports no called vulnerabilities.

## Feature completion — 2026-09-23

Historical record, true when written. Where a later run changed a statement, a dated note says so; current evidence is in the [evidence table](#evidence-table).

Scope: the Foundation refactor and packages P1–P6 (authentication, PGP, orders, inventory, test-network payments, transparency) merged on `finish-codebase`, plus the cross-package lifecycle test. Results are from a local macOS machine with PostgreSQL 16 unless stated.

### Passed

- `gofmt -l cmd internal` reports nothing; `go vet ./...` is clean.
- `go test -count=1 ./...` with `TEST_DATABASE_URL` (isolated schema per test): all packages pass, including every package's integration tests and `TestLifecycleDigitalAndDisputedPhysical`. That test drives the HTTP handlers with the in-process fake wallet: a vendor lists a digital item with delivery content and saves a payout address; the buyer drafts and requests payment (TESTNET-labelled address shown, stock reserved); a deposit below the confirmation threshold leaves the order awaiting payment; at the threshold one watcher pass marks it paid and auto-delivers as the system actor (content visible to the buyer, 404 for third parties and moderators); completion sends exactly one release payout to the vendor's address across repeated passes; one review is accepted and a second returns 409. A physical order is then paid, disputed, resolved as a refund by a moderator, and exactly one refund is sent to the buyer's address.
- Race detector: focused tests (including the lifecycle test) pass with `-race`. The full database suite was run **without** `-race` locally: real bcrypt cost-12 hashing takes about 29 s under the race detector on this machine, beyond the 12 s request timeout, so setup/login tests return 503 there. CI runs the full suite with `-race`.
- Playwright: 263 of 264 passed — every `preview*.spec.ts` in the six Chromium/Firefox/WebKit viewport projects, and `db-setup` plus all `db-*.spec.ts` against a fresh database. The one failure is the WebKit mobile skip-link focus check in `preview.spec.ts`, which also fails on the pre-package commit on this Mac.
- Compose configuration matrix: 6 of 6 (clearnet/Tor × internal/external database × mirror on/off), with node and `monero-wallet` profiles parsed.
- `govulncheck` v1.1.4: no vulnerabilities in imported packages; one module-level advisory in code the application does not call.
- Python regression tests: 18 pass.

### Not verified

- Live Docker image builds, container startup and Tor/onion reachability were not exercised in this effort. *Superseded in part, 2026-09-24:* every CI row in the evidence table builds the production image and starts it with internal and external PostgreSQL. Tor/onion reachability is still unverified.
- Live Bitcoin regtest/testnet run: the Bitcoin Core adapter is tested only against local HTTP fakes. `scripts/regtest-smoke.sh` is provided for the operator to run against `bitcoind -regtest`; it was not run here. *Superseded, 2026-09-24:* the smoke test and real local-chain browser journeys were run later on 2026-09-23; see the real-chain rows of the evidence table. No public testnet run is recorded.
- Monero stagenet live run: the monero-wallet-rpc adapter is tested only against local HTTP fakes. *Superseded in part, 2026-09-24:* real local-chain journeys on an offline stagenet fork are in the evidence table. The public stagenet has not been used.
- Mainnet payments: an operator opt-in (`BITCOIN_CHAIN=main`, `MONERO_NETWORK=mainnet`). Its refusals, labels and network-history guard are tested against fakes and simulated wallet RPC only; no mainnet run is recorded.
- The GitHub Actions run for this branch (including the full `-race` suite on PostgreSQL 17) is not recorded here. *Superseded, 2026-09-24:* later CI runs, with commits, are in the evidence table.
- Browser checks are not a complete WCAG AAA audit.

## Earlier baseline (before the feature packages)

Historical record, superseded by the [evidence table](#evidence-table) where they differ (CI now runs PostgreSQL 17 integration and starts the production image).

### Passed


- Go 1.26.8: `go test -race ./...` against an isolated local PostgreSQL 16.15 database. Tests cover exact atomic amounts, malformed/overflow prices, roles, CSRF, cookie flags, request bounds, setup lockout, registration, listing ownership, idempotent order drafts, private messages/orders, HTML escaping, session revocation, and restart persistence.
- `go vet ./...` and `go mod verify`.
- `govulncheck`: zero reachable vulnerabilities and zero advisories in imported packages. One module-wide advisory concerns the unused, deprecated `golang.org/x/crypto/openpgp` package; this application imports bcrypt, not that package.
- The initial machine toolchain (Go 1.26.2) reported standard-library vulnerabilities. Project minimum and Docker build moved to 1.26.8, then tests and scanning passed. [Go release history](https://go.dev/doc/devel/release).
- All 17 UI routes inspected at a 320px browser viewport: no page-level horizontal overflow. Catalog also inspected at 1280px; desktop filters render. Mobile search filters sample results correctly. Skip-link Tab/Enter behavior reaches main content.
- Original `[OPSECMKT]` wordmark verified in mobile browser. No application JavaScript or remote browser assets are shipped.
- Internal/external database and Tor/clearnet Compose configuration parsing, installer choice simulations, shell syntax, and optional mirror profile validation.
- Actual age-encrypted PostgreSQL backup/restore roundtrip, including Unicode data, 0600 output, overwrite refusal, wrong-key rejection, and transaction rollback on conflicting restore. Temporary backup databases and key material removed.

### Not verified or not implemented

- Live Docker image builds, container startup, live onion reachability, blockchain synchronization, external providers, and real host-failure recovery were not exercised.
- No payment/escrow or cryptocurrency funds were handled. See implementation-status.md for the unavailable application capabilities.
- Browser checks are not a complete WCAG AAA audit. Screen-reader interoperability, every color pairing, and exhaustive keyboard interaction remain release checks.
- PostgreSQL 17 integration runs are configured in CI but were not executed on GitHub during this session.

### CI/CD follow-up

The repository is on GitHub at https://github.com/yegamble/opsecmkt. Automated pipelines replace the earlier manual-only checks. PostgreSQL 17 integration, both production-container database modes, deployment matrices, and encrypted restore have passed on GitHub runners. The Tor image builds and validates its configuration locally without networking. Seventeen Python regression tests pass locally.

The JavaScript-disabled mobile browser journey exposed a real form-submission issue: `Referrer-Policy: no-referrer` caused Chromium to submit `Origin: null`. The header now uses `same-origin`; cross-origin checks and session-bound CSRF validation remain enforced. The complete setup/listing/buyer/order/profile/login journey passed after the fix.

Playwright contains 126 preview cases across Chromium/Firefox/WebKit plus one database-backed mobile journey. CI uploads failure traces/screenshots and HTML reports. The release workflow reruns the complete suite before creating private draft OCI image artifacts, checksums and provenance/SBOM metadata. It never deploys to a live host.

## Completion branch focused checks — 2026-10-08

These are local checks, not a final all-green CI record. CI run `37809875136` at `8f54ba1` failed on test fixtures, browser expectations, a vendor-dashboard archive/restore nonce omission, Compose validation fixtures and the authentic Tor rehearsal HTTP probe. The operations backup/restore tests passed within that failed run.

- At `a367cb0`, seven targeted Chromium 320 px and PostgreSQL browser checks passed in 23.3 s, including vendor-dashboard archive/restore and seeded catalog state.
- The fixture repair `c3d9cc8` (integrated as `ca7efa3`) passed 16 selected market race tests in 16.827 s with real PostgreSQL and no skips, followed by the host reset-command race test in 3.740 s. The real-Core Shape-B multipart journey compiled but was not run in this local check.
- Bitcoin physical shipping (`c614ccd`) and recovery (`ffea3a8`) each passed six selected PostgreSQL race suites without skips, in 7.735 s and 7.950 s. These pin exact saved-success replay, unchanged timestamps and effect counts, current party/password/session authorization, TOTP and persisted lost-commit recovery.
- Admin review hide/restore (`4914963`) passed three PostgreSQL race suites in 3.199 s without skips, including replay after a later restore and no second TOTP spend.
- Go vet and module verification passed at `c614ccd`. Later leaves still need the consolidated final gate.
- Operations fixture repair `fc04916` (integrated as `2a489da`) passed all ten Compose config combinations, scanner tests 11/11 and wallet-password tests 6/6. The full installer run passed 122/123, with the descendant-cleanup test blocked by sandbox access to process metadata; that same test passed individually with access in 2.013 s. The two changed installer cases also passed individually.

Alpha.1 has a real `/healthz` route. Its authentic Tor upgrade/rollback rehearsal timed out on the host-to-loopback-published app path; the revised harness resolves Compose's actual port, bypasses local HTTP proxies and retains failure diagnostics. No passing Tor deployment rehearsal, public-chain run or hardware-wallet compatibility is inferred from these checks.

- Additional confirmed receipt leaves: Bitcoin bump request (`ecd99df`) 9.022 s; admin settings (`d02755d`) 8.856 s; listing (`d8ee8ad`) 4.948 s plus tightened assertions 2.786 s; staff dispositions (`44fd6f7`) 68.109 s; PGP sign-in/cleanup (`dc625d3`) 8.260 s; payout addresses (`0c6d428`) 6.850 s. Collision/drain checks (`0b09274`) passed 24 top-level race groups in 16.641 s. These were real-PostgreSQL runs without skips.
- At `0c6d428`, the integrated 22-test receipt/adjacent race selector passed in 24.659 s with no skips. Retained local log: `/tmp/opsecmkt-completion7-combined-receipts.log`. The initial sandbox-only attempt was unable to bind a local fixture port; the actual database/HTTP run above had local access.
- Formatting, `go vet ./...` and `go mod verify` passed at `0c6d428` after the integrated receipt gate.
- Deadline/intake settings (`e000059`, source `20bd0bc`) passed the final focused real-PG race selector in 7.916 s without skips, including eight current-actor interleavings. Fee-setting receipts (`29390fc`, source `86c56a0`) passed seven top-level race tests plus five authorization subtests in 5.276 s with no skips, including original fee guards/concurrent first saves and no Core RPC on exact saved replay.
- Formatting, vet and module verification passed again at `29390fc` after integrating both payment-settings leaves.

- Escrow staff request receipts (`b775f69`) passed seven selected real-PG race groups in 10.309 s, without skips, including the original decision/request guards and a real simulated-wallet build-failure episode.
- Vendor queue/draft cancellation (`270ba15`) passed nine selected real-PG race groups in 9.795 s, without skips. Notification audit/disclosure and smaller staff/preview notes (`2525b26`) passed 16 groups in 11.550 s, without skips.
- Party concessions (`0197055`, source `adb47b0`) passed 16 selected real-PG race groups in 28.079 s, without skips. This includes 20 concession/resolution races, forged ordinary transition refusals, changed amounts, pooled and BTC/XMR outcomes, lost-ACK replay and adjacent staff-read/payout privacy behavior.

Full CI `37824485038` at `108d319` is **failed**, not completion evidence. Real Bitcoin regtest, all ten Compose configurations, installer, both container runtime modes, Tor image and simulated escrow browser journeys passed. Playwright passed 532 cases and failed one raw payout POST fixture; Go failures were missing password/nonce fixtures and an unscoped receipt count. Operations passed its encrypted restore roundtrip but the internal-restore cancellation confirmation was swallowed by a version probe. The authentic Tor upgrade found no host port for an app attached only to an internal bridge. The revised harness (`428580d`) detaches only the version probe from stdin and uses the Linux host's direct private bridge path after checking network isolation and absence of published ports. Shell syntax, the stdin regression and backup-space output/error checks passed; the real Docker rehearsal still must prove reachability in CI.

- BTC/XMR rebroadcast receipts (`c52b7f5`, source `e5fb4a0`) passed the focused real-PG race selector in 12.714 s, including original restore and relay-marker guards, no POST/replay RPC, and unchanged watcher state after a later re-hold. Earlier new fixture sequencing/count errors were corrected before this final pass.
- Monero wallet-destruction receipts (`b951eb5`, source `872b938`) passed three real-PG race tests in 5.835 s without skips. They cover the real form, original destruction/restore guards, lost acknowledgment, current party/password/session/TOTP eligibility, unchanged timestamps/effect counts and no wallet RPC. Recovery-gate replay keeps an already-destroyed wallet intact; it is not a full older-database restore experiment.
- CI fixture-only repair (`d43f678`, source `da40f77`) passed seven real-PG race groups in 5.885 s, without skips: the failed payout/provider tests, exact signature nonce refusal and payout-address receipt guards.

- Consolidated receipt/concession/queue/privacy gate at the `b951eb5` source tree plus the corrected notice expectation passed 27 top-level real-PG race tests in 35.204 s, with no skips. The first run failed only because the new notice assertion counted the status alert but not the existing identical page title; the expected count now includes both and still verifies reload consumes the notice. Log: `/tmp/opsecmkt-completion8-combined-final.log`.
- `go vet ./...` and `go mod verify` passed after that consolidated gate. The repaired provider-disabled browser POST then passed the actual database Chromium journey and setup, 2/2 in 5.6 s, with JavaScript disabled; log `/tmp/opsecmkt-completion8-payments-browser-final.log`. The first browser launch lacked its disposable database and exited before tests; the successful run used a fresh database, now removed. This is targeted evidence; full CI still must be rerun.

- A-111 timestamp pins (`7575133`, source `1914401`) passed the three-test real-PG race selector in 4.952 s without skips: 20 fresh hostile-DSN pools, real-row rendered UTC markers across all requested surfaces, distinct anchored Created/Last change labels, and the existing same-key extra-UID/third-party-certification renewal case with PGP sign-in enabled. No production code changed.

- A-112 (`950163b`, source `4a2e2f0`) passed 30 focused real-PG race tests in 18.076 s without skips. The forced failed-requeue/suspension row-lock interleave ends held; tests also cover restore/manual/large holds, failed and in-flight outcomes, historical markers after restoration, empty addresses, wallet ambiguity and receipt replay. Log: `/tmp/opsecmkt-a112-focused.log`.
- The combined final PG selector at `950163b` did **not** pass: Docker's filesystem became read-only, the database returned unexpected EOF and then refused connections. The task-owned container could not restart after freeing 1.18 GB of old task-owned compiler outputs. Log: `/tmp/opsecmkt-completion8-final-integration.log`. This infrastructure interruption does not replace the focused passes; full CI remains required.
- `go vet ./...` and `go mod verify` passed at `950163b` after the database interruption. A fresh lower-tier read-only adversarial review of A-112 found no actionable bypass, state regression or lock inversion in the suspension/requeue/recording paths.
- A-131 (source `1906c41`) passed the two new preview-only Playwright tests across all six configured projects, 12/12 in 14.7 s, with no database configured. The contrast table measures 12 named text/surface token pairs at ≥7:1 and records archived-card art as the exception; reduced-motion emulation disables the product-card transition and keeps auto scrolling. This is focused preview evidence, not a universal contrast or keyboard audit.

- Full CI `37831102118` at `597515c` finished **failed**. Installer, both container runtime modes, all ten Compose configurations, Tor image, real Bitcoin regtest and simulated escrow browser journeys passed. Operations passed all Python helper regressions and both actual encrypted restore paths, then the UTF-8 rehearsal failed because its client wrapper added connection flags to `pg_dump --version`. The authentic Tor rehearsal found the deprecated container-inspect IP field empty after all services became healthy. The browser suite passed 543 cases and failed two expectations superseded by quiet-draft behavior. Go failures were rowless disputed/resolved fixtures, the old draft-notification expectation and a suspended-recipient requeue missing address confirmation.
- Harness follow-ups (`2cc5812`, source `ba8f458`; `e5dbc57`, source `d056146`) use Docker network-inspect endpoint addresses while retaining exact network/container identity, internal-only and no-published-port checks, and generate PostgreSQL version probes without connection flags or stdin attachment. Integrated shell syntax and wrapper regressions passed; 35 Tor helper/configuration tests passed in 6.197 s. The first Python command used package names for a non-package tests directory and ran no tests; the corrected discovery command produced the 35-test pass. Actual Docker rehearsals remain for CI because the local daemon filesystem is still read-only.
- A-165 browser fixture follow-up (`a52dff5`, source `08aa15e`) asserts quiet cancellation and no incoming draft while preserving vendor order history and unread-navigation coverage through a real demotion notification. Database-project list-only discovery passed; these two repaired journeys have not been rerun locally.
- A-166 staff-read fixture ruling: its explicit criterion is a disputes row; the alpha.1 schema already has unique dispute records and current dispute actions write state plus record atomically. The rowless hand-written fixtures are corrected rather than widening the production privacy rule. Escrow fixture source `a544aed` passed formatting/diff checks; real-PG execution remains for CI.
- General staff fixtures (source `c34c4d1`) now include realistic Open/resolved dispute records, and the cancelled flagged vendor-order fixture has its matching payment-address record. Existing outsider/ordinary-order denials remain. The updated access matrix also deletes the dispute record and pins staff refusal on a rowless state; this database assertion is pending CI. The suspended requeue test now pins a 409 before the separately address-confirmed requeue; the draft transition test pins zero vendor notices.
- Fixture source at `223d27e` compiled (`go test -run '^$'`, no tests run). Subsequent final source at `1224fd3` passed vet and the rendered records/notice registry race selector, two tests in 3.813 s without skips. This selector needs no database; no local database coverage is implied. Formatting and diff checks passed.

- Full CI `37834479092` at `9fea142` finished **failed only on the authentic Tor upgrade rehearsal** (plus its required aggregator). Both PostgreSQL-backed Go suites, 545 Playwright cases, six wallet browser cases, real Bitcoin regtest, simulated escrow browser journeys, installer, both container modes, Tor image and all ten Compose configurations passed. Operations passed 324 Python tests, both actual encrypted restore paths, client-wrapper regressions and the actual UTF-8 migration rehearsal.
- Final Tor follow-up (`d0b4959`, source `dbf1dee`; `976840d`, source `42ed380`) resolves the canonical container ID, requests verbose network endpoint inspection and retains exact network identity, private IPv4, internal-only and no-published-port guards. Earlier attribution to Docker API deprecation was an inference, not a demonstrated cause. Offline reproduction found that the rehearsal’s `umask 077` removed source read/traverse bits during `tar -xf`; Docker copies those files as root before running the app as UID 65532. Both initial and replacement Git archives now preserve tracked modes via `tar -xpf`, without changing private environment file permissions. Bounded endpoint/state diagnostics expose no environment or full container configuration.
- Final integrated Tor helper/configuration selector passed 37 tests in 6.625 s, including actual Git-archive extraction controls and unchanged mode-600 environment files. Shell syntax and diff checks passed. Local Docker remains unavailable; the revised actual Tor upgrade/rollback is pending CI. This does not prove onion reachability or public-chain operation.
