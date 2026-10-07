#!/usr/bin/env bash
# Bitcoin escrow node set-up (war-room A-446, P4-R16; docs/operator-guide.md, "Escrow node set-up script"). For the
# operator's own escrow node on regtest, signet or testnet4; mainnet ("main") and testnet3 ("test") are refused, as
# the app refuses them (escrowChains in internal/market/escrow_tx.go).
#
#   scripts/escrow-node-setup.sh --chain signet [--cli 'docker exec -u bitcoin escrow-node bitcoin-cli']
#       [--rpc-host bitcoin] [--rpc-port 38332] [--escrow-user opsecmkt-escrow] [--pooled-user opsecmkt-pooled | --no-pooled]
#       [--arbiter-wallet opsecmkt-arbiter] [--watch-wallet opsecmkt-escrow-watch]
#
# Set-up (the default) talks to the node only through the operator's own bitcoin-cli (--cli, default bitcoin-cli; the
# script adds -chain=<chain>), so the operator's cookie or credentials stay where bitcoin-cli finds them. It
#   1. refuses unless the node reports --chain, and on signet prints its signet_challenge and whether it is public
#      signet's;
#   2. creates the arbiter wallet (a descriptor wallet with private keys, unencrypted) and the watch-only wallet
#      (private keys disabled, blank), both loaded at every node start, or keeps existing ones after checking exactly
#      that (it never overwrites a wallet);
#   3. reads the arbiter wallet's master key fingerprint as the app does (getrawchangeaddress, getaddressinfo);
#   4. generates the escrow user's password (and the pooled user's) here with openssl, and prints once, on standard
#      output, the bitcoin.conf lines (rpcauth, rpcwhitelistdefault=0 and each user's rpcwhitelist) and the .env lines
#      (ESCROW_BTC_RPC_URL and the wallet names; BITCOIN_RPC_URL for the pooled user). Nothing is written to a file:
#      copy the lines, add the bitcoin.conf ones and restart bitcoind.
#
#   ESCROW_BTC_RPC_URL=... [BITCOIN_RPC_URL=...] scripts/escrow-node-setup.sh --chain signet --verify
#
# --verify (after the restart) reads the two URLs from the environment (never from .env), sends each to curl on its
# standard input (never printed) and checks what the app needs and what A-440 requires refused: the escrow user answers
# getblockchaininfo on --chain (any other chain, or none, is refused before any wallet call, as in set-up mode) and
# getwalletinfo on both wallets, and is refused (HTTP 403) listdescriptors and
# gethdkeys; the pooled user answers getblockchaininfo and gets HTTP 403 (or "wallet not found" when it is another
# node) for listdescriptors, gethdkeys and walletprocesspsbt on the arbiter wallet's path.
#
# The whitelists are escrowRPCMethods (internal/market/escrow_rpc.go) and pooledRPCMethods
# (internal/market/payments_bitcoin.go); tests/test_escrow_public_script.py keeps them equal.
set -euo pipefail

escrow_methods=getblockchaininfo,getnetworkinfo,getrawchangeaddress,getaddressinfo,createpsbt,utxoupdatepsbt,importdescriptors,getdescriptorinfo,deriveaddresses,gettxout,keypoolrefill,walletprocesspsbt,descriptorprocesspsbt,combinepsbt,finalizepsbt,testmempoolaccept,sendrawtransaction,getwalletinfo,listreceivedbyaddress,gettransaction,listtransactions,listsinceblock
pooled_methods=getblockchaininfo,getwalletinfo,loadwallet,getnetworkinfo,getnewaddress,listreceivedbyaddress,gettransaction,sendtoaddress,validateaddress,getbalances
public_signet_challenge=512103ad5e0edad18cb1f0fc0d28a3d4f1f3e445640337489abb10404f2d1e086be430210359ef5021964fe22d6f8e05b2463c9540ce96883fe3b278760f048f5189f2e6c452ae

usage() {
  sed -n '6,8p' "$0" | sed 's/^# \{0,1\}//' >&2
  echo "   or: ESCROW_BTC_RPC_URL=... [BITCOIN_RPC_URL=...] $0 --chain <regtest|signet|testnet4> --verify" >&2
  exit 2
}
die() { echo "$*" >&2; exit 1; }

chain='' cli='bitcoin-cli' host='bitcoin' port='' escrow_user='opsecmkt-escrow' pooled_user='opsecmkt-pooled'
arbiter='opsecmkt-arbiter' watch='opsecmkt-escrow-watch' verify=no
while [[ $# -gt 0 ]]; do
  case "$1" in
    --chain) chain="${2:-}"; shift 2 ;;
    --cli) cli="${2:-}"; shift 2 ;;
    --rpc-host) host="${2:-}"; shift 2 ;;
    --rpc-port) port="${2:-}"; shift 2 ;;
    --escrow-user) escrow_user="${2:-}"; shift 2 ;;
    --pooled-user) pooled_user="${2:-}"; shift 2 ;;
    --no-pooled) pooled_user=''; shift ;;
    --arbiter-wallet) arbiter="${2:-}"; shift 2 ;;
    --watch-wallet) watch="${2:-}"; shift 2 ;;
    --verify) verify=yes; shift ;;
    -h|--help) usage ;;
    *) echo "Unknown argument: $1" >&2; usage ;;
  esac
done

case "$chain" in
  regtest) default_port=18443 ;;
  signet) default_port=38332 ;;
  testnet4) default_port=48332 ;;
  main|mainnet) die "Refusing: Bitcoin escrow never runs on mainnet (escrow uses test-network key versions and coin type 1)." ;;
  test|testnet|testnet3) die "Refusing: testnet3 is not an escrow chain; use testnet4 or signet." ;;
  '') echo "Give --chain regtest, signet or testnet4." >&2; usage ;;
  *) die "Refusing: --chain must be regtest, signet or testnet4." ;;
esac
port="${port:-$default_port}"
for v in "$escrow_user" "$pooled_user" "$arbiter" "$watch"; do
  [[ "$v" =~ ^[A-Za-z0-9._-]*$ ]] || die "Refusing: user and wallet names may hold only letters, digits, '.', '_' and '-'."
done
[[ -n "$escrow_user" && -n "$arbiter" && -n "$watch" ]] || die "Refusing: the escrow user and the wallet names cannot be empty."
[[ "$escrow_user" != "$pooled_user" ]] || die "Refusing: the escrow user and the pooled user must differ."
[[ "$arbiter" != "$watch" ]] || die "Refusing: the arbiter wallet and the watch-only wallet must differ."
[[ "$host" =~ ^[A-Za-z0-9.:_-]+$ && "$port" =~ ^[0-9]{1,5}$ ]] || die "Refusing: --rpc-host or --rpc-port is not a host name or port."

# field <name>: a scalar JSON field of the answer on standard input (bitcoin-cli's or curl's), quotes stripped.
field() { tr -d '\n' | sed -n "s/.*\"$1\": *\"\{0,1\}\([^\",}]*\)\"\{0,1\}.*/\1/p"; }

if [[ $verify == yes ]]; then
  command -v curl >/dev/null || die "curl is required"
  [[ -n "${ESCROW_BTC_RPC_URL:-}" ]] || die "Set ESCROW_BTC_RPC_URL in the environment (the line the set-up printed); this script never reads .env."
  failed=0
  # rpc <url> <path> <method> <params>: the answer, then a last line with the HTTP status. The URL reaches curl on its
  # standard input and is never printed.
  rpc() {
    printf 'url = "%s%s"\n' "${1%/}" "$2" | curl -sS --max-time 30 --noproxy '*' -H 'Content-Type: application/json' \
      --data "{\"jsonrpc\":\"1.0\",\"id\":\"setup\",\"method\":\"$3\",\"params\":$4}" -w '\n%{http_code}' --config - 2>/dev/null || echo
  }
  status_of() { tail -n 1 <<<"$1"; }
  check() { # check <description> <ok: yes|no>
    if [[ $2 == yes ]]; then echo "ok: $1"; else echo "FAILED: $1" >&2; failed=1; fi
  }
  answer=$(rpc "$ESCROW_BTC_RPC_URL" "" getblockchaininfo '[]')
  got=$(sed '$d' <<<"$answer" | field chain)
  case "$got" in main|test) die "Refusing: the escrow node reports chain $got; Bitcoin escrow runs only on regtest, signet or testnet4." ;; esac
  # As set-up mode does, nothing else is asked of a node on another chain, or one that names none (QA5W3A-9).
  [[ $(status_of "$answer") == 200 && $got == "$chain" ]] ||
    die "Refusing: the escrow node reports chain '${got:-nothing}' (HTTP $(status_of "$answer")), not $chain; nothing else was asked."
  check "escrow user: getblockchaininfo answers chain $chain" yes
  for w in "$arbiter" "$watch"; do
    answer=$(rpc "$ESCROW_BTC_RPC_URL" "/wallet/$w" getwalletinfo '[]')
    keys=$(sed '$d' <<<"$answer" | field private_keys_enabled)
    want=true
    [[ $w == "$watch" ]] && want=false
    check "escrow user: getwalletinfo on $w answers, private_keys_enabled $want (got ${keys:-none}, HTTP $(status_of "$answer"))" \
      "$([[ $(status_of "$answer") == 200 && $keys == "$want" ]] && echo yes || echo no)"
  done
  for m in 'listdescriptors [true]' 'gethdkeys [{"private":true}]'; do
    answer=$(rpc "$ESCROW_BTC_RPC_URL" "/wallet/$arbiter" "${m%% *}" "${m#* }")
    check "escrow user: ${m%% *} on $arbiter refused with HTTP 403 (got HTTP $(status_of "$answer"))" \
      "$([[ $(status_of "$answer") == 403 ]] && echo yes || echo no)"
  done
  if [[ -n "${BITCOIN_RPC_URL:-}" ]]; then
    answer=$(rpc "$BITCOIN_RPC_URL" "" getblockchaininfo '[]')
    check "pooled user: getblockchaininfo answers (HTTP $(status_of "$answer"))" "$([[ $(status_of "$answer") == 200 ]] && echo yes || echo no)"
    for m in 'listdescriptors [false]' 'gethdkeys [{"private":false}]' 'walletprocesspsbt ["",false]'; do
      answer=$(rpc "$BITCOIN_RPC_URL" "/wallet/$arbiter" "${m%% *}" "${m#* }")
      code=$(sed '$d' <<<"$answer" | sed -n 's/.*"error": *{[^}]*"code": *\(-\{0,1\}[0-9]*\).*/\1/p')
      check "pooled user: ${m%% *} on $arbiter refused (HTTP 403, or RPC -18 on another node; got HTTP $(status_of "$answer")${code:+, RPC $code})" \
        "$([[ $(status_of "$answer") == 403 || $code == -18 ]] && echo yes || echo no)"
    done
  else
    echo "not checked: the pooled user (BITCOIN_RPC_URL is not set; set it when the pooled payments use this node)"
  fi
  if [[ $failed -ne 0 ]]; then
    echo "FAILED: the escrow node is not set up as docs/operator-guide.md describes" >&2
    exit 1
  fi
  echo "PASS: the escrow node answers the escrow user on $chain and refuses what it must"
  exit 0
fi

command -v openssl >/dev/null || die "openssl is required"
read -ra cli_cmd <<<"$cli"
[[ ${#cli_cmd[@]} -gt 0 ]] || die "Refusing: --cli is empty."
core() { "${cli_cmd[@]}" "-chain=$chain" "$@"; }

info=$(core getblockchaininfo) || die "bitcoin-cli could not reach the node on $chain (check --cli)."
got=$(field chain <<<"$info")
[[ "$got" == "$chain" ]] || die "Refusing: the node reports chain '${got:-nothing}', not $chain."
echo "node: chain $chain, height $(field blocks <<<"$info")" >&2
if [[ $chain == signet ]]; then
  challenge=$(field signet_challenge <<<"$info")
  if [[ "$challenge" == "$public_signet_challenge" ]]; then
    echo "node: signet_challenge is public signet's" >&2
  else
    echo "node: signet_challenge $challenge is NOT public signet's: this is a private signet" >&2
  fi
fi

loaded=$(core listwallets)
on_disk=$(core listwalletdir)
has() { grep -q "\"$2\"" <<<"$1"; }
# wallet <name> <arbiter|watch>: create it, or load and keep an existing one, then check its kind.
wallet() {
  local name=$1 kind=$2 winfo keys desc
  if has "$loaded" "$name"; then
    echo "wallet $name: loaded already; kept" >&2
  elif has "$on_disk" "$name"; then
    core loadwallet "$name" true >/dev/null || die "Refusing: wallet $name exists but could not be loaded."
    echo "wallet $name: existed on disk; loaded (and set to load at every node start), kept" >&2
  elif [[ $kind == arbiter ]]; then
    core -named createwallet "wallet_name=$name" load_on_startup=true >/dev/null
    echo "wallet $name: created (descriptor wallet with private keys, unencrypted, loaded at every node start)" >&2
  else
    core -named createwallet "wallet_name=$name" disable_private_keys=true blank=true load_on_startup=true >/dev/null
    echo "wallet $name: created (watch-only: private keys disabled, blank, loaded at every node start)" >&2
  fi
  winfo=$(core "-rpcwallet=$name" getwalletinfo)
  keys=$(field private_keys_enabled <<<"$winfo")
  desc=$(field descriptors <<<"$winfo")
  [[ $desc == true ]] || die "Refusing: wallet $name is not a descriptor wallet."
  if [[ $kind == arbiter ]]; then
    [[ $keys == true ]] || die "Refusing: wallet $name has private keys disabled; the arbiter wallet must hold the market's key."
    ! grep -q '"unlocked_until"' <<<"$winfo" || die "Refusing: wallet $name is encrypted; the escrow watcher co-signs without a passphrase."
  else
    [[ $keys == false ]] || die "Refusing: wallet $name holds private keys; the watch-only wallet must not."
  fi
}
wallet "$arbiter" arbiter
wallet "$watch" watch
change=$(core "-rpcwallet=$arbiter" getrawchangeaddress)
fingerprint=$(core "-rpcwallet=$arbiter" getaddressinfo "$change" | field hdmasterfingerprint)
[[ "$fingerprint" =~ ^[0-9a-f]{8}$ ]] || die "Refusing: could not read the arbiter wallet's master key fingerprint."

# rpcauth <user> <password>: user:salt$HMAC-SHA256(key salt, password), as Core's share/rpcauth/rpcauth.py writes it.
rpcauth() {
  local salt hash
  salt=$(openssl rand -hex 16)
  hash=$(printf %s "$2" | openssl dgst -sha256 -hmac "$salt" | sed 's/^.* //')
  printf '%s:%s$%s' "$1" "$salt" "$hash"
}
escrow_pass=$(openssl rand -hex 32)
[[ -z "$pooled_user" ]] || pooled_pass=$(openssl rand -hex 32)

echo "arbiter wallet $arbiter: master key fingerprint $fingerprint (record it with the wallet's backup; the app pins it at its first start)" >&2
cat <<EOF
# ---- Shown once; this script wrote none of it anywhere. Copy it now. ----
# bitcoin.conf of the escrow node ($chain): add these lines (top level, or under [$chain]), then restart bitcoind.
# The operator user (bitcoin-cli's cookie or your own rpcauth) has no rpcwhitelist line: rpcwhitelistdefault=0 keeps it
# unrestricted; every user the app connects as gets exactly its line.
rpcauth=$(rpcauth "$escrow_user" "$escrow_pass")
EOF
[[ -z "$pooled_user" ]] || echo "rpcauth=$(rpcauth "$pooled_user" "$pooled_pass")"
echo "rpcwhitelistdefault=0"
echo "rpcwhitelist=$escrow_user:$escrow_methods"
[[ -z "$pooled_user" ]] || echo "rpcwhitelist=$pooled_user:$pooled_methods"
cat <<EOF
# .env of the market (with ESCROW_BTC=on when you turn escrow on):
ESCROW_BTC_RPC_URL=http://$escrow_user:$escrow_pass@$host:$port
ESCROW_BTC_ARBITER_WALLET=$arbiter
ESCROW_BTC_WATCH_WALLET=$watch
EOF
if [[ -n "$pooled_user" ]]; then
  echo "# Only if the pooled payments use this node (run escrow on a node of its own where you can):"
  echo "BITCOIN_RPC_URL=http://$pooled_user:$pooled_pass@$host:$port"
fi
echo "# After the restart: ESCROW_BTC_RPC_URL=...${pooled_user:+ BITCOIN_RPC_URL=...} $0 --chain $chain --verify"
