# Real isolated-chain tests

The market's only payment path is per-order escrow, so the real-chain checks are the escrow ones. They run actual
Bitcoin Core and Monero binaries with locally mined blocks and test coins. No public blockchain, faucet, mainnet funds
or existing wallet is used, and none of these commands is a deployment check. (The earlier pooled-wallet chain suite,
`setup-local-chains.py`'s node setup, its Playwright chain configuration and its smoke script were deleted with the
pooled wallet; their dated results are kept as history under *Prior build* in [verification.md](verification.md#prior-build-pooled-wallet-removed-local-chain-evidence-2026-09-23-to-2026-09-30).)

| What | Command | Runs in CI |
| --- | --- | --- |
| Bitcoin escrow on a real Bitcoin Core 31.1 regtest node (practice signing, funding, claims, refunds, restart and reorg legs, dispute decisions, the signer) | `scripts/test-escrow-regtest.sh` (`--private-signet` for the two-node private signet legs) | `escrow-regtest` |
| Monero escrow on a real `monerod` and `monero-wallet-rpc` 0.18.5.1 fakechain (15 legs: wallets, funding, physical orders, relay loss and restart, top-up, reorg, outside spend, re-request, restore, lookahead, wrong secret, destroy, refund below the fee) | `scripts/test-escrow-xmr-regtest.sh` | `escrow-xmr-regtest` |
| Monero escrow on an offline stagenet fork | `scripts/test-escrow-xmr-stagenet.sh` | no |
| Escrow forms in a browser with JavaScript off, against simulated nodes (not chain evidence) | `npm run test:e2e:escrow`, `npm run test:e2e:escrow-xmr` | `browser-escrow` |
| A public signet or testnet4 smoke, by hand with funds you provide | `scripts/test-escrow-public.sh`; `scripts/test-escrow-xmr-public-stagenet.py` | never |

Each script's header lists the nodes it needs (Docker images, ports, RPC users) and the environment variables it reads,
including `TEST_DATABASE_URL` for a **disposable** PostgreSQL database. The Docker commands that start the nodes are
in those headers, in `.github/workflows/ci.yaml` and, for the public-network smokes, in the
[operator guide](operator-guide.md#escrow-node-set-up-and-the-public-network-smoke). Pin the node images to the tags
the installer pins (Bitcoin Core 31.1, Monero v0.18.5.1; [node image versions](operator-guide.md#node-image-versions)).
Monero's `--regtest` fakechain is used for Monero escrow only because escrow's network guard accepts a fakechain or
stagenet daemon and nothing else.

Never point the application at these disposable wallets when restoring or testing real data. Evidence from a run is
only as good as its record: see [verification.md](verification.md) for what each recorded run covers.

## Stopping leftover processes

`scripts/setup-local-chains.py --stop STATE_DIR` is the only part of the old harness that remains. It stops the
processes an earlier pooled-chain run registered in `STATE_DIR/runtime.json` (it checks each PID against its
binary and private data path first, so a stale PID cannot stop an unrelated process) and keeps their data for
inspection. It refuses to set anything up. To run the retired pooled suite you need the revision that had it (for
example the commit `5a0dabcb` and its `docs/local-chain-testing.md`); this build refuses a database that suite wrote.
