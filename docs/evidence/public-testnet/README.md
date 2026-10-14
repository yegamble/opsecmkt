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
The operator selects either XMRChain or Librenode with an exact HTTPS block URL. Both the runner and Go test
independently fetch the selected host's block API and refuse a missing or mismatching response; there is no
automatic fallback. Public API availability and a syncing cloud node are preparation, not passing escrow evidence.
The [2026-10-10 public stagenet digital claim](2026-10-10-ac71878dfde4/stagenet-xmr/README.md) passed on frozen ac71878dfde4: actual 0.01 sXMR funding and confirmed release, ten confirmations each, and exact planned claimant receipt. It includes observed sync/storage, verified private recovery and cloud cleanup. Public refunds remain unmeasured; the offline stagenet fork is a separate evidence tier.

## Publishing supplemental cloud records

The runners write the proof manifest and transcript; supplemental cloud records are reviewed separately.
Publish measured capacity, cost, storage, timestamps and cleanup results. Omit cloud account and resource IDs,
resource names and tags, host addresses, region or VPC identifiers, SSH fingerprints and storage bucket names.
Never copy an unfiltered provider response into public evidence.

When removing metadata from an existing supplemental record, list the omitted fields and reason in its
`redaction` object. Preserve the proof transcript, its hash and the measured results. The records from
2026-10-09 and 2026-10-10 have had cloud identifiers removed; prior Git revisions retain their original values.
The redacted cleanup record reports the observed deletion but no longer identifies a provider resource.

Run `python3 -m unittest discover -s tests -p 'test_public_evidence_privacy.py' -v` before publishing.
This check limits supplemental cloud JSON fields; review prose and new evidence formats separately.
