# Development tools

The isolated Go modules here pin the tools used by `scripts/quality.sh` and Lefthook.
Run the shared commands from the repository root. Most use
`go tool -modfile=tools/go.mod`; actionlint uses the separate
`tools/actionlint/go.mod`. Development tool dependencies stay outside the
application's `go.mod` and `go.sum`. Go downloads and caches tools on first use.
Use the application toolchain selected by the root `go.mod`.

Dependabot watches both tool modules. To update one tool manually, use its module's
current package path and an explicit reviewed version:

```sh
go -C tools get -tool PACKAGE@VERSION
go -C tools mod tidy
make tools-smoke
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

Actionlint v1.7.12 uses the YAML v4 rc.3 error API; the current Go security
analyzer uses the incompatible rc.6 API. Keeping actionlint in its own module
preserves both tools' supported dependency graphs. Update it with
`go -C tools/actionlint get -tool github.com/rhysd/actionlint/cmd/actionlint@VERSION`
and `go -C tools/actionlint mod tidy`. `make mod-tidy` verifies both modules.

`make tools-smoke` executes every pinned tool's real version/help path. It is
required in the Go quality CI job, so shared dependency updates that prevent a
tool from compiling fail before integration tests finish.
