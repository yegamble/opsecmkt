#!/usr/bin/env bash
# scripts/go-fetch.sh against a local proxy (A-782 SRE113-2): a transient failure is retried with its message in the log, a
# tampered module fails after exactly one attempt, an unknown error fails after one, and every retry is bounded. The modules are
# made up here and go.sum holds their hashes, so nothing leaves the host: no public proxy or checksum database is asked.
# Uses `[ ]` and `||`, never `[[ ]]`, so that macOS's bash 3.2 cannot pass a failing check.
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
helper=$repo_dir/scripts/go-fetch.sh
for dependency in go python3 timeout; do
  command -v "$dependency" >/dev/null || { echo "$dependency is required" >&2; exit 1; }
done
work=$(mktemp -d)
proxy_pid=''
stop_proxy() {
  if [ -n "$proxy_pid" ]; then
    kill "$proxy_pid" 2>/dev/null || true
    wait "$proxy_pid" 2>/dev/null || true
    proxy_pid=''
  fi
}
cleanup() {
  stop_proxy
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "FAILED ($leg): $1" >&2
  [ ! -f "$work/leg/stderr" ] || sed 's/^/  stderr: /' "$work/leg/stderr" >&2
  [ ! -f "$work/leg/proxy.log" ] || sed 's/^/  proxy: /' "$work/leg/proxy.log" >&2
  exit 1
}

# The per-attempt limit of the legs that hang. A hang outlives it; the rest never reach it.
attempt_limit=4
# go, with each download attempt counted in the leg's directory for the proxy.
real_go=$(command -v go)
cat > "$work/go" <<SH
#!/bin/sh
if [ "\$1" = mod ]; then echo attempt >> attempts; fi
exec "$real_go" "\$@"
SH
chmod +x "$work/go"

cat > "$work/proxy.py" <<'PY'
"""A Go module proxy with one fault: proxy.py DIRECTORY MODE. It writes DIRECTORY/go.mod, go.sum and port."""
import base64, hashlib, http.server, io, sys, threading, time, zipfile
from pathlib import Path

directory, mode = Path(sys.argv[1]), sys.argv[2]
NAMES = ['fetch-a', 'fetch-b', 'fetch-c', 'fetch-d']
HANG = 60
SLOW = 3


def h1(files):
    """Go's dirhash.Hash1."""
    lines = ''.join('%s  %s\n' % (hashlib.sha256(files[name]).hexdigest(), name) for name in sorted(files))
    return 'h1:' + base64.b64encode(hashlib.sha256(lines.encode()).digest()).decode()


def module_files(name, body=b'package fetch\n'):
    prefix = 'example.test/%s@v1.0.0/' % name
    return {prefix + 'go.mod': go_mod(name), prefix + 'fetch.go': body}


def go_mod(name):
    return b'module example.test/%s\n\ngo 1.21\n' % name.encode()


def zipped(files):
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w') as archive:
        for name in sorted(files):
            archive.writestr(zipfile.ZipInfo(name, (2026, 1, 1, 0, 0, 0)), files[name])
    return buffer.getvalue()


ZIPS = {name: zipped(module_files(name)) for name in NAMES}
TAMPERED = zipped(module_files('fetch-a', b'package fetch\n\nconst Tampered = true\n'))
(directory / 'go.mod').write_text('module example.test/consumer\n\ngo 1.21\n\nrequire (\n' +
                                  ''.join('\texample.test/%s v1.0.0\n' % name for name in NAMES) + ')\n')
(directory / 'go.sum').write_text(''.join(
    'example.test/%s v1.0.0 %s\nexample.test/%s v1.0.0/go.mod %s\n' %
    (name, h1(module_files(name)), name, h1({'go.mod': go_mod(name)})) for name in NAMES))

lock = threading.Lock()
hits = {}
last_served = [0]  # the attempt that last got a module, in progress mode
log_file = (directory / 'proxy.log').open('a', buffering=1)


class Proxy(http.server.BaseHTTPRequestHandler):
    def log_message(self, *arguments):
        pass

    def send(self, status, body, length=None):
        self.send_response(status)
        self.send_header('Content-Length', str(len(body) if length is None else length))
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()

    def do_GET(self):
        parts = self.path.lstrip('/').split('/')
        name = parts[1] if len(parts) == 4 and parts[0] == 'example.test' and parts[2] == '@v' else ''
        if name not in NAMES:
            return self.send(404, b'not found\n')
        if parts[3] == 'v1.0.0.info':
            return self.send(200, b'{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}')
        if parts[3] == 'v1.0.0.mod':
            return self.send(200, go_mod(name))
        if parts[3] != 'v1.0.0.zip':
            return self.send(404, b'not found\n')
        with lock:
            hits[name] = hit = hits.get(name, 0) + 1
            log_file.write('asked %s\n' % name)
            # progress serves one module an attempt and leaves the others hanging: a slow first install. The attempt is
            # the line count of DIRECTORY/attempts, which $GO writes, not a clock, so a slow go start changes nothing.
            attempt = len((directory / 'attempts').read_text().splitlines())
            hold = mode == 'progress' and attempt == last_served[0]
            if not hold:
                last_served[0] = attempt
        first_a = name == 'fetch-a' and hit == 1
        if hold or mode == 'hang-always' or (mode == 'hang-once' and first_a):
            time.sleep(HANG)
            return None
        if mode == 'truncate-once' and first_a or mode == 'truncate-always' and name == 'fetch-a':
            self.send(200, ZIPS[name][:len(ZIPS[name]) // 2], len(ZIPS[name]))
            self.close_connection = True
            return None
        if mode == 'slow' and name == 'fetch-a':
            # A slow link: the whole module takes SLOW seconds, and a client that gives up takes the rest with it.
            body = ZIPS[name]
            self.send_response(200)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            step = len(body) // 6 + 1
            try:
                for start in range(0, len(body), step):
                    self.wfile.write(body[start:start + step])
                    self.wfile.flush()
                    time.sleep(SLOW / 6)
            except OSError:
                self.close_connection = True
                return None
            with lock:
                log_file.write('served %s\n' % name)
            return None
        if mode == '503-once' and first_a or mode == '503-always' and name == 'fetch-a':
            return self.send(503, b'unavailable\n')
        if mode == '403' and name == 'fetch-a':
            return self.send(403, b'refused\n')
        if mode == 'tamper' and name == 'fetch-a':
            return self.send(200, TAMPERED)
        with lock:
            log_file.write('served %s\n' % name)
        return self.send(200, ZIPS[name])


server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
server.daemon_threads = True
(directory / 'port.tmp').write_text(str(server.server_address[1]))
(directory / 'port.tmp').rename(directory / 'port')
server.serve_forever()
PY

# start_leg NAME MODE: a fresh consumer module and module cache, and a proxy with that fault.
start_leg() {
  stop_proxy
  leg=$1
  rm -rf "$work/leg"
  mkdir "$work/leg"
  : > "$work/leg/attempts"
  python3 "$work/proxy.py" "$work/leg" "$2" &
  proxy_pid=$!
  local waited=0
  until [ -s "$work/leg/port" ]; do
    waited=$((waited + 1))
    [ "$waited" -lt 100 ] || fail 'the proxy did not start'
    sleep 0.1
  done
}

# fetch [LIMIT]: the helper in the leg's module; its status goes to $status and its message to $work/leg/stderr. The checksum settings
# are the defaults: go.sum holds every hash, so the database is not asked, and it would not know these modules if it were.
fetch() {
  status=0
  (
    cd "$work/leg"
    env -u GOSUMDB -u GONOSUMDB -u GONOSUMCHECK -u GOPRIVATE -u GONOPROXY -u GOINSECURE -u GOVCS \
      GOPROXY="http://127.0.0.1:$(cat port)" GOMODCACHE="$work/leg/cache" GOFLAGS=-modcacherw GOTOOLCHAIN=local \
      GO="$work/go" GO_FETCH_TIMEOUT="${1:-$attempt_limit}" GO_FETCH_BACKOFF=0 sh "$helper"
  ) 2> "$work/leg/stderr" || status=$?
}

count() { # count PATTERN FILE: the matching lines, 0 for none
  { grep -c -e "$1" "$2" || true; } | tr -d ' '
}
failed_attempts() { count '^go-fetch: attempt ' "$work/leg/stderr"; }
asked() { count "^asked $1\$" "$work/leg/proxy.log"; }
served() { count "^served $1\$" "$work/leg/proxy.log"; }
cached() { [ -s "$work/leg/cache/cache/download/example.test/$1/@v/v1.0.0.zip" ]; }

expect_passed_all() {
  [ "$status" -eq 0 ] || fail "the fetch failed with status $status"
  local name
  for name in fetch-a fetch-b fetch-c fetch-d; do
    cached "$name" || fail "$name is not in the cache"
  done
}

# A clean fetch says nothing and asks once.
start_leg 'no fault' ok
fetch
expect_passed_all
[ ! -s "$work/leg/stderr" ] || fail 'a clean fetch wrote a message'
[ "$(asked fetch-a)" -eq 1 ] || fail 'a clean fetch asked more than once'

# A truncated stream passes on the retry, and the failure stays visible in the log.
start_leg 'truncate once' truncate-once
fetch
expect_passed_all
grep -q 'unexpected EOF' "$work/leg/stderr" || fail 'the truncation is not in the log'
[ "$(failed_attempts)" -eq 1 ] || fail 'expected one failed attempt'
[ "$(asked fetch-a)" -eq 2 ] || fail 'expected the module to be asked for twice'

# A 503 passes on the retry too.
start_leg '503 once' 503-once
fetch
expect_passed_all
grep -q ' 503 ' "$work/leg/stderr" || fail 'the 503 is not in the log'
[ "$(failed_attempts)" -eq 1 ] || fail 'expected one failed attempt'

# Transport failures that do not clear are bounded: three attempts, then the failure.
for mode in truncate-always 503-always; do
  start_leg "$mode" "$mode"
  fetch
  [ "$status" -ne 0 ] || fail 'a failure that never clears passed'
  [ "$(asked fetch-a)" -eq 3 ] || fail "expected exactly 3 attempts, the proxy saw $(asked fetch-a)"
  grep -q 'gave up after 3 attempts' "$work/leg/stderr" || fail 'it does not say that it gave up'
done

# A tampered zip: exactly one attempt, and go's own refusal in the log. The module does not reach the cache.
start_leg 'tampered zip' tamper
fetch
[ "$status" -ne 0 ] || fail 'a tampered module passed'
[ "$(asked fetch-a)" -eq 1 ] || fail "expected exactly 1 attempt, the proxy saw $(asked fetch-a)"
grep -q 'SECURITY ERROR' "$work/leg/stderr" || fail 'the refusal is not in the log'
if cached fetch-a; then fail 'the tampered module is in the cache'; fi
[ "$(failed_attempts)" -eq 0 ] || fail 'it reported a retry'

# An error it does not know: one attempt.
start_leg 'unknown error' 403
fetch
[ "$status" -ne 0 ] || fail 'a refused module passed'
[ "$(asked fetch-a)" -eq 1 ] || fail "expected exactly 1 attempt, the proxy saw $(asked fetch-a)"
grep -q '403' "$work/leg/stderr" || fail 'the error is not in the log'

# A hang: the attempt is cut off and the next one passes, without asking again for what finished.
start_leg 'hang once' hang-once
fetch
expect_passed_all
[ "$(asked fetch-a)" -eq 2 ] || fail 'expected the hung module to be asked for twice'
[ "$(asked fetch-b)" -eq 1 ] || fail 'a finished module was asked for again'
grep -q 'timed out' "$work/leg/stderr" || fail 'the timeout is not in the log'

# A slow first install: every attempt but the last reaches its limit and finishes a module. Four modules take three cut-off attempts
# and a fourth that fetches the last and passes. A helper that spent a retry on each cut-off gives up at the third. Each downloads once.
start_leg 'slow first install' progress
fetch
expect_passed_all
[ "$(count 'timed out' "$work/leg/stderr")" -eq 3 ] || fail 'expected three cut-off attempts'
[ "$(count . "$work/leg/attempts")" -eq 4 ] || fail 'expected four attempts'
[ "$(failed_attempts)" -eq 0 ] || fail 'an attempt that finished a module spent a retry'
for name in fetch-a fetch-b fetch-c fetch-d; do
  [ "$(served "$name")" -eq 1 ] || fail "$name was downloaded more than once"
done

# A hang with no progress: three attempts, then the failure.
start_leg 'hang always' hang-always
fetch
[ "$status" -ne 0 ] || fail 'a proxy that never answers passed'
[ "$(count 'timed out' "$work/leg/stderr")" -eq 3 ] || fail 'expected exactly 3 cut-off attempts'
grep -q 'gave up after 3 attempts' "$work/leg/stderr" || fail 'it does not say that it gave up'
grep -q 'GO_FETCH_TIMEOUT' "$work/leg/stderr" || fail 'the give-up message does not name the setting'

# A slow link: one module takes longer than a low limit, so it never finishes and its partial is lost, and the message names the
# setting. The same fetch with the limit raised passes: the attempt was too short, not the link too bad.
start_leg 'slow link' slow
fetch 2
[ "$status" -ne 0 ] || fail 'a module slower than the limit passed'
if cached fetch-a; then fail 'a module slower than the limit is in the cache'; fi
grep -q 'GO_FETCH_TIMEOUT' "$work/leg/stderr" || fail 'the give-up message does not name the setting'
[ "$(asked fetch-a)" -eq 4 ] || fail "expected a first attempt that finished the others and 3 without progress, saw $(asked fetch-a)"
fetch 30
expect_passed_all
if grep -q 'timed out' "$work/leg/stderr"; then fail 'the raised limit still cut it off'; fi

stop_proxy
leg='stub go'
rm -rf "$work/leg"
mkdir -p "$work/leg/bin"

# The checks are on go's words. Refusal words win over transport words in the same output, and arguments reach go.
cat > "$work/leg/go" <<'SH'
#!/bin/sh
[ "$1" = env ] && exit 0
printf '%s\n' "$*" >> "$STUB_LOG"
printf '%s\n' "$STUB_WORDS" >&2
exit 1
SH
chmod +x "$work/leg/go"
stub() { # stub WORDS [ARGUMENT...]: the helper with go replaced; sets status
  local words=$1
  shift
  status=0
  : > "$work/leg/calls"
  GO="$work/leg/go" STUB_LOG="$work/leg/calls" STUB_WORDS="$words" GO_FETCH_TIMEOUT=30 GO_FETCH_BACKOFF=0 \
    sh "$helper" "$@" 2> "$work/leg/stderr" || status=$?
}
stub 'read: connection reset by peer; verifying example.test/a@v1.0.0: checksum mismatch SECURITY ERROR' -modfile=tools/go.mod
[ "$status" -ne 0 ] && [ "$(count . "$work/leg/calls")" -eq 1 ] || fail 'a refusal beside a transport error was retried'
grep -qxF 'mod download -modfile=tools/go.mod' "$work/leg/calls" || fail 'the arguments did not reach go'
for words in 'stream error: stream ID 7; INTERNAL_ERROR' 'read: connection reset by peer' 'net/http: TLS handshake timeout' \
  'dial tcp: i/o timeout' 'reading https://proxy.example/a.zip: 502 Bad Gateway' 'reading https://proxy.example/a.zip: 504 Gateway Timeout'; do
  stub "$words"
  [ "$status" -ne 0 ] && [ "$(count . "$work/leg/calls")" -eq 3 ] || fail "not retried three times: $words"
done
for words in 'missing go.sum entry for module providing package x' 'go.mod requires go >= 9 (running go 1.27)' \
  'reading https://proxy.example/a.zip: 404 Not Found' 'verifying module: example.test/a: initial go.sum: no such file'; do
  stub "$words"
  [ "$status" -ne 0 ] && [ "$(count . "$work/leg/calls")" -eq 1 ] || fail "retried: $words"
done

# A host without timeout(1), as stock macOS: the same retries, without the limit.
for tool in sh find wc sleep cat; do
  ln -s "$(command -v "$tool")" "$work/leg/bin/$tool"
done
status=0
: > "$work/leg/calls"
PATH="$work/leg/bin" GO="$work/leg/go" STUB_LOG="$work/leg/calls" STUB_WORDS='unexpected EOF' GO_FETCH_BACKOFF=0 \
  sh "$helper" 2> "$work/leg/stderr" || status=$?
[ "$status" -ne 0 ] && [ "$(count . "$work/leg/calls")" -eq 3 ] || fail 'without timeout(1) the retries changed'

echo 'go-fetch regressions passed (truncated stream, 503, hang and slow first install retried with the failure in the log; tampered zip and unknown error after one attempt; persisting failures bounded; a module slower than a low limit passes with a raised one)'
