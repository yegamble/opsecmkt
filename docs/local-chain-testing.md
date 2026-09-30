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

The September 2026 local runs verified these exact archives (macOS ARM, then Linux x86_64 for war-room A-103):

| Archive | SHA-256 | Verified signing key fingerprint |
| --- | --- | --- |
| `bitcoin-31.1-arm64-apple-darwin.tar.gz` | `16a097c09fbd7eb78b240ce1dae123663ea2e5e377cfd6a951e71e227e23cf2f` | `CFB16E21C950F67FA95E558F2EEB9F5CC09526C1` (fanquake) |
| `monero-mac-armv8-v0.18.5.1.tar.bz2` | `dba08921841e675384ce019fd7c93b59fe7b1e6edaa0a3cf0e3253e263f61864` | `81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92` (binaryFate) |
| `bitcoin-31.1-x86_64-linux-gnu.tar.gz` | `b80d9c3e04da78fb6f0569685673418cf686fadba9042d926d13fb87ff503f9e` | `CFB16E21C950F67FA95E558F2EEB9F5CC09526C1` (fanquake) |
| `monero-linux-x64-v0.18.5.1.tar.bz2` | `22a7dda7b0cb699fdd6b7674c3b4a4465b337cc98a54983523b759e1e7cc9958` | `81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92` (binaryFate) |

Bitcoin's [31.1 SHA256SUMS](https://bitcoincore.org/bin/bitcoin-core-31.1/SHA256SUMS)
has a [detached signature](https://bitcoincore.org/bin/bitcoin-core-31.1/SHA256SUMS.asc);
its [builder keys](https://github.com/bitcoin-core/guix.sigs/tree/main/builder-keys)
include fanquake. Monero's [signed hashes](https://www.getmonero.org/downloads/hashes.txt)
verify against its [binaryFate key](https://github.com/monero-project/monero/blob/master/utils/gpg_keys/binaryfate.asc).
Use an isolated GPG keyring for these checks. A manifest hash match alone is not a
signature verification. Bitcoin's file contains multiple signatures: unavailable
keys do not invalidate another verified signature, but a bad signature must be
investigated. Pin and check the expected signer, not merely the presence of a
checksum line. The exact manifests, signatures and verification record from each
local run are in ignored `artifacts/real-chain/tools/` on the machine that ran it.

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

The script refuses a nonempty state directory, allocates loopback ports (any free
ones, or consecutive free ones from `--first-port PORT` when another run on the host
uses the usual ranges), creates
new wallets, mines spendable test coins, and pre-funds the marketplace wallets
for payout fees. It writes a protected `runtime.json`, process logs and
`setup-result.json` inside that directory. The runtime file contains local RPC
credentials; keep it private. The processes stay running for the browser tests.
Existing user configuration and wallets are untouched.

The funding wallet is named `buyer`; the marketplace wallet is `opsecmkt`.
Monero's fee reserve is unlocked before setup reports success. Network fees are
deducted from every payout on both coins (Monero `subtract_fee_from_outputs`,
releases included since A-232). `tests/e2e/chain-journey.spec.ts` expects every
payout it checks (`verifyReceipt`, on both coins) to arrive as its amount less a
non-zero fee, in exactly the transaction the market recorded, and the fixture's
`received` reads a Monero payout's fee from the market wallet
(`get_transfer_by_txid`). A real-chain run of these expectations is cited only once
it is recorded in the [evidence table](verification.md#evidence-table) (war-room
A-103).

On this offline stagenet fork a Monero transfer costs about 0.002 XMR, more than
the 0.001 XMR minimum automatic payout: a Monero payout (release, refund or
return) below about 0.002 XMR fails with wallet error -16 "tx not possible", which
the market records as refused before broadcast (nothing sent; an administrator can
requeue it). Use larger amounts for Monero payouts on the fork.

## Run the browser journeys

Use the development PostgreSQL container from the README, create a new database,
and install the usual browser dependencies (`npm ci`, then
`npx playwright install chromium`). The suite also needs the `psql` client on
`PATH`. The database must be fresh for each run.

```sh
docker exec opsecmkt-dev-db createdb -U opsecmkt opsecmkt_chain_e2e
export E2E_CHAIN_DATABASE_URL='postgres://opsecmkt:local-dev-only@127.0.0.1:55432/opsecmkt_chain_e2e?sslmode=disable'
export LOCAL_CHAIN_RUNTIME="$PWD/artifacts/local-chains/runtime.json"
npm run test:e2e:chain
```

Set `LOCAL_CHAIN_CURRENCIES=BTC` or `XMR` to exercise one currency; the default
runs both. Set `LOCAL_CHAIN_APP_PORT` to serve the application on a port other
than 18122. The suite provisions application users and listings through real
forms, pays with the real funding wallet, mines confirmations, checks fulfillment
and verifies the resulting release/refund transactions in the recipient wallet.
It does not fabricate deposits or change paid order states directly in SQL; it
only reads the run's `payouts` rows and the watcher's last poll time.

The tests run in order and each later one depends on the first (which initializes
the marketplace):

1. Partial deposits, confirmation, fulfillment and release, then a moderator's
   dispute refund.
2. An overpaid order: completing sends the price to the vendor and the excess back
   to the buyer's saved address, one transaction each.
3. A return larger than the price: completing sends the release and holds the
   return; the watcher never sends it; an administrator releases it on `/admin`
   with their password, and it is sent once.
4. An underpaid order: the buyer cancels it and the confirmed deposit is refunded
   once to their saved address.

Each payout is checked on the order page after a reload, in its `payouts` row (no
ambiguous, failed or stuck send anywhere) and in the recipient wallet, and again
after further watcher passes, so a payout sent twice fails the run.

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
