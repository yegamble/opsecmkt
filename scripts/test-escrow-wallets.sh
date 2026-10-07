#!/usr/bin/env bash
# Manual wallet-compatibility evidence for Bitcoin escrow (A-450, ruling P4-R12): a real local chain plus a real
# Electrum, run by hand, never in CI. REGTEST ONLY: the node it starts has no network at all (--network none).
#
# It downloads Electrum 4.8.2 over HTTPS, checks the tarball's sha256 against the pin below and its detached signature
# against the pinned fingerprint of ThomasV's release key (fetched from the Electrum repository at the release tag, in
# a temporary GnuPG home), installs it into a throwaway venv (the five PyPI packages below are pinned by the sha256
# hashes in the signed tarball's contrib/deterministic-build files; nothing else is fetched), starts its own Bitcoin
# Core 31.1 regtest container and runs scripts/escrow-wallets.py, offline, as a library user of Electrum. Legs, each
# reported PASS or FAILED:
#   core-shape-a         a Core party wallet (BIP48 account) signs the shape (a) PSBT the market serves today; market
#                        signature, combine, finalize, testmempoolaccept and broadcast succeed with the unsigned txid
#   electrum-shape-b     an Electrum keystore from a BIP39 mnemonic at m/48h/1h/0h/2h/i signs the shape (b) variants
#                        (bare cosigners; own origin only; every derivation with rooted global xpubs); the market signs
#                        through Core descriptorprocesspsbt; combine, finalize, testmempoolaccept and broadcast succeed
#                        and every final txid equals the unsigned one
#   es2-accepts-electrum the ES-2 paste check (Go, checkSettlementPaste, go test -run TestEscrowWalletFixtures on
#                        this run's files) accepts every Electrum signature and the Core one (A-456b relaxed its
#                        derivation refusal, P4-R13; until then it refused the Electrum ones); the text is logged
#   electrum-seed-type   Electrum's own seed type derives multisig keys at m/1h and keystore.from_seed takes no path
#
# Needs docker (the bitcoin/bitcoin:31.1 image), curl, gpg, python3 (with venv and a C compiler for libsecp256k1), go,
# git, tar and sha256sum, the Go module cache (the Go leg runs with GOPROXY=off) and about 100 MB in the temporary
# directory. No database.
#
#   ./scripts/test-escrow-wallets.sh [--update-fixtures]
#
# --update-fixtures copies this run's signed Electrum PSBTs and their manifest (public keys only) to
# internal/market/testdata/escrow-wallets/. ESCROW_WALLETS_CONTAINER names the container (default
# escrow-wallets-regtest; an existing container of that name is refused, never reused or removed);
# ESCROW_WALLETS_TMPDIR is where the temporary directory goes (default $TMPDIR or /tmp). The log is ESCROW_WALLETS_LOG
# (default artifacts/real-chain/a450-<commit>-wallets.log, gitignored), with a header naming the Electrum release and
# sha256, the node version, the commit and the finish time. The container, the venv and every temporary file are removed
# on exit. Seeds, mnemonics and private keys exist only in memory and on the throwaway node; none is printed or written.
set -euo pipefail

ELECTRUM_VERSION=4.8.2
ELECTRUM_SHA256=f38cee333c866986cdfb304428fa7487affc429c3853fc80e9822bd420bbc229
ELECTRUM_URL="https://download.electrum.org/$ELECTRUM_VERSION/Electrum-$ELECTRUM_VERSION.tar.gz"
# Thomas Voegtlin's release signing key, primary key fingerprint (the signature checked must be by this key).
ELECTRUM_SIGNER=6694D8DE7BE8EE5631BED9502BD5824B7F9470E6
ELECTRUM_KEY_URL="https://raw.githubusercontent.com/spesmilo/electrum/$ELECTRUM_VERSION/pubkeys/ThomasV.asc"
# From PyPI, by the sha256 hashes in the signed tarball's contrib/deterministic-build files (the rest of Electrum's
# dependencies ship in its packages/ directory): built from source, setuptools and electrum-ecc (it compiles
# libsecp256k1); as wheels, cryptography with cffi and pycparser (Electrum needs cryptography or pycryptodomex).
SOURCE_PINS=(setuptools==80.9.0 electrum-ecc==0.0.8)
WHEEL_PINS=(pycparser==2.23 cffi==2.0.0 cryptography==46.0.7)
CORE_IMAGE=bitcoin/bitcoin:31.1
LEGS=(core-shape-a electrum-shape-b es2-accepts-electrum electrum-seed-type)

cmdline="$*"
update=0
for arg in "$@"; do
  case "$arg" in
    --update-fixtures) update=1 ;;
    *) echo "usage: $0 [--update-fixtures]" >&2; exit 2 ;;
  esac
done
for tool in docker curl gpg python3 go git tar sha256sum; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done
container="${ESCROW_WALLETS_CONTAINER:-escrow-wallets-regtest}"
[[ "$container" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || { echo "ESCROW_WALLETS_CONTAINER is not a container name" >&2; exit 2; }
if docker container inspect "$container" >/dev/null 2>&1; then
  echo "Refusing: a container named $container exists; remove it or set ESCROW_WALLETS_CONTAINER." >&2
  exit 2
fi

cd "$(dirname "$0")/.."
repo="$PWD"
commit="$(git rev-parse HEAD)"
sha="$(git rev-parse --short=12 HEAD)"
if [[ -n "$(git status --porcelain -- internal scripts)" ]]; then
  sha="$sha-dirty"
  commit="$commit (uncommitted changes under internal/ or scripts/)"
fi
log="${ESCROW_WALLETS_LOG:-artifacts/real-chain/a450-$sha-wallets.log}"
mkdir -p "$(dirname "$log")"
work="$(mktemp -d "${ESCROW_WALLETS_TMPDIR:-${TMPDIR:-/tmp}}/escrow-wallets.XXXXXX")"
body="$work/body.log"
: >"$body"
started=0
electrum_line="Electrum $ELECTRUM_VERSION, Electrum-$ELECTRUM_VERSION.tar.gz sha256 $ELECTRUM_SHA256 (not verified: the run stopped before)"
node_line="not started"

finish() {
  local status=$?
  trap - EXIT INT TERM
  if [[ $started -eq 1 ]]; then
    docker rm -fv "$container" >/dev/null 2>&1 || true
  fi
  local result=PASS out="$log"
  [[ "$out" == /* ]] || out="$repo/$log"
  [[ $status -eq 0 ]] || result="FAILED (exit $status)"
  {
    echo "# scripts/test-escrow-wallets.sh (A-450): real local chain (Bitcoin Core regtest, --network none) plus a real Electrum, run by hand, not CI"
    echo "# command: scripts/test-escrow-wallets.sh $(printf '%s' "$cmdline" | sed -E 's#://[^@ ]*@#://<redacted>@#g')"
    echo "# commit: $commit"
    echo "# $electrum_line"
    echo "# node: $node_line"
    echo "# python: $(python3 --version 2>&1); go: $(go env GOVERSION 2>/dev/null)"
    echo "# finished: $(date -u +%Y-%m-%dT%H:%M:%SZ); result: $result"
    cat "$body"
  } >"$out"
  rm -rf "$work"
  echo "Log: $log"
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT TERM

say() { echo "$*" | tee -a "$body"; }
run() { # run <command…>: its output into the log; fails the script when it fails
  "$@" 2>&1 | tee -a "$body"
  return "${PIPESTATUS[0]}"
}

say "== Electrum $ELECTRUM_VERSION: download and verify"
mkdir -p "$work/dl" "$work/gnupg" "$work/src" "$work/out" "$work/tmp"
chmod 700 "$work/gnupg"
fetch() { curl -sSfL --proto '=https' --tlsv1.2 --max-time 600 -o "$1" "$2"; }
fetch "$work/dl/electrum.tar.gz" "$ELECTRUM_URL"
fetch "$work/dl/electrum.tar.gz.asc" "$ELECTRUM_URL.asc"
fetch "$work/dl/signer.asc" "$ELECTRUM_KEY_URL"
got="$(sha256sum "$work/dl/electrum.tar.gz" | cut -d' ' -f1)"
if [[ "$got" != "$ELECTRUM_SHA256" ]]; then
  say "FAILED: Electrum-$ELECTRUM_VERSION.tar.gz sha256 is $got, pinned $ELECTRUM_SHA256"
  exit 1
fi
say "sha256 $got matches the pin"
gpg --homedir "$work/gnupg" --batch --no-autostart --quiet --import "$work/dl/signer.asc" >>"$body" 2>&1 || true
if ! gpg --homedir "$work/gnupg" --batch --no-autostart --with-colons --fingerprint 2>/dev/null | grep -q "^fpr:*$ELECTRUM_SIGNER:"; then
  say "FAILED: the fetched signing key is not $ELECTRUM_SIGNER"
  exit 1
fi
status_lines="$(gpg --homedir "$work/gnupg" --batch --no-autostart --status-fd 1 --verify "$work/dl/electrum.tar.gz.asc" "$work/dl/electrum.tar.gz" 2>>"$body" || true)"
validsig="$(printf '%s\n' "$status_lines" | awk -v fpr="$ELECTRUM_SIGNER" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr')"
if [[ -z "$validsig" ]]; then
  say "FAILED: no valid signature by $ELECTRUM_SIGNER on Electrum-$ELECTRUM_VERSION.tar.gz"
  exit 1
fi
say "signature: ${validsig#\[GNUPG:\] }"
electrum_line="Electrum $ELECTRUM_VERSION, Electrum-$ELECTRUM_VERSION.tar.gz sha256 $ELECTRUM_SHA256 (verified), signed by $ELECTRUM_SIGNER (good signature)"

tar -xzf "$work/dl/electrum.tar.gz" -C "$work/src"
src="$work/src/Electrum-$ELECTRUM_VERSION"
pin_line() { # pin_line <name==version> <file>: "name==version --hash=sha256:… …" from a signed requirements file
  awk -v pin="$1 \\" '$0 == pin {on = 1; printf "%s", substr(pin, 1, length(pin) - 2); next}
    on && /^ *--hash=sha256:[0-9a-f]+( \\)?$/ {printf " %s", $1; n++; next} on {exit} END {if (n) print ""}' "$2"
}
: >"$work/req-source.txt"
: >"$work/req-wheel.txt"
for group in source wheel; do
  if [[ $group == source ]]; then
    pins=("${SOURCE_PINS[@]}") file=requirements.txt
  else
    pins=("${WHEEL_PINS[@]}") file=requirements-binaries.txt
  fi
  for pin in "${pins[@]}"; do
    line="$(pin_line "$pin" "$src/contrib/deterministic-build/$file")"
    if ! [[ "$line" =~ ^[^\ ]+(\ --hash=sha256:[0-9a-f]{64})+$ ]]; then
      say "FAILED: $pin has no sha256 in the signed contrib/deterministic-build/$file"
      exit 1
    fi
    echo "$line" >>"$work/req-$group.txt"
    say "pin ($group): $pin, $(($(wc -w <<<"$line") - 1)) sha256 from the signed contrib/deterministic-build/$file"
  done
done

say "== venv (temporary)"
python3 -m venv "$work/venv"
pip_install() { run env PIP_CONFIG_FILE=/dev/null TMPDIR="$work/tmp" "$work/venv/bin/python" -m pip install --quiet --no-cache-dir \
  --disable-pip-version-check --require-hashes --no-deps "$@"; }
pip_install --only-binary :all: -r "$work/req-wheel.txt"
while read -r line; do # one at a time: setuptools first, then electrum-ecc built with it
  echo "$line" >"$work/req-one.txt"
  pip_install --no-binary :all: --no-build-isolation -r "$work/req-one.txt"
done <"$work/req-source.txt"

say "== Bitcoin Core regtest container $container (no network)"
# create, then start: a failed create leaves nothing, so cleanup never removes a container this run did not make
docker create --name "$container" --network none "$CORE_IMAGE" bitcoind -regtest -server=1 -fallbackfee=0.0002 \
  -printtoconsole=0 >/dev/null
started=1
docker start "$container" >/dev/null
cli() { docker exec -u bitcoin "$container" bitcoin-cli -regtest -datadir=/home/bitcoin/.bitcoin "$@"; }
for _ in $(seq 1 60); do
  cli getblockchaininfo >/dev/null 2>&1 && break
  sleep 1
done
chain="$(cli getblockchaininfo | sed -n 's/.*"chain": *"\([a-z0-9]*\)".*/\1/p')"
if [[ "$chain" != regtest ]]; then
  say "Refusing: the node is not on regtest (or did not start)."
  exit 1
fi
node_line="$CORE_IMAGE ($(docker image inspect "$CORE_IMAGE" --format '{{index .RepoDigests 0}}' 2>/dev/null)), $(docker exec "$container" bitcoind -version | head -1), chain $chain, network none"
say "node: $node_line"

say "== Wallet legs (scripts/escrow-wallets.py, offline)"
set +e
run env PIP_NO_INDEX=1 TMPDIR="$work/tmp" ESCROW_WALLETS_CONTAINER="$container" ESCROW_WALLETS_ELECTRUM_SRC="$src" \
  ESCROW_WALLETS_ELECTRUM_VERSION="$ELECTRUM_VERSION" ESCROW_WALLETS_OUT="$work/out" ESCROW_WALLETS_COMMIT="$commit" \
  "$work/venv/bin/python" scripts/escrow-wallets.py
py=$?
say "== es2-accepts-electrum (Go: checkSettlementPaste on this run's files)"
run env GOPROXY=off ESCROW_WALLETS_FIXTURES="$work/out" go test -count=1 -run '^TestEscrowWalletFixtures$' -v ./internal/market/
gostatus=$?
set -e
if [[ $gostatus -eq 0 ]] && grep -q -- '--- PASS: TestEscrowWalletFixtures ' "$body" && grep -q 'core-shape-a.psbt' "$work/out/manifest.json" &&
  [[ "$(grep -c 'accepted by today.s ES-2 check' "$body")" -ge 4 ]]; then
  say "PASS: es2-accepts-electrum: the ES-2 check accepts every Electrum signature (text above) and the Core one"
else
  say "FAIL: es2-accepts-electrum (go test exited $gostatus)"
fi

if [[ $update -eq 1 && $py -eq 0 && $gostatus -eq 0 ]]; then
  dest=internal/market/testdata/escrow-wallets
  mkdir -p "$dest"
  rm -f "$dest"/*.psbt "$dest/manifest.json"
  cp "$work"/out/electrum-*.psbt "$dest/"
  python3 - "$work/out/manifest.json" "$dest/manifest.json" <<'EOF'
import json, sys
m = json.load(open(sys.argv[1]))
m['entries'] = [e for e in m['entries'] if e['wallet'].startswith('Electrum')]
json.dump(m, open(sys.argv[2], 'w'), indent=1)
open(sys.argv[2], 'a').write('\n')
EOF
  say "fixtures: copied $(ls "$dest"/*.psbt | wc -l) Electrum PSBTs and manifest.json to $dest"
fi

failed=0
for leg in "${LEGS[@]}"; do
  if grep -q "^PASS: $leg: " "$body"; then
    say "PASS: $leg"
  else
    say "FAILED: $leg; see $log"
    failed=1
  fi
done
if [[ $failed -ne 0 || $py -ne 0 || $gostatus -ne 0 ]]; then
  exit 1
fi
say "PASS: every wallet leg on regtest (real local chain plus a real Electrum $ELECTRUM_VERSION, run by hand, not CI)"
