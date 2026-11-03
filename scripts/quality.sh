#!/usr/bin/env bash
# Shared, check-only entry points for developers, Git hooks and GitHub Actions.
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_dir"
go_command=${GO:-go}

require_go() {
  if ! command -v "$go_command" >/dev/null 2>&1; then
    echo 'Install Go using the toolchain in go.mod before running quality checks.' >&2
    exit 1
  fi
}

tool() {
  require_go
  local manifest=tools/go.mod
  if [ "$1" = actionlint ]; then
    # actionlint needs YAML v4 rc.3; newer Go security analyzers need rc.6.
    manifest=tools/actionlint/go.mod
  fi
  "$go_command" tool -modfile="$manifest" "$@"
}

# The modules govulncheck builds from, which make vuln prefetches rather than the whole tool manifest. It cannot derive them, since
# listing them downloads them; tools_smoke fails when the list no longer matches.
govulncheck_modules=(golang.org/x/mod golang.org/x/sync golang.org/x/telemetry golang.org/x/tools golang.org/x/vuln)

tools_smoke() {
  local help_output status=0
  # goimports deliberately exits 2 for -h. Require its actual help text so a
  # compilation failure cannot be mistaken for this expected usage status.
  help_output=$(tool goimports -h 2>&1) || status=$?
  if { [ "$status" -ne 0 ] && [ "$status" -ne 2 ]; } || [[ "$help_output" != *'usage: goimports [flags] [path ...]'* ]]; then
    printf '%s\n' "$help_output" >&2
    if [ "$status" -eq 0 ]; then status=1; fi
    exit "$status"
  fi
  printf 'goimports help bootstrap passed\n'
  tool golangci-lint version
  tool lefthook version
  tool gitleaks version
  help_output=$(tool deadcode -help 2>&1) || {
    status=$?
    printf '%s\n' "$help_output" >&2
    exit "$status"
  }
  printf 'deadcode help bootstrap passed\n'
  tool govulncheck -version
  tool actionlint -version
  local listed expected
  listed=$("$go_command" list -modfile=tools/go.mod -deps -f '{{with .Module}}{{.Path}}{{end}}' golang.org/x/vuln/cmd/govulncheck | sed '/^$/d' | sort -u)
  expected=$(printf '%s\n' "${govulncheck_modules[@]}" | sort -u)
  if [ "$listed" != "$expected" ]; then
    printf 'govulncheck builds from other modules than govulncheck_modules in scripts/quality.sh; update the list. It builds from:\n%s\n' "$listed" >&2
    exit 1
  fi
}

format_go() {
  local mode=$1 changed filename
  shift
  local files=()
  if [ "$#" -gt 0 ]; then
    files=("$@")
  else
    git rev-parse --is-inside-work-tree >/dev/null
    while IFS= read -r -d '' filename; do
      files+=("$filename")
    done < <(git ls-files -z -- '*.go')
  fi
  if [ "${#files[@]}" -eq 0 ]; then
    return
  fi
  if [ "$mode" = write ]; then
    tool goimports -w "${files[@]}"
    return
  fi
  changed=$(tool goimports -l "${files[@]}")
  if [ -n "$changed" ]; then
    printf '%s\n' "$changed"
    echo 'Go formatting or imports need correction. Run make fmt, then review the diff.' >&2
    exit 1
  fi
}

case "${1:-help}" in
  fmt-check)
    shift
    format_go check "$@"
    ;;
  fmt)
    # An explicit opt-in write command; neither hooks nor CI call this.
    format_go write
    ;;
  lint)
    tool golangci-lint run ./...
    ;;
  lint-audit)
    # Advisory full audit: review findings before changing protocol/error behavior.
    tool golangci-lint run --no-config --enable-only errcheck,gosec --max-issues-per-linter=0 --max-same-issues=0 ./...
    ;;
  vet)
    require_go
    "$go_command" vet ./...
    ;;
  mod-tidy)
    require_go
    "$go_command" mod verify
    "$go_command" mod tidy -diff
    for module in tools tools/actionlint; do
      "$go_command" -C "$module" mod verify
      "$go_command" -C "$module" mod tidy -diff
    done
    ;;
  tools-smoke)
    tools_smoke
    ;;
  go-quality|check)
    format_go check
    bash scripts/quality.sh mod-tidy
    bash scripts/quality.sh vet
    bash scripts/quality.sh lint
    ;;
  vuln)
    # A transient proxy failure is retried here, not left to fail the scan part way through (A-782).
    require_go
    GO=$go_command sh scripts/go-fetch.sh
    GO=$go_command sh scripts/go-fetch.sh -modfile=tools/go.mod "${govulncheck_modules[@]}"
    tool govulncheck -test ./...
    ;;
  workflow-lint)
    tool actionlint
    ;;
  deadcode)
    tool deadcode ./cmd/server ./cmd/verify-audit
    ;;
  secrets-staged)
    tool gitleaks git --staged --redact --no-banner
    ;;
  hooks-install)
    tool lefthook install
    ;;
  *)
    echo 'Usage: scripts/quality.sh {fmt-check [files...]|fmt|lint|lint-audit|vet|mod-tidy|tools-smoke|go-quality|check|vuln|workflow-lint|deadcode|secrets-staged|hooks-install}' >&2
    exit 2
    ;;
esac
