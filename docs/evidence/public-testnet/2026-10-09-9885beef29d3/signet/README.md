# Public signet escrow — 2026-10-09

**PASS**, frozen source `9885beef29d3e222026fd68a907e94f2ac6c7041`, Bitcoin Core 31.1 and Go 1.27.2. The operator package configured and verified restricted RPC access; the app used its restricted account. `APP_MODE=tor` enabled the application proxy gate. The three smoke legs passed: practice signatures from the buyer and vendor's own Core wallets, a funded digital claim, and an underpaid buyer cancellation/refund. Both settlements confirmed once and their on-chain outputs matched the planned outputs. This sample covers Core party wallets; it supplies no Sparrow, physical-device, public testnet4, pooled-payment or Monero proof.

[Manifest](manifest.json) and [unaltered transcript](transcript.log) passed the runner's actual-password and pattern scan. Transcript SHA-256: `328994b5ff0e412a6f0da9d326a639167e9342a5db1191141834d8e6028aef20`. The transcript retains transient RPC EOFs and the initial not-yet-visible private settlement; polling recovered and the run passed in 2,081.62 seconds. Total wrapper time was 17:44:51–18:20:29 UTC (35 minutes 38 seconds).

| Leg | Funding confirmation | Settlement confirmation |
|---|---|---|
| Vendor digital claim | Height 325662 | Height 325663 |
| Buyer underpaid refund | Height 325662 | Height 325663 |

The manifest contains all four transaction IDs. The [broadcast observations](broadcast-observations.jsonl) sampled approximately every ten seconds:

- Funding wallet sends used ordinary peer relay through Tor. They were first logged at 17:46:00 and first observed confirmed at 18:00:16 UTC, about 14 minutes 16 seconds later.
- Both settlements were first logged at 18:00:37 and observed in the private-broadcast queue at 18:00:38. The refund's onion peer send/ack epochs differ by eight seconds. The claim had one sampled private peer send; no acknowledgement was sampled, and its sampled peer address was not classified as onion. The observer omits peer addresses, raw transaction hex and credentials.
- The refund and claim were first known by the recipient wallet at approximately 18:00:50 and 18:01:04 (Core timestamps). The observer saw them outside the queue at 18:01:01 and 18:01:11. Both were observed confirmed at 18:20:14, about 19 minutes 37 seconds after first settlement logging. These are sampled observations, not precise relay latency or a privacy guarantee.

The Bitcoin container had only the [internal node network](bitcoin-networks.json); [network inspection](isolated-network.json) confirms it was internal. [Core network information](tor-network-info.json) shows IPv4, IPv6 and onion proxies at `10.199.42.2:9050`; [Tor version](tor-version.txt) is 0.4.9.14. All smoke transactions occurred after Tor bootstrap and isolation. Bulk initial validation used direct outbound networking before that transition; this is not an entirely Tor-synced node or a deployed website/onion rehearsal.

[Sync samples](sync-samples.jsonl) begin at 16:00:37 UTC, already at height 231700; the first recorded fully synced state is 17:29:31 at height 325660. This 88-minute-54-second sampled window excludes the earlier download/startup and does not establish total fresh-sync duration. Maximum sampled `size_on_disk` was 5,726,143,884 bytes; that RPC field measures block files. It finished at 1,927,024,168 bytes after pruning. The [full Bitcoin directory measurement](cloud-storage.json) was 6,683,278,090 bytes before recovery backup, including chainstate and task wallets. Sampled maxima are not guaranteed absolute peaks.

[Cloud resource record](cloud-resource.json): temporary DigitalOcean host, resized to four CPUs/eight GiB, $0.07143/hour at that size. The passing proof was exported, and ten wallet copies in a private gitignored recovery archive passed SQLite integrity checks. The temporary droplet was deleted and its absence verified. No blockchain files were downloaded locally. Private wallet data is excluded from this directory.

Public Monero stagenet remains pending. Its separate harness has offline/refusal and authenticated daemon checks, but the fresh sync estimate was 5.1 days and the explorer returned HTTP 502; those services were stopped. No public Monero funding or refund-timing evidence is supplied here. Pooled removal still requires an empty real-market drain report and its other ledger gates.
