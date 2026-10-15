# Real isolated-chain tests

The only current payment path is Monero per-order custodial escrow. Real-chain checks run actual Monero binaries with local test coins; they are not deployment checks. Development and CI never use mainnet.

| What | Command | Runs in CI |
| --- | --- | --- |
| Monero escrow on real daemon and wallet-rpc fakechain | `scripts/test-escrow-xmr-regtest.sh` | `escrow-xmr-regtest` |
| Offline stagenet address/sync checks | `scripts/test-escrow-xmr-stagenet.sh` | no |
| Monero forms with JavaScript disabled and simulated RPC | `npm run test:e2e:escrow-xmr` | browser escrow |
| Public stagenet digital claim with dedicated test wallets | `scripts/test-escrow-xmr-public-stagenet.py` | never |

Each script's header lists the nodes it needs (Docker images, ports, RPC users) and the environment variables it reads,
including `TEST_DATABASE_URL` for a **disposable** PostgreSQL database. The Docker commands that start the nodes are
in those headers, in `.github/workflows/ci.yaml` and, for the public-network smokes, in the
[Monero operator guide](operator-guide.md#monero-escrow). Pin the node images to the tags
the installer pins (Monero v0.18.5.1; [node image versions](operator-guide.md#node-image-versions)).
Monero's `--regtest` fakechain is used for Monero escrow because, on a test-network market, escrow's network guard
accepts a fakechain or stagenet daemon and nothing else.

Never point the application at these disposable wallets when restoring or testing real data. Evidence from a run is
only as good as its record: see [verification.md](verification.md) for what each recorded run covers.

## Stopping leftover processes

`scripts/setup-local-chains.py --stop STATE_DIR` is the only part of an older local-chain harness that remains. It
stops the processes an earlier run registered in `STATE_DIR/runtime.json` (it checks each PID against its binary and
private data path first, so a stale PID cannot stop an unrelated process) and keeps their data for inspection. It
refuses to set anything up.
