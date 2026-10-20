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
