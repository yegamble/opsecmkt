package main

import (
	"bytes"
	"strings"
	"testing"
)

// The version command prints the link-time build identity the Dockerfile and release workflow pass
// (-X main.buildVersion, -X main.buildCommit), so `docker run --rm IMAGE version` names what serves (A-696).
func TestVersionPrintsBuildIdentity(t *testing.T) {
	prevVersion, prevCommit := buildVersion, buildCommit
	t.Cleanup(func() { buildVersion, buildCommit = prevVersion, prevCommit })
	for _, tc := range []struct{ version, commit, want string }{
		{"v0.1.0-alpha.2", "0123456789abcdef0123456789abcdef01234567", "opsecmkt v0.1.0-alpha.2 commit 0123456789abcdef0123456789abcdef01234567\n"},
		// Empty link-time values keep today's behaviour: "development", and no commit (a test binary has no vcs stamp).
		{"development", "", "opsecmkt development\n"},
		{"", "", "opsecmkt development\n"},
	} {
		buildVersion, buildCommit = tc.version, tc.commit
		var out, errOut bytes.Buffer
		// strings.Fields, not a one-element literal: gosec's G602 would otherwise flag command's guarded args[1].
		handled, err := command(strings.Fields("version"), strings.NewReader(""), &out, &errOut)
		if !handled || err != nil || out.String() != tc.want || errOut.Len() != 0 {
			t.Fatalf("version %q %q: handled=%v err=%v out=%q stderr=%q", tc.version, tc.commit, handled, err, out.String(), errOut.String())
		}
	}
	if _, err := command([]string{"version", "extra"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("version accepted an argument")
	}
}
