#!/usr/bin/env bash
# Run one gate command with its output kept out of the agent's context.
#
#   .claude/war-room/quiet.sh <label> <command> [args...]
#
# The full output goes to a log file (WR_LOG_DIR, default $TMPDIR/war-room-logs).
# On success it prints one PASS line, the log path and the last 3 lines (Go's
# "ok" package lines, Playwright's "N passed"). On failure it prints the exit
# status, up to 40 failure lines and the last 40 lines. The exit status is the
# command's, so `&&` chains and reports stay honest. Paste the printed lines as
# gate evidence; the log stays on disk for anyone who needs the rest.
set -u

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <label> <command> [args...]" >&2
  exit 2
fi
label=$1
shift

dir=${WR_LOG_DIR:-${TMPDIR:-/tmp}/war-room-logs}
mkdir -p "$dir"
log="$dir/$(printf '%s' "$label" | tr -c 'A-Za-z0-9._-' '_')-$(date +%Y%m%d-%H%M%S)-$$.log"

start=$(date +%s)
"$@" >"$log" 2>&1
status=$?
secs=$(( $(date +%s) - start ))

if [ "$status" -eq 0 ]; then
  echo "PASS $label (${secs}s) log: $log"
  tail -n 3 "$log"
else
  echo "FAIL $label exit $status (${secs}s) log: $log"
  echo "--- failure lines (first 40)"
  grep -nE -- '--- FAIL|^FAIL|panic:|DATA RACE|Error:|[✘✗]|[0-9]+ failed|Traceback|^E  ' "$log" | head -n 40
  echo "--- last 40 lines"
  tail -n 40 "$log"
fi
exit "$status"
