package market

import "runtime/debug"

// BuildVersion and BuildCommit are this market's own build (A-553), shown in the administrator's dependency table.
// cmd/server sets them before New from its link-time values (release builds: -X main.buildVersion=<tag>
// -X main.buildCommit=<commit>; images: the Dockerfile's BUILD_VERSION and BUILD_COMMIT, A-696). A blank commit falls back to the Go toolchain's vcs.revision when the binary was built
// from a git checkout.
var BuildVersion, BuildCommit = "development", ""

// marketBuild is the build version, its commit ("" when not recorded) and whether the build was made from a checkout
// with uncommitted changes (vcs.modified).
func marketBuild() (version, commit string, modified bool) {
	version, commit = BuildVersion, BuildCommit
	if version == "" {
		version = "development"
	}
	if commit != "" {
		return version, commit, false
	}
	if b, ok := debug.ReadBuildInfo(); ok {
		for _, s := range b.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	return version, commit, modified
}
