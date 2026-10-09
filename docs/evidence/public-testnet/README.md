# Public test-network escrow records

`scripts/test-escrow-public.sh` (A-446, P4-R16) writes one directory here per passing run on public signet or
testnet4: `<UTC date>-<commit, 12 hex>/<chain>/manifest.json` and `transcript.log`. The manifest names the full
commit, chain and `signet_challenge`, the node's version, image and digest, start and end heights, each leg's txids and
the transcript's sha256; both files passed the script's secret scan before they were written. How to run it:
[Escrow node set-up and the public-network smoke](../../operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).

[Public signet — 2026-10-09, frozen 9885beef29d3](2026-10-09-9885beef29d3/signet/README.md): practice signing, funded claim and buyer refund passed with real confirmations. Supplemental records measure sync/storage and Tor private-broadcast observations. Public testnet4 remains unrun.

`scripts/test-escrow-xmr-public-stagenet.py` writes the separate `stagenet-xmr/` directory only after a funded public
digital-claim journey passes. It records a fetched HTTPS explorer block matched to the own daemon, actual Docker
image/container identity, transaction hashes, funding and settlement stage times, and the planned amount received
with ten confirmations. Its public transcript contains validated proof fields; raw output stays private outside
the repository. A digital-claim sample is not refund timing or validation of A-482's experimental 24-hour threshold.
No funded public Monero result is recorded yet; the offline stagenet fork is a different evidence tier.
