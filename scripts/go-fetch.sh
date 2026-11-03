#!/bin/sh
# go mod download with a bounded retry on transport errors only (A-782 SRE113-2): the Dockerfile's download step and
# scripts/quality.sh vuln. Arguments go to go mod download. POSIX sh, for the build image.
#
# It sets nothing about where modules come from: GOPROXY, GOSUMDB and go.sum stay in charge, and every attempt verifies what it
# downloads. A checksum refusal or an error not named below fails at once, with go's own message. Finished modules stay in the
# module cache across attempts, so a retry asks only for the rest. An attempt that reaches the time limit but finished a module
# does not spend a retry, so a slow first install keeps going; the module count bounds those, and three attempts bound the others.
# A single module that cannot finish inside the limit is lost at each attempt, since go keeps no partial download: that is what
# GO_FETCH_TIMEOUT is for. The Dockerfile declares it as a build argument (600); make vuln has the default below.
set -u

go_command=${GO:-go}
attempt_limit=${GO_FETCH_TIMEOUT:-180}
# Overridden only by scripts/test-go-fetch.sh.
backoff=${GO_FETCH_BACKOFF:-10}
attempts=3

if command -v timeout >/dev/null 2>&1; then
  download() { timeout "$attempt_limit" "$go_command" mod download "$@" 2>&1; }
else
  download() { "$go_command" mod download "$@" 2>&1; }
fi

cache=$("$go_command" env GOMODCACHE) || exit 1
finished_modules() { find "$cache/cache/download" -name '*.zip' 2>/dev/null | wc -l; }

spent=0
while :; do
  before=$(finished_modules)
  status=0
  output=$(download "$@") || status=$?
  [ -z "$output" ] || printf '%s\n' "$output" >&2
  [ "$status" -ne 0 ] || exit 0
  case $output in
    *'SECURITY ERROR'* | *'checksum mismatch'*) exit 1 ;;
  esac
  if [ "$status" -eq 124 ]; then
    echo "go-fetch: go mod download timed out after ${attempt_limit}s" >&2
    if [ "$(finished_modules)" -gt "$before" ]; then
      echo 'go-fetch: it finished a module, so it continues' >&2
      continue
    fi
  else
    case $output in
      *'stream error'* | *'unexpected EOF'* | *'connection reset'* | *'TLS handshake timeout'* | *'i/o timeout'* | *' 502 '* | *' 503 '* | *' 504 '*) ;;
      *) exit 1 ;;
    esac
  fi
  spent=$((spent + 1))
  if [ "$spent" -ge "$attempts" ]; then
    echo "go-fetch: gave up after $attempts attempts without progress. If the link is only slow, raise the limit of ${attempt_limit}s per attempt with GO_FETCH_TIMEOUT (docs/install/troubleshooting.md#first-install-failed)" >&2
    exit 1
  fi
  echo "go-fetch: attempt $spent of $attempts failed in transport; retrying in $((spent * backoff))s" >&2
  sleep "$((spent * backoff))"
done
