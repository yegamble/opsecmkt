# Real isolated-chain browser tests

This suite runs actual Bitcoin Core and Monero binaries and wallets, with real
local blocks and transactions. It is separate from `test:e2e:wallet`, which uses
simulated JSON-RPC responses. No public blockchain, faucet, mainnet funds or
existing wallet is used.

Bitcoin uses regtest with peer networking disabled. Monero uses an **offline
stagenet fork**, fixed difficulty 1 and one mining thread. It retains genuine
stagenet addresses and the application's network checks. It is not the public
stagenet, and its old-height protocol rules do not prove current public-network
compatibility. Monero `--regtest` is deliberately not used: it uses fakechain
network/address semantics that the application's test-network guard rejects.

## Obtain and verify the binaries

Download the CLI archives for your OS/architecture from the official
[Bitcoin Core downloads](https://bitcoincore.org/en/download/) and
[Monero downloads](https://www.getmonero.org/downloads/) pages. Extract them into
an ignored local directory only after verifying the signed checksum manifests.
No system-wide installation is needed. Do not substitute arbitrary container
images or disable the application's network guards.

The September 2026 local macOS ARM run verified these exact archives:

| Archive | SHA-256 | Verified signing key fingerprint |
| --- | --- | --- |
| `bitcoin-31.1-arm64-apple-darwin.tar.gz` | `16a097c09fbd7eb78b240ce1dae123663ea2e5e377cfd6a951e71e227e23cf2f` | `CFB16E21C950F67FA95E558F2EEB9F5CC09526C1` (fanquake) |
| `monero-mac-armv8-v0.18.5.1.tar.bz2` | `dba08921841e675384ce019fd7c93b59fe7b1e6edaa0a3cf0e3253e263f61864` | `81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92` (binaryFate) |

Bitcoin's [31.1 SHA256SUMS](https://bitcoincore.org/bin/bitcoin-core-31.1/SHA256SUMS)
has a [detached signature](https://bitcoincore.org/bin/bitcoin-core-31.1/SHA256SUMS.asc);
its [builder keys](https://github.com/bitcoin-core/guix.sigs/tree/main/builder-keys)
include fanquake. Monero's [signed hashes](https://www.getmonero.org/downloads/hashes.txt)
verify against its [binaryFate key](https://github.com/monero-project/monero/blob/master/utils/gpg_keys/binaryfate.asc).
Use an isolated GPG keyring for these checks. A manifest hash match alone is not a
signature verification. Bitcoin's file contains multiple signatures: unavailable
keys do not invalidate another verified signature, but a bad signature must be
investigated. Pin and check the expected signer, not merely the presence of a
checksum line. The exact manifests, signatures and verification record from the
local run are in ignored `artifacts/real-chain/tools/`.

## Start the disposable nodes

Requirements: Python 3, curl with HTTP Digest support, verified Bitcoin/Monero CLI
binaries, and enough local disk for the short disposable chains. Run from the
repository root, substituting your extracted binary directories:

```sh
python3 scripts/setup-local-chains.py \
  --bitcoin-bin artifacts/real-chain/tools/bitcoin-31.1/bin \
  --monero-bin artifacts/real-chain/tools/monero-aarch64-apple-darwin11-v0.18.5.1 \
  --state-dir artifacts/local-chains
```

The script refuses a nonempty state directory, allocates loopback ports, creates
new wallets, mines spendable test coins, and pre-funds the marketplace wallets
for payout fees. It writes a protected `runtime.json`, process logs and
`setup-result.json` inside that directory. The runtime file contains local RPC
credentials; keep it private. The processes stay running for the browser tests.
Existing user configuration and wallets are untouched.

The funding wallet is named `buyer`; the marketplace wallet is `opsecmkt`.
Monero's fee reserve is unlocked before setup reports success. Bitcoin fees are
deducted from payouts; Monero fees are paid on top from the market reserve.

## Run the browser journeys

Use the development PostgreSQL container from the README, create a new database,
and install the usual browser dependencies (`npm ci`, then
`npx playwright install chromium`). The database must be fresh for each run.

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_chain_e2e
export E2E_CHAIN_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_chain_e2e?sslmode=disable'
export LOCAL_CHAIN_RUNTIME="$PWD/artifacts/local-chains/runtime.json"
npm run test:e2e:chain
```

Set `LOCAL_CHAIN_CURRENCIES=BTC` or `XMR` to exercise one currency; the default
runs both. The suite provisions application users and listings through real
forms, pays with the real funding wallet, mines confirmations, checks fulfillment
and verifies the resulting release/refund transactions in the recipient wallet.
It does not fabricate deposits or change paid order states directly in SQL.

`tests/e2e/fixtures/local-chain.py` supplies `address`, `deposit`, `mine` and
`received` commands to the suite. Every operation first checks that Bitcoin is
regtest with networking off or that Monero is offline stagenet. The driver's
`env` command is only for programmatic consumption by Playwright: **its output
contains RPC credentials; do not print or save it in public test logs**.

## Stop and repeat

Stop the application/test runner first, then:

```sh
python3 scripts/setup-local-chains.py --stop artifacts/local-chains
```

Shutdown checks each recorded PID against its binary and private data/config
path before signaling it, so a stale PID cannot stop an unrelated process.
Data is preserved for inspection. A setup failure stops registered processes;
forced termination or OS failure can still require manual cleanup. Use a new
state directory and fresh database for an independent run. Never point the
application at these disposable wallets when restoring or testing real data.
