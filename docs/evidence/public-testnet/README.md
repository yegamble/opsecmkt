# Public test-network escrow records

`scripts/test-escrow-public.sh` (A-446, P4-R16) writes one directory here per passing run on public signet or
testnet4: `<UTC date>-<commit, 12 hex>/<chain>/manifest.json` and `transcript.log`. The manifest names the full
commit, chain and `signet_challenge`, the node's version, image and digest, start and end heights, each leg's txids and
the transcript's sha256; both files passed the script's secret scan before they were written. How to run it:
[Escrow node set-up and the public-network smoke](../../operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).

No record is here yet: no public-network run of Bitcoin escrow has been recorded (A-447 is the owner's).
