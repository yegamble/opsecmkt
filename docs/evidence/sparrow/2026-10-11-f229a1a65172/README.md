# Sparrow 2.5.5 per-order signing on regtest

On 2026-10-11, the verified official Sparrow 2.5.5 ARM64 GUI signed disposable
buyer and vendor keys against an isolated Bitcoin Core 31.1 regtest node on
DigitalOcean. No public-network coins or local blockchain storage were used.
Application source was frozen at `f229a1a65172986065bbab4f3aa24a09725695ad`;
the added opt-in harness is `TestEscrowRegtestSparrowManualJourney` in
[`internal/market/escrow_regtest_sparrow_test.go`](../../../../internal/market/escrow_regtest_sparrow_test.go).
The exact as-run harness source is preserved at `9a61d3a04081043a2a651e7564e0529ab92d5064`;
its source hash is recorded separately from the application revision. The later
harness/evidence correction labels Sparrow's installer digest as a DMG hash,
without changing its value or the proof results.

| Handoff | Result |
| --- | --- |
| Buyer practice | GUI address matched; own partial `SIGHASH_ALL` signature accepted and per-order key activated |
| Vendor practice | GUI address matched; own partial `SIGHASH_ALL` signature accepted and per-order key activated |
| Vendor digital release, order index 2, `/0/0` | Ordinary PSBT signed in Sparrow, accepted through `/escrow/sign`, co-signed by the real market signer, mempool accepted, broadcast and confirmed; 99,682 sats received, 318 sats fee |
| Buyer sibling stray refund, order index 3, `/0/3` | Hardware-format PSBT with verified `non_witness_utxo` signed using the Sparrow software key, accepted through `/escrow/hardware-sign`, co-signed, mempool accepted, broadcast and confirmed; 69,682 sats received, 318 sats fee |

Both settlements reached three confirmations during the harness. Core verified
the unchanged transaction and every output against the frozen payout plan.
A separate read-only block check also matched every output and the funding
transaction IDs spent. The signer seed and extended private keys were absent
from the database and signer log. The live journey passed in 283.95 seconds.

[Allowlisted journey log](allowlisted-log.txt), [manifest](manifest.json), [independent confirmed-output check](confirmed-output-check.json),
[public handoffs](handoffs.json), [raw public transactions](public-transactions.json), and [application verification](verification.json), and [supplemental descriptor import](descriptor-import.json)
contain only allowlisted public evidence. GUI actions, address comparisons,
Sparrow version and release artifact provenance are explicitly operator-attested; Core
transaction checks are machine-checked. Writable handoff files alone do not
prove that a particular wallet produced a signature.

Sparrow wallets were opened from native wallet JSON containing fresh local test
roots and the application's rooted public cosigners. The cosigners had explicit
root key-origin metadata. This verifies GUI signing and address derivation at the
full per-order path. A subsequent, separate watch-only check imported the exact
application descriptor through File → Import Wallet → Output Descriptor → Import
File; its first address matched, the party path remained `m/48'/1'/0'/2'/2`,
and both rooted cosigners remained at `m`, with no observed path warning. The
original manifest's false descriptor-import flag describes the earlier signing
journey; the supplemental record attests this later GUI import. Private-key
attachment and signing from that imported watch-only wallet were not exercised.
The hardware-format result uses a
software key and establishes file/download/upload compatibility only. Physical
Ledger Flex signing, device policy registration and display checks, fee bumps,
and Sparrow signing on a public network remain unverified. A-458 remains partial.

The earlier extracted app failed a fresh integrity check and was excluded from
this proof. The mounted original installer and its `ditto` copy passed strict
deep code-signature verification; macOS accepted the copy as a notarized
Developer ID app. The installer checksum matched its manifest, whose signature
was verified against the pinned vendor fingerprint. Earlier simulated-Core
practice attempts are also excluded; that fixture accepts fake signatures only.

The task regtest droplet and its task-only SSH key were deleted after proof
and private recovery verification; absence was checked. The SSH tunnel and task
test PostgreSQL were stopped. [Cleanup record](cleanup.json) records this and the
restrictive private software-wallet archive checks. The cloud regtest blockchain
was disposable and was not downloaded.

To repeat, provide an existing mode-0700 `ESCROW_SPARROW_MANUAL_DIR` containing
`public-accounts.json` with buyer/vendor BIP48 public account keys, verified
`ESCROW_SPARROW_VERSION`, `ESCROW_SPARROW_DMG_SHA256` and
`ESCROW_SPARROW_APP_REVISION`, the three restricted regtest RPC environment
variables, and `TEST_DATABASE_URL` for an isolated PostgreSQL test database.
Run the named Go test with a timeout long enough for four GUI handoffs. Inspect
each request, compare the actual GUI address, sign and save its PSBT, then write
the address attestation and the selected settlement source (`ordinary` or
`hardware`). The test skips without manual opt-in, refuses stale handoffs and
never reads either party's private root. Preserve keys and logs privately;
never commit wallet files or RPC credentials.
