#!/usr/bin/env bash
# Bitcoin escrow smoke on a PUBLIC test network (war-room A-446, P4-R16): the owner's run (A-447), by hand, never in
# CI. It runs the legs of TestEscrowPublicSmoke (internal/market/escrow_public_test.go) against the operator's own
# Bitcoin Core node on public signet or testnet4 and, when every leg passed, writes the evidence record
# docs/evidence/public-testnet/<UTC date>-<commit, 12 hex>/<chain>/ (manifest.json and transcript.log) to commit.
#
# Refused before any wallet call, here and again in the Go test (publicChainRefusal): regtest, mainnet ("main"),
# testnet3 ("test"), any other chain, and a private signet: a node on signet counts as public signet only when its
# getblockchaininfo reports public signet's signet_challenge (Bitcoin Core's default, below). The regtest and
# private-signet harness is scripts/test-escrow-regtest.sh; its guards stay as they are.
#
# Legs (one App, two escrows funded together, so the run waits for confirmations twice):
#   practice-sign: a buyer's and a vendor's Core wallets (BIP48 accounts made on the node for the run) register
#     their keys and activate them by signing the practice transaction (never broadcastable);
#   claim: an escrow funded at the price completes its digital order; the vendor claims, signs in its own wallet, and
#     the market co-signs, broadcasts and follows it to ESCROW_PUBLIC_CONFIRMATIONS; each output is checked on chain;
#   refund: an escrow funded short of the price is cancelled by the buyer, who signs the refund; the market
#     co-signs, broadcasts and follows it to the confirmations; it pays the refund address pinned at checkout.
# The same body runs on regtest as TestEscrowRegtestSmoke (scripts/test-escrow-regtest.sh).
#
# Needs: the node set up with scripts/escrow-node-setup.sh (the escrow user's whitelist), synced, with peers; a
# disposable PostgreSQL database; Go, curl, git, docker (for the node image's digest) and openssl; a clean
# checkout (the manifest names the commit that ran). Environment:
#   ESCROW_PUBLIC_OPERATOR_RPC_URL  the operator user (creates the run's wallets: escrow-smoke-<random>-…; unloaded after)
#   ESCROW_PUBLIC_APP_RPC_URL       the escrow user, restricted by its rpcwhitelist line (what the App reaches Core as)
#   ESCROW_PUBLIC_FUNDING_WALLET    a wallet on that node holding at least 0.0016 test coins (from a faucet); the
#                                   escrows are paid from it and every settlement pays back to it
#   ESCROW_PUBLIC_NODE_IMAGE        the image the node runs (bitcoin/bitcoin:31.1); its digest goes in the manifest
#   TEST_DATABASE_URL               a disposable PostgreSQL database (never a market's)
#   ESCROW_PUBLIC_CONFIRMATIONS     confirmations funding and settlements wait for (1 to 100, default 1)
#   ESCROW_PUBLIC_FEE_RATE          sat/vB of the funding sends and every settlement (1 to 100, default 2; on testnet4
#                                   set it above the node's mempoolminfee)
#   ESCROW_PUBLIC_WAIT_MINUTES      how long each wait for confirmations may take (10 to 1440, default 240)
#   ESCROW_PUBLIC_LOG               the transcript (default artifacts/real-chain/test-escrow-public-<chain>-<UTC time>.log)
#   BITCOIN_MAX_TIP_AGE, APP_MODE   passed to the App as set (on testnet4 a block can take longer than 2h)
#
# URLs reach curl on its standard input and are never printed. Before anything is written to docs/evidence, the
# transcript, and then the record, pass a secret scan: no RPC or database password from the URLs above, no
# rpcpassword or rpcauth line, no salt$hash, no wallet passphrase, no cookie, no URL carrying a password, no serialized
# private key (tprv, xprv, test-network WIF). A record that fails it is deleted. The manifest holds: the full commit,
# start and finish times, chain and signet_challenge, the node's version, image and digest, the Go version, the
# confirmations and fee rate, start and end heights, each leg's result and txids, and the transcript's sha256.
set -euo pipefail

PUBLIC_SIGNET_CHALLENGE=512103ad5e0edad18cb1f0fc0d28a3d4f1f3e445640337489abb10404f2d1e086be430210359ef5021964fe22d6f8e05b2463c9540ce96883fe3b278760f048f5189f2e6c452ae
SMOKE_LEGS=(practice-sign claim refund)

# chain_refusal <chain> <signet_challenge>: why the smoke refuses that node, or nothing for public signet and testnet4.
# The Go test's publicChainRefusal is the same table.
chain_refusal() {
  case "$1" in
    testnet4) ;;
    signet) [[ "$2" == "$PUBLIC_SIGNET_CHALLENGE" ]] || echo "a private signet (its signet_challenge is not public signet's)" ;;
    regtest) echo "regtest is a local chain, not a public network" ;;
    main) echo "mainnet is never used for this smoke" ;;
    test) echo "testnet3 is not an escrow chain" ;;
    *) echo "not a chain escrow runs on" ;;
  esac
}

# rpc <url> <method>: the node's JSON-RPC answer, or nothing; the URL reaches curl on its standard input.
rpc() {
  printf 'url = "%s"\n' "${1%/}" | curl -sS --max-time 30 --noproxy '*' -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"1.0\",\"id\":\"public\",\"method\":\"$2\",\"params\":[]}" --config - 2>/dev/null || true
}
field() { sed -n "s/.*\"$1\": *\"\{0,1\}\([^\",}]*\)\"\{0,1\}[,}].*/\1/p"; }

# url_password <url>: the password a URL carries, or nothing.
url_password() {
  local rest=${1#*://}
  rest=${rest%%/*}
  [[ $rest == *@* ]] || return 0
  rest=${rest%@*}
  [[ $rest == *:* ]] && printf '%s\n' "${rest#*:}"
  return 0
}

# secret_scan <file>...: fails, naming the file and what was found (never the secret), when any file holds a literal
# from SECRET_VALUES (the passwords of the URLs this run was given) or something shaped like a secret.
SECRET_VALUES=()
secret_scan() {
  local f v name re
  local -a checks=(
    'rpcpassword|rpcpassword'
    'an rpcauth line|rpcauth='
    "rpcauth's salt\$hash|[0-9a-f]{32}\\\$[0-9a-f]{64}"
    'a cookie|__cookie__|\.cookie'
    'a wallet passphrase|walletpassphrase|"passphrase"|passphrase='
    'a URL carrying a password|[A-Za-z][A-Za-z0-9+.-]*://[^/@[:space:]:]+:[^/@[:space:]]+@'
    'a serialized private key|[tx]prv[1-9A-HJ-NP-Za-km-z]{100,}'
    "a test-network WIF private key|(^|[[:space:]\"'=:])c[1-9A-HJ-NP-Za-km-z]{51}([[:space:]\"',]|\$)"
  )
  for f in "$@"; do
    for v in "${SECRET_VALUES[@]+"${SECRET_VALUES[@]}"}"; do
      if [[ -n $v ]] && grep -qF -- "$v" "$f"; then
        echo "secret scan: $f holds a password this run was given" >&2
        return 1
      fi
    done
    for name in "${checks[@]}"; do
      re=${name#*|}
      if grep -qE -- "$re" "$f"; then
        echo "secret scan: $f holds ${name%%|*}" >&2
        return 1
      fi
    done
  done
}

# leg_txid <transcript> <leg> <role>: the txid the Go test logged as "escrow-public: txid <leg> <role> <txid>".
leg_txid() {
  sed -n "s/.*escrow-public: txid $2 $3 \([0-9a-f]\{64\}\)\$/\1/p" "$1" | tail -n 1
}

sha256_of() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# write_manifest <dir> <transcript>: copies the transcript into <dir> and writes <dir>/manifest.json from the M_*
# variables and the transcript's txid lines, then scans both. Every value must have its expected form (and the chain
# must be a public one), or nothing is left in <dir>.
write_manifest() {
  local dir=$1 log=$2 name re v leg
  local -a forms=(
    'M_COMMIT|^[0-9a-f]{40}$'
    'M_STARTED|^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
    'M_FINISHED|^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
    'M_CHAIN|^(signet|testnet4)$'
    'M_CHALLENGE|^([0-9a-f][0-9a-f]+)?$'
    'M_NODE_VERSION|^[A-Za-z0-9.:/()_ -]{1,80}$'
    'M_NODE_IMAGE|^[A-Za-z0-9./:@_-]{1,200}$'
    'M_NODE_DIGEST|^[A-Za-z0-9./:_-]{1,200}@sha256:[0-9a-f]{64}$'
    'M_GO_VERSION|^[A-Za-z0-9. /_-]{1,80}$'
    'M_CONFIRMATIONS|^[0-9]{1,3}$'
    'M_FEE_RATE|^[0-9]{1,3}$'
    'M_START_HEIGHT|^[0-9]{1,9}$'
    'M_END_HEIGHT|^[0-9]{1,9}$'
  )
  for name in "${forms[@]}"; do
    re=${name#*|} name=${name%%|*}
    v=${!name-}
    [[ $v =~ $re ]] || { echo "manifest: $name has an unexpected form; nothing written" >&2; return 1; }
  done
  # BSD regex engines cap counted repetitions at 255. The byte pattern above
  # and this length check preserve the bound on macOS as well as Linux.
  (( ${#M_CHALLENGE} <= 400 )) || { echo "manifest: M_CHALLENGE has an unexpected form; nothing written" >&2; return 1; }
  local why
  why=$(chain_refusal "$M_CHAIN" "$M_CHALLENGE")
  [[ -z $why ]] || { echo "manifest: refusing chain $M_CHAIN: $why; nothing written" >&2; return 1; }
  (( M_END_HEIGHT >= M_START_HEIGHT )) || { echo "manifest: the end height is below the start height; nothing written" >&2; return 1; }
  local claim_funding claim_settlement refund_funding refund_settlement
  claim_funding=$(leg_txid "$log" claim funding) claim_settlement=$(leg_txid "$log" claim settlement)
  refund_funding=$(leg_txid "$log" refund funding) refund_settlement=$(leg_txid "$log" refund settlement)
  for v in "$claim_funding" "$claim_settlement" "$refund_funding" "$refund_settlement"; do
    [[ $v =~ ^[0-9a-f]{64}$ ]] || { echo "manifest: the transcript lacks a leg's txid; nothing written" >&2; return 1; }
  done
  for leg in "${SMOKE_LEGS[@]}"; do
    grep -q "escrow-public: leg $leg passed" "$log" || { echo "manifest: leg $leg did not pass; nothing written" >&2; return 1; }
  done
  [[ ! -e $dir ]] || { echo "manifest: $dir exists; nothing written (a record is never overwritten)" >&2; return 1; }
  secret_scan "$log" || { echo "manifest: the transcript failed the secret scan; nothing written" >&2; return 1; }
  mkdir -p "$dir"
  cp "$log" "$dir/transcript.log"
  local sum network="public signet"
  sum=$(sha256_of "$dir/transcript.log")
  [[ $M_CHAIN == testnet4 ]] && network="public testnet4"
  cat >"$dir/manifest.json" <<EOF
{
  "record": "opsecmkt Bitcoin escrow public-network smoke (A-446, P4-R16)",
  "network": "$network",
  "commit": "$M_COMMIT",
  "started": "$M_STARTED",
  "finished": "$M_FINISHED",
  "chain": "$M_CHAIN",
  "signet_challenge": "$M_CHALLENGE",
  "node": {"version": "$M_NODE_VERSION", "image": "$M_NODE_IMAGE", "image_digest": "$M_NODE_DIGEST"},
  "go": "$M_GO_VERSION",
  "confirmations": $M_CONFIRMATIONS,
  "fee_rate_sat_vb": $M_FEE_RATE,
  "heights": {"start": $M_START_HEIGHT, "end": $M_END_HEIGHT},
  "legs": {
    "practice-sign": {"result": "passed", "txids": {}},
    "claim": {"result": "passed", "txids": {"funding": "$claim_funding", "settlement": "$claim_settlement"}},
    "refund": {"result": "passed", "txids": {"funding": "$refund_funding", "settlement": "$refund_settlement"}}
  },
  "transcript": {"file": "transcript.log", "sha256": "$sum"},
  "secret_scan": "passed"
}
EOF
  if ! secret_scan "$dir/manifest.json" "$dir/transcript.log"; then
    rm -rf "$dir"
    echo "manifest: the record failed the secret scan and was deleted" >&2
    return 1
  fi
}

main() {
  [[ $# -eq 0 ]] || { echo "Usage: $0 (configured by the environment; see the comment at the top of this script)" >&2; exit 2; }
  local v
  for v in ESCROW_PUBLIC_OPERATOR_RPC_URL ESCROW_PUBLIC_APP_RPC_URL ESCROW_PUBLIC_FUNDING_WALLET ESCROW_PUBLIC_NODE_IMAGE TEST_DATABASE_URL; do
    [[ -n "${!v:-}" ]] || { echo "Set $v (see the comment at the top of this script)." >&2; exit 2; }
  done
  for v in curl go git docker; do
    command -v "$v" >/dev/null || { echo "$v is required" >&2; exit 2; }
  done
  local confirmations=${ESCROW_PUBLIC_CONFIRMATIONS:-1} fee_rate=${ESCROW_PUBLIC_FEE_RATE:-2} wait=${ESCROW_PUBLIC_WAIT_MINUTES:-240}
  [[ $confirmations =~ ^[0-9]{1,3}$ ]] && (( confirmations >= 1 && confirmations <= 100 )) || { echo "ESCROW_PUBLIC_CONFIRMATIONS must be 1 to 100" >&2; exit 2; }
  [[ $fee_rate =~ ^[0-9]{1,3}$ ]] && (( fee_rate >= 1 && fee_rate <= 100 )) || { echo "ESCROW_PUBLIC_FEE_RATE must be 1 to 100" >&2; exit 2; }
  [[ $wait =~ ^[0-9]{1,4}$ ]] && (( wait >= 10 && wait <= 1440 )) || { echo "ESCROW_PUBLIC_WAIT_MINUTES must be 10 to 1440" >&2; exit 2; }
  [[ $ESCROW_PUBLIC_FUNDING_WALLET =~ ^[A-Za-z0-9._-]+$ ]] || { echo "ESCROW_PUBLIC_FUNDING_WALLET must be a wallet name (letters, digits, '.', '_', '-')" >&2; exit 2; }

  # The chain, before anything else touches the node.
  local info chain='' challenge='' first=''
  for v in ESCROW_PUBLIC_OPERATOR_RPC_URL ESCROW_PUBLIC_APP_RPC_URL; do
    info=$(rpc "${!v}" getblockchaininfo)
    chain=$(field chain <<<"$info")
    challenge=$(field signet_challenge <<<"$info")
    local why
    why=$(chain_refusal "$chain" "$challenge")
    if [[ -z $chain ]]; then
      echo "Refusing: the node at $v did not answer getblockchaininfo." >&2
      exit 1
    elif [[ -n $why ]]; then
      echo "Refusing: the node at $v reports chain $chain: $why. This smoke runs only on public signet or testnet4." >&2
      exit 1
    fi
    if [[ -n $first && $first != "$chain:$challenge" ]]; then
      echo "Refusing: the two URLs reach nodes on different chains." >&2
      exit 1
    fi
    first="$chain:$challenge"
  done

  cd "$(dirname "$0")/.."
  if [[ -n $(git status --porcelain -- . ':(exclude)docs/evidence/public-testnet') ]]; then
    echo "Refusing: the checkout has uncommitted changes; the manifest must name the commit that ran. Commit or stash them." >&2
    exit 1
  fi
  local commit digest
  commit=$(git rev-parse HEAD)
  digest=$(docker image inspect --format '{{join .RepoDigests " "}}' "$ESCROW_PUBLIC_NODE_IMAGE" 2>/dev/null | tr ' ' '\n' | grep '@sha256:' | head -n 1 || true)
  [[ -n $digest ]] || { echo "Refusing: no digest for ESCROW_PUBLIC_NODE_IMAGE (docker image inspect; pull the image the node runs)." >&2; exit 1; }
  local dir log day
  day=$(date -u +%Y-%m-%d)
  dir="docs/evidence/public-testnet/$day-${commit:0:12}/$chain"
  [[ ! -e $dir ]] || { echo "Refusing: $dir exists (a record is never overwritten)." >&2; exit 1; }
  log="${ESCROW_PUBLIC_LOG:-artifacts/real-chain/test-escrow-public-$chain-$(date -u +%Y%m%dT%H%M%SZ).log}"
  mkdir -p "$(dirname "$log")"
  SECRET_VALUES=()
  for v in ESCROW_PUBLIC_OPERATOR_RPC_URL ESCROW_PUBLIC_APP_RPC_URL TEST_DATABASE_URL; do
    SECRET_VALUES+=("$(url_password "${!v}")")
  done

  local started start_height status=0
  started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  start_height=$(rpc "$ESCROW_PUBLIC_APP_RPC_URL" getblockchaininfo | field blocks)
  echo "Bitcoin escrow smoke on public $chain from height $start_height (commit $commit); transcript: $log"
  set +e
  ESCROW_PUBLIC_OPERATOR_RPC_URL=$ESCROW_PUBLIC_OPERATOR_RPC_URL ESCROW_PUBLIC_APP_RPC_URL=$ESCROW_PUBLIC_APP_RPC_URL \
    ESCROW_PUBLIC_FUNDING_WALLET=$ESCROW_PUBLIC_FUNDING_WALLET TEST_DATABASE_URL=$TEST_DATABASE_URL \
    ESCROW_PUBLIC_CONFIRMATIONS=$confirmations ESCROW_PUBLIC_FEE_RATE=$fee_rate ESCROW_PUBLIC_WAIT_MINUTES=$wait \
    go test -count=1 -timeout "$(( 2 * wait + 240 ))m" -run '^TestEscrowPublicSmoke$' -v ./internal/market/ 2>&1 | tee "$log"
  status=${PIPESTATUS[0]}
  set -e
  local failed=0 leg
  if grep -q -- "--- PASS: TestEscrowPublicSmoke " "$log"; then
    echo "PASS: TestEscrowPublicSmoke"
  else
    echo "FAILED: TestEscrowPublicSmoke did not pass (a skip is not a pass); see $log" >&2
    failed=1
  fi
  for leg in "${SMOKE_LEGS[@]}"; do
    if grep -q "escrow-public: leg $leg passed" "$log"; then echo "PASS: leg $leg"; else echo "FAILED: leg $leg; see $log" >&2; failed=1; fi
  done
  if ! secret_scan "$log"; then
    echo "FAILED: the transcript failed the secret scan; nothing is written to docs/evidence. Delete $log." >&2
    exit 1
  fi
  if [[ $status -ne 0 || $failed -ne 0 ]]; then
    echo "FAILED: go test exited $status; nothing is written to docs/evidence; see $log" >&2
    exit 1
  fi
  M_COMMIT=$commit M_STARTED=$started M_FINISHED=$(date -u +%Y-%m-%dT%H:%M:%SZ) M_CHAIN=$chain M_CHALLENGE=$challenge \
    M_NODE_VERSION=$(rpc "$ESCROW_PUBLIC_APP_RPC_URL" getnetworkinfo | field subversion) M_NODE_IMAGE=$ESCROW_PUBLIC_NODE_IMAGE \
    M_NODE_DIGEST=$digest M_GO_VERSION=$(go version) M_CONFIRMATIONS=$confirmations M_FEE_RATE=$fee_rate \
    M_START_HEIGHT=$start_height M_END_HEIGHT=$(rpc "$ESCROW_PUBLIC_APP_RPC_URL" getblockchaininfo | field blocks) \
    write_manifest "$dir" "$log"
  echo "PASS: practice signing, one escrow claimed and one refunded, each confirmed $confirmations time(s), on public $chain; record in $dir (commit it)"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
