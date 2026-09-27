#!/usr/bin/env bash
# Time GET /canary on an onion address through a Tor SOCKS proxy, as a visitor would reach it. Run it from
# a machine other than the server (docs/operator-guide.md#watching-the-onion): on the server itself it cannot
# see what visitors see. Each probe uses its own SOCKS credentials, so Tor builds a new circuit and goes
# through the introduction point, the proof-of-work queue and a rendezvous every time, as a new visitor does.
#
#   scripts/onion-probe.sh [--socks HOST:PORT] [--count N] [--interval SECONDS] [--timeout SECONDS]
#                          [--path PATH] ADDRESS.onion
#
# --socks defaults to 127.0.0.1:9050 (a system tor; Tor Browser listens on 127.0.0.1:9150). The tor behind it
# must be 0.4.8 or later with the pow module (`tor --list-modules` prints `pow: yes`), or the probe cannot
# solve puzzles and waits behind visitors who do. Prints one line per probe and a summary; exits 0 when
# every probe got HTTP 200, 1 when any failed, 2 for a usage error. Needs curl. Logs nothing anywhere else.
set -euo pipefail

usage() {
  echo 'Usage: scripts/onion-probe.sh [--socks HOST:PORT] [--count N] [--interval SECONDS] [--timeout SECONDS] [--path PATH] ADDRESS.onion' >&2
  exit 2
}
bad() { echo "onion-probe: $*" >&2; usage; }

socks=127.0.0.1:9050 count=5 interval=10 timeout=120 path=/canary address=''
while (($#)); do
  case $1 in
    --socks|--count|--interval|--timeout|--path)
      (($# >= 2)) || bad "$1 needs a value"
      case $1 in
        --socks) socks=$2 ;;
        --count) count=$2 ;;
        --interval) interval=$2 ;;
        --timeout) timeout=$2 ;;
        --path) path=$2 ;;
      esac
      shift 2 ;;
    -h|--help) usage ;;
    -*) bad "unknown option $1" ;;
    *) [[ -z $address ]] || bad 'give one address'; address=$1; shift ;;
  esac
done
[[ -n $address ]] || usage
# Accept the bare address, or http://ADDRESS with an optional trailing slash; nothing else.
host=${address#http://}
host=${host%/}
[[ $host =~ ^[a-z2-7]{56}\.onion$ ]] || bad "not a v3 onion address: $address"
[[ $count =~ ^[1-9][0-9]{0,3}$ ]] || bad '--count must be a whole number from 1 to 9999'
[[ $interval =~ ^[0-9]{1,5}$ ]] || bad '--interval must be a whole number of seconds'
[[ $timeout =~ ^[1-9][0-9]{0,4}$ ]] || bad '--timeout must be a whole number of seconds, at least 1'
[[ $socks =~ ^[A-Za-z0-9.-]+:[0-9]{1,5}$|^\[[0-9A-Fa-f:]+\]:[0-9]{1,5}$ ]] || bad '--socks must be HOST:PORT'
[[ $path =~ ^/[A-Za-z0-9._~/-]*$ ]] || bad '--path must start with / and hold no query'
command -v curl >/dev/null || { echo 'onion-probe: curl is required' >&2; exit 2; }

url="http://$host$path"
tag="opsecmkt-probe-$$-$RANDOM"
errors=$(mktemp "${TMPDIR:-/tmp}/onion-probe.XXXXXX")
trap 'rm -f "$errors"' EXIT
ok=0 times=()
for ((i = 1; i <= count; i++)); do
  # socks5h: Tor resolves the onion address. A new user name per probe isolates it on a fresh circuit
  # (IsolateSOCKSAuth, on by default for every SocksPort). The password is required by the syntax only.
  status=0
  result=$(curl --silent --show-error --output /dev/null --max-time "$timeout" \
    --proxy "socks5h://$tag-$i:probe@$socks" --write-out '%{http_code} %{time_total}' "$url" 2>"$errors") ||
    status=$?
  read -r code seconds _ <<< "${result:-000 0}"
  when=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  if [[ $status -eq 0 && $code == 200 ]]; then
    ok=$((ok + 1))
    times+=("$seconds")
    echo "$when probe $i/$count ok http=$code seconds=$seconds"
  else
    # curl's own message, for example a SOCKS refusal or "Operation timed out".
    echo "$when probe $i/$count FAIL http=$code seconds=$seconds curl_exit=$status $(tr -s '\n' ' ' < "$errors")"
  fi
  ((i == count)) || sleep "$interval"
done

summary="$ok/$count ok"
if ((${#times[@]})); then
  summary="$summary; seconds for successful probes: $(printf '%s\n' "${times[@]}" | sort -n | awk '
    { t[NR] = $1 }
    END { printf "fastest %s, median %s, slowest %s", t[1], t[int((NR + 1) / 2)], t[NR] }')"
fi
echo "summary: $url via $socks: $summary"
((ok == count))
