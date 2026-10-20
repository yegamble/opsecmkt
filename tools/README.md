# Development tools

This isolated Go module pins the tools used by `scripts/quality.sh` and Lefthook.
Run the shared commands from the repository root; they use
`go tool -modfile=tools/go.mod` so development tool dependencies stay outside the
application's `go.mod` and `go.sum`. Go downloads and caches tools on first use.
Use the application toolchain selected by the root `go.mod`.

Dependabot watches this module. To update one tool manually, use its module's
current package path and an explicit reviewed version:

```sh
go -C tools get -tool PACKAGE@VERSION
go -C tools mod tidy
make go-quality
make workflow-lint
make vuln
```

Review analyzer/configuration changes with a tool update. A larger diagnostic
count is a reason to review findings; preserve the enforced checks and avoid a
new-only baseline or blanket suppression. `make lint-audit` and `make deadcode`
are additional review commands with the limitations described in
[Contributing](../CONTRIBUTING.md#gates).

The indirect `github.com/charmbracelet/x/cellbuf` pin keeps Gitleaks' terminal
output dependency compatible with the ANSI version also required by Lefthook.
Check both `go tool -modfile=tools/go.mod gitleaks version` and
`go tool -modfile=tools/go.mod lefthook version` when changing these dependencies.
