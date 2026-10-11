# Ledger Flex via Sparrow 2.5.5: attempted, failed before signing, unresolved

This is the "Ledger Flex diagnostic" section of
[PR #43](https://github.com/yegamble/opsecmkt/pull/43), copied from the PR body on
2026-10-11 (fetched with the GitHub REST API) with only this heading changed.
Result: attempted, failed before signing, unresolved. Only software signing is
verified; see the [Sparrow software-signing record](2026-10-11-f229a1a65172/README.md).

## Ledger Flex diagnostic (incomplete, 2026-10-11)

The Flex public test account export succeeded with firmware 1.7.0 and Bitcoin Test 2.5.1 using official Sparrow 2.5.5. Sparrow's practice receiving address matched the application. Device address display was refused by Sparrow's host integration because the per-order origin has five derivation levels and its display path permits at most four; this was not a device rejection.

The ordinary practice signing attempt reported `NoSuchElementException`; no exact stack trace was obtained. Its PSBT has three input derivations but no global xpubs. A private diagnostic copy added only the exact public global xpub/origin records, with every derived key checked against the original witness script and all original transaction/input/output maps preserved. This copy was not signed: USB opening still failed after Ledger Wallet was closed. It does not establish production hardware-download compatibility.

No Ledger signature or settlement funding occurred. The manual journey was deliberately stopped as an incomplete failure before the first practice signature, and no PASS manifest exists. Physical signing, on-device address confirmation, and public hardware-wallet settlement evidence remain open. Existing Sparrow software evidence and the CI result at `8bfebc08661364d03e2ddc0bdefa609b31bbd59d` remain unchanged.
