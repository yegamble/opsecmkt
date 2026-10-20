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
  "$go_command" tool -modfile=tools/go.mod "$@"
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
    "$go_command" -C tools mod verify
    "$go_command" -C tools mod tidy -diff
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
    echo 'Usage: scripts/quality.sh {fmt-check [files...]|fmt|lint|lint-audit|vet|mod-tidy|go-quality|check|vuln|workflow-lint|deadcode|secrets-staged|hooks-install}' >&2
    exit 2
    ;;
esac
