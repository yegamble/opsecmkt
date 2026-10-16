package secretfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range names {
		t.Setenv(key, "")
		t.Setenv(key+"_FILE", "")
	}
}

func TestLoadEnvironment(t *testing.T) {
	cleanEnvironment(t)
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("private-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_SECRET_KEY_FILE", p)
	if err := LoadEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("APP_SECRET_KEY"); got != "private-value" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadEnvironmentRefusesInvalidAndChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, body        string
		mode              os.FileMode
		conflict, symlink bool
	}{
		{name: "empty", mode: 0600}, {name: "embedded newline", body: "private\nvalue", mode: 0600},
		{name: "extra newline", body: "private\n\n", mode: 0600}, {name: "nul", body: "private\x00value", mode: 0600},
		{name: "public mode", body: "private-value", mode: 0644}, {name: "oversize", body: strings.Repeat("x", 65537), mode: 0600},
		{name: "conflict", body: "private-value", mode: 0600, conflict: true}, {name: "symlink", body: "private-value", mode: 0600, symlink: true},
		{name: "carriage return", body: "private\rvalue", mode: 0600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnvironment(t)
			dir := t.TempDir()
			good := filepath.Join(dir, "good")
			if err := os.WriteFile(good, []byte("good-secret"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DATABASE_URL_FILE", good)
			p := filepath.Join(dir, "bad")
			if err := os.WriteFile(p, []byte(tc.body), tc.mode); err != nil {
				t.Fatal(err)
			}
			if tc.symlink {
				link := filepath.Join(dir, "link")
				if err := os.Symlink(p, link); err != nil {
					t.Fatal(err)
				}
				p = link
			}
			t.Setenv("APP_SECRET_KEY_FILE", p)
			if tc.conflict {
				t.Setenv("APP_SECRET_KEY", "other")
			}
			err := LoadEnvironment()
			if err == nil {
				t.Fatal("accepted invalid secret")
			}
			if strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "good-secret") {
				t.Fatal("error disclosed secret")
			}
			if os.Getenv("DATABASE_URL") != "" {
				t.Fatal("partly changed environment")
			}
		})
	}
}

func TestLoadEnvironmentAllowlist(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("UNRECOGNIZED_SECRET_FILE", "/does/not/exist")
	if err := LoadEnvironment(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_SECRET_KEY_FILE", t.TempDir())
	if err := LoadEnvironment(); err == nil {
		t.Fatal("accepted directory")
	}
}
