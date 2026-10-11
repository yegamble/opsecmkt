#!/usr/bin/env bash
# The pooled adapter is retired. Refuse before any wallet or node mutation.
set -euo pipefail
printf '%s\n' 'Pooled payment smoke tests were retired with migration107. Use scripts/test-escrow-regtest.sh for current escrow checks; use the prior drain-capable revision for legacy pooling.' >&2
exit 2
