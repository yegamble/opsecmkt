# Public Monero stagenet digital-claim proof

PASS on frozen source `ac71878dfde42daed9dc7b4e883c8d24600816f3`, 2026-10-10
16:56:32–17:46:36 UTC (50m04s). The later documentation head does not replace that tested revision.
The manual runner drove checkout, actual 0.01 sXMR funding, vendor claim, and the application's watcher
build, relay and confirmation against an own public stagenet daemon and real wallet RPCs.

[Manifest](manifest.json) and [unaltered public transcript](transcript.log) are the runner's output.
The transcript contains the validated proof and its PASS scope line, not raw application logs.
Its SHA-256 is `39993af3f8e924a7b1cd2702dbd8e672849ac0c0c48af8e04aa0cbaa70d7e850`.
Both files were secret-scanned remotely and again locally, and independently reviewed.

| Event | UTC time | Observed daemon height |
| --- | --- | --- |
| Funding submitted | 16:56:53 | 2,226,289 |
| Funding confirmed, 10 confirmations | 17:18:51 | 2,226,299 |
| Vendor claim requested | 17:18:51 | 2,226,299 |
| Settlement built and relayed | 17:19:01 | 2,226,299 |
| Settlement confirmed, 10 confirmations | 17:46:35 | 2,226,309 |

Funding transaction: `fe2b54ccbe9102bc8164b691f7c57bad6a180511ee606a9dad1216257dc3dbc9`.
Release transaction: `f2518684f2102a2f13436c54ff80ea360898b2a339fc92abc6c3df039823833b`.
The dedicated funder received the exact planned claimant output, **0.00996962 sXMR**, at a fresh subaddress.
The release's network fee was 0.00003038 sXMR. Funding-to-confirmed observation was 21m58s;
claim-request-to-confirmed was 27m44s. Heights are observations at each event, not all transaction inclusion heights.

Both the Python wrapper and Go smoke fetched the selected Librenode HTTPS block API independently and
matched height 2,226,284/hash `ac3300c780ae75267e3090cbf2cec2e52fac680bd058606f87006adf103cebcf`
to the own daemon before wallet work. The manifest retains the fetched source, time and hash. The daemon
reported stagenet, hard fork 16, sync complete and 24 peers at the start and finish. Authenticated RPCs were
published only on loopback. The runner checked actual Docker image digests, ports and fresh escrow directory.
No block generation, clock or deadline changes were used.

The preceding funded attempt really confirmed a claim but failed its recorder because a watcher pass
stored the build and immediately relayed it; the transient built state was missed. That attempt emitted no
PASS manifest and is not this proof. The fix reads the unique durable build event written with the build,
refusing missing or ambiguous evidence. Its simulated-wallet PostgreSQL 17 regression passed with the race
detector, and [CI 38069776541](https://github.com/yegamble/opsecmkt/actions/runs/38069776541) passed all
22 workflow jobs, including the required gate, at `a52ce770f1b6deba08c5c0a92e64a239f8b66db0`.

## Observed sync, storage and cloud recovery

[Start settings](monero-sync-start-public.json), [sampled observations](monero-sync-public.jsonl) and
[cloud resource](cloud-resource.json) are supplemental public records. The fresh pruned node ran on four
vCPUs and 8 GiB RAM, with daemon limits of 3.5 CPUs/5 GiB and default sync batch sizing.
Its sync-start record is 2026-10-09T18:41:44.910238Z. It was still unsynced at 2026-10-10T12:36:28Z and
first observed fully synced at 14:38:01Z (height 2,226,241). Completion lies between those observations;
approximately 19h56m to the first synced sample is an upper bound, not the exact sync duration or a prediction.
The whole Monero data directory measured **7,031,277,142 bytes** at that sample (about 7.03 GB decimal).
Later samples remain around 7.03 GB. These are this host's measurements, not a universal disk requirement.

A consistent final private wallet archive was saved locally with restrictive permissions, checksum and
allowed-member checks, and no chain files. A separate fresh container reopened the archived funder with its
saved password, verified the same primary address and **0.01985132 sXMR fully unlocked**, and submitted no
transaction. The empty escrow's keys/recovery were preserved too. See the public
[backup summary](final-recovery-public-summary.json) and [read-only reopen check](final-recovery-reopen-public.json);
they contain no wallet keys, password or address. Private recovery stays gitignored, outside this public record.
Only task droplet **607700697** was deleted, after evidence and recovery verification. Its absence and the
continued presence of known unrelated droplets were checked at 17:52:01Z; see [cleanup](cloud-cleanup.json).
The observed resource rate was $0.07143/hour. No blockchain files were downloaded locally.

## Limits

This proves one public stagenet digital claim. It does not prove public refund or physical-order deadlines,
reorg recovery on the public network, Monero Tor delivery, a deployed market, Sparrow or hardware signing,
mainnet, a real-market pooled drain, or all Phase 3 journeys. The 27m44s claim sample does not measure refund
timing or validate A-482's experimental 24-hour alert threshold. Pooled removal and those other gates remain separate.
